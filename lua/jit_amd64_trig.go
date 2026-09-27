//go:build (darwin || linux) && amd64 && !amd64.v3

package lua

import (
	"math"

	. "github.com/matjam/apogee/internal/jit/amd64"
)

// Below GOAMD64=v3 Go compiles math/sin.go without fused multiply-adds,
// so compiled sin and cos follow it with one rounding per operation, in
// the source's order. At v3 Go fuses some of them, and sin and cos are
// left to Go.

func (c *amd64Compiler) trigIntrinsics() []intrinsic {
	return []intrinsic{
		{funcValue(math.Sin), func() { c.trig(false) }},
		{funcValue(math.Cos), func() { c.trig(true) }},
	}
}

// trigInline reports whether compiled code computes sin and cos itself.
const trigInline = true

// rTrig holds &trigTable while sin or cos runs.
const rTrig = rT2

// trig computes math.Sin, or math.Cos, of x0 into x0, exiting at the
// current instruction for arguments of 2^29 and more, which Go reduces
// with trigReduce. Infinities, and NaN for cos, give Go's NaN, as math.Sin
// and Cos return. Each operation is Go's,
// in Go's order; multiplying by a constant from memory rather than from
// a register rounds the same. It uses X0 to X5, which kernels leave free,
// and rTmp, rN, rTrig and rIdx, which a kernel saves around it.
func (c *amd64Compiler) trig(cos bool) {
	a := &c.a
	exit := c.intrinsicExit()
	done, nan := a.NewLabel(), a.NewLabel()
	c.outOfLine = append(c.outOfLine, func() { // Go's NaN, as math.Sin and Cos return
		a.Bind(nan)
		a.LoadSD(0, rTrig, offTrigNaN)
		a.Jmp(done)
	})
	a.MovImm(rTrig, trigTableAddr())
	a.Ucomisd(0, 0)
	if cos {
		a.J(P, nan)
	} else {
		a.J(P, done) // sin(NaN) is its argument
		a.XorPD(1, 1)
		a.Ucomisd(0, 1)
		a.J(E, done) // and so is sin(±0)
		a.MovqFromX(rN, 0)
	}
	a.MovSD(1, 0)
	a.MovImm(rTmp, 1<<63-1)
	a.MovqToX(2, rTmp)
	a.AndPD(1, 2) // x = |x|
	a.UcomisdMem(1, rTrig, offTrigInf)
	a.J(E, nan)
	a.UcomisdMem(1, rTrig, offTrigLimit)
	a.J(AE, exit)
	// j = uint64(x * (4/Pi)); y = float64(j); if j is odd, j++ and y++.
	a.MovSD(2, 1)
	a.MulSDMem(2, rTrig, offTrigFour)
	a.Cvttsd2si(rIdx, 2)
	c.toFloat(2, rIdx)
	even := a.NewLabel()
	a.Bt(rIdx, 0)
	a.J(AE, even)
	a.AddImm(rIdx, 1)
	a.AddSDMem(2, rTrig, offTrigOne)
	a.Bind(even)
	// z = ((x - y*PI4A) - y*PI4B) - y*PI4C
	for k := range uint32(3) {
		a.MovSD(4, 2)
		a.MulSDMem(4, rTrig, offTrigPI4+8*k)
		a.SubSD(1, 4)
	}
	a.MovSD(2, 1)
	a.MulSD(2, 1) // zz
	// j is even: bit 2 is j&7 > 3, and bit 1 picks the other series.
	other, signs := a.NewLabel(), a.NewLabel()
	a.Bt(rIdx, 1)
	a.J(B, other)
	if cos {
		c.cosSeries()
	} else {
		c.sinSeries()
	}
	a.Jmp(signs)
	a.Bind(other)
	if cos {
		c.sinSeries()
	} else {
		c.cosSeries()
	}
	a.Bind(signs)
	flipped := a.NewLabel()
	a.Bt(rIdx, 2)
	a.J(AE, flipped)
	c.signMask(5)
	a.XorPD(0, 5)
	a.Bind(flipped)
	if cos {
		a.Bt(rIdx, 1)
	} else {
		a.Bt(rN, 63) // x was negative
	}
	a.J(AE, done)
	c.signMask(5)
	a.XorPD(0, 5)
	a.Bind(done)
}

// sinSeries computes x0 = z + z*zz*P(zz) from z in x1 and zz in x2.
func (c *amd64Compiler) sinSeries() {
	a := &c.a
	a.MovSD(3, 1)
	a.MulSD(3, 2)
	c.series(offTrigSin)
	a.MulSD(3, 4)
	a.MovSD(0, 1)
	a.AddSD(0, 3)
}

// cosSeries computes x0 = 1 - 0.5*zz + zz*zz*Q(zz) from zz in x2.
func (c *amd64Compiler) cosSeries() {
	a := &c.a
	a.MovSD(3, 2)
	a.MulSDMem(3, rTrig, offTrigHalf)
	a.LoadSD(0, rTrig, offTrigOne)
	a.SubSD(0, 3)
	a.MovSD(3, 2)
	a.MulSD(3, 2)
	c.series(offTrigCos)
	a.MulSD(3, 4)
	a.AddSD(0, 3)
}

// series evaluates the six-term polynomial at offset off in trigTable at
// zz, in x2, by Horner's rule, leaving the result in x4.
func (c *amd64Compiler) series(off uint32) {
	c.a.LoadSD(4, rTrig, off)
	for k := uint32(1); k < 6; k++ {
		c.a.MulSD(4, 2)
		c.a.AddSDMem(4, rTrig, off+8*k)
	}
}
