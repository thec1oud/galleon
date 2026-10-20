package pg

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
)

// kind is how a handle relates to the transaction it runs in.
type kind uint8

const (
	rootKind   kind = iota // owns the transaction: its Commit commits
	joinedKind             // shares its parent's transaction and savepoint
	nestedKind             // a savepoint in its parent's transaction
)

type state uint8

const (
	open state = iota
	committed
	rolledBack
)

// root is what every handle of one database transaction shares.
type root struct {
	m     *Manager
	src   *source
	write bool
	iso   data.IsolationLevel
	span  trace.Span
	// outer is the span that was active when the transaction began. A
	// statement run with a ctx still at that span is made a child of the
	// transaction's span instead.
	outer   trace.SpanContext
	began   []uintptr // where it began, when an operation tracks it
	scope   *scope
	release func() // gives a Within manager's turn back
}

// handle is one handle on a transaction. Handles are not safe for
// concurrent use, as data.ReadTx says.
type handle struct {
	root         *root
	parent       *handle
	kind         kind
	write        bool
	tx           pgx.Tx // the root's transaction, or the savepoint's
	state        state
	rollbackOnly bool // a joined handle rolled back: Commit must fail
	children     []*handle
	// Callbacks of this handle and of the nested handles it released.
	// Joined handles keep none: theirs go to the handle they joined.
	afterCommit, afterRollback []func(context.Context)
}

// readTx and writeTx are the two faces of a handle. A writeTx is a readTx
// with the write methods; a readTx has none, and can't be asserted to a
// data.WriteTx.
type (
	readTx  struct{ h *handle }
	writeTx struct{ readTx }
)

var (
	_ data.ReadTx  = (*readTx)(nil)
	_ data.WriteTx = (*writeTx)(nil)
)

func misuse(code, msg string) error { return errs.New(errs.Internal, code, msg) }

var errEnded = misuse(data.CodeTxEnded, "the transaction handle was used after it ended")

// BeginRead begins a read transaction, or joins or nests in the one in ctx.
func (m *Manager) BeginRead(ctx context.Context, opts ...data.ReadOption) (data.ReadTx, error) {
	h, err := m.begin(ctx, false, data.ReadOptions(opts...), false)
	if err != nil {
		return nil, err
	}
	return &readTx{h}, nil
}

// BeginWrite begins a write transaction, or joins or nests in the one in ctx.
func (m *Manager) BeginWrite(ctx context.Context, opts ...data.WriteOption) (data.WriteTx, error) {
	h, err := m.begin(ctx, true, data.WriteOptions(opts...), false)
	if err != nil {
		return nil, err
	}
	return &writeTx{readTx{h}}, nil
}

// Read runs fn in a read transaction; see data.Manager.
func (m *Manager) Read(ctx context.Context, fn func(context.Context, data.ReadTx) error, opts ...data.ReadOption) error {
	return m.closure(ctx, false, data.ReadOptions(opts...), func(h *handle) data.ReadTx { return &readTx{h} },
		func(ctx context.Context, tx data.ReadTx) error { return fn(ctx, tx) })
}

// Write runs fn in a write transaction; see data.Manager.
func (m *Manager) Write(ctx context.Context, fn func(context.Context, data.WriteTx) error, opts ...data.WriteOption) error {
	return m.closure(ctx, true, data.WriteOptions(opts...), func(h *handle) data.ReadTx { return &writeTx{readTx{h}} },
		func(ctx context.Context, tx data.ReadTx) error { return fn(ctx, tx.(*writeTx)) })
}

// closure runs fn in a transaction it begins and ends, and reruns all of it
// on a serialization failure or deadlock when it owns the root transaction.
func (m *Manager) closure(ctx context.Context, write bool, o data.Options, face func(*handle) data.ReadTx, fn func(context.Context, data.ReadTx) error) error {
	once := func(ctx context.Context) error {
		h, err := m.begin(ctx, write, o, true)
		if err != nil {
			return err
		}
		tx := face(h)
		fctx := ctx
		if h.kind == rootKind {
			fctx = trace.ContextWithSpan(fctx, h.root.span)
		}
		if err := guarded(data.With(fctx, tx), h, tx, fn); err != nil {
			_ = h.rollback(ctx)
			return err
		}
		return h.commit(ctx)
	}
	owns := o.Propagation == data.Independent || o.Propagation == data.Never || data.From(ctx) == nil
	if !owns {
		// The transaction is already doomed; only its owner can rerun it.
		return once(ctx)
	}
	policy := m.retry
	if o.Retry != nil {
		policy = *o.Retry
	}
	return policy.Run(ctx, once)
}

