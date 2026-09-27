//go:build (darwin || linux) && arm64

package lua

import (
	"math"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

// Numeric loop kernels on arm64; see jit_kernel.go.

// Kernels keep float registers in D8 to D31 and integer registers in
// X12 to X17 and X19 to X25. D0 to D7, X9 to X11 stay free as
// temporaries.
const (
	kernelFirst FReg = 8
	kernelCount      = 24
	fZero       FReg = 7 // 0.0, for the loop step's sign
)

var kernelInts = []Reg{12, 13, 14, 15, 16, 17, 19, 20, 21, 22, 23, 24, 25}

type kernel struct {
	*irFunc
	exits   []Label // each snapshot's side exit, or -1 until it has one
	counted []Label // likewise, counting short runs: see kernelRuns
	runs    *uint64 // the short runs, when the kernel can leave
}

// reg and ireg return virtual register v's machine register, a float's
// and an integer's.
func (k *kernel) reg(v vreg) FReg { return kernelFirst + FReg(k.machine(v)) }
func (k *kernel) ireg(v vreg) Reg { return kernelInts[k.machine(v)] }

// findKernels returns the kernels for the FORLOOP at latch, an integer
// loop's first, or nil when its loop does not qualify.
func (c *arm64Compiler) findKernels(latch int) []*kernel {
	var fns []uint64
	for _, in := range intrinsics {
		fns = append(fns, in.fn)
	}
	constOK := func(k int) bool { _, ok := c.constant(k); return ok }
	env := newKernelEnv(c.cl, c.frame, constOK, fns, true)
	var ks []*kernel
	var plans []*kernelPlan
	for _, intLoop := range []bool{true, false} {
		if !intLoop && c.p.Code[latch].OpCode() == bytecode.OpJump {
			break // a while loop has no loop registers to be floats
		}
		plans = append(plans, planKernel(c.p, latch, intLoop, env))
		if c.frame == nil { // guesses with nothing to go on: floats too
			env.floats = true
			if alt := planKernel(c.p, latch, intLoop, env); alt != nil && !alt.sameTypes(plans[len(plans)-1]) {
				plans = append(plans, alt)
			}
			env.floats = false
		}
	}
	for _, plan := range plans {
		if plan == nil {
			continue
		}
		f := buildIR(c.p, plan)
		if !f.worksBetweenCalls() || !f.allocate(kernelCount, len(kernelInts)) {
			continue
		}
		k := &kernel{irFunc: f, exits: make([]Label, len(f.snaps)), counted: make([]Label, len(f.snaps))}
		for i := range k.exits {
			k.exits[i], k.counted[i] = -1, -1
		}
		if f.leaves {
			k.runs = new(uint64)
			c.p.jitRuns = append(c.p.jitRuns, k.runs)
		}
		for _, kc := range f.calls {
			if kc.closure != nil {
				c.p.jitKeep = append(c.p.jitKeep, kc.closure)
			}
		}
		ks = append(ks, k)
	}
	return ks
}

// emitKernel compiles k at the FORLOOP's pc. It falls through to normal,
// the next kernel or the ordinary FORLOOP code, when the entry check
// fails.
func (c *arm64Compiler) emitKernel(k *kernel, normal Label) {
	a := &c.a

	// Entry: the write barrier is off, the calls and buffers are what the
	// kernel was compiled for, and the live-in registers hold numbers of
	// their types.
	a.Cbnz(rBarrier, normal)
	if k.runs != nil {
		a.MovImm(rTmp, uint64(uintptr(unsafe.Pointer(k.runs))))
		a.Ldr(rTmp, rTmp, 0)
		a.CmpImm(rTmp, kernelRunsOff)
		a.BCond(HS, normal)
	}
	c.kernelGuards(k, normal)
	c.kernelLoad(k, k.entryRegs(), normal)
	a.FmovToF(fZero, ZR)
	c.countKernel(k)
	if k.runs != nil {
		a.Str(rBudget, rCtx, offEntry)
	}
	body, latch, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	if !k.while { // the first FORLOOP: nothing has changed if the loop does not run
		c.kernelStep(k, body, c.pcs[k.latch+1])
	}

	a.Bind(body)
	labels := map[int]Label{k.latch: latch}
	label := func(pc int) Label {
		l, ok := labels[pc]
		if !ok {
			l = a.NewLabel()
			labels[pc] = l
		}
		return l
	}
	for x := range k.insts {
		if in := &k.insts[x]; in.op == irLabel {
			a.Bind(label(in.target))
		} else {
			loads, store := k.spills(in)
			for _, v := range loads {
				c.loadVreg(k, v, reg(k.vregs[v].r))
			}
			c.kernelInstruction(k, in, label)
			if store != noVreg {
				c.storeVreg(k, store, reg(k.vregs[store].r))
			}
		}
	}
	for _, pc := range k.resumes {
		c.kernelResume(k, pc+1, label)
	}
	a.Bind(latch)
	next := a.NewLabel()
	if !k.while { // a while loop leaves by its tests: its JMP back only spends budget
		c.kernelStep(k, next, done)
	}
	a.Bind(next)
	// Spend budget; when it runs out, write back and resume at the body,
	// as the interpreter would after this FORLOOP.
	out := a.NewLabel()
	a.SubsImm(rBudget, rBudget, 1)
	a.BCond(EQ, out)
	a.B(body)
	a.Bind(out)
	end := &k.snaps[k.end]
	c.flush(k, end)
	if c.budget[k.start] < 0 {
		c.budget[k.start] = a.NewLabel()
	}
	a.B(c.budget[k.start])
	if !k.while {
		a.Bind(done)
		c.flush(k, end)
		a.B(c.pcs[k.latch+1])
	}
}

// kernelGuards branches to normal unless each upvalue k calls holds its
// intrinsic and each register k indexes holds a buffer, of floats if k
// reads it.
func (c *arm64Compiler) kernelGuards(k *kernel, normal Label) {
	a := &c.a
	for _, n := range k.guardedUpValues() {
		c.upValueAddr(n)
		fn, math, closure := k.upValueFn(n)
		if closure != nil { // a Lua function to inline: that one
			a.Ldr(rTmp, rAddr, offN)
			a.MovImm(rTmp2, tagOf(vkLuaClosure))
			a.Cmp(rTmp, rTmp2)
			a.BCond(NE, normal)
			a.Ldr(rTmp, rAddr, offP)
			a.MovImm(rTmp2, uint64(uintptr(unsafe.Pointer(closure))))
			a.Cmp(rTmp, rTmp2)
			a.BCond(NE, normal)
			continue
		}
		a.Ldr(rTmp, rAddr, offN)
		a.MovImm(rTmp2, tagOf(vkGoFunction))
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, normal)
		a.Ldr(rTmp, rAddr, offP)
		c.branchNumber(rTmp, normal) // a number whose bits match the tag
		if math {
			a.Ldr(rTmp, rTmp, 0) // the Function's code
		} else {
			a.Ldr(rTmp, rTmp, offGFNumber)
			a.Cbz(rTmp, normal)
			a.Ldr(rTmp, rTmp, offNFUnary)
		}
		a.MovImm(rTmp2, fn)
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, normal)
	}
	for _, r := range k.bufferRegs() {
		o := reg(r)
		a.Ldr(rTmp, o.base, o.off+offN)
		a.MovImm(rTmp2, tagOf(vkUserData))
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, normal)
		a.Ldr(rTmp, o.base, o.off+offP)
		c.branchNumber(rTmp, normal)
		a.Ldr(rTmp, rTmp, offUDBuf)
		a.Cbz(rTmp, normal)
		if k.buffers[r] { // read: floats only
			a.Ldrb(rTmp, rTmp, offBufKind)
			a.CmpImm(rTmp, uint32(bufferFloat32))
			a.BCond(HI, normal)
		}
	}
	// Upvalues the body reads, into jitContext.hoist: a number's bits, or
	// a buffer's *buffer.
	for s, h := range k.hoisted {
		c.upValueAddr(h.n)
		if h.kind == kindBuffer {
			a.Ldr(rTmp, rAddr, offN)
			a.MovImm(rTmp2, tagOf(vkUserData))
			a.Cmp(rTmp, rTmp2)
			a.BCond(NE, normal)
			a.Ldr(rTmp, rAddr, offP)
			c.branchNumber(rTmp, normal)
			a.Ldr(rTmp, rTmp, offUDBuf)
			a.Cbz(rTmp, normal)
			if h.read {
				a.Ldrb(rTmp2, rTmp, offBufKind)
				a.CmpImm(rTmp2, uint32(bufferFloat32))
				a.BCond(HI, normal)
			}
		} else if h.kind == kindTable {
			a.Ldr(rTmp, rAddr, offN)
			a.MovImm(rTmp2, tagOf(vkTable))
			a.Cmp(rTmp, rTmp2)
			a.BCond(NE, normal)
			a.Ldr(rTmp, rAddr, offP)
			c.branchNumber(rTmp, normal)
		} else {
			a.Ldr(rTmp, rAddr, offP)
			if h.kind == kindInt {
				a.Cmp(rTmp, rInteger)
			} else {
				a.Cmp(rTmp, rNumber)
			}
			a.BCond(NE, normal)
			a.Ldr(rTmp, rAddr, offN)
		}
		a.Str(rTmp, rCtx, offHoist+uint32(s)*8)
	}
}

