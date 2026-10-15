package lua

import (
	"log"

	"github.com/matjam/apogee/internal/bytecode"
)

func (l *State) push(v value) {
	l.stack[l.top] = v
	l.top++
}

func (l *State) pop() value {
	l.top--
	return l.stack[l.top]
}

// upValue is open while state is non-nil, and then refers to
// state.stack[index]. Closing it copies that slot into closed. Open upvalues
// form a list through next, sorted by index from the top of the stack down,
// as in C Lua.
type upValue struct {
	state  *State
	index  int
	closed value
	next   *upValue
}

type closure interface {
	upValue(i int) value
	setUpValue(i int, v value)
	upValueCount() int
}

type luaClosure struct {
	prototype *prototype
	upValues  []*upValue
	inline    [2]*upValue // backs upValues for closures with up to two
	own       upValue     // storage for the first upvalue this closure opens
}

type goClosure struct {
	function Function
	upValues []value
}

// Function wrapper, to allow go functions as keys in maps. Explicitly not a closure.
type goFunction struct {
	Function
	number *numberFunction // non-nil when the VM may call it frameless
}

func (c *luaClosure) upValue(i int) value { return c.upValues[i].value() }

func (c *luaClosure) setUpValue(i int, v value) {
	if uv := c.upValues[i]; uv.state != nil {
		uv.state.stack[uv.index] = v
	} else {
		uv.closed = v
	}
}

func (c *luaClosure) upValueCount() int        { return len(c.upValues) }
func (c *goClosure) upValue(i int) value       { return c.upValues[i] }
func (c *goClosure) setUpValue(i int, v value) { c.upValues[i] = v }
func (c *goClosure) upValueCount() int         { return len(c.upValues) }

func (uv *upValue) value() value {
	if uv.state != nil {
		return uv.state.stack[uv.index]
	}
	return uv.closed
}

func (uv *upValue) close() {
	if uv.state == nil {
		panic("attempt to close already-closed up value")
	}
	uv.closed, uv.state = uv.state.stack[uv.index], nil
}

func (uv *upValue) isInStackAt(level int) bool { return uv.state != nil && uv.index == level }

// sameHome reports whether uv and o refer to the same stack slot, or are
// both closed over equal values.
func (uv *upValue) sameHome(o *upValue) bool {
	if uv.state != nil || o.state != nil {
		return uv.state == o.state && uv.index == o.index
	}
	return uv.closed.identical(o.closed)
}

// close closes the open upvalues at or above stack index level, which head
// the sorted list.
func (l *State) close(level int) {
	for uv := l.upValues; uv != nil && uv.index >= level; uv = l.upValues {
		l.upValues, uv.next = uv.next, nil
		uv.close()
	}
}

// information about a call
type callInfo struct {
	function, top, resultCount int
	previous, next             *callInfo
	callStatus                 callStatus
	callMetamethods            uint8 // the __call metamethods the call went through, each adding an argument
	*luaCallInfo
	*goCallInfo
	extra int // a yield's function, or a yieldable pcall's old top

	// transferFirst and transferCount are the values a call or return
	// hook running for this call can see: the parameters or results, as
	// indices debug.getlocal takes. Zero outside such a hook.
	transferFirst, transferCount int
}

type luaCallInfo struct {
	frame   []value
	savedPC pc
	code    []bytecode.Instruction
	closure *luaClosure // the running function, as found at stack[function]
}

type goCallInfo struct {
	context, oldErrorFunction int
	continuation              Function
	oldAllowHook, shouldYield bool  // shouldYield: resumed after a yield, not an error
	error                     error // the error a yieldable pcall recovered from
}

func (ci *callInfo) setCallStatus(flag callStatus)     { ci.callStatus |= flag }
func (ci *callInfo) clearCallStatus(flag callStatus)   { ci.callStatus &^= flag }
func (ci *callInfo) isCallStatus(flag callStatus) bool { return ci.callStatus&flag != 0 }
func (ci *callInfo) isLua() bool                       { return ci.callStatus&callStatusLua != 0 }