// guarded runs fn, rolling back if it panics.
func guarded(ctx context.Context, h *handle, tx data.ReadTx, fn func(context.Context, data.ReadTx) error) error {
	defer func() {
		if p := recover(); p != nil {
			_ = h.rollback(ctx)
			panic(p)
		}
	}()
	return fn(ctx, tx)
}

// enclosing returns the handle ctx carries. foreign is set when ctx carries
// a transaction this Manager can't join.
func (m *Manager) enclosing(ctx context.Context) (h *handle, foreign bool) {
	switch t := data.From(ctx).(type) {
	case nil:
		return nil, false
	case *readTx:
		h = t.h
	case *writeTx:
		h = t.h
	default:
		return nil, true
	}
	if h.root.m != m {
		return nil, true
	}
	return h, false
}

func (m *Manager) begin(ctx context.Context, write bool, o data.Options, closure bool) (*handle, error) {
	if o.Retry != nil && !closure {
		return nil, misuse(data.CodePropagation, "data.Retry applies to the Read and Write closures; a pair never retries (use data.Retry(p).Do to rerun one)")
	}
	switch o.Isolation {
	case "", data.ReadCommitted, data.RepeatableRead, data.Serializable:
	default:
		return nil, misuse(data.CodePropagation, fmt.Sprintf("unknown isolation level %q", o.Isolation))
	}
	src, err := m.source(o.Source)
	if err != nil {
		return nil, err
	}
	enc, foreign := m.enclosing(ctx)
	inTx := enc != nil || foreign
	switch o.Propagation {
	case data.Join, data.Mandatory, data.Nested:
		if !inTx {
			if o.Propagation == data.Mandatory {
				return nil, misuse(data.CodePropagation, "data.Mandatory needs an enclosing transaction, and ctx has none")
			}
			return m.beginRoot(ctx, src, write, o)
		}
		if foreign {
			return nil, misuse(data.CodeCrossSource, "the transaction in ctx belongs to another data.Manager, and joining it would need two-phase commit")
		}
		if err := enc.joinable(ctx, src, write, o); err != nil {
			return nil, err
		}
		if o.Propagation == data.Nested {
			return enc.nest(ctx, write)
		}
		return enc.join(write), nil
	case data.Independent:
		if m.within != nil && inTx {
			return nil, misuse(data.CodePropagation, "data.Independent can't run inside a transaction of a Within manager, which has one connection")
		}
		return m.beginRoot(ctx, src, write, o)
	case data.Never:
		if inTx {
			return nil, misuse(data.CodePropagation, "data.Never refuses to run inside a transaction, and ctx has one")
		}
		return m.beginRoot(ctx, src, write, o)
	}
	return nil, misuse(data.CodePropagation, "unknown propagation "+o.Propagation.String())
}

func (m *Manager) beginRoot(ctx context.Context, src *source, write bool, o data.Options) (*handle, error) {
	attrs := []attribute.KeyValue{
		semconv.DBSystemNamePostgreSQL,
		semconv.DBNamespace(src.db),
		semconv.ServerAddress(src.host),
		attrSource.String(src.name),
		attrPropagation.String(o.Propagation.String()),
		attrAccess.String(access(write)),
	}
	if src.port != 0 {
		attrs = append(attrs, semconv.ServerPort(src.port))
	}
	if o.Isolation != "" {
		attrs = append(attrs, attrIsolation.String(string(o.Isolation)))
	}
	sctx, span := m.tracer().Start(ctx, "transaction", trace.WithAttributes(attrs...))
	r := &root{m: m, src: src, write: write, iso: o.Isolation, span: span, outer: trace.SpanContextFromContext(ctx)}
	var tx pgx.Tx
	var err error
	switch {
	case m.within != nil:
		tx, err = m.within.begin(sctx, write, r)
	default:
		pool := src.pool.Load()
		if pool == nil {
			err = errUnavailable
			break
		}
		opts := pgx.TxOptions{IsoLevel: pgx.TxIsoLevel(o.Isolation)}
		if !write {
			// The backstop for reads: the database refuses writes.
			opts.AccessMode = pgx.ReadOnly
		}
		tx, err = pool.BeginTx(sctx, opts)
	}
	if err != nil {
		end(span, err)
		return nil, classify(ctx, err)
	}
	h := &handle{root: r, kind: rootKind, write: write, tx: tx}
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		r.began, r.scope = callers(), s
		if !s.add(h) {
			r.began, r.scope = nil, nil
		}
	}
	return h, nil
}

