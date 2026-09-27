package lua

import "hash/maphash"

// A shape maps a table's string keys to slots in the table's slots slice.
//
// Tables that gain the same string keys in the same order share one shape,
// so instructions can cache a (shape, slot) pair and read a field with one
// slice access. Shared shapes never change: adding a key moves the table to
// the child shape for that key. Removing a key leaves its slot nil.
//
// A table with many keys, or whose shape has too many children, gets a
// private dictionary shape instead, which it changes in place. Renumbering
// a dictionary's slots replaces its shape, which invalidates cached slots.
type shape struct {
	slots map[string]int32 // key to slot
	keys  []value          // slot to key, as a string value
	next  map[string]*shape
	dict  bool
	// collects is set once the shape has held __gc or __mode, which a
	// metatable needs the collector to hear of: compiled setmetatable
	// sets a metatable whose shape has held neither.
	collects bool
}

// collectsKey reports whether key k is __gc or __mode.
func collectsKey(k string) bool { return k == "__gc" || k == "__mode" }

const (
	maxSharedShapeKeys = 32 // keys beyond this make a table a dictionary
	maxShapeChildren   = 64 // children beyond this make new tables dictionaries
	maxSharedKeyLen    = 40 // longer keys make a table a dictionary, as shared shapes outlive tables
)

func newRootShape() *shape { return &shape{} }

func (s *shape) slot(k string) (int32, bool) {
	i, ok := s.slots[k]
	return i, ok
}

// with returns the shape for s plus key k, whose value will live in slot
// len(s.keys). Dictionary shapes change in place.
func (s *shape) with(k value, key string) *shape {
	if s.dict {
		s.slots[key] = int32(len(s.keys))
		s.keys = append(s.keys, k)
		s.collects = s.collects || collectsKey(key)
		return s
	}
	if c, ok := s.next[key]; ok {
		return c
	}
	if len(s.keys) >= maxSharedShapeKeys || len(s.next) >= maxShapeChildren || len(key) > maxSharedKeyLen {
		d := s.dictionary()
		return d.with(k, key)
	}
	c := &shape{slots: make(map[string]int32, len(s.keys)+1), keys: make([]value, len(s.keys), len(s.keys)+1),
		collects: s.collects || collectsKey(key)}
	for kk, i := range s.slots {
		c.slots[kk] = i
	}
	copy(c.keys, s.keys)
	c.slots[key] = int32(len(s.keys))
	c.keys = append(c.keys, k)
	if s.next == nil {
		s.next = make(map[string]*shape)
	}
	s.next[key] = c
	return c
}

// buried returns a copy of dictionary s without the keys of t's nil slots,
// so that Go can free them, as C Lua frees dead keys. A buried key's slot
// keeps a tombstone, the key's hash, from which next resumes. The new
// shape invalidates slots that instructions cached for the old one.
func (s *shape) buried(slots []value) *shape {
	d := &shape{slots: make(map[string]int32, len(s.slots)), keys: make([]value, len(s.keys)), dict: true, collects: s.collects}
	for i, k := range s.keys {
		if str, ok := k.str(); ok && slots[i].isNil() {
			k = tombstone(str)
		} else if ok {
			d.slots[str] = int32(i)
		}
		d.keys[i] = k
	}
	return d
}

// tombstoneSeed hashes buried keys.
var tombstoneSeed = maphash.MakeSeed()

// tombstone is the key of a buried slot whose key was k: an integer, which
// no shape key otherwise is.
func tombstone(k string) value { return integerValue(int64(maphash.String(tombstoneSeed, k))) }

// buriedSlot returns the slot of dictionary s where key k was buried.
func (s *shape) buriedSlot(k string) (int32, bool) {
	if !s.dict {
		return 0, false
	}
	t := tombstone(k)
	for i, x := range s.keys {
		if x.isInteger() && x.i() == t.i() {
			return int32(i), true
		}
	}
	return 0, false
}

// dictionary returns a private, mutable copy of s.
func (s *shape) dictionary() *shape {
	d := &shape{slots: make(map[string]int32, len(s.keys)+1), keys: make([]value, len(s.keys)), dict: true, collects: s.collects}
	for k, i := range s.slots {
		d.slots[k] = i
	}
	copy(d.keys, s.keys)
	return d
}
