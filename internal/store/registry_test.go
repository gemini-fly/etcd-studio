package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryPersistsMultipleClustersWithSecurePermissions(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "nested", "clusters.json")
	registry, err := NewRegistry(filePath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	password := "secret"
	first, err := registry.CreateCluster(ClusterInput{
		Name: "生产集群", Endpoints: []string{"https://etcd-1.internal:2379", "https://etcd-2.internal:2379"},
		Username: "operator", Password: &password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.CreateCluster(ClusterInput{Name: "测试集群", Endpoints: []string{"http://127.0.0.1:12379"}}); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.ListClusters()); got != 2 {
		t.Fatalf("clusters = %d, want 2", got)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("file permission = %o, want 600", permission)
	}

	updated, err := registry.UpdateCluster(first.ID, ClusterInput{
		Name: "生产集群", Endpoints: []string{"https://etcd.internal:2379"}, Username: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Password != password {
		t.Fatal("omitted password did not preserve the stored credential")
	}

	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewRegistry(filePath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	clusters := reloaded.ListClusters()
	if len(clusters) != 2 {
		t.Fatalf("reloaded clusters = %d, want 2", len(clusters))
	}
	foundPassword := false
	for _, cluster := range clusters {
		if cluster.ID == first.ID && cluster.Password == password {
			foundPassword = true
		}
	}
	if !foundPassword {
		t.Fatal("persisted password was not restored")
	}
}

func TestRegistryRejectsDuplicateNamesAndInvalidEndpoints(t *testing.T) {
	registry, err := NewRegistry(filepath.Join(t.TempDir(), "clusters.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	if _, err := registry.CreateCluster(ClusterInput{Name: "Production", Endpoints: []string{"http://127.0.0.1:2379"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.CreateCluster(ClusterInput{Name: "production", Endpoints: []string{"http://127.0.0.1:3379"}}); !errors.Is(err, ErrClusterNameExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := registry.CreateCluster(ClusterInput{Name: "Broken", Endpoints: []string{"127.0.0.1:2379"}}); !errors.Is(err, ErrInvalidCluster) {
		t.Fatalf("invalid endpoint error = %v", err)
	}
	if _, err := registry.CreateCluster(ClusterInput{Name: "Credentials", Endpoints: []string{"http://user:pass@127.0.0.1:2379"}}); !errors.Is(err, ErrInvalidCluster) {
		t.Fatalf("credential endpoint error = %v", err)
	}
}

func TestRegistryDeleteOnlyRemovesConnectionConfiguration(t *testing.T) {
	registry, err := NewRegistry(filepath.Join(t.TempDir(), "clusters.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	cluster, err := registry.CreateCluster(ClusterInput{Name: "Disposable", Endpoints: []string{"http://127.0.0.1:2379"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteCluster(cluster.ID); err != nil {
		t.Fatal(err)
	}
	if len(registry.ListClusters()) != 0 {
		t.Fatal("deleted cluster remains in registry")
	}
	if _, _, err := registry.ClusterKV(cluster.ID); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("lookup error = %v", err)
	}
}
