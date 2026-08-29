# lw-inference-proxy — Specification

## Overview

`lw-inference-proxy` is a lightweight HTTP reverse proxy that aggregates multiple
[vllm](https://github.com/vllm-project/vllm) and [sglang](https://github.com/sgl-project/sglang)
inference servers running as Docker containers into a single OpenAI- and Anthropic-compatible
API surface. It is designed to sit behind reverse proxies, like [Traefik](https://traefik.io),
as a standard backend service, leveraging that proxy for TLS termination, certificates (eg ACME),
and external routing.

The proxy is intentionally minimal: it routes requests, aggregates model listings, and
gets out of the way. No load balancing, no governance, no authentication.

---

## Goals

- Unified `/v1/*` API surface across N inference backends
- Automatic routing based on the `model` field in request bodies
- Fully dynamic: add/remove backends by starting/stopping Docker containers
- Zero model configuration in compose files — model names discovered from backends
- High throughput, low overhead — streaming responses pass through without buffering

## Non-Goals

- Load balancing across multiple instances of the same model
- Authentication / authorization
- Request logging
- Metrics or tracing of its own (backends export their own; the proxy would only echo them)
- Managing container lifecycle (start/stop)
- TLS termination (delegated to elsewhere)
- ACME (such as LetsEncrypt) (delegated to elsewhere)
- API translation between OpenAI and Anthropic formats (backends handle natively)

---

## Architecture

```
                        ┌─────────────────────────────────────────┐
                        │             Docker Host                  │
                        │                                          │
Internet ──► Traefik ──►│  lw-inference-proxy                      │
          (TLS, routing)│      │   ▲                              │
                        │      │   │ Docker Events                │
                        │      │   │ (health_status, stop, die)   │
                        │      │   └──── Docker Daemon            │
                        │      │          (socket)                │
                        │      │                                   │
                        │      ├──► vllm  (model: llama-3.1-70b)  │
                        │      ├──► vllm  (model: qwen2.5-72b)    │
                        │      └──► sglang (model: deepseek-r1)   │
                        │                                          │
                        └─────────────────────────────────────────┘
```

The proxy mounts the Docker socket read-only. Traefik routes all `/v1/*` traffic to
the proxy via standard Docker label-based service discovery (unchanged from any other
Traefik-managed service).

---

## Docker Integration

### Inference Container Labels

Containers are opted into the proxy by setting the following Docker labels:

| Label | Required | Default | Description |
|---|---|---|---|
| `inference.enable` | Yes | — | Must be `"true"` to be managed by the proxy |
| `inference.port` | No | `8000` | Port the inference server listens on inside the container |
| `inference.api.base_path` | No | `/v1` | URL prefix for the inference server's API |

Example:

```yaml
labels:
  inference.enable: "true"
  inference.port: "8000"
  inference.api.base_path: "/v1"  # optional, shown for clarity
```

No model names are specified. The proxy queries the backend's `/v1/models` endpoint (or
the configured base path equivalent) to discover all model names after the container
becomes healthy. Every model in the response `data` array is registered.

### Health Check Requirement

Inference containers **must** define a Docker `HEALTHCHECK`. The proxy relies on Docker's
native `health_status` events to know when a backend is ready to serve traffic. The
proxy will not register a container until Docker reports it as `healthy`.

Recommended healthcheck for vllm:

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost:8000/health"]
  interval: 10s
  timeout: 5s
  retries: 3
  start_period: 120s
```

`start_period` should be set generously — model weight loading can take 1–5 minutes
depending on model size and storage speed.

### Docker Events Handled

The proxy subscribes to the Docker event stream, filtered to containers with
`inference.enable=true`:

| Event | Action |
|---|---|
| `health_status: healthy` | Query `{base_path}/models`, register model in routing table |
| `health_status: unhealthy` | Mark backend draining, drain in-flight requests, remove from routing table |
| `container stop` | Mark backend draining, drain in-flight requests, remove from routing table |
| `container die` | Immediately remove from routing table (no drain) |

### Proxy Startup

On startup, the proxy scans all currently running containers with `inference.enable=true`:

- Status `healthy` → register immediately (query `/models`, populate routing table)
- Status `starting` → wait for the `health_status: healthy` event
- Status `unhealthy` / not running → ignore

This ensures the routing table is fully populated when the proxy restarts without
requiring any inference container restarts.

---

## Routing Table

In-memory map, keyed by model name:

```
model_id (string) → BackendEntry {
    ContainerID  string
    BackendURL   string          // e.g. http://172.17.0.3:8000
    ModelObject  json.RawMessage // full model object from /v1/models response
    InFlight     atomic.Int64    // count of in-flight requests
    Status       enum { live, draining }
}
```

**Model name collision:** If a newly registered model name is already present in the
routing table, the new entry wins (last-in). The displaced entry's in-flight requests
complete normally. A warning is logged.

**Model name source:** The `id` field from each entry in the `data` array of the
backend's `/v1/models` response is the canonical routing key. All models exposed by a
container are registered. This respects operator overrides such as vllm's
`--served-model-name`.

---

## Request Routing

### Supported Endpoints

| Method | Path | Behavior |
|---|---|---|
| `GET` | `{base_path}/models` | Served from routing table cache (aggregated) |
| `POST` | `{base_path}/chat/completions` | Route by `model` field |
| `POST` | `{base_path}/completions` | Route by `model` field |
| `POST` | `{base_path}/embeddings` | Route by `model` field |
| `POST` | `{base_path}/messages` | Route by `model` field (Anthropic format) |
| `*` | `{base_path}/*` | Catch-all: route by `model` field in body |

All routing uses the same base path configured on the proxy (default `/v1`), not
the per-backend base path. The per-backend base path is used only when constructing
the upstream URL.

### Model Field Extraction

The proxy uses a streaming JSON tokenizer to extract the top-level `"model"` field
from the request body with minimal reads. Both OpenAI and Anthropic API formats place
`model` at the top level — routing logic is identical for both.

```
req.Body → streaming JSON tokenizer (reads until "model" found, ~50–200 bytes)
                        ↓
           io.MultiReader(buffered_prefix, remaining req.Body)
                        ↓
           backend request body — fully streamed, no copy of remainder
```

Only the small byte prefix consumed by the tokenizer is held in memory. The remainder
of the body — including large prompts or multimodal payloads — is piped directly to
the backend without buffering. `httputil.ReverseProxy` streams the reconstructed body
to the upstream without an additional copy.

This is strictly more efficient than a full-buffer approach.

If the `model` field is absent: `400 Bad Request`.
If the `model` is not in the routing table: `404 Not Found` with a JSON error body.
If the backend is draining: `503 Service Unavailable`.

### Pass-Through Behavior

Once routed, the request is forwarded verbatim to the backend:

- All headers forwarded as-is (including `Authorization`, `Content-Type`, etc.)
- Body forwarded verbatim — no transformation
- Streaming responses (`text/event-stream`) are proxied without buffering
- The proxy adds standard `X-Forwarded-For` / `X-Forwarded-Host` headers

No API translation is performed. vllm and sglang handle OpenAI and Anthropic
request formats natively.

### GET /v1/models

Served directly from the routing table. The response is constructed by collecting
the cached `ModelObject` for each live backend entry and returning them as the
`data` array in a standard OpenAI models list response. No fan-out to backends.

---

## Drain Behavior

When a container receives a stop signal or becomes unhealthy:

1. Backend status set to `draining` — no new requests routed to it
2. Proxy waits for `InFlight` counter to reach zero
3. If `PROXY_DRAIN_TIMEOUT` elapses before counter reaches zero, remaining
   in-flight requests are abandoned (connections closed)
4. Backend entry removed from routing table

`container die` events skip drain and remove immediately (the process is already gone).

---

## Configuration

All configuration is via environment variables on the proxy container. There are no
configuration files.

| Variable | Default | Description |
|---|---|---|
| `PROXY_PORT` | `8080` | Port the proxy listens on |
| `PROXY_DRAIN_TIMEOUT` | `60s` | Max time to wait for in-flight requests on backend removal |
---

## Implementation Stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | Go | Docker SDK, `httputil.ReverseProxy` |
| HTTP server | `net/http` | Standard, streaming-correct, sufficient performance |
| HTTP proxy | `httputil.ReverseProxy` | Streams request body and response without buffering; handles SSE, hop-by-hop headers correctly |
| Body peek | `encoding/json.Decoder` + `io.MultiReader` | Streaming model field extraction; only prefix bytes held in memory |
| Docker integration | `github.com/docker/docker/client` | Official SDK, event stream support |
| Routing table | `sync.RWMutex` + `map` | Simple, correct; no lock-free complexity needed |
| In-flight tracking | `atomic.Int64` per backend | Zero-contention counter |

---

## Full docker-compose Example

```yaml
services:
  inference-proxy:
    image: lw-inference-proxy:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      PROXY_PORT: "8080"
      PROXY_DRAIN_TIMEOUT: "60s"
    labels:
      traefik.enable: "true"
      traefik.http.routers.inference.rule: "PathPrefix(`/v1`)"
      traefik.http.services.inference.loadbalancer.server.port: "8080"

  vllm-llama:
    image: vllm/vllm-openai:latest
    command: ["--model", "meta-llama/Llama-3.1-70B-Instruct"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 120s
    labels:
      inference.enable: "true"
      inference.port: "8000"

  sglang-deepseek:
    image: lmsysorg/sglang:latest
    command: ["python", "-m", "sglang.launch_server",
              "--model-path", "deepseek-ai/DeepSeek-R1"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:30000/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 180s
    labels:
      inference.enable: "true"
      inference.port: "30000"
```
