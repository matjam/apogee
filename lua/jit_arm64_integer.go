//go:build (darwin || linux) && arm64

package lua

import (
	"math"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

// divide compiles % and // of two integers, as IntMod and IntFloorDiv
// compute them, and // and % of floats: by a constant floatModDivisor
// accepts in a few instructions, and by anything else with math.Mod's
// steps (floatModAny). A zero divisor exits, for Go to raise the error.
func (c *arm64Compiler) divide(ip int, op bytecode.OpCode, i bytecode.Instruction) {
	a := &c.a
	b, kb, okB := c.rkArith(i.B())
	cc, kc, okC := c.rkArith(i.C())
	var modBy float64
	floatMod := false
	if op == bytecode.OpMod && bytecode.IsConstant(i.C()) {
		modBy, floatMod = floatModDivisor(c.p.Constants[bytecode.ConstantIndex(i.C())])
	}
	if !okB || !okC {
		c.exitAlways(ip)
		return
	}
	dst := reg(i.A())
	c.guardStore(dst, noReg, ip)
	floats, done := a.NewLabel(), a.NewLabel()
	if plan, ok := constantDivisor(c.p, i.C()); ok && kb != kindFloat {
		c.branchUnlessInteger(b, kb, floats)
		a.Ldr(rP, b.base, b.off+offN)
		c.storeInteger(dst, c.divideByConstant(op, plan, rP))
		a.B(done)
	} else if kb != kindFloat && kc != kindFloat {
		c.branchUnlessInteger(b, kb, floats)
		c.branchUnlessInteger(cc, kc, floats)
		a.Ldr(rP, b.base, b.off+offN)
		a.Ldr(rN, cc.base, cc.off+offN)
		a.Cbz(rN, c.exit(ip)) // 'n%0' or 'n//0'
		minusOne, adjust := a.NewLabel(), a.NewLabel()
		a.AddImm(rTmp, rN, 1)
		a.Cbz(rTmp, minusOne)
		a.Sdiv(rIdx, rP, rN)       // the quotient, toward zero
		a.Msub(rTmp, rIdx, rN, rP) // the remainder, with the dividend's sign
		a.Cbz(rTmp, adjust)        // exact: nothing to adjust
		a.Eor(rTmp2, rTmp, rN)     // signs differ: floor is one lower,
		a.Tbz(rTmp2, 63, adjust)   // and the modulo takes the divisor's sign
		if op == bytecode.OpMod {
			a.Add(rTmp, rTmp, rN)
		} else {
			a.SubImm(rIdx, rIdx, 1)
		}
		a.Bind(adjust)
		if op == bytecode.OpMod {
			c.storeInteger(dst, rTmp)
		} else {
			c.storeInteger(dst, rIdx)
		}
		a.B(done)
		a.Bind(minusOne) // n % -1 is 0 and n // -1 is -n, wrapping for minint
		if op == bytecode.OpMod {
			c.storeInteger(dst, ZR)
		} else {
			a.Neg(rP, rP)
			c.storeInteger(dst, rP)
		}
		a.B(done)
	}
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
		a.Fdiv(0, 0, 1)
		a.Frintm(0, 0)
		c.storeNumber(dst, 0)
	}
	a.Bind(done)
}

