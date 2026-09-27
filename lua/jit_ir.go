//go:build (darwin || linux) && (arm64 || amd64)

package lua

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/matjam/apogee/internal/bytecode"
)

// The IR kernels compile through.
//
// planKernel (jit_kernel.go) decides that a loop qualifies and types its
// registers at each pc; buildIR turns the body into a list of typed
// operations on virtual registers, one for each type each Lua register
// takes (a vreg); allocate gives the virtual registers machine registers
// by liveness; and each architecture lowers the operations
// (jit_*_kernel.go). An operation that may leave the kernel names a
// snapshot: the Lua registers to write back, and from which virtual
// registers, before the ordinary code resumes at its pc.

// A vreg is a virtual register: an index into irFunc.vregs.
type vreg int32

// noVreg is an irArg's vreg when the argument is a constant, and an
// irInst's dst when it writes nothing.
const noVreg vreg = -1

type irOp uint8

const (
	irLabel     irOp = iota // the body pc target starts here
	irMove                  // dst = a
	irConst                 // dst = constant a, converted to dst's type
	irHoist                 // dst = jitContext.hoist[imm]
	irAdd                   // dst = a + b, of dst's type
	irSub                   // dst = a - b
	irMul                   // dst = a * b
	irDiv                   // dst = a / b, of floats
	irFloorDiv              // dst = floor(a / b), of floats
	irFloatMod              // dst = a % d, of floats, by floatModDivisor d, imm's bits
	irIntDiv                // dst = a // b, of integers, by the constant b, imm
	irIntMod                // dst = a % b, likewise
	irAnd                   // dst = a & b, of integers
	irOr                    // dst = a | b
	irXor                   // dst = a ~ b
	irNot                   // dst = ~a
	irShift                 // dst = a << imm, >> -imm when negative; 0 when flag
	irNeg                   // dst = -a, of dst's type
	irBranch                // to target when a cmp b is flag, or for TEST, when a is flag
	irJump                  // to target
	irIntrinsic             // dst = the number function imm (a)
	irMath                  // dst = math function imm (a, b), of a's type
	irBufGet                // dst = buffer[a]
	irBufSet                // buffer[a] = b
	irExit                  // leave, for the ordinary code to run the instruction at pc; a call's, flag, resumes after
	irArrayGet              // dst = a[b], guarded to have dst's type, or with no dst register obj on the stack
	irArraySet              // a[b] = c, over a value or with no metatable
	irFieldGet              // dst = a's own field that the fieldCache at pc names, guarded; of hoisted slot buf's when not -1
	irFieldSet              // that field = c, over a value
	irCopyUp                // register obj = upvalue imm, on the stack
	irCopy                  // register obj = register imm, on the stack
	irBoolNot               // dst = not a, of a boolean
	irForPrep               // an integer loop inside starts, on regs; to target when it runs no times
	irForLoop               // and goes on: back to target, or on
	irLen                   // dst = #a, of a table
)

var irOpNames = [...]string{"label", "move", "const", "hoist", "add", "sub", "mul", "div", "floordiv",
	"fmod", "idiv", "imod", "and", "or", "xor", "not", "shift", "neg", "branch", "jump", "intrinsic",
	"math", "bufget", "bufset", "exit", "aget", "aset", "fget", "fset", "copyup", "copy", "not", "forprep", "forloop", "len"}

func (o irOp) String() string { return irOpNames[o] }

// irArg is an operation's argument: a virtual register, or constant k, or
// when k is litK, the number lit, a constant of a function inlined.
type irArg struct {
	v   vreg
	k   int
	t   numKind // its type
	lit value
}

// noArg is an operation's unused argument.
var noArg = irArg{v: noVreg, k: -1}

// litK is an irArg's k when it holds its constant in lit.
const litK = -2

// isConst reports whether the argument is a constant.
func (a irArg) isConst() bool { return a.v == noVreg && (a.k >= 0 || a.k == litK) }

// constant returns the constant a holds, from p's constants or lit.
func (a irArg) constant(p *prototype) value {
	if a.k == litK {
		return a.lit
	}
	return p.Constants[a.k]
}

// irInst is one operation, from the instruction at pc.
type irInst struct {
	op     irOp
	pc     int
	dst    vreg
	a, b   irArg
	c      irArg   // a store's value
	regs   [4]vreg // a loop inside's index, count, step and variable
	imm    int64
	flag   bool
	cmp    bytecode.OpCode // a branch's comparison: EQ, LT or LE
	target int             // a label's or a jump's pc; a branch's is -1 when it leaves by snap
	buf    int             // a buffer access's hoisted slot, or -1 for register obj's
	obj    int
	tmp    vreg // an integer scratch register: a float buffer key's
	snap   int  // the snapshot it leaves by, or -1
}