// kernelSideExit returns a label that leaves k by snapshot n: it writes
// k's registers back, and the ordinary code runs the instruction and the
// rest of the iteration.
func (c *arm64Compiler) kernelSideExit(k *kernel, n int, counted bool) Label {
	cache := k.exits
	if counted {
		cache = k.counted
	}
	if cache[n] >= 0 {
		return cache[n]
	}
	a := &c.a
	l := a.NewLabel()
	cache[n] = l
	c.outOfLine = append(c.outOfLine, func() {
		s := &k.snaps[n]
		a.Bind(l)
		c.flush(k, s)
		if counted {
			c.kernelRuns(k)
		}
		for _, kc := range s.callees { // after the flush: it uses kernel registers
			c.upValueAddr(kc.upValue)
			c.load(operand{rAddr, 0})
			c.store(reg(kc.a))
		}
		if s.top >= 0 {
			c.setTop(s.top)
		}
		a.B(c.ordinaryAt(s.pc))
	})
	return l
}

// kernelRuns counts a short run of k, or clears the count after a long
// one; see kernelShortRun.
func (c *arm64Compiler) kernelRuns(k *kernel) {
	a := &c.a
	long, done := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, rCtx, offEntry)
	a.Sub(rTmp, rTmp, rBudget) // the iterations since it started
	a.MovImm(rTmp2, uint64(uintptr(unsafe.Pointer(k.runs))))
	a.CmpImm(rTmp, kernelShortRun)
	a.BCond(HS, long)
	a.Ldr(rTmp, rTmp2, 0)
	a.AddImm(rTmp, rTmp, 1)
	a.Str(rTmp, rTmp2, 0)
	a.B(done)
	a.Bind(long)
	a.Str(ZR, rTmp2, 0)
	a.Bind(done)
}

