package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gemini-fly/etcd-studio/internal/safefile"
)

const (
	auditRecordVersion = 1
	maxAuditRecordSize = 16 << 10
)

type auditRecord struct {
	FormatVersion int `json:"format_version"`
	AuditEvent
}

// FileAudit stores audit events in a dedicated append-only JSONL file.
type FileAudit struct {
	mu        sync.RWMutex
	file      *os.File
	filePath  string
	events    []AuditEvent
	closeOnce sync.Once
	closeErr  error
}

func NewFileAudit(filePath string, managedRoots ...string) (*FileAudit, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("audit file path cannot be empty")
	}
	managedRoot := filepath.Dir(filePath)
	if len(managedRoots) > 0 {
		managedRoot = managedRoots[0]
	}
	file, authorizedPath, err := safefile.OpenFile(managedRoot, filePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	cleanup := func(err error) (*FileAudit, error) {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		return cleanup(fmt.Errorf("secure audit file permissions: %w", err))
	}
	audit := &FileAudit{file: file, filePath: authorizedPath, events: make([]AuditEvent, 0)}
	if err := audit.load(); err != nil {
		return cleanup(err)
	}
	return audit, nil
}

func (a *FileAudit) SaveAudit(event AuditEvent) error {
	if err := validateAuditEvent(event); err != nil {
		return err
	}
	event.OccurredAt = event.OccurredAt.UTC()
	data, err := json.Marshal(auditRecord{FormatVersion: auditRecordVersion, AuditEvent: event})
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxAuditRecordSize {
		return fmt.Errorf("audit record exceeds %d bytes", maxAuditRecordSize)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return errors.New("audit file is closed")
	}
	originalSize, err := a.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek audit file: %w", err)
	}
	written, writeErr := a.file.Write(data)
	if writeErr != nil || written != len(data) {
		a.rollbackAppend(originalSize)
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return fmt.Errorf("append audit record: %w", writeErr)
	}
	if err := a.file.Sync(); err != nil {
		a.rollbackAppend(originalSize)
		return fmt.Errorf("sync audit record: %w", err)
	}
	a.events = append(a.events, event)
	return nil
}

func (a *FileAudit) ListAudit(query AuditQuery) (AuditPage, error) {
	if query.Limit < 1 {
		return AuditPage{Events: []AuditEvent{}}, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.file == nil {
		return AuditPage{}, errors.New("audit file is closed")
	}

	events := make([]AuditEvent, 0, min(query.Limit+1, len(a.events)))
	for index := len(a.events) - 1; index >= 0; index-- {
		event := a.events[index]
		if !auditMatches(event, query) {
			continue
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].ID > events[j].ID
		}
		return events[i].OccurredAt.After(events[j].OccurredAt)
	})
	hasMore := len(events) > query.Limit
	if hasMore {
		events = events[:query.Limit]
	}
	return AuditPage{Events: events, HasMore: hasMore}, nil
}

func (a *FileAudit) PruneAudit(before time.Time) error {
	if before.IsZero() {
		return nil
	}
	before = before.UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return errors.New("audit file is closed")
	}
	kept := make([]AuditEvent, 0, len(a.events))
	for _, event := range a.events {
		if !event.OccurredAt.Before(before) {
			kept = append(kept, event)
		}
	}
	if len(kept) == len(a.events) {
		return nil
	}
	return a.replaceLocked(kept)
}

func (a *FileAudit) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.file != nil {
			a.closeErr = a.file.Close()
			a.file = nil
		}
	})
	return a.closeErr
}

func (a *FileAudit) load() error {
	if _, err := a.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek audit file: %w", err)
	}
	reader := bufio.NewReader(a.file)
	var completeBytes int64
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(line) > 0 {
				if truncateErr := a.file.Truncate(completeBytes); truncateErr != nil {
					return fmt.Errorf("discard partial audit record: %w", truncateErr)
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read audit record %d: %w", lineNumber, err)
		}
		completeBytes += int64(len(line))
		if len(line) > maxAuditRecordSize {
			return fmt.Errorf("audit record %d exceeds %d bytes", lineNumber, maxAuditRecordSize)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record auditRecord
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return fmt.Errorf("decode audit record %d: %w", lineNumber, err)
		}
		if record.FormatVersion != auditRecordVersion {
			return fmt.Errorf("unsupported audit record version %d", record.FormatVersion)
		}
		if err := validateAuditEvent(record.AuditEvent); err != nil {
			return fmt.Errorf("validate audit record %d: %w", lineNumber, err)
		}
		record.OccurredAt = record.OccurredAt.UTC()
		a.events = append(a.events, record.AuditEvent)
	}
	if _, err := a.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek audit append position: %w", err)
	}
	return nil
}

func (a *FileAudit) replaceLocked(events []AuditEvent) error {
	filePath := a.filePath
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".audit-prune-*.tmp")
	if err != nil {
		return fmt.Errorf("create pruned audit file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure pruned audit file: %w", err)
	}
	writer := bufio.NewWriter(temporary)
	for _, event := range events {
		data, err := json.Marshal(auditRecord{FormatVersion: auditRecordVersion, AuditEvent: event})
		if err != nil {
			cleanup()
			return fmt.Errorf("encode pruned audit record: %w", err)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			cleanup()
			return fmt.Errorf("write pruned audit record: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("flush pruned audit file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync pruned audit file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close pruned audit file: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		cleanup()
		return fmt.Errorf("replace pruned audit file: %w", err)
	}
	replacement, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen pruned audit file: %w", err)
	}
	oldFile := a.file
	a.file = replacement
	a.events = append([]AuditEvent(nil), events...)
	if err := oldFile.Close(); err != nil {
		return fmt.Errorf("close replaced audit file: %w", err)
	}
	return nil
}

func (a *FileAudit) rollbackAppend(size int64) {
	_ = a.file.Truncate(size)
	_ = a.file.Sync()
}

func auditMatches(event AuditEvent, query AuditQuery) bool {
	if !query.Since.IsZero() && event.OccurredAt.Before(query.Since) {
		return false
	}
	if !query.Until.IsZero() && !event.OccurredAt.Before(query.Until) {
		return false
	}
	if !query.Before.IsZero() {
		if event.OccurredAt.After(query.Before) {
			return false
		}
		if event.OccurredAt.Equal(query.Before) && (query.BeforeID == "" || event.ID >= query.BeforeID) {
			return false
		}
	}
	if query.ClusterID != "" && event.ClusterID != query.ClusterID {
		return false
	}
	if query.Action != "" && event.Action != query.Action {
		return false
	}
	search := strings.ToLower(strings.TrimSpace(query.Search))
	if search == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{event.Actor, event.Target, event.ClusterName, event.Detail}, "\n"))
	return strings.Contains(haystack, search)
}

func validateAuditEvent(event AuditEvent) error {
	if len(event.ID) != 32 || event.OccurredAt.IsZero() || strings.TrimSpace(event.Actor) == "" ||
		strings.TrimSpace(event.Action) == "" || strings.TrimSpace(event.ResourceType) == "" ||
		strings.TrimSpace(event.Target) == "" || strings.TrimSpace(event.Result) == "" {
		return errors.New("audit event is incomplete")
	}
	if len(event.Actor) > 200 || len(event.ActorType) > 40 || len(event.ClientIP) > 80 ||
		len(event.Action) > 80 || len(event.ResourceType) > 40 || len(event.ClusterID) > 64 ||
		len(event.ClusterName) > 200 || len(event.Target) > 2048 || len(event.Detail) > 2048 || len(event.Result) > 32 {
		return errors.New("audit event field exceeds size limit")
	}
	return nil
}
