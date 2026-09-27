//go:build (darwin || linux) && amd64

package lua

import (
	"math"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/amd64"
)

// Table access and calls on amd64, mirroring jit_arm64_tables.go. Lua
// calls and returns go through runJIT.

func (c *amd64Compiler) tableAccess(ip int, i bytecode.Instruction) {
	switch i.OpCode() {
	case opGetField:
		c.tableOf(reg(i.B()), ip)
		c.getField(ip, reg(i.A()))
	case opGetFieldUp:
		c.upValueAddr(i.B())
		c.tableOf(operand{rAddr, 0}, ip)
		c.getField(ip, reg(i.A()))
	case opSelfField:
		c.selfField(ip, i)
	case opSetField:
		c.setField(ip, i, false)
	case opSetFieldUp:
		c.setField(ip, i, true)
	case bytecode.OpGetTable, bytecode.OpGetTableUp:
		c.getIndex(ip, i, i.OpCode() == bytecode.OpGetTableUp)
	case bytecode.OpSetTable, bytecode.OpSetTableUp:
		c.setIndex(ip, i, i.OpCode() == bytecode.OpSetTableUp)
	default:
		c.exitAlways(ip)
	}
}

func (c *amd64Compiler) tableOf(o operand, ip int) { c.objectOf(o, vkTable, rT, ip) }

// objectOf puts the object the value at o holds in r, exiting at ip unless
// it is of kind k.
func (c *amd64Compiler) objectOf(o operand, k valueKind, r Reg, ip int) {
	a := &c.a
	a.Load(rTmp, o.base, o.off+offN)
	a.MovImm(rTmp2, tagOf(k))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, c.exit(ip))
	a.Load(r, o.base, o.off+offP)
	c.branchNumber(r, c.exit(ip)) // a number whose bits match the tag
}

// element puts the address of element idx of the []value at obj+off in
// out, exiting at ip unless idx, taken as unsigned, is in range.
func (c *amd64Compiler) element(obj Reg, off uint32, idx, out Reg, ip int) {
	a := &c.a
	a.Load(rLen, obj, off+offSliceLen)
	a.Cmp(idx, rLen)
	a.J(AE, c.exit(ip))
	a.Load(rLen, obj, off)
	a.Mov(out, idx)
	a.Shl(out, 4)
	a.Add(out, rLen)
}

// cachedShape checks that the table in rT has the shape the fieldCache of
// the instruction at ip names, which it leaves in rCache, and puts the
// cache's own slot in rIdx.
func (c *amd64Compiler) cachedShape(ip int) {
	a := &c.a
	exit := c.exit(ip)
	a.MovImm(rCache, uint64(uintptr(unsafe.Pointer(&c.p.fields[ip]))))
	a.Load(rIdx, rT, offTShape)
	a.Test(rIdx, rIdx)
	a.J(E, exit)
	a.Load(rLen, rCache, offCShape)
	a.Cmp(rIdx, rLen)
	a.J(NE, exit)
	a.Load32(rIdx, rCache, offCSlot)
}

// cachedSlot puts in rSlot the address of the table in rT's own slot the
// fieldCache of the instruction at ip names, exiting if it names none.
func (c *amd64Compiler) cachedSlot(ip int) {
	c.cachedShape(ip)
	c.a.Bt(rIdx, 31)
	c.a.J(B, c.exit(ip))
	c.element(rT, offTSlots, rIdx, rSlot, ip)
}

// readField loads into rP and rN the field of the table in rT that the
// fieldCache of the instruction at ip names: its own, or when that is
// absent or nil, the one in its metatable's __index table or in the table
// after that. It exits for anything else, and for a nil found through the
// metatable, after which Go goes on looking.
func (c *amd64Compiler) readField(ip int) {
	a := &c.a
	exit := c.exit(ip)
	c.cachedShape(ip)
	fromIndex, haveMeta, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bt(rIdx, 31)
	a.J(B, fromIndex)
	c.element(rT, offTSlots, rIdx, rSlot, ip)
	c.load(operand{rSlot, 0})
	a.Test(rP, rP)
	a.J(NE, done)
	// An own field holding nil: nil without a metatable, or the metatable's.
	a.Load(rT2, rT, offTMeta)
	a.Test(rT2, rT2)
	a.J(NE, haveMeta)
	a.MovImm(rN, 0)
	a.Jmp(done)
	a.Bind(fromIndex)
	a.Load(rT2, rT, offTMeta)
	a.Test(rT2, rT2)
	a.J(E, exit)
	a.Bind(haveMeta)
	c.readIndex(ip)
	a.Bind(done)
}

