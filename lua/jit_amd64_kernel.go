//go:build (darwin || linux) && amd64

package lua

import (
	"math"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/amd64"
)

// Numeric loop kernels on amd64; see jit_kernel.go.

// Kernels keep float registers in X6 to X14 (X15 is Go's zero register)
// and integer registers in R8 to R13 and CX. X0 to X4, AX and DX stay free
// as temporaries: IDIV divides RDX:RAX.
const (
	kernelFirst XReg = 6
	kernelCount      = 9
)

var kernelInts = []Reg{R8, R9, R10, R11, R12, R13, CX}

type kernel struct {
	*irFunc
	exits   []Label // each snapshot's side exit, or -1 until it has one
	counted []Label // likewise, counting short runs: see kernelRuns
	runs    *uint64 // the short runs, when the kernel can leave
}

// reg and ireg return virtual register v's machine register, a float's
// and an integer's.
func (k *kernel) reg(v vreg) XReg { return kernelFirst + XReg(k.machine(v)) }
func (k *kernel) ireg(v vreg) Reg { return kernelInts[k.machine(v)] }

// findKernels returns the kernels for the FORLOOP at latch, an integer
// loop's first, or nil when its loop does not qualify.
func (c *amd64Compiler) findKernels(latch int) []*kernel {
	var fns []uint64
	for _, in := range c.intrinsics() {
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
	observed := func(r int) numKind {
		if r >= len(c.frame) {
			return kindAny
		}
		switch v := c.frame[r]; {
		case v.isFloat():
			return kindFloat
		case v.isInteger():
			return kindInt
		case v.table() != nil:
			return kindTable
		case v.userData() != nil && v.userData().buf != nil:
			return kindBuffer
		}
		return kindAny
	}
	var ks []*kernel
	for _, intLoop := range []bool{true, false} {
		plan := planKernel(c.p, latch, intLoop, constOK, intrinsic, upValue, c.sse41, observed)
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

// emitKernel compiles k at the FORLOOP's pc, jumping to normal, the next
// kernel or the ordinary FORLOOP code, when the entry check fails.
func (c *amd64Compiler) emitKernel(k *kernel, normal Label) {
	a := &c.a

	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, normal)
	if k.runs != nil {
		a.MovImm(rTmp, uint64(uintptr(unsafe.Pointer(k.runs))))
		a.CmpMem(rTmp, 0, kernelRunsOff)
		a.J(AE, normal)
	}
	c.kernelGuards(k, normal)
	for _, r := range k.liveIn {
		if k.types[r] == kindTable {
			a.Load(rTmp, rFrame, reg(r).off+offN)
			a.MovImm(rTmp2, tagOf(vkTable))
			a.Cmp(rTmp, rTmp2)
			a.J(NE, normal)
			a.Load(rTmp, rFrame, reg(r).off+offP)
			c.branchNumber(rTmp, normal) // a number whose bits match the tag
			continue
		}
		a.Load(rTmp, rFrame, reg(r).off+offP)
		a.Sub(rTmp, rNumber) // 0 for a float, 1 for an integer
		if k.types[r] == kindInt {
			a.CmpImm(rTmp, 1)
		} else {
			a.Test(rTmp, rTmp)
		}
		a.J(NE, normal)
	}
	for _, r := range k.liveIn {
		if v := k.value(r, k.types[r]); k.spilled(v) {
			continue // in its stack slot already
		} else if k.types[r] == kindTable {
			a.Load(k.ireg(v), rFrame, reg(r).off+offP)
		} else if k.types[r] == kindInt {
			a.Load(k.ireg(v), rFrame, reg(r).off+offN)
		} else {
			a.LoadSD(k.reg(v), rFrame, reg(r).off+offN)
		}
	}
	counter := offKernels
	if k.intLoop {
		counter += 8
	}
	a.SubMem(rCtx, counter, -1)
	if k.runs != nil {
		a.Load(rTmp, rCtx, offBudget)
		a.Store(rCtx, offEntry, rTmp)
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
				if r := reg(k.vregs[v].r); k.typeOf(v) == kindTable {
					a.Load(k.ireg(v), r.base, r.off+offP)
				} else if k.typeOf(v) == kindInt {
					a.Load(k.ireg(v), r.base, r.off+offN)
				} else {
					a.LoadSD(k.reg(v), r.base, r.off+offN)
				}
			}
			c.kernelInstruction(k, in, label)
			if store != noVreg {
				if r := reg(k.vregs[store].r); k.typeOf(store) == kindTable {
					c.storeTable(r, k.ireg(store))
				} else if k.typeOf(store) == kindInt {
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
	out := a.NewLabel()
	a.SubMem(rCtx, offBudget, 1)
	a.J(E, out)
	a.Jmp(body)
	a.Bind(out)
	end := &k.snaps[k.end]
	c.flush(k, end)
	if c.budget[k.start] < 0 {
		c.budget[k.start] = a.NewLabel()
	}
	a.Jmp(c.budget[k.start])
	a.Bind(done)
	c.flush(k, end)
	a.Jmp(c.pcs[k.latch+1])
}

// kernelGuards jumps to normal unless each upvalue k calls holds its
// intrinsic and each register k indexes holds a buffer, of floats if k
// reads it.
func (c *amd64Compiler) kernelGuards(k *kernel, normal Label) {
	a := &c.a
	for _, n := range k.guardedUpValues() {
		c.upValueAddr(n)
		a.Load(rTmp, rAddr, offN)
		a.MovImm(rTmp2, tagOf(vkGoFunction))
		a.Cmp(rTmp, rTmp2)
		a.J(NE, normal)
		a.Load(rTmp, rAddr, offP)
		c.branchNumber(rTmp, normal) // a number whose bits match the tag
		fn, math := k.upValueFn(n)
		if math {
			a.Load(rTmp, rTmp, 0) // the Function's code
		} else {
			a.Load(rTmp, rTmp, offGFNumber)
			a.Test(rTmp, rTmp)
			a.J(E, normal)
			a.Load(rTmp, rTmp, offNFUnary)
		}
		a.MovImm(rTmp2, fn)
		a.Cmp(rTmp, rTmp2)
		a.J(NE, normal)
	}
	for _, r := range k.bufferRegs() {
		o := reg(r)
		a.Load(rTmp, o.base, o.off+offN)
		a.MovImm(rTmp2, tagOf(vkUserData))
		a.Cmp(rTmp, rTmp2)
		a.J(NE, normal)
		a.Load(rTmp, o.base, o.off+offP)
		c.branchNumber(rTmp, normal)
		a.Load(rTmp, rTmp, offUDBuf)
		a.Test(rTmp, rTmp)
		a.J(E, normal)
		if k.buffers[r] { // read: floats only
			a.Load8(rTmp, rTmp, offBufKind)
			a.CmpImm(rTmp, int32(bufferFloat32))
			a.J(A, normal)
		}
	}
	// Upvalues the body reads, into jitContext.hoist: a number's bits, or
	// a buffer's *buffer.
	for s, h := range k.hoisted {
		c.upValueAddr(h.n)
		if h.kind == kindBuffer {
			a.Load(rTmp, rAddr, offN)
			a.MovImm(rTmp2, tagOf(vkUserData))
			a.Cmp(rTmp, rTmp2)
			a.J(NE, normal)
			a.Load(rTmp, rAddr, offP)
			c.branchNumber(rTmp, normal)
			a.Load(rTmp, rTmp, offUDBuf)
			a.Test(rTmp, rTmp)
			a.J(E, normal)
			if h.read {
				a.Load8(rTmp2, rTmp, offBufKind)
				a.CmpImm(rTmp2, int32(bufferFloat32))
				a.J(A, normal)
			}
		} else if h.kind == kindTable {
			a.Load(rTmp, rAddr, offN)
			a.MovImm(rTmp2, tagOf(vkTable))
			a.Cmp(rTmp, rTmp2)
			a.J(NE, normal)
			a.Load(rTmp, rAddr, offP)
			c.branchNumber(rTmp, normal)
		} else {
			a.Load(rTmp, rAddr, offP)
			a.Sub(rTmp, rNumber) // 0 for a float, 1 for an integer
			if h.kind == kindInt {
				a.CmpImm(rTmp, 1)
			} else {
				a.Test(rTmp, rTmp)
			}
			a.J(NE, normal)
			a.Load(rTmp, rAddr, offN)
		}
		a.Store(rCtx, offHoist+uint32(s)*8, rTmp)
	}
}

// kernelSideExit returns a label that leaves k by snapshot n: it writes
// k's registers back, and the ordinary code runs the instruction and the
// rest of the iteration.
func (c *amd64Compiler) kernelSideExit(k *kernel, n int, counted bool) Label {
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
		a.Jmp(c.pcs[s.pc])
	})
	return l
}

// kernelRuns counts a short run of k, or clears the count after a long
// one; see kernelShortRun.
func (c *amd64Compiler) kernelRuns(k *kernel) {
	a := &c.a
	long, done := a.NewLabel(), a.NewLabel()
	a.Load(rTmp, rCtx, offEntry)
	a.Load(DX, rCtx, offBudget)
	a.Sub(rTmp, DX) // the iterations since it started
	a.MovImm(DX, uint64(uintptr(unsafe.Pointer(k.runs))))
	a.CmpImm(rTmp, kernelShortRun)
	a.J(AE, long)
	a.SubMem(DX, 0, -1)
	a.Jmp(done)
	a.Bind(long)
	a.StoreZero(DX, 0)
	a.Bind(done)
}

// intrinsicSaved are the kernel registers sin and cos use.
var intrinsicSaved = []Reg{rN, rT2, rIdx}

// kernelCall compiles in, a number function's intrinsic: on the argument
// in X0, saving the kernel registers the intrinsic's code uses. The
// intrinsic's own exits leave the kernel with register A holding the
// function again, as the ordinary CALL expects.
func (c *amd64Compiler) kernelCall(k *kernel, in *irInst) {
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
			a.Store(rCtx, offSpill+uint32(j)*8, r)
		}
	}, func() {
		for j, r := range saved {
			a.Load(r, rCtx, offSpill+uint32(j)*8)
		}
	}
	if x := c.floatArg(k, in.a, 0); x != 0 {
		a.MovSD(0, x)
	}
	save()
	side, exit := a.NewLabel(), c.kernelSideExit(k, in.snap, false)
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(side)
		restore()
		a.Jmp(exit)
	})
	c.kernelExit = side
	c.ip = in.pc
	for _, in := range c.intrinsics() {
		if in.fn == fn {
			in.emit()
		}
	}
	c.kernelExit = -1
	restore()
	a.MovSD(k.reg(in.dst), 0)
}

