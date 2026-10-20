package bind_test

import (
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon/bind"
)

type src struct {
	path   map[string]string
	query  url.Values
	header http.Header
}

func (s src) PathValue(n string) string { return s.path[n] }
func (s src) Query(n string) []string   { return s.query[n] }
func (s src) Header(n string) []string  { return s.header.Values(n) }

type In struct {
	ID      string        `path:"id" json:"-"`
	Page    int           `query:"page" json:"-"`
	Limit   *uint16       `query:"limit" json:"-"`
	Expand  []string      `query:"expand" json:"-"`
	Ratio   float32       `query:"ratio" json:"-"`
	Strict  bool          `query:"strict" json:"-"`
	Wait    time.Duration `query:"wait" json:"-"`
	Since   time.Time     `query:"since" json:"-"`
	IP      net.IP        `header:"X-Real-IP" json:"-"`
	Tenant  string        `header:"X-Tenant" json:"-"`
	Body    string        `json:"body"`
	Missing *int          `query:"missing" json:"-"`
}

func TestBindFillsEverySource(t *testing.T) {
	b, err := bind.Plan(reflect.TypeFor[In]())
	if err != nil {
		t.Fatal(err)
	}
	s := src{
		path:   map[string]string{"id": "o-1"},
		query:  url.Values{"page": {"1", "3"}, "limit": {"50"}, "expand": {"items", "customer"}, "ratio": {"0.5"}, "strict": {"true"}, "wait": {"1m30s"}, "since": {"2026-01-02T03:04:05Z"}},
		header: http.Header{"X-Real-Ip": {"10.0.0.1"}, "X-Tenant": {"acme"}},
	}
	var in In
	if ve := b(&in, s); ve != nil {
		t.Fatalf("violations: %+v", ve.Violations)
	}
	if in.ID != "o-1" || in.Page != 3 || *in.Limit != 50 || strings.Join(in.Expand, ",") != "items,customer" ||
		in.Ratio != 0.5 || !in.Strict || in.Wait != 90*time.Second || !in.Since.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) ||
		in.IP.String() != "10.0.0.1" || in.Tenant != "acme" || in.Missing != nil {
		t.Fatalf("bound: %+v", in)
	}
}

func TestBindReportsValuesThatDontParse(t *testing.T) {
	b, _ := bind.Plan(reflect.TypeFor[In]())
	s := src{query: url.Values{"page": {"two"}, "limit": {"70000"}, "since": {"yesterday"}}, header: http.Header{"X-Real-Ip": {"nope"}}}
	var in In
	ve := b(&in, s)
	got := map[string]string{}
	for _, v := range ve.Violations {
		got[v.Path] = v.Code + "/" + v.Params["in"].(string)
	}
	want := map[string]string{"/page": "type/query", "/limit": "type/query", "/since": "type/query", "/X-Real-IP": "type/header"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestPlanWithoutParametersIsNil(t *testing.T) {
	b, err := bind.Plan(reflect.TypeFor[struct{ A string }]())
	if err != nil || b != nil {
		t.Fatal("a type without parameter tags needs no binder")
	}
}

func TestPlanRejectsBadFields(t *testing.T) {
	cases := map[string]any{
		"can't be bound": struct {
			A map[string]string `query:"a"`
		}{},
		"more than one of": struct {
			A string `query:"a" header:"A"`
		}{},
		"holds one value": struct {
			A []string `path:"a"`
		}{},
		"unexported": struct {
			a string `query:"a"`
		}{},
		"is not a struct": 0,
	}
	for want, v := range cases {
		_, err := bind.Plan(reflect.TypeOf(v))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
}

type Pagination struct {
	Page  int `query:"page" json:"-"`
	Limit int `query:"limit" json:"-"`
}

type ListIn struct {
	Pagination
	Q string `query:"q" json:"-"`
}

func TestBindFillsEmbeddedStructs(t *testing.T) {
	b, err := bind.Plan(reflect.TypeFor[ListIn]())
	if err != nil || b == nil {
		t.Fatal(err)
	}
	var in ListIn
	if ve := b(&in, src{query: url.Values{"page": {"2"}, "limit": {"10"}, "q": {"x"}}}); ve != nil {
		t.Fatal(ve.Violations)
	}
	if in.Page != 2 || in.Limit != 10 || in.Q != "x" {
		t.Fatalf("bound: %+v", in)
	}
}

func TestEmptyValuesAreAbsentExceptForStrings(t *testing.T) {
	b, _ := bind.Plan(reflect.TypeFor[ListIn]())
	in := ListIn{Pagination: Pagination{Limit: 7}, Q: "keep"}
	if ve := b(&in, src{query: url.Values{"limit": {""}, "q": {""}}}); ve != nil {
		t.Fatalf("an empty number is absent, not invalid: %+v", ve.Violations)
	}
	if in.Limit != 7 || in.Q != "" {
		t.Fatalf("got %+v", in)
	}
}
