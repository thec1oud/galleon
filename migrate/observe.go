package migrate

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/errs"
)

const tracerName = "github.com/thec1oud/galleon/migrate"

// Attribute keys of the spans, logs and metrics. None holds SQL text or a
// secret: a step's description, which may, is recorded only with
// Policy.Debug.
const (
	attrMigrator    = attribute.Key("galleon.migrate.migrator")
	attrStep        = attribute.Key("galleon.migrate.step")
	attrRisk        = attribute.Key("galleon.migrate.risk")
	attrDescription = attribute.Key("galleon.migrate.step.description")
	attrOutcome     = attribute.Key("galleon.migrate.outcome")
	attrToken       = attribute.Key("galleon.migrate.lock.token")
	attrFingerprint = attribute.Key("galleon.migrate.plan.fingerprint")
	attrWaited      = attribute.Key("galleon.migrate.lock.waited")
	attrResult      = attribute.Key("galleon.migrate.step.result")
	attrCode        = attribute.Key("galleon.migrate.error.code")
)

// metrics are the Runner's instruments:
//
//	galleon.migrate.applies         counter: applies of one migrator, by outcome
//	galleon.migrate.failures        counter: of those, the refused and failed ones, by error code
//	galleon.migrate.apply.duration  histogram, s: how long an apply took, lock wait included
//	galleon.migrate.steps           counter: steps run, by risk and result
type metrics struct {
	applies  metric.Int64Counter
	failures metric.Int64Counter
	duration metric.Float64Histogram
	steps    metric.Int64Counter
}

func newMetrics(mp metric.MeterProvider) *metrics {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	meter := mp.Meter(tracerName)
	// An instrument the provider can't make is a no-op one: metrics never
	// stop a migration.
	x := &metrics{applies: noop.Int64Counter{}, failures: noop.Int64Counter{}, duration: noop.Float64Histogram{}, steps: noop.Int64Counter{}}
	if c, err := meter.Int64Counter("galleon.migrate.applies", metric.WithDescription("Applies of one migrator, by outcome.")); err == nil {
		x.applies = c
	}
	if c, err := meter.Int64Counter("galleon.migrate.failures", metric.WithDescription("Applies that were refused or failed, by error code.")); err == nil {
		x.failures = c
	}
	if h, err := meter.Float64Histogram("galleon.migrate.apply.duration", metric.WithUnit("s"), metric.WithDescription("How long applying one migrator took, waiting for its lock included.")); err == nil {
		x.duration = h
	}
	if c, err := meter.Int64Counter("galleon.migrate.steps", metric.WithDescription("Migration steps run, by risk and result.")); err == nil {
		x.steps = c
	}
	return x
}

// finish ends an apply's span and records its log line and metrics.
func (r *Runner) finish(ctx context.Context, span trace.Span, log *slog.Logger, rep *ApplyReport) {
	set := metric.WithAttributes(attrMigrator.String(rep.Name), attrOutcome.String(string(rep.Outcome)))
	r.metrics.applies.Add(ctx, 1, set)
	r.metrics.duration.Record(ctx, rep.Duration.Seconds(), set)
	span.SetAttributes(attrOutcome.String(string(rep.Outcome)), attrWaited.Bool(rep.Waited))
	attrs := []any{"outcome", rep.Outcome, "steps", len(rep.Result.Applied), "duration", rep.Duration, "waited", rep.Waited}
	if rep.Err != nil {
		code := errs.From(rep.Err).Code
		r.metrics.failures.Add(ctx, 1, metric.WithAttributes(attrMigrator.String(rep.Name), attrOutcome.String(string(rep.Outcome)), attrCode.String(code)))
		span.SetAttributes(attrCode.String(code))
		span.SetStatus(codes.Error, rep.Err.Error())
		log.ErrorContext(ctx, "migration "+string(rep.Outcome), append(attrs, "error", rep.Err)...)
	} else {
		log.InfoContext(ctx, "migration "+string(rep.Outcome), attrs...)
	}
	span.End()
}

// observer maps a migrator's steps to spans, log records and metrics.
type observer struct {
	r        *Runner
	migrator string
	log      *slog.Logger
}

func (o *observer) Step(ctx context.Context, s StepInfo) (context.Context, func(error)) {
	attrs := []attribute.KeyValue{attrMigrator.String(o.migrator), attrStep.String(s.ID), attrRisk.String(s.Risk.String())}
	logAttrs := []any{"step", s.ID, "risk", s.Risk.String()}
	if o.r.policy.Debug {
		attrs = append(attrs, attrDescription.String(s.Description))
		logAttrs = append(logAttrs, "description", s.Description)
	}
	ctx, span := o.r.tracer.Start(ctx, "migrate.step", trace.WithAttributes(attrs...))
	start := time.Now()
	o.log.InfoContext(ctx, "migration step started", logAttrs...)
	return ctx, func(err error) {
		result := "applied"
		if err != nil {
			result = "failed"
			span.SetStatus(codes.Error, err.Error())
			o.log.ErrorContext(ctx, "migration step failed", append(logAttrs, "duration", time.Since(start), "error", err)...)
		} else {
			o.log.InfoContext(ctx, "migration step applied", append(logAttrs, "duration", time.Since(start))...)
		}
		o.r.metrics.steps.Add(ctx, 1, metric.WithAttributes(attrMigrator.String(o.migrator), attrRisk.String(s.Risk.String()), attrResult.String(result)))
		span.End()
	}
}
