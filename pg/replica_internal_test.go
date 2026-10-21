package pg

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// fakeTracker is a Manager whose replicas are never polled: the test sets
// their readings itself.
type fakeTracker struct {
	t   *testing.T
	m   *Manager
	now time.Time
}

func newFakeTracker(t *testing.T, cfg Config, zones ...string) *fakeTracker {
	t.Helper()
	cfg.Primary = Source{DSN: "postgres://app@primary/db"}
	for i, z := range zones {
		cfg.Replicas = append(cfg.Replicas, Source{DSN: "postgres://app@replica" + string(rune('a'+i)) + "/db", Zone: z})
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTracker{t: t, m: m, now: time.Unix(1_000_000, 0)}
	m.topo.now = func() time.Time { return f.now }
	return f
}

// set gives replica i a healthy reading, taken now.
func (f *fakeTracker) set(i int, replay uint64, lag time.Duration) {
	f.m.topo.replicas[i].reading.Store(&reading{at: f.now, healthy: true, replay: replay, lag: lag})
}

func (f *fakeTracker) down(i int) {
	f.m.topo.replicas[i].reading.Store(&reading{at: f.now, reason: reasonUnreachable, lag: unknownLag})
}

// where routes a read at l and names where it runs.
func (f *fakeTracker) where(ctx context.Context, l data.Level) (string, routing) {
	f.t.Helper()
	rt, err := f.m.route(ctx, l)
	if err != nil {
		f.t.Fatalf("%v: %v", l, err)
	}
	if rt.rep == nil {
		return data.PrimarySource, rt
	}
	return rt.rep.src.name, rt
}

func withFloor(lsn uint64) context.Context {
	c := &causal{}
	c.raise(lsn)
	return context.WithValue(context.Background(), causalKey{}, c)
}

func TestEachLevelChoosesFromTheTrackedReplicas(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: time.Second}, "", "")
	f.set(0, 100, 300*time.Millisecond)
	f.set(1, 200, 50*time.Millisecond)
	ctx := context.Background()
	for _, c := range []struct {
		ctx  context.Context
		l    data.Level
		want string
	}{
		{ctx, data.Primary, "primary"},
		{ctx, data.Any, "replica_2"}, // least lag
		{ctx, data.BoundedStaleness(100 * time.Millisecond), "replica_2"},
		{ctx, data.BoundedStaleness(10 * time.Millisecond), "primary"},
		{ctx, data.ReadYourWrites, "replica_2"}, // no floor
		{withFloor(150), data.ReadYourWrites, "replica_2"},
		{withFloor(201), data.ReadYourWrites, "primary"},
	} {
		if got, rt := f.where(c.ctx, c.l); got != c.want {
			t.Errorf("%v: read %s, want %s", c.l, got, c.want)
		} else if got == "primary" && c.l != data.Primary && rt.fallback != fallbackNoReplica {
			t.Errorf("%v: fell back saying %q", c.l, rt.fallback)
		}
	}
	// replica_1 alone has replayed the floor.
	f.set(0, 300, 300*time.Millisecond)
	if got, _ := f.where(withFloor(250), data.ReadYourWrites); got != "replica_1" {
		t.Errorf("read %s, want the only replica past the floor", got)
	}
}

func TestReplicasOutOfRotationServeNoRead(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: time.Second, MaxLagBytes: 1000}, "", "")
	f.set(0, 100, 2*time.Second) // past MaxLag
	f.down(1)
	for _, l := range []data.Level{data.Any, data.ReadYourWrites, data.BoundedStaleness(time.Hour)} {
		if got, _ := f.where(context.Background(), l); got != "primary" {
			t.Errorf("%v read %s", l, got)
		}
	}
	// Back within MaxLag, but too many bytes behind.
	f.m.topo.replicas[0].reading.Store(&reading{at: f.now, healthy: true, replay: 100, lag: 0, lagBytes: 5000})
	if got, _ := f.where(context.Background(), data.Any); got != "primary" {
		t.Errorf("a replica past MaxLagBytes served a read")
	}
	f.set(0, 100, 0)
	if got, _ := f.where(context.Background(), data.Any); got != "replica_1" {
		t.Errorf("a recovered replica didn't return to rotation: read %s", got)
	}
	// A reading the tracker hasn't renewed for a while no longer counts.
	f.now = f.now.Add(f.m.topo.staleAfter() + time.Millisecond)
	if got, rt := f.where(context.Background(), data.Any); got != "primary" || rt.fallback != fallbackNoReplica {
		t.Errorf("a stale reading served a read: %s", got)
	}
}

