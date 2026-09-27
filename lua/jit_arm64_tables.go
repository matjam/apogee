//go:build (darwin || linux) && arm64

package lua

import (
	"math"
	"unsafe"

	"github.com/matjam/apogee/internal/bytecode"
	. "github.com/matjam/apogee/internal/jit/arm64"
)

// Table access and calls. Each hit path mirrors the interpreter's fast
// path exactly (getField, setField, table.at and tryPut, and the unary
// number function call); anything else exits so the interpreter runs the
// instruction, filling the caches the next run hits.

// tableAccess compiles i, the exec form of a table instruction at ip.
func (c *arm64Compiler) tableAccess(ip int, i bytecode.Instruction) {
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

// tableOf puts the table the value at o holds in rT, exiting at ip if it
// holds anything else.
func (c *arm64Compiler) tableOf(o operand, ip int) {
	c.objectOf(o, vkTable, rT, ip)
}

// objectOf puts the object the value at o holds in r, exiting at ip unless
// it is of kind k.
func (c *arm64Compiler) objectOf(o operand, k valueKind, r Reg, ip int) {
	a := &c.a
	a.Ldr(rTmp, o.base, o.off+offN)
	a.MovImm(rTmp2, tagOf(k))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, c.exit(ip))
	a.Ldr(r, o.base, o.off+offP)
	c.branchNumber(r, c.exit(ip)) // a number whose bits match the tag
}

// element puts the address of element idx of the []value at obj+off in
// out, exiting at ip unless idx, taken as unsigned, is in range.
func (c *arm64Compiler) element(obj Reg, off uint32, idx, out Reg, ip int) {
	a := &c.a
	a.Ldr(rLen, obj, off+offSliceLen)
	a.Cmp(idx, rLen)
	a.BCond(HS, c.exit(ip))
	a.Ldr(rLen, obj, off)
	a.AddShifted(out, rLen, idx, 4)
}

// cachedShape checks that the table in rT has the shape the fieldCache of
// the instruction at ip names, which it leaves in rCache, and puts the
// cache's own slot in rIdx.
func (c *arm64Compiler) cachedShape(ip int) {
	a := &c.a
	a.MovImm(rCache, uint64(uintptr(unsafe.Pointer(&c.p.fields[ip]))))
	a.Ldr(rIdx, rT, offTShape)
	a.Cbz(rIdx, c.exit(ip))
	a.Ldr(rLen, rCache, offCShape)
	a.Cmp(rIdx, rLen)
	a.BCond(NE, c.exit(ip))
	a.LdrW(rIdx, rCache, offCSlot)
}

// cachedSlot puts in rSlot the address of the table in rT's own slot the
// fieldCache of the instruction at ip names, exiting if it names none.
func (c *arm64Compiler) cachedSlot(ip int) {
	c.cachedShape(ip)
	c.a.Tbnz(rIdx, 31, c.exit(ip))
	c.element(rT, offTSlots, rIdx, rSlot, ip)
}

// readField loads into rP and rN the field of the table in rT that the
// fieldCache of the instruction at ip names: its own, or when that is
// absent or nil, the one in its metatable's __index table or in the table
// after that. It exits for anything else, and for a nil found through the
// metatable, after which Go goes on looking.
func (c *arm64Compiler) readField(ip int) {
	a := &c.a
	c.cachedShape(ip)
	own, haveMeta, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Tbz(rIdx, 31, own)
	// fromIndex: the metatable's shape, then its __index table's.
	a.Ldr(rT2, rT, offTMeta)
	a.Cbz(rT2, c.exit(ip))
	a.Bind(haveMeta)
	c.readIndex(ip)
	a.B(done)
	a.Bind(own)
	c.element(rT, offTSlots, rIdx, rSlot, ip)
	c.load(operand{rSlot, 0})
	a.Cbnz(rP, done)
	// An own field holding nil: nil without a metatable, or the metatable's.
	a.Ldr(rT2, rT, offTMeta)
	a.Cbnz(rT2, haveMeta)
	a.Mov(rN, ZR)
	a.Bind(done)
}

