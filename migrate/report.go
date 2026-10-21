package migrate

import (
	"fmt"
	"io"
	"strings"
)

// The Write functions render reports for people, as `migrate status`,
// `migrate plan` and `migrate apply` print them. A capability a migrator
// lacks is said plainly, never left out.

// WriteStatus writes one block per migrator: its state, why it keeps the
// node out of traffic, its drift and its background tasks.
func WriteStatus(w io.Writer, reports []Report) error {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "%s:", r.Name)
		if r.Err != nil {
			fmt.Fprintf(&b, " status failed: %v\n", r.Err)
			continue
		}
		fmt.Fprintf(&b, " %s (database %s, build %s)", r.Status.State, orNone(r.Status.Current), orNone(r.Status.Expected))
		if r.Ready == nil {
			b.WriteString(", ready\n")
		} else {
			b.WriteString(", not ready\n")
		}
		if r.Status.Detail != "" {
			fmt.Fprintf(&b, "  %s\n", r.Status.Detail)
		}
		if len(r.Capabilities) == 0 {
			b.WriteString("  capabilities: none (it can't plan, verify or run background tasks)\n")
		} else {
			fmt.Fprintf(&b, "  capabilities: %s\n", strings.Join(r.Capabilities, ", "))
		}
		switch {
		case r.DriftErr != nil:
			fmt.Fprintf(&b, "  verify failed: %v\n", r.DriftErr)
		case r.Drift == nil:
			fmt.Fprintf(&b, "  verify: migrator %q can't verify\n", r.Name)
		case r.Drift.None():
			b.WriteString("  verify: no drift\n")
		default:
			for _, d := range r.Drift.Differences {
				fmt.Fprintf(&b, "  drift: %s\n", d)
			}
		}
		if r.BackgroundErr != nil {
			fmt.Fprintf(&b, "  background tasks failed: %v\n", r.BackgroundErr)
		}
		for _, t := range r.Background {
			switch {
			case t.Err != nil:
				fmt.Fprintf(&b, "  background %s: done failed: %v\n", t.ID, t.Err)
			case !t.Known:
				fmt.Fprintf(&b, "  background %s: pending? migrator %q can't say whether it is done\n", t.ID, r.Name)
			case t.Done:
				fmt.Fprintf(&b, "  background %s: done\n", t.ID)
			default:
				fmt.Fprintf(&b, "  background %s: pending\n", t.ID)
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WritePlans writes each migrator's plan, step by step, with what the
// policy would make of it.
func WritePlans(w io.Writer, plans []PlanReport) error {
	var b strings.Builder
	for _, p := range plans {
		if p.Err != nil {
			fmt.Fprintf(&b, "%s: %v\n", p.Name, p.Err)
			continue
		}
		steps := p.Plan.Steps()
		fmt.Fprintf(&b, "%s: %d step(s), max risk %s, fingerprint %s\n", p.Name, len(steps), p.Plan.MaxRisk(), p.Plan.Fingerprint())
		for _, s := range steps {
			fmt.Fprintf(&b, "  %s [%s] %s\n", s.ID, s.Risk, s.Description)
		}
		if p.Refused != nil {
			fmt.Fprintf(&b, "  refused: %v\n", p.Refused)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteApply writes what Apply did with each migrator.
func WriteApply(w io.Writer, reports []ApplyReport) error {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "%s: %s", r.Name, r.Outcome)
		if r.Waited {
			b.WriteString(" (after waiting for another node)")
		}
		if n := len(r.Result.Applied); n > 0 {
			fmt.Fprintf(&b, ", %d step(s)", n)
		}
		b.WriteString("\n")
		for _, s := range r.Result.Applied {
			fmt.Fprintf(&b, "  %s [%s]\n", s.ID, s.Risk)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  note: %s\n", n)
		}
		if r.Err != nil {
			fmt.Fprintf(&b, "  error: %v\n", r.Err)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