func TestStrategiesPickAsTheySay(t *testing.T) {
	f := newFakeTracker(t, Config{Select: RoundRobin}, "a", "b", "c")
	for i := range 3 {
		f.set(i, 100, time.Duration(i)*time.Millisecond)
	}
	seen := map[string]int{}
	for range 9 {
		got, _ := f.where(context.Background(), data.Any)
		seen[got]++
	}
	if len(seen) != 3 || seen["replica_1"] != 3 || seen["replica_3"] != 3 {
		t.Errorf("round robin: %v", seen)
	}

	f = newFakeTracker(t, Config{Select: Nearest, Zone: "b"}, "a", "b", "b")
	f.set(0, 100, 0)
	f.set(1, 100, 20*time.Millisecond)
	f.set(2, 100, 10*time.Millisecond)
	if got, _ := f.where(context.Background(), data.Any); got != "replica_3" {
		t.Errorf("nearest took %s, want the least lagging in zone b", got)
	}
	f.down(1)
	f.down(2)
	if got, _ := f.where(context.Background(), data.Any); got != "replica_1" {
		t.Errorf("nearest took %s, want another zone's when its own has none", got)
	}

	f = newFakeTracker(t, Config{Select: Random}, "", "")
	f.set(0, 100, 0)
	f.set(1, 100, 0)
	seen = map[string]int{}
	for i := 0; i < 1000 && len(seen) < 2; i++ {
		got, _ := f.where(context.Background(), data.Any)
		seen[got]++
	}
	if len(seen) != 2 {
		t.Errorf("random only ever took %v", seen)
	}
}

func TestReadYourWritesFallsBackAsItsPolicySays(t *testing.T) {
	f := newFakeTracker(t, Config{}, "")
	f.set(0, 100, 0)
	ctx := withFloor(500)
	if got, rt := f.where(ctx, data.ReadYourWrites); got != "primary" || rt.fallback != fallbackNoReplica || rt.waited != 0 {
		t.Errorf("default: read %s (%+v), want the primary at once", got, rt)
	}
	_, err := f.m.route(ctx, data.ReadYourWrites.OrFail())
	if e := errs.From(err); e.Kind != errs.Unavailable || e.Code != data.CodeConsistencyTimeout {
		t.Errorf("OrFail: %v", err)
	}
	// Waiting needs a replica to wait on: with none in rotation it
	// falls back at once.
	f.down(0)
	start := time.Now()
	if got, rt := f.where(ctx, data.ReadYourWrites.Wait(time.Minute)); got != "primary" || rt.fallback != fallbackNoReplica {
		t.Errorf("read %s (%+v)", got, rt)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("it waited for a replica that is down")
	}
}

func TestNoReplicasMeansThePrimaryForEveryLevel(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.topo != nil {
		t.Fatal("a topology without replicas")
	}
	if m.level != data.ReadYourWrites {
		t.Fatalf("the default consistency is %v", m.level)
	}
}

func TestTheCallsLevelWinsOverTheOperationsAndTheConfigs(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}, Consistency: "any"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if l, _ := m.levelFor(ctx, data.ReadOptions()); l != data.Any {
		t.Errorf("config: %v", l)
	}
	octx := data.WithConsistency(ctx, data.BoundedStaleness(time.Second))
	if l, _ := m.levelFor(octx, data.ReadOptions()); l != data.BoundedStaleness(time.Second) {
		t.Errorf("operation: %v", l)
	}
	if l, _ := m.levelFor(octx, data.ReadOptions(data.Consistency(data.Primary))); l != data.Primary {
		t.Errorf("call: %v", l)
	}
}

