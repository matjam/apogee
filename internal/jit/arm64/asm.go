// Package arm64 encodes the subset of A64 instructions the JIT emits.
//
// Methods append one instruction each. Branches take labels, which are
// resolved by Code. Encodings follow the Arm Architecture Reference Manual;
// asm_test.go checks them against the system assembler.
package arm64

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Reg is a general-purpose register, X0 to X30. ZR is the zero register,
// and SP the stack pointer in the instructions that accept it.
type Reg uint8

// FReg is a floating-point register, D0 to D31.
type FReg uint8

const (
	ZR Reg = 31
	SP Reg = 31
)

// Cond is a condition code.
type Cond uint8

const (
	EQ Cond = 0
	NE Cond = 1
	HS Cond = 2
	LO Cond = 3
	MI Cond = 4
	PL Cond = 5
	VS Cond = 6
	VC Cond = 7
	HI Cond = 8
	LS Cond = 9
	GE Cond = 10
	LT Cond = 11
	GT Cond = 12
	LE Cond = 13
)

// Label marks a position in the code.
type Label int

type fixupKind uint8

const (
	fixB26    fixupKind = iota // B, BL
	fixCond19                  // B.cond, CBZ, CBNZ
	fixTest14                  // TBZ, TBNZ
)

type fixup struct {
	at    int // instruction index
	label Label
	kind  fixupKind
}

// Asm accumulates instructions.
type Asm struct {
	words  []uint32
	labels []int // instruction index, or -1 while unbound
	fixups []fixup
	// LongTests makes Tbz and Tbnz reach any label, as two instructions:
	// the opposite test, skipping an unconditional branch to the label.
	// Without it they reach ±32 KB, and Code reports ErrTestRange for one
	// that does not.
	LongTests bool
}

// ErrTestRange is Code's error for a Tbz or Tbnz farther than ±32 KB from
// its label, which LongTests allows.
var ErrTestRange = errors.New("arm64: test branch out of range")

// ErrRange is Code's error for another branch farther than it reaches:
// ±1 MB for a conditional branch, ±128 MB for another, in code too long
// to compile.
var ErrRange = errors.New("arm64: branch out of range")

// Len returns the number of instructions emitted so far.
func (a *Asm) Len() int { return len(a.words) }

// NewLabel returns an unbound label.
func (a *Asm) NewLabel() Label {
	a.labels = append(a.labels, -1)
	return Label(len(a.labels) - 1)
}

// Bind binds l to the next instruction.
func (a *Asm) Bind(l Label) { a.labels[l] = len(a.words) }

// Offset returns the byte offset of bound label l.
func (a *Asm) Offset(l Label) int { return a.labels[l] * 4 }

// Code resolves branches and returns the machine code.
func (a *Asm) Code() ([]byte, error) {
	for _, f := range a.fixups {
		target := a.labels[f.label]
		if target < 0 {
			return nil, fmt.Errorf("arm64: label %d is not bound", f.label)
		}
		delta := target - f.at
		switch f.kind {
		case fixB26:
			if delta < -(1<<25) || delta >= 1<<25 {
				return nil, ErrRange
			}
			a.words[f.at] |= uint32(delta) & (1<<26 - 1)
		case fixCond19:
			if delta < -(1<<18) || delta >= 1<<18 {
				return nil, ErrRange
			}
			a.words[f.at] |= (uint32(delta) & (1<<19 - 1)) << 5
		case fixTest14:
			if delta < -(1<<13) || delta >= 1<<13 {
				return nil, ErrTestRange
			}
			a.words[f.at] |= (uint32(delta) & (1<<14 - 1)) << 5
		}
	}
	b := make([]byte, 0, len(a.words)*4)
	for _, w := range a.words {
		b = binary.LittleEndian.AppendUint32(b, w)
	}
	return b, nil
}

func (a *Asm) emit(w uint32) { a.words = append(a.words, w) }

func (a *Asm) branch(w uint32, l Label, k fixupKind) {
	a.fixups = append(a.fixups, fixup{at: len(a.words), label: l, kind: k})
	a.emit(w)
}

func (r Reg) u() uint32  { return uint32(r) }
func (r FReg) u() uint32 { return uint32(r) }

// MovImm loads a 64-bit constant into rd with MOVZ and MOVK.
func (a *Asm) MovImm(rd Reg, v uint64) {
	first := true
	for hw := uint32(0); hw < 4; hw++ {
		part := uint32(v>>(16*hw)) & 0xffff
		if part == 0 {
			continue
		}
		if first {
			a.emit(0xd2800000 | hw<<21 | part<<5 | rd.u()) // MOVZ
			first = false
		} else {
			a.emit(0xf2800000 | hw<<21 | part<<5 | rd.u()) // MOVK
		}
	}
	if first {
		a.emit(0xd2800000 | rd.u()) // MOVZ rd, #0
	}
}

