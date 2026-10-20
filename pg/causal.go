package pg

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
	"sync/atomic"
	"time"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/op"
)

// ConsistencyHeader is the response header, or RPC metadata, that carries a
// causal token after a request commits a write, and the request header a
// later request brings it back in.
const ConsistencyHeader = "Galleon-Consistency"

// causal is a request's causal floor: the WAL position its ReadYourWrites
// reads must see, raised by every write it commits and by the token it
// brought.
type causal struct {
	lsn atomic.Uint64
	// pending is a verified token's position, not yet clamped to the
	// primary's: that takes a query, so only a ReadYourWrites read that
	// needs it pays for it.
	pending atomic.Uint64
	// wrote is set once a root write transaction has committed something.
	wrote atomic.Bool
}

// raise raises the floor to lsn; it never lowers it.
func (c *causal) raise(lsn uint64) {
	for {
		cur := c.lsn.Load()
		if lsn <= cur || c.lsn.CompareAndSwap(cur, lsn) {
			return
		}
	}
}

// unknownFloor is the floor of a request that committed a write whose
// position couldn't be read: no replica qualifies for its ReadYourWrites
// reads, which read the primary.
const unknownFloor = math.MaxUint64

type causalKey struct{}

func causalFrom(ctx context.Context) *causal {
	c, _ := ctx.Value(causalKey{}).(*causal)
	return c
}

// CausalContext returns a ctx with a causal floor of its own, unless it
// already has one: ReadYourWrites reads begun with it, or with a ctx made
// from it, see the writes committed through it. Each operation call gets
// one from the module; code outside operations, such as a background job,
// uses this.
func CausalContext(ctx context.Context) context.Context {
	if causalFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, causalKey{}, &causal{})
}

// floor is the causal floor of ctx's request, its token clamped to the
// primary's current position first if that hasn't been done.
func (m *Manager) floor(ctx context.Context) uint64 {
	c := causalFrom(ctx)
	if c == nil {
		return 0
	}
	if tok := c.pending.Load(); tok != 0 {
		clamped := tok
		var latest sample
		ok := false
		if m.topo != nil {
			latest, ok = m.topo.primary.latest()
		}
		if !ok || tok > latest.lsn {
			// Past what the tracker last saw: a token a moment old, or a
			// forged one, which would otherwise hold reads off the
			// replicas. If the primary can't be asked, the token stands.
			if cur, err := m.primaryLSN(ctx); err == nil {
				clamped = min(tok, cur)
			}
		}
		c.raise(clamped)
		c.pending.CompareAndSwap(tok, 0)
	}
	return c.lsn.Load()
}

// primaryLSN is the primary's current WAL insert position.
func (m *Manager) primaryLSN(ctx context.Context) (uint64, error) {
	pool := m.sources[data.PrimarySource].pool.Load()
	if pool == nil {
		return 0, errUnavailable
	}
	var s string
	if err := pool.QueryRow(ctx, "SELECT pg_current_wal_insert_lsn()::text").Scan(&s); err != nil {
		return 0, err
	}
	return parseLSN(s)
}

// A causal token is base64url, without padding, of
//
//	version (1 byte) | LSN (8) | issued, Unix seconds (8) | MAC (16)
//
// where the MAC is HMAC-SHA256, truncated, over the domain and the first
// 17 bytes. 44 characters in all.
const (
	tokenVersion = 1
	tokenBody    = 1 + 8 + 8
	tokenMAC     = 16
	tokenLen     = 44 // base64.RawURLEncoding.EncodedLen(tokenBody + tokenMAC)
	// maxTokenHeader bounds what is read of an incoming header; anything
	// longer is refused unread.
	maxTokenHeader = 128
	tokenDomain    = "galleon.consistency\x00"
	// tokenSkew is how far in the future a token's issue time may be, for
	// clocks of nodes that disagree.
	tokenSkew = time.Minute
)

// keyring signs tokens with its first key and verifies them with any.
type keyring struct {
	keys [][]byte
	ttl  time.Duration
}

