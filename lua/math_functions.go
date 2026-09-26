package lua

import "math"

// MathFloor, MathCeil, MathAbs, MathMin and MathMax are Lua's
// math.floor, math.ceil, math.abs, math.min and math.max, which the
// standard library registers. Compiled code recognises these functions,
// wherever a script keeps them, and computes them inline.

// MathFloor is math.floor: an integer's value, or a float rounded down,
// as an integer when one holds it.
func MathFloor(l *State) int {
	if l.IsInteger(1) {
		l.SetTop(1)
	} else {
		pushIntegerIfFits(l, math.Floor(l.CheckNumber(1)))
	}
	return 1
}

// MathCeil is math.ceil: an integer's value, or a float rounded up, as an
// integer when one holds it.
func MathCeil(l *State) int {
	if l.IsInteger(1) {
		l.SetTop(1)
	} else {
		pushIntegerIfFits(l, math.Ceil(l.CheckNumber(1)))
	}
	return 1
}

// MathAbs is math.abs, keeping an integer an integer.
func MathAbs(l *State) int {
	if l.IsInteger(1) {
		n, _ := l.ToInteger(1)
		if n < 0 {
			n = -n // minint stays minint, as 0u - n does in C
		}
		l.PushInteger(n)
	} else {
		l.PushNumber(math.Abs(l.CheckNumber(1)))
	}
	return 1
}

// MathMin is math.min: the first argument no other is less than, keeping
// its type.
func MathMin(l *State) int { return minMax(l, false) }

// MathMax is math.max: the first argument no other is greater than,
// keeping its type.
func MathMax(l *State) int { return minMax(l, true) }

func minMax(l *State, max bool) int {
	n := l.Top()
	l.ArgumentCheck(n >= 1, 1, "value expected")
	best := 1
	l.CheckNumber(1)
	for i := 2; i <= n; i++ {
		l.CheckNumber(i)
		if max && l.Compare(best, i, OpLT) || !max && l.Compare(i, best, OpLT) {
			best = i
		}
	}
	l.PushValue(best)
	return 1
}

// pushIntegerIfFits pushes f as an integer if it has an integral value an
// integer holds, and as a float otherwise, as lmathlib.c's pushnumint.
func pushIntegerIfFits(l *State, f float64) {
	if f >= -(1<<63) && f < 1<<63 {
		l.PushInteger(int64(f))
		return
	}
	l.PushNumber(f)
}