// intrinsicSaved are the kernel registers sin and cos use.
var intrinsicSaved = []Reg{rTrig, rIdx, rLen}

// kernelCall compiles in, a number function's intrinsic: on the argument
// in D0, saving the kernel registers the intrinsic's code uses. The
// intrinsic's own exits leave the kernel with register A holding the
// function again, as the ordinary CALL expects.
func (c *arm64Compiler) kernelCall(k *kernel, in *irInst) {
	a := &c.a
	var saved []Reg
	fn := uint64(in.imm)
	if fn != funcValue(math.Sqrt) { // sqrt is one instruction
		for _, r := range intrinsicSaved {
			if k.usesInt(r) {
				saved = append(saved, r)
			}
		}
	}
	save, restore := func() {
		for j, r := range saved {
			a.Str(r, rCtx, offSpill+uint32(j)*8)
		}
	}, func() {
		for j, r := range saved {
			a.Ldr(r, rCtx, offSpill+uint32(j)*8)
		}
	}
	if x := c.floatArg(k, in.a, 0); x != 0 {
		a.Fmov(0, x)
	}
	save()
	side, exit := a.NewLabel(), c.kernelSideExit(k, in.snap, false)
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(side)
		restore()
		a.B(exit)
	})
	c.kernelExit = side
	for _, it := range intrinsics {
		if it.fn == fn {
			it.emit(c, in.pc)
		}
	}
	c.kernelExit = -1
	restore()
	a.Fmov(k.reg(in.dst), 0)
}

// kernelMath compiles in, a math function on registers, typed as
// callResult says. floor or ceil of a float leaves the kernel when the
// result holds no integer, for Go to give the float.
func (c *arm64Compiler) kernelMath(k *kernel, in *irInst) {
	a := &c.a
	m := mathFn(in.imm)
	if in.a.t == kindInt {
		x, d := k.ireg(in.a.v), k.ireg(in.dst)
		switch m {
		case mathFloor, mathCeil:
			a.Mov(d, x)
		case mathAbs:
			a.Neg(rTmp, x) // minint stays minint
			a.CmpImm(x, 0)
			a.Csel(d, rTmp, x, LT)
		case mathMin, mathMax:
			b := k.ireg(in.b.v)
			if m == mathMin {
				a.Cmp(b, x) // the second if it is less
			} else {
				a.Cmp(x, b) // the second if the first is less
			}
			a.Csel(d, b, x, LT)
		}
		return
	}
	x := k.reg(in.a.v)
	switch m {
	case mathFloor, mathCeil:
		if m == mathFloor {
			a.Frintm(0, x)
		} else {
			a.Frintp(0, x)
		}
		// An integer when -2^63 <= f < 2^63; otherwise Go gives the float.
		side := c.kernelSideExit(k, in.snap, false)
		a.MovImm(rTmp, math.Float64bits(1<<63))
		a.FmovToF(1, rTmp)
		a.Fcmp(0, 1)
		a.BCond(VS, side) // NaN
		a.BCond(GE, side)
		a.MovImm(rTmp, math.Float64bits(-(1 << 63)))
		a.FmovToF(1, rTmp)
		a.Fcmp(0, 1)
		a.BCond(MI, side)
		a.Fcvtzs(k.ireg(in.dst), 0)
	case mathAbs:
		a.Fabs(k.reg(in.dst), x)
	case mathMin, mathMax:
		keep, b := a.NewLabel(), k.reg(in.b.v)
		a.Fmov(0, x)
		if m == mathMin {
			a.Fcmp(b, x)
		} else {
			a.Fcmp(x, b)
		}
		a.BCond(PL, keep) // Lua's <: not less, or unordered
		a.Fmov(0, b)
		a.Bind(keep)
		a.Fmov(k.reg(in.dst), 0)
	}
}

// kernelBuffer checks that in's key is inside the buffer it accesses,
// branching to side otherwise, and leaves the address of its first
// element in rTmp2 and its kind in rTmp. It returns the register holding
// the key: its kernel register, or rExitPC for a constant or a float.
func (c *arm64Compiler) kernelBuffer(k *kernel, in *irInst, side Label) Reg {
	a := &c.a
	keyReg := rExitPC
	switch key := in.a; {
	case key.isConst():
		a.MovImm(rExitPC, uint64(c.p.Constants[key.k].i()))
	case key.t == kindFloat: // leave unless it has an integer value
		x := k.reg(key.v)
		a.Fcvtzs(rExitPC, x)
		a.Scvtf(0, rExitPC)
		a.Fcmp(0, x)
		a.BCond(NE, side) // NaN too
	default:
		keyReg = k.ireg(key.v)
	}
	if in.buf >= 0 {
		a.Ldr(rTmp, rCtx, offHoist+uint32(in.buf)*8) // the *buffer
	} else {
		a.Ldr(rTmp, rFrame, reg(in.obj).off+offP) // the userdata
		a.Ldr(rTmp, rTmp, offUDBuf)
	}
	a.Ldr(rTmp2, rTmp, offBufLen)
	a.Cmp(keyReg, rTmp2)
	a.BCond(HS, side) // unsigned: below 0 too
	a.Ldr(rTmp2, rTmp, offBufPtr)
	a.Ldrb(rTmp, rTmp, offBufKind)
	return keyReg
}