func (ci *callInfo) stackIndex(slot int) int { return ci.top - len(ci.frame) + slot }
func (ci *callInfo) base() int               { return ci.top - len(ci.frame) }
func (ci *callInfo) skip()                   { ci.savedPC++ }
func (ci *callInfo) jump(offset int)         { ci.savedPC += pc(offset) }

// hasFrame reports whether ci.frame tracks the stack: true for Lua frames
// and for the base frame. A Go frame keeps a stale luaCallInfo from an
// earlier Lua call so the slot can switch kinds without allocating.
func (ci *callInfo) hasFrame() bool {
	return ci.luaCallInfo != nil && (ci.isLua() || ci.goCallInfo == nil)
}

func (ci *callInfo) setTop(top int) {
	if ci.hasFrame() {
		diff := top - ci.top
		ci.frame = ci.frame[:len(ci.frame)+diff]
	}
	ci.top = top
}

func (ci *callInfo) frameIndex(stackSlot int) int {
	if stackSlot < ci.top-len(ci.frame) || ci.top <= stackSlot {
		panic("frameIndex called with out-of-range stackSlot")
	}
	return stackSlot - ci.top + len(ci.frame)
}

func (l *State) pushLuaFrame(function, base, resultCount int, c *luaClosure) *callInfo {
	p := c.prototype
	ci := l.callInfo.next
	if ci == nil {
		l.charge(callInfoBytes + luaCallInfoBytes)
		ci = &callInfo{previous: l.callInfo, luaCallInfo: &luaCallInfo{}}
		l.callInfo.next = ci
	} else if ci.luaCallInfo == nil {
		l.charge(luaCallInfoBytes)
		ci.luaCallInfo = &luaCallInfo{}
	}
	ci.savedPC, ci.code, ci.closure = 0, p.Code, c
	ci.function = function
	ci.top = base + p.MaxStackSize
	// TODO l.assert(ci.top <= l.stackLast)
	ci.resultCount = resultCount
	ci.callStatus, ci.callMetamethods = callStatusLua, 0
	ci.frame = l.stack[base:ci.top]
	l.callInfo = ci
	l.top = ci.top
	return ci
}

func (l *State) pushGoFrame(function, resultCount int) {
	ci := l.callInfo.next
	if ci == nil {
		l.charge(callInfoBytes + goCallInfoBytes)
		ci = &callInfo{previous: l.callInfo, goCallInfo: &goCallInfo{}}
		l.callInfo.next = ci
	} else if ci.goCallInfo == nil {
		l.charge(goCallInfoBytes)
		ci.goCallInfo = &goCallInfo{}
	}
	ci.function = function
	ci.top = l.top + MinStack
	// TODO l.assert(ci.top <= l.stackLast)
	ci.resultCount = resultCount
	ci.callStatus, ci.callMetamethods = 0, 0
	l.callInfo = ci
}

func (ci *luaCallInfo) step() bytecode.Instruction {
	i := ci.code[ci.savedPC]
	ci.savedPC++
	return i
}

// newLuaClosure makes a closure of p, which the caller has counted: see
// closureCost.
func (l *State) newLuaClosure(p *prototype) *luaClosure {
	c := &luaClosure{prototype: p}
	if n := len(p.UpValues); n <= len(c.inline) {
		c.upValues = c.inline[:n]
	} else {
		c.upValues = make([]*upValue, n)
	}
	return c
}

// findUpValue returns the open upvalue for stack index level, creating it
// in sorted position if there is none. A new upvalue uses storage when it
// is non-nil.
func (l *State) findUpValue(level int, storage *upValue) *upValue {
	link := &l.upValues
	for uv := *link; uv != nil && uv.index >= level; uv = *link {
		if uv.index == level {
			return uv
		}
		link = &uv.next
	}
	uv := storage
	if uv == nil {
		uv = new(upValue) // counted with its closure: see chargeClosure
	}
	*uv = upValue{state: l, index: level, next: *link}
	*link = uv
	return uv
}

