package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("PROXY_PORT", "")
	t.Setenv("PROXY_DRAIN_TIMEOUT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := config.Load()

	is := assert.New(t)
	is.Equal("8080", cfg.Port)
	is.Equal(60*time.Second, cfg.DrainTimeout)
	is.Equal("", cfg.OTelEndpoint)
	is.Equal("inference-proxy", cfg.OTelService)
}

func TestLoad_CustomPort(t *testing.T) {
	t.Setenv("PROXY_PORT", "9090")
	t.Setenv("PROXY_DRAIN_TIMEOUT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := config.Load()
	assert.Equal(t, "9090", cfg.Port)
}

func TestLoad_CustomDrainTimeout(t *testing.T) {
	t.Setenv("PROXY_DRAIN_TIMEOUT", "30s")
	t.Setenv("PROXY_PORT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := config.Load()
	assert.Equal(t, 30*time.Second, cfg.DrainTimeout)
}

func TestLoad_InvalidDrainTimeoutFallsBackToDefault(t *testing.T) {
	t.Setenv("PROXY_DRAIN_TIMEOUT", "not-a-duration")
	t.Setenv("PROXY_PORT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg := config.Load()
	assert.Equal(t, 60*time.Second, cfg.DrainTimeout)
}

func TestLoad_OTelVars(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel:4317")
	t.Setenv("OTEL_SERVICE_NAME", "my-proxy")
	t.Setenv("PROXY_PORT", "")
	t.Setenv("PROXY_DRAIN_TIMEOUT", "")

	cfg := config.Load()

	must := require.New(t)
	must.NotNil(cfg)

	is := assert.New(t)
	is.Equal("http://otel:4317", cfg.OTelEndpoint)
	is.Equal("my-proxy", cfg.OTelService)
}

func TestConfig_Addr(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{name: "default port", port: "8080", want: ":8080"},
		{name: "custom port", port: "9999", want: ":9999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Port: tt.port}
			assert.Equal(t, tt.want, cfg.Addr())
		})
	}
}

func TestConfig_OTelEnabled(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "enabled when endpoint set", endpoint: "http://otel:4317", want: true},
		{name: "disabled when endpoint empty", endpoint: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{OTelEndpoint: tt.endpoint}
			assert.Equal(t, tt.want, cfg.OTelEnabled())
		})
	}
}