// readIndex loads into rP and rN the field that the fieldCache in rCache,
// of the instruction at ip, names through the metatable in rT2: in its
// __index table or in the table after that. It exits for anything else,
// and for nil, after which Go goes on looking.
func (c *arm64Compiler) readIndex(ip int) {
	a := &c.a
	chain, load := a.NewLabel(), a.NewLabel()
	a.Ldr(rIdx, rT2, offTShape)
	a.Ldr(rLen, rCache, offCMtShape)
	a.Cmp(rIdx, rLen)
	a.BCond(NE, c.exit(ip))
	a.LdrW(rIdx, rCache, offCMtSlot)
	a.Tbnz(rIdx, 31, c.exit(ip))
	c.element(rT2, offTSlots, rIdx, rSlot, ip)
	c.objectOf(operand{rSlot, 0}, vkTable, rT2, ip)
	a.Ldr(rIdx, rT2, offTShape)
	a.Ldr(rLen, rCache, offCIndex)
	a.Cmp(rIdx, rLen)
	a.BCond(NE, c.exit(ip))
	a.LdrW(rIdx, rCache, offCIdxSlot)
	a.Tbnz(rIdx, 31, chain)
	c.element(rT2, offTSlots, rIdx, rSlot, ip)
	a.B(load)
	// One more __index table, as a method two classes up needs; longer
	// chains, and keys no table has, exit.
	a.Bind(chain)
	a.Ldr(rTmp, rCache, offCChain)
	a.Cbz(rTmp, c.exit(ip))
	a.Ldr(rTmp2, rTmp, offChLevels+offSliceLen)
	a.CmpImm(rTmp2, 1)
	a.BCond(NE, c.exit(ip))
	a.LdrW(rN, rTmp, offChSlot)
	a.Tbnz(rN, 31, c.exit(ip))
	a.Ldr(rCache, rTmp, offChLevels) // &levels[0]
	a.Ldr(rIdx, rT2, offTMeta)
	a.Cbz(rIdx, c.exit(ip))
	a.Ldr(rP, rIdx, offTShape)
	a.Ldr(rTmp, rCache, offLvMtShape)
	a.Cmp(rP, rTmp)
	a.BCond(NE, c.exit(ip))
	a.LdrW(rP, rCache, offLvMtSlot)
	c.element(rIdx, offTSlots, rP, rSlot, ip)
	c.objectOf(operand{rSlot, 0}, vkTable, rT2, ip)
	a.Ldr(rP, rT2, offTShape)
	a.Ldr(rTmp, rCache, offLvIndex)
	a.Cmp(rP, rTmp)
	a.BCond(NE, c.exit(ip))
	c.element(rT2, offTSlots, rN, rSlot, ip)
	a.Bind(load)
	c.load(operand{rSlot, 0})
	a.Cbz(rP, c.exit(ip))
}

// readStringMethod loads into rP and rN the field of a string that the
// fieldCache of the instruction at ip names, through the string metatable.
func (c *arm64Compiler) readStringMethod(ip int) {
	a := &c.a
	a.MovImm(rCache, uint64(uintptr(unsafe.Pointer(&c.p.fields[ip]))))
	a.Ldr(rIdx, rCache, offCShape)
	a.MovImm(rLen, uint64(uintptr(unsafe.Pointer(stringShape))))
	a.Cmp(rIdx, rLen)
	a.BCond(NE, c.exit(ip))
	a.MovImm(rT2, uint64(uintptr(unsafe.Pointer(&c.g.metaTables[TypeString]))))
	a.Ldr(rT2, rT2, 0)
	a.Cbz(rT2, c.exit(ip))
	c.readIndex(ip)
}

// getField stores the field of the table in rT that the instruction at ip
// caches to dst, exiting unless the cache hits.
func (c *arm64Compiler) getField(ip int, dst operand) {
	c.readField(ip)
	c.guardStore(dst, rP, ip)
	c.store(dst)
}

// absentIsNil handles a nil just read from the table in rT, which the
// interpreter looks up in the table's metatable: it exits at ip unless the
// table has none, when nil is the result.
func (c *arm64Compiler) absentIsNil(ip int) {
	ok := c.a.NewLabel()
	c.a.Cbnz(rP, ok)
	c.a.Ldr(rTmp, rT, offTMeta)
	c.a.Cbnz(rTmp, c.exit(ip))
	c.a.Mov(rN, ZR)
	c.a.Bind(ok)
}

// selfField compiles SELF with a constant string key: R(A+1) := R(B);
// R(A) := R(B)[K(C)].
func (c *arm64Compiler) selfField(ip int, i bytecode.Instruction) {
	a := &c.a
	fn, self, recv := reg(i.A()), reg(i.A()+1), reg(i.B())
	// tableOf, going to selfString's code, which stubs emits out of line,
	// for other kinds.
	c.strSelf[ip] = a.NewLabel()
	a.Ldr(rTmp, recv.base, recv.off+offN)
	a.MovImm(rTmp2, tagOf(vkTable))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, c.strSelf[ip])
	a.Ldr(rT, recv.base, recv.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.readField(ip)
	c.guardStore(fn, rP, ip)
	c.guardStore(self, rT, ip)
	c.store(fn)
	a.MovImm(rTmp, tagOf(vkTable))
	a.Str(rTmp, self.base, self.off+offN)
	a.Str(rT, self.base, self.off+offP)
}

// selfString runs the SELF i at ip for a receiver that is not a table,
// with its second word in rTmp: a string, whose methods come through the
// string metatable, and anything else exits. Self is the string, both of
// whose words are kept before fn is stored, as fn may be the register the
// string is in.
func (c *arm64Compiler) selfString(ip int, i bytecode.Instruction) {
	a := &c.a
	fn, self, recv := reg(i.A()), reg(i.A()+1), reg(i.B())
	a.Lsr(rTmp, rTmp, kindShift)
	a.CmpImm(rTmp, uint32(vkString))
	a.BCond(NE, c.exit(ip))
	a.Ldr(rT, recv.base, recv.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.readStringMethod(ip)
	c.guardStore(fn, rP, ip)
	c.guardStore(self, rT, ip)
	a.Ldr(rCache, recv.base, recv.off+offN)
	c.store(fn)
	a.Str(rCache, self.base, self.off+offN)
	a.Str(rT, self.base, self.off+offP)
}

// loadRK loads RK field into rP and rN, exiting at ip if it is nil unless
// nilOK. It reports false for a constant out of reach, or a nil one unless
// nilOK.
func (c *arm64Compiler) loadRK(field, ip int, nilOK bool) bool {
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
		c.a.Cbz(rP, c.exit(ip))
	}
	return true
}