// kernelGetBuffer compiles in, a buffer of floats' element.
func (c *arm64Compiler) kernelGetBuffer(k *kernel, in *irInst) {
	a := &c.a
	key := c.kernelBuffer(k, in, c.kernelSideExit(k, in.snap, false))
	d := k.reg(in.dst)
	f32, done := a.NewLabel(), a.NewLabel()
	a.Cbnz(rTmp, f32)
	a.AddShifted(rTmp2, rTmp2, key, 3)
	a.LdrD(d, rTmp2, 0)
	a.B(done)
	a.Bind(f32)
	a.AddShifted(rTmp2, rTmp2, key, 2)
	a.LdrS(d, rTmp2, 0)
	a.FcvtSD(d, d)
	a.Bind(done)
}

// kernelSetBuffer compiles in, a store to a buffer's element, converting
// as putBuffer does. An integer buffer given a float leaves the kernel,
// for Go to check it has an integer value.
func (c *arm64Compiler) kernelSetBuffer(k *kernel, in *irInst) {
	a := &c.a
	side := c.kernelSideExit(k, in.snap, false)
	key := c.kernelBuffer(k, in, side)
	val := in.b
	var constant value
	if val.isConst() {
		constant = c.p.Constants[val.k]
	}
	notF64, notF32, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Cbnz(rTmp, notF64)
	a.AddShifted(rTmp2, rTmp2, key, 3)
	switch {
	case constant.isNumber():
		a.MovImm(rTmp, math.Float64bits(constant.toFloat()))
		a.Str(rTmp, rTmp2, 0)
	case val.t == kindInt:
		a.Scvtf(0, k.ireg(val.v))
		a.StrD(0, rTmp2, 0)
	default:
		a.StrD(k.reg(val.v), rTmp2, 0)
	}
	a.B(done)
	a.Bind(notF64)
	a.CmpImm(rTmp, uint32(bufferFloat32))
	a.BCond(NE, notF32)
	a.AddShifted(rTmp2, rTmp2, key, 2)
	switch {
	case constant.isNumber():
		a.MovImm(rTmp, uint64(math.Float32bits(float32(constant.toFloat()))))
		a.StrW(rTmp, rTmp2, 0)
	case val.t == kindInt:
		a.Scvtf(0, k.ireg(val.v))
		a.FcvtDS(0, 0)
		a.StrS(0, rTmp2, 0)
	default:
		a.FcvtDS(0, k.reg(val.v))
		a.StrS(0, rTmp2, 0)
	}
	a.B(done)
	a.Bind(notF32) // an integer buffer: an integer value, or Go converts
	if val.t != kindInt {
		a.B(side)
	} else {
		isU8 := a.NewLabel()
		a.CmpImm(rTmp, uint32(bufferUint8))
		a.BCond(EQ, isU8)
		r := rTmp
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		} else {
			r = k.ireg(val.v)
		}
		a.AddShifted(rTmp2, rTmp2, key, 2)
		a.StrW(r, rTmp2, 0)
		a.B(done)
		a.Bind(isU8)
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		}
		a.AddShifted(rTmp2, rTmp2, key, 0)
		a.Strb(r, rTmp2, 0)
	}
	a.Bind(done)
}

// usesInt reports whether k keeps a virtual register in machine register
// r.
func (k *kernel) usesInt(r Reg) bool {
	for v, s := range k.vregs {
		if s.t == kindInt && k.loc[v] >= 0 && kernelInts[k.loc[v]] == r {
			return true
		}
	}
	return false
}

// kernelStep is FORLOOP on registers: it advances the index and branches
// to take, having set the external index, or to end.
func (c *arm64Compiler) kernelStep(k *kernel, take, end Label) {
	a := &c.a
	if k.intLoop {
		idx, count, step, ext := k.ireg(k.loop[0]), k.ireg(k.loop[1]), k.ireg(k.loop[2]), k.ireg(k.loop[3])
		a.Cbz(count, end)
		a.SubImm(count, count, 1)
		a.Add(idx, idx, step)
		if ext != idx {
			a.Mov(ext, idx)
		}
		a.B(take)
		return
	}
	idx, limit, step, ext := k.reg(k.loop[0]), k.reg(k.loop[1]), k.reg(k.loop[2]), k.reg(k.loop[3])
	positive, notPositive, yes := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Fadd(0, idx, step)
	a.Fcmp(step, fZero)
	a.BCond(GT, positive)
	a.BCond(LS, notPositive)
	a.B(end) // NaN step
	a.Bind(notPositive)
	a.Fcmp(limit, 0)
	a.BCond(LS, yes)
	a.B(end)
	a.Bind(positive)
	a.Fcmp(0, limit)
	a.BCond(LS, yes)
	a.B(end)
	a.Bind(yes)
	a.Fmov(idx, 0)
	if ext != idx {
		a.Fmov(ext, 0)
	}
	a.B(take)
}

// flush writes snapshot s's registers back to the frame.
func (c *arm64Compiler) flush(k *kernel, s *irSnap) {
	for _, r := range s.regs {
		if r.v != noVreg && k.spilled(r.v) {
			continue // in its stack slot already
		}
		if isRegKind(r.t) {
			c.storeVreg(k, r.v, reg(r.r))
		}
	}
	// A register aliasing a hoisted buffer gets the upvalue's value, as
	// the ordinary code expects. This uses kernel registers, so comes last.
	for _, r := range s.regs {
		if r.v == noVreg {
			c.upValueAddr(k.hoisted[r.alias].n)
			c.load(operand{rAddr, 0})
			c.store(reg(r.r))
		}
	}
}

