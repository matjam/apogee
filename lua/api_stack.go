package lua

import "fmt"

// XMove pops n values from l's stack and pushes them onto to's. Moving
// within one state leaves the stack as it is.
//
// https://www.lua.org/manual/5.5/manual.html#lua_xmove
func (l *State) XMove(to *State, n int) {
	if l == to {
		return
	}
	l.checkElementCount(n)
	for _, v := range l.stack[l.top-n : l.top] {
		to.apiPush(v)
	}
	l.top -= n
}

func (l *State) adjustResults(resultCount int) {
	if resultCount == MultipleReturns && l.callInfo.top < l.top {
		l.callInfo.setTop(l.top)
	}
}

func (l *State) apiIncrementTop() {
	l.top++
	if apiCheck && l.top > l.callInfo.top {
		panic("stack overflow")
	}
}

func (l *State) apiPush(v value) {
	l.push(v)
	if apiCheck && l.top > l.callInfo.top {
		panic("stack overflow")
	}
}

func (l *State) checkElementCount(n int) {
	if apiCheck && n >= l.top-l.callInfo.function {
		panic("not enough elements in the stack")
	}
}

func (l *State) checkResults(argCount, resultCount int) {
	if apiCheck && resultCount != MultipleReturns && l.callInfo.top-l.top < resultCount-argCount {
		panic("results from function overflow current stack size")
	}
}

func apiCheckStackIndex(index int, v value) {
	if apiCheck && (v.identical(none) || isPseudoIndex(index)) {
		panic(fmt.Sprintf("index %d not in the stack", index))
	}
}

func (l *State) indexToValue(index int) value {
	switch {
	case index > 0:
		// TODO apiCheck(index <= callInfo.top_-(callInfo.function+1), "unacceptable index")
		if i := l.callInfo.function + index; i < l.top {
			return l.stack[i]
		}
		return none
	case index > RegistryIndex: // negative index
		// TODO apiCheck(index != 0 && -index <= l.top-(callInfo.function+1), "invalid index")
		return l.stack[l.top+index]
	case index == RegistryIndex:
		return objectValue(l.global.registry)
	default: // upvalues
		i := RegistryIndex - index
		return l.stack[l.callInfo.function].goClosure().upValues[i-1]
		// if closure := l.stack[callInfo.function].(*goClosure); i <= len(closure.upValues) {
		// 	return closure.upValues[i-1]
		// }
		// return none
	}
}

func (l *State) setIndexToValue(index int, v value) {
	switch {
	case index > 0:
		l.stack[l.callInfo.function:l.top][index] = v
		// if i := callInfo.function + index; i < l.top {
		// 	l.stack[i] = v
		// } else {
		// 	panic("unacceptable index")
		// }
	case index > RegistryIndex: // negative index
		l.stack[l.top+index] = v
	case index == RegistryIndex:
		l.global.registry = v.table()
	default: // upvalues
		i := RegistryIndex - index
		l.stack[l.callInfo.function].goClosure().upValues[i-1] = v
	}
}

func (l *State) move(dest int, src value) { l.setIndexToValue(dest, src) }

// arg returns the value at a positive index, or nil when the index is not
// positive or is past the top. It is indexToValue's fast path for reading
// Go function arguments.
func (l *State) arg(index int) value {
	if i := l.callInfo.function + index; index > 0 && i < l.top {
		return l.stack[i]
	}
	return nilValue
}

func apiCheckStackSpace(l *State, n int) { l.assert(n < l.top-l.callInfo.function) }

func isPseudoIndex(i int) bool { return i <= RegistryIndex }

// UpValueIndex returns the pseudo-index that represents the i-th upvalue of
// the running function.
//
// https://www.lua.org/manual/5.5/manual.html#lua_upvalueindex
func UpValueIndex(i int) int { return RegistryIndex - i }

// AbsIndex converts the acceptable index index to an absolute index (that
// is, one that does not depend on the stack top).
//
// https://www.lua.org/manual/5.5/manual.html#lua_absindex
func (l *State) AbsIndex(index int) int {
	if index > 0 || isPseudoIndex(index) {
		return index
	}
	return l.top - l.callInfo.function + index
}

