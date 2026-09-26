//go:build (darwin || linux) && arm64

package lua

import (
	"math"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

// divide compiles % and // of two integers, as IntMod and IntFloorDiv
// compute them, // of floats, and % of floats by a constant
// floatModDivisor accepts. A zero divisor exits, for Go to raise the
// error, as does % of floats by anything else, which Go computes with
// fmod.
func (c *arm64Compiler) divide(ip int, op bytecode.OpCode, i bytecode.Instruction) {
	a := &c.a
	b, kb, okB := c.rkArith(i.B())
	cc, kc, okC := c.rkArith(i.C())
	var modBy float64
	floatMod := false
	if op == bytecode.OpMod && bytecode.IsConstant(i.C()) {
		modBy, floatMod = floatModDivisor(c.p.Constants[bytecode.ConstantIndex(i.C())])
	}
	floatsExit := op == bytecode.OpMod && !floatMod
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
	if !floatsExit {
		a.Bind(floats)
		c.loadFloat(0, b, kb, false, ip)
		if op == bytecode.OpMod {
			c.floatMod(modBy, c.exit(ip))
			c.storeNumber(dst, 3)
		} else {
			c.loadFloat(1, cc, kc, false, ip)
			a.Fdiv(0, 0, 1)
			a.Frintm(0, 0)
			c.storeNumber(dst, 0)
		}
	}
	a.Bind(done)
}

// floatMod computes D0 % d into D3, as FloatMod does, for d that
// floatModDivisor accepts: fmod exactly, as a - trunc(a / d) * d, with the
// sign of a when it is zero, then Lua's correction toward d's sign. An
// infinite or NaN a goes to nonFinite: Go's NaN may have another sign
// bit. It uses D1 to D4 and rTmp, which kernels leave free.
func (c *arm64Compiler) floatMod(d float64, nonFinite Label) {
	a := &c.a
	a.MovImm(rTmp, math.Float64bits(d))
	a.FmovToF(1, rTmp)
	a.Fdiv(2, 0, 1)
	a.Frintz(2, 2)
	a.Fmul(2, 2, 1)
	a.Fsub(3, 0, 2)
	a.Fcmp(3, 3)
	a.BCond(VS, nonFinite) // NaN only from an infinite or NaN a
	a.FmovToF(4, ZR)
	nonzero, done := a.NewLabel(), a.NewLabel()
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
	a.Bind(done)
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
