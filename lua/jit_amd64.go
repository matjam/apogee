//go:build (darwin || linux) && amd64

package lua

import (
	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/amd64"
)

const jitSupported = true

// Registers the generated code keeps for its whole run. The budget, the
// write barrier flag and the upvalues stay in the context, as amd64 has
// too few registers to keep them all.
const (
	rCtx    Reg = DI  // *jitContext
	rFrame  Reg = BX  // &frame[0]
	rConst  Reg = SI  // &constants[0]
	rNumber Reg = R15 // numberPtr(), the p of every number
	rTmp    Reg = AX
	rTmp2   Reg = CX
	rAddr   Reg = DX  // an upvalue's address, or a table slot's
	rP      Reg = R8  // a value's p, while it is copied
	rN      Reg = R9  // a value's second word, while it is copied
	rT      Reg = R10 // a table, while it is indexed
	rT2     Reg = R11 // a second table: a metatable or its __index
	rCache  Reg = R12 // the instruction's *fieldCache
	rIdx    Reg = R13 // a slot or array index
	rLen        = rTmp2
	rSlot       = rAddr
)

// maxOffset bounds frame and constant offsets, which are 32-bit
// displacements.
const maxOffset = 1 << 30

// amd64Compiler translates a prototype into amd64 code. Like the arm64
// compiler, each instruction's code checks everything it needs before it
// writes anything.
type amd64Compiler struct {
	a       Asm
	p       *prototype
	g       *globalState // the state p runs in, whose string metatable SELF reads
	cl      *luaClosure  // the closure being compiled, whose upvalues kernels speculate on
	code    []bytecode.Instruction
	pcs     []Label
	exits   []Label
	budget  []Label
	goCall  []Label // exits at a CALL of a Go function, created on demand
	plainGo []Label // goCallee's code for a Go function no number function (plainGoCall)
	numCall []Label // exits at a CALL of a number function, created on demand
	notLua  []Label // a CALL's out-of-line code for callees other than Lua closures
	strSelf []Label // a SELF's out-of-line code for receivers other than tables
	// outOfLine emit code instructions branch to rarely, after the
	// function's: buffers in table instructions, and kernels' side exits.
	outOfLine  []func()
	always     []bool
	sse41      bool  // ROUNDSD is available, for floor and modulo
	ip         int   // the instruction being compiled, for intrinsics' exits
	kernelExit Label // in a kernel's intrinsic call, its side exit; otherwise -1
}

// intrinsicExit is where an intrinsic's code exits: the instruction's
// exit, or in a kernel the side exit its call set.
func (c *amd64Compiler) intrinsicExit() Label {
	if c.kernelExit >= 0 {
		return c.kernelExit
	}
	return c.exit(c.ip)
}

func compileJIT(p *prototype, g *globalState, cl *luaClosure) (code []byte, offsets []int32, entries []int, kernels int) {
	if len(p.Code) > 1<<16 {
		return nil, nil, nil, 0
	}
	c := &amd64Compiler{p: p, g: g, cl: cl, code: p.jitOrig, sse41: HasSSE41(), kernelExit: -1}
	c.pcs = make([]Label, len(c.code))
	c.exits = make([]Label, len(c.code))
	c.budget = make([]Label, len(c.code))
	c.goCall = make([]Label, len(c.code))
	c.plainGo = make([]Label, len(c.code))
	c.numCall = make([]Label, len(c.code))
	c.notLua = make([]Label, len(c.code))
	c.strSelf = make([]Label, len(c.code))
	c.always = make([]bool, len(c.code))
	for i := range c.pcs {
		c.pcs[i], c.exits[i], c.budget[i] = c.a.NewLabel(), -1, -1
		c.goCall[i], c.numCall[i], c.notLua[i], c.strSelf[i] = -1, -1, -1, -1
		c.plainGo[i] = -1
	}
	c.prologue()
	loops := map[int][]*kernel{}
	for ip, i := range c.p.Code {
		if i.OpCode() == bytecode.OpForLoop && !isExtraArg(c.p.Code, ip) {
			if ks := c.findKernels(ip); ks != nil {
				loops[ip] = ks
			}
		}
	}
	for ip := 0; ip < len(c.code); ip++ {
		c.a.Bind(c.pcs[ip])
		for _, k := range loops[ip] {
			normal := c.a.NewLabel()
			c.emitKernel(k, normal)
			c.a.Bind(normal)
		}
		ip += c.instruction(ip)
	}
	c.stubs()
	code, err := c.a.Code()
	if err != nil {
		if jitStrict {
			panic(err)
		}
		return nil, nil, nil, 0
	}
	offsets = make([]int32, len(c.code))
	for i, l := range c.pcs {
		offsets[i] = int32(c.a.Offset(l))
		if isExtraArg(c.p.Code, i) {
			offsets[i] = -1
		}
	}
	exits := make([]bool, len(c.exits))
	for i, l := range c.exits {
		exits[i] = l >= 0
	}
	kernels = 0
	for _, ks := range loops {
		kernels += len(ks)
	}
	return code, offsets, jitEntries(c.p, exits, c.always), kernels
}

// prologue loads the fixed registers and jumps to ctx.target.
func (c *amd64Compiler) prologue() {
	a := &c.a
	a.Load(rFrame, rCtx, offFrame)
	a.Load(rConst, rCtx, offConstants)
	a.MovImm(rNumber, uint64(uintptr(numberPtr())))
	a.Load(rTmp, rCtx, offTarget)
	a.JmpReg(rTmp)
}

