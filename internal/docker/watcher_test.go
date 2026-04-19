package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"net/netip"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockernet "github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ---------------------------------------------------------------------------
// Fake Docker client
// ---------------------------------------------------------------------------

type fakeDockerClient struct {
	containers []container.Summary
	inspects   map[string]container.InspectResponse
	inspectErr map[string]error

	eventCh chan events.Message
	errCh   chan error
}

func newFakeClient() *fakeDockerClient {
	return &fakeDockerClient{
		inspects:   make(map[string]container.InspectResponse),
		inspectErr: make(map[string]error),
		eventCh:    make(chan events.Message, 16),
		errCh:      make(chan error, 1),
	}
}

func (f *fakeDockerClient) Events(_ context.Context, _ dockerclient.EventsListOptions) dockerclient.EventsResult {
	return dockerclient.EventsResult{Messages: f.eventCh, Err: f.errCh}
}

func (f *fakeDockerClient) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	return dockerclient.ContainerListResult{Items: f.containers}, nil
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, id string, _ dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	if err, ok := f.inspectErr[id]; ok {
		return dockerclient.ContainerInspectResult{}, err
	}
	if resp, ok := f.inspects[id]; ok {
		return dockerclient.ContainerInspectResult{Container: resp}, nil
	}
	return dockerclient.ContainerInspectResult{}, fmt.Errorf("container not found: %s", id)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestWatcher creates a Watcher wired to the given fake client and router.
// DrainTimeout is set short enough that drain goroutines finish quickly in tests.
func newTestWatcher(cli dockerClient, r *router.Router) *Watcher {
	return &Watcher{
		cfg: &config.Config{
			DrainTimeout: 500 * time.Millisecond,
		},
		cli:    cli,
		router: r,
		http:   &http.Client{Timeout: 5 * time.Second},
	}
}

// makeBackend starts an httptest server that serves a single model entry on
// GET <basePath>/models. Returns the server, its IP, and its port.
func makeBackend(t *testing.T, modelID string) (srv *httptest.Server, ip, port string) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"data": []map[string]string{{"id": modelID}},
		})
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	h, p, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	return srv, h, p
}

// makeErrorBackend starts an httptest server that returns statusCode on first N
// calls and then delegates to okHandler.
func makeRetryBackend(t *testing.T, failCount int32, modelID string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= failCount {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"data": []map[string]string{{"id": modelID}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// makeInspect builds a container.InspectResponse suitable for watcher tests.
func makeInspect(id, name, ip, port string, healthStatus container.HealthStatus) container.InspectResponse {
	return container.InspectResponse{
		ID:   id,
		Name: "/" + name,
		State: &container.State{
			Health: &container.Health{
				Status: healthStatus,
			},
		},
		Config: &container.Config{
			Labels: map[string]string{
				labelEnable: "true",
				labelPort:   port,
			},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*dockernet.EndpointSettings{
				"bridge": {IPAddress: netip.MustParseAddr(ip)},
			},
		},
	}
}

// makeInspectNoHealthcheck builds an inspect response without a Health field.
func makeInspectNoHealthcheck(id, name string) container.InspectResponse {
	return container.InspectResponse{
		ID:   id,
		Name: "/" + name,
		State: &container.State{
			Health: nil,
		},
		Config: &container.Config{
			Labels: map[string]string{labelEnable: "true"},
		},
		NetworkSettings: &container.NetworkSettings{},
	}
}

// waitForRegistration polls until the router has an entry for modelID or the
// context deadline is exceeded.
func waitForRegistration(t *testing.T, r *router.Router, modelID string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return r.Get(modelID) != nil
	}, 3*time.Second, 10*time.Millisecond, "backend for %q was not registered in time", modelID)
}

// waitForRemoval polls until the router has no entries for containerID or the
// context deadline is exceeded.
func waitForRemoval(t *testing.T, r *router.Router, modelID string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return r.Get(modelID) == nil
	}, 3*time.Second, 10*time.Millisecond, "backend for %q was not removed in time", modelID)
}

