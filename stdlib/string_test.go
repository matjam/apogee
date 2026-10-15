package stdlib_test

import (
	"strings"
	"testing"
	"time"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// run runs script as the chunk "=test" in a state with the standard
// libraries.
func run(t *testing.T, script string) {
	t.Helper()
	l := lua.NewState()
	stdlib.Open(l)
	err := l.Load(strings.NewReader(script), "=test", "t")
	if err == nil {
		err = l.ProtectedCall(0, 0, 0)
	}
	if err != nil {
		msg, _ := l.ToString(-1)
		t.Fatal(err, msg)
	}
}

func TestStringFind(t *testing.T) {
	run(t, `
		local function eq(want, ...)
			local got = table.concat({...}, ",")
			assert(got == want, "got " .. got .. ", want " .. want)
		end
		eq("2,2", string.find("a.b", ".", 1, true))
		eq("1,1", string.find("a.b", "."))
		eq("3,3", string.find("a.b", "b"))
		eq("2,3,bc", string.find("abcd", "(b%a)"))
		eq("", string.find("abc", "^b") or "")
		eq("2,3", string.find("abc", "b.", -2))
		eq("", string.find("abc", "x", 10) or "")
	`)
}

func TestStringMatch(t *testing.T) {
	run(t, `
		assert(string.match("key = value", "(%w+)%s*=%s*(%w+)") == "key")
		local k, v = string.match("key = value", "(%w+)%s*=%s*(%w+)")
		assert(k == "key" and v == "value")
		assert(string.match("hello", "()ll()") == 3)
		assert(select(2, string.match("hello", "()ll()")) == 5)
		assert(string.match("  trim  ", "^%s*(.-)%s*$") == "trim")
		assert(string.match("f(a(b)c)d", "%b()") == "(a(b)c)")
		assert(string.match("THE (quick) fox", "%f[%a]%a+%f[%A]") == "THE")
		assert(string.match("abab", "(ab)%1") == "ab")
		assert(string.match("x = 10", "%d+$") == "10")
		assert(string.match("a$b", "a$b") == "a$b")
		assert(string.match("[]", "[]]") == "]")
		assert(string.match("a-b", "[%-]") == "-")
		assert(string.match("zeta", "[a-f]") == "e")
		assert(string.match("abc", "[^ab]") == "c")
		assert(string.match("abc", "a*?") == nil) -- a literal '?' after a*
		assert(string.match("aaa", "a-") == "")
		assert(string.match("aaab", "a-b") == "aaab")
		assert(string.match("", ".?") == "")
		assert(string.match("abc", "^(a)(b)(c)$") == "a")
	`)
}

func TestStringGmatch(t *testing.T) {
	run(t, `
		local words = {}
		for w in string.gmatch("one two  three", "%a+") do words[#words+1] = w end
		assert(table.concat(words, ",") == "one,two,three")
		local t = {}
		for k, v in string.gmatch("a=1, b=2", "(%w+)=(%w+)") do t[k] = v end
		assert(t.a == "1" and t.b == "2")
		local n = 0
		for _ in string.gmatch("abc", "") do n = n + 1 end
		assert(n == 4, n)

		-- The iterator's upvalues are the subject, pattern, position and
		-- the end of the last match.
		local it = string.gmatch("x y", "%a")
		assert(it() == "x")
		local name, s = debug.getupvalue(it, 1)
		assert(name == "" and s == "x y" and select(2, debug.getupvalue(it, 2)) == "%a")
		assert(select(2, debug.getupvalue(it, 3)) == 1 and select(2, debug.getupvalue(it, 4)) == 1)
		assert(debug.getupvalue(it, 5) == nil)
	`)
}

func TestStringGsub(t *testing.T) {
	run(t, `
		local function eq(want, got, gotn, wantn)
			assert(got == want, "got " .. tostring(got) .. ", want " .. want)
			if wantn then assert(gotn == wantn, "count " .. gotn) end
		end
		eq("hello world", string.gsub("hello world", "o", "o"))
		eq("hellX wXrld", string.gsub("hello world", "o", "X"))
		eq("hellX world", string.gsub("hello world", "o", "X", 1))
		eq("world hello", string.gsub("hello world", "(%w+) (%w+)", "%2 %1"))
		eq("<hello> <world>", string.gsub("hello world", "%w+", "<%0>"))
		eq("x = 1", string.gsub("$name = $value", "%$(%w+)", {name = "x", value = 1}))
		eq("HELLO world", string.gsub("hello world", "^%w+", string.upper))
		eq("hello world", string.gsub("hello world", "%w+", function() return nil end))
		eq("-a-b-c-", string.gsub("abc", "", "-"))
		eq("100%", string.gsub("100", "$", "%%"))
		local s, n = string.gsub("abc", "%w", "%0%0")
		eq("aabbcc", s, n, 3)
	`)
}

func TestStringPatternErrors(t *testing.T) {
	run(t, `
		local function fails(msg, f, ...)
			local ok, err = pcall(f, ...)
			assert(not ok and err:find(msg, 1, true), "want " .. msg .. ", got " .. tostring(err))
		end
		fails("malformed pattern (ends with '%')", string.find, "a", "a%")
		fails("malformed pattern (missing ']')", string.find, "a", "[a")
		fails("malformed pattern (missing arguments to '%b')", string.find, "a", "%b")
		fails("missing '[' after '%f' in pattern", string.find, "a", "%fa")
		fails("invalid pattern capture", string.match, "a", "a)")
		fails("invalid capture index %1", string.find, "a", "%1")
		fails("unfinished capture", string.match, "a", "(a")
		fails("invalid use of '%' in replacement string", string.gsub, "a", "a", "%x")
		fails("invalid capture index", string.gsub, "a", "a", "%2")
		fails("invalid replacement value (a table)", string.gsub, "a", "a", function() return {} end)
		fails("string/function/table expected", string.gsub, "a", "a", true)
		fails("too many captures", string.find, "a", string.rep("()", 33))
		fails("pattern too complex", string.find, string.rep("a", 300), string.rep("a?", 300) .. string.rep("a", 300))
	`)
}

// string.format's %a and %A write hexadecimal floats as glibc's printf
// does, and an unknown option is an error naming it.
func TestStringFormatHexFloat(t *testing.T) {
	run(t, `
		local function eq(want, ...)
			local got = string.format(...)
			assert(got == want, "got " .. got .. ", want " .. want)
		end
		eq("0x1p+0", "%a", 1)
		eq("0x1p-1", "%a", 0.5)
		eq("0X1.FFP+7", "%A", 255.5)
		eq("0x1.55p-2", "%.2a", 1/3)
		eq("-0x0p+0", "%a", -0.0)
		eq("0x1.921fb54442d18p+1", "%a", math.pi)
		eq("    0x1p+0", "%10a", 1)
		eq("0x1p+0    |", "%-10a|", 1)
		eq("0x0001p+0", "%09a", 1)
		eq("+0x1p+0", "%+a", 1)
		eq("inf", "%a", math.huge)
		local ok, err = pcall(string.format, "%y", 1)
		assert(not ok and err:find("invalid conversion '%y' to 'format'", 1, true), err)
	`)
}

func TestStringDump(t *testing.T) {
	run(t, `
		local f = load(string.dump(function(a, b) return a * b + 1 end))
		assert(f(6, 7) == 43)
		local ok, err = pcall(string.dump, print)
		assert(not ok and err:find("unable to dump given function", 1, true), err)
		assert(not pcall(string.dump, 1))
	`)
}

// string.reverse, lower and upper work on bytes, as Lua's do in the C
// locale: only ASCII letters change case, and bytes that are not UTF-8
// survive.
func TestStringBytes(t *testing.T) {
	run(t, `
		assert(string.reverse("ab\255\0c") == "c\0\255ba")
		assert(string.upper("é\255a") == "é\255A")
		assert(string.lower("É\255A") == "É\255a")
	`)
}

// Backtracking in a pattern match runs the count hook, so a hook can stop
// a match that would otherwise run for an exponentially long time.
func TestPatternMatchingRunsTheCountHook(t *testing.T) {
	for _, call := range []string{
		`string.find(s, ".-.-.-.-.-b")`,
		`string.match(s, ".-.-.-.-.-b")`,
		`for _ in string.gmatch(s, ".-.-.-.-.-b") do end`,
		`string.gsub(s, ".-.-.-.-.-b", "")`,
	} {
		t.Run(call, func(t *testing.T) {
			l := lua.NewState()
			stdlib.Open(l)
			calls := 0
			l.SetHook(func(l *lua.State, _ lua.Debug) {
				if calls++; calls == 100 {
					l.Errorf("stopped by the hook")
				}
			}, lua.MaskCount, 1000)
			done := make(chan error, 1)
			go func() { done <- l.DoString(`local s = string.rep("a", 3000); ` + call) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "stopped by the hook") {
					t.Fatalf("got %v, want the hook's error", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hook did not stop the match")
			}
		})
	}
}