// SetTop accepts any index, or 0, and sets the stack top to index. If the
// new top is larger than the old one, then the new elements are filled with
// nil. If index is 0, then all stack elements are removed.
//
// If index is negative, the stack will be decremented by that much. If
// the decrement is larger than the stack, SetTop will panic().
//
// https://www.lua.org/manual/5.5/manual.html#lua_settop
func (l *State) SetTop(index int) {
	f := l.callInfo.function
	if index >= 0 {
		if apiCheck && index > l.stackLast-(f+1) {
			panic("new top too large")
		}
		i := l.top
		l.top = f + 1 + index
		if i < l.top {
			clear(l.stack[i:l.top])
		}
	} else {
		if apiCheck && -(index+1) > l.top-(f+1) {
			panic("invalid new top")
		}
		l.top += index + 1 // 'subtract' index (index is negative)
	}
}

// Remove the element at the given valid index, shifting down the elements
// above index to fill the gap. This function cannot be called with a
// pseudo-index, because a pseudo-index is not an actual stack position.
//
// https://www.lua.org/manual/5.5/manual.html#lua_remove
func (l *State) Remove(index int) {
	apiCheckStackIndex(index, l.indexToValue(index))
	i := l.callInfo.function + l.AbsIndex(index)
	copy(l.stack[i:l.top-1], l.stack[i+1:l.top])
	l.top--
}

// Insert moves the top element into the given valid index, shifting up the
// elements above this index to open space.  This function cannot be called
// with a pseudo-index, because a pseudo-index is not an actual stack position.
//
// https://www.lua.org/manual/5.5/manual.html#lua_insert
func (l *State) Insert(index int) {
	apiCheckStackIndex(index, l.indexToValue(index))
	i := l.callInfo.function + l.AbsIndex(index)
	copy(l.stack[i+1:l.top+1], l.stack[i:l.top])
	l.stack[i] = l.stack[l.top]
}

// Replace moves the top element into the given valid index without shifting
// any element (therefore replacing the value at the given index), and then
// pops the top element.
//
// https://www.lua.org/manual/5.5/manual.html#lua_replace
func (l *State) Replace(index int) {
	l.checkElementCount(1)
	l.move(index, l.stack[l.top-1])
	l.top--
}

// CheckStack ensures that there are at least size free stack slots in the
// stack, and reports whether it could. Unlike the other Check*() functions
// it doesn't raise an error when the stack can't grow, with one exception:
// when growing would pass the limit SetAllocationLimit set, it raises the
// memory error, so that running out of the allocation budget can't be
// caught as an ordinary failure, such as unpack's "too many results".
//
// https://www.lua.org/manual/5.5/manual.html#lua_checkstack
func (l *State) CheckStack(size int) bool {
	callInfo := l.callInfo
	ok := l.stackLast-l.top > size
	if !ok && l.top+extraStack <= maxStack-size {
		err := l.protect(func() { l.growStack(size) })
		if err == ErrMemory {
			l.throw(ErrMemory)
		}
		ok = err == nil
	}
	if ok && callInfo.top < l.top+size {
		callInfo.setTop(l.top + size)
	}
	return ok
}

// Top returns the index of the top element in the stack. Because Lua indices
// start at 1, this result is equal to the number of elements in the stack
// (hence 0 means an empty stack).
//
// https://www.lua.org/manual/5.5/manual.html#lua_gettop
func (l *State) Top() int { return l.top - (l.callInfo.function + 1) }

// Copy moves the element at the index from into the valid index to
// without shifting any element (therefore replacing the value at that
// position).
//
// https://www.lua.org/manual/5.5/manual.html#lua_copy
func (l *State) Copy(from, to int) { l.move(to, l.indexToValue(from)) }

// Pop pops n elements from the stack.
//
// https://www.lua.org/manual/5.5/manual.html#lua_pop
func (l *State) Pop(n int) { l.SetTop(-n - 1) }

// PushValue pushes a copy of the element at index onto the stack.
//
// https://www.lua.org/manual/5.5/manual.html#lua_pushvalue
func (l *State) PushValue(index int) { l.apiPush(l.indexToValue(index)) }