// irSnap is what leaving the kernel at pc writes back: numbers from
// virtual registers, then the upvalue each buffer alias holds, then the
// function each pending intrinsic call's callee register holds, and top,
// for a call whose arguments run to it, unless -1.
type irSnap struct {
	pc      int
	regs    []irSnapReg
	callees []kernelCall
	top     int
}

// irSnapReg is a Lua register a snapshot writes: from v, of type t, or,
// for an alias (v is noVreg), hoisted slot alias's upvalue.
type irSnapReg struct {
	r     int
	v     vreg
	t     numKind
	alias int
}

// irFunc is a kernel's IR.
type irFunc struct {
	*kernelPlan
	insts   []irInst
	vregs   []kslot // each one's Lua register and type
	index   map[kslot]vreg
	snaps   []irSnap
	snapAt  map[int]int           // snapshot indices by pc
	loop    [4]vreg               // the FORLOOP's index, limit (or count), step and variable
	end     int                   // the latch's snapshot, for leaving at the end of an iteration
	leaves  bool                  // whether an instruction or a branch leaves: see irExit
	resumes []int                 // the pcs of calls the kernel resumes after
	liveAt  map[int]map[vreg]bool // the virtual registers live at each body pc's label, but the pinned
	share   bool                  // the variable shares the index's vreg
	temps   int                   // inlined functions' temporaries: see temp
	loc     []int                 // each vreg's machine register, in its class, or -1: see allocate
	reload  [2]int                // the first of the float and integer registers holding spilled values, or -1
	cur     map[vreg]int          // spilled vregs the operation being lowered uses, and their reload registers
	fixed   map[vreg]bool         // loops inside's registers, which never spill
	shares  []irShare             // loops inside whose variable shares the index's register
}

// A kernel that can leave (irFunc.leaves) counts its short runs: those
// that leave within kernelShortRun iterations of starting, one after
// another, in a word of its prototype's jitRuns. Its entry check fails once
// there have been kernelRunsOff: the kernel leaves at most iterations, and
// costs more than the ordinary code.
const (
	kernelShortRun = 4
	kernelRunsOff  = 64
)

// kslot is a Lua register holding a type, which has a virtual register.
type kslot struct {
	r int
	t numKind
}

// value returns the virtual register for Lua register r holding type t.
func (f *irFunc) value(r int, t numKind) vreg {
	base := f.base
	if f.share && r == base+3 && t == f.types[base] {
		r = base
	}
	s := kslot{r, t}
	if v, ok := f.index[s]; ok {
		return v
	}
	v := vreg(len(f.vregs))
	f.vregs = append(f.vregs, s)
	f.index[s] = v
	return v
}

// typeOf returns v's type.
func (f *irFunc) typeOf(v vreg) numKind { return f.vregs[v].t }

// pinned reports whether v keeps its machine register throughout: the
// loop's registers, those live into the body and those below the loop,
// which outlive it, and the scratch key.
func (f *irFunc) pinned(v vreg) bool {
	r := f.vregs[v].r
	return r > inlineReg && (r < f.pinBelow() || slices.Contains(f.liveIn, r))
}

// arg returns RK field before ip as an argument.
func (f *irFunc) arg(p *prototype, ip, field int) irArg {
	if bytecode.IsConstant(field) {
		kk := bytecode.ConstantIndex(field)
		return irArg{v: noVreg, k: kk, t: constKind(p.Constants[kk])}
	}
	t := f.typeAt(ip, field)
	return irArg{v: f.valueAt(ip, field, t), t: t}
}

// irShare is a loop inside whose variable, register r, its body does not
// write, so that it shares the index's, a's, virtual register from the
// FORPREP at from to the FORLOOP at to.
type irShare struct{ r, a, from, to int }

// valueAt is value for register r before ip, where r may be a loop
// inside's variable, which shares its index's virtual register.
func (f *irFunc) valueAt(ip, r int, t numKind) vreg {
	for _, s := range f.shares {
		if t == kindInt && r == s.r && s.from < ip && ip <= s.to {
			return f.value(s.a, t)
		}
	}
	return f.value(r, t)
}

