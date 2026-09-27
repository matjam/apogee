//go:build (darwin || linux) && amd64

package lua

import (
	"math"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/amd64"
)

// divide compiles % and // of two integers, as IntMod and IntFloorDiv
// compute them, // of floats where ROUNDSD is available, and % of floats:
// by a constant floatModDivisor accepts in a few instructions with
// ROUNDSD, and otherwise with math.Mod's steps (floatModAny). A zero
// divisor exits, for Go to raise the error. IDIV divides RDX:RAX, so
// rAddr and rTmp are used.
func (c *amd64Compiler) divide(ip int, op bytecode.OpCode, i bytecode.Instruction) {
	a := &c.a
	b, kb, okB := c.rkArith(i.B())
	cc, kc, okC := c.rkArith(i.C())
	var modBy float64
	floatMod := false
	if op == bytecode.OpMod && bytecode.IsConstant(i.C()) && c.sse41 {
		modBy, floatMod = floatModDivisor(c.p.Constants[bytecode.ConstantIndex(i.C())])
	}
	floatsExit := op == bytecode.OpIDiv && !c.sse41
	if !okB || !okC || floatsExit && (kb == kindFloat || kc == kindFloat) {
		c.exitAlways(ip)
		return
	}
	dst := reg(i.A())
	c.guardStore(dst, noReg, ip)
	floats, done := a.NewLabel(), a.NewLabel()
	if floatsExit {
		floats = c.exit(ip)
	}
	if plan, ok := constantDivisor(c.p, i.C()); ok && kb != kindFloat {
		c.branchUnlessInteger(b, kb, floats)
		a.Load(rP, b.base, b.off+offN)
		c.storeInteger(dst, c.divideByConstant(op, plan, rP))
		a.Jmp(done)
	} else if kb != kindFloat && kc != kindFloat {
		c.branchUnlessInteger(b, kb, floats)
		c.branchUnlessInteger(cc, kc, floats)
		a.Load(rP, b.base, b.off+offN)
		a.Load(rN, cc.base, cc.off+offN)
		a.Test(rN, rN)
		a.J(E, c.exit(ip)) // 'n%0' or 'n//0'
		minusOne, adjust := a.NewLabel(), a.NewLabel()
		a.CmpImm(rN, -1)
		a.J(E, minusOne) // IDIV would trap on minint / -1
		a.Mov(AX, rP)
		a.Cqo()
		a.Idiv(rN)     // AX: the quotient, toward zero; DX: the remainder
		a.Test(DX, DX) // exact: nothing to adjust
		a.J(E, adjust)
		a.Mov(rTmp2, DX) // signs differ: floor is one lower, and the
		a.Xor(rTmp2, rN) // modulo takes the divisor's sign
		a.J(NS, adjust)
		if op == bytecode.OpMod {
			a.Add(DX, rN)
		} else {
			a.SubImm(AX, 1)
		}
		a.Bind(adjust)
		if op == bytecode.OpMod {
			c.storeInteger(dst, DX)
		} else {
			c.storeInteger(dst, AX)
		}
		a.Jmp(done)
		a.Bind(minusOne) // n % -1 is 0 and n // -1 is -n, wrapping for minint
		if op == bytecode.OpMod {
			a.MovImm(rP, 0)
		} else {
			a.Neg(rP)
		}
		c.storeInteger(dst, rP)
		a.Jmp(done)
	}
	if !floatsExit {
		a.Bind(floats)
		c.loadFloat(0, b, kb, false, ip)
		switch {
		case floatMod:
			c.floatMod(modBy)
			c.storeNumber(dst, 3)
		case op == bytecode.OpMod:
			c.loadFloat(1, cc, kc, false, ip)
			c.floatModAny(c.exit(ip))
			c.storeNumber(dst, 3)
		default:
			c.loadFloat(1, cc, kc, false, ip)
			a.DivSD(0, 1)
			a.RoundSD(0, 0, 1) // toward minus infinity
			c.storeNumber(dst, 0)
		}
	}
	a.Bind(done)
}

