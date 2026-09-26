package lua

import (
	"os"
	"runtime"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
	"github.com/matjam/apogee/internal/jit/call"
	"github.com/matjam/apogee/internal/jit/execmem"
)

// An Option configures a State created by NewState.
type Option func(*State)

// A new State compiles hot Lua functions to machine code on platforms that
// support it: linux and darwin on arm64 and amd64. Elsewhere it
// interprets them. Compiled code runs only while no debug hook is set.
// Setting the environment variable APOGEE_JIT=off disables compilation for
// every State.

// WithoutJIT turns the JIT off, so the State only interprets.
func WithoutJIT() Option {
	return func(l *State) { l.global.jit = false }
}

var jitDisabled = os.Getenv("APOGEE_JIT") == "off"

// jitDefault is whether a new State compiles.
var jitDefault = true

// jitThreshold is how many calls and loop iterations a function runs in
// the interpreter before it is compiled. Tests set it to zero.
var jitThreshold int32 = 1000

// jitBudget is how many loop back-edges compiled code runs before it
// returns to Go, so the goroutine can be preempted.
const jitBudget = 1 << 16

// jitContext is shared with generated code, which reads and writes it at
// fixed offsets. Keep the field order in sync with the compilers.
type jitContext struct {
	frame     unsafe.Pointer // &frame[0]
	constants unsafe.Pointer // &constants[0], or nil
	target    uintptr        // address to start at
	exitPC    uint64         // pc to resume the interpreter at
	budget    int64          // back-edges left
	upValues  unsafe.Pointer // &closure.upValues[0], or nil
	barrier   uint64         // nonzero while the GC write barrier is on
	reason    uint64         // why the last run exited, set by runJIT
	state     unsafe.Pointer // the *State, for calls and returns
	callee    unsafe.Pointer // the *goFunction or *goClosure a jitExitCallGo calls
	tbc       uintptr        // the innermost to-be-closed variable's address, or 0
	kernels   [2]uint64      // float and integer kernels entered, for tests
	spill     [4]uint64      // kernel registers an intrinsic's code needs, saved around it
	hoist     [8]uint64      // a kernel's upvalues: numbers' bits, and buffers' *buffer
}

// A jitExitCallGo exit leaves only the callee's object in the context, and
// runJIT calls its Function without knowing which kind it is, so the
// Function must come first in both.
// Each array length below overflows otherwise.
var (
	_ [0 - unsafe.Offsetof(goFunction{}.Function)]struct{}
	_ [0 - unsafe.Offsetof(goClosure{}.function)]struct{}
)

// writeBarrier is the runtime's flag that Go's own compiled code tests
// before storing a pointer. The runtime changes it only while the world is
// stopped, and a goroutine running generated code is never stopped, so a
// value read on entry holds for the whole run. While it is off, generated
// code stores any value directly, as Go does; while it is on, it stores
// only between nil and scalar values and exits otherwise.
//
//go:linkname writeBarrier runtime.writeBarrier
var writeBarrier struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// Reasons generated code returns.
const (
	jitExitInstruction = iota // the interpreter must run the instruction at exitPC
	jitExitBudget             // the back-edge budget ran out
	jitExitInterrupt          // enterJIT found an interrupt instead of entering
	jitExitCallGo             // the CALL at exitPC calls a Go function or Go closure
	jitExitCallNumber         // the CALL at exitPC calls a number function
)

// jitCode is a prototype's compiled code.
type jitCode struct {
	mem     *execmem.Code
	offsets []int32 // native offset of each pc's code, or -1
	base    uintptr // address of the code
	entry   uintptr // address of pc 0's code
	kernels int     // numeric loop kernels compiled, for tests

	returnsSoon bool // Go calling it interprets it; see returnsSoon
}

// The interpreter reaches compiled code through instructions patched into
// a prototype's exec code, so a state without the JIT runs unchanged code
// and pays nothing. prototype.jitOrig keeps the unpatched instructions.
//
// Until p is compiled, opJITCount sits at its entry and its loops' heads
// and latches (see patchJITCounters).
// Once it is hot and compiled, those revert and opJITEnter goes at each
// pc the compiler lists as an entry: function entry, loop latches, jump
// targets, and the instruction after each exit, so the interpreter hands
// back to compiled code as soon as it has run the instruction that exited.

