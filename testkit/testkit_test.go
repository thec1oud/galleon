package testkit_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/testkit"
)

type Mailer interface{ Send(to string) error }

type fakeMailer struct{ sent []string }

func (f *fakeMailer) Send(to string) error { f.sent = append(f.sent, to); return nil }

type Reminders struct {
	clk  clock.Clock
	mail Mailer
	log  *slog.Logger
}

// Due sends a reminder once a day has passed since start.
func (r *Reminders) Due(start time.Time) error {
	if r.clk.Since(start) < 24*time.Hour {
		return nil
	}
	r.log.Info("sending reminder")
	return r.mail.Send("owner@example.com")
}

func module() galleon.Module {
	return galleon.ModuleFunc("reminders", func(r *galleon.Registry) error {
		galleon.Provide[Mailer](r, func() Mailer { panic("the real mailer must not be built in tests") })
		galleon.Provide[*Reminders](r, func(c clock.Clock, m Mailer, l *slog.Logger) *Reminders { return &Reminders{c, m, l} })
		return nil
	})
}

func TestHarnessRunsTheAppOnFakes(t *testing.T) {
	mail := &fakeMailer{}
	h := testkit.New(t, galleon.Modules(module()), galleon.Replace[Mailer](mail))
	r := testkit.Get[*Reminders](h)

	start := h.Clock.Now()
	h.Clock.Advance(23 * time.Hour)
	if err := r.Due(start); err != nil || len(mail.sent) != 0 {
		t.Fatal("sent too early")
	}
	h.Clock.Advance(time.Hour)
	if err := r.Due(start); err != nil || len(mail.sent) != 1 {
		t.Fatalf("err=%v sent=%v", err, mail.sent)
	}
	if !h.Logs.Contains("sending reminder") {
		t.Fatalf("log not recorded:\n%s", h.Logs)
	}
	if err := h.Ready(context.Background()); err != nil {
		t.Fatalf("harness app should be ready: %v", err)
	}
}

type resource struct{ stopped *bool }

func (r resource) Stop(context.Context) error { *r.stopped = true; return nil }

func TestHarnessShutsDownWhenTheTestEnds(t *testing.T) {
	stopped := false
	t.Run("inner", func(t *testing.T) {
		mod := galleon.ModuleFunc("res", func(r *galleon.Registry) error {
			galleon.Provide[resource](r, func() resource { return resource{&stopped} })
			return nil
		})
		testkit.New(t, galleon.Modules(mod))
		if stopped {
			t.Fatal("stopped before the test ended")
		}
	})
	if !stopped {
		t.Fatal("the app was not shut down when its test ended")
	}
}
