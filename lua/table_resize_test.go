package lua_test

import (
	"testing"
	"time"

	"github.com/matjam/apogee/lua"
	"github.com/matjam/apogee/stdlib"
)

// Filling a table one integer key at a time takes time in proportion to
// its size, whatever the keys: sparse IDs, keys far past the array, every
// third key, or backwards. Each of these once rescanned the table on every
// key; 40,000 keys took six seconds or more. The values read back, and a
// table filled backwards is still a sequence.
func TestFillingTablesWithIntegerKeys(t *testing.T) {
	cases := map[string]string{
		"sparse IDs": `
			local t = {}
			for i = 1, 40000 do t[1000000000 + i * 7] = i end
			for i = 1, 40000 do assert(t[1000000000 + i * 7] == i) end`,
		"far past the array": `
			local t = {}
			for i = 1, 40000 do t[(1 << 40) + i] = i end
			for i = 1, 40000 do assert(t[(1 << 40) + i] == i) end`,
		"every third key": `
			local t = {}
			for i = 1, 120000, 3 do t[i] = i end
			for i = 1, 120000, 3 do assert(t[i] == i) end`,
		"backwards": `
			local t = {}
			for i = 40000, 1, -1 do t[i] = i end
			assert(#t == 40000)
			local n = 0
			for i, v in ipairs(t) do assert(v == i); n = n + 1 end
			assert(n == 40000)`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			l := lua.NewState()
			stdlib.Open(l)
			start := time.Now()
			if err := l.DoString(src); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("took %v", d)
			}
		})
	}
}
