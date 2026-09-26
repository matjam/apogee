//go:build (darwin || linux) && amd64

package lua

import (
	"math"

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
	*kernelPlan
	labels []Label
}

// reg and ireg return register r's machine register as a float and as an
// integer.
func (k *kernel) reg(r int) XReg { return kernelFirst + XReg(k.slot(r, kindFloat)) }
func (k *kernel) ireg(r int) Reg { return kernelInts[k.slot(r, kindInt)] }

// findKernels returns the kernels for the FORLOOP at latch, an integer
// loop's first, or nil when its loop does not qualify.
func (c *amd64Compiler) findKernels(latch int) []*kernel {
	var fns []uint64
	for _, in := range c.intrinsics() {
		fns = append(fns, in.fn)
	}
	constOK := func(k int) bool { _, ok := c.constant(k); return ok }
	intrinsic := func(n int) (uint64, bool) { return upValueIntrinsic(c.cl, n, fns) }
	upValue := func(n int) (numKind, bool) { return upValueKind(c.cl, n) }
	var ks []*kernel
	for _, intLoop := range []bool{true, false} {
		plan := planKernel(c.p, latch, intLoop, kernelCount, len(kernelInts), constOK, intrinsic, upValue)
		if plan == nil {
			continue
		}
		k := &kernel{kernelPlan: plan, labels: make([]Label, latch-plan.start)}
		for i := range k.labels {
			k.labels[i] = c.a.NewLabel()
		}
		ks = append(ks, k)
	}
	return ks
}

// emitKernel compiles k at the FORLOOP's pc, jumping to normal, the next
// kernel or the ordinary FORLOOP code, when the entry check fails.
func (c *amd64Compiler) emitKernel(k *kernel, normal Label) {
	a := &c.a
	base := c.p.Code[k.latch].A()

	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, normal)
	c.kernelGuards(k, normal)
	for _, r := range k.liveIn {
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
		if k.types[r] == kindInt {
			a.Load(k.ireg(r), rFrame, reg(r).off+offN)
		} else {
			a.LoadSD(k.reg(r), rFrame, reg(r).off+offN)
		}
	}
	counter := offKernels
	if k.intLoop {
		counter += 8
	}
	a.SubMem(rCtx, counter, -1)
	body, latch, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	// The first FORLOOP: nothing has changed if the loop does not run.
	c.kernelStep(k, base, body, c.pcs[k.latch+1])

	a.Bind(body)
	for ip := k.start; ip < k.latch; ip++ {
		a.Bind(k.labels[ip-k.start])
		ip += c.kernelInstruction(k, ip, latch)
	}
	a.Bind(latch)
	next := a.NewLabel()
	c.kernelStep(k, base, next, done)
	a.Bind(next)
	out := a.NewLabel()
	a.SubMem(rCtx, offBudget, 1)
	a.J(E, out)
	a.Jmp(body)
	a.Bind(out)
	end := k.at[k.latch-k.start]
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
		a.Load(rTmp, rTmp, offGFNumber)
		a.Test(rTmp, rTmp)
		a.J(E, normal)
		a.Load(rTmp, rTmp, offNFUnary)
		a.MovImm(rTmp2, k.upValueFn(n))
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

// kernelSideExit returns a label that leaves k at ip: it writes k's
// registers back, and the ordinary code runs the instruction and the rest
// of the iteration.
func (c *amd64Compiler) kernelSideExit(k *kernel, ip int) Label {
	a := &c.a
	l := a.NewLabel()
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(l)
		c.flush(k, k.at[ip-k.start])
		for _, kc := range k.calleesAt(ip) { // after the flush: it uses kernel registers
			c.upValueAddr(kc.upValue)
			c.load(operand{rAddr, 0})
			c.store(reg(kc.a))
		}
		a.Jmp(c.pcs[ip])
	})
	return l
}

// intrinsicSaved are the kernel registers sin and cos use.
var intrinsicSaved = []Reg{rN, rT2, rIdx}

