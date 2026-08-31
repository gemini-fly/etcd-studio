package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryManagerConfiguresAndReloadsLocalStorage(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configFile := filepath.Join(directory, "history-storage.json")
	historyFile := filepath.Join(directory, "values", "history.jsonl")
	manager, err := NewHistoryManager(configFile, historyFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Status().Configured {
		t.Fatal("new manager unexpectedly configured")
	}
	if err := manager.Save(ValueSnapshot{}); !errors.Is(err, ErrHistoryNotSetup) {
		t.Fatalf("save before setup error = %v", err)
	}
	if err := manager.Test(context.Background(), HistoryStorageInput{Type: HistoryStorageLocal}); err != nil {
		t.Fatalf("test local storage: %v", err)
	}
	if err := manager.Configure(context.Background(), HistoryStorageInput{Type: HistoryStorageLocal}); err != nil {
		t.Fatalf("configure local storage: %v", err)
	}
	status := manager.Status()
	if !status.Configured || status.ConfiguredAt.IsZero() || status.Type != HistoryStorageLocal || status.LocalFile != historyFile {
		t.Fatalf("status = %#v", status)
	}
	snapshot := ValueSnapshot{ClusterID: "cluster-1", Entry: Entry{Key: []byte("/feature"), Value: []byte("working"), ModRevision: 9}}
	if err := manager.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
	reloaded, err := NewHistoryManager(configFile, historyFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	previous, found, err := reloaded.LatestBefore("cluster-1", []byte("/feature"), 10)
	if err != nil || !found || string(previous.Entry.Value) != "working" {
		t.Fatalf("reloaded history = %#v, %v, %v", previous, found, err)
	}
}

func TestHistoryStorageInputValidationAndDefaults(t *testing.T) {
	t.Parallel()
	postgres, err := normalizeHistoryStorageInput(HistoryStorageInput{
		Type: "postgres", Host: "db.internal", Database: "etcd_studio", Username: "studio",
	}, "/data/history.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if postgres.Port != 5432 || postgres.TLSMode != "require" {
		t.Fatalf("postgres defaults = %#v", postgres)
	}
	mysql, err := normalizeHistoryStorageInput(HistoryStorageInput{
		Type: "mysql", Host: "db.internal", Database: "etcd_studio", Username: "studio",
	}, "/data/history.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if mysql.Port != 3306 || mysql.TLSMode != "preferred" {
		t.Fatalf("mysql defaults = %#v", mysql)
	}
	if _, err := normalizeHistoryStorageInput(HistoryStorageInput{Type: "postgres", Host: "db.internal"}, "/data/history.jsonl"); !errors.Is(err, ErrHistorySetup) {
		t.Fatalf("invalid database error = %v", err)
	}
	invalidRetention := maxRetentionVersions + 1
	if _, err := normalizeRetentionVersions(&invalidRetention); !errors.Is(err, ErrHistoryRetention) {
		t.Fatalf("invalid retention error = %v", err)
	}
}

func TestHistoryManagerRejectsLocalFileOutsideManagedDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "history.jsonl")
	manager, err := NewHistoryManager(
		filepath.Join(directory, "history-storage.json"),
		filepath.Join(directory, "history.jsonl"),
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Test(context.Background(), HistoryStorageInput{Type: HistoryStorageLocal, LocalFile: outside}); err == nil {
		t.Fatal("outside local history path was accepted")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside history file was created: %v", err)
	}
}

func TestHistoryManagerMigratesVersionOneConfigToDefaultRetention(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configFile := filepath.Join(directory, "history-storage.json")
	historyFile := filepath.Join(directory, "history.jsonl")
	data, err := json.Marshal(persistedHistoryStorage{
		Version:    1,
		Configured: time.Now().UTC(),
		Storage:    HistoryStorageInput{Type: HistoryStorageLocal, LocalFile: historyFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewHistoryManager(configFile, historyFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if manager.Status().RetentionVersions != defaultRetentionVersions {
		t.Fatalf("status = %#v", manager.Status())
	}
}

func TestDatabaseConnectionEscapesCredentials(t *testing.T) {
	t.Parallel()
	config := HistoryStorageInput{
		Type: HistoryStoragePostgres, Host: "db.internal", Port: 5432, Database: "etcd studio",
		Username: "user@name", Password: "p:/?#word", TLSMode: "require",
	}
	driver, dataSource, err := databaseConnection(config, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if driver != "pgx" || dataSource == "" {
		t.Fatalf("connection = %q, %q", driver, dataSource)
	}
	parsed, err := url.Parse(dataSource)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if parsed.User.Username() != config.Username || password != config.Password || parsed.Host != "db.internal:5432" || parsed.Path != "/etcd studio" {
		t.Fatalf("parsed connection = %#v", parsed)
	}
}

func TestHistoryManagerPersistsAndAppliesConfigurableRetention(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	manager, err := NewHistoryManager(filepath.Join(directory, "history-storage.json"), filepath.Join(directory, "history.jsonl"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	keepTwo := 2
	if err := manager.Configure(context.Background(), HistoryStorageInput{Type: HistoryStorageLocal, RetentionVersions: &keepTwo}); err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 4; revision++ {
		if err := manager.Save(ValueSnapshot{
			ClusterID: "cluster-1",
			Entry:     Entry{Key: []byte("/feature"), Value: []byte{byte(revision)}, ModRevision: revision, Version: revision},
		}); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := manager.ListBefore("cluster-1", []byte("/feature"), 10, 10)
	if err != nil || len(versions) != 2 || versions[0].Entry.ModRevision != 4 || versions[1].Entry.ModRevision != 3 {
		t.Fatalf("retained versions = %#v, %v", versions, err)
	}
	if manager.Status().RetentionVersions != 2 {
		t.Fatalf("status = %#v", manager.Status())
	}
	if err := manager.UpdateRetention(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	versions, err = manager.ListBefore("cluster-1", []byte("/feature"), 10, 10)
	if err != nil || len(versions) != 1 || versions[0].Entry.ModRevision != 4 {
		t.Fatalf("immediately pruned versions = %#v, %v", versions, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewHistoryManager(filepath.Join(directory, "history-storage.json"), filepath.Join(directory, "history.jsonl"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if reloaded.Status().RetentionVersions != 1 {
		t.Fatalf("reloaded status = %#v", reloaded.Status())
	}
	if err := reloaded.UpdateRetention(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	for revision := int64(5); revision <= 6; revision++ {
		if err := reloaded.Save(ValueSnapshot{ClusterID: "cluster-1", Entry: Entry{Key: []byte("/feature"), Value: []byte{byte(revision)}, ModRevision: revision, Version: revision}}); err != nil {
			t.Fatal(err)
		}
	}
	versions, err = reloaded.ListBefore("cluster-1", []byte("/feature"), 10, 10)
	if err != nil || len(versions) != 3 {
		t.Fatalf("unlimited versions = %#v, %v", versions, err)
	}
}
