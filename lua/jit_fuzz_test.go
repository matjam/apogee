package lua

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestJITFuzz runs random loops, built from the idioms scripts use, with
// and without the JIT, and fails when they disagree. It also reports the
// instructions that leave compiled code on every iteration, and fails on
// any listed in jitMustNotExit: a loop that exits each time runs slower
// than the interpreter. APOGEE_FUZZ_N sets how many programs (default
// 300) and APOGEE_FUZZ_SEED the first seed.
func TestJITFuzz(t *testing.T) {
	skipWithoutJIT(t)
	n, seed := 300, uint64(1)
	if v, err := strconv.Atoi(os.Getenv("APOGEE_FUZZ_N")); err == nil {
		n = v
	}
	if v, err := strconv.ParseUint(os.Getenv("APOGEE_FUZZ_SEED"), 10, 64); err == nil {
		seed = v
	}
	hot := map[string]int{} // exiting instruction kinds, by programs exiting every iteration
	for s := seed; s < seed+uint64(n); s++ {
		src := fuzzProgram(s)
		const iterations = 200
		exits := map[string]int{}
		lines := strings.Count(src, "\n") // fuzzReport's functions start after
		jitExitHook = func(p *prototype, ip int, reason uint64) {
			if p.LineDefined <= lines {
				exits[exitKind(p, ip, reason)]++
			}
		}
		jit, interp := runFuzz(t, src, iterations)
		jitExitHook = nil
		if jit != interp {
			t.Fatalf("seed %d: JIT and interpreter disagree\nprogram:\n%s\nJIT:    %s\ninterp: %s", s, src, jit, interp)
		}
		for kind, count := range exits {
			if count >= iterations { // at least once an iteration
				hot[kind]++
				if slices.ContainsFunc(jitMustNotExit, func(p string) bool { return strings.HasPrefix(kind, p) }) {
					t.Errorf("seed %d: %s exits on every iteration (%d times)\n%s", s, kind, count, src)
				}
			}
		}
	}
	kinds := make([]string, 0, len(hot))
	for k := range hot {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(a, b string) int { return hot[b] - hot[a] })
	for _, k := range kinds {
		t.Logf("%4d programs exit every iteration at %s", hot[k], k)
	}
}

// jitMustNotExit lists the exits, by the start of exitKind's description,
// no fuzz program may take on every iteration: Lua calls and returns of
// any number of values, # of a table, == and % of numbers, the math
// functions, ipairs and pairs, which are the only Go functions called,
// generic for loops over their arrays, including the JMP that closes one,
// VARARG, and stores of nil into arrays.
var jitMustNotExit = []string{"CALL B=", "RETURN B=", "LEN", "EQ", "MOD", "CALL (Go function)", "TFORCALL", "JMP",
	"VARARG", "SETTABLE", "SETTABUP"}

// exitKind describes an exit for the report: the instruction's name and
// what distinguishes its exits.
func exitKind(p *prototype, ip int, reason uint64) string {
	i := p.Code[ip] // the original instruction: jitOrig's may be specialised
	name := strings.Fields(i.String())[0]
	switch reason {
	case jitExitBudget:
		return "budget"
	case jitExitCallGo:
		return name + " (Go function)"
	case jitExitCallNumber:
		return name + " (number function)"
	}
	switch name {
	case "CALL", "TAILCALL":
		return fmt.Sprintf("%s B=%d C=%d", name, i.B(), i.C())
	case "RETURN":
		return fmt.Sprintf("%s B=%d", name, i.B())
	}
	return name
}

