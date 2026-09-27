package lua

import (
	"math"
	"unsafe"
)

// table is a Lua table. Integer keys from 1 to len(array) live in array.
// String keys live in slots, laid out by shape (see shape.go), and all
// remaining keys live in hash, which is allocated on first write.
type table struct {
	array       []value
	shape       *shape  // nil until the first string key
	slots       []value // len(slots) == len(shape.keys)
	hash        map[hashValue]value
	metaTable   *table
	site        *fieldCache // the NEWTABLE that made the table, which learns its shape
	extra       *tableExtra // allocated on first use
	flags       byte
	finalizable bool // marked for finalization: see gc.go
}

// tableExtra holds table state that most tables never need.
type tableExtra struct {
	dead          int         // nil slots in a dictionary shape
	buried        int         // how many of them a collection buried: see shape.buried
	iterationKeys []hashValue // snapshot of hash keys for next
	iterationNext int         // index in iterationKeys after the key next last returned
}

// Tables with a few string keys are allocated together with their slots.
type (
	table1 struct {
		table
		inline [1]value
	}
	table2 struct {
		table
		inline [2]value
	}
	table4 struct {
		table
		inline [4]value
	}
	table8 struct {
		table
		inline [8]value
	}
)

const minCompactSlots = 8

func newTable() *table                     { return &table{} }
func (t *table) invalidateTagMethodCache() { t.flags = 0 }

func (t *table) ext() *tableExtra {
	if t.extra == nil {
		t.extra = &tableExtra{}
	}
	return t.extra
}

func (t *table) invalidateIteration() {
	if t.extra != nil {
		t.extra.iterationKeys = nil
	}
}

// newTableWithSlots returns an empty table with room for slotCount string
// keys, in one allocation when slotCount is small.
func newTableWithSlots(slotCount int) *table {
	switch {
	case slotCount <= 0:
		return new(table)
	case slotCount == 1:
		x := new(table1)
		x.slots = x.inline[:0]
		return &x.table
	case slotCount == 2:
		x := new(table2)
		x.slots = x.inline[:0]
		return &x.table
	case slotCount <= 4:
		x := new(table4)
		x.slots = x.inline[:0]
		return &x.table
	case slotCount <= 8:
		x := new(table8)
		x.slots = x.inline[:0]
		return &x.table
	}
	t := new(table)
	t.slots = make([]value, 0, slotCount)
	return t
}

func newTableWithSize(arraySize, hashSize int) *table {
	t := newTableWithSlots(hashSize)
	if arraySize > 0 {
		t.array = make([]value, arraySize)
	}
	return t
}

// newTableAt creates a table for a NEWTABLE instruction whose cache is site.
// It starts in the shape the site's previous table reached, with every slot
// nil. Nil slots read as absent keys, so this is invisible to Lua, but the
// constructor's stores then fill slots without changing shape.
func newTableAt(site *fieldCache, arraySize, hashSize int) *table {
	s := site.shape
	if s == nil {
		t := newTableWithSize(arraySize, hashSize)
		t.site = site
		return t
	}
	t := newTableWithSlots(len(s.keys))
	t.shape, t.slots, t.site = s, t.slots[:len(s.keys)], site
	if arraySize > 0 {
		t.array = make([]value, arraySize)
	}
	return t
}

func (t *table) atString(k string) value {
	if t.shape != nil {
		if i, ok := t.shape.slot(k); ok {
			return t.slots[i]
		}
	}
	return nilValue
}

// putString sets string key k, whose text is key, to v. l is the owning
// state, which counts a new key's slot.
func (t *table) putString(l *State, k value, key string, v value) {
	if t.shape != nil {
		if i, ok := t.shape.slot(key); ok {
			old := t.slots[i]
			t.slots[i] = v
			if t.shape.dict {
				if old.isNil() && !v.isNil() {
					t.ext().dead--
				} else if !old.isNil() && v.isNil() {
					t.ext().dead++
				}
			}
			return
		}
	}
	if v.isNil() {
		return
	}
	s := t.shape
	if s == nil {
		s = l.global.rootShape
	} else if s.dict && t.extra != nil && t.extra.dead > minCompactSlots && t.extra.dead > len(t.slots)/2 {
		l.charge((len(t.slots) - t.extra.dead) * 3 * slotBytes) // the new shape's keys and map, and the slots
		t.compact()
		s = t.shape
	}
	l.charge(2 * slotBytes) // the slot, and the key in a new shape
	t.shape = s.with(k, key)
	t.slots = append(t.slots, v)
	t.invalidateIteration() // adding an entry invalidates iterations
	if t.site != nil {
		if t.shape.dict {
			t.site.shape = nil // do not pre-shape tables as dictionaries
		} else {
			t.site.shape = t.shape
		}
	}
}

