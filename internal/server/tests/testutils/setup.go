package testutils

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/database"
	"github.com/watzon/0x45/internal/models"
	"github.com/watzon/0x45/internal/server"
	"github.com/watzon/0x45/internal/storage"
	"go.uber.org/zap"
)

type TestEnv struct {
	App       http.Handler
	Server    *server.Server
	DB        *database.Database
	Config    *config.Config
	Storage   *storage.StorageManager
	Logger    *zap.Logger
	TempDir   string
	CleanupFn func()
}

func SetupTestEnv(t *testing.T) *TestEnv {
	t.Helper()

	// Create temp directory for uploads and views
	tempDir, err := os.MkdirTemp("", "0x45-test-*")
	if err != nil {
		t.Fatal(err)
	}

	// Create views directory and copy templates if needed
	viewsDir := filepath.Join(tempDir, "views")
	if err := os.MkdirAll(viewsDir, 0755); err != nil {
		os.RemoveAll(tempDir)
		t.Fatal(err)
	}

	if err := copyViews(viewsDir); err != nil {
		os.RemoveAll(tempDir)
		t.Fatal(err)
	}

	pubDir := filepath.Join(tempDir, "public")
	if err := os.MkdirAll(pubDir, 0755); err != nil {
		os.RemoveAll(tempDir)
		t.Fatal(err)
	}

	// Create test config
	cfg := &config.Config{
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			Name:   tempDir + "/paste69.db",
		},
		Storage: []config.StorageConfig{
			{
				Name:      "local",
				Type:      "local",
				Path:      tempDir,
				IsDefault: true,
			},
		},
		Server: config.ServerConfig{
			MaxUploadSize:     100 * 1024 * 1024, // 10MB
			DefaultUploadSize: 5 * 1024 * 1024,   // 5MB
			APIUploadSize:     8 * 1024 * 1024,   // 8MB
			AppName:           "0x45-test",
			ServerHeader:      "0x45-test",
			ViewsDirectory:    viewsDir,
			PublicDirectory:   pubDir,
		},
		Retention: config.RetentionConfig{
			NoKey: config.RetentionLimitConfig{
				MinAge: 1,  // 1 day minimum
				MaxAge: 30, // 30 days maximum
			},
			WithKey: config.RetentionLimitConfig{
				MinAge: 1,   // 1 day minimum
				MaxAge: 365, // 365 days maximum
			},
		},
	}

	// Initialize test logger
	logger, err := zap.NewDevelopment()
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatal(err)
	}
	defer func() { _ = logger.Sync() }()

	// Create server instance with modified config
	origCfg := *cfg                             // Make a copy of the original config
	cfg.Server.MaxUploadSize = 10 * 1024 * 1024 // 10MB
	cfg.Server.AppName = "0x45-test"
	cfg.Server.ServerHeader = "0x45-test"

	// Create server instance
	srv := server.New(cfg, logger)
	srv.SetupRoutes()

	// Add test API key
	err = srv.GetDB().Create(&models.APIKey{Email: "test@example.com", Key: "test-api-key", Verified: true, AllowShortlinks: true}).Error
	if err != nil {
		logger.Error("Error creating test API key", zap.Error(err))
		os.RemoveAll(tempDir)
		t.Fatal(err)
	}

	cleanup := func() {
		if err := srv.Cleanup(); err != nil {
			logger.Error("failed cleaning up server", zap.Error(err))
		}
		os.RemoveAll(tempDir)
	}

	return &TestEnv{
		App:       srv.Handler(),
		Server:    srv,
		DB:        srv.GetDB(),
		Config:    &origCfg,
		Storage:   srv.GetStorage(),
		Logger:    logger,
		TempDir:   tempDir,
		CleanupFn: cleanup,
	}
}

// Request serves req through the application handler and returns the recorded
// response. Fiber offered app.Test for this; gin is plain net/http, so the
// request goes through httptest instead. The error return keeps the call sites
// shaped like the http.Client contract the tests were written against.
func (e *TestEnv) Request(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	e.App.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// copyViews mirrors the repository views directory into dst. The template
// engine requires a populated primary views directory, and its "./views"
// fallback resolves relative to the test package rather than the repo root.
func copyViews(dst string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	src := filepath.Join(root, "views")
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, buf, 0644)
	})
}

// RepoRoot exposes the module root so a suite can run from it. Parts of the
// application resolve asset paths against the process working directory.
func RepoRoot() (string, error) {
	return repoRoot()
}

// repoRoot walks up from the working directory until it finds the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate module root from %s", dir)
		}
		dir = parent
	}
}
