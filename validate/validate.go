// Package validate is Galleon's built-in shape validation for hand-written
// operations. It reads `validate` struct tags once, at boot, into a plan; a
// call then only walks the plan. Operations whose validators are generated
// don't use it.
//
// Rules, separated by commas:
//
//	required        not the zero value; for pointers, slices and maps, not nil or empty
//	min=N, max=N    numbers: the value; strings: the length in characters; slices and maps: the length
//	len=N           exact length, as for min and max
//	oneof=a b c     one of the space-separated values (strings and integers)
//	email, uuid, url
//
// A regular expression goes in its own tag, `pattern:"^[a-z]+$"`, since it may
// contain commas. Nested structs, pointers to structs, and slices and arrays of
// them are validated too. Rules on a nil pointer are skipped, except required.
//
// Violation paths are JSON Pointers built from json tag names, such as
// "/items/3/quantity".
package validate

import (
	"context"
	"fmt"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thec1oud/galleon/op"
)

// Func validates a value of the planned type, passed as a pointer to it.
type Func func(v any) *op.ValidationError

// Plan builds the validator for struct type t. It returns nil, and no error,
// when no field of t has any rule, so callers can skip validation.
func Plan(t reflect.Type) (Func, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("validate: %v is not a struct", t)
	}
	p := &planner{done: map[reflect.Type]*structPlan{}}
	sp, err := p.structPlan(t)
	if err != nil {
		return nil, err
	}
	if !sp.any(map[*structPlan]bool{}) {
		return nil, nil
	}
	return func(v any) *op.ValidationError {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Type() != t {
			panic(fmt.Sprintf("validate: planned for *%v, called with %T", t, v))
		}
		var ve *op.ValidationError
		sp.run(rv.Elem(), "", &ve)
		return ve
	}, nil
}

// Self is implemented by input types that check their own shape. It runs after
// the tag rules.
type Self interface {
	Validate(ctx context.Context) *op.ValidationError
}

// ForOperation returns the shape validator Galleon adds to an operation with
// input type t, combining tag rules and the Self method, or nil when there is
// neither.
func ForOperation(t reflect.Type) (func(context.Context, any) *op.ValidationError, error) {
	tags, err := Plan(t)
	if err != nil {
		return nil, err
	}
	self := reflect.PointerTo(t).Implements(reflect.TypeFor[Self]())
	switch {
	case tags == nil && !self:
		return nil, nil
	case !self:
		return func(_ context.Context, v any) *op.ValidationError { return tags(v) }, nil
	case tags == nil:
		return func(ctx context.Context, v any) *op.ValidationError { return v.(Self).Validate(ctx) }, nil
	}
	return func(ctx context.Context, v any) *op.ValidationError {
		return tags(v).Merge(v.(Self).Validate(ctx))
	}, nil
}

type planner struct {
	done map[reflect.Type]*structPlan
}

type structPlan struct {
	fields []fieldPlan
}

type fieldPlan struct {
	index  int
	inline bool   // an embedded struct whose fields are promoted
	name   string // JSON name, for paths
	checks []check
	elem   *elemPlan // nested validation of the field's value
}

// elemPlan validates nested values: a struct, a pointer to one, or a slice or
// array of either.
type elemPlan struct {
	strct *structPlan
	each  *elemPlan // for slices and arrays
}

type check struct {
	code   string
	params map[string]any
	fn     func(v reflect.Value) bool // reports whether v passes
	// onNil means the check applies to a nil pointer too, and so sees the
	// pointer itself rather than what it points to.
	onNil bool
}

// any reports whether the plan checks anything. seen stops recursive types
// from looping.
func (sp *structPlan) any(seen map[*structPlan]bool) bool {
	if seen[sp] {
		return false
	}
	seen[sp] = true
	for _, f := range sp.fields {
		if len(f.checks) > 0 || f.elem.any(seen) {
			return true
		}
	}
	return false
}

func (e *elemPlan) any(seen map[*structPlan]bool) bool {
	switch {
	case e == nil:
		return false
	case e.strct != nil:
		return e.strct.any(seen)
	}
	return e.each.any(seen)
}