// floatArg returns a floating-point register holding x as a float: its
// kernel register, or tmp, into which it loads a constant or converts an
// integer.
func (c *arm64Compiler) floatArg(k *kernel, x irArg, tmp FReg) FReg {
	a := &c.a
	if !x.isConst() {
		if x.t == kindInt {
			a.Scvtf(tmp, k.ireg(x.v))
			return tmp
		}
		return k.reg(x.v)
	}
	if v := x.constant(c.p); v.isInteger() || x.k == litK {
		a.MovImm(rTmp, math.Float64bits(v.toFloat()))
		a.FmovToF(tmp, rTmp)
		return tmp
	}
	o, _ := c.constant(x.k)
	a.LdrD(tmp, o.base, o.off+offN)
	return tmp
}

// intArg returns a general-purpose register holding the integer x: its
// kernel register, or tmp, into which it loads a constant.
func (c *arm64Compiler) intArg(k *kernel, x irArg, tmp Reg) Reg {
	if !x.isConst() {
		return k.ireg(x.v)
	}
	if x.k == litK {
		c.a.MovImm(tmp, uint64(x.lit.i()))
		return tmp
	}
	o, _ := c.constant(x.k)
	c.a.Ldr(tmp, o.base, o.off+offN)
	return tmp
}

// kernelInstruction lowers the operation in; label returns a body pc's
// label.
func (c *arm64Compiler) kernelInstruction(k *kernel, in *irInst, label func(int) Label) {
	a := &c.a
	p := c.p
	isInt := in.dst != noVreg && k.isInt(in.dst)
	switch in.op {
	case irMove:
		if isInt {
			a.Mov(k.ireg(in.dst), k.ireg(in.a.v))
		} else {
			a.Fmov(k.reg(in.dst), k.reg(in.a.v))
		}
	case irConst:
		if v := in.a.constant(p); in.a.k == litK && isInt {
			a.MovImm(k.ireg(in.dst), uint64(v.i()))
			break
		} else if in.a.k == litK {
			a.MovImm(rTmp, math.Float64bits(v.toFloat()))
			a.FmovToF(k.reg(in.dst), rTmp)
			break
		}
		o, _ := c.constant(in.a.k)
		switch v := p.Constants[in.a.k]; {
		case isInt:
			a.Ldr(k.ireg(in.dst), o.base, o.off+offN)
		case v.isInteger(): // promoted: see promoteConstants
			a.MovImm(rTmp, math.Float64bits(float64(v.i())))
			a.FmovToF(k.reg(in.dst), rTmp)
		default:
			a.LdrD(k.reg(in.dst), o.base, o.off+offN)
		}
	case irAdd, irSub, irMul, irDiv:
		if isInt {
			b, cc, d := c.intArg(k, in.a, rTmp), c.intArg(k, in.b, rTmp2), k.ireg(in.dst)
			switch in.op {
			case irAdd:
				a.Add(d, b, cc)
			case irSub:
				a.Sub(d, b, cc)
			case irMul:
				a.Mul(d, b, cc)
			}
			break
		}
		b, cc, d := c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1), k.reg(in.dst)
		switch in.op {
		case irAdd:
			a.Fadd(d, b, cc)
		case irSub:
			a.Fsub(d, b, cc)
		case irMul:
			a.Fmul(d, b, cc)
		case irDiv:
			a.Fdiv(d, b, cc)
		}
	case irAnd, irOr, irXor, irNot, irShift:
		b, d := c.intArg(k, in.a, rTmp), k.ireg(in.dst)
		switch in.op {
		case irAnd:
			a.And(d, b, c.intArg(k, in.b, rTmp2))
		case irOr:
			a.Orr(d, b, c.intArg(k, in.b, rTmp2))
		case irXor:
			a.Eor(d, b, c.intArg(k, in.b, rTmp2))
		case irNot:
			a.Mvn(d, b)
		case irShift:
			switch count := in.imm; {
			case in.flag:
				a.Mov(d, ZR)
			case count >= 0:
				a.MovImm(rTmp2, uint64(count))
				a.Lslv(d, b, rTmp2)
			default:
				a.MovImm(rTmp2, uint64(-count))
				a.Lsrv(d, b, rTmp2)
			}
		}
	case irFloorDiv: // floor(b / c)
		b, cc, d := c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1), k.reg(in.dst)
		a.Fdiv(d, b, cc)
		a.Frintm(d, d)
	case irFloatMod: // by a floatModDivisor
		if b := c.floatArg(k, in.a, 0); b != 0 {
			a.Fmov(0, b)
		}
		c.floatMod(math.Float64frombits(uint64(in.imm)))
		a.Fmov(k.reg(in.dst), 3)
	case irIntDiv, irIntMod:
		// By a nonzero constant: floor the quotient toward minus infinity,
		// and give the modulo the divisor's sign.
		op := bytecode.OpIDiv
		if in.op == irIntMod {
			op = bytecode.OpMod
		}
		divisor := in.imm
		b, d := k.ireg(in.a.v), k.ireg(in.dst)
		if plan, ok := planDivide(divisor); ok {
			a.Mov(d, c.divideByConstant(op, plan, b))
			break
		}
		a.MovImm(rTmp2, uint64(divisor))
		a.Sdiv(rTmp, b, rTmp2)          // toward zero; minint / -1 wraps
		a.Msub(rExitPC, rTmp, rTmp2, b) // the remainder, with b's sign
		adjust := a.NewLabel()
		a.Cbz(rExitPC, adjust)
		if divisor > 0 {
			a.Tbz(rExitPC, 63, adjust)
		} else {
			a.Tbnz(rExitPC, 63, adjust)
		}
		if op == bytecode.OpMod {
			a.Add(rExitPC, rExitPC, rTmp2)
		} else {
			a.SubImm(rTmp, rTmp, 1)
		}
		a.Bind(adjust)
		if op == bytecode.OpMod {
			a.Mov(d, rExitPC)
		} else {
			a.Mov(d, rTmp)
		}
	case irNeg:
		if isInt {
			a.Neg(k.ireg(in.dst), k.ireg(in.a.v))
		} else {
			a.Fneg(k.reg(in.dst), k.reg(in.a.v))
		}
	case irBranch:
		if in.cmp == bytecode.OpTest { // of a boolean: to target when it is flag
			var yes Label
			if in.target >= 0 {
				yes = label(in.target)
			} else {
				yes = c.kernelSideExit(k, in.snap, false)
			}
			if in.flag {
				a.Cbnz(k.ireg(in.a.v), yes)
			} else {
				a.Cbz(k.ireg(in.a.v), yes)
			}
			a.B(label(in.pc + 2))
			break
		}
		var when Cond
		if in.a.t == kindInt && in.b.t == kindInt {
			a.Cmp(c.intArg(k, in.a, rTmp), c.intArg(k, in.b, rTmp2))
			when = map[bytecode.OpCode]Cond{bytecode.OpEqual: EQ, bytecode.OpLessThan: LT, bytecode.OpLessOrEqual: LE}[in.cmp]
		} else {
			a.Fcmp(c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1))
			when = map[bytecode.OpCode]Cond{bytecode.OpEqual: EQ, bytecode.OpLessThan: MI, bytecode.OpLessOrEqual: LS}[in.cmp]
		}
		if !in.flag {
			when = negate(when)
		}
		var yes Label
		if in.target >= 0 {
			yes = label(in.target)
		} else {
			yes = c.kernelSideExit(k, in.snap, false) // it ends the loop: no short run
		}
		a.BCond(when, yes)
		a.B(label(in.pc + 2))
	case irJump:
		a.B(label(in.target))
	case irExit: // a call's is not a short run: the kernel resumes after it
		a.B(c.kernelSideExit(k, in.snap, !in.flag))
	case irHoist: // a number, from the context
		if isInt {
			a.Ldr(k.ireg(in.dst), rCtx, offHoist+uint32(in.imm)*8)
		} else {
			a.LdrD(k.reg(in.dst), rCtx, offHoist+uint32(in.imm)*8)
		}
	case irIntrinsic:
		c.kernelCall(k, in)
	case irMath:
		c.kernelMath(k, in)
	case irBufGet:
		c.kernelGetBuffer(k, in)
	case irBufSet:
		c.kernelSetBuffer(k, in)
	case irBoolNot:
		a.MovImm(rTmp, 1)
		a.Eor(k.ireg(in.dst), k.ireg(in.a.v), rTmp)
	case irCopyUp:
		c.kernelUpValue(int(in.imm))
		c.copyToReg(in.obj, rTmp, 0)
	case irCopy:
		src := reg(int(in.imm))
		c.copyToReg(in.obj, src.base, src.off)
	case irArrayGet, irFieldGet:
		exit := c.kernelSideExit(k, in.snap, true)
		c.tableSlot(k, in, exit)
		c.tableValue(k, in, exit)
	case irArraySet, irFieldSet:
		exit, t := c.kernelSideExit(k, in.snap, true), k.ireg(in.a.v)
		c.tableSlot(k, in, exit)
		a.Ldr(rTmp2, rTmp, offP)
		if in.op == irFieldSet { // over a value only
			a.Cbz(rTmp2, exit)
		} else { // over a value, or nil in a table without a metatable
			present := a.NewLabel()
			a.Cbnz(rTmp2, present)
			a.Ldr(rTmp2, t, offTMeta)
			a.Cbnz(rTmp2, exit)
			a.Bind(present)
		}
		c.tableStore(k, in)
		if in.op == irFieldSet {
			a.Strb(ZR, t, offTFlags) // invalidateTagMethodCache
		}
	}
}

