package datatest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/testkit"
)

// ConsistencyHeader is the header causal tokens travel in.
const ConsistencyHeader = "Galleon-Consistency"

// tokenKey is the key the suite has the module sign tokens with.
const tokenKey = "datatest causal token key, 32 bytes or more"

// Row names a row of the test's table, and the read consistency to read
// it at, as data.ParseLevel reads it.
type Row struct {
	ID    int
	Level string
}

// Seen is what a read found.
type Seen struct {
	Found bool
	// OnReplica is whether the read ran on a replica.
	OnReplica bool
}

// readRow reads row id through r, and whether r runs on a replica.
func (e *env) readRow(ctx context.Context, r data.Reader, id int) (Seen, error) {
	var s Seen
	err := r.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+e.table+" WHERE id = $1), pg_is_in_recovery()", id).Scan(&s.Found, &s.OnReplica)
	return s, err
}

func (e *env) read(ctx context.Context, id int, opts ...data.ReadOption) (Seen, error) {
	var s Seen
	err := e.m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		var err error
		s, err = e.readRow(ctx, r, id)
		return err
	}, opts...)
	return s, err
}

var errAfterCommit = errors.New("failed after committing")

// tokenOps are operations that write, read and fail in the ways that
// decide whether a call sends a causal token.
func tokenOps(e **env) galleon.Module {
	return galleon.ModuleFunc("datatest_tokens", func(r *galleon.Registry) error {
		write := func(ctx context.Context, id int) error {
			return (*e).m.Write(ctx, func(ctx context.Context, w data.WriteTx) error { return (*e).insert(ctx, w, id) })
		}
		r.Operation(op.Unary[Row, Empty]("datatest.write").Public(), func(ctx context.Context, in *Row) (*Empty, error) {
			return &Empty{}, write(ctx, in.ID)
		})
		r.Operation(op.Unary[Row, Empty]("datatest.write_then_fail").Public(), func(ctx context.Context, in *Row) (*Empty, error) {
			if err := write(ctx, in.ID); err != nil {
				return nil, err
			}
			return nil, errAfterCommit
		})
		r.Operation(op.Unary[Row, Empty]("datatest.rolled_back").Public(), func(ctx context.Context, in *Row) (*Empty, error) {
			err := (*e).m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				if err := (*e).insert(ctx, w, in.ID); err != nil {
					return err
				}
				return errUndo
			})
			return nil, err
		})
		r.Operation(op.Unary[Row, Empty]("datatest.write_nothing").Public(), func(ctx context.Context, in *Row) (*Empty, error) {
			return &Empty{}, (*e).m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				_, err := (*e).readRow(ctx, w, in.ID)
				return err
			})
		})
		r.Operation(op.Unary[Row, Seen]("datatest.read").Public().Consistency(data.ReadYourWrites), func(ctx context.Context, in *Row) (*Seen, error) {
			s, err := (*e).read(ctx, in.ID)
			return &s, err
		})
		r.Operation(op.Unary[Row, Seen]("datatest.read_any").Public().Consistency(data.Any), func(ctx context.Context, in *Row) (*Seen, error) {
			s, err := (*e).read(ctx, in.ID)
			return &s, err
		})
		readAt := func(ctx context.Context, in *Row) (*Seen, error) {
			l, err := data.ParseLevel(in.Level)
			if err != nil {
				return nil, err
			}
			s, err := (*e).read(ctx, in.ID, data.Consistency(l))
			return &s, err
		}
		r.Operation(op.Unary[Row, Seen]("datatest.read_at").Public(), readAt)
		r.Operation(op.Unary[Row, Seen]("datatest.write_then_read").Public(), func(ctx context.Context, in *Row) (*Seen, error) {
			if err := write(ctx, in.ID); err != nil {
				return nil, err
			}
			return readAt(ctx, in)
		})
		return nil
	})
}

// invoke calls an operation through its whole pipeline, as a transport
// does, with request headers in and response headers out.
func (e *env) invoke(ctx context.Context, id string, in any, hdr http.Header) (any, http.Header, error) {
	e.t.Helper()
	x := testkit.Get[*galleon.Operations](e.h).Lookup(id)
	if x == nil {
		e.t.Fatalf("no operation %q", id)
	}
	if hdr == nil {
		hdr = http.Header{}
	}
	call := &op.Call{Input: in, Request: &op.Request{Transport: "test", Header: hdr}, Response: &op.Response{Header: http.Header{}}}
	out, err := x.Invoke(ctx, call)
	return out, call.Response.Header, err
}

