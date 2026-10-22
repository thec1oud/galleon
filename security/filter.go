package security

import (
	"context"
	"fmt"
	"reflect"
)

// Filterer is a Decider that can also answer "which resources may this
// subject do this action on?" as a predicate over their attributes, so a
// list operation can filter in its query rather than fetch rows only to
// drop them. It is an optional capability, found with a type assertion.
//
// A filter must be sound: every resource it matches must be one Check
// allows. The securitytest conformance suite checks that.
type Filterer interface {
	Filter(ctx context.Context, q FilterQuery) (Filter, error)
}

// FilterQuery asks which resources of a type the subject may act on.
type FilterQuery struct {
	Subject      Principal
	Action       string
	ResourceType string
	Context      OperatingContext
}

// FilterOp is what a Filter node tests.
type FilterOp uint8

const (
	FilterNever  FilterOp = iota // matches nothing; the zero Filter
	FilterAlways                 // matches everything
	FilterAnd                    // all of Args match
	FilterOr                     // any of Args match
	FilterNot                    // Args[0] doesn't match
	FilterEq                     // the attribute Field equals Value
	FilterIn                     // the attribute Field equals one of Values
)

// Filter is a predicate over a resource's attributes. The zero Filter
// matches nothing, so a filter left unset denies.
type Filter struct {
	Op     FilterOp
	Field  string   // for FilterEq and FilterIn
	Value  any      // for FilterEq
	Values []any    // for FilterIn
	Args   []Filter // for FilterAnd, FilterOr and FilterNot
}

// Filter constructors.
func Always() Filter                       { return Filter{Op: FilterAlways} }
func Never() Filter                        { return Filter{Op: FilterNever} }
func And(fs ...Filter) Filter              { return Filter{Op: FilterAnd, Args: fs} }
func Or(fs ...Filter) Filter               { return Filter{Op: FilterOr, Args: fs} }
func Not(f Filter) Filter                  { return Filter{Op: FilterNot, Args: []Filter{f}} }
func Eq(field string, value any) Filter    { return Filter{Op: FilterEq, Field: field, Value: value} }
func In(field string, values ...any) Filter { return Filter{Op: FilterIn, Field: field, Values: values} }

// Match reports whether a resource with attrs matches f. A missing
// attribute equals nothing. A malformed filter, such as a FilterNot without
// exactly one argument, matches nothing.
func (f Filter) Match(attrs Attributes) bool {
	switch f.Op {
	case FilterAlways:
		return true
	case FilterAnd:
		for _, a := range f.Args {
			if !a.Match(attrs) {
				return false
			}
		}
		return len(f.Args) > 0
	case FilterOr:
		for _, a := range f.Args {
			if a.Match(attrs) {
				return true
			}
		}
	case FilterNot:
		return len(f.Args) == 1 && f.Args[0].valid() && !f.Args[0].Match(attrs)
	case FilterEq:
		v, ok := attrs.Get(f.Field)
		return ok && equal(v, f.Value)
	case FilterIn:
		v, ok := attrs.Get(f.Field)
		if !ok {
			return false
		}
		for _, x := range f.Values {
			if equal(v, x) {
				return true
			}
		}
	}
	return false
}

// valid reports whether f and every filter in it are well formed, so that
// negating it is sound.
func (f Filter) valid() bool {
	switch f.Op {
	case FilterNever, FilterAlways:
		return true
	case FilterAnd, FilterOr:
		for _, a := range f.Args {
			if !a.valid() {
				return false
			}
		}
		return f.Op == FilterOr || len(f.Args) > 0
	case FilterNot:
		return len(f.Args) == 1 && f.Args[0].valid()
	case FilterEq, FilterIn:
		return f.Field != ""
	}
	return false
}

func (f Filter) String() string {
	switch f.Op {
	case FilterNever:
		return "never"
	case FilterAlways:
		return "always"
	case FilterAnd, FilterOr:
		op := " and "
		if f.Op == FilterOr {
			op = " or "
		}
		s := "("
		for i, a := range f.Args {
			if i > 0 {
				s += op
			}
			s += a.String()
		}
		return s + ")"
	case FilterNot:
		if len(f.Args) == 1 {
			return "not " + f.Args[0].String()
		}
	case FilterEq:
		return fmt.Sprintf("%s = %v", f.Field, f.Value)
	case FilterIn:
		return fmt.Sprintf("%s in %v", f.Field, f.Values)
	}
	return fmt.Sprintf("filter(%d)", f.Op)
}

// equal compares two attribute values of the same type, without panicking
// on ones that aren't comparable: 1 and 1.0 differ, as an int and a float64.
func equal(a, b any) (eq bool) {
	defer func() {
		if recover() != nil { // a comparable type holding an incomparable value
			eq = false
		}
	}()
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}
