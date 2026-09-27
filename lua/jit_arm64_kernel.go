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
	intrinsic := func(n int) (uint64, mathFn, bool) {
		if fn, ok := upValueIntrinsic(c.cl, n, fns); ok {
			return fn, mathNone, true
		}
		m, fn := mathFnOf(c.cl, n)
		return fn, m, m != mathNone
	}
	upValue := func(n int) (numKind, bool) { return upValueKind(c.cl, n) }
	var ks []*kernel
	for _, intLoop := range []bool{true, false} {
		plan := planKernel(c.p, latch, intLoop, constOK, intrinsic, upValue, true)
		if plan == nil {
			continue
		}
		f := buildIR(c.p, plan)
		if !f.allocate(kernelCount, len(kernelInts)) {
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
	for _, r := range k.liveIn {
		a.Ldr(rTmp, rFrame, reg(r).off+offP)
		if k.types[r] == kindInt {
			a.Cmp(rTmp, rInteger)
		} else {
			a.Cmp(rTmp, rNumber)
		}
		a.BCond(NE, normal)
	}
	for _, r := range k.liveIn {
		if v := k.value(r, k.types[r]); k.spilled(v) {
			continue // in its stack slot already
		} else if k.types[r] == kindInt {
			a.Ldr(k.ireg(v), rFrame, reg(r).off+offN)
		} else {
			a.LdrD(k.reg(v), rFrame, reg(r).off+offN)
		}
	}
	a.FmovToF(fZero, ZR)
	counter := offKernels
	if k.intLoop {
		counter += 8
	}
	a.Ldr(rTmp, rCtx, counter)
	a.AddImm(rTmp, rTmp, 1)
	a.Str(rTmp, rCtx, counter)
	if k.runs != nil {
		a.Str(rBudget, rCtx, offEntry)
	}
	body, latch, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	// The first FORLOOP: nothing has changed if the loop does not run.
	c.kernelStep(k, body, c.pcs[k.latch+1])

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
				if r := reg(k.vregs[v].r); k.typeOf(v) == kindInt {
					a.Ldr(k.ireg(v), r.base, r.off+offN)
				} else {
					a.LdrD(k.reg(v), r.base, r.off+offN)
				}
			}
			c.kernelInstruction(k, in, label)
			if store != noVreg {
				if r := reg(k.vregs[store].r); k.typeOf(store) == kindInt {
					c.storeInteger(r, k.ireg(store))
				} else {
					c.storeNumber(r, k.reg(store))
				}
			}
		}
	}
	a.Bind(latch)
	next := a.NewLabel()
	c.kernelStep(k, next, done)
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
	a.Bind(done)
	c.flush(k, end)
	a.B(c.pcs[k.latch+1])
}

// kernelGuards branches to normal unless each upvalue k calls holds its
// intrinsic and each register k indexes holds a buffer, of floats if k
// reads it.
func (c *arm64Compiler) kernelGuards(k *kernel, normal Label) {
	a := &c.a
	for _, n := range k.guardedUpValues() {
		c.upValueAddr(n)
		a.Ldr(rTmp, rAddr, offN)
		a.MovImm(rTmp2, tagOf(vkGoFunction))
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, normal)
		a.Ldr(rTmp, rAddr, offP)
		c.branchNumber(rTmp, normal) // a number whose bits match the tag
		fn, math := k.upValueFn(n)
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
		a.B(c.pcs[s.pc])
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
		switch r.t {
		case kindInt:
			c.storeInteger(reg(r.r), k.ireg(r.v))
		case kindFloat:
			c.storeNumber(reg(r.r), k.reg(r.v))
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
	if v := c.p.Constants[x.k]; v.isInteger() {
		a.MovImm(rTmp, math.Float64bits(float64(v.i())))
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
	o, _ := c.constant(x.k)
	c.a.Ldr(tmp, o.base, o.off+offN)
	return tmp
}

// kernelInstruction lowers the operation in; label returns a body pc's
// label.
func (c *arm64Compiler) kernelInstruction(k *kernel, in *irInst, label func(int) Label) {
	a := &c.a
	p := c.p
	isInt := in.dst != noVreg && k.typeOf(in.dst) == kindInt
	switch in.op {
	case irMove:
		if isInt {
			a.Mov(k.ireg(in.dst), k.ireg(in.a.v))
		} else {
			a.Fmov(k.reg(in.dst), k.reg(in.a.v))
		}
	case irConst:
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
			yes = c.kernelSideExit(k, in.snap, true)
		}
		a.BCond(when, yes)
		a.B(label(in.pc + 2))
	case irJump:
		a.B(label(in.target))
	case irExit:
		a.B(c.kernelSideExit(k, in.snap, true))
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
	}
}
