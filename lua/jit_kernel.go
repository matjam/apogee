//go:build (darwin || linux) && (arm64 || amd64)

package lua

import (
	"maps"
	"math"
	"slices"

	"github.com/matjam/apogee/internal/bytecode"
)

// Numeric loop kernels.
//
// An innermost numeric for loop whose body only moves numbers, loads
// number constants, does arithmetic and compares numbers is also compiled
// as a kernel: each Lua register the loop uses lives in a machine register,
// a general-purpose one for an integer and a floating-point one for a
// float, so iterations run without loads, stores or type checks. The
// body's temporaries share machine registers where their lives do not
// overlap (allocate). Each register has one type throughout, which planKernel
// infers from the loop's kind (an integer or a float loop) and the body;
// a register nothing decides, such as an accumulator that only adds to
// itself or integer constants, is an integer. A loop may get two kernels, one for each
// kind. A kernel starts at the FORLOOP, after checking that the registers
// the body reads before writing hold numbers of their types, and writes
// the registers back when the loop ends or its budget runs out. When the
// check fails the loop runs in the ordinary compiled code, which comes
// back to the check at every iteration.

// A kernel may also call an intrinsic, and read and write buffers:
//
//   - GETUPVAL A, n followed, with only arithmetic, buffer reads and other
//     intrinsic calls between, by CALL A of an intrinsic (sqrt, sin, cos,
//     or a math function) the upvalue holds when the function compiles. The kernel checks on entry that the upvalue still holds it;
//     the GETUPVAL emits nothing, and the CALL computes the intrinsic on
//     registers.
//   - GETTABLE and SETTABLE of a buffer in a register the loop only reads,
//     at an integer key. The kernel checks on entry that the register holds
//     a buffer, of floats if the loop reads it.
//   - Upvalues holding numbers or buffers (GETUPVAL, and GETTABUP and
//     SETTABUP of a buffer), which nothing in a kernel can change. The
//     kernel checks them on entry and keeps them in jitContext.hoist; a
//     register GETUPVAL gives a buffer aliases it there, and gets its value
//     only when the kernel leaves.
//
// Those instructions can find what the kernel cannot handle: a key outside
// the buffer, or an argument sin or cos leave to Go. They leave the kernel
// there, a side exit: the kernel writes its registers back, as at the end
// of the loop, and the ordinary code runs the instruction and the rest of
// the iteration. Registers the iteration has yet to write hold stale
// numbers until it does.

// kernelPlan is a loop that qualifies as a kernel.
type kernelPlan struct {
	start, latch int             // the body's first pc, and the FORLOOP's
	base         int             // the FORLOOP's A
	intLoop      bool            // an integer loop, or a float one
	shareVar     bool            // the body does not write the loop variable
	types        map[int]numKind // the loop's and live-in registers' types on entry
	liveIn       []int           // registers read before written, checked on entry
	written      []int           // registers written back when the loop ends
	calls        map[int]kernelCall
	virtual      map[int]bool // GETUPVAL pcs that emit nothing
	buffers      map[int]bool // registers holding buffers, true for those read

	// Instructions the kernel leaves at, for the ordinary code to run them
	// and the rest of the iteration (exits), and those it cannot reach,
	// after one, which it leaves out (unreached). badPC is the pc checkTypes
	// stopped at, or -1.
	exits, unreached map[int]bool
	badPC            int

	// Upvalues the body reads, checked and loaded on entry into
	// jitContext.hoist, by slot; nothing in a kernel can change them.
	hoisted []hoistedUpValue
	upLoads map[int]int // GETUPVAL pcs, by pc, and their slots
	bufUses []bufUse    // GETTABLE and SETTABLE pcs, resolved after typing
	bufFrom map[int]int // a buffer access's hoisted slot, or -1 for a register

	// LOADK pcs of integer constants the kernel loads as floats, which no
	// use can tell apart, provided the fields in needFloat are floats.
	promoted  map[int]bool
	floor     bool     // // of floats may compile: see planKernel
	floatKeys bool     // a buffer is indexed by a float: see bufferKey
	needFloat [][2]int // pc and RK field

	// A register may hold an integer at one pc and a float at another, as
	// Lua reuses registers for temporaries: at gives each register's type
	// before each body pc and at the latch, and results the type each body
	// instruction writes.
	at      []map[int]numKind
	results map[int]numKind
}

// kernelCall is an intrinsic call in a kernel, by the CALL's pc: of fn,
// the intrinsic's Go function, which upvalue upValue must hold.
type kernelCall struct {
	upValue int
	fn      uint64
	get, a  int    // the GETUPVAL's pc, and the register it and the CALL name
	args    int    // one or two, whether B gives them or, when 0, l.top
	math    mathFn // a math function, or mathNone for a number function
}

// calleesAt returns the intrinsic calls whose callee register the
// ordinary code expects to hold the function at body pc ip: after their
// GETUPVAL, up to their CALL. A side exit there stores it.
func (k *kernelPlan) calleesAt(ip int) []kernelCall {
	var out []kernelCall
	for call, kc := range k.calls {
		if kc.get < ip && ip <= call {
			out = append(out, kc)
		}
	}
	return out
}

// hoistedUpValue is an upvalue a kernel reads: a number of kind, or a
// buffer (kindBuffer), which read says the kernel reads from.
type hoistedUpValue struct {
	n    int
	kind numKind
	read bool
}

// maxHoisted is how many upvalues jitContext.hoist holds.
const maxHoisted = 8

