package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/database"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/server/handlers"
	"github.com/watzon/0x45/internal/server/middleware"
	"github.com/watzon/0x45/internal/server/respond"
	"github.com/watzon/0x45/internal/server/services"
	"github.com/watzon/0x45/internal/server/template"
	"github.com/watzon/0x45/internal/storage"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Server struct {
	app        *gin.Engine
	handler    http.Handler
	http       *http.Server
	routesOnce sync.Once
	db         *database.Database
	storage    *storage.StorageManager
	config     *config.Config
	logger     *zap.Logger
	services   *services.Services
	handlers   *handlers.Handlers
	middleware *middleware.Middleware
}

func New(config *config.Config, logger *zap.Logger) *Server {
	// Register markdown MIME types
	mimetype.Extend(func(raw []byte, limit uint32) bool {
		// Check for common markdown headers
		content := string(raw)
		if len(content) > 0 {
			firstLine := strings.Split(content, "\n")[0]
			if strings.HasPrefix(firstLine, "# ") || strings.HasPrefix(firstLine, "## ") {
				return true
			}
		}
		return false
	}, "text/markdown", ".md", ".markdown")

	// Initialize database
	db, err := database.New(config, &gorm.Config{
		// Logger: gormLogger,
	})
	if err != nil {
		logger.Fatal("Error connecting to database", zap.Error(err))
	}

	// Run migrations
	if err := db.Migrate(config); err != nil {
		logger.Fatal("Error running migrations", zap.Error(err))
	}

	// Initialize storage manager
	storageManager, err := storage.NewStorageManager(config)
	if err != nil {
		logger.Fatal("Failed to initialize storage", zap.Error(err))
	}

	// Initialize template engine with fallback support
	engine := template.New(config.Server.ViewsDirectory, "./views", ".hbs", logger)

	// Initialize services
	svc := services.NewServices(db.DB, logger, config)

	// Initialize middleware
	mw := middleware.NewMiddleware(db.DB, logger, config, svc)

	// Initialize handlers
	hdl := handlers.NewHandlers(db.DB, logger, config, svc)

	// Initialize the gin engine
	gin.SetMode(gin.ReleaseMode)
	app := gin.New()
	app.HTMLRender = engine

	// Trust X-Forwarded-For for client IPs, the equivalent of Fiber's ProxyHeader.
	app.ForwardedByClientIP = true
	if err := app.SetTrustedProxies(nil); err != nil {
		logger.Error("failed to configure trusted proxies", zap.Error(err))
	}
	app.RemoteIPHeaders = []string{"X-Forwarded-For"}

	// Fiber answered unmatched paths and rejected verbs through its ErrorHandler,
	// so both came back as JSON. gin renders plain text and leaves 405 off by
	// default, so the two cases are wired back to the original payloads here.
	app.HandleMethodNotAllowed = true
	app.NoRoute(func(c *gin.Context) {
		respond.AbortJSON(c, http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Cannot %s %s", c.Request.Method, c.Request.URL.Path),
		})
	})
	app.NoMethod(func(c *gin.Context) {
		respond.AbortJSON(c, http.StatusMethodNotAllowed, gin.H{"error": "Method Not Allowed"})
	})

	// Fiber advertised itself through Config.ServerHeader; gin has no such
	// option, so the header is written by a middleware instead.
	if config.Server.ServerHeader != "" {
		serverHeader := config.Server.ServerHeader
		app.Use(func(c *gin.Context) {
			c.Header("Server", serverHeader)
			c.Next()
		})
	}

	// Add all middleware in the correct order
	app.Use(mw.GetMiddleware()...)

	// Serve static files
	app.Static("/public", config.Server.PublicDirectory)

	server := &Server{
		app:        app,
		db:         db,
		storage:    storageManager,
		config:     config,
		logger:     logger,
		services:   svc,
		handlers:   hdl,
		middleware: mw,
	}

	// Fiber applied the body limit and the _method override before routing.
	// gin middleware only runs after a route matches, so both live in an
	// http.Handler that wraps the engine.
	server.handler = withMethodOverride(withBodyLimit(app, int64(config.Server.MaxUploadSize)))

	return server
}