func TestAnInvalidLevelFailsTheReadRatherThanFallingBack(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}, Consistency: "any"})
	if err != nil {
		t.Fatal(err)
	}
	bad := data.BoundedStaleness(-time.Second)
	for what, c := range map[string]struct {
		ctx  context.Context
		opts []data.ReadOption
	}{
		"from ctx":      {data.WithConsistency(context.Background(), bad), nil},
		"from the call": {context.Background(), []data.ReadOption{data.Consistency(bad)}},
	} {
		_, err := m.BeginRead(c.ctx, c.opts...)
		if e := errs.From(err); err == nil || e.Kind != errs.Internal || e.Code != data.CodeInvalidConsistency {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestACaughtUpReplicaOfAnIdlePrimaryLagsOnlyByItsLatestSamplesAge(t *testing.T) {
	var h history
	t0 := time.Unix(0, 0)
	if b, lag := h.lag(10, t0, time.Minute); b != unknownBytes || lag != unknownLag {
		t.Fatalf("without samples the lag is known: %d bytes, %v", b, lag)
	}
	// The primary sat at 100 for a minute: the replica at 100 is caught
	// up, however long ago its last replayed commit was, but for what the
	// primary may have written since its latest sample, a second ago.
	for i := range 60 {
		h.add(sample{at: t0.Add(time.Duration(i) * time.Second), lsn: 100})
	}
	if b, lag := h.lag(100, t0.Add(time.Minute), time.Minute); b != 0 || lag != time.Second {
		t.Errorf("caught up: %d bytes, %v", b, lag)
	}
	// The primary moved to 200 at 60s and 300 at 61s; a replica at 250
	// was last where the primary was at 60s.
	h.add(sample{at: t0.Add(60 * time.Second), lsn: 200})
	h.add(sample{at: t0.Add(61 * time.Second), lsn: 300})
	if b, lag := h.lag(250, t0.Add(62*time.Second), time.Minute); b != 50 || lag != 2*time.Second {
		t.Errorf("behind: %d bytes, %v", b, lag)
	}
	if _, lag := h.lag(50, t0.Add(62*time.Second), time.Minute); lag != unknownLag {
		t.Errorf("behind every sample: %v", lag)
	}
}

func TestTheHistoryKeepsItsNewestSamples(t *testing.T) {
	var h history
	for i := range historyLen + 10 {
		h.add(sample{at: time.Unix(int64(i), 0), lsn: uint64(i)})
	}
	if len(h.samples) != historyLen || h.samples[0].lsn != 10 {
		t.Fatalf("%d samples from %d", len(h.samples), h.samples[0].lsn)
	}
}

func TestLSNsReadAndPrintAsPostgresWritesThem(t *testing.T) {
	for s, want := range map[string]uint64{"0/0": 0, "16/B374D848": 0x16B374D848, "FFFFFFFF/FFFFFFFF": 1<<64 - 1} {
		got, err := parseLSN(s)
		if err != nil || got != want || formatLSN(got) != s {
			t.Errorf("%s: %x %v %s", s, got, err, formatLSN(got))
		}
	}
	for _, s := range []string{"", "12", "x/1", "1/100000000"} {
		if _, err := parseLSN(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
}

func TestTheFloorIsNeverLowered(t *testing.T) {
	c := &causal{}
	c.raise(10)
	c.raise(5)
	if c.lsn.Load() != 10 {
		t.Fatalf("lowered to %d", c.lsn.Load())
	}
	c.raise(20)
	if c.lsn.Load() != 20 {
		t.Fatalf("not raised: %d", c.lsn.Load())
	}
	ctx := CausalContext(context.Background())
	if CausalContext(ctx) != ctx || causalFrom(ctx) == nil {
		t.Fatal("CausalContext replaced the floor it had")
	}
}

func TestATokenWithinTheTrackedPrimaryIsTakenAsItIs(t *testing.T) {
	f := newFakeTracker(t, Config{}, "")
	f.m.topo.primary.add(sample{at: f.now, lsn: 1000})
	c := &causal{}
	c.pending.Store(700)
	ctx := context.WithValue(context.Background(), causalKey{}, c)
	if got := f.m.floor(ctx); got != 700 {
		t.Fatalf("floor %d", got)
	}
	// Past the tracked position, and with no primary to ask: it stands,
	// which only costs the read a replica.
	c.pending.Store(5000)
	if got := f.m.floor(ctx); got != 5000 {
		t.Fatalf("floor %d", got)
	}
}

var (
	key  = strings.Repeat("k", 32)
	key2 = strings.Repeat("q", 32)
)

func TestTokensVerifyOnlyAsSigned(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	k := newKeyring(Config{TokenKey: key, TokenTTL: time.Minute})
	tok := k.sign(0x16B374D848, now)
	if len(tok) != tokenLen {
		t.Fatalf("token of %d characters", len(tok))
	}
	if lsn, res := k.verify(tok, now.Add(30*time.Second)); res != tokenAccepted || lsn != 0x16B374D848 {
		t.Fatalf("%x %s", lsn, res)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	flip := func(i int) string {
		b := append([]byte(nil), raw...)
		b[i] ^= 1
		return base64.RawURLEncoding.EncodeToString(b)
	}
	other := newKeyring(Config{TokenKey: key2, TokenTTL: time.Minute})
	for name, c := range map[string]struct {
		tok  string
		at   time.Time
		want string
	}{
		"expired":             {tok, now.Add(2 * time.Minute), tokenExpired},
		"from the far future": {tok, now.Add(-2 * time.Minute), tokenExpired},
		"another key":         {other.sign(1, now), now, tokenForged},
		"lsn changed":         {flip(5), now, tokenForged},
		"mac changed":         {flip(tokenBody + 3), now, tokenForged},
		"wrong version":       {flip(0), now, tokenVersioned},
		"truncated":           {tok[:40], now, tokenMalformed},
		"not base64":          {strings.Repeat("!", tokenLen), now, tokenMalformed},
		"empty":               {"", now, tokenMalformed},
		"oversize":            {strings.Repeat("a", maxTokenHeader+1), now, tokenOversize},
	} {
		if lsn, res := k.verify(c.tok, c.at); res != c.want || lsn != 0 {
			t.Errorf("%s: %x %s, want %s", name, lsn, res, c.want)
		}
	}
}

func TestAPreviousKeyStillVerifies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := newKeyring(Config{TokenKey: key, TokenTTL: time.Minute})
	rotated := newKeyring(Config{TokenKey: key2, TokenPreviousKey: key, TokenTTL: time.Minute})
	if _, res := rotated.verify(old.sign(42, now), now); res != tokenAccepted {
		t.Fatalf("a token of the previous key: %s", res)
	}
	if _, res := old.verify(rotated.sign(42, now), now); res != tokenForged {
		t.Fatalf("new tokens must be signed with the new key: %s", res)
	}
	if newKeyring(Config{}) != nil {
		t.Fatal("a keyring without a key")
	}
}

// A call's token comes in, is verified into the floor, and goes out only
// after a commit.
func TestTokensTravelThroughTheInterceptor(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}, TokenKey: key}, Clock(clock.NewFake(time.Unix(1_700_000_000, 0))))
	if err != nil {
		t.Fatal(err)
	}
	desc := &op.Descriptor{ID: "orders.get", Consistency: data.Any}
	call := func(in string, commit bool) (*op.Call, *causal, data.Level) {
		hdr := http.Header{}
		if in != "" {
			hdr.Set(ConsistencyHeader, in)
		}
		c := &op.Call{Desc: desc, Request: &op.Request{Header: hdr}, Response: &op.Response{Header: http.Header{}}}
		var seen *causal
		var level data.Level
		_, _ = m.track(context.Background(), c, func(ctx context.Context, _ *op.Call) (any, error) {
			seen = causalFrom(ctx)
			level, _ = data.ConsistencyFrom(ctx)
			if commit {
				seen.raise(777)
				seen.wrote.Store(true)
			}
			return nil, errs.New(errs.Internal, "x", "failed after the commit")
		})
		return c, seen, level
	}
	c, _, level := call("", false)
	if got := c.Response.Header.Get(ConsistencyHeader); got != "" {
		t.Fatalf("a call that committed nothing sent %q", got)
	}
	if level != data.Any {
		t.Fatalf("the operation's default is %v", level)
	}
	c, _, _ = call("", true)
	tok := c.Response.Header.Get(ConsistencyHeader)
	if tok == "" {
		t.Fatal("a call that committed, then failed, sent no token")
	}
	if cc := c.Response.Header.Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("a response with a token may be cached: Cache-Control %q", cc)
	}
	_, seen, _ := call(tok, false)
	if seen.pending.Load() != 777 {
		t.Fatalf("the token brought floor %d", seen.pending.Load())
	}
	_, seen, _ = call("forged."+tok, false)
	if seen.pending.Load() != 0 {
		t.Fatal("a bad token set a floor")
	}
	// Without a key, nothing is sent or read.
	m.keys = nil
	c, seen, _ = call(tok, true)
	if c.Response.Header.Get(ConsistencyHeader) != "" || seen.pending.Load() != 0 {
		t.Fatal("a Manager without a key handled tokens")
	}
}

