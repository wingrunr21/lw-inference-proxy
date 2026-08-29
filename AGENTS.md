# Agent Instructions

## What this project is

`lw-inference-proxy` is a small Go reverse proxy that aggregates OpenAI/Anthropic-compatible
inference containers (vllm, sglang, llama.cpp) behind one `/v1` API surface. It discovers backends
from Docker events, routes by the `model` field in the request body, and gets out of the way.

[SPEC.md](SPEC.md) is the design contract. Read it before changing behavior, and update it in the
same change when behavior does change. [README.md](README.md) is the user-facing documentation —
labels, env vars, and routing semantics documented there must stay in sync with the code.

## Layout

| Path | Contents |
|---|---|
| `cmd/proxy/` | `main.go` — wiring, signal handling, `-healthcheck` mode |
| `internal/config/` | Env-var configuration (`PROXY_*`) |
| `internal/docker/` | Docker event stream watcher, backend discovery, drain orchestration |
| `internal/proxy/` | HTTP handler, `model` field extraction, `httputil.ReverseProxy` setup |
| `internal/router/` | Thread-safe routing table (model ID → backend) |

The codebase is deliberately small (~2k lines including tests). Keep it that way.

## Commands

```sh
make build            # bin/lw-inference-proxy
make test             # go test ./...
make lint             # golangci-lint run (no config file; stock ruleset)
make docker           # single-arch image
make docker-multiarch # linux/amd64 + linux/arm64, as CI builds it
go vet ./...          # CI runs this on every PR
```

Run `gofmt -l .` before finishing; CI does not enforce it but the code is expected to be formatted.

## Design constraints

These are non-goals from the spec. Do not add them, and push back if asked to add them casually:

- No load balancing across replicas of the same model — last healthy registration wins
- No authentication, authorization, or request logging
- No metrics or tracing of its own (OpenTelemetry was deliberately removed in `70abf86`)
- No container lifecycle management, TLS termination, or ACME — those belong to Traefik
- No API translation between OpenAI and Anthropic formats — backends handle both natively

Two properties are load-bearing and easy to break:

1. **Bodies stream.** `extractModel` reads only the prefix needed to find the top-level `model`
   field, then reassembles the body with `io.MultiReader`. Never buffer a full request or response
   body — large prompts and SSE streams depend on this.
2. **Drain is correct.** `TryAcquire` re-checks the draining flag after incrementing the in-flight
   counter to close a TOCTOU window. Preserve that pattern when touching `internal/router`.

## Go conventions

- Go 1.26 (`go.mod` is the version source of truth; CI reads it via `go-version-file`)
- Standard library first. The only runtime dependency is the Moby client — justify any addition
- Structured logging via `log/slog` with the JSON handler; no `fmt.Println`, no `log` package
- Wrap errors with `%w` and a short context prefix (`fmt.Errorf("docker client: %w", err)`)
- Define interfaces at the consumer, narrow — see `dockerClient` in `internal/docker/watcher.go`,
  which exists so the watcher can be tested without a daemon
- Doc comments on exported identifiers and on any non-obvious concurrency invariant

Go-specific skills live in `.agents/skills/` and `.claude/skills/` (naming, error handling, testing,
concurrency, safety, and more). Consult the relevant one before making substantial changes.

## Testing

- External test packages (`package router_test`) — test through the public API
- `stretchr/testify`: `require` for preconditions that must halt the test, `assert` for the rest
- `t.Parallel()` on every test that can take it
- `go.uber.org/goleak` via `VerifyTestMain` in `internal/docker` — goroutine leaks fail the suite
- Fake the Docker client through the `dockerClient` interface; tests must not need a live daemon

## Docker image

- Two stages: `golang:1.26-alpine` builds a static binary (`CGO_ENABLED=0`), and the final stage is
  `scratch` with only the binary and `ca-certificates.crt` copied from the builder. Keep it that way
  — a distroless base was evaluated and rejected as not worth the extra layer over `scratch`.
- Because the final stage is `scratch`, anything the binary needs at runtime must be copied in
  explicitly. Adding a dependency that requires cgo, a shell, or `/etc/passwd` is a design change.
- Runs as root on purpose. The proxy reads the mounted Docker socket (`root:docker`, `0660`);
  running as a non-root uid would require every deployment to add `group_add`.
- There is no shell in the image. `HEALTHCHECK` must stay in exec form and go through the binary's
  own `-healthcheck` flag. `docker exec` debugging is not available — use logs.
- The build is multi-arch (`linux/amd64`, `linux/arm64`). Anything added to the final stage must
  exist for both.
- Verify image changes by actually running the container: `docker run` it with the socket mounted,
  hit `/healthz`, and confirm `docker inspect` reports `healthy`.

## CI

Four workflows in `.github/workflows/`: `pr.yml` (vet, test, govulncheck, multi-arch build),
`edge.yml` (nightly build/push to ghcr + Trivy), `release.yml` (tagged releases, cross-compiled
binaries, cosign signing), `cleanup.yml` (prunes old nightly tags).

- **Pin every action to a full commit SHA** with a trailing `# vN` comment. Never use a bare tag.
- Trivy fails the build on `HIGH,CRITICAL`. A base image bump is the usual fix.
- Release images are signed with keyless cosign; `id-token: write` is required for that job.

## GitHub API operations

All GitHub API operations — looking up action SHAs, querying repository info, listing releases,
inspecting tags, working with PRs and issues — must follow this preference order:

1. **`gh` CLI** (`gh api`, `gh repo`, `gh release`, etc.) — use this first. It is authenticated,
   handles pagination and rate limiting correctly, and is the preferred tool for all GitHub work.
2. **WebFetch against `api.github.com`** — fall back to this if `gh` is unavailable or the specific
   operation fails.
3. **`curl`** — last resort only, when neither `gh` nor WebFetch is available.

```bash
# Resolve an action tag to a commit SHA
gh api repos/actions/checkout/git/ref/tags/v4

# Dereference an annotated tag to its commit SHA
gh api repos/actions/checkout/git/tags/<sha>

# List releases
gh release list

# Query repo metadata
gh repo view owner/repo --json name,defaultBranchRef
```

## Commits

Short, single-line, imperative subjects ("Remove OpenTelemetry support", "Log version on startup").
No AI attribution trailers. Prefer small focused commits over one batched change.
