package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrConflict          = errors.New("the key changed since it was loaded")
	ErrClusterNotFound   = errors.New("cluster not found")
	ErrClusterNameExists = errors.New("cluster name already exists")
	ErrInvalidCluster    = errors.New("invalid cluster configuration")
	ErrHistoryCompacted  = errors.New("etcd history has been compacted")
	ErrNoPreviousVersion = errors.New("key has no previous value version")
	ErrLocalHistory      = errors.New("local value history operation failed")
	ErrHistoryNotSetup   = errors.New("value history storage is not configured")
	ErrHistorySetup      = errors.New("invalid value history storage configuration")
	ErrHistoryConfigured = errors.New("value history storage is already configured")
	ErrHistoryConfigSave = errors.New("save value history storage configuration")
	ErrHistoryRetention  = errors.New("invalid value history retention setting")
	ErrAuditLog          = errors.New("audit log operation failed")
)

// Entry is an etcd key-value pair with its MVCC metadata.
type Entry struct {
	Key            []byte
	Value          []byte
	CreateRevision int64
	ModRevision    int64
	Version        int64
	Lease          int64
}

// Page is one lexicographically ordered range of keys.
type Page struct {
	Entries    []Entry
	NextCursor []byte
}

// ValueSnapshot is a durable local copy of one etcd value before a mutation.
// ClusterID namespaces keys from independently configured etcd clusters.
type ValueSnapshot struct {
	ClusterID  string
	Entry      Entry
	CapturedAt time.Time
}

// ValueHistory persists previous values independently from etcd MVCC history.
type ValueHistory interface {
	Save(snapshot ValueSnapshot) error
	LatestBefore(clusterID string, key []byte, modRevision int64) (ValueSnapshot, bool, error)
	ListBefore(clusterID string, key []byte, modRevision int64, limit int) ([]ValueSnapshot, error)
	PruneKey(clusterID string, key []byte, keep int) error
	Prune(keep int) error
	Close() error
}

const DefaultAuditRetentionDays = 90

// AuditEvent describes one state-changing operation. It intentionally excludes
// request bodies, etcd values, passwords, and certificate contents.
type AuditEvent struct {
	ID           string    `json:"id"`
	OccurredAt   time.Time `json:"occurred_at"`
	Actor        string    `json:"actor"`
	ActorType    string    `json:"actor_type"`
	ClientIP     string    `json:"client_ip,omitempty"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ClusterID    string    `json:"cluster_id,omitempty"`
	ClusterName  string    `json:"cluster_name,omitempty"`
	Target       string    `json:"target"`
	Detail       string    `json:"detail,omitempty"`
	Result       string    `json:"result"`
}

// AuditQuery selects a stable, reverse-chronological page of audit events.
type AuditQuery struct {
	Since     time.Time
	Until     time.Time
	Before    time.Time
	BeforeID  string
	Limit     int
	ClusterID string
	Actor     string
	ActorType string
	Action    string
	Search    string
}

type AuditPage struct {
	Events  []AuditEvent
	HasMore bool
}

type AuditLog interface {
	SaveAudit(event AuditEvent) error
	ListAudit(query AuditQuery) (AuditPage, error)
	PruneAudit(before time.Time) error
}

const (
	HistoryStorageLocal    = "local"
	HistoryStoragePostgres = "postgres"
	HistoryStorageMySQL    = "mysql"
)

// HistoryStorageInput is submitted by the first-run setup page.
type HistoryStorageInput struct {
	Type              string `json:"type"`
	LocalFile         string `json:"local_file"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Database          string `json:"database"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	TLSMode           string `json:"tls_mode"`
	RetentionVersions *int   `json:"retention_versions,omitempty"`
}

// HistoryStorageStatus is safe to return to the browser and never includes a password.
type HistoryStorageStatus struct {
	Configured         bool      `json:"configured"`
	ConfiguredAt       time.Time `json:"configured_at,omitempty"`
	Type               string    `json:"type,omitempty"`
	LocalFile          string    `json:"local_file,omitempty"`
	DefaultLocalFile   string    `json:"default_local_file,omitempty"`
	Host               string    `json:"host,omitempty"`
	Port               int       `json:"port,omitempty"`
	Database           string    `json:"database,omitempty"`
	Username           string    `json:"username,omitempty"`
	PasswordConfigured bool      `json:"password_configured"`
	TLSMode            string    `json:"tls_mode,omitempty"`
	RetentionVersions  int       `json:"retention_versions"`
	AuditFile          string    `json:"audit_file,omitempty"`
	AuditRetentionDays int       `json:"audit_retention_days"`
}

// HistoryStorage combines value history operations with first-run configuration.
type HistoryStorage interface {
	ValueHistory
	AuditLog
	Status() HistoryStorageStatus
	Test(ctx context.Context, input HistoryStorageInput) error
	Configure(ctx context.Context, input HistoryStorageInput) error
	UpdateRetention(ctx context.Context, keep int) error
}

// KV describes the operations needed by the HTTP application.
type KV interface {
	Health(ctx context.Context) error
	MemberStatuses(ctx context.Context, endpoints []string) []MemberStatus
	List(ctx context.Context, prefix, cursor []byte, limit int64) (Page, error)
	Get(ctx context.Context, key []byte) (Entry, bool, error)
	GetAtRevision(ctx context.Context, key []byte, revision int64) (Entry, bool, error)
	Put(ctx context.Context, key, value []byte, expectedModRevision *int64) (int64, error)
	Delete(ctx context.Context, key []byte, expectedModRevision *int64) (int64, bool, error)
}

// MemberStatus is the live maintenance status of one configured etcd endpoint.
// Member and leader IDs stay internal so the HTTP layer can expose a role-safe
// label instead of leaking endpoint details to users without cluster access.
type MemberStatus struct {
	Endpoint  string
	MemberID  uint64
	LeaderID  uint64
	Reachable bool
	Healthy   bool
	Version   string
	Error     string
}

// Cluster is one independent etcd cluster connection. Endpoints are members of
// the same cluster and are used by the etcd client for failover.
type Cluster struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Endpoints   []string  `json:"endpoints"`
	Username    string    `json:"username,omitempty"`
	Password    string    `json:"password,omitempty"`
	TLSCAFile   string    `json:"tls_ca_file,omitempty"`
	TLSCertFile string    `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string    `json:"tls_key_file,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ClusterInput is used for create/update/test operations. A nil password means
// preserve the existing password during update.
type ClusterInput struct {
	Name        string
	Endpoints   []string
	Username    string
	Password    *string
	TLSCAFile   string
	TLSCertFile string
	TLSKeyFile  string
}

// ClusterRegistry persists cluster connections and resolves their KV clients.
type ClusterRegistry interface {
	ListClusters() []Cluster
	CreateCluster(input ClusterInput) (Cluster, error)
	UpdateCluster(id string, input ClusterInput) (Cluster, error)
	DeleteCluster(id string) error
	TestCluster(ctx context.Context, id string, input ClusterInput) error
	ClusterKV(id string) (KV, Cluster, error)
	Close() error
}
