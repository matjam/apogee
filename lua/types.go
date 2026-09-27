package lua

import (
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
)

// value is a Lua value in two words. p is nil for nil, a sentinel address
// for floats, integers, booleans, none and the empty string, or else a
// pointer: to a string's bytes or to an object. n holds a float, an
// integer's bits, or for other values the bits returned by x: the value's
// kind in the top byte, with a boolean or a string's length below it. n is
// a float64 so float arithmetic loads and stores it in floating-point
// registers without moves.
//
// An integer's bits can be anything, including an object's tag, so a test
// for an object or a string must rule out both number sentinels. They are
// adjacent (see numberSentinels), so that is one unsigned compare.
//
// The zero value is nil, so cleared registers need no initialisation.
// Values must not be compared with ==, which compares a number's bits and a
// string's address: use rawEqual. Values used as keys of table.hash are
// normalised by hashKey.
type value struct {
	_ [0]func() // not comparable; see rawEqual. First, so it adds no padding.
	p unsafe.Pointer
	n float64
}

// x returns v's second word as bits.
func (v value) x() uint64 { return math.Float64bits(v.n) }

func tagged(p unsafe.Pointer, bits uint64) value {
	return value{p: p, n: math.Float64frombits(bits)}
}

// valueKind is what a value holds. Its constants start with vk, because
// the parser's expression kinds already start with kind.
type valueKind uint8

const (
	vkNil valueKind = iota
	vkNone
	vkBool
	vkNumber
	vkString
	vkTable
	vkLuaClosure
	vkGoClosure
	vkGoFunction
	vkUserData
	vkThread
	vkLightUserData // p points to a boxed Go value; see lightUserData
)

const kindShift = 56

func tagOf(k valueKind) uint64 { return uint64(k) << kindShift }

// Sentinels give scalar values a non-nil p. They are bytes so that each
// has its own address. The float and integer sentinels are one array, so
// that a number is a p less than two bytes past numberPtr.
var (
	numberSentinels                                 [2]byte
	boolSentinel, noneSentinel, emptyStringSentinel byte
	varArgSentinel                                  byte
)

// The sentinel addresses are functions, not variables, so each check
// compiles to a compare with an address constant instead of a load.
// numberPtr is a float's; integerPtr, one byte on, an integer's.
func numberPtr() unsafe.Pointer      { return unsafe.Pointer(&numberSentinels[0]) }
func integerPtr() unsafe.Pointer     { return unsafe.Pointer(&numberSentinels[1]) }
func boolPtr() unsafe.Pointer        { return unsafe.Pointer(&boolSentinel) }
func nonePtr() unsafe.Pointer        { return unsafe.Pointer(&noneSentinel) }
func emptyStringPtr() unsafe.Pointer { return unsafe.Pointer(&emptyStringSentinel) }

// varArgView is the value of a named vararg table that is only indexed
// (bytecode.VarArgView): indexing it reads the frame's extra arguments,
// and nothing else ever sees it.
var varArgView = value{p: unsafe.Pointer(&varArgSentinel)}

var (
	nilValue   = value{}
	none       = tagged(nonePtr(), tagOf(vkNone))
	trueValue  = tagged(boolPtr(), tagOf(vkBool)|1)
	falseValue = tagged(boolPtr(), tagOf(vkBool))
)

// numberValue is the float f. integerValue is the integer i.
func numberValue(f float64) value { return value{p: numberPtr(), n: f} }
func integerValue(i int64) value  { return value{p: integerPtr(), n: math.Float64frombits(uint64(i))} }

func stringValue(s string) value {
	if len(s) == 0 {
		return tagged(emptyStringPtr(), tagOf(vkString))
	}
	return tagged(unsafe.Pointer(unsafe.StringData(s)), tagOf(vkString)|uint64(len(s)))
}

func boolValue(b bool) value {
	if b {
		return trueValue
	}
	return falseValue
}

func tableValue(t *table) value           { return tagged(unsafe.Pointer(t), tagOf(vkTable)) }
func luaClosureValue(c *luaClosure) value { return tagged(unsafe.Pointer(c), tagOf(vkLuaClosure)) }
func goClosureValue(c *goClosure) value   { return tagged(unsafe.Pointer(c), tagOf(vkGoClosure)) }
func goFunctionValue(f *goFunction) value { return tagged(unsafe.Pointer(f), tagOf(vkGoFunction)) }
func userDataValue(d *userData) value     { return tagged(unsafe.Pointer(d), tagOf(vkUserData)) }
func threadValue(l *State) value          { return tagged(unsafe.Pointer(l), tagOf(vkThread)) }
func lightUserDataValue(box *lightUserData) value {
	return tagged(unsafe.Pointer(box), tagOf(vkLightUserData))
}

