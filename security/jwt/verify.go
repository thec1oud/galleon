package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	_ "crypto/sha256" // the hashes the algorithms use
	_ "crypto/sha512"
	"errors"
	"math/big"
)

// algorithm is a JWS signature algorithm (RFC 7518 §3, RFC 8037).
type algorithm struct {
	hash crypto.Hash // zero for EdDSA, which hashes internally
	kind string      // "rsa", "pss", "ecdsa" or "eddsa"
	size int         // an ECDSA signature's byte length
}

// algorithms are the algorithms Authenticator can verify. HMAC is left out
// on purpose: a shared secret can't be published as a JWK set, and
// accepting it invites the classic confusion of a public key used as an
// HMAC secret.
var algorithms = map[string]algorithm{
	"RS256": {crypto.SHA256, "rsa", 0},
	"RS384": {crypto.SHA384, "rsa", 0},
	"RS512": {crypto.SHA512, "rsa", 0},
	"PS256": {crypto.SHA256, "pss", 0},
	"PS384": {crypto.SHA384, "pss", 0},
	"PS512": {crypto.SHA512, "pss", 0},
	"ES256": {crypto.SHA256, "ecdsa", 64},
	"ES384": {crypto.SHA384, "ecdsa", 96},
	"ES512": {crypto.SHA512, "ecdsa", 132},
	"EdDSA": {0, "eddsa", 0},
}

// compatible reports whether a key of pub's type and size can verify alg.
func compatible(alg string, pub crypto.PublicKey) bool {
	a, ok := algorithms[alg]
	if !ok {
		return false
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return a.kind == "rsa" || a.kind == "pss"
	case *ecdsa.PublicKey:
		return a.kind == "ecdsa" && (k.Curve.Params().BitSize+7)/8*2 == a.size
	case ed25519.PublicKey:
		return a.kind == "eddsa"
	}
	return false
}

var errSignature = errors.New("the signature doesn't verify")

// verifySignature checks sig over input with pub, under alg, which the
// caller has checked is allowed and compatible with pub.
func verifySignature(alg string, pub crypto.PublicKey, input, sig []byte) error {
	a := algorithms[alg]
	var digest []byte
	if a.hash != 0 {
		h := a.hash.New()
		h.Write(input)
		digest = h.Sum(nil)
	}
	ok := false
	switch a.kind {
	case "rsa":
		ok = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), a.hash, digest, sig) == nil
	case "pss":
		ok = rsa.VerifyPSS(pub.(*rsa.PublicKey), a.hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	case "ecdsa":
		if len(sig) == a.size {
			r := new(big.Int).SetBytes(sig[:a.size/2])
			s := new(big.Int).SetBytes(sig[a.size/2:])
			ok = ecdsa.Verify(pub.(*ecdsa.PublicKey), digest, r, s)
		}
	case "eddsa":
		ok = ed25519.Verify(pub.(ed25519.PublicKey), input, sig)
	}
	if !ok {
		return errSignature
	}
	return nil
}