// begin takes the Within manager's turn and begins a savepoint as the root.
func (w *within) begin(ctx context.Context, write bool, r *root) (pgx.Tx, error) {
	t := time.NewTimer(w.wait)
	defer t.Stop()
	select {
	case w.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		w.mu.Lock()
		held := w.held
		w.mu.Unlock()
		return nil, misuse(data.CodeTxOpen, fmt.Sprintf("a Within manager runs one root transaction at a time, and the open one didn't end within %v; it began at:\n%s", w.wait, formatStack(held)))
	}
	w.mu.Lock()
	w.held = callers()
	w.mu.Unlock()
	r.release = func() { <-w.turn }
	sp, err := w.tx.Begin(ctx)
	if err == nil && !write {
		sp, err = readOnly(ctx, sp)
	}
	if err != nil {
		r.release()
		return nil, err
	}
	return sp, nil
}

// readOnly makes a savepoint read only. Ending it always rolls back to the
// savepoint, which undoes the setting along with anything else, so the
// enclosing transaction can write again afterwards.
func readOnly(ctx context.Context, sp pgx.Tx) (pgx.Tx, error) {
	if _, err := sp.Exec(ctx, "SET LOCAL transaction_read_only = on"); err != nil {
		_ = sp.Rollback(ctx)
		return nil, err
	}
	return readOnlySavepoint{sp}, nil
}

type readOnlySavepoint struct{ pgx.Tx }

func (s readOnlySavepoint) Commit(ctx context.Context) error { return s.Tx.Rollback(ctx) }

// joinable checks that a transaction with o may join or nest in h.
func (h *handle) joinable(ctx context.Context, src *source, write bool, o data.Options) error {
	if h.state == open && o.Isolation != "" && h.root.iso == "" {
		// Begun at the database's default: learn which level that is.
		var level string
		if err := h.tx.QueryRow(h.root.stmtCtx(ctx), "SHOW transaction_isolation").Scan(&level); err != nil {
			return classify(ctx, err)
		}
		h.root.iso = data.IsolationLevel(level)
	}
	switch {
	case h.state != open:
		return misuse(data.CodeTxEnded, "the transaction in ctx has ended")
	case h.root.src != src:
		return misuse(data.CodeCrossSource, fmt.Sprintf("a transaction on source %q can't join one on %q: there is no two-phase commit", src.name, h.root.src.name))
	case write && !h.write:
		return misuse(data.CodePropagation, "a write transaction can't join or nest in a read transaction; use data.Independent for a separate one")
	case o.Isolation != "" && o.Isolation != h.root.iso:
		return misuse(data.CodePropagation, fmt.Sprintf("a %s transaction can't join one begun with isolation %q", o.Isolation, h.root.iso))
	}
	return nil
}

func (h *handle) join(write bool) *handle {
	c := &handle{root: h.root, parent: h, kind: joinedKind, write: write, tx: h.tx}
	h.children = append(h.children, c)
	return c
}

func (h *handle) nest(ctx context.Context, write bool) (*handle, error) {
	if h.state != open {
		return nil, errEnded
	}
	sp, err := h.tx.Begin(h.root.stmtCtx(ctx))
	// Any read nested in a write transaction, a read view's included, is
	// made read only; one in a read transaction already is.
	if err == nil && h.root.write && !write {
		sp, err = readOnly(h.root.stmtCtx(ctx), sp)
	}
	if err != nil {
		return nil, classify(ctx, err)
	}
	c := &handle{root: h.root, parent: h, kind: nestedKind, write: write, tx: sp}
	h.children = append(h.children, c)
	return c, nil
}

// owner is the handle whose savepoint or transaction h runs in: h itself,
// unless h joined another.
func (h *handle) owner() *handle {
	for h.kind == joinedKind {
		h = h.parent
	}
	return h
}

func (h *handle) forget(c *handle) {
	h.children = slices.DeleteFunc(h.children, func(x *handle) bool { return x == c })
}

// openSavepoint reports whether a nested handle below h is still open.
func (h *handle) openSavepoint() bool {
	for _, c := range h.children {
		if c.state == open && (c.kind == nestedKind || c.openSavepoint()) {
			return true
		}
	}
	return false
}

