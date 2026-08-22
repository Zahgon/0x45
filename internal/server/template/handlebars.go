package template

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/mailgun/raymond/v2"
	"go.uber.org/zap"
)

// htmlContentType is the Content-Type emitted for every rendered view.
var htmlContentType = []string{"text/html; charset=utf-8"}

// Binding carries the view data together with the layout that should wrap it.
// gin's HTMLRender only passes a single object to Instance, so the layout name
// travels alongside the data instead of as a separate argument.
type Binding struct {
	Data   map[string]any
	Layout string
}

// WithLayout pairs view data with the layout used to wrap it.
func WithLayout(data map[string]any, layout string) Binding {
	return Binding{Data: data, Layout: layout}
}

// MultiHandlebars is a custom handlebars engine that supports multiple template directories.
// It implements gin's render.HTMLRender interface.
type MultiHandlebars struct {
	// Directory is the primary template directory.
	Directory string
	// Extension is the template file extension, including the leading dot.
	Extension string
	// Templates holds every parsed template, keyed by its path relative to the
	// directory it was loaded from (layouts are additionally keyed without the
	// extension).
	Templates map[string]*raymond.Template

	fallbackDir string
	logger      *zap.Logger
}

// New creates a new MultiHandlebars engine
func New(viewsDir, fallbackDir string, extension string, logger *zap.Logger) *MultiHandlebars {
	// Handle @views special path
	if viewsDir == "@views" {
		logger.Info("using fallback directory as primary directory",
			zap.String("fallback_dir", fallbackDir))
		viewsDir = fallbackDir
	}

	// Get absolute paths
	absViewsDir, err := filepath.Abs(viewsDir)
	if err != nil {
		panic(fmt.Sprintf("failed to get absolute path for views directory: %v", err))
	}

	absFallbackDir, err := filepath.Abs(fallbackDir)
	if err != nil {
		panic(fmt.Sprintf("failed to get absolute path for fallback directory: %v", err))
	}

	logger.Info("initializing template engine",
		zap.String("views_dir", absViewsDir),
		zap.String("fallback_dir", absFallbackDir))

	// Register helpers. raymond's helper registry is process global, so drop any
	// previous registration first to stay re-entrant.
	raymond.RemoveHelper("startsWith")
	raymond.RegisterHelper("startsWith", func(str, prefix string) bool {
		return strings.HasPrefix(str, prefix)
	})

	raymond.RemoveHelper("or")
	raymond.RegisterHelper("or", func(args ...interface{}) bool {
		for _, arg := range args {
			// Convert to boolean and check if true
			switch v := arg.(type) {
			case bool:
				if v {
					return true
				}
			case string:
				if v != "" {
					return true
				}
			case int, int64, float64:
				if v != 0 {
					return true
				}
			default:
				if v != nil {
					return true
				}
			}
		}
		return false
	})

	raymond.RemoveHelper("eq")
	raymond.RegisterHelper("eq", func(a, b interface{}) bool {
		return a == b
	})

	engine := &MultiHandlebars{
		Directory:   absViewsDir,
		Extension:   extension,
		Templates:   make(map[string]*raymond.Template),
		fallbackDir: absFallbackDir,
		logger:      logger,
	}

	// Load templates immediately
	if err := engine.Load(); err != nil {
		panic(fmt.Sprintf("failed to load templates: %v", err))
	}

	return engine
}

// loadFromDir loads templates from a directory
func (e *MultiHandlebars) loadFromDir(dir string) error {
	// Skip if directory doesn't exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		e.logger.Warn("directory does not exist, skipping", zap.String("dir", dir))
		return nil
	}

	e.logger.Info("loading templates from directory", zap.String("dir", dir))

	// Walk the directory
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip if not a file or doesn't have the right extension
		if info.IsDir() || !strings.HasSuffix(path, e.Extension) {
			return nil
		}

		// Get relative path from the directory
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path: %w", err)
		}

		// Skip if template already exists (for fallback directory)
		if e.Templates[rel] != nil {
			e.logger.Debug("template already exists, skipping",
				zap.String("path", path),
				zap.String("rel_path", rel))
			return nil
		}

		// Read the file
		buf, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", path, err)
		}

		// Parse the template
		tmpl, err := raymond.Parse(string(buf))
		if err != nil {
			return fmt.Errorf("failed to parse template %s: %w", path, err)
		}

		// Register partials
		if strings.Contains(rel, "partials/") {
			name := strings.TrimSuffix(rel, e.Extension)
			// raymond's partial registry is process global and panics on a
			// duplicate name, so drop any previous registration first.
			raymond.RemovePartial(name)
			raymond.RegisterPartial(name, string(buf))
			e.logger.Debug("registered partial",
				zap.String("name", name),
				zap.String("path", path))
		}

		// Register layouts
		if strings.Contains(rel, "layouts/") {
			name := strings.TrimSuffix(rel, e.Extension)
			e.Templates[name] = tmpl
			e.logger.Debug("registered layout",
				zap.String("name", name),
				zap.String("path", path))
		}

		// Register regular templates
		e.Templates[rel] = tmpl
		e.logger.Debug("registered template",
			zap.String("rel_path", rel),
			zap.String("path", path))

		return nil
	})
}

