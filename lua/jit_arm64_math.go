//go:build (darwin || linux) && arm64

package lua

import (
	"math"
	"unsafe"

	. "github.com/matjam/apogee/internal/jit/arm64"
)

// intrinsics are the unary number functions compiled inline, by the
// address of their func value, which is fixed for a top-level function.
// Each computes d0 from d0 bit for bit as the Go function does, and may
// exit at ip for arguments it leaves to Go.
var intrinsics = []struct {
	fn   uint64
	emit func(c *arm64Compiler, ip int)
}{
	// floor, ceil and abs return integers for integers, so they are no
	// longer number functions of floats.
	{funcValue(math.Sqrt), func(c *arm64Compiler, ip int) { c.a.Fsqrt(0, 0) }},
	{funcValue(math.Sin), func(c *arm64Compiler, ip int) { c.trig(ip, false) }},
	{funcValue(math.Cos), func(c *arm64Compiler, ip int) { c.trig(ip, true) }},
}

func funcValue(f func(float64) float64) uint64 { return uint64(*(*uintptr)(unsafe.Pointer(&f))) }

// trigInline reports whether compiled code computes sin and cos itself.
const trigInline = true

// rTrig holds &trigTable while sin or cos runs.
const rTrig = rT2

// tconst loads the constant at offset off in trigTable into f.
func (c *arm64Compiler) tconst(f FReg, off uint32) { c.a.LdrD(f, rTrig, off) }

// trig computes math.Sin, or math.Cos, of d0 into d0. It follows the code
// Go compiles for math/sin.go on arm64, including which multiply-adds it
// fuses, so results match bit for bit; TestJITTrigMatchesGo checks that.
// Arguments of 2^29 and more, which Go reduces with trigReduce, exit at
// ip; infinities, and NaN for cos, give Go's NaN, as math.Sin and Cos
// return. It uses D0 to D6, which kernels leave free, and rTrig, rIdx and
// rLen, which a kernel saves around it.
func (c *arm64Compiler) trig(ip int, cos bool) {
	a := &c.a
	exit := c.intrinsicExit(ip)
	done, nan := a.NewLabel(), a.NewLabel()
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(nan)
		c.tconst(0, offTrigNaN)
		a.B(done)
	})
	a.MovImm(rTrig, trigTableAddr())
	a.Fcmp(0, 0)
	if cos {
		a.BCond(VS, nan) // NaN
	} else {
		a.BCond(VS, done) // sin(NaN) is its argument
		a.FmovToF(6, ZR)
		a.Fcmp(0, 6)
		a.BCond(EQ, done) // and so is sin(±0)
		a.FmovFromF(rLen, 0)
	}
	a.Fabs(1, 0)
	c.tconst(2, offTrigInf)
	a.Fcmp(1, 2)
	a.BCond(EQ, nan)
	c.tconst(2, offTrigLimit) // reduceThreshold
	a.Fcmp(1, 2)
	a.BCond(GE, exit)
	// j = uint64(x * (4/Pi)); y = float64(j); if j is odd, j++ and y++.
	c.tconst(2, offTrigFour)
	a.Fmul(2, 1, 2)
	a.Fcvtzu(rIdx, 2)
	a.Ucvtf(2, rIdx)
	even := a.NewLabel()
	a.Tbz(rIdx, 0, even)
	a.AddImm(rIdx, rIdx, 1)
	c.tconst(3, offTrigOne)
	a.Fadd(2, 2, 3)
	a.Bind(even)
	// z = ((x - y*PI4A) - y*PI4B) - y*PI4C, each step fused.
	for k := range uint32(3) {
		c.tconst(4, offTrigPI4+8*k)
		a.Fmsub(1, 2, 4, 1)
	}
	a.Fmul(2, 1, 1) // zz
	// j is even, so j&7 > 3 is bit 2, and after taking 4 off, j == 1 || j
	// == 2 is bit 1. Bit 1 picks the sine series for cos and the cosine
	// series for sin.
	other, signs := a.NewLabel(), a.NewLabel()
	a.Tbnz(rIdx, 1, other)
	if cos {
		c.cosSeries()
	} else {
		c.sinSeries()
	}
	a.B(signs)
	a.Bind(other)
	if cos {
		c.sinSeries()
	} else {
		c.cosSeries()
	}
	a.Bind(signs)
	flipped := a.NewLabel()
	a.Tbz(rIdx, 2, flipped)
	a.Fneg(0, 0)
	a.Bind(flipped)
	if cos {
		a.Tbz(rIdx, 1, done)
	} else {
		a.Tbz(rLen, 63, done) // x was negative
	}
	a.Fneg(0, 0)
	a.Bind(done)
}

// sinSeries computes d0 = z + z*zz*P(zz) from z in d1 and zz in d2.
func (c *arm64Compiler) sinSeries() {
	c.a.Fmul(3, 1, 2)
	c.series(offTrigSin)
	c.a.Fmadd(0, 3, 2, 1)
}

// cosSeries computes d0 = 1 - 0.5*zz + zz*zz*Q(zz) from zz in d2.
func (c *arm64Compiler) cosSeries() {
	c.tconst(5, offTrigHalf)
	c.tconst(6, offTrigOne)
	c.a.Fmsub(1, 2, 5, 6)
	c.a.Fmul(3, 2, 2)
	c.series(offTrigCos)
	c.a.Fmadd(0, 3, 2, 1)
}

// series evaluates the six-term polynomial at offset off in trigTable at
// zz, in d2, by Horner's rule with fused steps, leaving the result in d2.
func (c *arm64Compiler) series(off uint32) {
	c.tconst(4, off)
	for k := uint32(1); k < 5; k++ {
		c.tconst(5, off+8*k)
		c.a.Fmadd(4, 2, 4, 5)
	}
	c.tconst(5, off+40)
	c.a.Fmadd(2, 2, 4, 5)
}