func newKeyring(cfg Config) *keyring {
	if cfg.TokenKey == "" {
		return nil
	}
	k := &keyring{keys: [][]byte{[]byte(cfg.TokenKey)}, ttl: cfg.TokenTTL}
	if cfg.TokenPreviousKey != "" {
		k.keys = append(k.keys, []byte(cfg.TokenPreviousKey))
	}
	return k
}

func mac(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(tokenDomain))
	h.Write(body)
	return h.Sum(nil)[:tokenMAC]
}

func (k *keyring) sign(lsn uint64, now time.Time) string {
	b := make([]byte, tokenBody, tokenBody+tokenMAC)
	b[0] = tokenVersion
	binary.BigEndian.PutUint64(b[1:], lsn)
	binary.BigEndian.PutUint64(b[9:], uint64(now.Unix()))
	return base64.RawURLEncoding.EncodeToString(append(b, mac(k.keys[0], b)...))
}

// Why a token was refused, as the token metric counts it.
const (
	tokenOversize  = "oversize"
	tokenMalformed = "malformed"
	tokenVersioned = "wrong_version"
	tokenForged    = "bad_signature"
	tokenExpired   = "expired"
	tokenAccepted  = "accepted"
)

// verify returns the LSN s carries, or why it was refused. It never
// panics, whatever s holds.
func (k *keyring) verify(s string, now time.Time) (uint64, string) {
	if len(s) > maxTokenHeader {
		return 0, tokenOversize
	}
	if len(s) != tokenLen {
		return 0, tokenMalformed
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != tokenBody+tokenMAC {
		return 0, tokenMalformed
	}
	if b[0] != tokenVersion {
		return 0, tokenVersioned
	}
	body, sum := b[:tokenBody], b[tokenBody:]
	ok := false
	for _, key := range k.keys {
		// Every key is tried, so the time taken doesn't tell which matched.
		if hmac.Equal(mac(key, body), sum) {
			ok = true
		}
	}
	if !ok {
		return 0, tokenForged
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(body[9:])), 0)
	if now.Sub(issued) > k.ttl || issued.Sub(now) > tokenSkew {
		return 0, tokenExpired
	}
	lsn := binary.BigEndian.Uint64(body[1:])
	if lsn == 0 {
		return 0, tokenMalformed
	}
	return lsn, tokenAccepted
}

// takeToken verifies the token a call brought, if any, into c's pending
// floor. A token that fails is ignored: it costs the call nothing but its
// read-your-writes across requests.
func (m *Manager) takeToken(ctx context.Context, call *op.Call, c *causal) {
	if m.keys == nil || call.Request == nil {
		return
	}
	vals := call.Request.Header.Values(ConsistencyHeader)
	if len(vals) == 0 {
		return
	}
	lsn, result := m.keys.verify(vals[0], m.clock.Now())
	m.metrics.token(ctx, result)
	if result != tokenAccepted {
		// The token itself is never logged.
		m.logger().DebugContext(ctx, "causal token ignored", "operation", call.Desc.ID, "reason", result)
		return
	}
	c.pending.Store(lsn)
}

// giveToken sets the response's token once the call has committed a write.
// A response with a token is the answer for one client, so it is marked
// "Cache-Control: private, no-store", unless the call set its own: a cache
// that kept it would hand one client's token to others, or replay a stale
// one.
func (m *Manager) giveToken(call *op.Call, c *causal) {
	if m.keys == nil || call.Response == nil || call.Response.Header == nil || !c.wrote.Load() {
		return
	}
	lsn := c.lsn.Load()
	if lsn == unknownFloor {
		return // see commitWrite: there is no position to sign
	}
	call.Response.Header.Set(ConsistencyHeader, m.keys.sign(lsn, m.clock.Now()))
	if call.Response.Header.Get("Cache-Control") == "" {
		call.Response.Header.Set("Cache-Control", "private, no-store")
	}
}
