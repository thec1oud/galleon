package op

import (
	"context"

	"github.com/thec1oud/galleon/errs"
)

// Validator checks the shape of a decoded input (stage 7). It must be pure:
// no I/O, deterministic, and safe to run on a client too.
type Validator[T any] interface {
	Validate(ctx context.Context, v *T) *ValidationError
}

// MeaningValidator checks what an input means (stage 9). ctx carries a read
// transaction; the validator must not write.
type MeaningValidator[T any] interface {
	ValidateMeaning(ctx context.Context, v *T) *ValidationError
}

// ValidatorFunc adapts a function to Validator.
type ValidatorFunc[T any] func(ctx context.Context, v *T) *ValidationError

func (f ValidatorFunc[T]) Validate(ctx context.Context, v *T) *ValidationError { return f(ctx, v) }

// MeaningValidatorFunc adapts a function to MeaningValidator.
type MeaningValidatorFunc[T any] func(ctx context.Context, v *T) *ValidationError

func (f MeaningValidatorFunc[T]) ValidateMeaning(ctx context.Context, v *T) *ValidationError {
	return f(ctx, v)
}

// Violation is one problem with an input. Its Path is a JSON Pointer into the
// wire representation, such as "/items/3/quantity".
type Violation = errs.Violation

// ValidationError is the one validation result shape, shared by operations,
// forms and domain code. A nil *ValidationError means valid.
type ValidationError struct{ Violations []Violation }

// Add appends a violation and returns the error, creating it if e is nil.
func (e *ValidationError) Add(path, code string, params map[string]any) *ValidationError {
	if e == nil {
		e = &ValidationError{}
	}
	e.Violations = append(e.Violations, Violation{Path: path, Code: code, Params: params, MsgKey: "validation." + code})
	return e
}

// Merge returns the violations of e and o together. Either may be nil.
func (e *ValidationError) Merge(o *ValidationError) *ValidationError {
	switch {
	case e == nil || len(e.Violations) == 0:
		if o == nil || len(o.Violations) == 0 {
			return nil
		}
		return o
	case o == nil || len(o.Violations) == 0:
		return e
	}
	return &ValidationError{Violations: append(append([]Violation(nil), e.Violations...), o.Violations...)}
}

// Err converts e to an InvalidArgument error carrying its violations, or nil.
func (e *ValidationError) Err() error {
	if e == nil || len(e.Violations) == 0 {
		return nil
	}
	return errs.New(errs.InvalidArgument, "galleon.invalid_input", "the input is invalid", errs.FieldViolations(e.Violations))
}
