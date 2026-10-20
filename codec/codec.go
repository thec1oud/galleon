// Package codec is the seam for payload formats. JSON is built in; other
// formats (protobuf, msgpack, CBOR, XML, CSV) register through the same
// interface, and transports pick one by content negotiation.
package codec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// Codec reads and writes one media type. It is defined in op so descriptors
// can carry codecs made for their types.
type Codec = op.Codec

// JSON is the built-in JSON codec, on encoding/json.
var JSON Codec = jsonCodec{}

type jsonCodec struct{}

func (jsonCodec) MediaType() string { return "application/json" }

func (jsonCodec) Encode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func (jsonCodec) Decode(r io.Reader, v any, unknown op.UnknownFields) error {
	dec := json.NewDecoder(r)
	if unknown == op.RejectUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty body leaves the input at its zero value
		}
		return jsonError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("", "trailing_data", nil, "the body has data after the JSON value")
	}
	return nil
}

// jsonError turns an encoding/json error into an InvalidArgument error with a
// violation pointing at the field, so clients see the same shape as for
// validation errors.
func jsonError(err error) error {
	var (
		typ    *json.UnmarshalTypeError
		syntax *json.SyntaxError
		tooBig *http.MaxBytesError
	)
	switch {
	case errors.As(err, &tooBig):
		return errs.New(errs.InvalidArgument, "galleon.body_too_large", "the request body is too large",
			errs.FieldViolations{{Path: "", Code: "body_too_large", Params: map[string]any{"limit": tooBig.Limit}, MsgKey: "validation.body_too_large"}})
	case errors.As(err, &typ):
		path := ""
		if typ.Field != "" {
			path = "/" + strings.ReplaceAll(typ.Field, ".", "/")
		}
		return invalid(path, "type", map[string]any{"got": typ.Value, "want": jsonKind(typ.Type.Kind().String())}, "a field has the wrong type")
	case errors.As(err, &syntax):
		return invalid("", "syntax", map[string]any{"offset": syntax.Offset}, "the body is not valid JSON")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return invalid("", "syntax", nil, "the body is not valid JSON")
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if n, err := strconv.Unquote(name); err == nil {
			name = n
		}
		return invalid("/"+name, "unknown_field", nil, "the body has a field the operation doesn't accept")
	}
	return errs.Wrap(err, errs.InvalidArgument, "galleon.invalid_body", "the body could not be read")
}

func jsonKind(goKind string) string {
	switch {
	case strings.HasPrefix(goKind, "int"), strings.HasPrefix(goKind, "uint"), strings.HasPrefix(goKind, "float"):
		return "number"
	case goKind == "bool":
		return "boolean"
	case goKind == "slice", goKind == "array":
		return "array"
	case goKind == "struct", goKind == "map":
		return "object"
	}
	return goKind
}

func invalid(path, code string, params map[string]any, message string) error {
	return errs.New(errs.InvalidArgument, "galleon.invalid_body", message,
		errs.FieldViolations{{Path: path, Code: code, Params: params, MsgKey: "validation." + code}})
}

// Set is the codecs an app can speak, in order of preference.
type Set struct{ codecs []Codec }

// NewSet returns a set with JSON first, then the given codecs. A codec for a
// media type already in the set replaces it.
func NewSet(extra ...Codec) *Set {
	s := &Set{codecs: []Codec{JSON}}
	for _, c := range extra {
		s.Add(c)
	}
	return s
}

// Add adds c, replacing any codec for the same media type.
func (s *Set) Add(c Codec) {
	mt := c.MediaType()
	if i := slices.IndexFunc(s.codecs, func(x Codec) bool { return x.MediaType() == mt }); i >= 0 {
		s.codecs[i] = c
		return
	}
	s.codecs = append(s.codecs, c)
}

// With returns a copy of the set with cs added, each replacing any codec for
// the same media type. Transports use it at boot to prefer an operation's own
// codecs (op.TypeInfo.Codecs) over the app's.
func (s *Set) With(cs ...Codec) *Set {
	if len(cs) == 0 {
		return s
	}
	out := &Set{codecs: append([]Codec(nil), s.codecs...)}
	for _, c := range cs {
		out.Add(c)
	}
	return out
}

// MediaTypes lists the media types in the set, most preferred first.
func (s *Set) MediaTypes() []string {
	out := make([]string, len(s.codecs))
	for i, c := range s.codecs {
		out[i] = c.MediaType()
	}
	return out
}

// ForContentType returns the codec for a Content-Type header value. An empty
// header means JSON. It returns nil when nothing in the set reads the type.
func (s *Set) ForContentType(header string) Codec {
	if header == "" {
		return s.codecs[0]
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return nil
	}
	return s.byType(mt)
}

func (s *Set) byType(mt string) Codec {
	for _, c := range s.codecs {
		if c.MediaType() == mt || (strings.HasSuffix(mt, "+json") && c.MediaType() == "application/json") {
			return c
		}
	}
	return nil
}

// Negotiate picks the codec for an Accept header value: the highest-weighted
// acceptable type the set has, preferring the set's order between equal
// weights. An empty header or */* picks the first codec. It returns nil when
// nothing acceptable is in the set.
func (s *Set) Negotiate(accept string) Codec {
	if strings.TrimSpace(accept) == "" {
		return s.codecs[0]
	}
	best, bestQ, bestRank := Codec(nil), 0.0, 0
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if v, ok := params["q"]; ok {
			if q, err = strconv.ParseFloat(v, 64); err != nil || q < 0 || q > 1 {
				continue
			}
		}
		if q == 0 {
			continue
		}
		for rank, c := range s.codecs {
			if !matches(mt, c.MediaType()) {
				continue
			}
			// Exact types beat wildcards at the same weight.
			specificity := strings.Count(mt, "*")
			r := rank + specificity*len(s.codecs)
			if q > bestQ || (q == bestQ && r < bestRank) {
				best, bestQ, bestRank = c, q, r
			}
		}
	}
	return best
}

func matches(pattern, mt string) bool {
	if pattern == "*/*" || pattern == mt {
		return true
	}
	if typ, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(mt, typ+"/")
	}
	return false
}

// Marshal encodes v with c into a buffer, so a failed encode can still be
// answered with an error rather than half a body.
func Marshal(c Codec, v any) ([]byte, error) {
	var b bytes.Buffer
	if err := c.Encode(&b, v); err != nil {
		return nil, fmt.Errorf("encoding %T as %s: %w", v, c.MediaType(), err)
	}
	return b.Bytes(), nil
}
