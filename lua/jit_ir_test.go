//go:build (darwin || linux) && (arm64 || amd64)

package lua

import (
	"strings"
	"testing"

	"github.com/matjam/apogee/internal/bytecode"
)

// irOf returns the IR of the integer kernel for the first numeric for loop
// in src's function run, allocated with at most maxFloats and maxInts
// registers, and whether allocation succeeded.
func irOf(t *testing.T, src string, maxFloats, maxInts int) (*irFunc, bool) {
	t.Helper()
	l := NewState()
	openLibraries(l)
	if err := l.DoString(src); err != nil {
		t.Fatal(err)
	}
	l.Global("run")
	p := l.ToValue(-1).(*luaClosure).prototype
	for ip, i := range p.Code {
		if i.OpCode() != bytecode.OpForLoop {
			continue
		}
		plan := planKernel(p, ip, true, func(int) bool { return true },
			func(int) (uint64, mathFn, bool) { return 0, mathNone, false },
			func(int) (numKind, bool) { return kindAny, false }, true)
		if plan == nil {
			t.Fatal("the loop is not a kernel")
		}
		f := buildIR(p, plan)
		return f, f.allocate(maxFloats, maxInts)
	}
	t.Fatal("no numeric for loop")
	return nil, false
}

func TestIRBuild(t *testing.T) {
	f, ok := irOf(t, `function run() local s = 0.0; for i = 1, 100 do local a = i * 2; local b = a / 3; if a > 10 then s = s + b end end; return s end`, 9, 7)
	if !ok {
		t.Fatal("allocation failed")
	}
	ir := f.String()
	for _, want := range []string{"mul i", "div f", "branch", "add f"} {
		if !strings.Contains(ir, want) {
			t.Errorf("IR lacks %q:\n%s", want, ir)
		}
	}
	// The loop variable shares the index's virtual register.
	if f.loop[3] != f.loop[0] {
		t.Errorf("loop variable %d, index %d", f.loop[3], f.loop[0])
	}
}

func TestIRTemporariesShare(t *testing.T) {
	// Four temporaries, each dead once the next is made, and the
	// accumulator: they fit in the loop's four integer registers and two more.
	src := `function run() local s = 0; for i = 1, 100 do local a = i + 1; local b = a + 1; local c = b + 1; local d = c + 1; s = s + d end; return s end`
	if _, ok := irOf(t, src, 0, 6); !ok {
		t.Error("temporaries did not share registers")
	}
	if _, ok := irOf(t, src, 0, 4); ok {
		t.Error("allocated the accumulator and a temporary into the loop's registers")
	}
}