// stubs emits the exits that instructions branch to.
func (c *amd64Compiler) stubs() {
	a := &c.a
	for ip, l := range c.notLua {
		if l >= 0 {
			a.Bind(l)
			c.goCallee(ip, c.code[ip])
			a.Jmp(c.exit(ip))
		}
	}
	for ip, l := range c.strSelf {
		if l >= 0 {
			a.Bind(l)
			c.selfString(ip, c.code[ip])
			a.Jmp(c.pcs[ip+1])
		}
	}
	for k := 0; k < len(c.outOfLine); k++ { // an emit may add more
		c.outOfLine[k]()
	}
	for ip, l := range c.exits {
		if l >= 0 {
			a.Bind(l)
			c.exitWith(ip, jitExitInstruction)
		}
	}
	for ip, l := range c.budget {
		if l >= 0 {
			a.Bind(l)
			c.exitWith(ip, jitExitBudget)
		}
	}
	for _, calls := range []struct {
		labels []Label
		reason uint64
	}{{c.goCall, jitExitCallGo}, {c.numCall, jitExitCallNumber}} {
		for ip, l := range calls.labels {
			if l >= 0 {
				a.Bind(l)
				fn := reg(c.code[ip].A())
				a.Load(rTmp, fn.base, fn.off+offP)
				a.Store(rCtx, offCallee, rTmp)
				a.Store(rCtx, offFrame, rFrame) // compiled calls may have moved it
				c.exitWith(ip, calls.reason)
			}
		}
	}
}

func (c *amd64Compiler) exitWith(ip int, reason uint64) {
	c.a.MovImm(rTmp, uint64(ip))
	c.a.Store(rCtx, offExitPC, rTmp)
	c.a.MovImm(rTmp, reason)
	c.a.Ret()
}

// exit returns the label that resumes the interpreter at ip.
func (c *amd64Compiler) exit(ip int) Label {
	if c.exits[ip] < 0 {
		c.exits[ip] = c.a.NewLabel()
	}
	return c.exits[ip]
}

// goCallExit returns the label that exits at the CALL at ip for runJIT to
// call the Go function in its register.
func (c *amd64Compiler) goCallExit(ip int) Label {
	if c.goCall[ip] < 0 {
		c.goCall[ip] = c.a.NewLabel()
	}
	return c.goCall[ip]
}

// numCallExit returns the label that exits at the CALL at ip for runJIT to
// call the number function in its register.
func (c *amd64Compiler) numCallExit(ip int) Label {
	if c.numCall[ip] < 0 {
		c.numCall[ip] = c.a.NewLabel()
	}
	return c.numCall[ip]
}

// exitAlways compiles the instruction at ip as an exit.
func (c *amd64Compiler) exitAlways(ip int) {
	c.always[ip] = true
	c.a.Jmp(c.exit(ip))
}

// operand locates a value in memory: its base register and byte offset.
type operand struct {
	base Reg
	off  uint32
}

func reg(r int) operand { return operand{rFrame, uint32(r) * valueSize} }

func (c *amd64Compiler) constant(k int) (operand, bool) {
	if uint64(k)*uint64(valueSize) >= maxOffset {
		return operand{}, false
	}
	return operand{rConst, uint32(k) * valueSize}, true
}

// isNumberConstant reports whether an RK field is a number constant.
func (c *amd64Compiler) isNumberConstant(field int) bool {
	return bytecode.IsConstant(field) && c.p.Constants[bytecode.ConstantIndex(field)].isFloat()
}

// rk returns the operand for an RK field.
func (c *amd64Compiler) rk(field int) (operand, bool) {
	if !bytecode.IsConstant(field) {
		return reg(field), true
	}
	return c.constant(bytecode.ConstantIndex(field))
}

// rkArith returns the operand for an RK field of arithmetic, with the
// number type of a constant, and false for a constant that is not a
// number.
func (c *amd64Compiler) rkArith(field int) (operand, numKind, bool) {
	if !bytecode.IsConstant(field) {
		return reg(field), kindAny, true
	}
	k := bytecode.ConstantIndex(field)
	kind := constKind(c.p.Constants[k])
	if kind == kindAny {
		return operand{}, kindAny, false
	}
	o, ok := c.constant(k)
	return o, kind, ok
}

// branchUnlessInteger jumps to l unless the number at o, of type kind, is
// an integer. For a register it leaves p - numberPtr() in rTmp: 0 for a
// float, 1 for an integer.
func (c *amd64Compiler) branchUnlessInteger(o operand, kind numKind, l Label) {
	a := &c.a
	switch kind {
	case kindFloat:
		a.Jmp(l)
	case kindAny:
		a.Load(rTmp, o.base, o.off+offP)
		a.Sub(rTmp, rNumber)
		a.CmpImm(rTmp, 1)
		a.J(NE, l)
	}
}

// toFloat converts the integer in r to a float in x. CVTSI2SD writes only
// x's low half, so waits for whatever last wrote x; clearing x first cuts
// that dependency, which otherwise chains unrelated calculations together.
func (c *amd64Compiler) toFloat(x XReg, r Reg) {
	c.a.XorPD(x, x)
	c.a.Cvtsi2sd(x, r)
}

// loadFloat loads the number at o, of type kind, into x as a float,
// converting an integer, and exits at ip for anything else. With exact
// set it exits for an integer the conversion would round, so that a
// comparison of the floats compares the numbers. It uses rTmp, rTmp2 and
// rN.
func (c *amd64Compiler) loadFloat(x XReg, o operand, kind numKind, exact bool, ip int) {
	a := &c.a
	switch kind {
	case kindFloat:
		a.LoadSD(x, o.base, o.off+offN)
		return
	case kindInt:
		if exact && !exactFloat(c.p.Constants[o.off/valueSize].i()) {
			a.Jmp(c.exit(ip))
			return
		}
		a.Load(rTmp, o.base, o.off+offN)
		c.toFloat(x, rTmp)
		return
	}
	isFloat, done := a.NewLabel(), a.NewLabel()
	a.Load(rTmp, o.base, o.off+offP)
	a.Sub(rTmp, rNumber)
	a.J(E, isFloat)
	a.CmpImm(rTmp, 1)
	a.J(NE, c.exit(ip))
	a.Load(rTmp, o.base, o.off+offN)
	if exact { // rTmp + 2^53 <= 2^54, unsigned
		a.MovImm(rTmp2, 1<<53)
		a.Add(rTmp2, rTmp)
		a.MovImm(rN, 1<<54)
		a.Cmp(rTmp2, rN)
		a.J(A, c.exit(ip))
	}
	c.toFloat(x, rTmp)
	a.Jmp(done)
	a.Bind(isFloat)
	a.LoadSD(x, o.base, o.off+offN)
	a.Bind(done)
}

