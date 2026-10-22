package security_test

import (
	"context"
	"strings"
	"testing"

	"github.com/thec1oud/galleon/security"
)

func TestRoleTableGrants(t *testing.T) {
	table := &security.RoleTable{Grants: map[string][]string{
		"admin":                    {"*"},
		"editor":                   {"notes:*"},
		"tagger":                   {"notes:tags:*"},
		security.RoleAuthenticated: {"profile:read"},
	}}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	user := func(roles ...string) security.Principal {
		return security.Identity{Subject: "u", Kind: security.KindUser, Claims: security.NewValues(map[string]any{"roles": roles})}.Principal()
	}
	cases := []struct {
		p      security.Principal
		action string
		allow  bool
	}{
		{user("admin"), "anything:at_all", true},
		{user("editor"), "notes:create", true},
		{user("editor"), "notes:tags:add", true},
		{user("editor"), "notes", false},
		{user("editor"), "notesx:create", false},
		{user("tagger"), "notes:tags:add", true},
		{user("tagger"), "notes:create", false},
		{user(), "profile:read", true},
		{user(), "notes:create", false},
		{security.Anonymous(), "profile:read", false},
		{user("admin"), "", false},
	}
	for _, c := range cases {
		dec, err := table.Check(context.Background(), security.Query{Subject: c.p, Action: c.action})
		if err != nil || dec.Allow != c.allow {
			t.Errorf("%v %q: allow %v (%s), want %v", c.p.Claims(), c.action, dec.Allow, dec.Reason, c.allow)
		}
	}
	// Roles from the context, rather than a claim.
	inOrg := &security.RoleTable{
		Grants: map[string][]string{"owner": {"org:delete"}},
		Roles: func(p security.Principal, c security.OperatingContext) []string {
			if c != nil && c.ID() == "acme" {
				return []string{"owner"}
			}
			return nil
		},
	}
	q := security.Query{Subject: user(), Action: "org:delete", Context: security.ContextData{Domain: "org", ID: "acme"}.Context()}
	if dec, _ := inOrg.Check(context.Background(), q); !dec.Allow {
		t.Fatal("the context's role wasn't used")
	}
}

func TestRoleTableValidates(t *testing.T) {
	for _, bad := range []string{"", "notes*", "*:notes", "notes:*:*", ":*", "notes:**"} {
		err := (&security.RoleTable{Grants: map[string][]string{"r": {bad}}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "an action is a name") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := (&security.RoleTable{Grants: map[string][]string{"": {"x"}}}).Validate(); err == nil {
		t.Error("an empty role was accepted")
	}
}

func TestFilterMatch(t *testing.T) {
	attrs := security.NewValues(map[string]any{"owner": "ann", "org": "acme", "n": 3})
	cases := []struct {
		f    security.Filter
		want bool
	}{
		{security.Filter{}, false},
		{security.Always(), true},
		{security.Eq("owner", "ann"), true},
		{security.Eq("owner", "bob"), false},
		{security.Eq("missing", nil), false},
		{security.Eq("n", 3.0), false}, // an int isn't a float64
		{security.In("org", "x", "acme"), true},
		{security.And(security.Eq("owner", "ann"), security.Eq("org", "acme")), true},
		{security.And(), false},
		{security.Or(security.Eq("owner", "bob"), security.Eq("org", "acme")), true},
		{security.Or(), false},
		{security.Not(security.Eq("owner", "bob")), true},
		{security.Not(security.Filter{Op: 99}), false}, // a malformed filter, negated, still matches nothing
		{security.Filter{Op: security.FilterNot}, false},
		{security.Eq("owner", []string{"ann"}), false}, // not comparable
	}
	for _, c := range cases {
		if got := c.f.Match(attrs); got != c.want {
			t.Errorf("%v: %v, want %v", c.f, got, c.want)
		}
	}
}

func TestValuesAreACopy(t *testing.T) {
	m := map[string]any{"roles": []any{"a", "b"}, "one": "x", "mixed": []any{"a", 1}}
	v := security.NewValues(m)
	m["one"] = "changed"
	if s, _ := v.String("one"); s != "x" {
		t.Fatal("NewValues didn't copy")
	}
	if r, ok := v.Strings("roles"); !ok || len(r) != 2 {
		t.Fatalf("roles: %v", r)
	}
	if r, ok := v.Strings("one"); !ok || r[0] != "x" {
		t.Fatalf("a lone string: %v", r)
	}
	if _, ok := v.Strings("mixed"); ok {
		t.Fatal("a mixed list read as strings")
	}
	w := v.With("one", "y")
	if s, _ := v.String("one"); s != "x" || w.Len() != 3 {
		t.Fatal("With changed the original")
	}
}
