package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("PROXY_PORT", "")
	t.Setenv("PROXY_DRAIN_TIMEOUT", "")

	cfg := config.Load()

	is := assert.New(t)
	is.Equal("8080", cfg.Port)
	is.Equal(60*time.Second, cfg.DrainTimeout)
}

func TestLoad_CustomPort(t *testing.T) {
	t.Setenv("PROXY_PORT", "9090")
	t.Setenv("PROXY_DRAIN_TIMEOUT", "")

	cfg := config.Load()
	assert.Equal(t, "9090", cfg.Port)
}

func TestLoad_CustomDrainTimeout(t *testing.T) {
	t.Setenv("PROXY_DRAIN_TIMEOUT", "30s")
	t.Setenv("PROXY_PORT", "")

	cfg := config.Load()
	assert.Equal(t, 30*time.Second, cfg.DrainTimeout)
}

func TestLoad_InvalidDrainTimeoutFallsBackToDefault(t *testing.T) {
	t.Setenv("PROXY_DRAIN_TIMEOUT", "not-a-duration")
	t.Setenv("PROXY_PORT", "")

	cfg := config.Load()
	assert.Equal(t, 60*time.Second, cfg.DrainTimeout)
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