// Load parses every template from the primary and fallback directories.
func (e *MultiHandlebars) Load() error {
	e.logger.Info("loading templates")

	// First load templates from the primary directory
	if err := e.loadFromDir(e.Directory); err != nil {
		e.logger.Error("failed to load templates from primary directory",
			zap.String("dir", e.Directory),
			zap.Error(err))
		return err
	}

	// Then load templates from the fallback directory
	// Only load templates that don't exist in the primary directory
	if err := e.loadFromDir(e.fallbackDir); err != nil {
		e.logger.Error("failed to load templates from fallback directory",
			zap.String("dir", e.fallbackDir),
			zap.Error(err))
		return err
	}

	// Verify that we have all required templates
	requiredTemplates := []string{
		"index.hbs",
		"layouts/main.hbs",
		"partials/head.hbs",
	}

	missingTemplates := []string{}
	for _, tmpl := range requiredTemplates {
		if e.Templates[tmpl] == nil {
			missingTemplates = append(missingTemplates, tmpl)
		}
	}

	if len(missingTemplates) > 0 {
		e.logger.Error("missing required templates",
			zap.Strings("missing", missingTemplates))
		return fmt.Errorf("missing required templates: %v", missingTemplates)
	}

	e.logger.Info("finished loading templates",
		zap.Int("total_templates", len(e.Templates)))

	// Log all loaded templates at debug level
	templates := make([]string, 0, len(e.Templates))
	for name := range e.Templates {
		templates = append(templates, name)
	}
	e.logger.Debug("loaded templates", zap.Strings("templates", templates))

	return nil
}

// Instance implements gin's render.HTMLRender interface. The data may either be
// a Binding (view data plus layout) or a plain map, in which case no layout is
// applied.
func (e *MultiHandlebars) Instance(name string, data any) render.Render {
	binding, ok := data.(Binding)
	if !ok {
		if m, isMap := data.(map[string]any); isMap {
			binding = Binding{Data: m}
		} else {
			binding = Binding{Data: map[string]any{}}
		}
	}

	return &htmlRender{engine: e, name: name, binding: binding}
}

// htmlRender renders a single view through the handlebars engine.
type htmlRender struct {
	engine  *MultiHandlebars
	name    string
	binding Binding
}

// Render writes the rendered view to the response.
func (r *htmlRender) Render(w http.ResponseWriter) error {
	r.WriteContentType(w)

	if r.binding.Layout == "" {
		return r.engine.Render(w, r.name, r.binding.Data)
	}
	return r.engine.Render(w, r.name, r.binding.Data, r.binding.Layout)
}

// WriteContentType sets the HTML content type on the response.
func (r *htmlRender) WriteContentType(w http.ResponseWriter) {
	header := w.Header()
	if len(header["Content-Type"]) == 0 {
		header["Content-Type"] = htmlContentType
	}
}

// Render executes a template, optionally wrapped in a layout, and writes the
// result to out.
func (e *MultiHandlebars) Render(out io.Writer, template string, binding interface{}, layout ...string) error {
	e.logger.Debug("rendering template",
		zap.String("template", template),
		zap.Any("layout", layout),
		zap.Any("binding", binding))

	// Get the template
	tmpl := e.Templates[template+e.Extension]
	if tmpl == nil {
		e.logger.Error("template not found",
			zap.String("template", template),
			zap.String("extension", e.Extension))
		return fmt.Errorf("template %s not found", template)
	}

	// If layout is specified, wrap the content in the layout
	var content interface{}
	if len(layout) > 0 && layout[0] != "" {
		layoutName := layout[0] + e.Extension
		layoutTmpl := e.Templates[layoutName]
		if layoutTmpl == nil {
			e.logger.Error("layout not found",
				zap.String("layout", layoutName))
			return fmt.Errorf("layout %s not found", layoutName)
		}

		// Execute the main template first
		result, err := tmpl.Exec(binding)
		if err != nil {
			e.logger.Error("failed to execute template",
				zap.String("template", template),
				zap.Error(err))
			return err
		}

		// Create layout binding with the template result
		layoutBinding := map[string]any{
			"embed": raymond.SafeString(result),
		}
		// Add all original binding values to layout binding
		if m, ok := binding.(map[string]any); ok {
			for k, v := range m {
				layoutBinding[k] = v
			}
		}

		content = layoutBinding
		tmpl = layoutTmpl
	} else {
		content = binding
	}

	// Execute the final template
	result, err := tmpl.Exec(content)
	if err != nil {
		e.logger.Error("failed to execute template",
			zap.String("template", template),
			zap.Error(err))
		return err
	}

	_, err = out.Write([]byte(result))
	return err
}

// Render writes a handlebars view to the response with the standard 200 status.
//
// gin's c.HTML only accepts a single data object, so the optional layout is
// folded into a Binding here. This keeps the layout selection at the call site,
// the way the views expect it.
func Render(c *gin.Context, name string, data map[string]any, layout ...string) error {
	binding := Binding{Data: data}
	if len(layout) > 0 {
		binding.Layout = layout[0]
	}
	c.HTML(http.StatusOK, name, binding)
	return nil
}
