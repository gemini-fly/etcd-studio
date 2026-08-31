package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const clusterFileVersion = 1

type managedCluster struct {
	config Cluster
	client *Etcd
}

// Registry is a file-backed collection of independent etcd connections.
type Registry struct {
	mu          sync.RWMutex
	filePath    string
	dialTimeout time.Duration
	clusters    map[string]*managedCluster
}

type clusterFile struct {
	Version  int       `json:"version"`
	Clusters []Cluster `json:"clusters"`
}

func NewRegistry(filePath string, dialTimeout time.Duration) (*Registry, error) {
	registry := &Registry{
		filePath:    filePath,
		dialTimeout: dialTimeout,
		clusters:    make(map[string]*managedCluster),
	}
	if err := registry.load(); err != nil {
		return nil, err
	}
	return registry, nil
}

func (r *Registry) ListClusters() []Cluster {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clusters := make([]Cluster, 0, len(r.clusters))
	for _, managed := range r.clusters {
		clusters = append(clusters, cloneCluster(managed.config))
	}
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].Name == clusters[j].Name {
			return clusters[i].ID < clusters[j].ID
		}
		return strings.ToLower(clusters[i].Name) < strings.ToLower(clusters[j].Name)
	})
	return clusters
}

func (r *Registry) CreateCluster(input ClusterInput) (Cluster, error) {
	cluster, err := clusterFromInput(input, Cluster{})
	if err != nil {
		return Cluster{}, err
	}
	cluster.ID, err = newClusterID()
	if err != nil {
		return Cluster{}, err
	}
	now := time.Now().UTC()
	cluster.CreatedAt = now
	cluster.UpdatedAt = now

	managed := &managedCluster{config: cluster}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nameExistsLocked(cluster.Name, "") {
		return Cluster{}, ErrClusterNameExists
	}
	r.clusters[cluster.ID] = managed
	if err := r.saveLocked(); err != nil {
		delete(r.clusters, cluster.ID)
		return Cluster{}, err
	}
	return cloneCluster(cluster), nil
}

func (r *Registry) UpdateCluster(id string, input ClusterInput) (Cluster, error) {
	r.mu.RLock()
	existing, found := r.clusters[id]
	if !found {
		r.mu.RUnlock()
		return Cluster{}, ErrClusterNotFound
	}
	base := cloneCluster(existing.config)
	r.mu.RUnlock()

	cluster, err := clusterFromInput(input, base)
	if err != nil {
		return Cluster{}, err
	}
	cluster.ID = base.ID
	cluster.CreatedAt = base.CreatedAt
	cluster.UpdatedAt = time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, found := r.clusters[id]
	if !found {
		return Cluster{}, ErrClusterNotFound
	}
	if r.nameExistsLocked(cluster.Name, id) {
		return Cluster{}, ErrClusterNameExists
	}
	r.clusters[id] = &managedCluster{config: cluster}
	if err := r.saveLocked(); err != nil {
		r.clusters[id] = current
		return Cluster{}, err
	}
	if current.client != nil {
		_ = current.client.Close()
	}
	return cloneCluster(cluster), nil
}

func (r *Registry) DeleteCluster(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	managed, found := r.clusters[id]
	if !found {
		return ErrClusterNotFound
	}
	delete(r.clusters, id)
	if err := r.saveLocked(); err != nil {
		r.clusters[id] = managed
		return err
	}
	if managed.client != nil {
		return managed.client.Close()
	}
	return nil
}

func (r *Registry) TestCluster(ctx context.Context, id string, input ClusterInput) error {
	var base Cluster
	if id != "" {
		r.mu.RLock()
		managed, found := r.clusters[id]
		if found {
			base = cloneCluster(managed.config)
		}
		r.mu.RUnlock()
		if !found {
			return ErrClusterNotFound
		}
	}
	cluster, err := clusterFromInput(input, base)
	if err != nil {
		return err
	}
	client, err := NewEtcd(cluster, r.dialTimeout)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCluster, err)
	}
	defer client.Close()
	return client.Health(ctx)
}

func (r *Registry) ClusterKV(id string) (KV, Cluster, error) {
	r.mu.RLock()
	managed, found := r.clusters[id]
	if !found {
		r.mu.RUnlock()
		return nil, Cluster{}, ErrClusterNotFound
	}
	cluster := cloneCluster(managed.config)
	if managed.client != nil {
		client := managed.client
		r.mu.RUnlock()
		return client, cluster, nil
	}
	r.mu.RUnlock()

	client, err := NewEtcd(cluster, r.dialTimeout)
	if err != nil {
		return nil, cluster, err
	}
	r.mu.Lock()
	current, found := r.clusters[id]
	if !found {
		r.mu.Unlock()
		_ = client.Close()
		return nil, Cluster{}, ErrClusterNotFound
	}
	if !current.config.UpdatedAt.Equal(cluster.UpdatedAt) {
		r.mu.Unlock()
		_ = client.Close()
		return r.ClusterKV(id)
	}
	if current.client != nil {
		existingClient := current.client
		currentCluster := cloneCluster(current.config)
		r.mu.Unlock()
		_ = client.Close()
		return existingClient, currentCluster, nil
	}
	current.client = client
	currentCluster := cloneCluster(current.config)
	r.mu.Unlock()
	return client, currentCluster, nil
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var closeErr error
	for _, managed := range r.clusters {
		if managed.client != nil {
			closeErr = errors.Join(closeErr, managed.client.Close())
		}
	}
	return closeErr
}