func (sp *structPlan) run(v reflect.Value, path string, ve **op.ValidationError) {
	for i := range sp.fields {
		f := &sp.fields[i]
		fv := v.Field(f.index)
		fpath := path + "/" + escape(f.name)
		if f.inline {
			fpath = path
		}
		isNil := fv.Kind() == reflect.Pointer && fv.IsNil()
		for _, c := range f.checks {
			if isNil && !c.onNil {
				continue
			}
			target := fv
			if fv.Kind() == reflect.Pointer && !c.onNil {
				target = fv.Elem()
			}
			if !c.fn(target) {
				*ve = (*ve).Add(fpath, c.code, c.params)
			}
		}
		if f.elem != nil {
			f.elem.run(fv, fpath, ve)
		}
	}
}

func (e *elemPlan) run(v reflect.Value, path string, ve **op.ValidationError) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch {
	case e.strct != nil:
		e.strct.run(v, path, ve)
	case e.each != nil:
		for i := 0; i < v.Len(); i++ {
			e.each.run(v.Index(i), path+"/"+strconv.Itoa(i), ve)
		}
	}
}

func (p *planner) structPlan(t reflect.Type) (*structPlan, error) {
	if sp, ok := p.done[t]; ok {
		return sp, nil // recursive types share one plan
	}
	sp := &structPlan{}
	p.done[t] = sp
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if inlined(sf) {
			// encoding/json promotes an untagged embedded struct's fields,
			// so they share this struct's paths.
			elem, err := p.elemPlan(sf.Type)
			if err != nil {
				return nil, err
			}
			checks, err := parseChecks(sf)
			if err != nil {
				return nil, fmt.Errorf("validate: %v.%s: %w", t, sf.Name, err)
			}
			if len(checks) > 0 {
				return nil, fmt.Errorf("validate: %v.%s: rules on an embedded struct have no field to apply to", t, sf.Name)
			}
			sp.fields = append(sp.fields, fieldPlan{index: i, inline: true, elem: elem})
			continue
		}
		if !sf.IsExported() {
			continue
		}
		name, skip := jsonName(sf)
		if skip {
			name = paramName(sf)
		}
		if name == "" {
			continue
		}
		f := fieldPlan{index: i, name: name}
		var err error
		if f.checks, err = parseChecks(sf); err != nil {
			return nil, fmt.Errorf("validate: %v.%s: %w", t, sf.Name, err)
		}
		if f.elem, err = p.elemPlan(sf.Type); err != nil {
			return nil, err
		}
		sp.fields = append(sp.fields, f)
	}
	return sp, nil
}

func (p *planner) elemPlan(t reflect.Type) (*elemPlan, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if t.PkgPath() == "time" { // time.Time and friends have no fields to check
			return nil, nil
		}
		sp, err := p.structPlan(t)
		if err != nil {
			return nil, err
		}
		return &elemPlan{strct: sp}, nil
	case reflect.Slice, reflect.Array:
		each, err := p.elemPlan(t.Elem())
		if err != nil || each == nil {
			return nil, err
		}
		return &elemPlan{each: each}, nil
	}
	return nil, nil
}

// inlined reports whether encoding/json promotes the fields of sf, an
// embedded struct (or pointer to one) without a JSON name.
func inlined(sf reflect.StructField) bool {
	if !sf.Anonymous {
		return false
	}
	if n, _, _ := strings.Cut(sf.Tag.Get("json"), ","); n != "" {
		return false // tagged with a name, or skipped with "-"
	}
	t := sf.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// jsonName returns the field's JSON name, and whether encoding/json skips it.
func jsonName(sf reflect.StructField) (string, bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name, false
	}
	n, _, _ := strings.Cut(tag, ",")
	if n == "-" {
		return "", true
	}
	if n == "" {
		return sf.Name, false
	}
	return n, false
}

// paramName names a field that isn't in the JSON body but is bound from the
// path, query or headers.
func paramName(sf reflect.StructField) string {
	for _, k := range []string{"path", "query", "header"} {
		if n := sf.Tag.Get(k); n != "" {
			return n
		}
	}
	return ""
}

