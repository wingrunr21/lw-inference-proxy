package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

const proxyBasePath = "/v1"

// Handler is the root HTTP handler for the proxy.
type Handler struct {
	cfg       *config.Config
	router    *router.Router
	transport http.RoundTripper
	tracer    oteltrace.Tracer

	requestCounter  metric.Int64Counter
	requestDuration metric.Float64Histogram
}

func NewHandler(cfg *config.Config, r *router.Router) *Handler {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	h := &Handler{
		cfg:       cfg,
		router:    r,
		transport: transport,
		tracer:    otel.Tracer("inference-proxy"),
	}
	h.initMetrics()
	return h
}

func (h *Handler) initMetrics() {
	meter := otel.Meter("inference-proxy")

	h.requestCounter, _ = meter.Int64Counter("proxy.requests.total",
		metric.WithDescription("Total proxied requests, labeled by model and status_code"),
	)
	h.requestDuration, _ = meter.Float64Histogram("proxy.request.duration",
		metric.WithDescription("End-to-end request latency"),
		metric.WithUnit("s"),
	)
	meter.Int64ObservableGauge("proxy.backends.active", //nolint:errcheck
		metric.WithDescription("Number of backends in the routing table"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(h.router.Count()))
			return nil
		}),
	)
	meter.Int64ObservableGauge("proxy.backend.inflight", //nolint:errcheck
		metric.WithDescription("In-flight requests per backend"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for model, entry := range h.router.Snapshot() {
				o.Observe(entry.InFlight(), metric.WithAttributes(attribute.String("model", model)))
			}
			return nil
		}),
	)
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
	_, span := h.tracer.Start(r.Context(), "models.list")
	defer span.End()

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
	start := time.Now()

	ctx, span := h.tracer.Start(r.Context(), "proxy.route",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	modelID, restoredBody, err := extractModel(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.AddEvent("routing.error", oteltrace.WithAttributes(attribute.String("reason", err.Error())))
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = restoredBody
	span.SetAttributes(attribute.String("model", modelID))

	entry := h.router.Get(modelID)
	if entry == nil {
		span.SetStatus(codes.Error, "model not found")
		span.AddEvent("routing.miss", oteltrace.WithAttributes(attribute.String("model", modelID)))
		writeError(w, http.StatusNotFound, "model not found: "+modelID)
		h.recordRequest(ctx, modelID, http.StatusNotFound, time.Since(start))
		return
	}

	if !entry.TryAcquire() {
		span.SetStatus(codes.Error, "backend draining")
		span.AddEvent("routing.draining", oteltrace.WithAttributes(attribute.String("model", modelID)))
		writeError(w, http.StatusServiceUnavailable, "backend unavailable: "+modelID)
		h.recordRequest(ctx, modelID, http.StatusServiceUnavailable, time.Since(start))
		return
	}
	defer entry.DecrInFlight()

	span.SetAttributes(attribute.String("backend.url", entry.BackendURL))

	target, err := url.Parse(entry.BackendURL)
	if err != nil {
		span.SetStatus(codes.Error, "invalid backend url")
		writeError(w, http.StatusInternalServerError, "internal error")
		h.recordRequest(ctx, modelID, http.StatusInternalServerError, time.Since(start))
		return
	}

	// Wrap the ResponseWriter to capture the status code for metrics.
	rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	rp := &httputil.ReverseProxy{
		Director:  h.director(target, entry.BasePath),
		Transport: h.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			span.SetStatus(codes.Error, err.Error())
			writeError(w, http.StatusBadGateway, "backend error")
		},
	}
	rp.ServeHTTP(rw, r.WithContext(ctx))

	streaming := strings.Contains(rw.Header().Get("Content-Type"), "text/event-stream")
	span.SetAttributes(
		attribute.Int("http.status_code", rw.statusCode),
		attribute.Bool("streaming", streaming),
	)
	h.recordRequest(ctx, modelID, rw.statusCode, time.Since(start))
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

func (h *Handler) recordRequest(ctx context.Context, model string, status int, dur time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String("model", model),
		attribute.Int("status_code", status),
	)
	h.requestCounter.Add(ctx, 1, attrs)
	h.requestDuration.Record(ctx, dur.Seconds(), attrs)
}

// responseWriter wraps http.ResponseWriter to capture the written status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
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
