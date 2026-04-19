package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

const (
	labelEnable   = "inference.enable"
	labelPort     = "inference.port"
	labelBasePath = "inference.api.base_path"

	defaultPort     = "8000"
	defaultBasePath = "/v1"

	fetchRetries   = 3
	fetchRetryWait = time.Second
)

// dockerClient is the subset of the Docker API used by Watcher.
// *dockerclient.Client satisfies this interface.
type dockerClient interface {
	Events(ctx context.Context, opts dockerclient.EventsListOptions) dockerclient.EventsResult
	ContainerList(ctx context.Context, opts dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	ContainerInspect(ctx context.Context, id string, opts dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error)
}

// Watcher subscribes to the Docker event stream and keeps the routing table
// in sync with inference containers.
type Watcher struct {
	cfg    *config.Config
	cli    dockerClient
	router *router.Router
	http   *http.Client
}

func NewWatcher(cfg *config.Config, r *router.Router) (*Watcher, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Watcher{
		cfg:    cfg,
		cli:    cli,
		router: r,
		http:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Run starts the event loop. Blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	f := make(dockerclient.Filters).
		Add("type", "container").
		Add("label", labelEnable+"=true")

	for {
		// Subscribe before scanning so no events are missed during the scan.
		evResult := w.cli.Events(ctx, dockerclient.EventsListOptions{Filters: f})

		w.scanExisting(ctx)

		reconnect := false
		for !reconnect {
			select {
			case <-ctx.Done():
				return
			case err := <-evResult.Err:
				if ctx.Err() != nil {
					return
				}
				slog.Error("docker event stream error, reconnecting in 5s", "error", err)
				reconnect = true
			case ev := <-evResult.Messages:
				w.handleEvent(ctx, ev)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *Watcher) scanExisting(ctx context.Context) {
	listResult, err := w.cli.ContainerList(ctx, dockerclient.ContainerListOptions{
		Filters: make(dockerclient.Filters).Add("label", labelEnable+"=true"),
	})
	if err != nil {
		slog.Error("failed to list existing containers", "error", err)
		return
	}

	for _, c := range listResult.Items {
		ir, err := w.cli.ContainerInspect(ctx, c.ID, dockerclient.ContainerInspectOptions{})
		if err != nil {
			slog.Error("failed to inspect container", "container", shortID(c.ID), "error", err)
			continue
		}
		inspect := ir.Container
		if inspect.State.Health == nil {
			slog.Warn("container has inference.enable=true but no HEALTHCHECK defined — skipping",
				"container", containerName(inspect))
			continue
		}
		if inspect.State.Health.Status == container.Healthy {
			w.register(ctx, inspect)
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, ev events.Message) {
	switch ev.Action {
	case events.ActionHealthStatus:
		// Docker API 1.45+: status is in actor attributes.
		w.handleHealthStatus(ctx, ev.Actor.ID, ev.Actor.Attributes["health_status"])

	case events.ActionHealthStatusHealthy:
		w.handleHealthStatus(ctx, ev.Actor.ID, "healthy")

	case events.ActionHealthStatusUnhealthy:
		w.handleHealthStatus(ctx, ev.Actor.ID, "unhealthy")

	case events.ActionStop, events.ActionKill:
		w.deregister(ev.Actor.ID, string(ev.Action))

	case events.ActionDie:
		// Process has exited — remove immediately without waiting for drain.
		w.deregisterImmediate(ev.Actor.ID)
	}
}

func (w *Watcher) handleHealthStatus(ctx context.Context, containerID, status string) {
	switch status {
	case "healthy":
		ir, err := w.cli.ContainerInspect(ctx, containerID, dockerclient.ContainerInspectOptions{})
		if err != nil {
			slog.Error("failed to inspect container on health event",
				"container", shortID(containerID), "error", err)
			return
		}
		w.register(ctx, ir.Container)
	case "unhealthy":
		w.deregister(containerID, "unhealthy")
	}
}

func (w *Watcher) register(ctx context.Context, inspect container.InspectResponse) {
	port := labelValue(inspect.Config.Labels, labelPort, defaultPort)
	basePath := labelValue(inspect.Config.Labels, labelBasePath, defaultBasePath)

	ip, err := containerIP(inspect)
	if err != nil {
		slog.Error("cannot determine container IP",
			"container", containerName(inspect), "error", err)
		return
	}

	backendURL := fmt.Sprintf("http://%s:%s", ip, port)
	modelID, modelObj, err := w.fetchModelWithRetry(ctx, backendURL, basePath)
	if err != nil {
		slog.Error("failed to fetch model from backend",
			"container", containerName(inspect), "url", backendURL, "error", err)
		return
	}

	entry := &router.BackendEntry{
		ContainerID: inspect.ID,
		BackendURL:  backendURL,
		BasePath:    basePath,
		ModelObject: modelObj,
	}

	prev := w.router.Add(modelID, entry)
	if prev != nil {
		slog.Warn("model name collision: new container wins",
			"model", modelID,
			"new_container", containerName(inspect),
			"displaced_container", shortID(prev.ContainerID),
		)
	}

	slog.Info("backend registered",
		"model", modelID,
		"container", containerName(inspect),
		"url", backendURL,
	)
}

func (w *Watcher) deregister(containerID, reason string) {
	entries := w.router.MarkDraining(containerID)
	if len(entries) == 0 {
		return
	}
	go func() {
		for _, e := range entries {
			w.drain(e)
		}
		removed := w.router.Remove(containerID)
		for _, e := range removed {
			slog.Info("backend deregistered",
				"container", shortID(containerID),
				"reason", reason,
				"url", e.BackendURL,
			)
		}
	}()
}

func (w *Watcher) deregisterImmediate(containerID string) {
	removed := w.router.Remove(containerID)
	for _, e := range removed {
		slog.Info("backend removed (died)",
			"container", shortID(containerID),
			"url", e.BackendURL,
		)
	}
}

func (w *Watcher) drain(e *router.BackendEntry) {
	deadline := time.Now().Add(w.cfg.DrainTimeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		if e.InFlight() == 0 {
			return
		}
	}
	slog.Warn("drain timeout reached, forcing removal",
		"container", shortID(e.ContainerID),
		"inflight", e.InFlight(),
	)
}

type modelsResponse struct {
	Data []json.RawMessage `json:"data"`
}

func (w *Watcher) fetchModelWithRetry(ctx context.Context, backendURL, basePath string) (string, json.RawMessage, error) {
	var lastErr error
	for i := range fetchRetries {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(fetchRetryWait):
			}
		}
		id, obj, err := w.fetchModel(ctx, backendURL, basePath)
		if err == nil {
			return id, obj, nil
		}
		lastErr = err
		slog.Debug("fetchModel attempt failed, retrying",
			"attempt", i+1, "url", backendURL, "error", err)
	}
	return "", nil, lastErr
}

func (w *Watcher) fetchModel(ctx context.Context, backendURL, basePath string) (string, json.RawMessage, error) {
	u := backendURL + basePath + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, u)
	}

	var models modelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		return "", nil, fmt.Errorf("parsing models response: %w", err)
	}
	if len(models.Data) == 0 {
		return "", nil, fmt.Errorf("backend returned empty models list")
	}

	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(models.Data[0], &m); err != nil {
		return "", nil, fmt.Errorf("parsing model id: %w", err)
	}
	if m.ID == "" {
		return "", nil, fmt.Errorf("model id is empty")
	}

	return m.ID, models.Data[0], nil
}

func containerIP(inspect container.InspectResponse) (string, error) {
	for _, ep := range inspect.NetworkSettings.Networks {
		if ep.IPAddress.IsValid() {
			return ep.IPAddress.String(), nil
		}
	}
	return "", fmt.Errorf("no network endpoint with IP found for container %s", shortID(inspect.ID))
}

func containerName(inspect container.InspectResponse) string {
	name := inspect.Name
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	return name
}

func labelValue(labels map[string]string, key, def string) string {
	if v, ok := labels[key]; ok && v != "" {
		return v
	}
	return def
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
