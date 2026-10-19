package errs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKindMappings(t *testing.T) {
	cases := []struct {
		k    Kind
		http int
		grpc uint32
	}{
		{Internal, 500, 13},
		{InvalidArgument, 400, 3},
		{Unauthenticated, 401, 16},
		{PermissionDenied, 403, 7},
		{NotFound, 404, 5},
		{Conflict, 409, 6},
		{FailedPrecondition, 422, 9},
		{TooManyRequests, 429, 8},
		{Canceled, 499, 1},
		{DeadlineExceeded, 504, 4},
		{Unavailable, 503, 14},
		{Unimplemented, 501, 12},
	}
	for _, c := range cases {
		if got := c.k.HTTPStatus(); got != c.http {
			t.Errorf("%v.HTTPStatus() = %d, want %d", c.k, got, c.http)
		}
		if got := c.k.GRPCCode(); got != c.grpc {
			t.Errorf("%v.GRPCCode() = %d, want %d", c.k, got, c.grpc)
		}
	}
}

func TestFromClassifies(t *testing.T) {
	if From(nil) != nil {
		t.Fatal("From(nil) should be nil")
	}
	if k := KindOf(context.Canceled); k != Canceled {
		t.Errorf("canceled: got %v", k)
	}
	if k := KindOf(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)); k != DeadlineExceeded {
		t.Errorf("deadline: got %v", k)
	}
	plain := errors.New("boom")
	e := From(plain)
	if e.Kind != Internal || !errors.Is(e, plain) {
		t.Errorf("plain error: got %+v", e)
	}
	nf := New(NotFound, "orders.not_found", "order not found")
	if From(fmt.Errorf("ctx: %w", nf)) != nf {
		t.Error("From should unwrap to the existing *Error")
	}
}

func TestIsMatchesKindAndCode(t *testing.T) {
	err := fmt.Errorf("lookup: %w", New(NotFound, "orders.not_found", "x"))
	if !errors.Is(err, New(NotFound, "orders.not_found", "")) {
		t.Error("same kind and code should match")
	}
	if !errors.Is(err, New(NotFound, "", "")) {
		t.Error("kind-only target should match")
	}
	if errors.Is(err, New(NotFound, "orders.other", "")) {
		t.Error("different code should not match")
	}
}

func TestProblemHidesInternalDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, errors.New("db password is hunter2"), "trace-1")
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") {
		t.Fatalf("internal cause leaked: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type = %q", ct)
	}
}

func TestProblemCarriesDetailsAndRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	err := New(TooManyRequests, "galleon.rate_limited", "slow down",
		RetryInfo{After: 1500 * time.Millisecond},
		FieldViolations{{Path: "/items/0/qty", Code: "min"}})
	WriteProblem(rec, err, "trace-2")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("status %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "galleon.rate_limited" || p.Instance != "trace-2" || p.Details["field_violations"] == nil {
		t.Errorf("unexpected problem: %+v", p)
	}
}
