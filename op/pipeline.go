package op

import (
	"context"
	"fmt"
	"net/http"
)

// Stage is one step of the fixed pipeline every call runs through.
type Stage uint8

const (
	StageRoute           Stage = iota + 1 // match a binding to a descriptor
	StageAdmit                            // timeouts, body limits, rate limits
	StageAuthenticate                     // produce a principal, or anonymous
	StageResolveContext                   // produce an operating context
	StageGate                             // does the operation accept that context?
	StagePreAuthorize                     // 5b: action-level check, before decode
	StageDecode                           // produce *In
	StageValidateShape                    // pure validators
	StageAuthorize                        // resource-level check
	StageValidateMeaning                  // validators that read
	StageHandle                           // the handler
	StageCommitHooks                      // after commit: outbox, cache, events
	StageEncode                           // the output, on its way out
)

var stageNames = [...]string{
	StageRoute:           "route",
	StageAdmit:           "admit",
	StageAuthenticate:    "authenticate",
	StageResolveContext:  "resolve_context",
	StageGate:            "gate",
	StagePreAuthorize:    "pre_authorize",
	StageDecode:          "decode",
	StageValidateShape:   "validate_shape",
	StageAuthorize:       "authorize",
	StageValidateMeaning: "validate_meaning",
	StageHandle:          "handle",
	StageCommitHooks:     "commit_hooks",
	StageEncode:          "encode",
}

func (s Stage) String() string {
	if s >= StageRoute && s <= StageEncode {
		return stageNames[s]
	}
	return fmt.Sprintf("stage(%d)", uint8(s))
}

// Valid reports whether s is one of the pipeline's stages.
func (s Stage) Valid() bool { return s >= StageRoute && s <= StageEncode }

// Where places an interceptor before or after a stage.
type Where uint8

const (
	BeforeStage Where = iota + 1
	AfterStage
)

// Position is where an interceptor attaches: before or after a named stage.
type Position struct {
	Stage Stage
	Where Where
}

func Before(s Stage) Position { return Position{Stage: s, Where: BeforeStage} }
func After(s Stage) Position  { return Position{Stage: s, Where: AfterStage} }

func (p Position) String() string {
	if p.Where == AfterStage {
		return "after " + p.Stage.String()
	}
	return "before " + p.Stage.String()
}

// Request is the transport-neutral view of an incoming call.
type Request struct {
	Transport string      // "http", "rpc", "event", "job" or "test"
	Binding   Binding     // the binding the call arrived on
	Header    http.Header // HTTP headers or RPC metadata
	Peer      string      // the caller's network address, if known

	// Decode fills into, a *In, from the call's payload. The transport sets
	// it; it enforces the body limit and the unknown-fields policy.
	Decode func(into any) error
}

// Call is one invocation passing through the pipeline.
type Call struct {
	Desc    *Descriptor
	Input   any // the decoded *In, set by the decode stage
	Request *Request
}

// Invoker runs the rest of the pipeline for a call and returns its output.
type Invoker func(ctx context.Context, call *Call) (any, error)

// Interceptor wraps the rest of the pipeline. It must call next at most once,
// must not call it after returning, and must pass on the ctx it received or
// one derived from it.
type Interceptor func(next Invoker) Invoker

// Placed is an interceptor at a position. Name labels it in route listings.
type Placed struct {
	At   Position
	Name string
	I    Interceptor
}

// Plan is the stage implementations and interceptors one operation runs, as
// prepared at boot.
type Plan struct {
	Stages map[Stage]Interceptor // stage implementations; StageHandle is ignored
	Placed []Placed              // in registration order
}

// Compile builds the operation's invoker once, so a call only walks a
// prepared chain. It also returns the chain's steps, outermost first.
//
// Stages 1 to 9 act on the way in, so each runs its Before interceptors, then
// itself, then its After interceptors. Encode and commit hooks act on the way
// out, after the handler returns, so they wrap the handler from outside in:
// After encode, encode, Before encode, After commit hooks, commit hooks,
// Before commit hooks, then Before handle and After handle around the handler
// itself. Interceptors at the same position run in registration order,
// earliest outermost.
func Compile(h Handler, p Plan) (Invoker, []string) {
	type step struct {
		name string
		i    Interceptor
	}
	var steps []step
	at := func(pos Position) {
		for _, pl := range p.Placed {
			if pl.At == pos {
				steps = append(steps, step{pos.String() + ": " + pl.Name, pl.I})
			}
		}
	}
	impl := func(s Stage) {
		if i := p.Stages[s]; i != nil {
			steps = append(steps, step{s.String(), i})
		}
	}
	for s := StageRoute; s <= StageValidateMeaning; s++ {
		at(Before(s))
		impl(s)
		at(After(s))
	}
	for _, s := range []Stage{StageEncode, StageCommitHooks} {
		at(After(s))
		impl(s)
		at(Before(s))
	}
	at(Before(StageHandle))
	at(After(StageHandle))

	inv := Invoker(func(ctx context.Context, call *Call) (any, error) { return h(ctx, call.Input) })
	for i := len(steps) - 1; i >= 0; i-- {
		inv = steps[i].i(inv)
	}
	names := make([]string, 0, len(steps)+1)
	for _, s := range steps {
		names = append(names, s.name)
	}
	return inv, append(names, StageHandle.String())
}
