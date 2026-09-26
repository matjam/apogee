package stdlib

import (
	"math"
	"math/bits"
	"time"
	"unsafe"

	"github.com/matjam/apogee/lua"
)

// The math library, after Lua 5.5's lmathlib.c.

const radiansPerDegree = math.Pi / 180.0

// mathNumberFunctions take and return floats whatever their arguments, so
// the VM and compiled code call them without a Go frame.
var mathNumberFunctions = []struct {
	name string
	f    func(float64) float64
}{
	{"acos", math.Acos},
	{"asin", math.Asin},
	{"cos", math.Cos},
	{"deg", func(x float64) float64 { return x / radiansPerDegree }},
	{"exp", math.Exp},
	{"rad", func(x float64) float64 { return x * radiansPerDegree }},
	{"sin", math.Sin},
	{"sqrt", math.Sqrt},
	{"tan", math.Tan},
}

// pushIntegerIfFits pushes f as an integer if it has an integral value an
// integer holds, and as a float otherwise, as lmathlib.c's pushnumint.
func pushIntegerIfFits(l *lua.State, f float64) {
	if f >= -(1<<63) && f < 1<<63 {
		l.PushInteger(int64(f))
		return
	}
	l.PushNumber(f)
}

var mathLibrary = []lua.RegistryFunction{
	{Name: "abs", Function: lua.MathAbs}, // which compiled code recognises, as floor, ceil, max and min
	{Name: "atan", Function: func(l *lua.State) int {
		y, x := l.CheckNumber(1), l.OptNumber(2, 1)
		l.PushNumber(math.Atan2(y, x))
		return 1
	}},
	{Name: "ceil", Function: lua.MathCeil},
	{Name: "floor", Function: lua.MathFloor},
	{Name: "fmod", Function: func(l *lua.State) int {
		if l.IsInteger(1) && l.IsInteger(2) {
			a, _ := l.ToInteger(1)
			d, _ := l.ToInteger(2)
			switch d {
			case 0:
				l.ArgumentError(2, "zero")
			case -1:
				l.PushInteger(0) // avoids overflowing minint % -1
			default:
				l.PushInteger(a % d) // C's %: the dividend's sign
			}
		} else {
			l.PushNumber(math.Mod(l.CheckNumber(1), l.CheckNumber(2)))
		}
		return 1
	}},
	{Name: "frexp", Function: func(l *lua.State) int { // back in Lua 5.5
		m, e := math.Frexp(l.CheckNumber(1))
		l.PushNumber(m)
		l.PushInteger(e)
		return 2
	}},
	{Name: "ldexp", Function: func(l *lua.State) int {
		x, e := l.CheckNumber(1), l.CheckInteger(2)
		l.PushNumber(math.Ldexp(x, int(int32(e)))) // C's (int) cast
		return 1
	}},
	{Name: "log", Function: func(l *lua.State) int {
		x := l.CheckNumber(1)
		switch {
		case l.IsNoneOrNil(2):
			l.PushNumber(math.Log(x))
		default:
			switch base := l.CheckNumber(2); base {
			case 2:
				l.PushNumber(math.Log2(x))
			case 10:
				l.PushNumber(math.Log10(x))
			default:
				l.PushNumber(math.Log(x) / math.Log(base))
			}
		}
		return 1
	}},
	{Name: "max", Function: lua.MathMax},
	{Name: "min", Function: lua.MathMin},
	{Name: "modf", Function: func(l *lua.State) int {
		if l.IsInteger(1) {
			l.SetTop(1)     // an integer is its own integral part
			l.PushNumber(0) // with no fraction
			return 2
		}
		n := l.CheckNumber(1)
		ip := math.Floor(n)
		if n < 0 {
			ip = math.Ceil(n)
		}
		pushIntegerIfFits(l, ip)
		if n == ip {
			l.PushNumber(0) // inf - inf would be NaN
		} else {
			l.PushNumber(n - ip)
		}
		return 2
	}},
	{Name: "tointeger", Function: func(l *lua.State) int {
		if n, ok := l.ToInteger(1); ok {
			l.PushInteger(n)
		} else {
			l.CheckAny(1)
			l.PushNil()
		}
		return 1
	}},
	{Name: "type", Function: func(l *lua.State) int {
		switch l.CheckAny(1); {
		case l.TypeOf(1) != lua.TypeNumber:
			l.PushNil()
		case l.IsInteger(1):
			l.PushString("integer")
		default:
			l.PushString("float")
		}
		return 1
	}},
	{Name: "ult", Function: func(l *lua.State) int {
		a, b := l.CheckInteger(1), l.CheckInteger(2)
		l.PushBoolean(uint64(a) < uint64(b))
		return 1
	}},
}