// kernelCall compiles the CALL i at ip that k computes an intrinsic for:
// on the argument in X0, saving the kernel registers the intrinsic's code
// uses, into register A. The intrinsic's own exits leave the kernel with
// register A holding the function again, as the ordinary CALL expects.
func (c *amd64Compiler) kernelCall(k *kernel, ip int, i bytecode.Instruction) {
	a := &c.a
	kc := k.calls[ip]
	var saved []Reg
	if kc.fn != funcValue(math.Sqrt) { // sqrt is one instruction
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
	arg := i.A() + 1
	if k.typeAt(ip, arg) == kindInt {
		c.toFloat(0, k.ireg(arg))
	} else {
		a.MovSD(0, k.reg(arg))
	}
	save()
	side := a.NewLabel()
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(side)
		restore()
		c.flush(k, k.at[ip-k.start])
		// The ordinary CALL finds the function in register A again.
		c.upValueAddr(kc.upValue)
		c.load(operand{rAddr, 0})
		c.store(reg(i.A()))
		a.Jmp(c.pcs[ip])
	})
	c.kernelExit = side
	c.ip = ip
	for _, in := range c.intrinsics() {
		if in.fn == kc.fn {
			in.emit()
		}
	}
	c.kernelExit = -1
	restore()
	a.MovSD(k.reg(i.A()), 0)
}

