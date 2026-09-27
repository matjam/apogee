package lua

// BasePairs, BaseIPairs, BaseNext and BaseIPairsIterator are Lua's pairs,
// ipairs and next, and the iterator ipairs returns, which the standard
// library registers: pairs as a Go closure over next, and ipairs over its
// iterator. Compiled code recognises these functions, and runs a generic
// for over a table's array part with them inline.

// BasePairs is pairs, a Go closure whose upvalue is next, which it
// returns, so it returns the same function each time, unless __pairs says
// otherwise.
func BasePairs(l *State) int {
	l.CheckAny(1)
	if l.MetaField(1, "__pairs") != TypeNil {
		l.PushValue(1) // argument 'self' to metamethod
		// 4 values, the last a closing value, as Lua 5.5; the metamethod
		// may yield.
		l.CallWithContinuation(1, 4, 0, func(*State) int { return 4 })
		return 4
	}
	l.PushValue(UpValueIndex(1))
	l.PushValue(1)
	l.PushNil()
	l.PushNil() // no closing value
	return 4
}

// BaseIPairs is ipairs, a Go closure whose upvalue is its iterator, so
// that it returns the same function each time, which indexes the value
// with metamethods, as Lua 5.4's does.
func BaseIPairs(l *State) int {
	l.CheckAny(1)
	l.PushValue(UpValueIndex(1))
	l.PushValue(1)
	l.PushInteger(0)
	return 3
}

// BaseNext is next: the key after the second argument in the table, and
// its value, or nil after the last.
func BaseNext(l *State) int {
	l.CheckType(1, TypeTable)
	l.SetTop(2)
	if l.Next(1) {
		return 2
	}
	l.PushNil()
	return 1
}

// BaseIPairsIterator is the iterator ipairs returns: the index after the
// second argument and the table's value there, with metamethods, or nil
// where that value is nil.
func BaseIPairsIterator(l *State) int {
	i := l.CheckInteger(2) + 1
	l.PushInteger(i)
	l.PushInteger(i)
	l.Table(1)
	if l.IsNil(-1) {
		return 1
	}
	return 2
}

// BaseSetMetatable is setmetatable, which the standard library registers.
// Compiled code sets the metatable of a table that has none itself, when
// the new one's shape has never held __gc or __mode (shape.collects).
func BaseSetMetatable(l *State) int {
	t := l.TypeOf(2)
	l.CheckType(1, TypeTable)
	l.ArgumentCheck(t == TypeNil || t == TypeTable, 2, "nil or table expected")
	if l.MetaField(1, "__metatable") != TypeNil {
		l.Errorf("cannot change a protected metatable")
	}
	l.SetTop(2)
	l.SetMetaTable(1)
	return 1
}
