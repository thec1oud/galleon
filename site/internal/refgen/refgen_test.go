package refgen

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var loaded = sync.OnceValues(func() (*source, error) { return load("../../..") })

func galleon(t *testing.T) *source {
	t.Helper()
	s, err := loaded()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fixture loads a module of testdata, outside the workspace.
func fixture(t *testing.T, name string) *source {
	t.Helper()
	s, err := load(filepath.Join("testdata", name), "GOWORK=off", "GOFLAGS=-mod=mod")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The pages in the site must be what the source generates now.
func TestThePagesAreUpToDate(t *testing.T) {
	pages, err := galleon(t).generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range Pages {
		got, err := os.ReadFile(filepath.Join("../..", rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ReplaceAll(string(got), "\r\n", "\n") != pages[rel] { // as a checkout with core.autocrlf has it
			t.Errorf("%s is out of date: run `go generate ./...` in site/", rel)
		}
	}
}

func TestEveryCodeHasAKindAndAMeaning(t *testing.T) {
	codes, err := galleon(t).codes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) < 50 {
		t.Fatalf("found only %d codes", len(codes))
	}
	for _, c := range codes {
		if len(c.Kinds) == 0 {
			t.Errorf("%s: no kind found", c.Code)
		}
		if meaning(c) == "" {
			t.Errorf("%s: no meaning", c.Code)
		}
	}
}

func TestEverySectionIsFound(t *testing.T) {
	sections, err := galleon(t).sections()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"client": true, "http": true, "migrate": true, "pg": true}
	for _, s := range sections {
		if !want[s.Name] {
			t.Errorf("unexpected section %q: add it to Defaults, or to this test if it has no defaults", s.Name)
		}
		delete(want, s.Name)
	}
	for name := range want {
		t.Errorf("section %q not found", name)
	}
}

// Every key a reader can set needs a doc comment to describe it.
func TestEveryKeyHasADoc(t *testing.T) {
	sections, err := galleon(t).sections()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sections {
		for _, f := range s.Fields {
			if f.Doc == "" {
				t.Errorf("%s.%s has no doc comment", s.Name, f.Key)
			}
		}
	}
}

func TestTheScannerFollowsEveryWayToReturnACode(t *testing.T) {
	codes, err := fixture(t, "good").codes()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Code{}
	for _, c := range codes {
		got[c.Code] = c
	}
	for code, kind := range map[string]string{
		"use.aliased": "NotFound", "use.doc": "Conflict", "use.folded": "Conflict", "use.formatted": "InvalidArgument",
		"use.literal": "PermissionDenied", "use.returned": "Unauthenticated", "use.helper": "NotFound",
		"use.method": "Conflict", "use.closure": "InvalidArgument", "use.two_level": "NotFound", "use.wrapper": "Conflict",
		"use.keyed": "Unauthenticated", "use.generic": "PermissionDenied", "use.assigned": "NotFound",
	} {
		c, ok := got[code]
		if !ok {
			t.Errorf("%s not found", code)
			continue
		}
		if !reflect.DeepEqual(c.Kinds, []string{kind}) || !reflect.DeepEqual(c.Packages, []string{"use"}) {
			t.Errorf("%s: kinds %v, packages %v; want [%s], [use]", code, c.Kinds, c.Packages, kind)
		}
	}
	if c := got["use.two_sites"]; !reflect.DeepEqual(c.Kinds, []string{"Conflict", "NotFound"}) || !reflect.DeepEqual(c.Messages, []string{"first", "second"}) {
		t.Errorf("use.two_sites: kinds %v, messages %v", c.Kinds, c.Messages)
	}
	if len(got) != 18 { // with the server package's three
		t.Errorf("found %d codes, want 18: %v", len(got), codes)
	}
	for code, want := range map[string]string{
		"use.doc":       "Documented. SQLSTATE 40001. Message: “the doc”.",
		"use.folded":    "Message: “folded … times”.",
		"use.formatted": "Message: “… is …”.",
		"use.helper":    "Message: “by helper”.",
		"use.two_level": "Message: “by outer”.",
		"use.wrapper":   "Message: “by fail”.",
	} {
		if m := meaning(got[code]); m != want {
			t.Errorf("meaning of %s = %q, want %q", code, m, want)
		}
	}
}

func TestTheScannerFailsOnACodeItCantResolve(t *testing.T) {
	s := fixture(t, "bad")
	_, err := s.codes()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		`use/use.go:11: an error's code isn't a constant`,
		`use/use.go:13: "Use.Bad" isn't a catalog code`,
		`use/use.go:15: code "use.kind_var" comes with a kind that isn't a constant`,
		`use/use.go:19: an error's code isn't a constant`,
		`use/use.go:27: an error's kind is neither a constant nor a parameter`,
		`use/use.go:30: an errs.Error literal sets a Code but no Kind`,
		`use/use.go:32: helper passes a parameter on as an error's code, and is used other than in a call`,
		`use/use.go:55: fail is called through an interface`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q:\n%v", want, err)
		}
	}
	_, err = s.sections()
	for _, want := range []string{
		"use/use.go:23: a config section's name isn't a constant",
		`section "twice": inner.key is tagged secret, but only a section's own fields are redacted`,
		`use/use.go:44: section "twice" is loaded as S here and as T elsewhere`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("sections: the error lacks %q:\n%v", want, err)
		}
	}
	_, err = s.statuses()
	for _, want := range []string{
		"server/server.go:21: status is set to a status that isn't a constant inside",
		"server/server.go:30: status is set to a status that isn't a constant inside", // in an else
		"server/server.go:35: writeError's status status isn't a local of param",
		"server/server.go:39: server.format is sent with status 415 here and 400 elsewhere",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("statuses: the error lacks %q:\n%v", want, err)
		}
	}
}

