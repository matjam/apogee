package stdlib

import (
	"io"
	"os"
	"strings"

	"github.com/matjam/apogee/lua"
)

var gcOptions = []string{"stop", "restart", "collect", "count", "step", "isrunning", "generational", "incremental", "param"}

// gcOptionValues are the GC options for gcOptions, in order.
var gcOptionValues = []lua.GCOption{lua.GCStop, lua.GCRestart, lua.GCCollect, lua.GCCount, lua.GCStep,
	lua.GCIsRunning, lua.GCGenerational, lua.GCIncremental, lua.GCParam}

// gcParams are collectgarbage "param"'s parameters, as lua.GCP values.
var gcParams = []string{"minormul", "majorminor", "minormajor", "pause", "stepmul", "stepsize"}

// collectGarbage is collectgarbage, after lbaselib.c's luaB_collectgarbage.
// See State.GC for what each option does in apogee.
// Inside a finalizer, as in C Lua, it returns fail.
func collectGarbage(l *lua.State) int {
	o := gcOptionValues[l.CheckOption(1, "collect", gcOptions)]
	var res int
	switch o {
	case lua.GCStep:
		res = l.GC(o, int(l.OptInteger(2, 0)))
	case lua.GCParam:
		p := l.CheckOption(2, "", gcParams)
		l.PushInteger(int64(l.GC(o, p, int(l.OptInteger(3, -1)))))
		return 1
	default:
		res = l.GC(o)
	}
	switch {
	case res == -1:
		l.PushNil() // fail
	case o == lua.GCCount:
		l.PushNumber(float64(res) + float64(l.GC(lua.GCCountBytes))/1024) // kilobytes, with the remainder
	case o == lua.GCStep, o == lua.GCIsRunning:
		l.PushBoolean(res != 0)
	case o == lua.GCGenerational, o == lua.GCIncremental:
		if lua.GCOption(res) == lua.GCIncremental {
			l.PushString("incremental")
		} else {
			l.PushString("generational")
		}
	default:
		l.PushInteger(int64(res))
	}
	return 1
}

// baseError is error(message [, level]).
func baseError(l *lua.State) int {
	level := l.OptInteger(2, 1)
	l.SetTop(1)
	if l.TypeOf(1) == lua.TypeString && level > 0 { // a string, not a number, as luaB_error
		l.Where(int(level))
		l.PushValue(1)
		l.Concat(2)
	}
	l.Error()
	panic("unreachable")
}

func finishProtectedCall(l *lua.State, status bool) int {
	if !l.CheckStack(1) {
		l.SetTop(0) // create space for return values
		l.PushBoolean(false)
		l.PushString("stack overflow")
		return 2 // return false, message
	}
	l.PushBoolean(status) // first result (status)
	l.Replace(1)          // put first result in the first slot
	return l.Top()
}

func protectedCallContinuation(l *lua.State) int {
	_, shouldYield, _ := l.Context()
	return finishProtectedCall(l, shouldYield)
}

func loadHelper(l *lua.State, s error, e int) int {
	if s == nil {
		if e != 0 {
			l.PushValue(e)
			if _, ok := l.SetUpValue(-2, 1); !ok {
				l.Pop(1)
			}
		}
		return 1
	}
	l.PushNil()
	l.Insert(-2)
	return 2
}

type genericReader struct {
	l *lua.State
	r *strings.Reader
	e error
}

func (r *genericReader) Read(b []byte) (n int, err error) {
	if r.e != nil {
		return 0, r.e
	}
	if l := r.l; r.r == nil {
		// The chunk ends at the first nil or empty piece. r.e keeps it
		// ended, as bufio reads again after an io.EOF.
		l.CheckStackWithMessage(2, "too many nested functions")
		l.PushValue(1)
		l.Call(0, 1)
		if !l.IsNil(-1) && !l.IsString(-1) {
			l.Errorf("reader function must return a string")
		}
		s, _ := l.ToString(-1)
		l.Pop(1)
		if s == "" {
			r.e = io.EOF
			return 0, io.EOF
		}
		r.r = strings.NewReader(s)
	}
	if n, err = r.r.Read(b); err == io.EOF {
		r.r, err = nil, nil
	} else if err != nil {
		r.e = err
	}
	return
}