// bufUse is a buffer access at ip, of register reg, reading or writing.
type bufUse struct {
	ip, reg int
	read    bool
}

// kindOf is the type a register loaded from hoisted slot s takes: the
// upvalue's number kind, or an alias of its buffer.
func (h hoistedUpValue) kindOf(s int) numKind {
	if h.kind == kindBuffer {
		return kindBufferAlias + numKind(s)
	}
	return h.kind
}

// floatModDivisor returns constant v as a float divisor compiled code
// computes % of floats by: a power of two, 1 to 2^52 either sign. Then
// a / v and trunc(a / v) * v are exact, so a - trunc(a / v) * v is
// fmod(a, v) exactly, as FloatMod starts from.
func floatModDivisor(v value) (float64, bool) {
	if !v.isNumber() {
		return 0, false
	}
	f := v.toFloat()
	frac, exp := math.Frexp(math.Abs(f))
	return f, frac == 0.5 && exp >= 1 && exp <= 53
}

// kernelDivisor reports whether constant v may divide in a kernel: a
// nonzero integer small enough for an immediate.
func kernelDivisor(v value) bool {
	i, ok := v.integer()
	return ok && v.isInteger() && i != 0 && -(1<<31) <= i && i < 1<<31
}

// planKernel returns the kernel for the FORLOOP at latch in p, as an
// integer or a float loop, or nil when its loop does not qualify. constOK
// reports whether generated code can reach constant k; intrinsic returns
// the intrinsic upvalue n holds, if it holds one compiled code computes: a
// number function's code, or a math function (mathFn) and its code;
// upValue returns the kind of number, or kindBuffer, upvalue n holds, if
// it holds one; floor reports whether the machine rounds a float down in
// one instruction, for // of floats.
func planKernel(p *prototype, latch int, intLoop bool, constOK func(k int) bool, intrinsic func(n int) (uint64, mathFn, bool), upValue func(n int) (numKind, bool), floor bool) *kernelPlan {
	exits := map[int]bool{}
	for range maxExits + 1 {
		k, bad := planKernelWith(p, latch, intLoop, constOK, intrinsic, upValue, floor, exits)
		if k != nil || bad < 0 || exits[bad] || len(exits) == maxExits {
			return k
		}
		exits[bad] = true
	}
	return nil
}

// maxExits is how many instructions planKernel lets a kernel leave at.
const maxExits = 8