func (r *Registry) load() error {
	file, err := os.Open(r.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open clusters file: %w", err)
	}
	defer file.Close()

	var persisted clusterFile
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&persisted); err != nil {
		return fmt.Errorf("decode clusters file: %w", err)
	}
	if persisted.Version != clusterFileVersion {
		return fmt.Errorf("unsupported clusters file version %d", persisted.Version)
	}
	for _, cluster := range persisted.Clusters {
		if cluster.ID == "" {
			return errors.New("clusters file contains an empty cluster id")
		}
		if _, exists := r.clusters[cluster.ID]; exists {
			return fmt.Errorf("clusters file contains duplicate id %q", cluster.ID)
		}
		if err := validateCluster(cluster); err != nil {
			return fmt.Errorf("invalid saved cluster %q: %w", cluster.Name, err)
		}
		r.clusters[cluster.ID] = &managedCluster{config: cluster}
	}
	return nil
}

func (r *Registry) saveLocked() error {
	clusters := make([]Cluster, 0, len(r.clusters))
	for _, managed := range r.clusters {
		clusters = append(clusters, managed.config)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ID < clusters[j].ID })
	data, err := json.MarshalIndent(clusterFile{Version: clusterFileVersion, Clusters: clusters}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode clusters file: %w", err)
	}
	data = append(data, '\n')

	directory := filepath.Dir(r.filePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create clusters directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".clusters-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary clusters file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("set clusters file permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write clusters file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync clusters file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close clusters file: %w", err)
	}
	if err := os.Rename(temporaryPath, r.filePath); err != nil {
		cleanup()
		return fmt.Errorf("replace clusters file: %w", err)
	}
	if err := os.Chmod(r.filePath, 0o600); err != nil {
		return fmt.Errorf("secure clusters file permissions: %w", err)
	}
	return nil
}

func (r *Registry) nameExistsLocked(name, exceptID string) bool {
	for id, managed := range r.clusters {
		if id != exceptID && strings.EqualFold(managed.config.Name, name) {
			return true
		}
	}
	return false
}

func clusterFromInput(input ClusterInput, base Cluster) (Cluster, error) {
	cluster := base
	cluster.Name = strings.TrimSpace(input.Name)
	cluster.Username = strings.TrimSpace(input.Username)
	cluster.TLSCAFile = strings.TrimSpace(input.TLSCAFile)
	cluster.TLSCertFile = strings.TrimSpace(input.TLSCertFile)
	cluster.TLSKeyFile = strings.TrimSpace(input.TLSKeyFile)
	cluster.Endpoints = make([]string, 0, len(input.Endpoints))
	seen := make(map[string]struct{}, len(input.Endpoints))
	for _, endpoint := range input.Endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if _, exists := seen[endpoint]; !exists {
			cluster.Endpoints = append(cluster.Endpoints, endpoint)
			seen[endpoint] = struct{}{}
		}
	}
	if input.Password != nil {
		cluster.Password = *input.Password
	}
	if err := validateCluster(cluster); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

func validateCluster(cluster Cluster) error {
	if cluster.Name == "" {
		return fmt.Errorf("%w: 集群名称不能为空", ErrInvalidCluster)
	}
	if len([]rune(cluster.Name)) > 80 {
		return fmt.Errorf("%w: 集群名称不能超过 80 个字符", ErrInvalidCluster)
	}
	if len(cluster.Endpoints) == 0 {
		return fmt.Errorf("%w: 至少需要一个 Endpoint", ErrInvalidCluster)
	}
	for _, endpoint := range cluster.Endpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%w: Endpoint 必须是有效的 http 或 https 地址: %s", ErrInvalidCluster, endpoint)
		}
		if parsed.User != nil {
			return fmt.Errorf("%w: Endpoint 中不能包含用户名或密码", ErrInvalidCluster)
		}
	}
	if (cluster.TLSCertFile == "") != (cluster.TLSKeyFile == "") {
		return fmt.Errorf("%w: 客户端证书和私钥必须同时配置", ErrInvalidCluster)
	}
	if cluster.Username == "" && cluster.Password != "" {
		return fmt.Errorf("%w: 配置密码时必须填写用户名", ErrInvalidCluster)
	}
	return nil
}

func newClusterID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate cluster id: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func cloneCluster(cluster Cluster) Cluster {
	cluster.Endpoints = append([]string(nil), cluster.Endpoints...)
	return cluster
}