// storeInteger writes the integer in r to dst. Its guardStore must come
// first.
func (c *amd64Compiler) storeInteger(dst operand, r Reg) {
	tmp := rTmp
	if r == rTmp {
		tmp = rTmp2
	}
	c.a.Store(dst.base, dst.off+offN, r)
	c.a.Mov(tmp, rNumber)
	c.a.AddImm(tmp, 1) // integerPtr, the next byte
	c.a.Store(dst.base, dst.off+offP, tmp)
}

// branchNumber jumps to l when p, a value's first word, is a number's: the
// float or the integer sentinel, which are adjacent. An integer's second
// word can match any tag, so tests for objects and strings must rule both
// out. It uses rTmp2, so p must be another register.
func (c *amd64Compiler) branchNumber(p Reg, l Label) {
	a := &c.a
	a.Mov(rTmp2, p)
	a.Sub(rTmp2, rNumber)
	a.CmpImm(rTmp2, 1)
	a.J(BE, l)
}

// guardNumber exits at ip unless o holds a float.
func (c *amd64Compiler) guardNumber(o operand, ip int) {
	if o.base == rConst {
		return
	}
	c.a.Load(rTmp, o.base, o.off+offP)
	c.a.Cmp(rTmp, rNumber)
	c.a.J(NE, c.exit(ip))
}

// guardScalar exits at ip unless p, a value's first word, is nil, a number
// or a boolean. It uses rTmp, so p must be another register.
func (c *amd64Compiler) guardScalar(p Reg, ip int) {
	a := &c.a
	ok := a.NewLabel()
	a.Test(p, p)
	a.J(E, ok)
	a.Mov(rTmp, p) // a float or an integer
	a.Sub(rTmp, rNumber)
	a.CmpImm(rTmp, 1)
	a.J(BE, ok)
	a.MovImm(rTmp, uint64(uintptr(boolPtr())))
	a.Cmp(p, rTmp)
	a.J(NE, c.exit(ip))
	a.Bind(ok)
}

// guardStore exits at ip if the write barrier is on and dst holds a heap
// pointer, or newP, when it is not noReg, is one.
func (c *amd64Compiler) guardStore(dst operand, newP Reg, ip int) {
	a := &c.a
	done := a.NewLabel()
	a.CmpMem(rCtx, offBarrier, 0)
	a.J(E, done)
	if newP != noReg {
		c.guardScalar(newP, ip)
	}
	a.Load(rTmp2, dst.base, dst.off+offP)
	c.guardScalar(rTmp2, ip)
	a.Bind(done)
}

const noReg Reg = 255

// storeNumber writes the number in x to dst. Its guardStore must come
// first.
func (c *amd64Compiler) storeNumber(dst operand, x XReg) {
	c.a.StoreSD(dst.base, dst.off+offN, x)
	c.a.Store(dst.base, dst.off+offP, rNumber)
}

// load reads the value at o into rP and rN.
func (c *amd64Compiler) load(o operand) {
	c.a.Load(rP, o.base, o.off+offP)
	c.a.Load(rN, o.base, o.off+offN)
}

// store writes rP and rN to dst. Its guardStore must come first.
func (c *amd64Compiler) store(dst operand) {
	c.a.Store(dst.base, dst.off+offN, rN)
	c.a.Store(dst.base, dst.off+offP, rP)
}

func (c *amd64Compiler) copyValue(dst, src operand, ip int) {
	c.load(src)
	c.guardStore(dst, rP, ip)
	c.store(dst)
}

// storeBool writes the boolean with tag bits bits to dst.
func (c *amd64Compiler) storeBool(dst operand, bits uint64) {
	c.a.MovImm(rTmp, bits)
	c.a.Store(dst.base, dst.off+offN, rTmp)
	c.a.MovImm(rTmp, uint64(uintptr(boolPtr())))
	c.a.Store(dst.base, dst.off+offP, rTmp)
}

// branchFalsy branches to l when the value at o is nil or false.
func (c *amd64Compiler) branchFalsy(o operand, l Label) {
	a := &c.a
	truthy := a.NewLabel()
	a.Load(rTmp, o.base, o.off+offP)
	a.Test(rTmp, rTmp)
	a.J(E, l)
	a.MovImm(rTmp2, uint64(uintptr(boolPtr())))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, truthy)
	a.Load(rTmp, o.base, o.off+offN)
	a.Bt(rTmp, 0)
	a.J(AE, l) // bit 0 clear: false
	a.Bind(truthy)
}

// spend spends one unit of budget, exiting at ip to resume there once it
// runs out.
func (c *amd64Compiler) spend(ip int) {
	if c.budget[ip] < 0 {
		c.budget[ip] = c.a.NewLabel()
	}
	c.a.SubMem(rCtx, offBudget, 1)
	c.a.J(E, c.budget[ip])
}

// backEdge spends budget and jumps to target.
func (c *amd64Compiler) backEdge(target int) {
	c.spend(target)
	c.a.Jmp(c.pcs[target])
}

// jumpTo jumps to target from ip, spending budget if it is backward.
func (c *amd64Compiler) jumpTo(ip, target int) {
	if target <= ip {
		c.backEdge(target)
	} else {
		c.a.Jmp(c.pcs[target])
	}
}

