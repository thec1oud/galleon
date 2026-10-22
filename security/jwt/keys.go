package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// key is one verification key from a JWK set.
type key struct {
	kid string
	alg string // the JWK's alg, if it names one
	pub crypto.PublicKey
}

// jwk is a JSON Web Key (RFC 7517), as much of it as verification reads.
type jwk struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	Alg    string   `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC and OKP
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseKeySet parses a JWK set, keeping the keys that can verify a
// signature: it skips encryption keys and keys of a type or curve it doesn't
// support, and fails on a signing key that is malformed or too weak, and on
// a set with no usable key.
func parseKeySet(data []byte) ([]key, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("the JWK set isn't valid JSON: %w", err)
	}
	var keys []key
	for i, raw := range set.Keys {
		var j jwk
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if (j.Use != "" && j.Use != "sig") || (len(j.KeyOps) > 0 && !slices.Contains(j.KeyOps, "verify")) {
			continue // not for verifying signatures
		}
		pub, err := j.publicKey()
		if errors.Is(err, errUnknownKeyType) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("key %d (kid %q): %w", i, j.Kid, err)
		}
		if j.Alg != "" && !compatible(j.Alg, pub) {
			return nil, fmt.Errorf("key %d (kid %q): its alg %q doesn't suit a %s key", i, j.Kid, j.Alg, j.Kty)
		}
		keys = append(keys, key{kid: j.Kid, alg: j.Alg, pub: pub})
	}
	if len(keys) == 0 {
		return nil, errors.New("the JWK set has no key for verifying signatures")
	}
	return keys, nil
}

var errUnknownKeyType = errors.New("unknown key type")

func (j *jwk) publicKey() (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		n, err := b64int(j.N)
		if err != nil {
			return nil, fmt.Errorf("n: %w", err)
		}
		e, err := b64int(j.E)
		if err != nil {
			return nil, fmt.Errorf("e: %w", err)
		}
		if n.BitLen() < 2048 {
			return nil, fmt.Errorf("the RSA key has %d bits; at least 2048 are needed", n.BitLen())
		}
		if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
			return nil, errors.New("the RSA key's exponent is invalid")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch j.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errUnknownKeyType // such as secp256k1
		}
		size := (curve.Params().BitSize + 7) / 8
		x, err1 := b64(j.X)
		y, err2 := b64(j.Y)
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, fmt.Errorf("the EC key's coordinates must be %d bytes each, base64url-encoded", size)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, fmt.Errorf("the EC key isn't a point on %s: %w", j.Crv, err)
		}
		return pub, nil
	case "OKP":
		if j.Crv != "Ed25519" {
			return nil, errUnknownKeyType // such as X25519, for key agreement
		}
		x, err := b64(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("the Ed25519 key must be 32 bytes, base64url-encoded")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errUnknownKeyType
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

func b64int(s string) (*big.Int, error) {
	b, err := b64(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("not a base64url-encoded integer")
	}
	return new(big.Int).SetBytes(b), nil
}
