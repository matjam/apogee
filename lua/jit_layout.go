//go:build (darwin || linux) && (arm64 || amd64)

package lua

import "unsafe"

// Offsets the compilers use to reach the interpreter's structures.
const (
	offFrame     = uint32(unsafe.Offsetof(jitContext{}.frame))
	offConstants = uint32(unsafe.Offsetof(jitContext{}.constants))
	offTarget    = uint32(unsafe.Offsetof(jitContext{}.target))
	offExitPC    = uint32(unsafe.Offsetof(jitContext{}.exitPC))
	offBudget    = uint32(unsafe.Offsetof(jitContext{}.budget))
	offUpValues  = uint32(unsafe.Offsetof(jitContext{}.upValues))
	offBarrier   = uint32(unsafe.Offsetof(jitContext{}.barrier))
	offKernels   = uint32(unsafe.Offsetof(jitContext{}.kernels))
	offSpill     = uint32(unsafe.Offsetof(jitContext{}.spill))
	offHoist     = uint32(unsafe.Offsetof(jitContext{}.hoist))
	valueSize    = uint32(unsafe.Sizeof(value{}))
	offP         = uint32(unsafe.Offsetof(value{}.p))
	offN         = uint32(unsafe.Offsetof(value{}.n))
	offUVState   = uint32(unsafe.Offsetof(upValue{}.state))
	offUVIndex   = uint32(unsafe.Offsetof(upValue{}.index))
	offLUpValues = uint32(unsafe.Offsetof(State{}.upValues))
	offUVClosed  = uint32(unsafe.Offsetof(upValue{}.closed))
	offStack     = uint32(unsafe.Offsetof(State{}.stack))
	offTShape    = uint32(unsafe.Offsetof(table{}.shape))
	offTSlots    = uint32(unsafe.Offsetof(table{}.slots))
	offTArray    = uint32(unsafe.Offsetof(table{}.array))
	offTHash     = uint32(unsafe.Offsetof(table{}.hash))
	offUDBuf     = uint32(unsafe.Offsetof(userData{}.buf))
	offBufPtr    = uint32(unsafe.Offsetof(buffer{}.ptr))
	offBufLen    = uint32(unsafe.Offsetof(buffer{}.len))
	offBufKind   = uint32(unsafe.Offsetof(buffer{}.kind))
	offTMeta     = uint32(unsafe.Offsetof(table{}.metaTable))
	offTFlags    = uint32(unsafe.Offsetof(table{}.flags))
	offShapeDict = uint32(unsafe.Offsetof(shape{}.dict))
	offShapeGC   = uint32(unsafe.Offsetof(shape{}.collects))
	offCShape    = uint32(unsafe.Offsetof(fieldCache{}.shape))
	offCSlot     = uint32(unsafe.Offsetof(fieldCache{}.slot))
	offCMtShape  = uint32(unsafe.Offsetof(fieldCache{}.mtShape))
	offCMtSlot   = uint32(unsafe.Offsetof(fieldCache{}.mtSlot))
	offCIndex    = uint32(unsafe.Offsetof(fieldCache{}.index))
	offCIdxSlot  = uint32(unsafe.Offsetof(fieldCache{}.indexSlot))
	offCChain    = uint32(unsafe.Offsetof(fieldCache{}.chain))
	offChLevels  = uint32(unsafe.Offsetof(fieldChain{}.levels))
	offChSlot    = uint32(unsafe.Offsetof(fieldChain{}.slot))
	offLvMtShape = uint32(unsafe.Offsetof(chainLevel{}.mtShape))
	offLvMtSlot  = uint32(unsafe.Offsetof(chainLevel{}.mtSlot))
	offLvIndex   = uint32(unsafe.Offsetof(chainLevel{}.index))
	offGFNumber  = uint32(unsafe.Offsetof(goFunction{}.number))
	offGCUpVals  = uint32(unsafe.Offsetof(goClosure{}.upValues))
	offNFUnary   = uint32(unsafe.Offsetof(numberFunction{}.unary))
	offSliceLen  = 8
	offCtxS      = uint32(unsafe.Offsetof(jitContext{}.state))
	offCallee    = uint32(unsafe.Offsetof(jitContext{}.callee))
	offCtxTBC    = uint32(unsafe.Offsetof(jitContext{}.tbc))

	offClProto   = uint32(unsafe.Offsetof(luaClosure{}.prototype))
	offClUpVals  = uint32(unsafe.Offsetof(luaClosure{}.upValues))
	offPConsts   = uint32(unsafe.Offsetof(prototype{}.Constants))
	offPCode     = uint32(unsafe.Offsetof(prototype{}.Code))
	offPParams   = uint32(unsafe.Offsetof(prototype{}.ParameterCount))
	offPMaxStack = uint32(unsafe.Offsetof(prototype{}.MaxStackSize))
	offPVarArg   = uint32(unsafe.Offsetof(prototype{}.IsVarArg))
	offPJit      = uint32(unsafe.Offsetof(prototype{}.jit))
	offJCEntry   = uint32(unsafe.Offsetof(jitCode{}.entry))
	offJCBase    = uint32(unsafe.Offsetof(jitCode{}.base))
	offJCOffsets = uint32(unsafe.Offsetof(jitCode{}.offsets))

	offLTop       = uint32(unsafe.Offsetof(State{}.top))
	offLCallInfo  = uint32(unsafe.Offsetof(State{}.callInfo))
	offLStackLast = uint32(unsafe.Offsetof(State{}.stackLast))

	offCIFunction = uint32(unsafe.Offsetof(callInfo{}.function))
	offCITop      = uint32(unsafe.Offsetof(callInfo{}.top))
	offCIResults  = uint32(unsafe.Offsetof(callInfo{}.resultCount))
	offCIPrev     = uint32(unsafe.Offsetof(callInfo{}.previous))
	offCINext     = uint32(unsafe.Offsetof(callInfo{}.next))
	offCIStatus   = uint32(unsafe.Offsetof(callInfo{}.callStatus))
	offCIMeta     = uint32(unsafe.Offsetof(callInfo{}.callMetamethods))
	offCILua      = uint32(unsafe.Offsetof(callInfo{}.luaCallInfo))
	offLFrame     = uint32(unsafe.Offsetof(luaCallInfo{}.frame))
	offLSavedPC   = uint32(unsafe.Offsetof(luaCallInfo{}.savedPC))
	offLCode      = uint32(unsafe.Offsetof(luaCallInfo{}.code))
	offLClosure   = uint32(unsafe.Offsetof(luaCallInfo{}.closure))
	offSliceCap   = 16
)

// Generated code scales a stack index by shifting it left by 4, so a value
// must be 16 bytes. Each array length below is negative otherwise.
var (
	_ [valueSize - 16]struct{}
	_ [16 - valueSize]struct{}
)

// bitOf returns the bit number of a single-bit flag.
func bitOf(f callStatus) uint32 {
	for b := range uint32(8) {
		if f == 1<<b {
			return b
		}
	}
	panic("bitOf: not a single bit")
}