// newClosure makes a closure for CLOSURE, after running a Lua collection
// if one is due, as C Lua checks at CLOSURE. That leaves the running
// thread's stack where it is: see State.newTableAt.
func (l *State) newClosure(p *prototype, upValues []*upValue, base int) value {
	if l.global.gcMayBeDue() {
		l.checkGC()
	}
	l.charge(closureCost(p))
	c := l.newLuaClosure(p)
	p.cache = c
	storage := &c.own
	for i, uv := range p.UpValues {
		if uv.IsLocal { // upValue refers to local variable
			c.upValues[i] = l.findUpValue(base+uv.Index, storage)
			if c.upValues[i] == storage {
				storage = nil // used
			}
		} else { // get upValue from enclosing function
			c.upValues[i] = upValues[uv.Index]
		}
	}
	return objectValue(c)
}

func cached(p *prototype, upValues []*upValue, base int) *luaClosure {
	c := p.cache
	if c != nil {
		for i, uv := range p.UpValues {
			if uv.IsLocal && !c.upValues[i].isInStackAt(base+uv.Index) {
				return nil
			} else if !uv.IsLocal && !c.upValues[i].sameHome(upValues[uv.Index]) {
				return nil
			}
		}
	}
	return c
}

func (l *State) callGo(f value, function int, resultCount int, callMetamethods uint8) {
	l.checkStack(MinStack)
	l.pushGoFrame(function, resultCount)
	l.callInfo.callMetamethods = callMetamethods
	if l.hookMask&MaskCall != 0 {
		l.hookTransfer(HookCall, 1, l.top-function-1) // the arguments
	}
	var n int
	if c := f.goClosure(); c != nil {
		n = c.function(l)
	} else {
		n = f.goFunction().Function(l)
	}
	apiCheckStackSpace(l, n)
	l.postCall(l.top - n)
}

func (l *State) preCall(function int, resultCount int) bool {
	var metamethods uint8 // __call metamethods in the way, as ldo.c counts them
	for {
		switch fv := l.stack[function]; fv.kind() {
		case vkGoClosure, vkGoFunction:
			l.callGo(fv, function, resultCount, metamethods)
			return true
		case vkLuaClosure:
			f := fv.luaClosure()
			p := f.prototype
			if p.IsVarArg { // the fixed parameters move above the arguments, nils filling any missing
				l.checkStack(p.MaxStackSize + p.ParameterCount)
			} else {
				l.checkStack(p.MaxStackSize)
			}
			argCount, parameterCount := l.top-function-1, p.ParameterCount
			if argCount < parameterCount {
				extra := parameterCount - argCount
				args := l.stack[l.top : l.top+extra]
				clear(args)
				l.top += extra
				argCount += extra
			}
			base := function + 1
			if p.IsVarArg {
				base = l.adjustVarArgs(p, argCount)
			}
			ci := l.pushLuaFrame(function, base, resultCount, f)
			ci.callMetamethods = metamethods
			if l.hookMask&MaskCall != 0 {
				l.callHook(ci)
			}
			return false
		default:
			// A __call metamethod, which may be a callable table in turn, up
			// to 15 deep, as Lua 5.5 allows.
			tm := l.tagMethodByObject(l.stack[function], tmCall)
			if tm.isNil() {
				l.callError(l.stack[function])
			}
			if metamethods == 15 {
				l.runtimeError("'__call' chain too long")
			}
			metamethods++
			// Slide the args + function up 1 slot and poke in the tag method
			for p := l.top; p > function; p-- {
				l.stack[p] = l.stack[p-1]
			}
			l.top++
			l.checkStack(0)
			l.stack[function] = tm
		}
	}
}

func (l *State) callHook(ci *callInfo) {
	ci.savedPC++                             // hooks assume 'pc' is already incremented
	n := ci.closure.prototype.ParameterCount // the parameters, as luaD_hookcall
	if pci := ci.previous; pci.isLua() && pci.code[pci.savedPC-1].OpCode() == bytecode.OpTailCall {
		ci.setCallStatus(callStatusTail)
		l.hookTransfer(HookTailCall, 1, n)
	} else {
		l.hookTransfer(HookCall, 1, n)
	}
	ci.savedPC-- // correct 'pc'
}

