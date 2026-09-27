//go:build (darwin || linux) && arm64

package lua

import (
	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

// Lua-to-Lua calls and returns between compiled functions, in machine
// code. They do what the interpreter's fast paths do (callLua and
// pushLuaFrame, and opReturn's fixed-results case) and jump straight into
// the other function's code, so recursion and method calls stay compiled.
// They store pointers into call frames, so they run only while the write
// barrier is off; otherwise they exit and runJIT makes the call in Go.

const (
	rState Reg = 22 // *State
	rCI    Reg = 23 // the running *callInfo
	rNext  Reg = 24 // the callee's or caller's *callInfo
	rStack Reg = 25 // &l.stack[0], or a jump target
)

// callLua compiles the CALL i at ip for a callee in rT that is a compiled,
// fixed-parameter Lua closure. It branches to notLua when the callee is not
// a Lua closure, and exits for any other Lua closure.
func (c *arm64Compiler) callLua(ip int, i bytecode.Instruction, notLua Label) {
	a := &c.a
	ra, b, results := i.A(), i.B(), i.C()-1
	fn := reg(ra)
	exit := c.exit(ip)
	a.Ldr(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkLuaClosure))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, notLua)
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, exit) // a number whose bits match the tag
	a.Cbnz(rBarrier, exit)
	a.Ldr(rT2, rT, offClProto)
	a.Ldrb(rTmp, rT2, offPVarKind)
	a.CmpImm(rTmp, uint32(bytecode.VarArgTable))
	a.BCond(EQ, exit) // Go makes the table
	a.Ldr(rCache, rT2, offPJit)
	a.Cbz(rCache, exit)
	a.Ldr(rState, rCtx, offCtxS)
	a.Ldr(rStack, rState, offStack)
	// function := ci.stackIndex(a); l.top = function + 1 + argCount.
	a.AddImm(rIdx, rFrame, uint32(ra)*valueSize)
	a.Sub(rIdx, rIdx, rStack)
	a.Lsr(rIdx, rIdx, 4)
	if b == 0 {
		a.Ldr(rSlot, rState, offLTop) // where the call before left it
	} else {
		a.AddImm(rSlot, rIdx, uint32(b))
	}
	// checkStack(p.maxStackSize) would grow the stack, as would a vararg
	// function's, which adds the parameters: this checks both.
	a.Ldr(rLen, rT2, offPMaxStack)
	a.Ldr(rNext, rState, offLStackLast)
	a.Sub(rNext, rNext, rSlot)
	a.Ldr(rTmp, rT2, offPParams)
	a.Add(rTmp, rTmp, rLen)
	a.Cmp(rNext, rTmp)
	a.BCond(LE, exit)
	// pushLuaFrame reuses l.callInfo.next, which must have Lua storage.
	a.Ldr(rCI, rState, offLCallInfo)
	a.Ldr(rNext, rCI, offCINext)
	a.Cbz(rNext, exit)
	a.Ldr(rP, rNext, offCILua)
	a.Cbz(rP, exit)
	c.spend(ip)

	// Clear the parameters the call does not pass.
	loop, cleared := a.NewLabel(), a.NewLabel()
	a.Ldr(rN, rT2, offPParams)
	if b == 0 { // the arguments: l.top - function - 1
		a.Sub(rTmp, rSlot, rIdx)
		a.SubImm(rTmp, rTmp, 1)
	} else {
		a.MovImm(rTmp, uint64(b-1))
	}
	a.Bind(loop)
	a.Cmp(rTmp, rN)
	a.BCond(GE, cleared)
	a.AddShifted(rTmp2, rFrame, rTmp, 4)
	a.Str(ZR, rTmp2, uint32(ra+1)*valueSize+offP)
	a.Str(ZR, rTmp2, uint32(ra+1)*valueSize+offN)
	a.AddImm(rTmp, rTmp, 1)
	a.B(loop)
	a.Bind(cleared)

	// The caller resumes after the call.
	a.Ldr(rTmp, rCI, offCILua)
	a.MovImm(rTmp2, uint64(ip+1))
	a.Str(rTmp2, rTmp, offLSavedPC)

	// pushLuaFrame(function, function+1, results, closure), then
	// setCallStatus(callStatusReentry). rP holds the new luaCallInfo.
	a.Str(ZR, rP, offLSavedPC)
	for w := uint32(0); w < 24; w += 8 {
		a.Ldr(rTmp2, rT2, offPCode+w)
		a.Str(rTmp2, rP, offLCode+w)
	}
	a.Str(rT, rP, offLClosure)
	a.Str(rIdx, rNext, offCIFunction)
	varArgs, based := a.NewLabel(), a.NewLabel()
	a.Ldrb(rTmp, rT2, offPVarArg)
	a.Cbnz(rTmp, varArgs)
	a.AddImm(rSlot, rIdx, 1) // base
	a.Bind(based)
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(varArgs)
		c.adjustVarArgs(rCI)
		a.B(based)
	})
	a.AddShifted(rLen, rSlot, rLen, 0) // top = base + maxStackSize
	a.Str(rLen, rNext, offCITop)
	a.MovImm(rTmp2, uint64(int64(results)))
	a.Str(rTmp2, rNext, offCIResults)
	a.MovImm(rTmp2, uint64(callStatusLua|callStatusReentry))
	a.Strb(rTmp2, rNext, offCIStatus)
	a.Strb(ZR, rNext, offCIMeta) // no __call metamethods
	// frame = l.stack[base:top]
	a.AddShifted(rTmp, rStack, rSlot, 4)
	a.Str(rTmp, rP, offLFrame)
	a.Sub(rTmp2, rLen, rSlot)
	a.Str(rTmp2, rP, offLFrame+offSliceLen)
	a.Ldr(rTmp2, rState, offStack+offSliceCap)
	a.Sub(rTmp2, rTmp2, rSlot)
	a.Str(rTmp2, rP, offLFrame+offSliceCap)
	a.Str(rNext, rState, offLCallInfo)
	a.Str(rLen, rState, offLTop)

	// Enter the callee.
	a.Mov(rFrame, rTmp)
	a.Ldr(rConst, rT2, offPConsts)
	a.Ldr(rUpVals, rT, offClUpVals)
	a.Ldr(rTmp, rCache, offJCEntry)
	a.Br(rTmp)
}