var baseLibrary = []lua.RegistryFunction{
	{Name: "assert", Function: func(l *lua.State) int {
		if l.ToBoolean(1) {
			return l.Top()
		}
		// The message, which may be any value, as luaB_assert raises it.
		l.CheckAny(1)
		l.Remove(1)
		l.PushString("assertion failed!")
		l.SetTop(1)
		return baseError(l)
	}},
	{Name: "collectgarbage", Function: collectGarbage},
	{Name: "dofile", Function: func(l *lua.State) int {
		f := l.OptString(1, "")
		if l.SetTop(1); l.LoadFile(f, "") != nil {
			l.Error()
			panic("unreachable")
		}
		continuation := func(l *lua.State) int { return l.Top() - 1 }
		l.CallWithContinuation(0, lua.MultipleReturns, 0, continuation)
		return continuation(l)
	}},
	{Name: "error", Function: baseError},
	{Name: "warn", Function: func(l *lua.State) int { // after lbaselib.c's luaB_warn
		n := l.Top()
		l.CheckString(1) // at least one argument
		for i := 2; i <= n; i++ {
			l.CheckString(i) // all strings
		}
		for i := 1; i < n; i++ {
			s, _ := l.ToString(i)
			l.Warning(s, true)
		}
		s, _ := l.ToString(n)
		l.Warning(s, false)
		return 0
	}},
	{Name: "getmetatable", Function: func(l *lua.State) int {
		l.CheckAny(1)
		if !l.MetaTable(1) {
			l.PushNil()
			return 1
		}
		l.MetaField(1, "__metatable")
		return 1
	}},
	{Name: "loadfile", Function: func(l *lua.State) int {
		f, m, e := l.OptString(1, ""), l.OptString(2, ""), 3
		if l.IsNone(e) {
			e = 0
		}
		return loadHelper(l, l.LoadFile(f, m), e)
	}},
	{Name: "load", Function: func(l *lua.State) int {
		m, e := l.OptString(3, "bt"), 4
		l.ArgumentCheck(!strings.Contains(m, "B"), 3, "invalid mode") // C's fixed buffers
		if l.IsNone(e) {
			e = 0
		}
		var err error
		if s, ok := l.ToString(1); ok {
			err = l.LoadBuffer(s, l.OptString(2, s), m)
		} else {
			chunkName := l.OptString(2, "=(load)")
			l.CheckType(1, lua.TypeFunction)
			err = l.Load(&genericReader{l: l}, chunkName, m)
		}
		return loadHelper(l, err, e)
	}},
	{Name: "next", Function: lua.BaseNext}, // which compiled code recognises, as ipairs' iterator
	{Name: "pcall", Function: func(l *lua.State) int {
		l.CheckAny(1)
		l.PushNil()
		l.Insert(1) // create space for status result
		return finishProtectedCall(l, nil == l.ProtectedCallWithContinuation(l.Top()-2, lua.MultipleReturns, 0, 0, protectedCallContinuation))
	}},
	{Name: "print", Function: func(l *lua.State) int {
		n := l.Top()
		l.Global("tostring")
		for i := 1; i <= n; i++ {
			l.PushValue(-1) // function to be called
			l.PushValue(i)  // value to print
			l.Call(1, 1)
			s, ok := l.ToString(-1)
			if !ok {
				l.Errorf("'tostring' must return a string to 'print'")
				panic("unreachable")
			}
			if i > 1 {
				os.Stdout.WriteString("\t")
			}
			os.Stdout.WriteString(s)
			l.Pop(1) // pop result
		}
		os.Stdout.WriteString("\n")
		os.Stdout.Sync()
		return 0
	}},
	{Name: "rawequal", Function: func(l *lua.State) int {
		l.CheckAny(1)
		l.CheckAny(2)
		l.PushBoolean(l.RawEqual(1, 2))
		return 1
	}},
	{Name: "rawlen", Function: func(l *lua.State) int {
		t := l.TypeOf(1)
		l.ArgumentCheck(t == lua.TypeTable || t == lua.TypeString, 1, "table or string expected")
		l.PushInteger(l.RawLength(1))
		return 1
	}},
	{Name: "rawget", Function: func(l *lua.State) int {
		l.CheckType(1, lua.TypeTable)
		l.CheckAny(2)
		l.SetTop(2)
		l.RawGet(1)
		return 1
	}},
	{Name: "rawset", Function: func(l *lua.State) int {
		l.CheckType(1, lua.TypeTable)
		l.CheckAny(2)
		l.CheckAny(3)
		l.SetTop(3)
		l.RawSet(1)
		return 1
	}},
	{Name: "select", Function: func(l *lua.State) int {
		n := int64(l.Top())
		if l.TypeOf(1) == lua.TypeString {
			if s, _ := l.ToString(1); s[0] == '#' {
				l.PushInteger(n - 1)
				return 1
			}
		}
		i := l.CheckInteger(1)
		if i < 0 {
			i = n + i
		} else if i > n {
			i = n
		}
		l.ArgumentCheck(1 <= i, 1, "index out of range")
		return int(n - i)
	}},
	{Name: "setmetatable", Function: lua.BaseSetMetatable}, // which compiled code recognises
	{Name: "tonumber", Function: func(l *lua.State) int {
		if l.IsNoneOrNil(2) { // standard conversion
			if l.TypeOf(1) == lua.TypeNumber {
				l.SetTop(1)
				return 1
			}
			if s, ok := l.ToString(1); ok && l.StringToNumber(s) {
				return 1
			}
			l.CheckAny(1)
		} else {
			base := l.CheckInteger(2)
			l.CheckType(1, lua.TypeString)
			s, _ := l.ToString(1)
			l.ArgumentCheck(2 <= base && base <= 36, 2, "base out of range")
			if n, ok := stringToIntBase(s, base); ok {
				l.PushInteger(n)
				return 1
			}
		}
		l.PushNil()
		return 1
	}},
	{Name: "tostring", Function: func(l *lua.State) int {
		l.CheckAny(1)
		l.ToStringMeta(1)
		return 1
	}},
	{Name: "type", Function: func(l *lua.State) int {
		l.CheckAny(1)
		l.PushString(l.TypeName(1))
		return 1
	}},
	{Name: "xpcall", Function: func(l *lua.State) int {
		n := l.Top()
		l.ArgumentCheck(n >= 2, 2, "value expected")
		l.PushValue(1) // exchange function and error handler
		l.Copy(2, 1)
		l.Replace(2)
		return finishProtectedCall(l, nil == l.ProtectedCallWithContinuation(n-2, lua.MultipleReturns, 1, 0, protectedCallContinuation))
	}},
}