// readIndex loads into rP and rN the field that the fieldCache in rCache,
// of the instruction at ip, names through the metatable in rT2: in its
// __index table or in the table after that. It exits for anything else,
// and for nil, after which Go goes on looking.
func (c *amd64Compiler) readIndex(ip int) {
	a := &c.a
	exit := c.exit(ip)
	chain, load := a.NewLabel(), a.NewLabel()
	a.Load(rIdx, rT2, offTShape)
	a.Load(rLen, rCache, offCMtShape)
	a.Cmp(rIdx, rLen)
	a.J(NE, exit)
	a.Load32(rIdx, rCache, offCMtSlot)
	a.Bt(rIdx, 31)
	a.J(B, exit)
	c.element(rT2, offTSlots, rIdx, rSlot, ip)
	c.objectOf(operand{rSlot, 0}, vkTable, rT2, ip)
	a.Load(rIdx, rT2, offTShape)
	a.Load(rLen, rCache, offCIndex)
	a.Cmp(rIdx, rLen)
	a.J(NE, exit)
	a.Load32(rIdx, rCache, offCIdxSlot)
	a.Bt(rIdx, 31)
	a.J(B, chain)
	c.element(rT2, offTSlots, rIdx, rSlot, ip)
	a.Jmp(load)
	// One more __index table, as a method two classes up needs; longer
	// chains, and keys no table has, exit.
	a.Bind(chain)
	a.Load(rTmp, rCache, offCChain)
	a.Test(rTmp, rTmp)
	a.J(E, exit)
	a.Load(rTmp2, rTmp, offChLevels+offSliceLen)
	a.CmpImm(rTmp2, 1)
	a.J(NE, exit)
	a.Load32(rN, rTmp, offChSlot)
	a.Bt(rN, 31)
	a.J(B, exit)
	a.Load(rCache, rTmp, offChLevels) // &levels[0]
	a.Load(rIdx, rT2, offTMeta)
	a.Test(rIdx, rIdx)
	a.J(E, exit)
	a.Load(rP, rIdx, offTShape)
	a.Load(rTmp, rCache, offLvMtShape)
	a.Cmp(rP, rTmp)
	a.J(NE, exit)
	a.Load32(rP, rCache, offLvMtSlot)
	c.element(rIdx, offTSlots, rP, rSlot, ip)
	c.objectOf(operand{rSlot, 0}, vkTable, rT2, ip)
	a.Load(rP, rT2, offTShape)
	a.Load(rTmp, rCache, offLvIndex)
	a.Cmp(rP, rTmp)
	a.J(NE, exit)
	c.element(rT2, offTSlots, rN, rSlot, ip)
	a.Bind(load)
	c.load(operand{rSlot, 0})
	a.Test(rP, rP)
	a.J(E, exit)
}

// readStringMethod loads into rP and rN the field of a string that the
// fieldCache of the instruction at ip names, through the string metatable.
func (c *amd64Compiler) readStringMethod(ip int) {
	a := &c.a
	exit := c.exit(ip)
	a.MovImm(rCache, uint64(uintptr(unsafe.Pointer(&c.p.fields[ip]))))
	a.Load(rIdx, rCache, offCShape)
	a.MovImm(rLen, uint64(uintptr(unsafe.Pointer(stringShape))))
	a.Cmp(rIdx, rLen)
	a.J(NE, exit)
	a.MovImm(rT2, uint64(uintptr(unsafe.Pointer(&c.g.metaTables[TypeString]))))
	a.Load(rT2, rT2, 0)
	a.Test(rT2, rT2)
	a.J(E, exit)
	c.readIndex(ip)
}