// markJIT marks p and the prototypes nested in it for compilation.
func markJIT(p *prototype) {
	p.jitOn = true
	for i := range p.Prototypes {
		markJIT(&p.Prototypes[i])
	}
}

// patchJITCounters puts opJITCount at p's entry, numeric for loops'
// latches, and the heads of other loops: the targets of backward jumps,
// which while and repeat loops end with, and of TFORLOOP. A function that
// runs once with a hot while loop compiles as one called often does. It
// runs when p's exec code is built.
func (p *prototype) patchJITCounters() {
	p.jitOrig = append([]bytecode.Instruction(nil), p.exec...)
	count := func(ip int) {
		if !isConsumed(p.Code, ip) {
			p.exec[ip] = patched(p.jitOrig[ip], opJITCount)
		}
	}
	count(0)
	for ip, i := range p.Code {
		if isExtraArg(p.Code, ip) {
			continue
		}
		switch i.OpCode() {
		case bytecode.OpForLoop:
			count(ip)
		case bytecode.OpJump, bytecode.OpTForLoop:
			if i.SBx() < 0 {
				count(ip + 1 + i.SBx())
			}
		}
	}
}

// jitInstruction handles i, a patched instruction the interpreter fetched
// before ip. opJITCount counts; opJITEnter runs compiled code, which may
// advance the program and call or return into other functions. It returns
// the original instruction the interpreter must run next in l.callInfo,
// and the ip after that instruction. The interpreter reloads its frame from
// l.callInfo after every call.
//
// It stays out of line: its body inside the interpreter loop measured 10%
// slower on arithmetic.
//
//go:noinline
func (l *State) jitInstruction(i bytecode.Instruction, ip pc) (bytecode.Instruction, pc) {
	ci := l.callInfo
	if i.OpCode() == opJITCount {
		l.countJIT(ci.closure)
	} else if l.hookMask == 0 {
		l.runJIT(ci, ip-1, nil)
		ci = l.callInfo
		ip = ci.savedPC + 1
		ci.savedPC = ip
		// A Go function compiled code called may have set a hook, which the
		// interpreter would check before this instruction.
		if l.hookMask&(MaskLine|MaskCount) != 0 {
			if l.hookCount--; l.hookCount == 0 || l.hookMask&MaskLine != 0 {
				l.traceExecution()
			}
		}
	}
	return ci.closure.prototype.jitOrig[ip-1], ip
}

// countJIT counts one call or loop iteration of cl's prototype and compiles
// it once it is hot. The compiler may speculate on what cl's upvalues hold,
// checking before it relies on it.
func (l *State) countJIT(cl *luaClosure) {
	p := cl.prototype
	if p.hot++; p.hot <= jitThreshold || p.jit != nil {
		return
	}
	copy(p.exec, p.jitOrig) // remove the counters
	code, offsets, entries, kernels := compileJIT(p, l.global, cl)
	if code == nil {
		return
	}
	mem, err := execmem.Load(code)
	if err != nil {
		return
	}
	soon := returnsSoon(p.Code)
	p.jit = &jitCode{mem: mem, offsets: offsets, base: mem.Addr(0), entry: mem.Addr(int(offsets[0])), kernels: kernels, returnsSoon: soon}
	for _, ip := range entries {
		if ip == 0 && soon { // compiled callers call it natively
			continue
		}
		p.exec[ip] = patched(p.exec[ip], opJITEnter)
	}
}

// maxSetList bounds the elements a compiled SETLIST stores, which it
// addresses with an immediate offset.
const maxSetList = 1024

// maxInlineCompare is the longest string compiled == compares itself;
// it exits for longer ones, which Go compares faster.
const maxInlineCompare = 32

// jitMinRun is how many instructions compiled code must run from an entry
// before its first unconditional exit for entering it to pay: a round trip
// between the interpreter and compiled code costs about as much as
// interpreting a few instructions. Tests set it to zero, so compiled code
// is entered wherever it can run.
var jitMinRun = defaultJITMinRun

