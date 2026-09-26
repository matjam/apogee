//go:build (darwin || linux) && arm64

package lua

import (
	"math"

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
	*kernelPlan
	labels []Label // kernel code for each body pc
}

// reg and ireg return register r's machine register as a float and as an
// integer.
func (k *kernel) reg(r int) FReg { return kernelFirst + FReg(k.slot(r, kindFloat)) }
func (k *kernel) ireg(r int) Reg { return kernelInts[k.slot(r, kindInt)] }

// findKernels returns the kernels for the FORLOOP at latch, an integer
// loop's first, or nil when its loop does not qualify.
func (c *arm64Compiler) findKernels(latch int) []*kernel {
	var fns []uint64
	for _, in := range intrinsics {
		fns = append(fns, in.fn)
	}
	constOK := func(k int) bool { _, ok := c.constant(k); return ok }
	intrinsic := func(n int) (uint64, bool) { return upValueIntrinsic(c.cl, n, fns) }
	upValue := func(n int) (numKind, bool) { return upValueKind(c.cl, n) }
	var ks []*kernel
	for _, intLoop := range []bool{true, false} {
		plan := planKernel(c.p, latch, intLoop, kernelCount, len(kernelInts), constOK, intrinsic, upValue, true)
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

// emitKernel compiles k at the FORLOOP's pc. It falls through to normal,
// the next kernel or the ordinary FORLOOP code, when the entry check
// fails.
func (c *arm64Compiler) emitKernel(k *kernel, normal Label) {
	a := &c.a
	base := c.p.Code[k.latch].A()

	// Entry: the write barrier is off, the calls and buffers are what the
	// kernel was compiled for, and the live-in registers hold numbers of
	// their types.
	a.Cbnz(rBarrier, normal)
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
		if k.types[r] == kindInt {
			a.Ldr(k.ireg(r), rFrame, reg(r).off+offN)
		} else {
			a.LdrD(k.reg(r), rFrame, reg(r).off+offN)
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
	// Spend budget; when it runs out, write back and resume at the body,
	// as the interpreter would after this FORLOOP.
	out := a.NewLabel()
	a.SubsImm(rBudget, rBudget, 1)
	a.BCond(EQ, out)
	a.B(body)
	a.Bind(out)
	end := k.at[k.latch-k.start]
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
		a.Ldr(rTmp, rTmp, offGFNumber)
		a.Cbz(rTmp, normal)
		a.Ldr(rTmp, rTmp, offNFUnary)
		a.MovImm(rTmp2, k.upValueFn(n))
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

// kernelSideExit returns a label that leaves k at ip: it writes k's
// registers back, and the ordinary code runs the instruction and the rest
// of the iteration.
func (c *arm64Compiler) kernelSideExit(k *kernel, ip int) Label {
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
		a.B(c.pcs[ip])
	})
	return l
}

// intrinsicSaved are the kernel registers sin and cos use.
var intrinsicSaved = []Reg{rTrig, rIdx, rLen}

// kernelCall compiles the CALL i at ip that k computes an intrinsic for:
// on the argument in D0, saving the kernel registers the intrinsic's code
// uses, into register A. The intrinsic's own exits leave the kernel with
// register A holding the function again, as the ordinary CALL expects.
func (c *arm64Compiler) kernelCall(k *kernel, ip int, i bytecode.Instruction) {
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
			a.Str(r, rCtx, offSpill+uint32(j)*8)
		}
	}, func() {
		for j, r := range saved {
			a.Ldr(r, rCtx, offSpill+uint32(j)*8)
		}
	}
	arg := i.A() + 1
	if k.typeAt(ip, arg) == kindInt {
		a.Scvtf(0, k.ireg(arg))
	} else {
		a.Fmov(0, k.reg(arg))
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
		a.B(c.pcs[ip])
	})
	c.kernelExit = side
	for _, in := range intrinsics {
		if in.fn == kc.fn {
			in.emit(c, ip)
		}
	}
	c.kernelExit = -1
	restore()
	a.Fmov(k.reg(i.A()), 0)
}