// absentIsNil handles a nil just read from the table in rT: it exits at ip
// unless the table has no metatable, when nil is the result.
func (c *amd64Compiler) absentIsNil(ip int) {
	a := &c.a
	ok := a.NewLabel()
	a.Test(rP, rP)
	a.J(NE, ok)
	a.Load(rTmp, rT, offTMeta)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip))
	a.MovImm(rN, 0)
	a.Bind(ok)
}

func (c *amd64Compiler) getField(ip int, dst operand) {
	c.readField(ip)
	c.guardStore(dst, rP, ip)
	c.store(dst)
}

func (c *amd64Compiler) selfField(ip int, i bytecode.Instruction) {
	a := &c.a
	fn, self, recv := reg(i.A()), reg(i.A()+1), reg(i.B())
	// tableOf, going to selfString's code, which stubs emits out of line,
	// for other kinds.
	c.strSelf[ip] = a.NewLabel()
	a.Load(rTmp, recv.base, recv.off+offN)
	a.MovImm(rTmp2, tagOf(vkTable))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, c.strSelf[ip])
	a.Load(rT, recv.base, recv.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.readField(ip)
	c.guardStore(fn, rP, ip)
	c.guardStore(self, rT, ip)
	c.store(fn)
	a.MovImm(rTmp, tagOf(vkTable))
	a.Store(self.base, self.off+offN, rTmp)
	a.Store(self.base, self.off+offP, rT)
}

// selfString runs the SELF i at ip for a receiver that is not a table,
// with its second word in rTmp: a string, whose methods come through the
// string metatable, and anything else exits. Self is the string, both of
// whose words are kept before fn is stored, as fn may be the register the
// string is in.
func (c *amd64Compiler) selfString(ip int, i bytecode.Instruction) {
	a := &c.a
	fn, self, recv := reg(i.A()), reg(i.A()+1), reg(i.B())
	a.Shr(rTmp, kindShift)
	a.CmpImm(rTmp, int32(vkString))
	a.J(NE, c.exit(ip))
	a.Load(rT, recv.base, recv.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.readStringMethod(ip)
	c.guardStore(fn, rP, ip)
	c.guardStore(self, rT, ip)
	a.Load(rCache, recv.base, recv.off+offN)
	c.store(fn)
	a.Store(self.base, self.off+offN, rCache)
	a.Store(self.base, self.off+offP, rT)
}

// loadRK loads RK field into rP and rN, exiting at ip if it is nil unless
// nilOK. It reports false for a constant out of reach, or a nil one unless
// nilOK.
func (c *amd64Compiler) loadRK(field, ip int, nilOK bool) bool {
	src := reg(field)
	if bytecode.IsConstant(field) {
		k, ok := c.constant(bytecode.ConstantIndex(field))
		if !ok || !nilOK && c.p.Constants[bytecode.ConstantIndex(field)].isNil() {
			return false
		}
		src = k
	}
	c.load(src)
	if !nilOK {
		c.a.Test(rP, rP)
		c.a.J(E, c.exit(ip))
	}
	return true
}

func (c *amd64Compiler) setField(ip int, i bytecode.Instruction, up bool) {
	a := &c.a
	if !c.loadRK(i.C(), ip, true) {
		c.exitAlways(ip)
		return
	}
	t := reg(i.A())
	if up {
		c.upValueAddr(i.A())
		t = operand{rAddr, 0}
	}
	c.tableOf(t, ip)
	c.cachedSlot(ip)
	// An absent key: setField stores it only in a table with a shared
	// shape and without a metatable, or one known to lack __newindex. A
	// dictionary counts its nil slots, so storing nil in one exits too.
	slot, present, store := operand{rSlot, 0}, a.NewLabel(), a.NewLabel()
	a.Load(rTmp, rSlot, offP)
	a.Test(rTmp, rTmp)
	a.J(NE, present)
	a.Load(rTmp, rT, offTShape)
	a.Load8(rTmp, rTmp, offShapeDict)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip))
	a.Load(rTmp, rT, offTMeta)
	a.Test(rTmp, rTmp)
	a.J(E, store)
	a.Load8(rTmp, rTmp, offTFlags)
	a.Bt(rTmp, uint8(tmNewIndex))
	a.J(AE, c.exit(ip)) // __newindex may be there
	a.Jmp(store)
	a.Bind(present)
	a.Test(rP, rP)
	a.J(NE, store)
	a.Load(rTmp, rT, offTShape)
	a.Load8(rTmp, rTmp, offShapeDict)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip))
	a.Bind(store)
	c.guardStore(slot, rP, ip)
	c.store(slot)
	a.StoreZero8(rT, offTFlags) // invalidateTagMethodCache
}

