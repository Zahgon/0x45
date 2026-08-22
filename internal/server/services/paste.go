package services

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // Register GIF format
	_ "image/jpeg" // Register JPEG format
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/watzon/0x45/internal/server/respond"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"github.com/watzon/hdur"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/models"
	"github.com/watzon/0x45/internal/server/binding"
	"github.com/watzon/0x45/internal/server/template"
	"github.com/watzon/0x45/internal/storage"
	"github.com/watzon/0x45/internal/utils"
)

type PasteService struct {
	db        *gorm.DB
	logger    *zap.Logger
	config    *config.Config
	storage   storage.Provider
	analytics *AnalyticsService
}

func NewPasteService(db *gorm.DB, logger *zap.Logger, config *config.Config) *PasteService {
	return &PasteService{
		db:        db,
		logger:    logger,
		config:    config,
		storage:   storage.NewProvider(config),
		analytics: NewAnalyticsService(db, logger, config),
	}
}

// CreatePaste handles the creation of a new paste
func (s *PasteService) UploadPaste(c *gin.Context) error {
	rawBody, _ := binding.ReadBody(c)
	s.logger.Debug("Received upload request",
		zap.String("content-type", c.GetHeader("Content-Type")),
		zap.String("body", string(rawBody)))

	p := new(PasteOptions)
	contentType := c.GetHeader("Content-Type")

	// Handle form data differently from JSON/other formats
	if strings.Contains(contentType, "multipart/form-data") || strings.Contains(contentType, "application/x-www-form-urlencoded") {
		// Parse form values. A field that fails to convert is reported but does
		// not abort the upload: the source decoder collected per-field errors
		// and still populated the rest, so rejecting here would change the
		// response for inputs the original accepted.
		if err := binding.Body(c, p); err != nil {
			s.logger.Error("Failed to parse form values",
				zap.Error(err))
		}
	} else {
		// For JSON and other formats
		if err := binding.Body(c, p); err != nil {
			s.logger.Error("Failed to parse request body",
				zap.Error(err),
				zap.String("content-type", c.GetHeader("Content-Type")),
				zap.String("body", string(rawBody)))
			return httperr.New(http.StatusBadRequest, "Invalid request body")
		}
	}

	s.logger.Debug("Parsed paste options",
		zap.Any("options", p))

	// Get file content
	var content []byte
	var filename string
	if file, err := c.FormFile("file"); err == nil {
		// Read file content
		f, err := file.Open()
		if err != nil {
			return httperr.New(http.StatusInternalServerError, "Failed to open uploaded file")
		}
		defer f.Close()

		content, err = io.ReadAll(f)
		if err != nil {
			return httperr.New(http.StatusInternalServerError, "Failed to read file content")
		}

		// First check for a filename in form field
		if formFilename := c.PostForm("filename"); formFilename != "" {
			filename = formFilename
		} else if file.Filename != "" && file.Filename != "-" { // Don't use "-" as filename
			filename = file.Filename
		} else {
			filename = "paste.txt" // Default filename
		}
	} else if p.URL != "" {
		// Read content from the given URL
		content, err = utils.GetContentFromURL(p.URL)
		if err != nil {
			return httperr.New(http.StatusBadRequest, "Failed to fetch URL")
		}

		// Try to get filename from URL if not explicitly provided
		if p.Filename == "" {
			filename = utils.GetFilenameFromURL(p.URL)
		}
	} else if p.Content != "" {
		// Use content from the request body
		content = []byte(p.Content)
	} else {
		return httperr.New(http.StatusBadRequest, "No file provided")
	}

	// Check for empty content
	if len(content) == 0 {
		return httperr.New(http.StatusBadRequest, "Empty file")
	}

	// If we found a filename and none was provided in the request, use it
	if filename != "" && p.Filename == "" {
		p.Filename = filename
	}

	var apiKey *models.APIKey
	if key, ok := c.Get("apiKey"); ok && key != nil {
		apiKey = key.(*models.APIKey)
	}

	// Check if the user is attempting to do something they're not allowed to do
	if p.Private && apiKey == nil {
		return httperr.New(http.StatusUnauthorized, "Private pastes can only be created with an API key")
	}

	// Create the paste
	paste, err := s.createPaste(bytes.NewReader(content), apiKey, int64(len(content)), p)
	if err != nil {
		return err
	}

	baseURL := s.config.Server.BaseURL
	response := &PasteResponse{
		ID:        paste.ID,
		Filename:  paste.Filename,
		URL:       fmt.Sprintf("%s/p/%s.%s", baseURL, paste.ID, paste.Extension),
		DeleteURL: fmt.Sprintf("%s/p/%s.%s/%s", baseURL, paste.ID, paste.Extension, paste.DeleteKey),
		Private:   paste.Private,
		MimeType:  paste.MimeType,
		Size:      paste.Size,
		ExpiresAt: paste.ExpiresAt,
	}

	// If this is a browser form submission, redirect to the paste view
	acceptHeader := c.GetHeader("Accept")
	if strings.Contains(acceptHeader, "text/html") {
		// Store the deletion URL in the session for display after redirect
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     "deletion_url",
			Value:    response.DeleteURL,
			Path:     "/",
			Expires:  time.Now().Add(5 * time.Minute),
			HttpOnly: true,
		})
		c.Header("Location", response.URL)
		c.Status(http.StatusFound)
		return nil
	}

	// For API requests, return JSON response
	respond.JSON(c, http.StatusOK, response)
	return nil
}

