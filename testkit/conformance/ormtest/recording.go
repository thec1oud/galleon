package ormtest

import (
	"context"
	"slices"
	"sync"

	"github.com/thec1oud/galleon/data"
)

// recording keeps what ran through the handles the suite gave an adapter:
// for each statement, the model its ctx named, if any.
type recording struct {
	mu     sync.Mutex
	n      int
	marked []string
}

func (r *recording) add(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	if name, ok := data.ModelFrom(ctx); ok && !slices.Contains(r.marked, name) {
		r.marked = append(r.marked, name)
	}
}

func (r *recording) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// models returns the models statements were marked for.
func (r *recording) models() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.marked)
}

func (r *recording) reader(tx data.ReadTx) data.ReadTx   { return &recReader{ReadTx: tx, rec: r} }
func (r *recording) writer(tx data.WriteTx) data.WriteTx { return &recWriter{WriteTx: tx, rec: r} }

// recReader and recWriter record each statement before running it on the
// transaction they wrap, and wrap the handles of nested transactions too.
type recReader struct {
	data.ReadTx
	rec *recording
}

func (t *recReader) Query(ctx context.Context, sql string, args ...any) (data.Rows, error) {
	t.rec.add(ctx)
	return t.ReadTx.Query(ctx, sql, args...)
}

func (t *recReader) QueryRow(ctx context.Context, sql string, args ...any) data.Row {
	t.rec.add(ctx)
	return t.ReadTx.QueryRow(ctx, sql, args...)
}

func (t *recReader) SendBatch(ctx context.Context, b *data.Batch) data.BatchResults {
	t.rec.add(ctx)
	return t.ReadTx.SendBatch(ctx, b)
}

func (t *recReader) Begin(ctx context.Context) (data.ReadTx, error) {
	tx, err := t.ReadTx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return t.rec.reader(tx), nil
}

type recWriter struct {
	data.WriteTx
	rec *recording
}

func (t *recWriter) Query(ctx context.Context, sql string, args ...any) (data.Rows, error) {
	t.rec.add(ctx)
	return t.WriteTx.Query(ctx, sql, args...)
}

func (t *recWriter) QueryRow(ctx context.Context, sql string, args ...any) data.Row {
	t.rec.add(ctx)
	return t.WriteTx.QueryRow(ctx, sql, args...)
}

func (t *recWriter) SendBatch(ctx context.Context, b *data.Batch) data.BatchResults {
	t.rec.add(ctx)
	return t.WriteTx.SendBatch(ctx, b)
}

func (t *recWriter) Exec(ctx context.Context, sql string, args ...any) (data.CommandTag, error) {
	t.rec.add(ctx)
	return t.WriteTx.Exec(ctx, sql, args...)
}

func (t *recWriter) CopyFrom(ctx context.Context, table data.Identifier, columns []string, src data.CopyFromSource) (int64, error) {
	t.rec.add(ctx)
	return t.WriteTx.CopyFrom(ctx, table, columns, src)
}

// Ended forwards the wrapped handle's, so data.AfterCommit refuses a
// recorded handle that has ended as it refuses the handle itself.
func (t *recWriter) Ended() bool {
	e, ok := t.WriteTx.(data.Ender)
	return ok && e.Ended()
}

func (t *recWriter) Begin(ctx context.Context) (data.ReadTx, error) {
	tx, err := t.WriteTx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return t.rec.reader(tx), nil
}

func (t *recWriter) BeginWrite(ctx context.Context) (data.WriteTx, error) {
	tx, err := t.WriteTx.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	return t.rec.writer(tx), nil
}
