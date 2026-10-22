package security

import (
	"maps"
	"slices"
)

// Values is a read-only set of named values, such as a principal's claims
// or a context's attributes. The zero Values is empty and ready to use.
// Values never changes once made; don't modify the maps or slices a value
// in it holds.
type Values struct{ m map[string]any }

// Claims are verified claims about a principal, such as a token's.
type Claims = Values

// Attributes describe an operating context or a resource, for decisions:
// a device, a location, a plan, an owner.
type Attributes = Values

// NewValues returns Values holding a copy of m.
func NewValues(m map[string]any) Values {
	if len(m) == 0 {
		return Values{}
	}
	return Values{m: maps.Clone(m)}
}

// Get returns the value named name.
func (v Values) Get(name string) (any, bool) {
	x, ok := v.m[name]
	return x, ok
}

// String returns the value named name if it is a string.
func (v Values) String(name string) (string, bool) {
	s, ok := v.m[name].(string)
	return s, ok
}

// Strings returns the value named name as strings: a []string, a []any of
// strings, as JSON decodes an array, or a lone string. It reports false for
// anything else, or a list holding anything but strings.
func (v Values) Strings(name string) ([]string, bool) {
	switch x := v.m[name].(type) {
	case string:
		return []string{x}, true
	case []string:
		return slices.Clone(x), true
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// Len returns how many values there are.
func (v Values) Len() int { return len(v.m) }

// Names returns the names of the values, sorted.
func (v Values) Names() []string { return slices.Sorted(maps.Keys(v.m)) }

// With returns a copy of v with name set to x.
func (v Values) With(name string, x any) Values {
	m := make(map[string]any, len(v.m)+1)
	maps.Copy(m, v.m)
	m[name] = x
	return Values{m: m}
}
