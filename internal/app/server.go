package app

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gemini-fly/etcd-studio/internal/auth"
	"github.com/gemini-fly/etcd-studio/internal/store"
)

const (
	defaultPageSize  = 50
	maxPageSize      = 100
	rollbackPageSize = 25
	maxRollbackPage  = 100
	auditPageSize    = 50
	maxAuditPageSize = 100
	maxRequestBytes  = 2 << 20
	operationTimeout = 8 * time.Second
)

//go:embed web/*
var webAssets embed.FS

// Server exposes the JSON API and embedded browser application.
type Server struct {
	clusters     store.ClusterRegistry
	history      store.HistoryStorage
	auth         *auth.Manager
	logger       *slog.Logger
	auditMu      sync.Mutex
	pendingAudit []store.AuditEvent
}

func NewServer(clusters store.ClusterRegistry, history store.HistoryStorage, logger *slog.Logger, authentication ...*auth.Manager) *Server {
	if history == nil {
		panic("value history is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var authManager *auth.Manager
	if len(authentication) > 0 {
		authManager = authentication[0]
	}
	return &Server{clusters: clusters, history: history, auth: authManager, logger: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/v1/auth/change-password", s.handleChangePassword)
	mux.HandleFunc("GET /api/v1/auth/settings", s.handleAuthSettings)
	mux.HandleFunc("PUT /api/v1/auth/settings", s.handleUpdateAuthSettings)
	mux.HandleFunc("POST /api/v1/auth/ldap/test", s.handleTestLDAP)
	mux.HandleFunc("GET /api/v1/users", s.handleListUsers)
	mux.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	mux.HandleFunc("PUT /api/v1/users/{username}", s.handleUpdateUser)
	mux.HandleFunc("DELETE /api/v1/users/{username}", s.handleDeleteUser)
	mux.HandleFunc("GET /api/v1/clusters", s.handleListClusters)
	mux.HandleFunc("POST /api/v1/clusters", s.handleCreateCluster)
	mux.HandleFunc("PUT /api/v1/clusters/{id}", s.handleUpdateCluster)
	mux.HandleFunc("DELETE /api/v1/clusters/{id}", s.handleDeleteCluster)
	mux.HandleFunc("POST /api/v1/clusters/test", s.handleTestCluster)
	mux.HandleFunc("GET /api/v1/history-storage", s.handleHistoryStorageStatus)
	mux.HandleFunc("POST /api/v1/history-storage/test", s.handleTestHistoryStorage)
	mux.HandleFunc("PUT /api/v1/history-storage", s.handleConfigureHistoryStorage)
	mux.HandleFunc("PATCH /api/v1/history-storage/retention", s.handleUpdateHistoryRetention)
	mux.HandleFunc("GET /api/v1/audit", s.handleListAudit)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/keys", s.handleListKeys)
	mux.HandleFunc("GET /api/v1/key", s.handleGetKey)
	mux.HandleFunc("PUT /api/v1/key", s.handlePutKey)
	mux.HandleFunc("DELETE /api/v1/key", s.handleDeleteKey)
	mux.HandleFunc("GET /api/v1/key/rollback-versions", s.handleRollbackVersions)
	mux.HandleFunc("GET /api/v1/key/rollback-preview", s.handleRollbackPreview)
	mux.HandleFunc("POST /api/v1/key/rollback", s.handleRollbackKey)

	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(fmt.Sprintf("load embedded web assets: %v", err))
	}
	fileServer := http.FileServer(http.FS(assets))
	mux.Handle("GET /", fileServer)

	return s.securityHeaders(s.logRequests(s.requireAuthentication(mux)))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	clusterID := strings.TrimSpace(r.URL.Query().Get("cluster_id"))
	if clusterID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"connected": false,
			"message":   "请先选择或配置一个 etcd 集群",
		})
		return
	}
	if !s.requireClusterAccess(w, r, clusterID) {
		return
	}
	kv, cluster, err := s.clusters.ClusterKV(clusterID)
	if err != nil {
		s.writeClusterResolveError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	endpoint := endpointLabel(cluster)
	if !s.requestIsAdmin(r) {
		endpoint = cluster.Name
	}
	memberStatuses := kv.MemberStatuses(ctx, cluster.Endpoints)
	connection := summarizeMemberStatuses(memberStatuses)
	maintenanceUnavailable := false
	if connection.Reachable == 0 {
		if err := kv.Health(ctx); err == nil {
			maintenanceUnavailable = true
		} else {
			s.logger.Warn("etcd health check failed", "cluster_id", clusterID, "error", err)
		}
	}
	if connection.Reachable == 0 && !maintenanceUnavailable {
		writeJSON(w, http.StatusOK, map[string]any{
			"connected":            false,
			"endpoint":             endpoint,
			"member_count":         len(memberStatuses),
			"healthy_member_count": connection.Healthy,
			"message":              "无法连接到 etcd，请检查服务地址和认证配置",
		})
		return
	}
	leader := ""
	if connection.LeaderIndex >= 0 {
		if s.requestIsAdmin(r) {
			leader = memberStatuses[connection.LeaderIndex].Endpoint
		} else {
			leader = fmt.Sprintf("节点 %d", connection.LeaderIndex+1)
		}
	}
	degraded := connection.Reachable > 0 && connection.Healthy < len(memberStatuses)
	message := fmt.Sprintf("%d 个节点连接正常", connection.Healthy)
	if maintenanceUnavailable {
		message = "连接正常，但当前无法读取节点状态"
	} else if degraded {
		message = fmt.Sprintf("%d/%d 个节点正常，请检查异常节点", connection.Healthy, len(memberStatuses))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":            true,
		"degraded":             degraded,
		"endpoint":             endpoint,
		"leader":               leader,
		"member_count":         len(memberStatuses),
		"healthy_member_count": connection.Healthy,
		"message":              message,
	})
}