// GetPaste retrieves a paste by ID with expiry checking
func (s *PasteService) GetPaste(id string) (*models.Paste, error) {
	// Strip any extension from the ID
	if idx := strings.LastIndex(id, "."); idx != -1 {
		id = id[:idx]
	}

	var paste models.Paste
	err := s.db.Where("id = ? AND (expires_at IS NULL OR expires_at > ?)", id, time.Now()).First(&paste).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, httperr.New(http.StatusNotFound, "Paste not found or expired")
		}
		return nil, err
	}
	return &paste, nil
}

// GetPasteImage returns an image of the paste suitable for Open Graph
func (s *PasteService) GetPasteImage(c *gin.Context, paste *models.Paste) error {
	// Get the content
	content, err := s.storage.Get(paste.StoragePath)
	if err != nil {
		s.logger.Error("Failed to get paste content for image generation",
			zap.Error(err),
			zap.String("id", paste.ID),
			zap.String("storage_path", paste.StoragePath))
		return err
	}

	var imageBytes []byte

	// Handle different content types
	if s.isTextContent(paste.MimeType) {
		// For text content, generate a code preview image
		imageBytes, err = GenerateCodeImage(string(content), paste.Filename)
		if err != nil {
			s.logger.Error("Failed to generate code image",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}
	} else if s.isImageContent(paste.MimeType) {
		// For images, use the image directly but resize if needed
		img, _, err := image.Decode(bytes.NewReader(content))
		if err != nil {
			s.logger.Error("Failed to decode image",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}

		// Generate preview with watermark
		preview, err := GenerateImagePreview(img)
		if err != nil {
			s.logger.Error("Failed to generate image preview",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}

		// Encode the preview image
		var buf bytes.Buffer
		if err := png.Encode(&buf, preview); err != nil {
			s.logger.Error("Failed to encode preview image",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}
		imageBytes = buf.Bytes()
	} else {
		// For binary content, generate a placeholder image
		img, err := GenerateBinaryPreviewImage(paste.Filename, paste.MimeType)
		if err != nil {
			s.logger.Error("Failed to generate binary preview image",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}

		// Encode the image
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			s.logger.Error("Failed to encode binary preview image",
				zap.Error(err),
				zap.String("id", paste.ID))
			return err
		}
		imageBytes = buf.Bytes()
	}

	c.Header("Cache-Control", "max-age=31536000, immutable")
	c.Data(http.StatusOK, "image/png", imageBytes)
	return nil
}

// RenderPaste renders the paste view for text content
func (s *PasteService) RenderPaste(c *gin.Context, paste *models.Paste) error {
	content, err := s.storage.Get(paste.StoragePath)
	if err != nil {
		return err
	}

	// Check for deletion URL cookie
	var deletionUrl string
	if cookie, err := c.Cookie("deletion_url"); err == nil && cookie != "" {
		// Clear the cookie before reading it to ensure one-time use
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     "deletion_url",
			Value:    "",
			Path:     "/",
			Expires:  time.Now().Add(-24 * time.Hour),
			HttpOnly: true,
		})
		deletionUrl = cookie
	}

	// Set cache headers
	if deletionUrl != "" {
		// Only set no-cache headers if we have a deletion URL
		c.Header("Cache-Control", "private, no-cache, no-store, must-revalidate, max-age=0")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		c.Header("CDN-Cache-Control", "no-store")
		c.Header("Cloudflare-CDN-Cache-Control", "no-store")
	} else {
		// If no deletion URL, content is immutable and can be cached
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		c.Header("ETag", paste.ID)
	}

	var renderedContent string

	if s.isTextContent(paste.MimeType) {
		// Handle text content with syntax highlighting
		renderedContent, err = s.renderHighlightedText(string(content), paste.Extension, paste.MimeType)
		if err != nil {
			return err
		}
	}

	// Build paste ID with extension if available
	pasteID := paste.ID
	if paste.Extension != "" {
		pasteID = paste.ID + "." + paste.Extension
	}

	return template.Render(c, "paste", gin.H{
		"isPaste":     true,
		"id":          pasteID,
		"filename":    paste.Filename,
		"extension":   paste.Extension,
		"created":     paste.CreatedAt.Format("2006-01-02 15:04:05"),
		"expires":     formatExpiryTime(paste.ExpiresAt),
		"language":    s.getLanguageName(paste.Extension, paste.MimeType),
		"content":     renderedContent,
		"rawContent":  string(content),
		"baseUrl":     s.config.Server.BaseURL,
		"deletionUrl": deletionUrl,
		"metadata": gin.H{
			"size":      formatSize(paste.Size),
			"mimeType":  paste.MimeType,
			"createdAt": paste.CreatedAt,
			"expiresAt": paste.ExpiresAt,
		},
	}, "layouts/main")
}

// Helper function to get language name
func (s *PasteService) getLanguageName(extension, mimeType string) string {
	var lexer chroma.Lexer
	if extension != "" {
		lexer = lexers.Get(extension)
	}
	if lexer == nil {
		lexer = lexers.Get(mimeType)
	}
	if lexer == nil {
		return "plain"
	}
	return lexer.Config().Name
}

// Helper function to render highlighted text
func (s *PasteService) renderHighlightedText(content, extension, mimeType string) (string, error) {
	// Determine lexer based on extension or content
	var lexer chroma.Lexer
	if extension != "" {
		lexer = lexers.Get(extension)
	}
	if lexer == nil {
		lexer = lexers.Get(mimeType)
	}
	if lexer == nil {
		lexer = lexers.Analyse(content)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	// Create formatter
	formatter := html.New(
		html.WithLineNumbers(true),
		html.WithLinkableLineNumbers(true, ""),
		html.TabWidth(4),
		html.WithClasses(false), // Use inline styles
	)

	// Create buffer for highlighted code
	var codeBuffer bytes.Buffer

	// Write highlighted code
	iterator, err := lexer.Tokenise(nil, content)
	if err != nil {
		return "", err
	}

	// Use GitHub Dark style
	style := styles.Get("github-dark")
	if style == nil {
		style = styles.Fallback
	}

	if err := formatter.Format(&codeBuffer, style, iterator); err != nil {
		return "", err
	}

	return codeBuffer.String(), nil
}

// RenderPasteRaw serves the raw content with proper content type
func (s *PasteService) RenderPasteRaw(c *gin.Context, paste *models.Paste) error {
	content, err := s.storage.Get(paste.StoragePath)
	if err != nil {
		return err
	}
	// Add permanent cache headers since content is immutable
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", paste.ID)
	c.Data(http.StatusOK, paste.MimeType, content)
	return nil
}

// RenderPasteJSON serves the paste as JSON. If the paste is text, the content will be included
// in the response. Otherwise only the URL will be included for downloading purposes.
func (s *PasteService) RenderPasteJSON(c *gin.Context, paste *models.Paste) error {
	pasteJson := struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
		MimeType string `json:"mimeType"`
		URL      string `json:"url"`
		Content  string `json:"content"`
	}{
		ID:       paste.ID,
		Filename: paste.Filename,
		MimeType: paste.MimeType,
		URL:      fmt.Sprintf("%s/p/%s.%s", s.config.Server.BaseURL, paste.ID, paste.Extension),
	}

	if s.isTextContent(paste.MimeType) {
		content, err := s.storage.Get(paste.StoragePath)
		if err != nil {
			return err
		}
		pasteJson.Content = string(content)
	}

	respond.JSON(c, http.StatusOK, pasteJson)

	return nil
}

// RenderDownload serves the content as a downloadable file
func (s *PasteService) RenderDownload(c *gin.Context, paste *models.Paste) error {
	content, err := s.storage.Get(paste.StoragePath)
	if err != nil {
		return err
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, paste.Filename))
	// Add permanent cache headers since content is immutable
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", paste.ID)
	c.Data(http.StatusOK, "application/octet-stream", content)
	return nil
}

// DeleteWithKey deletes a paste using its deletion key
func (s *PasteService) DeleteWithKey(c *gin.Context, id string) error {
	key := c.Param("key") // Get key from URL path instead of query
	if key == "" {
		return httperr.New(http.StatusBadRequest, "Deletion key is required")
	}

	// Strip any extension from the ID
	if idx := strings.LastIndex(id, "."); idx != -1 {
		id = id[:idx]
	}

	paste, err := s.GetPaste(id)
	if err != nil {
		// Pass through the 404 error from GetPaste
		return err
	}

	if paste.DeleteKey != key {
		return httperr.New(http.StatusUnauthorized, "Invalid deletion key")
	}

	// For DELETE requests, delete the paste
	if c.Request.Method == http.MethodDelete {
		if err := s.Delete(c, id); err != nil {
			return err
		}

		// Return appropriate response based on Accept header
		if strings.Contains(c.GetHeader("Accept"), "application/json") {
			respond.JSON(c, http.StatusOK, gin.H{
				"message": "Paste deleted successfully",
				"id":      id,
			})
			return nil
		}

		// For HTML requests, render the success page
		return template.Render(c, "delete_success", gin.H{
			"isDeleteSuccess": true,
			"baseUrl":         s.config.Server.BaseURL,
		}, "layouts/main")
	}

	// For GET requests, show a confirmation page
	if strings.Contains(c.GetHeader("Accept"), "application/json") {
		respond.JSON(c, http.StatusOK, gin.H{
			"message": "Paste found and will be deleted",
			"id":      id,
		})
		return nil
	}

	// For HTML requests, render the confirmation page
	return template.Render(c, "delete_confirm", gin.H{
		"isDeleteConfirm": true,
		"baseUrl":         s.config.Server.BaseURL,
		"pasteId":         id,
		"deleteKey":       key,
	}, "layouts/main")
}

// Delete removes a paste and its associated files
func (s *PasteService) Delete(c *gin.Context, id string) error {
	// Strip any extension from the ID
	if idx := strings.LastIndex(id, "."); idx != -1 {
		id = id[:idx]
	}

	paste, err := s.GetPaste(id)
	if err != nil {
		return err
	}

	if err := s.storage.Delete(paste.StoragePath); err != nil {
		s.logger.Error("failed to delete paste content", zap.Error(err))
	}

	return s.db.Delete(paste).Error
}

// ListPastes returns a paginated list of pastes for the API key
func (s *PasteService) ListPastes(c *gin.Context) error {
	apiKey := c.MustGet("apiKey").(*models.APIKey)

	var pastes []models.Paste
	query := s.db.Where("api_key = ?", apiKey.Key)

	// Add pagination
	page := utils.QueryInt(c, "page", 1)
	limit := utils.QueryInt(c, "limit", 20)
	offset := (page - 1) * limit

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return err
	}

	if err := query.Offset(offset).Limit(limit).Find(&pastes).Error; err != nil {
		return err
	}

	// Convert pastes to response format
	respose := NewListPastesResponse(pastes, s.config.Server.BaseURL)
	respond.JSON(c, http.StatusOK, respose)
	return nil
}

