package security

import (
	"context"
	"fmt"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/op"
)

// The built-in providers' names. Every module has them; an operation names
// the ones it uses with op.Builder.Inputs, as with any provider.
const (
	// InputRequest supplies what the transport knows of the call: "ip",
	// the caller's address, empty if unknown; "user_agent"; "transport",
	// such as "http" or "rpc"; and "binding", such as "POST /orders".
	InputRequest = "request"
	// InputTime supplies "now", the app clock's time, in UTC.
	InputTime = "time"
)

type requestProvider struct{}

func (requestProvider) Name() string { return InputRequest }

func (requestProvider) Provide(_ context.Context, _ Principal, r *op.Request) (Attributes, error) {
	m := map[string]any{"ip": r.Peer, "transport": r.Transport, "user_agent": "", "binding": ""}
	if r.Header != nil {
		m["user_agent"] = r.Header.Get("User-Agent")
	}
	if s, ok := r.Binding.(fmt.Stringer); ok {
		m["binding"] = s.String()
	}
	return Values{m: m}, nil
}

type timeProvider struct{ clock clock.Clock }

func (timeProvider) Name() string { return InputTime }

func (t timeProvider) Provide(context.Context, Principal, *op.Request) (Attributes, error) {
	return Values{m: map[string]any{"now": t.clock.Now().UTC()}}, nil
}
