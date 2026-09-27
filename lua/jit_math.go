//go:build (darwin || linux) && (arm64 || amd64)

package lua

import (
	"slices"
	"unsafe"
)

// mathFn is one of the math functions compiled code computes inline:
// MathFloor, MathCeil, MathAbs, MathMin and MathMax. They are Go
// functions, not number functions, since they keep integers integers, so
// compiled code tells them apart by their code: a Function's first word.
type mathFn uint8

const (
	mathNone mathFn = iota
	mathFloor
	mathCeil
	mathAbs
	mathMin
	mathMax
)

// functionValue returns the first word of f, which is fixed for a
// top-level function: what a goFunction's embedded Function holds.
func functionValue(f Function) uint64 { return uint64(*(*uintptr)(unsafe.Pointer(&f))) }

// mathFns are the math functions by their first words; unary ones first.
// init fills it: MathFloor's callees reach the compiler, which reads it.
var mathFns []mathFnValue

type mathFnValue struct {
	fn uint64 // functionValue of the function
	id mathFn
}

func init() {
	for id, f := range map[mathFn]Function{mathFloor: MathFloor, mathCeil: MathCeil, mathAbs: MathAbs, mathMin: MathMin, mathMax: MathMax} {
		mathFns = append(mathFns, mathFnValue{functionValue(f), id})
	}
	slices.SortFunc(mathFns, func(a, b mathFnValue) int { return int(a.id) - int(b.id) })
}

// unary reports whether m takes one argument, rather than two.
func (m mathFn) unary() bool { return m <= mathAbs }

// mathFnOf returns the math function cl's upvalue n holds, if it holds
// one.
func mathFnOf(cl *luaClosure, n int) (mathFn, uint64) {
	if cl == nil || n >= len(cl.upValues) || cl.upValues[n] == nil {
		return mathNone, 0
	}
	f := cl.upValues[n].value().goFunction()
	if f == nil || f.number != nil {
		return mathNone, 0
	}
	for _, m := range mathFns {
		if functionValue(f.Function) == m.fn {
			return m.id, m.fn
		}
	}
	return mathNone, 0
}

// iterNext and iterIPairs are BaseNext's and BaseIPairsIterator's first
// words: the generic for iterators whose steps through a table's array
// part compiled code takes itself (tforCall). callPairs and callIPairs are
// BasePairs's and BaseIPairs's, which compiled code runs (pairsCall).
// init sets them, as mathFns.
var iterNext, iterIPairs, callPairs, callIPairs uint64

func init() {
	iterNext, iterIPairs = functionValue(BaseNext), functionValue(BaseIPairsIterator)
	callPairs, callIPairs = functionValue(BasePairs), functionValue(BaseIPairs)
}