// setField compiles SETTABLE, or SETTABUP when up is true, with a constant
// string key, storing RK(C) to the cached slot as setField does.
func (c *arm64Compiler) setField(ip int, i bytecode.Instruction, up bool) {
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
	slot := operand{rSlot, 0}
	// An absent key: setField stores it only in a table with a shared
	// shape and without a metatable, or one known to lack __newindex. A
	// dictionary counts its nil slots, so storing nil in one exits too.
	present, store := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, rSlot, offP)
	a.Cbnz(rTmp, present)
	a.Ldr(rTmp, rT, offTShape)
	a.Ldrb(rTmp, rTmp, offShapeDict)
	a.Cbnz(rTmp, c.exit(ip))
	a.Ldr(rTmp, rT, offTMeta)
	a.Cbz(rTmp, store)
	a.Ldrb(rTmp, rTmp, offTFlags)
	a.Tbz(rTmp, uint32(tmNewIndex), c.exit(ip)) // __newindex may be there
	a.B(store)
	a.Bind(present)
	a.Cbnz(rP, store)
	a.Ldr(rTmp, rT, offTShape)
	a.Ldrb(rTmp, rTmp, offShapeDict)
	a.Cbnz(rTmp, c.exit(ip))
	a.Bind(store)
	c.guardStore(slot, rP, ip)
	c.store(slot)
	a.Strb(ZR, rT, offTFlags) // invalidateTagMethodCache
}

// arrayIndex puts the zero-based array index for the number key RK(field)
// in rIdx: an integer, or a float with an integer value, as the table
// normalises it; anything else exits at ip. It reports false for a
// constant key that is not a number.
func (c *arm64Compiler) arrayIndex(field, ip int) bool {
	a := &c.a
	k, kind, ok := c.rkArith(field)
	if !ok {
		return false
	}
	floatKey, done := a.NewLabel(), a.NewLabel()
	c.branchUnlessInteger(k, kind, floatKey)
	a.Ldr(rIdx, k.base, k.off+offN)
	a.B(done)
	a.Bind(floatKey)
	if kind == kindInt {
		a.B(done) // never reached
	} else {
		if kind == kindAny {
			a.Cmp(rTmp, rNumber)
			a.BCond(NE, c.exit(ip))
		}
		a.LdrD(0, k.base, k.off+offN)
		a.Fcvtzs(rIdx, 0)
		a.Scvtf(1, rIdx)
		a.Fcmp(0, 1)
		a.BCond(NE, c.exit(ip))
	}
	a.Bind(done)
	a.SubImm(rIdx, rIdx, 1)
	return true
}

// indexedObject puts in rT the table the value in register or upvalue n
// holds, or branches to buf with the userdata it holds there, and exits at
// ip for anything else.
func (c *arm64Compiler) indexedObject(n int, up bool, ip int, buf Label) {
	a := &c.a
	o := reg(n)
	if up {
		c.upValueAddr(n)
		o = operand{rAddr, 0}
	}
	notTable := a.NewLabel()
	a.Ldr(rTmp, o.base, o.off+offN)
	a.MovImm(rTmp2, tagOf(vkTable))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, notTable)
	a.Ldr(rT, o.base, o.off+offP)
	c.branchNumber(rT, c.exit(ip)) // a number whose bits match the tag
	c.outOfLine = append(c.outOfLine, func() {
		a.Bind(notTable)
		a.MovImm(rTmp2, tagOf(vkUserData))
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, c.exit(ip))
		a.Ldr(rT, o.base, o.off+offP)
		c.branchNumber(rT, c.exit(ip))
		a.B(buf)
	})
}

// bufferElement checks that the userdata in rT is a buffer, with element
// rIdx+1 in range, as arrayIndex leaves the key less one, and exits at ip
// otherwise, for Go to read nil or raise the error. It leaves the index in
// rIdx and the first element's address in rSlot, and branches to the code
// for the buffer's kind.
func (c *arm64Compiler) bufferElement(ip int) (f64, f32, i32, u8 Label) {
	a := &c.a
	exit := c.exit(ip)
	a.Ldr(rT2, rT, offUDBuf)
	a.Cbz(rT2, exit) // a userdata that is not a buffer
	a.AddImm(rIdx, rIdx, 1)
	a.Ldr(rLen, rT2, offBufLen)
	a.Cmp(rIdx, rLen)
	a.BCond(HS, exit) // unsigned: below 0 too
	a.Ldr(rSlot, rT2, offBufPtr)
	a.Ldrb(rTmp, rT2, offBufKind)
	f64, f32, i32, u8 = a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Cbz(rTmp, f64)
	a.CmpImm(rTmp, uint32(bufferFloat32))
	a.BCond(EQ, f32)
	a.CmpImm(rTmp, uint32(bufferInt32))
	a.BCond(EQ, i32)
	a.B(u8)
	return
}

