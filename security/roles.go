package security

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RoleAuthenticated is a role every principal but the anonymous one has, so
// a RoleTable can grant an action to anyone signed in.
const RoleAuthenticated = "authenticated"

// RoleTable is the default decider: it grants actions to roles, and denies
// everything it doesn't grant.
//
//	&security.RoleTable{Grants: map[string][]string{
//		"admin":  {"*"},
//		"editor": {"bookmarks:*"},
//		security.RoleAuthenticated: {"bookmarks:read"},
//	}}
//
// An action is granted by its exact name, by "*", or by a prefix ending in
// ":*", which grants every action under it: "bookmarks:*" grants
// "bookmarks:create" and "bookmarks:tags:add", but not "bookmarks".
//
// It never allows the anonymous principal: an operation anyone may call is
// Public. It decides by action alone, so a resource check passes whenever
// the action does. Checks that depend on the resource, such as ownership,
// need a decider that looks at it; a DeciderFunc can wrap a RoleTable for
// the rest.
type RoleTable struct {
	// Grants maps each role to the actions it grants.
	Grants map[string][]string
	// Roles returns the principal's roles in its context. Nil reads the
	// principal's "roles" claim, as a list of strings or a single string.
	// Either way, every principal but the anonymous one also has
	// RoleAuthenticated.
	Roles func(p Principal, c OperatingContext) []string
}

// Validate checks the grants: every role and action is non-empty, and a
// wildcard is "*" or ends a prefix as ":*".
func (t *RoleTable) Validate() error {
	if t == nil {
		return errors.New("security: the role table is nil")
	}
	var problems []error
	roles := make([]string, 0, len(t.Grants))
	for role := range t.Grants {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if role == "" {
			problems = append(problems, errors.New("a role is empty"))
		}
		for _, a := range t.Grants[role] {
			if a == "" || (strings.Contains(a, "*") && a != "*" && (!strings.HasSuffix(a, ":*") || strings.Count(a, "*") > 1 || a == ":*")) {
				problems = append(problems, fmt.Errorf("role %q grants %q: an action is a name, \"*\", or a prefix ending in \":*\"", role, a))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("security: role table: %w", errors.Join(problems...))
	}
	return nil
}

func (t *RoleTable) Check(_ context.Context, q Query) (Decision, error) {
	if IsAnonymous(q.Subject) {
		return Deny("anonymous"), nil
	}
	var roles []string
	if t.Roles != nil {
		roles = t.Roles(q.Subject, q.Context)
	} else {
		roles, _ = q.Subject.Claims().Strings("roles")
	}
	for _, role := range append(roles[:len(roles):len(roles)], RoleAuthenticated) {
		for _, a := range t.Grants[role] {
			if grants(a, q.Action) {
				return Allow("role " + role + " grants " + a), nil
			}
		}
	}
	return Deny("no role grants " + q.Action), nil
}

func (t *RoleTable) CheckMany(ctx context.Context, qs []Query) ([]Decision, error) {
	return checkEach(ctx, t, qs)
}

// grants reports whether the grant g covers action.
func grants(g, action string) bool {
	switch {
	case g == "*":
		return action != ""
	case strings.HasSuffix(g, ":*"):
		prefix := g[:len(g)-1] // keeps the colon
		return len(action) > len(prefix) && strings.HasPrefix(action, prefix)
	}
	return g == action
}