// compact drops the nil slots of a dictionary. The new shape invalidates
// slots that instructions cached for the old one.
func (t *table) compact() {
	live := len(t.slots) - t.extra.dead
	d := &shape{slots: make(map[string]int32, live), keys: make([]value, 0, live), dict: true, collects: t.shape.collects}
	slots := make([]value, 0, live)
	for i, v := range t.slots {
		if !v.isNil() { // a live slot, so not buried
			k := t.shape.keys[i]
			s, _ := k.str()
			d.slots[s] = int32(len(slots))
			d.keys = append(d.keys, k)
			slots = append(slots, v)
		}
	}
	t.shape, t.slots, t.extra.dead, t.extra.buried = d, slots, 0, 0
}

func (l *State) fastTagMethod(table *table, event tm) value {
	if table == nil || table.flags&(1<<event) != 0 {
		return nilValue
	}
	return table.tagMethod(event, l.global.tagMethodNames[event])
}

// intKey reports whether k is an integer, or a float with an integral
// value, that an int holds.
func intKey(k value) (int, bool) {
	if i, ok := k.integer(); ok && int64(int(i)) == i {
		return int(i), true
	}
	return 0, false
}

// extendArray grows the array part to last elements, which l counts.
func (t *table) extendArray(l *State, last int) {
	l.charge((last - len(t.array)) * slotBytes)
	t.array = append(t.array, make([]value, last-len(t.array))...)
	for k, v := range t.hash {
		if i, ok := intKey(k.value()); ok && 0 < i && i <= len(t.array) {
			t.array[i-1] = v
			delete(t.hash, k)
		}
	}
}

func (t *table) atInt(k int) value {
	if 0 < k && k <= len(t.array) {
		return t.array[k-1]
	}
	return t.hash[hashKey(integerValue(int64(k)))]
}

func (t *table) maybeResizeArray(l *State, key int) bool {
	// Precondition: key > len(t.array).
	occupancy := 0
	for _, v := range t.array {
		if !v.isNil() {
			occupancy++
		}
	}
	for k, v := range t.hash {
		if i, ok := intKey(k.value()); ok && i <= key && !v.isNil() {
			occupancy++
		}
	}
	if occupancy >= key>>1 {
		t.extendArray(l, max(occupancy*2, key)) // TODO Tune growth function.
		return true
	}
	return false
}

// addOrInsertHash stores a non-nil v at a key that is not a string and is
// outside the array part. l counts a new entry.
func (t *table) addOrInsertHash(l *State, k hashValue, v value) {
	if _, ok := t.hash[k]; !ok {
		l.charge(hashEntryBytes)
		if t.hash == nil {
			t.hash = make(map[hashValue]value)
		}
		t.invalidateIteration() // adding an entry invalidates iterations
	}
	t.hash[k] = v
}

func (t *table) putAtInt(l *State, k int, v value) {
	if 0 < k && k <= len(t.array) {
		t.array[k-1] = v
	} else if k > 0 && !v.isNil() && t.maybeResizeArray(l, k) {
		t.array[k-1] = v
	} else if v.isNil() {
		delete(t.hash, hashKey(integerValue(int64(k))))
	} else {
		t.addOrInsertHash(l, hashKey(integerValue(int64(k))), v)
	}
}

func (t *table) at(k value) value {
	switch k.kind() {
	case vkNil:
		return nilValue
	case vkString:
		s, _ := k.str()
		return t.atString(s)
	case vkNumber:
		if k.isFloat() {
			k = normaliseKey(k)
		}
		if k.isInteger() {
			if i := k.i(); 0 < i && i <= int64(len(t.array)) { // OPT: Inlined copy of atInt.
				return t.array[i-1]
			}
		}
	}
	return t.hash[hashKey(k)]
}

func (t *table) put(l *State, k, v value) {
	switch k.kind() {
	case vkNil:
		l.runtimeError("table index is nil")
		return
	case vkString:
		s, _ := k.str()
		t.putString(l, k, s, v)
		return
	case vkNumber:
		if k = normaliseKey(k); k.isInteger() {
			if i := k.i(); int64(int(i)) == i {
				t.putAtInt(l, int(i), v)
				return
			}
		} else if math.IsNaN(k.f()) {
			l.runtimeError("table index is NaN")
			return
		}
	}
	if v.isNil() {
		delete(t.hash, hashKey(k))
	} else {
		t.addOrInsertHash(l, hashKey(k), v)
	}
}

