package server_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/op"
)

// stamp is a module whose interceptor adds a response header to every call,
// and tries to replace Content-Type, which the transport keeps.
var stamp = galleon.ModuleFunc("stamp", func(r *galleon.Registry) error {
	r.Intercept(op.Before(op.StageRoute), func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			out, err := next(ctx, call)
			if call.Response != nil {
				call.Response.Header.Set("X-Stamp", call.Request.Transport)
				call.Response.Header.Set("Content-Type", "text/evil")
			}
			return out, err
		}
	})
	return nil
})

func TestInterceptorsSetHTTPResponseHeadersOnSuccessAndError(t *testing.T) {
	e := boot(t, "", stamp)
	for target, status := range map[string]int{"/orders": 200, "/fail": 500} {
		w := e.do("GET", target, "")
		if w.Code != status || w.Header().Get("X-Stamp") != "http" {
			t.Errorf("%s: %d, X-Stamp %q", target, w.Code, w.Header().Get("X-Stamp"))
		}
		if ct := w.Header().Get("Content-Type"); ct == "text/evil" {
			t.Errorf("%s: an interceptor replaced the transport's Content-Type", target)
		}
	}
}

func TestInterceptorsSetRPCResponseMetadataOnSuccessAndError(t *testing.T) {
	e := bootRPC(t, nil, stamp)
	for _, opt := range []connect.ClientOption{connect.WithGRPC(), connect.WithGRPCWeb(), connect.WithHTTPGet()} {
		_, hdr, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), opt)
		if err != nil || hdr.Get("X-Stamp") != "rpc" {
			t.Errorf("success: %v, X-Stamp %q", err, hdr.Get("X-Stamp"))
		}
		_, _, err = greet[GreetIn, GreetOut](e, "Fail", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), opt)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Meta().Get("X-Stamp") != "rpc" {
			t.Errorf("error: %v, meta %v", err, metaOf(ce))
		}
	}
}

func metaOf(ce *connect.Error) http.Header {
	if ce == nil {
		return nil
	}
	return ce.Meta()
}
