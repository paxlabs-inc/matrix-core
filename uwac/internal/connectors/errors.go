package connectors

import "fmt"

// ArgError marks a caller-side argument problem (missing/invalid arg), which the
// engine maps to CodeInvalidRequest rather than a provider error.
type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return e.Msg }

// Bad constructs an *ArgError.
func Bad(format string, a ...any) error { return &ArgError{Msg: fmt.Sprintf(format, a...)} }
