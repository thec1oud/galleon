package client

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/errs"
)

// retrying retries idempotent requests that failed in a way a retry can fix.
type retrying struct {
	next   http.RoundTripper
	cfg    Config
	clock  clock.Clock
	budget *budget
}

type resendKey struct{}

// resendCount is the attempt's number among retries: 0 for the first attempt.
func resendCount(ctx context.Context) int {
	n, _ := ctx.Value(resendKey{}).(int)
	return n
}

func (r *retrying) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	r.budget.deposit()
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	retryable := replayable && idempotent(req)
	for attempt := 0; ; attempt++ {
		areq := req
		if attempt > 0 {
			areq = req.Clone(context.WithValue(ctx, resendKey{}, attempt))
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				areq.Body = body
			}
		}
		res, err := r.next.RoundTrip(areq)
		if !retryable || attempt >= r.cfg.Retries {
			return res, err
		}
		wait, ok := r.retryAfter(ctx, res, err, attempt)
		if !ok || !r.budget.withdraw() {
			return res, err
		}
		if res != nil {
			discard(res)
		}
		t := r.clock.NewTimer(wait)
		select {
		case <-t.C():
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
}

// retryAfter says whether a retry can fix the outcome and how long to wait
// before it: a jittered backoff, or the server's Retry-After when longer.
func (r *retrying) retryAfter(ctx context.Context, res *http.Response, err error, attempt int) (time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, false // the caller gave up
	}
	wait := backoff(r.cfg.RetryBackoff, r.cfg.RetryBackoffMax, attempt)
	switch {
	case err != nil:
		var e *errs.Error
		if errors.As(err, &e) {
			return 0, false // an open circuit or a refused address won't change
		}
	case res.StatusCode == http.StatusTooManyRequests, res.StatusCode == http.StatusBadGateway,
		res.StatusCode == http.StatusServiceUnavailable, res.StatusCode == http.StatusGatewayTimeout:
		if ra, ok := parseRetryAfter(res.Header.Get("Retry-After"), r.clock.Now()); ok {
			if ra > r.cfg.MaxRetryAfter {
				return 0, false
			}
			wait = max(wait, ra)
		}
	default:
		return 0, false
	}
	if dl, ok := ctx.Deadline(); ok && r.clock.Now().Add(wait).After(dl) {
		return 0, false // the retry couldn't finish in time
	}
	return wait, true
}

// backoff is a random wait up to base doubled per attempt, capped at ceiling
// ("full jitter"), so clients that failed together don't retry together.
func backoff(base, ceiling time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := ceiling
	if attempt < 30 && base<<attempt < ceiling && base<<attempt > 0 {
		d = base << attempt
	}
	return rand.N(d) + 1
}

// longestRetryAfter is the most seconds parseRetryAfter returns: a year,
// far past any sensible MaxRetryAfter.
const longestRetryAfter = 365 * 24 * 60 * 60

func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseInt(v, 10, 64); (err == nil || errors.Is(err, strconv.ErrRange)) && s >= 0 {
		// Capped before converting, so a huge value stays huge rather than
		// overflowing into a short or negative wait.
		return time.Duration(min(s, longestRetryAfter)) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

func idempotent(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != ""
}

// discard drains a little of a response nobody will read, so its connection
// can be reused, and closes it.
func discard(res *http.Response) {
	_, _ = io.CopyN(io.Discard, res.Body, 4<<10)
	_ = res.Body.Close()
}

// budget limits retries to a share of requests: each request earns ratio
// tokens, up to burst, and each retry spends one.
type budget struct {
	mu           sync.Mutex
	tokens       float64
	ratio, burst float64
}

func newBudget(ratio, burst float64) *budget {
	return &budget{tokens: burst, ratio: ratio, burst: burst}
}

func (b *budget) deposit() {
	b.mu.Lock()
	b.tokens = min(b.tokens+b.ratio, b.burst)
	b.mu.Unlock()
}

func (b *budget) withdraw() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// breakers keeps a circuit breaker per host.
type breakers struct {
	next     http.RoundTripper
	failures int
	openFor  time.Duration
	clock    clock.Clock

	mu    sync.Mutex
	hosts map[string]*breaker
}

// maxBreakers bounds how many hosts are tracked, for clients that call
// user-supplied URLs; closed breakers are dropped first.
const maxBreakers = 1024

type breaker struct {
	failures  int
	openUntil time.Time
	probing   bool
}

func (b *breakers) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if !b.allow(host) {
		closeBody(req)
		return nil, errs.New(errs.Unavailable, "galleon.circuit_open", "the circuit to "+host+" is open")
	}
	res, err := b.next.RoundTrip(req)
	switch {
	case err != nil && req.Context().Err() != nil:
		b.record(host, outcomeNeutral) // the caller gave up; that says nothing about the host
	case err != nil:
		var e *errs.Error
		if errors.As(err, &e) {
			b.record(host, outcomeNeutral) // refused before reaching the host
		} else {
			b.record(host, outcomeFailure)
		}
	case res.StatusCode >= 500:
		b.record(host, outcomeFailure)
	default:
		b.record(host, outcomeSuccess)
	}
	return res, err
}

func (b *breakers) allow(host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.hosts[host]
	if br == nil || br.failures < b.failures {
		return true
	}
	if b.clock.Now().Before(br.openUntil) || br.probing {
		return false
	}
	br.probing = true // half open: one request finds out whether the host is back
	return true
}

type outcome uint8

const (
	outcomeSuccess outcome = iota
	outcomeFailure
	outcomeNeutral
)

func (b *breakers) record(host string, o outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.hosts[host]
	switch o {
	case outcomeSuccess:
		if br != nil {
			delete(b.hosts, host)
		}
	case outcomeNeutral:
		if br != nil {
			br.probing = false
		}
	case outcomeFailure:
		if br == nil {
			if len(b.hosts) >= maxBreakers {
				b.evict()
			}
			br = &breaker{}
			b.hosts[host] = br
		}
		br.failures++
		br.probing = false
		if br.failures >= b.failures {
			br.openUntil = b.clock.Now().Add(b.openFor)
		}
	}
}

// evict drops the breakers that aren't open, or any one if all are.
func (b *breakers) evict() {
	for h, br := range b.hosts {
		if br.failures < b.failures {
			delete(b.hosts, h)
		}
	}
	for h := range b.hosts {
		if len(b.hosts) < maxBreakers {
			return
		}
		delete(b.hosts, h)
	}
}