func (l *State) adjustVarArgs(p *prototype, argCount int) int {
	fixedArgCount := p.ParameterCount
	l.assert(argCount >= fixedArgCount)
	// move fixed parameters to final position
	fixed := l.top - argCount // first fixed argument
	base := l.top             // final position of first argument
	fixedArgs := l.stack[fixed : fixed+fixedArgCount]
	copy(l.stack[base:base+fixedArgCount], fixedArgs)
	clear(fixedArgs)
	switch p.VarArgKind { // Lua 5.5's vararg table, after the parameters
	case bytecode.VarArgNone:
		l.stack[base+fixedArgCount] = nilValue // "(vararg table)"
	case bytecode.VarArgView:
		l.stack[base+fixedArgCount] = varArgView
	case bytecode.VarArgTable:
		l.stack[base+fixedArgCount] = l.varArgTable(l.stack[fixed+fixedArgCount : base])
	}
	return base
}

func (l *State) postCall(firstResult int) bool {
	ci := l.callInfo
	if l.hookMask&MaskReturn != 0 {
		base := ci.function + 1 // where debug.getlocal counts from
		if ci.isLua() {
			base = ci.base()
		}
		l.hookTransfer(HookReturn, firstResult-base+1, l.top-firstResult) // the results, as rethook
	}
	result, wanted, i := ci.function, ci.resultCount, 0
	l.callInfo = ci.previous // back to caller
	// Move the results down to the function's slot, then pad with nil to
	// the count wanted. With MultipleReturns (-1), i never reaches 0, so
	// every result moves and nothing is padded.
	for i = wanted; i != 0 && firstResult < l.top; i-- {
		l.stack[result] = l.stack[firstResult]
		result++
		firstResult++
	}
	for ; i > 0; i-- {
		l.stack[result] = nilValue
		result++
	}
	l.top = result
	if l.hookMask&(MaskReturn|MaskLine) != 0 && l.callInfo.isLua() {
		l.oldPC = l.callInfo.savedPC // oldPC for caller function
	}
	return wanted != MultipleReturns
}

// Call a Go or Lua function. The function to be called is at function.
// The arguments are on the stack, right after the function. On return, all the
// results are on the stack, starting at the original function position.
func (l *State) call(function int, resultCount int, allowYield bool) {
	if l.nestedGoCallCount++; l.nestedGoCallCount == maxCallCount {
		l.runtimeError("C stack overflow") // as C Lua says, which scripts match
	} else if l.nestedGoCallCount >= maxCallCount+maxCallCount>>3 {
		l.throw(ErrErrorHandler) // error while handling stack error
	}
	if !allowYield {
		l.nonYieldableCallCount++
	}
	if !l.preCall(function, resultCount) { // is a Lua function?
		if !l.global.jit || !l.callJIT() { // compiled code may run all of it
			l.execute()
		}
	}
	if !allowYield {
		l.nonYieldableCallCount--
	}
	if l.nestedGoCallCount--; l.nestedGoCallCount == 0 {
		l.dropInterrupt()
	}
}

func (l *State) throw(errorCode error) {
	if l.protectFunction != nil {
		panic(errorCode)
	} else {
		l.error = errorCode
		if g := l.global.mainThread; g.protectFunction != nil {
			g.push(l.stack[l.top-1])
			g.throw(errorCode)
		} else {
			if l.global.panicFunction != nil {
				l.global.panicFunction(l)
			}
			log.Panicf("Uncaught Lua error: %v", errorCode)
		}
	}
}

