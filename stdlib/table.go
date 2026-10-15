package stdlib

import (
	"math"
	"strings"

	"github.com/matjam/apogee/lua"
)

// The table library, after Lua 5.5's ltablib.c. Its functions take a
// table or any value whose metatable gives it the operations they need,
// and read and write elements through metamethods.

// What a value must do to stand for a table.
const (
	tableRead   = 1 << iota // __index
	tableWrite              // __newindex
	tableLength             // __len
)

// checkTable checks that argument arg is a table, or behaves like one for
// what, as ltablib.c's checktab.
func checkTable(l *lua.State, arg, what int) {
	if l.TypeOf(arg) == lua.TypeTable {
		return
	}
	has := func(event string) bool {
		l.PushString(event) // a raw field of the metatable, on top
		l.RawGet(-2)
		defer l.Pop(1)
		return !l.IsNil(-1)
	}
	if l.MetaTable(arg) {
		ok := (what&tableRead == 0 || has("__index")) &&
			(what&tableWrite == 0 || has("__newindex")) &&
			(what&tableLength == 0 || l.TypeOf(arg) == lua.TypeString || has("__len"))
		l.Pop(1)
		if ok {
			return
		}
	}
	l.CheckType(arg, lua.TypeTable) // the error
}

// tableLen checks argument arg for what and returns its length.
func tableLen(l *lua.State, arg, what int) int64 {
	checkTable(l, arg, what|tableLength)
	return l.Len(arg)
}

// tableCreate is table.create(nseq [, nrest]): a table with room for them.
// The sizes are hints; beyond maxPrealloc apogee allocates as the table
// grows, as Go aborts the process when an allocation fails.
func tableCreate(l *lua.State) int {
	const maxHash, maxPrealloc = 1 << 30, 1 << 24
	seq, rest := uint64(l.CheckInteger(1)), uint64(l.OptInteger(2, 0))
	l.ArgumentCheck(seq <= math.MaxInt32, 1, "out of range")
	l.ArgumentCheck(rest <= math.MaxInt32, 2, "out of range")
	if rest > maxHash { // as ltable.c's MAXHBITS limits a hash part
		l.Errorf("table overflow")
	}
	l.CreateTable(int(min(seq, maxPrealloc)), int(min(rest, maxPrealloc)))
	return 1
}

// loopBatch is how many iterations a table function's element loop runs
// between reports to CountInstructions. The loops in insert, remove and
// move run in Go for as many elements as the script's table, length or
// range asks, so without the reports a count hook or Interrupt couldn't
// stop them.
const loopBatch = 256

// loopCounter reports a Go loop's iterations to CountInstructions.
type loopCounter struct {
	l *lua.State
	n int
}

func (c *loopCounter) step() {
	if c.n++; c.n == loopBatch {
		c.n = 0
		c.l.CountInstructions(loopBatch)
	}
}

func tableInsert(l *lua.State) int {
	e := tableLen(l, 1, tableRead|tableWrite) + 1 // the first empty element
	pos := e
	switch l.Top() {
	case 2: // at the end
	case 3:
		pos = l.CheckInteger(2)
		l.ArgumentCheck(uint64(pos)-1 < uint64(e), 2, "position out of bounds")
		c := loopCounter{l: l}
		for i := e; i > pos; i-- { // move up
			l.FieldInt(1, i-1)
			l.SetFieldInt(1, i)
			c.step()
		}
	default:
		l.Errorf("wrong number of arguments to 'insert'")
	}
	l.SetFieldInt(1, pos)
	return 0
}

func tableRemove(l *lua.State) int {
	size := tableLen(l, 1, tableRead|tableWrite)
	pos := l.OptInteger(2, size)
	if pos != size { // validate a given pos: in [1, size+1]
		l.ArgumentCheck(uint64(pos)-1 <= uint64(size), 2, "position out of bounds")
	}
	l.FieldInt(1, pos) // the result
	c := loopCounter{l: l}
	for ; pos < size; pos++ {
		l.FieldInt(1, pos+1)
		l.SetFieldInt(1, pos)
		c.step()
	}
	l.PushNil()
	l.SetFieldInt(1, pos)
	return 1
}

// tableMove is table.move(a1, f, e, t [, a2]): a2[t], ... = a1[f], ...,
// a1[e], copying upward where the ranges allow, and returns a2.
func tableMove(l *lua.State) int {
	f, e, t := l.CheckInteger(2), l.CheckInteger(3), l.CheckInteger(4)
	tt := 1 // the destination
	if !l.IsNoneOrNil(5) {
		tt = 5
	}
	checkTable(l, 1, tableRead)
	checkTable(l, tt, tableWrite)
	if e >= f {
		l.ArgumentCheck(f > 0 || e < math.MaxInt64+f, 3, "too many elements to move")
		n := e - f + 1
		l.ArgumentCheck(t <= math.MaxInt64-n+1, 4, "destination wrap around")
		c := loopCounter{l: l}
		if t > e || t <= f || (tt != 1 && !l.Compare(1, tt, lua.OpEq)) {
			for i := int64(0); i < n; i++ {
				l.FieldInt(1, f+i)
				l.SetFieldInt(tt, t+i)
				c.step()
			}
		} else {
			for i := n - 1; i >= 0; i-- {
				l.FieldInt(1, f+i)
				l.SetFieldInt(tt, t+i)
				c.step()
			}
		}
	}
	l.PushValue(tt)
	return 1
}

