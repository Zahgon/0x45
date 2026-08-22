package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/models"
	"github.com/watzon/0x45/internal/server/services"
)

type AuthMiddleware struct {
	db       *gorm.DB
	logger   *zap.Logger
	config   *config.Config
	services *services.Services
}

func NewAuthMiddleware(db *gorm.DB, logger *zap.Logger, config *config.Config, services *services.Services) *AuthMiddleware {
	return &AuthMiddleware{
		db:       db,
		logger:   logger,
		config:   config,
		services: services,
	}
}

// Auth returns a middleware that validates API keys
func (m *AuthMiddleware) Auth(required bool) gin.HandlerFunc {
	return httperr.Wrap(func(c *gin.Context) error {
		// First try to get API key from Authorization header
		auth := c.GetHeader("Authorization")
		apiKey := ""

		if strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		} else {
			// If not in header, try to get from query parameter
			apiKey = c.Query("api_key")
		}

		// If no API key found in either place
		if apiKey == "" {
			if required {
				return httperr.New(http.StatusUnauthorized, "API key required")
			}
			c.Next()
			return nil
		}

		// Validate API key and set rate limits
		key, err := m.validateAPIKey(apiKey)
		if err != nil {
			if required {
				return httperr.New(http.StatusUnauthorized, "Invalid API key")
			}
			c.Next()
			return nil
		}

		// Store API key in context
		c.Set("apiKey", key)
		c.Next()
		return nil
	})
}

func (m *AuthMiddleware) validateAPIKey(key string) (*models.APIKey, error) {
	var apiKey models.APIKey
	err := m.db.Where("key = ? AND verified = ?", key, true).First(&apiKey).Error
	if err != nil {
		return nil, err
	}

	// if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now()) {
	// 	return nil, httperr.New(http.StatusUnauthorized, "API key has expired")
	// }

	// Update last used timestamp and usage count
	if err := m.db.Model(&apiKey).Updates(map[string]any{
		"last_used_at": time.Now(),
		"usage_count":  gorm.Expr("usage_count + 1"),
	}).Error; err != nil {
		m.logger.Error("failed to update API key usage",
			zap.String("key", key),
			zap.Error(err),
		)
	}

	return &apiKey, nil
}
