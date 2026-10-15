package lua_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// newLimited returns a state with the standard libraries and an allocation
// limit of limit bytes, compiling or only interpreting.
func newLimited(t *testing.T, jit bool, limit int) *lua.State {
	t.Helper()
	var opts []lua.Option
	if !jit {
		opts = append(opts, lua.WithoutJIT())
	}
	l := lua.NewState(opts...)
	stdlib.Open(l)
	l.SetAllocationLimit(limit)
	return l
}

func forBoth(t *testing.T, f func(t *testing.T, jit bool)) {
	for _, jit := range []bool{true, false} {
		t.Run(map[bool]string{true: "jit", false: "interpreted"}[jit], func(t *testing.T) { f(t, jit) })
	}
}

// Each script allocates without bound. Under a limit it stops with a
// memory error, "not enough memory" on the stack, before it allocates much
// more than the limit, and the state runs again once the limit is reset.
func TestAllocationLimit(t *testing.T) {
	scripts := map[string]string{
		"string.rep":         `return string.rep("x", 1e9)`, // sizes within int on 32-bit targets
		"string.rep sep":     `return string.rep("x", 3e8, "yy")`,
		"array fill":         `local t = {} for i = 1, 1e9 do t[i] = i end`,
		"array append":       `local t = {} for i = 1, 1e9 do t[#t + 1] = i end`,
		"hash fill":          `local t = {} for i = 1, 1e9 do t[i + 0.5] = i end`,
		"string keys":        `local t = {} for i = 1, 1e9 do t["k" .. i] = i end`,
		"records":            `local t = {} for i = 1, 1e9 do t[i] = {x = i, y = i} end`,
		"closures":           `local t = {} for i = 1, 1e9 do t[i] = function() return i end end`,
		"concat":             `local s = "" for i = 1, 1e9 do s = s .. "x" end`,
		"tostring":           `local t = {} for i = 1, 1e9 do t[i] = tostring(i) end`,
		"table.concat":       `local t = {} for i = 1, 1000 do t[i] = "x" end return table.concat(t, string.rep("y", 1e5))`,
		"gsub":               `return (string.rep("x", 1e5):gsub("x", string.rep("y", 1e5)))`,
		"string.pack":        `return string.pack("c1000000000", "")`,
		"string.format":      `local s = "x" for i = 1, 1e9 do s = string.format("%s%s", s, s) end`,
		"upper":              `local s = "x" for i = 1, 1e9 do s = (s .. s):upper() end`,
		"table.create":       `local t = {} for i = 1, 1e9 do t[i] = table.create(1e5) end`,
		"recursion":          `local function f(n) return 1 + f(n + 1) end return f(1)`,
		"coroutine.wrap":     `return coroutine.wrap(function() local t = {} for i = 1, 1e9 do t[i] = i end end)()`,
		"coroutines":         `local t = {} for i = 1, 1e9 do t[i] = coroutine.create(print) end`,
		"table.unpack":       `local t = {} for i = 1, 1e5 do t[i] = i end local function f(...) return 1 + f(table.unpack(t)) end return f()`,
		"pairs over growing": `local t = {} for i = 1, 1e9 do t[i + 0.5] = i for k in pairs(t) do break end end`,
	}
	const limit = 4 << 20
	for name, src := range scripts {
		t.Run(name, func(t *testing.T) {
			forBoth(t, func(t *testing.T, jit bool) {
				l := newLimited(t, jit, limit)
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				err := l.DoString(src)
				runtime.ReadMemStats(&after)
				// coroutine.wrap raises a coroutine's error again, as a
				// runtime error, as C Lua's auxwrap does.
				if !errors.Is(err, lua.ErrMemory) && (name != "coroutine.wrap" || err == nil) {
					t.Fatalf("got %v, want a memory error", err)
				}
				if msg, _ := l.ToString(-1); msg != "not enough memory" {
					t.Fatalf("error value %q, want %q", msg, "not enough memory")
				}
				if got := after.TotalAlloc - before.TotalAlloc; got > 64*limit {
					t.Errorf("allocated %d bytes under a limit of %d", got, limit)
				}
				if n := l.Allocated(); n > limit {
					t.Errorf("Allocated() = %d, over the limit of %d", n, limit)
				}
				l.SetTop(0)
				l.SetAllocationLimit(limit)
				if err := l.DoString(`local t = {} for i = 1, 100 do t[i] = {i} end return #t`); err != nil {
					t.Fatalf("after resetting the limit: %v", err)
				}
			})
		})
	}
}

