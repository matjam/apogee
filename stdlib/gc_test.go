package stdlib_test

import (
	"strings"
	"testing"
	"time"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// While the collector is stopped, "count" grows by what the script
// allocates, even a few small objects, and a collection lowers it.
func TestCountWhileStopped(t *testing.T) {
	run(t, `
		collectgarbage("stop")
		for round = 1, 20 do
		  collectgarbage()
		  local before = collectgarbage("count")
		  local t = {}
		  for i = 1, 100 do t[i] = {} end
		  local after = collectgarbage("count")
		  assert(after > before, round)
		  t = nil
		  repeat until collectgarbage("step", 0)
		  assert(collectgarbage("count") < after, round)
		end
		collectgarbage("restart")
	`)
}

// Weak tables lose the entries whose objects a collection does not reach:
// weak values, weak keys, and ephemerons, whose values live while their
// keys do. Strings and numbers are values, never removed.
func TestWeakTables(t *testing.T) {
	run(t, `
		local keep = {}
		local wv = setmetatable({}, {__mode = "v"})
		wv[1], wv[2], wv.s, wv.n = {}, keep, "str", 42
		local wk = setmetatable({}, {__mode = "k"})
		local k1 = {}
		wk[k1], wk[{}], wk.s = 1, 2, 3
		local e = {}
		wk[e] = {e} -- the value refers to its own key
		collectgarbage()
		assert(wv[1] == nil and wv[2] == keep and wv.s == "str" and wv.n == 42)
		local n = 0
		for _ in pairs(wk) do n = n + 1 end
		assert(n == 3 and wk[k1] == 1 and wk[e][1] == e)
		e = nil
		collectgarbage()
		n = 0
		for _ in pairs(wk) do n = n + 1 end
		assert(n == 2, n) -- the ephemeron went with its key

		-- __mode read from a metatable changed after it was set
		local mt = {}
		local late = setmetatable({}, mt)
		late[1] = {}
		mt.__mode = "v"
		collectgarbage()
		assert(late[1] == nil)
	`)
}

// Finalizers run once for an unreachable object, which they resurrect,
// newest first, cycles included; errors in them are reported.
func TestFinalizers(t *testing.T) {
	run(t, `
		local log = {}
		local mt = {__gc = function(o) log[#log + 1] = o.name; saved = o end}
		do
			setmetatable({name = "a"}, mt)
			setmetatable({name = "b"}, mt)
			local c = setmetatable({name = "c"}, mt)
			c.self = c -- a cycle
		end
		collectgarbage()
		assert(table.concat(log, ",") == "c,b,a", table.concat(log, ","))
		assert(saved.name == "a") -- resurrected
		saved = nil
		collectgarbage()
		assert(#log == 3) -- only once

		-- __gc is read when the metatable is set, as in C Lua
		local late = {}
		setmetatable({}, late)
		late.__gc = function() log[#log + 1] = "late" end
		collectgarbage()
		assert(#log == 3)
	`)
}

// An error in a finalizer is a warning, as in Lua 5.4 on.
func TestFinalizerErrorWarns(t *testing.T) {
	l := lua.NewState()
	stdlib.Open(l)
	var b strings.Builder
	l.SetWarnFunction(lua.StderrWarnings(&b))
	if err := l.DoString(`
		warn("@on")
		setmetatable({}, {__gc = function() error("boom", 0) end})
		collectgarbage()
		warn("done", "!")`); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "Lua warning: error in __gc (boom)\nLua warning: done!\n"; got != want {
		t.Errorf("warnings %q, want %q", got, want)
	}
}

// Collections run automatically as the state allocates, once a metatable
// with __gc or __mode exists, unless stopped.
func TestAutomaticCollection(t *testing.T) {
	run(t, `
		local done = false
		setmetatable({}, {__gc = function() done = true end})
		local n = 0
		repeat n = n + 1; local t = {} until done or n > 5e7
		assert(done, "no automatic collection")

		assert(collectgarbage("isrunning"))
		collectgarbage("stop")
		assert(not collectgarbage("isrunning"))
		done = false
		setmetatable({}, {__gc = function() done = true end})
		for i = 1, 2e5 do local t = {} end
		assert(not done, "collected while stopped")
		local x = collectgarbage("count")
		for i = 1, 2e4 do local t = {} end
		assert(collectgarbage("count") > x) -- memory grows while stopped
		collectgarbage("restart")
		collectgarbage()
		assert(done)
		assert(collectgarbage("step", 1e6) == true)
	`)
}

// WithoutFinalizers: __gc never runs, in a collection or at Close, so a
// finalizer that loops can't hang the host. Weak tables still work.
func TestWithoutFinalizers(t *testing.T) {
	l := lua.NewState(lua.WithoutFinalizers())
	stdlib.Open(l)
	err := l.DoString(`
		ran = false
		setmetatable({}, {__gc = function() ran = true end})
		setmetatable({}, {__gc = function() while true do end end})
		local wv = setmetatable({}, {__mode = "v"})
		wv[1] = {}
		collectgarbage()
		collectgarbage()
		assert(not ran, "a finalizer ran")
		assert(wv[1] == nil, "weak tables still clear")
	`)
	if err != nil {
		msg, _ := l.ToString(-1)
		t.Fatal(err, msg)
	}
	done := make(chan struct{})
	go func() { l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close ran a finalizer")
	}
}

// By default a finalizer still runs, in a collection or at Close.
func TestFinalizersRunByDefault(t *testing.T) {
	l := lua.NewState()
	stdlib.Open(l)
	var log []string
	l.Register("note", func(l *lua.State) int { log = append(log, l.CheckString(1)); return 0 })
	if err := l.DoString(`
		setmetatable({}, {__gc = function() note("collected") end})
		collectgarbage()
		keep = setmetatable({}, {__gc = function() note("closed") end})
	`); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if strings.Join(log, ",") != "collected,closed" {
		t.Fatalf("finalizers ran %v, want [collected closed]", log)
	}
}

// A chain of ephemeron entries, each key the previous entry's value, stays
// alive while its head is reachable and goes when it isn't. Marking it
// takes time in proportion to its length: a 40,000-key chain, ordered so
// each key is found only after the entry before it, once took minutes.
func TestEphemeronChains(t *testing.T) {
	l := lua.NewState()
	stdlib.Open(l)
	start := time.Now()
	err := l.DoString(`
		local n = 40000
		local eph = setmetatable({}, {__mode = "k"})
		local keys = {}
		for i = 1, n + 1 do keys[i] = {} end
		for i = n, 1, -1 do eph[keys[i]] = keys[i + 1] end -- reversed: the worst order to scan
		head = keys[1]
		keys = nil
		collectgarbage()
		local count = 0
		for _ in pairs(eph) do count = count + 1 end
		assert(count == n, "the chain lost entries: " .. count)
		head = nil
		collectgarbage()
		collectgarbage()
		assert(next(eph) == nil, "the chain outlived its head")
	`)
	if err != nil {
		msg, _ := l.ToString(-1)
		t.Fatal(err, msg)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("collecting a 40,000-key ephemeron chain took %v", d)
	}
}
