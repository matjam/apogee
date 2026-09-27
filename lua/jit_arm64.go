//go:build (darwin || linux) && arm64

package lua

import (
	"errors"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

const jitSupported = true

// Registers the generated code keeps for its whole run.
const (
	rCtx     Reg = 0  // *jitContext
	rFrame   Reg = 1  // &frame[0]
	rConst   Reg = 2  // &constants[0]
	rNumber  Reg = 3  // numberPtr(), the p of every number
	rBudget  Reg = 4  // back-edges left
	rBarrier Reg = 5  // nonzero while the GC write barrier is on
	rBool    Reg = 6  // boolPtr(), the p of every boolean
	rUpVals  Reg = 7  // &closure.upValues[0]
	rInteger Reg = 8  // integerPtr(), the p of every integer
	rP       Reg = 12 // a value's p, while it is copied
	rN       Reg = 13 // a value's second word, while it is copied
	rTmp     Reg = 9
	rTmp2    Reg = 10
	rExitPC  Reg = 11
	rAddr    Reg = 14
	rT       Reg = 15 // a table, while it is indexed
	rT2      Reg = 16 // a second table: a metatable or its __index
	rCache   Reg = 17 // the instruction's *fieldCache
	rIdx     Reg = 19 // a slot or array index
	rLen     Reg = 20
	rSlot    Reg = 21 // the address of a table slot
)

// maxOffset bounds the offsets LDR and STR reach.
const maxOffset = 32768

// arm64Compiler translates a prototype into arm64 code.
//
// Each instruction's code checks everything it needs before it writes
// anything, so a failed check can exit to the interpreter at that
// instruction, which then runs it from the start.
type arm64Compiler struct {
	a       Asm
	p       *prototype
	g       *globalState // the state p runs in, whose string metatable SELF reads
	cl      *luaClosure  // the closure being compiled, whose upvalues kernels speculate on
	code    []bytecode.Instruction
	pcs     []Label // start of each pc's code
	exits   []Label // exit to the interpreter at each pc, created on demand
	budget  []Label // budget exits by back-edge target pc, created on demand
	goCall  []Label // exits at a CALL of a Go function, created on demand
	plainGo []Label // goCallee's code for a Go function no number function (plainGoCall)
	numCall []Label // exits at a CALL of a number function, created on demand
	notLua  []Label // a CALL's out-of-line code for callees other than Lua closures
	strSelf []Label // a SELF's out-of-line code for receivers other than tables
	// outOfLine emit code instructions branch to rarely, after the
	// function's: buffers in table instructions, and kernels' side exits.
	outOfLine  []func()
	always     []bool // instructions compiled as an unconditional exit
	kernelExit Label  // in a kernel's intrinsic call, its side exit; otherwise -1
}

// intrinsicExit is where an intrinsic's code at ip exits: the
// instruction's exit, or in a kernel the side exit its call set.
func (c *arm64Compiler) intrinsicExit(ip int) Label {
	if c.kernelExit >= 0 {
		return c.kernelExit
	}
	return c.exit(ip)
}

func compileJIT(p *prototype, g *globalState, cl *luaClosure) (code []byte, offsets []int32, entries []int, kernels int) {
	if len(p.Code) > 1<<16 || uint32(p.MaxStackSize+3)*valueSize >= maxOffset {
		return nil, nil, nil, 0
	}
	code, offsets, entries, kernels, err := compileARM64(p, g, cl, false)
	if errors.Is(err, ErrTestRange) {
		// Over 32 KB of code, which a function of a hundred instructions
		// can be: test branches take two instructions to reach.
		code, offsets, entries, kernels, err = compileARM64(p, g, cl, true)
	}
	if err != nil {
		return nil, nil, nil, 0
	}
	return code, offsets, entries, kernels
}

func compileARM64(p *prototype, g *globalState, cl *luaClosure, longTests bool) (code []byte, offsets []int32, entries []int, kernels int, err error) {
	c := &arm64Compiler{p: p, g: g, cl: cl, code: p.jitOrig, kernelExit: -1}
	c.a.LongTests = longTests
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
	code, err = c.a.Code()
	if err != nil {
		return nil, nil, nil, 0, err
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
	return code, offsets, jitEntries(c.p, exits, c.always), kernels, nil
}

// prologue loads the fixed registers and branches to ctx.target.
func (c *arm64Compiler) prologue() {
	a := &c.a
	a.Ldr(rFrame, rCtx, offFrame)
	a.Ldr(rConst, rCtx, offConstants)
	a.Ldr(rBudget, rCtx, offBudget)
	a.Ldr(rBarrier, rCtx, offBarrier)
	a.Ldr(rUpVals, rCtx, offUpValues)
	a.MovImm(rNumber, uint64(uintptr(numberPtr())))
	a.AddImm(rInteger, rNumber, 1)
	a.MovImm(rBool, uint64(uintptr(boolPtr())))
	a.Ldr(rTmp, rCtx, offTarget)
	a.Br(rTmp)
}

// stubs emits the exits that instructions branch to.
func (c *arm64Compiler) stubs() {
	a := &c.a
	common := a.NewLabel()
	budget := a.NewLabel()
	for ip, l := range c.notLua {
		if l >= 0 {
			a.Bind(l)
			c.goCallee(ip, c.code[ip])
			a.B(c.exit(ip))
		}
	}
	for ip, l := range c.strSelf {
		if l >= 0 {
			a.Bind(l)
			c.selfString(ip, c.code[ip])
			a.B(c.pcs[ip+1])
		}
	}
	for _, emit := range c.outOfLine {
		emit()
	}
	for ip, l := range c.exits {
		if l >= 0 {
			a.Bind(l)
			a.MovImm(rExitPC, uint64(ip))
			a.B(common)
		}
	}
	for ip, l := range c.budget {
		if l >= 0 {
			a.Bind(l)
			a.MovImm(rExitPC, uint64(ip))
			a.B(budget)
		}
	}
	a.Bind(common)
	a.Str(rExitPC, rCtx, offExitPC)
	a.MovImm(0, jitExitInstruction)
	a.Ret()
	a.Bind(budget)
	a.Str(rExitPC, rCtx, offExitPC)
	a.MovImm(0, jitExitBudget)
	a.Ret()
	for _, calls := range []struct {
		labels []Label
		reason uint64
	}{{c.goCall, jitExitCallGo}, {c.numCall, jitExitCallNumber}} {
		tail := a.NewLabel()
		for ip, l := range calls.labels {
			if l >= 0 {
				a.Bind(l)
				fn := reg(c.code[ip].A())
				a.Ldr(rTmp, fn.base, fn.off+offP)
				a.Str(rTmp, rCtx, offCallee)
				a.MovImm(rExitPC, uint64(ip))
				a.B(tail)
			}
		}
		a.Bind(tail)
		a.Str(rFrame, rCtx, offFrame) // compiled calls may have moved it
		a.Str(rExitPC, rCtx, offExitPC)
		a.MovImm(0, calls.reason)
		a.Ret()
	}
}

// numCallExit returns the label that exits at the CALL at ip for runJIT to
// call the number function in its register.
func (c *arm64Compiler) numCallExit(ip int) Label {
	if c.numCall[ip] < 0 {
		c.numCall[ip] = c.a.NewLabel()
	}
	return c.numCall[ip]
}

// goCallExit returns the label that exits at the CALL at ip for runJIT to
// call the Go function in its register.
func (c *arm64Compiler) goCallExit(ip int) Label {
	if c.goCall[ip] < 0 {
		c.goCall[ip] = c.a.NewLabel()
	}
	return c.goCall[ip]
}

// exit returns the label that resumes the interpreter at ip.
func (c *arm64Compiler) exit(ip int) Label {
	if c.exits[ip] < 0 {
		c.exits[ip] = c.a.NewLabel()
	}
	return c.exits[ip]
}

// exitAlways compiles the instruction at ip as an exit: the interpreter,
// or runJIT for calls and returns, always runs it.
func (c *arm64Compiler) exitAlways(ip int) {
	c.always[ip] = true
	c.a.B(c.exit(ip))
}

// operand locates a value in memory: its base register and byte offset.
type operand struct {
	base Reg
	off  uint32
}

func reg(r int) operand { return operand{rFrame, uint32(r) * valueSize} }

// constant returns the operand for constant k, and false when it is out of
// reach of an immediate offset.
func (c *arm64Compiler) constant(k int) (operand, bool) {
	if uint32(k)*valueSize >= maxOffset {
		return operand{}, false
	}
	return operand{rConst, uint32(k) * valueSize}, true
}

// isNumberConstant reports whether an RK field is a number constant.
func (c *arm64Compiler) isNumberConstant(field int) bool {
	return bytecode.IsConstant(field) && c.p.Constants[bytecode.ConstantIndex(field)].isFloat()
}

// length compiles LEN of a string, whose length is its second word less
// the kind, as an integer; anything else exits, and Go measures a table.
func (c *arm64Compiler) length(ip int, i bytecode.Instruction) {
	a := &c.a
	exit := c.exit(ip)
	src, dst := reg(i.B()), reg(i.A())
	a.Ldr(rP, src.base, src.off+offP)
	c.branchNumber(rP, exit) // a number whose bits match the tag
	a.Ldr(rN, src.base, src.off+offN)
	a.Lsr(rTmp, rN, kindShift)
	done, notString, table := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpImm(rTmp, uint32(vkString))
	a.BCond(NE, notString)
	a.MovImm(rTmp, tagOf(vkString))
	a.Sub(rN, rN, rTmp)
	a.B(done)
	a.Bind(notString)
	a.CmpImm(rTmp, uint32(vkTable))
	a.BCond(EQ, table)
	a.CmpImm(rTmp, uint32(vkUserData))
	a.BCond(NE, exit)
	a.Ldr(rTmp, rP, offUDBuf) // a buffer's length; other userdata's is Go's
	a.Cbz(rTmp, exit)
	a.Ldr(rN, rTmp, offBufLen)
	a.B(done)
	a.Bind(table)
	c.tableLength(exit)
	a.Bind(done)
	c.guardStore(dst, noReg, ip)
	a.Str(rN, dst.base, dst.off+offN)
	a.Str(rInteger, dst.base, dst.off+offP)
}

// tableLength computes the length of the table in rP into rN, as
// table.length does: the array part's length when its last element is
// not nil and there is no hash part, and a border found by binary search
// when it is nil. It exits for a table whose metatable may have __len,
// or whose border may be in its hash part. It uses rTmp, rIdx, rT and
// rT2.
func (c *arm64Compiler) tableLength(exit Label) {
	a := &c.a
	noMeta := a.NewLabel()
	a.Ldr(rT, rP, offTMeta)
	a.Cbz(rT, noMeta)
	a.Ldrb(rIdx, rT, offTFlags)
	a.Tbz(rIdx, uint32(tmLen), exit) // the metatable may have __len
	a.Bind(noMeta)
	done, full, search := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Ldr(rN, rP, offTArray+offSliceLen) // j
	a.Cbz(rN, full)
	a.Ldr(rTmp, rP, offTArray)
	c.elementP(rIdx, rN)
	a.Cbz(rIdx, search) // array[j-1] is nil
	a.Bind(full)
	a.Ldr(rT, rP, offTHash)
	a.Cbnz(rT, exit)
	a.B(done)
	a.Bind(search) // for a border between i, in rT2, and j
	loop, found := a.NewLabel(), a.NewLabel()
	a.MovImm(rT2, 0)
	a.Bind(loop)
	a.Sub(rIdx, rN, rT2)
	a.CmpImm(rIdx, 1)
	a.BCond(LS, found)
	a.Add(rIdx, rN, rT2)
	a.Lsr(rIdx, rIdx, 1) // m
	c.elementP(rT, rIdx)
	nonNil := a.NewLabel()
	a.Cbnz(rT, nonNil)
	a.Mov(rN, rIdx)
	a.B(loop)
	a.Bind(nonNil)
	a.Mov(rT2, rIdx)
	a.B(loop)
	a.Bind(found)
	a.Mov(rN, rT2)
	a.Bind(done)
}

// elementP loads the first word of array element n-1, from the array at
// rTmp, into d: zero when the element is nil.
func (c *arm64Compiler) elementP(d, n Reg) {
	a := &c.a
	a.SubImm(d, n, 1)
	a.AddShifted(d, rTmp, d, 4)
	a.Ldr(d, d, offP)
}

// rk returns the operand for an RK field.
func (c *arm64Compiler) rk(field int) (operand, bool) {
	if !bytecode.IsConstant(field) {
		return reg(field), true
	}
	return c.constant(bytecode.ConstantIndex(field))
}

// floatEqualsInt branches to eq when the float in D0 equals the integer
// in r, which it does when it converts to that integer and back to
// itself, and to ne otherwise. It uses rT, rT2 and D1.
func (c *arm64Compiler) floatEqualsInt(r Reg, eq, ne Label) {
	a := &c.a
	a.Fcvtzs(rT, 0) // saturating: 0 for NaN, and the nearest end out of range
	a.Scvtf(1, rT)
	a.Fcmp(0, 1)
	a.BCond(NE, ne) // or unordered
	a.MovImm(rT2, 1<<63-1)
	a.Cmp(rT, rT2)
	a.BCond(EQ, ne) // 2^63 saturates to 2^63-1, which no float equals
	a.Cmp(rT, r)
	a.BCond(EQ, eq)
	a.B(ne)
}

// equal compiles EQ, whose operands may be any values. Floats compare as
// floats, and a float and an integer exactly (floatEqualsInt). Other
// values, integers among them, are equal when both words are, and when they are
// not can be equal only as strings of the same length, whose bytes it
// compares up to maxInlineCompare, or through __eq, which only two tables or two userdata try: it exits
// for those, unless both tables' metatables are known to lack __eq.
func (c *arm64Compiler) equal(ip int, i bytecode.Instruction) {
	a := &c.a
	target, ok := c.jumpAfter(ip)
	b, okB := c.rk(i.B())
	cc, okC := c.rk(i.C())
	if !ok || !okB || !okC {
		c.exitAlways(ip)
		return
	}
	// The JMP runs when the comparison's result equals A.
	yes, no := c.pcs[target], c.pcs[ip+2]
	back := a.NewLabel()
	if target <= ip {
		yes = back
	}
	eq, ne := yes, no
	if i.A() == 0 {
		eq, ne = no, yes
	}
	if kb, kc := c.isNumberConstant(i.B()), c.isNumberConstant(i.C()); kb || kc {
		// Against a float, only another number can be equal.
		if o, k := cc, b; !kb || !kc {
			if !kb {
				o, k = b, cc
			}
			floats, integer := a.NewLabel(), a.NewLabel()
			a.Ldr(rP, o.base, o.off+offP)
			a.Cmp(rP, rNumber)
			a.BCond(EQ, floats)
			c.branchNumber(rP, integer)
			a.B(ne)
			a.Bind(integer)
			a.LdrD(0, k.base, k.off+offN)
			a.Ldr(rIdx, o.base, o.off+offN)
			c.floatEqualsInt(rIdx, eq, ne)
			a.Bind(floats)
		}
		a.LdrD(0, b.base, b.off+offN)
		a.LdrD(1, cc.base, cc.off+offN)
		a.Fcmp(0, 1)
		a.BCond(EQ, eq) // NaN is unordered: not EQ
		a.B(ne)
		if target <= ip {
			a.Bind(back)
			c.backEdge(target)
		}
		return
	}
	exit := c.exit(ip)
	notNumber, differ := a.NewLabel(), a.NewLabel()
	a.Ldr(rP, b.base, b.off+offP)
	a.Ldr(rTmp, cc.base, cc.off+offP)
	a.Cmp(rP, rNumber)
	a.BCond(NE, notNumber)
	floats, integer := a.NewLabel(), a.NewLabel()
	a.Cmp(rTmp, rNumber)
	a.BCond(EQ, floats)
	c.branchNumber(rTmp, integer)
	a.B(ne)
	a.Bind(integer) // a float and an integer
	a.LdrD(0, b.base, b.off+offN)
	a.Ldr(rIdx, cc.base, cc.off+offN)
	c.floatEqualsInt(rIdx, eq, ne)
	a.Bind(floats)
	a.LdrD(0, b.base, b.off+offN)
	a.LdrD(1, cc.base, cc.off+offN)
	a.Fcmp(0, 1)
	a.BCond(EQ, eq) // NaN is unordered: not EQ
	a.B(ne)
	a.Bind(notNumber)
	a.Ldr(rN, b.base, b.off+offN)
	a.Ldr(rTmp2, cc.base, cc.off+offN)
	a.Cmp(rP, rTmp)
	a.BCond(NE, differ)
	a.Cmp(rN, rTmp2)
	a.BCond(EQ, eq)
	a.B(ne) // one address, other bits: true and false, integers, or strings' lengths
	a.Bind(differ)
	// Other first words. An integer and a float may be equal; a number
	// and anything else are not.
	notInteger := a.NewLabel()
	a.Sub(rIdx, rP, rNumber)
	a.CmpImm(rIdx, 1)
	a.BCond(NE, notInteger)
	a.Cmp(rTmp, rNumber)
	a.BCond(NE, ne)
	a.FmovToF(0, rTmp2)
	c.floatEqualsInt(rN, eq, ne)
	a.Bind(notInteger)
	a.Sub(rIdx, rTmp, rNumber)
	a.CmpImm(rIdx, 1)
	a.BCond(LS, ne)
	a.Lsr(rIdx, rN, kindShift)
	a.Lsr(rT, rTmp2, kindShift)
	a.Cmp(rIdx, rT)
	a.BCond(NE, ne) // different kinds
	strs, tables := a.NewLabel(), a.NewLabel()
	a.CmpImm(rIdx, uint32(vkString))
	a.BCond(EQ, strs)
	a.CmpImm(rIdx, uint32(vkTable))
	a.BCond(EQ, tables)
	a.CmpImm(rIdx, uint32(vkUserData))
	a.BCond(EQ, exit)
	a.B(ne) // other objects are equal only at one address
	// Strings: compare the bytes of short ones; Go compares long ones.
	a.Bind(strs)
	a.Cmp(rN, rTmp2)
	a.BCond(NE, ne) // lengths differ
	a.MovImm(rT, tagOf(vkString))
	a.Sub(rIdx, rN, rT) // the length, at least 1: the empty string has one address
	a.CmpImm(rIdx, maxInlineCompare)
	a.BCond(HI, exit)
	loop := a.NewLabel()
	a.Bind(loop)
	a.Ldrb(rT, rP, 0)
	a.Ldrb(rT2, rTmp, 0)
	a.Cmp(rT, rT2)
	a.BCond(NE, ne)
	a.AddImm(rP, rP, 1)
	a.AddImm(rTmp, rTmp, 1)
	a.SubsImm(rIdx, rIdx, 1)
	a.BCond(NE, loop)
	a.B(eq)
	a.Bind(tables) // either table's __eq, as Lua 5.4 on
	second := a.NewLabel()
	a.Ldr(rT, rP, offTMeta)
	a.Cbz(rT, second)
	a.Ldrb(rIdx, rT, offTFlags)
	a.Tbz(rIdx, uint32(tmEq), exit) // the metatable may have __eq
	a.Bind(second)
	a.Ldr(rT, rTmp, offTMeta)
	a.Cbz(rT, ne)
	a.Ldrb(rIdx, rT, offTFlags)
	a.Tbnz(rIdx, uint32(tmEq), ne) // the metatable has no __eq
	a.B(exit)
	if target <= ip {
		a.Bind(back)
		c.backEdge(target)
	}
}

// rkArith returns the operand for an RK field of arithmetic, with the
// number type of a constant, and false for a constant that is not a
// number.
func (c *arm64Compiler) rkArith(field int) (operand, numKind, bool) {
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

// branchUnlessInteger branches to l unless the number at o, of type kind,
// is an integer. It uses rTmp.
func (c *arm64Compiler) branchUnlessInteger(o operand, kind numKind, l Label) {
	switch kind {
	case kindFloat:
		c.a.B(l)
	case kindAny:
		c.a.Ldr(rTmp, o.base, o.off+offP)
		c.a.Cmp(rTmp, rInteger)
		c.a.BCond(NE, l)
	}
}

// loadFloat loads the number at o, of type kind, into d as a float,
// converting an integer, and exits at ip for anything else. With exact
// set it exits for an integer the conversion would round, so that a
// comparison of the floats compares the numbers. It uses rTmp.
func (c *arm64Compiler) loadFloat(d FReg, o operand, kind numKind, exact bool, ip int) {
	a := &c.a
	switch kind {
	case kindFloat:
		a.LdrD(d, o.base, o.off+offN)
		return
	case kindInt:
		if exact && !exactFloat(c.p.Constants[o.off/valueSize].i()) {
			a.B(c.exit(ip))
			return
		}
		a.Ldr(rTmp, o.base, o.off+offN)
		a.Scvtf(d, rTmp)
		return
	}
	isFloat, done := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, o.base, o.off+offP)
	a.Cmp(rTmp, rNumber)
	a.BCond(EQ, isFloat)
	a.Cmp(rTmp, rInteger)
	a.BCond(NE, c.exit(ip))
	a.Ldr(rTmp, o.base, o.off+offN)
	if exact { // rTmp + 2^53 <= 2^54, unsigned
		a.MovImm(rTmp2, 1<<53)
		a.Add(rTmp2, rTmp, rTmp2)
		a.MovImm(rP, 1<<54)
		a.Cmp(rTmp2, rP)
		a.BCond(HI, c.exit(ip))
	}
	a.Scvtf(d, rTmp)
	a.B(done)
	a.Bind(isFloat)
	a.LdrD(d, o.base, o.off+offN)
	a.Bind(done)
}

// storeInteger writes the integer in r to dst. Its guardStore must come
// first.
func (c *arm64Compiler) storeInteger(dst operand, r Reg) {
	c.a.Str(r, dst.base, dst.off+offN)
	c.a.Str(rInteger, dst.base, dst.off+offP)
}

// branchNumber branches to l when p, a value's first word, is a number's:
// the float or the integer sentinel, which are adjacent. An integer's
// second word can match any tag, so tests for objects and strings must
// rule both out. It uses rTmp2.
func (c *arm64Compiler) branchNumber(p Reg, l Label) {
	c.a.Sub(rTmp2, p, rNumber)
	c.a.CmpImm(rTmp2, 1)
	c.a.BCond(LS, l)
}

// guardNumber exits at ip unless o holds a float. Constants were checked
// when compiling.
func (c *arm64Compiler) guardNumber(o operand, ip int) {
	if o.base == rConst {
		return
	}
	c.a.Ldr(rTmp, o.base, o.off+offP)
	c.a.Cmp(rTmp, rNumber)
	c.a.BCond(NE, c.exit(ip))
}

// guardScalar exits at ip unless p, a value's first word, is nil, a number
// or a boolean: a value without a heap pointer. It uses rTmp, so p must be
// another register.
func (c *arm64Compiler) guardScalar(p Reg, ip int) {
	ok := c.a.NewLabel()
	c.a.Cbz(p, ok)
	c.a.Sub(rTmp, p, rNumber) // a float or an integer
	c.a.CmpImm(rTmp, 1)
	c.a.BCond(LS, ok)
	c.a.Cmp(p, rBool)
	c.a.BCond(NE, c.exit(ip))
	c.a.Bind(ok)
}

// guardStore exits at ip if the write barrier is on and dst holds a heap
// pointer, or newP, when it is not NoReg, is one. With the barrier off Go
// stores pointers without one too.
func (c *arm64Compiler) guardStore(dst operand, newP Reg, ip int) {
	done := c.a.NewLabel()
	c.a.Cbz(rBarrier, done)
	if newP != noReg {
		c.guardScalar(newP, ip)
	}
	c.a.Ldr(rTmp2, dst.base, dst.off+offP)
	c.guardScalar(rTmp2, ip)
	c.a.Bind(done)
}

const noReg Reg = 255

// storeNumber writes the number in f to dst. Its guardStore must come
// first.
func (c *arm64Compiler) storeNumber(dst operand, f FReg) {
	c.a.StrD(f, dst.base, dst.off+offN)
	c.a.Str(rNumber, dst.base, dst.off+offP)
}

// load reads the value at o into rP and rN.
func (c *arm64Compiler) load(o operand) {
	c.a.Ldr(rP, o.base, o.off+offP)
	c.a.Ldr(rN, o.base, o.off+offN)
}

// store writes rP and rN to dst. Its guardStore must come first.
func (c *arm64Compiler) store(dst operand) {
	c.a.Str(rN, dst.base, dst.off+offN)
	c.a.Str(rP, dst.base, dst.off+offP)
}

// copyValue copies src to dst, exiting at ip first if the write barrier
// forbids it.
func (c *arm64Compiler) copyValue(dst, src operand, ip int) {
	c.load(src)
	c.guardStore(dst, rP, ip)
	c.store(dst)
}

// branchFalsy branches to l when the value at o is nil or false.
func (c *arm64Compiler) branchFalsy(o operand, l Label) {
	truthy := c.a.NewLabel()
	c.a.Ldr(rTmp, o.base, o.off+offP)
	c.a.Cbz(rTmp, l)
	c.a.Cmp(rTmp, rBool)
	c.a.BCond(NE, truthy)
	c.a.Ldr(rTmp, o.base, o.off+offN)
	c.a.Tbz(rTmp, 0, l)
	c.a.Bind(truthy)
}

// backEdge spends one unit of budget and jumps to target, or exits there
// once the budget runs out.
func (c *arm64Compiler) backEdge(target int) {
	if c.budget[target] < 0 {
		c.budget[target] = c.a.NewLabel()
	}
	c.a.SubsImm(rBudget, rBudget, 1)
	c.a.BCond(EQ, c.budget[target])
	c.a.B(c.pcs[target])
}

// jumpTo jumps to target from ip, spending budget if it is backward.
func (c *arm64Compiler) jumpTo(ip, target int) {
	if target <= ip {
		c.backEdge(target)
	} else {
		c.a.B(c.pcs[target])
	}
}

// jumpAfter returns the target of the JMP at ip+1 that a test instruction
// at ip consumes, and false when that JMP closes upvalues.
func (c *arm64Compiler) jumpAfter(ip int) (int, bool) {
	j := c.code[ip+1]
	if j.A() != 0 {
		return 0, false
	}
	return ip + 2 + j.SBx(), true
}

// instruction compiles the instruction at ip and returns how many extra
// code words it consumed.
func (c *arm64Compiler) instruction(ip int) int {
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
		a.MovImm(rN, bits)
		a.Str(rN, dst.base, dst.off+offN)
		a.Str(rBool, dst.base, dst.off+offP)
		if orig.C() != 0 {
			a.B(c.pcs[ip+2])
		}
	case bytecode.OpLoadNil:
		for r := orig.A(); r <= orig.A()+orig.B(); r++ {
			c.guardStore(reg(r), noReg, ip)
		}
		for r := orig.A(); r <= orig.A()+orig.B(); r++ {
			a.Str(ZR, rFrame, reg(r).off+offP)
			a.Str(ZR, rFrame, reg(r).off+offN)
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
			a.Ldr(rP, b.base, b.off+offN)
			a.Ldr(rN, cc.base, cc.off+offN)
			switch op {
			case bytecode.OpAdd:
				a.Add(rN, rP, rN)
			case bytecode.OpSub:
				a.Sub(rN, rP, rN)
			case bytecode.OpMul:
				a.Mul(rN, rP, rN)
			}
			c.storeInteger(dst, rN)
			a.B(done)
		}
		a.Bind(floats)
		c.loadFloat(0, b, kb, false, ip)
		c.loadFloat(1, cc, kc, false, ip)
		switch op {
		case bytecode.OpAdd:
			a.Fadd(0, 0, 1)
		case bytecode.OpSub:
			a.Fsub(0, 0, 1)
		case bytecode.OpMul:
			a.Fmul(0, 0, 1)
		case bytecode.OpDiv:
			a.Fdiv(0, 0, 1)
		}
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
		a.Ldr(rN, src.base, src.off+offN)
		a.Neg(rN, rN)
		c.storeInteger(dst, rN)
		a.B(done)
		a.Bind(floats)
		a.Cmp(rTmp, rNumber)
		a.BCond(NE, c.exit(ip))
		a.LdrD(0, src.base, src.off+offN)
		a.Fneg(0, 0)
		c.storeNumber(dst, 0)
		a.Bind(done)
	case bytecode.OpNot:
		src, dst := reg(orig.B()), reg(orig.A())
		c.guardStore(dst, noReg, ip)
		falsy, done := a.NewLabel(), a.NewLabel()
		c.branchFalsy(src, falsy)
		a.MovImm(rN, tagOf(vkBool))
		a.B(done)
		a.Bind(falsy)
		a.MovImm(rN, tagOf(vkBool)|1)
		a.Bind(done)
		a.Str(rN, dst.base, dst.off+offN)
		a.Str(rBool, dst.base, dst.off+offP)
	case bytecode.OpJump:
		if orig.A() != 0 {
			c.exitAlways(ip)
			break
		}
		if target := ip + 1 + orig.SBx(); target <= ip {
			c.backEdge(target)
		} else {
			a.B(c.pcs[target])
		}
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
		// Two integers compare as integers; otherwise both as floats,
		// which is exact while an integer converts exactly, and Go
		// compares the rest.
		floats := a.NewLabel()
		if kb != kindFloat && kc != kindFloat {
			c.branchUnlessInteger(b, kb, floats)
			c.branchUnlessInteger(cc, kc, floats)
			a.Ldr(rP, b.base, b.off+offN)
			a.Ldr(rN, cc.base, cc.off+offN)
			a.Cmp(rP, rN)
			when := LT
			if op == bytecode.OpLessOrEqual {
				when = LE
			}
			if orig.A() == 0 {
				when = negate(when)
			}
			c.branchTo(when, ip, target)
			a.B(c.pcs[ip+2])
		}
		a.Bind(floats)
		c.loadFloat(0, b, kb, true, ip)
		c.loadFloat(1, cc, kc, true, ip)
		a.Fcmp(0, 1)
		// The JMP runs when the comparison's result equals A.
		when := MI
		if op == bytecode.OpLessOrEqual {
			when = LS
		}
		if orig.A() == 0 {
			when = negate(when)
		}
		c.branchTo(when, ip, target)
		a.B(c.pcs[ip+2])
	case bytecode.OpTest:
		target, ok := c.jumpAfter(ip)
		if !ok {
			c.exitAlways(ip)
			break
		}
		// The JMP runs when the value's truth differs from C. A backward
		// one, a repeat-until's, spends budget on the way.
		jump, skip, back := c.pcs[target], c.pcs[ip+2], a.NewLabel()
		if target <= ip {
			jump = back
		}
		if orig.C() == 0 {
			c.branchFalsy(reg(orig.A()), jump)
			a.B(skip)
		} else {
			c.branchFalsy(reg(orig.A()), skip)
			a.B(jump)
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
			a.B(skip)
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
		floatLoop, body, skip := a.NewLabel(), a.NewLabel(), a.NewLabel()
		c.branchUnlessInteger(init, kindAny, floatLoop)
		c.branchUnlessInteger(step, kindAny, floatLoop)
		c.branchUnlessInteger(limit, kindAny, c.exit(ip)) // Go clips a float limit
		a.Ldr(rP, init.base, init.off+offN)
		a.Ldr(rN, limit.base, limit.off+offN)
		a.Ldr(rIdx, step.base, step.off+offN)
		a.Cbz(rIdx, c.exit(ip)) // 'for' step is zero
		down, count := a.NewLabel(), a.NewLabel()
		a.Tbnz(rIdx, 63, down)
		a.Cmp(rP, rN)
		a.BCond(GT, skip)
		a.Sub(rLen, rN, rP) // (limit - init) / step, unsigned
		a.Udiv(rLen, rLen, rIdx)
		a.B(count)
		a.Bind(down)
		a.Cmp(rP, rN)
		a.BCond(LT, skip)
		a.Sub(rLen, rP, rN) // (init - limit) / -step, unsigned
		a.AddImm(rIdx, rIdx, 1)
		a.Neg(rIdx, rIdx) // -(step+1) + 1, which cannot overflow
		a.AddImm(rIdx, rIdx, 1)
		a.Udiv(rLen, rLen, rIdx)
		a.Bind(count)
		c.storeInteger(limit, rLen)
		c.storeInteger(ext, rP)
		a.B(c.pcs[ip+1])
		a.Bind(floatLoop)
		c.guardNumber(init, ip)
		c.guardNumber(limit, ip)
		c.guardNumber(step, ip)
		a.LdrD(0, init.base, init.off+offN)
		a.LdrD(1, limit.base, limit.off+offN)
		a.LdrD(2, step.base, step.off+offN)
		a.FmovToF(3, ZR)
		a.Fcmp(2, 3)
		a.BCond(EQ, c.exit(ip)) // 'for' step is zero
		positive := a.NewLabel()
		a.BCond(GT, positive)
		a.Fcmp(0, 1) // step < 0 or NaN: no times if init < limit
		a.BCond(MI, skip)
		a.B(body)
		a.Bind(positive) // no times if limit < init
		a.Fcmp(1, 0)
		a.BCond(MI, skip)
		a.Bind(body)
		c.storeNumber(ext, 0)
		a.B(c.pcs[ip+1])
		a.Bind(skip)
		a.B(c.pcs[ip+2+orig.SBx()])
	case bytecode.OpForLoop:
		// The loop's own registers hold what FORPREP put there, which Lua
		// code cannot change: an integer step means an integer loop, which
		// counts down in the limit's register.
		idx, limit, step, ext := reg(orig.A()), reg(orig.A()+1), reg(orig.A()+2), reg(orig.A()+3)
		target := ip + 1 + orig.SBx()
		c.guardStore(ext, noReg, ip)
		floatLoop := a.NewLabel()
		c.branchUnlessInteger(step, kindAny, floatLoop)
		a.Ldr(rN, limit.base, limit.off+offN)
		a.Cbz(rN, c.pcs[ip+1]) // no iterations left
		a.SubImm(rN, rN, 1)
		a.Ldr(rP, idx.base, idx.off+offN)
		a.Ldr(rIdx, step.base, step.off+offN)
		a.Add(rP, rP, rIdx)
		a.Str(rN, limit.base, limit.off+offN)
		a.Str(rP, idx.base, idx.off+offN)
		c.storeInteger(ext, rP)
		c.backEdge(target)
		a.Bind(floatLoop)
		c.guardNumber(step, ip)
		take, positive, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.LdrD(0, idx.base, idx.off+offN)
		a.LdrD(1, limit.base, limit.off+offN)
		a.LdrD(2, step.base, step.off+offN)
		a.Fadd(0, 0, 2)
		a.FmovToF(3, ZR)
		a.Fcmp(2, 3)
		a.BCond(GT, positive)
		a.BCond(LS, done) // step <= 0: continue while limit <= idx
		a.B(c.pcs[ip+1])  // step is NaN: the loop ends
		a.Bind(done)
		a.Fcmp(1, 0)
		a.BCond(LS, take)
		a.B(c.pcs[ip+1])
		a.Bind(positive)
		a.Fcmp(0, 1)
		a.BCond(LS, take)
		a.B(c.pcs[ip+1])
		a.Bind(take)
		a.StrD(0, idx.base, idx.off+offN)
		c.storeNumber(ext, 0)
		c.backEdge(target)
	case bytecode.OpGetTable, bytecode.OpGetTableUp, bytecode.OpSelf, bytecode.OpSetTable, bytecode.OpSetTableUp:
		c.tableAccess(ip, c.code[ip])
	case bytecode.OpCall:
		c.call(ip, orig)
	case bytecode.OpTailCall:
		c.tailCallLua(ip, orig)
	case bytecode.OpReturn:
		if hasTBC(c.p) { // Go closes the frame's to-be-closed variables first
			a.Ldr(rTmp, rCtx, offCtxTBC)
			a.Cmp(rTmp, rFrame)
			a.BCond(HS, c.exit(ip))
		}
		c.returnLua(ip, orig)
	case bytecode.OpTBC: // a nil closing value, as pairs gives, closes nothing
		r := reg(orig.A())
		if orig.B() == 0 {
			a.Ldr(rTmp, r.base, r.off+offP)
			a.Cbnz(rTmp, c.exit(ip))
			break
		}
		// A generic for's: the closing value, at a+1, swaps with the
		// control variable at a. Every exit comes before a store, and the
		// write barrier exits, so the interpreter never sees half a swap.
		closing := reg(orig.A() + 1)
		a.Cbnz(rBarrier, c.exit(ip))
		a.Ldr(rTmp, closing.base, closing.off+offP)
		a.Cbnz(rTmp, c.exit(ip))
		c.load(r)
		c.store(closing)
		a.Str(ZR, r.base, r.off+offP)
		a.Str(ZR, r.base, r.off+offN)
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

// branchTo branches to target when cond holds after a test at ip,
// spending budget on backward branches.
func (c *arm64Compiler) branchTo(cond Cond, ip, target int) {
	if target > ip {
		c.a.BCond(cond, c.pcs[target])
		return
	}
	skip := c.a.NewLabel()
	c.a.BCond(negate(cond), skip)
	c.backEdge(target)
	c.a.Bind(skip)
}

// negate returns the condition that holds exactly when c does not, for
// the conditions a floating-point comparison sets: NE for EQ, and for the
// orderings the condition that is also true when either side is NaN.
func negate(c Cond) Cond {
	switch c {
	case EQ:
		return NE
	case NE:
		return EQ
	case MI: // less
		return PL // greater, equal or unordered
	case PL:
		return MI
	case LS: // less or equal
		return HI // greater or unordered
	case HI:
		return LS
	case LT: // integer orderings
		return GE
	case GE:
		return LT
	case LE:
		return GT
	case GT:
		return LE
	}
	panic("negate: unexpected condition")
}

// upValueAddr puts the address of upvalue n's value in rAddr: the stack
// slot while the upvalue is open, its closed copy after.
func (c *arm64Compiler) upValueAddr(n int) {
	a := &c.a
	closed, done := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, rUpVals, uint32(n)*8) // *upValue
	a.Ldr(rTmp2, rTmp, offUVState)
	a.Cbz(rTmp2, closed)
	a.Ldr(rTmp2, rTmp2, offStack)  // &state.stack[0]
	a.Ldr(rAddr, rTmp, offUVIndex) // index
	a.AddShifted(rAddr, rTmp2, rAddr, 4)
	a.B(done)
	a.Bind(closed)
	a.AddImm(rAddr, rTmp, offUVClosed)
	a.Bind(done)
}
