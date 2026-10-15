package lua

import (
	"math"
	"os"
	"sync/atomic"
)

type pc int
type callStatus byte

const (
	callStatusLua                callStatus = 1 << iota // call is running a Lua function
	callStatusHooked                                    // call is running a debug hook
	callStatusReentry                                   // call is running on same invocation of execute of previous call
	callStatusYielded                                   // call reentered after suspension
	callStatusYieldableProtected                        // call is a yieldable protected call
	callStatusError                                     // call has an error status (pcall)
	callStatusTail                                      // call was tail called
	callStatusHookYielded                               // last hook called yielded
)

// A State is an opaque structure representing per thread Lua state.
type State struct {
	error                 error
	status                ThreadStatus
	top                   int // first free slot in the stack
	global                *globalState
	callInfo              *callInfo // call info for current function
	oldPC                 pc        // last pC traced
	stackLast             int       // last free slot in the stack
	stack                 []value
	nonYieldableCallCount int
	nestedGoCallCount     int
	hookMask              byte
	allowHook             bool
	baseHookCount         int
	hookCount             int
	hooker                Hook
	upValues              *upValue // open upvalues, sorted by stack index, highest first
	tbc                   []int    // to-be-closed variables' stack indices, ascending
	deathError            error    // the error that killed a dead coroutine, for CloseThread
	deathValue            value    // its value
	closedError           error    // the error a coroutine that closed itself returns
	errorFunction         int      // current error handling function (stack index)
	baseCallInfo          callInfo // callInfo for first level (go calling lua)
	protectFunction       func()
	jitCtx                jitContext // shared with generated code while it runs
	jitRuns               uint64     // entries into compiled code, for tests
	interruptPoll         int32      // loop iterations until the next interrupt check
	jitBarrierRuns        uint64     // entries while the write barrier was on, for tests
}

type globalState struct {
	mainThread         *State
	tagMethodNames     [tmCount]string
	metaTables         [typeCount]*table // metatables for basic types
	registry           *table
	panicFunction      Function // to be called in unprotected errors
	warn               WarnFunction
	memoryErrorMessage string
	rootShape          *shape // shape tree for this state's tables
	lightBoxes         map[any]*lightUserData
	jit                bool        // compile hot functions; see WithoutJIT
	interrupt          atomic.Bool // set by Interrupt, from any goroutine
	goName             string      // what debug information calls Go functions

	// Lua collections; see gc.go.
	finalizable          []value           // objects with __gc, in the order they were marked
	toFinalize           []value           // finalizers due, in the order they run
	finalizerThread      *State            // runs finalizers; see callFinalizer
	gcStopped            bool              // collectgarbage "stop"
	closed               bool              // Close has begun
	noFinalizers         bool              // never call __gc; see WithoutFinalizers
	gcWatch              bool              // a metatable with __mode or __gc was set
	gcBusy               bool              // collecting or finalizing: no nested collection
	gcParams             [gcParamCount]int // collectgarbage "param"; apogee's collector uses only the pause
	gcGenerational       bool              // collectgarbage's mode, which apogee's collector ignores
	gcCycles             uint64            // goGCCycles when checkGC last looked
	gcHeapBase           uint64            // Go's live heap at the last Lua collection
	gcAllocatedBase      uint64            // Go's allocated bytes at the last Lua collection
	gcStepWork           uint64            // collectgarbage "step" work toward a cycle, in bytes
	gcBackoff            uint              // automatic collections wait 2^gcBackoff times longer
	gcCountBase          uint64            // for "count" while stopped: the heap when counting began
	gcCountAllocatedBase uint64            // and Go's allocated bytes then

	// The allocation limit; see memory.go.
	allocRemaining int // bytes Lua may still allocate
	allocLimit     int // what allocRemaining started from, math.MaxInt for no limit
	allocCounted   int // bytes counted before allocRemaining last started
	// seed uint // randomized seed for hashes
	// upValueHead upValue // head of double-linked list of all open upvalues
}

func (g *globalState) metaTable(o value) *table {
	if t := typeOf(o); t != TypeNone {
		return g.metaTables[t]
	}
	return nil
}

// typeOf returns the Lua type of v, or TypeNone for none.
func typeOf(v value) Type {
	switch v.kind() {
	case vkNil:
		return TypeNil
	case vkBool:
		return TypeBoolean
	case vkNumber:
		return TypeNumber
	case vkString:
		return TypeString
	case vkTable:
		return TypeTable
	case vkLuaClosure, vkGoClosure, vkGoFunction:
		return TypeFunction
	case vkUserData:
		return TypeUserData
	case vkThread:
		return TypeThread
	case vkLightUserData:
		return TypeLightUserData
	}
	return TypeNone
}

// NewState creates a new thread running in a new, independent state.
//
// https://www.lua.org/manual/5.5/manual.html#lua_newstate
func NewState(options ...Option) *State {
	l := &State{allowHook: true, error: nil, nonYieldableCallCount: 1}
	g := &globalState{mainThread: l, registry: newTable(), memoryErrorMessage: "not enough memory", rootShape: newRootShape(),
		gcParams: defaultGCParams, warn: StderrWarnings(os.Stderr), allocRemaining: math.MaxInt, allocLimit: math.MaxInt}
	l.global = g
	l.initializeStack()
	g.registry.putAtInt(l, RegistryIndexMainThread, objectValue(l))
	g.registry.putAtInt(l, RegistryIndexGlobals, objectValue(newTable()))
	copy(g.tagMethodNames[:], eventNames)
	g.jit = jitDefault && jitSupported && !jitDisabled
	g.goName = "Go"
	if os.Getenv("APOGEE_GO_AS_C") == "1" {
		g.goName = "C"
	}
	for _, o := range options {
		o(l)
	}
	return l
}

// AtPanic sets a new panic function and returns the old one.
func (l *State) AtPanic(panicFunction Function) Function {
	panicFunction, l.global.panicFunction = l.global.panicFunction, panicFunction
	return panicFunction
}

func (l *State) valueToType(v value) Type { return typeOf(v) }