// arrayIndex puts the zero-based array index for the number key RK(field)
// in rIdx: an integer, or a float with an integer value, as the table
// normalises it; anything else exits at ip.
func (c *amd64Compiler) arrayIndex(field, ip int) bool {
	a := &c.a
	k, kind, ok := c.rkArith(field)
	if !ok {
		return false
	}
	floatKey, done := a.NewLabel(), a.NewLabel()
	c.branchUnlessInteger(k, kind, floatKey)
	a.Load(rIdx, k.base, k.off+offN)
	a.Jmp(done)
	a.Bind(floatKey)
	if kind != kindInt {
		if kind == kindAny {
			a.Test(rTmp, rTmp) // p - numberPtr(): 0 for a float
			a.J(NE, c.exit(ip))
		}
		a.LoadSD(0, k.base, k.off+offN)
		a.Cvttsd2si(rIdx, 0)
		c.toFloat(1, rIdx)
		a.Ucomisd(0, 1)
		a.J(P, c.exit(ip))
		a.J(NE, c.exit(ip))
	}
	a.Bind(done)
	a.SubImm(rIdx, 1)
	return true
}

// indexedObject puts in rT the table the value in register or upvalue n
// holds, or branches to buf with the userdata it holds there, and exits at
// ip for anything else.
func (c *amd64Compiler) indexedObject(n int, up bool, ip int, buf Label) {
	a := &c.a
	o := reg(n)
	if up {
		c.upValueAddr(n)
		o = operand{rAddr, 0}
	}
	notTable := a.NewLabel()
	a.Load(rTmp, o.base, o.off+offN)
	a.MovImm(rTmp2, tagOf(vkTable))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, notTable)
	a.Load(rT, o.base, o.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(notTable)
		a.MovImm(rTmp2, tagOf(vkUserData))
		a.Cmp(rTmp, rTmp2)
		a.J(NE, c.exit(ip))
		a.Load(rT, o.base, o.off+offP)
		c.branchNumber(rT, c.exit(ip))
		a.Jmp(buf)
	})
}

// bufferElement checks that the userdata in rT is a buffer, with element
// rIdx+1 in range, as arrayIndex leaves the key less one. It leaves the
// element's address in rSlot for an element of size 1 << shift, the kind
// in rTmp, and exits at ip otherwise, for Go to read nil or raise the error.
// The branch to each kind's code follows.
func (c *amd64Compiler) bufferElement(ip int) (f64, f32, i32, u8 Label) {
	a := &c.a
	exit := c.exit(ip)
	a.Load(rT2, rT, offUDBuf)
	a.Test(rT2, rT2)
	a.J(E, exit) // a userdata that is not a buffer
	a.AddImm(rIdx, 1)
	a.Load(rLen, rT2, offBufLen)
	a.Cmp(rIdx, rLen)
	a.J(AE, exit) // unsigned: below 0 too
	a.Load(rSlot, rT2, offBufPtr)
	a.Load8(rTmp, rT2, offBufKind)
	f64, f32, i32, u8 = a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Test(rTmp, rTmp)
	a.J(E, f64)
	a.CmpImm(rTmp, int32(bufferFloat32))
	a.J(E, f32)
	a.CmpImm(rTmp, int32(bufferInt32))
	a.J(E, i32)
	a.Jmp(u8)
	return
}

