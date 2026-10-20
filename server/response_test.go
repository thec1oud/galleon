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

// reserved are headers that are the transport's alone.
var reserved = []string{"Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive",
	"Upgrade", "Trailer", "Te", "Proxy-Authenticate", "Grpc-Status", "Grpc-Message", "Connect-Protocol-Version"}

// noneEvil fails the test if h has any header an interceptor set to "evil".
func noneEvil(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, v := range h {
		for _, x := range v {
			if x == "evil" {
				t.Errorf("%s: an interceptor set the transport's %s", what, k)
			}
		}
	}
}

// stamp is a module whose interceptor adds a response header to every call,
// and tries to set the transport's own.
var stamp = galleon.ModuleFunc("stamp", func(r *galleon.Registry) error {
	r.Intercept(op.Before(op.StageRoute), func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			out, err := next(ctx, call)
			if call.Response != nil {
				h := call.Response.Header
				h.Set("X-Stamp", call.Request.Transport)
				for _, k := range reserved {
					h.Set(k, "evil")
				}
				h["content-length"] = []string{"evil"} // not in canonical form
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
		noneEvil(t, target, w.Header())
	}
}

func TestInterceptorsSetRPCResponseMetadataOnSuccessAndError(t *testing.T) {
	e := bootRPC(t, nil, stamp)
	for _, opt := range []connect.ClientOption{connect.WithGRPC(), connect.WithGRPCWeb(), connect.WithHTTPGet()} {
		_, hdr, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), opt)
		if err != nil || hdr.Get("X-Stamp") != "rpc" {
			t.Errorf("success: %v, X-Stamp %q", err, hdr.Get("X-Stamp"))
		}
		noneEvil(t, "rpc success", hdr)
		_, _, err = greet[GreetIn, GreetOut](e, "Fail", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), opt)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Meta().Get("X-Stamp") != "rpc" {
			t.Errorf("error: %v, meta %v", err, metaOf(ce))
		}
		noneEvil(t, "rpc error", metaOf(ce))
	}
}

func metaOf(ce *connect.Error) http.Header {
	if ce == nil {
		return nil
	}
	return ce.Meta()
}
