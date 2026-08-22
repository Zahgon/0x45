package utils

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// QueryInt gets an integer query parameter with a default value
func QueryInt(c *gin.Context, key string, defaultValue int) int {
	val := c.Query(key)
	if val == "" {
		return defaultValue
	}

	intVal, err := strconv.Atoi(val)
	if err != nil {
		return defaultValue
	}

	return intVal
}