// getIndex compiles GETTABLE, or GETTABUP when up is set, for an integer
// key: an array element, nil past the array of a table without a hash
// part or a metatable, or a buffer's element. Other keys exit.
func (c *amd64Compiler) getIndex(ip int, i bytecode.Instruction, up bool) {
	a := &c.a
	if !c.arrayIndex(i.C(), ip) {
		c.exitAlways(ip)
		return
	}
	buf, store, stored := a.NewLabel(), a.NewLabel(), a.NewLabel()
	dst := reg(i.A())
	c.indexedObject(i.B(), up, ip, buf)
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(buf)
		f64, f32, i32, u8 := c.bufferElement(ip)
		a.Bind(f64)
		a.Lea(rSlot, rSlot, rIdx, 3, 0)
		a.Load(rN, rSlot, 0)
		a.Mov(rP, rNumber)
		a.Jmp(store)
		a.Bind(f32) // stored from X0: see setIndex
		a.Lea(rSlot, rSlot, rIdx, 2, 0)
		a.LoadSSToSD(0, rSlot, 0)
		c.guardStore(dst, noReg, ip)
		c.storeNumber(dst, 0)
		a.Jmp(stored)
		integer := a.NewLabel()
		a.Bind(i32)
		a.Lea(rSlot, rSlot, rIdx, 2, 0)
		a.Load32S(rN, rSlot, 0)
		a.Jmp(integer)
		a.Bind(u8)
		a.Lea(rSlot, rSlot, rIdx, 0, 0)
		a.Load8(rN, rSlot, 0)
		a.Bind(integer)
		a.Mov(rP, rNumber)
		a.AddImm(rP, 1) // integerPtr, the next byte
		a.Jmp(store)
	})
	outside, done := a.NewLabel(), a.NewLabel()
	a.Load(rLen, rT, offTArray+offSliceLen)
	a.Cmp(rIdx, rLen)
	a.J(AE, outside) // unsigned: keys below 1 too
	a.Load(rLen, rT, offTArray)
	a.Mov(rSlot, rIdx)
	a.Shl(rSlot, 4)
	a.Add(rSlot, rLen)
	c.load(operand{rSlot, 0})
	c.absentIsNil(ip)
	a.Jmp(done)
	a.Bind(outside)
	a.Load(rTmp, rT, offTHash)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip)) // the key may be there
	a.Load(rTmp, rT, offTMeta)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip))
	a.MovImm(rP, 0)
	a.MovImm(rN, 0)
	a.Bind(done)
	a.Bind(store)
	c.guardStore(dst, rP, ip)
	c.store(dst)
	a.Bind(stored)
}

// setIndex compiles SETTABLE, or SETTABUP when up is set, for an array
// element or a buffer's element; other keys exit.
func (c *amd64Compiler) setIndex(ip int, i bytecode.Instruction, up bool) {
	a := &c.a
	if !c.loadRK(i.C(), ip, false) {
		c.exitAlways(ip)
		return
	}
	if !c.arrayIndex(i.B(), ip) {
		c.exitAlways(ip)
		return
	}
	buf, done := a.NewLabel(), a.NewLabel()
	src, _ := c.rk(i.C()) // loadRK reached it
	c.indexedObject(i.A(), up, ip, buf)
	c.outOfLine = append(c.outOfLine, func() {
		exit := c.exit(ip)
		a.Bind(buf)
		f64, f32, i32, u8 := c.bufferElement(ip)
		// A float buffer takes any number; an integer buffer an integer,
		// and Go converts a float with an integer value. A float's bits
		// are stored from rN, or loaded again from src into X0: moving
		// them from rN to an SSE register before the store measured three
		// times slower in plasma, fed by sin.
		isInteger := func() {
			a.Mov(rTmp, rP)
			a.Sub(rTmp, rNumber)
			a.CmpImm(rTmp, 1)
			a.J(NE, exit)
		}
		stored := a.NewLabel()
		a.Bind(f64)
		a.Lea(rSlot, rSlot, rIdx, 3, 0)
		integer64 := a.NewLabel()
		a.Cmp(rP, rNumber)
		a.J(NE, integer64)
		a.Store(rSlot, 0, rN)
		a.Jmp(stored)
		a.Bind(integer64)
		isInteger()
		c.toFloat(0, rN)
		a.StoreSD(rSlot, 0, 0)
		a.Jmp(stored)
		a.Bind(f32)
		integer32, convert := a.NewLabel(), a.NewLabel()
		a.Cmp(rP, rNumber)
		a.J(NE, integer32)
		a.LoadSD(0, src.base, src.off+offN)
		a.Jmp(convert)
		a.Bind(integer32)
		isInteger()
		c.toFloat(0, rN)
		a.Bind(convert)
		a.Cvtsd2ss(0, 0)
		a.Lea(rSlot, rSlot, rIdx, 2, 0)
		a.StoreSS(rSlot, 0, 0)
		a.Jmp(stored)
		a.Bind(i32)
		isInteger()
		a.Lea(rSlot, rSlot, rIdx, 2, 0)
		a.Store32(rSlot, 0, rN)
		a.Jmp(stored)
		a.Bind(u8)
		isInteger()
		a.Lea(rSlot, rSlot, rIdx, 0, 0)
		a.Store8(rSlot, 0, rN)
		a.Bind(stored)
		a.Jmp(done)
	})
	c.element(rT, offTArray, rIdx, rSlot, ip)
	slot, present := operand{rSlot, 0}, a.NewLabel()
	a.Load(rTmp, rSlot, offP)
	a.Test(rTmp, rTmp)
	a.J(NE, present)
	a.Load(rTmp, rT, offTMeta)
	a.Test(rTmp, rTmp)
	a.J(NE, c.exit(ip))
	a.Bind(present)
	c.guardStore(slot, rP, ip)
	c.store(slot)
	a.StoreZero8(rT, offTFlags) // invalidateTagMethodCache
	a.Bind(done)
}