// lightUserData boxes a Go value pushed as light userdata. A state interns
// boxes for comparable values, so equal Go values share a box and compare
// equal, as Go's == would.
type lightUserData struct{ v any }

// objectValue wraps a table, closure, userdata or thread.
func objectValue(x any) value {
	switch x := x.(type) {
	case *table:
		return tableValue(x)
	case *luaClosure:
		return luaClosureValue(x)
	case *goClosure:
		return goClosureValue(x)
	case *goFunction:
		return goFunctionValue(x)
	case *userData:
		return userDataValue(x)
	case *State:
		return threadValue(x)
	case closure:
		panic(fmt.Sprintf("unknown closure type %T", x))
	}
	panic(fmt.Sprintf("objectValue(%T)", x))
}

// valueOf converts a Go value into a Lua value. nil, float64, bool and
// string become the matching Lua values, apogee's own objects are stored as
// themselves, and any other value becomes light userdata.
func (l *State) valueOf(x any) value {
	switch x := x.(type) {
	case nil:
		return nilValue
	case float64:
		return numberValue(x)
	case float32:
		return numberValue(float64(x))
	case int64:
		return integerValue(x)
	case int:
		return integerValue(int64(x))
	case int32:
		return integerValue(int64(x))
	case bool:
		return boolValue(x)
	case string:
		return stringValue(x)
	case *table, *luaClosure, *goClosure, *goFunction, *userData, *State:
		return objectValue(x)
	}
	return lightUserDataValue(l.global.lightUserData(x))
}

func (g *globalState) lightUserData(x any) *lightUserData {
	if !reflect.ValueOf(x).Comparable() {
		return &lightUserData{x}
	}
	if g.lightBoxes == nil {
		g.lightBoxes = make(map[any]*lightUserData)
	}
	box, ok := g.lightBoxes[x]
	if !ok {
		box = &lightUserData{x}
		g.lightBoxes[x] = box
	}
	return box
}

func (v value) isNil() bool { return v.p == nil }

// isFloat and isInteger report whether v is a float or an integer, and
// isNumber whether it is either.
func (v value) isFloat() bool   { return v.p == numberPtr() }
func (v value) isInteger() bool { return v.p == integerPtr() }
func (v value) isNumber() bool  { return uintptr(v.p)-uintptr(numberPtr()) < 2 }

// f returns the float in v, which must be a float; i returns the integer
// in v, which must be an integer.
func (v value) f() float64 { return v.n }
func (v value) i() int64   { return int64(math.Float64bits(v.n)) }

// number returns v as a float, converting an integer, if it is a number.
func (v value) number() (float64, bool) {
	switch v.p {
	case numberPtr():
		return v.n, true
	case integerPtr():
		return float64(v.i()), true
	}
	return 0, false
}

// toFloat returns the number v, which must be a number, as a float.
func (v value) toFloat() float64 {
	if v.p == integerPtr() {
		return float64(v.i())
	}
	return v.n
}

// integer returns v as an integer if it is an integer, or a float with an
// integral value that an integer can hold, as lua_tointegerx does for
// numbers.
func (v value) integer() (int64, bool) {
	switch v.p {
	case integerPtr():
		return v.i(), true
	case numberPtr():
		return floatToInteger(v.n)
	}
	return 0, false
}

// floatToInteger returns f as an integer if it has an integral value that
// an integer can hold.
func floatToInteger(f float64) (int64, bool) {
	// -2^63 converts exactly; 2^63 is the first float too large.
	if f >= -(1<<63) && f < 1<<63 {
		if i := int64(f); float64(i) == f {
			return i, true
		}
	}
	return 0, false
}

func (v value) kind() valueKind {
	switch v.p {
	case numberPtr(), integerPtr():
		return vkNumber
	case nil:
		return vkNil
	}
	return valueKind(v.x() >> kindShift)
}

// is reports whether v is an object of kind k, which must not be a scalar
// kind. nil fails the tag test, since its bits are zero.
func (v value) is(k valueKind) bool { return v.x() == tagOf(k) && !v.isNumber() }

func (v value) boolean() (b, ok bool) {
	if v.p == boolPtr() {
		return v.x()&1 != 0, true
	}
	return false, false
}

func (v value) isString() bool {
	return !v.isNumber() && v.p != nil && v.x()>>kindShift == uint64(vkString)
}

func (v value) str() (string, bool) {
	if !v.isString() {
		return "", false
	}
	if v.p == emptyStringPtr() {
		return "", true
	}
	return unsafe.String((*byte)(v.p), int(v.x()&(1<<kindShift-1))), true
}

func (v value) table() *table {
	if v.is(vkTable) {
		return (*table)(v.p)
	}
	return nil
}

func (v value) luaClosure() *luaClosure {
	if v.is(vkLuaClosure) {
		return (*luaClosure)(v.p)
	}
	return nil
}

