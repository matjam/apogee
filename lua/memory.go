package lua

import (
	"math"
	"unsafe"
)

// The allocation limit. A state counts what its Lua code allocates, the
// garbage included, and refuses an allocation that would take the count
// past the limit the host set, before making it, with a memory error.
// Go's collector frees memory whenever it likes, so the count never goes
// down: the limit bounds what one evaluation, from SetAllocationLimit on,
// may allocate, and the same script with the same inputs reaches the same
// count every time.
//
// The count is an estimate in bytes: 16 for each value slot in a table,
// stack or closure, the length of each new string, and the size of each
// object. What a Go function allocates for itself is not counted unless
// it calls Charge.

// Sizes the count uses, in bytes.
const (
	slotBytes      = int(unsafe.Sizeof(value{}))
	pointerBytes   = int(unsafe.Sizeof(uintptr(0)))
	tableBytes     = int(unsafe.Sizeof(table{}))
	hashEntryBytes = int(unsafe.Sizeof(hashValue{})) + slotBytes + 8 // a key, a value, and the map's overhead
	closureBytes   = int(unsafe.Sizeof(luaClosure{}))
	upValueBytes   = int(unsafe.Sizeof(upValue{}))
	goClosureBytes = int(unsafe.Sizeof(goClosure{}))
	userDataBytes  = int(unsafe.Sizeof(userData{}))
	threadBytes    = int(unsafe.Sizeof(State{})) + basicStackSize*slotBytes

	// A call frame's record: callInfo, and the Lua or Go part a frame of
	// that kind adds. A state keeps the records it has made for reuse, so
	// only a call deeper than any before it makes, and counts, new ones.
	callInfoBytes    = int(unsafe.Sizeof(callInfo{}))
	luaCallInfoBytes = int(unsafe.Sizeof(luaCallInfo{}))
	goCallInfoBytes  = int(unsafe.Sizeof(goCallInfo{}))
)

// SetAllocationLimit limits what the state and its threads may allocate
// from now on to about n bytes, counting from zero again, and removes the
// limit for n <= 0. An allocation that would exceed it raises a memory
// error instead: ProtectedCall returns ErrMemory, pcall returns "not
// enough memory", and xpcall does not call its message handler, as in C
// Lua. Allocations that fit what is left still succeed.
//
// A host running untrusted scripts sets a limit before each evaluation, as
// it arranges an Interrupt, so that no script can exhaust memory:
//
//	l.SetAllocationLimit(8 << 20)
//	err := l.ProtectedCall(0, 1, 0) // errors.Is(err, lua.ErrMemory) when it ran out
func (l *State) SetAllocationLimit(n int) {
	g := l.global
	g.allocCounted = 0
	if n <= 0 {
		g.allocLimit, g.allocRemaining = math.MaxInt, math.MaxInt
	} else {
		g.allocLimit, g.allocRemaining = n, n
	}
}

// Allocated returns the bytes the state and its threads have allocated
// since SetAllocationLimit, or since NewState, as the limit counts them.
func (l *State) Allocated() int {
	g := l.global
	return g.allocCounted + g.allocLimit - g.allocRemaining
}

// Charge counts n bytes against the allocation limit, raising a memory
// error if they do not fit. A Go function calls it for memory it
// allocates for Lua that the state cannot see, such as the Go value behind
// a userdata. n <= 0 counts nothing.
func (l *State) Charge(n int) {
	if n > 0 {
		l.charge(n)
	}
}

// CheckAllocation raises a memory error if n more bytes would not fit in
// the allocation limit, without counting them. A Go function calls it
// before building a large value that pushing will count, such as a string.
func (l *State) CheckAllocation(n int) {
	if g := l.global; n > g.allocRemaining && g.allocLimit != math.MaxInt {
		l.throw(ErrMemory)
	}
}

// charge counts n >= 0 bytes, before they are allocated.
func (l *State) charge(n int) {
	if g := l.global; n <= g.allocRemaining {
		g.allocRemaining -= n
	} else {
		l.outOfMemory(n)
	}
}

// outOfMemory raises the memory error for an allocation of n bytes that
// does not fit, unless the state has no limit, in which case the count
// starts again: without a limit, allocRemaining only keeps the count.
func (l *State) outOfMemory(n int) {
	g := l.global
	if g.allocLimit == math.MaxInt {
		g.allocCounted += g.allocLimit - g.allocRemaining
		g.allocRemaining = math.MaxInt - n
		return
	}
	l.throw(ErrMemory)
}

// closureCost is what a new closure of p counts: the closure and an
// upvalue for each variable it captures, which findUpValue may make, so
// that findUpValue need not count and stays small enough to inline.
func closureCost(p *prototype) int {
	return closureBytes + len(p.UpValues)*(pointerBytes+upValueBytes)
}

// chargeTable counts a new table with room for arraySize and hashSize
// entries.
func (l *State) chargeTable(arraySize, hashSize int) {
	l.charge(tableBytes + (max(arraySize, 0)+max(hashSize, 0))*slotBytes)
}
