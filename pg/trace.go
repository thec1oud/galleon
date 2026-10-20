package pg

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/data"
)

const tracerName = "github.com/thec1oud/galleon/pg"

// Attribute keys of Galleon's own, beside the semantic conventions.
const (
	attrSource      = attribute.Key("galleon.data.source")
	attrPropagation = attribute.Key("galleon.data.propagation")
	attrAccess      = attribute.Key("galleon.data.access")
	attrIsolation   = attribute.Key("galleon.data.isolation")
)

// queryTracer makes a client span of every statement, batch and copy, with
// the database semantic conventions. It records the SQL text, which carries
// placeholders, and never the arguments.
type queryTracer struct {
	m       *Manager
	source  string
	replica string // the replica's name, on a replica's connections
}

func (t *queryTracer) attrs(conn *pgx.Conn) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNamePostgreSQL, attrSource.String(t.source)}
	if t.replica != "" {
		attrs = append(attrs, attrReplica.String(t.replica))
	}
	if conn != nil {
		cfg := conn.Config()
		attrs = append(attrs, semconv.DBNamespace(cfg.Database), semconv.ServerAddress(cfg.Host))
		if cfg.Port != 0 {
			attrs = append(attrs, semconv.ServerPort(int(cfg.Port)))
		}
	}
	return attrs
}

func (t *queryTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	op := operation(d.SQL)
	attrs := append(t.attrs(conn), semconv.DBQueryText(d.SQL))
	name := "postgresql"
	if op != "" {
		attrs = append(attrs, semconv.DBOperationName(op))
		name = op
	}
	ctx, _ = t.m.tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	end(trace.SpanFromContext(ctx), d.Err)
}

func (t *queryTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceBatchStartData) context.Context {
	attrs := append(t.attrs(conn), semconv.DBOperationName("BATCH"))
	if d.Batch != nil {
		attrs = append(attrs, semconv.DBOperationBatchSize(d.Batch.Len()))
	}
	ctx, _ = t.m.tracer().Start(ctx, "BATCH", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx
}

func (t *queryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, d pgx.TraceBatchQueryData) {
	attrs := []attribute.KeyValue{semconv.DBQueryText(d.SQL)}
	if code := sqlState(d.Err); code != "" {
		attrs = append(attrs, semconv.DBResponseStatusCode(code))
	}
	trace.SpanFromContext(ctx).AddEvent("query", trace.WithAttributes(attrs...))
}

func (t *queryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceBatchEndData) {
	end(trace.SpanFromContext(ctx), d.Err)
}

func (t *queryTracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceCopyFromStartData) context.Context {
	attrs := append(t.attrs(conn), semconv.DBOperationName("COPY"), semconv.DBCollectionName(d.TableName.Sanitize()))
	ctx, _ = t.m.tracer().Start(ctx, "COPY "+d.TableName.Sanitize(), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx
}

func (t *queryTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceCopyFromEndData) {
	end(trace.SpanFromContext(ctx), d.Err)
}

// commitSpan starts the client span of a commit sent on conn without pgx's
// tracer, as a statement span named COMMIT.
func (m *Manager) commitSpan(ctx context.Context, conn *pgx.Conn, sql string) (context.Context, trace.Span) {
	var attrs []attribute.KeyValue
	if t, ok := conn.Config().Tracer.(*queryTracer); ok {
		attrs = t.attrs(conn)
	} else {
		attrs = (&queryTracer{source: data.PrimarySource}).attrs(conn)
	}
	attrs = append(attrs, semconv.DBQueryText(sql), semconv.DBOperationName("COMMIT"))
	return m.tracer().Start(ctx, "COMMIT", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
}

// end ends a statement or transaction span, recording the SQLSTATE of a
// failure but not the database's message, which can quote data.
func end(span trace.Span, err error) {
	if err != nil {
		code := sqlState(err)
		typ := code
		if code != "" {
			span.SetAttributes(semconv.DBResponseStatusCode(code))
		} else {
			typ = "_OTHER"
		}
		span.SetAttributes(semconv.ErrorTypeKey.String(typ))
		span.SetStatus(codes.Error, typ)
	}
	span.End()
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// operation is the statement's first keyword, such as SELECT, or "" if it
// doesn't start with one.
func operation(sql string) string {
	sql = strings.TrimLeft(sql, " \t\r\n(")
	i := 0
	for i < len(sql) && (sql[i] >= 'a' && sql[i] <= 'z' || sql[i] >= 'A' && sql[i] <= 'Z') {
		i++
	}
	return strings.ToUpper(sql[:i])
}