// tryPut stores v at k when k already holds a non-nil value, where Lua does
// a raw assignment without consulting __newindex. It reports whether it did.
func (t *table) tryPut(l *State, k, v value) bool {
	switch k.kind() {
	case vkNil:
		return false
	case vkString:
		if t.shape == nil {
			return false
		}
		s, _ := k.str()
		if i, ok := t.shape.slot(s); ok && !t.slots[i].isNil() {
			if v.isNil() && t.shape.dict {
				t.ext().dead++
			}
			t.slots[i] = v
			return true
		}
		return false
	case vkNumber:
		if k = normaliseKey(k); k.isInteger() {
			if i := k.i(); 0 < i && i <= int64(len(t.array)) && !t.array[i-1].isNil() {
				t.array[i-1] = v
				return true
			}
		} else if math.IsNaN(k.f()) {
			return false
		}
	}
	hk := hashKey(k)
	if old, ok := t.hash[hk]; ok && !old.isNil() {
		if v.isNil() {
			delete(t.hash, hk)
		} else {
			t.hash[hk] = v
		}
		return true
	}
	return false
}

func (t *table) unboundSearch(j int) int {
	i := j
	for j++; !t.atInt(j).isNil(); {
		i = j
		if j *= 2; j < 0 {
			for i = 1; !t.atInt(i).isNil(); i++ {
			}
			return i - 1
		}
	}
	for j-i > 1 {
		m := (i + j) / 2
		if t.atInt(m).isNil() {
			j = m
		} else {
			i = m
		}
	}
	return i
}

func (t *table) length() int {
	j := len(t.array)
	if j > 0 && t.array[j-1].isNil() {
		i := 0
		for j-i > 1 {
			m := (i + j) / 2
			if t.array[m-1].isNil() {
				j = m
			} else {
				i = m
			}
		}
		return i
	} else if t.hash == nil {
		return j
	}
	return t.unboundSearch(j)
}

func arrayIndex(k value) int {
	if i, ok := intKey(k); ok {
		return i
	}
	return -1
}

// next replaces the key at stack index key with the following key and value,
// and reports whether there was one. It visits the array part, then string
// keys in slot order, then other keys from a snapshot of hash. Fields
// cleared during a traversal are still found, as Lua allows.
func (l *State) next(t *table, key int) bool {
	k := l.stack[key]
	i, s := 0, 0 // array and slot positions to resume from
	if !k.isNil() {
		if i = arrayIndex(k); 0 < i && i <= len(t.array) {
		} else if str, ok := k.str(); ok {
			slot, ok := int32(0), false
			if t.shape != nil {
				if slot, ok = t.shape.slot(str); !ok { // cleared, and then buried by a collection?
					slot, ok = t.shape.buriedSlot(str)
				}
			}
			if !ok {
				l.runtimeError("invalid key to 'next'")
			}
			i, s = len(t.array), int(slot)+1
		} else {
			return l.nextHashKey(t, key, k)
		}
	}
	for ; i < len(t.array); i++ {
		if !t.array[i].isNil() {
			l.stack[key] = integerValue(int64(i + 1))
			l.stack[key+1] = t.array[i]
			return true
		}
	}
	for ; s < len(t.slots); s++ {
		if v := t.slots[s]; !v.isNil() {
			l.stack[key], l.stack[key+1] = t.shape.keys[s], v
			return true
		}
	}
	if len(t.hash) == 0 {
		return false
	}
	if t.extra == nil || t.extra.iterationKeys == nil {
		t.snapshotKeys(l)
	}
	return l.nextMapKey(t, key, 0)
}

func (l *State) nextHashKey(t *table, key int, kv value) bool {
	k := hashKey(kv)
	if t.extra == nil || t.extra.iterationKeys == nil {
		if _, ok := t.hash[k]; !ok {
			l.runtimeError("invalid key to 'next'")
		}
		t.snapshotKeys(l)
	}
	x := t.extra
	if j := x.iterationNext - 1; 0 <= j && j < len(x.iterationKeys) && x.iterationKeys[j] == k {
		return l.nextMapKey(t, key, j+1)
	}
	for j, hk := range x.iterationKeys {
		if hk == k {
			return l.nextMapKey(t, key, j+1)
		}
	}
	l.runtimeError("invalid key to 'next'")
	return false
}

// snapshotKeys lists the hash part's keys for next to walk, which l counts.
func (t *table) snapshotKeys(l *State) {
	l.charge(len(t.hash) * int(unsafe.Sizeof(hashValue{})))
	keys := make([]hashValue, 0, len(t.hash))
	for hk := range t.hash {
		keys = append(keys, hk)
	}
	x := t.ext()
	x.iterationKeys, x.iterationNext = keys, 0
}

// nextMapKey pushes the first key from iterationKeys[from:] still present.
func (l *State) nextMapKey(t *table, key, from int) bool {
	x := t.extra
	for j := from; j < len(x.iterationKeys); j++ {
		hk := x.iterationKeys[j]
		if v := t.hash[hk]; !v.isNil() {
			l.stack[key], l.stack[key+1] = hk.value(), v
			x.iterationNext = j + 1
			return true
		}
	}
	return false // no more elements
}