// withBodyLimit rejects payloads larger than the configured upload size, the
// replacement for Fiber's Config.BodyLimit.
func withBodyLimit(next http.Handler, limit int64) http.Handler {
	if limit <= 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"Request Entity Too Large"}`))
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// withMethodOverride lets HTML forms emulate PUT and DELETE through a _method
// field. Fiber could rewrite the method from a middleware because it matched
// routes lazily; gin resolves the route first, so the rewrite has to happen
// before the engine sees the request.
func withMethodOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			contentType := r.Header.Get("Content-Type")
			if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
				strings.HasPrefix(contentType, "multipart/form-data") {
				// PostFormValue parses the body once and caches it, so the
				// handlers still see the form fields and uploaded files.
				if method := r.PostFormValue("_method"); method != "" {
					r.Method = strings.ToUpper(method)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// SetupRoutes configures all the routes for the server. It is safe to call
// more than once; gin panics when a route is registered twice.
func (s *Server) SetupRoutes() {
	s.routesOnce.Do(s.registerRoutes)
}

// SetupMiddleware configures all the middleware for the server.
//
// The _method override Fiber registered here now lives in the http.Handler
// wrapper built by New, because gin resolves routes before middleware runs.
func (s *Server) SetupMiddleware() {
	// Setup CORS
	s.app.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	// Add request logging
	s.app.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("%s %s %d %s %s %s\n",
			param.TimeStamp.Format("2006/01/02 15:04:05"),
			param.ClientIP,
			param.StatusCode,
			param.Latency,
			param.Method,
			param.Path,
		)
	}))
}

func (s *Server) registerRoutes() {
	// Setup middleware first
	s.SetupMiddleware()

	// Web interface routes
	s.app.GET("/", httperr.Wrap(s.handlers.Web.HandleIndex))
	s.app.GET("/stats", httperr.Wrap(s.handlers.Web.HandleStats))
	s.app.GET("/docs", httperr.Wrap(s.handlers.Web.HandleDocs))
	s.app.GET("/submit", httperr.Wrap(s.handlers.Web.HandleSubmit))

	// API Key routes
	keys := s.app.Group("/keys")
	keys.POST("/request", httperr.Wrap(s.handlers.APIKey.HandleRequestAPIKey))
	keys.GET("/verify", httperr.Wrap(s.handlers.APIKey.HandleVerifyAPIKey))

	// URL redirect route - must be before the group to avoid auth middleware
	s.app.GET("/u/:id", httperr.Wrap(s.handlers.URL.HandleRedirect))

	// URL management routes
	urls := s.app.Group("/u")
	urls.Use(s.middleware.Auth.Auth(true))
	urls.POST("/", httperr.Wrap(s.handlers.URL.HandleURLShorten))
	urls.GET("/list", httperr.Wrap(s.handlers.URL.HandleListURLs))
	urls.GET("/:id/stats", httperr.Wrap(s.handlers.URL.HandleURLStats))
	urls.DELETE("/:id", httperr.Wrap(s.handlers.URL.HandleDeleteURL))
	urls.PUT("/:id/expiry", httperr.Wrap(s.handlers.URL.HandleUpdateURLExpiration))

	// Paste routes - authenticated routes first
	pastes := s.app.Group("/p")
	pastes.POST("/", s.middleware.Auth.Auth(false), httperr.Wrap(s.handlers.Paste.HandleUpload))
	pastes.GET("/list", s.middleware.Auth.Auth(true), httperr.Wrap(s.handlers.Paste.HandleListPastes))
	pastes.DELETE("/:id", s.middleware.Auth.Auth(false), httperr.Wrap(s.handlers.Paste.HandleDeletePaste))
	pastes.PUT("/:id/expiry", s.middleware.Auth.Auth(true), httperr.Wrap(s.handlers.Paste.HandleUpdateExpiration))

	// Public paste routes.
	//
	// Fiber also declared /p/:id.:ext, /p/:id/raw.:ext, /p/:id/download.:ext
	// and /p/:id.:ext/image. gin allows only one wildcard per path segment and
	// panics on those shapes, so the extension is parsed inside the handlers:
	//   /p/abc.txt        -> /p/:id        with id "abc.txt"
	//   /p/abc.txt/image  -> /p/:id/image  with id "abc.txt"
	//   /p/abc/raw.txt    -> /p/:id/:key   dispatched by HandlePasteSegment
	// GetPaste strips the extension, so every URL form still resolves.
	s.app.GET("/p/:id", httperr.Wrap(s.handlers.Paste.HandleView))
	s.app.GET("/p/:id/raw", httperr.Wrap(s.handlers.Paste.HandleRawView))
	s.app.GET("/p/:id/download", httperr.Wrap(s.handlers.Paste.HandleDownload))
	s.app.GET("/p/:id/image", httperr.Wrap(s.handlers.Paste.HandleGetPasteImage))
	s.app.GET("/p/:id/preview", httperr.Wrap(s.handlers.Paste.HandlePreview))
	s.app.DELETE("/p/:id/:key", httperr.Wrap(s.handlers.Paste.HandleDeleteWithKey))
	s.app.GET("/p/:id/:key", httperr.Wrap(s.handlers.Paste.HandlePasteSegment))

	s.logger.Info("routes registered", zap.Int("count", len(s.app.Routes())))
}

func (s *Server) Start(addr string) error {
	// Start cleanup scheduler
	if s.config.Server.Cleanup.Enabled {
		interval := fmt.Sprintf("%ds", s.config.Server.Cleanup.Interval)
		if err := s.services.StartCleanupScheduler(interval); err != nil {
			s.logger.Error("failed to start cleanup scheduler", zap.Error(err))
		}
	}

	// Setup routes
	s.SetupRoutes()

	// Start server
	s.http = &http.Server{
		Addr:    addr,
		Handler: s.handler,
	}

	// Fiber printed Config.AppName in its startup banner; gin has no banner, so the
	// configured app name is surfaced here instead. It was never sent on the wire.
	s.logger.Info("server listening",
		zap.String("address", addr),
		zap.String("app_name", s.config.Server.AppName))
	// fiber.Config{Prefork} re-implemented over net/http — see prefork.go.
	if s.config.Server.Prefork {
		return s.listenPrefork(addr)
	}

	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// GetApp returns the gin engine backing the server.
func (s *Server) GetApp() *gin.Engine {
	return s.app
}

// Handler returns the outermost http.Handler, including the body limit and
// _method override that sit in front of the gin engine.
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) GetDB() *database.Database {
	return s.db
}

func (s *Server) GetStorage() *storage.StorageManager {
	return s.storage
}

func (s *Server) GetConfig() *config.Config {
	return s.config
}

func (s *Server) GetLogger() *zap.Logger {
	return s.logger
}

func (s *Server) GetServices() *services.Services {
	return s.services
}

func (s *Server) GetHandlers() *handlers.Handlers {
	return s.handlers
}

func (s *Server) GetMiddleware() *middleware.Middleware {
	return s.middleware
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

func (s *Server) Cleanup() error {
	if s.db != nil && s.db.DB != nil {
		if err := s.db.Close(); err != nil {
			s.logger.Error("failed to close database", zap.Error(err))
		}
	}
	return nil
}