func escape(s string) string {
	if !strings.ContainsAny(s, "~/") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func parseChecks(sf reflect.StructField) ([]check, error) {
	var out []check
	t := sf.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if tag := sf.Tag.Get("validate"); tag != "" {
		for _, rule := range strings.Split(tag, ",") {
			name, arg, hasArg := strings.Cut(strings.TrimSpace(rule), "=")
			c, err := makeCheck(name, arg, hasArg, t)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	if pat, ok := sf.Tag.Lookup("pattern"); ok {
		if t.Kind() != reflect.String {
			return nil, fmt.Errorf("pattern applies to strings, not %v", t)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
		out = append(out, check{code: "pattern", params: map[string]any{"pattern": pat}, fn: func(v reflect.Value) bool {
			return re.MatchString(v.String())
		}})
	}
	return out, nil
}

func makeCheck(name, arg string, hasArg bool, t reflect.Type) (check, error) {
	needArg := func() error {
		if !hasArg || arg == "" {
			return fmt.Errorf("%s needs a value, as in %s=3", name, name)
		}
		return nil
	}
	isString := t.Kind() == reflect.String
	switch name {
	case "required":
		return check{code: "required", onNil: true, fn: func(v reflect.Value) bool {
			if v.Kind() == reflect.Pointer {
				return !v.IsNil()
			}
			switch v.Kind() {
			case reflect.Slice, reflect.Map:
				return v.Len() > 0
			}
			return !v.IsZero()
		}}, nil
	case "min", "max", "len":
		if err := needArg(); err != nil {
			return check{}, err
		}
		return boundCheck(name, arg, t)
	case "oneof":
		if err := needArg(); err != nil {
			return check{}, err
		}
		allowed := strings.Fields(arg)
		set := map[string]bool{}
		for _, a := range allowed {
			set[a] = true
		}
		switch {
		case isString:
			return check{code: "oneof", params: map[string]any{"values": allowed}, fn: func(v reflect.Value) bool { return set[v.String()] }}, nil
		case isInt(t):
			return check{code: "oneof", params: map[string]any{"values": allowed}, fn: func(v reflect.Value) bool { return set[strconv.FormatInt(v.Int(), 10)] }}, nil
		case isUint(t):
			return check{code: "oneof", params: map[string]any{"values": allowed}, fn: func(v reflect.Value) bool { return set[strconv.FormatUint(v.Uint(), 10)] }}, nil
		}
		return check{}, fmt.Errorf("oneof applies to strings and integers, not %v", t)
	case "email", "uuid", "url":
		if !isString {
			return check{}, fmt.Errorf("%s applies to strings, not %v", name, t)
		}
		fn := map[string]func(string) bool{"email": isEmail, "uuid": isUUID, "url": isURL}[name]
		return check{code: name, fn: func(v reflect.Value) bool { return v.String() == "" || fn(v.String()) }}, nil
	}
	return check{}, fmt.Errorf("unknown rule %q", name)
}

func boundCheck(name, arg string, t reflect.Type) (check, error) {
	cmp := func(n, bound float64) bool {
		switch name {
		case "min":
			return n >= bound
		case "max":
			return n <= bound
		}
		return n == bound
	}
	switch {
	case t.Kind() == reflect.String, t.Kind() == reflect.Slice, t.Kind() == reflect.Map, t.Kind() == reflect.Array:
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return check{}, fmt.Errorf("%s=%s: want a non-negative integer length", name, arg)
		}
		code := name + "_length"
		return check{code: code, params: map[string]any{name: n}, fn: func(v reflect.Value) bool {
			l := v.Len()
			if v.Kind() == reflect.String {
				l = utf8.RuneCountInString(v.String())
			}
			return cmp(float64(l), float64(n))
		}}, nil
	case isInt(t), isUint(t), t.Kind() == reflect.Float32, t.Kind() == reflect.Float64:
		if name == "len" {
			return check{}, fmt.Errorf("len applies to strings, slices and maps, not %v", t)
		}
		b, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return check{}, fmt.Errorf("%s=%s: want a number", name, arg)
		}
		var param any = b
		if b == float64(int64(b)) {
			param = int64(b)
		}
		return check{code: name, params: map[string]any{name: param}, fn: func(v reflect.Value) bool {
			switch {
			case v.CanInt():
				return cmp(float64(v.Int()), b)
			case v.CanUint():
				return cmp(float64(v.Uint()), b)
			}
			return cmp(v.Float(), b)
		}}, nil
	}
	return check{}, fmt.Errorf("%s applies to numbers, strings, slices and maps, not %v", name, t)
}

func isInt(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUint(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == ""
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidPattern.MatchString(s) }

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}