// snapshot returns the index of the snapshot for leaving at ip, where
// the ordinary code runs the instruction at ip and the rest of the
// iteration. It writes back every register the loop writes that is
// defined at ip: registers the rest of the iteration has yet to write
// may hold stale numbers until it does.
func (f *irFunc) snapshot(p *prototype, ip int) int {
	if s, ok := f.snapAt[ip]; ok {
		return s
	}
	types := f.at[ip-f.start]
	s := irSnap{pc: ip, top: -1}
	for _, r := range f.writtenOnce() {
		switch t := types[r]; {
		case isRegKind(t):
			s.regs = append(s.regs, irSnapReg{r: r, v: f.valueAt(ip, r, t), t: t, alias: -1})
		default:
			if slot, ok := bufferSlot(t); ok {
				s.regs = append(s.regs, irSnapReg{r: r, v: noVreg, t: t, alias: slot})
			}
		}
	}
	s.callees = f.calleesAt(ip)
	slices.SortFunc(s.callees, func(x, y kernelCall) int { return x.get - y.get })
	if kc, ok := f.calls[ip]; ok && p.Code[ip].B() == 0 { // its arguments run to l.top
		s.top = kc.a + 1 + kc.args
	}
	// An instruction the ordinary code runs whose values run to l.top,
	// after a call of all results the kernel computed, which has one.
	switch i := p.Code[ip]; i.OpCode() {
	case bytecode.OpCall, bytecode.OpTailCall, bytecode.OpReturn:
		if _, ok := f.calls[ip-1]; ok && i.B() == 0 && ip > f.start && p.Code[ip-1].C() == 0 {
			s.top = p.Code[ip-1].A() + 1
		}
	}
	f.snaps = append(f.snaps, s)
	f.snapAt[ip] = len(f.snaps) - 1
	return len(f.snaps) - 1
}

