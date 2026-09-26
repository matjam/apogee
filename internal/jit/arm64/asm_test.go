package arm64

import (
	"encoding/binary"
	"errors"
	"testing"
)

// The expected words come from clang -c -arch arm64 on the assembly in each
// case's comment.
func TestEncodings(t *testing.T) {
	var a Asm
	a.MovImm(3, 0x0007_0000_abcd_1234) // movz x3,#0x1234; movk x3,#0xabcd,lsl #16; movk x3,#0x7,lsl #48
	a.Mov(5, 9)                        // mov x5, x9
	a.AddImm(1, 2, 4095)               // add x1, x2, #4095
	a.SubImm(4, 4, 1)                  // sub x4, x4, #1
	a.SubsImm(7, 8, 12)                // subs x7, x8, #12
	a.CmpImm(2, 0)                     // cmp x2, #0
	a.Cmp(3, 4)                        // cmp x3, x4
	a.AddShifted(6, 1, 2, 4)           // add x6, x1, x2, lsl #4
	a.Ldr(10, 1, 32760)                // ldr x10, [x1, #32760]
	a.Str(11, 2, 8)                    // str x11, [x2, #8]
	a.LdrW(12, 3, 16380)               // ldr w12, [x3, #16380]
	a.StrW(13, 4, 4)                   // str w13, [x4, #4]
	a.LdrD(0, 1, 24)                   // ldr d0, [x1, #24]
	a.StrD(31, 2, 32760)               // str d31, [x2, #32760]
	a.Ret()                            // ret
	a.Br(16)                           // br x16
	a.Fadd(1, 2, 3)                    // fadd d1, d2, d3
	a.Fsub(4, 5, 6)                    // fsub d4, d5, d6
	a.Fmul(7, 8, 9)                    // fmul d7, d8, d9
	a.Fdiv(10, 11, 12)                 // fdiv d10, d11, d12
	a.Fneg(13, 14)                     // fneg d13, d14
	a.Fabs(15, 16)                     // fabs d15, d16
	a.Fsqrt(17, 18)                    // fsqrt d17, d18
	a.Frintm(19, 20)                   // frintm d19, d20
	a.Frintp(21, 22)                   // frintp d21, d22
	a.Fcmp(23, 24)                     // fcmp d23, d24
	a.FmovToF(25, 26)                  // fmov d25, x26
	a.FmovFromF(27, 28)                // fmov x27, d28
	a.Fmov(29, 30)                     // fmov d29, d30
	want := []uint32{
		0xd2824683, 0xf2b579a3, 0xf2e000e3, 0xaa0903e5, 0x913ffc41, 0xd1000484,
		0xf1003107, 0xf100005f, 0xeb04007f, 0x8b021026, 0xf97ffc2a, 0xf900044b,
		0xb97ffc6c, 0xb900048d, 0xfd400c20, 0xfd3ffc5f, 0xd65f03c0, 0xd61f0200,
		0x1e632841, 0x1e6638a4, 0x1e690907, 0x1e6c196a, 0x1e6141cd, 0x1e60c20f,
		0x1e61c251, 0x1e654293, 0x1e64c2d5, 0x1e7822e0, 0x9e670359, 0x9e66039b,
		0x1e6043dd,
	}
	check(t, &a, want)
}

func TestConversions(t *testing.T) {
	var a Asm
	a.Strb(ZR, 3, 4095) // strb wzr, [x3, #4095]
	a.Strb(5, 6, 1)     // strb w5, [x6, #1]
	a.Fcvtzs(7, 8)      // fcvtzs x7, d8
	a.Scvtf(9, 10)      // scvtf d9, x10
	a.Fmadd(1, 2, 3, 4) // fmadd d1, d2, d3, d4
	a.Fmsub(5, 6, 7, 8) // fmsub d5, d6, d7, d8
	a.Fcvtzu(9, 10)     // fcvtzu x9, d10
	a.Ucvtf(11, 12)     // ucvtf d11, x12
	a.Ldrb(1, 2, 4095)  // ldrb w1, [x2, #4095]
	a.Lsr(3, 4, 4)      // lsr x3, x4, #4
	a.Sub(5, 6, 7)      // sub x5, x6, x7
	check(t, &a, []uint32{
		0x393ffc7f, 0x390004c5, 0x9e780107, 0x9e620149,
		0x1f431041, 0x1f47a0c5, 0x9e790149, 0x9e63018b,
		0x397ffc41, 0xd344fc83, 0xcb0700c5,
	})
}

func TestIntegerArithmetic(t *testing.T) {
	var a Asm
	a.Add(5, 6, 7)      // add x5, x6, x7
	a.Mul(5, 6, 7)      // mul x5, x6, x7
	a.Sdiv(5, 6, 7)     // sdiv x5, x6, x7
	a.Udiv(5, 6, 7)     // udiv x5, x6, x7
	a.Msub(5, 6, 7, 8)  // msub x5, x6, x7, x8
	a.Neg(5, 6)         // neg x5, x6
	a.And(5, 6, 7)      // and x5, x6, x7
	a.Orr(5, 6, 7)      // orr x5, x6, x7
	a.Eor(5, 6, 7)      // eor x5, x6, x7
	a.Lslv(5, 6, 7)     // lsl x5, x6, x7
	a.Lsrv(5, 6, 7)     // lsr x5, x6, x7
	a.Mvn(5, 6)         // mvn x5, x6
	a.Csel(5, 6, 7, LT) // csel x5, x6, x7, lt
	check(t, &a, []uint32{
		0x8b0700c5, 0x9b077cc5, 0x9ac70cc5, 0x9ac708c5, 0x9b07a0c5, 0xcb0603e5,
		0x8a0700c5, 0xaa0700c5, 0xca0700c5, 0x9ac720c5, 0x9ac724c5, 0xaa2603e5,
		0x9a87b0c5,
	})
}