// kernelMath compiles in, a math function on registers, typed as
// callResult says. floor or ceil of a float leaves the kernel when the
// result holds no integer, for Go to give the float.
func (c *amd64Compiler) kernelMath(k *kernel, in *irInst) {
	a := &c.a
	m := mathFn(in.imm)
	if in.a.t == kindInt {
		a.Mov(AX, k.ireg(in.a.v))
		switch m {
		case mathAbs:
			pos := a.NewLabel()
			a.Test(AX, AX)
			a.J(NS, pos)
			a.Neg(AX) // minint stays minint
			a.Bind(pos)
		case mathMin, mathMax:
			keep, b := a.NewLabel(), k.ireg(in.b.v)
			if m == mathMin {
				a.Cmp(b, AX) // the second if it is less
			} else {
				a.Cmp(AX, b) // the second if the first is less
			}
			a.J(GE, keep)
			a.Mov(AX, b)
			a.Bind(keep)
		}
		a.Mov(k.ireg(in.dst), AX) // floor and ceil: the integer itself
		return
	}
	a.MovSD(0, k.reg(in.a.v))
	switch m {
	case mathFloor, mathCeil:
		mode := byte(1)
		if m == mathCeil {
			mode = 2
		}
		a.RoundSD(0, 0, mode)
		a.Cvttsd2si(AX, 0) // 1<<63 for NaN and out of range, and -2^63
		a.MovImm(DX, 1<<63)
		a.Cmp(AX, DX)
		a.J(E, c.kernelSideExit(k, in.snap, false))
		a.Mov(k.ireg(in.dst), AX)
		return
	case mathAbs:
		a.MovImm(AX, 1<<63-1)
		a.MovqToX(1, AX)
		a.AndPD(0, 1)
	case mathMin, mathMax:
		keep, b := a.NewLabel(), k.reg(in.b.v)
		if m == mathMin {
			a.Ucomisd(b, 0)
		} else {
			a.Ucomisd(0, b)
		}
		a.J(P, keep) // Lua's <, false for NaN
		a.J(AE, keep)
		a.MovSD(0, b)
		a.Bind(keep)
	}
	a.MovSD(k.reg(in.dst), 0)
}

