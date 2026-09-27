package lua

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/matjam/apogee/internal/compiler"
)

func testString(t *testing.T, s string) { testStringHelper(t, s, false) }

// Commented out to avoid a warning relating to the method not being used. Left here since it's useful for debugging.
//func traceString(t *testing.T, s string) { testStringHelper(t, s, true) }

func testNoPanicString(t *testing.T, s string) {
	defer func() {
		if rc := recover(); rc != nil {
			var buffer [8192]byte
			t.Errorf("got panic %v; expected none", rc)
			t.Logf("trace:\n%s", buffer[:runtime.Stack(buffer[:], false)])
		}
	}()
	testStringHelper(t, s, false)
}

func testStringHelper(t *testing.T, s string, trace bool) {
	l := NewState()
	openLibraries(l)
	l.LoadString(s)
	if trace {
		l.SetHook(func(state *State, ar Debug) {
			ci := state.callInfo
			p := state.prototype(ci)
			println(stack(state.stack[ci.base():state.top]))
			println(ci.code[ci.savedPC-1].String(), p.Source, p.LineInfo[ci.savedPC-1])
		}, MaskCount, 1)
	}
	l.Call(0, 0)
}

func TestProtectedCall(t *testing.T) {
	l := NewState()
	openLibraries(l)
	l.SetHook(func(state *State, ar Debug) {
		ci := state.callInfo
		_ = stack(state.stack[ci.base():state.top])
		_ = ci.code[ci.savedPC-1].String()
	}, MaskCount, 1)
	l.LoadString("assert(not pcall(string.rep, {}))")
	l.Call(0, 0)
}

func benchmarkSort(b *testing.B, program string) {
	l := NewState()
	openLibraries(l)
	s := `a = {}
		for i=1,%d do
			a[i] = math.random()
		end`
	l.LoadString(fmt.Sprintf(s, b.N))
	if err := l.ProtectedCall(0, 0, 0); err != nil {
		b.Error(err.Error())
	}
	l.LoadString(program)
	b.ResetTimer()
	if err := l.ProtectedCall(0, 0, 0); err != nil {
		b.Error(err.Error())
	}
}

func BenchmarkSort(b *testing.B) { benchmarkSort(b, "table.sort(a)") }
func BenchmarkSort2(b *testing.B) {
	benchmarkSort(b, "i = 0; table.sort(a, function(x,y) i=i+1; return y<x end)")
}

func BenchmarkFibonnaci(b *testing.B) {
	l := NewState()
	s := `return function(n)
			if n == 0 then
				return 0
			elseif n == 1 then
				return 1
			end
			local n0, n1 = 0, 1
			for i = n, 2, -1 do
				local tmp = n0 + n1
				n0 = n1
				n1 = tmp
			end
			return n1
		end`
	l.LoadString(s)
	if err := l.ProtectedCall(0, 1, 0); err != nil {
		b.Error(err.Error())
	}
	l.PushInteger(b.N)
	b.ResetTimer()
	if err := l.ProtectedCall(1, 1, 0); err != nil {
		b.Error(err.Error())
	}
}

// TestTailCallRecursive tests for failures where both the callee and caller are making a tailcall.
func TestTailCallRecursive(t *testing.T) {
	s := `function tailcall(n, m)
			if n > m then return n end
			return tailcall(n + 1, m)
		end
		return tailcall(0, 5)`
	testNoPanicString(t, s)
}

// TestTailCallRecursiveDiffFn tests for failures where only the caller is making a tailcall.
func TestTailCallRecursiveDiffFn(t *testing.T) {
	s := `function tailcall(n) return n+1 end
		return tailcall(5)`
	testNoPanicString(t, s)
}

// TestTailCallSameFn tests for failures where only the callee is making a tailcall.
func TestTailCallSameFn(t *testing.T) {
	s := `function tailcall(n, m)
			if n > m then return n end
			return tailcall(n + 1, m)
		end
		return (tailcall(0, 5))`
	testNoPanicString(t, s)
}

// TestNoTailCall tests for failures when neither callee nor caller make a tailcall.
func TestNormalCall(t *testing.T) {
	s := `function notailcall() return 5 end
		return (notailcall())`
	testNoPanicString(t, s)
}