// getIndex compiles GETTABLE, or GETTABUP when up is set, for an integer
// key: an array element, nil past the array of a table without a hash
// part or a metatable, or a buffer's element. Other keys exit.
func (c *arm64Compiler) getIndex(ip int, i bytecode.Instruction, up bool) {
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
		integer := a.NewLabel()
		a.Bind(f64)
		a.AddShifted(rSlot, rSlot, rIdx, 3)
		a.Ldr(rN, rSlot, 0)
		a.Mov(rP, rNumber)
		a.B(store)
		a.Bind(f32) // stored from D0: see setIndex
		a.AddShifted(rSlot, rSlot, rIdx, 2)
		a.LdrS(0, rSlot, 0)
		a.FcvtSD(0, 0)
		c.guardStore(dst, noReg, ip)
		c.storeNumber(dst, 0)
		a.B(stored)
		a.Bind(i32)
		a.AddShifted(rSlot, rSlot, rIdx, 2)
		a.Ldrsw(rN, rSlot, 0)
		a.B(integer)
		a.Bind(u8)
		a.AddShifted(rSlot, rSlot, rIdx, 0)
		a.Ldrb(rN, rSlot, 0)
		a.Bind(integer)
		a.Mov(rP, rInteger)
		a.B(store)
	})
	// An array element, or nil past the array of a table without a hash
	// part or a metatable.
	outside, done := a.NewLabel(), a.NewLabel()
	a.Ldr(rLen, rT, offTArray+offSliceLen)
	a.Cmp(rIdx, rLen)
	a.BCond(HS, outside) // unsigned: keys below 1 too
	a.Ldr(rLen, rT, offTArray)
	a.AddShifted(rSlot, rLen, rIdx, 4)
	c.load(operand{rSlot, 0})
	c.absentIsNil(ip)
	a.B(done)
	a.Bind(outside)
	a.Ldr(rTmp, rT, offTHash)
	a.Cbnz(rTmp, c.exit(ip)) // the key may be there
	a.Ldr(rTmp, rT, offTMeta)
	a.Cbnz(rTmp, c.exit(ip))
	a.Mov(rP, ZR)
	a.Mov(rN, ZR)
	a.Bind(done)
	a.Bind(store)
	c.guardStore(dst, rP, ip)
	c.store(dst)
	a.Bind(stored)
}

// setIndex compiles SETTABLE, or SETTABUP when up is set, storing to an
// element of a table's array part, nil included, as tryPut, or put for a
// table without a metatable, does, or to a buffer's element. Other keys
// exit.
func (c *arm64Compiler) setIndex(ip int, i bytecode.Instruction, up bool) {
	a := &c.a
	if !c.loadRK(i.C(), ip, true) {
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
		// are stored from rN, or loaded again from src, rather than moved
		// between register files before the store, as on amd64.
		a.Bind(f64)
		a.AddShifted(rSlot, rSlot, rIdx, 3)
		integer64 := a.NewLabel()
		a.Cmp(rP, rNumber)
		a.BCond(NE, integer64)
		a.Str(rN, rSlot, 0)
		a.B(done)
		a.Bind(integer64)
		a.Cmp(rP, rInteger)
		a.BCond(NE, exit)
		a.Scvtf(0, rN)
		a.StrD(0, rSlot, 0)
		a.B(done)
		a.Bind(f32)
		integer32, convert := a.NewLabel(), a.NewLabel()
		a.Cmp(rP, rNumber)
		a.BCond(NE, integer32)
		a.LdrD(0, src.base, src.off+offN)
		a.B(convert)
		a.Bind(integer32)
		a.Cmp(rP, rInteger)
		a.BCond(NE, exit)
		a.Scvtf(0, rN)
		a.Bind(convert)
		a.FcvtDS(0, 0)
		a.AddShifted(rSlot, rSlot, rIdx, 2)
		a.StrS(0, rSlot, 0)
		a.B(done)
		a.Bind(i32)
		a.Cmp(rP, rInteger)
		a.BCond(NE, exit)
		a.AddShifted(rSlot, rSlot, rIdx, 2)
		a.StrW(rN, rSlot, 0)
		a.B(done)
		a.Bind(u8)
		a.Cmp(rP, rInteger)
		a.BCond(NE, exit)
		a.AddShifted(rSlot, rSlot, rIdx, 0)
		a.Strb(rN, rSlot, 0)
		a.B(done)
	})
	c.element(rT, offTArray, rIdx, rSlot, ip)
	// A nil element: setTableAt stores over it when there is no
	// metatable to consult for __newindex.
	slot, present := operand{rSlot, 0}, a.NewLabel()
	a.Ldr(rTmp, rSlot, offP)
	a.Cbnz(rTmp, present)
	a.Ldr(rTmp, rT, offTMeta)
	a.Cbnz(rTmp, c.exit(ip))
	a.Bind(present)
	c.guardStore(slot, rP, ip)
	c.store(slot)
	a.Strb(ZR, rT, offTFlags) // invalidateTagMethodCache
	a.Bind(done)
}