// kernelBuffer checks that in's key is inside the buffer it accesses,
// branching to side otherwise, and leaves the address of its first
// element in DX and its kind in AX. The key is in keyReg, or a constant.
func (c *amd64Compiler) kernelBuffer(k *kernel, in *irInst, side Label) (keyReg Reg, keyConst int64, isConst bool) {
	a := &c.a
	if in.buf >= 0 {
		a.Load(rTmp, rCtx, offHoist+uint32(in.buf)*8) // the *buffer
	} else {
		a.Load(rTmp, rFrame, reg(in.obj).off+offP) // the userdata
		a.Load(rTmp, rTmp, offUDBuf)
	}
	a.Load(DX, rTmp, offBufLen)
	if key := in.a; key.isConst() {
		keyConst, isConst = c.p.Constants[key.k].i(), true
		if keyConst < 0 || keyConst >= 1<<31 {
			a.Jmp(side)
		} else {
			a.CmpImm(DX, int32(keyConst))
			a.J(BE, side) // unsigned: len <= key
		}
	} else {
		if key.t == kindFloat {
			keyReg = c.floatKey(k, key.v, in.tmp, side)
		} else {
			keyReg = k.ireg(key.v)
		}
		a.Cmp(keyReg, DX)
		a.J(AE, side)
	}
	a.Load(DX, rTmp, offBufPtr)
	a.Load8(rTmp, rTmp, offBufKind)
	return
}

