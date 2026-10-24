// Package apierr defines the error types that the Barq HTTP API
// returns.
//
// The pattern follows the one that mature Go services converge on:
// every error carries its own HTTP status and a machine readable code,
// and matching uses errors.Is rather than string comparison. Handlers
// therefore never have to map error strings to status codes, and the
// client can branch on a stable code.
package apierr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an API error with a status code and a stable code.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Code is a stable, machine readable identifier. The client
	// branches on this rather than on the message.
	Code string
	// Message is the human readable explanation.
	Message string
	// Err is the underlying cause. It is wrapped, so errors.Is and
	// errors.As keep working.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches another error by code.
//
// Matching on the code rather than on the concrete type lets a caller
// write errors.Is(err, apierr.NotFound()) without knowing whether the
// error was constructed with a specific message.
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return other.Code == e.Code
}

// New creates an error with a status, a code and a message.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Wrap creates an error that wraps a cause.
func Wrap(status int, code, message string, err error) *Error {
	return &Error{Status: status, Code: code, Message: message, Err: err}
}

// The error codes. They are part of the API contract.
const (
	// CodeBadRequest means the request was malformed.
	CodeBadRequest = "bad_request"
	// CodeUnauthorized means authentication is required.
	CodeUnauthorized = "unauthorized"
	// CodeForbidden means the caller is authenticated but not allowed.
	CodeForbidden = "forbidden"
	// CodeNotFound means the resource does not exist.
	CodeNotFound = "not_found"
	// CodeConflict means the request conflicts with the current state.
	CodeConflict = "conflict"
	// CodeUnprocessable means the request was well formed but the
	// request is semantically invalid.
	CodeUnprocessable = "unprocessable"
	// CodeRateLimited means the caller is sending too many requests.
	CodeRateLimited = "rate_limited"
	// CodeInternal means the server failed unexpectedly.
	CodeInternal = "internal"
	// CodeUnavailable means a dependency is down.
	CodeUnavailable = "unavailable"
)

// The constructors for the common cases.

// BadRequest creates a 400 error.
func BadRequest(message string) *Error {
	return New(http.StatusBadRequest, CodeBadRequest, message)
}

// Unauthorized creates a 401 error.
func Unauthorized(message string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthorized, message)
}

// Forbidden creates a 403 error.
func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

// NotFound creates a 404 error.
func NotFound(message string) *Error {
	return New(http.StatusNotFound, CodeNotFound, message)
}

// Conflict creates a 409 error.
func Conflict(message string) *Error {
	return New(http.StatusConflict, CodeConflict, message)
}

// Unprocessable creates a 422 error.
//
// The distinction between 400 and 422 matters. A 400 means the request
// could not be understood. A 422 means the request was understood and
// is syntactically valid, but it does not make sense in the current
// state, for example rebuilding a run that has no failed wave.
func Unprocessable(message string) *Error {
	return New(422, CodeUnprocessable, message)
}

// Internal creates a 500 error.
func Internal(message string) *Error {
	return New(http.StatusInternalServerError, CodeInternal, message)
}

// Unavailable creates a 503 error.
func Unavailable(message string) *Error {
	return New(http.StatusServiceUnavailable, CodeUnavailable, message)
}

// From converts an arbitrary error into an API error.
//
// A nil error becomes nil. An error that is already an API error is
// returned unchanged. Anything else becomes an internal error that
// hides the details from the client while keeping them in the log.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var apiError *Error
	if errors.As(err, &apiError) {
		return apiError
	}
	return Wrap(http.StatusInternalServerError, CodeInternal, "an unexpected error occurred", err)
}