func TestReplicaSettingsAreValidated(t *testing.T) {
	p := Source{DSN: "postgres://app:hunter2@db/orders"}
	r := Source{DSN: "postgres://app:hunter2@replica/orders"}
	ok := Config{Primary: p, Replicas: []Source{r, {DSN: r.DSN, Name: "eu", Zone: "eu-1"}}, Select: Nearest, Zone: "eu-1", TokenKey: key, TokenPreviousKey: key2, Leaks: leaksError}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Replicas[0].Name != "replica_1" || ok.Replicas[1].Name != "eu" {
		t.Fatalf("names %q %q", ok.Replicas[0].Name, ok.Replicas[1].Name)
	}
	for want, c := range map[string]Config{
		"replicas[0] (replica_1): dsn must be set":              {Primary: p, Replicas: []Source{{}}},
		`replicas[1]: name "a" is already another replica's`:    {Primary: p, Replicas: []Source{{DSN: r.DSN, Name: "a"}, {DSN: r.DSN, Name: "a"}}},
		`replicas[0]: name "primary" is the primary's`:          {Primary: p, Replicas: []Source{{DSN: r.DSN, Name: "primary"}}},
		`replicas[0]: name "Big" must be lower snake case`:      {Primary: p, Replicas: []Source{{DSN: r.DSN, Name: "Big"}}},
		`replicas[0]: name "other" is already a named source's`: {Primary: p, Named: map[string]Source{"other": p}, Replicas: []Source{{DSN: r.DSN, Name: "other"}}},
		"primary: name and zone apply to replicas only":         {Primary: Source{DSN: p.DSN, Zone: "a"}},
		"named \"x\": name and zone apply to replicas only":     {Primary: p, Named: map[string]Source{"x": {DSN: p.DSN, Zone: "a"}}},
		"select must be":                                         {Primary: p, Select: "fastest"},
		`select "nearest" needs zone`:                            {Primary: p, Select: Nearest},
		"max_lag must not be negative":                           {Primary: p, MaxLag: -1},
		"max_lag_bytes must not be negative":                     {Primary: p, MaxLagBytes: -1},
		"lag_poll must be at least 5ms":                          {Primary: p, LagPoll: time.Millisecond},
		"consistency: unknown read consistency":                  {Primary: p, Consistency: "eventually"},
		"token_key must be at least 32 bytes":                    {Primary: p, TokenKey: "short"},
		"token_previous_key is only accepted beside a token_key": {Primary: p, TokenPreviousKey: key},
		"token_previous_key must be at least 32 bytes":           {Primary: p, TokenKey: key, TokenPreviousKey: "short"},
		"token_ttl must not be negative":                         {Primary: p, TokenTTL: -1},
	} {
		err := c.Validate()
		switch {
		case err == nil:
			t.Errorf("%s: accepted", want)
		case !strings.Contains(err.Error(), want):
			t.Errorf("want %q in: %v", want, err)
		case strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "short") || strings.Contains(err.Error(), key):
			t.Errorf("%s: the error shows a secret: %v", want, err)
		}
	}
}

func TestReplicasPrintWithoutTheirDSN(t *testing.T) {
	c := Config{Primary: Source{DSN: "postgres://app:hunter2@db/orders"}, Replicas: []Source{{DSN: "postgres://app:hunter2@replica/orders", Zone: "a"}}, Leaks: leaksError}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := c.Replicas[0].String()
	if strings.Contains(s, "hunter2") || !strings.Contains(s, "name:replica_1 zone:a") || !strings.Contains(s, "@replica:5432/orders") {
		t.Fatalf("%s", s)
	}
}

