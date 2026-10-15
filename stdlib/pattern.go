package stdlib

import (
	"strings"

	"github.com/matjam/apogee/lua"
)

// Lua 5.5's pattern matching, ported from lstrlib.c. Positions are byte
// offsets into the subject and the pattern, and -1 is C's NULL: no match.
// Reading at the end of either string gives 0, as C reads the terminating
// NUL there.

const (
	maxCaptures   = 32  // LUA_MAXCAPTURES
	maxMatchCalls = 200 // MAXCCALLS
	// matchStepBatch is how many match calls run between reports to
	// CountInstructions. A pattern such as ".-.-.-.-b" backtracks through
	// exponentially many of them, so without the reports a count hook or
	// Interrupt couldn't stop it.
	matchStepBatch  = 256
	capUnfinished   = -1
	capPosition     = -2
	patternSpecials = "^$*+?.([%-"
)

type matchState struct {
	l       *lua.State
	src     string
	pat     string
	depth   int // match calls left before "pattern too complex"
	steps   int // match calls not yet reported to CountInstructions
	level   int // captures started
	capture [maxCaptures]struct{ init, len int }
}

func newMatchState(l *lua.State, src, pat string) *matchState {
	return &matchState{l: l, src: src, pat: pat}
}

// reset prepares ms for a match attempt.
func (ms *matchState) reset() {
	ms.level = 0
	ms.depth = maxMatchCalls
}

func (ms *matchState) errorf(format string, args ...any) {
	ms.l.Errorf(format, args...)
	panic("unreachable")
}

// s and p return the subject's and pattern's byte at i, or 0 at the end.
func (ms *matchState) s(i int) byte {
	if i < len(ms.src) {
		return ms.src[i]
	}
	return 0
}

func (ms *matchState) p(i int) byte {
	if i < len(ms.pat) {
		return ms.pat[i]
	}
	return 0
}

func (ms *matchState) checkCapture(c byte) int {
	l := int(c) - '1'
	if l < 0 || l >= ms.level || ms.capture[l].len == capUnfinished {
		ms.errorf("invalid capture index %%%d", l+1)
	}
	return l
}

func (ms *matchState) captureToClose() int {
	for level := ms.level - 1; level >= 0; level-- {
		if ms.capture[level].len == capUnfinished {
			return level
		}
	}
	ms.errorf("invalid pattern capture")
	return 0
}

// classEnd returns the end of the single-character class at p.
func (ms *matchState) classEnd(p int) int {
	c := ms.pat[p]
	p++
	switch c {
	case '%':
		if p >= len(ms.pat) {
			ms.errorf("malformed pattern (ends with '%%')")
		}
		return p + 1
	case '[':
		if ms.p(p) == '^' {
			p++
		}
		for { // look for a ']'
			if p >= len(ms.pat) {
				ms.errorf("malformed pattern (missing ']')")
			}
			c := ms.pat[p]
			p++
			if c == '%' && p < len(ms.pat) {
				p++ // skip escapes, such as '%]'
			}
			if ms.p(p) == ']' {
				return p + 1
			}
		}
	}
	return p
}

// matchClass reports whether c is in the class %cl, with the C locale's
// character classes: only ASCII is classified.
func matchClass(c, cl byte) bool {
	var res bool
	switch toLower(cl) {
	case 'a':
		res = isAlpha(c)
	case 'c':
		res = c < ' ' || c == 0x7f
	case 'd':
		res = isDigit(c)
	case 'g':
		res = isGraph(c)
	case 'l':
		res = 'a' <= c && c <= 'z'
	case 'p':
		res = isGraph(c) && !isAlpha(c) && !isDigit(c)
	case 's':
		res = c == ' ' || '\t' <= c && c <= '\r'
	case 'u':
		res = 'A' <= c && c <= 'Z'
	case 'w':
		res = isAlpha(c) || isDigit(c)
	case 'x':
		res = isDigit(c) || 'a' <= toLower(c) && toLower(c) <= 'f'
	case 'z': // deprecated
		res = c == 0
	default:
		return cl == c
	}
	if 'a' <= cl && cl <= 'z' {
		return res
	}
	return !res
}

func isAlpha(c byte) bool { return 'a' <= toLower(c) && toLower(c) <= 'z' }
func isDigit(c byte) bool { return '0' <= c && c <= '9' }
func isGraph(c byte) bool { return '!' <= c && c <= '~' }

func toLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// matchBracketClass reports whether c is in the set [...] from p to ec,
// the position of its ']'.
func (ms *matchState) matchBracketClass(c byte, p, ec int) bool {
	sig := true
	if ms.p(p+1) == '^' {
		sig = false
		p++ // skip the '^'
	}
	for p++; p < ec; p++ {
		switch {
		case ms.pat[p] == '%':
			p++
			if matchClass(c, ms.p(p)) {
				return sig
			}
		case ms.p(p+1) == '-' && p+2 < ec:
			p += 2
			if ms.pat[p-2] <= c && c <= ms.pat[p] {
				return sig
			}
		case ms.pat[p] == c:
			return sig
		}
	}
	return !sig
}

func (ms *matchState) singleMatch(s, p, ep int) bool {
	if s >= len(ms.src) {
		return false
	}
	c := ms.src[s]
	switch ms.pat[p] {
	case '.':
		return true
	case '%':
		return matchClass(c, ms.p(p+1))
	case '[':
		return ms.matchBracketClass(c, p, ep-1)
	}
	return ms.pat[p] == c
}

func (ms *matchState) matchBalance(s, p int) int {
	if p >= len(ms.pat)-1 {
		ms.errorf("malformed pattern (missing arguments to '%%b')")
	}
	if ms.s(s) != ms.pat[p] {
		return -1
	}
	b, e, cont := ms.pat[p], ms.pat[p+1], 1
	for s++; s < len(ms.src); s++ {
		if c := ms.src[s]; c == e {
			if cont--; cont == 0 {
				return s + 1
			}
		} else if c == b {
			cont++
		}
	}
	return -1 // the subject ends out of balance
}

func (ms *matchState) maxExpand(s, p, ep int) int {
	i := 0 // the most repetitions of the item that match
	for ms.singleMatch(s+i, p, ep) {
		i++
	}
	for ; i >= 0; i-- { // try the rest with fewer and fewer
		if res := ms.match(s+i, ep+1); res != -1 {
			return res
		}
	}
	return -1
}

func (ms *matchState) minExpand(s, p, ep int) int {
	for {
		if res := ms.match(s, ep+1); res != -1 {
			return res
		} else if !ms.singleMatch(s, p, ep) {
			return -1
		}
		s++ // try with one more repetition
	}
}

func (ms *matchState) startCapture(s, p, what int) int {
	level := ms.level
	if level >= maxCaptures {
		ms.errorf("too many captures")
	}
	ms.capture[level].init, ms.capture[level].len = s, what
	ms.level = level + 1
	res := ms.match(s, p)
	if res == -1 {
		ms.level-- // undo the capture
	}
	return res
}

func (ms *matchState) endCapture(s, p int) int {
	l := ms.captureToClose()
	ms.capture[l].len = s - ms.capture[l].init // close the capture
	res := ms.match(s, p)
	if res == -1 {
		ms.capture[l].len = capUnfinished // undo it
	}
	return res
}

func (ms *matchState) matchCapture(s int, c byte) int {
	l := ms.checkCapture(c)
	init, n := ms.capture[l].init, ms.capture[l].len
	// A position capture's length is negative: it matches nothing, as its
	// size_t length is huge in C.
	if n >= 0 && len(ms.src)-s >= n && ms.src[init:init+n] == ms.src[s:s+n] {
		return s + n
	}
	return -1
}

// match matches the pattern from p against the subject from s, and returns
// the end of the match or -1.
func (ms *matchState) match(s, p int) int {
	if ms.depth == 0 {
		ms.errorf("pattern too complex")
	}
	if ms.steps++; ms.steps == matchStepBatch {
		ms.steps = 0
		ms.l.CountInstructions(matchStepBatch)
	}
	ms.depth--
	s = ms.doMatch(s, p)
	ms.depth++
	return s
}