// floatKey converts the float key in key to an integer in tmp, leaving
// by side unless it has an integer value.
func (c *amd64Compiler) floatKey(k *kernel, key, tmp vreg, side Label) Reg {
	a := &c.a
	x, s := k.reg(key), k.ireg(tmp)
	a.Cvttsd2si(s, x)
	c.toFloat(0, s)
	a.Ucomisd(0, x)
	a.J(NE, side)
	a.J(P, side) // NaN
	return s
}

// elementAddr returns the offset from DX of the element of 1<<scale bytes
// at the key, which it adds to DX unless the key is a constant.
func (c *amd64Compiler) elementAddr(keyReg Reg, keyConst int64, isConst bool, scale uint8) uint32 {
	if isConst {
		return uint32(keyConst) << scale
	}
	c.a.Lea(DX, DX, keyReg, scale, 0)
	return 0
}

// kernelGetBuffer compiles in, a buffer of floats' element.
func (c *amd64Compiler) kernelGetBuffer(k *kernel, in *irInst) {
	a := &c.a
	keyReg, keyConst, isConst := c.kernelBuffer(k, in, c.kernelSideExit(k, in.snap, false))
	f32, done := a.NewLabel(), a.NewLabel()
	a.Test(rTmp, rTmp)
	a.J(NE, f32)
	off := c.elementAddr(keyReg, keyConst, isConst, 3)
	a.LoadSD(k.reg(in.dst), DX, off)
	a.Jmp(done)
	a.Bind(f32)
	off = c.elementAddr(keyReg, keyConst, isConst, 2)
	a.LoadSSToSD(k.reg(in.dst), DX, off)
	a.Bind(done)
}

