package security

// The error codes of the security plane.
const (
	// The call needs an authenticated caller, and has none: no credentials
	// any of the operation's schemes accept were presented.
	CodeUnauthenticated = "security.unauthenticated"
	// Credentials were presented, and don't verify: a bad signature, an
	// expired token, an unknown API key.
	CodeInvalidCredentials = "security.invalid_credentials"
	// The decider denied the action, or the action on the resource.
	CodeDenied = "security.denied"
	// The operation doesn't accept the caller's operating context.
	CodeContextNotAccepted = "security.context_not_accepted"
	// The caller's operating context has expired.
	CodeContextExpired = "security.context_expired"
	// An authenticator, the context resolver, a context provider or the
	// decider failed, so no decision could be made, and the call is denied.
	// Retryable.
	CodeUnavailable = "security.unavailable"
	// Can, Require or CanEach was called with a context of no call the
	// security module serves.
	CodeNoCall = "security.no_call"
)
