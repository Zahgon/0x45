package handlers

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/dustin/go-humanize"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/server/services"
	"github.com/watzon/0x45/internal/server/template"
	"github.com/watzon/0x45/internal/utils"
)

type WebHandlers struct {
	services *services.Services
	logger   *zap.Logger
	config   *config.Config
}

func NewWebHandlers(services *services.Services, logger *zap.Logger, config *config.Config) *WebHandlers {
	return &WebHandlers{
		services: services,
		logger:   logger,
		config:   config,
	}
}

var httpRe = regexp.MustCompile(`^https?://`)

func (h *WebHandlers) getBaseURLHost() string {
	return httpRe.ReplaceAllString(h.config.Server.BaseURL, "")
}

// HandleIndex serves the main web interface page
func (h *WebHandlers) HandleIndex(c *gin.Context) error {
	h.logger.Debug("generating retention data for index page")
	retentionStats, err := utils.GenerateRetentionData(int64(h.config.Server.MaxUploadSize), h.config)
	if err != nil {
		h.logger.Error("failed to generate retention data", zap.Error(err))
	}

	h.logger.Debug("marshaling retention history data")
	noKeyHistory, err := json.Marshal(retentionStats.Data["noKey"])
	if err != nil {
		h.logger.Error("failed to marshal noKey history", zap.Error(err))
		return err
	}

	withKeyHistory, err := json.Marshal(retentionStats.Data["withKey"])
	if err != nil {
		h.logger.Error("failed to marshal withKey history", zap.Error(err))
		return err
	}

	h.logger.Debug("preparing template data",
		zap.String("baseUrlHost", h.getBaseURLHost()),
		zap.Any("retention", retentionStats))

	err = template.Render(c, "index", gin.H{
		"retention": gin.H{
			"noKey":          retentionStats.NoKeyRange,
			"withKey":        retentionStats.WithKeyRange,
			"minAge":         h.config.Retention.NoKey.MinAge,
			"maxAge":         h.config.Retention.WithKey.MaxAge,
			"maxSize":        h.config.Server.MaxUploadSize / (1024 * 1024),
			"maxSizeMiB":     humanize.IBytes(uint64(h.config.Server.MaxUploadSize)),
			"noKeyHistory":   string(noKeyHistory),
			"withKeyHistory": string(withKeyHistory),
		},
		"baseUrlHost": h.getBaseURLHost(),
		"baseUrl":     h.config.Server.BaseURL,
	}, "layouts/main")

	if err != nil {
		h.logger.Error("failed to render index template",
			zap.Error(err),
			zap.String("template", "index"),
			zap.String("layout", "layouts/main"))
		return err
	}

	return nil
}

// HandleStats serves the statistics page
func (h *WebHandlers) HandleStats(c *gin.Context) error {
	stats, err := h.services.Stats.GetSystemStats()
	if err != nil {
		return err
	}

	return template.Render(c, "stats", gin.H{
		"stats":       stats,
		"baseUrlHost": h.getBaseURLHost(),
		"baseUrl":     h.config.Server.BaseURL,
	}, "layouts/main")
}

// HandleDocs serves the API documentation page
func (h *WebHandlers) HandleDocs(c *gin.Context) error {
	retentionStats, err := utils.GenerateRetentionData(int64(h.config.Server.MaxUploadSize), h.config)
	if err != nil {
		h.logger.Error("failed to generate retention data", zap.Error(err))
	}

	return template.Render(c, "docs", gin.H{
		"baseUrlHost":    h.getBaseURLHost(),
		"baseUrl":        h.config.Server.BaseURL,
		"apiKeysEnabled": h.services.APIKey.IsEnabled(),
		"retention": gin.H{
			"noKey":   retentionStats.NoKeyRange,
			"withKey": retentionStats.WithKeyRange,
			"minAge":  h.config.Retention.NoKey.MinAge,
			"maxAge":  h.config.Retention.WithKey.MaxAge,
		},
		"rateLimits": gin.H{
			"global": fmt.Sprintf("%.0f/s", h.config.Server.RateLimit.Global.Rate),
			"perIP":  fmt.Sprintf("%.0f/s", h.config.Server.RateLimit.PerIP.Rate),
		},
		"maxSize": h.config.Server.MaxUploadSize / (1024 * 1024),
	}, "layouts/main")
}

// HandleSubmit serves the paste submission page
func (h *WebHandlers) HandleSubmit(c *gin.Context) error {
	return template.Render(c, "submit", gin.H{
		"baseUrlHost": h.getBaseURLHost(),
		"baseUrl":     h.config.Server.BaseURL,
	}, "layouts/main")
}
