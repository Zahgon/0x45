package services

import (
	"crypto/rand"
	"encoding/hex"
	"github.com/watzon/0x45/internal/server/respond"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/mailer"
	"github.com/watzon/0x45/internal/models"
	"github.com/watzon/0x45/internal/server/binding"
	"github.com/watzon/0x45/internal/server/template"
)

type APIKeyService struct {
	db     *gorm.DB
	logger *zap.Logger
	config *config.Config
	mailer *mailer.Mailer
}

func NewAPIKeyService(db *gorm.DB, logger *zap.Logger, config *config.Config) *APIKeyService {
	m, err := mailer.New(config)
	if err != nil {
		logger.Error("failed to initialize mailer", zap.Error(err))
	}

	return &APIKeyService{
		db:     db,
		logger: logger,
		config: config,
		mailer: m,
	}
}

// RequestKey handles the initial API key request
func (s *APIKeyService) RequestKey(c *gin.Context) error {
	var req APIKeyRequest

	if err := binding.Body(c, &req); err != nil {
		return httperr.New(http.StatusBadRequest, "Invalid request body")
	}

	// Validate email
	if req.Email == "" {
		return httperr.New(http.StatusBadRequest, "Email is required")
	}

	// Check for existing unverified key
	var existingKey models.APIKey
	err := s.db.Where("email = ? AND verified = ?", req.Email, false).First(&existingKey).Error
	if err == nil {
		// Delete existing unverified key
		s.db.Delete(&existingKey)
	}

	// Generate verification token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		s.logger.Error("failed to generate verification token", zap.Error(err))
		return httperr.New(http.StatusInternalServerError, "Failed to generate verification token")
	}
	token := hex.EncodeToString(tokenBytes)

	// Create new API key with defaults
	apiKey := models.NewAPIKey()
	apiKey.Email = req.Email
	apiKey.Name = req.Name
	apiKey.VerifyToken = token
	apiKey.VerifyExpiry = time.Now().Add(24 * time.Hour)
	apiKey.MaxFileSize = int64(s.config.Server.APIUploadSize)
	apiKey.RateLimit = int(s.config.Server.RateLimit.Global.Rate)

	if err := s.db.Create(apiKey).Error; err != nil {
		s.logger.Error("failed to create API key", zap.Error(err))
		return httperr.New(http.StatusInternalServerError, "Failed to create API key")
	}

	// Send verification email
	if err := s.sendVerificationEmail(req.Email, token); err != nil {
		s.logger.Error("failed to send verification email",
			zap.String("email", req.Email),
			zap.Error(err),
		)
		return httperr.New(http.StatusInternalServerError, "Failed to send verification email. Please contact the administrator.")
	}

	respond.JSON(c, http.StatusOK, gin.H{
		"message": "API key created. Please check your email for verification.",
	})

	return nil
}

// VerifyKey verifies the email and activates the API key
func (s *APIKeyService) VerifyKey(c *gin.Context) error {
	token := c.Query("token")
	if token == "" {
		return httperr.New(http.StatusBadRequest, "Verification token is required")
	}

	var apiKey models.APIKey
	err := s.db.Where("verify_token = ? AND verified = ? AND verify_expiry > ?", token, false, time.Now()).First(&apiKey).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return httperr.New(http.StatusNotFound, "Invalid or expired verification token")
		}
		return err
	}

	// Update API key
	apiKey.Verified = true
	apiKey.VerifyToken = ""          // Clear verification token
	apiKey.LastUsedAt = &time.Time{} // Initialize LastUsedAt
	apiKey.UsageCount = 0            // Initialize UsageCount

	if err := s.db.Save(&apiKey).Error; err != nil {
		return httperr.New(http.StatusInternalServerError, "Failed to verify API key")
	}

	return template.Render(c, "verify_success", gin.H{
		"baseUrl": s.config.Server.BaseURL,
		"apiKey":  apiKey.Key,
	}, "layouts/main")
}

func (s *APIKeyService) HasMailer() bool {
	return s.mailer != nil
}

func (s *APIKeyService) IsEnabled() bool {
	return s.config.SMTP.Enabled && s.HasMailer()
}

// Helper functions

func (s *APIKeyService) sendVerificationEmail(email, token string) error {
	if s.mailer == nil {
		return httperr.New(
			http.StatusServiceUnavailable,
			"Email verification is not available. Please contact the administrator.",
		)
	}

	return s.mailer.SendVerification(email, token)
}

// CleanupUnverifiedKeys removes unverified API keys older than 24 hours
func (s *APIKeyService) CleanupUnverifiedKeys() int64 {
	cutoff := time.Now().Add(-24 * time.Hour)
	result := s.db.Where("verified = ? AND verify_expiry < ?", false, cutoff).Delete(&models.APIKey{})
	if result.Error != nil {
		s.logger.Error("failed to cleanup unverified keys", zap.Error(result.Error))
		return 0
	}
	return result.RowsAffected
}