// storeTable stores the table r points to at dst.
func (c *arm64Compiler) storeTable(dst operand, r Reg) {
	c.a.MovImm(rTmp, tagOf(vkTable))
	c.a.Str(rTmp, dst.base, dst.off+offN)
	c.a.Str(r, dst.base, dst.off+offP)
}

// tableSlot leaves in rTmp the address of the value the table operation
// in reads or writes: the array element at key b, or the own field the
// fieldCache at in.pc names. It branches to exit when there is none, and
// uses rTmp2 and rExitPC.
func (c *arm64Compiler) tableSlot(k *kernel, in *irInst, exit Label) {
	a := &c.a
	var t Reg
	if in.buf < 0 {
		t = k.ireg(in.a.v)
	}
	if in.op == irArrayGet || in.op == irArraySet {
		if in.b.isConst() {
			a.MovImm(rTmp, uint64(c.p.Constants[in.b.k].i()-1))
		} else {
			a.SubImm(rTmp, k.ireg(in.b.v), 1)
		}
		a.Ldr(rTmp2, t, offTArray+offSliceLen)
		a.Cmp(rTmp, rTmp2)
		a.BCond(HS, exit) // unsigned: keys below 1 too
		a.Ldr(rTmp2, t, offTArray)
		a.AddShifted(rTmp, rTmp2, rTmp, 4)
		return
	}
	// A hoisted table is loaded from the context at each use, as the
	// field needs all three scratch registers.
	table := func(dst Reg) Reg {
		if in.buf < 0 {
			return t
		}
		a.Ldr(dst, rCtx, offHoist+uint32(in.buf)*8)
		return dst
	}
	a.Ldr(rTmp, table(rTmp), offTShape)
	a.Cbz(rTmp, exit)
	a.MovImm(rTmp2, uint64(uintptr(unsafe.Pointer(&c.p.fields[in.pc]))))
	a.Ldr(rExitPC, rTmp2, offCShape)
	a.Cmp(rTmp, rExitPC)
	a.BCond(NE, exit)
	a.Ldrsw(rExitPC, rTmp2, offCSlot)
	a.Ldr(rTmp, table(rTmp), offTSlots+offSliceLen)
	a.Cmp(rExitPC, rTmp)
	a.BCond(HS, exit) // unsigned: a slot below 0, not the table's own, too
	a.Ldr(rTmp2, table(rTmp2), offTSlots)
	a.AddShifted(rTmp, rTmp2, rExitPC, 4)
}

