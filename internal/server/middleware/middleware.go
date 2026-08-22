package middleware

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"hash/crc32"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/watzon/0x45/internal/server/respond"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/server/services"
)

// Middleware holds all middleware instances
type Middleware struct {
	Auth      *AuthMiddleware
	RateLimit *RateLimiter
	db        *gorm.DB
	logger    *zap.Logger
	config    *config.Config
	services  *services.Services
}

// NewMiddleware creates a new Middleware instance with all middleware dependencies
func NewMiddleware(db *gorm.DB, logger *zap.Logger, config *config.Config, services *services.Services) *Middleware {
	return &Middleware{
		Auth:      NewAuthMiddleware(db, logger, config, services),
		RateLimit: NewRateLimiter(logger, config),
		db:        db,
		logger:    logger,
		config:    config,
		services:  services,
	}
}

// Common middleware functions

// Logger returns a middleware that logs request information
func (m *Middleware) Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)

		status := c.Writer.Status()
		m.logger.Debug("request completed",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", status),
			zap.Duration("duration", duration),
			zap.String("ip", c.ClientIP()),
		)
	}
}

// Recover returns a middleware that recovers from panics
func (m *Middleware) Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error("recovered from panic",
					zap.Any("error", r),
					zap.String("stack", string(debug.Stack())),
				)
				respond.AbortJSON(c, http.StatusInternalServerError, gin.H{
					"error": "Internal Server Error",
				})
			}
		}()
		c.Next()
	}
}

