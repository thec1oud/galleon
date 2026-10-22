package data

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ModelInfo describes a model an ORM, query builder or repository stores:
// enough for spans to name the table a statement serves, and for the admin
// battery to list models later. It holds no driver types.
//
// Names are as the catalog spells them, unquoted: "order_lines", not
// "\"order_lines\"".
type ModelInfo struct {
	// Name is the model's name, unique in the app, such as "order".
	Name string
	// Schema and Table name the table the model is stored in. Both are
	// required, since an unqualified table depends on the search path.
	Schema, Table string
	// Key is the primary key's columns, in order.
	Key []string
	// Columns are the table's columns the model maps, or nil when the
	// adapter doesn't say. When set, they include Key's and every
	// relation's columns.
	Columns []string
	// Relations are the model's references to other models, by foreign
	// key: optional.
	Relations []Relation
}

// Relation is a model's reference to another model: Columns of this model's
// table refer to References of the other's, pair by pair.
type Relation struct {
	// Name is the relation's name within its model, such as "customer".
	Name string
	// Model is the name of the model it refers to, which must be
	// registered too.
	Model               string
	Columns, References []string
}

// Identifier is the model's table, schema-qualified, as pgx quotes it.
func (m ModelInfo) Identifier() Identifier { return Identifier{m.Schema, m.Table} }

// Validate reports what is wrong with m on its own: a missing name, schema,
// table or key, an empty or repeated column, a relation without a model or
// with unpaired columns, or a column outside Columns when it is set.
func (m ModelInfo) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if m.Name == "" || strings.TrimSpace(m.Name) != m.Name {
		bad("the name %q is empty or has spaces around it", m.Name)
	}
	if m.Schema == "" || m.Table == "" {
		bad("the table must be schema-qualified: schema %q, table %q", m.Schema, m.Table)
	}
	if len(m.Key) == 0 {
		bad("it has no primary key columns")
	}
	if err := columns("Key", m.Key); err != nil {
		errs = append(errs, err)
	}
	if err := columns("Columns", m.Columns); err != nil {
		errs = append(errs, err)
	}
	mapped := func(field string, cols []string) {
		for _, c := range cols {
			if m.Columns != nil && !slices.Contains(m.Columns, c) {
				bad("%s names %q, which isn't in Columns", field, c)
			}
		}
	}
	mapped("Key", m.Key)
	var names []string
	for _, r := range m.Relations {
		field := fmt.Sprintf("relation %q", r.Name)
		switch {
		case r.Name == "":
			bad("a relation has no name")
		case slices.Contains(names, r.Name):
			bad("%s is declared twice", field)
		}
		names = append(names, r.Name)
		if r.Model == "" {
			bad("%s names no model", field)
		}
		if len(r.Columns) == 0 || len(r.Columns) != len(r.References) {
			bad("%s must pair each of its columns with a referenced column: %d columns, %d references", field, len(r.Columns), len(r.References))
		}
		if err := columns(field+" Columns", r.Columns); err != nil {
			errs = append(errs, err)
		}
		if err := columns(field+" References", r.References); err != nil {
			errs = append(errs, err)
		}
		mapped(field, r.Columns)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("model %q: %w", m.Name, err)
	}
	return nil
}

// columns reports an empty or repeated column of field.
func columns(field string, cols []string) error {
	for i, c := range cols {
		if c == "" {
			return fmt.Errorf("%s has an empty column", field)
		}
		if slices.Contains(cols[:i], c) {
			return fmt.Errorf("%s names %q twice", field, c)
		}
	}
	return nil
}

// clone returns m with slices of its own, so a copy handed out can't change
// the registry's.
func (m ModelInfo) clone() ModelInfo {
	m.Key = slices.Clone(m.Key)
	m.Columns = slices.Clone(m.Columns)
	m.Relations = slices.Clone(m.Relations)
	for i, r := range m.Relations {
		m.Relations[i].Columns = slices.Clone(r.Columns)
		m.Relations[i].References = slices.Clone(r.References)
	}
	return m
}

// Models is the app's model registry. It is built once, at boot, and never
// changes afterwards, so it is safe for concurrent use. A nil *Models has no
// models.
//
// Modules register models with galleon.Collect(r, models...), which checks
// each with Validate, and the data module (pg.Module) builds the registry
// from them, failing Start when two share a name or a table.
type Models struct {
	byName map[string]ModelInfo
	names  []string // sorted
}

// NewModels builds a registry of models, checking each with Validate, that
// no two share a name or a table, and that every relation refers to a
// model among them, by columns it maps when it lists them.
func NewModels(models ...ModelInfo) (*Models, error) {
	r := &Models{byName: map[string]ModelInfo{}}
	tables := map[[2]string]string{}
	named := map[string]bool{} // every model given, registered or refused
	var errs []error
	for _, m := range models {
		named[m.Name] = true
		if err := m.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, ok := r.byName[m.Name]; ok {
			errs = append(errs, fmt.Errorf("model %q is registered twice", m.Name))
			continue
		}
		table := [2]string{m.Schema, m.Table}
		if prev, ok := tables[table]; ok {
			errs = append(errs, fmt.Errorf("models %q and %q are both stored in %s", prev, m.Name, m.Identifier().Sanitize()))
			continue
		}
		tables[table] = m.Name
		r.byName[m.Name] = m.clone()
		r.names = append(r.names, m.Name)
	}
	for _, name := range r.names {
		for _, rel := range r.byName[name].Relations {
			to, ok := r.byName[rel.Model]
			if !ok && named[rel.Model] {
				// Refused, and reported, already.
				continue
			}
			if !ok {
				errs = append(errs, fmt.Errorf("model %q: relation %q refers to model %q, which isn't registered", name, rel.Name, rel.Model))
				continue
			}
			for _, c := range rel.References {
				if to.Columns != nil && !slices.Contains(to.Columns, c) {
					errs = append(errs, fmt.Errorf("model %q: relation %q refers to %q, which isn't among model %q's columns", name, rel.Name, c, to.Name))
				}
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.Sort(r.names)
	return r, nil
}

// Lookup returns the model named name.
func (r *Models) Lookup(name string) (ModelInfo, bool) {
	if r == nil {
		return ModelInfo{}, false
	}
	m, ok := r.byName[name]
	if !ok {
		return ModelInfo{}, false
	}
	return m.clone(), true
}

// All returns every model, by name.
func (r *Models) All() []ModelInfo {
	if r == nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(r.names))
	for _, name := range r.names {
		out = append(out, r.byName[name].clone())
	}
	return out
}

type modelKey struct{}

// ForModel returns a ctx whose statements serve the model named name. The
// data plane's spans of statements, batches and copies run with it carry
// the model's name, and its table when the model is registered; nothing
// else changes. An empty name marks no model.
func ForModel(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, modelKey{}, name)
}

// ModelFrom returns the model ctx's statements serve, if ForModel set one.
func ModelFrom(ctx context.Context) (string, bool) {
	name, _ := ctx.Value(modelKey{}).(string)
	return name, name != ""
}