// tableValue loads the value at rTmp into in's result, branching to exit
// unless it has the result's type. It uses rTmp2 and rExitPC.
func (c *arm64Compiler) tableValue(k *kernel, in *irInst, exit Label) {
	a := &c.a
	if in.dst == noVreg { // any value but nil, to the stack
		a.Ldr(rTmp2, rTmp, offP)
		a.Cbz(rTmp2, exit)
		c.copyToReg(in.obj, rTmp, 0)
		return
	}
	switch k.typeOf(in.dst) {
	case kindFloat:
		a.Ldr(rTmp2, rTmp, offP)
		a.Cmp(rTmp2, rNumber)
		a.BCond(NE, exit)
		a.LdrD(k.reg(in.dst), rTmp, offN)
	case kindInt:
		a.Ldr(rTmp2, rTmp, offP)
		a.Cmp(rTmp2, rInteger)
		a.BCond(NE, exit)
		a.Ldr(k.ireg(in.dst), rTmp, offN)
	case kindBool:
		a.Ldr(rTmp2, rTmp, offP)
		a.Cmp(rTmp2, rBool)
		a.BCond(NE, exit)
		a.Ldr(rTmp2, rTmp, offN)
		a.MovImm(rExitPC, 1)
		a.And(k.ireg(in.dst), rTmp2, rExitPC)
	case kindTable:
		a.Ldr(rTmp2, rTmp, offN)
		a.MovImm(rExitPC, tagOf(vkTable))
		a.Cmp(rTmp2, rExitPC)
		a.BCond(NE, exit)
		a.Ldr(rExitPC, rTmp, offP)
		c.branchNumber(rExitPC, exit) // a number whose bits match the tag
		a.Mov(k.ireg(in.dst), rExitPC)
	}
}

// tableStore stores in's value, c, at rTmp. It uses rTmp2.
func (c *arm64Compiler) tableStore(k *kernel, in *irInst) {
	a := &c.a
	v := in.c
	t := v.t
	if t == kindBool {
		if v.isConst() {
			bit, _ := v.constant(c.p).boolean()
			a.MovImm(rTmp2, tagOf(vkBool)|uint64(b2i(bit)))
		} else {
			a.MovImm(rTmp2, tagOf(vkBool))
			a.Orr(rTmp2, rTmp2, k.ireg(v.v))
		}
		a.Str(rTmp2, rTmp, offN)
		a.Str(rBool, rTmp, offP)
		return
	}
	switch {
	case v.isConst():
		n := c.p.Constants[v.k]
		if t == kindFloat {
			a.MovImm(rTmp2, math.Float64bits(n.toFloat()))
		} else {
			a.MovImm(rTmp2, uint64(n.i()))
		}
		a.Str(rTmp2, rTmp, offN)
	case t == kindFloat:
		a.StrD(k.reg(v.v), rTmp, offN)
	case t == kindInt:
		a.Str(k.ireg(v.v), rTmp, offN)
	case t == kindTable:
		a.MovImm(rTmp2, tagOf(vkTable))
		a.Str(rTmp2, rTmp, offN)
		a.Str(k.ireg(v.v), rTmp, offP)
		return
	}
	if t == kindFloat {
		a.Str(rNumber, rTmp, offP)
	} else {
		a.Str(rInteger, rTmp, offP)
	}
}

// kernelLoad checks that the Lua register of each of vs holds a value of
// its type, branching to fail if not, then loads those not spilled.
func (c *arm64Compiler) kernelLoad(k *kernel, vs []vreg, fail Label) {
	for _, v := range vs {
		c.checkVreg(k, v, reg(k.vregs[v].r), fail)
	}
	for _, v := range vs {
		if !k.spilled(v) { // else in its stack slot already
			c.loadVreg(k, v, reg(k.vregs[v].r))
		}
	}
}

