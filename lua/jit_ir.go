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
	irBranch                // to target when a cmp b is flag
	irJump                  // to target
	irIntrinsic             // dst = the number function imm (a)
	irMath                  // dst = math function imm (a, b), of a's type
	irBufGet                // dst = buffer[a]
	irBufSet                // buffer[a] = b
)

var irOpNames = [...]string{"label", "move", "const", "hoist", "add", "sub", "mul", "div", "floordiv",
	"fmod", "idiv", "imod", "and", "or", "xor", "not", "shift", "neg", "branch", "jump", "intrinsic",
	"math", "bufget", "bufset"}

func (o irOp) String() string { return irOpNames[o] }

// irArg is an operation's argument: a virtual register, or constant k.
type irArg struct {
	v vreg
	k int
	t numKind // its type
}

// noArg is an operation's unused argument.
var noArg = irArg{v: noVreg, k: -1}

// isConst reports whether the argument is a constant.
func (a irArg) isConst() bool { return a.v == noVreg && a.k >= 0 }

// irInst is one operation, from the instruction at pc.
type irInst struct {
	op     irOp
	pc     int
	dst    vreg
	a, b   irArg
	imm    int64
	flag   bool
	cmp    bytecode.OpCode // a branch's comparison: EQ, LT or LE
	target int             // a label's or a jump's pc
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
	insts  []irInst
	vregs  []kslot // each one's Lua register and type
	index  map[kslot]vreg
	snaps  []irSnap
	snapAt map[int]int // snapshot indices by pc
	loop   [4]vreg     // the FORLOOP's index, limit (or count), step and variable
	end    int         // the latch's snapshot, for leaving at the end of an iteration
	share  bool        // the variable shares the index's vreg
	loc    []int       // each vreg's machine register, in its class: see allocate
}

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
	return r < f.base+4 || slices.Contains(f.liveIn, r)
}

// arg returns RK field before ip as an argument.
func (f *irFunc) arg(p *prototype, ip, field int) irArg {
	if bytecode.IsConstant(field) {
		kk := bytecode.ConstantIndex(field)
		return irArg{v: noVreg, k: kk, t: constKind(p.Constants[kk])}
	}
	t := f.typeAt(ip, field)
	return irArg{v: f.value(field, t), t: t}
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
		case isNumKind(t):
			s.regs = append(s.regs, irSnapReg{r: r, v: f.value(r, t), t: t, alias: -1})
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
	f.snaps = append(f.snaps, s)
	f.snapAt[ip] = len(f.snaps) - 1
	return len(f.snaps) - 1
}

