package protocol

import (
	"errors"
	"fmt"
)

const (
	ErrCodeMissingField = "missing_field"
	ErrCodeInvalidValue = "invalid_value"
)

type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func IsValidationCode(err error, code string) bool {
	var vErr *ValidationError
	return errors.As(err, &vErr) && vErr.Code == code
}

func newValidationError(code, message string) error {
	return &ValidationError{
		Code:    code,
		Message: message,
	}
}