type memberConnectionSummary struct {
	Reachable   int
	Healthy     int
	LeaderIndex int
}

func summarizeMemberStatuses(statuses []store.MemberStatus) memberConnectionSummary {
	summary := memberConnectionSummary{LeaderIndex: -1}
	var leaderID uint64
	for _, member := range statuses {
		if member.Reachable {
			summary.Reachable++
		}
		if member.Healthy {
			summary.Healthy++
		}
		if leaderID == 0 && member.LeaderID != 0 {
			leaderID = member.LeaderID
		}
	}
	if leaderID == 0 {
		return summary
	}
	for index, member := range statuses {
		if member.Reachable && member.MemberID == leaderID {
			summary.LeaderIndex = index
			break
		}
	}
	return summary
}

func (s *Server) handleHistoryStorageStatus(w http.ResponseWriter, r *http.Request) {
	status := s.history.Status()
	if !s.requestIsAdmin(r) {
		writeJSON(w, http.StatusOK, map[string]bool{"configured": status.Configured})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleTestHistoryStorage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request store.HistoryStorageInput
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := s.history.Test(ctx, request); err != nil {
		if errors.Is(err, store.ErrHistorySetup) || errors.Is(err, store.ErrHistoryRetention) {
			writeError(w, http.StatusBadRequest, "invalid_history_storage", historySetupErrorMessage(err))
			return
		}
		s.logger.Warn("history storage connection test failed", "type", request.Type, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"connected": false,
			"message":   "连接失败，请检查地址、端口、数据库、账号、密码和 TLS 配置",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"message":   "连接成功",
	})
}

func (s *Server) handleConfigureHistoryStorage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request store.HistoryStorageInput
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := s.history.Configure(ctx, request); err != nil {
		switch {
		case errors.Is(err, store.ErrHistorySetup), errors.Is(err, store.ErrHistoryRetention):
			writeError(w, http.StatusBadRequest, "invalid_history_storage", historySetupErrorMessage(err))
		case errors.Is(err, store.ErrHistoryConfigured):
			writeError(w, http.StatusConflict, "history_storage_configured", "历史存储已经配置，不能在页面直接切换")
		case errors.Is(err, store.ErrHistoryConfigSave):
			s.logger.Error("save history storage configuration", "error", err)
			writeError(w, http.StatusInternalServerError, "history_storage_config_error", "历史存储配置文件保存失败，请检查数据目录权限")
		default:
			s.logger.Error("configure history storage", "type", request.Type, "error", err)
			writeError(w, http.StatusBadGateway, "history_storage_unavailable", "历史存储初始化失败，请先测试连接并检查建表权限")
		}
		return
	}
	s.flushPendingAudit()
	s.recordAudit(r, store.AuditEvent{
		Action: "history_storage.configure", ResourceType: "system_setting", Target: "历史存储",
		Detail: "初始化为 " + historyStorageTypeLabel(s.history.Status().Type),
	})
	writeJSON(w, http.StatusOK, s.history.Status())
}

func historySetupErrorMessage(err error) string {
	message := strings.TrimPrefix(err.Error(), store.ErrHistorySetup.Error()+": ")
	return strings.TrimPrefix(message, store.ErrHistoryRetention.Error()+": ")
}