func TestVarArgMeta(t *testing.T) {
	s := `function f(t, ...) return t, {...} end
		local a = setmetatable({}, {__call = f})
		local x, y = a(table.unpack{"a", 1})
		assert(#x == 0)
		assert(#y == 2 and y[1] == "a" and y[2] == 1)`
	testString(t, s)
}

// A metatable caches which of its tag methods are absent, one bit each:
// finding no __index must not hide __len or __eq.
func TestTagMethodCache(t *testing.T) {
	testString(t, `
		local mt = {__len = function() return 42 end, __eq = function() return true end}
		local t, u = setmetatable({}, mt), setmetatable({}, mt)
		local _ = t.x, u.x -- no __index: cached as absent
		assert(#t == 42)
		assert(t == u)`)
}

func TestCanRemoveNilObjectFromStack(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("failed to remove `nil`, %v", r)
		}
	}()

	l := NewState()
	l.PushString("hello")
	l.Remove(-1)
	l.PushNil()
	l.Remove(-1)
}

func TestTableUserdataEquality(t *testing.T) {
	const s = `return function(x)
		local b = x == {}
		assert(type(b) == "boolean")
		assert(b == false)
		-- reverse
		b = {} == x
		assert(type(b) == "boolean")
		assert(b == false)
	end`

	l := NewState()
	openLibraries(l)
	l.LoadString(s)
	if err := l.ProtectedCall(0, 1, 0); err != nil {
		t.Error(err.Error())
	}

	l.PushUserData(5)
	if err := l.ProtectedCall(1, 0, 0); err != nil {
		t.Error(err.Error())
	}
}

func TestUserDataEqualityNil(t *testing.T) {
	const s = `return function(x)
		local b = x == nil
		assert(type(b) == "boolean")
		assert(b == false)
	end`

	l := NewState()
	openLibraries(l)
	l.LoadString(s)
	if err := l.ProtectedCall(0, 1, 0); err != nil {
		t.Error(err.Error())
	}

	l.PushUserData(5)
	if err := l.ProtectedCall(1, 0, 0); err != nil {
		t.Error(err.Error())
	}
}

func TestTableEqualityNil(t *testing.T) {
	const s = `local b = {} == nil
	assert(type(b) == "boolean")
	assert(b == false)`

	testString(t, s)
}

func TestTableNext(t *testing.T) {
	l := NewState()
	openLibraries(l)
	l.CreateTable(10, 0)
	for i := 1; i <= 4; i++ {
		l.PushInteger(i)
		l.PushValue(-1)
		l.SetTable(-3)
	}
	if length := l.Len(-1); length != 4 {
		t.Errorf("expected table length to be 4, but was %d", length)
	}
	count := 0
	for l.PushNil(); l.Next(-2); count++ {
		if k, v := l.CheckInteger(-2), l.CheckInteger(-1); k != v {
			t.Errorf("key %d != value %d", k, v)
		}
		l.Pop(1)
	}
	if count != 4 {
		t.Errorf("incorrect iteration count %d in Next()", count)
	}
}

func TestError(t *testing.T) {
	l := NewState()
	openLibraries(l)
	errorHandled := false
	program := "error('error')"
	l.PushGoFunction(func(l *State) int {
		if l.Top() == 0 {
			t.Error("error handler received no arguments")
		} else if errorMessage, ok := l.ToString(-1); !ok {
			t.Errorf("error handler received %s instead of string", l.TypeName(-1))
		} else if errorMessage != compiler.ChunkID(program)+":1: error" {
			t.Errorf("error handler received '%s' instead of 'error'", errorMessage)
		}
		errorHandled = true
		return 1
	})
	l.LoadString(program)
	l.ProtectedCall(0, 0, -2)
	if !errorHandled {
		t.Error("error not handled")
	}
}

