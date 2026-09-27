//go:build (darwin || linux) && amd64

package lua

import (
	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/amd64"
)

// Lua-to-Lua calls and returns between compiled functions on amd64; see
// jit_arm64_calls.go. With fewer registers, values are reloaded from the
// structures they live in rather than kept.

// Lua compiles a call whose last argument is another call as that call
// with all its results (C 0), which leaves l.top after them, then this
// call with arguments up to l.top (B 0); a RETURN of a call's results
// likewise (B 0). Compiled code keeps l.top only between the two, as
// the interpreter does.

// checkArgs branches to fail unless the CALL i has n arguments: up to
// l.top when it has B 0. It uses rTmp and rTmp2.
func (c *amd64Compiler) checkArgs(i bytecode.Instruction, n int, fail Label) {
	a := &c.a
	if i.B() != 0 {
		if i.B()-1 != n {
			a.Jmp(fail)
		}
		return
	}
	a.Load(rTmp, rCtx, offCtxS)
	a.Load(rTmp2, rTmp, offStack)
	a.Load(rTmp, rTmp, offLTop)
	a.Shl(rTmp, 4)
	a.Add(rTmp, rTmp2) // &stack[l.top]
	a.Mov(rTmp2, rFrame)
	a.AddImm(rTmp2, int32(uint32(i.A()+1+n)*valueSize))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, fail)
}

// resultTop, for the CALL i when it wants all results (C 0), leaves l.top
// after the one result it has stored in register A. It uses rTmp, rTmp2
// and rAddr.
func (c *amd64Compiler) resultTop(i bytecode.Instruction) {
	if i.C() == 0 {
		c.setTop(i.A() + 1)
	}
}

// setTop sets l.top to register r, using rTmp, rTmp2 and rAddr.
func (c *amd64Compiler) setTop(r int) {
	a := &c.a
	a.Load(rTmp, rCtx, offCtxS)
	a.Load(rAddr, rTmp, offStack)
	a.Mov(rTmp2, rFrame)
	a.AddImm(rTmp2, int32(uint32(r)*valueSize))
	a.Sub(rTmp2, rAddr)
	a.Shr(rTmp2, 4)
	a.Store(rTmp, offLTop, rTmp2)
}

