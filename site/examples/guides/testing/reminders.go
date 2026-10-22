// Package reminders holds the samples of the docs' guide to testing with
// testkit: a module that sends reminders when they fall due.
package reminders

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
)

// docs:start module
// Sender delivers a reminder. The real one would send an email.
type Sender interface {
	Send(ctx context.Context, to, text string) error
}

// logSender is the Sender the module provides: it only logs.
type logSender struct{ log *slog.Logger }

func (s logSender) Send(ctx context.Context, to, text string) error {
	s.log.InfoContext(ctx, "reminder sent", "to", to)
	return nil
}

// Reminders keeps reminders and sends the ones that are due.
type Reminders struct {
	clock  clock.Clock
	sender Sender
	mu     sync.Mutex
	due    []reminder
}

type reminder struct {
	at       time.Time
	to, text string
}

// Add schedules a reminder after d.
func (r *Reminders) Add(to, text string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.due = append(r.due, reminder{at: r.clock.Now().Add(d), to: to, text: text})
}

// SendDue sends every reminder whose time has come, and returns how many.
func (r *Reminders) SendDue(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now, sent := r.clock.Now(), 0
	var kept []reminder
	for i, m := range r.due {
		if m.at.After(now) {
			kept = append(kept, m)
			continue
		}
		if err := r.sender.Send(ctx, m.to, m.text); err != nil {
			r.due = append(kept, r.due[i:]...) // keep what wasn't sent
			return sent, err
		}
		sent++
	}
	r.due = kept
	return sent, nil
}

// Module provides the Sender and the Reminders.
type Module struct{}

func (Module) Name() string { return "reminders" }

func (Module) Register(r *galleon.Registry) error {
	galleon.Provide[Sender](r, func(log *slog.Logger) Sender { return logSender{log} })
	galleon.Provide[*Reminders](r, func(c clock.Clock, s Sender) *Reminders {
		return &Reminders{clock: c, sender: s}
	})
	return nil
}

// docs:end module