func (l *State) protect(f func()) (err error) {
	nestedGoCallCount, protectFunction := l.nestedGoCallCount, l.protectFunction
	l.protectFunction = func() {
		if e := recover(); e != nil {
			l.nestedGoCallCount, l.protectFunction = nestedGoCallCount, protectFunction
			if nestedGoCallCount == 0 {
				l.dropInterrupt()
			}
			var ok bool
			if err, ok = e.(error); !ok {
				panic(e) // not a Lua error: pass it on unchanged
			}
		}
	}
	defer l.protectFunction()
	f()
	l.nestedGoCallCount, l.protectFunction = nestedGoCallCount, protectFunction
	return err
}

// hookTransfer runs the hook for event, a call or return, able to see
// count values from local index first.
func (l *State) hookTransfer(event, first, count int) {
	ci := l.callInfo
	ci.transferFirst, ci.transferCount = first, count
	l.hook(event, -1)
	ci.transferFirst, ci.transferCount = 0, 0
}

func (l *State) hook(event, line int) {
	if l.hooker == nil || !l.allowHook {
		return
	}
	ci := l.callInfo
	top := l.top
	ciTop := ci.top
	ar := Debug{Event: event, CurrentLine: line, callInfo: ci}
	l.checkStack(MinStack)
	ci.setTop(l.top + MinStack)
	l.assert(ci.top <= l.stackLast)
	l.allowHook = false // can't hook calls inside a hook
	ci.setCallStatus(callStatusHooked)
	l.hooker(l, ar)
	l.assert(!l.allowHook)
	l.allowHook = true
	ci.setTop(ciTop)
	l.top = top
	ci.clearCallStatus(callStatusHooked)
}

func (l *State) initializeStack() {
	l.stack = make([]value, basicStackSize)
	l.stackLast = basicStackSize - extraStack
	l.top++
	l.baseCallInfo.luaCallInfo = &luaCallInfo{frame: l.stack[:0]}
	l.baseCallInfo.setTop(l.top + MinStack)
	l.callInfo = &l.baseCallInfo
}

func (l *State) checkStack(n int) {
	if l.stackLast-l.top <= n {
		l.growStack(n)
	}
}

func (l *State) reallocStack(newSize int) {
	l.assert(newSize <= maxStack || newSize == errorStackSize)
	l.assert(l.stackLast == len(l.stack)-extraStack)
	if newSize < len(l.stack) { // shrinking: what is above is not in use
		l.stack = append(make([]value, 0, newSize), l.stack[:newSize]...)
	} else {
		l.stack = append(l.stack, make([]value, newSize-len(l.stack))...)
	}
	l.stackLast = len(l.stack) - extraStack
	l.callInfo.next = nil
	for ci := l.callInfo; ci != nil; ci = ci.previous {
		if ci.hasFrame() {
			top := ci.top
			ci.frame = l.stack[top-len(ci.frame) : top]
		} else if ci.luaCallInfo != nil {
			ci.frame = nil // stale; drop the old stack
		}
	}
}

// shrinkStack gives back the stack an error left unused, after ldo.c's
// luaD_shrinkstack: in particular the extra room a stack overflow grows it
// by, so that the next overflow is an overflow again.
func (l *State) shrinkStack() {
	inUse := l.top
	for ci := l.callInfo; ci != nil; ci = ci.previous {
		inUse = max(inUse, ci.top)
	}
	inUse++
	goodSize := min(inUse+inUse/8+2*extraStack, maxStack)
	if inUse <= maxStack && goodSize < len(l.stack) {
		l.reallocStack(goodSize)
	}
}

func (l *State) growStack(n int) {
	if len(l.stack) > maxStack { // error after extra size?
		l.throw(ErrErrorHandler)
	} else {
		needed := l.top + n + extraStack
		newSize := 2 * len(l.stack)
		if newSize > maxStack {
			newSize = maxStack
		}
		if newSize < needed {
			newSize = needed
		}
		if newSize > maxStack { // stack overflow?
			l.reallocStack(errorStackSize)
			l.runtimeError("stack overflow")
		} else {
			l.charge((newSize - len(l.stack)) * slotBytes)
			l.reallocStack(newSize)
		}
	}
}
