package main

import (
	"context"
	"slices"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/apikey"
)

// docs:start policy
// roles grants actions to roles. Every caller with a key has the
// "authenticated" role.
var roles = &security.RoleTable{Grants: map[string][]string{
	security.RoleAuthenticated: {"bookmarks:create", "bookmarks:read"},
	"admin":                    {"bookmarks:*"},
}}

// policy adds one rule the roles can't express: a bookmark is read only by
// its owner, or an admin.
func policy(ctx context.Context, q security.Query) (security.Decision, error) {
	if q.Resource != nil && q.Resource.Type == "bookmark" {
		owner, _ := q.Resource.Attributes.String("owner")
		callerRoles, _ := q.Subject.Claims().Strings("roles")
		if owner != q.Subject.Subject() && !slices.Contains(callerRoles, "admin") {
			return security.Deny("not the owner"), nil
		}
	}
	return roles.Check(ctx, q)
}

// docs:end policy

// docs:start security
// Security is the security module: callers authenticate with an API key,
// which Keys looks up, and policy decides.
func Security() galleon.Module {
	return security.Module(
		security.AuthenticateWith("apikey", func(keys *Keys) security.Authenticator {
			return apikey.New(keys)
		}),
		security.Decide(security.DeciderFunc(policy)),
	)
}

// docs:end security