func (v value) goClosure() *goClosure {
	if v.is(vkGoClosure) {
		return (*goClosure)(v.p)
	}
	return nil
}

func (v value) goFunction() *goFunction {
	if v.is(vkGoFunction) {
		return (*goFunction)(v.p)
	}
	return nil
}

func (v value) userData() *userData {
	if v.is(vkUserData) {
		return (*userData)(v.p)
	}
	return nil
}

func (v value) thread() *State {
	if v.is(vkThread) {
		return (*State)(v.p)
	}
	return nil
}

// closure returns v as a closure, or nil when v is not a Lua or Go closure.
func (v value) closure() closure {
	switch v.kind() {
	case vkLuaClosure:
		return (*luaClosure)(v.p)
	case vkGoClosure:
		return (*goClosure)(v.p)
	}
	return nil
}

// isFunction reports whether v can be called without a __call metamethod.
func (v value) isFunction() bool {
	switch v.kind() {
	case vkLuaClosure, vkGoClosure, vkGoFunction:
		return true
	}
	return false
}

// obj returns v as a Go value for switching on its type: nil, float64,
// bool, string, an apogee object, or *lightUserData. Strings allocate, so hot
// paths use the typed accessors.
func (v value) obj() any {
	switch v.kind() {
	case vkNil, vkNone:
		return nil
	case vkNumber:
		if v.isInteger() {
			return v.i()
		}
		return v.f()
	case vkBool:
		return v.x()&1 != 0
	case vkString:
		s, _ := v.str()
		return s
	case vkTable:
		return (*table)(v.p)
	case vkLuaClosure:
		return (*luaClosure)(v.p)
	case vkGoClosure:
		return (*goClosure)(v.p)
	case vkGoFunction:
		return (*goFunction)(v.p)
	case vkUserData:
		return (*userData)(v.p)
	case vkThread:
		return (*State)(v.p)
	case vkLightUserData:
		return (*lightUserData)(v.p)
	}
	return nil
}

// toAny converts a Lua value into the Go value the public API exposes:
// float64 or int64, bool, string, nil, the object itself, or a light
// userdata's Go value.
func (v value) toAny() any {
	if b, ok := v.obj().(*lightUserData); ok {
		return b.v
	}
	return v.obj()
}

// identical reports whether v and w are the same bits: the same object, or
// the same scalar encoding.
func (v value) identical(w value) bool { return v.p == w.p && v.x() == w.x() }

// rawEqual reports whether a and b are equal without metamethods: numbers
// by mathematical value, strings by content, everything else by identity.
func rawEqual(a, b value) bool {
	switch a.p {
	case numberPtr():
		switch b.p {
		case numberPtr():
			return a.n == b.n
		case integerPtr():
			return floatEqualsInteger(a.n, b.i())
		}
		return false
	case integerPtr():
		switch b.p {
		case integerPtr():
			return a.i() == b.i()
		case numberPtr():
			return floatEqualsInteger(b.n, a.i())
		}
		return false
	}
	if a.isString() {
		if !b.isString() || a.x() != b.x() {
			return false
		}
		as, _ := a.str()
		bs, _ := b.str()
		return as == bs
	}
	return a.identical(b)
}

// hashKey is the table.hash key for k, which is neither nil, NaN nor a
// string, nor a float with an integral value: normaliseKey has made those
// integers.
func hashKey(k value) hashValue { return hashValue{p: k.p, x: k.x()} }

// normaliseKey returns k as a table stores it: a float with an integral
// value an integer can hold becomes that integer, so that 1 and 1.0 are
// one key and -0.0 is 0, as Lua 5.3 and later normalise keys.
func normaliseKey(k value) value {
	if k.p == numberPtr() {
		if i, ok := floatToInteger(k.n); ok {
			return integerValue(i)
		}
	}
	return k
}

// hashValue is a comparable copy of a value, for table.hash keys.
type hashValue struct {
	p unsafe.Pointer
	x uint64
}

func (h hashValue) value() value { return tagged(h.p, h.x) }

func isFalse(s value) bool {
	switch s.p {
	case nil, nonePtr():
		return true
	case boolPtr():
		return s.x()&1 == 0
	}
	return false
}

type userData struct {
	metaTable   *table
	userValues  []value // Lua 5.4's user values, any values, from 1
	data        any
	finalizable bool    // marked for finalization: see gc.go
	buf         *buffer // a buffer's memory, or nil: see buffer.go
}

