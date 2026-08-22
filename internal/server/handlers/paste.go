package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/parser"
	"go.uber.org/zap"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/server/services"
	"github.com/watzon/0x45/internal/server/template"
)

type PasteHandlers struct {
	services *services.Services
	logger   *zap.Logger
	config   *config.Config
}

func NewPasteHandlers(services *services.Services, logger *zap.Logger, config *config.Config) *PasteHandlers {
	return &PasteHandlers{
		services: services,
		logger:   logger,
		config:   config,
	}
}

// @id HandleUpload
// @Summary Upload a new paste
// @Tags Paste
// @Accept multipart/form-data
// @Produce json
// @Param file formData file true "File to upload"
// @Success 200 {object} services.PasteResponse
// @Failure 400 {object} httperr.Error
func (h *PasteHandlers) HandleUpload(c *gin.Context) error {
	return h.services.Paste.UploadPaste(c)
}

// HandleView serves the content with syntax highlighting if applicable
func (h *PasteHandlers) HandleView(c *gin.Context) error {
	id := getPasteID(c)

	// Get extension from locals if available
	if ext, ok := c.Get("extension"); ok {
		id = id + "." + ext.(string)
	}

	paste, err := h.services.Paste.GetPaste(id)
	if err != nil {
		return err
	}

	if err := h.services.Analytics.LogPasteView(c, paste.ID); err != nil {
		h.logger.Error("failed to log paste view", zap.Error(err))
	}

	// If the accepts header contains our vendor-specific MIME type, return the paste as JSON
	if strings.Contains(c.GetHeader("Accept"), "application/vnd.0x45.paste+json") {
		return h.services.Paste.RenderPasteJSON(c, paste)
	}

	// If the client wants HTML (browsers), render the HTML view.
	// Specifically using "application/xhtml+xml" here since all browsers include it in their
	// Accept header, and it won't ever be automatically added as a mime type for a paste.
	if strings.Contains(c.GetHeader("Accept"), "application/xhtml+xml") {
		return h.services.Paste.RenderPaste(c, paste)
	}

	// For all other cases, check if the client accepts the paste's mime type
	acceptHeader := c.GetHeader("Accept")
	if acceptHeader != "" && acceptHeader != "*/*" {
		// Split accept header on commas and check if any of the accepted types match
		acceptedTypes := strings.Split(acceptHeader, ",")
		matched := false

		// Strip quality values from paste's mime type
		pasteMimeType := strings.TrimSpace(strings.Split(paste.MimeType, ";")[0])

		for _, t := range acceptedTypes {
			// Trim whitespace and remove quality values (e.g., "text/html;q=0.9")
			mediaType := strings.TrimSpace(strings.Split(t, ";")[0])
			if mediaType == pasteMimeType {
				matched = true
				break
			}
		}
		if !matched {
			return httperr.New(
				http.StatusNotAcceptable,
				fmt.Sprintf("Client accepts %s but paste has mime type %s", acceptHeader, pasteMimeType),
			)
		}
	}

	// Set content type and return raw content
	c.Header("Content-Type", paste.MimeType)
	return h.services.Paste.RenderPasteRaw(c, paste)
}

// HandleRawView serves the raw content of a paste
func (h *PasteHandlers) HandleRawView(c *gin.Context) error {
	id := getPasteID(c)

	// Get extension from locals if available
	if ext, ok := c.Get("extension"); ok {
		id = id + "." + ext.(string)
	}

	paste, err := h.services.Paste.GetPaste(id)
	if err != nil {
		return err
	}

	return h.services.Paste.RenderPasteRaw(c, paste)
}

// HandleDownload serves the content as a downloadable file
func (h *PasteHandlers) HandleDownload(c *gin.Context) error {
	id := getPasteID(c)

	// Get extension from locals if available
	if ext, ok := c.Get("extension"); ok {
		id = id + "." + ext.(string)
	}

	paste, err := h.services.Paste.GetPaste(id)
	if err != nil {
		return err
	}

	return h.services.Paste.RenderDownload(c, paste)
}