// spend spends one unit of budget at ip, where compiled code starts again
// once runJIT has let the goroutine be preempted.
func (c *arm64Compiler) spend(ip int) {
	if c.budget[ip] < 0 {
		c.budget[ip] = c.a.NewLabel()
	}
	c.a.SubsImm(rBudget, rBudget, 1)
	c.a.BCond(EQ, c.budget[ip])
}

// exitIfUpValuesOpen exits at ip if any upvalue is open at or above the base
// of the frame whose callInfo is in rCI, of the state in rState: a function
// with nested functions returns or tail calls in compiled code only when
// there are none for Go to close. The open upvalues are sorted highest
// first. It uses rTmp and rTmp2.
func (c *arm64Compiler) exitIfUpValuesOpen(ip int) {
	a := &c.a
	none := a.NewLabel()
	a.Ldr(rTmp, rState, offLUpValues)
	a.Cbz(rTmp, none)
	a.Ldr(rTmp, rTmp, offUVIndex)
	a.Ldr(rTmp2, rCI, offCIFunction)
	a.Cmp(rTmp, rTmp2)
	a.BCond(GT, c.exit(ip)) // index >= base, which is function + 1
	a.Bind(none)
}

// adjustVarArgs moves a vararg callee's frame above its arguments, as
// adjustVarArgs does. With the function's index in rIdx, the arguments'
// end in rSlot, the prototype in rT2 and &stack[0] in rStack, it leaves the
// base in rSlot, the fixed parameters moved there and nil where they were,
// and the vararg marker after them: nil, or the view of a named vararg
// table only indexed. It uses rTmp, rTmp2, rN and tmp.
func (c *arm64Compiler) adjustVarArgs(tmp Reg) {
	a := &c.a
	a.Ldr(rN, rT2, offPParams)
	a.Add(rTmp, rIdx, rN)
	a.AddImm(rTmp, rTmp, 1) // after the parameters, the missing ones cleared
	a.Cmp(rSlot, rTmp)
	a.Csel(rSlot, rSlot, rTmp, GE) // the base
	a.AddImm(rTmp, rIdx, 1)
	a.AddShifted(rTmp, rStack, rTmp, 4)   // the first parameter
	a.AddShifted(rTmp2, rStack, rSlot, 4) // where it goes, past the last
	loop, marker := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cbz(rN, marker)
	a.Ldr(tmp, rTmp, offP)
	a.Str(tmp, rTmp2, offP)
	a.Ldr(tmp, rTmp, offN)
	a.Str(tmp, rTmp2, offN)
	a.Str(ZR, rTmp, offP)
	a.Str(ZR, rTmp, offN)
	a.AddImm(rTmp, rTmp, valueSize)
	a.AddImm(rTmp2, rTmp2, valueSize)
	a.SubImm(rN, rN, 1)
	a.B(loop)
	a.Bind(marker) // rTmp2: the register after the parameters
	view, done := a.NewLabel(), a.NewLabel()
	a.Str(ZR, rTmp2, offN)
	a.Ldrb(rTmp, rT2, offPVarKind)
	a.CmpImm(rTmp, uint32(bytecode.VarArgView))
	a.BCond(EQ, view)
	a.Str(ZR, rTmp2, offP)
	a.B(done)
	a.Bind(view)
	a.MovImm(rTmp, uint64(uintptr(varArgView.p)))
	a.Str(rTmp, rTmp2, offP)
	a.Bind(done)
}