// buildIR returns k's IR.
func buildIR(p *prototype, k *kernelPlan) *irFunc {
	code := p.Code
	f := &irFunc{kernelPlan: k, index: map[kslot]vreg{}, snapAt: map[int]int{}, share: k.shareVar, cur: map[vreg]int{},
		liveAt: map[int]map[vreg]bool{}, fixed: map[vreg]bool{}}
	base := k.base
	// Virtual registers in the order allocate colours them: the loop's,
	// the live-in ones, then as the body writes them.
	f.loop = [4]vreg{noVreg, noVreg, noVreg, noVreg}
	for r := base; r <= base+3 && !k.while; r++ {
		f.loop[r-base] = f.value(r, k.types[base])
	}
	for _, r := range k.liveIn {
		if isRegKind(k.types[r]) {
			f.value(r, k.types[r])
		}
	}
	for ip := k.start; ip < k.latch; ip++ {
		if t := k.results[ip]; isRegKind(t) {
			f.value(code[ip].A(), t)
		}
	}
	// A loop inside whose body does not write its variable, which Lua 5.5
	// forbids, keeps it in the index's register, as the loop's own does.
	for ip := k.start; ip < k.latch; ip++ {
		if i := code[ip]; i.OpCode() == bytecode.OpForPrep && !k.exits[ip] {
			a, end := i.A(), ip+1+i.SBx()
			written := false
			for j := ip + 1; j < end; j++ {
				if _, ok := k.results[j]; ok && code[j].A() == a+3 {
					written = true
				}
			}
			if !written {
				f.shares = append(f.shares, irShare{r: a + 3, a: a, from: ip, to: end})
			}
		}
	}
	scratch := noVreg
	if k.floatKeys {
		scratch = f.value(keyScratch, kindInt)
	}
	emit := func(in irInst) {
		switch in.op {
		case irBranch, irJump, irLabel, irForPrep, irForLoop:
		default:
			in.target = -1
		}
		f.insts = append(f.insts, in)
	}
	for ip := k.start; ip < k.latch; ip++ {
		i := code[ip]
		if k.unreached[ip] {
			continue
		}
		emit(irInst{op: irLabel, pc: ip, target: ip, dst: noVreg, a: noArg, b: noArg, c: noArg, snap: -1, buf: -1, tmp: noVreg})
		if k.exits[ip] { // a break ends the loop, which is no short run
			f.leaves = true
			emit(irInst{op: irExit, pc: ip, dst: noVreg, a: noArg, b: noArg, c: noArg, snap: f.snapshot(p, ip), buf: -1, tmp: noVreg, flag: k.breaks[ip]})
			continue
		}
		in := irInst{pc: ip, dst: noVreg, a: noArg, b: noArg, c: noArg, snap: -1, buf: -1, tmp: noVreg}
		res := k.results[ip]
		dst := func() vreg { return f.value(i.A(), res) }
		switch op := i.OpCode(); op {
		case bytecode.OpMove:
			if res == kindBoxed {
				in.op, in.obj, in.imm = irCopy, i.A(), int64(i.B())
				break
			}
			if !isRegKind(res) {
				continue // an alias of a hoisted buffer: nothing to move
			}
			in.op, in.dst, in.a = irMove, dst(), f.arg(p, ip, i.B())
		case bytecode.OpLoadConstant:
			kk := i.Bx()
			in.op, in.dst, in.a = irConst, dst(), irArg{v: noVreg, k: kk, t: constKind(p.Constants[kk])}
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
			in.op = map[bytecode.OpCode]irOp{bytecode.OpAdd: irAdd, bytecode.OpSub: irSub,
				bytecode.OpMul: irMul, bytecode.OpDiv: irDiv}[op]
			in.dst, in.a, in.b = dst(), f.arg(p, ip, i.B()), f.arg(p, ip, i.C())
		case bytecode.OpBitwise:
			ao := bytecode.ArithOp(code[ip+1].Ax())
			in.dst, in.a = dst(), f.arg(p, ip, i.B())
			switch ao {
			case bytecode.ArithBAnd, bytecode.ArithBOr, bytecode.ArithBXor:
				in.op = map[bytecode.ArithOp]irOp{bytecode.ArithBAnd: irAnd, bytecode.ArithBOr: irOr, bytecode.ArithBXor: irXor}[ao]
				in.b = f.arg(p, ip, i.C())
			case bytecode.ArithBNot:
				in.op = irNot
			case bytecode.ArithShl, bytecode.ArithShr:
				n := p.Constants[bytecode.ConstantIndex(i.C())].i()
				count, zero := constantShift(n, ao == bytecode.ArithShr)
				in.op, in.imm, in.flag = irShift, int64(count), zero
			}
			emit(in)
			ip++ // the operator's word
			continue
		case bytecode.OpIDiv, bytecode.OpMod:
			in.dst, in.a, in.b = dst(), f.arg(p, ip, i.B()), f.arg(p, ip, i.C())
			switch {
			case res == kindFloat && op == bytecode.OpIDiv:
				in.op = irFloorDiv
			case res == kindFloat:
				d, _ := floatModDivisor(p.Constants[bytecode.ConstantIndex(i.C())])
				in.op, in.imm = irFloatMod, int64(math.Float64bits(d))
			default:
				in.op, in.imm = irIntDiv, p.Constants[bytecode.ConstantIndex(i.C())].i()
				if op == bytecode.OpMod {
					in.op = irIntMod
				}
			}
		case bytecode.OpUnaryMinus:
			in.op, in.dst, in.a = irNeg, dst(), f.arg(p, ip, i.B())
		case bytecode.OpLoadBool:
			in.op, in.dst, in.a = irConst, dst(), irArg{v: noVreg, k: litK, t: kindInt, lit: integerValue(int64(min(i.B(), 1)))}
		case bytecode.OpNot:
			if in.a = f.arg(p, ip, i.B()); in.a.t == kindBool {
				in.op, in.dst = irBoolNot, dst()
				break
			}
			// A number or a table: true, so not it is false.
			in.op, in.dst, in.a = irConst, dst(), irArg{v: noVreg, k: litK, t: kindInt, lit: integerValue(0)}
		case bytecode.OpTest:
			t, ok := k.jump(code, ip)
			in.op, in.cmp, in.flag, in.target, in.a = irBranch, bytecode.OpTest, i.C() != 0, t, f.arg(p, ip, i.A())
			if !ok { // it leaves the loop: the ordinary code tests again
				in.target, in.snap, f.leaves = -1, f.snapshot(p, ip), true
			} else if t <= ip { // back: it spends budget
				in.snap = f.snapshot(p, t)
			}
			if in.a.t != kindBool { // a number or a table, which is true
				switch {
				case !in.flag: // never jumps
					ip++
					continue
				case in.target >= 0:
					in.op = irJump
				default:
					in.op, in.flag = irExit, true
				}
			}
			emit(in)
			ip++ // the JMP
			continue
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			t, ok := k.jump(code, ip)
			in.op, in.cmp, in.flag, in.target = irBranch, op, i.A() != 0, t
			in.a, in.b = f.arg(p, ip, i.B()), f.arg(p, ip, i.C())
			if !ok { // it leaves the loop: the ordinary code tests again
				in.target, in.snap, f.leaves = -1, f.snapshot(p, ip), true
			} else if t <= ip { // back: it spends budget
				in.snap = f.snapshot(p, t)
			}
			emit(in)
			ip++ // the JMP
			continue
		case bytecode.OpJump:
			t, _ := k.jump(code, ip)
			in.op, in.target = irJump, t
			if t <= ip { // back: it spends budget, and leaves when it runs out
				in.snap = f.snapshot(p, t)
			}
		case bytecode.OpForPrep, bytecode.OpForLoop: // an integer loop inside
			for j := range in.regs {
				in.regs[j] = f.valueAt(ip+1, i.A()+j, kindInt)
				f.fixed[in.regs[j]] = true
			}
			if op == bytecode.OpForPrep {
				in.op, in.target, in.snap = irForPrep, ip+2+i.SBx(), f.snapshot(p, ip) // step 0: Go raises the error
				in.a, in.b, in.c = irArg{v: in.regs[0], t: kindInt}, irArg{v: in.regs[1], t: kindInt}, irArg{v: in.regs[2], t: kindInt}
				break
			}
			in.op, in.target = irForLoop, ip+1+i.SBx()
			in.snap = f.snapshot(p, in.target)
		case bytecode.OpLength:
			in.op, in.dst, in.a, in.snap = irLen, dst(), f.arg(p, ip, i.B()), f.snapshot(p, ip)
		case bytecode.OpGetUpValue:
			// The function an intrinsic call checked on entry, or a buffer the
			// context holds: nothing. A number, from the context.
			if k.upCopies[ip] {
				in.op, in.obj, in.imm = irCopyUp, i.A(), int64(i.B())
				break
			}
			s, ok := k.upLoads[ip]
			if !ok || !isRegKind(res) {
				continue
			}
			in.op, in.dst, in.imm = irHoist, dst(), int64(s)
		case bytecode.OpCall:
			if k.resumes[ip] {
				f.leaves = true
				f.resumes = append(f.resumes, ip)
				in.op, in.flag, in.snap = irExit, true, f.snapshot(p, ip)
				break
			}
			kc := k.calls[ip]
			if kc.closure != nil {
				f.inline(p, ip, kc, dst(), emit)
				continue
			}
			in.dst, in.a = dst(), f.arg(p, ip, i.A()+1)
			if kc.math == mathNone {
				in.op, in.imm, in.snap = irIntrinsic, int64(kc.fn), f.snapshot(p, ip)
				break
			}
			in.op, in.imm = irMath, int64(kc.math)
			if kc.args == 2 {
				in.b = f.arg(p, ip, i.A()+2)
			}
			if (kc.math == mathFloor || kc.math == mathCeil) && in.a.t == kindFloat {
				in.snap = f.snapshot(p, ip) // no integer: Go gives the float
			}
		case bytecode.OpGetTable, bytecode.OpGetTableUp: // GETTABUP's B, the upvalue, is hoisted
			in.snap = f.snapshot(p, ip)
			if s, ok := k.upFields[ip]; ok { // the table in the context's hoisted slot s
				in.op, in.buf = irFieldGet, s
				if in.dst, in.obj = noVreg, i.A(); res != kindBoxed {
					in.dst = dst()
				}
				break
			}
			if field, ok := k.tabUses[ip]; ok {
				in.op, in.a = irArrayGet, f.arg(p, ip, i.B())
				if in.dst, in.obj = noVreg, i.A(); res != kindBoxed {
					in.dst = dst()
				}
				if field {
					in.op = irFieldGet
				} else {
					in.b = f.arg(p, ip, i.C())
				}
				break
			}
			in.op, in.dst, in.a, in.buf, in.obj = irBufGet, dst(), f.arg(p, ip, i.C()), k.bufFrom[ip], i.B()
		case bytecode.OpSetTable, bytecode.OpSetTableUp:
			in.snap = f.snapshot(p, ip)
			if field, ok := k.tabUses[ip]; ok {
				in.op, in.a, in.c = irArraySet, f.arg(p, ip, i.A()), f.arg(p, ip, i.C())
				if field {
					in.op = irFieldSet
				} else {
					in.b = f.arg(p, ip, i.B())
				}
				break
			}
			in.op, in.a, in.b, in.buf, in.obj = irBufSet, f.arg(p, ip, i.B()), f.arg(p, ip, i.C()), k.bufFrom[ip], i.A()
		}
		if in.op >= irArrayGet { // a guess about a table may fail at every iteration
			f.leaves = true
		}
		if (in.op == irBufGet || in.op == irBufSet) && in.a.t == kindFloat {
			in.tmp = scratch
		}
		emit(in)
	}
	f.end = f.snapshot(p, k.latch)
	return f
}

