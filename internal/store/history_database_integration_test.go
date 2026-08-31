package store

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestDatabaseHistoryAuditIntegration(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		storageType string
		username    string
		password    string
	}{
		{name: "postgres", environment: "ETCD_STUDIO_TEST_POSTGRES_ADDR", storageType: HistoryStoragePostgres, username: "postgres", password: "audit-test"},
		{name: "mysql", environment: "ETCD_STUDIO_TEST_MYSQL_ADDR", storageType: HistoryStorageMySQL, username: "root", password: "audit-test"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			address := os.Getenv(testCase.environment)
			if address == "" {
				t.Skipf("%s is not set", testCase.environment)
			}
			host, rawPort, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(rawPort)
			if err != nil {
				t.Fatal(err)
			}
			config := HistoryStorageInput{
				Type: testCase.storageType, Host: host, Port: port, Database: "etcd_studio",
				Username: testCase.username, Password: testCase.password, TLSMode: "disable",
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			history, err := NewDatabaseHistory(ctx, config, 5*time.Second, true)
			if err != nil {
				t.Fatal(err)
			}
			defer history.Close()
			id, err := newAuditID()
			if err != nil {
				t.Fatal(err)
			}
			event := AuditEvent{
				ID: id, OccurredAt: time.Now().UTC(), Actor: "integration-user", ActorType: "authenticated_user",
				Action: "key.update", ResourceType: "key", ClusterID: "cluster-1", ClusterName: "集成测试",
				Target: "/integration/audit", Detail: "保存为 etcd 修订版本 #1", Result: "success",
			}
			if err := history.SaveAudit(event); err != nil {
				t.Fatal(err)
			}
			page, err := history.ListAudit(AuditQuery{Since: time.Now().Add(-time.Hour), Limit: 10, Search: "integration-user"})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 1 || page.Events[0].ID != id {
				t.Fatalf("audit events = %#v", page.Events)
			}
			if err := history.PruneAudit(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			page, err = history.ListAudit(AuditQuery{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 0 {
				t.Fatalf("pruned audit events = %#v", page.Events)
			}
		})
	}
}
