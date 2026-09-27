package lua

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matjam/apogee/internal/jitvm"
)

// skipWithoutJIT skips a test of compiled code where nothing compiles: on
// platforms without the JIT, or with APOGEE_JIT=off.
func skipWithoutJIT(t *testing.T) {
	t.Helper()
	if !jitSupported {
		t.Skip("no JIT on this platform")
	}
	if jitDisabled {
		t.Skip("APOGEE_JIT=off")
	}
}

func TestJITInterpreterIsGenerated(t *testing.T) {
	vm, err := os.ReadFile("vm.go")
	if err != nil {
		t.Fatal(err)
	}
	want, err := jitvm.Generate(vm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("vm_jit.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("vm_jit.go is stale: run go generate")
	}
}

// runBoth runs src in a state with the JIT compiling on first use and in
// one without, and returns what the global function run returns in each,
// formatted with %.17g, and the state that compiled.
func runBoth(t *testing.T, src string) (jit, interp string, lj *State) {
	t.Helper()
	return runBothWith(t, src, func(*State) {})
}

// runBothWith is runBoth with setup run on each state first.
func runBothWith(t *testing.T, src string, setup func(*State)) (jit, interp string, lj *State) {
	t.Helper()
	saved, savedRun := jitThreshold, jitMinRun
	jitThreshold, jitMinRun = 0, 0
	defer func() { jitThreshold, jitMinRun = saved, savedRun }()
	result := func(l *State) string {
		if err := l.DoString(src); err != nil {
			t.Fatal(err)
		}
		l.Global("run")
		l.Call(0, MultipleReturns)
		var parts []string
		for i := 1; i <= l.Top(); i++ {
			if n, ok := l.ToInteger(i); ok && l.IsInteger(i) {
				parts = append(parts, fmt.Sprintf("int %d", n))
			} else if n, ok := l.ToNumber(i); ok && l.TypeOf(i) == TypeNumber {
				parts = append(parts, fmt.Sprintf("%.17g", n))
			} else {
				parts = append(parts, fmt.Sprint(l.ToValue(i)))
			}
		}
		return strings.Join(parts, ",")
	}
	lj = NewState()
	openLibraries(lj)
	setup(lj)
	jit = result(lj)
	li := NewState(WithoutJIT())
	openLibraries(li)
	setup(li)
	interp = result(li)
	return jit, interp, lj
}

func TestJITMatchesInterpreter(t *testing.T) {
	skipWithoutJIT(t)
	tests := []struct {
		name string
		src  string
	}{
		{"arithmetic", `function run() local a, b = 3, 4.5; local c = a * b - a / b + -a; return c, a + b end`},
		{"constants", `function run() local x = 2; return x * 0.5 + 10, 1e300 * 1e10, -0.0 end`},
		{"moves", `function run() local a = 7; local b = a; local c = b; return c end`},
		{"non-number operand exits", `function run() local s = "3"; return s + 1, s * 2 end`},
		{"string move exits", `function run() local s = "x"; local t = s; return t end`},
		{"metamethod exits", `
			local V = setmetatable({}, {__add = function() return 42 end})
			function run() local v = V; return v + 1 end`},
		{"nan and infinity", `function run() local z = 0; return z / z ~= z / z, 1 / z, -1 / z end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jit, interp, _ := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
		})
	}
}

func TestJITControlFlow(t *testing.T) {
	skipWithoutJIT(t)
	tests := []struct {
		name string
		src  string
	}{
		{"numeric for", `function run() local s = 0; for i = 1, 100 do s = s + i end; return s end`},
		{"for with fractional step", `function run() local s = 0; for i = 0, 1, 0.1 do s = s + i end; return s end`},
		{"for counting down", `function run() local s = 0; for i = 10, 1, -3 do s = s * 2 + i end; return s end`},
		{"for that never runs", `function run() local s = 7; for i = 5, 1 do s = 0 end; return s end`},
		{"for with NaN step", `function run() local s = 0; local z = 0; for i = 1, 3, z/z do s = s + 1 end; return s end`},
		{"loop variable's copy reassigned", `function run() local s = 0; for i = 1, 5 do local j = i; s = s + j; j = "x" end; return s end`},
		{"nested loops", `function run() local s = 0; for i = 1, 20 do for j = i, 20 do s = s + i * j % 7 end end; return s end`},
		{"while", `function run() local i, s = 0, 0; while i < 50 do i = i + 1; s = s + i end; return i, s end`},
		{"repeat", `function run() local i = 0; repeat i = i + 2 until i >= 9; return i end`},
		{"repeat until a value", `function run() local i, done = 0, false; repeat i = i + 1; done = i >= 10 and "yes" until done; return i, done end`},
		{"repeat until false", `function run() local i = 0; repeat i = i + 3; if i > 20 then break end until false; return i end`},
		{"repeat until or", `function run() local i, a = 0, nil; repeat i = i + 1; if i == 7 then a = i end until a or i > 100; return i, a end`},
		{"repeat until not", `function run() local i, t = 0, {}; repeat i = i + 1; t[i] = i until not t[5]; return i end`},
		{"long repeat spends budget", `function run() local i = 0; repeat i = i + 1 until i == 200000 and true; return i end`},
		{"if chain", `function run() local a = 0; for i = 1, 30 do if i < 10 then a = a + 1 elseif i <= 20 then a = a + 10 else a = a + 100 end end; return a end`},
		{"equality", `function run() local n = 0; for i = 1, 10 do if i == 5 then n = n + 1 end; if i ~= 5 then n = n + 2 end end; return n end`},
		{"comparisons with NaN", `function run() local z = 0; local n = z/z; return n < 1, n <= 1, n > 1, n >= 1, n == n, n ~= n end`},
		{"and or not", `function run() local a, b = nil, false; return a or 3, b and 4, not a, not 0, a == nil end`},
		{"test on values", `function run() local t, n = {}, 0; for i = 1, 5 do local v = (i % 2 == 0) and t or nil; if v then n = n + 1 end end; return n end`},
		{"modulo", `function run() local r = {}; local a, b = -7, 3; return a % b, 7 % -3, 5.5 % 2, a % 0.5 end`},
		{"booleans and nil", `function run() local a, b, c = true, false, nil; local d = a; return a, b, c, d, not b end`},
		{"upvalues", `
			local count = 0
			local function bump(n) for i = 1, n do count = count + i end end
			function run() bump(10); bump(5); return count end`},
		{"closed upvalues", `
			local function counter() local n = 0; return function() for i = 1, 3 do n = n + 1 end; return n end end
			local c = counter()
			function run() c(); return c() end`},
		{"pointer copies", `function run() local t = {1}; local u = t; local s = "s"; local v = s; return u[1], v end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jit, interp, _ := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
		})
	}
}

