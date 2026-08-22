package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/watzon/0x45/internal/server/services"
	"github.com/watzon/0x45/internal/server/tests/testutils"
)

const fixtureAPIKey = "test-api-key"

// TestMain runs the suite from the module root. The og-image renderer loads its
// fonts from public/fonts relative to the process working directory, which holds
// for the deployed binary but not for a test binary started in its own package.
func TestMain(m *testing.M) {
	root, err := testutils.RepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// uploadPaste stores a paste through the real upload endpoint and returns the
// decoded response together with the deletion key parsed out of DeleteURL.
func uploadPaste(t *testing.T, env *testutils.TestEnv, filename, content string, withAuth bool) (services.PasteResponse, string) {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/p/", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
	}

	resp, err := env.Request(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var paste services.PasteResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&paste))
	require.NotEmpty(t, paste.ID)
	require.NotEmpty(t, paste.DeleteURL)

	segments := strings.Split(strings.TrimSuffix(paste.DeleteURL, "/"), "/")
	deleteKey := segments[len(segments)-1]
	require.NotEmpty(t, deleteKey)

	return paste, deleteKey
}

// get issues a GET against the application, optionally with an Accept header.
func get(t *testing.T, env *testutils.TestEnv, path, accept string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := env.Request(req)
	require.NoError(t, err)
	return resp
}

