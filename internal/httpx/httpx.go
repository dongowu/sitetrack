// Package httpx holds the response envelope and the error taxonomy shared by
// every handler.
//
// Success: {"data": ...} plus optional "meta".  Lists always carry meta with
// paging info and the unread notification count the UI shows on every screen.
// Failure: {"error": {"code": "...", "message": "..."}} with a matching status.
//
// Everything takes a *gin.Context rather than a ResponseWriter so a handler
// never has to think about status codes or headers twice: one call writes the
// whole envelope and aborts the chain.
package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
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

func JSON(c *gin.Context, status int, payload any) {
	if payload == nil {
		c.Status(status)
		return
	}
	c.JSON(status, payload)
}

func OK(c *gin.Context, data any) {
	JSON(c, http.StatusOK, Envelope{Data: data})
}

func Created(c *gin.Context, data any) {
	JSON(c, http.StatusCreated, Envelope{Data: data})
}

func List(c *gin.Context, data any, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	JSON(c, http.StatusOK, Envelope{Data: data, Meta: meta})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Fail writes err as an error envelope. Unknown errors become a 500 with a
// generic message; the detail only goes to the log.
func Fail(c *gin.Context, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		slog.Error("unhandled error", "err", err, "method", c.Request.Method, "path", c.Request.URL.Path)
		apiErr = Internal(err)
	}
	c.AbortWithStatusJSON(apiErr.Status, errorEnvelope{Error: ErrorBody{Code: apiErr.Code, Message: apiErr.Message}})
}