func (s *Server) handleUpdateHistoryRetention(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	previous := s.history.Status().RetentionVersions
	var request historyRetentionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := s.history.UpdateRetention(ctx, request.RetentionVersions); err != nil {
		switch {
		case errors.Is(err, store.ErrHistoryRetention):
			writeError(w, http.StatusBadRequest, "invalid_history_retention", strings.TrimPrefix(err.Error(), store.ErrHistoryRetention.Error()+": "))
		case errors.Is(err, store.ErrHistoryNotSetup):
			writeError(w, http.StatusServiceUnavailable, "history_storage_not_configured", "请先完成历史存储配置")
		case errors.Is(err, store.ErrHistoryConfigSave):
			s.logger.Error("save history retention setting", "error", err)
			writeError(w, http.StatusInternalServerError, "history_retention_save_error", "保留策略保存失败，请检查数据目录权限")
		case errors.Is(err, store.ErrLocalHistory):
			s.logger.Error("prune value history", "error", err)
			writeError(w, http.StatusInternalServerError, "history_prune_error", "保留数量已保存，但立即清理失败；服务重启或下次写入时会重试")
		default:
			s.logger.Error("update history retention", "error", err)
			writeError(w, http.StatusInternalServerError, "history_retention_error", "更新历史保留策略失败")
		}
		return
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "history_retention.update", ResourceType: "system_setting", Target: "Value 历史保留策略",
		Detail: fmt.Sprintf("每个 Key 保留版本数：%d → %d", previous, request.RetentionVersions),
	})
	writeJSON(w, http.StatusOK, s.history.Status())
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit, err := parseAuditLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	query := store.AuditQuery{
		Limit: limit, ClusterID: strings.TrimSpace(r.URL.Query().Get("cluster_id")),
		Action: strings.TrimSpace(r.URL.Query().Get("action")), Search: strings.TrimSpace(r.URL.Query().Get("search")),
	}
	if !s.requestIsAdmin(r) {
		if query.ClusterID == "" {
			writeError(w, http.StatusForbidden, "cluster_scope_required", "操作员查看审计日志时必须选择已授权集群")
			return
		}
		if !s.requireClusterAccess(w, r, query.ClusterID) {
			return
		}
	}
	query.Since, query.Until, err = parseAuditTimeRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date_range", err.Error())
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := decodeAuditCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "审计分页位置无效，请重新查询")
			return
		}
		query.Before = cursor.OccurredAt
		query.BeforeID = cursor.ID
	}
	page, err := s.history.ListAudit(query)
	if err != nil {
		if errors.Is(err, store.ErrHistoryNotSetup) {
			writeError(w, http.StatusServiceUnavailable, "history_storage_not_configured", "请先完成历史存储配置")
			return
		}
		s.logger.Error("list audit events", "error", err)
		writeError(w, http.StatusInternalServerError, "audit_log_error", "读取审计记录失败，请检查历史存储连接")
		return
	}
	response := auditListResponse{
		Items: page.Events, PageSize: len(page.Events), RetentionDays: store.DefaultAuditRetentionDays,
	}
	if page.HasMore && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		response.NextCursor = encodeAuditCursor(auditCursor{OccurredAt: last.OccurredAt, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	kv, _, err := s.clusterKVFromRequest(w, r)
	if err != nil {
		return
	}
	prefix := []byte(r.URL.Query().Get("prefix"))
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	var cursor []byte
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "分页游标无效，请重新查询")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	page, err := kv.List(ctx, prefix, cursor, limit)
	if err != nil {
		s.writeStoreError(w, "list keys", err)
		return
	}

	items := make([]entrySummary, 0, len(page.Entries))
	for _, entry := range page.Entries {
		items = append(items, makeEntrySummary(entry))
	}
	response := listResponse{Items: items, PageSize: len(items)}
	if len(page.NextCursor) > 0 {
		response.NextCursor = base64.RawURLEncoding.EncodeToString(page.NextCursor)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	kv, _, err := s.clusterKVFromRequest(w, r)
	if err != nil {
		return
	}
	key, err := keyFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	entry, found, err := kv.Get(ctx, key)
	if err != nil {
		s.writeStoreError(w, "get key", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "key_not_found", "该 Key 已不存在，可能已被其他用户删除")
		return
	}
	writeJSON(w, http.StatusOK, makeEntryDetail(entry))
}

func (s *Server) handlePutKey(w http.ResponseWriter, r *http.Request) {
	var request putRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.requireClusterAccess(w, r, request.ClusterID) {
		return
	}
	kv, cluster, err := s.clusterKV(request.ClusterID)
	if err != nil {
		s.writeClusterResolveError(w, err)
		return
	}
	key, err := decodeKey(request.Key, request.KeyBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	value, err := decodeValue(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if _, _, err := s.archiveCurrent(ctx, cluster.ID, kv, key, request.ExpectedModRevision); err != nil {
		s.writeStoreError(w, "archive value before put", err)
		return
	}
	revision, err := kv.Put(ctx, key, value, request.ExpectedModRevision)
	if err != nil {
		s.writeStoreError(w, "put key", err)
		return
	}
	action := "key.update"
	detail := fmt.Sprintf("保存为 etcd 修订版本 #%d", revision)
	if request.ExpectedModRevision != nil && *request.ExpectedModRevision == 0 {
		action = "key.create"
		detail = fmt.Sprintf("创建为 etcd 修订版本 #%d", revision)
	}
	s.recordAudit(r, store.AuditEvent{
		Action: action, ResourceType: "key", ClusterID: cluster.ID, ClusterName: cluster.Name,
		Target: auditKeyLabel(key), Detail: detail,
	})
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	kv, cluster, err := s.clusterKVFromRequest(w, r)
	if err != nil {
		return
	}
	key, err := keyFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	expected, err := optionalInt64(r.URL.Query().Get("expected_mod_revision"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_revision", "版本号必须是非负整数")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if _, found, err := s.archiveCurrent(ctx, cluster.ID, kv, key, expected); err != nil {
		s.writeStoreError(w, "archive value before delete", err)
		return
	} else if !found {
		writeError(w, http.StatusNotFound, "key_not_found", "该 Key 已不存在")
		return
	}
	revision, deleted, err := kv.Delete(ctx, key, expected)
	if err != nil {
		s.writeStoreError(w, "delete key", err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "key_not_found", "该 Key 已不存在")
		return
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "key.delete", ResourceType: "key", ClusterID: cluster.ID, ClusterName: cluster.Name,
		Target: auditKeyLabel(key), Detail: fmt.Sprintf("删除为 etcd 修订版本 #%d", revision),
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "revision": revision})
}

func (s *Server) handleRollbackPreview(w http.ResponseWriter, r *http.Request) {
	kv, cluster, err := s.clusterKVFromRequest(w, r)
	if err != nil {
		return
	}
	key, err := keyFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	expected, err := optionalInt64(r.URL.Query().Get("expected_mod_revision"))
	if err != nil || expected == nil || *expected < 1 {
		writeError(w, http.StatusBadRequest, "invalid_revision", "回滚预览需要有效的当前修改版本")
		return
	}
	target, err := optionalInt64(r.URL.Query().Get("target_mod_revision"))
	if err != nil || target == nil || *target < 1 || *target >= *expected {
		writeError(w, http.StatusBadRequest, "invalid_target_revision", "请选择一个早于当前版本的有效回滚版本")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	current, previous, source, capturedAt, err := s.rollbackTarget(ctx, cluster.ID, kv, key, *expected, *target)
	if err != nil {
		s.writeStoreError(w, "preview key rollback", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_mod_revision": current.ModRevision,
		"history_source":       source,
		"captured_at":          capturedAt,
		"previous":             makeEntryDetail(previous),
	})
}

func (s *Server) handleRollbackVersions(w http.ResponseWriter, r *http.Request) {
	kv, cluster, err := s.clusterKVFromRequest(w, r)
	if err != nil {
		return
	}
	key, err := keyFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	expected, err := optionalInt64(r.URL.Query().Get("expected_mod_revision"))
	if err != nil || expected == nil || *expected < 1 {
		writeError(w, http.StatusBadRequest, "invalid_revision", "历史版本列表需要有效的当前修改版本")
		return
	}
	before := *expected
	if raw := r.URL.Query().Get("before_mod_revision"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 1 || parsed > *expected {
			writeError(w, http.StatusBadRequest, "invalid_revision", "历史版本分页位置无效")
			return
		}
		before = parsed
	}
	limit, err := parseRollbackLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	current, found, err := kv.Get(ctx, key)
	if err != nil {
		s.writeStoreError(w, "get key for rollback versions", err)
		return
	}
	if !found {
		s.writeStoreError(w, "get key for rollback versions", store.ErrNoPreviousVersion)
		return
	}
	if current.ModRevision != *expected {
		s.writeStoreError(w, "list key rollback versions", store.ErrConflict)
		return
	}
	candidates, hasMore, err := s.rollbackCandidates(ctx, cluster.ID, kv, key, before, limit)
	if err != nil {
		s.writeStoreError(w, "list key rollback versions", err)
		return
	}
	if len(candidates) == 0 {
		s.writeStoreError(w, "list key rollback versions", store.ErrNoPreviousVersion)
		return
	}
	items := make([]rollbackVersionResponse, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, rollbackVersionResponse{
			Entry:         makeEntrySummary(candidate.Entry),
			HistorySource: candidate.Source,
			CapturedAt:    candidate.CapturedAt,
		})
	}
	response := rollbackVersionsResponse{
		CurrentModRevision: current.ModRevision,
		Items:              items,
		PageSize:           len(items),
	}
	if hasMore {
		response.NextBeforeModRevision = candidates[len(candidates)-1].Entry.ModRevision
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRollbackKey(w http.ResponseWriter, r *http.Request) {
	var request rollbackRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.ExpectedModRevision == nil || *request.ExpectedModRevision < 1 {
		writeError(w, http.StatusBadRequest, "invalid_revision", "回滚需要有效的当前修改版本")
		return
	}
	if request.TargetModRevision == nil || *request.TargetModRevision < 1 || *request.TargetModRevision >= *request.ExpectedModRevision {
		writeError(w, http.StatusBadRequest, "invalid_target_revision", "请选择一个早于当前版本的有效回滚版本")
		return
	}
	if !s.requireClusterAccess(w, r, request.ClusterID) {
		return
	}
	kv, cluster, err := s.clusterKV(request.ClusterID)
	if err != nil {
		s.writeClusterResolveError(w, err)
		return
	}
	key, err := decodeKey(request.Key, request.KeyBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	current, previous, source, _, err := s.rollbackTarget(ctx, cluster.ID, kv, key, *request.ExpectedModRevision, *request.TargetModRevision)
	if err != nil {
		s.writeStoreError(w, "prepare key rollback", err)
		return
	}
	if source == "etcd" {
		if err := s.saveSnapshot(cluster.ID, previous); err != nil {
			s.writeStoreError(w, "archive rollback target", err)
			return
		}
	}
	if err := s.saveSnapshot(cluster.ID, current); err != nil {
		s.writeStoreError(w, "archive value before rollback", err)
		return
	}
	revision, err := kv.Put(ctx, key, previous.Value, &current.ModRevision)
	if err != nil {
		s.writeStoreError(w, "rollback key", err)
		return
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "key.rollback", ResourceType: "key", ClusterID: cluster.ID, ClusterName: cluster.Name,
		Target: auditKeyLabel(key),
		Detail: fmt.Sprintf("从修改版本 #%d 回滚，生成 etcd 修订版本 #%d", previous.ModRevision, revision),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":               revision,
		"restored_from_revision": previous.ModRevision,
		"history_source":         source,
	})
}

func (s *Server) rollbackTarget(ctx context.Context, clusterID string, kv store.KV, key []byte, expectedModRevision, targetModRevision int64) (store.Entry, store.Entry, string, time.Time, error) {
	current, found, err := kv.Get(ctx, key)
	if err != nil {
		return store.Entry{}, store.Entry{}, "", time.Time{}, err
	}
	if !found {
		return store.Entry{}, store.Entry{}, "", time.Time{}, store.ErrNoPreviousVersion
	}
	if current.ModRevision != expectedModRevision {
		return store.Entry{}, store.Entry{}, "", time.Time{}, store.ErrConflict
	}
	if targetModRevision < 1 || targetModRevision >= current.ModRevision {
		return store.Entry{}, store.Entry{}, "", time.Time{}, store.ErrNoPreviousVersion
	}
	snapshots, err := s.history.ListBefore(clusterID, key, targetModRevision+1, 1)
	if err != nil {
		return store.Entry{}, store.Entry{}, "", time.Time{}, fmt.Errorf("%w: %w", store.ErrLocalHistory, err)
	}
	if len(snapshots) > 0 && snapshots[0].Entry.ModRevision == targetModRevision {
		return current, snapshots[0].Entry, "storage", snapshots[0].CapturedAt, nil
	}
	previous, etcdFound, err := kv.GetAtRevision(ctx, key, targetModRevision)
	if err != nil {
		return store.Entry{}, store.Entry{}, "", time.Time{}, err
	}
	if etcdFound && previous.ModRevision == targetModRevision {
		return current, previous, "etcd", time.Time{}, nil
	}
	return store.Entry{}, store.Entry{}, "", time.Time{}, store.ErrNoPreviousVersion
}

type rollbackCandidate struct {
	Entry      store.Entry
	Source     string
	CapturedAt time.Time
}

func (s *Server) rollbackCandidates(ctx context.Context, clusterID string, kv store.KV, key []byte, beforeModRevision int64, limit int) ([]rollbackCandidate, bool, error) {
	fetchLimit := limit + 1
	snapshots, err := s.history.ListBefore(clusterID, key, beforeModRevision, fetchLimit)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", store.ErrLocalHistory, err)
	}
	byRevision := make(map[int64]rollbackCandidate, len(snapshots))
	for _, snapshot := range snapshots {
		byRevision[snapshot.Entry.ModRevision] = rollbackCandidate{
			Entry: snapshot.Entry, Source: "storage", CapturedAt: snapshot.CapturedAt,
		}
	}

	etcdEntries, compacted, err := etcdVersionsBefore(ctx, kv, key, beforeModRevision, fetchLimit)
	if err != nil {
		return nil, false, err
	}
	for _, entry := range etcdEntries {
		if _, exists := byRevision[entry.ModRevision]; !exists {
			byRevision[entry.ModRevision] = rollbackCandidate{Entry: entry, Source: "etcd"}
		}
	}
	candidates := make([]rollbackCandidate, 0, len(byRevision))
	for _, candidate := range byRevision {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Entry.ModRevision > candidates[j].Entry.ModRevision
	})
	if len(candidates) == 0 && compacted {
		return nil, false, store.ErrHistoryCompacted
	}
	hasMore := len(candidates) > limit
	if hasMore {
		candidates = candidates[:limit]
	}
	return candidates, hasMore, nil
}

func etcdVersionsBefore(ctx context.Context, kv store.KV, key []byte, beforeModRevision int64, limit int) ([]store.Entry, bool, error) {
	entries := make([]store.Entry, 0, limit)
	revision := beforeModRevision - 1
	for revision > 0 && len(entries) < limit {
		entry, found, err := kv.GetAtRevision(ctx, key, revision)
		if errors.Is(err, store.ErrHistoryCompacted) {
			return entries, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if !found || entry.ModRevision < 1 || entry.ModRevision > revision {
			break
		}
		entries = append(entries, entry)
		revision = entry.ModRevision - 1
	}
	return entries, false, nil
}

func (s *Server) archiveCurrent(ctx context.Context, clusterID string, kv store.KV, key []byte, expectedModRevision *int64) (store.Entry, bool, error) {
	current, found, err := kv.Get(ctx, key)
	if err != nil {
		return store.Entry{}, false, err
	}
	if expectedModRevision != nil {
		if found && current.ModRevision != *expectedModRevision {
			return store.Entry{}, false, store.ErrConflict
		}
	}
	if !found {
		return store.Entry{}, false, nil
	}
	if err := s.saveSnapshot(clusterID, current); err != nil {
		return store.Entry{}, false, err
	}
	return current, true, nil
}

func (s *Server) saveSnapshot(clusterID string, entry store.Entry) error {
	if err := s.history.Save(store.ValueSnapshot{ClusterID: clusterID, Entry: entry, CapturedAt: time.Now().UTC()}); err != nil {
		return fmt.Errorf("%w: %w", store.ErrLocalHistory, err)
	}
	return nil
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	clusters := s.clusters.ListClusters()
	if !s.requestIsAdmin(r) {
		items := make([]clusterSummaryResponse, 0, len(clusters))
		for _, cluster := range clusters {
			if s.requestCanAccessCluster(r, cluster.ID) {
				items = append(items, clusterSummaryResponse{ID: cluster.ID, Name: cluster.Name})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	items := make([]clusterResponse, 0, len(clusters))
	for _, cluster := range clusters {
		items = append(items, makeClusterResponse(cluster))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request clusterRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cluster, err := s.clusters.CreateCluster(request.toInput())
	if err != nil {
		s.writeClusterMutationError(w, "create cluster", err)
		return
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "cluster.create", ResourceType: "cluster", ClusterID: cluster.ID, ClusterName: cluster.Name,
		Target: cluster.Name, Detail: fmt.Sprintf("配置 %d 个 Endpoint", len(cluster.Endpoints)),
	})
	writeJSON(w, http.StatusCreated, makeClusterResponse(cluster))
}

func (s *Server) handleUpdateCluster(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request clusterRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cluster, err := s.clusters.UpdateCluster(r.PathValue("id"), request.toInput())
	if err != nil {
		s.writeClusterMutationError(w, "update cluster", err)
		return
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "cluster.update", ResourceType: "cluster", ClusterID: cluster.ID, ClusterName: cluster.Name,
		Target: cluster.Name, Detail: fmt.Sprintf("更新为 %d 个 Endpoint", len(cluster.Endpoints)),
	})
	writeJSON(w, http.StatusOK, makeClusterResponse(cluster))
}

func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	clusterID := r.PathValue("id")
	cluster := s.clusterConfiguration(clusterID)
	if err := s.clusters.DeleteCluster(clusterID); err != nil {
		s.writeClusterMutationError(w, "delete cluster", err)
		return
	}
	if s.auth != nil {
		if err := s.auth.RemoveClusterPermissions(clusterID); err != nil {
			s.logger.Error("remove deleted cluster permissions", "cluster_id", clusterID, "error", err)
		}
	}
	target := cluster.Name
	if target == "" {
		target = clusterID
	}
	s.recordAudit(r, store.AuditEvent{
		Action: "cluster.delete", ResourceType: "cluster", ClusterID: clusterID, ClusterName: cluster.Name,
		Target: target, Detail: "删除 Etcd Studio 集群连接配置",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleTestCluster(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request clusterRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := s.clusters.TestCluster(ctx, request.ID, request.toInput()); err != nil {
		if errors.Is(err, store.ErrInvalidCluster) || errors.Is(err, store.ErrClusterNotFound) {
			s.writeClusterMutationError(w, "test cluster", err)
			return
		}
		s.logger.Warn("etcd cluster connection test failed", "cluster_id", request.ID, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"connected": false,
			"message":   "连接失败，请检查 Endpoint、认证信息和 TLS 文件",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"message":   "连接成功",
	})
}

func (s *Server) clusterKVFromRequest(w http.ResponseWriter, r *http.Request) (store.KV, store.Cluster, error) {
	clusterID := strings.TrimSpace(r.URL.Query().Get("cluster_id"))
	if !s.requireClusterAccess(w, r, clusterID) {
		return nil, store.Cluster{}, errors.New("cluster access denied")
	}
	kv, cluster, err := s.clusterKV(clusterID)
	if err != nil {
		s.writeClusterResolveError(w, err)
	}
	return kv, cluster, err
}

func (s *Server) clusterKV(id string) (store.KV, store.Cluster, error) {
	if strings.TrimSpace(id) == "" {
		return nil, store.Cluster{}, store.ErrClusterNotFound
	}
	return s.clusters.ClusterKV(id)
}

func (s *Server) clusterConfiguration(id string) store.Cluster {
	for _, cluster := range s.clusters.ListClusters() {
		if cluster.ID == id {
			return cluster
		}
	}
	return store.Cluster{}
}

func (s *Server) recordAudit(r *http.Request, event store.AuditEvent) {
	if principal, ok := authPrincipalFromContext(r.Context()); ok {
		s.recordAuditForPrincipal(r, principal, event)
		return
	}
	event.Actor, event.ActorType, event.ClientIP = auditActor(r)
	s.saveAuditEvent(event)
}

func (s *Server) recordAuditForPrincipal(r *http.Request, principal auth.Principal, event store.AuditEvent) {
	_, _, clientIP := auditActor(r)
	event.Actor = principal.Username
	event.ActorType = principal.Provider
	event.ClientIP = clientIP
	s.saveAuditEvent(event)
}

func (s *Server) saveAuditEvent(event store.AuditEvent) {
	if err := s.history.SaveAudit(event); err != nil {
		if errors.Is(err, store.ErrHistoryNotSetup) {
			s.auditMu.Lock()
			if len(s.pendingAudit) >= 100 {
				s.pendingAudit = append(s.pendingAudit[:0], s.pendingAudit[len(s.pendingAudit)-99:]...)
			}
			s.pendingAudit = append(s.pendingAudit, event)
			s.auditMu.Unlock()
			return
		}
		s.logger.Error("save audit event", "action", event.Action, "resource_type", event.ResourceType, "error", err)
	}
}

func (s *Server) flushPendingAudit() {
	s.auditMu.Lock()
	pending := append([]store.AuditEvent(nil), s.pendingAudit...)
	s.pendingAudit = nil
	s.auditMu.Unlock()
	for index, event := range pending {
		if err := s.history.SaveAudit(event); err != nil {
			s.logger.Error("flush pending audit event", "action", event.Action, "resource_type", event.ResourceType, "error", err)
			if errors.Is(err, store.ErrHistoryNotSetup) {
				s.auditMu.Lock()
				s.pendingAudit = append(pending[index:], s.pendingAudit...)
				s.auditMu.Unlock()
				return
			}
		}
	}
}

func auditActor(r *http.Request) (actor, actorType, clientIP string) {
	clientIP = strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	clientIP = sanitizedAuditLabel(clientIP, 80)
	for _, header := range []string{"X-Authenticated-User", "X-Forwarded-User"} {
		if value := sanitizedAuditLabel(r.Header.Get(header), 200); value != "" {
			return value, "authenticated_user", clientIP
		}
	}
	if clientIP == "" {
		return "未知客户端", "unknown", ""
	}
	return clientIP, "client_ip", clientIP
}

func auditKeyLabel(key []byte) string {
	if !utf8.Valid(key) {
		return "base64:" + base64.StdEncoding.EncodeToString(key)
	}
	if label := sanitizedAuditLabel(string(key), 512); label != "" {
		return label
	}
	return "base64:" + base64.StdEncoding.EncodeToString(key)
}

func sanitizedAuditLabel(value string, limit int) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func historyStorageTypeLabel(storageType string) string {
	switch storageType {
	case store.HistoryStoragePostgres:
		return "PostgreSQL"
	case store.HistoryStorageMySQL:
		return "MySQL"
	default:
		return "本地文件"
	}
}

func (s *Server) writeClusterResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, "cluster_not_found", "所选集群不存在，请重新选择")
		return
	}
	s.logger.Error("resolve etcd cluster", "error", err)
	writeError(w, http.StatusBadGateway, "cluster_unavailable", "集群客户端不可用，请检查集群配置")
}

func (s *Server) writeClusterMutationError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidCluster):
		writeError(w, http.StatusBadRequest, "invalid_cluster", strings.TrimPrefix(err.Error(), store.ErrInvalidCluster.Error()+": "))
	case errors.Is(err, store.ErrClusterNotFound):
		writeError(w, http.StatusNotFound, "cluster_not_found", "集群不存在")
	case errors.Is(err, store.ErrClusterNameExists):
		writeError(w, http.StatusConflict, "cluster_name_exists", "集群名称已存在")
	default:
		s.logger.Error("cluster configuration operation failed", "operation", operation, "error", err)
		writeError(w, http.StatusInternalServerError, "cluster_config_error", "集群配置保存失败，请检查服务日志和文件权限")
	}
}

func (s *Server) writeStoreError(w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, store.ErrHistoryNotSetup) {
		writeError(w, http.StatusServiceUnavailable, "history_storage_not_configured", "请先完成历史存储配置，再执行写操作")
		return
	}
	if errors.Is(err, store.ErrLocalHistory) {
		s.logger.Error("value history storage operation failed", "operation", operation, "error", err)
		writeError(w, http.StatusInternalServerError, "local_history_error", "历史存储不可用，本次操作未执行，请检查文件权限或数据库连接")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "revision_conflict", "操作前该 Key 已被其他操作修改，请刷新后重试")
		return
	}
	if errors.Is(err, store.ErrHistoryCompacted) {
		writeError(w, http.StatusConflict, "history_compacted", "该 Key 的 etcd 历史已被压缩，并且独立历史存储中没有可用备份")
		return
	}
	if errors.Is(err, store.ErrNoPreviousVersion) {
		writeError(w, http.StatusNotFound, "rollback_unavailable", "该 Key 没有可回滚的历史版本")
		return
	}
	s.logger.Error("etcd operation failed", "operation", operation, "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "etcd_timeout", "etcd 操作超时")
		return
	}
	writeError(w, http.StatusBadGateway, "etcd_error", "etcd 操作失败，请检查连接和服务日志")
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}

