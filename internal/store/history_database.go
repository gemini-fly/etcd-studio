package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// DatabaseHistory persists shared history in PostgreSQL or MySQL.
type DatabaseHistory struct {
	db      *sql.DB
	dialect string
	timeout time.Duration
}

func NewDatabaseHistory(ctx context.Context, config HistoryStorageInput, connectTimeout time.Duration, initialize bool) (*DatabaseHistory, error) {
	driverName, dataSource, err := databaseConnection(config, connectTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, dataSource)
	if err != nil {
		return nil, fmt.Errorf("open %s history database: %w", config.Type, err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to %s history database: %w", config.Type, err)
	}
	history := &DatabaseHistory{db: db, dialect: config.Type, timeout: connectTimeout}
	if initialize {
		if err := history.ensureSchema(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return history, nil
}

func (h *DatabaseHistory) Save(snapshot ValueSnapshot) error {
	snapshot = cloneSnapshot(snapshot)
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	if snapshot.CapturedAt.IsZero() {
		snapshot.CapturedAt = time.Now().UTC()
	} else {
		snapshot.CapturedAt = snapshot.CapturedAt.UTC()
	}
	hash := sha256.Sum256(snapshot.Entry.Key)
	var query string
	if h.dialect == HistoryStoragePostgres {
		query = `INSERT INTO etcd_studio_value_history
            (cluster_id, key_hash, key_data, value_data, create_revision, mod_revision, key_version, lease, captured_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
            ON CONFLICT (cluster_id, key_hash, mod_revision) DO NOTHING`
	} else {
		query = `INSERT INTO etcd_studio_value_history
            (cluster_id, key_hash, key_data, value_data, create_revision, mod_revision, key_version, lease, captured_at)
            VALUES (?,?,?,?,?,?,?,?,?)
            ON DUPLICATE KEY UPDATE mod_revision = mod_revision`
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	_, err := h.db.ExecContext(ctx, query,
		snapshot.ClusterID, hash[:], snapshot.Entry.Key, snapshot.Entry.Value,
		snapshot.Entry.CreateRevision, snapshot.Entry.ModRevision, snapshot.Entry.Version,
		snapshot.Entry.Lease, snapshot.CapturedAt,
	)
	if err != nil {
		return fmt.Errorf("insert value history: %w", err)
	}
	return nil
}

func (h *DatabaseHistory) SaveAudit(event AuditEvent) error {
	if err := validateAuditEvent(event); err != nil {
		return err
	}
	event.OccurredAt = event.OccurredAt.UTC()
	query := `INSERT INTO etcd_studio_audit_log
		(id, occurred_at, actor, actor_type, client_ip, action, resource_type, cluster_id, cluster_name, target, detail, result)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	if h.dialect == HistoryStorageMySQL {
		query = `INSERT INTO etcd_studio_audit_log
			(id, occurred_at, actor, actor_type, client_ip, action, resource_type, cluster_id, cluster_name, target, detail, result)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	if _, err := h.db.ExecContext(ctx, query,
		event.ID, event.OccurredAt, event.Actor, event.ActorType, event.ClientIP,
		event.Action, event.ResourceType, event.ClusterID, event.ClusterName,
		event.Target, event.Detail, event.Result,
	); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func (h *DatabaseHistory) ListAudit(query AuditQuery) (AuditPage, error) {
	if query.Limit < 1 {
		return AuditPage{Events: []AuditEvent{}}, nil
	}
	statement := `SELECT id, occurred_at, actor, actor_type, client_ip, action, resource_type,
		cluster_id, cluster_name, target, detail, result FROM etcd_studio_audit_log`
	conditions := make([]string, 0, 7)
	arguments := make([]any, 0, 10)
	bind := func(value any) string {
		arguments = append(arguments, value)
		if h.dialect == HistoryStoragePostgres {
			return fmt.Sprintf("$%d", len(arguments))
		}
		return "?"
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "occurred_at >= "+bind(query.Since.UTC()))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "occurred_at < "+bind(query.Until.UTC()))
	}
	if query.ClusterID != "" {
		conditions = append(conditions, "cluster_id = "+bind(query.ClusterID))
	}
	if query.Actor != "" {
		conditions = append(conditions, "actor = "+bind(query.Actor))
	}
	if query.ActorType != "" {
		conditions = append(conditions, "actor_type = "+bind(query.ActorType))
	}
	if query.Action != "" {
		conditions = append(conditions, "action = "+bind(query.Action))
	}
	if search := strings.ToLower(strings.TrimSpace(query.Search)); search != "" {
		pattern := "%" + search + "%"
		conditions = append(conditions, "(LOWER(actor) LIKE "+bind(pattern)+" OR LOWER(target) LIKE "+bind(pattern)+" OR LOWER(cluster_name) LIKE "+bind(pattern)+" OR LOWER(detail) LIKE "+bind(pattern)+")")
	}
	if !query.Before.IsZero() {
		beforeTime := bind(query.Before.UTC())
		beforeTimeEqual := bind(query.Before.UTC())
		beforeID := bind(query.BeforeID)
		conditions = append(conditions, "(occurred_at < "+beforeTime+" OR (occurred_at = "+beforeTimeEqual+" AND id < "+beforeID+"))")
	}
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	statement += " ORDER BY occurred_at DESC, id DESC LIMIT " + bind(query.Limit+1)

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	rows, err := h.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return AuditPage{}, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	events := make([]AuditEvent, 0, query.Limit+1)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(
			&event.ID, &event.OccurredAt, &event.Actor, &event.ActorType, &event.ClientIP,
			&event.Action, &event.ResourceType, &event.ClusterID, &event.ClusterName,
			&event.Target, &event.Detail, &event.Result,
		); err != nil {
			return AuditPage{}, fmt.Errorf("scan audit event: %w", err)
		}
		event.OccurredAt = event.OccurredAt.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, fmt.Errorf("read audit events: %w", err)
	}
	hasMore := len(events) > query.Limit
	if hasMore {
		events = events[:query.Limit]
	}
	return AuditPage{Events: events, HasMore: hasMore}, nil
}

func (h *DatabaseHistory) PruneAudit(before time.Time) error {
	if before.IsZero() {
		return nil
	}
	query := `DELETE FROM etcd_studio_audit_log WHERE occurred_at < $1`
	if h.dialect == HistoryStorageMySQL {
		query = `DELETE FROM etcd_studio_audit_log WHERE occurred_at < ?`
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	if _, err := h.db.ExecContext(ctx, query, before.UTC()); err != nil {
		return fmt.Errorf("delete expired audit events: %w", err)
	}
	return nil
}

func (h *DatabaseHistory) LatestBefore(clusterID string, key []byte, modRevision int64) (ValueSnapshot, bool, error) {
	snapshots, err := h.ListBefore(clusterID, key, modRevision, 1)
	if err != nil || len(snapshots) == 0 {
		return ValueSnapshot{}, false, err
	}
	return snapshots[0], true, nil
}

func (h *DatabaseHistory) ListBefore(clusterID string, key []byte, modRevision int64, limit int) ([]ValueSnapshot, error) {
	if clusterID == "" || len(key) == 0 || modRevision < 1 {
		return []ValueSnapshot{}, nil
	}
	if limit < 1 {
		return []ValueSnapshot{}, nil
	}
	hash := sha256.Sum256(key)
	var query string
	if h.dialect == HistoryStoragePostgres {
		query = `SELECT key_data, value_data, create_revision, mod_revision, key_version, lease, captured_at
            FROM etcd_studio_value_history
            WHERE cluster_id=$1 AND key_hash=$2 AND key_data=$3 AND mod_revision<$4
			ORDER BY mod_revision DESC LIMIT $5`
	} else {
		query = `SELECT key_data, value_data, create_revision, mod_revision, key_version, lease, captured_at
            FROM etcd_studio_value_history
            WHERE cluster_id=? AND key_hash=? AND key_data=? AND mod_revision<?
			ORDER BY mod_revision DESC LIMIT ?`
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	rows, err := h.db.QueryContext(ctx, query, clusterID, hash[:], key, modRevision, limit)
	if err != nil {
		return nil, fmt.Errorf("query value history: %w", err)
	}
	defer rows.Close()
	snapshots := make([]ValueSnapshot, 0, limit)
	for rows.Next() {
		snapshot := ValueSnapshot{ClusterID: clusterID}
		if err := rows.Scan(
			&snapshot.Entry.Key, &snapshot.Entry.Value, &snapshot.Entry.CreateRevision,
			&snapshot.Entry.ModRevision, &snapshot.Entry.Version, &snapshot.Entry.Lease,
			&snapshot.CapturedAt,
		); err != nil {
			return nil, fmt.Errorf("scan value history: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read value history: %w", err)
	}
	return snapshots, nil
}

func (h *DatabaseHistory) PruneKey(clusterID string, key []byte, keep int) error {
	if keep == 0 || clusterID == "" || len(key) == 0 {
		return nil
	}
	if keep < 0 {
		return errors.New("history retention cannot be negative")
	}
	hash := sha256.Sum256(key)
	var cutoffQuery, deleteQuery string
	if h.dialect == HistoryStoragePostgres {
		cutoffQuery = `SELECT mod_revision FROM etcd_studio_value_history
			WHERE cluster_id=$1 AND key_hash=$2 AND key_data=$3
			ORDER BY mod_revision DESC OFFSET $4 LIMIT 1`
		deleteQuery = `DELETE FROM etcd_studio_value_history
			WHERE cluster_id=$1 AND key_hash=$2 AND key_data=$3 AND mod_revision<$4`
	} else {
		cutoffQuery = `SELECT mod_revision FROM etcd_studio_value_history
			WHERE cluster_id=? AND key_hash=? AND key_data=?
			ORDER BY mod_revision DESC LIMIT 1 OFFSET ?`
		deleteQuery = `DELETE FROM etcd_studio_value_history
			WHERE cluster_id=? AND key_hash=? AND key_data=? AND mod_revision<?`
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	var cutoff int64
	err := h.db.QueryRowContext(ctx, cutoffQuery, clusterID, hash[:], key, keep-1).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find history retention cutoff: %w", err)
	}
	if _, err := h.db.ExecContext(ctx, deleteQuery, clusterID, hash[:], key, cutoff); err != nil {
		return fmt.Errorf("delete expired value history: %w", err)
	}
	return nil
}

func (h *DatabaseHistory) Prune(keep int) error {
	if keep == 0 {
		return nil
	}
	if keep < 0 {
		return errors.New("history retention cannot be negative")
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT cluster_id, key_data FROM etcd_studio_value_history`)
	if err != nil {
		cancel()
		return fmt.Errorf("list history keys for retention: %w", err)
	}
	type historyKey struct {
		clusterID string
		key       []byte
	}
	keys := make([]historyKey, 0)
	for rows.Next() {
		var item historyKey
		if err := rows.Scan(&item.clusterID, &item.key); err != nil {
			_ = rows.Close()
			cancel()
			return fmt.Errorf("scan history key for retention: %w", err)
		}
		keys = append(keys, item)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	cancel()
	if rowsErr != nil {
		return fmt.Errorf("read history keys for retention: %w", rowsErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close history key query: %w", closeErr)
	}
	for _, item := range keys {
		if err := h.PruneKey(item.clusterID, item.key, keep); err != nil {
			return err
		}
	}
	return nil
}

func (h *DatabaseHistory) Close() error {
	return h.db.Close()
}

func (h *DatabaseHistory) ensureSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS etcd_studio_value_history (
            id BIGSERIAL PRIMARY KEY,
            cluster_id VARCHAR(64) NOT NULL,
            key_hash BYTEA NOT NULL,
            key_data BYTEA NOT NULL,
            value_data BYTEA NOT NULL,
            create_revision BIGINT NOT NULL,
            mod_revision BIGINT NOT NULL,
            key_version BIGINT NOT NULL,
            lease BIGINT NOT NULL,
            captured_at TIMESTAMPTZ NOT NULL,
            UNIQUE (cluster_id, key_hash, mod_revision)
        )`,
		`CREATE INDEX IF NOT EXISTS etcd_studio_history_lookup
	            ON etcd_studio_value_history (cluster_id, key_hash, mod_revision DESC)`,
		`CREATE TABLE IF NOT EXISTS etcd_studio_audit_log (
			id VARCHAR(32) PRIMARY KEY,
			occurred_at TIMESTAMPTZ NOT NULL,
			actor VARCHAR(200) NOT NULL,
			actor_type VARCHAR(40) NOT NULL,
			client_ip VARCHAR(80) NOT NULL,
			action VARCHAR(80) NOT NULL,
			resource_type VARCHAR(40) NOT NULL,
			cluster_id VARCHAR(64) NOT NULL,
			cluster_name VARCHAR(200) NOT NULL,
			target TEXT NOT NULL,
			detail TEXT NOT NULL,
			result VARCHAR(32) NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS etcd_studio_audit_time
			ON etcd_studio_audit_log (occurred_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS etcd_studio_audit_filters
			ON etcd_studio_audit_log (cluster_id, action, occurred_at DESC)`,
	}
	if h.dialect == HistoryStorageMySQL {
		statements = []string{`CREATE TABLE IF NOT EXISTS etcd_studio_value_history (
            id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
            cluster_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
            key_hash BINARY(32) NOT NULL,
            key_data LONGBLOB NOT NULL,
            value_data LONGBLOB NOT NULL,
            create_revision BIGINT NOT NULL,
            mod_revision BIGINT NOT NULL,
            key_version BIGINT NOT NULL,
            lease BIGINT NOT NULL,
            captured_at DATETIME(6) NOT NULL,
            PRIMARY KEY (id),
            UNIQUE KEY etcd_studio_history_unique (cluster_id, key_hash, mod_revision),
            KEY etcd_studio_history_lookup (cluster_id, key_hash, mod_revision)
		        ) ENGINE=InnoDB`,
			`CREATE TABLE IF NOT EXISTS etcd_studio_audit_log (
				id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
				occurred_at DATETIME(6) NOT NULL,
				actor VARCHAR(200) NOT NULL,
				actor_type VARCHAR(40) NOT NULL,
				client_ip VARCHAR(80) NOT NULL,
				action VARCHAR(80) NOT NULL,
				resource_type VARCHAR(40) NOT NULL,
				cluster_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
				cluster_name VARCHAR(200) NOT NULL,
				target TEXT NOT NULL,
				detail TEXT NOT NULL,
				result VARCHAR(32) NOT NULL,
				PRIMARY KEY (id),
				KEY etcd_studio_audit_time (occurred_at DESC, id DESC),
				KEY etcd_studio_audit_filters (cluster_id, action, occurred_at DESC)
			) ENGINE=InnoDB`}
	}
	for _, statement := range statements {
		if _, err := h.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize %s history schema: %w", h.dialect, err)
		}
	}
	return nil
}

func databaseConnection(config HistoryStorageInput, timeout time.Duration) (string, string, error) {
	address := net.JoinHostPort(config.Host, fmt.Sprintf("%d", config.Port))
	if config.Type == HistoryStoragePostgres {
		connectionURL := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(config.Username, config.Password),
			Host:   address,
			Path:   config.Database,
		}
		query := connectionURL.Query()
		query.Set("sslmode", config.TLSMode)
		query.Set("connect_timeout", fmt.Sprintf("%d", max(1, int(timeout.Seconds()))))
		connectionURL.RawQuery = query.Encode()
		return "pgx", connectionURL.String(), nil
	}
	if config.Type == HistoryStorageMySQL {
		mysqlConfig := mysqlDriver.NewConfig()
		mysqlConfig.User = config.Username
		mysqlConfig.Passwd = config.Password
		mysqlConfig.Net = "tcp"
		mysqlConfig.Addr = address
		mysqlConfig.DBName = config.Database
		mysqlConfig.ParseTime = true
		mysqlConfig.Loc = time.UTC
		mysqlConfig.Timeout = timeout
		mysqlConfig.ReadTimeout = timeout
		mysqlConfig.WriteTimeout = timeout
		switch config.TLSMode {
		case "require":
			mysqlConfig.TLSConfig = "true"
		case "skip-verify":
			mysqlConfig.TLSConfig = "skip-verify"
		case "preferred":
			mysqlConfig.TLSConfig = "preferred"
		}
		return "mysql", mysqlConfig.FormatDSN(), nil
	}
	return "", "", fmt.Errorf("%w: unsupported database type %q", ErrHistorySetup, config.Type)
}
