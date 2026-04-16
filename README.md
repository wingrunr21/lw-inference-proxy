# lw-inference-proxy

A zero-config reverse proxy that turns a pile of [vllm](https://github.com/vllm-project/vllm) and
[sglang](https://github.com/sgl-project/sglang) containers into a single OpenAI/Anthropic-compatible
API endpoint. Add a container, it appears. Stop a container, it's gone. No restarts, no config files.

Designed to sit behind [Traefik](https://traefik.io) or similar reverse proxies. Dropping this proxy
into a running stack should take about 5 minutes.

## How it works

The proxy mounts the Docker socket and watches for containers labeled `inference.enable=true`. When one
becomes healthy, it queries that container's `/v1/models` endpoint to learn the model name, then starts
routing requests to it. The `model` field in each request body is the routing key — no manual mapping
required. Bring a container up, it's live. Bring it down, in-flight requests drain before it's removed.

`GET /v1/models` is served from an in-memory cache and reflects whatever models are currently healthy.
Everything else — chat completions, completions, embeddings, Anthropic messages — passes through
verbatim. The proxy touches only the `model` field (to route) and nothing else.

## Requirements

- Docker socket available within the proxy container
- Each inference container **must** define a Docker `HEALTHCHECK` — the proxy uses Docker's native
  health events to know when a backend is ready. Without a healthcheck, containers are ignored.

## Quick start

```yaml
services:
  inference-proxy:
    image: ghcr.io/wingrunr21/lw-inference-proxy:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    labels:
      traefik.enable: "true"
      traefik.http.routers.inference.rule: "PathPrefix(`/v1`)"
      traefik.http.services.inference.loadbalancer.server.port: "8080"

  vllm:
    image: vllm/vllm-openai:latest
    command: ["--model", "meta-llama/Llama-3.1-8B-Instruct"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 120s
    labels:
      inference.enable: "true"

  sglang:
    image: lmsysorg/sglang:latest
    command: ["python", "-m", "sglang.launch_server", "--model-path", "Qwen/Qwen2.5-7B-Instruct"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:30000/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 120s
    labels:
      inference.enable: "true"
      inference.port: "30000"
```

That's it. Both models show up in `GET /v1/models`. Requests to `/v1/chat/completions` are routed by
the `model` field in the body. No proxy config needed.

The proxy also handles containers that were already running when it starts — useful if you restart the
proxy without touching your inference containers.

## Labels

Labels go on the **inference containers**, not the proxy.

| Label | Default | Description |
|---|---|---|
| `inference.enable` | — | Set to `"true"` to opt a container in |
| `inference.port` | `8000` | Port the inference server listens on |
| `inference.api.base_path` | `/v1` | URL prefix for the inference server's API |

## Configuration

Configuration goes on the **proxy container** as environment variables.

| Variable | Default | Description |
|---|---|---|
| `PROXY_PORT` | `8080` | Port the proxy listens on |
| `PROXY_DRAIN_TIMEOUT` | `60s` | How long to wait for in-flight requests when a backend is removed |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Omit to disable telemetry entirely |
| `OTEL_SERVICE_NAME` | `inference-proxy` | Service name in traces and metrics |

### OpenTelemetry

Set `OTEL_EXPORTER_OTLP_ENDPOINT` and you get traces (one span per request with model name,
backend URL, status code, and whether it was a streaming response) plus metrics
(`proxy.requests.total`, `proxy.request.duration`, `proxy.backend.inflight`,
`proxy.backends.active`). Leave it unset and there's zero OTel overhead.

## Routing behavior

- **Model not found**: `404` with a JSON error body
- **Backend draining** (container stopping): `503`
- **Missing `model` field**: `400`
- **`container die`** (crash/OOM): removed immediately, no drain
- **`container stop`**: drained up to `PROXY_DRAIN_TIMEOUT`, then removed
- **Two containers with the same model name**: last one to become healthy wins, with a warning logged

## Building

```sh
make build   # produces bin/lw-inference-proxy
make docker  # builds the image
make test
```

Go 1.24+ required.

## Contributing

Issues and PRs welcome. The codebase is intentionally small — the core proxy logic is in
`internal/proxy/`, Docker event handling in `internal/docker/`, and the routing table in
`internal/router/`. If you're adding a feature, check the [spec](SPEC.md) first to understand
the design constraints.