// floatMod computes X0 % d into X3, as FloatMod does, for d that
// floatModDivisor accepts: fmod exactly, as a - trunc(a / d) * d, with the
// sign of a when it is zero, then Lua's correction toward d's sign. An
// infinite or NaN a gives Go's NaN, as math.Mod does. It uses X1 to X4
// and rTmp, which kernels leave free.
func (c *amd64Compiler) floatMod(d float64) {
	a := &c.a
	a.MovImm(rTmp, math.Float64bits(d))
	a.MovqToX(1, rTmp)
	a.MovSD(2, 0)
	a.DivSD(2, 1)
	a.RoundSD(2, 2, 3) // toward zero
	a.MulSD(2, 1)
	a.MovSD(3, 0)
	a.SubSD(3, 2)
	nonzero, done, nan := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Ucomisd(3, 3)
	a.J(P, nan) // NaN only from an infinite or NaN a
	a.XorPD(4, 4)
	a.Ucomisd(3, 4)
	a.J(NE, nonzero)
	a.J(P, nonzero)
	a.MovSD(3, 0)
	a.MulSD(3, 4) // a zero with a's sign, as fmod gives
	a.Bind(nonzero)
	a.Ucomisd(3, 4)
	if d > 0 {
		a.J(AE, done) // add d to a negative m
	} else {
		a.J(BE, done) // add d to a positive m
	}
	a.AddSD(3, 1)
	a.Jmp(done)
	a.Bind(nan)
	c.goNaN(3)
	a.Bind(done)
}

// floatModAny computes X0 % X1 into X3, as FloatMod does, with math.Mod's
// own steps: while r = |a| >= |b|, subtract |b| scaled by the power of
// two that leaves r nonnegative, each step exact. Positive floats order
// as their bits do, so it works on those; the scaling adds to the
// exponent. An infinite or NaN a, and a zero or NaN b, give Go's NaN, as
// math.Mod does; an infinite b leaves a. A subnormal b goes to other,
// for Go. It uses X2 to X4, rTmp, rTmp2, rP, rN, rT, rT2 and rIdx.
func (c *amd64Compiler) floatModAny(other Label) {
	a := &c.a
	a.MovqFromX(rP, 0) // a's bits, for its sign
	a.MovqFromX(rT2, 1)
	a.MovImm(rTmp, 1<<63-1)
	a.And(rT2, rTmp) // |b|
	a.Mov(rN, rP)
	a.And(rN, rTmp) // r = |a|
	loop, out, nan, finite := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Mov(rIdx, rN)
	a.Shr(rIdx, 52)
	a.CmpImm(rIdx, 0x7ff)
	a.J(E, nan) // a is infinite or NaN
	a.Test(rT2, rT2)
	a.J(E, nan) // b is zero
	a.Mov(rT, rT2)
	a.Shr(rT, 52) // b's exponent
	a.CmpImm(rT, 0x7ff)
	a.J(NE, finite)
	a.MovImm(rTmp, 0x7ff<<52)
	a.Cmp(rT2, rTmp)
	a.J(NE, nan)
	a.Jmp(out) // b is infinite: r is |a|
	a.Bind(finite)
	a.CmpImm(rT, 0)
	a.J(E, other) // b is subnormal
	a.Bind(loop)
	a.Cmp(rN, rT2)
	a.J(B, out)
	a.Mov(rIdx, rN)
	a.Shr(rIdx, 52)
	a.Sub(rIdx, rT) // r's exponent over b's
	a.Mov(rTmp, rN)
	a.Shl(rTmp, 12)
	a.Mov(rTmp2, rT2)
	a.Shl(rTmp2, 12)
	whole := a.NewLabel()
	a.Cmp(rTmp, rTmp2)
	a.J(AE, whole)
	a.SubImm(rIdx, 1) // r's significand is less than b's
	a.Bind(whole)
	a.Shl(rIdx, 52)
	a.Add(rIdx, rT2) // |b| * 2^n
	a.MovqToX(2, rN)
	a.MovqToX(3, rIdx)
	a.SubSD(2, 3)
	a.MovqFromX(rN, 2)
	a.Jmp(loop)
	a.Bind(out)
	a.MovImm(rTmp, 1<<63)
	a.And(rP, rTmp)
	a.Or(rN, rP) // a's sign
	a.MovqToX(3, rN)
	// Lua's correction: m takes b's sign.
	a.XorPD(4, 4)
	done, positive := a.NewLabel(), a.NewLabel()
	a.Ucomisd(3, 4)
	a.J(E, done)
	a.J(A, positive)
	a.Ucomisd(1, 4)
	a.J(BE, done) // m < 0: add a positive b
	a.AddSD(3, 1)
	a.Jmp(done)
	a.Bind(positive)
	a.Ucomisd(1, 4)
	a.J(AE, done) // m > 0: add a negative b
	a.AddSD(3, 1)
	a.Jmp(done)
	a.Bind(nan)
	c.goNaN(3)
	a.Bind(done)
}

