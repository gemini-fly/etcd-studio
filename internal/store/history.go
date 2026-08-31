package store

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gemini-fly/etcd-studio/internal/safefile"
)

const (
	historyRecordVersion = 1
	maxHistoryRecordSize = 8 << 20
)

type historyRecord struct {
	FormatVersion  int       `json:"format_version"`
	ClusterID      string    `json:"cluster_id"`
	KeyBase64      string    `json:"key_base64"`
	ValueBase64    string    `json:"value_base64"`
	CreateRevision int64     `json:"create_revision"`
	ModRevision    int64     `json:"mod_revision"`
	KeyVersion     int64     `json:"key_version"`
	Lease          int64     `json:"lease"`
	CapturedAt     time.Time `json:"captured_at"`
}

// FileHistory is an append-only, file-backed ValueHistory implementation.
// Values and keys are base64 encoded so arbitrary etcd bytes round-trip safely.
type FileHistory struct {
	mu        sync.RWMutex
	file      *os.File
	filePath  string
	entries   map[string][]ValueSnapshot
	seen      map[string]struct{}
	closeOnce sync.Once
	closeErr  error
}

func NewFileHistory(filePath, managedRoot string) (*FileHistory, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("history file path cannot be empty")
	}
	file, authorizedPath, err := safefile.OpenFile(managedRoot, filePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open history file: %w", err)
	}
	cleanup := func(err error) (*FileHistory, error) {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		return cleanup(fmt.Errorf("secure history file permissions: %w", err))
	}
	history := &FileHistory{
		file:     file,
		filePath: authorizedPath,
		entries:  make(map[string][]ValueSnapshot),
		seen:     make(map[string]struct{}),
	}
	if err := history.load(); err != nil {
		return cleanup(err)
	}
	return history, nil
}

func (h *FileHistory) Save(snapshot ValueSnapshot) error {
	snapshot = cloneSnapshot(snapshot)
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	if snapshot.CapturedAt.IsZero() {
		snapshot.CapturedAt = time.Now().UTC()
	} else {
		snapshot.CapturedAt = snapshot.CapturedAt.UTC()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return errors.New("history file is closed")
	}
	seenKey := historySeenKey(snapshot)
	if _, exists := h.seen[seenKey]; exists {
		return nil
	}
	record := recordFromSnapshot(snapshot)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode history record: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxHistoryRecordSize {
		return fmt.Errorf("history record exceeds %d bytes", maxHistoryRecordSize)
	}
	originalSize, err := h.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek history file: %w", err)
	}
	written, writeErr := h.file.Write(data)
	if writeErr != nil || written != len(data) {
		h.rollbackAppend(originalSize)
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return fmt.Errorf("append history record: %w", writeErr)
	}
	if err := h.file.Sync(); err != nil {
		h.rollbackAppend(originalSize)
		return fmt.Errorf("sync history record: %w", err)
	}
	h.addLocked(snapshot)
	return nil
}

func (h *FileHistory) LatestBefore(clusterID string, key []byte, modRevision int64) (ValueSnapshot, bool, error) {
	snapshots, err := h.ListBefore(clusterID, key, modRevision, 1)
	if err != nil || len(snapshots) == 0 {
		return ValueSnapshot{}, false, err
	}
	return snapshots[0], true, nil
}

func (h *FileHistory) ListBefore(clusterID string, key []byte, modRevision int64, limit int) ([]ValueSnapshot, error) {
	if strings.TrimSpace(clusterID) == "" || len(key) == 0 || modRevision < 1 {
		return []ValueSnapshot{}, nil
	}
	if limit < 1 {
		return []ValueSnapshot{}, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.file == nil {
		return nil, errors.New("history file is closed")
	}
	snapshots := make([]ValueSnapshot, 0, min(limit, len(h.entries[historyEntryKey(clusterID, key)])))
	for _, snapshot := range h.entries[historyEntryKey(clusterID, key)] {
		if snapshot.Entry.ModRevision >= modRevision {
			continue
		}
		snapshots = append(snapshots, cloneSnapshot(snapshot))
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Entry.ModRevision > snapshots[j].Entry.ModRevision
	})
	if len(snapshots) > limit {
		snapshots = snapshots[:limit]
	}
	return snapshots, nil
}