// varArg compiles VARARG A B of a function without a vararg table: B-1 of
// its extra arguments, nil past the last, or for B 0 all of them, leaving
// l.top after them. The extra arguments lie below the frame, after the
// function and its fixed parameters' old places. It exits while the write
// barrier is on, and when all of them would not fit the stack, for Go to
// grow it.
func (c *arm64Compiler) varArg(ip int, i bytecode.Instruction) {
	a := &c.a
	if c.p.VarArgKind == bytecode.VarArgTable {
		c.exitAlways(ip)
		return
	}
	exit := c.exit(ip)
	a.Cbnz(rBarrier, exit)
	a.Ldr(rT2, rCtx, offCtxS)    // the state
	a.Ldr(rT, rT2, offLCallInfo) // the callInfo
	a.Ldr(rTmp, rT2, offStack)
	a.Sub(rIdx, rFrame, rTmp)
	a.Lsr(rIdx, rIdx, 4) // the base
	a.Ldr(rTmp, rT, offCIFunction)
	a.Sub(rTmp2, rIdx, rTmp)
	a.SubImm(rTmp2, rTmp2, uint32(c.p.ParameterCount+1)) // n, the extra arguments
	a.AddShifted(rAddr, ZR, rTmp2, 4)
	a.Sub(rAddr, rFrame, rAddr) // the first
	ra, wanted := i.A(), i.B()-1
	if wanted >= 0 {
		for j := range wanted {
			dst, none, next := reg(ra+j), a.NewLabel(), a.NewLabel()
			a.CmpImm(rTmp2, uint32(j))
			a.BCond(LE, none)
			c.load(operand{rAddr, uint32(j) * valueSize})
			c.store(dst)
			a.B(next)
			a.Bind(none)
			a.Str(ZR, dst.base, dst.off+offP)
			a.Str(ZR, dst.base, dst.off+offN)
			a.Bind(next)
		}
		return
	}
	// All of them, the frame growing to hold them as varArgs grows it,
	// within the stack; Go grows the stack.
	fits := a.NewLabel()
	a.AddImm(rP, rIdx, uint32(ra))
	a.Add(rP, rP, rTmp2) // l.top after them
	a.Ldr(rTmp, rT, offCITop)
	a.Cmp(rP, rTmp)
	a.BCond(LE, fits)
	a.Ldr(rN, rT2, offLStackLast)
	a.Cmp(rP, rN)
	a.BCond(GE, exit)
	a.Str(rP, rT, offCITop)
	a.Ldr(rN, rT, offCILua)
	a.Sub(rTmp, rP, rIdx)
	a.Str(rTmp, rN, offLFrame+offSliceLen) // frame = l.stack[base:ci.top]
	a.Bind(fits)
	a.Str(rP, rT2, offLTop)
	loop, done := a.NewLabel(), a.NewLabel()
	a.AddImm(rT, rFrame, uint32(ra)*valueSize) // the destination
	a.Bind(loop)
	a.Cbz(rTmp2, done)
	c.load(operand{rAddr, 0})
	c.store(operand{rT, 0})
	a.AddImm(rAddr, rAddr, valueSize)
	a.AddImm(rT, rT, valueSize)
	a.SubImm(rTmp2, rTmp2, 1)
	a.B(loop)
	a.Bind(done)
}

