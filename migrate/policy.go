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
	// above it is refused before anything is applied. The zero value means
	// Safe, the most restrictive.
	MaxRisk Risk
	// AllowUnknown lets steps of Unknown risk run, and migrators that can't
	// plan apply at all, since all their changes are Unknown. Production
	// refuses them by default: Unknown requires a person's approval, given
	// by setting this for the deploy.
	AllowUnknown bool
	// TolerateAhead counts a database newer than this build as ready, for
	// the old nodes of a rolling deploy.
	TolerateAhead bool
	// Approved pins, per migrator, the fingerprint of the plan CI approved:
	// the online plan, of the changes pending on the database, as
	// Runner.Plan(ctx, false) and WritePlans report it. An offline plan
	// covers the whole build, so its fingerprint differs. Apply refuses any
	// other plan, and an empty plan where the status is Pending, since what
	// it would apply is in no plan; a migrator with nothing pending has no
	// plan to check; and a migrator that can't plan can't be pinned.
	Approved map[string]string
	// LockTTL is the lease's time to live; the Runner renews it at a quarter
	// of that while an apply runs. The default is 30s; NewRunner refuses
	// one below MinLockTTL.
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

// DefaultLockTTL is the LockTTL of a Policy that sets none.
const DefaultLockTTL = 30 * time.Second

func (p Policy) lockTTL() time.Duration {
	if p.LockTTL > 0 {
		return p.LockTTL
	}
	return DefaultLockTTL
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

// planRisk is the risk the policy takes plan pl to have: its steps'
// highest, or Unknown when the status is pending but the plan is empty,
// since what would be applied is in no plan.
func planRisk(pl Plan, pending bool) Risk {
	if len(pl.Steps()) == 0 && pending {
		return Unknown
	}
	return pl.MaxRisk()
}

// check returns why the policy refuses plan pl of migrator name, or nil.
// pending says the migrator's status is Pending.
func (p Policy) check(name string, pl Plan, pending bool) error {
	if len(pl.Steps()) == 0 {
		if !pending {
			return nil // nothing to apply: no plan to approve or refuse
		}
		// What would be applied is in no plan: an approval can't vouch
		// for it, and its risk is unknown.
		if want, pinned := p.Approved[name]; pinned {
			return errs.New(errs.FailedPrecondition, CodeFingerprintMismatch, fmt.Sprintf("migrator %q: its status is pending, but its plan is empty, so it can't be checked against the approved fingerprint %s; nothing was applied", name, want))
		}
		if !Allowed(Unknown, p.MaxRisk, p.AllowUnknown) {
			return errs.New(errs.FailedPrecondition, CodeRiskRefused, fmt.Sprintf("migrator %q: its status is pending, but its plan is empty, so what it would apply is of unknown risk, which needs approval (allow_unknown); nothing was applied", name))
		}
		return nil
	}
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
	why := fmt.Sprintf("the maximum risk is %s", Ceiling(p.MaxRisk))
	if !p.AllowUnknown {
		why += ", and unknown risk needs approval (allow_unknown)"
	}
	return errs.New(errs.FailedPrecondition, CodeRiskRefused, fmt.Sprintf("migrator %q: the policy refuses %s, since %s; nothing was applied", name, strings.Join(refused, ", "), why))
}