// CORS returns a middleware that handles CORS.
//
// gin ships no CORS middleware, so the headers Fiber's cors.New produced are
// written directly here, including the short circuit for preflight requests.
func (m *Middleware) CORS() gin.HandlerFunc {
	const (
		allowMethods = "GET,POST,PUT,DELETE,OPTIONS"
		allowHeaders = "Origin,Content-Type,Accept,Authorization"
		maxAge       = "300"
	)
	allowOrigins := strings.Join(m.config.Server.CORSOrigins, ",")

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		allowed := allowOrigins
		if allowOrigins != "*" {
			if !originAllowed(allowOrigins, origin) {
				c.Next()
				return
			}
			allowed = origin
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Origin", allowed)

		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", allowMethods)
			c.Header("Access-Control-Allow-Headers", allowHeaders)
			c.Header("Access-Control-Max-Age", maxAge)
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// originAllowed reports whether origin appears in a comma separated allow list.
func originAllowed(allowList, origin string) bool {
	for _, candidate := range strings.Split(allowList, ",") {
		if strings.EqualFold(strings.TrimSpace(candidate), origin) {
			return true
		}
	}
	return false
}

// Compression returns a middleware that compresses responses.
//
// gin has no bundled compression middleware; this negotiates gzip and deflate
// from Accept-Encoding the way Fiber's compress middleware did.
func (m *Middleware) Compression() gin.HandlerFunc {
	return func(c *gin.Context) {
		encoding := negotiateEncoding(c.GetHeader("Accept-Encoding"))
		if encoding == "" {
			c.Next()
			return
		}

		original := c.Writer
		compressed := &compressWriter{ResponseWriter: original}
		switch encoding {
		case "br":
			compressed.writer = brotli.NewWriterLevel(original, brotli.DefaultCompression)
		case "gzip":
			compressed.writer = gzip.NewWriter(original)
		default:
			writer, err := flate.NewWriter(original, flate.DefaultCompression)
			if err != nil {
				c.Next()
				return
			}
			compressed.writer = writer
		}

		c.Header("Content-Encoding", encoding)
		c.Header("Vary", "Accept-Encoding")
		c.Writer.Header().Del("Content-Length")

		c.Writer = compressed
		defer func() {
			_ = compressed.writer.Close()
			c.Writer = original
		}()

		c.Next()
	}
}

// negotiateEncoding picks the response encoding for an Accept-Encoding header.
//
// The preference order matches the fasthttp compressor Fiber wrapped: brotli
// first, then gzip, then deflate.
func negotiateEncoding(accept string) string {
	accept = strings.ToLower(accept)
	switch {
	case strings.Contains(accept, "br"):
		return "br"
	case strings.Contains(accept, "gzip"):
		return "gzip"
	case strings.Contains(accept, "deflate"):
		return "deflate"
	default:
		return ""
	}
}

// compressWriter streams a handler's output through a compressor.
type compressWriter struct {
	gin.ResponseWriter
	writer interface {
		Write([]byte) (int, error)
		Close() error
	}
}

func (w *compressWriter) Write(data []byte) (int, error) {
	return w.writer.Write(data)
}

func (w *compressWriter) WriteString(s string) (int, error) {
	return w.writer.Write([]byte(s))
}

// RequestID returns a middleware that adds a request ID to each request
func (m *Middleware) RequestID() gin.HandlerFunc {
	const header = "X-Request-ID"

	return func(c *gin.Context) {
		id := c.GetHeader(header)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("requestid", id)
		c.Header(header, id)
		c.Next()
	}
}

// ETag returns a middleware that adds ETag headers.
//
// gin has no ETag middleware, so the response body is buffered and hashed with
// the same length-CRC32 scheme Fiber used, including If-None-Match handling.
func (m *Middleware) ETag() gin.HandlerFunc {
	const crcPol = 0xD5828281
	table := crc32.MakeTable(crcPol)

	return func(c *gin.Context) {
		original := c.Writer
		buffered := &bufferedWriter{ResponseWriter: original, status: http.StatusOK}
		c.Writer = buffered

		defer func() {
			c.Writer = original
			body := buffered.body.Bytes()

			// Skip invalid responses, empty bodies, and responses that already
			// carry a validator.
			if buffered.status != http.StatusOK || len(body) == 0 || original.Header().Get("Etag") != "" {
				buffered.flush(original)
				return
			}

			etag := fmt.Sprintf(`"%d-%d"`, len(body), crc32.Checksum(body, table))
			clientETag := strings.TrimPrefix(c.GetHeader("If-None-Match"), "W/")
			if clientETag != "" && strings.Contains(clientETag, etag) {
				original.WriteHeader(http.StatusNotModified)
				return
			}

			original.Header().Set("Etag", etag)
			buffered.flush(original)
		}()

		c.Next()
	}
}

// bufferedWriter captures a handler's response so it can be hashed before it
// reaches the client.
type bufferedWriter struct {
	gin.ResponseWriter
	body     bytes.Buffer
	status   int
	explicit bool
}

func (w *bufferedWriter) WriteHeader(code int) {
	w.status = code
	w.explicit = true
}

func (w *bufferedWriter) WriteHeaderNow() {}

func (w *bufferedWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *bufferedWriter) WriteString(s string) (int, error) {
	return w.body.WriteString(s)
}

func (w *bufferedWriter) Status() int {
	return w.status
}

func (w *bufferedWriter) Size() int {
	return w.body.Len()
}

func (w *bufferedWriter) Written() bool {
	return w.body.Len() > 0
}

// flush replays the buffered response onto the real writer. When no handler ran
// at all it leaves the writer untouched: gin resolves unmatched routes and
// rejected methods after the middleware chain returns, and committing a status
// here would make gin treat the response as already written and skip its own.
func (w *bufferedWriter) flush(out gin.ResponseWriter) {
	if !w.explicit && w.body.Len() == 0 {
		return
	}
	out.WriteHeader(w.status)
	if w.body.Len() > 0 {
		_, _ = out.Write(w.body.Bytes())
	}
}

// GetMiddleware returns all middleware handlers in the recommended order
func (m *Middleware) GetMiddleware() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		m.RequestID(),
		// m.Logger(),
		m.Recover(),
		m.CORS(),
		m.Compression(),
		m.ETag(),
	}
}