// callLua compiles the CALL i at ip for a compiled, fixed-parameter Lua
// closure. It jumps to notLua when the callee is not a Lua closure, and
// exits for any other Lua closure. Arguments may run up to l.top (B 0),
// and the callee may return all its results (C 0).
func (c *amd64Compiler) callLua(ip int, i bytecode.Instruction, notLua Label) {
	a := &c.a
	ra, b, results := i.A(), i.B(), i.C()-1
	exit := c.exit(ip)
	fn := reg(ra)
	a.Load(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkLuaClosure))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, notLua)
	a.Load(R10, fn.base, fn.off+offP) // closure
	c.branchNumber(R10, exit)         // a number whose bits match the tag
	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, exit)
	a.Load(R11, R10, offClProto) // prototype
	a.Load8(AX, R11, offPVarArg)
	a.Test(AX, AX)
	a.J(NE, exit)
	a.Load(AX, R11, offPJit)
	a.Test(AX, AX)
	a.J(E, exit)
	a.Load(R12, rCtx, offCtxS) // state
	// function := ci.stackIndex(a); l.top = function + 1 + argCount.
	a.Mov(R13, rFrame)
	a.AddImm(R13, int32(uint32(ra)*valueSize))
	a.Load(AX, R12, offStack)
	a.Sub(R13, AX)
	a.Shr(R13, 4) // function
	if b == 0 {
		a.Load(DX, R12, offLTop) // where the call before left it
	} else {
		a.Mov(DX, R13)
		a.AddImm(DX, int32(b)) // l.top
	}
	// checkStack(p.maxStackSize) would grow the stack.
	a.Load(CX, R12, offLStackLast)
	a.Sub(CX, DX)
	a.Load(AX, R11, offPMaxStack)
	a.Cmp(CX, AX)
	a.J(LE, exit)
	// pushLuaFrame reuses l.callInfo.next, which must have Lua storage.
	a.Load(R8, R12, offLCallInfo) // ci
	a.Load(R9, R8, offCINext)     // the callee's callInfo
	a.Test(R9, R9)
	a.J(E, exit)
	a.Load(DX, R9, offCILua) // its luaCallInfo
	a.Test(DX, DX)
	a.J(E, exit)
	c.spend(ip)

	// Clear the parameters the call does not pass.
	loop, cleared := a.NewLabel(), a.NewLabel()
	if b == 0 { // first missing: &stack[l.top]
		a.Load(AX, R12, offLTop)
		a.Shl(AX, 4)
		a.Load(CX, R12, offStack)
		a.Add(AX, CX)
	} else {
		a.Mov(AX, rFrame)
		a.AddImm(AX, int32(uint32(ra+b)*valueSize))
	}
	a.Load(CX, R11, offPParams)
	a.Shl(CX, 4)
	a.Add(CX, rFrame)
	a.AddImm(CX, int32(uint32(ra+1)*valueSize)) // end
	a.Bind(loop)
	a.Cmp(AX, CX)
	a.J(AE, cleared)
	a.StoreZero(AX, offP)
	a.StoreZero(AX, offN)
	a.AddImm(AX, int32(valueSize))
	a.Jmp(loop)
	a.Bind(cleared)

	// The caller resumes after the call.
	a.Load(AX, R8, offCILua)
	a.MovImm(CX, uint64(ip+1))
	a.Store(AX, offLSavedPC, CX)

	// pushLuaFrame(function, function+1, results, closure), then
	// setCallStatus(callStatusReentry).
	a.StoreZero(DX, offLSavedPC)
	for w := uint32(0); w < 24; w += 8 {
		a.Load(AX, R11, offPCode+w)
		a.Store(DX, offLCode+w, AX)
	}
	a.Store(DX, offLClosure, R10)
	a.Store(R9, offCIFunction, R13)
	a.AddImm(R13, 1) // base
	a.Load(AX, R11, offPMaxStack)
	a.Add(AX, R13) // top
	a.Store(R9, offCITop, AX)
	a.MovImm(CX, uint64(int64(results)))
	a.Store(R9, offCIResults, CX)
	a.MovImm(CX, uint64(callStatusLua|callStatusReentry))
	a.Store8(R9, offCIStatus, CX)
	a.StoreZero8(R9, offCIMeta) // no __call metamethods
	// frame = l.stack[base:top]
	a.Load(CX, R12, offStack)
	a.Mov(R8, R13)
	a.Shl(R8, 4)
	a.Add(R8, CX)
	a.Store(DX, offLFrame, R8)
	a.Mov(CX, AX)
	a.Sub(CX, R13)
	a.Store(DX, offLFrame+offSliceLen, CX)
	a.Load(CX, R12, offStack+offSliceCap)
	a.Sub(CX, R13)
	a.Store(DX, offLFrame+offSliceCap, CX)
	a.Store(R12, offLCallInfo, R9)
	a.Store(R12, offLTop, AX)

	// Enter the callee.
	a.Mov(rFrame, R8)
	a.Load(rConst, R11, offPConsts)
	a.Load(AX, R10, offClUpVals)
	a.Store(rCtx, offUpValues, AX)
	a.Load(AX, R11, offPJit)
	a.Load(AX, AX, offJCEntry)
	a.JmpReg(AX)
}

