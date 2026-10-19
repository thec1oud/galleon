package galleon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaulter is implemented by config structs that set their own defaults.
// Defaults are applied before file values and environment overrides.
type Defaulter interface{ Default() }

// Validator is implemented by config structs that check themselves after loading.
type Validator interface{ Validate() error }

// ConfigValue is one effective top-level config value and where it came from.
type ConfigValue struct {
	Section string
	Field   string
	Source  string // "default", "file", "env"
	Value   string // "[REDACTED]" for fields tagged `secret:"true"`
}

// Config loads the section named section into a new *T and also provides *T to
// dependency injection, so constructors can take it as a parameter.
//
// Precedence: Default() < config file < environment. Environment variables are
// named <APP>_<SECTION>_<FIELD> in upper snake case, for top-level scalar fields
// (strings, booleans, integers, floats and durations).
func Config[T any](r *Registry, section string) *T {
	a := r.app
	cfg := new(T)
	if reflect.TypeFor[T]().Kind() != reflect.Struct {
		r.errorf("Config[%s]: config must be a struct", reflect.TypeFor[T]())
		return cfg
	}
	if prev, ok := a.configOwners[section]; ok {
		r.errorf("config section %q is already claimed by module %q", section, prev)
		return cfg
	}
	a.configOwners[section] = r.module

	sources := map[string]string{}
	if d, ok := any(cfg).(Defaulter); ok {
		d.Default()
	}
	fields := topLevelFields(reflect.TypeFor[T]())
	for _, f := range fields {
		sources[f.name] = "default"
	}

	if raw, ok := a.configRaw[section]; ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			r.errorf("config section %q: %v", section, err)
			return cfg
		}
		var present map[string]json.RawMessage
		_ = json.Unmarshal(raw, &present)
		for _, f := range fields {
			if _, ok := present[f.name]; ok {
				sources[f.name] = "file"
			}
		}
	}

	rv := reflect.ValueOf(cfg).Elem()
	for _, f := range fields {
		key := envKey(a.name, section, f.name)
		s, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if err := setScalar(rv.Field(f.index), s); err != nil {
			r.errorf("config %s: %v", key, err)
			continue
		}
		sources[f.name] = "env"
	}

	if v, ok := any(cfg).(Validator); ok {
		if err := v.Validate(); err != nil {
			r.errorf("config section %q is invalid: %v", section, err)
		}
	}

	for _, f := range fields {
		val := fmt.Sprint(rv.Field(f.index).Interface())
		if f.secret {
			val = "[REDACTED]"
		}
		a.configReport = append(a.configReport, ConfigValue{Section: section, Field: f.name, Source: sources[f.name], Value: val})
	}
	Supply[*T](r, cfg)
	return cfg
}

// ConfigReport lists every effective top-level config value and its source,
// with secrets redacted.
func (a *App) ConfigReport() []ConfigValue {
	out := append([]ConfigValue(nil), a.configReport...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		return out[i].Field < out[j].Field
	})
	return out
}

type configField struct {
	name   string
	index  int
	secret bool
}

func topLevelFields(t reflect.Type) []configField {
	var out []configField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := sf.Name
		if tag, ok := sf.Tag.Lookup("json"); ok {
			n, _, _ := strings.Cut(tag, ",")
			if n == "-" {
				continue
			}
			if n != "" {
				name = n
			}
		}
		out = append(out, configField{name: name, index: i, secret: sf.Tag.Get("secret") == "true"})
	}
	return out
}

func envKey(app, section, field string) string {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToUpper(s) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return norm(app) + "_" + norm(section) + "_" + norm(field)
}

var durationType = reflect.TypeFor[time.Duration]()

func setScalar(v reflect.Value, s string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("only scalar fields can be set from the environment, not %s", v.Type())
	}
	return nil
}