// doMatch is match's body. Where lstrlib.c jumps back to its start to
// avoid a tail call, it continues its loop.
func (ms *matchState) doMatch(s, p int) int {
	for p < len(ms.pat) {
		switch ms.pat[p] {
		case '(':
			if ms.p(p+1) == ')' { // position capture
				return ms.startCapture(s, p+2, capPosition)
			}
			return ms.startCapture(s, p+1, capUnfinished)
		case ')':
			return ms.endCapture(s, p+1)
		case '$':
			if p+1 == len(ms.pat) { // '$' at the end anchors the match
				if s == len(ms.src) {
					return s
				}
				return -1
			}
		case '%':
			switch ms.p(p + 1) {
			case 'b': // balanced string
				if s = ms.matchBalance(s, p+2); s == -1 {
					return -1
				}
				p += 4
				continue
			case 'f': // frontier
				p += 2
				if ms.p(p) != '[' {
					ms.errorf("missing '[' after '%%f' in pattern")
				}
				ep := ms.classEnd(p)
				var previous byte
				if s > 0 {
					previous = ms.src[s-1]
				}
				if !ms.matchBracketClass(previous, p, ep-1) && ms.matchBracketClass(ms.s(s), p, ep-1) {
					p = ep
					continue
				}
				return -1
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9': // back reference
				if s = ms.matchCapture(s, ms.pat[p+1]); s == -1 {
					return -1
				}
				p += 2
				continue
			}
		}
		// A single-character class and an optional suffix.
		ep := ms.classEnd(p)
		if !ms.singleMatch(s, p, ep) {
			if c := ms.p(ep); c == '*' || c == '?' || c == '-' { // may match empty
				p = ep + 1
				continue
			}
			return -1
		}
		switch ms.p(ep) {
		case '?':
			if res := ms.match(s+1, ep+1); res != -1 {
				return res
			}
			p = ep + 1
			continue
		case '+':
			return ms.maxExpand(s+1, p, ep)
		case '*':
			return ms.maxExpand(s, p, ep)
		case '-':
			return ms.minExpand(s, p, ep)
		}
		s, p = s+1, ep
	}
	return s
}

// pushCapture pushes capture i, or the whole match from s to e if the
// pattern has no captures.
func (ms *matchState) pushCapture(i, s, e int) {
	if i >= ms.level {
		if i != 0 {
			ms.errorf("invalid capture index %%%d", i+1)
		}
		ms.l.PushString(ms.src[s:e])
		return
	}
	switch c := ms.capture[i]; c.len {
	case capUnfinished:
		ms.errorf("unfinished capture")
	case capPosition:
		ms.l.PushInteger(c.init + 1)
	default:
		ms.l.PushString(ms.src[c.init : c.init+c.len])
	}
}

// pushCaptures pushes the captures, or the whole match from s to e if
// there are none and s is not -1, and returns how many it pushed.
func (ms *matchState) pushCaptures(s, e int) int {
	n := ms.level
	if n == 0 && s != -1 {
		n = 1
	}
	ms.l.CheckStackWithMessage(n, "too many captures")
	for i := range n {
		ms.pushCapture(i, s, e)
	}
	return n
}

// find is string.find and, if !isFind, string.match.
func find(l *lua.State, isFind bool) int {
	s, p := l.CheckString(1), l.CheckString(2)
	init := relativePosition(optInt(l, 3, 1), len(s))
	if init < 1 {
		init = 1
	} else if init > len(s)+1 { // starts after the end
		l.PushNil()
		return 1
	}
	if isFind && (l.ToBoolean(4) || !strings.ContainsAny(p, patternSpecials)) {
		if start := strings.Index(s[init-1:], p); start >= 0 {
			l.PushInteger(start + init)
			l.PushInteger(start + init + len(p) - 1)
			return 2
		}
	} else {
		anchor := len(p) > 0 && p[0] == '^'
		if anchor {
			p = p[1:]
		}
		ms := newMatchState(l, s, p)
		for s1 := init - 1; ; s1++ {
			ms.reset()
			if e := ms.match(s1, 0); e != -1 {
				if isFind {
					l.PushInteger(s1 + 1)
					l.PushInteger(e)
					return ms.pushCaptures(-1, 0) + 2
				}
				return ms.pushCaptures(s1, e)
			}
			if s1 >= len(s) || anchor {
				break
			}
		}
	}
	l.PushNil()
	return 1
}

// gmatch is string.gmatch(s, pattern [, init]), after lstrlib.c: its
// iterator is a Go closure whose upvalues are the subject, the pattern,
// the position to go on from, and the end of the last match, where an
// empty match does not count (Lua 5.3.3's rule).
func gmatch(l *lua.State) int {
	s := l.CheckString(1)
	l.CheckString(2)
	init := unpackPosition(l.OptInteger(3, 1), len(s)) - 1
	if init > len(s) { // after the end
		init = len(s) + 1
	}
	l.SetTop(2)
	l.PushInteger(init)
	l.PushInteger(-1) // no last match
	l.PushGoClosure(gmatchNext, 4)
	return 1
}