// kernelBuffer checks that key is inside the buffer the access at ip
// reads, in register obj or hoisted, branching to side otherwise, and
// leaves the address of its first element in rTmp2 and its kind in rTmp.
// It returns the register holding the key: its kernel register, or
// rExitPC for a constant.
func (c *arm64Compiler) kernelBuffer(k *kernel, ip, obj, key int, side Label) Reg {
	a := &c.a
	keyReg := rExitPC
	switch {
	case bytecode.IsConstant(key):
		a.MovImm(rExitPC, uint64(c.p.Constants[bytecode.ConstantIndex(key)].i()))
	case k.typeAt(ip, key) == kindFloat: // leave unless it has an integer value
		x := k.reg(key)
		a.Fcvtzs(rExitPC, x)
		a.Scvtf(0, rExitPC)
		a.Fcmp(0, x)
		a.BCond(NE, side) // NaN too
	default:
		keyReg = k.ireg(key)
	}
	if s := k.bufFrom[ip]; s >= 0 {
		a.Ldr(rTmp, rCtx, offHoist+uint32(s)*8) // the *buffer
	} else {
		a.Ldr(rTmp, rFrame, reg(obj).off+offP) // the userdata
		a.Ldr(rTmp, rTmp, offUDBuf)
	}
	a.Ldr(rTmp2, rTmp, offBufLen)
	a.Cmp(keyReg, rTmp2)
	a.BCond(HS, side) // unsigned: below 0 too
	a.Ldr(rTmp2, rTmp, offBufPtr)
	a.Ldrb(rTmp, rTmp, offBufKind)
	return keyReg
}