// When the tracker stops hearing from the primary, its history no longer
// says a replica is caught up: only the time since the replica's last
// replayed commit bounds the lag, and without it the lag is unknown.
func TestAStalePrimaryHistoryNeverReadsAsNoLag(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: 2 * time.Second}, "")
	topo := f.m.topo
	topo.primary.add(sample{at: f.now.Add(-10 * time.Minute), lsn: 500})
	if _, lag := topo.lagOf(500, f.now, unknownLag); lag != unknownLag {
		t.Errorf("a replica at a 10-minute-old primary position, with no commit replayed: lag %v", lag)
	}
	if _, lag := topo.lagOf(500, f.now, 3*time.Second); lag != 3*time.Second {
		t.Errorf("with a commit replayed 3s ago: lag %v", lag)
	}
	topo.replicas[0].reading.Store(&reading{at: f.now, healthy: true, replay: 500, lag: unknownLag})
	for _, l := range []data.Level{data.Any, data.BoundedStaleness(time.Hour)} {
		if got, _ := f.where(context.Background(), l); got != "primary" {
			t.Errorf("%v read a replica of unknown lag", l)
		}
	}

	// A fresh history of an idle primary: caught up, however long ago the
	// last commit was.
	topo.primary.add(sample{at: f.now.Add(-10 * time.Millisecond), lsn: 500})
	if _, lag := topo.lagOf(500, f.now, time.Hour); lag != 10*time.Millisecond {
		t.Errorf("caught up with an idle primary sampled 10ms ago: lag %v", lag)
	}
	// Behind it, the smaller bound wins.
	topo.primary.add(sample{at: f.now, lsn: 900})
	if _, lag := topo.lagOf(500, f.now, time.Second); lag != 10*time.Millisecond {
		t.Errorf("behind: lag %v", lag)
	}
}

func TestBoundedStalenessCountsTheReadingsAge(t *testing.T) {
	f := newFakeTracker(t, Config{}, "")
	f.set(0, 100, 50*time.Millisecond)
	f.now = f.now.Add(60 * time.Millisecond)
	if got, _ := f.where(context.Background(), data.BoundedStaleness(100*time.Millisecond)); got != "primary" {
		t.Errorf("50ms of lag in a reading 60ms old is past 100ms, yet it read %s", got)
	}
	if got, _ := f.where(context.Background(), data.BoundedStaleness(200*time.Millisecond)); got != "replica_1" {
		t.Errorf("but within 200ms: read %s", got)
	}
}

// A replica caught up with the primary's latest sample is behind by as
// much as that sample is old: the primary may have written since.
func TestAReplicaCaughtUpWithAnOldSampleIsAsFarBehindAsItsAge(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: time.Second}, "")
	topo := f.m.topo
	topo.primary.add(sample{at: f.now.Add(-400 * time.Millisecond), lsn: 1000})
	bytes, lag := topo.lagOf(1000, f.now, unknownLag)
	if bytes != 0 || lag != 400*time.Millisecond {
		t.Fatalf("lag %d bytes, %v", bytes, lag)
	}
	topo.replicas[0].reading.Store(&reading{at: f.now, healthy: true, replay: 1000, lagBytes: bytes, lag: lag})
	if got, _ := f.where(context.Background(), data.BoundedStaleness(100*time.Millisecond)); got != data.PrimarySource {
		t.Errorf("a bound of 100ms read %s, at most 400ms behind", got)
	}
	if got, _ := f.where(context.Background(), data.BoundedStaleness(time.Second)); got != "replica_1" {
		t.Errorf("a bound of 1s read %s", got)
	}
}

func TestNoBoundedStalenessOfZeroReadsAReplica(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: time.Second}, "")
	f.set(0, 1000, 0) // as caught up as a reading can say, taken now
	if got, _ := f.where(context.Background(), data.BoundedStaleness(0)); got != data.PrimarySource {
		t.Errorf("BoundedStaleness(0) read %s", got)
	}
}

// Without a fresh reading of the primary, a replica's lag in bytes is
// unknown, and MaxLagBytes keeps it out.
func TestAStalePrimaryHistoryNeverReadsAsNoLagInBytes(t *testing.T) {
	f := newFakeTracker(t, Config{MaxLag: time.Hour, MaxLagBytes: 1 << 10}, "")
	topo := f.m.topo
	if bytes, _ := topo.lagOf(1000, f.now, 500*time.Millisecond); bytes != unknownBytes {
		t.Errorf("no history: %d bytes", bytes)
	}
	topo.primary.add(sample{at: f.now.Add(-time.Minute), lsn: 1 << 40})
	bytes, lag := topo.lagOf(1000, f.now, 500*time.Millisecond)
	if bytes != unknownBytes {
		t.Errorf("a minute-old history: %d bytes", bytes)
	}
	topo.replicas[0].reading.Store(&reading{at: f.now, healthy: true, replay: 1000, lagBytes: bytes, lag: lag})
	if got, _ := f.where(context.Background(), data.Any); got != data.PrimarySource {
		t.Errorf("Any read %s, of unknown lag in bytes", got)
	}
	// Without MaxLagBytes, the time bound alone decides.
	topo.maxBytes = 0
	if got, _ := f.where(context.Background(), data.Any); got != "replica_1" {
		t.Errorf("without MaxLagBytes, Any read %s", got)
	}
}