// call compiles CALL. A unary intrinsic applied to a number runs here, and
// a call to a compiled Lua function enters it directly. A call to any
// other Go function exits with jitExitCallGo, and runJIT makes it; other
// calls exit to the interpreter.
func (c *arm64Compiler) call(ip int, i bytecode.Instruction) {
	a := &c.a
	notGoFunction := a.NewLabel()
	if (i.B() == 2 || i.B() == 0) && (i.C() == 2 || i.C() == 0) { // one argument, one result
		// Out of line: the intrinsics' code would stand between a Lua
		// call's few instructions here and the rest in callLua.
		goFunction, fn := a.NewLabel(), reg(i.A())
		a.Ldr(rTmp, fn.base, fn.off+offN)
		a.MovImm(rTmp2, tagOf(vkGoFunction))
		a.Cmp(rTmp, rTmp2)
		a.BCond(EQ, goFunction)
		c.outOfLine = append(c.outOfLine, func() {
			a.Bind(goFunction)
			c.intrinsic(ip, i, notGoFunction)
		})
	}
	a.Bind(notGoFunction)
	c.notLua[ip] = c.a.NewLabel()
	c.callLua(ip, i, c.notLua[ip])
}

// goCallee exits with jitExitCallGo when the callee of the CALL i at ip is
// a Go function or Go closure, and otherwise falls through. stubs emits it
// out of line, after the Lua closure check, so calls between Lua functions
// neither run it nor have it in their way.
func (c *arm64Compiler) goCallee(ip int, i bytecode.Instruction) {
	a := &c.a
	fn := reg(i.A())
	notGo, closure := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkGoClosure))
	a.Cmp(rTmp, rTmp2)
	a.BCond(EQ, closure)
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, notGo)
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	a.Ldr(rTmp, rT, offGFNumber)
	a.Cbnz(rTmp, c.numCallExit(ip)) // runJIT may call it frameless
	a.Bind(c.plainGoCall(ip))
	c.mathCall(ip, i)
	a.B(c.goCallExit(ip))
	a.Bind(closure)
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	c.pairsCall(ip, i)
	a.B(c.goCallExit(ip))
	a.Bind(notGo)
}

// pairsCall computes the CALL i at ip inline when its callee, the Go
// closure in rT, is ipairs or pairs of one argument: the closure's
// iterator, the argument, and 0 or nil, which a generic for takes. pairs
// takes only a table without a metatable, which could hold __pairs; it
// calls Go for anything else. Other closures fall through.
func (c *arm64Compiler) pairsCall(ip int, i bytecode.Instruction) {
	if i.B() != 2 {
		return
	}
	a := &c.a
	goCall := c.goCallExit(ip)
	other, ipairs := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp, rT, 0) // the Function's code
	a.MovImm(rTmp2, callIPairs)
	a.Cmp(rTmp, rTmp2)
	a.BCond(EQ, ipairs)
	a.MovImm(rTmp2, callPairs)
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, other)
	arg := reg(i.A() + 1)
	a.Ldr(rTmp, arg.base, arg.off+offN)
	a.Lsr(rTmp, rTmp, kindShift)
	a.CmpImm(rTmp, uint32(vkTable))
	a.BCond(NE, goCall)
	a.Ldr(rTmp, arg.base, arg.off+offP)
	c.branchNumber(rTmp, goCall)
	a.Ldr(rTmp, rTmp, offTMeta)
	a.Cbnz(rTmp, goCall)
	c.pairsResults(ip, i, false)
	a.Bind(ipairs)
	c.pairsResults(ip, i, true)
	a.Bind(other)
}

// pairsResults stores ipairs' results, or pairs', for the CALL i at ip of
// the closure in rT: its iterator over the callee, the argument where it
// is, and then 0 for ipairs, and nil, as many as the call wants; then it
// goes on at ip+1.
func (c *arm64Compiler) pairsResults(ip int, i bytecode.Instruction, ipairs bool) {
	a := &c.a
	a.Cbnz(rBarrier, c.goCallExit(ip)) // it stores the iterator over the closure
	a.Ldr(rTmp, rT, offGCUpVals)
	c.load(operand{rTmp, 0})
	c.store(reg(i.A()))
	n := 4 // results
	if ipairs {
		n = 3
	}
	last := i.A() + i.C() - 1 // the register after those wanted
	if i.C() == 0 {
		last = i.A() + n
	}
	from := i.A() + 2 // nil from here
	if ipairs && last > from {
		c.storeInteger(reg(from), ZR)
		from++
	}
	for r := from; r < last; r++ {
		a.Str(ZR, rFrame, reg(r).off+offP)
		a.Str(ZR, rFrame, reg(r).off+offN)
	}
	if i.C() == 0 {
		c.setTop(last)
	}
	a.B(c.pcs[ip+1])
}