// closeJump compiles JMP A sBx with A > 0, which closes upvalues and
// to-be-closed variables from register A-1 up, as a generic for's exit
// does: it jumps when nothing there is open, and exits for Go to close
// what is.
func (c *arm64Compiler) closeJump(ip int, i bytecode.Instruction) {
	a := &c.a
	exit := c.exit(ip)
	level := reg(i.A() - 1)
	a.AddImm(rTmp2, level.base, level.off) // its address
	a.Ldr(rTmp, rCtx, offCtxTBC)
	a.Cmp(rTmp, rTmp2)
	a.BCond(HS, exit) // a to-be-closed variable at or above it
	none := a.NewLabel()
	a.Ldr(rState, rCtx, offCtxS)
	a.Ldr(rAddr, rState, offLUpValues)
	a.Cbz(rAddr, none)
	a.Ldr(rAddr, rAddr, offUVIndex) // the highest open upvalue's index
	a.Ldr(rTmp, rState, offStack)
	a.Sub(rTmp2, rTmp2, rTmp)
	a.Lsr(rTmp2, rTmp2, 4) // its index
	a.Cmp(rAddr, rTmp2)
	a.BCond(GE, exit)
	a.Bind(none)
	c.jumpTo(ip, ip+1+i.SBx())
}

// tailSetMeta compiles the TAILCALL i at ip, with the callee's second
// word in rTmp, when the callee is setmetatable, which a constructor's
// return setmetatable(t, mt) calls: setMetaTable, then a return of t.
// Anything else exits, and Go runs the TAILCALL; setting the same
// metatable again is harmless.
func (c *arm64Compiler) tailSetMeta(ip int, i bytecode.Instruction) {
	a := &c.a
	exit, fn := c.exit(ip), reg(i.A())
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, exit)
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, exit)
	a.Ldr(rTmp, rT, 0) // the Function's code
	a.MovImm(rTmp2, callSetMeta)
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, exit)
	c.setMetaTable(i, exit)
	c.returnLua(ip, bytecode.CreateABC(bytecode.OpReturn, i.A()+1, 2, 0))
}