// inline emits the call at ip of kc's Lua function, into dst: its
// registers are virtual registers of their own, or the arguments' and
// constants it moves, and its RETURN moves the result. inlinable made sure
// nothing in it leaves.
func (f *irFunc) inline(p *prototype, ip int, kc kernelCall, dst vreg, emit func(irInst)) {
	q, a := kc.closure.prototype, p.Code[ip].A()
	regs := map[int]irArg{}
	for j := range kc.args {
		// The argument's type, which its source, dead once copied, has.
		t := f.typeAt(ip, a+1+j)
		regs[j] = irArg{v: f.valueAt(ip, copied(p.Code, kc.get, ip, a+1+j), t), t: t}
	}
	rk := func(field int) irArg {
		if bytecode.IsConstant(field) {
			v := q.Constants[bytecode.ConstantIndex(field)]
			return irArg{v: noVreg, k: litK, t: constKind(v), lit: v}
		}
		return regs[field]
	}
	state := func() map[int]numKind {
		m := map[int]numKind{}
		for r, x := range regs {
			m[r] = x.t
		}
		return m
	}
	for _, i := range q.Code {
		in := irInst{pc: ip, dst: noVreg, a: noArg, b: noArg, c: noArg, snap: -1, buf: -1, tmp: noVreg, target: -1}
		switch op := i.OpCode(); op {
		case bytecode.OpReturn:
			if x := regs[i.A()]; x.isConst() {
				in.op, in.dst, in.a = irConst, dst, x
			} else {
				in.op, in.dst, in.a = irMove, dst, x
			}
			emit(in)
			return
		case bytecode.OpMove:
			regs[i.A()] = regs[i.B()]
		case bytecode.OpLoadConstant:
			v := q.Constants[i.Bx()]
			regs[i.A()] = irArg{v: noVreg, k: litK, t: constKind(v), lit: v}
		default:
			t, _ := result(q, i, state())
			in.op = map[bytecode.OpCode]irOp{bytecode.OpAdd: irAdd, bytecode.OpSub: irSub,
				bytecode.OpMul: irMul, bytecode.OpDiv: irDiv, bytecode.OpUnaryMinus: irNeg}[op]
			in.dst, in.a = f.temp(t), rk(i.B())
			if op != bytecode.OpUnaryMinus {
				in.b = rk(i.C())
			}
			emit(in)
			regs[i.A()] = irArg{v: in.dst, t: t}
		}
	}
}