type entrySummary struct {
	Key            string `json:"key"`
	KeyBase64      string `json:"key_base64"`
	KeyIsUTF8      bool   `json:"key_is_utf8"`
	ValuePreview   string `json:"value_preview"`
	ValueIsUTF8    bool   `json:"value_is_utf8"`
	ValueBytes     int    `json:"value_bytes"`
	CreateRevision int64  `json:"create_revision"`
	ModRevision    int64  `json:"mod_revision"`
	Version        int64  `json:"version"`
	Lease          int64  `json:"lease"`
}

type entryDetail struct {
	entrySummary
	Value       string `json:"value"`
	ValueBase64 string `json:"value_base64"`
}

type listResponse struct {
	Items      []entrySummary `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
	PageSize   int            `json:"page_size"`
}

type putRequest struct {
	ClusterID           string `json:"cluster_id"`
	Key                 string `json:"key"`
	KeyBase64           string `json:"key_base64"`
	Value               string `json:"value"`
	ValueBase64         string `json:"value_base64"`
	ValueEncoding       string `json:"value_encoding"`
	ExpectedModRevision *int64 `json:"expected_mod_revision"`
}

type rollbackRequest struct {
	ClusterID           string `json:"cluster_id"`
	Key                 string `json:"key"`
	KeyBase64           string `json:"key_base64"`
	ExpectedModRevision *int64 `json:"expected_mod_revision"`
	TargetModRevision   *int64 `json:"target_mod_revision"`
}

type historyRetentionRequest struct {
	RetentionVersions int `json:"retention_versions"`
}

type auditCursor struct {
	OccurredAt time.Time `json:"occurred_at"`
	ID         string    `json:"id"`
}

type auditListResponse struct {
	Items         []store.AuditEvent `json:"items"`
	NextCursor    string             `json:"next_cursor,omitempty"`
	PageSize      int                `json:"page_size"`
	RetentionDays int                `json:"retention_days"`
}

type rollbackVersionResponse struct {
	Entry         entrySummary `json:"entry"`
	HistorySource string       `json:"history_source"`
	CapturedAt    time.Time    `json:"captured_at,omitempty"`
}

type rollbackVersionsResponse struct {
	CurrentModRevision    int64                     `json:"current_mod_revision"`
	Items                 []rollbackVersionResponse `json:"items"`
	PageSize              int                       `json:"page_size"`
	NextBeforeModRevision int64                     `json:"next_before_mod_revision,omitempty"`
}

type clusterRequest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Endpoints     []string `json:"endpoints"`
	Username      string   `json:"username"`
	Password      *string  `json:"password"`
	ClearPassword bool     `json:"clear_password"`
	TLSCAFile     string   `json:"tls_ca_file"`
	TLSCertFile   string   `json:"tls_cert_file"`
	TLSKeyFile    string   `json:"tls_key_file"`
}

type clusterResponse struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Endpoints          []string  `json:"endpoints"`
	Username           string    `json:"username"`
	PasswordConfigured bool      `json:"password_configured"`
	TLSCAFile          string    `json:"tls_ca_file"`
	TLSCertFile        string    `json:"tls_cert_file"`
	TLSKeyFile         string    `json:"tls_key_file"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type clusterSummaryResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r clusterRequest) toInput() store.ClusterInput {
	password := r.Password
	if r.ClearPassword {
		empty := ""
		password = &empty
	}
	return store.ClusterInput{
		Name:        r.Name,
		Endpoints:   r.Endpoints,
		Username:    r.Username,
		Password:    password,
		TLSCAFile:   r.TLSCAFile,
		TLSCertFile: r.TLSCertFile,
		TLSKeyFile:  r.TLSKeyFile,
	}
}