func TestJITTablesAndCalls(t *testing.T) {
	skipWithoutJIT(t)
	tests := []struct {
		name string
		src  string
	}{
		{"fields", `function run() local p = {x = 1, y = 2}; local s = 0; for i = 1, 10 do p.x = p.x + i; s = s + p.x * p.y end; return s, p.x end`},
		{"constructors", `
			local function f() return 7, 8 end
			function run()
			  local s = 0
			  for i = 1, 20 do
			    local a, b, c = {i, i + 1, "x"}, {i, x = i, 2 * i}, {f()}
			    local d = {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
			      26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, i}
			    s = s + a[1] + a[2] + #a[3] + b[2] + b.x + c[1] + c[2] + d[50] + d[53] + #d
			  end
			  return s
			end`},
		{"integer keys past the array", `
			function run()
			  local s, t, h, m = 0, {1}, {1}, setmetatable({1}, {__index = function(_, k) return k * 10 end})
			  h[10] = 5
			  for i = 1, 10 do
			    local a, b, c = t[2], t[0], t[-1]
			    s = s + (a or 1) + (b or 2) + (c or 3) + (h[10] or 0) + (h[11] or 4) + m[3] + t[1]
			  end
			  return s
			end`},
		{"field added in loop", `function run() local t = {}; for i = 1, 5 do t.a = i; t.b = (t.b or 0) + t.a end; return t.a, t.b end`},
		{"field set to nil", `function run() local t = {a = 1}; for i = 1, 3 do t.a = nil; t.a = i end; return t.a end`},
		{"globals", `g = 0; function run() for i = 1, 10 do g = g + i end; return g end`},
		{"__index table", `
			local B = {v = 5}; local M = {__index = B}
			function run() local o = setmetatable({}, M); local s = 0; for i = 1, 10 do s = s + o.v end; B.v = 7; return s + o.v end`},
		{"__index function", `
			local o = setmetatable({}, {__index = function(t, k) return #k end})
			function run() local s = 0; for i = 1, 10 do s = s + o.abc end; return s end`},
		{"__newindex", `
			local log = 0
			local o = setmetatable({}, {__newindex = function(t, k, v) log = log + v end})
			function run() for i = 1, 10 do o.x = i end; return log, rawget(o, "x") end`},
		{"shape changes", `
			function run()
			  local s = 0
			  for i = 1, 20 do
			    local t = (i % 2 == 0) and {a = i, b = 1} or {b = 2, a = i}
			    s = s + t.a * t.b
			  end
			  return s
			end`},
		{"methods", `
			local P = {}; P.__index = P
			function P.new(x) return setmetatable({x = x}, P) end
			function P:add(n) self.x = self.x + n; return self.x end
			function run() local p = P.new(1); local s = 0; for i = 1, 10 do s = s + p:add(i) end; return s end`},
		{"arrays", `function run() local t = {1, 2, 3, 4}; for i = 1, 4 do t[i] = t[i] * 2 end; return t[1], t[4], t[5] end`},
		{"array holes and growth", `function run() local t = {}; for i = 1, 10 do t[i] = i end; t[5] = nil; local s = 0; for i = 1, 10 do s = s + (t[i] or 100) end; return s, #t >= 4 end`},
		{"fractional and odd keys", `function run() local t = {1, 2}; t[1.5] = 3; t[-1] = 4; t[0] = 5; return t[1.5], t[-1], t[0], t[1] end`},
		{"string values", `function run() local t = {"a", "b"}; local s = ""; for i = 1, 2 do s = s .. t[i]; t[i] = s end; return s, t[2] end`},
		{"indexing a non-table", `function run() local ok, err = pcall(function() local x = 5; return x.y end); return ok, err end`},
		{"string methods", `function run() local s = "abc"; return s:upper(), s:len() end`},
		{"string methods in loops", `
			local obj = {sub = function(self, i, j) return "T" end}
			function run()
			  local out = {}
			  local s = "hello world"
			  for i = 1, 20 do
			    local x = s:sub(i % 5 + 1, i % 5 + 2)
			    x = x:upper()
			    local r = (i % 2 == 0 and s or obj):sub(1, 1)
			    out[#out + 1] = x .. r .. s:byte(i % 11 + 1)
			    if i == 10 then function string.shout(t) return t .. "!" end end
			    if i > 10 then out[#out + 1] = s:shout() end
			  end
			  local ok, err = pcall(function() return s:nosuch() end)
			  return table.concat(out, ","), ok, err
			end`},
		{"string metatable __index replaced", `
			local mt = getmetatable("")
			local lib = mt.__index
			-- Every name, in another order, each returning its own length,
			-- so that a slot read from the old layout gives a wrong answer.
			local names = {}
			for k in pairs(lib) do names[#names + 1] = k end
			table.sort(names, function(a, b) return a > b end)
			local other = {}
			for _, k in ipairs(names) do other[k] = function() return -#k end end
			function run()
			  local s = 0
			  for i = 1, 20 do
			    s = s + ("abc"):len()
			    if i == 10 then mt.__index = other end
			  end
			  mt.__index = lib
			  return s
			end`},
		{"lengths", `
			local strs = {"", "a", "hello", string.rep("x", 1000), "a" .. "bc"}
			local t = {1, 2, 3}
			function run()
			  local s = 0
			  for i = 1, 20 do
			    local v = strs[i % #strs + 1]
			    s = s + #v + #t
			  end
			  local ok, err = pcall(function() local n = 5; return #n end)
			  return s, ok, err
			end`},
		{"lua calls", `
			local function add(a, b) return a + b end
			function run() local s = 0; for i = 1, 100 do s = add(s, i) end; return s end`},
		{"recursion", `local function fib(n) if n < 2 then return n end return fib(n-1) + fib(n-2) end; function run() return fib(20) end`},
		{"multiple results", `
			local function two(x) return x, x * 2 end
			function run() local s = 0; for i = 1, 10 do local a, b = two(i); s = s + a + b end; return s, two(3) end`},
		{"missing and extra arguments", `
			local function f(a, b, c) return (a or 0) + (b or 0) + (c or 0) end
			function run() return f(1), f(1, 2), f(1, 2, 3, 4) end`},
		{"varargs", `local function f(...) return select("#", ...) end; function run() local s = 0; for i = 1, 5 do s = s + f(i, i) end; return s end`},
		{"go calls", `function run() local s = 0; for i = 1, 10 do s = s + math.max(i, 5) + select(2, i, i * 2) end; return s end`},
		{"intrinsics", `
			local floor, ceil, sqrt, abs, sin, cos = math.floor, math.ceil, math.sqrt, math.abs, math.sin, math.cos
			function run()
			  local s = 0
			  for i = -10, 10 do local x = i * 0.37; s = s + floor(x) + ceil(x) + abs(x) + sqrt(abs(x)) + sin(x) + cos(x) end
			  return s, sin(-0.0), sin(1e10), cos(1e300), floor(-0.5), sqrt(-1) ~= sqrt(-1)
			end`},
		{"intrinsic replaced", `
			local f = math.floor
			function run() local s = 0; for i = 1, 10 do s = s + f(i / 3); if i == 5 then f = math.ceil end end; return s end`},
		{"intrinsic on string", `function run() return math.floor("2.5"), math.sin("0") end`},
		{"errors in callees", `
			local function bad(x) if x > 3 then error("boom " .. x) end; return x end
			function run() local s = 0; local ok, err = pcall(function() for i = 1, 10 do s = s + bad(i) end end); return s, ok, err end`},
		{"runtime error position", `
			local function bad(t) return t.x.y end
			function run() local ok, err = pcall(function() for i = 1, 3 do bad({x = (i < 3) and {y = 1} or nil}) end end); return ok, err end`},
		{"deep recursion", `local function d(n) if n == 0 then return 0 end return 1 + d(n - 1) end; function run() return d(5000) end`},
		{"closures in loops", `function run() local s = 0; for i = 1, 20 do local f = function(x) return x + i end; s = s + f(1) end; return s end`},
		{"tail calls", `local function t(n, acc) if n == 0 then return acc end return t(n - 1, acc + n) end; function run() return t(100, 0) end`},
		{"methods two classes up", `
			local Base = {}
			function Base.get(o) return o.v end
			local Mid = setmetatable({name = "mid"}, {__index = Base})
			local function new(v) return setmetatable({v = v}, {__index = Mid}) end
			function run()
			  local s = 0
			  for i = 1, 30 do
			    local o = new(i)
			    s = s + o:get()
			    if i == 10 then function Mid.get(o) return -o.v end end
			    if i == 20 then Mid.get = nil; function Base.get(o) return 2 * o.v end end
			  end
			  return s
			end`},
		{"a class two up replaced by one of another layout", `
			local Base = {}
			function Base.get(o) return o.v end
			local Other = {pad = function() return "pad" end}
			function Other.get(o) return -o.v end
			local midmt = {__index = Base}
			local Mid = setmetatable({name = "mid"}, midmt)
			local function new(v) return setmetatable({v = v}, {__index = Mid}) end
			function run()
			  local s = 0
			  for i = 1, 30 do
			    s = s + new(i):get()
			    if i == 15 then midmt.__index = Other end
			  end
			  return s
			end`},
		{"own nil fields fall back to the class", `
			local C = {x = "class"}
			local function new() local o = setmetatable({x = 1}, {__index = C}); o.x = nil; return o end
			function run()
			  local out = {}
			  for i = 1, 20 do
			    local o = new()
			    out[#out + 1] = tostring(o.x)
			    if i % 3 == 0 then o.x = i; out[#out + 1] = tostring(o.x) end
			    if i == 10 then C.x = nil end
			  end
			  local bare = {x = 1}; bare.x = nil
			  for i = 1, 3 do out[#out + 1] = tostring(bare.x) end
			  return table.concat(out, ",")
			end`},
		{"tail calls to Go", `
			local C = {}
			local function new(x) return setmetatable({x = x}, {__index = C}) end
			local function two(x) return select(1, x, x * 2) end
			local function all(...) return select("#", ...) end
			function run()
			  local s = 0
			  for i = 1, 20 do
			    local a, b = two(i)
			    s = s + new(i).x + a + b + all(two(i)) + select("#", two(i))
			  end
			  return s, two(3)
			end`},
		{"tail calls to Lua and varargs", `
			local function sum(...) local s = 0; for i = 1, select("#", ...) do s = s + select(i, ...) end; return s end
			local function fwd(a, b, c) return sum(a, b, c) end
			local function fixed(a, b) return a - b end
			local function viafixed(a, b) return fixed(b, a) end
			function run() local s = 0; for i = 1, 20 do s = s + fwd(i, 1, 2) + viafixed(i, 1) end; return s end`},
		{"tail call to a callable table", `
			local callable = setmetatable({}, {__call = function(_, x) return x + 1 end})
			local function f(x) return callable(x) end
			function run() local s = 0; for i = 1, 20 do s = s + f(i) end; return s end`},
		{"yield from a tail-called Go function", `
			local function y(x) return coroutine.yield(x) end
			function run()
			  local co = coroutine.wrap(function() local s = 0; for i = 1, 10 do s = s + y(i) end; return s end)
			  local v = co()
			  for i = 1, 9 do v = co(v * 2) end
			  return co(v * 2)
			end`},
		// A compiled function returns from a frame another function
		// tail-called, whose callInfo the next compiled call reuses.
		{"calls after returning from a tail-called frame", `
			local function new(x) return {x = x} end
			local function plus(a) return new(a.x + 1) end
			local function minus(a) return new(a.x - 1) end
			local n = 0
			local function rec(v, depth)
			  if depth == 0 then n = n + v.x return end
			  rec(minus(v), depth - 1)
			  rec(plus(v), depth - 1)
			end
			function run() for i = 1, 200 do rec(new(i), 6) end return n end`},
		{"absent keys without a metatable", `function run() local t = {1, nil, 3, x = 1}; local n = 0; for i = 1, 4 do if t[i] == nil then n = n + 1 end; if t.y == nil then n = n + 10 end end; return n end`},
		{"absent keys with a metatable", `
			local t = setmetatable({1, nil, 3}, {__index = function(_, k) return k end})
			function run() local s = 0; for i = 1, 3 do s = s + t[i] end; return s end`},
		{"appends", `function run() local t = {}; for i = 1, 1000 do t[i] = i * 2 end; local s = 0; for i = 1, #t do s = s + t[i] end; return s, #t end`},
		{"appends with __newindex", `
			local log = {}
			local t = setmetatable({}, {__newindex = function(t, k, v) log[#log + 1] = k; rawset(t, k, v) end})
			function run() for i = 1, 5 do t[i] = i end; return #log, t[5] end`},
		{"arrays in upvalues", `
			local t, u = {1, 2, 3, 4}, {}
			function run()
			  for i = 1, 8 do u[i] = (t[i] or 0) * 2 end
			  t[2] = nil
			  local s = 0
			  for i = 1, 8 do s = s + (t[i] or 100) + u[i] end
			  return s, #u, t[5]
			end`},
		{"arrays in upvalues with metatables", `
			local log = 0
			local t = setmetatable({1}, {__index = function(_, k) return k * 10 end,
			  __newindex = function(t, k, v) log = log + v; rawset(t, k, v) end})
			function run() local s = 0; for i = 1, 5 do s = s + t[i]; t[i + 1] = i end; return s, log end`},
		{"indexing a non-table upvalue", `
			local n = 5
			function run() local ok, err = pcall(function() for i = 1, 3 do local x = n[i] end end); return ok, err end`},
		{"new fields on shaped tables", `function run() local s = 0; for i = 1, 10 do local t = {}; t.a = i; t.b = i * 2; s = s + t.a + t.b end; return s end`},
		{"dictionary tables", `
			function run()
			  local t = {}
			  for i = 1, 100 do t["k" .. i] = i end
			  for i = 1, 100, 2 do t["k" .. i] = nil end
			  local s = 0; for i = 1, 10 do t.k1 = i; s = s + t.k1 + (t.k3 or 0) + t.k2 end
			  return s
			end`},
		{"captured loop variables", `function run() local fs = {}; for i = 1, 10 do fs[i] = function() return i end end; local s = 0; for i = 1, 10 do s = s + fs[i]() end; return s end`},
		{"while with captured locals", `function run() local fs, i = {}, 0; while i < 5 do i = i + 1; local j = i; fs[i] = function() return j end end; return fs[1]() + fs[5]() end`},
		{"sort with comparator", `function run() local t = {5, 3, 9, 1, 7}; table.sort(t, function(a, b) return a > b end); return t[1], t[5] end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jit, interp, _ := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
		})
	}
}

// A Go function called from compiled code that sets a hook sees it fire
// from the next instruction, as in the interpreter.
func TestJITHookSetFromGo(t *testing.T) {
	skipWithoutJIT(t)
	src := `function run()
		local s = 0
		for i = 1, 50 do
			s = s + i
			if i == 20 then hookon() end
		end
		return s, hooks()
	end`
	for _, mask := range []byte{MaskCount, MaskLine} {
		jit, interp, _ := runBothWith(t, src, func(l *State) {
			count := 0
			l.Register("hookon", func(l *State) int {
				l.SetHook(func(*State, Debug) { count++ }, mask, 1)
				return 0
			})
			l.Register("hooks", func(l *State) int {
				l.SetHook(nil, 0, 0)
				l.PushInteger(count)
				return 1
			})
		})
		if jit != interp {
			t.Fatalf("mask %d: JIT %q, interpreter %q", mask, jit, interp)
		}
	}
}

// Compiled code exits to runJIT for each call of a Go function, Go closure
// or number function that is not an intrinsic, and goes on after it.
func TestJITGoCallExits(t *testing.T) {
	skipWithoutJIT(t)
	src := `local function inner(x) return gofn(x) + 1 end
	function run()
		local exp, s = math.exp, 0
		for i = 1, 50 do
			s = s + inner(i) -- a Go call from a frame compiled code entered
			s = s + gofn(i) + counter() + exp(i / 50) + gofn(i, s)
			local ok = pcall(fail, i)
			if ok then s = s + 1 end
			local p, q, r = two(i)
			local u = two(i)
			if r == nil then s = s + p + q + u end
			-- number functions: frameless, and falling back when the
			-- arguments do not fit
			s = s + mad(i, 2, 3) + mad(i, 2, 3, 4) + mad(i, "2", 3) + (hyp(i, 1))
			record(i, s, 1)
			local w, z = hyp(3, i)
			if z == nil then s = s + w end
		end
		return s, counter(), recorded()
	end`
	jit, interp, _ := runBothWith(t, src, func(l *State) {
		l.Register("gofn", func(l *State) int {
			v, _ := l.ToNumber(1)
			l.PushNumber(v * 2)
			return 1
		})
		l.PushNumberFunction(func(a, b, c float64) float64 { return a*b + c })
		l.SetGlobal("mad")
		l.PushNumberFunction(math.Hypot)
		l.SetGlobal("hyp")
		recorded := 0.0
		l.PushNumberFunction(func(a, b, c float64) { recorded += a + b*c })
		l.SetGlobal("record")
		l.Register("recorded", func(l *State) int { l.PushNumber(recorded); return 1 })
		l.Register("two", func(l *State) int {
			v, _ := l.ToNumber(1)
			l.PushNumber(v + 1)
			l.PushNumber(v * 3)
			return 2
		})
		l.Register("fail", func(l *State) int {
			if n, _ := l.ToNumber(1); int(n)%7 == 0 {
				l.Errorf("fail %d", int(n))
			}
			return 0
		})
		l.PushInteger(0)
		l.PushGoClosure(func(l *State) int {
			n, _ := l.ToInteger(UpValueIndex(1))
			l.PushInteger(n + 1)
			l.PushValue(-1)
			l.Replace(UpValueIndex(1))
			return 1
		}, 1)
		l.SetGlobal("counter")
	})
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// Go calling a compiled Lua function runs it from l.call without the
// interpreter, returning to Go from compiled code, or hands over to the
// interpreter part way through.
func TestJITCalledFromGo(t *testing.T) {
	skipWithoutJIT(t)
	src := `
		local function fixed(a, b) return a + b, a * b end
		local function tail(a) return fixed(a, 2) end
		local function interpreted(a) local s = "x" .. a; return #s end
		local function closes(a) local f = function() return a end; return f() + 1 end
		local function fails(a) if a % 5 == 0 then error("five") end return a end
		local function varResults(...) return ... end
		function run()
			local s = 0
			s = s + callEach(fixed, 1) + callEach(fixed, 2) + callEach(tail, 2)
			s = s + callEach(interpreted, 1) + callEach(closes, 1) + callEach(varResults, 3)
			local ok, err = pcall(callEach, fails, 1)
			local t = {}
			for i = 1, 40 do t[i] = (i * 7919) % 41 end
			table.sort(t, function(a, b) return a > b end)
			return s, ok, err, t[1], t[40]
		end`
	jit, interp, _ := runBothWith(t, src, func(l *State) {
		// callEach(f, n) calls f(i, i) from Go for i = 1..20, wanting n
		// results each time (MultipleReturns when n is 3), and sums them.
		l.Register("callEach", func(l *State) int {
			want, _ := l.ToInteger(2)
			if want == 3 {
				want = MultipleReturns
			}
			sum := 0.0
			for i := 1; i <= 20; i++ {
				top := l.Top()
				l.PushValue(1)
				l.PushInteger(i)
				l.PushInteger(i)
				l.Call(2, int(want))
				for k := top + 1; k <= l.Top(); k++ {
					v, _ := l.ToNumber(k)
					sum += v
				}
				l.SetTop(top)
			}
			l.PushNumber(sum)
			return 1
		})
	})
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// A Go function called from compiled code may grow the stack, moving every
// frame.
func TestJITStackGrowsInGoCall(t *testing.T) {
	skipWithoutJIT(t)
	src := `function run()
		local a, b, s = 1, 2, 0
		for i = 1, 20 do
			local t = {i}
			grow(i * 50)
			s = s + a + b + t[1]
		end
		return s
	end`
	jit, interp, _ := runBothWith(t, src, func(l *State) {
		l.Register("grow", func(l *State) int {
			n := int(l.CheckInteger(1))
			l.CheckStackWithMessage(n, "grow")
			for range n {
				l.PushNil()
			}
			l.Pop(n)
			return 0
		})
	})
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// Compiled sin and cos follow math.Sin and math.Cos bit for bit.
func TestJITTrigMatchesGo(t *testing.T) {
	skipWithoutJIT(t)
	r := rand.New(rand.NewPCG(3, 4))
	xs := []float64{0, math.Copysign(0, -1), 1, -1, math.Pi, math.Pi / 2, math.Pi / 4, 1<<29 - 1, -(1<<29 - 1), 1e-300, 5e-324, 1e-8,
		math.Inf(1), math.Inf(-1), math.NaN()}
	for range 200000 {
		switch r.IntN(3) {
		case 0:
			xs = append(xs, (r.Float64()*2-1)*10)
		case 1:
			xs = append(xs, (r.Float64()*2-1)*(1<<29))
		default:
			xs = append(xs, math.Ldexp(r.Float64()*2-1, r.IntN(60)-30))
		}
	}
	saved, savedRun := jitThreshold, jitMinRun
	jitThreshold, jitMinRun = 0, 0
	defer func() { jitThreshold, jitMinRun = saved, savedRun }()
	l := NewState()
	openLibraries(l)
	var bad int
	l.Register("x", func(l *State) int { l.PushNumber(xs[l.CheckInteger(1)-1]); return 1 })
	l.Register("check", func(l *State) int {
		x, s, c := l.CheckNumber(1), l.CheckNumber(2), l.CheckNumber(3)
		if math.Float64bits(s) != math.Float64bits(math.Sin(x)) || math.Float64bits(c) != math.Float64bits(math.Cos(x)) {
			if bad++; bad < 5 {
				t.Errorf("x %v (%#x): sin %v, want %v; cos %v, want %v", x, math.Float64bits(x), s, math.Sin(x), c, math.Cos(x))
			}
		}
		return 0
	})
	src := fmt.Sprintf(`local sin, cos = math.sin, math.cos
		for i = 1, %d do local v = x(i); check(v, sin(v), cos(v)) end`, len(xs))
	if err := l.DoString(src); err != nil {
		t.Fatal(err)
	}
	if bad > 0 {
		t.Fatalf("%d of %d mismatches", bad, len(xs))
	}
	if l.jitRuns == 0 {
		t.Fatal("compiled code never ran")
	}
}

// Numeric loops on floats compile to kernels that keep numbers in
// registers; each case says how many kernels it should compile. Integer
// loops do not compile to kernels yet.
// Integers in compiled code: arithmetic wraps, mixes with floats, and
// compares exactly; integer loops count as forPrep does; integer keys
// index arrays.
func TestJITIntegers(t *testing.T) {
	skipWithoutJIT(t)
	for _, src := range []string{
		`function run() local s = 0; for i = 1, 100 do s = s + i * 3 - 1 end; return s end`,
		`function run() local s = 0; for i = 10, 1, -3 do s = s + i end; return s end`,
		`function run() local s = 0; for i = 1, 0 do s = s + 1 end; for i = 5, 6, -1 do s = s + 1 end; return s end`,
		`function run() local s = 0; for i = math.maxinteger - 2, math.maxinteger do s = s + 1 end; return s end`,
		`function run() local s = 0; for i = math.mininteger, math.mininteger + 4, 2 do s = s + 1 end; return s end`,
		`function run() local s = 0; for i = math.maxinteger, math.maxinteger - 10, math.mininteger do s = s + 1 end; return s end`,
		`function run() local s = 0; for i = 1, 3.5 do s = s + i end; return s end`,
		`function run() local s = 0.0; for i = 1, 10 do s = s + i / 2 end; return s end`,
		`function run() local x = math.maxinteger; for i = 1, 3 do x = x + 1 end; return x, -math.mininteger end`,
		`function run() local a, b = 0, 0.0; for i = 1, 20 do a = a + i; b = b + i * 0.5 end; return a, b, a * b end`,
		`function run() local n = 0; for i = -10, 10 do if i < 3 then n = n + 1 end; if i <= -2.5 then n = n + 10 end end; return n end`,
		`function run() local n = 0; local big = 2^53 | 0; for i = big - 2, big + 2 do if i < 2^53 then n = n + 1 end end; return n end`,
		`function run() local n = 0; for i = 1, 10 do if 5 < i then n = n + 1 end; if i >= 7.5 then n = n + 100 end end; return n end`,
		`function run() local t = {}; for i = 1, 50 do t[i] = i * i end; local s = 0; for i = 1, 50 do s = s + t[i] end; return s, t[2.0], #t end`,
		`function run() local t = {10, 20, 30}; local s = 0; for i = 1, 3 do s = s + t[i] + t[1] end; return s end`,
		`function run() local s = 0; for i = 1, 10 do local n = -i; if n == -5 then s = s + 100 end; s = s + n end; return s end`,
		`function run() local s = 0; for i = 1, 10 do s = s - -i + (i == 3.0 and 1000 or 0) end; return s end`,
		`function run() local s = 1; local i = 0; while i < 20 do i = i + 1; s = s * 3 end; return s, i end`,
		`function run() local s = 0; for i = 1.0, 3 do s = s + i end; for i = 1, 3, 0.5 do s = s + i end; return s end`,
		`function run() local s = 0.0; for i = 1, 30 do s = s + math.sqrt(i) + math.sin(i) end; return s end`,
		`function run() local s = 0; for i = -20, 20 do s = s + i % 7 + i % -3 + i // 4 + i // -3 + (i * 1000003) % 65536 end; return s end`,
		`function run() local s = 0; for i = 1, 10 do s = s + math.mininteger // -1 + math.mininteger % -1 + i // 2.5 + i % 2.5 end; return s end`,
		`function run() local s = 0.0; for i = 1, 10 do s = s + (i + 0.5) // 2 + 7.5 // -i end; return s end`,
		`function run() local s, x = 0, 0; for i = 1, 70 do x = x ~ (i << (i % 5)) ~ (-i >> 3); s = s + (x & 0xff) + (x | i) % 97 + ~i + (1 << i) + (-1 >> i) + (i << -2) + (i >> -1) end; return s, x end`,
		`function run() local s = 0; for i = 1, 5 do s = s + (i << 63) + (i << 64) + (i >> 64) + (i << -64) end; return s end`,
		`function run() local s = 0; for i = 1, 5 do s = s | (i & 3.0) end; return s end`,
		`function run() local ok = pcall(function() local s = 0; for i = 1, 5 do s = s + i // (i - 3) end end); return ok end`,
		`function run() local ok, e = pcall(function() local s = 0; for i = 1, 5 do s = s + i % (3 - i) end end); return ok, e end`,
	} {
		jit, interp, _ := runBoth(t, src)
		if jit != interp {
			t.Errorf("%s:\nJIT %q, interpreter %q", src, jit, interp)
		}
	}
}

func TestJITKernels(t *testing.T) {
	skipWithoutJIT(t)
	// runs says which kernels must run: "int", "float", "both" or "".
	tests := []struct{ name, runs, src string }{
		{"integer sum", "int", `function run() local s = 0; for i = 1, 1000 do s = s + i end; return s end`},
		{"float sum", "float", `function run() local s = 0; for i = 1.0, 1000 do s = s + i * 0.5 end; return s end`},
		{"integer loop, float accumulator", "int", `function run() local s = 0.0; for i = 1, 1000 do s = s + i * 0.5 end; return s end`},
		{"accumulator turning float", "int", `function run() local s = 0; for i = 1, 100 do s = s + i / 3 end; return s end`},
		{"integer modulo", "int", `function run() local s = 0; for i = 1, 1000 do s = s + (i * i) % 7 end; return s end`},
		{"floor division and signs", "int", `function run() local s = 0; for i = -50, 50 do s = s + i // 3 + i % -4 + (-i) // 5 + (-i) % 6 + i // -7 end; return s end`},
		{"divisor -1", "int", `function run() local s = 0; for i = math.mininteger, math.mininteger + 3 do s = s + i % -1 + i // -1 end; return s end`},
		{"float modulo is left to Go", "", `function run() local s = 0.0; for i = 1.0, 1000 do s = s + (i * i) % 7 end; return s end`},
		{"modulo by a register is not a kernel", "", `function run() local s, m = 0, 7; for i = 1, 100 do s = s + i % m end; return s end`},
		{"temporaries", "int", `function run() local s, t = 0, {}; for i = 1, 100 do local a = i * 2; local b = a - 1; s = s + a / b end; return s, type(t) end`},
		{"old value in a temporary", "int", `function run() local s = 0; do local x = {} end; for i = 1, 10 do local y = i; s = s + y end; return s end`},
		{"branches", "int", `function run() local a, b = 0, 0; for i = 1, 100 do if i % 3 == 0 then a = a + i elseif i < 50 then b = b - 1 else b = b + 2 end end; return a, b end`},
		{"conditional write keeps old value", "int", `function run() local x, s = 5, 0; for i = 1, 10 do if i > 5 then x = i end; s = s + x end; return x, s end`},
		{"zero iterations", "", `function run() local s = 3; for i = 5, 1 do s = s + i end; return s end`},
		{"negative and fractional steps", "both", `function run() local s = 0; for i = 10, 1, -3 do s = s + i end; for i = 10, 1, -0.5 do s = s + i end; for j = 0, 1, 0.1 do s = s * 1.01 + j end; return s end`},
		{"NaN step", "float", `function run() local s, z = 0, 0; for i = 1.0, 3, z/z do s = s + 1 end; return s end`},
		{"string live-in converts after one iteration", "int", `function run() local s = "1"; for i = 1, 3 do s = s + i end; return s end`},
		{"number constants", "int", `function run() local s = 0; for i = 1, 10 do local k = 2.5; s = s - k + i end; return s end`},
		{"unary minus and equality", "int", `function run() local s = 0; for i = 1, 20 do local n = -i; if n == -10 then s = s + 100 end; s = s + n end; return s end`},
		{"float loop against integer constants", "float", `function run() local s = 0; for i = 1.0, 20 do if i < 10 then s = s + 1 end; if i == 15 then s = s + 100 end end; return s end`},
		{"mixed register comparison is not a kernel", "", `function run() local s, x = 0, 2.5; for i = 1, 10 do if i < x then s = s + 1 end end; return s end`},
		{"overflow wraps", "int", `function run() local s = math.maxinteger - 5; for i = 1, 10 do s = s + 1 end; return s end`},
		{"loop at the end of the range", "int", `function run() local s = 0; for i = math.maxinteger - 3, math.maxinteger do s = s + i % 5 end; return s end`},
		{"long loop spends budget", "int", `function run() local s = 0; for i = 1, 300000 do s = s + 1 end; return s end`},
		{"loop variable after the loop", "int", `function run() local last = 0; for i = 1, 7 do last = i end; return last end`},
		{"nested: inner only", "int", `function run() local s = 0; for i = 1, 10 do for j = 1, 10 do s = s + i * j end end; return s end`},
		{"break leaves the kernel", "int", `function run() local s = 0; for i = 1, 10 do s = s + i; if s > 20 then break end end; return s end`},
		{"break from a float comparison", "int", `function run() local s = 0.5; for i = 1, 100 do s = s * 1.5; if s >= 1000 then break end end; return s end`},
		{"calls are not kernels", "", `function run() local s = 0; for i = 1, 10 do s = s + math.floor(i / 2) end; return s end`},
		{"a call on a rare path leaves the kernel", "int", `function run() local s, t = 0, {}; for i = 1, 1000 do s = s + i * 3; if i % 100 == 0 then t[#t + 1] = s end end; return s, #t, t[4] end`},
		{"a return on a rare path", "int", `function run() local s = 0; for i = 1, 1000 do s = s + i; if s > 5000 then return s, i end end; return s end`},
		{"an exit on every path is not a kernel", "", `function run() local s = 0; for i = 1, 100 do s = s + i; local _ = tostring(s) end; return s end`},
		{"locals written after an exit", "int", `function run() local a, b, t = 0, 0, {}; for i = 1, 300 do a = a + i; if i % 7 == 0 then t[#t + 1] = a end; b = b + a end; return a, b, #t end`},
		{"temporaries live across an exit", "int", `function run() local s, t = 0, {}; for i = 1, 200 do local x = i * 2; local y = i * 3; if i % 50 == 0 then t[#t + 1] = x end; s = s + y end; return s, t[1], t[4] end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Kernels run only while the write barrier is off: finish any
			// collection and hold off the next.
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			jit, interp, lj := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
			lj.Global("run")
			if p := lj.ToValue(-1).(*luaClosure).prototype; p.jit == nil {
				t.Fatal("run was not compiled")
			}
			floats, ints := lj.jitCtx.kernels[0] > 0, lj.jitCtx.kernels[1] > 0
			if want := tt.runs; floats != (want == "float" || want == "both") || ints != (want == "int" || want == "both") {
				t.Fatalf("float kernels ran: %v, integer kernels ran: %v; want %q", floats, ints, want)
			}
		})
	}
}

// Kernels that call intrinsics and read and write buffers agree with the
// interpreter, including where they leave the kernel mid-loop, and run.
func TestJITKernelCallsAndBuffers(t *testing.T) {
	skipWithoutJIT(t)
	setup := func(l *State) {
		l.PushBuffer(make([]float64, 100))
		l.SetGlobal("f64")
		l.PushBuffer(make([]float32, 100))
		l.SetGlobal("f32")
		l.PushBuffer(make([]int32, 100))
		l.SetGlobal("i32")
		l.PushBuffer(make([]uint8, 100))
		l.SetGlobal("u8")
	}
	tests := []struct{ name, runs, src string }{
		{"sin", "int", `local sin = math.sin
			function run() local s = 0.0; for i = 1, 1000 do s = s + sin(i * 0.01) end; return s end`},
		// sqrt's argument register holds an integer, and later cos's result.
		{"sqrt and cos", "int", `local sqrt, cos = math.sqrt, math.cos
			function run() local s = 0.0; for i = 1, 1000 do s = s + sqrt(i) * cos(i / 7) end; return s end`},
		{"float loop", "float", `local sin = math.sin
			function run() local s = 0.0; for x = 0.5, 100.5 do s = s + sin(x) end; return s end`},
		{"integer argument", "int", `local sqrt = math.sqrt
			function run() local s = 0.0; for i = 1, 100 do s = s + sqrt(i) end; return s end`},
		{"arguments sin leaves to Go", "int", `local sin = math.sin
			function run() local s = 0.0; for i = 1, 100 do s = s + sin(i * 1e9) end; return s end`},
		{"cos of huge numbers and infinity", "int", `local cos = math.cos
			function run() local s = 0.0; for i = 1, 20 do s = s + cos(i * 1e307) end; return s end`},
		{"buffers", "int", `
			function run()
			  local f64, f32, i32, u8 = f64, f32, i32, u8
			  for i = 0, 99 do f64[i] = i * 0.5; f32[i] = i / 3; i32[i] = i * 1000; u8[i] = i * 3 end
			  local s = 0.0
			  for i = 0, 99 do s = s + f64[i] + f32[i] * 2 end
			  return s, f32[7], i32[99], u8[99], f64[3]
			end`},
		{"constant keys and values", "int", `
			function run()
			  local f64, i32 = f64, i32
			  for i = 1, 10 do f64[3] = i; f64[i] = 2.5; i32[4] = 7; i32[i] = -3 end
			  return f64[3], f64[10], i32[4], i32[10]
			end`},
		{"keys outside leave the kernel", "int", `
			function run()
			  local f64 = f64
			  local ok, e = pcall(function() for i = 90, 110 do f64[i] = i end end)
			  local s = 0.0
			  for i = 95, 105 do if i < 100 then s = s + f64[i] end end
			  return ok, e, s, f64[99]
			end`},
		{"locals written after a side exit keep the last iteration's values", "int", `
			local get
			local function fill(buf)
			  local x, y = 0, 0.5
			  get = function() return x, y end
			  for i = 90, 110 do buf[i] = i; x = i; y = y * 2.0 end
			end
			function run()
			  local ok = pcall(fill, f64)
			  return ok, get()
			end`},
		{"buffers and numbers in upvalues", "int", `
			local a, out, scale = f64, f32, 0.5
			function run()
			  for i = 0, 99 do a[i] = i end
			  for i = 0, 99 do out[i] = a[i] * scale end
			  return out[3], out[99], a[50]
			end`},
		{"a register holding an upvalue's buffer", "int", `
			local a = f64
			local function fill(n)
			  local s = 0.0
			  for i = 90, n do
			    do local b = a; b[i] = i end
			    local x = i * 2.0 -- in b's register
			    s = s + x
			  end
			  return s
			end
			function run()
			  local ok, e = pcall(fill, 110) -- leaves the kernel at 100, with b set
			  return ok, e, a[99]
			end`},
		{"an intrinsic's argument reads a buffer", "int", `
			local sqrt, a = math.sqrt, f64
			local function roots(n) local s = 0.0; for i = 90, n do s = s + sqrt(a[i]) end; return s end
			function run()
			  for i = 0, 99 do a[i] = i end
			  -- a[100] is nil: the kernel leaves at the read, with sqrt in its register
			  return roots(99), pcall(roots, 100)
			end`},
		{"an integer constant clamps a float", "int", `
			function run()
			  local a, out = f64, f32
			  for i = 0, 99 do a[i] = i * 0.03 end
			  for i = 0, 99 do local v = a[i] * 1.5; if v > 1 then v = 1 end; out[i] = v end
			  return out[5], out[99]
			end`},
		{"an integer constant times an integer stays an integer", "", `
			function run()
			  local out = f64
			  for i = 0, 9 do local v = i * 0.5; if v > 2 then v = 3 end; out[i] = v * 2 end
			  return out[1], out[9]
			end`},
		{"bitwise operators", "int", `
			function run()
			  local i32, s = i32, 0
			  for i = 0, 99 do i32[i] = (i - 50) & 0xff | 3 end
			  for i = -50, 49 do s = s + ((i << 3) ~ (i >> 2)) end
			  for i = -50, 49 do s = s + (~i) + (i << 64) end
			  for i = -50, 49 do s = s + (i >> -3) + (i >> 70) end
			  for i = -50, 49 do s = s + (i << -2) end
			  return s, i32[0], i32[37], i32[99]
			end`},
		{"// of floats", "int", `
			function run()
			  local a, out, d = f64, f32, 0.0
			  for i = 0, 99 do a[i] = (i - 50) * 0.37 end
			  local s = 0.0
			  for i = 0, 99 do out[i] = a[i] // 1.0; s = s + a[i] // -2.5 + i // 4.0 + (a[i] // d) end
			  return s, out[0], out[99], out[50]
			end`},
		{"% of floats by powers of two", "int", `
			local xs = {0.0, -0.0, 4.0, -4.0, 5.5, -5.5, 1e300, -1e300, 2^60 + 2^8, -3e-310, 5e-324, -5e-324,
			  1/0, -1/0, 0/0, 7, -7, 2^53 + 1, 0.1, -0.1}
			local a = f64
			for i, x in ipairs(xs) do a[i - 1] = x end
			local n = #xs
			local function show(v) return string.format("%.17g/%s", v, 1/v) end
			function run()
			  for i = 0, n - 1 do -- a kernel: a is an upvalue buffer
			    local x = a[i]
			    a[20 + i] = x % 2
			    a[40 + i] = x % -4.0
			    a[60 + i] = x % 1
			  end
			  local out = {}
			  for i = 0, n - 1 do
			    out[#out + 1] = show(a[20 + i]) .. show(a[40 + i]) .. show(a[60 + i])
			  end
			  for _, x in ipairs(xs) do -- ordinary compiled code
			    out[#out + 1] = show(x % 2) .. show(x % -0.5) .. show(x % 2^52) .. show(3 % 2.0)
			  end
			  return table.concat(out, ";")
			end`},
		{"float keys", "int", `
			function run()
			  local a, out = f64, f32
			  for i = 0, 99 do a[i] = i end
			  for i = 0, 98 do local k = i * 1.0; out[k] = a[(i + 0.5) // 1] end
			  local s = 0.0
			  for i = 0, 99 do s = s + (a[i * 0.5] or 0) end -- fractions leave the kernel
			  local ok, e = pcall(function() for i = 0, 10 do out[i * 0.5] = 1 end end)
			  return s, out[5], out[98], ok, e
			end`},
		{"math functions in kernels", "int", `
			local floor, ceil, abs, min, max = math.floor, math.ceil, math.abs, math.min, math.max
			local a, out = f64, f32
			function run()
			  for i = 0, 99 do a[i] = (i - 50) * 0.73 end
			  a[7], a[8], a[9] = 1e300, -1/0, 0/0 -- floor leaves the kernel for these
			  local s1, s2, s3, t = 0, 0, 0, 0.0
			  for i = 0, 99 do out[i] = floor(a[i]) end
			  for i = 0, 99 do s1 = s1 + ceil(i * 0.25) end
			  for i = 0, 99 do s2 = s2 + abs(i - 50) end
			  for i = 0, 99 do s3 = s3 + min(i, 60) - max(i, 40) end -- a kernel where registers allow
			  for i = 10, 99 do t = t + abs(a[i]) end
			  for i = 10, 99 do t = t + min(a[i], 3.5) end
			  for i = 10, 99 do t = t - max(a[i], -2.25) end
			  return s1, s2, s3, t, out[0], out[99], out[7], out[8], out[9]
			end`},
		{"min of a float and an integer constant keeps the integer", "int", `
			local min, a = math.min, f64
			function run()
			  for i = 0, 99 do a[i] = i * 0.5 end
			  local v
			  for i = 0, 99 do v = min(a[i], 1) end
			  return v, math.type(v)
			end`},
		{"a math call's argument from another", "int", `
			local floor, min, a, out = math.floor, math.min, f64, f64
			function run()
			  for i = 0, 99 do a[i] = (i - 50) * 7.3 end
			  a[7], a[8], a[9] = 1e300, -1/0, 0/0 -- floor leaves the kernel for these, and min gets floats
			  local s = 0
			  for i = 10, 99 do s = s + min(255, floor(a[i])) end
			  local ok, v = pcall(function() local t = 0; for i = 0, 99 do t = t + min(255, floor(a[i])) end; return t end)
			  return s, ok, v
			end`},
		{"an intrinsic's argument from a math call", "int", `
			local floor, sin = math.floor, math.sin
			function run()
			  local s = 0.0
			  for i = 0, 99 do -- sin(2^40 + 366) leaves the kernel, its argument at l.top
			    s = s + sin(floor(i * 3.7 + i // 99 * 2^40))
			  end
			  return s
			end`},
		{"float into an integer buffer", "int", `
			function run()
			  local i32 = i32
			  for i = 0, 9 do i32[i] = i * 2.0 end
			  local ok, e = pcall(function() for i = 0, 9 do i32[i] = i + 0.5 end end)
			  return i32[9], ok, e
			end`},
		{"plasma into a buffer", "int", `local sin = math.sin
			function run()
			  local f64 = f64
			  for y = 0, 9 do for x = 0, 9 do f64[y * 10 + x] = sin(x * 0.1 + 1) + sin(y * 0.07 + 1) + sin((x + y) * 0.05 + 1) end end
			  return f64[0], f64[37], f64[99] -- only the inner loop can be a kernel
			end`},
		{"plasma at a time", "int", `local sin = math.sin
			local function frame(t)
			  local canvas = f64
			  for y = 0, 9 do for x = 0, 9 do canvas[y * 10 + x] = sin(x*0.1+t) + sin(y*0.07+t) + sin((x+y)*0.05+t) end end
			end
			function run() frame(0.5); return f64[0], f64[37], f64[99] end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1)) // kernels run while the barrier is off
			jit, interp, lj := runBothWith(t, tt.src, setup)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
			floats, ints := lj.jitCtx.kernels[0] > 0, lj.jitCtx.kernels[1] > 0
			want := tt.runs
			if !trigInline && (strings.Contains(tt.src, "sin") || strings.Contains(tt.src, "cos")) {
				want = "none" // Go computes them, so they are not intrinsics
			}
			if floats != (want == "float" || want == "both") || ints != (want == "int" || want == "both") {
				t.Fatalf("float kernels ran: %v, integer kernels ran: %v; want %q", floats, ints, want)
			}
		})
	}
}

// A kernel's temporaries share machine registers where their lives do not
// overlap, so clamps and nested calls fit in the few integer registers:
// each loop here, alone in its function, runs as a kernel. Their inputs
// make floor leave the kernel mid-iteration, with shared registers
// holding other temporaries' values.
func TestJITKernelRegisters(t *testing.T) {
	skipWithoutJIT(t)
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(i%60)*9.3 - 200.5
	}
	xs[7], xs[8], xs[9] = 1e300, math.Inf(-1), math.NaN()
	setup := func(l *State) {
		l.PushBuffer(xs)
		l.SetGlobal("a")
		l.PushBuffer(make([]int32, 100))
		l.SetGlobal("iout")
	}
	for _, body := range []string{
		"iout[i] = max(0, min(255, floor(a[i])))",
		"local k = floor(a[i]); iout[i] = min(255, k)",
		"local k = floor(a[i]); if k < 0 then k = 0 elseif k > 255 then k = 255 end; iout[i] = k",
		"local x = floor(a[i]) // 7; s = s + min(x, 100)",
		// Eight integers, but the loop variable shares the index's register.
		"local x = floor(a[i]); local y = min(x, 100); s = s + max(y, -100) + x // 7",
	} {
		t.Run(body, func(t *testing.T) {
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1)) // kernels run while the barrier is off
			jit, interp, lj := runBothWith(t, `
				local floor, min, max, a, iout = math.floor, math.min, math.max, a, iout
				function run()
				  local s = 0
				  local ok, e = pcall(function() for i = 0, 99 do `+body+` end end)
				  s = 0 -- perhaps a float now
				  for i = 10, 99 do `+body+` end
				  return s, iout[0], iout[10], iout[50], iout[99], ok, e
				end`, setup)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
			if lj.jitCtx.kernels[1] == 0 {
				t.Fatal("no kernel ran")
			}
		})
	}
}

