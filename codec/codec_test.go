package codec_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thec1oud/galleon/codec"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

type Item struct {
	Quantity int `json:"quantity"`
}
type Order struct {
	Customer string `json:"customer"`
	Items    []Item `json:"items"`
	Ship     struct {
		Express bool `json:"express"`
	} `json:"ship"`
}

func violation(t *testing.T, err error) errs.Violation {
	t.Helper()
	e := errs.From(err)
	if e.Kind != errs.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	return e.Details[0].(errs.FieldViolations)[0]
}

func TestJSONDecodes(t *testing.T) {
	var o Order
	err := codec.JSON.Decode(strings.NewReader(`{"customer":"ann","items":[{"quantity":2}],"ship":{"express":true}}`), &o, op.RejectUnknown)
	if err != nil || o.Customer != "ann" || o.Items[0].Quantity != 2 || !o.Ship.Express {
		t.Fatalf("got %+v, %v", o, err)
	}
	if err := codec.JSON.Decode(strings.NewReader(""), &o, op.RejectUnknown); err != nil {
		t.Fatalf("an empty body is allowed: %v", err)
	}
}

func TestJSONErrorsPointAtTheField(t *testing.T) {
	cases := []struct {
		body, path, code string
		unknown          op.UnknownFields
	}{
		{`{"customer":1}`, "/customer", "type", op.RejectUnknown},
		{`{"ship":{"express":"yes"}}`, "/ship/express", "type", op.RejectUnknown},
		{`{"items":[{"quantity":"x"}]}`, "/items/quantity", "type", op.RejectUnknown},
		{`{"customer":"a","extra":1}`, "/extra", "unknown_field", op.RejectUnknown},
		{`{"customer":`, "", "syntax", op.RejectUnknown},
		{`{"customer" "a"}`, "", "syntax", op.RejectUnknown},
		{`{} {}`, "", "trailing_data", op.RejectUnknown},
	}
	for _, c := range cases {
		var o Order
		v := violation(t, codec.JSON.Decode(strings.NewReader(c.body), &o, c.unknown))
		if v.Path != c.path || v.Code != c.code {
			t.Errorf("%s: got %s %s, want %s %s", c.body, v.Path, v.Code, c.path, c.code)
		}
	}
	var o Order
	if err := codec.JSON.Decode(strings.NewReader(`{"customer":"a","extra":1}`), &o, op.IgnoreUnknown); err != nil {
		t.Fatalf("unknown fields may be ignored: %v", err)
	}
}

func TestJSONBodyLimitIsEnforcedWhileReading(t *testing.T) {
	big := `{"customer":"` + strings.Repeat("a", 1<<20) + `"}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(big))
	body := http.MaxBytesReader(httptest.NewRecorder(), r.Body, 1024)
	var o Order
	err := codec.JSON.Decode(body, &o, op.RejectUnknown)
	e := errs.From(err)
	if e.Code != "galleon.body_too_large" || e.Details[0].(errs.FieldViolations)[0].Params["limit"] != int64(1024) {
		t.Fatalf("got %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestJSONStopsReadingAtTheLimit(t *testing.T) {
	cr := &countingReader{r: strings.NewReader(`{"customer":"` + strings.Repeat("a", 1<<22) + `"}`)}
	body := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(cr), 1024)
	var o Order
	_ = codec.JSON.Decode(body, &o, op.RejectUnknown)
	if cr.n > 64<<10 {
		t.Fatalf("read %d bytes for a 1 KiB limit; the body must not be buffered whole", cr.n)
	}
}

type fakeCodec struct{ mt string }

func (f fakeCodec) MediaType() string                           { return f.mt }
func (fakeCodec) Decode(io.Reader, any, op.UnknownFields) error { return nil }
func (fakeCodec) Encode(w io.Writer, v any) error               { return errors.New("cannot") }

func TestNegotiate(t *testing.T) {
	set := codec.NewSet(fakeCodec{"application/msgpack"}, fakeCodec{"application/xml"})
	cases := map[string]string{
		"":                "application/json",
		"*/*":             "application/json",
		"application/xml": "application/xml",
		"application/xml;q=0.5, application/msgpack": "application/msgpack",
		"application/*":                    "application/json",
		"text/html, application/xml;q=0.9": "application/xml",
		"application/xml, */*;q=0.1":       "application/xml",
		"*/*;q=0.1, application/msgpack":   "application/msgpack",
		"application/xml;q=0":              "",
		"text/html":                        "",
	}
	for accept, want := range cases {
		got := ""
		if c := set.Negotiate(accept); c != nil {
			got = c.MediaType()
		}
		if got != want {
			t.Errorf("Accept %q: got %q, want %q", accept, got, want)
		}
	}
}

func TestForContentType(t *testing.T) {
	set := codec.NewSet(fakeCodec{"application/xml"})
	cases := map[string]string{
		"":                                "application/json",
		"application/json; charset=utf-8": "application/json",
		"application/problem+json":        "application/json",
		"application/xml":                 "application/xml",
		"text/plain":                      "",
		"not a type;;":                    "",
	}
	for ct, want := range cases {
		got := ""
		if c := set.ForContentType(ct); c != nil {
			got = c.MediaType()
		}
		if got != want {
			t.Errorf("Content-Type %q: got %q, want %q", ct, got, want)
		}
	}
}

func TestSetReplacesByMediaType(t *testing.T) {
	set := codec.NewSet(fakeCodec{"application/json"})
	if got := set.MediaTypes(); len(got) != 1 {
		t.Fatalf("a codec for the same type replaces the built-in one: %v", got)
	}
	if _, ok := set.ForContentType("").(fakeCodec); !ok {
		t.Fatal("the replacement must be used")
	}
}

func TestMarshalBuffersAndReportsErrors(t *testing.T) {
	b, err := codec.Marshal(codec.JSON, map[string]int{"a": 1})
	if err != nil || !bytes.Equal(b, []byte("{\"a\":1}\n")) {
		t.Fatalf("got %q, %v", b, err)
	}
	if _, err := codec.Marshal(fakeCodec{"x/y"}, 1); err == nil || !strings.Contains(err.Error(), "as x/y") {
		t.Fatalf("got %v", err)
	}
}

func TestWithPrefersTypeCodecsWithoutChangingTheSet(t *testing.T) {
	app := codec.NewSet(fakeCodec{"application/xml"})
	own := app.With(fakeCodec{"application/json"})
	if _, ok := own.ForContentType("").(fakeCodec); !ok {
		t.Fatal("the operation's own JSON codec must win")
	}
	if app.ForContentType("") != codec.JSON {
		t.Fatal("With must not change the app's set")
	}
	if app.With() != app {
		t.Fatal("With nothing returns the same set")
	}
}
