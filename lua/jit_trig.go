//go:build (darwin || linux) && (arm64 || amd64)

package lua

import (
	"math"
	"unsafe"
)

// The constants of math's sin and cos, in math/sin.go.
const (
	trigPI4A = 7.85398125648498535156e-1
	trigPI4B = 3.77489470793079817668e-8
	trigPI4C = 2.69515142907905952645e-15
)

var trigSin = [...]float64{
	1.58962301576546568060e-10,
	-2.50507477628578072866e-8,
	2.75573136213857245213e-6,
	-1.98412698295895385996e-4,
	8.33333333332211858878e-3,
	-1.66666666666666307295e-1,
}

var trigCos = [...]float64{
	-1.13585365213876817300e-11,
	2.08757008419747316778e-9,
	-2.75573141792967388112e-7,
	2.48015872888517045348e-5,
	-1.38888888888730564116e-3,
	4.16666666666665929218e-2,
}

// trigTable holds the constants compiled sin and cos use. Compiled code
// reads each from memory with one instruction, rather than building it in
// a general register and moving it across.
var trigTable = struct {
	limit, fourOverPi, one, half float64
	pi4                          [3]float64
	sin, cos                     [6]float64
	inf, nan                     float64 // math.Sin and Cos give math.NaN() for infinities
}{1 << 29, 4 / math.Pi, 1, 0.5, [3]float64{trigPI4A, trigPI4B, trigPI4C}, trigSin, trigCos, math.Inf(1), math.NaN()}

// Offsets into trigTable.
var (
	offTrigLimit = uint32(unsafe.Offsetof(trigTable.limit))
	offTrigFour  = uint32(unsafe.Offsetof(trigTable.fourOverPi))
	offTrigOne   = uint32(unsafe.Offsetof(trigTable.one))
	offTrigHalf  = uint32(unsafe.Offsetof(trigTable.half))
	offTrigPI4   = uint32(unsafe.Offsetof(trigTable.pi4))
	offTrigSin   = uint32(unsafe.Offsetof(trigTable.sin))
	offTrigCos   = uint32(unsafe.Offsetof(trigTable.cos))
	offTrigInf   = uint32(unsafe.Offsetof(trigTable.inf))
	offTrigNaN   = uint32(unsafe.Offsetof(trigTable.nan))
)

// trigTableAddr is the address generated code loads trigTable from. A
// package variable never moves.
func trigTableAddr() uint64 { return uint64(uintptr(unsafe.Pointer(&trigTable))) }