const defaultJITMinRun = 4

// jitEntries lists the pcs where the interpreter should hand over to
// compiled code: function entry, loop latches, jump targets, and the
// instruction after each pc where exits is true, since compiled code exits
// there. It leaves out entries that would reach an instruction always
// exits at, other than a call or return runJIT handles, too soon.
func jitEntries(p *prototype, exits, always []bool) []int {
	n := len(p.Code)
	entry := make([]bool, n)
	mark := func(ip int) {
		if 0 <= ip && ip < n && !isConsumed(p.Code, ip) && worthEntering(p.Code, always, ip) {
			entry[ip] = true
		}
	}
	mark(0)
	for ip, i := range p.Code {
		if isExtraArg(p.Code, ip) {
			continue
		}
		switch i.OpCode() {
		case bytecode.OpJump, bytecode.OpForPrep, bytecode.OpForLoop, bytecode.OpTForLoop:
			mark(ip + 1 + i.SBx())
		}
		if i.OpCode() == bytecode.OpForLoop {
			mark(ip)
		}
		if exits[ip] {
			next := ip + 1
			if isExtraArg(p.Code, next) {
				next++
			}
			mark(next)
		}
	}
	var entries []int
	for ip, ok := range entry {
		if ok {
			entries = append(entries, ip)
		}
	}
	return entries
}

// worthEntering reports whether compiled code entered at ip runs at least
// jitMinRun instructions, or reaches a loop, before an instruction the
// interpreter must run.
func worthEntering(code []bytecode.Instruction, always []bool, ip int) bool {
	for run := 0; ip < len(code); ip++ {
		if isExtraArg(code, ip) {
			continue
		}
		op := code[ip].OpCode()
		if always[ip] && op != bytecode.OpCall && op != bytecode.OpReturn && !jitSteps(code[ip]) {
			return false
		}
		if run++; run >= jitMinRun || op == bytecode.OpForLoop || op == bytecode.OpJump && code[ip].SBx() < 0 {
			return true
		}
	}
	return false
}

// returnsSoon reports whether code run from pc 0 reaches a RETURN before
// jitMinRun instructions, without a loop. Go or the interpreter calling
// such a function, a sort comparator for example, interprets it: its RETURN
// goes back to Go or to interpreted code, and the round trip into compiled
// code costs more than the instructions. Compiled callers still call it
// natively. It counts along the path that runs: a test and the JMP
// it consumes are one instruction, and a LOADBOOL that skips skips.
func returnsSoon(code []bytecode.Instruction) bool {
	for run, ip := 0, 0; ip < len(code); ip++ {
		i := code[ip]
		switch op := i.OpCode(); {
		case isExtraArg(code, ip):
		case isConsumed(code, ip):
			if op == bytecode.OpJump && i.SBx() < 0 { // a repeat loop's latch
				return false
			}
		case op == bytecode.OpReturn || op == bytecode.OpTailCall:
			return run < jitMinRun
		default:
			if run++; run >= jitMinRun || op == bytecode.OpForLoop || op == bytecode.OpJump && i.SBx() < 0 {
				return false
			}
			if op == bytecode.OpLoadBool && i.C() != 0 {
				ip++
			}
		}
	}
	return false
}

func patched(i bytecode.Instruction, op bytecode.OpCode) bytecode.Instruction {
	i.SetOpCode(op)
	return i
}

// isConsumed reports whether the instruction before code[ip] reads it as
// part of itself: an extra-argument word, the JMP after a test, or the
// TFORLOOP after TFORCALL. Such instructions are never patched.
func isConsumed(code []bytecode.Instruction, ip int) bool {
	if ip == 0 {
		return false
	}
	switch code[ip-1].OpCode() {
	case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual, bytecode.OpTest, bytecode.OpTestSet, bytecode.OpTForCall:
		return true
	}
	return isExtraArg(code, ip)
}