// kernelResume makes the code compiled code enters at pc, after a call k
// leaves at, go on in k: it checks what k's entry does and that the
// registers live at pc hold values of their types, and loads them. When a
// check fails it counts a short run and goes on in the ordinary code.
func (c *arm64Compiler) kernelResume(k *kernel, pc int, label func(int) Label) {
	if _, ok := c.resume[pc]; ok { // another kernel resumes there
		return
	}
	a := &c.a
	l := a.NewLabel()
	c.resume[pc] = l
	c.outOfLine = append(c.outOfLine, func() {
		fail, ordinary := a.NewLabel(), c.ordinaryAt(pc)
		a.Bind(l)
		a.Cbnz(rBarrier, ordinary)
		a.MovImm(rTmp2, uint64(uintptr(unsafe.Pointer(k.runs))))
		a.Ldr(rTmp, rTmp2, 0)
		a.CmpImm(rTmp, kernelRunsOff)
		a.BCond(HS, ordinary)
		c.kernelGuards(k, fail)
		c.kernelLoad(k, k.resumeRegs(pc), fail)
		c.countKernel(k)
		a.FmovToF(fZero, ZR)
		a.Str(rBudget, rCtx, offEntry)
		a.B(label(pc))
		a.Bind(fail)
		a.MovImm(rTmp2, uint64(uintptr(unsafe.Pointer(k.runs))))
		a.Ldr(rTmp, rTmp2, 0)
		a.AddImm(rTmp, rTmp, 1)
		a.Str(rTmp, rTmp2, 0)
		a.B(ordinary)
	})
}

// kernelUpValue leaves in rTmp the address of upvalue n's value, using
// rTmp2 but no kernel register, as upValueAddr does not.
func (c *arm64Compiler) kernelUpValue(n int) {
	a := &c.a
	closed, done := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, rUpVals, uint32(n)*8) // *upValue
	a.Ldr(rTmp2, rTmp, offUVState)
	a.Cbz(rTmp2, closed)
	a.Ldr(rTmp2, rTmp2, offStack) // &state.stack[0]
	a.Ldr(rTmp, rTmp, offUVIndex) // index
	a.AddShifted(rTmp, rTmp2, rTmp, 4)
	a.B(done)
	a.Bind(closed)
	a.AddImm(rTmp, rTmp, offUVClosed)
	a.Bind(done)
}

// copyToReg copies the value at src+off to register dst, using rTmp2.
func (c *arm64Compiler) copyToReg(dst int, src Reg, off uint32) {
	a := &c.a
	o := reg(dst)
	a.Ldr(rTmp2, src, off+offP)
	a.Str(rTmp2, o.base, o.off+offP)
	a.Ldr(rTmp2, src, off+offN)
	a.Str(rTmp2, o.base, o.off+offN)
}

// countKernel counts an entry to k, for tests.
func (c *arm64Compiler) countKernel(k *kernel) {
	a := &c.a
	counter := offKernels
	if k.intLoop {
		counter += 8
	}
	a.Ldr(rTmp, rCtx, counter)
	a.AddImm(rTmp, rTmp, 1)
	a.Str(rTmp, rCtx, counter)
}

// checkVreg branches to fail unless the value at o has v's type. It uses
// rTmp and rExitPC.
func (c *arm64Compiler) checkVreg(k *kernel, v vreg, o operand, fail Label) {
	a := &c.a
	switch t := k.typeOf(v); t {
	case kindTable:
		a.Ldr(rTmp, o.base, o.off+offN)
		a.MovImm(rExitPC, tagOf(vkTable))
		a.Cmp(rTmp, rExitPC)
		a.BCond(NE, fail)
		a.Ldr(rTmp, o.base, o.off+offP)
		c.branchNumber(rTmp, fail) // a number whose bits match the tag
	case kindBool:
		a.Ldr(rTmp, o.base, o.off+offP)
		a.Cmp(rTmp, rBool)
		a.BCond(NE, fail)
	default:
		a.Ldr(rTmp, o.base, o.off+offP)
		if t == kindInt {
			a.Cmp(rTmp, rInteger)
		} else {
			a.Cmp(rTmp, rNumber)
		}
		a.BCond(NE, fail)
	}
}

// loadVreg loads v from the value at o, which has its type.
func (c *arm64Compiler) loadVreg(k *kernel, v vreg, o operand) {
	a := &c.a
	switch k.typeOf(v) {
	case kindTable:
		a.Ldr(k.ireg(v), o.base, o.off+offP)
	case kindInt:
		a.Ldr(k.ireg(v), o.base, o.off+offN)
	case kindBool:
		a.Ldr(k.ireg(v), o.base, o.off+offN)
		a.MovImm(rTmp, 1)
		a.And(k.ireg(v), k.ireg(v), rTmp)
	default:
		a.LdrD(k.reg(v), o.base, o.off+offN)
	}
}

// storeVreg stores v as a value at o.
func (c *arm64Compiler) storeVreg(k *kernel, v vreg, o operand) {
	a := &c.a
	switch k.typeOf(v) {
	case kindTable:
		c.storeTable(o, k.ireg(v))
	case kindInt:
		c.storeInteger(o, k.ireg(v))
	case kindBool:
		a.MovImm(rTmp, tagOf(vkBool))
		a.Orr(rTmp, rTmp, k.ireg(v))
		a.Str(rTmp, o.base, o.off+offN)
		a.Str(rBool, o.base, o.off+offP)
	default:
		c.storeNumber(o, k.reg(v))
	}
}

// ordinaryAt returns the label of the ordinary code for pc, past any
// kernels that start there: a while loop's kernel leaves at its start.
func (c *arm64Compiler) ordinaryAt(pc int) Label {
	if l, ok := c.ordinary[pc]; ok {
		return l
	}
	return c.pcs[pc]
}
