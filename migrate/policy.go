package migrate

import (
	"fmt"
	"strings"
	"time"

	"github.com/thec1oud/galleon/errs"
)

// Mode is what the kernel does with migrators at boot.
type Mode uint8

const (
	// ModeOff checks nothing: no status is read and no readiness gate is
	// registered.
	ModeOff Mode = iota + 1
	// ModeVerify reads each migrator's status and gates readiness on it,
	// but never applies anything: another node, or `migrate apply`, does.
	// It is the production default.
	ModeVerify
	// ModeApply applies every migrator under the cluster lock, in order,
	// then gates readiness on the status read again after the apply. It is
	// the default in development and tests.
	ModeApply
)

var modeNames = [...]string{ModeOff: "off", ModeVerify: "verify", ModeApply: "apply"}

func (m Mode) String() string {
	if m >= ModeOff && int(m) < len(modeNames) {
		return modeNames[m]
	}
	return "unset"
}

// ParseMode parses "off", "verify" or "apply".
func ParseMode(s string) (Mode, error) {
	for m, name := range modeNames {
		if m > 0 && strings.EqualFold(strings.TrimSpace(s), name) {
			return Mode(m), nil
		}
	}
	return 0, fmt.Errorf("unknown migration mode %q: want off, verify or apply", s)
}

// Policy is the deploy policy the Runner enforces.
type Policy struct {
	// Mode is what the kernel does at boot. The Runner itself ignores it.
	Mode Mode
	// MaxRisk is the riskiest step a deploy may apply. A plan with a step
	// above it is refused before anything is applied.
	MaxRisk Risk
	// AllowUnknown lets steps of Unknown risk run, and migrators that can't
	// plan apply at all, since all their changes are Unknown. Production
	// refuses them by default: Unknown requires a person's approval, given
	// by setting this for the deploy.
	AllowUnknown bool
	// TolerateAhead counts a database newer than this build as ready, for
	// the old nodes of a rolling deploy.
	TolerateAhead bool
	// Approved pins, per migrator, the fingerprint of the plan CI approved.
	// Apply refuses any other plan, and a migrator that can't plan can't be
	// pinned.
	Approved map[string]string
	// LockTTL is the lease's time to live; the Runner renews it at a third
	// of that while an apply runs. The default is 30s.
	LockTTL time.Duration
	// LockWait bounds how long a node waits for another node's apply
	// before it gives up; zero waits as long as the context allows.
	LockWait time.Duration
	// Debug puts step descriptions, which may hold SQL text, into span
	// attributes and logs. Off, only step IDs and risks are recorded.
	Debug bool
}

// ProductionPolicy is the policy of the production profile: verify at boot,
// apply up to Caution, refuse Unknown.
func ProductionPolicy() Policy {
	return Policy{Mode: ModeVerify, MaxRisk: Caution}
}

// DevelopmentPolicy is the policy of the development and test profiles:
// apply at boot, whatever the risk.
func DevelopmentPolicy() Policy {
	return Policy{Mode: ModeApply, MaxRisk: Manual, AllowUnknown: true}
}

func (p Policy) lockTTL() time.Duration {
	if p.LockTTL > 0 {
		return p.LockTTL
	}
	return 30 * time.Second
}

// Ready reports whether a migrator in status s lets the node take traffic:
// UpToDate does, Ahead does only with TolerateAhead, and Pending and
// Drifted never do. The error says why not.
func (p Policy) Ready(name string, s Status) error {
	detail := ""
	if s.Detail != "" {
		detail = ": " + s.Detail
	}
	switch s.State {
	case UpToDate:
		return nil
	case Ahead:
		if p.TolerateAhead {
			return nil
		}
		return errs.New(errs.Unavailable, CodeNotReady, fmt.Sprintf("migrator %q: the database is ahead of this build (database %s, build %s)%s; tolerate it during a rolling deploy with tolerate_ahead", name, s.Current, s.Expected, detail))
	case Pending:
		return errs.New(errs.Unavailable, CodeNotReady, fmt.Sprintf("migrator %q: changes are pending (database %s, build %s)%s; apply them with mode apply or migrate apply", name, s.Current, s.Expected, detail))
	case Drifted:
		return errs.New(errs.Unavailable, CodeNotReady, fmt.Sprintf("migrator %q: the database has drifted from this build (database %s, build %s)%s", name, s.Current, s.Expected, detail))
	}
	return errs.New(errs.Unavailable, CodeNotReady, fmt.Sprintf("migrator %q: status reports no state", name))
}

// check returns why the policy refuses plan pl of migrator name, or nil.
func (p Policy) check(name string, pl Plan) error {
	if want, ok := p.Approved[name]; ok && want != pl.Fingerprint() {
		return errs.New(errs.FailedPrecondition, CodeFingerprintMismatch, fmt.Sprintf("migrator %q: the plan's fingerprint is %s, but %s was approved; review the new plan and approve it", name, pl.Fingerprint(), want))
	}
	var refused []string
	for _, s := range pl.Steps() {
		if !Allowed(s.Risk, p.MaxRisk, p.AllowUnknown) {
			refused = append(refused, fmt.Sprintf("%s (%s)", s.ID, s.Risk))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	why := fmt.Sprintf("the maximum risk is %s", p.MaxRisk)
	if !p.AllowUnknown {
		why += ", and unknown risk needs approval (allow_unknown)"
	}
	return errs.New(errs.FailedPrecondition, CodeRiskRefused, fmt.Sprintf("migrator %q: the policy refuses %s, since %s; nothing was applied", name, strings.Join(refused, ", "), why))
}