// OpenBase opens the basic library. Usually passed to Require.
func OpenBase(l *lua.State) int {
	l.PushGlobalTable()
	l.PushGlobalTable()
	l.SetField(-2, "_G")
	l.SetFunctions(baseLibrary, 0)
	// pairs returns next, and ipairs one iterator, as C Lua's do.
	l.Field(-1, "next")
	l.PushGoClosure(lua.BasePairs, 1)
	l.SetField(-2, "pairs")
	l.PushGoFunction(lua.BaseIPairsIterator)
	l.PushGoClosure(lua.BaseIPairs, 1)
	l.SetField(-2, "ipairs")
	l.PushString(lua.VersionString)
	l.SetField(-2, "_VERSION")
	return 1
}

// stringToIntBase converts s, an integer in base with optional space
// around it and a minus sign, as lbaselib.c's b_str2int does, wrapping
// around on overflow.
func stringToIntBase(s string, base int64) (int64, bool) {
	s = strings.Trim(s, " \f\n\r\t\v")
	negative := strings.HasPrefix(s, "-")
	if negative || strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d int64
		switch {
		case '0' <= c && c <= '9':
			d = int64(c - '0')
		case 'a' <= c && c <= 'z':
			d = int64(c-'a') + 10
		case 'A' <= c && c <= 'Z':
			d = int64(c-'A') + 10
		default:
			return 0, false
		}
		if d >= base {
			return 0, false
		}
		n = n*uint64(base) + uint64(d)
	}
	if negative {
		n = -n
	}
	return int64(n), true
}
