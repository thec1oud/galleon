# 0205. Verify bearer tokens only with an issuer's published asymmetric keys

Status: Proposed
Date: 2026-10-22

## Context

JSON Web Tokens can be signed with an asymmetric key, which an issuer
publishes as a JWK set, or with a shared HMAC secret. Libraries that accept
both have repeatedly been tricked: a token signed with HMAC, using the
issuer's public key as the secret, verifies when the verifier picks the
algorithm from the token's header. The `none` algorithm is the other
classic. A shared secret also has to be copied to every service that
verifies, and can't be rotated through a published key set.

## Decision

- Galleon's bearer authenticator, `security/jwt`, accepts only RS256,
  RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512 and EdDSA. HMAC
  and `none` are never accepted, whatever the config says.
- A key verifies only algorithms that suit its type and size, and only
  the one it names if it names one, so a token's header can't choose a
  different use for a key.
- Keys come from the issuer's JWK set, found through OpenID Connect
  discovery or configured, and are refreshed in the background. A token
  must also pass `iss`, `aud`, `exp` and `nbf`, with at most five minutes
  of allowance for clock skew; `exp` is required.

## Consequences

- The algorithm confusion attacks have nothing to work with.
- An issuer that only signs with a shared secret can't be used with the
  built-in authenticator. An app that must can write an `Authenticator` of
  its own, behind the same seam.
- A token without an expiry is refused, so every accepted token runs out.

## Alternatives considered

- **Accept HMAC with a configured secret.** Common in small setups, but it
  brings back the confusion risk and a secret on every service.
- **Pick the algorithm from the token's header.** The cause of the attacks
  above.

## Sources

- [RFC 8725: JSON Web Token Best Current Practices](https://www.rfc-editor.org/rfc/rfc8725)
- [Part 6, §7.1, rule 2: token verification](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#71-identity)
