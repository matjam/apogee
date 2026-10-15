package lua

// Interrupt stops the Lua code running in l's state, on whichever of its
// threads is running, with the error "interrupted!" at its next loop
// iteration or tail call, as the standalone lua does on Ctrl-C. Unlike
// every other method, it may be called from any goroutine. A Go function
// the code called runs to completion first, and an interrupt that arrives
// once the outermost call from Go has returned is dropped.
func (l *State) Interrupt() { l.global.interrupt.Store(true) }

// interrupted raises the error Interrupt asked for. The interpreter and
// the JIT driver call it where they find the flag set.
//
//go:noinline
func (l *State) interrupted() {
	if l.global.interrupt.CompareAndSwap(true, false) {
		l.runtimeError("interrupted!")
	}
}

// interruptPoll is how many loop iterations the interpreter runs between
// checks for an interrupt. An atomic load on every iteration measured 6%
// slower on an interpreted numeric loop.
const interruptPoll = 1024

// pollInterrupt counts one loop iteration or tail call, and checks for an
// interrupt every interruptPoll of them.
func (l *State) pollInterrupt() {
	if l.interruptPoll--; l.interruptPoll <= 0 {
		l.checkInterrupt()
	}
}

// CountInstructions counts n steps of work that a Go function does for Lua
// as if they were n VM instructions. It advances the count hook that
// SetHook set with MaskCount, calling the hook when the count runs out,
// and checks for an Interrupt as a loop does. A Go function that can run
// for a long time calls it from its loop, so that a hook or Interrupt can
// stop it as they stop Lua code; string matching calls it as it
// backtracks. An error the hook raises, or the interrupt's, is raised from
// here, as if by the Go function. The hook runs at most once per call.
// n <= 0 counts nothing.
func (l *State) CountInstructions(n int) {
	if n <= 0 {
		return
	}
	if l.hookMask&MaskCount != 0 {
		if l.hookCount -= n; l.hookCount <= 0 {
			l.resetHookCount()
			l.hook(HookCount, -1)
		}
	}
	if int(l.interruptPoll) <= n {
		l.checkInterrupt()
	} else {
		l.interruptPoll -= int32(n)
	}
}

//go:noinline
func (l *State) checkInterrupt() {
	l.interruptPoll = interruptPoll
	if l.global.interrupt.Load() {
		l.interrupted()
	}
}

// dropInterrupt clears an interrupt that arrived after the code it was for
// had finished.
func (l *State) dropInterrupt() {
	if l.global.interrupt.Load() {
		l.global.interrupt.Store(false)
	}
}
