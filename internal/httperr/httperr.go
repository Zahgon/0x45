// Package httperr provides the typed HTTP error used across the application
// together with the helpers that turn such an error into a JSON response.
//
// Gin has no global error handler hook, so handlers and services keep returning
// errors and the routing layer wraps them with Wrap, which renders the error
// exactly once.
package httperr

import (
	"errors"
	"github.com/watzon/0x45/internal/server/respond"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Error is an HTTP error carrying the status code and the message that should
// be sent back to the client.
type Error struct {
	Code    int
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return e.Message
}

// New builds an *Error for the given status code and message.
func New(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// HandlerFunc is a gin handler that is allowed to return an error.
type HandlerFunc func(c *gin.Context) error

// Wrap adapts a HandlerFunc to a gin.HandlerFunc, rendering any returned error
// as JSON.
func Wrap(h HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h(c); err != nil {
			Render(c, err)
		}
	}
}

// Render writes err to the response as {"error": message} and aborts the
// remaining handler chain. Errors that are not an *Error become a generic 500.
func Render(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	message := "Internal Server Error"

	var httpError *Error
	if errors.As(err, &httpError) {
		code = httpError.Code
		message = httpError.Message
	}

	_ = c.Error(err)

	if c.Writer.Written() {
		c.Abort()
		return
	}
	respond.AbortJSON(c, code, gin.H{"error": message})
}