func (c *amd64Compiler) jumpAfter(ip int) (int, bool) {
	j := c.code[ip+1]
	if j.A() != 0 {
		return 0, false
	}
	return ip + 2 + j.SBx(), true
}

// compare branches to yes when x op y is jump, where op is EQ, LT or LE,
// and to no otherwise. NaN makes every comparison false.
func (c *amd64Compiler) compare(op bytecode.OpCode, jump bool, x, y XReg, yes, no Label) {
	a := &c.a
	switch op {
	case bytecode.OpEqual:
		a.Ucomisd(x, y)
		if jump {
			a.J(P, no)
			a.J(E, yes)
		} else {
			a.J(P, yes)
			a.J(NE, yes)
		}
	case bytecode.OpLessThan:
		a.Ucomisd(y, x) // A: y > x
		if jump {
			a.J(A, yes)
		} else {
			a.J(BE, yes)
		}
	case bytecode.OpLessOrEqual:
		a.Ucomisd(y, x) // AE: y >= x
		if jump {
			a.J(AE, yes)
		} else {
			a.J(B, yes)
		}
	}
	a.Jmp(no)
}

// floatEqualsInt branches to eq when the float in X0 equals the integer
// in r, which it does when it converts to that integer and back to
// itself, and to ne otherwise. It uses rT and X1.
func (c *amd64Compiler) floatEqualsInt(r Reg, eq, ne Label) {
	a := &c.a
	a.Cvttsd2si(rT, 0) // -2^63 for NaN and out of range, which convert back to another float
	c.toFloat(1, rT)
	a.Ucomisd(0, 1)
	a.J(P, ne)
	a.J(NE, ne)
	a.Cmp(rT, r)
	a.J(E, eq)
	a.Jmp(ne)
}

// equal compiles EQ, whose operands may be any values. Floats compare as
// floats, and a float and an integer exactly (floatEqualsInt). Other
// values, integers among them, are equal when both words are, and when they are
// not can be equal only as strings of the same length, whose bytes it
// compares up to maxInlineCompare, or through __eq, which only two tables or two userdata try: it exits
// for those, unless both tables' metatables are known to lack __eq.
func (c *amd64Compiler) equal(ip int, i bytecode.Instruction) {
	a := &c.a
	target, ok := c.jumpAfter(ip)
	b, okB := c.rk(i.B())
	cc, okC := c.rk(i.C())
	if !ok || !okB || !okC {
		c.exitAlways(ip)
		return
	}
	// The JMP runs when the comparison's result equals A.
	jump := i.A() != 0
	yes, no := c.pcs[target], c.pcs[ip+2]
	back := a.NewLabel()
	if target <= ip {
		yes = back
	}
	eq, ne := yes, no
	if !jump {
		eq, ne = no, yes
	}
	if kb, kc := c.isNumberConstant(i.B()), c.isNumberConstant(i.C()); kb || kc {
		// Against a float, only another number can be equal.
		if o, k := cc, b; !kb || !kc {
			if !kb {
				o, k = b, cc
			}
			floats, integer := a.NewLabel(), a.NewLabel()
			a.Load(rP, o.base, o.off+offP)
			a.Cmp(rP, rNumber)
			a.J(E, floats)
			c.branchNumber(rP, integer)
			a.Jmp(ne)
			a.Bind(integer)
			a.LoadSD(0, k.base, k.off+offN)
			a.Load(rIdx, o.base, o.off+offN)
			c.floatEqualsInt(rIdx, eq, ne)
			a.Bind(floats)
		}
		a.LoadSD(0, b.base, b.off+offN)
		a.LoadSD(1, cc.base, cc.off+offN)
		c.compare(bytecode.OpEqual, jump, 0, 1, yes, no)
		if target <= ip {
			a.Bind(back)
			c.backEdge(target)
		}
		return
	}
	exit := c.exit(ip)
	notNumber, differ := a.NewLabel(), a.NewLabel()
	a.Load(rP, b.base, b.off+offP)
	a.Load(rTmp, cc.base, cc.off+offP)
	a.Cmp(rP, rNumber)
	a.J(NE, notNumber)
	floats, integer := a.NewLabel(), a.NewLabel()
	a.Cmp(rTmp, rNumber)
	a.J(E, floats)
	c.branchNumber(rTmp, integer)
	a.Jmp(ne)
	a.Bind(integer) // a float and an integer
	a.LoadSD(0, b.base, b.off+offN)
	a.Load(rIdx, cc.base, cc.off+offN)
	c.floatEqualsInt(rIdx, eq, ne)
	a.Bind(floats)
	a.LoadSD(0, b.base, b.off+offN)
	a.LoadSD(1, cc.base, cc.off+offN)
	c.compare(bytecode.OpEqual, jump, 0, 1, yes, no)
	a.Bind(notNumber)
	a.Load(rN, b.base, b.off+offN)
	a.Load(rTmp2, cc.base, cc.off+offN)
	a.Cmp(rP, rTmp)
	a.J(NE, differ)
	a.Cmp(rN, rTmp2)
	a.J(E, eq)
	a.Jmp(ne) // one address, other bits: true and false, integers, or strings' lengths
	a.Bind(differ)
	// Other first words. An integer and a float may be equal; a number
	// and anything else are not.
	notInteger := a.NewLabel()
	a.Mov(rIdx, rP)
	a.Sub(rIdx, rNumber)
	a.CmpImm(rIdx, 1)
	a.J(NE, notInteger)
	a.Cmp(rTmp, rNumber)
	a.J(NE, ne)
	a.MovqToX(0, rTmp2)
	c.floatEqualsInt(rN, eq, ne)
	a.Bind(notInteger)
	a.Mov(rIdx, rTmp)
	a.Sub(rIdx, rNumber)
	a.CmpImm(rIdx, 1)
	a.J(BE, ne)
	a.Mov(rIdx, rN)
	a.Shr(rIdx, kindShift)
	a.Mov(rT, rTmp2)
	a.Shr(rT, kindShift)
	a.Cmp(rIdx, rT)
	a.J(NE, ne) // different kinds
	strs, tables := a.NewLabel(), a.NewLabel()
	a.CmpImm(rIdx, int32(vkString))
	a.J(E, strs)
	a.CmpImm(rIdx, int32(vkTable))
	a.J(E, tables)
	a.CmpImm(rIdx, int32(vkUserData))
	a.J(E, exit)
	a.Jmp(ne) // other objects are equal only at one address
	// Strings: compare the bytes of short ones; Go compares long ones.
	a.Bind(strs)
	a.Cmp(rN, rTmp2)
	a.J(NE, ne) // lengths differ
	a.Mov(rIdx, rN)
	a.MovImm(rT, tagOf(vkString))
	a.Sub(rIdx, rT) // the length, at least 1: the empty string has one address
	a.CmpImm(rIdx, maxInlineCompare)
	a.J(A, exit)
	loop := a.NewLabel()
	a.Bind(loop)
	a.Load8(rT, rP, 0)
	a.Load8(rT2, rTmp, 0)
	a.Cmp(rT, rT2)
	a.J(NE, ne)
	a.AddImm(rP, 1)
	a.AddImm(rTmp, 1)
	a.SubImm(rIdx, 1)
	a.J(NE, loop)
	a.Jmp(eq)
	a.Bind(tables) // either table's __eq, as Lua 5.4 on
	second := a.NewLabel()
	a.Load(rT, rP, offTMeta)
	a.Test(rT, rT)
	a.J(E, second)
	a.Load8(rIdx, rT, offTFlags)
	a.Bt(rIdx, uint8(tmEq))
	a.J(AE, exit) // the metatable may have __eq
	a.Bind(second)
	a.Load(rT, rTmp, offTMeta)
	a.Test(rT, rT)
	a.J(E, ne)
	a.Load8(rIdx, rT, offTFlags)
	a.Bt(rIdx, uint8(tmEq))
	a.J(B, ne) // the metatable has no __eq
	a.Jmp(exit)
	if target <= ip {
		a.Bind(back)
		c.backEdge(target)
	}
}

