package config

import (
	"testing"
	"time"
)

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR", "CLUSTERS_FILE", "HISTORY_CONFIG_FILE", "HISTORY_FILE", "AUTH_FILE", "ETCD_DIAL_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:8080" {
		t.Fatalf("listen address = %q", cfg.ListenAddress)
	}
	if cfg.ClustersFile != "./data/clusters.json" {
		t.Fatalf("clusters file = %q", cfg.ClustersFile)
	}
	if cfg.HistoryFile != "data/history.jsonl" {
		t.Fatalf("history file = %q", cfg.HistoryFile)
	}
	if cfg.HistoryConfigFile != "data/history-storage.json" {
		t.Fatalf("history config file = %q", cfg.HistoryConfigFile)
	}
	if cfg.AuthFile != "data/auth.json" {
		t.Fatalf("auth file = %q", cfg.AuthFile)
	}
	if cfg.DialTimeout != 5*time.Second {
		t.Fatalf("dial timeout = %s", cfg.DialTimeout)
	}
}

func TestLoadCustomClustersFileAndTimeout(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CLUSTERS_FILE", "/var/lib/etcd-studio/clusters.json")
	t.Setenv("HISTORY_FILE", "/secure/etcd-studio/history.jsonl")
	t.Setenv("HISTORY_CONFIG_FILE", "/secure/etcd-studio/history-storage.json")
	t.Setenv("AUTH_FILE", "/secure/etcd-studio/auth.json")
	t.Setenv("ETCD_DIAL_TIMEOUT", "12s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClustersFile != "/var/lib/etcd-studio/clusters.json" || cfg.HistoryFile != "/secure/etcd-studio/history.jsonl" || cfg.HistoryConfigFile != "/secure/etcd-studio/history-storage.json" || cfg.AuthFile != "/secure/etcd-studio/auth.json" || cfg.DialTimeout != 12*time.Second {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestHistoryFileDefaultsNextToCustomClustersFile(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CLUSTERS_FILE", "/var/lib/etcd-studio/clusters.json")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HistoryFile != "/var/lib/etcd-studio/history.jsonl" {
		t.Fatalf("history file = %q", cfg.HistoryFile)
	}
	if cfg.HistoryConfigFile != "/var/lib/etcd-studio/history-storage.json" {
		t.Fatalf("history config file = %q", cfg.HistoryConfigFile)
	}
	if cfg.AuthFile != "/var/lib/etcd-studio/auth.json" {
		t.Fatalf("auth file = %q", cfg.AuthFile)
	}
}

func TestLoadRejectsInvalidTimeout(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ETCD_DIAL_TIMEOUT", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected timeout validation error")
	}
}

func TestLoadBlankClustersFileUsesDefault(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CLUSTERS_FILE", "   ")
	if _, err := Load(); err != nil {
		t.Fatalf("blank environment should use the default: %v", err)
	}
}
