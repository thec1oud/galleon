package pg

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/thec1oud/galleon/data"
)

const meterName = tracerName

// metrics are the data plane's instruments. Their attributes name replicas
// and levels, never a DSN or a token.
//
//	galleon.data.replica.lag          gauge, s: each replica's tracked lag in time
//	galleon.data.replica.lag_bytes    gauge, By: and in WAL bytes
//	galleon.data.replica.in_rotation  gauge: 1 while a replica is in rotation, else 0
//	galleon.data.reads                counter: reads that began a transaction on the primary source, by where they ran
//	galleon.data.read.fallbacks       counter: of those, the ones that read the primary when a replica might have served
//	galleon.data.read.wait            histogram, s: how long ReadYourWrites reads waited for a replica
//	galleon.data.consistency.tokens   counter: incoming causal tokens, by result
type metrics struct {
	reads     metric.Int64Counter
	fallbacks metric.Int64Counter
	wait      metric.Float64Histogram
	tokens    metric.Int64Counter
}

// Attribute keys of the metrics and spans.
const (
	attrReplica     = attribute.Key("galleon.data.replica")
	attrConsistency = attribute.Key("galleon.data.consistency")
	attrFallback    = attribute.Key("galleon.data.fallback")
	attrWaited      = attribute.Key("galleon.data.wait.duration")
	attrTarget      = attribute.Key("galleon.data.target")
	attrResult      = attribute.Key("galleon.data.token.result")
)

func newMetrics(mp metric.MeterProvider, topo *topology) *metrics {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	meter := mp.Meter(meterName)
	// An instrument the provider can't make is a no-op one, and its error
	// the provider's to report: metrics never stop the data plane.
	x := &metrics{reads: noop.Int64Counter{}, fallbacks: noop.Int64Counter{}, wait: noop.Float64Histogram{}, tokens: noop.Int64Counter{}}
	if c, err := meter.Int64Counter("galleon.data.reads", metric.WithDescription("Reads that began a transaction on the primary source, by where they ran.")); err == nil {
		x.reads = c
	}
	if c, err := meter.Int64Counter("galleon.data.read.fallbacks", metric.WithDescription("Reads that read the primary when a replica might have served them, by reason.")); err == nil {
		x.fallbacks = c
	}
	if h, err := meter.Float64Histogram("galleon.data.read.wait", metric.WithUnit("s"), metric.WithDescription("How long ReadYourWrites reads waited for a replica to catch up.")); err == nil {
		x.wait = h
	}
	if c, err := meter.Int64Counter("galleon.data.consistency.tokens", metric.WithDescription("Incoming causal tokens, by result.")); err == nil {
		x.tokens = c
	}
	if topo == nil {
		return x
	}
	lag, err1 := meter.Float64ObservableGauge("galleon.data.replica.lag", metric.WithUnit("s"), metric.WithDescription("Each replica's tracked lag behind the primary, in time."))
	lagBytes, err2 := meter.Int64ObservableGauge("galleon.data.replica.lag_bytes", metric.WithUnit("By"), metric.WithDescription("Each replica's tracked lag behind the primary, in WAL bytes."))
	rotating, err3 := meter.Int64ObservableGauge("galleon.data.replica.in_rotation", metric.WithDescription("1 while a replica is in rotation, else 0."))
	if err1 != nil || err2 != nil || err3 != nil {
		return x
	}
	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		now := topo.now()
		for _, r := range topo.replicas {
			set := metric.WithAttributes(attrReplica.String(r.src.name))
			rd := r.reading.Load()
			in, _ := topo.rotating(rd, now)
			o.ObserveInt64(rotating, b2i(in), set)
			if rd == nil || !rd.healthy {
				continue
			}
			if rd.lag != unknownLag {
				o.ObserveFloat64(lag, rd.lag.Seconds(), set)
			}
			o.ObserveInt64(lagBytes, rd.lagBytes, set)
		}
		return nil
	}, lag, lagBytes, rotating)
	return x
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// read counts a read that began on the primary source.
func (x *metrics) read(ctx context.Context, l data.Level, rt routing) {
	target := data.PrimarySource
	if rt.rep != nil {
		target = rt.rep.src.name
	}
	kind := kindName(l)
	x.reads.Add(ctx, 1, metric.WithAttributes(attrTarget.String(target), attrConsistency.String(kind)))
	if rt.fallback != "" {
		x.fallbacks.Add(ctx, 1, metric.WithAttributes(attrConsistency.String(kind), attrFallback.String(rt.fallback)))
	}
	if rt.waited > 0 {
		x.wait.Record(ctx, rt.waited.Seconds(), metric.WithAttributes(attrTarget.String(target)))
	}
}

func (x *metrics) token(ctx context.Context, result string) {
	x.tokens.Add(ctx, 1, metric.WithAttributes(attrResult.String(result)))
}

// kindName names a level's kind without its parameters, for metrics.
func kindName(l data.Level) string {
	switch l.Kind() {
	case data.PrimaryKind:
		return "primary"
	case data.AnyKind:
		return "any"
	case data.BoundedStalenessKind:
		return "bounded_staleness"
	case data.ReadYourWritesKind:
		return "read_your_writes"
	}
	return "unknown"
}