// buildIR returns k's IR.
func buildIR(p *prototype, k *kernelPlan) *irFunc {
	code := p.Code
	f := &irFunc{kernelPlan: k, index: map[kslot]vreg{}, snapAt: map[int]int{}, share: k.shareVar}
	base := k.base
	// Virtual registers in the order allocate colours them: the loop's,
	// the live-in ones, then as the body writes them.
	for r := base; r <= base+3; r++ {
		f.loop[r-base] = f.value(r, k.types[base])
	}
	for _, r := range k.liveIn {
		f.value(r, k.types[r])
	}
	for ip := k.start; ip < k.latch; ip++ {
		if t := k.results[ip]; isNumKind(t) {
			f.value(code[ip].A(), t)
		}
	}
	scratch := noVreg
	if k.floatKeys {
		scratch = f.value(keyScratch, kindInt)
	}
	emit := func(in irInst) {
		if in.op != irBranch && in.op != irJump && in.op != irLabel {
			in.target = -1
		}
		f.insts = append(f.insts, in)
	}
	for ip := k.start; ip < k.latch; ip++ {
		i := code[ip]
		emit(irInst{op: irLabel, pc: ip, target: ip, dst: noVreg, a: noArg, b: noArg, snap: -1, buf: -1, tmp: noVreg})
		in := irInst{pc: ip, dst: noVreg, a: noArg, b: noArg, snap: -1, buf: -1, tmp: noVreg}
		res := k.results[ip]
		dst := func() vreg { return f.value(i.A(), res) }
		switch op := i.OpCode(); op {
		case bytecode.OpMove:
			if !isNumKind(res) {
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
		case bytecode.OpEqual, bytecode.OpLessThan, bytecode.OpLessOrEqual:
			t, _ := kernelJump(code, ip, k.latch)
			in.op, in.cmp, in.flag, in.target = irBranch, op, i.A() != 0, t
			in.a, in.b = f.arg(p, ip, i.B()), f.arg(p, ip, i.C())
			emit(in)
			ip++ // the JMP
			continue
		case bytecode.OpJump:
			t, _ := kernelJump(code, ip, k.latch)
			in.op, in.target = irJump, t
		case bytecode.OpGetUpValue:
			// The function an intrinsic call checked on entry, or a buffer the
			// context holds: nothing. A number, from the context.
			s, ok := k.upLoads[ip]
			if !ok || !isNumKind(res) {
				continue
			}
			in.op, in.dst, in.imm = irHoist, dst(), int64(s)
		case bytecode.OpCall:
			kc := k.calls[ip]
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
			in.op, in.dst, in.a, in.buf, in.obj = irBufGet, dst(), f.arg(p, ip, i.C()), k.bufFrom[ip], i.B()
			in.snap = f.snapshot(p, ip)
		case bytecode.OpSetTable, bytecode.OpSetTableUp:
			in.op, in.a, in.b, in.buf, in.obj = irBufSet, f.arg(p, ip, i.B()), f.arg(p, ip, i.C()), k.bufFrom[ip], i.A()
			in.snap = f.snapshot(p, ip)
		}
		if (in.op == irBufGet || in.op == irBufSet) && in.a.t == kindFloat {
			in.tmp = scratch
		}
		emit(in)
	}
	f.end = f.snapshot(p, k.latch)
	return f
}

// uses returns the virtual registers in reads.
func (in *irInst) uses() []vreg {
	var vs []vreg
	for _, a := range []irArg{in.a, in.b} {
		if a.v != noVreg {
			vs = append(vs, a.v)
		}
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
	// Liveness of the temporaries, backwards: jumps only go forward.
	live := make([]map[vreg]bool, len(f.insts)+1)
	live[len(f.insts)] = map[vreg]bool{}
	for x := len(f.insts) - 1; x >= 0; x-- {
		in := &f.insts[x]
		succ := []int{x + 1}
		switch in.op {
		case irLabel:
			live[x] = live[x+1]
			continue
		case irJump:
			succ = []int{labels[in.target]}
		case irBranch:
			succ = append(succ, labels[in.target])
		}
		out := map[vreg]bool{}
		for _, s := range succ {
			maps.Copy(out, live[s])
		}
		in2 := maps.Clone(out)
		var needed []vreg
		if in.dst != noVreg && !f.pinned(in.dst) {
			delete(in2, in.dst)
			needed = append(needed, in.dst)
		}
		for _, v := range in.uses() {
			if !f.pinned(v) {
				in2[v] = true
			}
		}
		live[x] = in2
		// What an operation reads needs a register of its own, as does
		// what it writes and what lives past it. Each operation reads its
		// operands before it writes its result, and leaves the kernel
		// before either, so the result may take an operand's register when
		// it reads it last.
		for v := range out {
			needed = append(needed, v)
		}
		for _, set := range [][]vreg{needed, slices.Collect(maps.Keys(in2))} {
			for _, x := range set {
				for _, y := range set {
					conflict[x][y] = true
				}
			}
		}
	}

	f.loc = make([]int, n)
	floats, ints := 0, 0
	for x := range f.vregs {
		v := vreg(x)
		taken := map[int]bool{}
		for y := range x {
			w := vreg(y)
			if f.vregs[y].t == f.vregs[x].t && (f.pinned(v) || f.pinned(w) || conflict[x][y]) {
				taken[f.loc[y]] = true
			}
		}
		m := 0
		for taken[m] {
			m++
		}
		f.loc[x] = m
		if f.vregs[x].t == kindInt {
			ints = max(ints, m+1)
		} else {
			floats = max(floats, m+1)
		}
	}
	return floats <= maxFloats && ints <= maxInts
}

// String lists f's operations and their virtual registers, for tests and
// debugging.
func (f *irFunc) String() string {
	var b strings.Builder
	name := func(v vreg) string {
		s := f.vregs[v]
		t := "f"
		if s.t == kindInt {
			t = "i"
		}
		return fmt.Sprintf("%s%d.r%d", t, v, s.r)
	}
	arg := func(a irArg) string {
		if a.isConst() {
			return fmt.Sprintf("k%d", a.k)
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
			for j, a := range []irArg{in.a, in.b} {
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
