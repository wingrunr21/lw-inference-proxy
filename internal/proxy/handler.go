package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

const proxyBasePath = "/v1"

// Handler is the root HTTP handler for the proxy.
type Handler struct {
	cfg       *config.Config
	router    *router.Router
	transport http.RoundTripper
}

func NewHandler(cfg *config.Config, r *router.Router) *Handler {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Handler{
		cfg:       cfg,
		router:    r,
		transport: transport,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, proxyBasePath) {
		http.NotFound(w, r)
		return
	}

	relPath := r.URL.Path[len(proxyBasePath):]

	if r.Method == http.MethodGet && relPath == "/models" {
		h.serveModels(w, r)
		return
	}

	h.routeRequest(w, r)
}

func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	type modelsResponse struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}

	data := h.router.Models()
	if data == nil {
		data = []json.RawMessage{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modelsResponse{Object: "list", Data: data}) //nolint:errcheck
}

func (h *Handler) routeRequest(w http.ResponseWriter, r *http.Request) {
	modelID, restoredBody, err := extractModel(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = restoredBody

	entry := h.router.Get(modelID)
	if entry == nil {
		writeError(w, http.StatusNotFound, "model not found: "+modelID)
		return
	}

	if !entry.TryAcquire() {
		writeError(w, http.StatusServiceUnavailable, "backend unavailable: "+modelID)
		return
	}
	defer entry.DecrInFlight()

	target, err := url.Parse(entry.BackendURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	rp := &httputil.ReverseProxy{
		Director:  h.director(target, entry.BasePath),
		Transport: h.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "backend error")
		},
	}
	rp.ServeHTTP(w, r)
}

func (h *Handler) director(target *url.URL, backendBasePath string) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host

		// Rewrite path: strip proxy base path, prepend backend base path.
		relPath := req.URL.Path[len(proxyBasePath):]
		req.URL.Path = backendBasePath + relPath
		if req.URL.RawPath != "" {
			rawRel := req.URL.RawPath[len(proxyBasePath):]
			req.URL.RawPath = backendBasePath + rawRel
		}

		if clientIP := req.RemoteAddr; clientIP != "" {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				clientIP = prior + ", " + clientIP
			}
			req.Header.Set("X-Forwarded-For", clientIP)
		}
	}
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorResponse{ //nolint:errcheck
		Error: errorDetail{Message: msg, Type: "proxy_error"},
	})
}