func TestErrorf(t *testing.T) {
	l := NewState()
	openLibraries(l)
	program := "-- script that is bigger than the max ID size\nhelper()\n" + strings.Repeat("--", compiler.IDSize)
	expectedErrorMessage := compiler.ChunkID(program) + ":2: error"
	l.PushGoFunction(func(l *State) int {
		l.Errorf("error")
		return 0
	})
	l.SetGlobal("helper")
	errorHandled := false
	l.PushGoFunction(func(l *State) int {
		if l.Top() == 0 {
			t.Error("error handler received no arguments")
		} else if errorMessage, ok := l.ToString(-1); !ok {
			t.Errorf("error handler received %s instead of string", l.TypeName(-1))
		} else if errorMessage != expectedErrorMessage {
			t.Errorf("error handler received '%s' instead of '%s'", errorMessage, expectedErrorMessage)
		}
		errorHandled = true
		return 1
	})
	l.LoadString(program)
	l.ProtectedCall(0, 0, -2)
	if !errorHandled {
		t.Error("error not handled")
	}
}

func TestPairsSplit(t *testing.T) {
	testString(t, `
	local t = {}
	-- first two keys go into array
	t[1] = true
	t[2] = true
	-- next key forced into map instead of array since it's non-sequential
	t[16] = true
	-- next key inserted into array
	t[3] = true

	local keys = {}
	for k, v in pairs(t) do
		keys[#keys + 1] = k
	end

	table.sort(keys)
	assert(keys[1] == 1, 'got ' .. tostring(keys[1]) .. '; want 1')
	assert(keys[2] == 2, 'got ' .. tostring(keys[2]) .. '; want 2')
	assert(keys[3] == 3, 'got ' .. tostring(keys[3]) .. '; want 3')
	assert(keys[4] == 16, 'got ' .. tostring(keys[4]) .. '; want 16')
	`)
}

func TestConcurrentNext(t *testing.T) {
	testString(t, `
	t = {}
	t[1], t[2], t[3] = true, true, true

	outer = {}
	for k1 in pairs(t) do
		table.insert(outer, k1)
		inner = {}
		for k2 in pairs(t) do
			table.insert(inner, k2)
		end
		table.sort(inner)
		got = table.concat(inner, '')
		assert(got == '123', 'got ' .. got .. '; want 123')
	end

	table.sort(outer)
	got = table.concat(outer, '')
	assert(got == '123', 'got ' .. got .. '; want 123')
	`)
}

func TestLocIsCorrectOnRegisteredFuncCall(t *testing.T) {
	l := NewState()
	l.Register("barf", func(l *State) int {
		l.Errorf("Boom!")
		return 0
	})
	if err := l.Load(strings.NewReader(`
			local thing = barf()  -- line 2; is the source of the error
			print(thing)          -- line 3; this won't execute, and must NOT be the loc of the error!
			`), "test", ""); err != nil {
		t.Errorf("Unexpected error! Got %v", err)
	}
	err := l.ProtectedCall(0, 0, 0)
	if err == nil {
		t.Errorf("Expected error! Got none... :(")
	} else {
		if err.Error() != "runtime error: [string \"test\"]:2: Boom!" {
			t.Errorf("Wrong error reported: %v", err)
		}
	}
}

func TestLocIsCorrectOnFuncCall(t *testing.T) {
	l := NewState()
	if err := l.Load(strings.NewReader(`
			function barf()
				a = 3 + 2
				isNotDefined("Boom!", a)  -- line 4; is the source of the error
			end
			barf()                        -- line 6
			`), "test", ""); err != nil {
		t.Errorf("Unexpected error! Got %v", err)
	}
	err := l.ProtectedCall(0, 0, 0)
	if err == nil {
		t.Errorf("Expected error! Got none... :(")
	} else {
		if err.Error() != "runtime error: [string \"test\"]:4: attempt to call a nil value (global 'isNotDefined')" {
			t.Errorf("Wrong error reported: %v", err)
		}
	}
}

func TestLocIsCorrectOnError(t *testing.T) {
	l := NewState()
	if err := l.Load(strings.NewReader(`
			a = 3 - 3
			b = 3 / q  -- line 3; errs!
			`), "test", ""); err != nil {
		t.Errorf("Unexpected error! Got %v", err)
	}
	err := l.ProtectedCall(0, 0, 0)
	if err == nil {
		t.Errorf("Expected error! Got none... :(")
	} else {
		if err.Error() != "runtime error: [string \"test\"]:3: attempt to perform arithmetic on a nil value (global 'q')" {
			t.Errorf("Wrong error reported: %v", err)
		}
	}
}
