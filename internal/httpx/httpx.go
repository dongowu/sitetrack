// Package httpx holds the response envelope and the error taxonomy shared by
// every handler.
//
// Success: {"data": ...} plus optional "meta".  Lists always carry meta with
// paging info and the unread notification count the UI shows on every screen.
// Failure: {"error": {"code": "...", "message": "..."}} with a matching status.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

type Envelope struct {
	Data any            `json:"data,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// APIError is an error that maps directly onto an HTTP status and a stable code
// the frontend can branch on.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func NewError(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func BadRequest(format string, args ...any) *APIError {
	return NewError(http.StatusBadRequest, "invalid_request", format, args...)
}

func Unauthorized(format string, args ...any) *APIError {
	return NewError(http.StatusUnauthorized, "unauthorized", format, args...)
}

func Forbidden(format string, args ...any) *APIError {
	return NewError(http.StatusForbidden, "forbidden", format, args...)
}

// NotFound is used for every "you may not see this" case as well: the brief
// requires that a non-member cannot tell a project ID apart from a project ID
// that does not exist.
func NotFound(format string, args ...any) *APIError {
	return NewError(http.StatusNotFound, "not_found", format, args...)
}

func Conflict(format string, args ...any) *APIError {
	return NewError(http.StatusConflict, "conflict", format, args...)
}

func Unprocessable(format string, args ...any) *APIError {
	return NewError(http.StatusUnprocessableEntity, "rule_violation", format, args...)
}

func Internal(err error) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal error"}
}

func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func OK(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, Envelope{Data: data})
}

func Created(w http.ResponseWriter, data any) {
	JSON(w, http.StatusCreated, Envelope{Data: data})
}

func List(w http.ResponseWriter, data any, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	JSON(w, http.StatusOK, Envelope{Data: data, Meta: meta})
}

func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Fail writes err as an error envelope. Unknown errors become a 500 with a
// generic message; the detail only goes to the log.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		slog.Error("unhandled error", "err", err, "method", r.Method, "path", r.URL.Path)
		apiErr = Internal(err)
	}
	JSON(w, apiErr.Status, errorEnvelope{Error: ErrorBody{Code: apiErr.Code, Message: apiErr.Message}})
}