// Stop clears the readings while reads choose by them; a read never
// trips over a reading that went away after it was chosen.
func TestReadsRouteSafelyWhileTheTrackerStops(t *testing.T) {
	for _, sel := range []Selection{LeastLag, Random, RoundRobin, Nearest} {
		f := newFakeTracker(t, Config{Select: sel, Zone: "a"}, "a", "a", "b")
		topo := f.m.topo
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 200 {
				for i := range topo.replicas {
					f.set(i, 100, time.Duration(i)*time.Millisecond)
				}
				topo.life.Lock()
				topo.cancel = func() {} // as if started
				topo.life.Unlock()
				topo.stop()
			}
		}()
		for {
			select {
			case <-done:
			default:
				for _, l := range []data.Level{data.Any, data.ReadYourWrites, data.BoundedStaleness(time.Second)} {
					if _, err := f.m.route(context.Background(), l); err != nil {
						t.Fatal(err)
					}
				}
				continue
			}
			break
		}
	}
}

func TestAWriteWhosePositionIsUnknownSendsTheRequestsReadsToThePrimary(t *testing.T) {
	f := newFakeTracker(t, Config{TokenKey: key}, "")
	f.set(0, 1<<40, 0)
	c := &causal{}
	c.raise(unknownFloor)
	c.wrote.Store(true)
	ctx := context.WithValue(context.Background(), causalKey{}, c)
	rt, err := f.m.route(ctx, data.ReadYourWrites.Wait(time.Minute).OrFail())
	if err != nil || rt.rep != nil {
		t.Fatalf("routed to %v, %v; want the primary, without failing or waiting", rt.rep, err)
	}

	// It still sends a token, and the next request that brings it, when the
	// primary can't be asked for its position, reads the primary too.
	call := &op.Call{Response: &op.Response{Header: http.Header{}}}
	f.m.giveToken(call, c)
	tok := call.Response.Header.Get(ConsistencyHeader)
	if tok == "" {
		t.Fatal("a call that committed a write of unknown position sent no token")
	}
	next := &causal{}
	f.m.takeToken(context.Background(), &op.Call{Desc: &op.Descriptor{ID: "next"}, Request: &op.Request{Header: http.Header{ConsistencyHeader: {tok}}}}, next)
	if next.pending.Load() != unknownFloor {
		t.Fatalf("the next request's pending floor is %d", next.pending.Load())
	}
	rt, err = f.m.route(context.WithValue(context.Background(), causalKey{}, next), data.ReadYourWrites.Wait(time.Minute).OrFail())
	if err != nil || rt.rep != nil {
		t.Fatalf("the next request routed to %v, %v; want the primary", rt.rep, err)
	}
}

// A floor that is already unknown can't rise, so a token is not worth a
// query of the primary.
func TestAnUnknownFloorAsksThePrimaryNothing(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}})
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	cfg := m.sources[data.PrimarySource].config.Copy()
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
		dials.Add(1)
		return errors.New("no primary here")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m.sources[data.PrimarySource].pool.Store(pool)

	c := &causal{}
	c.raise(unknownFloor)
	c.pending.Store(42)
	if got := m.floor(context.WithValue(context.Background(), causalKey{}, c)); got != unknownFloor || c.pending.Load() != 0 {
		t.Fatalf("floor %d, pending %d", got, c.pending.Load())
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the primary was asked %d times", n)
	}
	// While a known floor's token is clamped by asking it.
	c = &causal{}
	c.pending.Store(42)
	m.floor(context.WithValue(context.Background(), causalKey{}, c))
	if dials.Load() == 0 {
		t.Fatal("the test asks nothing of the primary either way")
	}
}

func TestTheFloorNeedsNoTopology(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &causal{}
	c.pending.Store(42)
	// Not started: the primary can't be asked, and the token stands.
	if got := m.floor(context.WithValue(context.Background(), causalKey{}, c)); got != 42 {
		t.Fatalf("floor %d", got)
	}
}

// A token's response is never left for a shared cache: the call's own
// Cache-Control stands only if it has private or no-store.
func TestATokensResponseIsNeverSharedCacheable(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}, TokenKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		set  []string
		want string
	}{
		// Kept: they keep shared caches out.
		{[]string{"private, max-age=10"}, "private, max-age=10"},
		{[]string{"no-store"}, "no-store"},
		{[]string{"max-age=0", "No-Store"}, "max-age=0"}, // Get reads the first; both stay
		// Replaced: a shared cache may store them under RFC 9111.
		{nil, "private, no-store"},
		{[]string{"max-age=60"}, "private, no-store"},
		{[]string{"no-cache"}, "private, no-store"},
		{[]string{"must-revalidate, max-age=60"}, "private, no-store"},
		{[]string{"max-age=31536000, immutable"}, "private, no-store"},
		{[]string{"PUBLIC"}, "private, no-store"},
		{[]string{"public, max-age=60"}, "private, no-store"},
		{[]string{"max-age=60", " S-MaxAge=60"}, "private, no-store"},
		{[]string{`private="Set-Cookie", max-age=60`}, "private, no-store"},
	} {
		for _, how := range []string{"wrote", "reissued"} {
			cs := &causal{}
			cs.raise(7)
			if how == "wrote" {
				cs.wrote.Store(true)
			} else {
				cs.reissue.Store(true)
			}
			h := http.Header{}
			if c.set != nil {
				h["Cache-Control"] = c.set
			}
			m.giveToken(&op.Call{Response: &op.Response{Header: h}}, cs)
			kept := c.want != "private, no-store"
			if h.Get(ConsistencyHeader) == "" || h.Get("Cache-Control") != c.want || (!kept && len(h.Values("Cache-Control")) != 1) || (kept && len(h.Values("Cache-Control")) != len(c.set)) {
				t.Errorf("%s, Cache-Control %q: %q, token %q", how, c.set, h.Values("Cache-Control"), h.Get(ConsistencyHeader))
			}
		}
	}
}

