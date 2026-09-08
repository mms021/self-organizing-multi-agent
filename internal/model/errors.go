// Package model holds the wire types shared across the platform: the error
// envelope (RFC-1000 §8), the message envelope (RFC-1100 §5), and the
// Agent/Task/Verification DTOs (RFC-1800).
package model

import (
	"encoding/json"
	"net/http"
)

// Baseline error_code set, RFC-1000 §8. rate_limited is reserved but unused
// this milestone (no rate limiting implemented, RFC-1100 §13 is a SHOULD).
const (
	ErrNotFound        = "not_found"
	ErrAccessDenied    = "access_denied"
	ErrValidationError = "validation_error"
	ErrRateLimited     = "rate_limited"
	ErrConflict        = "conflict"
	ErrUnavailable     = "unavailable"
	// ErrInternal covers failures that are the platform's own fault. Its
	// message is deliberately fixed and uninformative: internal detail (SQL
	// text, file paths) must not cross the trust boundary (RFC-1600 §2). The
	// detail belongs in the server log, correlated by trace_id.
	ErrInternal = "internal"
)

// APIError is the RFC-1000 §8 error body.
type APIError struct {
	ErrorCode       string `json:"error_code"`
	Message         string `json:"message"`
	Retryable       bool   `json:"retryable"`
	Details         any    `json:"details,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

func newErr(code, message string, retryable bool) *APIError {
	return &APIError{ErrorCode: code, Message: message, Retryable: retryable}
}

func NotFound(message string) *APIError           { return newErr(ErrNotFound, message, false) }
func AccessDenied(message string) *APIError       { return newErr(ErrAccessDenied, message, false) }
func ValidationError(message string) *APIError    { return newErr(ErrValidationError, message, false) }
func Conflict(message string) *APIError           { return newErr(ErrConflict, message, false) }
func RateLimited(message string) *APIError        { return newErr(ErrRateLimited, message, true) }
func ServiceUnavailable(message string) *APIError { return newErr(ErrUnavailable, message, true) }

// Internal is the only way to build an internal error: it takes no message,
// so no caller can accidentally leak detail into the response body.
func Internal() *APIError { return newErr(ErrInternal, "internal error", true) }

// IsCode reports whether err is an APIError carrying the given error_code.
func IsCode(err error, code string) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.ErrorCode == code
}

// WithDetails attaches a details object and returns the same error for chaining.
func (e *APIError) WithDetails(details any) *APIError {
	e.Details = details
	return e
}

// WithTraceID attaches a trace_id and returns the same error for chaining.
func (e *APIError) WithTraceID(traceID string) *APIError {
	e.TraceID = traceID
	return e
}

// HTTPStatus maps an error_code to the HTTP status it MUST be reported under.
func HTTPStatus(code string) int {
	switch code {
	case ErrNotFound:
		return http.StatusNotFound
	case ErrAccessDenied:
		return http.StatusForbidden
	case ErrValidationError:
		return http.StatusBadRequest
	case ErrRateLimited:
		return http.StatusTooManyRequests
	case ErrConflict:
		return http.StatusConflict
	case ErrInternal:
		return http.StatusInternalServerError
	case ErrUnavailable:
		return http.StatusServiceUnavailable
	default:
		// An unrecognised code is itself a bug: report it as a server fault
		// rather than guessing a 4xx the client could act on.
		return http.StatusInternalServerError
	}
}

// WriteError writes an APIError as the HTTP response, setting traceID if the
// error doesn't already carry one.
func WriteError(w http.ResponseWriter, traceID string, err *APIError) {
	if err.TraceID == "" {
		err.TraceID = traceID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(HTTPStatus(err.ErrorCode))
	_ = json.NewEncoder(w).Encode(err)
}
