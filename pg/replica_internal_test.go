package pg

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

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
	if l := m.levelFor(ctx, data.ReadOptions()); l != data.Any {
		t.Errorf("config: %v", l)
	}
	octx := data.WithConsistency(ctx, data.BoundedStaleness(time.Second))
	if l := m.levelFor(octx, data.ReadOptions()); l != data.BoundedStaleness(time.Second) {
		t.Errorf("operation: %v", l)
	}
	if l := m.levelFor(octx, data.ReadOptions(data.Consistency(data.Primary))); l != data.Primary {
		t.Errorf("call: %v", l)
	}
}

func TestLagIsZeroForACaughtUpReplicaOfAnIdlePrimary(t *testing.T) {
	var h history
	t0 := time.Unix(0, 0)
	if _, lag := h.lag(10, t0); lag != unknownLag {
		t.Fatal("without samples the lag is known")
	}
	// The primary sat at 100 for a minute: the replica at 100 is caught
	// up, however long ago its last replayed commit was.
	for i := range 60 {
		h.add(sample{at: t0.Add(time.Duration(i) * time.Second), lsn: 100})
	}
	if b, lag := h.lag(100, t0.Add(time.Minute)); b != 0 || lag != 0 {
		t.Errorf("caught up: %d bytes, %v", b, lag)
	}
	// The primary moved to 200 at 60s and 300 at 61s; a replica at 250
	// was last where the primary was at 60s.
	h.add(sample{at: t0.Add(60 * time.Second), lsn: 200})
	h.add(sample{at: t0.Add(61 * time.Second), lsn: 300})
	if b, lag := h.lag(250, t0.Add(62*time.Second)); b != 50 || lag != 2*time.Second {
		t.Errorf("behind: %d bytes, %v", b, lag)
	}
	if _, lag := h.lag(50, t0.Add(62*time.Second)); lag != unknownLag {
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