// endChildren ends every handle below h, which h's end ends too. Those
// rolled back with it hand their AfterRollback callbacks to h's owner.
func (h *handle) endChildren(s state) {
	for _, c := range h.children {
		if c.state != open {
			continue
		}
		c.endChildren(s)
		c.state = s
		if c.kind == nestedKind {
			o := h.owner()
			o.afterRollback = append(o.afterRollback, c.afterRollback...)
		}
	}
	h.children = nil
}

func (h *handle) commit(ctx context.Context) error {
	switch h.state {
	case committed:
		return misuse(data.CodeTxEnded, "Commit called twice")
	case rolledBack:
		return misuse(data.CodeTxEnded, "Commit called after Rollback")
	}
	if h.openSavepoint() {
		_ = h.rollback(ctx)
		return misuse(data.CodeTxOpen, "Commit called while a nested transaction was still open; rolled back instead")
	}
	if h.rollbackOnly {
		_ = h.rollback(ctx)
		return misuse(data.CodeRollbackOnly, "a transaction that joined this one rolled back, so it was rolled back instead of committed")
	}
	h.endChildren(committed)
	switch h.kind {
	case joinedKind:
		h.state = committed
		h.parent.forget(h)
		return nil
	case nestedKind:
		err := h.tx.Commit(h.root.stmtCtx(ctx))
		h.parent.forget(h)
		p := h.parent.owner()
		if err != nil {
			h.state = rolledBack
			p.rollbackOnly = true
			run(ctx, h.afterRollback)
			return classify(ctx, err)
		}
		h.state = committed
		p.afterCommit = append(p.afterCommit, h.afterCommit...)
		p.afterRollback = append(p.afterRollback, h.afterRollback...)
		return nil
	}
	err := h.tx.Commit(h.root.stmtCtx(ctx))
	if err != nil {
		// pgx has ended the transaction either way.
		h.state = rolledBack
		h.root.finish(h, "rollback", err)
		run(ctx, h.afterRollback)
		return classify(ctx, err)
	}
	h.state = committed
	h.root.finish(h, "commit", nil)
	run(ctx, h.afterCommit)
	return nil
}

func (h *handle) rollback(ctx context.Context) error {
	if h.state != open {
		return nil
	}
	h.endChildren(rolledBack)
	h.state = rolledBack
	if h.kind == joinedKind {
		h.parent.forget(h)
		if h.write {
			h.parent.owner().rollbackOnly = true
		}
		return nil
	}
	// A cancelled ctx would fail the rollback and cost the connection.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := h.tx.Rollback(h.root.stmtCtx(dctx))
	if h.kind == nestedKind {
		h.parent.forget(h)
		if err != nil {
			h.parent.owner().rollbackOnly = true
		}
	} else {
		h.root.finish(h, "rollback", err)
	}
	run(ctx, h.afterRollback)
	return classify(ctx, err)
}

// finish ends the root transaction's span and tracking.
func (r *root) finish(h *handle, outcome string, err error) {
	r.span.SetAttributes(attribute.String("galleon.data.outcome", outcome))
	end(r.span, err)
	if r.scope != nil {
		r.scope.remove(h)
	}
	if r.release != nil {
		r.release()
	}
}

// run runs callbacks in order, with ctx's values but not its cancellation:
// the transaction has ended whatever happens to the caller now.
func run(ctx context.Context, fns []func(context.Context)) {
	if len(fns) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, fn := range fns {
		fn(ctx)
	}
}

func (r *root) stmtCtx(ctx context.Context) context.Context {
	if trace.SpanContextFromContext(ctx).Equal(r.outer) {
		return trace.ContextWithSpan(ctx, r.span)
	}
	return ctx
}

func (h *handle) addCallback(after *[]func(context.Context), fn func(context.Context)) {
	if fn != nil && h.state == open {
		*after = append(*after, fn)
	}
}

// Statements.

func (h *handle) query(ctx context.Context, sql string, args []any) (data.Rows, error) {
	if h.state != open {
		return nil, errEnded
	}
	rows, err := h.tx.Query(h.root.stmtCtx(ctx), sql, args...)
	if err != nil {
		return nil, classify(ctx, err)
	}
	return &classifiedRows{Rows: rows, ctx: ctx}, nil
}