// tforCall compiles TFORCALL A C, a generic for's call of its iterator.
// For next, and for ipairs' iterator on a table whose metatable lacks
// __index, it steps through the table's array part itself: the index
// after the control variable and its value, or nil at the end when no
// other keys follow. Anything else exits, for Go to call the iterator.
func (c *arm64Compiler) tforCall(ip int, i bytecode.Instruction) {
	a := &c.a
	exit := c.exit(ip)
	fn, state, ctl := reg(i.A()), reg(i.A()+1), reg(i.A()+3)
	a.Cbnz(rBarrier, exit) // it stores values
	a.Ldr(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, exit)
	a.Ldr(rTmp, fn.base, fn.off+offP)
	c.branchNumber(rTmp, exit) // a number whose bits match the tag
	a.Ldr(rIdx, rTmp, 0)       // the Function's code
	// The state: a table, in rT, with its array's length in rN.
	a.Ldr(rTmp, state.base, state.off+offN)
	a.Lsr(rTmp, rTmp, kindShift)
	a.CmpImm(rTmp, uint32(vkTable))
	a.BCond(NE, exit)
	a.Ldr(rT, state.base, state.off+offP)
	c.branchNumber(rT, exit)
	a.Ldr(rN, rT, offTArray+offSliceLen)
	a.Ldr(rTmp, ctl.base, ctl.off+offP) // the control variable's first word
	ipairs, found, end, done := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(rTmp2, iterIPairs)
	a.Cmp(rIdx, rTmp2)
	a.BCond(EQ, ipairs)
	a.MovImm(rTmp2, iterNext)
	a.Cmp(rIdx, rTmp2)
	a.BCond(NE, exit)
	// next: the first non-nil element from the start, for a nil key, or
	// after an index in the array part.
	from, loop, rest := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(rIdx, 0)
	a.Cbz(rTmp, from)
	a.Cmp(rTmp, rInteger)
	a.BCond(NE, exit)
	a.Ldr(rIdx, ctl.base, ctl.off+offN)
	a.SubImm(rTmp2, rIdx, 1)
	a.Cmp(rTmp2, rN)
	a.BCond(HS, exit) // not in the array part
	a.Bind(from)      // rIdx: where to look from, 0-based
	a.Ldr(rP, rT, offTArray)
	a.Bind(loop)
	a.Cmp(rIdx, rN)
	a.BCond(HS, rest)
	a.AddShifted(rAddr, rP, rIdx, 4)
	a.Ldr(rTmp, rAddr, offP)
	a.Cbnz(rTmp, found)
	a.AddImm(rIdx, rIdx, 1)
	a.B(loop)
	a.Bind(rest) // string keys or a hash part may follow, which Go finds
	a.Ldr(rTmp, rT, offTShape)
	a.Cbnz(rTmp, exit)
	a.Ldr(rTmp, rT, offTHash)
	a.Cbnz(rTmp, exit)
	a.B(end)
	// ipairs' iterator: the element after the integer control variable,
	// until one is nil.
	a.Bind(ipairs)
	noMeta, inArray := a.NewLabel(), a.NewLabel()
	a.Ldr(rTmp2, rT, offTMeta)
	a.Cbz(rTmp2, noMeta)
	a.Ldrb(rAddr, rTmp2, offTFlags)
	a.Tbz(rAddr, uint32(tmIndex), exit) // the metatable may have __index
	a.Bind(noMeta)
	a.Cmp(rTmp, rInteger)
	a.BCond(NE, exit)
	a.Ldr(rIdx, ctl.base, ctl.off+offN) // the next index, 0-based
	a.Cmp(rIdx, rN)
	a.BCond(LO, inArray)
	a.Tbnz(rIdx, 63, exit) // an index below 1, which Go looks for in the hash part
	a.Ldr(rTmp, rT, offTHash)
	a.Cbnz(rTmp, exit)
	a.B(end)
	a.Bind(inArray)
	a.Ldr(rAddr, rT, offTArray)
	a.AddShifted(rAddr, rAddr, rIdx, 4)
	a.Ldr(rTmp, rAddr, offP)
	a.Cbz(rTmp, end)
	// The element at rAddr, index rIdx, 0-based: its key and value.
	a.Bind(found)
	c.load(operand{rAddr, 0})
	a.AddImm(rIdx, rIdx, 1)
	c.storeInteger(ctl, rIdx)
	if i.C() >= 2 {
		c.store(reg(i.A() + 4))
	}
	for r := i.A() + 5; r < i.A()+3+i.C(); r++ {
		a.Str(ZR, rFrame, reg(r).off+offP)
		a.Str(ZR, rFrame, reg(r).off+offN)
	}
	a.B(done)
	a.Bind(end)
	for r := i.A() + 3; r < i.A()+3+i.C(); r++ {
		a.Str(ZR, rFrame, reg(r).off+offP)
		a.Str(ZR, rFrame, reg(r).off+offN)
	}
	a.Bind(done)
}

// plainGoCall returns the label in goCallee's code for the CALL at ip
// when its callee is a Go function but not a number function.
func (c *arm64Compiler) plainGoCall(ip int) Label {
	if c.plainGo[ip] < 0 {
		c.plainGo[ip] = c.a.NewLabel()
	}
	return c.plainGo[ip]
}

// mathCall computes the CALL i at ip inline when its callee, a Go
// function, is a math function of its arity (see mathFn), writing the
// result to register A and going on at ip+1. It falls through for other
// functions, and exits to Go for arguments the function would reject or
// that it leaves to Go: mixed integers and floats for min and max.
func (c *arm64Compiler) mathCall(ip int, i bytecode.Instruction) {
	if i.C() != 2 && i.C() != 0 || i.B() != 2 && i.B() != 3 && i.B() != 0 {
		return
	}
	a := &c.a
	fn := reg(i.A())
	a.Ldr(rT, fn.base, fn.off+offP) // the *goFunction
	a.Ldr(rTmp, rT, 0)              // its Function's code
	var bodies []func()
	for _, m := range mathFns {
		if i.B() != 0 && m.id.unary() != (i.B() == 2) {
			continue
		}
		l := a.NewLabel()
		a.MovImm(rIdx, m.fn)
		a.Cmp(rTmp, rIdx)
		a.BCond(EQ, l)
		bodies = append(bodies, func() { a.Bind(l); c.mathBody(ip, i, m.id) })
	}
	skip := a.NewLabel()
	a.B(skip)
	for _, b := range bodies {
		b()
	}
	a.Bind(skip)
}