func tableConcat(l *lua.State) int {
	last := tableLen(l, 1, tableRead)
	sep := l.OptString(2, "")
	i := l.OptInteger(3, 1)
	last = l.OptInteger(4, last)
	var b strings.Builder
	add := func(i int64) {
		l.FieldInt(1, i)
		s, ok := l.ToString(-1)
		if !ok {
			l.Errorf("invalid value (%s) at index %d in table for 'concat'", l.TypeName(-1), i)
		}
		b.WriteString(s)
		l.Pop(1)
	}
	for ; i < last; i++ {
		add(i)
		b.WriteString(sep)
		l.CheckAllocation(b.Len()) // the result, which a long sep can make far longer than the table
	}
	if i == last { // the last, if the interval was not empty
		add(i)
	}
	l.PushString(b.String())
	return 1
}

func tablePack(l *lua.State) int {
	n := l.Top()
	l.CreateTable(n, 1)
	l.Insert(1)
	for i := n; i >= 1; i-- {
		l.SetFieldInt(1, i)
	}
	l.PushInteger(n)
	l.SetField(1, "n")
	return 1
}

func tableUnpack(l *lua.State) int {
	i := l.OptInteger(2, 1)
	var e int64
	if l.IsNoneOrNil(3) {
		e = tableLen(l, 1, tableRead)
	} else {
		e = l.CheckInteger(3)
	}
	if i > e {
		return 0
	}
	n := uint64(e) - uint64(i) // the count less one
	if n >= math.MaxInt32 || !l.CheckStack(int(n+1)) {
		l.Errorf("too many results to unpack")
	}
	for ; i < e; i++ { // to e-1, which cannot overflow
		l.FieldInt(1, i)
	}
	l.FieldInt(1, e)
	return int(n + 1)
}

func tableSort(l *lua.State) int {
	n := tableLen(l, 1, tableRead|tableWrite)
	if n > 1 {
		l.ArgumentCheck(n < math.MaxInt32, 1, "array too big")
		if !l.IsNoneOrNil(2) {
			l.CheckType(2, lua.TypeFunction)
		}
		l.SetTop(2)
		if l.TypeOf(1) == lua.TypeTable && !l.MetaTable(1) {
			comparator := 0
			if !l.IsNil(2) {
				comparator = 2
			}
			l.SortArray(1, int(n), comparator) // a plain table: sorted in place, fast
		} else {
			if l.TypeOf(1) == lua.TypeTable {
				l.Pop(1) // the metatable
			}
			sortAux(l, 1, uint(n), 0)
		}
	}
	return 0
}

// The quicksort of ltablib.c, through metamethods, for tables with them
// and values that stand for tables.

func sortSet2(l *lua.State, i, j uint) {
	l.SetFieldInt(1, i)
	l.SetFieldInt(1, j)
}

// sortLess reports whether the value at a is less than the one at b.
func sortLess(l *lua.State, a, b int) bool {
	if l.IsNil(2) {
		return l.Compare(a, b, lua.OpLT)
	}
	l.PushValue(2)
	l.PushValue(a - 1)
	l.PushValue(b - 2)
	l.Call(2, 1)
	r := l.ToBoolean(-1)
	l.Pop(1)
	return r
}

func sortPartition(l *lua.State, lo, up uint) uint {
	i, j := lo, up-1
	for {
		for i++; l.FieldInt(1, i) >= 0 && sortLess(l, -1, -2); i++ {
			if i == up-1 {
				l.Errorf("invalid order function for sorting")
			}
			l.Pop(1)
		}
		for j--; l.FieldInt(1, j) >= 0 && sortLess(l, -3, -1); j-- {
			if j < i {
				l.Errorf("invalid order function for sorting")
			}
			l.Pop(1)
		}
		if j < i {
			l.Pop(1)
			sortSet2(l, up-1, i)
			return i
		}
		sortSet2(l, i, j)
	}
}

func sortAux(l *lua.State, lo, up uint, rnd uint) {
	for lo < up {
		l.FieldInt(1, lo)
		l.FieldInt(1, up)
		if sortLess(l, -1, -2) {
			sortSet2(l, lo, up)
		} else {
			l.Pop(2)
		}
		if up-lo == 1 {
			return
		}
		p := (lo + up) / 2
		if up-lo >= 100 && rnd != 0 {
			r4 := (up - lo) / 4
			p = (rnd^lo^up)%(r4*2) + lo + r4
		}
		l.FieldInt(1, p)
		l.FieldInt(1, lo)
		if sortLess(l, -2, -1) {
			sortSet2(l, p, lo)
		} else {
			l.Pop(1)
			l.FieldInt(1, up)
			if sortLess(l, -1, -2) {
				sortSet2(l, p, up)
			} else {
				l.Pop(2)
			}
		}
		if up-lo == 2 {
			return
		}
		l.FieldInt(1, p)
		l.PushValue(-1)
		l.FieldInt(1, up-1)
		sortSet2(l, p, up-1)
		p = sortPartition(l, lo, up)
		var n uint
		if p-lo < up-p {
			sortAux(l, lo, p-1, rnd)
			n, lo = p-lo, p+1
		} else {
			sortAux(l, p+1, up, rnd)
			n, up = up-p, p-1
		}
		if (up-lo)/128 > n { // too unbalanced: randomize the pivot
			rnd = uint(lo*2654435761 ^ up)
		}
	}
}

var tableLibrary = []lua.RegistryFunction{
	{Name: "concat", Function: tableConcat},
	{Name: "create", Function: tableCreate},
	{Name: "insert", Function: tableInsert},
	{Name: "pack", Function: tablePack},
	{Name: "unpack", Function: tableUnpack},
	{Name: "remove", Function: tableRemove},
	{Name: "move", Function: tableMove},
	{Name: "sort", Function: tableSort},
}

// OpenTable opens the table library. Usually passed to Require.
func OpenTable(l *lua.State) int {
	l.NewLibrary(tableLibrary)
	return 1
}