func gmatchNext(l *lua.State) int {
	s, _ := l.ToString(lua.UpValueIndex(1))
	p, _ := l.ToString(lua.UpValueIndex(2))
	start, _ := l.ToInteger(lua.UpValueIndex(3))
	last, _ := l.ToInteger(lua.UpValueIndex(4))
	ms := newMatchState(l, s, p)
	for src := int(start); src <= len(s); src++ {
		ms.reset()
		if e := ms.match(src, 0); e != -1 && e != int(last) {
			l.PushInteger(e)
			l.Replace(lua.UpValueIndex(3))
			l.PushInteger(e)
			l.Replace(lua.UpValueIndex(4))
			return ms.pushCaptures(src, e)
		}
	}
	return 0
}

func gsub(l *lua.State) int {
	src, p := l.CheckString(1), l.CheckString(2)
	tr := l.TypeOf(3)
	maxN := optInt(l, 4, len(src)+1) // negative is unlimited, as in C
	l.ArgumentCheck(tr == lua.TypeNumber || tr == lua.TypeString || tr == lua.TypeFunction || tr == lua.TypeTable, 3, "string/function/table expected")
	anchor := len(p) > 0 && p[0] == '^'
	if anchor {
		p = p[1:]
	}
	var repl string
	if tr == lua.TypeNumber || tr == lua.TypeString {
		repl, _ = l.ToString(3)
	}
	ms := newMatchState(l, src, p)
	var b strings.Builder
	s, n, last := 0, 0, -1 // last: the end of the last match, where an empty one does not count
	changed := false
	for maxN < 0 || n < maxN {
		ms.reset()
		if e := ms.match(s, 0); e != -1 && e != last {
			n++
			changed = ms.addValue(&b, s, e, tr, repl) || changed
			l.CheckAllocation(b.Len()) // the result, which replacements can make far longer than src
			s, last = e, e
		} else if s < len(src) {
			b.WriteByte(src[s])
			s++
		} else {
			break
		}
		if anchor {
			break
		}
	}
	if changed {
		b.WriteString(src[s:])
		l.PushString(b.String())
	} else {
		l.PushValue(1) // the original string, as lstrlib.c returns it
	}
	l.PushInteger(n)
	return 2
}

// addValue appends the replacement for the match from s to e.
// It reports whether the text changed: a function or table that gives nil
// or false keeps the original.
func (ms *matchState) addValue(b *strings.Builder, s, e int, tr lua.Type, repl string) bool {
	l := ms.l
	switch tr {
	case lua.TypeFunction:
		l.PushValue(3)
		l.Call(ms.pushCaptures(s, e), 1)
	case lua.TypeTable:
		ms.pushCapture(0, s, e)
		l.Table(3)
	default:
		ms.addString(b, s, e, repl)
		return true
	}
	changed := true
	if !l.ToBoolean(-1) { // nil or false keeps the original text
		b.WriteString(ms.src[s:e])
		changed = false
	} else if !l.IsString(-1) {
		ms.errorf("invalid replacement value (a %s)", l.TypeOf(-1).String())
	} else {
		r, _ := l.ToString(-1)
		b.WriteString(r)
	}
	l.Pop(1)
	return changed
}

// addString appends repl with its %0 to %9 replaced by the match and its
// captures. It checks the allocation limit before copying a match or a
// capture: one replacement can copy the match many times, as "%0%0%0..."
// does, so checking once the replacement is built could let it grow to
// many times the limit first. Literal text is bounded by repl itself.
func (ms *matchState) addString(b *strings.Builder, s, e int, repl string) {
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		i++ // skip the '%'
		var d byte
		if i < len(repl) {
			d = repl[i]
		}
		switch {
		case !isDigit(d):
			if d != '%' {
				ms.errorf("invalid use of '%%' in replacement string")
			}
			b.WriteByte(d)
		case d == '0':
			ms.l.CheckAllocation(b.Len() + e - s)
			b.WriteString(ms.src[s:e])
		default:
			ms.pushCapture(int(d-'1'), s, e)
			r, _ := ms.l.ToString(-1)
			ms.l.CheckAllocation(b.Len() + len(r))
			b.WriteString(r)
			ms.l.Pop(1)
		}
	}
}