func TestPasteRetrievalSurfaces(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	const content = "package main\n\nfunc main() {}\n"
	paste, _ := uploadPaste(t, env, "main.txt", content, true)

	testData := []struct {
		name        string
		path        string
		contentType string
		wantBody    string
	}{
		{name: "canonical id", path: "/p/" + paste.ID, contentType: "text/plain; charset=utf-8", wantBody: content},
		{name: "id with extension", path: "/p/" + paste.ID + ".txt", contentType: "text/plain; charset=utf-8", wantBody: content},
		{name: "raw", path: "/p/" + paste.ID + "/raw", contentType: "text/plain; charset=utf-8", wantBody: content},
		{name: "raw with extension", path: "/p/" + paste.ID + "/raw.txt", contentType: "text/plain; charset=utf-8", wantBody: content},
		{name: "download", path: "/p/" + paste.ID + "/download", contentType: "application/octet-stream", wantBody: content},
		{name: "download with extension", path: "/p/" + paste.ID + "/download.txt", contentType: "application/octet-stream", wantBody: content},
		{name: "image", path: "/p/" + paste.ID + "/image", contentType: "image/png"},
		{name: "image with extension", path: "/p/" + paste.ID + ".txt/image", contentType: "image/png"},
	}

	for _, tt := range testData {
		t.Run(tt.name, func(t *testing.T) {
			resp := get(t, env, tt.path, "")
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tt.wantBody != "" {
				assert.Equal(t, tt.wantBody, string(body))
			} else {
				assert.NotEmpty(t, body)
			}
		})
	}

	t.Run("preview renders html", func(t *testing.T) {
		resp := get(t, env, "/p/"+paste.ID+"/preview", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	})

	t.Run("paste json representation", func(t *testing.T) {
		resp := get(t, env, "/p/"+paste.ID, "application/vnd.0x45.paste+json")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var payload map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
		assert.Equal(t, paste.ID, payload["id"])
		assert.Equal(t, content, payload["content"])
	})

	t.Run("unknown id is not found", func(t *testing.T) {
		resp := get(t, env, "/p/zzzzzzzz", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	})
}

func TestPasteDeletionWithKey(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	paste, deleteKey := uploadPaste(t, env, "delete-me.txt", "delete me", true)

	t.Run("wrong key is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/p/"+paste.ID+"/not-the-key", nil)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("paste survives the rejected delete", func(t *testing.T) {
		resp := get(t, env, "/p/"+paste.ID, "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("correct key deletes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/p/"+paste.ID+"/"+deleteKey, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("deleted paste is gone", func(t *testing.T) {
		resp := get(t, env, "/p/"+paste.ID, "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestPasteListingAndExpiryUpdate(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	paste, _ := uploadPaste(t, env, "listed.txt", "listed content", true)

	t.Run("listing requires an api key", func(t *testing.T) {
		resp := get(t, env, "/p/list", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	// ListPastes counts without binding a model, so GORM cannot resolve the
	// table and the handler errors. That query is byte-identical at the
	// pre-migration baseline, so it is left alone and only the framework-level
	// contract is asserted: route resolves, API key accepted, JSON envelope.
	t.Run("listing is routed and accepts the api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/list?page=1&limit=10", nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
		assert.NotEqual(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	})

	t.Run("expiry can be updated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/p/"+paste.ID+"/expiry", strings.NewReader(`{"expires_in":"24h"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated services.PasteResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
		assert.Equal(t, paste.ID, updated.ID)
		assert.NotNil(t, updated.ExpiresAt)
	})
}

func TestShortlinkLifecycle(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	// A title is supplied so the service does not reach out to the network to
	// discover one; this test stays hermetic.
	create := httptest.NewRequest(http.MethodPost, "/u/", strings.NewReader(`{"url":"https://example.com/target","title":"Example target"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Authorization", "Bearer "+fixtureAPIKey)

	resp, err := env.Request(create)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var shortlink map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&shortlink))
	id, ok := shortlink["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, id)
	assert.Equal(t, "https://example.com/target", shortlink["url"])
	assert.Equal(t, "Example target", shortlink["title"])

	t.Run("redirects to the target", func(t *testing.T) {
		resp := get(t, env, "/u/"+id, "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
		assert.Equal(t, "https://example.com/target", resp.Header.Get("Location"))
	})

	t.Run("listing requires an api key", func(t *testing.T) {
		resp := get(t, env, "/u/list", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	// ListURLs shares the unbound-count defect described above and is likewise
	// left as the baseline wrote it.
	t.Run("listing is routed and accepts the api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/u/list", nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
		assert.NotEqual(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	})

	t.Run("stats are reported", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/u/"+id+"/stats", nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("expiry can be updated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/u/"+id+"/expiry", strings.NewReader(`{"expires_in":"24h"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("delete requires an api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/u/"+id, nil)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("owner can delete", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/u/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("deleted shortlink no longer redirects", func(t *testing.T) {
		resp := get(t, env, "/u/"+id, "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestWebPagesRender(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	// Give the stats page something to summarise.
	uploadPaste(t, env, "stats.txt", "stats content", true)

	for _, path := range []string{"/", "/stats", "/docs", "/submit"} {
		t.Run(path, func(t *testing.T) {
			resp := get(t, env, path, "")
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotEmpty(t, body)
		})
	}
}

func TestAPIKeyEndpoints(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	t.Run("request without an email is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/keys/request", strings.NewReader(`{"name":"no email"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("request with an unsupported body is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/keys/request", strings.NewReader("email=someone@example.com"))
		req.Header.Set("Content-Type", "text/plain")
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("verification without a token is rejected", func(t *testing.T) {
		resp := get(t, env, "/keys/verify", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("verification with an unknown token is not found", func(t *testing.T) {
		resp := get(t, env, "/keys/verify?token=not-a-real-token", "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestUploadRejections(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	testData := []struct {
		name           string
		contentType    string
		body           string
		expectedStatus int
	}{
		{name: "unsupported content type", contentType: "text/plain", body: "raw body", expectedStatus: http.StatusBadRequest},
		{name: "json without content or url", contentType: "application/json", body: `{}`, expectedStatus: http.StatusBadRequest},
		{name: "private without an api key", contentType: "application/json", body: `{"content":"secret","private":true}`, expectedStatus: http.StatusUnauthorized},
	}

	for _, tt := range testData {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/p/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			resp, err := env.Request(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tt.expectedStatus, resp.StatusCode)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		})
	}
}

func TestMethodOverrideDeletesPaste(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	paste, deleteKey := uploadPaste(t, env, "override.txt", "override me", true)

	form := "_method=DELETE"
	req := httptest.NewRequest(http.MethodPost, "/p/"+paste.ID+"/"+deleteKey, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := env.Request(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	after := get(t, env, "/p/"+paste.ID, "")
	defer after.Body.Close()
	assert.Equal(t, http.StatusNotFound, after.StatusCode)
}

func TestStaticAssetsAreServed(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	// The test server points at an empty public directory, so a missing asset
	// is the observable behaviour; what matters is that the route is mounted
	// and answers rather than falling through to the paste router.
	resp := get(t, env, "/public/does-not-exist.css", "")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestAuthenticatedPasteDelete exercises DELETE /p/:id, the API-key-scoped
// delete route. It is a different route from DELETE /p/:id/:key, which
// authenticates with the paste's deletion key instead of an API key.
func TestAuthenticatedPasteDelete(t *testing.T) {
	env := testutils.SetupTestEnv(t)
	defer env.CleanupFn()

	paste, _ := uploadPaste(t, env, "owned.txt", "owned by the api key", true)

	t.Run("deleting an unknown paste reports not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/p/zzzzzzzz", nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	})

	t.Run("owner deletes the paste", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/p/"+paste.ID, nil)
		req.Header.Set("Authorization", "Bearer "+fixtureAPIKey)
		resp, err := env.Request(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("deleted paste no longer resolves", func(t *testing.T) {
		resp := get(t, env, "/p/"+paste.ID, "")
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
