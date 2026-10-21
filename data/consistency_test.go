package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/thec1oud/galleon/data"
)

func TestLevelsReadBackAsTheyPrint(t *testing.T) {
	for _, l := range []data.Level{
		data.Primary, data.Any, data.ReadYourWrites, data.BoundedStaleness(0), data.BoundedStaleness(1500 * time.Millisecond),
		data.ReadYourWrites.Wait(200 * time.Millisecond), data.ReadYourWrites.OrFail(), data.ReadYourWrites.Wait(time.Second).OrFail(),
	} {
		got, err := data.ParseLevel(l.String())
		if err != nil || got != l {
			t.Errorf("%s read back as %v, %v", l, got, err)
		}
		if l.ConsistencyLevel() != l.String() {
			t.Errorf("%s names itself %q", l, l.ConsistencyLevel())
		}
	}
	if l, _ := data.ParseLevel("  bounded_staleness( 2s ) "); l != data.BoundedStaleness(2*time.Second) {
		t.Errorf("spaces: %v", l)
	}
}

func TestBadLevelsAreRefused(t *testing.T) {
	for _, s := range []string{"", "fast", "primary(1s)", "bounded_staleness", "bounded_staleness(soon)", "bounded_staleness(-1s)",
		"read_your_writes(wait)", "read_your_writes(wait -1s)", "read_your_writes(or else)", "any(", "read_your_writes(wait 1s"} {
		if l, err := data.ParseLevel(s); err == nil {
			t.Errorf("%q parsed as %v", s, l)
		}
	}
	for _, l := range []data.Level{{}, data.BoundedStaleness(-time.Second), data.ReadYourWrites.Wait(-time.Second),
		data.Any.Wait(time.Second), data.Primary.OrFail(), data.BoundedStaleness(time.Second).OrFail()} {
		if l.Validate() == nil {
			t.Errorf("%v is valid", l)
		}
	}
}

func TestLevelsExposeTheirParameters(t *testing.T) {
	l := data.ReadYourWrites.Wait(300 * time.Millisecond).OrFail()
	if l.Kind() != data.ReadYourWritesKind || l.MaxWait() != 300*time.Millisecond || !l.FailsOnTimeout() {
		t.Fatalf("%v", l)
	}
	if b := data.BoundedStaleness(time.Second); b.Kind() != data.BoundedStalenessKind || b.Staleness() != time.Second {
		t.Fatalf("%v", b)
	}
	if data.ReadYourWrites.FailsOnTimeout() || data.ReadYourWrites.MaxWait() != 0 {
		t.Fatal("ReadYourWrites must read the primary at once by default")
	}
}

func TestConsistencyIsAReadOption(t *testing.T) {
	o := data.ReadOptions(data.Consistency(data.Any), data.Independent)
	if o.Consistency == nil || *o.Consistency != data.Any || o.Propagation != data.Independent {
		t.Fatalf("%+v", o)
	}
	if data.ReadOptions().Consistency != nil {
		t.Fatal("no option set a consistency")
	}
	ctx := context.Background()
	if _, ok := data.ConsistencyFrom(ctx); ok {
		t.Fatal("an empty ctx has a default")
	}
	if l, ok := data.ConsistencyFrom(data.WithConsistency(ctx, data.Primary)); !ok || l != data.Primary {
		t.Fatalf("got %v %v", l, ok)
	}
}
