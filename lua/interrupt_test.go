package lua_test

import (
	"strings"
	"testing"
	"time"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// Interrupt, from another goroutine, stops every kind of loop, interpreted
// and compiled, and leaves the state usable.
func TestInterrupt(t *testing.T) {
	loops := map[string]string{
		"while":            `while true do end`,
		"repeat":           `repeat until false`,
		"numeric for":      `for i = 1, math.huge do end`,
		"kernel":           `local s = 0; for i = 1, math.huge do s = s + i * 0.5 end`,
		"generic for":      `for _ in function() return 1 end do end`,
		"tail calls":       `local function f() return f() end; return f()`,
		"Lua calls":        `local function f(x) return x + 1 end; local n = 0; while true do n = f(n) end`,
		"calls into Go":    `local s = 0; while true do s = s + math.abs(-1) + #tostring(s) end`,
		"interpreted step": `local s; while true do s = "x" .. "y" end`,
		"pattern matching": `string.find(string.rep("a", 3000), ".-.-.-.-.-b")`,
	}
	for name, src := range loops {
		for _, jit := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "/jit", false: "/interpreted"}[jit], func(t *testing.T) {
				var opts []lua.Option
				if !jit {
					opts = append(opts, lua.WithoutJIT())
				}
				l := lua.NewState(opts...)
				stdlib.Open(l)
				done := make(chan error)
				go func() { done <- l.DoString(src) }()
				time.Sleep(20 * time.Millisecond)
				l.Interrupt()
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), "interrupted!") {
						t.Fatalf("got %v, want interrupted!", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("not interrupted")
				}
				l.SetTop(0)
				if err := l.DoString(`local s = 0; for i = 1, 3000 do s = s + i end; assert(s == 4501500)`); err != nil {
					t.Fatalf("after the interrupt: %v", err)
				}
			})
		}
	}
}

// An interrupt that arrives when nothing is running is dropped once the
// next call from Go returns, rather than stopping the call after it.
func TestInterruptWhileIdle(t *testing.T) {
	l := lua.NewState()
	stdlib.Open(l)
	l.Interrupt()
	for range 2 {
		if err := l.DoString(`local s = 0; for i = 1, 10 do s = s + i end; return s`); err != nil && !strings.Contains(err.Error(), "interrupted!") {
			t.Fatal(err)
		}
		l.SetTop(0)
	}
	if err := l.DoString(`for i = 1, 10 do end`); err != nil {
		t.Fatalf("a dropped interrupt stopped a later call: %v", err)
	}
}

// CountInstructions advances the count hook, calling it at most once per
// call, and checks for an interrupt, so a long Go function can be stopped.
func TestCountInstructions(t *testing.T) {
	l := lua.NewState()
	calls := 0
	l.SetHook(func(*lua.State, lua.Debug) { calls++ }, lua.MaskCount, 10)
	for _, step := range []struct{ n, want int }{
		{0, 0}, {-5, 0}, {9, 0}, {1, 1}, {25, 2}, {9, 2}, {1, 3},
	} {
		l.CountInstructions(step.n)
		if calls != step.want {
			t.Fatalf("after CountInstructions(%d): hook ran %d times, want %d", step.n, calls, step.want)
		}
	}

	l.SetHook(nil, 0, 0)
	l.Register("work", func(l *lua.State) int {
		for range 100 {
			l.CountInstructions(1000)
		}
		return 0
	})
	l.Interrupt()
	if err := l.DoString(`work()`); err == nil || !strings.Contains(err.Error(), "interrupted!") {
		t.Fatalf("got %v, want interrupted!", err)
	}
}
