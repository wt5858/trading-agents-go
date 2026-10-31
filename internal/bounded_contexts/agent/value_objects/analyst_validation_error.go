package value_objects

import "fmt"

// AnalystValidationError keeps safe delivery diagnostics distinct from raw model
// output and infrastructure errors. Cause preserves JSON decoding errors.
type AnalystValidationError struct {
	Code    string
	Finding int
	Detail  string
	Cause   error
}

func (e *AnalystValidationError) Error() string {
	if e.Finding >= 0 {
		return fmt.Sprintf("finding %d: %s", e.Finding+1, e.Detail)
	}
	return e.Detail
}
func (e *AnalystValidationError) Unwrap() error { return e.Cause }
func validationError(code string, finding int, detail string, cause error) error {
	return &AnalystValidationError{code, finding, detail, cause}
}