func (h *FileHistory) PruneKey(clusterID string, key []byte, keep int) error {
	if keep == 0 || strings.TrimSpace(clusterID) == "" || len(key) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return errors.New("history file is closed")
	}
	entryKey := historyEntryKey(clusterID, key)
	if len(h.entries[entryKey]) <= keep {
		return nil
	}
	updated := cloneHistoryEntries(h.entries)
	updated[entryKey] = newestSnapshots(updated[entryKey], keep)
	return h.replaceEntriesLocked(updated)
}

func (h *FileHistory) Prune(keep int) error {
	if keep == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return errors.New("history file is closed")
	}
	updated := cloneHistoryEntries(h.entries)
	changed := false
	for entryKey, snapshots := range updated {
		if len(snapshots) <= keep {
			continue
		}
		updated[entryKey] = newestSnapshots(snapshots, keep)
		changed = true
	}
	if !changed {
		return nil
	}
	return h.replaceEntriesLocked(updated)
}

func (h *FileHistory) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.file != nil {
			h.closeErr = h.file.Close()
			h.file = nil
		}
	})
	return h.closeErr
}

func (h *FileHistory) load() error {
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek history file: %w", err)
	}
	reader := bufio.NewReader(h.file)
	var completeBytes int64
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(line) > 0 {
				if truncateErr := h.file.Truncate(completeBytes); truncateErr != nil {
					return fmt.Errorf("discard partial history record: %w", truncateErr)
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read history record %d: %w", lineNumber, err)
		}
		completeBytes += int64(len(line))
		if len(line) > maxHistoryRecordSize {
			return fmt.Errorf("history record %d exceeds %d bytes", lineNumber, maxHistoryRecordSize)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record historyRecord
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return fmt.Errorf("decode history record %d: %w", lineNumber, err)
		}
		snapshot, err := record.toSnapshot()
		if err != nil {
			return fmt.Errorf("validate history record %d: %w", lineNumber, err)
		}
		h.addLocked(snapshot)
	}
	if _, err := h.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek history append position: %w", err)
	}
	return nil
}

func (h *FileHistory) addLocked(snapshot ValueSnapshot) {
	seenKey := historySeenKey(snapshot)
	if _, exists := h.seen[seenKey]; exists {
		return
	}
	entryKey := historyEntryKey(snapshot.ClusterID, snapshot.Entry.Key)
	h.entries[entryKey] = append(h.entries[entryKey], cloneSnapshot(snapshot))
	h.seen[seenKey] = struct{}{}
}

func (h *FileHistory) replaceEntriesLocked(updated map[string][]ValueSnapshot) error {
	filePath := h.filePath
	directory := filepath.Dir(filePath)
	temporary, err := os.CreateTemp(directory, ".history-prune-*.tmp")
	if err != nil {
		return fmt.Errorf("create pruned history file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure pruned history file: %w", err)
	}
	writer := bufio.NewWriter(temporary)
	entryKeys := make([]string, 0, len(updated))
	for entryKey := range updated {
		entryKeys = append(entryKeys, entryKey)
	}
	sort.Strings(entryKeys)
	for _, entryKey := range entryKeys {
		for _, snapshot := range updated[entryKey] {
			data, err := json.Marshal(recordFromSnapshot(snapshot))
			if err != nil {
				cleanup()
				return fmt.Errorf("encode pruned history record: %w", err)
			}
			data = append(data, '\n')
			if len(data) > maxHistoryRecordSize {
				cleanup()
				return fmt.Errorf("pruned history record exceeds %d bytes", maxHistoryRecordSize)
			}
			if _, err := writer.Write(data); err != nil {
				cleanup()
				return fmt.Errorf("write pruned history record: %w", err)
			}
		}
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("flush pruned history file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync pruned history file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close pruned history file: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		cleanup()
		return fmt.Errorf("replace pruned history file: %w", err)
	}
	replacement, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen pruned history file: %w", err)
	}
	oldFile := h.file
	h.file = replacement
	h.entries = updated
	h.seen = make(map[string]struct{})
	for _, snapshots := range h.entries {
		for _, snapshot := range snapshots {
			h.seen[historySeenKey(snapshot)] = struct{}{}
		}
	}
	if err := oldFile.Close(); err != nil {
		return fmt.Errorf("close replaced history file: %w", err)
	}
	return nil
}

