package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEtcdMemberStatusesIntegration(t *testing.T) {
	rawEndpoints := strings.TrimSpace(os.Getenv("ETCD_INTEGRATION_ENDPOINTS"))
	if rawEndpoints == "" {
		t.Skip("set ETCD_INTEGRATION_ENDPOINTS to run against a real etcd cluster")
	}
	endpoints := strings.Split(rawEndpoints, ",")
	client, err := NewEtcd(Cluster{Endpoints: endpoints}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statuses := client.MemberStatuses(ctx, endpoints)
	if len(statuses) != len(endpoints) {
		t.Fatalf("statuses = %d, want %d", len(statuses), len(endpoints))
	}
	leaders := 0
	for _, status := range statuses {
		if !status.Reachable || !status.Healthy {
			t.Errorf("endpoint %s is not healthy: %s", status.Endpoint, status.Error)
		}
		if status.MemberID != 0 && status.MemberID == status.LeaderID {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want exactly 1; statuses = %#v", leaders, statuses)
	}
}
