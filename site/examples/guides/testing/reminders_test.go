package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start clock
func TestAReminderIsSentWhenItIsDue(t *testing.T) {
	h := testkit.New(t, galleon.Modules(Module{}))
	reminders := testkit.Get[*Reminders](h)
	ctx := context.Background()

	reminders.Add("ada@example.com", "standup", time.Hour)
	if n, _ := reminders.SendDue(ctx); n != 0 {
		t.Fatalf("sent %d before it was due", n)
	}
	h.Clock.Advance(time.Hour) // the app's clock; no test waits an hour
	if n, _ := reminders.SendDue(ctx); n != 1 {
		t.Fatalf("sent %d once due, want 1", n)
	}
	if !h.Logs.Contains("reminder sent") {
		t.Errorf("nothing logged:\n%s", h.Logs)
	}
}

// docs:end clock

// docs:start replace
// fakeSender records what it is asked to send.
type fakeSender struct{ sent []string }

func (f *fakeSender) Send(ctx context.Context, to, text string) error {
	f.sent = append(f.sent, to)
	return nil
}

func TestTheSenderGetsEachDueReminder(t *testing.T) {
	fake := &fakeSender{}
	h := testkit.New(t, galleon.Modules(Module{}), galleon.Replace[Sender](fake))
	reminders := testkit.Get[*Reminders](h)

	reminders.Add("ada@example.com", "standup", 0)
	reminders.Add("alan@example.com", "review", 0)
	if _, err := reminders.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.sent) != 2 {
		t.Errorf("sent to %v, want both", fake.sent)
	}
}

// docs:end replace

// docs:start db
func TestEachTestHasItsOwnTransaction(t *testing.T) {
	db := testkit.DB(t, testkit.Schema("CREATE TABLE notes (body text NOT NULL)"))
	ctx := context.Background()
	m := db.Manager()

	err := m.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO notes VALUES ('only in this test')")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = m.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&n)
	})
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; another test's rows would show here", n, err)
	}
}

// docs:end db