func TestTwoDocsForOneCodeFail(t *testing.T) {
	_, err := fixture(t, "dup").codes()
	if want := "both describe dup.twice"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("the error lacks %q: %v", want, err)
	}
}

func TestTheScannerListsEveryKey(t *testing.T) {
	sections, err := fixture(t, "good").sections()
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Name != "use" {
		t.Fatalf("sections: %+v", sections)
	}
	want := []Field{
		{Key: "deep", Type: "int", Doc: "Deep is in an embedded struct.", index: []int{0, 0}},
		{Key: "top", Type: "string", Doc: "Top is a top-level scalar.", Secret: true, Env: true, index: []int{1}},
		{Key: "items", Type: "map[string]Item", Doc: "Items are items.", index: []int{2}},
		{Key: "items.<name>.name", Type: "string", Doc: "Name names it."},
		{Key: "items.<name>.next", Type: "[]Other", Doc: "Next is recursive through a slice, which ends."},
		{Key: "items.<name>.next[].value", Type: "string", Doc: "Value is a value."},
		{Key: "a", Type: "int", Doc: "A and B share a doc.", Env: true, index: []int{3}},
		{Key: "b", Type: "int", Doc: "A and B share a doc.", Env: true, index: []int{4}},
		{Key: "c", Type: "int", Env: true, index: []int{5}},
	}
	if got := sections[0].Fields; !reflect.DeepEqual(got, want) {
		t.Errorf("fields:\n got %+v\nwant %+v", got, want)
	}
}

func TestMeaning(t *testing.T) {
	for _, tc := range []struct {
		c    Code
		want string
	}{
		{Code{Kinds: []string{"Conflict"}, Doc: "Conflict, retryable: rerunning the whole transaction can succeed.", Note: "SQLSTATE 40001"}, "Rerunning the whole transaction can succeed. Retryable. SQLSTATE 40001."},
		{Code{Kinds: []string{"Conflict"}, Doc: "Conflict: the data already exists.", Note: "23505"}, "The data already exists. SQLSTATE 23505."},
		{Code{Kinds: []string{"DeadlineExceeded"}, Doc: "DeadlineExceeded: statement_timeout canceled the statement."}, "statement_timeout canceled the statement."},
		{Code{Messages: []string{"no such route", "no route %d"}}, "Message: “no such route”; “no route %d”."},
		{Code{Kinds: []string{"NotFound"}, Doc: "Apply: a word that isn't the code's kind stays."}, "Apply: a word that isn't the code's kind stays."},
		{Code{Code: "galleon.panic"}, "A handler panicked. The server recovered and logged it."},
	} {
		if got := meaning(tc.c); got != tc.want {
			t.Errorf("meaning(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"the data":                   "The data.",
		"éclair first":               "Éclair first.",
		"pg.Config is read":          "pg.Config is read.",
		"statement_timeout canceled": "statement_timeout canceled.",
		"ReadYourWrites read":        "ReadYourWrites read.",
		"and so on…":                 "And so on…",
	} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"a | b {c} <d> [e] *f* g\\h": `a \| b \{c\} &lt;d&gt; \[e] \*f\* g\\h`,
		"keep `a_b{c}*` as code":     "keep `a_b{c}*` as code",
		"pipe in `a|b`":              "pipe in `a\\|b`",
		"unclosed `a_b":              "unclosed `a\\_b",
	} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute: "1m", 90 * time.Second: "1m30s", time.Hour: "1h", 2*time.Hour + time.Minute: "2h1m",
		100 * time.Millisecond: "100ms", 30 * time.Second: "30s",
	} {
		if got := duration(d); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTheScannerFindsTheStatusOverrides(t *testing.T) {
	got, err := fixture(t, "good").statuses()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"server.method": 405, "server.format": 415}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

func TestDefaultOfRefusesWhatItCantWrite(t *testing.T) {
	v := reflect.ValueOf(struct{ L []string }{L: []string{"a"}})
	if _, ok := defaultOf(v, []int{0}); ok {
		t.Error("a non-empty slice default was written as blank")
	}
}

func TestDefaultOf(t *testing.T) {
	type inner struct{ N int }
	type section struct {
		inner
		D time.Duration
		P *inner
		S string
	}
	v := reflect.ValueOf(section{inner: inner{N: 3}, D: time.Minute})
	for _, tc := range []struct {
		index []int
		want  string
	}{
		{[]int{0, 0}, "`3`"}, {[]int{1}, "`1m`"}, {[]int{2, 0}, ""}, {[]int{3}, ""}, {nil, ""},
	} {
		if got, _ := defaultOf(v, tc.index); got != tc.want {
			t.Errorf("defaultOf(%v) = %q, want %q", tc.index, got, tc.want)
		}
	}
}