// floatModAny computes D0 % D1 into D3, as FloatMod does, with math.Mod's
// own steps: while r = |a| >= |b|, subtract |b| scaled by the power of
// two that leaves r nonnegative, each step exact. Positive floats order
// as their bits do, so it works on those; the scaling adds to the
// exponent. An infinite or NaN a, and a zero or NaN b, give Go's NaN, as
// math.Mod does; an infinite b leaves a. A subnormal b goes to other,
// for Go. It uses D2 to D4, rTmp, rTmp2, rP, rN, rT, rT2 and rIdx.
func (c *arm64Compiler) floatModAny(other Label) {
	a := &c.a
	a.FmovFromF(rP, 0) // a's bits, for its sign
	a.FmovFromF(rT2, 1)
	a.MovImm(rTmp, 1<<63-1)
	a.And(rT2, rT2, rTmp) // |b|
	a.And(rN, rP, rTmp)   // r = |a|
	loop, out, nan, finite := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Lsr(rIdx, rN, 52)
	a.CmpImm(rIdx, 0x7ff)
	a.BCond(EQ, nan)   // a is infinite or NaN
	a.Cbz(rT2, nan)    // b is zero
	a.Lsr(rT, rT2, 52) // b's exponent
	a.CmpImm(rT, 0x7ff)
	a.BCond(NE, finite)
	a.MovImm(rTmp, 0x7ff<<52)
	a.Cmp(rT2, rTmp)
	a.BCond(NE, nan)
	a.B(out) // b is infinite: r is |a|
	a.Bind(finite)
	a.Cbz(rT, other) // b is subnormal
	a.Bind(loop)
	a.Cmp(rN, rT2)
	a.BCond(LO, out)
	a.Lsr(rIdx, rN, 52)
	a.Sub(rIdx, rIdx, rT) // r's exponent over b's
	a.AddShifted(rTmp, ZR, rN, 12)
	a.AddShifted(rTmp2, ZR, rT2, 12)
	whole := a.NewLabel()
	a.Cmp(rTmp, rTmp2)
	a.BCond(HS, whole)
	a.SubImm(rIdx, rIdx, 1) // r's significand is less than b's
	a.Bind(whole)
	a.AddShifted(rIdx, rT2, rIdx, 52) // |b| * 2^n
	a.FmovToF(2, rN)
	a.FmovToF(3, rIdx)
	a.Fsub(2, 2, 3)
	a.FmovFromF(rN, 2)
	a.B(loop)
	a.Bind(out)
	a.MovImm(rTmp, 1<<63)
	a.And(rP, rP, rTmp)
	a.Orr(rN, rN, rP) // a's sign
	a.FmovToF(3, rN)
	// Lua's correction: m takes b's sign.
	a.FmovToF(4, ZR)
	done, positive := a.NewLabel(), a.NewLabel()
	a.Fcmp(3, 4)
	a.BCond(EQ, done)
	a.BCond(GT, positive)
	a.Fcmp(1, 4)
	a.BCond(LS, done) // m < 0: add a positive b
	a.Fadd(3, 3, 1)
	a.B(done)
	a.Bind(positive)
	a.Fcmp(1, 4)
	a.BCond(GE, done) // m > 0: add a negative b
	a.Fadd(3, 3, 1)
	a.B(done)
	a.Bind(nan)
	c.goNaN(3)
	a.Bind(done)
}

// floatMod computes D0 % d into D3, as FloatMod does, for d that
// floatModDivisor accepts: fmod exactly, as a - trunc(a / d) * d, with the
// sign of a when it is zero, then Lua's correction toward d's sign. An
// infinite or NaN a gives Go's NaN, as math.Mod does. It uses D1 to D4
// and rTmp, which kernels leave free.
func (c *arm64Compiler) floatMod(d float64) {
	a := &c.a
	a.MovImm(rTmp, math.Float64bits(d))
	a.FmovToF(1, rTmp)
	a.Fdiv(2, 0, 1)
	a.Frintz(2, 2)
	a.Fmul(2, 2, 1)
	a.Fsub(3, 0, 2)
	nonzero, done, nan := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Fcmp(3, 3)
	a.BCond(VS, nan) // NaN only from an infinite or NaN a
	a.FmovToF(4, ZR)
	a.Fcmp(3, 4)
	a.BCond(NE, nonzero) // unordered too
	a.Fmul(3, 0, 4)      // a zero with a's sign, as fmod gives
	a.Bind(nonzero)
	a.Fcmp(3, 4)
	if d > 0 {
		a.BCond(GE, done) // add d to a negative m
	} else {
		a.BCond(LS, done) // add d to a positive m
	}
	a.Fadd(3, 3, 1)
	a.B(done)
	a.Bind(nan)
	c.goNaN(3)
	a.Bind(done)
}

