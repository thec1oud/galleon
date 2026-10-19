package errs

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// Problem is the RFC 9457 problem+json body Galleon sends for HTTP errors.
type Problem struct {
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Status   int            `json:"status"`
	Instance string         `json:"instance,omitempty"`
	Code     string         `json:"code,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// ToProblem renders err for clients. instance is usually the trace ID. Internal
// errors never expose their message or cause.
func ToProblem(err error, instance string) Problem {
	e := From(err)
	p := Problem{
		Type:     "urn:galleon:problem:" + e.Kind.String(),
		Title:    e.Message,
		Status:   e.Kind.HTTPStatus(),
		Instance: instance,
		Code:     e.Code,
	}
	if e.Kind == Internal {
		p.Title = "internal error"
		p.Code = "galleon.internal"
		return p
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	for _, d := range e.Details {
		if p.Details == nil {
			p.Details = map[string]any{}
		}
		switch v := d.(type) {
		case RetryInfo:
			p.Details[d.DetailType()] = map[string]any{"after_seconds": math.Ceil(v.After.Seconds())}
		default:
			p.Details[d.DetailType()] = d
		}
	}
	return p
}

// WriteProblem writes err as application/problem+json.
func WriteProblem(w http.ResponseWriter, err error, instance string) {
	p := ToProblem(err, instance)
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	for _, d := range From(err).Details {
		if ri, ok := d.(RetryInfo); ok && ri.After > 0 {
			h.Set("Retry-After", strconv.Itoa(int(math.Ceil(ri.After.Seconds()))))
		}
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