// copied returns the register the argument register r of the call at ip
// copies, when the last write to it after from, the call's GETUPVAL, is a
// MOVE whose source nothing writes after it: the argument's value, which
// the inlined function reads instead, so that the copy dies at once and
// takes no register through the call. Otherwise it returns r.
func copied(code []bytecode.Instruction, from, ip, r int) int {
	src := r
	for j := from + 1; j < ip; j++ {
		i := code[j]
		if i.A() == src && src != r { // the source written: stay with r
			src = r
		}
		if i.A() == r {
			src = r
			if i.OpCode() == bytecode.OpMove {
				src = i.B()
			}
		}
	}
	return src
}

// inlineReg is the Lua register of the first virtual register an inlined
// function's temporary takes; the next take the ones below.
const inlineReg = -1000

// temp returns a new virtual register of type t for a temporary of an
// inlined function, which never spills.
func (f *irFunc) temp(t numKind) vreg {
	f.temps++
	return f.value(inlineReg-f.temps, t)
}

// defs returns the virtual registers in writes.
func (f *irFunc) defs(in *irInst) []vreg {
	switch in.op {
	case irForPrep, irForLoop:
		return []vreg{in.regs[0], in.regs[1], in.regs[3]}
	}
	if in.dst != noVreg {
		return []vreg{in.dst}
	}
	return nil
}

// uses returns the virtual registers in reads. An exit reads what it
// writes back: the ordinary code may read any of it.
func (f *irFunc) uses(in *irInst) []vreg {
	var vs []vreg
	if in.op == irExit {
		for _, r := range f.snaps[in.snap].regs {
			if r.v != noVreg {
				vs = append(vs, r.v)
			}
		}
	}
	for _, a := range []irArg{in.a, in.b, in.c} {
		if a.v != noVreg {
			vs = append(vs, a.v)
		}
	}
	if in.op == irForLoop {
		vs = append(vs, in.regs[0], in.regs[1], in.regs[2])
	}
	return vs
}