// length compiles LEN of a string, whose length is its second word less
// the kind, as an integer; anything else exits, and Go measures a table.
func (c *amd64Compiler) length(ip int, i bytecode.Instruction) {
	a := &c.a
	exit := c.exit(ip)
	src, dst := reg(i.B()), reg(i.A())
	a.Load(rP, src.base, src.off+offP)
	c.branchNumber(rP, exit) // a number whose bits match the tag
	a.Load(rN, src.base, src.off+offN)
	a.Mov(rTmp, rN)
	a.Shr(rTmp, kindShift)
	done, notString, table := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpImm(rTmp, int32(vkString))
	a.J(NE, notString)
	a.MovImm(rTmp, tagOf(vkString))
	a.Sub(rN, rTmp)
	a.Jmp(done)
	a.Bind(notString)
	a.CmpImm(rTmp, int32(vkTable))
	a.J(E, table)
	a.CmpImm(rTmp, int32(vkUserData))
	a.J(NE, exit)
	a.Load(rTmp, rP, offUDBuf) // a buffer's length; other userdata's is Go's
	a.Test(rTmp, rTmp)
	a.J(E, exit)
	a.Load(rN, rTmp, offBufLen)
	a.Jmp(done)
	a.Bind(table)
	c.tableLength(exit)
	a.Bind(done)
	c.guardStore(dst, noReg, ip)
	c.storeInteger(dst, rN)
}

// tableLength computes the length of the table in rP into rN, as
// table.length does: the array part's length when its last element is
// not nil and there is no hash part, and a border found by binary search
// when it is nil. It exits for a table whose metatable may have __len,
// or whose border may be in its hash part. It uses rTmp, rIdx, rT and
// rT2.
func (c *amd64Compiler) tableLength(exit Label) {
	a := &c.a
	noMeta := a.NewLabel()
	a.Load(rT, rP, offTMeta)
	a.Test(rT, rT)
	a.J(E, noMeta)
	a.Load8(rIdx, rT, offTFlags)
	a.Bt(rIdx, uint8(tmLen))
	a.J(AE, exit) // the metatable may have __len
	a.Bind(noMeta)
	done, full, search := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(rN, rP, offTArray+offSliceLen) // j
	a.Test(rN, rN)
	a.J(E, full)
	a.Load(rTmp, rP, offTArray)
	a.Mov(rIdx, rN)
	c.elementP(rIdx)
	a.J(E, search) // array[j-1] is nil
	a.Bind(full)
	a.Load(rT, rP, offTHash)
	a.Test(rT, rT)
	a.J(NE, exit)
	a.Jmp(done)
	a.Bind(search) // for a border between i, in rT2, and j
	loop, found := a.NewLabel(), a.NewLabel()
	a.MovImm(rT2, 0)
	a.Bind(loop)
	a.Mov(rIdx, rN)
	a.Sub(rIdx, rT2)
	a.CmpImm(rIdx, 1)
	a.J(BE, found)
	a.Mov(rIdx, rN)
	a.Add(rIdx, rT2)
	a.Shr(rIdx, 1) // m
	a.Mov(rT, rIdx)
	c.elementP(rT)
	nonNil := a.NewLabel()
	a.J(NE, nonNil)
	a.Mov(rN, rIdx)
	a.Jmp(loop)
	a.Bind(nonNil)
	a.Mov(rT2, rIdx)
	a.Jmp(loop)
	a.Bind(found)
	a.Mov(rN, rT2)
	a.Bind(done)
}

