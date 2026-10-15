package stdlib_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// The table library's element loops run the count hook, so a hook can stop
// them partway: a huge move, and an insert or remove that shifts a big
// table, or a table whose __len claims to be huge.
func TestTableLoopsRunTheCountHook(t *testing.T) {
	cases := map[string]string{
		"move a huge range": `table.move({}, 1, 1e12, 2)`,
		"remove from a big table": `
			local t = {}
			for i = 1, 300000 do t[i] = i end
			arm()
			table.remove(t, 1)`,
		"insert into a big table": `
			local t = {}
			for i = 1, 300000 do t[i] = i end
			arm()
			table.insert(t, 1, 0)`,
		"remove with a huge __len": `
			local t = setmetatable({}, {__len = function() return 1e15 end})
			arm()
			table.remove(t, 1)`,
		"insert with a huge __len": `
			local t = setmetatable({}, {__len = function() return 1e15 end})
			arm()
			table.insert(t, 1, 0)`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			l := lua.NewState()
			stdlib.Open(l)
			armed := !strings.Contains(script, "arm()")
			l.Register("arm", func(*lua.State) int { armed = true; return 0 })
			l.SetHook(func(l *lua.State, _ lua.Debug) {
				if armed {
					l.Errorf("stopped by the hook")
				}
			}, lua.MaskCount, 1000)
			done := make(chan error, 1)
			go func() { done <- l.DoString(script) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "stopped by the hook") {
					t.Fatalf("got %v, want the hook's error", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hook did not stop the loop")
			}
		})
	}
}

// Call frames count against the allocation limit, so deep recursion stops
// near the limit rather than allocating many times it before the stack
// overflows.
func TestDeepRecursionCountsItsFrames(t *testing.T) {
	const limit = 16 << 20
	l := lua.NewState()
	stdlib.Open(l)
	l.SetAllocationLimit(limit)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := l.DoString(`local function f(n) return 1 + f(n + 1) end; f(1)`)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("unbounded recursion succeeded")
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 3*limit {
		t.Fatalf("recursion allocated %d MB under a %d MB limit", used>>20, limit>>20)
	}
}

// Growing the stack past the allocation limit raises the memory error, not
// unpack's ordinary "too many results", so a host's pcall can tell that the
// budget ran out, and without a pcall the call fails with ErrMemory.
func TestCheckStackRaisesTheMemoryError(t *testing.T) {
	l := lua.NewState()
	stdlib.Open(l)
	l.SetAllocationLimit(8 << 20)
	if err := l.DoString(`
		local ok, err = pcall(table.unpack, {}, 1, 999000)
		assert(not ok and err == "not enough memory", tostring(err))
	`); err != nil {
		t.Fatal(err)
	}
	l.SetTop(0)
	l.SetAllocationLimit(8 << 20)
	if err := l.DoString(`table.unpack({}, 1, 999000)`); !errors.Is(err, lua.ErrMemory) {
		t.Fatalf("got %v, want ErrMemory", err)
	}
}
