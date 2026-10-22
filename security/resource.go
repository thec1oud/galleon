package security

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// resourceID reads a resource's ID from a decoded input, a *In.
type resourceID func(in any) string

// planResourceID plans reading the ID named field from inputs of struct type
// t, once, at boot. field is the field's name on the wire: its json name, or
// its path, query or header parameter name. The field must be a string or an
// integer, and only one field may have the name.
func planResourceID(t reflect.Type, field string) (resourceID, error) {
	if t == nil || t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("the input type %v isn't a struct", t)
	}
	var found [][]int
	var names []string
	collectFields(t, nil, map[reflect.Type]bool{}, func(sf reflect.StructField, index []int) {
		for _, name := range wireNames(sf) {
			if name == field {
				found = append(found, index)
				names = append(names, sf.Name)
				return
			}
		}
	})
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("its resource ID field %q isn't a field of %v: name it as its json tag, or its path, query or header parameter, does", field, t)
	case 1:
	default:
		return nil, fmt.Errorf("its resource ID field %q names %d fields of %v (%s)", field, len(found), t, strings.Join(names, ", "))
	}
	index := found[0]
	ft := t.FieldByIndex(index).Type
	var get func(v reflect.Value) string
	switch ft.Kind() {
	case reflect.String:
		get = func(v reflect.Value) string { return v.String() }
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		get = func(v reflect.Value) string { return strconv.FormatInt(v.Int(), 10) }
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		get = func(v reflect.Value) string { return strconv.FormatUint(v.Uint(), 10) }
	default:
		return nil, fmt.Errorf("its resource ID field %q of %v is a %v; it must be a string or an integer", field, t, ft)
	}
	return func(in any) string { return get(reflect.ValueOf(in).Elem().FieldByIndex(index)) }, nil
}

// collectFields calls fn with every exported field of t, and of the structs
// t embeds by value, whose fields Go promotes, with its index from t.
func collectFields(t reflect.Type, index []int, seen map[reflect.Type]bool, fn func(reflect.StructField, []int)) {
	if seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		idx := append(slices.Clone(index), i)
		if sf.Anonymous && sf.Type.Kind() == reflect.Struct && sf.Tag.Get("json") == "" {
			collectFields(sf.Type, idx, seen, fn)
			continue
		}
		if sf.IsExported() {
			fn(sf, idx)
		}
	}
}

// wireNames returns the names a field goes by on the wire: its json name,
// unless it has none, and any path, query or header parameter name.
func wireNames(sf reflect.StructField) []string {
	var names []string
	switch tag, _, _ := strings.Cut(sf.Tag.Get("json"), ","); tag {
	case "-":
	case "":
		names = append(names, sf.Name)
	default:
		names = append(names, tag)
	}
	for _, k := range []string{"path", "query", "header"} {
		if n := sf.Tag.Get(k); n != "" {
			names = append(names, n)
		}
	}
	return names
}