func makeClusterResponse(cluster store.Cluster) clusterResponse {
	return clusterResponse{
		ID:                 cluster.ID,
		Name:               cluster.Name,
		Endpoints:          append([]string(nil), cluster.Endpoints...),
		Username:           cluster.Username,
		PasswordConfigured: cluster.Password != "",
		TLSCAFile:          cluster.TLSCAFile,
		TLSCertFile:        cluster.TLSCertFile,
		TLSKeyFile:         cluster.TLSKeyFile,
		CreatedAt:          cluster.CreatedAt,
		UpdatedAt:          cluster.UpdatedAt,
	}
}

func endpointLabel(cluster store.Cluster) string {
	if len(cluster.Endpoints) == 1 {
		return cluster.Endpoints[0]
	}
	return strconv.Itoa(len(cluster.Endpoints)) + " 个节点"
}

func makeEntrySummary(entry store.Entry) entrySummary {
	keyIsUTF8 := utf8.Valid(entry.Key)
	valueIsUTF8 := utf8.Valid(entry.Value)
	key := ""
	if keyIsUTF8 {
		key = string(entry.Key)
	}
	preview := ""
	if valueIsUTF8 {
		preview = compactPreview(string(entry.Value), 100)
	}
	return entrySummary{
		Key:            key,
		KeyBase64:      base64.StdEncoding.EncodeToString(entry.Key),
		KeyIsUTF8:      keyIsUTF8,
		ValuePreview:   preview,
		ValueIsUTF8:    valueIsUTF8,
		ValueBytes:     len(entry.Value),
		CreateRevision: entry.CreateRevision,
		ModRevision:    entry.ModRevision,
		Version:        entry.Version,
		Lease:          entry.Lease,
	}
}