func withToken(tok string) http.Header {
	h := http.Header{}
	h.Set(ConsistencyHeader, tok)
	return h
}

func consistencyWithoutReplicas(t *testing.T, f Factory) {
	var e *env
	e = newEnv(t, f, Setup{}, galleon.Modules(tokenOps(&e)))
	c := ctx(t)
	e.exec("INSERT INTO " + e.table + " (id, v) VALUES (1, 'x')")
	levels := []data.Level{data.Primary, data.Any, data.BoundedStaleness(0), data.BoundedStaleness(time.Hour), data.ReadYourWrites,
		data.ReadYourWrites.Wait(time.Second), data.ReadYourWrites.OrFail(), data.ReadYourWrites.Wait(time.Second).OrFail()}
	for _, l := range levels {
		s, err := e.read(c, 1, data.Consistency(l))
		if err != nil || !s.Found || s.OnReplica {
			t.Errorf("%v without replicas: %+v, %v", l, s, err)
		}
		r, err := e.m.BeginRead(c, data.Consistency(l))
		if err != nil {
			t.Fatalf("%v, pair form: %v", l, err)
		}
		if s, err := e.readRow(c, r, 1); err != nil || !s.Found {
			t.Errorf("%v, pair form: %+v, %v", l, s, err)
		}
		if err := r.Commit(c); err != nil {
			t.Fatal(err)
		}
	}

	// A read inside a write transaction stays on it, whatever it asks for.
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		e.mustInsert(ctx, w, 2)
		for _, l := range []data.Level{data.Any, data.BoundedStaleness(time.Hour), data.ReadYourWrites} {
			for _, p := range []data.Propagation{data.Join, data.Nested} {
				r, err := e.m.BeginRead(ctx, data.Consistency(l), p)
				if err != nil {
					return err
				}
				if s, err := e.readRow(ctx, r, 2); err != nil || !s.Found || s.OnReplica {
					t.Errorf("a %v read with %v in a write transaction: %+v, %v", p, l, s, err)
				}
				if err := r.Commit(ctx); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// An invalid level, from the call or from ctx, fails the read; it never
	// reads at another level.
	for _, l := range []data.Level{{}, data.BoundedStaleness(-time.Second), data.Any.Wait(time.Second)} {
		_, err := e.m.BeginRead(c, data.Consistency(l))
		wantCode(t, err, errs.InvalidArgument, data.CodeInvalidConsistency)
		err = e.m.Read(c, func(context.Context, data.ReadTx) error { return nil }, data.Consistency(l))
		wantCode(t, err, errs.InvalidArgument, data.CodeInvalidConsistency)
		lc := data.WithConsistency(c, l)
		_, err = e.m.BeginRead(lc)
		wantCode(t, err, errs.InvalidArgument, data.CodeInvalidConsistency)
		err = e.m.Read(lc, func(context.Context, data.ReadTx) error { return nil })
		wantCode(t, err, errs.InvalidArgument, data.CodeInvalidConsistency)
	}

	// An operation's default consistency applies to its reads.
	for _, id := range []string{"datatest.read", "datatest.read_any"} {
		out, _, err := e.invoke(c, id, &Row{ID: 1}, nil)
		if err != nil || !out.(*Seen).Found {
			t.Errorf("%s: %v, %v", id, out, err)
		}
	}
}

func causalTokens(t *testing.T, f Factory) {
	var e *env
	e = newEnv(t, f, Setup{TokenKey: tokenKey}, galleon.Modules(tokenOps(&e)))
	c := ctx(t)

	_, hdr, err := e.invoke(c, "datatest.write", &Row{ID: 1}, nil)
	tok := hdr.Get(ConsistencyHeader)
	if err != nil || tok == "" {
		t.Fatalf("a call that committed a write sent token %q, %v", tok, err)
	}
	if len(tok) > 64 || strings.Contains(tok, "/") {
		t.Errorf("token %q isn't a short, opaque, header-safe string", tok)
	}
	_, hdr, err = e.invoke(c, "datatest.write_then_fail", &Row{ID: 2}, nil)
	if !errors.Is(err, errAfterCommit) && errs.KindOf(err) != errs.Internal {
		t.Fatalf("write_then_fail: %v", err)
	}
	if hdr.Get(ConsistencyHeader) == "" {
		t.Error("a call that committed, then failed, sent no token")
	}
	for _, id := range []string{"datatest.rolled_back", "datatest.write_nothing", "datatest.read"} {
		_, hdr, _ := e.invoke(c, id, &Row{ID: 3}, nil)
		if got := hdr.Get(ConsistencyHeader); got != "" {
			t.Errorf("%s committed no write, but sent token %q", id, got)
		}
	}

	// The next request brings the token back, and reads what the first
	// wrote. A token that fails verification only costs it the floor.
	for name, h := range map[string]http.Header{
		"its token":  withToken(tok),
		"no token":   nil,
		"garbage":    withToken("not a token"),
		"tampered":   withToken(tok[:len(tok)-2] + "AA"),
		"oversize":   withToken(strings.Repeat("A", 64<<10)),
		"two tokens": {ConsistencyHeader: {tok, "x"}},
	} {
		out, _, err := e.invoke(c, "datatest.read", &Row{ID: 1}, h)
		if err != nil || !out.(*Seen).Found {
			t.Errorf("a read with %s: %v, %v", name, out, err)
		}
	}
}

// readYourWritesOnALaggingReplica: a request reads what it, or a request
// whose token it carries, wrote, though the replica hasn't replayed it.
func readYourWritesOnALaggingReplica(t *testing.T, f Factory) {
	var e *env
	e = newEnvWith(t, f, Setup{TokenKey: tokenKey}, true, galleon.Modules(tokenOps(&e)))
	c := ctx(t)
	e.exec("INSERT INTO " + e.table + " (id, v) VALUES (1, 'x')")
	e.replica.CatchUp()
	until(t, "an Any read to run on the replica", func() bool {
		s, err := e.read(c, 1, data.Consistency(data.Any))
		return err == nil && s.OnReplica && s.Found
	})

	e.replica.Pause()
	_, hdr, err := e.invoke(c, "datatest.write", &Row{ID: 2}, nil)
	tok := hdr.Get(ConsistencyHeader)
	if err != nil || tok == "" {
		t.Fatalf("write: %q, %v", tok, err)
	}
	if out, _, err := e.invoke(c, "datatest.read_any", &Row{ID: 2}, withToken(tok)); err != nil || out.(*Seen).Found || !out.(*Seen).OnReplica {
		t.Fatalf("the replica is meant to lag: an Any read found %+v, %v", out, err)
	}
	if out, _, err := e.invoke(c, "datatest.read", &Row{ID: 2}, withToken(tok)); err != nil || !out.(*Seen).Found {
		t.Fatalf("read-your-writes on a lagging replica, across requests: %+v, %v", out, err)
	}
	if out, _, err := e.invoke(c, "datatest.read", &Row{ID: 2}, nil); err != nil || out.(*Seen).Found {
		// Without the token there is no floor, and the replica serves.
		t.Fatalf("a request without the token: %+v, %v", out, err)
	}

	// Within one request: its own write, on the lagging replica.
	if out, _, err := e.invoke(c, "datatest.write_then_read", &Row{ID: 3, Level: "read_your_writes"}, nil); err != nil || !out.(*Seen).Found {
		t.Fatalf("a request reads its own write: %+v, %v", out, err)
	}
	// A wait that runs out fails, or reads the primary.
	_, _, err = e.invoke(c, "datatest.read_at", &Row{ID: 2, Level: "read_your_writes(wait 100ms, or fail)"}, withToken(tok))
	wantCode(t, err, errs.Unavailable, data.CodeConsistencyTimeout)
	if out, _, err := e.invoke(c, "datatest.read_at", &Row{ID: 2, Level: "read_your_writes(wait 50ms)"}, withToken(tok)); err != nil || !out.(*Seen).Found || out.(*Seen).OnReplica {
		t.Fatalf("a wait that runs out reads the primary: %+v, %v", out, err)
	}

	// A wait that the replica ends by catching up reads the replica.
	type result struct {
		out any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, _, err := e.invoke(c, "datatest.read_at", &Row{ID: 2, Level: "read_your_writes(wait 30s)"}, withToken(tok))
		done <- result{out, err}
	}()
	e.replica.Resume()
	r := <-done
	if r.err != nil || !r.out.(*Seen).Found || !r.out.(*Seen).OnReplica {
		t.Fatalf("waiting for the replica: %+v, %v", r.out, r.err)
	}
}

// until polls cond until it holds, failing the test after 30 seconds.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 30s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