type prototype struct {
	Constants                    []value
	Code                         []bytecode.Instruction
	exec                         []bytecode.Instruction // see execCode
	fields                       []fieldCache           // by pc, for exec's field instructions
	Prototypes                   []prototype
	LineInfo                     []int32
	LocalVariables               []bytecode.LocalVariable
	UpValues                     []bytecode.UpValueDesc
	cache                        *luaClosure
	Source                       string
	LineDefined, LastLineDefined int
	ParameterCount, MaxStackSize int
	IsVarArg                     bool
	VarArgKind                   bytecode.VarArgKind

	// JIT state, last so the interpreter's hot fields stay together.
	jitOn   bool                   // loaded by a state that compiles
	hot     int32                  // calls and loop iterations counted
	jit     *jitCode               // compiled code, or nil
	jitOrig []bytecode.Instruction // exec before JIT patches; see jit.go
	jitRuns []*uint64              // kernels' short runs, which compiled code counts: see kernelRuns
	jitKeep []*luaClosure          // functions kernels inline, whose addresses their code compares
}

func (p *prototype) upValueName(index int) string {
	if s := p.UpValues[index].Name; s != "" {
		return s
	}
	return "?"
}

func (p *prototype) lastLoad(reg int, lastPC pc) (loadPC pc, found bool) {
	var ip, jumpTarget pc
	for ; ip < lastPC; ip++ {
		i, maybe := p.Code[ip], false
		switch i.OpCode() {
		case bytecode.OpLoadNil:
			maybe = i.A() <= reg && reg <= i.A()+i.B()
		case bytecode.OpTForCall:
			maybe = reg >= i.A()+2
		case bytecode.OpCall, bytecode.OpTailCall:
			maybe = reg >= i.A()
		case bytecode.OpJump:
			if dest := ip + 1 + pc(i.SBx()); ip < dest && dest <= lastPC && dest > jumpTarget {
				jumpTarget = dest
			}
		case bytecode.OpTest:
			maybe = reg == i.A()
		default:
			maybe = bytecode.TestAMode(i.OpCode()) && reg == i.A()
		}
		if maybe {
			if ip < jumpTarget { // Can't know loading instruction because code is conditional.
				found = false
			} else {
				loadPC, found = ip, true
			}
		}
	}
	return
}

func (p *prototype) objectName(reg int, lastPC pc) (name, kind string) {
	if name, isLocal := p.localName(reg+1, lastPC); isLocal {
		return name, "local"
	}
	if pc, found := p.lastLoad(reg, lastPC); found {
		i := p.Code[pc]
		switch op := i.OpCode(); op {
		case bytecode.OpMove:
			if b := i.B(); b < i.A() {
				return p.objectName(b, pc)
			}
		case bytecode.OpGetTableUp:
			name, kind = p.constantName(i.C(), pc), "field"
			if p.upValueName(i.B()) == "_ENV" {
				kind = "global"
			}
			return
		case bytecode.OpGetTable:
			name, kind = p.constantName(i.C(), pc), "field"
			if v, ok := p.localName(i.B()+1, pc); ok && v == "_ENV" {
				kind = "global"
			}
			return
		case bytecode.OpGetUpValue:
			return p.upValueName(i.B()), "upvalue"
		case bytecode.OpLoadConstant:
			if s, ok := p.Constants[i.Bx()].str(); ok {
				return s, "constant"
			}
		case bytecode.OpLoadConstantEx:
			if s, ok := p.Constants[p.Code[pc+1].Ax()].str(); ok {
				return s, "constant"
			}
		case bytecode.OpSelf:
			if !bytecode.IsConstant(i.C()) { // as 5.5, which does a GETTABLE when the key is not a constant
				return p.constantName(i.C(), pc), "field"
			}
			return p.constantName(i.C(), pc), "method"
		}
	}
	return
}

func (p *prototype) constantName(k int, pc pc) string {
	if bytecode.IsConstant(k) {
		if s, ok := p.Constants[bytecode.ConstantIndex(k)].str(); ok {
			return s
		}
	} else if name, kind := p.objectName(k, pc); kind == "constant" {
		return name
	}
	return "?"
}

func (p *prototype) localName(index int, at pc) (string, bool) {
	pc := int(at)
	for i := 0; i < len(p.LocalVariables) && p.LocalVariables[i].StartPC <= pc; i++ {
		if pc < p.LocalVariables[i].EndPC {
			if index--; index == 0 {
				return p.LocalVariables[i].Name, true
			}
		}
	}
	return "", false
}

func (l *State) toString(index int) (s string, ok bool) {
	v := l.stack[index]
	if s, ok = v.str(); ok {
		return s, true
	}
	if s, ok = toString(v); ok { // a number, converted in place
		l.charge(len(s))
		l.stack[index] = stringValue(s)
	}
	return
}

func toString(r value) (string, bool) {
	if s, ok := r.str(); ok {
		return s, true
	}
	if r.isNumber() {
		return numberToString(r), true
	}
	return "", false
}

func pairAsStrings(p1, p2 value) (s1, s2 string, ok bool) {
	if s1, ok = p1.str(); !ok {
		return
	}
	s2, ok = p2.str()
	return
}
