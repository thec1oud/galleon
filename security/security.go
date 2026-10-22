// Package security is the security plane: who is calling (identity), in
// which operating context (context resolution), and whether they may do what
// they ask (decisions). Each is a seam any identity or authorization system
// can fill, and Module runs them as the operation pipeline's security
// stages:
//
//	authenticate (3)     the operation's authenticators produce a Principal,
//	                     or the anonymous one
//	resolve context (4)  the ContextResolver produces an OperatingContext
//	gate (5)             the operation accepts that context's domain
//	pre-authorize (5b)   the Decider allows the action, before decode
//	authorize (8)        the Decider allows the action on the resource the
//	                     decoded input names
//
// Access is denied by default: an operation that isn't Public needs a
// decider's allowing decision, and an engine error, an unknown obligation or
// an expired context denies.
//
//	galleon.Modules(
//		server.Module(),
//		security.Module(
//			jwt.Bearer(),
//			security.Decide(&security.RoleTable{Grants: map[string][]string{
//				"editor": {"bookmarks:*"},
//			}}),
//		),
//	)
//
// Handlers reach the caller through Caller and Context, and ask for
// further decisions with Can, Require and CanEach; decisions are cached for
// the rest of the call.
package security

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// PrincipalKind is what kind of caller a principal is.
type PrincipalKind uint8

const (
	// KindAnonymous is a caller that presented no credentials.
	KindAnonymous PrincipalKind = iota
	// KindUser is a person.
	KindUser
	// KindService is another service, calling as itself.
	KindService
	// KindAPIKey is a caller identified by an API key.
	KindAPIKey
)

func (k PrincipalKind) String() string {
	switch k {
	case KindAnonymous:
		return "anonymous"
	case KindUser:
		return "user"
	case KindService:
		return "service"
	case KindAPIKey:
		return "api_key"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Principal is a verified caller.
type Principal interface {
	// Subject is the caller's stable ID, unique within its Issuer. It is
	// empty only for the anonymous principal.
	Subject() string
	Kind() PrincipalKind
	// Issuer names who vouched for the caller, such as a token's issuer.
	Issuer() string
	// Claims are the verified claims about the caller.
	Claims() Claims
	// Assurance is how strongly the caller was authenticated.
	Assurance() Assurance
}

// Assurance is how, when and how strongly a principal was authenticated.
type Assurance struct {
	// AuthTime is when the caller last authenticated actively, if known.
	AuthTime time.Time
	// Methods are how, as RFC 8176 values such as "pwd", "otp" or "hwk".
	Methods []string
	// Level is an app-defined strength, higher being stronger; 0 is unknown.
	Level int
}

// Identity is the data of a principal. Its Principal method makes one, for
// authenticators and tests:
//
//	p := security.Identity{Subject: "u_1", Kind: security.KindUser}.Principal()
type Identity struct {
	Subject   string
	Kind      PrincipalKind
	Issuer    string
	Claims    Claims
	Assurance Assurance
}

// Principal returns a Principal with the identity's data. Later changes to
// the Assurance's Methods slice don't reach it.
func (i Identity) Principal() Principal {
	i.Assurance.Methods = slices.Clone(i.Assurance.Methods)
	return identity{i}
}

type identity struct{ i Identity }

func (p identity) Subject() string     { return p.i.Subject }
func (p identity) Kind() PrincipalKind { return p.i.Kind }
func (p identity) Issuer() string      { return p.i.Issuer }
func (p identity) Claims() Claims      { return p.i.Claims }
func (p identity) Assurance() Assurance {
	a := p.i.Assurance
	a.Methods = slices.Clone(a.Methods)
	return a
}

var anonymous Principal = Identity{Kind: KindAnonymous}.Principal()

// Anonymous returns the principal of a caller that presented no
// credentials: its kind is KindAnonymous and its subject is empty.
func Anonymous() Principal { return anonymous }

// IsAnonymous reports whether p is nil or of KindAnonymous.
func IsAnonymous(p Principal) bool { return p == nil || p.Kind() == KindAnonymous }

// Authenticator produces a principal from a request's credentials, for one
// scheme, such as "bearer" or "apikey". An operation lists the schemes it
// accepts with op.Builder.Schemes; one that lists none accepts every scheme
// the module has.
type Authenticator interface {
	// Scheme names the scheme. It is lower case, such as "bearer".
	Scheme() string
	// Authenticate returns the principal the request's credentials prove.
	// It returns ErrNoCredentials when the request carries none for this
	// scheme, so that the next scheme is tried, and an Unauthenticated
	// errs.Error, such as InvalidCredentials returns, when it carries some
	// that don't verify; no other scheme is tried after that. Any other
	// error means the authenticator couldn't decide, and the call fails as
	// Unavailable.
	Authenticate(ctx context.Context, r *op.Request) (Principal, error)
}

// ErrNoCredentials is what an Authenticator returns for a request that
// carries no credentials for its scheme.
var ErrNoCredentials = errors.New("security: no credentials for this scheme")

// Challenger is an Authenticator that names how a client should
// authenticate. Its challenge is sent as a WWW-Authenticate header when a
// call fails for want of authentication.
type Challenger interface {
	// Challenge returns a WWW-Authenticate value, such as `Bearer`.
	Challenge() string
}

// InvalidCredentials returns the error an Authenticator returns for
// credentials that are present but don't verify. message is safe for
// clients; cause is logged, never sent.
func InvalidCredentials(message string, cause error) error {
	if message == "" {
		message = "the credentials are invalid"
	}
	return errs.Wrap(cause, errs.Unauthenticated, CodeInvalidCredentials, message)
}