// tailCallLua compiles the TAILCALL i at ip for a compiled, fixed-parameter
// Lua closure as the interpreter's TAILCALL replaces the frame: the callee
// and its arguments move down to the frame's function slot, and the frame,
// its base unchanged, runs the callee. It exits for any other callee, for
// arguments up to l.top, and when upvalues are open in the frame of a
// function with nested functions, for Go to close them first.
func (c *arm64Compiler) tailCallLua(ip int, i bytecode.Instruction) {
	a := &c.a
	ra, b := i.A(), i.B()
	fn := reg(ra)
	exit := c.exit(ip)
	a.Ldr(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkLuaClosure))
	a.Cmp(rTmp, rTmp2)
	if b == 3 { // perhaps return setmetatable(t, mt), out of line
		notLua := a.NewLabel()
		a.BCond(NE, notLua)
		c.outOfLine = append(c.outOfLine, func() {
			a.Bind(notLua)
			c.tailSetMeta(ip, i)
		})
	} else {
		a.BCond(NE, exit)
	}
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, exit) // a number whose bits match the tag
	a.Cbnz(rBarrier, exit)
	a.Ldr(rT2, rT, offClProto)
	a.Ldrb(rTmp, rT2, offPVarKind)
	a.CmpImm(rTmp, uint32(bytecode.VarArgTable))
	a.BCond(EQ, exit) // Go makes the table
	a.Ldr(rCache, rT2, offPJit)
	a.Cbz(rCache, exit)
	a.Ldr(rState, rCtx, offCtxS)
	a.Ldr(rCI, rState, offLCallInfo)
	if len(c.p.Prototypes) > 0 {
		c.exitIfUpValuesOpen(ip)
	}
	a.Ldr(rIdx, rCI, offCIFunction)
	a.Ldr(rStack, rState, offStack)
	// rN: the callee and its arguments, b, or up to l.top for B 0.
	if b == 0 {
		a.Ldr(rN, rState, offLTop)
		a.AddShifted(rN, rStack, rN, 4)
		a.Sub(rN, rN, rFrame)
		a.SubImm(rN, rN, uint32(ra)*valueSize)
		a.Lsr(rN, rN, 4)
	} else {
		a.MovImm(rN, uint64(b))
	}
	// checkStack(p.maxStackSize) with l.top at ci.function + rN, and the
	// parameters for a vararg function: this checks both.
	a.Ldr(rLen, rT2, offPMaxStack)
	a.Ldr(rNext, rState, offLStackLast)
	a.Sub(rNext, rNext, rIdx)
	a.Sub(rNext, rNext, rN)
	a.Ldr(rTmp, rT2, offPParams)
	a.Add(rTmp, rTmp, rLen)
	a.Cmp(rNext, rTmp)
	a.BCond(LE, exit)
	c.spend(ip) // a loop of tail calls has no back-edge
	// Move the callee and its arguments down to stack[ci.function:], one
	// slot below the frame, or further in a vararg function, whose
	// arguments lie between; the callee's frame starts after it, and
	// l.top after its arguments.
	a.AddShifted(rSlot, rStack, rIdx, 4)
	if b != 0 {
		for k := range b {
			src := reg(ra + k)
			a.Ldr(rTmp, rFrame, src.off+offP)
			a.Str(rTmp, rSlot, uint32(k)*valueSize+offP)
			a.Ldr(rTmp, rFrame, src.off+offN)
			a.Str(rTmp, rSlot, uint32(k)*valueSize+offN)
		}
		a.AddImm(rFrame, rSlot, valueSize)
	} else {
		moved, next := a.NewLabel(), a.NewLabel()
		a.AddImm(rTmp2, rFrame, uint32(ra)*valueSize)
		a.AddImm(rFrame, rSlot, valueSize)
		a.Mov(rAddr, rN)
		a.Bind(next)
		a.Cbz(rAddr, moved)
		a.Ldr(rTmp, rTmp2, offP)
		a.Str(rTmp, rSlot, offP)
		a.Ldr(rTmp, rTmp2, offN)
		a.Str(rTmp, rSlot, offN)
		a.AddImm(rTmp2, rTmp2, valueSize)
		a.AddImm(rSlot, rSlot, valueSize)
		a.SubImm(rAddr, rAddr, 1)
		a.B(next)
		a.Bind(moved)
	}
	a.Add(rTmp, rIdx, rN)
	a.Str(rTmp, rState, offLTop)
	// Clear the parameters the call does not pass.
	loop, cleared := a.NewLabel(), a.NewLabel()
	a.Ldr(rNext, rT2, offPParams)
	a.SubImm(rTmp, rN, 1)
	a.Bind(loop)
	a.Cmp(rTmp, rNext)
	a.BCond(GE, cleared)
	a.AddShifted(rTmp2, rFrame, rTmp, 4)
	a.Str(ZR, rTmp2, offP)
	a.Str(ZR, rTmp2, offN)
	a.AddImm(rTmp, rTmp, 1)
	a.B(loop)
	a.Bind(cleared)

	// The frame now runs the callee: its code, closure, top, and frame
	// slice, which keeps its base; it was tail called.
	a.Ldr(rP, rCI, offCILua)
	a.Str(ZR, rP, offLSavedPC)
	for w := uint32(0); w < 24; w += 8 {
		a.Ldr(rTmp2, rT2, offPCode+w)
		a.Str(rTmp2, rP, offLCode+w)
	}
	a.Str(rT, rP, offLClosure)
	varArgs, based := a.NewLabel(), a.NewLabel()
	a.Ldrb(rTmp, rT2, offPVarArg)
	a.Cbnz(rTmp, varArgs)
	a.AddImm(rSlot, rIdx, 1) // base
	a.Bind(based)
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(varArgs) // the frame starts above the arguments
		a.Add(rSlot, rIdx, rN)
		c.adjustVarArgs(rNext)
		a.AddShifted(rFrame, rStack, rSlot, 4)
		a.B(based)
	})
	a.Str(rFrame, rP, offLFrame)
	a.Str(rLen, rP, offLFrame+offSliceLen)
	a.AddShifted(rLen, rSlot, rLen, 0) // top = base + maxStackSize
	a.Str(rLen, rCI, offCITop)
	a.Str(rLen, rState, offLTop)
	a.Ldr(rTmp2, rState, offStack+offSliceCap)
	a.Sub(rTmp2, rTmp2, rSlot)
	a.Str(rTmp2, rP, offLFrame+offSliceCap)
	a.Ldrb(rTmp, rCI, offCIStatus)
	a.MovImm(rTmp2, uint64(callStatusTail))
	a.Orr(rTmp, rTmp, rTmp2)
	a.Strb(rTmp, rCI, offCIStatus)
	a.Strb(ZR, rCI, offCIMeta) // no __call metamethods

	// Enter the callee.
	a.Ldr(rConst, rT2, offPConsts)
	a.Ldr(rUpVals, rT, offClUpVals)
	a.Ldr(rTmp, rCache, offJCEntry)
	a.Br(rTmp)
}