// goNaN loads math.NaN(), which math.Mod returns and arm64 arithmetic
// does not, into d. It uses rTmp.
func (c *arm64Compiler) goNaN(d FReg) {
	c.a.MovImm(rTmp, math.Float64bits(math.NaN()))
	c.a.FmovToF(d, rTmp)
}

// divideByConstant computes n % d or n // d, floored, for the constant d
// that plan describes (see jit_divide.go), and returns the register that
// holds the result. It uses rTmp, rTmp2 and rExitPC, so n must be none of
// them.
func (c *arm64Compiler) divideByConstant(op bytecode.OpCode, plan divPlan, n Reg) Reg {
	a := &c.a
	if plan.pow2 {
		if op == bytecode.OpMod {
			a.MovImm(rTmp2, uint64(plan.d-1))
			a.And(rTmp, n, rTmp2)
		} else {
			a.Asr(rTmp, n, uint32(plan.shift))
		}
		return rTmp
	}
	q, d, r := rTmp, rTmp2, rExitPC
	a.MovImm(d, uint64(plan.magic))
	a.Smulh(q, n, d)
	if plan.addN {
		a.Add(q, q, n)
	} else if plan.subN {
		a.Sub(q, q, n)
	}
	if plan.shift > 0 {
		a.Asr(q, q, uint32(plan.shift))
	}
	if plan.d > 0 { // plus one for a negative quotient: n's sign, or q's
		a.Lsr(d, n, 63)
	} else {
		a.Lsr(d, q, 63)
	}
	a.Add(q, q, d) // the quotient, toward zero
	a.MovImm(d, uint64(plan.d))
	a.Msub(r, q, d, n) // the remainder, with n's sign
	adjust := a.NewLabel()
	a.Cbz(r, adjust) // exact
	if plan.d > 0 {
		a.Tbz(r, 63, adjust)
	} else {
		a.Tbnz(r, 63, adjust)
	}
	if op == bytecode.OpMod { // signs differ: one step toward minus infinity
		a.Add(r, r, d)
	} else {
		a.SubImm(q, q, 1)
	}
	a.Bind(adjust)
	if op == bytecode.OpMod {
		return r
	}
	return q
}

// bitwise compiles a bitwise operator on two integers, or one for ~. A
// float operand exits, for Go to convert it or raise the error.
func (c *arm64Compiler) bitwise(ip int, i bytecode.Instruction, op bytecode.ArithOp) {
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
	a.Ldr(rP, b.base, b.off+offN)
	a.Ldr(rN, cc.base, cc.off+offN)
	switch op {
	case bytecode.ArithBAnd:
		a.And(rN, rP, rN)
	case bytecode.ArithBOr:
		a.Orr(rN, rP, rN)
	case bytecode.ArithBXor:
		a.Eor(rN, rP, rN)
	case bytecode.ArithBNot:
		a.Mvn(rN, rP)
	case bytecode.ArithShl, bytecode.ArithShr:
		// ShiftLeft: logical, the other way for a negative count, and 0
		// for a count of 64 or more either way.
		if op == bytecode.ArithShr {
			a.Neg(rN, rN)
		}
		left, right, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.CmpImm(rN, 64)
		a.BCond(LO, left)
		a.Neg(rTmp, rN)
		a.CmpImm(rTmp, 64)
		a.BCond(LO, right)
		a.MovImm(rN, 0)
		a.B(done)
		a.Bind(left)
		a.Lslv(rN, rP, rN)
		a.B(done)
		a.Bind(right)
		a.Lsrv(rN, rP, rTmp)
		a.Bind(done)
	}
	c.storeInteger(dst, rN)
}
