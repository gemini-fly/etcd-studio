package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileHistoryPersistsBinarySnapshotsAndFindsLatestBeforeRevision(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "nested", "history.jsonl")
	history, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	first := ValueSnapshot{
		ClusterID:  "cluster-1",
		Entry:      Entry{Key: []byte{0xff, 0x01}, Value: []byte{0x00, 0xff}, ModRevision: 10, Version: 1},
		CapturedAt: time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC),
	}
	second := ValueSnapshot{
		ClusterID:  "cluster-1",
		Entry:      Entry{Key: []byte{0xff, 0x01}, Value: []byte("second"), ModRevision: 20, Version: 2},
		CapturedAt: time.Date(2026, 8, 28, 8, 1, 0, 0, time.UTC),
	}
	if err := history.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := history.Save(second); err != nil {
		t.Fatal(err)
	}
	if err := history.Save(second); err != nil {
		t.Fatalf("duplicate save: %v", err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("history permissions = %o, want 600", got)
	}
	reopened, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	latest, found, err := reopened.LatestBefore("cluster-1", first.Entry.Key, 21)
	if err != nil || !found {
		t.Fatalf("latest before 21 = %#v, %v, %v", latest, found, err)
	}
	if latest.Entry.ModRevision != 20 || !bytes.Equal(latest.Entry.Value, []byte("second")) {
		t.Fatalf("latest = %#v", latest)
	}
	previous, found, err := reopened.LatestBefore("cluster-1", first.Entry.Key, 20)
	if err != nil || !found {
		t.Fatalf("latest before 20 = %#v, %v, %v", previous, found, err)
	}
	if previous.Entry.ModRevision != 10 || !bytes.Equal(previous.Entry.Value, []byte{0x00, 0xff}) {
		t.Fatalf("previous = %#v", previous)
	}
	versions, err := reopened.ListBefore("cluster-1", first.Entry.Key, 21, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Entry.ModRevision != 20 || versions[1].Entry.ModRevision != 10 {
		t.Fatalf("versions = %#v", versions)
	}
}

func TestFileHistoryDiscardsPartialTrailingRecord(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "history.jsonl")
	history, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ValueSnapshot{ClusterID: "cluster-1", Entry: Entry{Key: []byte("/key"), Value: []byte("one"), ModRevision: 2}}
	if err := history.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"format_version":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Save(ValueSnapshot{ClusterID: "cluster-1", Entry: Entry{Key: []byte("/key"), Value: []byte("two"), ModRevision: 3}}); err != nil {
		t.Fatal(err)
	}
	latest, found, err := reopened.LatestBefore("cluster-1", []byte("/key"), 4)
	if err != nil || !found || string(latest.Entry.Value) != "two" {
		t.Fatalf("latest after recovery = %#v, %v, %v", latest, found, err)
	}
}

func TestFileHistoryPrunesNewestVersionsPerKeyAndPersistsRewrite(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "history.jsonl")
	history, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 5; revision++ {
		if err := history.Save(ValueSnapshot{
			ClusterID: "cluster-1",
			Entry:     Entry{Key: []byte("/feature"), Value: []byte{byte(revision)}, ModRevision: revision, Version: revision},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := history.Save(ValueSnapshot{ClusterID: "cluster-1", Entry: Entry{Key: []byte("/other"), Value: []byte("keep"), ModRevision: 9, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := history.Prune(2); err != nil {
		t.Fatal(err)
	}
	versions, err := history.ListBefore("cluster-1", []byte("/feature"), 10, 10)
	if err != nil || len(versions) != 2 || versions[0].Entry.ModRevision != 5 || versions[1].Entry.ModRevision != 4 {
		t.Fatalf("pruned versions = %#v, %v", versions, err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileHistory(filePath, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	versions, err = reopened.ListBefore("cluster-1", []byte("/feature"), 10, 10)
	if err != nil || len(versions) != 2 || versions[0].Entry.ModRevision != 5 || versions[1].Entry.ModRevision != 4 {
		t.Fatalf("reloaded pruned versions = %#v, %v", versions, err)
	}
	other, err := reopened.ListBefore("cluster-1", []byte("/other"), 10, 10)
	if err != nil || len(other) != 1 || string(other[0].Entry.Value) != "keep" {
		t.Fatalf("other key = %#v, %v", other, err)
	}
}