// exitIfUpValuesOpen exits at ip if any upvalue is open at or above the base
// of the frame whose callInfo is in R8, of the state in R12: a function with
// nested functions returns or tail calls in compiled code only when there
// are none for Go to close. The open upvalues are sorted highest first. It
// uses AX and CX.
func (c *amd64Compiler) exitIfUpValuesOpen(ip int) {
	a := &c.a
	none := a.NewLabel()
	a.Load(AX, R12, offLUpValues)
	a.Test(AX, AX)
	a.J(E, none)
	a.Load(AX, AX, offUVIndex)
	a.Load(CX, R8, offCIFunction)
	a.Cmp(AX, CX)
	a.J(G, c.exit(ip)) // index >= base, which is function + 1
	a.Bind(none)
}

// closeJump compiles JMP A sBx with A > 0, which closes upvalues and
// to-be-closed variables from register A-1 up, as a generic for's exit
// does: it jumps when nothing there is open, and exits for Go to close
// what is.
func (c *amd64Compiler) closeJump(ip int, i bytecode.Instruction) {
	a := &c.a
	exit := c.exit(ip)
	level := reg(i.A() - 1)
	a.Mov(rTmp2, level.base)
	a.AddImm(rTmp2, int32(level.off)) // its address
	a.Load(rTmp, rCtx, offCtxTBC)
	a.Cmp(rTmp, rTmp2)
	a.J(AE, exit) // a to-be-closed variable at or above it
	none := a.NewLabel()
	a.Load(rTmp, rCtx, offCtxS)
	a.Load(rAddr, rTmp, offLUpValues)
	a.Test(rAddr, rAddr)
	a.J(E, none)
	a.Load(rAddr, rAddr, offUVIndex) // the highest open upvalue's index
	a.Load(rTmp, rTmp, offStack)
	a.Sub(rTmp2, rTmp)
	a.Shr(rTmp2, 4) // its index
	a.Cmp(rAddr, rTmp2)
	a.J(GE, exit)
	a.Bind(none)
	c.jumpTo(ip, ip+1+i.SBx())
}