// A random is Lua 5.4's generator, xoshiro256**, with its four words of
// state.
type random struct{ s [4]uint64 }

// seed seeds the generator as lmathlib.c's setseed does, discarding the
// first values.
func (r *random) seed(n1, n2 uint64) {
	r.s = [4]uint64{n1, 0xff, n2, 0}
	for range 16 {
		r.step()
	}
}

// step advances the generator and returns its next value, exactly as the
// reference xoshiro256** does.
func (r *random) step() uint64 {
	s := &r.s
	result := bits.RotateLeft64(s[1]*5, 7) * 9
	t := s[1] << 17
	s[2] ^= s[0]
	s[3] ^= s[1]
	s[1] ^= s[2]
	s[0] ^= s[3]
	s[2] ^= t
	s[3] = bits.RotateLeft64(s[3], 45)
	return result
}

// randomFloat converts a random value to a float in [0, 1), from its top 53
// bits, as I2d does.
func randomFloat(x uint64) float64 { return float64(x>>11) * 0x1p-53 }

// project reduces the random value ran to [0, n], as lmathlib.c's project
// does: masking to the smallest 2^b-1 not below n, and drawing again until
// the value is in range.
func (r *random) project(ran, n uint64) uint64 {
	if n&(n+1) == 0 { // n+1 is a power of two
		return ran & n
	}
	lim := n
	lim |= lim >> 1
	lim |= lim >> 2
	lim |= lim >> 4
	lim |= lim >> 8
	lim |= lim >> 16
	lim |= lim >> 32
	for ran &= lim; ran > n; ran = r.step() & lim {
	}
	return ran
}

// openRandom adds math.random and math.randomseed, sharing a generator
// seeded randomly, as Lua 5.4 seeds it.
func openRandom(l *lua.State) {
	r := &random{}
	randomSeed := func() (uint64, uint64) {
		n1, n2 := uint64(time.Now().UnixNano()), uint64(uintptr(unsafe.Pointer(r)))
		r.seed(n1, n2)
		return n1, n2
	}
	randomSeed()
	l.PushGoFunction(func(l *lua.State) int {
		rv := r.step()
		var low, up int64
		switch l.Top() {
		case 0:
			l.PushNumber(randomFloat(rv))
			return 1
		case 1:
			low, up = 1, l.CheckInteger(1)
			if up == 0 { // math.random(0): a random integer with all bits
				l.PushInteger(int64(rv))
				return 1
			}
		case 2:
			low, up = l.CheckInteger(1), l.CheckInteger(2)
		default:
			l.Errorf("wrong number of arguments")
		}
		l.ArgumentCheck(low <= up, 1, "interval is empty")
		l.PushInteger(int64(r.project(rv, uint64(up)-uint64(low)) + uint64(low)))
		return 1
	})
	l.SetField(-2, "random")
	l.PushGoFunction(func(l *lua.State) int {
		var n1, n2 uint64
		if l.IsNone(1) {
			n1, n2 = randomSeed()
		} else {
			n1 = uint64(seedArgument(l, 1))
			n2 = uint64(l.OptInteger(2, 0))
			r.seed(n1, n2)
		}
		l.PushInteger(int64(n1))
		l.PushInteger(int64(n2))
		return 2
	})
	l.SetField(-2, "randomseed")
}

// seedArgument is randomseed's first argument: an integer, or a float's
// bits, as 5.4 takes any number.
func seedArgument(l *lua.State, index int) int64 {
	if n, ok := l.ToInteger(index); ok {
		return n
	}
	return int64(math.Float64bits(l.CheckNumber(index)))
}

// OpenMath opens the math library. Usually passed to Require.
func OpenMath(l *lua.State) int {
	l.CreateTable(0, len(mathLibrary)+len(mathNumberFunctions)+6)
	l.SetFunctions(mathLibrary, 0)
	for _, f := range mathNumberFunctions {
		l.PushNumberFunction(f.f)
		l.SetField(-2, f.name)
	}
	openRandom(l)
	l.PushNumber(math.Pi)
	l.SetField(-2, "pi")
	l.PushNumber(math.Inf(1))
	l.SetField(-2, "huge")
	l.PushInteger(int64(math.MaxInt64))
	l.SetField(-2, "maxinteger")
	l.PushInteger(int64(math.MinInt64))
	l.SetField(-2, "mininteger")
	return 1
}
