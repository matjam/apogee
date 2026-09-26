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
	xs := []float64{0, math.Copysign(0, -1), 1, -1, math.Pi, math.Pi / 2, math.Pi / 4, 1<<29 - 1, -(1<<29 - 1), 1e-300, 5e-324, 1e-8}
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
		{"break is not a kernel", "", `function run() local s = 0; for i = 1, 10 do s = s + i; if s > 20 then break end end; return s end`},
		{"calls are not kernels", "", `function run() local s = 0; for i = 1, 10 do s = s + math.floor(i / 2) end; return s end`},
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