// goNaN loads math.NaN(), which math.Mod returns and x86 arithmetic does
// not, into x. It uses rTmp.
func (c *amd64Compiler) goNaN(x XReg) {
	c.a.MovImm(rTmp, math.Float64bits(math.NaN()))
	c.a.MovqToX(x, rTmp)
}

// divideByConstant computes n % d or n // d, floored, for the constant d
// that plan describes (see jit_divide.go), and returns the register that
// holds the result: AX or DX, which it uses, so n must be neither.
func (c *amd64Compiler) divideByConstant(op bytecode.OpCode, plan divPlan, n Reg) Reg {
	a := &c.a
	if plan.pow2 {
		a.Mov(AX, n)
		if op == bytecode.OpMod {
			a.AndImm(AX, int32(plan.d-1))
		} else {
			a.Sar(AX, uint8(plan.shift))
		}
		return AX
	}
	a.MovImm(AX, uint64(plan.magic))
	a.ImulWide(n) // DX: the high half of n * magic
	if plan.addN {
		a.Add(DX, n)
	} else if plan.subN {
		a.Sub(DX, n)
	}
	if plan.shift > 0 {
		a.Sar(DX, uint8(plan.shift))
	}
	if plan.d > 0 { // plus one for a negative quotient: n's sign, or q's
		a.Mov(AX, n)
	} else {
		a.Mov(AX, DX)
	}
	a.Shr(AX, 63)
	a.Add(DX, AX) // the quotient, toward zero
	a.ImulImm(AX, DX, int32(plan.d))
	a.Neg(AX)
	a.Add(AX, n) // the remainder, with n's sign
	adjust := a.NewLabel()
	a.Test(AX, AX)
	a.J(E, adjust) // exact
	if plan.d > 0 {
		a.J(NS, adjust)
	} else {
		a.J(S, adjust)
	}
	if op == bytecode.OpMod { // signs differ: one step toward minus infinity
		a.AddImm(AX, int32(plan.d))
	} else {
		a.SubImm(DX, 1)
	}
	a.Bind(adjust)
	if op == bytecode.OpMod {
		return AX
	}
	return DX
}

// bitwise compiles a bitwise operator on two integers, or one for ~. A
// float operand exits, for Go to convert it or raise the error. Shifts
// count in CL, which is rTmp2.
func (c *amd64Compiler) bitwise(ip int, i bytecode.Instruction, op bytecode.ArithOp) {
	a := &c.a
	b, kb, okB := c.rkArith(i.B())
	cc, kc, okC := b, kb, okB
	if op != bytecode.ArithBNot {
		cc, kc, okC = c.rkArith(i.C())
	}
	if !okB || !okC || kb == kindFloat || kc == kindFloat {
		c.exitAlways(ip)
		return
	}
	dst := reg(i.A())
	c.guardStore(dst, noReg, ip)
	c.branchUnlessInteger(b, kb, c.exit(ip))
	c.branchUnlessInteger(cc, kc, c.exit(ip))
	a.Load(rP, b.base, b.off+offN)
	a.Load(rN, cc.base, cc.off+offN)
	switch op {
	case bytecode.ArithBAnd:
		a.And(rP, rN)
	case bytecode.ArithBOr:
		a.Or(rP, rN)
	case bytecode.ArithBXor:
		a.Xor(rP, rN)
	case bytecode.ArithBNot:
		a.Not(rP)
	case bytecode.ArithShl, bytecode.ArithShr:
		// ShiftLeft: logical, the other way for a negative count, and 0
		// for a count of 64 or more either way.
		if op == bytecode.ArithShr {
			a.Neg(rN)
		}
		left, right, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.CmpImm(rN, 64)
		a.J(B, left)
		a.Mov(rTmp2, rN)
		a.Neg(rTmp2)
		a.CmpImm(rTmp2, 64)
		a.J(B, right)
		a.MovImm(rP, 0)
		a.Jmp(done)
		a.Bind(left)
		a.Mov(rTmp2, rN)
		a.ShlCL(rP)
		a.Jmp(done)
		a.Bind(right)
		a.ShrCL(rP)
		a.Bind(done)
	}
	c.storeInteger(dst, rP)
}
