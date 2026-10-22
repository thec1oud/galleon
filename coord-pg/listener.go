package coordpg

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	"github.com/thec1oud/galleon/coord"
)

// listener owns the one connection a backend listens on. It keeps the
// channels it listens on matched to those wanted: the events channel
// always, and each topic's channel while it has a subscription. When the
// connection drops it reconnects, listens again, and wakes every waiter
// and watcher, since notifications sent meanwhile are lost.
type listener struct {
	b   *Backend
	cfg *pgx.ConnConfig

	mu        sync.Mutex
	want      map[string]int // channel → subscriptions wanting it
	active    map[string]bool
	waiting   map[string][]chan struct{} // closed once their channel is active
	dirty     bool
	interrupt context.CancelFunc // ends the wait for a notification, to reconcile
	done      chan struct{}
}

func newListener(b *Backend, cfg *pgx.ConnConfig) *listener {
	cfg = cfg.Copy()
	// A cancelled wait for a notification must leave the connection
	// usable, so a cancel only sets a deadline, rather than sending a
	// cancel request that could reach the next statement.
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: c.Conn()}
	}
	return &listener{
		b: b, cfg: cfg,
		want:    map[string]int{b.events: 1},
		active:  map[string]bool{},
		waiting: map[string][]chan struct{}{},
		done:    make(chan struct{}),
	}
}

// start connects and listens on the events channel, then keeps listening
// until loops ends.
func (l *listener) start(ctx, loops context.Context) error {
	conn, err := l.connect(ctx)
	if err != nil {
		close(l.done)
		return l.b.fail(ctx, err)
	}
	go l.run(loops, conn)
	return nil
}

// wait returns once the listener has stopped.
func (l *listener) wait() { <-l.done }

func (l *listener) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, l.cfg)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.active = map[string]bool{}
	l.dirty = true
	l.mu.Unlock()
	if err := l.reconcile(ctx, conn); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return conn, nil
}

// reconcile listens on the wanted channels it doesn't, and stops listening
// on the others, until nothing changed meanwhile.
func (l *listener) reconcile(ctx context.Context, conn *pgx.Conn) error {
	for {
		l.mu.Lock()
		if !l.dirty {
			l.mu.Unlock()
			return nil
		}
		l.dirty = false
		var on, off []string
		for ch := range l.want {
			if !l.active[ch] {
				on = append(on, ch)
			}
		}
		for ch := range l.active {
			if l.want[ch] == 0 {
				off = append(off, ch)
			}
		}
		l.mu.Unlock()
		for _, ch := range on {
			if _, err := conn.Exec(ctx, "LISTEN "+ident(ch)); err != nil {
				return err
			}
		}
		for _, ch := range off {
			if _, err := conn.Exec(ctx, "UNLISTEN "+ident(ch)); err != nil {
				return err
			}
		}
		l.mu.Lock()
		for _, ch := range on {
			l.active[ch] = true
			for _, w := range l.waiting[ch] {
				close(w)
			}
			delete(l.waiting, ch)
		}
		for _, ch := range off {
			delete(l.active, ch)
		}
		l.mu.Unlock()
	}
}

func (l *listener) run(ctx context.Context, conn *pgx.Conn) {
	defer close(l.done)
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()
	backoff := 50 * time.Millisecond
	for ctx.Err() == nil {
		if conn == nil {
			c, err := l.connect(ctx)
			if err != nil {
				if ctx.Err() == nil {
					l.b.log.Warn("coordination listener can't reconnect; retrying", "error", err, "in", backoff)
				}
				t := time.NewTimer(backoff)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
				}
				backoff = min(2*backoff, 2*time.Second)
				continue
			}
			conn, backoff = c, 50*time.Millisecond
			l.b.log.Info("coordination listener reconnected")
			l.b.wakeAll()
		}
		if err := l.reconcile(ctx, conn); err != nil {
			l.broken(&conn, err)
			continue
		}
		l.mu.Lock()
		if l.dirty {
			l.mu.Unlock()
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		l.interrupt = cancel
		l.mu.Unlock()
		n, err := conn.WaitForNotification(wctx)
		l.mu.Lock()
		l.interrupt = nil
		l.mu.Unlock()
		interrupted := wctx.Err() != nil
		cancel()
		switch {
		case err == nil:
			l.dispatch(n)
		case ctx.Err() != nil:
		case interrupted && !conn.IsClosed():
			// Woken to listen or unlisten.
		default:
			l.broken(&conn, err)
		}
	}
}

func (l *listener) broken(conn **pgx.Conn, err error) {
	l.b.log.Warn("coordination listener lost its connection; reconnecting", "error", err)
	_ = (*conn).Close(context.Background())
	*conn = nil
}

func (l *listener) dispatch(n *pgconn.Notification) {
	if n.Channel != l.b.events {
		if m, ok := decodeMessage(n.Payload); ok {
			l.b.deliver(m)
		}
		return
	}
	if n.Payload == "" {
		return
	}
	switch n.Payload[0] {
	case 'l':
		l.b.changed(n.Payload[1:])
	case 'k':
		l.b.keyChanged(n.Payload[1:], len(n.Payload) > 1)
	}
}

// listen returns once ch is listened on, or ctx ends.
func (l *listener) listen(ctx context.Context, ch string) error {
	l.mu.Lock()
	l.want[ch]++
	if l.active[ch] {
		l.mu.Unlock()
		return nil
	}
	w := make(chan struct{})
	l.waiting[ch] = append(l.waiting[ch], w)
	l.changedLocked()
	l.mu.Unlock()
	select {
	case <-w:
		return nil
	case <-ctx.Done():
		l.unlisten(ch)
		return ctx.Err()
	case <-l.done:
		l.unlisten(ch)
		return coord.ErrClosed
	}
}

// unlisten drops one subscription's want of ch.
func (l *listener) unlisten(ch string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.want[ch] == 0 {
		return
	}
	l.want[ch]--
	if l.want[ch] == 0 {
		delete(l.want, ch)
		l.changedLocked()
	}
}

func (l *listener) changedLocked() {
	l.dirty = true
	if l.interrupt != nil {
		l.interrupt()
	}
}
