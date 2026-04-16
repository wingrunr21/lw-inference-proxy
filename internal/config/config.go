package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all proxy configuration, loaded from environment variables.
type Config struct {
	Port         string
	DrainTimeout time.Duration
	OTelEndpoint string
	OTelService  string
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PROXY_PORT", "8080"),
		DrainTimeout: getDuration("PROXY_DRAIN_TIMEOUT", 60*time.Second),
		OTelEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTelService:  getEnv("OTEL_SERVICE_NAME", "inference-proxy"),
	}
}

func (c *Config) Addr() string {
	return fmt.Sprintf(":%s", c.Port)
}

// OTelEnabled reports whether OpenTelemetry is configured.
// OTel is implicitly disabled when no endpoint is set.
func (c *Config) OTelEnabled() bool {
	return c.OTelEndpoint != ""
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
