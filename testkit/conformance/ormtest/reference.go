package ormtest

import (
	"context"
	"errors"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
)

// Reference is a reference adapter: a hand-written repository of widgets
// over data.Querier, which passes the suite. It shows the whole of what an
// adapter owes the data seam: every statement runs through the handle it
// is given (or, with FromContext, the one in ctx), each marked with its
// model; side effects go to AfterCommit; and its model is described, for
// the module to register with galleon.Collect(r, ref.Models()...).
type Reference struct {
	s Setup
	// FromContext makes it take its handle from ctx, with
	// data.QuerierFrom and data.ReaderFrom, as an ORM that doesn't take
	// one explicitly does, rather than use the one it is given.
	FromContext bool
}

// NewReference returns a Reference configured from s.
func NewReference(s Setup) *Reference { return &Reference{s: s} }

// ModelName is the name of the Reference's model.
const ModelName = "widget"

// errNoHandle is what a Reference taking its handle from ctx returns when
// ctx carries none: it never opens a connection of its own instead.
var errNoHandle = errs.New(errs.Internal, data.CodePropagation, "no transaction in ctx to run the statement in")

func (r *Reference) querier(ctx context.Context, q data.Querier) (data.Querier, error) {
	if !r.FromContext {
		return q, nil
	}
	if q, ok := data.QuerierFrom(ctx); ok {
		return q, nil
	}
	return nil, errNoHandle
}

func (r *Reference) reader(ctx context.Context, rd data.Reader) (data.Reader, error) {
	if !r.FromContext {
		return rd, nil
	}
	if rd, ok := data.ReaderFrom(ctx); ok {
		return rd, nil
	}
	return nil, errNoHandle
}

func (r *Reference) table() string { return data.Identifier{r.s.Schema, r.s.Table}.Sanitize() }

// saved registers the side effect of saving w with the transaction the
// save went through: q, or, taking its handle from ctx, the one there,
// through data.AfterCommit, as an ORM that never holds a handle would.
func (r *Reference) saved(ctx context.Context, q data.Querier, w Widget) error {
	fn := func(ctx context.Context) { r.s.Saved(ctx, w) }
	if tx, ok := q.(data.WriteTx); ok && !r.FromContext {
		tx.AfterCommit(fn)
		return nil
	}
	return data.AfterCommit(ctx, fn)
}

func (r *Reference) Insert(ctx context.Context, q data.Querier, w Widget) error {
	ctx = data.ForModel(ctx, ModelName)
	q, err := r.querier(ctx, q)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, "INSERT INTO "+r.table()+" (id, name, count) VALUES ($1, $2, $3)", w.ID, w.Name, w.Count); err != nil {
		return err
	}
	return r.saved(ctx, q, w)
}

func (r *Reference) Find(ctx context.Context, rd data.Reader, id int64) (Widget, error) {
	ctx = data.ForModel(ctx, ModelName)
	rd, err := r.reader(ctx, rd)
	if err != nil {
		return Widget{}, err
	}
	var w Widget
	err = rd.QueryRow(ctx, "SELECT id, name, count FROM "+r.table()+" WHERE id = $1", id).Scan(&w.ID, &w.Name, &w.Count)
	return w, err
}

func (r *Reference) Update(ctx context.Context, q data.Querier, w Widget) error {
	ctx = data.ForModel(ctx, ModelName)
	q, err := r.querier(ctx, q)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, "UPDATE "+r.table()+" SET name = $2, count = $3 WHERE id = $1", w.ID, w.Name, w.Count)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("ormtest: no widget to update")
	}
	return r.saved(ctx, q, w)
}

// Models describes the widget model.
func (r *Reference) Models() []data.ModelInfo {
	return []data.ModelInfo{{
		Name: ModelName, Schema: r.s.Schema, Table: r.s.Table,
		Key: []string{"id"}, Columns: []string{"id", "name", "count"},
	}}
}