// setList compiles SETLIST of a fixed count of values, as a constructor
// such as {a, b, c} ends, into the array part NEWTABLE sized for them. It
// exits when the array is too short, for jitStep to extend it, and while
// the write barrier is on.
func (c *amd64Compiler) setList(ip int, i bytecode.Instruction) {
	a := &c.a
	n, start := i.B(), (i.C()-1)*bytecode.ListItemsPerFlush
	if n == 0 || start+n > maxSetList { // values up to the stack top, which compiled code does not track
		c.exitAlways(ip)
		return
	}
	exit := c.exit(ip)
	c.tableOf(reg(i.A()), ip)
	a.CmpMem(rCtx, offBarrier, 0)
	a.J(NE, exit)
	a.Load(rLen, rT, offTArray+offSliceLen)
	a.CmpImm(rLen, int32(start+n))
	a.J(L, exit)
	a.Load(rSlot, rT, offTArray)
	for k := range n {
		c.load(reg(i.A() + 1 + k))
		c.store(operand{rSlot, uint32(start+k) * valueSize})
	}
}

// intrinsic is a unary number function compiled inline: emit computes
// x0 from x0 bit for bit as the Go function does.
type intrinsic struct {
	fn   uint64
	emit func()
}

// intrinsics returns the functions compiled inline on amd64: sin and cos
// need a GOAMD64 level Go does not fuse at. floor, ceil and abs return
// integers for integers, so they are no longer number functions of floats.
func (c *amd64Compiler) intrinsics() []intrinsic {
	a := &c.a
	list := []intrinsic{
		{funcValue(math.Sqrt), func() { a.SqrtSD(0, 0) }},
	}
	return append(list, c.trigIntrinsics()...)
}

func funcValue(f func(float64) float64) uint64 { return uint64(*(*uintptr)(unsafe.Pointer(&f))) }

// call compiles CALL. A unary intrinsic applied to a number runs here, and
// a call to a compiled Lua function enters it directly. A call to any
// other Go function exits with jitExitCallGo, and runJIT makes it; other
// calls exit to the interpreter.
func (c *amd64Compiler) call(ip int, i bytecode.Instruction) {
	notGoFunction := c.a.NewLabel()
	if (i.B() == 2 || i.B() == 0) && (i.C() == 2 || i.C() == 0) { // one argument, one result
		c.intrinsic(ip, i, notGoFunction)
	}
	c.a.Bind(notGoFunction)
	c.notLua[ip] = c.a.NewLabel()
	c.callLua(ip, i, c.notLua[ip])
}