// allocate gives each virtual register a machine register, in loc, by
// its index in its class. The pinned ones each keep their own; the
// body's temporaries, dead at the latch, share by liveness: two share
// one unless some operation needs both. It reports false when the kernel
// needs more than maxFloats float or maxInts integer registers.
//
// A side exit writes back every register the body writes, and so may
// store a dead temporary's register, holding another's number, into it.
// The ordinary code writes a dead register before reading it, as the
// kernel does.
func (f *irFunc) allocate(maxFloats, maxInts int) bool {
	n := len(f.vregs)
	conflict := make([][]bool, n)
	for x := range conflict {
		conflict[x] = make([]bool, n)
	}
	labels := map[int]int{f.latch: len(f.insts)} // instruction indices by pc
	for x, in := range f.insts {
		if in.op == irLabel {
			labels[in.target] = x
		}
	}
	succ := func(x int) []int {
		switch in := &f.insts[x]; in.op {
		case irJump:
			return []int{labels[in.target]}
		case irBranch, irForPrep, irForLoop:
			if in.target >= 0 {
				return []int{x + 1, labels[in.target]}
			}
		case irExit:
			return nil
		}
		return []int{x + 1}
	}
	// Liveness of the temporaries, backwards, again until it settles, as
	// loops inside jump back.
	live := make([]map[vreg]bool, len(f.insts)+1)
	for x := range live {
		live[x] = map[vreg]bool{}
	}
	for changed := true; changed; {
		changed = false
		for x := len(f.insts) - 1; x >= 0; x-- {
			in := &f.insts[x]
			next := map[vreg]bool{}
			for _, s := range succ(x) {
				maps.Copy(next, live[s])
			}
			if in.op != irLabel {
				for _, d := range f.defs(in) {
					delete(next, d)
				}
				for _, v := range f.uses(in) {
					if !f.pinned(v) {
						next[v] = true
					}
				}
			}
			if !maps.Equal(next, live[x]) {
				live[x], changed = next, true
			}
		}
	}
	for x := range f.insts {
		in := &f.insts[x]
		if in.op == irLabel {
			f.liveAt[in.target] = live[x]
			continue
		}
		// What an operation reads needs a register of its own, as does
		// what it writes and what lives past it. Each operation reads its
		// operands before it writes its result, and leaves the kernel
		// before either, so the result may take an operand's register when
		// it reads it last.
		var needed []vreg
		for _, d := range f.defs(in) {
			if !f.pinned(d) {
				needed = append(needed, d)
			}
		}
		for _, s := range succ(x) {
			for v := range live[s] {
				needed = append(needed, v)
			}
		}
		for _, set := range [][]vreg{needed, slices.Collect(maps.Keys(live[x]))} {
			for _, x := range set {
				for _, y := range set {
					conflict[x][y] = true
				}
			}
		}
	}

	f.reload = [2]int{-1, -1}
	floats, ints := f.colour(conflict, maxFloats, maxInts, false, false)
	if floats <= maxFloats && ints <= maxInts {
		return true
	}
	// An integer loop's count and step may live in their stack slots,
	// which needs no registers for loading them: only its latch uses them.
	loopSpill := f.intLoop && !f.while
	if loopSpill {
		if floats, ints = f.colour(conflict, maxFloats, maxInts, false, true); floats <= maxFloats && ints <= maxInts {
			return true
		}
	}
	// Too few: keep the last two of each class that runs out for loading
	// spilled values, which live in their Lua registers' stack slots, and
	// spill what does not fit in the rest.
	limitF, limitI := maxFloats, maxInts
	if floats > maxFloats {
		limitF -= spillRegs
		f.reload[0] = limitF
	}
	if ints > maxInts {
		limitI -= spillRegs
		f.reload[1] = limitI
	}
	floats, ints = f.colour(conflict, limitF, limitI, true, loopSpill)
	return floats <= limitF && ints <= limitI
}

// spillRegs is how many machine registers of a class hold spilled values
// while an operation uses them: operations read at most two of a class,
// and a result may take an operand's.
const spillRegs = 2

// colour gives each virtual register the first machine register in its
// class no earlier one it conflicts with has, in loc, and returns how
// many of each class it used. With spill, one that would take register
// limitF or limitI or above spills, loc -1, unless it is one of the
// loop's, a loop inside's, or the scratch key, which then take it. With
// loopSpill, the loop's count and step, which only its latch uses, come
// last and spill rather than take such a register.
func (f *irFunc) colour(conflict [][]bool, limitF, limitI int, spill, loopSpill bool) (floats, ints int) {
	f.loc = make([]int, len(f.vregs))
	late := func(v vreg) bool { return loopSpill && (v == f.loop[1] || v == f.loop[2]) }
	order := make([]int, 0, len(f.vregs))
	for x := range f.vregs {
		if !late(vreg(x)) {
			order = append(order, x)
		}
	}
	for x := range f.vregs {
		if late(vreg(x)) {
			order = append(order, x)
		}
	}
	done := make([]bool, len(f.vregs))
	for _, x := range order {
		v := vreg(x)
		taken := map[int]bool{}
		for y := range f.vregs {
			w := vreg(y)
			if done[y] && f.class(w) == f.class(v) && f.loc[y] >= 0 && (f.pinned(v) || f.pinned(w) || conflict[x][y]) {
				taken[f.loc[y]] = true
			}
		}
		m := 0
		for taken[m] {
			m++
		}
		limit := limitF
		if f.class(v) == 1 {
			limit = limitI
		}
		done[x] = true
		if m >= limit && (late(v) || spill && f.vregs[x].r >= 0 && !slices.Contains(f.loop[:], v) && !f.fixed[v]) {
			f.loc[x] = -1
			continue
		}
		f.loc[x] = m
		if f.class(v) == 1 {
			ints = max(ints, m+1)
		} else {
			floats = max(floats, m+1)
		}
	}
	return floats, ints
}

// worksBetweenCalls reports whether f does enough in registers for the
// calls it leaves at and resumes after to pay: each costs writing the
// registers back and checking and loading them again, which a kernel of
// little else spends more on than the ordinary code would.
func (f *irFunc) worksBetweenCalls() bool {
	work := 0
	for _, in := range f.insts {
		switch in.op {
		case irLabel, irMove, irConst, irHoist, irJump, irExit, irCopyUp, irCopy:
		default:
			work++
		}
	}
	return work >= minCallWork*len(f.resumes)
}

