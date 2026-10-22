// Package apikey authenticates API keys. Keys are stored only as digests,
// and looked up through a seam, so they can live in a database, a secret
// store or, for a few fixed keys, a map:
//
//	key, digest, err := apikey.Generate("bk") // show key once; store digest
//
//	security.Module(
//		security.AuthenticateWith("apikey", func(s *Store) security.Authenticator {
//			return apikey.New(s) // *Store implements apikey.Lookup
//		}),
//		security.Decide(roles),
//	)
//
// A key is sent in the X-API-Key header, unless New is told another. Keys
// must be long and random, as Generate makes them, since a digest is a
// plain SHA-256: a key that could be guessed could be brute-forced from it.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
)

// Digest is the SHA-256 digest of a key, which is what is stored.
type Digest [sha256.Size]byte

// Hash returns key's digest.
func Hash(key string) Digest { return sha256.Sum256([]byte(key)) }

func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// ParseDigest parses a digest as String writes it.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(d) {
		return d, errors.New("apikey: a digest is 64 hexadecimal digits")
	}
	copy(d[:], b)
	return d, nil
}

var prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// Generate returns a new key, made of prefix, an underscore and 32 random
// bytes in base64url, and its digest. The prefix, such as "bk", tells what
// a leaked key is for; it is up to 16 lower-case letters and digits.
func Generate(prefix string) (key string, digest Digest, err error) {
	if !prefixPattern.MatchString(prefix) {
		return "", Digest{}, errors.New("apikey: the prefix must be 1 to 16 lower-case letters and digits, starting with a letter")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Digest{}, err
	}
	key = prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
	return key, Hash(key), nil
}

// Lookup finds the principal a key's digest belongs to. It returns
// ErrUnknownKey for a digest of no key, or of a revoked or expired one; any
// other error means it couldn't tell, and the call fails as Unavailable.
// The principal it returns is usually of security.KindAPIKey.
type Lookup interface {
	Lookup(ctx context.Context, digest Digest) (security.Principal, error)
}

// ErrUnknownKey is what a Lookup returns for a key it doesn't know.
var ErrUnknownKey = errors.New("apikey: unknown key")

// Keys is a fixed set of keys, by digest: a Lookup for keys set in config
// or code.
type Keys map[Digest]security.Principal

func (k Keys) Lookup(_ context.Context, d Digest) (security.Principal, error) {
	if p, ok := k[d]; ok && p != nil {
		return p, nil
	}
	return nil, ErrUnknownKey
}

// Option configures an Authenticator.
type Option func(*Authenticator)

// Header sets the header the key is read from, X-API-Key by default.
func Header(name string) Option { return func(a *Authenticator) { a.header = name } }

// Scheme sets the scheme name, "apikey" by default.
func Scheme(name string) Option { return func(a *Authenticator) { a.scheme = name } }

// Authenticator authenticates the key in a request header.
type Authenticator struct {
	lookup Lookup
	header string
	scheme string
}

// maxKeyLength bounds a key, so a huge header isn't hashed.
const maxKeyLength = 512

// New returns an Authenticator that finds keys through l.
func New(l Lookup, opts ...Option) *Authenticator {
	a := &Authenticator{lookup: l, header: "X-API-Key", scheme: "apikey"}
	for _, o := range opts {
		o(a)
	}
	return a
}

func (a *Authenticator) Scheme() string { return a.scheme }

// Authenticate looks up the request's key. A request without the header
// has no credentials for it.
func (a *Authenticator) Authenticate(ctx context.Context, r *op.Request) (security.Principal, error) {
	values := r.Header.Values(a.header)
	switch {
	case len(values) == 0:
		return nil, security.ErrNoCredentials
	case len(values) > 1:
		return nil, security.InvalidCredentials("the request has more than one API key", nil)
	case values[0] == "" || len(values[0]) > maxKeyLength:
		return nil, security.InvalidCredentials("the API key is malformed", nil)
	}
	p, err := a.lookup.Lookup(ctx, Hash(values[0]))
	switch {
	case errors.Is(err, ErrUnknownKey):
		return nil, security.InvalidCredentials("the API key is not valid", err)
	case err != nil:
		return nil, err // the module fails the call as Unavailable, unless it is Unauthenticated
	case security.IsAnonymous(p):
		return nil, security.InvalidCredentials("the API key is not valid", errors.New("the lookup returned no principal"))
	}
	return p, nil
}