// HandlePasteSegment resolves the trailing segment of GET /p/:id/:key.
//
// Fiber matched partial-segment patterns such as /p/:id/raw.:ext and
// /p/:id/download.:ext. gin's router allows only one wildcard per path
// segment and panics on those shapes at registration time, so the extension
// is parsed here instead and the request is dispatched to the same handler
// Fiber would have reached.
func (h *PasteHandlers) HandlePasteSegment(c *gin.Context) error {
	key := c.Param("key")
	name, ext, hasExt := strings.Cut(key, ".")
	if hasExt {
		c.Set("extension", ext)
	}

	switch name {
	case "raw":
		return h.HandleRawView(c)
	case "download":
		return h.HandleDownload(c)
	case "image":
		return h.HandleGetPasteImage(c)
	case "preview":
		return h.HandlePreview(c)
	default:
		return h.HandleDeleteWithKey(c)
	}
}

// HandleDeleteWithKey deletes a paste using its deletion key
func (h *PasteHandlers) HandleDeleteWithKey(c *gin.Context) error {
	return h.services.Paste.DeleteWithKey(c, getPasteID(c))
}

// HandleListPastes returns a paginated list of pastes for the API key
func (h *PasteHandlers) HandleListPastes(c *gin.Context) error {
	return h.services.Paste.ListPastes(c)
}

// HandleDeletePaste deletes a paste (requires API key ownership)
func (h *PasteHandlers) HandleDeletePaste(c *gin.Context) error {
	return h.services.Paste.Delete(c, getPasteID(c))
}

// HandleUpdateExpiration updates a paste's expiration time
func (h *PasteHandlers) HandleUpdateExpiration(c *gin.Context) error {
	return h.services.Paste.UpdateExpiration(c, getPasteID(c))
}

// HandleGetPasteImage returns an image of the paste suitable for Open Graph
func (h *PasteHandlers) HandleGetPasteImage(c *gin.Context) error {
	id := getPasteID(c)

	// Get extension from locals if available
	if ext, ok := c.Get("extension"); ok {
		id = id + "." + ext.(string)
	}

	paste, err := h.services.Paste.GetPaste(id)
	if err != nil {
		return err
	}

	return h.services.Paste.GetPasteImage(c, paste)
}

// HandlePreview renders a markdown preview
func (h *PasteHandlers) HandlePreview(c *gin.Context) error {
	id := getPasteID(c)

	// Get extension from locals if available and build full ID
	var fullID string
	if ext, ok := c.Get("extension"); ok {
		fullID = id + "." + ext.(string)
	} else {
		fullID = id
	}

	paste, err := h.services.Paste.GetPaste(id)
	if err != nil {
		return err
	}

	// Get the raw content straight from storage
	content, err := h.services.Paste.GetContent(paste)
	if err != nil {
		return err
	}

	// Convert markdown to HTML
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	renderedContent := string(markdown.ToHTML(content, p, nil))

	// Format the expiry time
	var expiryTime string
	if paste.ExpiresAt != nil {
		expiryTime = formatExpiryTime(paste.ExpiresAt)
	}

	// Render the preview template
	return template.Render(c, "preview", gin.H{
		"id":       fullID, // Use the ID with extension
		"filename": paste.Filename,
		"created":  paste.CreatedAt.Format("2006-01-02 15:04:05"),
		"expires":  expiryTime,
		"metadata": gin.H{
			"size":     formatSize(paste.Size),
			"mimeType": paste.MimeType,
		},
		"renderedContent": renderedContent,
	}, "layouts/main")
}

// formatExpiryTime formats a time pointer into a string
func formatExpiryTime(t *time.Time) string {
	if t == nil {
		return "Never"
	}
	return t.Format("2006-01-02 15:04:05")
}

// formatSize formats a size in bytes into a human readable string
func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