func makeEntryDetail(entry store.Entry) entryDetail {
	detail := entryDetail{entrySummary: makeEntrySummary(entry)}
	if detail.ValueIsUTF8 {
		detail.Value = string(entry.Value)
	}
	detail.ValueBase64 = base64.StdEncoding.EncodeToString(entry.Value)
	return detail
}

func compactPreview(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func parseLimit(raw string) (int64, error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit < 1 || limit > maxPageSize {
		return 0, fmt.Errorf("每页数量必须在 1 到 %d 之间", maxPageSize)
	}
	return limit, nil
}

func parseRollbackLimit(raw string) (int, error) {
	if raw == "" {
		return rollbackPageSize, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxRollbackPage {
		return 0, fmt.Errorf("每页历史版本数量必须在 1 到 %d 之间", maxRollbackPage)
	}
	return limit, nil
}

func parseAuditLimit(raw string) (int, error) {
	if raw == "" {
		return auditPageSize, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxAuditPageSize {
		return 0, fmt.Errorf("每页审计记录数量必须在 1 到 %d 之间", maxAuditPageSize)
	}
	return limit, nil
}

func parseAuditTimeRange(rawFrom, rawTo string) (time.Time, time.Time, error) {
	var from, to time.Time
	var err error
	if rawFrom = strings.TrimSpace(rawFrom); rawFrom != "" {
		from, err = time.Parse(time.RFC3339, rawFrom)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("开始日期格式无效")
		}
	}
	if rawTo = strings.TrimSpace(rawTo); rawTo != "" {
		to, err = time.Parse(time.RFC3339, rawTo)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("结束日期格式无效")
		}
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("结束日期必须晚于开始日期")
	}
	return from.UTC(), to.UTC(), nil
}

func encodeAuditCursor(cursor auditCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeAuditCursor(raw string) (auditCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditCursor{}, err
	}
	var cursor auditCursor
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.OccurredAt.IsZero() || len(cursor.ID) != 32 {
		return auditCursor{}, errors.New("invalid audit cursor")
	}
	return cursor, nil
}

func keyFromQuery(r *http.Request) ([]byte, error) {
	return decodeKey(r.URL.Query().Get("key"), r.URL.Query().Get("key_base64"))
}

func decodeKey(key, encoded string) ([]byte, error) {
	if encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("Key 的 Base64 编码无效")
		}
		if len(decoded) == 0 {
			return nil, errors.New("Key 不能为空")
		}
		return decoded, nil
	}
	if key == "" {
		return nil, errors.New("Key 不能为空")
	}
	return []byte(key), nil
}

func decodeValue(request putRequest) ([]byte, error) {
	switch request.ValueEncoding {
	case "", "utf8":
		return []byte(request.Value), nil
	case "base64":
		value, err := base64.StdEncoding.DecodeString(strings.TrimSpace(request.ValueBase64))
		if err != nil {
			return nil, errors.New("Value 不是有效的 Base64 数据")
		}
		return value, nil
	default:
		return nil, errors.New("Value 编码只支持 utf8 或 base64")
	}
}

func optionalInt64(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, errors.New("invalid non-negative integer")
	}
	return &value, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			return errors.New("请求内容超过 2 MiB 限制")
		}
		return errors.New("请求 JSON 格式无效")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("请求只能包含一个 JSON 对象")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Default().Error("write JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