// Kernels read and write tables' arrays and fields, guarding each value's
// type, and leave where a guess or the table does not hold.
func TestJITKernelTables(t *testing.T) {
	skipWithoutJIT(t)
	tests := []struct{ name, src string }{
		{"array sum", `function run() local t = {} for i = 1, 100 do t[i] = i end local s = 0 for i = 1, #t do s = s + t[i] end return s end`},
		{"float array", `function run() local t = {} for i = 1, 100 do t[i] = i * 0.5 end local s = 0.0 for i = 1, 100 do s = s + t[i] * 2 end return s end`},
		{"array written in place", `function run() local t = {} for i = 1, 50 do t[i] = 0 end for i = 1, 50 do t[i] = t[i] + i * 3 end return t[1], t[50] end`},
		{"array of mixed numbers leaves", `function run() local t = {} for i = 1, 100 do t[i] = i % 7 == 0 and i + 0.5 or i end local s = 0 for i = 1, 100 do s = s + t[i] end return s end`},
		{"hole in the array", `function run() local t = {} for i = 1, 100 do t[i] = i end t[40] = nil local s, n = 0, 0 for i = 1, 100 do local v = t[i] if v then s = s + v else n = n + 1 end end return s, n end`},
		{"key past the array", `function run() local t = {1, 2, 3} local s = 0 for i = 1, 6 do local v = t[i]; if i > 3 then v = 10 end; s = s + v end return s end`},
		{"fields", `function run() local p = {x = 1.5, y = 2.5} for i = 1, 100 do p.x = p.x + p.y * 0.5; p.y = p.y - 0.25 end return p.x, p.y end`},
		{"records", `function run() local ps = {} for i = 1, 100 do ps[i] = {x = i, y = i * 2} end local s = 0 for i = 1, #ps do local p = ps[i]; s = s + p.x + p.y end return s end`},
		{"records of floats", `function run() local ps = {} for i = 1, 100 do ps[i] = {x = i * 0.5, v = 1.0} end for i = 1, #ps do local p = ps[i]; p.x = p.x + p.v * 0.1 end return ps[1].x, ps[100].x end`},
		{"field through __index leaves", `function run() local mt = {__index = {y = 3}} local s = 0 local p = setmetatable({x = 1}, mt) for i = 1, 100 do s = s + p.x + p.y end return s end`},
		{"__newindex on a missing field", `function run() local log = 0 local p = setmetatable({}, {__newindex = function(t, k, v) log = log + v end}) for i = 1, 10 do p.x = i end return log end`},
		{"a field turns into a float", `function run() local p = {x = 1} local s = 0 for i = 1, 100 do if i == 50 then p.x = 2.5 end s = s + p.x end return s end`},
		{"table values", `function run() local a, b = {v = 1}, {v = 2} local t = {a, b, a} local s = 0 for i = 1, 3 do s = s + t[i].v end return s end`},
		{"nbody-like", `
			local sqrt = math.sqrt
			function run()
			  local bodies = {}
			  for i = 1, 5 do bodies[i] = {x = i * 1.0, y = i * 2.0, vx = 0.0, vy = 0.0, mass = i * 0.5} end
			  for step = 1, 20 do
			    for i = 1, #bodies do
			      local bi = bodies[i]
			      for j = i + 1, #bodies do
			        local bj = bodies[j]
			        local dx, dy = bi.x - bj.x, bi.y - bj.y
			        local d2 = dx * dx + dy * dy
			        local mag = 0.01 / (d2 * sqrt(d2))
			        bi.vx = bi.vx - dx * bj.mass * mag
			        bj.vx = bj.vx + dx * bi.mass * mag
			        bi.vy = bi.vy - dy * bj.mass * mag
			        bj.vy = bj.vy + dy * bi.mass * mag
			      end
			    end
			  end
			  return bodies[1].vx, bodies[5].vy
			end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1)) // kernels run while the barrier is off
			jit, interp, lj := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
			if lj.jitCtx.kernels[0]+lj.jitCtx.kernels[1] == 0 {
				t.Error("no kernel ran")
			}
		})
	}
}

// Kernels needing more registers than the machine has spill the rest to
// their stack slots, and agree with the interpreter.
func TestJITKernelSpills(t *testing.T) {
	skipWithoutJIT(t)
	var names, init, body, sum []string
	for j := range 30 {
		n := fmt.Sprintf("f%d", j)
		names, init = append(names, n), append(init, fmt.Sprintf("%d.5", j))
		prev := "i * 0.25"
		if j > 0 {
			prev = names[j-1]
		}
		body = append(body, fmt.Sprintf("%s = %s * 0.5 + %s", n, n, prev))
		sum = append(sum, n)
	}
	for j := range 16 {
		n := fmt.Sprintf("n%d", j)
		names, init = append(names, n), append(init, fmt.Sprint(j))
		body = append(body, fmt.Sprintf("%s = (%s + i * %d) %% 1000", n, n, j+1))
		sum = append(sum, n)
	}
	for _, tt := range []struct{ name, extra string }{
		{"accumulators", ""},
		{"with a buffer and an exit", "; buf[i % 8] = f29; if i == 150 then t[1] = n15 end"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			src := fmt.Sprintf(`local buf, t = buf, {}
				function run()
				  local %s = %s
				  for i = 1, 200 do %s%s end
				  return %s, t[1], buf[3]
				end`, strings.Join(names, ", "), strings.Join(init, ", "), strings.Join(body, "; "), tt.extra, strings.Join(sum, ", "))
			jit, interp, lj := runBothWith(t, src, func(l *State) {
				l.PushBuffer(make([]float64, 8))
				l.SetGlobal("buf")
			})
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
			if lj.jitCtx.kernels[1] == 0 {
				t.Fatal("no kernel ran")
			}
		})
	}
}

// An intrinsic call's kernel checks the upvalue still holds the intrinsic
// it compiled for; one that no longer does runs the ordinary code.
func TestJITKernelIntrinsicChanged(t *testing.T) {
	skipWithoutJIT(t)
	jit, interp, _ := runBoth(t, `
		local f = math.sin
		local function sum() local s = 0.0; for i = 1, 200 do s = s + f(i * 0.01) end; return s end
		function run()
		  local a = sum()
		  f = math.cos
		  local b = sum()
		  f = function(x) return x * 2 end
		  return a, b, sum()
		end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// A call whose last argument is a call, and a RETURN of a call, pass any
// number of values through l.top: Lua functions, Go functions and inline
// math functions, returning none, one or several, in compiled code
// without leaving it each iteration.
func TestJITOpenCalls(t *testing.T) {
	skipWithoutJIT(t)
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // calls exit while the barrier is on
	jit, interp, lj := runBoth(t, `
		local floor, min, byte, select = math.floor, math.min, string.byte, select
		local function none() end
		local function two(a) return a, a * 2 end
		local function count(...) return select("#", ...) end
		local function first(a) return a end
		local function sum3(a, b, c) return (a or 0) + (b or 0) + (c or 0) end
		local function pass(...) return two(...) end -- RETURN of a call, B 0
		function run()
		  local s = 0
		  for i = 1, 2000 do -- no vararg functions or Go functions: no exits
		    s = s + sum3(two(i)) + first(two(i)) + (first(none()) or 0)
		    s = s + floor(min(i * 0.5, 255.0)) + min(255, floor(i * 0.25)) + sum3(1, two(i))
		  end
		  for i = 1, 20 do
		    s = s + count(two(i)) + count(none()) + sum3(byte("abc", 1, 3)) + count(pass(i))
		  end
		  return s, count(), count(two(1)), sum3(pass(3))
		end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	if lj.jitRuns > 500 { // the 20 iterations with vararg and Go functions exit about 9 times each
		t.Fatalf("compiled code entered %d times for 2,000 iterations", lj.jitRuns)
	}
}

// math.min and max of a float and an integer run in compiled code when the
// integer converts to a float exactly, returning the argument chosen with
// its type; Go compares larger integers.
func TestJITMixedMinMax(t *testing.T) {
	skipWithoutJIT(t)
	exits, stop := exitsAt("CALL (Go function)")
	defer stop()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `
		local min, max = math.min, math.max
		local exact = {0, -0.0, 0.0, 1, 1.0, -3, 2.5, 1 << 53, 2^53, -(1 << 53), -2^53, 2^53 + 2, 0/0, 1/0, -1/0}
		local big = {(1 << 53) + 1, math.maxinteger, math.mininteger, 2^63, -2^63, 1.5}
		local out, k = {}, 0
		local function all(xs)
		  for i = 1, #xs do
		    for j = 1, #xs do
		      local x, y = xs[i], xs[j]
		      local lo, hi = min(x, y), max(x, y)
		      out[k + 1], out[k + 2], out[k + 3] = lo, hi, 1 / lo -- the sign of zero
		      k = k + 3
		    end
		  end
		end
		function run()
		  all(exact)
		  all(big)
		  return table.unpack(out)
		end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	// big's 18 pairs of an integer beyond 2^53 and a float call Go for
	// min and max. (table.unpack is a TAILCALL.)
	if want := 18 * 2; *exits != want {
		t.Fatalf("%d Go calls, want %d", *exits, want)
	}
}

// Generic for loops with ipairs, pairs and next over array parts run in
// compiled code, calls of ipairs and pairs included; tables with other
// keys, __index or __pairs, and loops whose variables a closure captures,
// exit where they must, and agree.
func TestJITGenericFor(t *testing.T) {
	skipWithoutJIT(t)
	exits := map[string]int{}
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if p.LineDefined == 2 {
			exits[exitKind(p, ip, reason)]++
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `local ipairs, pairs, next = ipairs, pairs, next
		local function arrays(t, u) -- line 2: no exits
		  local s = 0
		  for r = 1, 50 do
		    for i, v in ipairs(t) do s = s + i * v end
		    for k, v in pairs(t) do s = s + k - v end
		    for k in pairs(u) do s = s + k end -- holes
		    for k, v in next, t do s = s + v end
		    for i, v, extra in ipairs(t) do s = s + (extra or 1) end
		    for i in ipairs(u) do s = s + i end -- stops at the first hole
		    local f, st, c = ipairs(t)
		    local g, st2, c2, cl = pairs(t)
		    s = s + c + (c2 or 7) + (cl or 9) + (f == ipairs(u) and 1 or 0) + (g == next and 1 or 0)
		  end
		  return s
		end
		local function others(mixed, meta, custom, fs)
		  local out = {}
		  for k, v in pairs(mixed) do out[#out + 1] = tostring(k) .. "=" .. tostring(v) end
		  for i, v in ipairs(meta) do out[#out + 1] = i .. ":" .. v end
		  for k, v in pairs(custom) do out[#out + 1] = k .. "~" .. v end
		  for i, v in ipairs(fs) do fs[i] = function() return i + v end end -- captures: Go closes
		  for i = 1, #fs do out[#out + 1] = fs[i]() end
		  for k, v in pairs({1, 2, 3}) do if k == 2 then break end out[#out + 1] = v end
		  out[#out + 1] = select("#", ipairs(meta)) .. select("#", pairs(mixed))
		  return table.concat(out, " ")
		end
		function run()
		  local t, u = {1, 2.5, 3, 4}, {1, 2, nil, 4, nil, nil, 7}
		  local mixed = {10, 20, 30, x = 1, [100] = 5}
		  local meta = setmetatable({1, 2}, {__index = function(_, k) if k < 5 then return k * 10 end end})
		  local custom = setmetatable({}, {__pairs = function(t) return function(_, k) if not k then return "a", 1 end end, t, nil end})
		  return arrays(t, u), others(mixed, meta, custom, {5, 6, 7})
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	if len(exits) != 0 {
		t.Fatalf("exits: %v", exits)
	}
}

// Tail calls run in compiled code with arguments up to l.top (return
// f(...)) and into vararg functions, whose frame then starts above their
// arguments, as a call's does.
func TestJITVarArgTailCalls(t *testing.T) {
	skipWithoutJIT(t)
	exits := map[string]int{}
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if p.LineDefined > 0 { // not the chunk, which makes the functions
			exits[exitKind(p, ip, reason)]++
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `
		local function sum(...) local a, b, c = ... return (a or 0) + (b or 0) + (c or 0) end
		local function wrap(...) return sum(...) end
		local function fixed(x, y) return x - (y or 0) end
		local function wrap2(...) return fixed(...) end
		local function vt(a, ...) return sum(a, ...) end
		local function nested(a, ...) if a == 0 then return sum(...) end return nested(a - 1, a, ...) end
		function run()
		  local s = 0
		  for i = 1, 300 do
		    s = s + wrap(i, 2, 3, 4) + wrap() + wrap2(i, 1, 9) + wrap2(i) + vt(i) + vt(i, 5, 6) + nested(3)
		  end
		  return s
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	for kind, n := range exits { // a call's first, before its callee compiles
		if n > 5 {
			t.Fatalf("%s exited %d times in 300 iterations: %v", kind, n, exits)
		}
	}
}

// A tail call from a vararg function puts the callee at the caller's
// function slot, below the extra arguments, as the interpreter does, so
// Go sees the callee's frame where it is when the callee calls Go.
func TestJITTailCallFromVarArgs(t *testing.T) {
	skipWithoutJIT(t)
	jit, interp, _ := runBoth(t, `
		local function g(x, y) return tostring(x) .. ":" .. tostring(y) .. ":" .. select("#", x, y) end
		local function f(a, ...) local b = ... return g(a, b) end
		function run()
		  local out = {}
		  for i = 1, 300 do out[#out + 1] = f(i, i * 2, 3, 4, 5, 6, 7, 8) end
		  return table.concat(out, " ", 290)
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
}

// Calls of vararg functions, and VARARG in them, run in compiled code:
// any number of extra arguments, missing fixed parameters, all of them
// passed on or returned, and a named vararg table only indexed. One that
// needs a real table exits, and all agree.
func TestJITVarArgs(t *testing.T) {
	skipWithoutJIT(t)
	exits := map[string]int{}
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		// view, at line 7, indexes a vararg view, which Go does.
		if p.LineDefined >= 2 && p.LineDefined <= 8 && p.LineDefined != 7 {
			if k := exitKind(p, ip, reason); !strings.HasPrefix(k, "CALL (Go") {
				exits[k]++
			}
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `local select = select
		local function sum(...) local a, b, c = ... return (a or 0) + (b or 0) + (c or 0) end -- line 2
		local function fixed(x, y, ...) local a = ... return x + (y or 100) + (a or 1000) end
		local function pass(...) local s = sum(...) return s end -- not a tail call
		local function all(...) return ... end
		local function count(...) local n = select("#", ...) return n end
		local function view(...t) return (t[1] or 0) + t.n end
		local function loop(n) -- line 8: no exits
		  local s = 0
		  for i = 1, n do
		    s = s + sum() + sum(i) + sum(i, 2) + sum(i, 2, 3) + sum(i, 2, 3, 4, 5)
		    s = s + fixed(i) + fixed(i, 1) + fixed(i, 1, 2) + fixed(i, 1, 2, 3)
		    s = s + pass(i, i) + select(2, all(i, i + 1)) + count(all(i, nil, nil)) + view(i, 2) + view()
		  end
		  return s
		end
		local function tbl(...t) return #t, t[2] end
		local function nested(a, ...) if a == 0 then return ... end return nested(a - 1, a, ...) end
		function run()
		  local a, b, c = nested(3)
		  return loop(200), a, b, c, tbl(1, 2, 3), count(), count(nil, nil)
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	for kind, n := range exits { // a call's first, before its callee compiles
		if n > 5 {
			t.Fatalf("%s exited %d times in 200 iterations: %v", kind, n, exits)
		}
	}
}

// nil stored into an array element, through a register or an upvalue,
// runs in compiled code; into an absent element of a table with
// __newindex, or a buffer, it exits, and agrees.
func TestJITNilArrayStores(t *testing.T) {
	skipWithoutJIT(t)
	exits := map[string]int{}
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if p.LineDefined == 2 {
			exits[exitKind(p, ip, reason)]++
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBothWith(t, `local up = {1, 2, 3, 4}
		local function clear(t) -- line 2: no exits
		  for i = 1, 200 do
		    local k = i % 4 + 1
		    t[k] = nil; up[k] = nil
		    t[k] = i; up[k] = i * 2
		  end
		  t[2], up[3] = nil, nil
		end
		function run()
		  local t = {1, 2, 3, 4}
		  clear(t)
		  local log = {}
		  local m = setmetatable({1, nil, 3}, {__newindex = function(_, k, v) log[#log + 1] = k .. "=" .. tostring(v) end})
		  for i = 1, 3 do m[i] = nil end
		  m[2] = nil
		  local ok, e = pcall(function() f64[1] = nil end)
		  return t[1], t[2], t[3], up[2], up[3], #t, m[1], m[3], table.concat(log, ","), ok, e
		end`, func(l *State) {
		l.PushBuffer(make([]float64, 4))
		l.SetGlobal("f64")
	})
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	if len(exits) != 0 {
		t.Fatalf("exits: %v", exits)
	}
}

// setmetatable of a new table, called or tail called as constructors do,
// runs in compiled code when the metatable is known to lack __gc and
// __mode; anything else goes to Go, and agrees.
func TestJITSetMetatable(t *testing.T) {
	skipWithoutJIT(t)
	exits := map[string]int{}
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if p.LineDefined >= 3 && p.LineDefined <= 5 {
			exits[exitKind(p, ip, reason)]++
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `local setmetatable, getmetatable = setmetatable, getmetatable
		local P = {} P.__index = P
		function P.new(x) return setmetatable({x = x}, P) end -- line 3: a tail call
		function P.new2(x) local p = setmetatable({x = x}, P) return p end
		function P.new3(x) return setmetatable({x = x}, {__index = P}) end -- a fresh metatable
		local G = setmetatable({}, {__gc = function() end})
		function run()
		  local s = 0
		  for i = 1, 300 do s = s + P.new(i).x + P.new2(i).x + P.new3(i).x end
		  local out = {s, getmetatable(P.new(1)) == P, getmetatable(P.new2(1)) == P}
		  local prot = setmetatable({}, {__metatable = "locked"})
		  out[#out + 1] = select(2, pcall(setmetatable, prot, P))
		  local has = setmetatable({}, {})
		  out[#out + 1] = getmetatable(setmetatable(has, P)) == P
		  out[#out + 1] = getmetatable(setmetatable({}, nil)) == nil
		  out[#out + 1] = select(2, pcall(setmetatable, 1, P))
		  out[#out + 1] = select(2, pcall(setmetatable, {}, 1))
		  local ran = false
		  local gc = {__gc = function() ran = true end}
		  out[#out + 1] = getmetatable(setmetatable({}, gc)) == gc
		  collectgarbage()
		  out[#out + 1] = ran -- the collector heard of the finalizer
		  for i = 1, #out do out[i] = tostring(out[i]) end
		  return table.concat(out, " ")
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	for kind, n := range exits { // a call's first, and constructors' tables learning their shapes
		if kind != "NEWTABLE" && n > 10 {
			t.Fatalf("%s exited %d times in 300 iterations: %v", kind, n, exits)
		}
	}
}

// % of floats by any divisor runs in compiled code, bit for bit as
// math.Mod and Lua's correction give it, NaN included; random normal
// floats never exit, and edge cases, subnormal divisors left to Go,
// agree.
func TestJITFloatMod(t *testing.T) {
	skipWithoutJIT(t)
	exits := 0
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if p.LineDefined == 2 && exitKind(p, ip, reason) == "MOD" {
			exits++
		}
	}
	defer func() { jitExitHook = nil }()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `
		local function normals(xs, out) -- line 2: no exits
		  for i = 1, #xs do
		    local x = xs[i]
		    for j = 1, #xs do out[#out + 1] = x % xs[j] end
		  end
		end
		local function all(xs, out)
		  for i = 1, #xs do
		    local x = xs[i]
		    for j = 1, #xs do out[#out + 1] = x % xs[j] end
		  end
		end
		local edge = {0.0, -0.0, 1.0, -1.0, 2.5, -2.5, 0.1, 3, -7, 1e308, -1e308, 2^-1074, -2^-1074, 2^-1022,
		  1.7976931348623157e308, 5e-324 * 3, 1/0, -1/0, 0/0, 2^53, 2^53 + 2, 1/3, math.pi, 2^-1060}
		math.randomseed(7)
		local normal = {}
		for k = 1, 150 do
		  local v = string.unpack("<d", string.pack("<i8", math.random(math.mininteger, math.maxinteger)))
		  if v == v and v - v == 0 and (v > 2^-1022 or v < -2^-1022) then normal[#normal + 1] = v end
		end
		function run()
		  local out, edges = {}, {}
		  normals(normal, out)
		  all(edge, edges)
		  local s = {} -- bits: Go's NaN is math.NaN()'s
		  for i = 1, #out do s[#s + 1] = string.format("%x", string.unpack("<i8", string.pack("<d", out[i]))) end
		  for i = 1, #edges do s[#s + 1] = string.format("%x", string.unpack("<i8", string.pack("<d", edges[i]))) end
		  return #out, table.concat(s, " ")
		end`)
	if jit != interp {
		t.Fatalf("JIT %q\ninterpreter %q", jit, interp)
	}
	if exits != 0 {
		t.Fatalf("MOD of normal floats exited %d times", exits)
	}
}

// # of strings, buffers and tables without __len runs in compiled code:
// a table's array length, or a border by binary search when its last
// element is nil. A hash part, __len or other userdata exit, and agree.
func TestJITLength(t *testing.T) {
	skipWithoutJIT(t)
	exits, stop := exitsAt("LEN")
	defer stop()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBothWith(t, `
		local full, holes, empty = {}, {1, 2, 3, nil, 5, nil, nil, nil}, {}
		for i = 1, 100 do full[i] = i end
		local sparse = {1, 2, 3}
		sparse[1000] = 1 -- a hash part: Go searches it
		local plain = setmetatable({1, 2}, {}) -- the first # caches its metatable's lack of __len
		local counted = setmetatable({1}, {__len = function() return 42 end})
		function run()
		  local s = 0
		  for i = 1, 2000 do -- no exits
		    s = s + #full + #holes + #empty + #plain + #"abc" + #f64
		    holes[i % 8 + 1] = (i % 3 == 0) and i or nil
		  end
		  local t = {}
		  for i = 1, 20 do t[#t + 1] = #sparse + #counted end
		  return s, #full, #holes, #empty, #plain, #t, #sparse, #counted, pcall(function() return #io.stdout end)
		end`, func(l *State) {
		l.PushBuffer(make([]float64, 7))
		l.SetGlobal("f64")
	})
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	if *exits != 21+21+1+1 { // sparse, counted, io.stdout and the first plain
		t.Fatalf("LEN exited %d times", *exits)
	}
}

// exitsAt counts compiled code's exits at instructions whose exitKind
// starts with kind, until stop.
func exitsAt(kind string) (count *int, stop func()) {
	n := 0
	jitExitHook = func(p *prototype, ip int, reason uint64) {
		if strings.HasPrefix(exitKind(p, ip, reason), kind) {
			n++
		}
	}
	return &n, func() { jitExitHook = nil }
}

// EQ of a float and an integer runs in compiled code, exactly: equal only
// when the float has that integer value.
func TestJITMixedEqual(t *testing.T) {
	skipWithoutJIT(t)
	exits, stop := exitsAt("EQ")
	defer stop()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // stores exit while the barrier is on
	jit, interp, _ := runBoth(t, `
		local xs = {0, -0.0, 1, 1.0, 1.5, -3, -3.0, 2^53, 2^53 + 1, 2^63, -2^63, math.maxinteger,
		  math.mininteger, 0/0, 1/0, -1/0, 9007199254740993, 2^53 + 2.0, "1", true}
		local n = #xs
		function run()
		  local out = {}
		  for i = 1, n do
		    for j = 1, n do
		      if xs[i] == xs[j] then out[#out + 1] = i .. "=" .. j end
		    end
		    local x = xs[i]
		    out[#out + 1] = tostring(x == 1) .. tostring(x == 1.0) .. tostring(1 == x) .. tostring(x ~= -3.0) .. tostring(2^63 == x)
		  end
		  return table.concat(out, " ")
		end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	if *exits != 0 {
		t.Fatalf("EQ exited %d times", *exits)
	}
}

// math.floor, ceil, abs, min and max compile inline, and agree with Go's
// for every kind of argument.
func TestJITMathFunctions(t *testing.T) {
	skipWithoutJIT(t)
	jit, interp, _ := runBoth(t, `
		local floor, ceil, abs, min, max = math.floor, math.ceil, math.abs, math.min, math.max
		local xs = {0, -0.0, 0.0, 1, -1, 7, 2.5, -2.5, 1e308, -1e308, 1/0, -1/0, 0/0,
		  2^63, -2^63, 2^63 - 1024, math.maxinteger, math.mininteger, 0.49999999999999994, -0.5}
		local function show(v) return math.type(v) .. ":" .. string.format("%.17g", v) .. ":" .. tostring(1/v) end
		local n = #xs
		local fl, ce, ab, mi, ma = {}, {}, {}, {}, {}
		function run()
		  for i = 1, n do -- no exits: the calls run in compiled code
		    local x, y = xs[i], xs[n + 1 - i]
		    fl[i], ce[i], ab[i], mi[i], ma[i] = floor(x), ceil(x), abs(x), min(x, y), max(x, y)
		  end
		  local out = {}
		  for i = 1, n do
		    out[i] = show(fl[i]) .. show(ce[i]) .. show(ab[i]) .. show(mi[i]) .. show(ma[i])
		  end
		  local e1 = select(2, pcall(function() return floor("x") end))
		  local e2 = select(2, pcall(function() return min(1, {}) end))
		  return table.concat(out, ";"), e1, e2, min(1, 2, 0), max(3), floor("3.5")
		end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// A function of a few hundred instructions compiles to more than the 32 KB
// arm64's test branches reach, and still compiles.
func TestJITLargeFunction(t *testing.T) {
	skipWithoutJIT(t)
	var b strings.Builder
	b.WriteString("function run()\n  local t, s = {a = 1, b = 2.5, c = 3}, 0\n  for i = 1, 3 do\n")
	for k := range 200 {
		fmt.Fprintf(&b, "    if t.a < t.c then s = s + t.b * %d - t.c else s = s - t.a end\n", k)
	}
	b.WriteString("  end\n  return s\nend\n")
	jit, interp, lj := runBoth(t, b.String())
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	lj.Global("run")
	if p := lj.ToValue(-1).(*luaClosure).prototype; p.jit == nil {
		t.Fatal("run was not compiled")
	}
}

// A loop longer than the budget returns to Go on the way.
func TestJITBudget(t *testing.T) {
	skipWithoutJIT(t)
	jit, interp, lj := runBoth(t, `function run() local s = 0; for i = 1, 1000000 do s = s + 1 end; return s end`)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
	if runs := lj.jitRuns; runs < 1000000/jitBudget {
		t.Fatalf("compiled code ran %d times, want at least %d budget exits", runs, 1000000/jitBudget)
	}
}

// Compiled code copies pointers while another goroutine keeps the GC
// marking, so some runs start with the write barrier on.
func TestJITUnderGC(t *testing.T) {
	skipWithoutJIT(t)
	var stop atomic.Bool
	defer stop.Store(true)
	go func() {
		var keep [][]byte
		for !stop.Load() {
			keep = append(keep, make([]byte, 1<<16))
			if len(keep) > 64 {
				keep = keep[:0]
			}
		}
	}()
	src := `function run()
		local n = 0
		local r = {a = {}, b = "x", [1] = {}, [2] = "y"}
		local function id(x, y) return y, x end
		for i = 1, 20000 do
			r.a, r.b = id(r.a, r.b)
			local t = {i}
			local u = t
			local s = "k"
			local v = s
			r.a, r.b, r[1], r[2] = r.b, r.a, r[2], r[1]
			local w = r.a
			if u[1] == i and v == "k" and w ~= nil then n = n + 1 end
		end
		return n
	end`
	var barrierRuns uint64
	for range 20 {
		jit, interp, lj := runBoth(t, src)
		if jit != interp {
			t.Fatalf("JIT %q, interpreter %q", jit, interp)
		}
		barrierRuns += lj.jitBarrierRuns
	}
	if barrierRuns == 0 {
		t.Fatal("no compiled run started with the write barrier on")
	}
}

// Compiled == and ~= match the interpreter for every kind of operand.
func TestJITEquality(t *testing.T) {
	skipWithoutJIT(t)
	src := `
		local eqmt = {__eq = function() return true end}
		local plain = {}
		local noeq = setmetatable({}, {})
		local ta, tb = setmetatable({}, eqmt), setmetatable({}, eqmt)
		local f, g = function() end, function() end
		local ab = "a" .. string.rep("b", 1)
		local long = string.rep("x", 32)
		local vals = {n = nil, false, true, 0, 1, -0.0, 0/0, "ab", ab, "ac", "abc", "",
		  plain, {}, noeq, ta, tb, f, g, print,
		  ("q"):sub(1), "q", "r", long, string.rep("x", 31) .. "x", string.rep("x", 31) .. "y",
		  string.rep("z", 40), string.rep("z", 39) .. "z"}
		local nvals = 28
		function run()
		  local out = {}
		  for i = 1, nvals do
		    for j = 1, nvals do
		      local a, b = vals[i], vals[j]
		      out[#out + 1] = (a == b) and "1" or "0"
		      out[#out + 1] = (a ~= b) and "1" or "0"
		    end
		    local a = vals[i]
		    out[#out + 1] = (a == nil and "n" or "-") .. (a == "ab" and "s" or "-") ..
		      (a == true and "t" or "-") .. (a == 1 and "1" or "-") .. (nil == a and "N" or "-") ..
		      (0 == a and "0" or "-") .. (a ~= -0.0 and "m" or "-")
		  end
		  -- __eq only on the second: tried, as Lua 5.4 on
		  out[#out + 1] = (plain == ta) and "y" or "n"
		  -- a loop whose back edge is an equality test
		  local k, s = 0, nil
		  repeat k = k + 1; if k == 7 then s = "done" end until s == "done"
		  out[#out + 1] = tostring(k)
		  return table.concat(out)
		end`
	jit, interp, _ := runBoth(t, src)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}

// A function called once compiles when a loop in it is hot, whatever the
// kind of loop.
func TestJITCompilesHotLoops(t *testing.T) {
	skipWithoutJIT(t)
	loops := map[string]string{
		"numeric for": `for i = 1, 5000 do s = s + i end`,
		"while":       `local i = 0; while i < 5000 do i = i + 1; s = s + i end`,
		"repeat":      `local i = 0.0; repeat i = i + 1.0; s = s + i until i >= 5000.0`,
		"generic for": `for c in string.gmatch(string.rep("a", 5000), "a") do local n = #c * 1.0; s = s + n * 2.0 - n + 1.0 - 1.0 end`,
	}
	for name, loop := range loops {
		t.Run(name, func(t *testing.T) {
			l := NewState()
			openLibraries(l)
			if err := l.DoString(`local function f() local s = 0; ` + loop + `; return s end; return f()`); err != nil {
				t.Fatal(err)
			}
			if l.jitRuns == 0 {
				t.Fatal("the loop never ran compiled")
			}
		})
	}
}

// Compiled code can tail-call a function that has never run.
func TestJITTailCallToFunctionNotYetRun(t *testing.T) {
	skipWithoutJIT(t)
	l := NewState()
	openLibraries(l)
	err := l.DoString(`
		local function never(x) return x + 1 end
		local function hot(i) if i == 3000 then return never(i) end return i end
		local s = 0
		for i = 1, 3000 do s = s + hot(i) end
		assert(s == 3000 * 3001 / 2 + 1, s)`)
	if err != nil {
		t.Fatal(err)
	}
}

// Tail calls between compiled Lua functions replace the frame in compiled
// code; others exit. Both agree with the interpreter.
func TestJITTailCalls(t *testing.T) {
	skipWithoutJIT(t)
	tests := []struct{ name, src string }{
		{"mutual recursion without growing the stack", `
			local even, odd
			function even(n) if n == 0 then return true end return odd(n - 1) end
			function odd(n) if n == 0 then return false end return even(n - 1) end
			function run() return even(1000001), odd(7) end`},
		{"accumulator", `
			local function sum(n, acc) if n == 0 then return acc end return sum(n - 1, acc + n) end
			function run() return sum(100000, 0) end`},
		{"fewer and more arguments than parameters", `
			local function f(a, b, c) return (a or 0) + (b or 10) + (c or 100) end
			local function g(x) return f(x) end
			local function h(x) return f(x, x, x, x, x) end
			function run() local s = 0; for i = 1, 20 do s = s + g(i) + h(i) end; return s end`},
		{"multiple results", `
			local function three(x) return x, x * 2, x * 3 end
			local function tail(x) return three(x) end
			function run() local s = 0; for i = 1, 20 do local a, b = tail(i); local c, d, e, f = tail(i); s = s + a + b + c + d + e + (f or 0) end; return s end`},
		{"methods", `
			local P = {}; P.__index = P
			function P:get(k) return self.v * k end
			function P:via(k) return self:get(k + 1) end
			function run() local o = setmetatable({v = 3}, P); local s = 0; for i = 1, 30 do s = s + o:via(i) end; return s end`},
		{"to Go, vararg and not yet compiled functions", `
			local function va(...) return select("#", ...) end
			local function f(x) if x % 3 == 0 then return math.max(x, 5) elseif x % 3 == 1 then return va(x, x) end return tostring(x) end
			function run() local s = ""; for i = 1, 12 do s = s .. f(i) end; return s end`},
		{"from a function with nested functions", `
			local function k(x) return x + 1 end
			local function f(x) local g = function() return x end; return k(g()) end
			function run() local s = 0; for i = 1, 20 do s = s + f(i) end; return s end`},
		{"passing a closure over the frame", `
			local function call(g) return g() * 2 end
			local function f(x) local g = function() return x end; x = x + 1; return call(g) end
			function run() local s = 0; for i = 1, 20 do s = s + f(i) end; return s end`},
		{"returns from functions with nested functions", `
			local function none(x) local g = function(y) return y * 2 end; return g(x) + 1 end
			local function keeps(x) local get = function() return x end; x = x * 10; return get end
			local function maybe(x) if x % 2 == 0 then local get = function() return x end; return get() end; local unused = function() end; return x end
			function run()
			  local s, getters = 0, {}
			  for i = 1, 30 do s = s + none(i) + maybe(i); getters[i] = keeps(i) end
			  for i = 1, 30 do s = s + getters[i]() end
			  return s
			end`},
		{"an outer frame's open upvalue", `
			local function inner(x) local g = function() return 1 end; return x + g() end
			function run()
			  local s, n = 0, 0
			  local bump = function() n = n + 1 end
			  for i = 1, 30 do s = s + inner(i); bump() end
			  return s, n
			end`},
		{"tail called status", `
			local function inner() local info = debug.getinfo(1, "t"); return info.istailcall end
			local function outer() return inner() end
			function run() local r; for i = 1, 20 do r = outer() end; return r end`},
		{"called from Go", `
			local function add(a, b) return a + b end
			local function via(a, b) return add(a, b) end
			function run() local t = {5, 3, 9, 1}; table.sort(t, function(x, y) return via(x, 0) < via(y, 0) end); return t[1], t[4] end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jit, interp, _ := runBoth(t, tt.src)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
		})
	}
}

// Compiled code reads and writes buffers' elements inline, converting as
// Go does; keys and values it cannot handle exit, and all agree with the
// interpreter.
func TestJITBuffers(t *testing.T) {
	skipWithoutJIT(t)
	setup := func(l *State) {
		l.PushBuffer(make([]float64, 8))
		l.SetGlobal("f64")
		l.PushBuffer(make([]float32, 8))
		l.SetGlobal("f32")
		l.PushBuffer(make([]int32, 8))
		l.SetGlobal("i32")
		l.PushBuffer(make([]uint8, 8))
		l.SetGlobal("u8")
		l.PushUserData(3)
		l.SetGlobal("plain")
	}
	tests := []struct{ name, src string }{
		{"writes and reads", `
			function run()
			  local f64, f32, i32, u8 = f64, f32, i32, u8
			  local s = 0
			  for i = 0, 7 do
			    f64[i] = i * 1.5; f32[i] = i / 3; i32[i] = i * 1000000007; u8[i] = i * 100
			  end
			  for i = 0, 7 do s = s + f64[i] + f32[i] + i32[i] + u8[i] end
			  return s, f32[1], i32[7], u8[7], math.type(i32[3]), math.type(f64[2])
			end`},
		{"float keys and integer values into float buffers", `
			function run()
			  local s = 0
			  for i = 0, 7 do f64[i + 0.0] = i; f32[i] = i end
			  for i = 0.0, 7.0 do s = s + f64[i] + f32[i] end
			  return s, math.type(f64[3])
			end`},
		{"outside the buffer and other keys", `
			function run()
			  local n = 0
			  for i = -2, 10 do if f64[i] == nil then n = n + 1 end; if u8[i] == nil then n = n + 10 end end
			  return n, f64[0.5], f64.x, #f64, #u8
			end`},
		{"errors", `
			function run()
			  local r = {}
			  for _, f in ipairs{
			    function() f64[8] = 1 end, function() i32[0] = 0.5 end, function() u8[1] = "x" end,
			    function() return plain[1] end, function() plain[1] = 1 end,
			  } do r[#r + 1] = select(2, pcall(f)) end
			  return table.concat(r, "|")
			end`},
		{"floats with integer values into integer buffers", `
			function run() for i = 0, 7 do i32[i] = i * 2.0; u8[i] = 255.0 end; return i32[7], u8[3] end`},
		{"upvalue buffers", `
			local buf = f64
			function run() for i = 0, 7 do buf[i] = i * i end; local s = 0; for i = 0, 7 do s = s + buf[i] end; return s end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jit, interp, _ := runBothWith(t, tt.src, setup)
			if jit != interp {
				t.Fatalf("JIT %q, interpreter %q", jit, interp)
			}
		})
	}
}

func TestJITRuns(t *testing.T) {
	skipWithoutJIT(t)
	_, _, lj := runBoth(t, `function run() local a = 1; local b = a + 2; return b * 3 end`)
	if lj.jitRuns == 0 {
		t.Fatal("compiled code never ran")
	}
}

// A function that returns after a few instructions, such as a sort
// comparator Go calls, is interpreted even when compiled: entering compiled
// code and returning from it to Go costs more than the instructions.
func TestJITShortFunctionsCalledFromGo(t *testing.T) {
	skipWithoutJIT(t)
	saved, savedRun := jitThreshold, jitMinRun
	jitThreshold, jitMinRun = 0, defaultJITMinRun
	defer func() { jitThreshold, jitMinRun = saved, savedRun }()
	for _, tt := range []struct {
		name, src string
		enters    bool
	}{
		{"comparator", `f = function(a, b) return a < b end`, false},
		{"two instructions", `f = function(a, b) local c = a + b; return c * 2 end`, false},
		{"four instructions", `f = function(a, b) local c = a + b; c = c * 2; return c - 1, a end`, true},
		{"loop", `f = function(a, b) for i = 1, 2 do a = a + b end return a end`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := NewState()
			openLibraries(l)
			if err := l.DoString(tt.src); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				l.Global("f")
				l.PushNumber(1)
				l.PushNumber(2)
				l.Call(2, 1)
				l.Pop(1)
			}
			l.Global("f")
			if l.ToValue(-1).(*luaClosure).prototype.jit == nil {
				t.Fatal("f was not compiled")
			}
			if entered := l.jitRuns > 0; entered != tt.enters {
				t.Fatalf("compiled code ran %d times, want entered %v", l.jitRuns, tt.enters)
			}
		})
	}
}

// Random arithmetic over locals, checked against the interpreter.
func TestJITRandomArithmetic(t *testing.T) {
	skipWithoutJIT(t)
	r := rand.New(rand.NewPCG(1, 2))
	ops := []string{"+", "-", "*", "/"}
	for n := range 400 {
		var b strings.Builder
		// Half the programs start from integers, some near the ends of
		// their range, and mix in integer constants.
		start := "1.5, -2, 3.25, 1e-3"
		if n%2 == 1 {
			start = "7, -3, math.maxinteger - 5, 2^53 // 1"
		}
		fmt.Fprintf(&b, "function run()\n local v0, v1, v2, v3 = %s\n", start)
		for range 12 {
			dst := r.IntN(4)
			x := fmt.Sprintf("v%d", r.IntN(4))
			y := fmt.Sprintf("v%d", r.IntN(4))
			switch r.IntN(4) {
			case 0:
				y = fmt.Sprintf("%g", r.Float64()*10-5)
			case 1:
				y = fmt.Sprint(r.IntN(20) - 10)
			}
			if r.IntN(5) == 0 {
				x = "-" + x
			}
			op := ops[r.IntN(len(ops))]
			if n%2 == 0 && r.IntN(4) == 0 { // floats: no division by zero errors
				op = []string{"//", "%"}[r.IntN(2)]
			}
			fmt.Fprintf(&b, " v%d = %s %s %s\n", dst, x, op, y)
		}
		b.WriteString(" return v0, v1, v2, v3\nend")
		jit, interp, _ := runBoth(t, b.String())
		if jit != interp {
			t.Fatalf("program %d:\n%s\nJIT %q, interpreter %q", n, b.String(), jit, interp)
		}
	}
}

// Compiled code runs a generic for's TBC when its closing value is nil,
// and returns from a function with one unless a variable in its frame is
// to be closed: those Go closes.
func TestJITToBeClosed(t *testing.T) {
	skipWithoutJIT(t)
	src := `
		local log = {}
		local function closing(name)
		  return setmetatable({}, {__close = function() log[#log + 1] = name end})
		end
		local function find(t, x) -- returns from inside a pairs loop
		  for k, v in pairs(t) do
		    if v == x then return k end
		  end
		  return nil
		end
		local function closed(t) -- returns with a variable to close
		  for k in next, t, nil, closing("loop") do
		    return k
		  end
		end
		local function nested(n)
		  local c <close> = closing("outer" .. n)
		  if n > 0 then return nested(n - 1) + 1 end
		  return find({10, 20, 30}, 20)
		end
		function run()
		  local s = 0
		  for i = 1, 200 do
		    s = s + find({1, 2, 3, i}, i)
		    s = s + closed({5})
		    for _, v in ipairs({1, 2, 3}) do s = s + v end
		    local fs = {}
		    for k, v in pairs({4, 5, 6}) do -- closures: the loop's jump closes upvalues
		      fs[#fs + 1] = function() return k + v end
		      if k == 2 then break end
		    end
		    for _, f in ipairs(fs) do s = s + f() end
		  end
		  s = s + nested(3)
		  return s .. " " .. #log .. " " .. log[1] .. " " .. log[#log]
		end`
	jit, interp, _ := runBoth(t, src)
	if jit != interp {
		t.Fatalf("JIT %q, interpreter %q", jit, interp)
	}
}
