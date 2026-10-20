// Package bind fills an operation's input from request parameters: path
// segments, query values and headers. Struct tags name the source:
//
//	type GetOrderIn struct {
//		ID     string   `path:"id" json:"-"`
//		Expand []string `query:"expand" json:"-"`
//		Tenant string   `header:"X-Tenant" json:"-"`
//	}
//
// Tags are read once, at boot, into a plan. Supported field types are strings,
// booleans, integers, floats, time.Duration, time.Time (RFC 3339), anything
// implementing encoding.TextUnmarshaler, pointers to those (left nil when the
// parameter is absent) and, for query values and headers, slices of them.
package bind

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/thec1oud/galleon/op"
)

// Source is where parameters come from.
type Source interface {
	PathValue(name string) string
	Query(name string) []string
	Header(name string) []string
}

// Func binds parameters into a pointer to the planned type. Values that don't
// parse are reported as violations with the code "type".
type Func func(into any, src Source) *op.ValidationError

// Plan builds the binder for struct type t. It returns nil, and no error, when
// t has no parameter fields.
func Plan(t reflect.Type) (Func, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("bind: %v is not a struct", t)
	}
	var fields []field
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		var f field
		n := 0
		for _, k := range []kind{fromPath, fromQuery, fromHeader} {
			if name := sf.Tag.Get(k.tag()); name != "" {
				f = field{index: i, from: k, name: name}
				n++
			}
		}
		if n == 0 {
			continue
		}
		if !sf.IsExported() {
			return nil, fmt.Errorf("bind: %v.%s is unexported", t, sf.Name)
		}
		if n > 1 {
			return nil, fmt.Errorf("bind: %v.%s has more than one of the path, query and header tags", t, sf.Name)
		}
		set, multi, err := setterFor(sf.Type)
		if err != nil {
			return nil, fmt.Errorf("bind: %v.%s: %w", t, sf.Name, err)
		}
		if multi && f.from == fromPath {
			return nil, fmt.Errorf("bind: %v.%s: a path segment holds one value, not a slice", t, sf.Name)
		}
		f.set = set
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return func(into any, src Source) *op.ValidationError {
		v := reflect.ValueOf(into).Elem()
		var ve *op.ValidationError
		for _, f := range fields {
			var vals []string
			switch f.from {
			case fromPath:
				if s := src.PathValue(f.name); s != "" {
					vals = []string{s}
				}
			case fromQuery:
				vals = src.Query(f.name)
			case fromHeader:
				vals = src.Header(f.name)
			}
			if len(vals) == 0 {
				continue
			}
			if err := f.set(v.Field(f.index), vals); err != nil {
				ve = ve.Add("/"+f.name, "type", map[string]any{"in": f.from.tag(), "error": err.Error()})
			}
		}
		return ve
	}, nil
}

type kind uint8

const (
	fromPath kind = iota + 1
	fromQuery
	fromHeader
)

func (k kind) tag() string {
	return [...]string{fromPath: "path", fromQuery: "query", fromHeader: "header"}[k]
}

type field struct {
	index int
	from  kind
	name  string
	set   func(v reflect.Value, vals []string) error
}

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	durationType    = reflect.TypeFor[time.Duration]()
	timeType        = reflect.TypeFor[time.Time]()
)

// setterFor returns a setter for fields of type t, and whether it takes many
// values.
func setterFor(t reflect.Type) (func(reflect.Value, []string) error, bool, error) {
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		one, err := scalarSetter(t.Elem())
		if err != nil {
			return nil, false, err
		}
		return func(v reflect.Value, vals []string) error {
			s := reflect.MakeSlice(t, len(vals), len(vals))
			for i, x := range vals {
				if err := one(s.Index(i), x); err != nil {
					return err
				}
			}
			v.Set(s)
			return nil
		}, true, nil
	}
	one, err := scalarSetter(t)
	if err != nil {
		return nil, false, err
	}
	return func(v reflect.Value, vals []string) error { return one(v, vals[len(vals)-1]) }, false, nil
}

func scalarSetter(t reflect.Type) (func(reflect.Value, string) error, error) {
	if t.Kind() == reflect.Pointer {
		inner, err := scalarSetter(t.Elem())
		if err != nil {
			return nil, err
		}
		return func(v reflect.Value, s string) error {
			p := reflect.New(t.Elem())
			if err := inner(p.Elem(), s); err != nil {
				return err
			}
			v.Set(p)
			return nil
		}, nil
	}
	switch {
	case t == timeType:
		return func(v reflect.Value, s string) error {
			tm, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return fmt.Errorf("want an RFC 3339 time")
			}
			v.Set(reflect.ValueOf(tm))
			return nil
		}, nil
	case t == durationType:
		return func(v reflect.Value, s string) error {
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("want a duration such as 1m30s")
			}
			v.SetInt(int64(d))
			return nil
		}, nil
	case reflect.PointerTo(t).Implements(textUnmarshaler):
		return func(v reflect.Value, s string) error {
			return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s))
		}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return func(v reflect.Value, s string) error { v.SetString(s); return nil }, nil
	case reflect.Bool:
		return func(v reflect.Value, s string) error {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("want true or false")
			}
			v.SetBool(b)
			return nil
		}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := t.Bits()
		return func(v reflect.Value, s string) error {
			n, err := strconv.ParseInt(s, 10, bits)
			if err != nil {
				return fmt.Errorf("want an integer that fits in %d bits", bits)
			}
			v.SetInt(n)
			return nil
		}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bits := t.Bits()
		return func(v reflect.Value, s string) error {
			n, err := strconv.ParseUint(s, 10, bits)
			if err != nil {
				return fmt.Errorf("want a non-negative integer that fits in %d bits", bits)
			}
			v.SetUint(n)
			return nil
		}, nil
	case reflect.Float32, reflect.Float64:
		bits := t.Bits()
		return func(v reflect.Value, s string) error {
			f, err := strconv.ParseFloat(s, bits)
			if err != nil {
				return fmt.Errorf("want a number")
			}
			v.SetFloat(f)
			return nil
		}, nil
	}
	return nil, fmt.Errorf("%v can't be bound from a parameter", t)
}