// runFuzz runs src's run(iterations) with and without the JIT, compiling
// everything at once, and returns what each returned, formatted exactly.
func runFuzz(t *testing.T, src string, iterations int) (jit, interp string) {
	t.Helper()
	saved, savedRun := jitThreshold, jitMinRun
	jitThreshold, jitMinRun = 0, 0
	defer func() { jitThreshold, jitMinRun = saved, savedRun }()
	// Compiled calls and stores of pointers exit while Go's write barrier
	// is on, as they must: finish any collection and hold off the next, so
	// the exits counted are the program's.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	result := func(l *State) string {
		l.PushBuffer(make([]float64, 64))
		l.SetGlobal("f64")
		l.PushBuffer(make([]int32, 64))
		l.SetGlobal("i32")
		if err := l.DoString(src + fuzzReport); err != nil {
			t.Fatalf("JIT %v: %v\n%s", l.global.jit, err, src)
		}
		l.Global("report")
		l.PushInteger(int64(iterations))
		l.Call(1, 1)
		s, _ := l.ToString(-1)
		return s
	}
	lj := NewState()
	openLibraries(lj)
	li := NewState(WithoutJIT())
	openLibraries(li)
	return result(lj), result(li)
}

// fuzzReport formats what run returns, and the buffers and table it
// wrote, telling integers from floats and -0 from 0. NaN's sign is left
// out: Lua does not specify it, and x86 passes on the first NaN operand's,
// so it follows the order Go's compiler happens to give an operation's
// operands.
const fuzzReport = `
local function show(v)
  if v ~= v then return "nan" end
  if math.type(v) == "float" then return string.format("%.17g", v) .. (1/v < 0 and "-" or "+") end
  return tostring(v)
end
function report(n)
  local r = table.pack(pcall(run, n))
  for i = 0, 63 do r[#r + 1] = f64[i]; r[#r + 1] = i32[i] end
  for i = 1, 64 do r[#r + 1] = tab[i] end
  r[#r + 1], r[#r + 1] = obj.x, obj.y
  for i = 1, r.n > #r and r.n or #r do r[i] = show(r[i]) end
  return table.concat(r, " ")
end
`

// fuzzProgram returns a random program: locals of both number types,
// buffers, a table, an object and two Lua functions, and a numeric for
// loop over statements that mix them.
func fuzzProgram(seed uint64) string {
	g := &fuzzGen{r: rand.New(rand.NewPCG(seed, seed*7919))}
	var b strings.Builder
	b.WriteString(`local floor, ceil, abs, min, max, sqrt, sin = math.floor, math.ceil, math.abs, math.min, math.max, math.sqrt, math.sin
f64, i32 = f64, i32
tab = {} for i = 1, 64 do tab[i] = i * 0.5 end
obj = {x = 1.5, y = 2}
local f64, i32, tab, obj = f64, i32, tab, obj
local function g(a) return a * 2 end
local function h(a, b) return a + b, a - b end
local function va(...) local a, b = ... return a + b end
local function vn(...) local n = 0 for _, v in ipairs({...}) do n = n + 1 end return n end
local small = {1, 2.5, 3}
local function sum(t) local s = 0 for _, v in ipairs(t) do s = s + v end return s end
local function psum(t) local s = 0 for _, v in pairs(t) do s = s + v end return s end
local scale = 0.75
function run(n)
  local i1, i2, f1, f2 = 3, -7, 0.25, -1.5
  for i = 1, n do
`)
	for range 1 + g.r.IntN(5) {
		b.WriteString("    " + g.stmt(0) + "\n")
	}
	b.WriteString("  end\n  return i1, i2, f1, f2\nend\n")
	return b.String()
}

type fuzzGen struct{ r *rand.Rand }