// tailCallLua compiles the TAILCALL i at ip for a compiled, fixed-parameter
// Lua closure as the interpreter's TAILCALL replaces the frame: the callee
// and its arguments move down to the frame's function slot, and the frame,
// its base unchanged, runs the callee. It exits for any other callee, for
// arguments up to l.top, and when upvalues are open in the frame of a
// function with nested functions, for Go to close them first.
func (c *amd64Compiler) tailCallLua(ip int, i bytecode.Instruction) {
	a := &c.a
	ra, b := i.A(), i.B()
	if b == 0 {
		c.exitAlways(ip)
		return
	}
	exit := c.exit(ip)
	fn := reg(ra)
	a.Load(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkLuaClosure))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, exit)
	a.Load(R10, fn.base, fn.off+offP) // closure
	c.branchNumber(R10, exit)         // a number whose bits match the tag
	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, exit)
	a.Load(R11, R10, offClProto) // prototype
	a.Load8(AX, R11, offPVarArg)
	a.Test(AX, AX)
	a.J(NE, exit)
	a.Load(AX, R11, offPJit)
	a.Test(AX, AX)
	a.J(E, exit)
	a.Load(R12, rCtx, offCtxS)    // state
	a.Load(R8, R12, offLCallInfo) // ci
	if len(c.p.Prototypes) > 0 {
		c.exitIfUpValuesOpen(ip)
	}
	// checkStack(p.maxStackSize) with l.top at ci.function + b.
	a.Load(CX, R12, offLStackLast)
	a.Load(DX, R8, offCIFunction)
	a.Sub(CX, DX)
	a.SubImm(CX, int32(b))
	a.Load(AX, R11, offPMaxStack)
	a.Cmp(CX, AX)
	a.J(LE, exit)
	c.spend(ip) // a loop of tail calls has no back-edge
	// Move the callee and its arguments down to stack[ci.function:], one
	// slot below the frame.
	a.Mov(DX, rFrame)
	a.SubImm(DX, int32(valueSize))
	for k := range b {
		src := reg(ra + k)
		a.Load(AX, rFrame, src.off+offP)
		a.Store(DX, uint32(k)*valueSize+offP, AX)
		a.Load(AX, rFrame, src.off+offN)
		a.Store(DX, uint32(k)*valueSize+offN, AX)
	}
	// Clear the parameters the call does not pass.
	loop, cleared := a.NewLabel(), a.NewLabel()
	a.Load(CX, R11, offPParams)
	a.Shl(CX, 4)
	a.Add(CX, rFrame) // end
	a.Mov(AX, rFrame)
	a.AddImm(AX, int32(uint32(b-1)*valueSize)) // first missing
	a.Bind(loop)
	a.Cmp(AX, CX)
	a.J(AE, cleared)
	a.StoreZero(AX, offP)
	a.StoreZero(AX, offN)
	a.AddImm(AX, int32(valueSize))
	a.Jmp(loop)
	a.Bind(cleared)

	// The frame now runs the callee: its code, closure, top, and frame
	// slice, which keeps its base; it was tail called.
	a.Load(DX, R8, offCILua)
	a.StoreZero(DX, offLSavedPC)
	for w := uint32(0); w < 24; w += 8 {
		a.Load(AX, R11, offPCode+w)
		a.Store(DX, offLCode+w, AX)
	}
	a.Store(DX, offLClosure, R10)
	a.Load(R13, R8, offCIFunction)
	a.AddImm(R13, 1) // base
	a.Load(AX, R11, offPMaxStack)
	a.Store(DX, offLFrame+offSliceLen, AX)
	a.Add(AX, R13) // top
	a.Store(R8, offCITop, AX)
	a.Store(R12, offLTop, AX)
	a.Load(CX, R12, offStack+offSliceCap)
	a.Sub(CX, R13)
	a.Store(DX, offLFrame+offSliceCap, CX)
	a.Load8(AX, R8, offCIStatus)
	a.MovImm(CX, uint64(callStatusTail))
	a.Or(AX, CX)
	a.Store8(R8, offCIStatus, AX)
	a.StoreZero8(R8, offCIMeta) // no __call metamethods

	// Enter the callee.
	a.Load(rConst, R11, offPConsts)
	a.Load(AX, R10, offClUpVals)
	a.Store(rCtx, offUpValues, AX)
	a.Load(AX, R11, offPJit)
	a.Load(AX, AX, offJCEntry)
	a.JmpReg(AX)
}

// copyResults copies R13 values from register ra to R10, the caller's
// function slot below, counting in R11 from 0. It uses AX and DX.
func (c *amd64Compiler) copyResults(ra int) {
	a := &c.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.MovImm(R11, 0)
	a.Bind(loop)
	a.Cmp(R11, R13)
	a.J(GE, done)
	for _, off := range []uint32{offP, offN} {
		a.Mov(AX, R11)
		a.Shl(AX, 4)
		a.Add(AX, rFrame)
		a.Load(DX, AX, uint32(ra)*valueSize+off)
		a.Mov(AX, R11)
		a.Shl(AX, 4)
		a.Add(AX, R10)
		a.Store(AX, off, DX)
	}
	a.AddImm(R11, 1)
	a.Jmp(loop)
	a.Bind(done)
}