// elementP loads the first word of array element r-1, from the array at
// rTmp, into r and tests it: E when the element is nil.
func (c *amd64Compiler) elementP(r Reg) {
	a := &c.a
	a.SubImm(r, 1)
	a.Shl(r, 4)
	a.Add(r, rTmp)
	a.Load(r, r, offP)
	a.Test(r, r)
}

// signMask loads the sign bit into x.
func (c *amd64Compiler) signMask(x XReg) {
	c.a.MovImm(rTmp, 1<<63)
	c.a.MovqToX(x, rTmp)
}

// arith computes x0 = x0 op x1 for ADD to MOD. It reports false for MOD
// without ROUNDSD.
func (c *amd64Compiler) arith(op bytecode.OpCode, d, x, y XReg) bool {
	a := &c.a
	if d != x {
		a.MovSD(d, x)
	}
	switch op {
	case bytecode.OpAdd:
		a.AddSD(d, y)
	case bytecode.OpSub:
		a.SubSD(d, y)
	case bytecode.OpMul:
		a.MulSD(d, y)
	case bytecode.OpDiv:
		a.DivSD(d, y)
	}
	return true
}

// instruction compiles the instruction at ip and returns how many extra
// code words it consumed.
func (c *amd64Compiler) instruction(ip int) int {
	a := &c.a
	orig := c.p.Code[ip]
	switch op := orig.OpCode(); op {
	case bytecode.OpMove:
		c.copyValue(reg(orig.A()), reg(orig.B()), ip)
	case bytecode.OpLoadConstant:
		k, ok := c.constant(orig.Bx())
		if !ok {
			c.exitAlways(ip)
			break
		}
		c.copyValue(reg(orig.A()), k, ip)
	case bytecode.OpLoadBool:
		dst := reg(orig.A())
		c.guardStore(dst, noReg, ip)
		bits := tagOf(vkBool)
		if orig.B() != 0 {
			bits |= 1
		}
		c.storeBool(dst, bits)
		if orig.C() != 0 {
			a.Jmp(c.pcs[ip+2])
		}
	case bytecode.OpLoadNil:
		for r := orig.A(); r <= orig.A()+orig.B(); r++ {
			c.guardStore(reg(r), noReg, ip)
		}
		for r := orig.A(); r <= orig.A()+orig.B(); r++ {
			a.StoreZero(rFrame, reg(r).off+offP)
			a.StoreZero(rFrame, reg(r).off+offN)
		}
	case bytecode.OpGetUpValue:
		c.upValueAddr(orig.B())
		c.copyValue(reg(orig.A()), operand{rAddr, 0}, ip)
	case bytecode.OpSetUpValue:
		c.upValueAddr(orig.B())
		c.copyValue(operand{rAddr, 0}, reg(orig.A()), ip)
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
		// Two integers give an integer, wrapping around, except for /; a
		// float operand makes both floats. % is left to Go, which computes
		// it as Lua 5.4 does, with fmod.
		b, kb, okB := c.rkArith(orig.B())
		cc, kc, okC := c.rkArith(orig.C())
		if !okB || !okC {
			c.exitAlways(ip)
			break
		}
		dst := reg(orig.A())
		c.guardStore(dst, noReg, ip)
		floats, done := a.NewLabel(), a.NewLabel()
		if op != bytecode.OpDiv && kb != kindFloat && kc != kindFloat {
			c.branchUnlessInteger(b, kb, floats)
			c.branchUnlessInteger(cc, kc, floats)
			a.Load(rN, b.base, b.off+offN)
			a.Load(rP, cc.base, cc.off+offN)
			switch op {
			case bytecode.OpAdd:
				a.Add(rN, rP)
			case bytecode.OpSub:
				a.Sub(rN, rP)
			case bytecode.OpMul:
				a.Imul(rN, rP)
			}
			c.storeInteger(dst, rN)
			a.Jmp(done)
		}
		a.Bind(floats)
		c.loadFloat(0, b, kb, false, ip)
		c.loadFloat(1, cc, kc, false, ip)
		c.arith(op, 0, 0, 1)
		c.storeNumber(dst, 0)
		a.Bind(done)
	case bytecode.OpMod, bytecode.OpIDiv:
		c.divide(ip, op, orig)
	case bytecode.OpBitwise:
		c.bitwise(ip, orig, bytecode.ArithOp(c.p.Code[ip+1].Ax()))
		return 1 // the operator's word is not an instruction
	case bytecode.OpUnaryMinus:
		src, dst := reg(orig.B()), reg(orig.A())
		c.guardStore(dst, noReg, ip)
		floats, done := a.NewLabel(), a.NewLabel()
		c.branchUnlessInteger(src, kindAny, floats)
		a.Load(rN, src.base, src.off+offN)
		a.Neg(rN)
		c.storeInteger(dst, rN)
		a.Jmp(done)
		a.Bind(floats)
		a.Test(rTmp, rTmp) // p - numberPtr(): 0 for a float
		a.J(NE, c.exit(ip))
		a.LoadSD(0, src.base, src.off+offN)
		c.signMask(1)
		a.XorPD(0, 1)
		c.storeNumber(dst, 0)
		a.Bind(done)
	case bytecode.OpNot:
		src, dst := reg(orig.B()), reg(orig.A())
		c.guardStore(dst, noReg, ip)
		falsy, done := a.NewLabel(), a.NewLabel()
		c.branchFalsy(src, falsy)
		c.storeBool(dst, tagOf(vkBool))
		a.Jmp(done)
		a.Bind(falsy)
		c.storeBool(dst, tagOf(vkBool)|1)
		a.Bind(done)
	case bytecode.OpJump:
		if orig.A() != 0 {
			c.closeJump(ip, orig)
			break
		}
		c.jumpTo(ip, ip+1+orig.SBx())
	case bytecode.OpTForCall:
		c.tforCall(ip, orig)
	case bytecode.OpTForLoop: // after TFORCALL: on while the control variable is not nil
		r := reg(orig.A() + 3)
		done := a.NewLabel()
		a.Load(rTmp, r.base, r.off+offP)
		a.Test(rTmp, rTmp)
		a.J(E, done)
		c.backEdge(ip + 1 + orig.SBx())
		a.Bind(done)
	case bytecode.OpEqual:
		c.equal(ip, orig)
	case bytecode.OpLessThan, bytecode.OpLessOrEqual:
		target, ok := c.jumpAfter(ip)
		b, kb, okB := c.rkArith(orig.B())
		cc, kc, okC := c.rkArith(orig.C())
		if !ok || !okB || !okC {
			c.exitAlways(ip)
			break
		}
		// The JMP runs when the comparison's result equals A.
		yes := c.pcs[target]
		back := a.NewLabel()
		if target <= ip {
			yes = back
		}
		// Two integers compare as integers; otherwise both as floats,
		// which is exact while an integer converts exactly, and Go
		// compares the rest.
		floats := a.NewLabel()
		if kb != kindFloat && kc != kindFloat {
			c.branchUnlessInteger(b, kb, floats)
			c.branchUnlessInteger(cc, kc, floats)
			a.Load(rP, b.base, b.off+offN)
			a.Load(rN, cc.base, cc.off+offN)
			a.Cmp(rP, rN)
			when := L
			if op == bytecode.OpLessOrEqual {
				when = LE
			}
			if orig.A() == 0 {
				when ^= 1 // GE or G
			}
			a.J(when, yes)
			a.Jmp(c.pcs[ip+2])
		}
		a.Bind(floats)
		c.loadFloat(0, b, kb, true, ip)
		c.loadFloat(1, cc, kc, true, ip)
		c.compare(op, orig.A() != 0, 0, 1, yes, c.pcs[ip+2])
		if target <= ip {
			a.Bind(back)
			c.backEdge(target)
		}
	case bytecode.OpTest:
		target, ok := c.jumpAfter(ip)
		if !ok {
			c.exitAlways(ip)
			break
		}
		// A backward JMP, a repeat-until's, spends budget on the way.
		jump, skip, back := c.pcs[target], c.pcs[ip+2], a.NewLabel()
		if target <= ip {
			jump = back
		}
		if orig.C() == 0 {
			c.branchFalsy(reg(orig.A()), jump)
			a.Jmp(skip)
		} else {
			c.branchFalsy(reg(orig.A()), skip)
			a.Jmp(jump)
		}
		if target <= ip {
			a.Bind(back)
			c.backEdge(target)
		}
	case bytecode.OpTestSet:
		target, ok := c.jumpAfter(ip)
		if !ok {
			c.exitAlways(ip)
			break
		}
		src, dst := reg(orig.B()), reg(orig.A())
		assign, skip := a.NewLabel(), c.pcs[ip+2]
		if orig.C() == 0 {
			c.branchFalsy(src, assign)
			a.Jmp(skip)
		} else {
			c.branchFalsy(src, skip)
		}
		a.Bind(assign)
		c.copyValue(dst, src, ip)
		c.jumpTo(ip, target)
	case bytecode.OpForPrep:
		// As forPrep runs it: the body next with the control variable set,
		// or past the FORLOOP when the loop runs no times. An integer loop
		// with an integer limit keeps the count of iterations left in the
		// limit's register; a float loop needs three floats. A zero step
		// and anything else exit.
		init, limit, step, ext := reg(orig.A()), reg(orig.A()+1), reg(orig.A()+2), reg(orig.A()+3)
		c.guardStore(limit, noReg, ip)
		c.guardStore(ext, noReg, ip)
		floatLoop, skip := a.NewLabel(), a.NewLabel()
		c.branchUnlessInteger(init, kindAny, floatLoop)
		c.branchUnlessInteger(step, kindAny, floatLoop)
		c.branchUnlessInteger(limit, kindAny, c.exit(ip)) // Go clips a float limit
		a.Load(rP, init.base, init.off+offN)
		a.Load(rN, limit.base, limit.off+offN)
		a.Load(rIdx, step.base, step.off+offN)
		a.Test(rIdx, rIdx)
		a.J(E, c.exit(ip)) // 'for' step is zero
		down, count := a.NewLabel(), a.NewLabel()
		a.J(S, down)
		a.Cmp(rP, rN)
		a.J(G, skip)
		a.Mov(rTmp, rN) // (limit - init) / step, unsigned, in RAX
		a.Sub(rTmp, rP)
		a.MovImm(DX, 0)
		a.Div(rIdx)
		a.Jmp(count)
		a.Bind(down)
		a.Cmp(rP, rN)
		a.J(L, skip)
		a.Mov(rTmp, rP) // (init - limit) / -step, unsigned
		a.Sub(rTmp, rN)
		a.AddImm(rIdx, 1)
		a.Neg(rIdx) // -(step+1) + 1, which cannot overflow
		a.AddImm(rIdx, 1)
		a.MovImm(DX, 0)
		a.Div(rIdx)
		a.Bind(count)
		c.storeInteger(limit, rTmp)
		c.storeInteger(ext, rP)
		a.Jmp(c.pcs[ip+1])
		a.Bind(floatLoop)
		c.guardNumber(init, ip)
		c.guardNumber(limit, ip)
		c.guardNumber(step, ip)
		a.LoadSD(0, init.base, init.off+offN)
		a.LoadSD(1, limit.base, limit.off+offN)
		a.LoadSD(2, step.base, step.off+offN)
		a.XorPD(3, 3)
		notPositive, positive, body := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.Ucomisd(2, 3)
		a.J(P, notPositive) // a NaN step
		a.J(E, c.exit(ip))  // 'for' step is zero
		a.J(A, positive)
		a.Bind(notPositive) // no times if init < limit
		a.Ucomisd(1, 0)
		a.J(P, body)
		a.J(A, skip)
		a.Jmp(body)
		a.Bind(positive) // no times if limit < init
		a.Ucomisd(0, 1)
		a.J(P, body)
		a.J(A, skip)
		a.Bind(body)
		c.storeNumber(ext, 0)
		a.Jmp(c.pcs[ip+1])
		a.Bind(skip)
		a.Jmp(c.pcs[ip+2+orig.SBx()])
	case bytecode.OpForLoop:
		// The loop's own registers hold what FORPREP put there, which Lua
		// code cannot change: an integer step means an integer loop, which
		// counts down in the limit's register.
		idx, limit, step, ext := reg(orig.A()), reg(orig.A()+1), reg(orig.A()+2), reg(orig.A()+3)
		c.guardStore(ext, noReg, ip)
		floatLoop := a.NewLabel()
		c.branchUnlessInteger(step, kindAny, floatLoop)
		a.Load(rN, limit.base, limit.off+offN)
		a.Test(rN, rN)
		a.J(E, c.pcs[ip+1]) // no iterations left
		a.SubImm(rN, 1)
		a.Load(rP, idx.base, idx.off+offN)
		a.Load(rIdx, step.base, step.off+offN)
		a.Add(rP, rIdx)
		a.Store(limit.base, limit.off+offN, rN)
		a.Store(idx.base, idx.off+offN, rP)
		c.storeInteger(ext, rP)
		c.backEdge(ip + 1 + orig.SBx())
		a.Bind(floatLoop)
		c.guardNumber(step, ip)
		take := a.NewLabel()
		a.LoadSD(0, idx.base, idx.off+offN)
		a.LoadSD(1, limit.base, limit.off+offN)
		a.LoadSD(2, step.base, step.off+offN)
		c.forStep(0, 1, 2, take, c.pcs[ip+1])
		a.Bind(take)
		a.StoreSD(idx.base, idx.off+offN, 0)
		c.storeNumber(ext, 0)
		c.backEdge(ip + 1 + orig.SBx())
	case bytecode.OpGetTable, bytecode.OpGetTableUp, bytecode.OpSelf, bytecode.OpSetTable, bytecode.OpSetTableUp:
		c.tableAccess(ip, c.code[ip])
	case bytecode.OpCall:
		c.call(ip, orig)
	case bytecode.OpTailCall:
		c.tailCallLua(ip, orig)
	case bytecode.OpReturn:
		if hasTBC(c.p) { // Go closes the frame's to-be-closed variables first
			a.Load(rTmp, rCtx, offCtxTBC)
			a.Cmp(rTmp, rFrame)
			a.J(AE, c.exit(ip))
		}
		c.returnLua(ip, orig)
	case bytecode.OpTBC: // a nil closing value, as pairs gives, closes nothing
		r := reg(orig.A())
		if orig.B() == 0 {
			a.Load(rTmp, r.base, r.off+offP)
			a.Test(rTmp, rTmp)
			a.J(NE, c.exit(ip))
			break
		}
		// A generic for's: see the arm64 compiler.
		closing := reg(orig.A() + 1)
		a.CmpMem(rCtx, offBarrier, 0)
		a.J(NE, c.exit(ip))
		a.Load(rTmp, closing.base, closing.off+offP)
		a.Test(rTmp, rTmp)
		a.J(NE, c.exit(ip))
		c.load(r)
		c.store(closing)
		a.StoreZero(r.base, r.off+offP)
		a.StoreZero(r.base, r.off+offN)
	case bytecode.OpLength:
		c.length(ip, orig)
	case bytecode.OpSetList:
		if orig.C() == 0 {
			c.exitAlways(ip)
			return 1 // the extra-argument word is not an instruction
		}
		c.setList(ip, orig)
	case bytecode.OpLoadConstantEx:
		c.exitAlways(ip)
		return 1 // the extra-argument word is not an instruction
	default:
		c.exitAlways(ip)
	}
	return 0
}