// ---------------------------------------------------------------------------
// scanExisting tests
// ---------------------------------------------------------------------------

func TestScanExisting_RegistersHealthyContainer(t *testing.T) {
	const (
		cid     = "abc123"
		modelID = "llama-3"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.containers = []container.Summary{{ID: cid}}
	fake.inspects[cid] = makeInspect(cid, "llama-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.scanExisting(context.Background())

	entry := r.Get(modelID)
	require.NotNil(t, entry, "healthy container must be registered")
	assert.Equal(t, cid, entry.ContainerID)
}

func TestScanExisting_SkipsContainerWithoutHealthcheck(t *testing.T) {
	const cid = "abc123"

	fake := newFakeClient()
	fake.containers = []container.Summary{{ID: cid}}
	fake.inspects[cid] = makeInspectNoHealthcheck(cid, "no-health")

	r := router.New()
	w := newTestWatcher(fake, r)
	w.scanExisting(context.Background())

	assert.Equal(t, 0, r.Count(), "container without HEALTHCHECK must be skipped")
}

func TestScanExisting_SkipsUnhealthyContainer(t *testing.T) {
	const cid = "abc123"

	fake := newFakeClient()
	fake.containers = []container.Summary{{ID: cid}}
	fake.inspects[cid] = makeInspect(cid, "sick-server", "127.0.0.1", "8000", container.Unhealthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.scanExisting(context.Background())

	assert.Equal(t, 0, r.Count(), "unhealthy container must not be registered")
}

func TestScanExisting_MultipleContainers(t *testing.T) {
	_, ip1, port1 := makeBackend(t, "model-a")
	_, ip2, port2 := makeBackend(t, "model-b")

	fake := newFakeClient()
	fake.containers = []container.Summary{
		{ID: "ctr1"},
		{ID: "ctr2"},
		{ID: "ctr3"},
	}
	fake.inspects["ctr1"] = makeInspect("ctr1", "server-a", ip1, port1, container.Healthy)
	fake.inspects["ctr2"] = makeInspect("ctr2", "server-b", ip2, port2, container.Healthy)
	fake.inspects["ctr3"] = makeInspect("ctr3", "server-c", "127.0.0.1", "9999", container.Starting)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.scanExisting(context.Background())

	is := assert.New(t)
	is.Equal(2, r.Count(), "only healthy containers should be registered")
	is.NotNil(r.Get("model-a"))
	is.NotNil(r.Get("model-b"))
}

// ---------------------------------------------------------------------------
// handleEvent tests
// ---------------------------------------------------------------------------

func TestHandleEvent_HealthyRegistersBackend(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "gpt-4"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "gpt-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor: events.Actor{
			ID:         cid,
			Attributes: map[string]string{"health_status": "healthy"},
		},
	})

	entry := r.Get(modelID)
	require.NotNil(t, entry)
	assert.Equal(t, cid, entry.ContainerID)
}

func TestHandleEvent_HealthyRegistersBackend_OldActionFormat(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "gpt-4"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "gpt-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatusHealthy,
		Actor:  events.Actor{ID: cid},
	})

	entry := r.Get(modelID)
	require.NotNil(t, entry)
	assert.Equal(t, cid, entry.ContainerID)
}

func TestHandleEvent_UnhealthyDeregistersBackend_OldActionFormat(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "gpt-4"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "gpt-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatusHealthy,
		Actor:  events.Actor{ID: cid},
	})
	require.NotNil(t, r.Get(modelID))

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatusUnhealthy,
		Actor:  events.Actor{ID: cid},
	})
	waitForRemoval(t, r, modelID)
}

func TestHandleEvent_UnhealthyDeregistersBackend(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "gpt-4"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "gpt-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	// Register first.
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: cid, Attributes: map[string]string{"health_status": "healthy"}},
	})
	require.NotNil(t, r.Get(modelID))

	// Deregister via unhealthy event (spawns drain goroutine).
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: cid, Attributes: map[string]string{"health_status": "unhealthy"}},
	})

	waitForRemoval(t, r, modelID)
}