// UpdateExpiration updates a paste's expiration time
func (s *PasteService) UpdateExpiration(c *gin.Context, id string) error {
	// Strip any extension from the ID
	if idx := strings.LastIndex(id, "."); idx != -1 {
		id = id[:idx]
	}

	paste, err := s.GetPaste(id)
	if err != nil {
		return err
	}

	req := new(UpdatePasteExpirationRequest)
	if err := binding.Body(c, &req); err != nil {
		return httperr.New(http.StatusBadRequest, "Invalid request body")
	}

	expiryTime, err := s.calculateExpiry(ExpiryOptions{
		Size:      paste.Size,
		HasAPIKey: paste.APIKey != "",
		ExpiresAt: req.ExpiresAt,
		ExpiresIn: req.ExpiresIn,
	})
	if err != nil {
		return err
	}

	paste.ExpiresAt = expiryTime
	if err := s.db.Save(paste).Error; err != nil {
		return err
	}

	// Build response
	response := NewPasteResponse(paste, s.config.Server.BaseURL)
	respond.JSON(c, http.StatusOK, response)
	return nil
}

// CleanupExpired removes expired pastes and their associated files
func (s *PasteService) CleanupExpired() (int64, error) {
	var totalDeleted int64

	// Use a transaction to ensure consistency
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var pastes []models.Paste
		if err := tx.Where("expires_at < ? AND expires_at IS NOT NULL", time.Now()).Find(&pastes).Error; err != nil {
			return err
		}

		for _, paste := range pastes {
			// Delete storage content first
			if err := s.storage.Delete(paste.StoragePath); err != nil {
				s.logger.Error("failed to delete paste content",
					zap.String("id", paste.ID),
					zap.String("path", paste.StoragePath),
					zap.Error(err),
				)
				// Skip this paste if we can't delete the storage
				continue
			}

			// Delete the database record only if storage deletion was successful
			if err := tx.Delete(&paste).Error; err != nil {
				s.logger.Error("failed to delete paste record",
					zap.String("id", paste.ID),
					zap.Error(err),
				)
				// Try to recover the storage file since we couldn't delete the record
				if _, err := s.storage.Put(paste.StoragePath, bytes.NewReader([]byte{})); err != nil {
					s.logger.Error("failed to recover storage after failed deletion",
						zap.String("id", paste.ID),
						zap.String("path", paste.StoragePath),
						zap.Error(err),
					)
				}
				continue
			}

			totalDeleted++
		}

		return nil
	})

	if err != nil {
		return 0, err
	}

	return totalDeleted, nil
}