// isExtraArg reports whether code[ip] is the extra-argument word of the
// instruction before it rather than an instruction.
// hasTBC reports whether p declares to-be-closed variables, which a
// generic for does.
func hasTBC(p *prototype) bool {
	for _, i := range p.Code {
		if i.OpCode() == bytecode.OpTBC {
			return true
		}
	}
	return false
}

func isExtraArg(code []bytecode.Instruction, ip int) bool {
	if ip == 0 {
		return false
	}
	switch prev := code[ip-1]; prev.OpCode() {
	case bytecode.OpLoadConstantEx, bytecode.OpBitwise:
		return true
	case bytecode.OpSetList:
		return prev.C() == 0
	}
	return false
}

// runJIT runs compiled code from ip in ci's function. It handles the calls
// and returns compiled code exits at itself, moving between compiled
// functions without the interpreter, and stops at the first instruction
// the interpreter must run: that instruction's pc is left in the savedPC
// of l.callInfo, which may now be another frame.
//
// bottom, when not nil, is a frame that Go called through l.call. runJIT
// returns from it to Go itself, as the interpreter would, and then reports
// true.
//
// c and p follow ci, and are reloaded only when ci or its closure changes:
// an exit for a Go call comes back to the frame it left, and the chain of
// loads from ci to its prototype would otherwise be repeated on every
// crossing.
func (l *State) runJIT(ci *callInfo, ip pc, bottom *callInfo) bool {
	c := ci.closure
	p := c.prototype
	for {
		jc := p.jit
		if jc == nil || l.hookMask != 0 {
			break
		}
		off := jc.offsets[ip]
		if off < 0 {
			break
		}
		l.enterJIT(ci, c, p, jc, off, ip)
		// Compiled calls and returns move between frames. A frame compiled
		// code returned from and another call reused can be ci again,
		// running another function.
		if nci := l.callInfo; nci != ci || nci.closure != c {
			ci, c = nci, nci.closure
			p = c.prototype
		}
		ip = pc(l.jitCtx.exitPC)
		switch l.jitCtx.reason {
		case jitExitBudget:
			if l.global.interrupt.Load() {
				ci.savedPC = ip + 1 // where the error reports
				l.interrupted()
			}
			runtime.Gosched()
			continue
		case jitExitInterrupt:
			ci.savedPC = ip + 1
			l.interrupted()
			continue
		case jitExitCallGo:
			l.jitCallGoFunction(ci, p.jitOrig[ip], ip)
			ip++
			continue
		case jitExitCallNumber:
			l.jitCallNumber(ci, p.jitOrig[ip], ip)
			ip++
			continue
		}
		if l.hookMask != 0 { // a Go function set a hook
			break
		}
		switch i := p.jitOrig[ip]; i.OpCode() {
		case bytecode.OpCall:
			if nci, ok := l.jitCall(ci, i, ip); ok {
				if nci == ci {
					ip++
				} else {
					ci, ip, c = nci, 0, nci.closure
					p = c.prototype
				}
				continue
			}
		case bytecode.OpReturn:
			if ci == bottom {
				if l.jitReturnToGo(ci, p, i) {
					return true
				}
			} else if l.jitReturn(ci, i) {
				ci = l.callInfo
				ip, c = ci.savedPC, ci.closure
				p = c.prototype
				continue
			}
		case bytecode.OpJump: // one that closes upvalues, which compiled code leaves to Go
			ci.savedPC = ip + 1
			ip = l.jumpFrom(ci, i, ip+1)
			continue
		default:
			if jitSteps(i) {
				ci.savedPC = ip + 1
				l.jitStep(ci, i, ip)
				ip++
				continue
			}
		}
		break
	}
	ci.savedPC = ip
	return false
}

// callJIT runs the Lua function that preCall has just entered for l.call,
// if it is compiled and does not return too soon to be worth entering. It
// reports whether the function has returned; otherwise the interpreter goes
// on from the savedPC of l.callInfo.
func (l *State) callJIT() bool {
	ci := l.callInfo
	if jc := ci.closure.prototype.jit; jc == nil || jc.returnsSoon || l.hookMask != 0 {
		return false
	}
	return l.runJIT(ci, 0, ci)
}