func (g *fuzzGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

// stmt returns a statement; depth bounds nested ifs.
func (g *fuzzGen) stmt(depth int) string {
	switch n := g.r.IntN(10); {
	case n < 3:
		return g.pick("i1", "i2") + " = " + g.intExpr(2)
	case n < 6:
		return g.pick("f1", "f2") + " = " + g.floatExpr(2)
	case n == 6:
		return "f64[" + g.key() + "] = " + g.floatExpr(2)
	case n == 7:
		return "i32[i % 64] = " + g.intExpr(2)
	case n == 8 && depth < 2:
		v := g.pick("f1", "f2")
		return fmt.Sprintf("if %s %s %s then %s = %s else %s end", v, g.pick("<", ">", "<=", "=="), g.pick("1", "1.5", "0", "i"),
			v, g.pick("1", "1.0", "0", "-2.5", "i"), g.stmt(depth+1))
	case n == 9 && g.r.IntN(2) == 0:
		return g.pick(
			"f1 = f1 + va("+g.floatExpr(1)+", "+g.intExpr(1)+")",
			"i1 = vn(i, i1)",
			"f2 = f2 + sum(small)",
			"f2 = f2 + psum(small)",
			"small[i % 3 + 1] = nil; small[i % 3 + 1] = "+g.floatExpr(1),
			"tab[i % 64 + 1] = nil; tab[i % 64 + 1] = "+g.floatExpr(1),
		)
	default:
		return g.pick("tab[i % 64 + 1] = "+g.floatExpr(1), "obj.y = "+g.intExpr(1), "obj.x = "+g.floatExpr(1))
	}
}

// key returns a buffer key in range: an integer, or a float with an
// integer value.
func (g *fuzzGen) key() string {
	return g.pick("i % 64", "(i * 1.0) % 64", "63 - i % 64", "floor(i * 0.5) % 64")
}

// intExpr returns an expression that is always an integer.
func (g *fuzzGen) intExpr(depth int) string {
	if depth == 0 {
		return g.pick("i", "i1", "i2", "3", "-5", "i32[i % 64]", "obj.y")
	}
	a, b := g.intExpr(depth-1), g.intExpr(depth-1)
	switch g.r.IntN(15) {
	case 0:
		return "(" + a + " + " + b + ")"
	case 1:
		return "(" + a + " - " + b + ")"
	case 2:
		return "(" + a + " * " + b + ")"
	case 3:
		return "(" + a + " // " + g.pick("3", "-4", "8") + ")"
	case 4:
		return "(" + a + " % " + g.pick("3", "-4", "8", "256") + ")"
	case 5:
		return "(" + a + " " + g.pick("&", "|", "~") + " " + b + ")"
	case 6:
		return "(" + a + " " + g.pick("<<", ">>") + " " + g.pick("1", "3", "-2", "70") + ")"
	case 7:
		return "(~" + a + ")"
	case 8:
		return g.pick("floor", "ceil") + "(" + g.floatExpr(depth-1) + ")"
	case 9:
		return "abs(" + a + ")"
	case 10:
		return g.pick("min", "max") + "(" + a + ", " + b + ")"
	case 11:
		return g.pick("min", "max") + "(255, " + g.pick("floor", "ceil") + "(" + g.floatExpr(depth-1) + "))"
	case 12:
		return "(#tab + " + a + ")"
	case 13:
		return "floor(min(" + g.floatExpr(depth-1) + ", 255.0))"
	default:
		return a
	}
}

// floatExpr returns an expression that is a float, or a number either
// way where it can be (min and max of mixed types, Lua calls).
func (g *fuzzGen) floatExpr(depth int) string {
	if depth == 0 {
		return g.pick("f1", "f2", "(i * 0.5)", "scale", "1.25", "f64[i % 64]", "tab[i % 64 + 1]", "obj.x", "(i + 0.0)")
	}
	a, b := g.floatExpr(depth-1), g.floatExpr(depth-1)
	switch g.r.IntN(14) {
	case 0:
		return "(" + a + " + " + b + ")"
	case 1:
		return "(" + a + " - " + b + ")"
	case 2:
		return "(" + a + " * " + b + ")"
	case 3:
		return "(" + a + " / " + b + ")"
	case 4:
		return "(" + a + " // " + g.pick("1.0", "2", "0.5", b) + ")"
	case 5:
		return "(" + a + " % " + g.pick("2", "1.0", "0.5", "2.5") + ")"
	case 6:
		return g.pick("sqrt", "sin", "abs") + "(" + a + ")"
	case 7:
		return g.pick("min", "max") + "(" + a + ", " + g.pick(b, "1", "255.0", "0") + ")"
	case 8:
		return "g(" + a + ")"
	case 9:
		return "(h(" + a + ", " + b + "))"
	case 10:
		return "(" + a + " + " + g.intExpr(depth-1) + ")"
	case 11:
		return "(-" + a + ")"
	case 12:
		return "g(h(" + a + ", " + b + "))"
	default:
		return a
	}
}