// planKernelWith is planKernel with the instructions at exits left to the
// ordinary code. When the loop does not qualify it returns the pc of an
// instruction that stops it, or -1.
func planKernelWith(p *prototype, latch int, intLoop bool, constOK func(k int) bool, intrinsic func(n int) (uint64, mathFn, bool), upValue func(n int) (numKind, bool), floor bool, exits map[int]bool) (*kernelPlan, int) {
	code := p.Code
	fl := code[latch]
	start := latch + 1 + fl.SBx()
	if start > latch {
		return nil, -1
	}
	k := &kernelPlan{start: start, latch: latch, base: fl.A(), intLoop: intLoop, types: map[int]numKind{},
		calls: map[int]kernelCall{}, virtual: map[int]bool{}, buffers: map[int]bool{},
		upLoads: map[int]int{}, bufFrom: map[int]int{}, floor: floor, exits: map[int]bool{}, unreached: map[int]bool{}}
	wrote := map[int]bool{} // registers the body writes, which cannot hold buffers
	used := map[int]bool{}  // registers holding numbers
	base := fl.A()
	use := func(r int) { used[r] = true }
	loopKind := kindFloat
	if intLoop {
		loopKind = kindInt
	}
	for r := base; r <= base+3; r++ {
		use(r)
		k.types[r] = loopKind
	}
	// FORLOOP defines the loop registers before the body; the index,
	// limit and step are checked on entry.
	defined := map[int]bool{base + 3: true}
	k.liveIn = []int{base, base + 1, base + 2}
	k.written = []int{base, base + 1, base + 3}
	if !intLoop {
		k.written = []int{base, base + 3} // a float loop's limit stays
	}
	seen := map[int]bool{base: true, base + 1: true, base + 2: true, base + 3: true}
	// skipped reports whether a jump from before ip lands after it, so
	// that a write at ip may not happen.
	skipped := func(ip int) bool {
		for j := start; j < ip; j++ {
			if t, ok := kernelJump(code, j, latch); ok && t > ip {
				return true
			}
		}
		return false
	}
	number := func(kk int) bool { return constOK(kk) && constKind(p.Constants[kk]) != kindAny }
	// A body temporary first written where a jump may skip the write is
	// live in only if the body reads it: one only an exit reads is dead
	// where the write is skipped.
	maybe := map[int]bool{}
	read := func(field int) bool {
		if bytecode.IsConstant(field) {
			return number(bytecode.ConstantIndex(field))
		}
		use(field)
		if maybe[field] {
			delete(maybe, field)
			k.liveIn = append(k.liveIn, field) // may keep its old value
		}
		if !seen[field] {
			seen[field] = true
			if !defined[field] {
				k.liveIn = append(k.liveIn, field)
			}
		}
		return true
	}
	write := func(r, ip int) bool {
		wrote[r] = true
		use(r)
		if !seen[r] {
			seen[r] = true
			switch {
			case !skipped(ip):
				defined[r] = true
			case r > base+3:
				maybe[r] = true
			default:
				k.liveIn = append(k.liveIn, r) // may keep its old value
			}
		}
		k.written = append(k.written, r)
		return true
	}
	live, targets := true, map[int]bool{} // whether ip is reachable, and where jumps go
	for ip := start; ip < latch; ip++ {
		i := code[ip]
		if targets[ip] {
			live = true
		}
		if !live || isExtraArg(code, ip) {
			k.exits[ip], k.unreached[ip] = true, true
			continue
		}
		if exits[ip] || !kernelOp(i.OpCode()) {
			// The ordinary code runs it and the rest of the iteration, but
			// not at every iteration.
			if !skipped(ip) {
				return nil, -1
			}
			k.exits[ip] = true
			live = false
			continue
		}
		switch i.OpCode() {
		case bytecode.OpMove:
			if !read(i.B()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpLoadConstant:
			if !number(i.Bx()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
			if !read(i.B()) || !read(i.C()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpMod:
			// By a constant: an integer by a kernelDivisor, never zero and of
			// fixed sign, or a float by a floatModDivisor. checkTypes tells.
			if !bytecode.IsConstant(i.C()) || !read(i.C()) {
				return nil, ip
			}
			d := p.Constants[bytecode.ConstantIndex(i.C())]
			if _, ok := floatModDivisor(d); !ok && !kernelDivisor(d) {
				return nil, ip
			}
			if !read(i.B()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpIDiv:
			// Of integers by such a constant, or of floats by anything: the
			// floor of the quotient. checkTypes tells which.
			if !read(i.B()) || !read(i.C()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpBitwise: // on integers; a shift by a constant
			op := bytecode.ArithOp(code[ip+1].Ax())
			shift := op == bytecode.ArithShl || op == bytecode.ArithShr
			if shift && (!bytecode.IsConstant(i.C()) || !p.Constants[bytecode.ConstantIndex(i.C())].isInteger()) {
				return nil, ip
			}
			if !read(i.B()) || op != bytecode.ArithBNot && !read(i.C()) || !write(i.A(), ip) {
				return nil, ip
			}
			ip++ // the operator's word
		case bytecode.OpUnaryMinus:
			if !read(i.B()) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			if !read(i.B()) || !read(i.C()) {
				return nil, ip
			}
			if t, ok := kernelJump(code, ip, latch); ok {
				targets[t] = true
			} else if !leavingJump(code, ip, start, latch) {
				return nil, ip
			}
			ip++ // the JMP
		case bytecode.OpJump:
			t, ok := kernelJump(code, ip, latch)
			if !ok {
				return nil, ip
			}
			targets[t] = true
		case bytecode.OpGetUpValue:
			// An intrinsic for the CALL it feeds, or a number or buffer the
			// kernel loads on entry.
			if call, args, ok := intrinsicCall(code, ip, start, latch); ok {
				fn, m, isIntrinsic := intrinsic(i.B())
				if isIntrinsic && args == 1 == (m == mathNone || m.unary()) {
					k.calls[call] = kernelCall{upValue: i.B(), fn: fn, get: ip, a: i.A(), args: args, math: m}
					k.virtual[ip] = true
					break
				}
			}
			kind, ok := upValue(i.B())
			if !ok || !write(i.A(), ip) {
				return nil, ip
			}
			s := k.hoist(i.B(), kind)
			if s < 0 {
				return nil, ip
			}
			k.upLoads[ip] = s
		case bytecode.OpCall:
			if kc, ok := k.calls[ip]; !ok || !read(i.A()+1) || kc.args == 2 && !read(i.A()+2) || !write(i.A(), ip) {
				return nil, ip
			}
		case bytecode.OpGetTable: // a buffer's element, at a key checked below
			if !read(i.C()) || !write(i.A(), ip) {
				return nil, ip
			}
			k.bufUses = append(k.bufUses, bufUse{ip, i.B(), true})
		case bytecode.OpSetTable:
			if !read(i.B()) || !read(i.C()) {
				return nil, ip
			}
			k.bufUses = append(k.bufUses, bufUse{ip, i.A(), false})
		case bytecode.OpGetTableUp, bytecode.OpSetTableUp: // an upvalue buffer's element
			up, key, get := i.B(), i.C(), i.OpCode() == bytecode.OpGetTableUp
			if !get {
				up, key = i.A(), i.B()
			}
			if kind, ok := upValue(up); !ok || kind != kindBuffer || !read(key) {
				return nil, ip
			}
			if get && !write(i.A(), ip) || !get && !read(i.C()) {
				return nil, ip
			}
			s := k.hoist(up, kindBuffer)
			if s < 0 {
				return nil, ip
			}
			k.bufFrom[ip] = s
			k.hoisted[s].read = k.hoisted[s].read || i.OpCode() == bytecode.OpGetTableUp
		}
	}
	if len(k.calls) > 0 || len(k.bufUses) > 0 || len(k.exits) > 0 {
		// A side exit leaves mid-iteration, where the enclosing function's
		// locals, below the loop's registers, that the body has yet to write
		// must hold the last iteration's values: an error from there may
		// close upvalues over them. So they are live in, and always held.
		// The body's own registers are dead until it writes them.
		for _, r := range k.writtenOnce() {
			if r < base && !slices.Contains(k.liveIn, r) {
				k.liveIn = append(k.liveIn, r)
			}
		}
	}
	for _, r := range k.liveIn {
		if _, ok := k.types[r]; !ok {
			k.types[r] = kindAny // decided by inferTypes
		}
	}
	entry := maps.Clone(k.types)
	if !k.inferTypes(p) {
		// Perhaps only because an integer constant meets floats, as in
		// `if v > 1 then v = 1 end` on a float v.
		k.types = entry
		if !k.promoteConstants(p, base) || !k.inferTypes(p) || !k.promotionsHold(p) {
			return nil, k.badPC
		}
	}
	// Each buffer access reads an upvalue's buffer, which its register
	// aliases there, or a register's.
	for _, u := range k.bufUses {
		if s, ok := bufferSlot(k.typeAt(u.ip, u.reg)); ok {
			k.bufFrom[u.ip] = s
			k.hoisted[s].read = k.hoisted[s].read || u.read
			continue
		}
		k.bufFrom[u.ip] = -1
		k.buffers[u.reg] = k.buffers[u.reg] || u.read
	}
	for _, r := range k.bufferRegs() {
		// A buffer register the loop never writes, and not a number, nor
		// one the function makes a table in: that register is surely a
		// table, and a kernel whose entry check fails costs the ordinary
		// loop the check each iteration.
		table := used[r] || wrote[r]
		for _, i := range code {
			table = table || i.OpCode() == bytecode.OpNewTable && i.A() == r
		}
		if table {
			for _, u := range k.bufUses {
				if u.reg == r {
					return nil, u.ip
				}
			}
		}
	}
	k.shareVar = !wrote[base+3]
	return k, -1
}

// promoteConstants marks the body's LOADKs of integer constants that
// convert to floats exactly, into a body register, whose value no use can
// tell from the float: until the register is written again, every use
// stores it into a buffer (which converts it either way), divides by it
// or into it, or adds, subtracts, multiplies or compares it with a float,
// which needFloat records for promotionsHold to check once typed. It
// reports whether it marked any.
func (k *kernelPlan) promoteConstants(p *prototype, base int) bool {
	code := p.Code
	k.promoted = map[int]bool{}
	for ip := k.start; ip < k.latch; ip++ {
		i := code[ip]
		if i.OpCode() != bytecode.OpLoadConstant || i.A() <= base+3 || slices.Contains(k.liveIn, i.A()) {
			continue
		}
		if v := p.Constants[i.Bx()]; !v.isInteger() || !exactFloat(v.i()) {
			continue
		}
		if needs, ok := k.floatUses(p, ip, i.A()); ok {
			k.promoted[ip] = true
			k.needFloat = append(k.needFloat, needs...)
		}
	}
	return len(k.promoted) > 0
}

// floatUses follows the body from after ip until register r is written
// again, and returns the fields each use needs to be floats for a float
// in r to act as the integer would, or false for a use that could tell.
func (k *kernelPlan) floatUses(p *prototype, ip, r int) ([][2]int, bool) {
	code := p.Code
	var needs [][2]int
	seen := map[int]bool{}
	work := []int{ip + 1}
	for len(work) > 0 {
		j := work[len(work)-1]
		work = work[:len(work)-1]
		if j >= k.latch || seen[j] {
			continue // at the latch r is dead: not live in
		}
		seen[j] = true
		i := code[j]
		if k.exits[j] { // the ordinary code could tell
			return nil, false
		}
		reads := func(f int) bool { return !bytecode.IsConstant(f) && f == r }
		switch op := i.OpCode(); op {
		case bytecode.OpSetTable, bytecode.OpSetTableUp:
			if reads(i.B()) || op == bytecode.OpSetTable && i.A() == r {
				return nil, false
			}
		case bytecode.OpDiv:
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul,
			bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			switch {
			case reads(i.B()) && reads(i.C()):
				return nil, false
			case reads(i.B()):
				needs = append(needs, [2]int{j, i.C()})
			case reads(i.C()):
				needs = append(needs, [2]int{j, i.B()})
			}
		case bytecode.OpMove, bytecode.OpUnaryMinus, bytecode.OpMod, bytecode.OpIDiv,
			bytecode.OpGetTable, bytecode.OpGetTableUp:
			if reads(i.B()) || op != bytecode.OpMove && op != bytecode.OpUnaryMinus && reads(i.C()) {
				return nil, false
			}
		case bytecode.OpCall: // any argument: min(x, 1) returns the integer
			if r > i.A() && (i.B() == 0 || r < i.A()+i.B()) {
				return nil, false
			}
		case bytecode.OpBitwise:
			if reads(i.B()) || reads(i.C()) {
				return nil, false
			}
		}
		// The next pcs, unless this one writes r.
		switch i.OpCode() {
		case bytecode.OpBitwise: // past the operator's word
			if i.A() != r {
				work = append(work, j+2)
			}
			continue
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			work = append(work, j+2)
			if t, ok := kernelJump(code, j, k.latch); ok { // or the loop's left, and r dead
				work = append(work, t)
			}
			continue
		case bytecode.OpJump:
			t, _ := kernelJump(code, j, k.latch)
			work = append(work, t)
			continue
		case bytecode.OpSetTable, bytecode.OpSetTableUp:
		default:
			if i.A() == r {
				continue
			}
		}
		work = append(work, j+1)
	}
	return needs, true
}

// promotionsHold reports whether the fields promoteConstants needs to be
// floats are.
func (k *kernelPlan) promotionsHold(p *prototype) bool {
	for _, n := range k.needFloat {
		if k.kind(p, n[0], n[1]) != kindFloat {
			return false
		}
	}
	return true
}

// hoist returns upvalue n's slot in jitContext.hoist, adding it, or -1
// when the slots are full or n holds something else elsewhere.
func (k *kernelPlan) hoist(n int, kind numKind) int {
	for s, h := range k.hoisted {
		if h.n == n {
			if h.kind != kind {
				return -1
			}
			return s
		}
	}
	if len(k.hoisted) == maxHoisted {
		return -1
	}
	k.hoisted = append(k.hoisted, hoistedUpValue{n: n, kind: kind})
	return len(k.hoisted) - 1
}

// upValueIntrinsic returns the Go function of the intrinsic cl's upvalue n
// holds, one of fns, if it holds one.
func upValueIntrinsic(cl *luaClosure, n int, fns []uint64) (uint64, bool) {
	if cl == nil || n >= len(cl.upValues) || cl.upValues[n] == nil {
		return 0, false
	}
	f := cl.upValues[n].value().goFunction()
	if f == nil || f.number == nil || f.number.unary == nil {
		return 0, false
	}
	fn := funcValue(f.number.unary)
	return fn, slices.Contains(fns, fn)
}

// upValueKind returns the kind of number cl's upvalue n holds, or
// kindBuffer for a buffer, when the function compiles.
func upValueKind(cl *luaClosure, n int) (numKind, bool) {
	if cl == nil || n >= len(cl.upValues) || cl.upValues[n] == nil {
		return kindAny, false
	}
	switch v := cl.upValues[n].value(); {
	case v.isFloat():
		return kindFloat, true
	case v.isInteger():
		return kindInt, true
	case v.userData() != nil && v.userData().buf != nil:
		return kindBuffer, true
	}
	return kindAny, false
}

// intrinsicCall returns the pc of the CALL A B C that the GETUPVAL A at ip
// feeds, and its argument count, one or two, when only arithmetic,
// upvalue loads, buffer reads and calls above A lie between them, and no
// jump in the body lands there. A buffer read or a call may leave the
// kernel; its side exit stores the function in A (calleesAt), as the
// GETUPVAL would have. An intrinsic has one result, so a call of one
// that ends another's arguments, C 0 then B 0, passes a fixed count.
func intrinsicCall(code []bytecode.Instruction, ip, start, latch int) (int, int, bool) {
	a := code[ip].A()
	for j := ip + 1; j < latch; j++ {
		i := code[j]
		for from := start; from < latch; from++ {
			if t, ok := kernelJump(code, from, latch); ok && t == j {
				return 0, 0, false
			}
		}
		clobbers := false
		switch i.OpCode() {
		case bytecode.OpCall:
			if i.A() > a {
				clobbers = i.C() == 0 && (j+1 >= latch || code[j+1].OpCode() != bytecode.OpCall || code[j+1].B() != 0)
				break
			}
			args := i.B() - 1
			if i.B() == 0 { // up to the call before's result
				args = code[j-1].A() - a
			}
			ok := i.A() == a && (args == 1 || args == 2) && (i.C() == 2 || i.C() == 0 && j+1 < latch &&
				code[j+1].OpCode() == bytecode.OpCall && code[j+1].B() == 0)
			return j, args, ok
		case bytecode.OpMove, bytecode.OpUnaryMinus:
			clobbers = i.A() == a || i.B() == a
		case bytecode.OpLoadConstant, bytecode.OpGetUpValue:
			clobbers = i.A() == a
		case bytecode.OpGetTable, bytecode.OpGetTableUp:
			clobbers = i.A() == a || i.C() == a || i.OpCode() == bytecode.OpGetTable && i.B() == a
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod, bytecode.OpIDiv:
			clobbers = i.A() == a || i.B() == a || i.C() == a
		default:
			clobbers = true
		}
		if clobbers {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// guardedUpValues returns the upvalues k's intrinsic calls come from, each
// once, in order.
func (k *kernelPlan) guardedUpValues() []int {
	var ns []int
	for _, kc := range k.calls {
		if !slices.Contains(ns, kc.upValue) {
			ns = append(ns, kc.upValue)
		}
	}
	slices.Sort(ns)
	return ns
}

// upValueFn returns the intrinsic upvalue n must hold: a number
// function's code, or a math function's, when math is set.
func (k *kernelPlan) upValueFn(n int) (fn uint64, math bool) {
	for _, kc := range k.calls {
		if kc.upValue == n {
			return kc.fn, kc.math != mathNone
		}
	}
	return 0, false
}

// callResult returns the type the intrinsic call kc at i gives A with
// the registers of types state: a number function's float; floor and
// ceil's integer, which the kernel leaves for Go to make when the float
// has none; abs's argument's type; and min and max's arguments' type, the
// same for both. kindAny while an argument's is unknown; false for mixed
// ones.
func callResult(kc kernelCall, i bytecode.Instruction, state map[int]numKind) (numKind, bool) {
	arg := state[i.A()+1]
	switch kc.math {
	case mathNone:
		return kindFloat, true
	case mathFloor, mathCeil:
		if arg == kindAny {
			return kindAny, true
		}
		return kindInt, true
	case mathAbs:
		return arg, true
	}
	arg2 := state[i.A()+2]
	switch {
	case arg == kindAny || arg2 == kindAny:
		return kindAny, true
	case arg != arg2:
		return kindAny, false
	}
	return arg, true
}

// bufferRegs returns the registers k indexes as buffers, in order.
func (k *kernelPlan) bufferRegs() []int {
	var rs []int
	for r := range k.buffers {
		rs = append(rs, r)
	}
	slices.Sort(rs)
	return rs
}

// kind returns the type of RK field before the body pc ip.
func (k *kernelPlan) kind(p *prototype, ip, field int) numKind {
	if bytecode.IsConstant(field) {
		return constKind(p.Constants[bytecode.ConstantIndex(field)])
	}
	return k.at[ip-k.start][field]
}

// result returns the type instruction i gives its register A when the
// registers have the types in state, kindAny while an operand's type is
// unknown, and false if it cannot run in a kernel with these types.
func result(p *prototype, i bytecode.Instruction, state map[int]numKind) (numKind, bool) {
	kind := func(field int) numKind {
		if bytecode.IsConstant(field) {
			return constKind(p.Constants[bytecode.ConstantIndex(field)])
		}
		return state[field]
	}
	switch i.OpCode() {
	case bytecode.OpMove, bytecode.OpUnaryMinus:
		return state[i.B()], true
	case bytecode.OpLoadConstant:
		return constKind(p.Constants[i.Bx()]), true
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul:
		b, c := kind(i.B()), kind(i.C())
		switch {
		case b == kindFloat || c == kindFloat:
			return kindFloat, true
		case b == kindInt && c == kindInt:
			return kindInt, true
		}
		return kindAny, true
	case bytecode.OpDiv, bytecode.OpCall, bytecode.OpGetTable, bytecode.OpGetTableUp: // intrinsics and buffers of floats
		return kindFloat, true
	case bytecode.OpMod, bytecode.OpIDiv: // see checkTypes for which divisors
		b, c := kind(i.B()), kind(i.C())
		switch {
		case b == kindFloat || c == kindFloat:
			return kindFloat, true
		case b == kindInt && c == kindInt:
			return kindInt, true
		}
	}
	return kindAny, true
}

// constantShift folds a shift by constant n, right when right is set, as
// ShiftLeft does: left by count, right by -count when it is negative, and
// zero for 64 or more either way.
func constantShift(n int64, right bool) (count int, zero bool) {
	if right {
		if n == math.MinInt64 {
			return 0, true
		}
		n = -n
	}
	if n >= 64 || n <= -64 {
		return 0, true
	}
	return int(n), false
}

// bitwiseResult returns the type OpBitwise i, of operator op, gives A:
// an integer of integers, kindAny while an operand's type is unknown,
// and false for a float operand, which must convert exactly first.
func bitwiseResult(p *prototype, i bytecode.Instruction, op bytecode.ArithOp, state map[int]numKind) (numKind, bool) {
	fields := []int{i.B(), i.C()}
	if op == bytecode.ArithBNot {
		fields = fields[:1]
	}
	t := kindInt
	for _, f := range fields {
		k := state[f]
		if bytecode.IsConstant(f) {
			k = constKind(p.Constants[bytecode.ConstantIndex(f)])
		}
		switch k {
		case kindAny:
			t = kindAny
		case kindInt:
		default:
			return kindAny, false
		}
	}
	return t, true
}

// inferTypes types each register at each pc of the body, choosing the
// live-in registers' types on entry: a register the body writes carries the
// type it ends an iteration with into the next, and a register nothing
// decides takes the type guess gives it. It reports false when two paths
// reach a pc with a register of different types, or an instruction cannot
// run on them.
func (k *kernelPlan) inferTypes(p *prototype) bool {
	for {
		at, results, ok := k.flow(p)
		if !ok {
			return false
		}
		end := at[k.latch-k.start]
		changed := false
		for _, r := range k.liveIn {
			if t, ok := end[r]; ok && t != kindAny && k.types[r] == kindAny {
				k.types[r], changed = t, true
			}
		}
		if changed {
			continue
		}
		undecided := -1
		for _, r := range k.liveIn {
			if k.types[r] == kindAny && (undecided < 0 || r < undecided) {
				undecided = r
			}
		}
		if undecided >= 0 {
			k.types[undecided] = k.guess(p, at, undecided)
			continue
		}
		k.at, k.results = at, results
		return k.checkTypes(p)
	}
}

// guess returns the likelier type of live-in register r, which nothing in
// the body decides: a float if arithmetic and comparisons meet it with
// floats more often than with integers, as a time or scale parameter is,
// and otherwise an integer, as a counter or an enclosing loop's index is.
// A wrong guess costs only the kernel: its entry check fails and the
// ordinary code runs.
func (k *kernelPlan) guess(p *prototype, at []map[int]numKind, r int) numKind {
	floats, ints := 0, 0
	for ip := k.start; ip < k.latch; ip++ {
		i := p.Code[ip]
		if k.exits[ip] {
			continue
		}
		switch i.OpCode() {
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv,
			bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
		default:
			continue
		}
		var other int
		switch r {
		case i.B():
			other = i.C()
		case i.C():
			other = i.B()
		default:
			continue
		}
		t := at[ip-k.start][other]
		if bytecode.IsConstant(other) {
			t = constKind(p.Constants[bytecode.ConstantIndex(other)])
		}
		switch t {
		case kindFloat:
			floats++
		case kindInt:
			ints++
		}
	}
	if floats > ints {
		return kindFloat
	}
	return kindInt
}

// flow computes the registers' types before each body pc, and at the
// latch, from their types on entry, and the type each instruction writes.
// Jumps in a body only go forward, so one pass finds them. It reports
// false where two paths reach a pc with a register of different types.
func (k *kernelPlan) flow(p *prototype) ([]map[int]numKind, map[int]numKind, bool) {
	code := p.Code
	at := make([]map[int]numKind, k.latch-k.start+1)
	incoming := make([][]map[int]numKind, len(at))
	results := map[int]numKind{}
	cur := maps.Clone(k.types) // nil where no path falls through
	for ip := k.start; ip <= k.latch; ip++ {
		states := incoming[ip-k.start]
		if cur != nil {
			states = append(states, cur)
		}
		merged, ok := mergeTypes(states)
		if !ok {
			return nil, nil, false
		}
		at[ip-k.start] = merged
		if ip == k.latch {
			break
		}
		cur = maps.Clone(merged)
		i := code[ip]
		if k.exits[ip] { // the path leaves here
			cur = nil
			continue
		}
		switch i.OpCode() {
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			if t, ok := kernelJump(code, ip, k.latch); ok { // or it leaves
				incoming[t-k.start] = append(incoming[t-k.start], cur)
			}
			at[ip+1-k.start] = cur // the JMP, which the test consumes
			ip++
		case bytecode.OpJump:
			t, _ := kernelJump(code, ip, k.latch)
			incoming[t-k.start] = append(incoming[t-k.start], cur)
			cur = nil
		case bytecode.OpBitwise:
			t, ok := bitwiseResult(p, i, bytecode.ArithOp(code[ip+1].Ax()), cur)
			if !ok {
				return nil, nil, false
			}
			results[ip] = t
			cur[i.A()] = t
			at[ip+1-k.start] = maps.Clone(cur) // the operator's word
			ip++
		case bytecode.OpCall:
			t, ok := callResult(k.calls[ip], i, cur)
			if !ok {
				return nil, nil, false
			}
			results[ip] = t
			cur[i.A()] = t
		case bytecode.OpSetTable, bytecode.OpSetTableUp: // nothing written
		case bytecode.OpGetUpValue: // an intrinsic's writes nothing
			if s, ok := k.upLoads[ip]; ok {
				t := k.hoisted[s].kindOf(s)
				results[ip] = t
				cur[i.A()] = t
			}
		default:
			t, ok := result(p, i, cur)
			if k.promoted[ip] {
				t = kindFloat
			}
			if !ok {
				return nil, nil, false
			}
			results[ip] = t
			cur[i.A()] = t
		}
	}
	return at, results, true
}

// mergeTypes merges the registers' types where paths meet: a register
// defined on only some of them is not defined, and one of two types is a
// conflict. An undecided type takes the other path's: it comes only from a
// live-in register inferTypes has yet to type, and so is optimistic, as
// the last pass, with every type decided, checks the merge exactly.
func mergeTypes(states []map[int]numKind) (map[int]numKind, bool) {
	if len(states) == 0 {
		return map[int]numKind{}, true // unreachable
	}
	out := maps.Clone(states[0])
	for _, s := range states[1:] {
		for r, t := range out {
			switch u, ok := s[r]; {
			case !ok:
				delete(out, r)
			case t == u || u == kindAny:
			case t == kindAny:
				out[r] = u
			default:
				return nil, false
			}
		}
	}
	return out, true
}

// checkTypes reports whether every instruction of the body can run on its
// operands' types: known, integer keys, comparisons of two of a type or a
// float and an integer constant that converts exactly, and a register the
// body writes that ends each iteration with the type it starts with.
func (k *kernelPlan) checkTypes(p *prototype) bool {
	code := p.Code
	k.floatKeys, k.badPC = false, -1
	end := k.at[k.latch-k.start]
	for _, r := range k.writtenOnce() {
		if t, ok := k.types[r]; ok && end[r] != t {
			return false
		}
	}
	for _, r := range k.liveIn {
		if !isNumKind(k.types[r]) { // not an alias, checked on entry as a number
			return false
		}
	}
	for ip := k.start; ip < k.latch; ip++ {
		if k.exits[ip] {
			continue
		}
		i := code[ip]
		known := func(fields ...int) bool { // numbers, not aliases
			for _, f := range fields {
				if !isNumKind(k.kind(p, ip, f)) {
					return k.fail(ip)
				}
			}
			return true
		}
		switch i.OpCode() {
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			b, c := k.kind(p, ip, i.B()), k.kind(p, ip, i.C())
			if !known(i.B(), i.C()) || b != c && !k.exactConstant(p, i.B()) && !k.exactConstant(p, i.C()) {
				return k.fail(ip)
			}
			ip++
			continue
		case bytecode.OpJump, bytecode.OpGetUpValue:
			continue
		case bytecode.OpSetTable, bytecode.OpSetTableUp:
			if !k.bufferKey(p, ip, i.B()) || !known(i.C()) {
				return k.fail(ip)
			}
			continue
		case bytecode.OpGetTable, bytecode.OpGetTableUp:
			if !k.bufferKey(p, ip, i.C()) {
				return k.fail(ip)
			}
		case bytecode.OpCall:
			if !known(i.A()+1) || k.calls[ip].args == 2 && !known(i.A()+2) {
				return k.fail(ip)
			}
			m := k.calls[ip].math
			if (m == mathFloor || m == mathCeil) && k.typeAt(ip, i.A()+1) == kindFloat && !k.floor {
				return k.fail(ip)
			}
		case bytecode.OpMove: // of a number or an alias
			if k.kind(p, ip, i.B()) == kindAny {
				return k.fail(ip)
			}
		case bytecode.OpUnaryMinus:
			if !known(i.B()) {
				return k.fail(ip)
			}
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
			if !known(i.B(), i.C()) {
				return k.fail(ip)
			}
		case bytecode.OpMod, bytecode.OpIDiv:
			// Integers by a kernelDivisor; floats by anything for //, and by
			// a floatModDivisor for %, rounding in one instruction.
			if !known(i.B(), i.C()) {
				return k.fail(ip)
			}
			var d value
			if bytecode.IsConstant(i.C()) {
				d = p.Constants[bytecode.ConstantIndex(i.C())]
			}
			switch k.results[ip] {
			case kindInt:
				if !kernelDivisor(d) {
					return k.fail(ip)
				}
			case kindFloat:
				_, modOK := floatModDivisor(d)
				if !k.floor || i.OpCode() == bytecode.OpMod && !modOK {
					return k.fail(ip)
				}
			}
		case bytecode.OpBitwise: // bitwiseResult checked the operands
			if k.results[ip] != kindInt {
				return k.fail(ip)
			}
			ip++
			continue
		}
		if k.results[ip] == kindAny {
			return k.fail(ip)
		}
	}
	return true
}

// fail records ip as the pc checkTypes stopped at, and reports false.
func (k *kernelPlan) fail(ip int) bool {
	k.badPC = ip
	return false
}

// bufferKey reports whether field can key a buffer access at ip: an
// integer, or a register holding a float, which the kernel converts,
// leaving it when the float has no integer value. A float key needs the
// scratch integer register planKernel then reserves.
func (k *kernelPlan) bufferKey(p *prototype, ip, field int) bool {
	switch k.kind(p, ip, field) {
	case kindInt:
		return true
	case kindFloat:
		k.floatKeys = k.floatKeys || !bytecode.IsConstant(field)
		return !bytecode.IsConstant(field)
	}
	return false
}

// keyScratch is the Lua register of the integer virtual register a float
// key converts into.
const keyScratch = -1

// typeAt returns register r's type before the body pc ip.
func (k *kernelPlan) typeAt(ip, r int) numKind { return k.at[ip-k.start][r] }

// exactConstant reports whether RK field is an integer constant that
// converts to a float exactly.
func (k *kernelPlan) exactConstant(p *prototype, field int) bool {
	if !bytecode.IsConstant(field) {
		return false
	}
	v := p.Constants[bytecode.ConstantIndex(field)]
	return v.isInteger() && exactFloat(v.i())
}

// kernelJump returns where the jump at ip, or the JMP after the test at
// ip, goes, and false unless it is forward, closes no upvalues, and stays
// in the loop body or goes to its FORLOOP at latch.
func kernelJump(code []bytecode.Instruction, ip, latch int) (int, bool) {
	i := code[ip]
	switch i.OpCode() {
	case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
		ip++
		i = code[ip]
	case bytecode.OpJump:
	default:
		return 0, false
	}
	if i.OpCode() != bytecode.OpJump || i.A() != 0 {
		return 0, false
	}
	t := ip + 1 + i.SBx()
	return t, t > ip && t <= latch
}

// kernelOp reports whether a kernel may run op; it leaves at any other.
func kernelOp(op bytecode.OpCode) bool {
	switch op {
	case bytecode.OpMove, bytecode.OpLoadConstant, bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul,
		bytecode.OpDiv, bytecode.OpMod, bytecode.OpIDiv, bytecode.OpBitwise, bytecode.OpUnaryMinus,
		bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual, bytecode.OpJump,
		bytecode.OpGetUpValue, bytecode.OpCall, bytecode.OpGetTable, bytecode.OpSetTable,
		bytecode.OpGetTableUp, bytecode.OpSetTableUp:
		return true
	}
	return false
}

// leavingJump reports whether the JMP after the test at ip leaves the
// loop body, from start to latch, closing no upvalues: as a break does.
// The kernel leaves at the test when it would jump, for the ordinary code
// to test again and jump.
func leavingJump(code []bytecode.Instruction, ip, start, latch int) bool {
	j := code[ip+1]
	if j.OpCode() != bytecode.OpJump || j.A() != 0 {
		return false
	}
	t := ip + 2 + j.SBx()
	return t > latch || t < start
}

// writtenOnce returns k's written registers without repeats.
func (k *kernelPlan) writtenOnce() []int {
	var out []int
	done := map[int]bool{}
	for _, r := range k.written {
		if !done[r] {
			done[r] = true
			out = append(out, r)
		}
	}
	return out
}

// A numKind is what compiling knows of an operand's number type: a
// constant's, or nothing, for a register.
type numKind uint8

const (
	kindAny numKind = iota
	kindFloat
	kindInt
	kindBuffer // an upvalue holding a buffer, in hoistedUpValue

	// kindBufferAlias + s is a kernel register holding hoisted slot s's
	// buffer, which the kernel keeps in the context, not the register.
	kindBufferAlias numKind = 16
)

// isNumKind reports whether t is a number's type.
func isNumKind(t numKind) bool { return t == kindFloat || t == kindInt }

// bufferSlot returns the hoisted slot a register of type t aliases, if it
// is an alias.
func bufferSlot(t numKind) (int, bool) {
	return int(t - kindBufferAlias), t >= kindBufferAlias
}

// constKind is the number type of constant v, or kindAny if it is not a
// number.
func constKind(v value) numKind {
	switch {
	case v.isFloat():
		return kindFloat
	case v.isInteger():
		return kindInt
	}
	return kindAny
}

// exactFloat reports whether the integer i converts to a float exactly,
// so that comparing the float compares i.
func exactFloat(i int64) bool { return -(1<<53) <= i && i <= 1<<53 }
