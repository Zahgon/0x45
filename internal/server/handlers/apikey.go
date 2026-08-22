package handlers

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/server/services"
)

type APIKeyHandlers struct {
	services *services.Services
	logger   *zap.Logger
	config   *config.Config
}

func NewAPIKeyHandlers(services *services.Services, logger *zap.Logger, config *config.Config) *APIKeyHandlers {
	return &APIKeyHandlers{
		services: services,
		logger:   logger,
		config:   config,
	}
}

// @id HandleRequestAPIKey
// @Summary Request a new API key
// @Tags API Key
// @Accept json
// @Produce json
// @Param request body services.APIKeyRequest true "Request a new API key"
// @Success 200 {object} services.APIKeyResponse
// @Failure 400 {object} httperr.Error
// @Router /api/keys/request [post]
func (h *APIKeyHandlers) HandleRequestAPIKey(c *gin.Context) error {
	return h.services.APIKey.RequestKey(c)
}

func (h *APIKeyHandlers) HandleVerifyAPIKey(c *gin.Context) error {
	return h.services.APIKey.VerifyKey(c)
}