// Helper functions

// validateFileSize checks if the file size is within the allowed limits
func (s *PasteService) validateFileSize(size int64, apiKey *models.APIKey) error {
	// First check against absolute maximum size for security
	if size > int64(s.config.Server.MaxUploadSize) {
		return httperr.New(http.StatusBadRequest, fmt.Sprintf("File exceeds maximum allowed size of %d bytes", s.config.Server.MaxUploadSize))
	}

	// Then check against the appropriate tier limit
	if apiKey != nil {
		if size > int64(s.config.Server.APIUploadSize) {
			return httperr.New(http.StatusBadRequest, fmt.Sprintf("File exceeds API upload limit of %d bytes", s.config.Server.APIUploadSize))
		}
	} else {
		if size > int64(s.config.Server.DefaultUploadSize) {
			return httperr.New(http.StatusBadRequest, fmt.Sprintf("File exceeds default upload limit of %d bytes", s.config.Server.DefaultUploadSize))
		}
	}

	return nil
}

func (s *PasteService) createPaste(content io.Reader, apiKey *models.APIKey, size int64, opts *PasteOptions) (*models.Paste, error) {
	// Read content for MIME type detection
	contentBytes, err := io.ReadAll(content)
	if err != nil {
		return nil, httperr.New(http.StatusInternalServerError, "Failed to read content")
	}

	// Check file size against limit either globally or per API key
	if err := s.validateFileSize(size, apiKey); err != nil {
		return nil, err
	}

	// Detect MIME type if not provided
	mime := mimetype.Detect(contentBytes)
	contentType := mime.String()

	// Check if the file has a markdown extension
	if opts.Extension != "" && (opts.Extension == "md" || opts.Extension == "markdown") {
		contentType = "text/markdown"
	} else if opts.Filename != "" {
		ext := strings.ToLower(filepath.Ext(opts.Filename))
		if ext == ".md" || ext == ".markdown" {
			contentType = "text/markdown"
		}
	}

	// Create paste record
	paste := &models.Paste{
		Filename:  opts.Filename,
		MimeType:  contentType,
		Size:      size,
		Extension: opts.Extension,
		Private:   opts.Private,
	}

	// Set extension in order of precedence
	if paste.Extension == "" {
		if paste.Filename != "" {
			parts := strings.Split(paste.Filename, ".")
			if len(parts) > 1 {
				paste.Extension = parts[len(parts)-1]
			}
		}

		if paste.Extension == "" {
			mime := mimetype.Detect(contentBytes)
			paste.Extension = strings.TrimPrefix(mime.Extension(), ".")

			if paste.Extension == "" && strings.HasPrefix(contentType, "text/") {
				paste.Extension = "txt"
			}
		}
	}

	// Calculate expiry time if provided
	expiry, err := s.calculateExpiry(ExpiryOptions{
		Size:      int64(len(contentBytes)),
		HasAPIKey: apiKey != nil,
		ExpiresIn: opts.ExpiresIn,
		ExpiresAt: opts.ExpiresAt,
	})
	if err != nil {
		return nil, httperr.New(http.StatusBadRequest, err.Error())
	}
	paste.ExpiresAt = expiry

	// Set API key if provided
	if apiKey != nil {
		paste.APIKey = apiKey.Key
	}

	// Use a transaction for the entire creation process
	var storagePath string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// Set the default storage configuration
		for _, storage := range s.config.Storage {
			if storage.IsDefault {
				paste.StorageName = storage.Name
				paste.StorageType = storage.Type
				break
			}
		}

		if paste.StorageName == "" {
			return httperr.New(http.StatusInternalServerError, "No default storage configuration found")
		}

		// Create the initial database record
		if err := tx.Create(paste).Error; err != nil {
			return httperr.New(http.StatusInternalServerError, "Failed to save paste")
		}

		// Generate filename
		filename := paste.ID
		if paste.Extension != "" {
			filename = paste.ID + "." + paste.Extension
		}

		// Store the content and get the storage path
		var err error
		storagePath, err = s.storage.Put(filename, bytes.NewReader(contentBytes))
		if err != nil {
			return httperr.New(http.StatusInternalServerError, "Failed to store content")
		}

		// Update the paste with the storage path
		paste.StoragePath = storagePath
		if err := tx.Save(paste).Error; err != nil {
			// Try to cleanup the stored content since we couldn't update the record
			_ = s.storage.Delete(storagePath)
			return httperr.New(http.StatusInternalServerError, "Failed to update paste")
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return paste, nil
}

func (s *PasteService) isTextContent(mimeType string) bool {
	switch {
	case strings.HasPrefix(mimeType, "text/"):
		return true
	case strings.Contains(mimeType, "json"):
		return true
	case strings.Contains(mimeType, "xml"):
		return true
	case strings.Contains(mimeType, "javascript"):
		return true
	case strings.Contains(mimeType, "yaml"):
		return true
	case strings.Contains(mimeType, "x-www-form-urlencoded"):
		return true
	default:
		return false
	}
}

func (s *PasteService) isImageContent(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/")
}

func formatExpiryTime(t *time.Time) string {
	if t == nil {
		return "Never"
	}
	return t.Format("2006-01-02 15:04:05")
}

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

func (s *PasteService) calculateExpiry(opts ExpiryOptions) (*time.Time, error) {
	// Calculate maximum allowed retention based on file size
	maxRetention := s.calculateMaxRetention(opts.Size, opts.HasAPIKey)
	maxDuration := hdur.Hours(maxRetention * 24)

	// Handle explicit expiry requests
	if opts.ExpiresAt != nil {
		now := time.Now()
		if opts.ExpiresAt.Before(now) {
			return nil, httperr.New(http.StatusBadRequest, "Expiration time must be in the future")
		}
		requestedDuration := hdur.Sub(*opts.ExpiresAt, now)
		if requestedDuration.Days > maxDuration.Days {
			return nil, httperr.New(http.StatusBadRequest,
				fmt.Sprintf("Maximum allowed expiry for this file size is %.1f days", float64(maxDuration.Days)))
		}
		return opts.ExpiresAt, nil
	}

	if opts.ExpiresIn != nil {
		if opts.ExpiresIn.Days > maxDuration.Days {
			return nil, httperr.New(http.StatusBadRequest,
				fmt.Sprintf("Maximum allowed expiry for this file size is %.1f days", float64(maxDuration.Days)))
		}
		expiryTime := opts.ExpiresIn.Add(time.Now())
		return &expiryTime, nil
	}

	// If no explicit expiry is set, use maximum retention
	expiryTime := maxDuration.Add(time.Now())
	return &expiryTime, nil
}

func (s *PasteService) calculateMaxRetention(size int64, hasAPIKey bool) float64 {
	// Get retention limits based on API key status
	var retention config.RetentionLimitConfig
	if hasAPIKey {
		retention = s.config.Retention.WithKey
	} else {
		retention = s.config.Retention.NoKey
	}

	// Calculate retention based on file size ratio
	sizeRatio := float64(size) / float64(s.config.Server.MaxUploadSize)
	if sizeRatio > 1 {
		sizeRatio = 1
	}

	// Linear interpolation between min and max age based on size ratio
	return retention.MinAge + (retention.MaxAge-retention.MinAge)*(1-sizeRatio)
}

// GetContent returns the stored bytes for a paste.
//
// The preview handler needs the raw content without writing it to the
// response, so it reads through this instead of round-tripping a response.
func (s *PasteService) GetContent(paste *models.Paste) ([]byte, error) {
	return s.storage.Get(paste.StoragePath)
}