// pcall catches a memory error, with "not enough memory"; xpcall's
// message handler is not called for it, as in C Lua. What fits in the rest
// of the limit still allocates.
func TestAllocationLimitProtected(t *testing.T) {
	forBoth(t, func(t *testing.T, jit bool) {
		l := newLimited(t, jit, 1<<20)
		err := l.DoString(`
			local ok, e = pcall(string.rep, "x", 1e9)
			assert(not ok and e == "not enough memory", e)
			local handled = false
			ok, e = xpcall(string.rep, function(m) handled = true return m end, "x", 1e9)
			assert(not ok and e == "not enough memory" and not handled, e)
			local t = {1, 2, 3} -- small allocations still fit
			assert(#t == 3)
			ok, e = pcall(function() local t = {} for i = 1, 1e9 do t[i] = {} end end)
			assert(not ok and e == "not enough memory", e)`)
		if err != nil {
			t.Fatal(err)
		}
	})
}

// Allocated counts from SetAllocationLimit, the same for the same script
// in two states whenever Go's collector runs, and a limit of 0 removes the
// limit.
func TestAllocated(t *testing.T) {
	const src = `local t = {} for i = 1, 1000 do t[i] = {x = i, s = tostring(i) .. "!"} end`
	forBoth(t, func(t *testing.T, jit bool) {
		var counts [2]int
		for i := range counts {
			l := newLimited(t, jit, 0)
			if n := l.Allocated(); n != 0 {
				t.Fatalf("Allocated() = %d after SetAllocationLimit, want 0", n)
			}
			runtime.GC()
			if err := l.DoString(src); err != nil {
				t.Fatal(err)
			}
			counts[i] = l.Allocated()
		}
		if counts[0] < 1000*16 {
			t.Fatalf("Allocated() = %d after making 1000 tables", counts[0])
		}
		if counts[0] != counts[1] {
			t.Errorf("Allocated() = %d in one state, %d in another", counts[0], counts[1])
		}
	})
}

// A Go function counts what it allocates with Charge, and checks a size
// it is about to build with CheckAllocation.
func TestCharge(t *testing.T) {
	l := newLimited(t, true, 0)
	l.Register("charge", func(l *lua.State) int {
		l.Charge(int(l.CheckInteger(1)))
		return 0
	})
	l.Register("check", func(l *lua.State) int {
		l.CheckAllocation(int(l.CheckInteger(1)))
		return 0
	})
	// Call frames count when a call first reaches a depth, so make the
	// frames this test uses before the limit, to count only what Charge does.
	if err := l.DoString(`check(0) pcall(check, 0)`); err != nil {
		t.Fatal(err)
	}
	l.SetTop(0)
	err := l.LoadString(`
		check(1000) check(1000) -- checking counts nothing
		charge(600)
		assert(not pcall(check, 500))
		assert(not pcall(charge, 500))
		charge(400)
		local ok, e = pcall(charge, 1)
		assert(not ok and e == "not enough memory", e)`)
	if err != nil {
		t.Fatal(err)
	}
	l.SetAllocationLimit(1000)
	if err := l.ProtectedCall(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := l.Allocated(); n < 1000 {
		t.Errorf("Allocated() = %d after charging 1000", n)
	}
}

// Reading a file larger than the limit stops with a memory error; asking
// for more bytes than a small file holds reads it.
func TestAllocationLimitRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(t.TempDir(), "small")
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APOGEE_TEST_BIG", path)
	t.Setenv("APOGEE_TEST_SMALL", small)
	l := newLimited(t, true, 4<<20)
	err := l.DoString(`
		local f = io.open(os.getenv("APOGEE_TEST_SMALL"))
		assert(f:read(1e12) == "hello")
		f:close()
		for _, how in ipairs{"a", 1e12} do
			f = io.open(os.getenv("APOGEE_TEST_BIG"), "rb")
			local ok, e = pcall(f.read, f, how)
			assert(not ok and e == "not enough memory", e)
			f:close()
		end`)
	if err != nil {
		t.Fatal(err)
	}
}

// Without a limit nothing is refused, and SetAllocationLimit with a
// negative limit also removes it.
func TestNoAllocationLimit(t *testing.T) {
	l := newLimited(t, true, 100)
	l.SetAllocationLimit(-1)
	if err := l.DoString(`return #string.rep("x", 1e6)`); err != nil {
		t.Fatal(err)
	}
	if n, _ := l.ToInteger(-1); n != 1e6 {
		t.Fatalf("got %d", n)
	}
	if !strings.Contains(lua.ErrMemory.Error(), "memory") {
		t.Fatal(lua.ErrMemory)
	}
}