// jitReturnToGo runs the RETURN i that compiled code exited at in ci, a
// frame Go called running p, as the interpreter's general RETURN does. It
// reports false, having changed nothing, when the results run up to
// l.top, which compiled code does not track.
func (l *State) jitReturnToGo(ci *callInfo, p *prototype, i bytecode.Instruction) bool {
	a, b := i.A(), i.B()
	if b == 0 || l.hasTBC(ci.base()) {
		return false
	}
	l.top = ci.stackIndex(a + b - 1)
	if len(p.Prototypes) > 0 {
		l.close(ci.base())
	}
	l.postCall(ci.stackIndex(a))
	return true
}

// jitSteps reports whether runJIT runs i itself when compiled code exits
// at it, and goes on in compiled code after it: a SETLIST only of a fixed
// count of values, without an extra argument.
func jitSteps(i bytecode.Instruction) bool {
	switch i.OpCode() {
	case bytecode.OpJump, bytecode.OpNewTable, bytecode.OpClosure, bytecode.OpLength, bytecode.OpGetTable, bytecode.OpGetTableUp, bytecode.OpSelf, bytecode.OpSetTable, bytecode.OpSetTableUp,
		opGetField, opGetFieldUp, opSelfField, opSetField, opSetFieldUp:
		return true
	case bytecode.OpSetList:
		return i.B() != 0 && i.C() != 0
	}
	return false
}

// jitStep runs i, the exec instruction at ip, as the interpreter would.
// Each case is a copy of the interpreter's.
func (l *State) jitStep(ci *callInfo, i bytecode.Instruction, ip pc) {
	closure, frame := ci.closure, ci.frame
	constants := closure.prototype.Constants
	switch i.OpCode() {
	case bytecode.OpNewTable:
		a := i.A()
		b, c := bytecode.IntFromFloat8(i.B()), bytecode.IntFromFloat8(i.C())
		frame[a] = objectValue(l.newTableAt(&closure.prototype.fields[ip], b, c))
		clear(frame[a+1:])
	case bytecode.OpClosure:
		a, p := i.A(), &closure.prototype.Prototypes[i.Bx()]
		if ncl := cached(p, closure.upValues, ci.base()); ncl == nil {
			frame[a] = l.newClosure(p, closure.upValues, ci.base())
		} else {
			frame[a] = objectValue(ncl)
		}
		clear(frame[a+1:])
	case bytecode.OpLength:
		tmp := l.objectLength(frame[i.B()])
		ci.frame[i.A()] = tmp
	case bytecode.OpGetTableUp:
		tmp := l.tableAt(closure.upValue(i.B()), k(i.C(), constants, frame))
		ci.frame[i.A()] = tmp
	case bytecode.OpGetTable:
		tmp := l.tableAt(frame[i.B()], k(i.C(), constants, frame))
		ci.frame[i.A()] = tmp
	case bytecode.OpSetTableUp:
		l.setTableAt(closure.upValue(i.A()), k(i.B(), constants, frame), k(i.C(), constants, frame))
	case bytecode.OpSetTable:
		l.setTableAt(frame[i.A()], k(i.B(), constants, frame), k(i.C(), constants, frame))
	case bytecode.OpSelf:
		a, t := i.A(), frame[i.B()]
		tmp := l.tableAt(t, k(i.C(), constants, frame))
		frame = ci.frame
		frame[a+1], frame[a] = t, tmp
	case opGetField, opGetFieldUp, opSelfField:
		var t value
		if i.OpCode() == opGetFieldUp {
			t = closure.upValue(i.B())
		} else {
			t = frame[i.B()]
		}
		key := constants[i.C()]
		fc := &closure.prototype.fields[ip]
		v, ok := getField(t, key, fc)
		if !ok && t.isString() { // the interpreter leaves strings to tableAt
			v, ok = getStringField(l.global.metaTables[TypeString], key, fc)
		}
		if !ok {
			v = l.tableAt(t, key)
			frame = ci.frame
		}
		if i.OpCode() == opSelfField {
			frame[i.A()+1] = t
		}
		frame[i.A()] = v
	case opSetField, opSetFieldUp:
		var t value
		if i.OpCode() == opSetFieldUp {
			t = closure.upValue(i.A())
		} else {
			t = frame[i.A()]
		}
		key, v := constants[i.B()], k(i.C(), constants, frame)
		if !setField(t, key, v, &closure.prototype.fields[ip]) {
			l.setTableAt(t, key, v)
		}
	case bytecode.OpSetList: // a fixed count, as jitSteps allows
		a, n := i.A(), i.B()
		h := frame[a].table()
		start := (i.C() - 1) * bytecode.ListItemsPerFlush
		if last := start + n; last > len(h.array) {
			h.extendArray(l, last)
		}
		copy(h.array[start:start+n], frame[a+1:a+1+n])
	}
}