// Buffer elements: float32 and sign-extended 32-bit loads and stores.
func TestNarrowElements(t *testing.T) {
	var a Asm
	a.LdrS(3, 14, 0)   // ldr s3, [x14]
	a.LdrS(0, 21, 16)  // ldr s0, [x21, #16]
	a.StrS(1, 21, 0)   // str s1, [x21]
	a.StrS(31, 2, 8)   // str s31, [x2, #8]
	a.FcvtSD(0, 3)     // fcvt d0, s3
	a.FcvtSD(17, 2)    // fcvt d17, s2
	a.FcvtDS(0, 0)     // fcvt s0, d0
	a.FcvtDS(5, 9)     // fcvt s5, d9
	a.Ldrsw(13, 21, 0) // ldrsw x13, [x21]
	a.Ldrsw(2, 4, 12)  // ldrsw x2, [x4, #12]
	check(t, &a, []uint32{0xbd4001c3, 0xbd4012a0, 0xbd0002a1, 0xbd00085f, 0x1e22c060, 0x1e22c051,
		0x1e624000, 0x1e624125, 0xb98002ad, 0xb9800c82})
}

func TestFrintz(t *testing.T) {
	var a Asm
	a.Frintz(2, 7) // frintz d2, d7
	a.Frintz(0, 0) // frintz d0, d0
	check(t, &a, []uint32{0x1e65c0e2, 0x1e65c000})
}

func TestDivideByConstant(t *testing.T) {
	var a Asm
	a.Smulh(3, 20, 9) // smulh x3, x20, x9
	a.Smulh(0, 1, 2)  // smulh x0, x1, x2
	a.Asr(5, 6, 3)    // asr x5, x6, #3
	a.Asr(28, 1, 63)  // asr x28, x1, #63
	check(t, &a, []uint32{0x9b497e83, 0x9b427c20, 0x9343fcc5, 0x937ffc3c})
}

func TestBranches(t *testing.T) {
	var a Asm
	fwd, back := a.NewLabel(), a.NewLabel()
	a.B(fwd)         // b 1f
	a.BCond(NE, fwd) // b.ne 1f
	a.Cbz(3, fwd)    // cbz x3, 1f
	a.Cbnz(4, back)  // cbnz x4, 2f
	a.Bind(fwd)      // 1:
	a.Ret()
	a.BCond(MI, fwd) // b.mi 1b
	a.Bind(back)     // 2:
	a.Ret()
	want := []uint32{0x14000004, 0x54000061, 0xb4000043, 0xb5000064, 0xd65f03c0, 0x54ffffe4, 0xd65f03c0}
	check(t, &a, want)
}

func TestTestBranches(t *testing.T) {
	var a Asm
	l := a.NewLabel()
	a.Tbz(13, 0, l)  // tbz x13, #0, 1f
	a.Tbnz(7, 63, l) // tbnz x7, #63, 1f
	a.Bind(l)        // 1:
	a.Ret()
	a.Tbz(2, 3, l) // tbz w2, #3, 1b
	check(t, &a, []uint32{0x3600004d, 0xb7f80027, 0xd65f03c0, 0x361fffe2})
}

func TestLongTestBranches(t *testing.T) {
	a := Asm{LongTests: true}
	l := a.NewLabel()
	a.Tbz(13, 0, l)  // tbnz x13, #0, 1f; b 3f; 1:
	a.Tbnz(7, 63, l) // tbz x7, #63, 2f; b 3f; 2:
	a.Bind(l)        // 3:
	a.Ret()
	check(t, &a, []uint32{0x3700004d, 0x14000003, 0xb6f80047, 0x14000001, 0xd65f03c0})
}

// A test branch beyond ±32 KB is ErrTestRange, and LongTests reaches it.
func TestTestBranchRange(t *testing.T) {
	for _, long := range []bool{false, true} {
		a := Asm{LongTests: long}
		l := a.NewLabel()
		a.Tbz(1, 0, l)
		for range 1 << 13 {
			a.Ret()
		}
		a.Bind(l)
		if _, err := a.Code(); long && err != nil || !long && !errors.Is(err, ErrTestRange) {
			t.Errorf("LongTests %v: Code gave %v", long, err)
		}
	}
}

func TestMovImm(t *testing.T) {
	tests := []struct {
		v    uint64
		want []uint32
	}{
		{0, []uint32{0xd2800000}},
		{0xffff, []uint32{0xd29fffe0}},
		{1 << 48, []uint32{0xd2e00020}},
	}
	for _, tt := range tests {
		var a Asm
		a.MovImm(0, tt.v)
		check(t, &a, tt.want)
	}
}

func TestUnboundLabel(t *testing.T) {
	var a Asm
	a.B(a.NewLabel())
	if _, err := a.Code(); err == nil {
		t.Fatal("Code succeeded with an unbound label")
	}
}

func check(t *testing.T, a *Asm, want []uint32) {
	t.Helper()
	code, err := a.Code()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 4*len(want) {
		t.Fatalf("got %d instructions, want %d", len(code)/4, len(want))
	}
	for i, w := range want {
		if got := binary.LittleEndian.Uint32(code[4*i:]); got != w {
			t.Errorf("instruction %d = %#08x, want %#08x", i, got, w)
		}
	}
}