// Any token the clamp lowers is reissued at the clamped floor; one it
// leaves isn't. It needs a Postgres to clamp against.
func TestAClampThatLowersATokenReissuesIt(t *testing.T) {
	dsn, _ := ownDatabase(t, false)
	m, err := New(Config{Primary: Source{DSN: dsn}, TokenKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	cur, err := m.primaryLSN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tok     uint64
		reissue bool
	}{{cur + 1<<30, true}, {unknownFloor, true}, {cur, false}, {1, false}} {
		cs := &causal{}
		cs.pending.Store(c.tok)
		got := m.floor(context.WithValue(context.Background(), causalKey{}, cs))
		if cs.reissue.Load() != c.reissue || got > max(c.tok, cur) || (c.reissue && got > cur+1<<20) {
			t.Errorf("a token at %d: floor %d, reissued %v", c.tok, got, cs.reissue.Load())
		}
	}
}

func TestInsertPositionsJustPastAPageHeaderMoveBackToTheBoundary(t *testing.T) {
	const block, seg = 8192, 16 << 20
	w := &walLayout{block: block, seg: seg, short: 24, long: 40}
	for _, c := range []struct {
		what     string
		lsn, end uint64
	}{
		{"past a page's short header", 5*seg + 3*block + 24, 5*seg + 3*block},
		{"past a segment's long header", 5*seg + 40, 5 * seg},
		{"past the first segment's long header", 40, 0},
		{"mid-page", 5*seg + 3*block + 1000, 5*seg + 3*block + 1000},
		{"at a page boundary", 5*seg + 3*block, 5*seg + 3*block},
		{"24 into a segment's first page, inside its long header", 5*seg + 24, 5*seg + 24},
		{"40 into a page other than a segment's first", 5*seg + 3*block + 40, 5*seg + 3*block + 40},
	} {
		if got := w.recordEnd(c.lsn); got != c.end {
			t.Errorf("%s: %d is %d, want %d", c.what, c.lsn, got, c.end)
		}
	}
	// A 32-bit build aligns the headers to 4.
	w = &walLayout{block: block, seg: seg, short: 20, long: 36}
	if got := w.recordEnd(3*block + 20); got != 3*block {
		t.Errorf("a short header of 20: %d", got)
	}
	// Without a layout, positions are as they are.
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.atRecordEnd(3*block + 24); got != 3*block+24 {
		t.Errorf("without a layout: %d", got)
	}
}

func TestAClampedUnknownTokenIsReissued(t *testing.T) {
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}, TokenKey: key})
	if err != nil {
		t.Fatal(err)
	}
	c := &causal{}
	c.raise(1234)
	call := &op.Call{Response: &op.Response{Header: http.Header{}}}
	m.giveToken(call, c)
	if call.Response.Header.Get(ConsistencyHeader) != "" {
		t.Fatal("a call that neither wrote nor clamped an unknown token sent one")
	}
	c.reissue.Store(true)
	m.giveToken(call, c)
	tok := call.Response.Header.Get(ConsistencyHeader)
	if lsn, result := m.keys.verify(tok, m.clock.Now()); result != tokenAccepted || lsn != 1234 {
		t.Fatalf("reissued %d, %s", lsn, result)
	}
}

func TestLogsSayUnknownLagIsUnknown(t *testing.T) {
	rd := &reading{lag: unknownLag, lagBytes: unknownBytes}
	if lagAttr(rd) != "unknown" || lagBytesAttr(rd) != "unknown" {
		t.Errorf("%v, %v", lagAttr(rd), lagBytesAttr(rd))
	}
	rd = &reading{lag: time.Second, lagBytes: 10}
	if lagAttr(rd) != time.Second || lagBytesAttr(rd) != int64(10) {
		t.Errorf("%v, %v", lagAttr(rd), lagBytesAttr(rd))
	}
}

func TestTheWALLayoutIsReadOnlyWhenItIsWholeAndSane(t *testing.T) {
	w, err := parseWALLayout([][]byte{[]byte("8192"), []byte("16777216"), []byte("8")})
	if err != nil || w != (walLayout{block: 8192, seg: 16 << 20, short: 24, long: 40}) {
		t.Fatalf("%+v, %v", w, err)
	}
	if w, err := parseWALLayout([][]byte{[]byte("8192"), []byte("16777216"), []byte("4")}); err != nil || w.short != 20 || w.long != 36 {
		t.Fatalf("aligned to 4: %+v, %v", w, err)
	}
	for what, row := range map[string][][]byte{
		"no row":              nil,
		"no alignment":        {[]byte("8192"), []byte("16777216")},
		"a null alignment":    {[]byte("8192"), []byte("16777216"), nil},
		"a zero page":         {[]byte("0"), []byte("16777216"), []byte("8")},
		"segments of no page": {[]byte("8192"), []byte("10000"), []byte("8")},
		"an odd alignment":    {[]byte("8192"), []byte("16777216"), []byte("6")},
		"not a number":        {[]byte("8kB"), []byte("16777216"), []byte("8")},
	} {
		if _, err := parseWALLayout(row); err == nil {
			t.Errorf("%s: a layout", what)
		}
	}
}