// enterJIT runs ci's compiled code from native offset off, pc ip, until it
// exits, leaving the exit's pc and reason in l.jitCtx.
//
// Every 1024th entry it checks for an interrupt first, and reports one as
// jitExitInterrupt at ip. The budget alone would not find it: it is
// refilled on every entry, so a loop that leaves compiled code on each
// iteration never runs out.
func (l *State) enterJIT(ci *callInfo, c *luaClosure, p *prototype, jc *jitCode, off int32, ip pc) {
	ctx := &l.jitCtx
	if l.jitRuns++; l.jitRuns%1024 == 0 && l.global.interrupt.Load() {
		ctx.exitPC, ctx.reason = uint64(ip), jitExitInterrupt
		return
	}
	// Compiled code reads constants and upvalues only when the function
	// has them, so an empty slice's data pointer, whatever it is, does.
	frame := ci.frame
	ctx.frame = unsafe.Pointer(unsafe.SliceData(frame))
	ctx.constants = unsafe.Pointer(unsafe.SliceData(p.Constants))
	ctx.upValues = unsafe.Pointer(unsafe.SliceData(c.upValues))
	ctx.barrier = 0
	if writeBarrier.enabled {
		ctx.barrier = 1
		l.jitBarrierRuns++
	}
	ctx.target = jc.base + uintptr(off)
	ctx.budget = jitBudget
	ctx.state = unsafe.Pointer(l)
	// Only Go makes variables to be closed, and it enters compiled code
	// again afterwards, so this holds for the run: a RETURN in a frame
	// below it returns in compiled code.
	ctx.tbc = 0
	if n := len(l.tbc); n > 0 {
		ctx.tbc = uintptr(unsafe.Pointer(&l.stack[l.tbc[n-1]]))
	}
	ctx.reason = call.Call(jc.base, unsafe.Pointer(ctx))
	runtime.KeepAlive(frame)
	runtime.KeepAlive(c)
}

// jitCall runs the CALL i at ip that compiled code exited at, when the
// callee is a Go function, or a compiled Lua function it can enter
// directly. It returns ci when the call is done and compiled code can go
// on after it, or the callee's new frame. It reports false, having changed
// nothing, when the interpreter must make the call.
func (l *State) jitCall(ci *callInfo, i bytecode.Instruction, ip pc) (*callInfo, bool) {
	a, b, c := i.A(), i.B(), i.C()
	if b == 0 { // arguments up to l.top, which compiled code does not track
		return nil, false
	}
	switch fv := ci.frame[a]; fv.kind() {
	case vkGoFunction, vkGoClosure:
		if c == 0 {
			return nil, false
		}
		l.jitCallGo(ci, i, ip)
		return ci, true
	case vkLuaClosure:
		f := fv.luaClosure()
		if f.prototype.IsVarArg || f.prototype.jit == nil {
			return nil, false
		}
		ci.savedPC = ip + 1
		return l.callLua(ci, f, a, b-1, c-1), true
	}
	return nil, false
}

