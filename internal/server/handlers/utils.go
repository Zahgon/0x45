package handlers

import "github.com/gin-gonic/gin"

// getPasteID extracts paste ID from request parameters
func getPasteID(c *gin.Context) string {
	// First try the :id parameter
	if id := c.Param("id"); id != "" {
		return id
	}
	// Then try the path parameter
	return c.Param("*")
}
