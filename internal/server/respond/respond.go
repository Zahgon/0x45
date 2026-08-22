// Package respond renders JSON with the exact Content-Type Fiber emitted.
//
// Fiber's c.JSON sent a bare "application/json"; gin's render.JSON sends
// "application/json; charset=utf-8". gin only sets the header when it is
// absent (render.writeContentType), so pre-setting it preserves the
// baseline wire format.
package respond

import "github.com/gin-gonic/gin"

const contentTypeJSON = "application/json"

// JSON writes obj as JSON with Fiber's Content-Type.
func JSON(c *gin.Context, code int, obj any) {
	c.Header("Content-Type", contentTypeJSON)
	c.JSON(code, obj)
}

// AbortJSON aborts the chain and writes obj as JSON with Fiber's Content-Type.
func AbortJSON(c *gin.Context, code int, obj any) {
	c.Header("Content-Type", contentTypeJSON)
	c.AbortWithStatusJSON(code, obj)
}
