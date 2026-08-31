package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultListenAddress = "127.0.0.1:8080"
	defaultClustersFile  = "./data/clusters.json"
	defaultDialTimeout   = 5 * time.Second
)

// Config contains the process configuration loaded from environment variables.
type Config struct {
	ListenAddress     string
	ClustersFile      string
	HistoryConfigFile string
	HistoryFile       string
	AuthFile          string
	DialTimeout       time.Duration
}

// Load reads configuration from the environment and applies safe local defaults.
func Load() (Config, error) {
	cfg := Config{
		ListenAddress: envOrDefault("LISTEN_ADDR", defaultListenAddress),
		ClustersFile:  envOrDefault("CLUSTERS_FILE", defaultClustersFile),
		DialTimeout:   defaultDialTimeout,
	}
	dataDirectory := filepath.Dir(cfg.ClustersFile)
	cfg.HistoryConfigFile = envOrDefault("HISTORY_CONFIG_FILE", filepath.Join(dataDirectory, "history-storage.json"))
	cfg.HistoryFile = envOrDefault("HISTORY_FILE", filepath.Join(dataDirectory, "history.jsonl"))
	cfg.AuthFile = envOrDefault("AUTH_FILE", filepath.Join(dataDirectory, "auth.json"))

	if raw := strings.TrimSpace(os.Getenv("ETCD_DIAL_TIMEOUT")); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Config{}, errors.New("ETCD_DIAL_TIMEOUT must be a positive duration, for example 5s")
		}
		cfg.DialTimeout = timeout
	}

	if strings.TrimSpace(cfg.ClustersFile) == "" {
		return Config{}, errors.New("CLUSTERS_FILE cannot be empty")
	}
	if strings.TrimSpace(cfg.HistoryFile) == "" {
		return Config{}, errors.New("HISTORY_FILE cannot be empty")
	}
	if strings.TrimSpace(cfg.HistoryConfigFile) == "" {
		return Config{}, errors.New("HISTORY_CONFIG_FILE cannot be empty")
	}
	if strings.TrimSpace(cfg.AuthFile) == "" {
		return Config{}, errors.New("AUTH_FILE cannot be empty")
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
