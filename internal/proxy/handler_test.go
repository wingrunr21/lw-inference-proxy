package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/proxy"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

func newTestHandler(r *router.Router) *proxy.Handler {
	cfg := &config.Config{Port: "8080"}
	return proxy.NewHandler(cfg, r)
}

func addBackend(t *testing.T, r *router.Router, modelID string, backend *httptest.Server) {
	t.Helper()
	r.Add(modelID, &router.BackendEntry{
		ContainerID: "ctr-" + modelID,
		BackendURL:  backend.URL,
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"` + modelID + `","object":"model"}`),
	})
}

// postJSON sends a POST to the handler with a JSON body and returns the recorder.
func postJSON(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_NonV1PathReturns404(t *testing.T) {
	t.Parallel()
	h := newTestHandler(router.New())

	tests := []string{"/", "/health", "/other/path"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}

func TestHandler_MissingModelFieldReturns400(t *testing.T) {
	t.Parallel()
	h := newTestHandler(router.New())

	rec := postJSON(h, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "model")
}

func TestHandler_UnknownModelReturns404(t *testing.T) {
	t.Parallel()
	h := newTestHandler(router.New()) // empty router

	rec := postJSON(h, "/v1/chat/completions", `{"model":"unknown-model","messages":[]}`)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandler_DrainingBackendReturns503(t *testing.T) {
	t.Parallel()
	r := router.New()
	entry := &router.BackendEntry{
		ContainerID: "ctr1",
		BackendURL:  "http://127.0.0.1:1", // unreachable — never reached
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"gpt-4"}`),
	}
	entry.SetDraining()
	r.Add("gpt-4", entry)

	h := newTestHandler(r)
	rec := postJSON(h, "/v1/chat/completions", `{"model":"gpt-4","messages":[]}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHandler_GetModels_AggregatesLiveBackends(t *testing.T) {
	t.Parallel()
	r := router.New()
	r.Add("gpt-4", &router.BackendEntry{
		ContainerID: "ctr1",
		BackendURL:  "http://10.0.0.1:8000",
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"gpt-4","object":"model"}`),
	})
	r.Add("claude-3", &router.BackendEntry{
		ContainerID: "ctr2",
		BackendURL:  "http://10.0.0.2:8000",
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"claude-3","object":"model"}`),
	})

	h := newTestHandler(r)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	must := require.New(t)
	must.Equal(http.StatusOK, rec.Code)

	var resp struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	must.NoError(json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "list", resp.Object)
	assert.Len(t, resp.Data, 2)
}

func TestHandler_GetModels_EmptyWhenNoBackends(t *testing.T) {
	t.Parallel()
	h := newTestHandler(router.New())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	must := require.New(t)
	must.Equal(http.StatusOK, rec.Code)

	var resp struct {
		Data []json.RawMessage `json:"data"`
	}
	must.NoError(json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Data)
}

func TestHandler_GetModels_ExcludesDrainingBackends(t *testing.T) {
	t.Parallel()
	r := router.New()
	live := &router.BackendEntry{
		ContainerID: "ctr1",
		BackendURL:  "http://10.0.0.1:8000",
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"live-model"}`),
	}
	draining := &router.BackendEntry{
		ContainerID: "ctr2",
		BackendURL:  "http://10.0.0.2:8000",
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"draining-model"}`),
	}
	draining.SetDraining()
	r.Add("live-model", live)
	r.Add("draining-model", draining)

	h := newTestHandler(r)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	must := require.New(t)
	must.Equal(http.StatusOK, rec.Code)

	var resp struct {
		Data []json.RawMessage `json:"data"`
	}
	must.NoError(json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Data, 1, "draining backend must be excluded from /v1/models")
}

func TestHandler_ProxiesRequestToBackend(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp1","object":"chat.completion"}`)) //nolint:errcheck
	}))
	defer backend.Close()

	r := router.New()
	addBackend(t, r, "gpt-4", backend)

	h := newTestHandler(r)
	payload := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	rec := postJSON(h, "/v1/chat/completions", payload)

	must := require.New(t)
	must.Equal(http.StatusOK, rec.Code)

	// The proxy should strip the /v1 prefix from the client path and prepend
	// the backend's base path, yielding /v1/chat/completions on the backend.
	assert.Equal(t, "/v1/chat/completions", gotPath)

	// Full original body must arrive at the backend.
	assert.JSONEq(t, payload, gotBody)
}

func TestHandler_PathRewrittenCorrectly(t *testing.T) {
	t.Parallel()

	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	r := router.New()
	// Use a non-default base path on the backend.
	r.Add("llama", &router.BackendEntry{
		ContainerID: "ctr1",
		BackendURL:  backend.URL,
		BasePath:    "/api/v1",
		ModelObject: json.RawMessage(`{"id":"llama"}`),
	})

	h := newTestHandler(r)
	rec := postJSON(h, "/v1/completions", `{"model":"llama","prompt":"hi"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/api/v1/completions", gotPath)
}