// kernelSetBuffer compiles in, a store to a buffer's element, converting
// as putBuffer does. An integer buffer given a float leaves the kernel,
// for Go to check it has an integer value.
func (c *amd64Compiler) kernelSetBuffer(k *kernel, in *irInst) {
	a := &c.a
	side := c.kernelSideExit(k, in.snap, false)
	keyReg, keyConst, isConst := c.kernelBuffer(k, in, side)
	val := in.b
	var constant value
	if val.isConst() {
		constant = c.p.Constants[val.k]
	}
	notF64, notF32, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Test(rTmp, rTmp)
	a.J(NE, notF64)
	off := c.elementAddr(keyReg, keyConst, isConst, 3)
	switch {
	case constant.isNumber(): // the bits, from a general register: see setIndex
		a.MovImm(rTmp, math.Float64bits(constant.toFloat()))
		a.Store(DX, off, rTmp)
	case val.t == kindInt:
		c.toFloat(0, k.ireg(val.v))
		a.StoreSD(DX, off, 0)
	default:
		a.StoreSD(DX, off, k.reg(val.v))
	}
	a.Jmp(done)
	a.Bind(notF64)
	a.CmpImm(rTmp, int32(bufferFloat32))
	a.J(NE, notF32)
	off = c.elementAddr(keyReg, keyConst, isConst, 2)
	switch {
	case constant.isNumber():
		a.MovImm(rTmp, uint64(math.Float32bits(float32(constant.toFloat()))))
		a.Store32(DX, off, rTmp)
	case val.t == kindInt:
		c.toFloat(0, k.ireg(val.v))
		a.Cvtsd2ss(0, 0)
		a.StoreSS(DX, off, 0)
	default:
		a.Cvtsd2ss(0, k.reg(val.v))
		a.StoreSS(DX, off, 0)
	}
	a.Jmp(done)
	a.Bind(notF32) // an integer buffer: an integer value, or Go converts
	if val.t != kindInt {
		a.Jmp(side)
	} else {
		r := rTmp
		isU8 := a.NewLabel()
		a.CmpImm(rTmp, int32(bufferUint8))
		a.J(E, isU8)
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		} else {
			r = k.ireg(val.v)
		}
		off = c.elementAddr(keyReg, keyConst, isConst, 2)
		a.Store32(DX, off, r)
		a.Jmp(done)
		a.Bind(isU8)
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		}
		off = c.elementAddr(keyReg, keyConst, isConst, 0)
		a.Store8(DX, off, r)
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

// kernelStep is FORLOOP on registers.
func (c *amd64Compiler) kernelStep(k *kernel, take, end Label) {
	a := &c.a
	if k.intLoop {
		idx, count, step, ext := k.ireg(k.loop[0]), k.ireg(k.loop[1]), k.ireg(k.loop[2]), k.ireg(k.loop[3])
		a.Test(count, count)
		a.J(E, end)
		a.SubImm(count, 1)
		a.Add(idx, step)
		if ext != idx {
			a.Mov(ext, idx)
		}
		a.Jmp(take)
		return
	}
	idx, limit, step, ext := k.reg(k.loop[0]), k.reg(k.loop[1]), k.reg(k.loop[2]), k.reg(k.loop[3])
	yes := a.NewLabel()
	c.forStep(idx, limit, step, yes, end)
	a.Bind(yes)
	a.MovSD(idx, 0)
	if ext != idx {
		a.MovSD(ext, 0)
	}
	a.Jmp(take)
}

