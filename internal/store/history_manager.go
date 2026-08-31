package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	historyStorageConfigVersion = 2
	defaultRetentionVersions    = 100
	maxRetentionVersions        = 10000
)

type persistedHistoryStorage struct {
	Version           int                 `json:"version"`
	Configured        time.Time           `json:"configured_at"`
	Storage           HistoryStorageInput `json:"storage"`
	RetentionVersions int                 `json:"retention_versions"`
}

// HistoryManager loads one configured backend and delegates all history calls.
// With no configuration file it remains unconfigured so the first-run page can
// collect the storage choice.
type HistoryManager struct {
	mu                sync.RWMutex
	configFile        string
	defaultLocalFile  string
	localFileRoot     string
	connectTimeout    time.Duration
	configuredAt      time.Time
	config            HistoryStorageInput
	retentionVersions int
	backend           historyBackend
}

type historyBackend interface {
	ValueHistory
	AuditLog
}

func NewHistoryManager(configFile, defaultLocalFile string, connectTimeout time.Duration) (*HistoryManager, error) {
	if strings.TrimSpace(configFile) == "" {
		return nil, errors.New("history storage config file cannot be empty")
	}
	if strings.TrimSpace(defaultLocalFile) == "" {
		return nil, errors.New("default local history file cannot be empty")
	}
	if connectTimeout <= 0 {
		return nil, errors.New("history storage connection timeout must be positive")
	}
	localFileRoot := filepath.Dir(defaultLocalFile)
	if err := os.MkdirAll(localFileRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create local history root: %w", err)
	}
	manager := &HistoryManager{
		configFile:        configFile,
		defaultLocalFile:  defaultLocalFile,
		localFileRoot:     localFileRoot,
		connectTimeout:    connectTimeout,
		retentionVersions: defaultRetentionVersions,
	}
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *HistoryManager) Status() HistoryStorageStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := HistoryStorageStatus{
		Configured:         m.backend != nil,
		ConfiguredAt:       m.configuredAt,
		DefaultLocalFile:   m.defaultLocalFile,
		RetentionVersions:  m.retentionVersions,
		AuditRetentionDays: DefaultAuditRetentionDays,
	}
	if m.backend == nil {
		return status
	}
	status.Type = m.config.Type
	status.LocalFile = m.config.LocalFile
	if m.config.Type == HistoryStorageLocal {
		status.AuditFile = auditFilePath(m.config.LocalFile)
	}
	status.Host = m.config.Host
	status.Port = m.config.Port
	status.Database = m.config.Database
	status.Username = m.config.Username
	status.PasswordConfigured = m.config.Password != ""
	status.TLSMode = m.config.TLSMode
	return status
}

func (m *HistoryManager) Test(ctx context.Context, input HistoryStorageInput) error {
	if _, err := normalizeRetentionVersions(input.RetentionVersions); err != nil {
		return err
	}
	normalized, err := normalizeHistoryStorageInput(input, m.defaultLocalFile)
	if err != nil {
		return err
	}
	backend, err := openHistoryBackend(ctx, normalized, m.connectTimeout, false, m.localFileRoot)
	if err != nil {
		return err
	}
	return backend.Close()
}

func (m *HistoryManager) Configure(ctx context.Context, input HistoryStorageInput) error {
	retentionVersions, err := normalizeRetentionVersions(input.RetentionVersions)
	if err != nil {
		return err
	}
	normalized, err := normalizeHistoryStorageInput(input, m.defaultLocalFile)
	if err != nil {
		return err
	}
	normalized.RetentionVersions = nil
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backend != nil {
		return ErrHistoryConfigured
	}
	backend, err := openHistoryBackend(ctx, normalized, m.connectTimeout, true, m.localFileRoot)
	if err != nil {
		return err
	}
	configuredAt := time.Now().UTC()
	if err := m.saveConfig(normalized, configuredAt, retentionVersions); err != nil {
		_ = backend.Close()
		return fmt.Errorf("%w: %v", ErrHistoryConfigSave, err)
	}

	m.config = normalized
	m.configuredAt = configuredAt
	m.retentionVersions = retentionVersions
	m.backend = backend
	return nil
}

func (m *HistoryManager) Save(snapshot ValueSnapshot) error {
	m.mu.RLock()
	backend := m.backend
	retentionVersions := m.retentionVersions
	m.mu.RUnlock()
	if backend == nil {
		return ErrHistoryNotSetup
	}
	if err := backend.Save(snapshot); err != nil {
		return err
	}
	return backend.PruneKey(snapshot.ClusterID, snapshot.Entry.Key, retentionVersions)
}

