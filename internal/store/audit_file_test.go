package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFileAuditPersistsFiltersAndPrunesEvents(t *testing.T) {
	t.Parallel()
	filePath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewFileAudit(filePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	events := []AuditEvent{
		{ID: "00000000000000000000000000000001", OccurredAt: now.AddDate(0, 0, -91), Actor: "old-user", ActorType: "authenticated_user", Action: "key.update", ResourceType: "key", ClusterID: "cluster-1", ClusterName: "生产", Target: "/old", Result: "success"},
		{ID: "00000000000000000000000000000002", OccurredAt: now.Add(-time.Hour), Actor: "alice", ActorType: "authenticated_user", Action: "key.update", ResourceType: "key", ClusterID: "cluster-1", ClusterName: "生产", Target: "/feature", Detail: "修订版本 #8", Result: "success"},
		{ID: "00000000000000000000000000000003", OccurredAt: now, Actor: "bob", ActorType: "authenticated_user", Action: "cluster.create", ResourceType: "cluster", ClusterID: "cluster-2", ClusterName: "测试", Target: "测试", Result: "success"},
	}
	for _, event := range events {
		if err := audit.SaveAudit(event); err != nil {
			t.Fatal(err)
		}
	}
	page, err := audit.ListAudit(AuditQuery{Limit: 10, ClusterID: "cluster-1", Search: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Actor != "alice" {
		t.Fatalf("filtered events = %#v", page.Events)
	}
	page, err = audit.ListAudit(AuditQuery{Limit: 10, Since: now.Add(-30 * time.Minute), Until: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Actor != "bob" {
		t.Fatalf("date-filtered events = %#v", page.Events)
	}
	if err := audit.PruneAudit(now.AddDate(0, 0, -DefaultAuditRetentionDays)); err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileAudit(filePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err = reopened.ListAudit(AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Actor != "bob" || page.Events[1].Actor != "alice" {
		t.Fatalf("reloaded events = %#v", page.Events)
	}
}

func TestHistoryManagerAddsAuditIdentityAndAppliesNinetyDayRetention(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	manager, err := NewHistoryManager(filepath.Join(directory, "history-storage.json"), filepath.Join(directory, "history.jsonl"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Configure(t.Context(), HistoryStorageInput{Type: HistoryStorageLocal}); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(); status.AuditRetentionDays != 90 || status.AuditFile != filepath.Join(directory, "history.audit.jsonl") {
		t.Fatalf("audit status = %#v", status)
	}
	event := AuditEvent{
		OccurredAt: time.Now().UTC().AddDate(0, 0, -91), Actor: "alice", ActorType: "authenticated_user",
		Action: "key.update", ResourceType: "key", Target: "/expired",
	}
	if err := manager.SaveAudit(event); err != nil {
		t.Fatal(err)
	}
	page, err := manager.ListAudit(AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("expired events were retained: %#v", page.Events)
	}
}