func (h *handle) queryRow(ctx context.Context, sql string, args []any) data.Row {
	if h.state != open {
		return errRow{errEnded}
	}
	return classifiedRow{row: h.tx.QueryRow(h.root.stmtCtx(ctx), sql, args...), ctx: ctx}
}

func (h *handle) sendBatch(ctx context.Context, b *data.Batch) data.BatchResults {
	if h.state != open {
		return errBatch{errEnded}
	}
	return classifiedBatch{br: h.tx.SendBatch(h.root.stmtCtx(ctx), b), ctx: ctx}
}

func (h *handle) exec(ctx context.Context, sql string, args []any) (data.CommandTag, error) {
	if h.state != open {
		return data.CommandTag{}, errEnded
	}
	tag, err := h.tx.Exec(h.root.stmtCtx(ctx), sql, args...)
	return tag, classify(ctx, err)
}

func (h *handle) copyFrom(ctx context.Context, table data.Identifier, cols []string, src data.CopyFromSource) (int64, error) {
	if h.state != open {
		return 0, errEnded
	}
	n, err := h.tx.CopyFrom(h.root.stmtCtx(ctx), table, cols, src)
	return n, classify(ctx, err)
}

func (t *readTx) Query(ctx context.Context, sql string, args ...any) (data.Rows, error) {
	return t.h.query(ctx, sql, args)
}
func (t *readTx) QueryRow(ctx context.Context, sql string, args ...any) data.Row {
	return t.h.queryRow(ctx, sql, args)
}
func (t *readTx) SendBatch(ctx context.Context, b *data.Batch) data.BatchResults {
	return t.h.sendBatch(ctx, b)
}
func (t *readTx) Begin(ctx context.Context) (data.ReadTx, error) {
	c, err := t.h.nest(ctx, false)
	if err != nil {
		return nil, err
	}
	return &readTx{c}, nil
}
func (t *readTx) Commit(ctx context.Context) error   { return t.h.commit(ctx) }
func (t *readTx) Rollback(ctx context.Context) error { return t.h.rollback(ctx) }

func (t *writeTx) Exec(ctx context.Context, sql string, args ...any) (data.CommandTag, error) {
	return t.h.exec(ctx, sql, args)
}
func (t *writeTx) CopyFrom(ctx context.Context, table data.Identifier, cols []string, src data.CopyFromSource) (int64, error) {
	return t.h.copyFrom(ctx, table, cols, src)
}
func (t *writeTx) BeginWrite(ctx context.Context) (data.WriteTx, error) {
	c, err := t.h.nest(ctx, true)
	if err != nil {
		return nil, err
	}
	return &writeTx{readTx{c}}, nil
}
func (t *writeTx) AfterCommit(fn func(context.Context)) {
	o := t.h.owner()
	t.h.addCallback(&o.afterCommit, fn)
}
func (t *writeTx) AfterRollback(fn func(context.Context)) {
	o := t.h.owner()
	t.h.addCallback(&o.afterRollback, fn)
}

// classifiedRows, classifiedRow and classifiedBatch classify the errors
// pgx reports late, when rows are read.
type classifiedRows struct {
	pgx.Rows
	ctx context.Context
}

func (r *classifiedRows) Err() error { return classify(r.ctx, r.Rows.Err()) }

type classifiedRow struct {
	row pgx.Row
	ctx context.Context
}

func (r classifiedRow) Scan(dest ...any) error { return classify(r.ctx, r.row.Scan(dest...)) }

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type classifiedBatch struct {
	br  pgx.BatchResults
	ctx context.Context
}

func (b classifiedBatch) Exec() (data.CommandTag, error) {
	tag, err := b.br.Exec()
	return tag, classify(b.ctx, err)
}

func (b classifiedBatch) Query() (data.Rows, error) {
	rows, err := b.br.Query()
	if err != nil {
		return rows, classify(b.ctx, err)
	}
	return &classifiedRows{Rows: rows, ctx: b.ctx}, nil
}

func (b classifiedBatch) QueryRow() data.Row {
	return classifiedRow{row: b.br.QueryRow(), ctx: b.ctx}
}

func (b classifiedBatch) Close() error { return classify(b.ctx, b.br.Close()) }

type errBatch struct{ err error }

func (b errBatch) Exec() (data.CommandTag, error) { return data.CommandTag{}, b.err }
func (b errBatch) Query() (data.Rows, error)      { return nil, b.err }
func (b errBatch) QueryRow() data.Row             { return errRow{b.err} }
func (b errBatch) Close() error                   { return b.err }