// flush writes snapshot s's registers back to the frame.
func (c *amd64Compiler) flush(k *kernel, s *irSnap) {
	for _, r := range s.regs {
		if r.v != noVreg && k.spilled(r.v) {
			continue // in its stack slot already
		}
		switch r.t {
		case kindTable:
			c.storeTable(reg(r.r), k.ireg(r.v))
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

// floatArg returns an SSE register holding x as a float: its kernel
// register, or tmp, into which it loads a constant or converts an
// integer.
func (c *amd64Compiler) floatArg(k *kernel, x irArg, tmp XReg) XReg {
	a := &c.a
	if !x.isConst() {
		if x.t == kindInt {
			c.toFloat(tmp, k.ireg(x.v))
			return tmp
		}
		return k.reg(x.v)
	}
	if v := c.p.Constants[x.k]; v.isInteger() {
		a.MovImm(rTmp, math.Float64bits(float64(v.i())))
		a.MovqToX(tmp, rTmp)
		return tmp
	}
	o, _ := c.constant(x.k)
	a.LoadSD(tmp, o.base, o.off+offN)
	return tmp
}

// intArg returns a register holding the integer x: its kernel register,
// or tmp, into which it loads a constant.
func (c *amd64Compiler) intArg(k *kernel, x irArg, tmp Reg) Reg {
	if !x.isConst() {
		return k.ireg(x.v)
	}
	o, _ := c.constant(x.k)
	c.a.Load(tmp, o.base, o.off+offN)
	return tmp
}

// irArith is the bytecode operator an arithmetic operation computes.
var irArith = map[irOp]bytecode.OpCode{irAdd: bytecode.OpAdd, irSub: bytecode.OpSub, irMul: bytecode.OpMul, irDiv: bytecode.OpDiv}

// kernelInstruction lowers the operation in; label returns a body pc's
// label.
func (c *amd64Compiler) kernelInstruction(k *kernel, in *irInst, label func(int) Label) {
	a := &c.a
	p := c.p
	isInt := in.dst != noVreg && k.isInt(in.dst)
	switch in.op {
	case irMove:
		if isInt {
			a.Mov(k.ireg(in.dst), k.ireg(in.a.v))
		} else {
			a.MovSD(k.reg(in.dst), k.reg(in.a.v))
		}
	case irConst:
		o, _ := c.constant(in.a.k)
		switch v := p.Constants[in.a.k]; {
		case isInt:
			a.Load(k.ireg(in.dst), o.base, o.off+offN)
		case v.isInteger(): // promoted: see promoteConstants
			a.MovImm(rTmp, math.Float64bits(float64(v.i())))
			a.MovqToX(k.reg(in.dst), rTmp)
		default:
			a.LoadSD(k.reg(in.dst), o.base, o.off+offN)
		}
	case irAdd, irSub, irMul, irDiv:
		if isInt {
			// In AX, as the destination may be an operand.
			if b := c.intArg(k, in.a, AX); b != AX {
				a.Mov(AX, b)
			}
			cc := c.intArg(k, in.b, DX)
			switch in.op {
			case irAdd:
				a.Add(AX, cc)
			case irSub:
				a.Sub(AX, cc)
			case irMul:
				a.Imul(AX, cc)
			}
			a.Mov(k.ireg(in.dst), AX)
			break
		}
		b, cc := c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1)
		c.arith(irArith[in.op], 4, b, cc) // in X4, as the destination may be an operand
		a.MovSD(k.reg(in.dst), 4)
	case irAnd, irOr, irXor, irNot, irShift:
		if b := c.intArg(k, in.a, AX); b != AX {
			a.Mov(AX, b)
		}
		switch in.op {
		case irAnd:
			a.And(AX, c.intArg(k, in.b, DX))
		case irOr:
			a.Or(AX, c.intArg(k, in.b, DX))
		case irXor:
			a.Xor(AX, c.intArg(k, in.b, DX))
		case irNot:
			a.Not(AX)
		case irShift:
			switch count := in.imm; {
			case in.flag:
				a.MovImm(AX, 0)
			case count >= 0:
				a.Shl(AX, uint8(count))
			default:
				a.Shr(AX, uint8(-count))
			}
		}
		a.Mov(k.ireg(in.dst), AX)
	case irFloorDiv: // floor(b / c)
		b, cc := c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1)
		c.arith(bytecode.OpDiv, 4, b, cc)
		a.RoundSD(4, 4, 1)
		a.MovSD(k.reg(in.dst), 4)
	case irFloatMod: // by a floatModDivisor
		if b := c.floatArg(k, in.a, 0); b != 0 {
			a.MovSD(0, b)
		}
		c.floatMod(math.Float64frombits(uint64(in.imm)))
		a.MovSD(k.reg(in.dst), 3)
	case irIntDiv, irIntMod:
		// By a nonzero constant: floor the quotient toward minus infinity,
		// and give the modulo the divisor's sign.
		op := bytecode.OpIDiv
		if in.op == irIntMod {
			op = bytecode.OpMod
		}
		divisor := in.imm
		b, d := k.ireg(in.a.v), k.ireg(in.dst)
		if divisor == -1 { // IDIV would trap on minint / -1
			if op == bytecode.OpMod {
				a.MovImm(d, 0)
			} else {
				a.Mov(d, b)
				a.Neg(d)
			}
			break
		}
		if plan, ok := planDivide(divisor); ok {
			a.Mov(d, c.divideByConstant(op, plan, b))
			break
		}
		o, _ := c.constant(in.b.k)
		a.Mov(AX, b)
		a.Cqo()
		a.IdivMem(o.base, o.off+offN) // AX: toward zero; DX: b's sign
		adjust := a.NewLabel()
		a.Test(DX, DX)
		a.J(E, adjust)
		if divisor > 0 {
			a.J(NS, adjust)
		} else {
			a.J(S, adjust)
		}
		if op == bytecode.OpMod {
			a.AddImm(DX, int32(divisor))
		} else {
			a.SubImm(AX, 1)
		}
		a.Bind(adjust)
		if op == bytecode.OpMod {
			a.Mov(d, DX)
		} else {
			a.Mov(d, AX)
		}
	case irNeg:
		if isInt {
			a.Mov(k.ireg(in.dst), k.ireg(in.a.v))
			a.Neg(k.ireg(in.dst))
			break
		}
		a.MovSD(4, k.reg(in.a.v))
		c.signMask(3)
		a.XorPD(4, 3)
		a.MovSD(k.reg(in.dst), 4)
	case irBranch:
		var yes Label
		if in.target >= 0 {
			yes = label(in.target)
		} else {
			yes = c.kernelSideExit(k, in.snap, true)
		}
		no := label(in.pc + 2)
		if in.a.t == kindInt && in.b.t == kindInt {
			a.Cmp(c.intArg(k, in.a, AX), c.intArg(k, in.b, DX))
			when := map[bytecode.OpCode]Cond{bytecode.OpEqual: E, bytecode.OpLessThan: L, bytecode.OpLessOrEqual: LE}[in.cmp]
			if !in.flag {
				when ^= 1
			}
			a.J(when, yes)
			a.Jmp(no)
			return
		}
		b, cc := c.floatArg(k, in.a, 0), c.floatArg(k, in.b, 1)
		c.compare(in.cmp, in.flag, b, cc, yes, no)
	case irJump:
		a.Jmp(label(in.target))
	case irExit:
		a.Jmp(c.kernelSideExit(k, in.snap, true))
	case irHoist: // a number, from the context
		if isInt {
			a.Load(k.ireg(in.dst), rCtx, offHoist+uint32(in.imm)*8)
		} else {
			a.LoadSD(k.reg(in.dst), rCtx, offHoist+uint32(in.imm)*8)
		}
	case irIntrinsic:
		c.kernelCall(k, in)
	case irMath:
		c.kernelMath(k, in)
	case irBufGet:
		c.kernelGetBuffer(k, in)
	case irBufSet:
		c.kernelSetBuffer(k, in)
	case irArrayGet, irFieldGet:
		exit := c.kernelSideExit(k, in.snap, true)
		c.tableSlot(k, in, exit)
		c.tableValue(k, in, exit)
	case irArraySet, irFieldSet:
		exit, t := c.kernelSideExit(k, in.snap, true), k.ireg(in.a.v)
		c.tableSlot(k, in, exit)
		a.Load(DX, AX, offP)
		a.Test(DX, DX)
		if in.op == irFieldSet { // over a value only
			a.J(E, exit)
		} else { // over a value, or nil in a table without a metatable
			present := a.NewLabel()
			a.J(NE, present)
			a.Load(DX, t, offTMeta)
			a.Test(DX, DX)
			a.J(NE, exit)
			a.Bind(present)
		}
		c.tableStore(k, in)
		if in.op == irFieldSet {
			a.StoreZero8(t, offTFlags) // invalidateTagMethodCache
		}
	}
}

// storeTable stores the table r points to at dst.
func (c *amd64Compiler) storeTable(dst operand, r Reg) {
	c.a.MovImm(rTmp, tagOf(vkTable))
	c.a.Store(dst.base, dst.off+offN, rTmp)
	c.a.Store(dst.base, dst.off+offP, r)
}

// tableSlot leaves in AX the address of the value the table operation in
// reads or writes: the array element at key b, or the own field the
// fieldCache at in.pc names. It branches to exit when there is none, and
// uses DX.
func (c *amd64Compiler) tableSlot(k *kernel, in *irInst, exit Label) {
	a := &c.a
	t := k.ireg(in.a.v)
	if in.op == irArrayGet || in.op == irArraySet {
		if in.b.isConst() {
			a.MovImm(AX, uint64(c.p.Constants[in.b.k].i()-1))
		} else {
			a.Mov(AX, k.ireg(in.b.v))
			a.SubImm(AX, 1)
		}
		a.Load(DX, t, offTArray+offSliceLen)
		a.Cmp(AX, DX)
		a.J(AE, exit) // unsigned: keys below 1 too
		a.Shl(AX, 4)
		a.Load(DX, t, offTArray)
		a.Add(AX, DX)
		return
	}
	cache := uint64(uintptr(unsafe.Pointer(&c.p.fields[in.pc])))
	a.Load(AX, t, offTShape)
	a.Test(AX, AX)
	a.J(E, exit)
	a.MovImm(DX, cache)
	a.Load(DX, DX, offCShape)
	a.Cmp(AX, DX)
	a.J(NE, exit)
	a.MovImm(DX, cache)
	a.Load32S(DX, DX, offCSlot)
	a.Load(AX, t, offTSlots+offSliceLen)
	a.Cmp(DX, AX)
	a.J(AE, exit) // unsigned: a slot below 0, not the table's own, too
	a.Shl(DX, 4)
	a.Load(AX, t, offTSlots)
	a.Add(AX, DX)
}

// tableValue loads the value at AX into in's result, branching to exit
// unless it has the result's type. It uses DX.
func (c *amd64Compiler) tableValue(k *kernel, in *irInst, exit Label) {
	a := &c.a
	switch k.typeOf(in.dst) {
	case kindFloat:
		a.Load(DX, AX, offP)
		a.Cmp(DX, rNumber)
		a.J(NE, exit)
		a.LoadSD(k.reg(in.dst), AX, offN)
	case kindInt:
		a.Load(DX, AX, offP)
		a.Sub(DX, rNumber)
		a.CmpImm(DX, 1)
		a.J(NE, exit)
		a.Load(k.ireg(in.dst), AX, offN)
	case kindTable:
		a.Load(DX, AX, offN)
		a.Shr(DX, kindShift)
		a.CmpImm(DX, int32(vkTable))
		a.J(NE, exit)
		a.Load(DX, AX, offP)
		a.Mov(AX, DX)
		a.Sub(AX, rNumber)
		a.CmpImm(AX, 1)
		a.J(BE, exit) // a number whose bits match the tag
		a.Mov(k.ireg(in.dst), DX)
	}
}

// tableStore stores in's value, c, at AX. It uses DX.
func (c *amd64Compiler) tableStore(k *kernel, in *irInst) {
	a := &c.a
	v := in.c
	t := v.t
	switch {
	case v.isConst():
		n := c.p.Constants[v.k]
		if t == kindFloat {
			a.MovImm(DX, math.Float64bits(n.toFloat()))
		} else {
			a.MovImm(DX, uint64(n.i()))
		}
		a.Store(AX, offN, DX)
	case t == kindFloat:
		a.StoreSD(AX, offN, k.reg(v.v))
	case t == kindInt:
		a.Store(AX, offN, k.ireg(v.v))
	case t == kindTable:
		a.MovImm(DX, tagOf(vkTable))
		a.Store(AX, offN, DX)
		a.Store(AX, offP, k.ireg(v.v))
		return
	}
	if t == kindFloat {
		a.Store(AX, offP, rNumber)
		return
	}
	a.Mov(DX, rNumber)
	a.AddImm(DX, 1) // integerPtr, the next byte
	a.Store(AX, offP, DX)
}