// returnLua compiles RETURN i at ip to a compiled Lua caller: of a fixed
// number of results or those up to l.top (B 0), to a caller that wants a
// fixed number or all of them (-1), whom it leaves l.top after them.
// Anything else exits.
func (c *amd64Compiler) returnLua(ip int, i bytecode.Instruction) {
	a := &c.a
	ra, b := i.A(), i.B()
	exit := c.exit(ip)
	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, exit)
	a.Load(R12, rCtx, offCtxS)    // state
	a.Load(R8, R12, offLCallInfo) // ci
	if len(c.p.Prototypes) > 0 {
		c.exitIfUpValuesOpen(ip)
	}
	a.Load8(AX, R8, offCIStatus)
	a.Bt(AX, uint8(bitOf(callStatusReentry)))
	a.J(AE, exit)
	a.Load(CX, R8, offCIResults) // wanted, or -1 for all
	// The caller must be compiled at the pc it resumes at.
	a.Load(R9, R8, offCIPrev)
	a.Load(DX, R9, offCILua)
	a.Test(DX, DX)
	a.J(E, exit)
	a.Load(R10, DX, offLClosure)
	a.Load(R11, R10, offClProto)
	a.Load(R13, R11, offPJit)
	a.Test(R13, R13)
	a.J(E, exit)
	a.Load(AX, DX, offLSavedPC)
	a.Shl(AX, 2)
	a.Load(R10, R13, offJCOffsets)
	a.Add(AX, R10)
	a.Load32(AX, AX, 0)
	a.Bt(AX, 31)
	a.J(B, exit)
	a.Load(R10, R13, offJCBase)
	a.Add(AX, R10)
	a.Store(rCtx, offTarget, AX)

	// The results to stack[ci.function:] (R10): R13 of them, from register
	// ra, up to l.top for B 0.
	a.Load(R10, R8, offCIFunction)
	a.Shl(R10, 4)
	a.Load(R11, R12, offStack)
	a.Add(R10, R11)
	if b == 0 {
		a.Load(R13, R12, offLTop)
		a.Shl(R13, 4)
		a.Add(R13, R11)
		a.Sub(R13, rFrame)
		a.SubImm(R13, int32(uint32(ra)*valueSize))
		a.Shr(R13, 4)
	}
	all, copied, resume := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bt(CX, 63)
	a.J(B, all)
	// All of them, with l.top after them: out of line, as callers of
	// fixed results are the most.
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(all)
		if b != 0 {
			a.MovImm(R13, uint64(b-1))
		}
		c.copyResults(ra)
		a.Load(AX, R8, offCIFunction)
		a.Add(AX, R13)
		a.Store(R12, offLTop, AX)
		a.Jmp(resume)
	})
	// A wanted number: min(R13, wanted) results, then nil up to wanted.
	if b != 0 {
		for k := range b - 1 {
			a.CmpImm(CX, int32(k))
			a.J(BE, copied)
			src := reg(ra + k)
			a.Load(AX, rFrame, src.off+offP)
			a.Store(R10, uint32(k)*valueSize+offP, AX)
			a.Load(AX, rFrame, src.off+offN)
			a.Store(R10, uint32(k)*valueSize+offN, AX)
		}
		a.Bind(copied)
		a.MovImm(R11, uint64(b-1))
	} else {
		fewer := a.NewLabel()
		a.Cmp(R13, CX)
		a.J(LE, fewer)
		a.Mov(R13, CX)
		a.Bind(fewer)
		c.copyResults(ra) // leaves R11 at R13
		a.Bind(copied)
	}
	loop, cleared := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cmp(R11, CX)
	a.J(GE, cleared)
	a.Mov(AX, R11)
	a.Shl(AX, 4)
	a.Add(AX, R10)
	a.StoreZero(AX, offP)
	a.StoreZero(AX, offN)
	a.AddImm(R11, 1)
	a.Jmp(loop)
	a.Bind(cleared)
	a.Load(AX, R9, offCITop) // l.top = ci.previous.top
	a.Store(R12, offLTop, AX)

	// l.callInfo = ci.previous; resume the caller.
	a.Bind(resume)
	a.Store(R12, offLCallInfo, R9)
	a.Load(DX, R9, offCILua)
	a.Load(rFrame, DX, offLFrame)
	a.Load(R10, DX, offLClosure)
	a.Load(R11, R10, offClProto)
	a.Load(rConst, R11, offPConsts)
	a.Load(AX, R10, offClUpVals)
	a.Store(rCtx, offUpValues, AX)
	a.Load(AX, rCtx, offTarget)
	a.JmpReg(AX)
}
