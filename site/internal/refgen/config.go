package refgen

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// A Section is one config section.
type Section struct {
	Name    string
	Module  string // the module that loads it
	Package string // the package that loads it, relative to the core module
	Fields  []Field
}

// A Field is one key of a config section.
type Field struct {
	Key    string
	Type   string
	Doc    string
	Secret bool
	Env    bool // settable from the environment

	index []int // the path of its field from the section's struct, or nil in a map or slice
}

// sections finds every config section the module loads with
// galleon.Config[T](app, name), sorted by name, with their keys in
// declaration order. It fails on a section whose name isn't a constant, or
// whose type isn't a struct.
func (s *source) sections() ([]Section, error) {
	var out []Section
	var problems []string
	seen := map[string]types.Type{}
	for _, p := range s.pkgs {
		info := p.TypesInfo
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				idx, ok := ast.Unparen(call.Fun).(*ast.IndexExpr)
				if !ok || !isObj(callee(call, info), Module, "Config") {
					return true
				}
				if len(call.Args) != 2 {
					problems = append(problems, fmt.Sprintf("%s: galleon.Config called with %d arguments", s.where(call.Pos()), len(call.Args)))
					return true
				}
				tv := info.Types[call.Args[1]]
				if tv.Value == nil || tv.Value.Kind() != constant.String {
					problems = append(problems, fmt.Sprintf("%s: a config section's name isn't a constant, so the reference can't list it", s.where(call.Args[1].Pos())))
					return true
				}
				name := constant.StringVal(tv.Value)
				t := info.TypeOf(idx.Index)
				if prev, ok := seen[name]; ok {
					if !types.Identical(prev, t) {
						problems = append(problems, fmt.Sprintf("%s: section %q is loaded as %s here and as %s elsewhere", s.where(call.Pos()), name, typeName(t), typeName(prev)))
					}
					return true
				}
				seen[name] = t
				st, ok := t.Underlying().(*types.Struct)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: section %q is a %s, not a struct", s.where(idx.Index.Pos()), name, t))
					return true
				}
				w := &walker{s: s, visited: map[types.Type]bool{t: true}}
				w.fields(st, "", true, []int{})
				for _, pr := range w.problems {
					problems = append(problems, fmt.Sprintf("section %q: %s", name, pr))
				}
				out = append(out, Section{Name: name, Module: p.Module.Path, Package: rel(p.PkgPath), Fields: w.out})
				return true
			})
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("refgen:\n  %s", strings.Join(problems, "\n  "))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type walker struct {
	s        *source
	visited  map[types.Type]bool // the types being walked, outermost first
	out      []Field
	problems []string
}

// fields lists the keys of st, prefixed by prefix. The fields of an
// embedded struct are listed as its own, as encoding/json reads them. The
// keys of a struct field, or of the struct element of a map or slice, are
// listed under their field's key. index is the path of st's fields from
// the section's struct, or nil inside a map or slice.
//
// Only the section's own fields of a basic type can be set from the
// environment, and only its own fields are redacted, so top is set for
// those alone; a secret tag deeper down is a problem, since nothing
// redacts it.
func (w *walker) fields(st *types.Struct, prefix string, top bool, index []int) {
	for i := 0; i < st.NumFields(); i++ {
		v := st.Field(i)
		tag := reflect.StructTag(st.Tag(i))
		key, _, _ := strings.Cut(tag.Get("json"), ",")
		if key == "-" {
			continue
		}
		var idx []int
		if index != nil {
			idx = append(slices.Clone(index), i)
		}
		if v.Embedded() && key == "" {
			if es, ok := deref(v.Type()).Underlying().(*types.Struct); ok {
				if w.enter(v.Type()) {
					w.fields(es, prefix, false, idx)
					w.leave(v.Type())
				} else {
					w.problems = append(w.problems, fmt.Sprintf("%s embeds itself", typeName(v.Type())))
				}
				continue
			}
		}
		if !v.Exported() {
			continue
		}
		if key == "" {
			key = v.Name()
		}
		key = prefix + key
		d := w.s.docs[v.Pos()]
		doc := d.text
		if d.note != "" {
			doc = strings.TrimSpace(doc + "\n" + d.note)
		}
		secret := tag.Get("secret") == "true"
		if secret && !top {
			w.problems = append(w.problems, fmt.Sprintf("%s is tagged secret, but only a section's own fields are redacted", key))
		}
		_, basic := v.Type().Underlying().(*types.Basic)
		w.out = append(w.out, Field{
			Key:    key,
			Type:   typeName(v.Type()),
			Doc:    doc,
			Secret: secret,
			Env:    top && basic,
			index:  idx,
		})
		t, sub, sidx := v.Type(), ".", idx
		switch u := t.Underlying().(type) {
		case *types.Map:
			t, sub, sidx = u.Elem(), ".<name>.", nil
		case *types.Slice:
			t, sub, sidx = u.Elem(), "[].", nil
		}
		if es, ok := deref(t).Underlying().(*types.Struct); ok && !isWellKnown(deref(t)) {
			if w.enter(t) {
				w.fields(es, key+sub, false, sidx)
				w.leave(t)
			} else {
				w.problems = append(w.problems, fmt.Sprintf("%s: %s refers to itself", key, typeName(t)))
			}
		}
	}
}

// leave unmarks t once its fields are listed.
func (w *walker) leave(t types.Type) { delete(w.visited, deref(t)) }

// enter reports whether t isn't already being walked, which would recurse
// forever, and marks it.
func (w *walker) enter(t types.Type) bool {
	t = deref(t)
	if w.visited[t] {
		return false
	}
	w.visited[t] = true
	return true
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// isWellKnown reports whether t is a struct of the standard library, such
// as time.Time, that JSON reads as a value, not as an object of its fields.
func isWellKnown(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && !strings.Contains(n.Obj().Pkg().Path(), ".")
}

// typeName writes t as the config file sees it: a named type of a basic
// type, such as pg's Selection or migrate's durationText, as that basic
// type, except time.Duration, whose JSON form the page explains; others
// qualified by their package's name, except the module's own.
func typeName(t types.Type) string {
	if n, ok := t.(*types.Named); ok && !isObj(n.Obj(), "time", "Duration") {
		if _, basic := n.Underlying().(*types.Basic); basic {
			t = n.Underlying()
		}
	}
	return types.TypeString(t, func(p *types.Package) string {
		if strings.HasPrefix(p.Path(), Module) {
			return ""
		}
		return p.Name()
	})
}