// goCallee exits with jitExitCallGo when the callee of the CALL i at ip is
// a Go function or Go closure, and otherwise falls through. stubs emits it
// out of line, after the Lua closure check, so calls between Lua functions
// neither run it nor have it in their way.
func (c *amd64Compiler) goCallee(ip int, i bytecode.Instruction) {
	a := &c.a
	fn := reg(i.A())
	notGo, closure := a.NewLabel(), a.NewLabel()
	a.Load(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkGoClosure))
	a.Cmp(rTmp, rTmp2)
	a.J(E, closure)
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, notGo)
	a.Load(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	a.Load(rTmp, rT, offGFNumber)
	a.Test(rTmp, rTmp)
	a.J(NE, c.numCallExit(ip)) // runJIT may call it frameless
	a.Bind(c.plainGoCall(ip))
	c.mathCall(ip, i)
	a.Jmp(c.goCallExit(ip))
	a.Bind(closure)
	a.Load(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	a.Jmp(c.goCallExit(ip))
	a.Bind(notGo)
}

// plainGoCall returns the label in goCallee's code for the CALL at ip
// when its callee is a Go function but not a number function.
func (c *amd64Compiler) plainGoCall(ip int) Label {
	if c.plainGo[ip] < 0 {
		c.plainGo[ip] = c.a.NewLabel()
	}
	return c.plainGo[ip]
}

// mathCall computes the CALL i at ip inline when its callee, a Go
// function, is a math function of its arity (see mathFn), writing the
// result to register A and going on at ip+1. It falls through for other
// functions, and exits to Go for arguments the function would reject or
// that it leaves to Go: mixed integers and floats for min and max, and
// floats for floor and ceil without ROUNDSD.
func (c *amd64Compiler) mathCall(ip int, i bytecode.Instruction) {
	if i.C() != 2 && i.C() != 0 || i.B() != 2 && i.B() != 3 && i.B() != 0 {
		return
	}
	a := &c.a
	fn := reg(i.A())
	a.Load(rT, fn.base, fn.off+offP) // the *goFunction
	a.Load(rTmp, rT, 0)              // its Function's code
	var bodies []func()
	for _, m := range mathFns {
		if i.B() != 0 && m.id.unary() != (i.B() == 2) {
			continue
		}
		l := a.NewLabel()
		a.MovImm(rIdx, m.fn)
		a.Cmp(rTmp, rIdx)
		a.J(E, l)
		bodies = append(bodies, func() { a.Bind(l); c.mathBody(ip, i, m.id) })
	}
	skip := a.NewLabel()
	a.Jmp(skip)
	for _, b := range bodies {
		b()
	}
	a.Bind(skip)
}

// mathBody computes math function m for the CALL i at ip, as mathCall
// describes.
func (c *amd64Compiler) mathBody(ip int, i bytecode.Instruction, m mathFn) {
	a := &c.a
	fn, arg, arg2 := reg(i.A()), reg(i.A()+1), reg(i.A()+2)
	goCall := c.goCallExit(ip)
	next := c.pcs[ip+1] // after the result is stored
	if i.C() == 0 {     // leaving l.top after it
		open := a.NewLabel()
		c.outOfLine = append(c.outOfLine, func() {
			a.Bind(open)
			c.resultTop(i)
			a.Jmp(c.pcs[ip+1])
		})
		next = open
	}
	args := 1
	if !m.unary() {
		args = 2
	}
	c.checkArgs(i, args, goCall)
	isFloat := a.NewLabel()
	// rTmp: 0 for a float argument, 1 for an integer; anything else calls
	// Go, which raises the error.
	a.Load(rTmp, arg.base, arg.off+offP)
	a.Sub(rTmp, rNumber)
	if !m.unary() { // the same type, both
		a.Load(rTmp2, arg2.base, arg2.off+offP)
		a.Sub(rTmp2, rNumber)
		a.Cmp(rTmp, rTmp2)
		a.J(NE, goCall)
	}
	a.Test(rTmp, rTmp)
	a.J(E, isFloat)
	a.CmpImm(rTmp, 1)
	a.J(NE, goCall)
	c.guardStore(fn, noReg, ip)
	// Integers.
	a.Load(rN, arg.base, arg.off+offN)
	switch m {
	case mathAbs:
		pos := a.NewLabel()
		a.Test(rN, rN)
		a.J(NS, pos)
		a.Neg(rN) // minint stays minint
		a.Bind(pos)
	case mathMin, mathMax:
		keep := a.NewLabel()
		a.Load(rP, arg2.base, arg2.off+offN)
		if m == mathMin {
			a.Cmp(rP, rN) // the second if it is less
		} else {
			a.Cmp(rN, rP) // the second if the first is less
		}
		a.J(GE, keep)
		a.Mov(rN, rP)
		a.Bind(keep)
	}
	c.storeInteger(fn, rN) // floor and ceil: the integer itself
	a.Jmp(next)
	// Floats.
	a.Bind(isFloat)
	if (m == mathFloor || m == mathCeil) && !c.sse41 {
		a.Jmp(goCall)
		return
	}
	c.guardStore(fn, noReg, ip)
	a.LoadSD(0, arg.base, arg.off+offN)
	switch m {
	case mathFloor, mathCeil:
		mode := byte(1) // toward minus infinity
		if m == mathCeil {
			mode = 2
		}
		a.RoundSD(0, 0, mode)
		// An integer when one holds it, -2^63 <= f < 2^63, as
		// pushIntegerIfFits decides; otherwise the float.
		float := a.NewLabel()
		a.MovImm(rTmp, math.Float64bits(1<<63))
		a.MovqToX(1, rTmp)
		a.Ucomisd(0, 1)
		a.J(P, float) // NaN
		a.J(AE, float)
		a.MovImm(rTmp, math.Float64bits(-(1 << 63)))
		a.MovqToX(1, rTmp)
		a.Ucomisd(0, 1)
		a.J(B, float)
		a.Cvttsd2si(rN, 0)
		c.storeInteger(fn, rN)
		a.Jmp(next)
		a.Bind(float)
	case mathAbs:
		a.MovImm(rTmp, 1<<63-1)
		a.MovqToX(1, rTmp)
		a.AndPD(0, 1)
	case mathMin, mathMax:
		// Lua's <, false for NaN either way: the first unless the second
		// is less (min), or the first is less than the second (max).
		keep := a.NewLabel()
		a.LoadSD(1, arg2.base, arg2.off+offN)
		if m == mathMin {
			a.Ucomisd(1, 0)
		} else {
			a.Ucomisd(0, 1)
		}
		a.J(P, keep)
		a.J(AE, keep)
		a.MovSD(0, 1)
		a.Bind(keep)
	}
	c.storeNumber(fn, 0)
	a.Jmp(next)
}

// intrinsic compiles a unary intrinsic call, jumping to notGo when the
// callee is not a Go function.
func (c *amd64Compiler) intrinsic(ip int, i bytecode.Instruction, notGo Label) {
	a := &c.a
	c.ip = ip
	fn, arg := reg(i.A()), reg(i.A()+1)
	a.Load(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.J(NE, notGo)
	a.Load(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	a.Load(rT, rT, offGFNumber)
	a.Test(rT, rT)
	a.J(E, c.plainGoCall(ip)) // perhaps a math function
	a.Load(rT, rT, offNFUnary)
	c.checkArgs(i, 1, c.numCallExit(ip))
	c.loadFloat(0, arg, kindAny, false, ip) // an integer converts, as for a number function
	done := a.NewLabel()
	for _, in := range c.intrinsics() {
		next := a.NewLabel()
		a.MovImm(rIdx, in.fn)
		a.Cmp(rT, rIdx)
		a.J(NE, next)
		in.emit()
		a.Jmp(done)
		a.Bind(next)
	}
	a.Jmp(c.numCallExit(ip)) // a number function runJIT may call frameless
	a.Bind(done)
	c.guardStore(fn, noReg, ip)
	c.storeNumber(fn, 0)
	c.resultTop(i)
	a.Jmp(c.pcs[ip+1])
}