// forStep is FORLOOP's test: with next = index + step in x0 computed here
// from idx, limit and step, it branches to take when the loop goes on and
// to end otherwise. X3 is clobbered.
func (c *amd64Compiler) forStep(idx, limit, step XReg, take, end Label) {
	a := &c.a
	positive, notPositive := a.NewLabel(), a.NewLabel()
	if idx != 0 {
		a.MovSD(0, idx)
	}
	a.AddSD(0, step)
	a.XorPD(3, 3)
	a.Ucomisd(step, 3)
	a.J(P, end) // NaN step: the loop ends
	a.J(A, positive)
	a.Bind(notPositive) // step <= 0: go on while limit <= next
	a.Ucomisd(0, limit)
	a.J(P, end)
	a.J(AE, take)
	a.Jmp(end)
	a.Bind(positive) // go on while next <= limit
	a.Ucomisd(limit, 0)
	a.J(P, end)
	a.J(AE, take)
	a.Jmp(end)
}

// upValueAddr puts the address of upvalue n's value in rAddr.
func (c *amd64Compiler) upValueAddr(n int) {
	a := &c.a
	closed, done := a.NewLabel(), a.NewLabel()
	a.Load(rTmp, rCtx, offUpValues)
	a.Load(rTmp, rTmp, uint32(n)*8) // *upValue
	a.Load(rTmp2, rTmp, offUVState)
	a.Test(rTmp2, rTmp2)
	a.J(E, closed)
	a.Load(rTmp2, rTmp2, offStack)  // &state.stack[0]
	a.Load(rAddr, rTmp, offUVIndex) // index
	a.Shl(rAddr, 4)
	a.Add(rAddr, rTmp2)
	a.Jmp(done)
	a.Bind(closed)
	a.Mov(rAddr, rTmp)
	a.AddImm(rAddr, int32(offUVClosed))
	a.Bind(done)
}