// Mov copies rm to rd.
func (a *Asm) Mov(rd, rm Reg) { a.emit(0xaa0003e0 | rm.u()<<16 | rd.u()) }

// AddImm computes rd = rn + imm, for imm in [0, 4095].
func (a *Asm) AddImm(rd, rn Reg, imm uint32) {
	a.emit(0x91000000 | checkImm12(imm)<<10 | rn.u()<<5 | rd.u())
}

// SubImm computes rd = rn - imm, for imm in [0, 4095].
func (a *Asm) SubImm(rd, rn Reg, imm uint32) {
	a.emit(0xd1000000 | checkImm12(imm)<<10 | rn.u()<<5 | rd.u())
}

// SubsImm computes rd = rn - imm and sets the flags.
func (a *Asm) SubsImm(rd, rn Reg, imm uint32) {
	a.emit(0xf1000000 | checkImm12(imm)<<10 | rn.u()<<5 | rd.u())
}

// CmpImm compares rn with imm.
func (a *Asm) CmpImm(rn Reg, imm uint32) { a.SubsImm(ZR, rn, imm) }

// Cmp compares rn with rm.
func (a *Asm) Cmp(rn, rm Reg) { a.emit(0xeb000000 | rm.u()<<16 | rn.u()<<5 | ZR.u()) }

// AddShifted computes rd = rn + rm<<shift.
func (a *Asm) AddShifted(rd, rn, rm Reg, shift uint32) {
	a.emit(0x8b000000 | rm.u()<<16 | (shift&63)<<10 | rn.u()<<5 | rd.u())
}

// Ldr loads the 64-bit word at rn+off, where off is a multiple of 8 below
// 32768.
func (a *Asm) Ldr(rt, rn Reg, off uint32) {
	a.emit(0xf9400000 | scaled(off, 8)<<10 | rn.u()<<5 | rt.u())
}

// Str stores rt at rn+off, where off is a multiple of 8 below 32768.
func (a *Asm) Str(rt, rn Reg, off uint32) {
	a.emit(0xf9000000 | scaled(off, 8)<<10 | rn.u()<<5 | rt.u())
}

// LdrW loads the 32-bit word at rn+off, zero-extended.
func (a *Asm) LdrW(rt, rn Reg, off uint32) {
	a.emit(0xb9400000 | scaled(off, 4)<<10 | rn.u()<<5 | rt.u())
}

// StrW stores the low 32 bits of rt at rn+off.
func (a *Asm) StrW(rt, rn Reg, off uint32) {
	a.emit(0xb9000000 | scaled(off, 4)<<10 | rn.u()<<5 | rt.u())
}

// Ldrb loads the byte at rn+off into rt, zero-extended.
func (a *Asm) Ldrb(rt, rn Reg, off uint32) {
	a.emit(0x39400000 | checkImm12(off)<<10 | rn.u()<<5 | rt.u())
}

// Lsr computes rd = rn >> shift, unsigned.
func (a *Asm) Lsr(rd, rn Reg, shift uint32) {
	a.emit(0xd340fc00 | (shift&63)<<16 | rn.u()<<5 | rd.u())
}

// Asr shifts rn right, signed, by shift into rd.
func (a *Asm) Asr(rd, rn Reg, shift uint32) {
	a.emit(0x9340fc00 | (shift&63)<<16 | rn.u()<<5 | rd.u())
}