// entryRegs returns the virtual registers a kernel loads on entry: the
// live-in ones, but those left on the stack.
func (f *irFunc) entryRegs() []vreg {
	var vs []vreg
	for _, r := range f.liveIn {
		if t := f.types[r]; isRegKind(t) {
			vs = append(vs, f.value(r, t))
		}
	}
	return vs
}

// resumeRegs returns the virtual registers a kernel resuming at pc, after
// a call, loads: those live there, and the pinned ones whose registers
// hold their types there.
func (f *irFunc) resumeRegs(pc int) []vreg {
	types := f.at[pc-f.start]
	var vs []vreg
	for x, s := range f.vregs {
		v := vreg(x)
		if s.r >= 0 && (f.liveAt[pc][v] || f.pinned(v)) && types[s.r] == s.t && !slices.Contains(vs, v) {
			vs = append(vs, v)
		}
	}
	return vs
}

// spilled reports whether v lives in its Lua register's stack slot.
func (f *irFunc) spilled(v vreg) bool { return f.loc[v] < 0 }

// class is the index of v's class in reload: 0 for floats, 1 for
// integers and tables, which general-purpose registers hold.
func (f *irFunc) class(v vreg) int {
	if f.vregs[v].t == kindFloat {
		return 0
	}
	return 1
}

// isInt reports whether a general-purpose register holds v.
func (f *irFunc) isInt(v vreg) bool { return f.class(v) == 1 }

// machine returns the index, in its class, of the machine register that
// holds v: its own, or the reload register it has while an operation uses
// it, when it is spilled.
func (f *irFunc) machine(v vreg) int {
	if l := f.loc[v]; l >= 0 {
		return l
	}
	o, ok := f.cur[v]
	if !ok {
		panic("kernel: a spilled register used without loading")
	}
	return f.reload[f.class(v)] + o
}

// spills gives the spilled virtual registers in uses reload registers
// for it, and returns those to load before it and the one to store after,
// or noVreg.
func (f *irFunc) spills(in *irInst) (loads []vreg, store vreg) {
	clear(f.cur)
	next := [2]int{}
	for _, a := range []irArg{in.a, in.b, in.c} {
		if v := a.v; v != noVreg && f.spilled(v) {
			if _, ok := f.cur[v]; !ok {
				f.cur[v] = next[f.class(v)]
				next[f.class(v)]++
				loads = append(loads, v)
			}
		}
	}
	store = noVreg
	if v := in.dst; v != noVreg && f.spilled(v) {
		if _, ok := f.cur[v]; !ok {
			f.cur[v] = 0 // it reads its operands first
		}
		store = v
	}
	return loads, store
}

// String lists f's operations and their virtual registers, for tests and
// debugging.
func (f *irFunc) String() string {
	var b strings.Builder
	name := func(v vreg) string {
		s := f.vregs[v]
		t := map[numKind]string{kindFloat: "f", kindInt: "i", kindTable: "t", kindBool: "b"}[s.t]
		return fmt.Sprintf("%s%d.r%d", t, v, s.r)
	}
	arg := func(a irArg) string {
		if a.k == litK {
			return fmt.Sprint(a.lit.toFloat())
		}
		if a.isConst() {
			return fmt.Sprintf("k%d", a.k)
		}
		if a.v == noVreg {
			return "-"
		}
		return name(a.v)
	}
	for _, in := range f.insts {
		if in.op == irLabel {
			fmt.Fprintf(&b, "%d:\n", in.target)
			continue
		}
		fmt.Fprintf(&b, "\t%s", in.op)
		if in.dst != noVreg {
			fmt.Fprintf(&b, " %s =", name(in.dst))
		}
		switch in.op {
		case irBranch:
			fmt.Fprintf(&b, " %s %v %s %v -> %d", arg(in.a), in.cmp, arg(in.b), in.flag, in.target)
		case irJump:
			fmt.Fprintf(&b, " -> %d", in.target)
		case irHoist:
			fmt.Fprintf(&b, " #%d", in.imm)
		default:
			for j, a := range []irArg{in.a, in.b, in.c} {
				if a.v != noVreg || a.isConst() {
					if j > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, " %s", arg(a))
				}
			}
			if in.op == irShift || in.op == irIntDiv || in.op == irIntMod || in.op == irMath {
				fmt.Fprintf(&b, " #%d", in.imm)
			}
		}
		if in.snap >= 0 {
			fmt.Fprintf(&b, " exit@%d", f.snaps[in.snap].pc)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// b2i is 1 for true and 0 for false.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
