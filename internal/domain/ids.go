package domain

import (
	"fmt"
	"strconv"
	"strings"
	"uuid"
)

// NewID returns a new UUIDv7, the ID type of every entity.
func NewID() uuid.UUID {
	return uuid.NewV7()
}

// RefKind is the prefix of a human ref.
type RefKind string

// Human ref kinds. Numbers are allocated per account and kind, starting at 1.
const (
	RefGoal RefKind = "GOAL"
	RefSpec RefKind = "SPEC"
	RefTask RefKind = "TASK"
	RefJob  RefKind = "JOB"
	RefGate RefKind = "GATE"
)

// RefKinds returns every ref kind.
func RefKinds() []RefKind {
	return []RefKind{RefGoal, RefSpec, RefTask, RefJob, RefGate}
}

// Ref formats a human ref such as GOAL-3.
func Ref(kind RefKind, number int64) string {
	return string(kind) + "-" + strconv.FormatInt(number, 10)
}

// ParseRef parses a human ref such as GOAL-3 or goal-3 into its kind and number.
func ParseRef(s string) (RefKind, int64, error) {
	prefix, num, ok := strings.Cut(s, "-")
	if !ok {
		return "", 0, fmt.Errorf("%q: %w", s, ErrInvalidRef)
	}
	kind := RefKind(strings.ToUpper(prefix))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 1 || !kind.valid() {
		return "", 0, fmt.Errorf("%q: %w", s, ErrInvalidRef)
	}
	return kind, n, nil
}

func (k RefKind) valid() bool {
	switch k {
	case RefGoal, RefSpec, RefTask, RefJob, RefGate:
		return true
	}
	return false
}