// copyResults copies rSlot values from register ra to rIdx, the caller's
// function slot below, counting in rTmp from 0. It uses rTmp2, rP and rN.
func (c *arm64Compiler) copyResults(ra int) {
	a := &c.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.Mov(rTmp, ZR)
	a.Bind(loop)
	a.Cmp(rTmp, rSlot)
	a.BCond(GE, done)
	a.AddShifted(rTmp2, rFrame, rTmp, 4)
	a.Ldr(rP, rTmp2, uint32(ra)*valueSize+offP)
	a.Ldr(rN, rTmp2, uint32(ra)*valueSize+offN)
	a.AddShifted(rTmp2, rIdx, rTmp, 4)
	a.Str(rP, rTmp2, offP)
	a.Str(rN, rTmp2, offN)
	a.AddImm(rTmp, rTmp, 1)
	a.B(loop)
	a.Bind(done)
}

// returnLua compiles RETURN i at ip to a compiled Lua caller in the same
// interpreter loop: of a fixed number of results or those up to l.top (B
// 0), to a caller that wants a fixed number or all of them (-1), whom it
// leaves l.top after them. Anything else exits.
func (c *arm64Compiler) returnLua(ip int, i bytecode.Instruction) {
	a := &c.a
	ra, b := i.A(), i.B()
	exit := c.exit(ip)
	a.Cbnz(rBarrier, exit)
	a.Ldr(rState, rCtx, offCtxS)
	a.Ldr(rCI, rState, offLCallInfo)
	if len(c.p.Prototypes) > 0 {
		c.exitIfUpValuesOpen(ip)
	}
	a.Ldrb(rTmp, rCI, offCIStatus)
	a.Tbz(rTmp, bitOf(callStatusReentry), exit)
	a.Ldr(rLen, rCI, offCIResults) // wanted, or -1 for all
	// The caller must be compiled at the pc it resumes at.
	a.Ldr(rNext, rCI, offCIPrev)
	a.Ldr(rTmp, rNext, offCILua)
	a.Cbz(rTmp, exit)
	a.Ldr(rT, rTmp, offLClosure)
	a.Ldr(rT2, rT, offClProto)
	a.Ldr(rCache, rT2, offPJit)
	a.Cbz(rCache, exit)
	a.Ldr(rIdx, rTmp, offLSavedPC)
	a.Ldr(rSlot, rCache, offJCOffsets)
	a.AddShifted(rSlot, rSlot, rIdx, 2)
	a.LdrW(rSlot, rSlot, 0)
	a.Tbnz(rSlot, 31, exit)
	a.Ldr(rStack, rCache, offJCBase)
	a.AddShifted(rStack, rStack, rSlot, 0)

	// The results to stack[ci.function:] (rIdx): rSlot of them, from
	// register ra, up to l.top for B 0.
	a.Ldr(rIdx, rCI, offCIFunction)
	a.Ldr(rTmp2, rState, offStack)
	a.AddShifted(rIdx, rTmp2, rIdx, 4)
	if b == 0 {
		a.Ldr(rSlot, rState, offLTop)
		a.AddShifted(rSlot, rTmp2, rSlot, 4)
		a.Sub(rSlot, rSlot, rFrame)
		a.SubImm(rSlot, rSlot, uint32(ra)*valueSize)
		a.Lsr(rSlot, rSlot, 4)
	}
	all, copied, resume := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Tbnz(rLen, 63, all)
	// All of them, with l.top after them: out of line, as callers of
	// fixed results are the most.
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(all)
		if b != 0 {
			a.MovImm(rSlot, uint64(b-1))
		}
		c.copyResults(ra)
		a.Ldr(rTmp2, rCI, offCIFunction)
		a.Add(rTmp2, rTmp2, rSlot)
		a.Str(rTmp2, rState, offLTop)
		a.B(resume)
	})
	// A wanted number: min(rSlot, wanted) results, then nil up to wanted.
	if b != 0 {
		for k := range b - 1 {
			a.CmpImm(rLen, uint32(k))
			a.BCond(LS, copied)
			c.load(reg(ra + k))
			a.Str(rP, rIdx, uint32(k)*valueSize+offP)
			a.Str(rN, rIdx, uint32(k)*valueSize+offN)
		}
		a.Bind(copied)
		a.MovImm(rTmp, uint64(b-1))
	} else {
		a.Cmp(rSlot, rLen)
		a.Csel(rSlot, rLen, rSlot, GT)
		c.copyResults(ra) // leaves rTmp at rSlot
	}
	loop, cleared := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cmp(rTmp, rLen)
	a.BCond(GE, cleared)
	a.AddShifted(rTmp2, rIdx, rTmp, 4)
	a.Str(ZR, rTmp2, offP)
	a.Str(ZR, rTmp2, offN)
	a.AddImm(rTmp, rTmp, 1)
	a.B(loop)
	a.Bind(cleared)
	a.Ldr(rTmp2, rNext, offCITop) // l.top = ci.previous.top
	a.Str(rTmp2, rState, offLTop)

	// l.callInfo = ci.previous; resume the caller.
	a.Bind(resume)
	a.Str(rNext, rState, offLCallInfo)
	a.Ldr(rTmp, rNext, offCILua)
	a.Ldr(rFrame, rTmp, offLFrame)
	a.Ldr(rConst, rT2, offPConsts)
	a.Ldr(rUpVals, rT, offClUpVals)
	a.Br(rStack)
}