// The primary's poller reads the WAL layout, and tries again until it can;
// without replicas nothing reads it. It needs a primary and a replica.
func TestThePrimarysPollerReadsTheWALLayoutUntilItCan(t *testing.T) {
	// pg_control_init's grant is the database's own: the test's database
	// keeps the change to itself.
	dsn, rep := ownDatabase(t, true)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	alone, err := New(Config{Primary: Source{DSN: dsn}})
	if err != nil {
		t.Fatal(err)
	}
	if err := alone.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alone.Stop(ctx) })

	// A role that may not call pg_control_init gets no layout, until it may.
	role := fmt.Sprintf("galleon_layout_%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE ROLE " + role + " LOGIN", "REVOKE EXECUTE ON FUNCTION pg_control_init() FROM PUBLIC"} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	granted := false
	grant := func() {
		if !granted {
			granted = true
			if _, err := admin.Exec(ctx, "GRANT EXECUTE ON FUNCTION pg_control_init() TO PUBLIC"); err != nil {
				t.Errorf("restoring pg_control_init: %v", err)
			}
		}
	}
	t.Cleanup(func() { grant(); _, _ = admin.Exec(context.Background(), "DROP ROLE "+role) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	m, err := New(Config{Primary: Source{DSN: u.String()}, Replicas: []Source{{DSN: rep}}, LagPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m.topo.layoutRetry = 20 * time.Millisecond
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })
	// Start tried the layout and couldn't; whatever the poller has tried
	// since, by its second sample, couldn't either.
	deadline := time.Now().Add(30 * time.Second)
	for {
		m.topo.primary.mu.Lock()
		n := len(m.topo.primary.samples)
		m.topo.primary.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the primary's poller took no two samples")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.wal.Load() != nil {
		t.Fatal("a layout without pg_control_init")
	}
	grant()
	deadline = time.Now().Add(30 * time.Second)
	for m.wal.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the poller never read the layout once it could")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w := m.wal.Load(); w.block == 0 || w.short == 0 {
		t.Fatalf("%+v", w)
	}
	if alone.wal.Load() != nil {
		t.Fatal("a Manager without replicas read the layout")
	}
}

// ownDatabase creates a database of the test's own on GALLEON_TEST_POSTGRES,
// dropped when it ends, and returns its DSN, and with replica, the DSN of
// the same database on GALLEON_TEST_POSTGRES_REPLICA once the replica has
// it. It skips the test without them.
func ownDatabase(t *testing.T, replica bool) (string, string) {
	t.Helper()
	admin, rep := os.Getenv("GALLEON_TEST_POSTGRES"), os.Getenv("GALLEON_TEST_POSTGRES_REPLICA")
	if admin == "" || (replica && rep == "") {
		t.Skip("GALLEON_TEST_POSTGRES, and for this test GALLEON_TEST_POSTGRES_REPLICA, are not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("galleon_pg_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	in := func(dsn string) string {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		return u.String()
	}
	if !replica {
		return in(admin), ""
	}
	repDSN := in(rep)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := pgx.Connect(ctx, repDSN)
		if err == nil {
			_ = c.Close(ctx)
			return in(admin), repDSN
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replica never had the test's database: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// foreignLevel is a read consistency of another data module, which passes
// the op plane's checks.
type foreignLevel struct{}

func (foreignLevel) ConsistencyLevel() string { return "foreign" }
func (foreignLevel) Validate() error          { return nil }

func TestACallWhoseOperationDeclaresAForeignConsistencyFails(t *testing.T) {
	var logs strings.Builder
	m, err := New(Config{Primary: Source{DSN: "postgres://app@primary/db"}}, Logger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	lvl := data.Any
	for _, c := range []op.ReadConsistency{foreignLevel{}, &lvl} {
		d := &op.Descriptor{ID: "orders.list", Consistency: c}
		for range 2 {
			ran := false
			_, err := m.track(context.Background(), &op.Call{Desc: d, Request: &op.Request{Header: http.Header{}}}, func(context.Context, *op.Call) (any, error) {
				ran = true
				return nil, nil
			})
			if e := errs.From(err); err == nil || e.Kind != errs.Internal || e.Code != data.CodeInvalidConsistency || ran {
				t.Errorf("%T: %v, ran %v", c, err, ran)
			}
		}
	}
	if n := strings.Count(logs.String(), "declares a read consistency of type"); n != 1 {
		t.Errorf("logged %d times, want once for the operation:\n%s", n, logs.String())
	}
	// A data.Level reaches the call's reads.
	d := &op.Descriptor{ID: "orders.get", Consistency: data.ReadYourWrites}
	if _, err := m.track(context.Background(), &op.Call{Desc: d, Request: &op.Request{Header: http.Header{}}}, func(ctx context.Context, _ *op.Call) (any, error) {
		if l, ok := data.ConsistencyFrom(ctx); !ok || l != data.ReadYourWrites {
			t.Errorf("the call's reads default to %v, %v", l, ok)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}
