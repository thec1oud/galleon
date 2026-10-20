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
	"reflect"
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
		return jsonError(err, reflect.TypeOf(v))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return jsonError(err, nil)
		}
		return invalid("", "trailing_data", nil, "the body has data after the JSON value")
	}
	return nil
}

// jsonError turns an encoding/json error into an InvalidArgument error with a
// violation, so clients see the same shape as for validation errors. The
// violation points at the field where encoding/json says enough to know it,
// and at the deepest place it can be sure of otherwise: an array, since
// encoding/json doesn't report indices, or the whole body.
func jsonError(err error, into reflect.Type) error {
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
		if typ.Field != "" && into != nil {
			path = wirePath(into, typ.Field)
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
		// encoding/json names the field but not where it is.
		return invalid("", "unknown_field", map[string]any{"field": name}, "the body has a field the operation doesn't accept")
	}
	// A type's own UnmarshalJSON failed, such as time.Time on a bad date.
	return errs.Wrap(err, errs.InvalidArgument, "galleon.invalid_body", "the body has a value that could not be read",
		errs.FieldViolations{{Path: "", Code: "invalid_value", MsgKey: "validation.invalid_value"}})
}

// wirePath turns the dotted field path encoding/json reports into a JSON
// Pointer, walking t so that embedded struct names (which encoding/json
// includes) are dropped and names containing dots stay whole. It stops at
// arrays and maps, whose indices and keys encoding/json doesn't report, and at
// anything it can't match.
func wirePath(t reflect.Type, field string) string {
	segs := strings.Split(field, ".")
	path := ""
	for i := 0; i < len(segs); {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			name, ft, n, embedded := matchField(t, segs[i:])
			if n == 0 {
				return path
			}
			if !embedded {
				path += "/" + escapePointer(name)
			}
			t, i = ft, i+n
		default: // arrays and maps: encoding/json reports no index or key
			return path
		}
	}
	return path
}

// matchField finds the field of struct t that segs starts with. It prefers the
// longest match, so a JSON name containing dots isn't split, and it recognises
// an embedded struct's Go name, which encoding/json puts in the path although
// the wire has no level for it. It returns how many segments matched.
func matchField(t reflect.Type, segs []string) (name string, ft reflect.Type, n int, embedded bool) {
	for k := len(segs); k > 0; k-- {
		want := strings.Join(segs[:k], ".")
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if sf.Anonymous && k == 1 && sf.Name == want && jsonTagName(sf) == "" {
				return "", sf.Type, 1, true
			}
			if !sf.IsExported() {
				continue
			}
			if jn := jsonFieldName(sf); jn != "" && jn == want {
				return jn, sf.Type, k, false
			}
		}
	}
	return "", nil, 0, false
}

func jsonTagName(sf reflect.StructField) string {
	n, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
	return n
}

// jsonFieldName is the name encoding/json uses for sf, or "" if it skips it.
func jsonFieldName(sf reflect.StructField) string {
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if n := jsonTagName(sf); n != "" {
		return n
	}
	return sf.Name
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
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

// byType returns the codec for mt. An exact match wins; a type with a +json
// suffix, such as application/problem+json, falls back to the JSON codec.
func (s *Set) byType(mt string) Codec {
	for _, c := range s.codecs {
		if c.MediaType() == mt {
			return c
		}
	}
	if strings.HasSuffix(mt, "+json") {
		return s.byType("application/json")
	}
	return nil
}

// Negotiate picks the codec for an Accept header value, following RFC 9110:
// each codec's weight comes from the most specific range that matches it
// (type/subtype over type/* over */*), a weight of 0 refuses it, and the
// highest weight wins, with the set's order breaking ties. An empty header
// picks the first codec. It returns nil when nothing acceptable is in the set.
func (s *Set) Negotiate(accept string) Codec {
	if strings.TrimSpace(accept) == "" {
		return s.codecs[0]
	}
	type rng struct {
		mt          string
		q           float64
		specificity int // 2 exact, 1 type/*, 0 */*
	}
	var ranges []rng
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
		spec := 2
		switch {
		case mt == "*/*":
			spec = 0
		case strings.HasSuffix(mt, "/*"):
			spec = 1
		}
		ranges = append(ranges, rng{mt, q, spec})
	}
	var best Codec
	bestQ := 0.0
	for _, c := range s.codecs {
		q, spec := 0.0, -1
		for _, r := range ranges {
			if matches(r.mt, c.MediaType()) && r.specificity > spec {
				q, spec = r.q, r.specificity
			}
		}
		if q > bestQ {
			best, bestQ = c, q
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