// kernelBuffer checks that key is inside the buffer the access at ip
// reads, in register obj or hoisted, branching to side otherwise, and
// leaves the address of its first element in DX and its kind in AX. The
// key is in keyReg, or a constant.
func (c *amd64Compiler) kernelBuffer(k *kernel, ip, obj, key int, side Label) (keyReg Reg, keyConst int64, isConst bool) {
	a := &c.a
	if s := k.bufFrom[ip]; s >= 0 {
		a.Load(rTmp, rCtx, offHoist+uint32(s)*8) // the *buffer
	} else {
		a.Load(rTmp, rFrame, reg(obj).off+offP) // the userdata
		a.Load(rTmp, rTmp, offUDBuf)
	}
	a.Load(DX, rTmp, offBufLen)
	if bytecode.IsConstant(key) {
		keyConst, isConst = c.p.Constants[bytecode.ConstantIndex(key)].i(), true
		if keyConst < 0 || keyConst >= 1<<31 {
			a.Jmp(side)
		} else {
			a.CmpImm(DX, int32(keyConst))
			a.J(BE, side) // unsigned: len <= key
		}
	} else {
		keyReg = k.ireg(key)
		a.Cmp(keyReg, DX)
		a.J(AE, side)
	}
	a.Load(DX, rTmp, offBufPtr)
	a.Load8(rTmp, rTmp, offBufKind)
	return
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

// kernelGetBuffer compiles GETTABLE A B C in k: a buffer of floats' element.
func (c *amd64Compiler) kernelGetBuffer(k *kernel, ip int, i bytecode.Instruction) {
	a := &c.a
	side := c.kernelSideExit(k, ip)
	keyReg, keyConst, isConst := c.kernelBuffer(k, ip, i.B(), i.C(), side)
	f32, done := a.NewLabel(), a.NewLabel()
	a.Test(rTmp, rTmp)
	a.J(NE, f32)
	off := c.elementAddr(keyReg, keyConst, isConst, 3)
	a.LoadSD(k.reg(i.A()), DX, off)
	a.Jmp(done)
	a.Bind(f32)
	off = c.elementAddr(keyReg, keyConst, isConst, 2)
	a.LoadSSToSD(k.reg(i.A()), DX, off)
	a.Bind(done)
}

// kernelSetBuffer compiles SETTABLE A B C in k: to a buffer's element,
// converting as putBuffer does. An integer buffer given a float leaves
// the kernel, for Go to check it has an integer value.
func (c *amd64Compiler) kernelSetBuffer(k *kernel, ip int, i bytecode.Instruction) {
	a := &c.a
	side := c.kernelSideExit(k, ip)
	keyReg, keyConst, isConst := c.kernelBuffer(k, ip, i.A(), i.B(), side)
	val := i.C()
	kind := k.kind(c.p, ip, val)
	var constant value
	if bytecode.IsConstant(val) {
		constant = c.p.Constants[bytecode.ConstantIndex(val)]
	}
	notF64, notF32, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Test(rTmp, rTmp)
	a.J(NE, notF64)
	off := c.elementAddr(keyReg, keyConst, isConst, 3)
	switch {
	case constant.isNumber(): // the bits, from a general register: see setIndex
		a.MovImm(rTmp, math.Float64bits(constant.toFloat()))
		a.Store(DX, off, rTmp)
	case kind == kindInt:
		c.toFloat(0, k.ireg(val))
		a.StoreSD(DX, off, 0)
	default:
		a.StoreSD(DX, off, k.reg(val))
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
	case kind == kindInt:
		c.toFloat(0, k.ireg(val))
		a.Cvtsd2ss(0, 0)
		a.StoreSS(DX, off, 0)
	default:
		a.Cvtsd2ss(0, k.reg(val))
		a.StoreSS(DX, off, 0)
	}
	a.Jmp(done)
	a.Bind(notF32) // an integer buffer: an integer value, or Go converts
	if kind != kindInt {
		a.Jmp(side)
	} else {
		r := rTmp
		isU8 := a.NewLabel()
		a.CmpImm(rTmp, int32(bufferUint8))
		a.J(E, isU8)
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		} else {
			r = k.ireg(val)
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

// usesInt reports whether k keeps a Lua register in machine register r.
func (k *kernel) usesInt(r Reg) bool {
	for s, n := range k.slots {
		if s.t == kindInt && kernelInts[n] == r {
			return true
		}
	}
	return false
}

// kernelStep is FORLOOP on registers.
func (c *amd64Compiler) kernelStep(k *kernel, base int, take, end Label) {
	a := &c.a
	if k.intLoop {
		idx, count, step, ext := k.ireg(base), k.ireg(base+1), k.ireg(base+2), k.ireg(base+3)
		a.Test(count, count)
		a.J(E, end)
		a.SubImm(count, 1)
		a.Add(idx, step)
		a.Mov(ext, idx)
		a.Jmp(take)
		return
	}
	idx, limit, step, ext := k.reg(base), k.reg(base+1), k.reg(base+2), k.reg(base+3)
	yes := a.NewLabel()
	c.forStep(idx, limit, step, yes, end)
	a.Bind(yes)
	a.MovSD(idx, 0)
	a.MovSD(ext, 0)
	a.Jmp(take)
}

// flush writes k's registers back to the frame, where they have types:
// those the loop writes that are defined there.
func (c *amd64Compiler) flush(k *kernel, types map[int]numKind) {
	for _, r := range k.writtenOnce() {
		switch types[r] {
		case kindInt:
			c.storeInteger(reg(r), k.ireg(r))
		case kindFloat:
			c.storeNumber(reg(r), k.reg(r))
		}
	}
	// A register aliasing a hoisted buffer gets the upvalue's value, as
	// the ordinary code expects. This uses kernel registers, so comes last.
	for _, r := range k.writtenOnce() {
		if s, ok := bufferSlot(types[r]); ok {
			c.upValueAddr(k.hoisted[s].n)
			c.load(operand{rAddr, 0})
			c.store(reg(r))
		}
	}
}

// floatOperand returns an SSE register holding RK field as a float before
// ip: its kernel register, or tmp, into which it loads a constant or
// converts an integer.
func (c *amd64Compiler) floatOperand(k *kernel, ip, field int, tmp XReg) XReg {
	a := &c.a
	if !bytecode.IsConstant(field) {
		if k.typeAt(ip, field) == kindInt {
			c.toFloat(tmp, k.ireg(field))
			return tmp
		}
		return k.reg(field)
	}
	kk := bytecode.ConstantIndex(field)
	if v := c.p.Constants[kk]; v.isInteger() {
		a.MovImm(rTmp, math.Float64bits(float64(v.i())))
		a.MovqToX(tmp, rTmp)
		return tmp
	}
	o, _ := c.constant(kk)
	a.LoadSD(tmp, o.base, o.off+offN)
	return tmp
}

// intOperand returns a register holding the integer RK field: its kernel
// register, or tmp, into which it loads a constant.
func (c *amd64Compiler) intOperand(k *kernel, field int, tmp Reg) Reg {
	if !bytecode.IsConstant(field) {
		return k.ireg(field)
	}
	o, _ := c.constant(bytecode.ConstantIndex(field))
	c.a.Load(tmp, o.base, o.off+offN)
	return tmp
}

func (k *kernel) target(t int, latch Label) Label {
	if t == k.latch {
		return latch
	}
	return k.labels[t-k.start]
}

func (c *amd64Compiler) kernelInstruction(k *kernel, ip int, latch Label) int {
	a := &c.a
	p := c.p
	i := p.Code[ip]
	isInt := k.results[ip] == kindInt // what the instruction writes to A
	switch op := i.OpCode(); op {
	case bytecode.OpMove:
		if !isNumKind(k.results[ip]) {
			break // an alias of a hoisted buffer: nothing to move
		}
		if isInt {
			a.Mov(k.ireg(i.A()), k.ireg(i.B()))
		} else {
			a.MovSD(k.reg(i.A()), k.reg(i.B()))
		}
	case bytecode.OpLoadConstant:
		o, _ := c.constant(i.Bx())
		if isInt {
			a.Load(k.ireg(i.A()), o.base, o.off+offN)
		} else {
			a.LoadSD(k.reg(i.A()), o.base, o.off+offN)
		}
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
		if isInt {
			// In AX, as the destination may be an operand.
			if b := c.intOperand(k, i.B(), AX); b != AX {
				a.Mov(AX, b)
			}
			cc := c.intOperand(k, i.C(), DX)
			switch op {
			case bytecode.OpAdd:
				a.Add(AX, cc)
			case bytecode.OpSub:
				a.Sub(AX, cc)
			case bytecode.OpMul:
				a.Imul(AX, cc)
			}
			a.Mov(k.ireg(i.A()), AX)
			break
		}
		b, cc := c.floatOperand(k, ip, i.B(), 0), c.floatOperand(k, ip, i.C(), 1)
		c.arith(op, 4, b, cc) // in X4, as the destination may be an operand
		a.MovSD(k.reg(i.A()), 4)
	case bytecode.OpMod, bytecode.OpIDiv:
		// By a nonzero constant: floor the quotient toward minus infinity,
		// and give the modulo the divisor's sign.
		kk := bytecode.ConstantIndex(i.C())
		divisor := p.Constants[kk].i()
		b, d := k.ireg(i.B()), k.ireg(i.A())
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
		o, _ := c.constant(kk)
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
	case bytecode.OpUnaryMinus:
		if isInt {
			a.Mov(k.ireg(i.A()), k.ireg(i.B()))
			a.Neg(k.ireg(i.A()))
			break
		}
		a.MovSD(4, k.reg(i.B()))
		c.signMask(3)
		a.XorPD(4, 3)
		a.MovSD(k.reg(i.A()), 4)
	case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
		t, _ := kernelJump(p.Code, ip, k.latch)
		yes, no := k.target(t, latch), k.target(ip+2, latch)
		if k.kind(p, ip, i.B()) == kindInt && k.kind(p, ip, i.C()) == kindInt {
			a.Cmp(c.intOperand(k, i.B(), AX), c.intOperand(k, i.C(), DX))
			when := map[bytecode.OpCode]Cond{bytecode.OpEqual: E, bytecode.OpLessThan: L, bytecode.OpLessOrEqual: LE}[op]
			if i.A() == 0 {
				when ^= 1
			}
			a.J(when, yes)
			a.Jmp(no)
			return 1
		}
		b, cc := c.floatOperand(k, ip, i.B(), 0), c.floatOperand(k, ip, i.C(), 1)
		c.compare(op, i.A() != 0, b, cc, yes, no)
		return 1
	case bytecode.OpJump:
		t, _ := kernelJump(p.Code, ip, k.latch)
		a.Jmp(k.target(t, latch))
	case bytecode.OpGetUpValue:
		// The function an intrinsic call checked on entry, or a buffer the
		// context holds: nothing. A number, from the context.
		if s, ok := k.upLoads[ip]; ok && isNumKind(k.results[ip]) {
			if isInt {
				a.Load(k.ireg(i.A()), rCtx, offHoist+uint32(s)*8)
			} else {
				a.LoadSD(k.reg(i.A()), rCtx, offHoist+uint32(s)*8)
			}
		}
	case bytecode.OpCall:
		c.kernelCall(k, ip, i)
	case bytecode.OpGetTable, bytecode.OpGetTableUp: // GETTABUP's B, the upvalue, is hoisted
		c.kernelGetBuffer(k, ip, i)
	case bytecode.OpSetTable, bytecode.OpSetTableUp:
		c.kernelSetBuffer(k, ip, i)
	}
	return 0
}