func cloneHistoryEntries(entries map[string][]ValueSnapshot) map[string][]ValueSnapshot {
	cloned := make(map[string][]ValueSnapshot, len(entries))
	for entryKey, snapshots := range entries {
		cloned[entryKey] = append([]ValueSnapshot(nil), snapshots...)
	}
	return cloned
}

func newestSnapshots(snapshots []ValueSnapshot, keep int) []ValueSnapshot {
	newest := append([]ValueSnapshot(nil), snapshots...)
	sort.Slice(newest, func(i, j int) bool {
		return newest[i].Entry.ModRevision > newest[j].Entry.ModRevision
	})
	return newest[:keep]
}

func (h *FileHistory) rollbackAppend(size int64) {
	_ = h.file.Truncate(size)
	_ = h.file.Sync()
}

func recordFromSnapshot(snapshot ValueSnapshot) historyRecord {
	return historyRecord{
		FormatVersion:  historyRecordVersion,
		ClusterID:      snapshot.ClusterID,
		KeyBase64:      base64.StdEncoding.EncodeToString(snapshot.Entry.Key),
		ValueBase64:    base64.StdEncoding.EncodeToString(snapshot.Entry.Value),
		CreateRevision: snapshot.Entry.CreateRevision,
		ModRevision:    snapshot.Entry.ModRevision,
		KeyVersion:     snapshot.Entry.Version,
		Lease:          snapshot.Entry.Lease,
		CapturedAt:     snapshot.CapturedAt,
	}
}

func (r historyRecord) toSnapshot() (ValueSnapshot, error) {
	if r.FormatVersion != historyRecordVersion {
		return ValueSnapshot{}, fmt.Errorf("unsupported format version %d", r.FormatVersion)
	}
	key, err := base64.StdEncoding.DecodeString(r.KeyBase64)
	if err != nil {
		return ValueSnapshot{}, fmt.Errorf("decode key: %w", err)
	}
	value, err := base64.StdEncoding.DecodeString(r.ValueBase64)
	if err != nil {
		return ValueSnapshot{}, fmt.Errorf("decode value: %w", err)
	}
	snapshot := ValueSnapshot{
		ClusterID: r.ClusterID,
		Entry: Entry{
			Key:            key,
			Value:          value,
			CreateRevision: r.CreateRevision,
			ModRevision:    r.ModRevision,
			Version:        r.KeyVersion,
			Lease:          r.Lease,
		},
		CapturedAt: r.CapturedAt,
	}
	if err := validateSnapshot(snapshot); err != nil {
		return ValueSnapshot{}, err
	}
	return snapshot, nil
}

func validateSnapshot(snapshot ValueSnapshot) error {
	if strings.TrimSpace(snapshot.ClusterID) == "" {
		return errors.New("snapshot cluster id cannot be empty")
	}
	if len(snapshot.Entry.Key) == 0 {
		return errors.New("snapshot key cannot be empty")
	}
	if snapshot.Entry.ModRevision < 1 {
		return errors.New("snapshot modification revision must be positive")
	}
	return nil
}

func historyEntryKey(clusterID string, key []byte) string {
	return clusterID + "\x00" + base64.RawStdEncoding.EncodeToString(key)
}

func historySeenKey(snapshot ValueSnapshot) string {
	return historyEntryKey(snapshot.ClusterID, snapshot.Entry.Key) + "\x00" + strconv.FormatInt(snapshot.Entry.ModRevision, 10)
}

func cloneSnapshot(snapshot ValueSnapshot) ValueSnapshot {
	snapshot.Entry.Key = append([]byte(nil), snapshot.Entry.Key...)
	snapshot.Entry.Value = append([]byte(nil), snapshot.Entry.Value...)
	return snapshot
}
