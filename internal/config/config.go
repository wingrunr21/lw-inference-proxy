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
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PROXY_PORT", "8080"),
		DrainTimeout: getDuration("PROXY_DRAIN_TIMEOUT", 60*time.Second),
	}
}

func (c *Config) Addr() string {
	return fmt.Sprintf(":%s", c.Port)
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
