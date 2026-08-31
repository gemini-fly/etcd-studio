package store

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

type localHistoryBackend struct {
	*FileHistory
	audit *FileAudit
}

func newLocalHistoryBackend(historyFile, managedRoot string) (*localHistoryBackend, error) {
	history, err := NewFileHistory(historyFile, managedRoot)
	if err != nil {
		return nil, err
	}
	audit, err := NewFileAudit(auditFilePath(historyFile), managedRoot)
	if err != nil {
		_ = history.Close()
		return nil, err
	}
	return &localHistoryBackend{FileHistory: history, audit: audit}, nil
}

func (b *localHistoryBackend) SaveAudit(event AuditEvent) error {
	return b.audit.SaveAudit(event)
}

func (b *localHistoryBackend) ListAudit(query AuditQuery) (AuditPage, error) {
	return b.audit.ListAudit(query)
}

func (b *localHistoryBackend) PruneAudit(before time.Time) error {
	return b.audit.PruneAudit(before)
}

func (b *localHistoryBackend) Close() error {
	return errors.Join(b.FileHistory.Close(), b.audit.Close())
}

func auditFilePath(historyFile string) string {
	extension := filepath.Ext(historyFile)
	base := strings.TrimSuffix(historyFile, extension)
	return base + ".audit.jsonl"
}