// Sub computes rd = rn - rm.
func (a *Asm) Sub(rd, rn, rm Reg) { a.emit(0xcb000000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Add computes rd = rn + rm.
func (a *Asm) Add(rd, rn, rm Reg) { a.emit(0x8b000000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Neg computes rd = -rm.
func (a *Asm) Neg(rd, rm Reg) { a.Sub(rd, ZR, rm) }

// Mul computes rd = rn * rm, keeping the low 64 bits.
func (a *Asm) Mul(rd, rn, rm Reg) { a.emit(0x9b007c00 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Smulh computes rd = the high 64 bits of the signed 128-bit rn * rm.
func (a *Asm) Smulh(rd, rn, rm Reg) { a.emit(0x9b407c00 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Msub computes rd = ra - rn*rm.
func (a *Asm) Msub(rd, rn, rm, ra Reg) {
	a.emit(0x9b008000 | rm.u()<<16 | ra.u()<<10 | rn.u()<<5 | rd.u())
}

// Sdiv and Udiv compute rd = rn / rm, signed and unsigned, rounding toward
// zero. Dividing by zero gives zero.
func (a *Asm) Sdiv(rd, rn, rm Reg) { a.emit(0x9ac00c00 | rm.u()<<16 | rn.u()<<5 | rd.u()) }
func (a *Asm) Udiv(rd, rn, rm Reg) { a.emit(0x9ac00800 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// And, Orr and Eor compute rd = rn & rm, rn | rm and rn ^ rm.
func (a *Asm) And(rd, rn, rm Reg) { a.emit(0x8a000000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }
func (a *Asm) Orr(rd, rn, rm Reg) { a.emit(0xaa000000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }
func (a *Asm) Eor(rd, rn, rm Reg) { a.emit(0xca000000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Mvn computes rd = ^rm.
func (a *Asm) Mvn(rd, rm Reg) { a.emit(0xaa2003e0 | rm.u()<<16 | rd.u()) }

// Lslv and Lsrv shift rn left or right, logically, by rm mod 64.
func (a *Asm) Lslv(rd, rn, rm Reg) { a.emit(0x9ac02000 | rm.u()<<16 | rn.u()<<5 | rd.u()) }
func (a *Asm) Lsrv(rd, rn, rm Reg) { a.emit(0x9ac02400 | rm.u()<<16 | rn.u()<<5 | rd.u()) }

// Csel computes rd = rn if c holds, else rm.
func (a *Asm) Csel(rd, rn, rm Reg, c Cond) {
	a.emit(0x9a800000 | rm.u()<<16 | uint32(c)<<12 | rn.u()<<5 | rd.u())
}

// Strb stores the low byte of rt at rn+off.
func (a *Asm) Strb(rt, rn Reg, off uint32) {
	a.emit(0x39000000 | checkImm12(off)<<10 | rn.u()<<5 | rt.u())
}

// LdrD loads the double at rn+off.
func (a *Asm) LdrD(ft FReg, rn Reg, off uint32) {
	a.emit(0xfd400000 | scaled(off, 8)<<10 | rn.u()<<5 | ft.u())
}

// StrD stores the double ft at rn+off.
func (a *Asm) StrD(ft FReg, rn Reg, off uint32) {
	a.emit(0xfd000000 | scaled(off, 8)<<10 | rn.u()<<5 | ft.u())
}

// LdrS loads the float32 at rn+off into the low bits of ft.
func (a *Asm) LdrS(ft FReg, rn Reg, off uint32) {
	a.emit(0xbd400000 | scaled(off, 4)<<10 | rn.u()<<5 | ft.u())
}

// StrS stores the float32 in the low bits of ft at rn+off.
func (a *Asm) StrS(ft FReg, rn Reg, off uint32) {
	a.emit(0xbd000000 | scaled(off, 4)<<10 | rn.u()<<5 | ft.u())
}

// Ldrsw loads the 32-bit word at rn+off into rt, sign-extended.
func (a *Asm) Ldrsw(rt, rn Reg, off uint32) {
	a.emit(0xb9800000 | scaled(off, 4)<<10 | rn.u()<<5 | rt.u())
}

// FcvtSD converts the float32 in fn to a double in fd; FcvtDS rounds the
// double in fn to a float32 in fd.
func (a *Asm) FcvtSD(fd, fn FReg) { a.emit(0x1e22c000 | fn.u()<<5 | fd.u()) }
func (a *Asm) FcvtDS(fd, fn FReg) { a.emit(0x1e624000 | fn.u()<<5 | fd.u()) }

// B branches to l.
func (a *Asm) B(l Label) { a.branch(0x14000000, l, fixB26) }

// BCond branches to l when c holds.
func (a *Asm) BCond(c Cond, l Label) { a.branch(0x54000000|uint32(c), l, fixCond19) }

// Cbz branches to l when rt is zero.
func (a *Asm) Cbz(rt Reg, l Label) { a.branch(0xb4000000|rt.u(), l, fixCond19) }

// Cbnz branches to l when rt is not zero.
func (a *Asm) Cbnz(rt Reg, l Label) { a.branch(0xb5000000|rt.u(), l, fixCond19) }

// Tbz branches to l when bit b of rt is zero.
func (a *Asm) Tbz(rt Reg, b uint32, l Label) { a.test(0x36000000|testBit(b)|rt.u(), l) }

// Tbnz branches to l when bit b of rt is one.
func (a *Asm) Tbnz(rt Reg, b uint32, l Label) { a.test(0x37000000|testBit(b)|rt.u(), l) }

// test emits the TBZ or TBNZ w to l, or with LongTests the other one past
// a B to l. Bit 24 tells them apart.
func (a *Asm) test(w uint32, l Label) {
	if !a.LongTests {
		a.branch(w, l, fixTest14)
		return
	}
	a.emit(w ^ 1<<24 | 2<<5) // two instructions on: past the B
	a.B(l)
}

func testBit(b uint32) uint32 { return (b>>5)<<31 | (b&31)<<19 }

// Ret returns through the link register.
func (a *Asm) Ret() { a.emit(0xd65f03c0) }

// Br branches to the address in rn.
func (a *Asm) Br(rn Reg) { a.emit(0xd61f0000 | rn.u()<<5) }

// Fadd computes fd = fn + fm.
func (a *Asm) Fadd(fd, fn, fm FReg) { a.emit(0x1e602800 | fm.u()<<16 | fn.u()<<5 | fd.u()) }

// Fsub computes fd = fn - fm.
func (a *Asm) Fsub(fd, fn, fm FReg) { a.emit(0x1e603800 | fm.u()<<16 | fn.u()<<5 | fd.u()) }

// Fmul computes fd = fn * fm.
func (a *Asm) Fmul(fd, fn, fm FReg) { a.emit(0x1e600800 | fm.u()<<16 | fn.u()<<5 | fd.u()) }

// Fdiv computes fd = fn / fm.
func (a *Asm) Fdiv(fd, fn, fm FReg) { a.emit(0x1e601800 | fm.u()<<16 | fn.u()<<5 | fd.u()) }

// Fneg computes fd = -fn.
func (a *Asm) Fneg(fd, fn FReg) { a.emit(0x1e614000 | fn.u()<<5 | fd.u()) }

// Fabs computes fd = |fn|.
func (a *Asm) Fabs(fd, fn FReg) { a.emit(0x1e60c000 | fn.u()<<5 | fd.u()) }

// Fsqrt computes fd = sqrt(fn).
func (a *Asm) Fsqrt(fd, fn FReg) { a.emit(0x1e61c000 | fn.u()<<5 | fd.u()) }

// Frintm rounds fn toward minus infinity.
func (a *Asm) Frintm(fd, fn FReg) { a.emit(0x1e654000 | fn.u()<<5 | fd.u()) }

// Frintp rounds fn toward plus infinity.
func (a *Asm) Frintp(fd, fn FReg) { a.emit(0x1e64c000 | fn.u()<<5 | fd.u()) }

// Frintz rounds fn toward zero into fd.
func (a *Asm) Frintz(fd, fn FReg) { a.emit(0x1e65c000 | fn.u()<<5 | fd.u()) }

// Fcmp compares fn with fm. After it, MI means less, LS less or equal, GT
// greater and GE greater or equal, all false when either is NaN; EQ is
// equal and NE is not equal or unordered.
func (a *Asm) Fcmp(fn, fm FReg) { a.emit(0x1e602000 | fm.u()<<16 | fn.u()<<5) }

// FmovToF copies the bits of rn to fd.
func (a *Asm) FmovToF(fd FReg, rn Reg) { a.emit(0x9e670000 | rn.u()<<5 | fd.u()) }

// FmovFromF copies the bits of fn to rd.
func (a *Asm) FmovFromF(rd Reg, fn FReg) { a.emit(0x9e660000 | fn.u()<<5 | rd.u()) }

// Fmadd computes fd = fa + fn*fm with a single rounding.
func (a *Asm) Fmadd(fd, fn, fm, fa FReg) {
	a.emit(0x1f400000 | fm.u()<<16 | fa.u()<<10 | fn.u()<<5 | fd.u())
}

// Fmsub computes fd = fa - fn*fm with a single rounding.
func (a *Asm) Fmsub(fd, fn, fm, fa FReg) {
	a.emit(0x1f408000 | fm.u()<<16 | fa.u()<<10 | fn.u()<<5 | fd.u())
}

// Fcvtzu converts fn to an unsigned integer in rd, rounding toward zero
// and saturating, as Go's uint64(f) does on arm64.
func (a *Asm) Fcvtzu(rd Reg, fn FReg) { a.emit(0x9e790000 | fn.u()<<5 | rd.u()) }

// Ucvtf converts the unsigned integer in rn to a double in fd.
func (a *Asm) Ucvtf(fd FReg, rn Reg) { a.emit(0x9e630000 | rn.u()<<5 | fd.u()) }

// Fcvtzs converts fn to a signed integer in rd, rounding toward zero and
// saturating, as Go's int(f) does on arm64.
func (a *Asm) Fcvtzs(rd Reg, fn FReg) { a.emit(0x9e780000 | fn.u()<<5 | rd.u()) }

// Scvtf converts the signed integer in rn to a double in fd.
func (a *Asm) Scvtf(fd FReg, rn Reg) { a.emit(0x9e620000 | rn.u()<<5 | fd.u()) }

// Fmov copies fn to fd.
func (a *Asm) Fmov(fd, fn FReg) { a.emit(0x1e604000 | fn.u()<<5 | fd.u()) }

func checkImm12(imm uint32) uint32 {
	if imm > 4095 {
		panic(fmt.Sprintf("arm64: immediate %d out of range", imm))
	}
	return imm
}

func scaled(off, size uint32) uint32 {
	if off%size != 0 || off/size > 4095 {
		panic(fmt.Sprintf("arm64: offset %d not encodable for %d-byte access", off, size))
	}
	return off / size
}