func TestHandleEvent_StopDeregistersBackend(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "llama-3"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "llama-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: cid, Attributes: map[string]string{"health_status": "healthy"}},
	})
	require.NotNil(t, r.Get(modelID))

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionStop,
		Actor:  events.Actor{ID: cid},
	})

	waitForRemoval(t, r, modelID)
}

func TestHandleEvent_DieRemovesImmediately(t *testing.T) {
	const (
		cid     = "ctr1"
		modelID = "llama-3"
	)
	_, ip, port := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects[cid] = makeInspect(cid, "llama-server", ip, port, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: cid, Attributes: map[string]string{"health_status": "healthy"}},
	})
	require.NotNil(t, r.Get(modelID))

	// "die" removes immediately without draining.
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionDie,
		Actor:  events.Actor{ID: cid},
	})

	assert.Nil(t, r.Get(modelID), "die event must remove entry immediately")
}

func TestHandleEvent_ModelCollisionNewContainerWins(t *testing.T) {
	const modelID = "shared-model"
	_, ip1, port1 := makeBackend(t, modelID)
	_, ip2, port2 := makeBackend(t, modelID)

	fake := newFakeClient()
	fake.inspects["ctr1"] = makeInspect("ctr1", "server-1", ip1, port1, container.Healthy)
	fake.inspects["ctr2"] = makeInspect("ctr2", "server-2", ip2, port2, container.Healthy)

	r := router.New()
	w := newTestWatcher(fake, r)

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: "ctr1", Attributes: map[string]string{"health_status": "healthy"}},
	})
	require.NotNil(t, r.Get(modelID))

	// Second container with same model name — new one wins.
	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionHealthStatus,
		Actor:  events.Actor{ID: "ctr2", Attributes: map[string]string{"health_status": "healthy"}},
	})

	entry := r.Get(modelID)
	require.NotNil(t, entry)
	assert.Equal(t, "ctr2", entry.ContainerID, "second container must displace first")
}

// ---------------------------------------------------------------------------
// fetchModel retry tests
// ---------------------------------------------------------------------------

func TestFetchModel_RetriesOnServerError(t *testing.T) {
	const modelID = "retry-model"
	// Fail twice, succeed on the third attempt (fetchRetries == 3).
	srv := makeRetryBackend(t, 2, modelID)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)

	fake := newFakeClient()
	r := router.New()
	w := newTestWatcher(fake, r)

	id, obj, err := w.fetchModelWithRetry(context.Background(), "http://"+host+":"+port, "/")

	require.NoError(t, err)
	assert.Equal(t, modelID, id)
	assert.NotEmpty(t, obj)
}

func TestFetchModel_FailsAfterAllRetries(t *testing.T) {
	// Always return 500 — exhaust all retries.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)

	fake := newFakeClient()
	r := router.New()
	w := newTestWatcher(fake, r)

	_, _, err := w.fetchModelWithRetry(context.Background(), "http://"+host+":"+port, "/")

	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// containerIP / containerName helpers
// ---------------------------------------------------------------------------

func TestContainerIP_ReturnsFirstNonEmptyIP(t *testing.T) {
	inspect := container.InspectResponse{
		ID: "abc",
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*dockernet.EndpointSettings{
				"bridge": {IPAddress: netip.MustParseAddr("10.0.0.5")},
			},
		},
	}
	ip, err := containerIP(inspect)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.5", ip)
}

func TestContainerIP_ErrorWhenNoNetworks(t *testing.T) {
	inspect := container.InspectResponse{
		ID:              "abc",
		NetworkSettings: &container.NetworkSettings{},
	}
	_, err := containerIP(inspect)
	assert.Error(t, err)
}

func TestContainerName_StripLeadingSlash(t *testing.T) {
	inspect := container.InspectResponse{Name: "/my-container"}
	assert.Equal(t, "my-container", containerName(inspect))
}

func TestContainerName_NoSlash(t *testing.T) {
	inspect := container.InspectResponse{Name: "my-container"}
	assert.Equal(t, "my-container", containerName(inspect))
}