func (m *HistoryManager) SaveAudit(event AuditEvent) error {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return ErrHistoryNotSetup
	}
	if event.ID == "" {
		id, err := newAuditID()
		if err != nil {
			return fmt.Errorf("%w: generate event id: %v", ErrAuditLog, err)
		}
		event.ID = id
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if event.Result == "" {
		event.Result = "success"
	}
	if err := backend.SaveAudit(event); err != nil {
		return fmt.Errorf("%w: %v", ErrAuditLog, err)
	}
	if err := backend.PruneAudit(auditCutoff(time.Now())); err != nil {
		return fmt.Errorf("%w: prune expired events: %v", ErrAuditLog, err)
	}
	return nil
}

func (m *HistoryManager) ListAudit(query AuditQuery) (AuditPage, error) {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return AuditPage{}, ErrHistoryNotSetup
	}
	cutoff := auditCutoff(time.Now())
	if query.Since.IsZero() || query.Since.Before(cutoff) {
		query.Since = cutoff
	}
	page, err := backend.ListAudit(query)
	if err != nil {
		return AuditPage{}, fmt.Errorf("%w: %v", ErrAuditLog, err)
	}
	return page, nil
}

func (m *HistoryManager) PruneAudit(before time.Time) error {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return ErrHistoryNotSetup
	}
	if err := backend.PruneAudit(before.UTC()); err != nil {
		return fmt.Errorf("%w: %v", ErrAuditLog, err)
	}
	return nil
}

func (m *HistoryManager) UpdateRetention(ctx context.Context, keep int) error {
	if err := validateRetentionVersions(keep); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.backend == nil {
		m.mu.Unlock()
		return ErrHistoryNotSetup
	}
	if err := m.saveConfig(m.config, m.configuredAt, keep); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrHistoryConfigSave, err)
	}
	m.retentionVersions = keep
	backend := m.backend
	m.mu.Unlock()
	if err := backend.Prune(keep); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalHistory, err)
	}
	return nil
}

func (m *HistoryManager) LatestBefore(clusterID string, key []byte, modRevision int64) (ValueSnapshot, bool, error) {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return ValueSnapshot{}, false, ErrHistoryNotSetup
	}
	return backend.LatestBefore(clusterID, key, modRevision)
}

func (m *HistoryManager) ListBefore(clusterID string, key []byte, modRevision int64, limit int) ([]ValueSnapshot, error) {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return nil, ErrHistoryNotSetup
	}
	return backend.ListBefore(clusterID, key, modRevision, limit)
}

func (m *HistoryManager) PruneKey(clusterID string, key []byte, keep int) error {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return ErrHistoryNotSetup
	}
	return backend.PruneKey(clusterID, key, keep)
}

func (m *HistoryManager) Prune(keep int) error {
	m.mu.RLock()
	backend := m.backend
	m.mu.RUnlock()
	if backend == nil {
		return ErrHistoryNotSetup
	}
	return backend.Prune(keep)
}

func (m *HistoryManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backend == nil {
		return nil
	}
	err := m.backend.Close()
	m.backend = nil
	return err
}

