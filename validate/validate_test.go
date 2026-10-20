package validate_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/validate"
)

type Item struct {
	SKU      string `json:"sku" validate:"required"`
	Quantity int    `json:"quantity" validate:"min=1,max=99"`
}

type Order struct {
	Customer string            `json:"customer" validate:"required,max=5"`
	Email    string            `json:"email" validate:"email"`
	Status   string            `json:"status" validate:"oneof=new paid"`
	Code     string            `json:"code" pattern:"^[A-Z]{2},[0-9]+$"`
	Items    []Item            `json:"items" validate:"min=1"`
	Ship     *Item             `json:"ship"`
	Note     *string           `json:"note" validate:"required,len=2"`
	Tags     map[string]string `json:"tags" validate:"max=1"`
	ID       string            `path:"id" json:"-" validate:"uuid"`
	Ref      string            `json:"a/b~c" validate:"url"`
	When     time.Time         `json:"when"`
	Priority uint8             `json:"priority" validate:"oneof=1 2"`
	Score    float64           `json:"score" validate:"max=1.5"`
	internal string            //nolint:unused
}

func violations(t *testing.T, ve *op.ValidationError) map[string]string {
	t.Helper()
	out := map[string]string{}
	if ve == nil {
		return out
	}
	for _, v := range ve.Violations {
		if _, dup := out[v.Path]; dup {
			out[v.Path] += "," + v.Code
			continue
		}
		out[v.Path] = v.Code
	}
	return out
}

func TestPlanReportsEveryViolationWithWirePaths(t *testing.T) {
	check, err := validate.Plan(reflect.TypeFor[Order]())
	if err != nil {
		t.Fatal(err)
	}
	note := "toolong"
	got := violations(t, check(&Order{
		Customer: "abcdefg",
		Email:    "not an email",
		Status:   "lost",
		Code:     "ab,12",
		Items:    []Item{{SKU: "a", Quantity: 1}, {Quantity: 100}},
		Ship:     &Item{Quantity: 0, SKU: "x"},
		Note:     &note,
		Tags:     map[string]string{"a": "1", "b": "2"},
		ID:       "nope",
		Ref:      "nope",
		Priority: 3,
		Score:    2,
	}))
	want := map[string]string{
		"/customer":         "max_length",
		"/email":            "email",
		"/status":           "oneof",
		"/code":             "pattern",
		"/items/1/sku":      "required",
		"/items/1/quantity": "max",
		"/ship/quantity":    "min",
		"/note":             "len_length",
		"/tags":             "max_length",
		"/id":               "uuid",
		"/a~1b~0c":          "url",
		"/priority":         "oneof",
		"/score":            "max",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violations:\n got %v\nwant %v", got, want)
	}
}

func TestValidInputHasNoViolations(t *testing.T) {
	check, _ := validate.Plan(reflect.TypeFor[Order]())
	note := "hi"
	ve := check(&Order{
		Customer: "ann",
		Email:    "ann@example.com",
		Status:   "paid",
		Code:     "AB,12",
		Items:    []Item{{SKU: "a", Quantity: 1}},
		Note:     &note,
		ID:       "123e4567-e89b-12d3-a456-426614174000",
		Ref:      "https://example.com/x",
		Priority: 2,
		Score:    1.5,
	})
	if ve != nil {
		t.Fatalf("unexpected violations: %+v", ve.Violations)
	}
}

func TestRequiredAndNilPointers(t *testing.T) {
	check, _ := validate.Plan(reflect.TypeFor[Order]())
	got := violations(t, check(&Order{Items: []Item{{SKU: "a", Quantity: 1}}}))
	if got["/customer"] != "required" || got["/note"] != "required" {
		t.Fatalf("required fields: %v", got)
	}
	if _, ok := got["/ship"]; ok {
		t.Fatal("a nil optional pointer is skipped")
	}
	// Empty optional strings pass format rules; only required rejects them.
	for _, p := range []string{"/email", "/id", "/a~1b~0c"} {
		if _, ok := got[p]; ok {
			t.Fatalf("%s must accept an empty value: %v", p, got)
		}
	}
	empty := ""
	if got := violations(t, check(&Order{Customer: "a", Note: &empty, Items: []Item{{SKU: "a", Quantity: 1}}})); got["/note"] != "len_length" {
		t.Fatalf("a non-nil pointer to an empty string is present for required, and checked by len: %v", got)
	}
}

func TestLengthCountsCharacters(t *testing.T) {
	type In struct {
		Name string `json:"name" validate:"max=3"`
	}
	check, _ := validate.Plan(reflect.TypeFor[In]())
	if ve := check(&In{Name: "héé"}); ve != nil {
		t.Fatalf("three characters fit max=3 even though they are five bytes: %+v", ve)
	}
}

type Node struct {
	Name     string  `json:"name" validate:"required"`
	Children []*Node `json:"children"`
}

func TestRecursiveTypes(t *testing.T) {
	check, err := validate.Plan(reflect.TypeFor[Node]())
	if err != nil {
		t.Fatal(err)
	}
	got := violations(t, check(&Node{Name: "root", Children: []*Node{{Name: "a"}, {Children: []*Node{nil, {}}}}}))
	want := map[string]string{"/children/1/name": "required", "/children/1/children/1/name": "required"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestNoRulesMeansNoValidator(t *testing.T) {
	type Plain struct {
		A string
		B []struct{ C int }
	}
	check, err := validate.Plan(reflect.TypeFor[Plain]())
	if err != nil || check != nil {
		t.Fatalf("a type without rules needs no validator, got %v, %v", check != nil, err)
	}
}

func TestBadRulesFailPlanning(t *testing.T) {
	cases := map[string]any{
		"unknown rule": struct {
			A string `validate:"sometimes"`
		}{},
		"min needs a value": struct {
			A int `validate:"min"`
		}{},
		"want a number": struct {
			A int `validate:"max=lots"`
		}{},
		"len applies to strings": struct {
			A int `validate:"len=2"`
		}{},
		"oneof applies": struct {
			A bool `validate:"oneof=a b"`
		}{},
		"email applies": struct {
			A int `validate:"email"`
		}{},
		"pattern applies": struct {
			A int `pattern:"x"`
		}{},
		"pattern: error parsing": struct {
			A string `pattern:"("`
		}{},
		"non-negative integer": struct {
			A string `validate:"min=-1"`
		}{},
		"validate_test.Item2.A": struct{ B []Item2 }{},
	}
	for want, v := range cases {
		_, err := validate.Plan(reflect.TypeOf(v))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
}

type Item2 struct {
	A string `validate:"nope"`
}

type Selfish struct {
	A string `json:"a" validate:"required"`
}

func (s *Selfish) Validate(context.Context) *op.ValidationError {
	if s.A == "bad" {
		return (*op.ValidationError)(nil).Add("/a", "selfish.bad", nil)
	}
	return nil
}

func TestForOperationCombinesTagsAndSelf(t *testing.T) {
	v, err := validate.ForOperation(reflect.TypeFor[Selfish]())
	if err != nil || v == nil {
		t.Fatal(err)
	}
	if got := violations(t, v(context.Background(), &Selfish{})); got["/a"] != "required" {
		t.Fatalf("tags: %v", got)
	}
	if got := violations(t, v(context.Background(), &Selfish{A: "bad"})); got["/a"] != "selfish.bad" {
		t.Fatalf("self: %v", got)
	}
	none, err := validate.ForOperation(reflect.TypeFor[struct{ A string }]())
	if err != nil || none != nil {
		t.Fatal("no tags and no Validate method means no validator")
	}
}