// kernelGetBuffer compiles GETTABLE A B C in k: a buffer of floats' element.
func (c *arm64Compiler) kernelGetBuffer(k *kernel, ip int, i bytecode.Instruction) {
	a := &c.a
	key := c.kernelBuffer(k, ip, i.B(), i.C(), c.kernelSideExit(k, ip))
	d := k.reg(i.A())
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

// kernelSetBuffer compiles SETTABLE A B C in k: to a buffer's element,
// converting as putBuffer does. An integer buffer given a float leaves
// the kernel, for Go to check it has an integer value.
func (c *arm64Compiler) kernelSetBuffer(k *kernel, ip int, i bytecode.Instruction) {
	a := &c.a
	side := c.kernelSideExit(k, ip)
	key := c.kernelBuffer(k, ip, i.A(), i.B(), side)
	val := i.C()
	kind := k.kind(c.p, ip, val)
	var constant value
	if bytecode.IsConstant(val) {
		constant = c.p.Constants[bytecode.ConstantIndex(val)]
	}
	notF64, notF32, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Cbnz(rTmp, notF64)
	a.AddShifted(rTmp2, rTmp2, key, 3)
	switch {
	case constant.isNumber():
		a.MovImm(rTmp, math.Float64bits(constant.toFloat()))
		a.Str(rTmp, rTmp2, 0)
	case kind == kindInt:
		a.Scvtf(0, k.ireg(val))
		a.StrD(0, rTmp2, 0)
	default:
		a.StrD(k.reg(val), rTmp2, 0)
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
	case kind == kindInt:
		a.Scvtf(0, k.ireg(val))
		a.FcvtDS(0, 0)
		a.StrS(0, rTmp2, 0)
	default:
		a.FcvtDS(0, k.reg(val))
		a.StrS(0, rTmp2, 0)
	}
	a.B(done)
	a.Bind(notF32) // an integer buffer: an integer value, or Go converts
	if kind != kindInt {
		a.B(side)
	} else {
		isU8 := a.NewLabel()
		a.CmpImm(rTmp, uint32(bufferUint8))
		a.BCond(EQ, isU8)
		r := rTmp
		if constant.isNumber() {
			a.MovImm(rTmp, uint64(constant.i()))
		} else {
			r = k.ireg(val)
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

// usesInt reports whether k keeps a Lua register in machine register r.
func (k *kernel) usesInt(r Reg) bool {
	for s, n := range k.slots {
		if s.t == kindInt && kernelInts[n] == r {
			return true
		}
	}
	return false
}

// kernelStep is FORLOOP on registers: it advances the index and branches
// to take, having set the external index, or to end.
func (c *arm64Compiler) kernelStep(k *kernel, base int, take, end Label) {
	a := &c.a
	if k.intLoop {
		idx, count, step, ext := k.ireg(base), k.ireg(base+1), k.ireg(base+2), k.ireg(base+3)
		a.Cbz(count, end)
		a.SubImm(count, count, 1)
		a.Add(idx, idx, step)
		a.Mov(ext, idx)
		a.B(take)
		return
	}
	idx, limit, step, ext := k.reg(base), k.reg(base+1), k.reg(base+2), k.reg(base+3)
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
	a.Fmov(ext, 0)
	a.B(take)
}

// flush writes k's registers back to the frame, where they have types:
// those the loop writes that are defined there.
func (c *arm64Compiler) flush(k *kernel, types map[int]numKind) {
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

// floatOperand returns a floating-point register holding RK field as a
// float before ip: its kernel register, or tmp, into which it loads a
// constant or converts an integer.
func (c *arm64Compiler) floatOperand(k *kernel, ip, field int, tmp FReg) FReg {
	a := &c.a
	if !bytecode.IsConstant(field) {
		if k.typeAt(ip, field) == kindInt {
			a.Scvtf(tmp, k.ireg(field))
			return tmp
		}
		return k.reg(field)
	}
	kk := bytecode.ConstantIndex(field)
	if v := c.p.Constants[kk]; v.isInteger() {
		a.MovImm(rTmp, math.Float64bits(float64(v.i())))
		a.FmovToF(tmp, rTmp)
		return tmp
	}
	o, _ := c.constant(kk)
	a.LdrD(tmp, o.base, o.off+offN)
	return tmp
}

// intOperand returns a general-purpose register holding the integer RK
// field: its kernel register, or tmp, into which it loads a constant.
func (c *arm64Compiler) intOperand(k *kernel, field int, tmp Reg) Reg {
	if !bytecode.IsConstant(field) {
		return k.ireg(field)
	}
	o, _ := c.constant(bytecode.ConstantIndex(field))
	c.a.Ldr(tmp, o.base, o.off+offN)
	return tmp
}

// target returns the kernel label for pc t in k's body, or latch.
func (k *kernel) target(t int, latch Label) Label {
	if t == k.latch {
		return latch
	}
	return k.labels[t-k.start]
}

// kernelInstruction compiles the body instruction at ip for k and returns
// how many extra code words it consumed.
func (c *arm64Compiler) kernelInstruction(k *kernel, ip int, latch Label) int {
	a := &c.a
	i := c.p.Code[ip]
	p := c.p
	isInt := k.results[ip] == kindInt // what the instruction writes to A
	switch op := i.OpCode(); op {
	case bytecode.OpMove:
		if !isNumKind(k.results[ip]) {
			break // an alias of a hoisted buffer: nothing to move
		}
		if isInt {
			a.Mov(k.ireg(i.A()), k.ireg(i.B()))
		} else {
			a.Fmov(k.reg(i.A()), k.reg(i.B()))
		}
	case bytecode.OpLoadConstant:
		o, _ := c.constant(i.Bx())
		switch v := p.Constants[i.Bx()]; {
		case isInt:
			a.Ldr(k.ireg(i.A()), o.base, o.off+offN)
		case v.isInteger(): // promoted: see promoteConstants
			a.MovImm(rTmp, math.Float64bits(float64(v.i())))
			a.FmovToF(k.reg(i.A()), rTmp)
		default:
			a.LdrD(k.reg(i.A()), o.base, o.off+offN)
		}
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
		if isInt {
			b, cc, d := c.intOperand(k, i.B(), rTmp), c.intOperand(k, i.C(), rTmp2), k.ireg(i.A())
			switch op {
			case bytecode.OpAdd:
				a.Add(d, b, cc)
			case bytecode.OpSub:
				a.Sub(d, b, cc)
			case bytecode.OpMul:
				a.Mul(d, b, cc)
			}
			break
		}
		b, cc, d := c.floatOperand(k, ip, i.B(), 0), c.floatOperand(k, ip, i.C(), 1), k.reg(i.A())
		switch op {
		case bytecode.OpAdd:
			a.Fadd(d, b, cc)
		case bytecode.OpSub:
			a.Fsub(d, b, cc)
		case bytecode.OpMul:
			a.Fmul(d, b, cc)
		case bytecode.OpDiv:
			a.Fdiv(d, b, cc)
		}
	case bytecode.OpBitwise:
		op := bytecode.ArithOp(p.Code[ip+1].Ax())
		b, d := c.intOperand(k, i.B(), rTmp), k.ireg(i.A())
		switch op {
		case bytecode.ArithBAnd:
			a.And(d, b, c.intOperand(k, i.C(), rTmp2))
		case bytecode.ArithBOr:
			a.Orr(d, b, c.intOperand(k, i.C(), rTmp2))
		case bytecode.ArithBXor:
			a.Eor(d, b, c.intOperand(k, i.C(), rTmp2))
		case bytecode.ArithBNot:
			a.Mvn(d, b)
		case bytecode.ArithShl, bytecode.ArithShr:
			n := p.Constants[bytecode.ConstantIndex(i.C())].i()
			switch count, zero := constantShift(n, op == bytecode.ArithShr); {
			case zero:
				a.Mov(d, ZR)
			case count >= 0:
				a.MovImm(rTmp2, uint64(count))
				a.Lslv(d, b, rTmp2)
			default:
				a.MovImm(rTmp2, uint64(-count))
				a.Lsrv(d, b, rTmp2)
			}
		}
		return 1 // the operator's word
	case bytecode.OpIDiv, bytecode.OpMod:
		if op == bytecode.OpIDiv && !isInt { // floats: floor(b / c)
			b, cc, d := c.floatOperand(k, ip, i.B(), 0), c.floatOperand(k, ip, i.C(), 1), k.reg(i.A())
			a.Fdiv(d, b, cc)
			a.Frintm(d, d)
			break
		}
		if !isInt { // floats, by a floatModDivisor
			if b := c.floatOperand(k, ip, i.B(), 0); b != 0 {
				a.Fmov(0, b)
			}
			d, _ := floatModDivisor(p.Constants[bytecode.ConstantIndex(i.C())])
			c.floatMod(d, c.kernelSideExit(k, ip))
			a.Fmov(k.reg(i.A()), 3)
			break
		}
		// By a nonzero constant: floor the quotient toward minus infinity,
		// and give the modulo the divisor's sign.
		divisor := p.Constants[bytecode.ConstantIndex(i.C())].i()
		b, d := k.ireg(i.B()), k.ireg(i.A())
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
	case bytecode.OpUnaryMinus:
		if isInt {
			a.Neg(k.ireg(i.A()), k.ireg(i.B()))
		} else {
			a.Fneg(k.reg(i.A()), k.reg(i.B()))
		}
	case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
		t, _ := kernelJump(p.Code, ip, k.latch)
		var when Cond
		if k.kind(p, ip, i.B()) == kindInt && k.kind(p, ip, i.C()) == kindInt {
			a.Cmp(c.intOperand(k, i.B(), rTmp), c.intOperand(k, i.C(), rTmp2))
			when = map[bytecode.OpCode]Cond{bytecode.OpEqual: EQ, bytecode.OpLessThan: LT, bytecode.OpLessOrEqual: LE}[op]
		} else {
			a.Fcmp(c.floatOperand(k, ip, i.B(), 0), c.floatOperand(k, ip, i.C(), 1))
			when = map[bytecode.OpCode]Cond{bytecode.OpEqual: EQ, bytecode.OpLessThan: MI, bytecode.OpLessOrEqual: LS}[op]
		}
		if i.A() == 0 {
			when = negate(when)
		}
		a.BCond(when, k.target(t, latch))
		a.B(k.target(ip+2, latch))
		return 1
	case bytecode.OpJump:
		t, _ := kernelJump(p.Code, ip, k.latch)
		a.B(k.target(t, latch))
	case bytecode.OpGetUpValue:
		// The function an intrinsic call checked on entry, or a buffer the
		// context holds: nothing. A number, from the context.
		if s, ok := k.upLoads[ip]; ok && isNumKind(k.results[ip]) {
			if isInt {
				a.Ldr(k.ireg(i.A()), rCtx, offHoist+uint32(s)*8)
			} else {
				a.LdrD(k.reg(i.A()), rCtx, offHoist+uint32(s)*8)
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