func (m *HistoryManager) load() error {
	file, err := os.Open(m.configFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open history storage config: %w", err)
	}
	defer file.Close()
	var persisted persistedHistoryStorage
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&persisted); err != nil {
		return fmt.Errorf("decode history storage config: %w", err)
	}
	if persisted.Version != 1 && persisted.Version != historyStorageConfigVersion {
		return fmt.Errorf("unsupported history storage config version %d", persisted.Version)
	}
	retentionVersions := persisted.RetentionVersions
	if persisted.Version == 1 {
		retentionVersions = defaultRetentionVersions
	}
	if err := validateRetentionVersions(retentionVersions); err != nil {
		return fmt.Errorf("validate history retention: %w", err)
	}
	normalized, err := normalizeHistoryStorageInput(persisted.Storage, m.defaultLocalFile)
	if err != nil {
		return fmt.Errorf("validate history storage config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.connectTimeout)
	defer cancel()
	backend, err := openHistoryBackend(ctx, normalized, m.connectTimeout, true, m.localFileRoot)
	if err != nil {
		return fmt.Errorf("open configured history storage: %w", err)
	}
	m.config = normalized
	m.configuredAt = persisted.Configured
	m.retentionVersions = retentionVersions
	m.backend = backend
	if err := backend.Prune(retentionVersions); err != nil {
		_ = backend.Close()
		m.backend = nil
		return fmt.Errorf("prune configured history storage: %w", err)
	}
	if err := backend.PruneAudit(auditCutoff(time.Now())); err != nil {
		_ = backend.Close()
		m.backend = nil
		return fmt.Errorf("prune configured audit log: %w", err)
	}
	return nil
}

func (m *HistoryManager) saveConfig(config HistoryStorageInput, configuredAt time.Time, retentionVersions int) error {
	data, err := json.MarshalIndent(persistedHistoryStorage{
		Version:           historyStorageConfigVersion,
		Configured:        configuredAt,
		Storage:           config,
		RetentionVersions: retentionVersions,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode history storage config: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(m.configFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create history storage config directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".history-storage-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary history storage config: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure history storage config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write history storage config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync history storage config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close history storage config: %w", err)
	}
	if err := os.Rename(temporaryPath, m.configFile); err != nil {
		cleanup()
		return fmt.Errorf("replace history storage config: %w", err)
	}
	if err := os.Chmod(m.configFile, 0o600); err != nil {
		return fmt.Errorf("secure history storage config permissions: %w", err)
	}
	return nil
}

func normalizeHistoryStorageInput(input HistoryStorageInput, defaultLocalFile string) (HistoryStorageInput, error) {
	input.Type = strings.ToLower(strings.TrimSpace(input.Type))
	input.LocalFile = strings.TrimSpace(input.LocalFile)
	input.Host = strings.TrimSpace(input.Host)
	input.Database = strings.TrimSpace(input.Database)
	input.Username = strings.TrimSpace(input.Username)
	input.TLSMode = strings.ToLower(strings.TrimSpace(input.TLSMode))
	switch input.Type {
	case HistoryStorageLocal:
		if input.LocalFile == "" {
			input.LocalFile = defaultLocalFile
		}
		input.Host = ""
		input.Port = 0
		input.Database = ""
		input.Username = ""
		input.Password = ""
		input.TLSMode = ""
	case HistoryStoragePostgres:
		if input.Port == 0 {
			input.Port = 5432
		}
		if input.TLSMode == "" {
			input.TLSMode = "require"
		}
		if !oneOf(input.TLSMode, "disable", "prefer", "require", "verify-ca", "verify-full") {
			return HistoryStorageInput{}, fmt.Errorf("%w: PostgreSQL SSL 模式无效", ErrHistorySetup)
		}
	case HistoryStorageMySQL:
		if input.Port == 0 {
			input.Port = 3306
		}
		if input.TLSMode == "" {
			input.TLSMode = "preferred"
		}
		if !oneOf(input.TLSMode, "disable", "preferred", "require", "skip-verify") {
			return HistoryStorageInput{}, fmt.Errorf("%w: MySQL TLS 模式无效", ErrHistorySetup)
		}
	default:
		return HistoryStorageInput{}, fmt.Errorf("%w: 请选择本地文件、PostgreSQL 或 MySQL", ErrHistorySetup)
	}
	if input.Type != HistoryStorageLocal {
		input.LocalFile = ""
		if input.Host == "" || input.Database == "" || input.Username == "" {
			return HistoryStorageInput{}, fmt.Errorf("%w: 数据库地址、数据库名和账号不能为空", ErrHistorySetup)
		}
		if input.Port < 1 || input.Port > 65535 {
			return HistoryStorageInput{}, fmt.Errorf("%w: 数据库端口必须在 1 到 65535 之间", ErrHistorySetup)
		}
	}
	return input, nil
}

func normalizeRetentionVersions(value *int) (int, error) {
	if value == nil {
		return defaultRetentionVersions, nil
	}
	if err := validateRetentionVersions(*value); err != nil {
		return 0, err
	}
	return *value, nil
}

func validateRetentionVersions(value int) error {
	if value < 0 || value > maxRetentionVersions {
		return fmt.Errorf("%w: 每个 Key 的保留版本数必须在 0 到 %d 之间", ErrHistoryRetention, maxRetentionVersions)
	}
	return nil
}

func openHistoryBackend(ctx context.Context, config HistoryStorageInput, connectTimeout time.Duration, initialize bool, localFileRoot string) (historyBackend, error) {
	if config.Type == HistoryStorageLocal {
		return newLocalHistoryBackend(config.LocalFile, localFileRoot)
	}
	return NewDatabaseHistory(ctx, config, connectTimeout, initialize)
}

func auditCutoff(now time.Time) time.Time {
	return now.UTC().AddDate(0, 0, -DefaultAuditRetentionDays)
}

func newAuditID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