// jitCallGo runs the CALL i at ip, which has fixed arguments and results
// and calls the Go function or Go closure in its register A.
func (l *State) jitCallGo(ci *callInfo, i bytecode.Instruction, ip pc) {
	a, b, c := i.A(), i.B(), i.C()
	frame := ci.frame
	fv := frame[a]
	ci.savedPC = ip + 1
	if f := fv.goFunction(); f != nil && f.number != nil && b > 1 {
		if r, ok := f.number.tryCall(frame[a+1 : a+b]); ok {
			l.numberResult(ci, a, c-1, f.number.results, r)
			return
		}
	}
	l.top = ci.stackIndex(a + b)
	l.callGo(fv, ci.stackIndex(a), c-1, 0)
	l.top = ci.top
}

// jitCallGoFunction runs the CALL i at ip that compiled code exited at
// with jitExitCallGo: fixed arguments and results, to the Go function or
// Go closure whose object compiled code left in l.jitCtx.callee, with the
// frame's address in l.jitCtx.frame. It does what callGo does, without
// reloading the callee from the frame, and copies the results itself
// unless a hook is set. No hook is set on entry, as compiled code runs
// only without one.
func (l *State) jitCallGoFunction(ci *callInfo, i bytecode.Instruction, ip pc) {
	ctx := &l.jitCtx
	f := *(*Function)(ctx.callee)
	ctx.callee = nil
	a, b, wanted := i.A(), i.B(), i.C()-1
	base := int((uintptr(ctx.frame) - uintptr(unsafe.Pointer(unsafe.SliceData(l.stack)))) / unsafe.Sizeof(value{}))
	function := base + a
	ci.savedPC = ip + 1
	l.top = function + b
	l.checkStack(MinStack)
	l.pushGoFrame(function, wanted)
	n := f(l)
	apiCheckStackSpace(l, n)
	if l.hookMask != 0 { // the function set one
		l.postCall(l.top - n)
	} else {
		// A loop, not copy: copy of a few values is a runtime call.
		l.callInfo = ci
		first, k := l.top-n, 0
		for ; k < wanted && k < n; k++ {
			l.stack[function+k] = l.stack[first+k]
		}
		for ; k < wanted; k++ {
			l.stack[function+k] = nilValue
		}
	}
	l.top = ci.top
}

// jitCallNumber runs the CALL i at ip that compiled code exited at with
// jitExitCallNumber, leaving the number function's *goFunction in
// l.jitCtx.callee and the frame's address in l.jitCtx.frame. It calls the
// function without a frame, as the interpreter does, when the arguments
// fit it, and otherwise as an ordinary Go function.
func (l *State) jitCallNumber(ci *callInfo, i bytecode.Instruction, ip pc) {
	ctx := &l.jitCtx
	nf := (*goFunction)(ctx.callee).number
	a, b, wanted := i.A(), i.B(), i.C()-1
	function := int((uintptr(ctx.frame)-uintptr(unsafe.Pointer(unsafe.SliceData(l.stack))))/unsafe.Sizeof(value{})) + a
	r, ok := nf.tryCall(l.stack[function+1 : function+b])
	if !ok {
		l.jitCallGoFunction(ci, i, ip)
		return
	}
	ctx.callee = nil
	ci.savedPC = ip + 1
	for k := range wanted { // a loop, not clear: see jitCallGoFunction
		l.stack[function+k] = nilValue
	}
	if wanted > 0 && nf.results == 1 {
		l.stack[function] = numberValue(r)
	}
	l.top = ci.top
}

// jitReturn runs the RETURN i that compiled code exited at, when it returns
// a fixed number of results to a Lua caller in the same interpreter loop
// that wants a fixed number. It reports false, having changed nothing,
// when the interpreter must return.
func (l *State) jitReturn(ci *callInfo, i bytecode.Instruction) bool {
	a, b, wanted := i.A(), i.B(), ci.resultCount
	if b == 0 || wanted < 0 || !ci.isCallStatus(callStatusReentry) || l.hasTBC(ci.base()) {
		return false
	}
	if len(ci.closure.prototype.Prototypes) > 0 {
		l.close(ci.base())
	}
	res, results := l.stack[ci.function:ci.function+wanted], ci.frame[a:a+b-1]
	n := copy(res, results)
	clear(res[n:])
	ci = ci.previous
	l.callInfo, l.top = ci, ci.top
	return true
}
