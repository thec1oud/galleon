package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/thec1oud/galleon/errs"
)

// StatusError is a response outside 2xx. Problem is the upstream's
// problem+json, when it sent one. errs.From treats a StatusError as Internal,
// so an upstream's 401 or 404 doesn't become the caller's; Kind gives the
// upstream's kind for callers that want to pass it on.
type StatusError struct {
	Status  int
	Header  http.Header
	Problem *errs.Problem
	Body    []byte // up to 64 KiB of the body
}

func (e *StatusError) Error() string {
	if e.Problem != nil && e.Problem.Code != "" {
		return fmt.Sprintf("upstream responded %d: %s: %s", e.Status, e.Problem.Code, e.Problem.Title)
	}
	return fmt.Sprintf("upstream responded %d %s", e.Status, http.StatusText(e.Status))
}

// Kind is the error kind the status stands for.
func (e *StatusError) Kind() errs.Kind {
	switch e.Status {
	case 400:
		return errs.InvalidArgument
	case 401:
		return errs.Unauthenticated
	case 403:
		return errs.PermissionDenied
	case 404:
		return errs.NotFound
	case 409:
		return errs.Conflict
	case 412, 422:
		return errs.FailedPrecondition
	case 429:
		return errs.TooManyRequests
	case 499:
		return errs.Canceled
	case 501:
		return errs.Unimplemented
	case 502, 503:
		return errs.Unavailable
	case 504:
		return errs.DeadlineExceeded
	}
	if e.Status >= 400 && e.Status < 500 {
		return errs.InvalidArgument
	}
	return errs.Internal
}

// Send sends in as JSON (none when in is nil) and decodes a JSON response
// into a new Out. A 204, or an empty body, gives a zero Out. A failure to
// reach the server is Unavailable; an open circuit or a refused address keeps
// its own error; a response outside 2xx is a *StatusError.
func Send[Out any](ctx context.Context, c *Client, method, target string, in any) (*Out, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "galleon.encode_failed", "")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "galleon.bad_request", "")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	res, err := c.Do(req)
	if err != nil {
		var e *errs.Error
		switch {
		case errors.As(err, &e):
			return nil, e
		case ctx.Err() != nil:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errs.Wrap(err, errs.DeadlineExceeded, "galleon.upstream_timeout", "")
			}
			return nil, errs.Wrap(err, errs.Canceled, "galleon.canceled", "")
		}
		return nil, errs.Wrap(err, errs.Unavailable, "galleon.upstream_unavailable", "the upstream service could not be reached")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, statusError(res)
	}
	out := new(Out)
	if res.StatusCode == http.StatusNoContent {
		return out, nil
	}
	limit := c.cfg.MaxResponseBytes
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, errs.Wrap(err, errs.Unavailable, "galleon.upstream_unavailable", "the upstream response could not be read")
	}
	if int64(len(data)) > limit {
		return nil, errs.New(errs.Internal, "galleon.upstream_too_large", fmt.Sprintf("the upstream response is over %d bytes", limit))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "galleon.upstream_invalid", "the upstream response is not the expected JSON")
	}
	return out, nil
}

func statusError(res *http.Response) *StatusError {
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	e := &StatusError{Status: res.StatusCode, Header: res.Header, Body: data}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt == "application/problem+json" {
		var p errs.Problem
		if json.Unmarshal(data, &p) == nil {
			e.Problem = &p
		}
	}
	return e
}