// mathBody computes math function m for the CALL i at ip, as mathCall
// describes.
func (c *arm64Compiler) mathBody(ip int, i bytecode.Instruction, m mathFn) {
	a := &c.a
	fn, arg, arg2 := reg(i.A()), reg(i.A()+1), reg(i.A()+2)
	goCall := c.goCallExit(ip)
	next := c.pcs[ip+1] // after the result is stored
	if i.C() == 0 {     // leaving l.top after it
		open := a.NewLabel()
		c.outOfLine = append(c.outOfLine, func() {
			a.Bind(open)
			c.resultTop(i)
			a.B(c.pcs[ip+1])
		})
		next = open
	}
	args := 1
	if !m.unary() {
		args = 2
	}
	c.checkArgs(i, args, goCall)
	isFloat, mixed := a.NewLabel(), a.NewLabel()
	// rTmp: 0 for a float argument, 1 for an integer; anything else calls
	// Go, which raises the error.
	a.Ldr(rTmp, arg.base, arg.off+offP)
	a.Sub(rTmp, rTmp, rNumber)
	if !m.unary() { // the same type, both
		a.Ldr(rTmp2, arg2.base, arg2.off+offP)
		a.Sub(rTmp2, rTmp2, rNumber)
		a.Cmp(rTmp, rTmp2)
		a.BCond(NE, mixed)
	}
	a.Cbz(rTmp, isFloat)
	a.CmpImm(rTmp, 1)
	a.BCond(NE, goCall)
	c.guardStore(fn, noReg, ip)
	// Integers.
	a.Ldr(rN, arg.base, arg.off+offN)
	switch m {
	case mathAbs:
		a.Neg(rTmp, rN) // minint stays minint
		a.CmpImm(rN, 0)
		a.Csel(rN, rTmp, rN, LT)
	case mathMin, mathMax:
		a.Ldr(rP, arg2.base, arg2.off+offN)
		if m == mathMin {
			a.Cmp(rP, rN) // the second if it is less
		} else {
			a.Cmp(rN, rP) // the second if the first is less
		}
		a.Csel(rN, rP, rN, LT)
	}
	c.storeInteger(reg(i.A()), rN) // floor and ceil: the integer itself
	a.B(next)
	// Floats.
	a.Bind(isFloat)
	c.guardStore(fn, noReg, ip)
	a.LdrD(0, arg.base, arg.off+offN)
	switch m {
	case mathFloor, mathCeil:
		if m == mathFloor {
			a.Frintm(0, 0)
		} else {
			a.Frintp(0, 0)
		}
		// An integer when one holds it, -2^63 <= f < 2^63, as
		// pushIntegerIfFits decides; otherwise the float.
		float := a.NewLabel()
		a.MovImm(rTmp, math.Float64bits(1<<63))
		a.FmovToF(1, rTmp)
		a.Fcmp(0, 1)
		a.BCond(VS, float) // NaN
		a.BCond(GE, float)
		a.MovImm(rTmp, math.Float64bits(-(1 << 63)))
		a.FmovToF(1, rTmp)
		a.Fcmp(0, 1)
		a.BCond(MI, float)
		a.Fcvtzs(rN, 0)
		c.storeInteger(fn, rN)
		a.B(next)
		a.Bind(float)
	case mathAbs:
		a.Fabs(0, 0)
	case mathMin, mathMax:
		// Lua's <, false for NaN either way: the first unless the second
		// is less (min), or the first is less than the second (max).
		keep := a.NewLabel()
		a.LdrD(1, arg2.base, arg2.off+offN)
		if m == mathMin {
			a.Fcmp(1, 0)
		} else {
			a.Fcmp(0, 1)
		}
		a.BCond(PL, keep) // not less, or unordered
		a.Fmov(0, 1)
		a.Bind(keep)
	}
	c.storeNumber(fn, 0)
	a.B(next)
	if m.unary() {
		return
	}
	// A float and an integer: the one chosen, keeping its type. They
	// compare as floats when the integer converts exactly, as Lua's
	// LTintfloat does; Go compares the rest.
	a.Bind(mixed)
	a.CmpImm(rTmp, 1)
	a.BCond(HI, goCall) // not a number
	a.CmpImm(rTmp2, 1)
	a.BCond(HI, goCall)
	intFirst, compare := a.NewLabel(), a.NewLabel()
	a.Cbnz(rTmp, intFirst)
	a.LdrD(0, arg.base, arg.off+offN)
	c.exactFloat(1, arg2, goCall)
	a.B(compare)
	a.Bind(intFirst)
	c.exactFloat(0, arg, goCall)
	a.LdrD(1, arg2.base, arg2.off+offN)
	a.Bind(compare)
	c.guardStore(fn, noReg, ip)
	first := a.NewLabel()
	if m == mathMin {
		a.Fcmp(1, 0)
	} else {
		a.Fcmp(0, 1)
	}
	a.BCond(PL, first) // not less, or unordered
	c.load(arg2)
	c.store(fn)
	a.B(next)
	a.Bind(first)
	c.load(arg)
	c.store(fn)
	a.B(next)
}

