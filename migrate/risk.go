package migrate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Risk is how dangerous a step is, on a scale every tool shares. In order:
// Safe, Caution, Destructive, Manual, then Unknown, which ranks above them
// all. The zero value is Unknown, so a step nobody classified is never
// taken for safe. A tool that can't classify a step reports Unknown, and
// the deploy policy decides what Unknown means (see Policy.Unknown).
type Risk uint8

const (
	// Unknown: the tool can't say.
	Unknown Risk = iota
	// Safe: no effect on running code or data, such as adding a table or a
	// nullable column.
	Safe
	// Caution: safe for data, but it may lock or slow the database, or
	// affect running code, such as adding an index or a constraint.
	Caution
	// Destructive: data can be lost, such as dropping a column.
	Destructive
	// Manual: a person must act, or check, before or after it runs.
	Manual
)

var riskNames = [...]string{Unknown: "unknown", Safe: "safe", Caution: "caution", Destructive: "destructive", Manual: "manual"}

func (r Risk) String() string {
	if int(r) < len(riskNames) {
		return riskNames[r]
	}
	return fmt.Sprintf("risk(%d)", uint8(r))
}

func (r Risk) rank() int {
	if r == Unknown || int(r) >= len(riskNames) {
		return len(riskNames)
	}
	return int(r)
}

// Above reports whether r ranks above max.
func (r Risk) Above(max Risk) bool { return r.rank() > max.rank() }

// ParseRisk parses a risk's name, in any case.
func ParseRisk(s string) (Risk, error) {
	for r, name := range riskNames {
		if strings.EqualFold(strings.TrimSpace(s), name) {
			return Risk(r), nil
		}
	}
	return Unknown, fmt.Errorf("unknown risk %q: want safe, caution, destructive, manual or unknown", s)
}

func (r Risk) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

func (r *Risk) UnmarshalText(b []byte) error {
	v, err := ParseRisk(string(b))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// Fingerprint returns a fingerprint of steps: a SHA-256 over each step's ID,
// description and risk, and content[i], when given, as step i's content
// beyond those, such as its SQL text.
func Fingerprint(steps []StepInfo, content [][]byte) string {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for i, s := range steps {
		field([]byte(s.ID))
		field([]byte(s.Description))
		field([]byte{byte(s.Risk)})
		if i < len(content) {
			field(content[i])
		} else {
			field(nil)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