// exactFloat loads the integer at o into d as a float, branching to fail
// unless it converts exactly: -2^53 <= i <= 2^53. It uses rN, rTmp and
// rTmp2.
func (c *arm64Compiler) exactFloat(d FReg, o operand, fail Label) {
	a := &c.a
	a.Ldr(rN, o.base, o.off+offN)
	a.MovImm(rTmp, 1<<53)
	a.Add(rTmp, rTmp, rN)
	a.MovImm(rTmp2, 1<<54)
	a.Cmp(rTmp, rTmp2)
	a.BCond(HI, fail) // unsigned: outside
	a.Scvtf(d, rN)
}

// setList compiles SETLIST of a fixed count of values, as a constructor
// such as {a, b, c} ends, into the array part NEWTABLE sized for them. It
// exits when the array is too short, for jitStep to extend it, and while
// the write barrier is on.
func (c *arm64Compiler) setList(ip int, i bytecode.Instruction) {
	a := &c.a
	n, start := i.B(), (i.C()-1)*bytecode.ListItemsPerFlush
	if n == 0 || start+n > maxSetList { // values up to the stack top, which compiled code does not track
		c.exitAlways(ip)
		return
	}
	exit := c.exit(ip)
	c.tableOf(reg(i.A()), ip)
	a.Cbnz(rBarrier, exit)
	a.Ldr(rLen, rT, offTArray+offSliceLen)
	a.CmpImm(rLen, uint32(start+n))
	a.BCond(LT, exit)
	a.Ldr(rSlot, rT, offTArray)
	for k := range n {
		c.load(reg(i.A() + 1 + k))
		c.store(operand{rSlot, uint32(start+k) * valueSize})
	}
}

// intrinsic compiles a unary intrinsic call, branching to notGo when the
// callee is not a Go function.
func (c *arm64Compiler) intrinsic(ip int, i bytecode.Instruction, notGo Label) {
	a := &c.a
	fn, arg := reg(i.A()), reg(i.A()+1)
	a.Ldr(rTmp, fn.base, fn.off+offN)
	a.MovImm(rTmp2, tagOf(vkGoFunction))
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, notGo)
	a.Ldr(rT, fn.base, fn.off+offP)
	c.branchNumber(rT, notGo) // a number whose bits match the tag
	a.Ldr(rT, rT, offGFNumber)
	a.Cbz(rT, c.plainGoCall(ip)) // perhaps a math function
	a.Ldr(rT, rT, offNFUnary)
	c.checkArgs(i, 1, c.numCallExit(ip))
	c.loadFloat(0, arg, kindAny, false, ip) // an integer converts, as for a number function
	done := a.NewLabel()
	for _, in := range intrinsics {
		next := a.NewLabel()
		a.MovImm(rIdx, in.fn)
		a.Cmp(rT, rIdx)
		a.BCond(NE, next)
		in.emit(c, ip)
		a.B(done)
		a.Bind(next)
	}
	a.B(c.numCallExit(ip)) // a number function runJIT may call frameless
	a.Bind(done)
	c.guardStore(fn, noReg, ip)
	c.storeNumber(fn, 0)
	c.resultTop(i)
	a.B(c.pcs[ip+1])
}

// checkArgs branches to fail unless the CALL i has n arguments: up to
// l.top when it has B 0. It uses rTmp and rTmp2.
func (c *arm64Compiler) checkArgs(i bytecode.Instruction, n int, fail Label) {
	a := &c.a
	if i.B() != 0 {
		if i.B()-1 != n {
			a.B(fail)
		}
		return
	}
	a.Ldr(rTmp, rCtx, offCtxS)
	a.Ldr(rTmp2, rTmp, offStack)
	a.Ldr(rTmp, rTmp, offLTop)
	a.AddShifted(rTmp, rTmp2, rTmp, 4) // &stack[l.top]
	a.AddImm(rTmp2, rFrame, uint32(i.A()+1+n)*valueSize)
	a.Cmp(rTmp, rTmp2)
	a.BCond(NE, fail)
}

// resultTop, for the CALL i when it wants all results (C 0), leaves l.top
// after the one result it has stored in register A. It uses rTmp, rTmp2
// and rAddr.
func (c *arm64Compiler) resultTop(i bytecode.Instruction) {
	if i.C() == 0 {
		c.setTop(i.A() + 1)
	}
}

// setTop sets l.top to register r, using rTmp, rTmp2 and rAddr.
func (c *arm64Compiler) setTop(r int) {
	a := &c.a
	a.Ldr(rTmp, rCtx, offCtxS)
	a.Ldr(rAddr, rTmp, offStack)
	a.AddImm(rTmp2, rFrame, uint32(r)*valueSize)
	a.Sub(rTmp2, rTmp2, rAddr)
	a.Lsr(rTmp2, rTmp2, 4)
	a.Str(rTmp2, rTmp, offLTop)
}
