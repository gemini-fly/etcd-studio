package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gemini-fly/etcd-studio/internal/auth"
	"github.com/gemini-fly/etcd-studio/internal/store"
)

type fakeKV struct {
	healthErr         error
	memberStatuses    []store.MemberStatus
	page              store.Page
	entry             store.Entry
	found             bool
	historical        store.Entry
	historicalEntries []store.Entry
	historyHit        bool
	historyErr        error
	historyRev        int64
	historyRevs       []int64
	putErr            error
	deleteErr         error
	deleted           bool
	putKey            []byte
	putValue          []byte
	putExpect         *int64
	deleteKey         []byte
	deleteWant        *int64
}

func (f *fakeKV) Health(context.Context) error { return f.healthErr }

func (f *fakeKV) MemberStatuses(_ context.Context, endpoints []string) []store.MemberStatus {
	if f.memberStatuses != nil {
		return append([]store.MemberStatus(nil), f.memberStatuses...)
	}
	statuses := make([]store.MemberStatus, len(endpoints))
	for index, endpoint := range endpoints {
		statuses[index] = store.MemberStatus{
			Endpoint: endpoint, MemberID: uint64(index + 1), LeaderID: 1,
			Reachable: f.healthErr == nil, Healthy: f.healthErr == nil,
		}
		if f.healthErr != nil {
			statuses[index].Error = f.healthErr.Error()
		}
	}
	return statuses
}

func (f *fakeKV) List(context.Context, []byte, []byte, int64) (store.Page, error) {
	return f.page, nil
}

func (f *fakeKV) Get(context.Context, []byte) (store.Entry, bool, error) {
	return f.entry, f.found, nil
}

func (f *fakeKV) GetAtRevision(_ context.Context, _ []byte, revision int64) (store.Entry, bool, error) {
	f.historyRev = revision
	f.historyRevs = append(f.historyRevs, revision)
	if len(f.historicalEntries) > 0 {
		var selected store.Entry
		found := false
		for _, entry := range f.historicalEntries {
			if entry.ModRevision <= revision && (!found || entry.ModRevision > selected.ModRevision) {
				selected = entry
				found = true
			}
		}
		return selected, found, f.historyErr
	}
	return f.historical, f.historyHit, f.historyErr
}

func (f *fakeKV) Put(_ context.Context, key, value []byte, expected *int64) (int64, error) {
	f.putKey = append([]byte(nil), key...)
	f.putValue = append([]byte(nil), value...)
	f.putExpect = expected
	return 41, f.putErr
}

func (f *fakeKV) Delete(_ context.Context, key []byte, expected *int64) (int64, bool, error) {
	f.deleteKey = append([]byte(nil), key...)
	f.deleteWant = expected
	return 42, f.deleted, f.deleteErr
}

type fakeRegistry struct {
	kv       store.KV
	clusters []store.Cluster
	testErr  error
}

func (f *fakeRegistry) ListClusters() []store.Cluster {
	return append([]store.Cluster(nil), f.clusters...)
}

func (f *fakeRegistry) CreateCluster(input store.ClusterInput) (store.Cluster, error) {
	cluster := store.Cluster{ID: "created", Name: input.Name, Endpoints: input.Endpoints, Username: input.Username}
	if input.Password != nil {
		cluster.Password = *input.Password
	}
	f.clusters = append(f.clusters, cluster)
	return cluster, nil
}

func (f *fakeRegistry) UpdateCluster(id string, input store.ClusterInput) (store.Cluster, error) {
	for index, cluster := range f.clusters {
		if cluster.ID == id {
			cluster.Name = input.Name
			cluster.Endpoints = input.Endpoints
			cluster.Username = input.Username
			if input.Password != nil {
				cluster.Password = *input.Password
			}
			f.clusters[index] = cluster
			return cluster, nil
		}
	}
	return store.Cluster{}, store.ErrClusterNotFound
}

func (f *fakeRegistry) DeleteCluster(id string) error {
	for index, cluster := range f.clusters {
		if cluster.ID == id {
			f.clusters = append(f.clusters[:index], f.clusters[index+1:]...)
			return nil
		}
	}
	return store.ErrClusterNotFound
}

func (f *fakeRegistry) TestCluster(context.Context, string, store.ClusterInput) error {
	return f.testErr
}

func (f *fakeRegistry) ClusterKV(id string) (store.KV, store.Cluster, error) {
	for _, cluster := range f.clusters {
		if cluster.ID == id {
			return f.kv, cluster, nil
		}
	}
	return nil, store.Cluster{}, store.ErrClusterNotFound
}

func (f *fakeRegistry) Close() error { return nil }

type fakeHistory struct {
	saved             []store.ValueSnapshot
	snapshots         []store.ValueSnapshot
	latest            store.ValueSnapshot
	latestFound       bool
	latestErr         error
	saveErr           error
	latestCluster     string
	latestKey         []byte
	latestRevision    int64
	unconfigured      bool
	testErr           error
	configureErr      error
	configuredWith    store.HistoryStorageInput
	retentionVersions int
	prunedWith        int
	statusOverride    *store.HistoryStorageStatus
	auditEvents       []store.AuditEvent
	auditErr          error
	auditQuery        store.AuditQuery
}

func (f *fakeHistory) Save(snapshot store.ValueSnapshot) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	snapshot.Entry.Key = append([]byte(nil), snapshot.Entry.Key...)
	snapshot.Entry.Value = append([]byte(nil), snapshot.Entry.Value...)
	f.saved = append(f.saved, snapshot)
	return nil
}

func (f *fakeHistory) LatestBefore(clusterID string, key []byte, modRevision int64) (store.ValueSnapshot, bool, error) {
	f.latestCluster = clusterID
	f.latestKey = append([]byte(nil), key...)
	f.latestRevision = modRevision
	return f.latest, f.latestFound, f.latestErr
}

func (f *fakeHistory) ListBefore(clusterID string, key []byte, modRevision int64, limit int) ([]store.ValueSnapshot, error) {
	f.latestCluster = clusterID
	f.latestKey = append([]byte(nil), key...)
	f.latestRevision = modRevision
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	candidates := append([]store.ValueSnapshot(nil), f.snapshots...)
	if len(candidates) == 0 && f.latestFound {
		candidates = append(candidates, f.latest)
	}
	filtered := candidates[:0]
	for _, snapshot := range candidates {
		if snapshot.Entry.ModRevision < modRevision {
			filtered = append(filtered, snapshot)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Entry.ModRevision > filtered[j].Entry.ModRevision
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (f *fakeHistory) PruneKey(string, []byte, int) error { return nil }

func (f *fakeHistory) Prune(keep int) error {
	f.prunedWith = keep
	return nil
}

func (f *fakeHistory) Close() error { return nil }

func (f *fakeHistory) SaveAudit(event store.AuditEvent) error {
	if f.unconfigured {
		return store.ErrHistoryNotSetup
	}
	if f.auditErr != nil {
		return f.auditErr
	}
	f.auditEvents = append(f.auditEvents, event)
	return nil
}

func (f *fakeHistory) ListAudit(query store.AuditQuery) (store.AuditPage, error) {
	f.auditQuery = query
	if f.auditErr != nil {
		return store.AuditPage{}, f.auditErr
	}
	events := append([]store.AuditEvent(nil), f.auditEvents...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].ID > events[j].ID
		}
		return events[i].OccurredAt.After(events[j].OccurredAt)
	})
	if len(events) > query.Limit {
		return store.AuditPage{Events: events[:query.Limit], HasMore: true}, nil
	}
	return store.AuditPage{Events: events}, nil
}

func (f *fakeHistory) PruneAudit(time.Time) error { return f.auditErr }

func (f *fakeHistory) Status() store.HistoryStorageStatus {
	if f.statusOverride != nil {
		return *f.statusOverride
	}
	retentionVersions := f.retentionVersions
	if retentionVersions == 0 {
		retentionVersions = 100
	}
	return store.HistoryStorageStatus{
		Configured:        !f.unconfigured,
		Type:              store.HistoryStorageLocal,
		LocalFile:         "/data/history.jsonl",
		DefaultLocalFile:  "/data/history.jsonl",
		RetentionVersions: retentionVersions,
	}
}

func (f *fakeHistory) UpdateRetention(_ context.Context, keep int) error {
	f.retentionVersions = keep
	f.prunedWith = keep
	return nil
}

func (f *fakeHistory) Test(context.Context, store.HistoryStorageInput) error {
	return f.testErr
}

func (f *fakeHistory) Configure(_ context.Context, input store.HistoryStorageInput) error {
	if f.configureErr != nil {
		return f.configureErr
	}
	f.configuredWith = input
	f.unconfigured = false
	return nil
}

func newTestServer(kv store.KV) http.Handler {
	return newTestServerWithHistory(kv, &fakeHistory{})
}

func newTestServerWithHistory(kv store.KV, history store.HistoryStorage) http.Handler {
	registry := &fakeRegistry{
		kv: kv,
		clusters: []store.Cluster{{
			ID: "cluster-1", Name: "测试集群", Endpoints: []string{"http://127.0.0.1:2379"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}},
	}
	return NewServer(registry, history, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

func TestHistoryStorageFirstRunAPI(t *testing.T) {
	t.Parallel()
	history := &fakeHistory{unconfigured: true}
	server := newTestServerWithHistory(&fakeKV{}, history)

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/history-storage", nil)
	statusRecorder := httptest.NewRecorder()
	server.ServeHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK || !bytes.Contains(statusRecorder.Body.Bytes(), []byte(`"configured":false`)) {
		t.Fatalf("status = %d, body = %s", statusRecorder.Code, statusRecorder.Body.String())
	}

	testRequest := httptest.NewRequest(http.MethodPost, "/api/v1/history-storage/test", bytes.NewBufferString(`{"type":"local","local_file":"/data/history.jsonl"}`))
	testRecorder := httptest.NewRecorder()
	server.ServeHTTP(testRecorder, testRequest)
	if testRecorder.Code != http.StatusOK || !bytes.Contains(testRecorder.Body.Bytes(), []byte(`"connected":true`)) {
		t.Fatalf("test status = %d, body = %s", testRecorder.Code, testRecorder.Body.String())
	}

	configureRequest := httptest.NewRequest(http.MethodPut, "/api/v1/history-storage", bytes.NewBufferString(`{"type":"local","local_file":"/data/history.jsonl"}`))
	configureRecorder := httptest.NewRecorder()
	server.ServeHTTP(configureRecorder, configureRequest)
	if configureRecorder.Code != http.StatusOK || history.configuredWith.Type != store.HistoryStorageLocal {
		t.Fatalf("configure status = %d, body = %s, input = %#v", configureRecorder.Code, configureRecorder.Body.String(), history.configuredWith)
	}
}

func TestTemporaryAdminMustSetStrongPasswordBeforeUsingWorkspace(t *testing.T) {
	t.Parallel()
	authentication, err := auth.NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPassword, created, err := authentication.EnsureTemporaryAdmin()
	if err != nil || !created {
		t.Fatalf("temporary admin created = %v, err = %v", created, err)
	}
	registry := &fakeRegistry{kv: &fakeKV{}, clusters: []store.Cluster{{ID: "cluster-1", Name: "测试集群"}}}
	server := NewServer(registry, &fakeHistory{}, slog.New(slog.NewTextHandler(io.Discard, nil)), authentication).Handler()

	protected := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	protectedRecorder := httptest.NewRecorder()
	server.ServeHTTP(protectedRecorder, protected)
	if protectedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated protected status = %d, body = %s", protectedRecorder.Code, protectedRecorder.Body.String())
	}

	loginBody := fmt.Sprintf(`{"provider":"local","username":"admin","password":%q}`, temporaryPassword)
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(loginBody))
	loginRecorder := httptest.NewRecorder()
	server.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK || !bytes.Contains(loginRecorder.Body.Bytes(), []byte(`"must_change_password":true`)) {
		t.Fatalf("temporary login status = %d, body = %s", loginRecorder.Code, loginRecorder.Body.String())
	}
	cookies := loginRecorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookies = %#v", cookies)
	}

	blocked := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	blocked.AddCookie(cookies[0])
	blockedRecorder := httptest.NewRecorder()
	server.ServeHTTP(blockedRecorder, blocked)
	if blockedRecorder.Code != http.StatusForbidden || !bytes.Contains(blockedRecorder.Body.Bytes(), []byte(`"code":"password_change_required"`)) {
		t.Fatalf("temporary session status = %d, body = %s", blockedRecorder.Code, blockedRecorder.Body.String())
	}

	weakChange := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", bytes.NewBufferString(
		`{"new_password":"longbutweakpassword"}`,
	))
	weakChange.AddCookie(cookies[0])
	weakRecorder := httptest.NewRecorder()
	server.ServeHTTP(weakRecorder, weakChange)
	if weakRecorder.Code != http.StatusBadRequest {
		t.Fatalf("weak password status = %d, body = %s", weakRecorder.Code, weakRecorder.Body.String())
	}

	change := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", bytes.NewBufferString(
		`{"new_password":"Permanent-Admin-2026!"}`,
	))
	change.AddCookie(cookies[0])
	changeRecorder := httptest.NewRecorder()
	server.ServeHTTP(changeRecorder, change)
	if changeRecorder.Code != http.StatusOK || bytes.Contains(changeRecorder.Body.Bytes(), []byte(`"must_change_password":true`)) {
		t.Fatalf("change password status = %d, body = %s", changeRecorder.Code, changeRecorder.Body.String())
	}

	authorized := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	authorized.AddCookie(cookies[0])
	authorizedRecorder := httptest.NewRecorder()
	server.ServeHTTP(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body = %s", authorizedRecorder.Code, authorizedRecorder.Body.String())
	}

	ordinaryChange := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", bytes.NewBufferString(
		`{"new_password":"Another-Admin-2026!"}`,
	))
	ordinaryChange.AddCookie(cookies[0])
	ordinaryRecorder := httptest.NewRecorder()
	server.ServeHTTP(ordinaryRecorder, ordinaryChange)
	if ordinaryRecorder.Code != http.StatusUnauthorized || !bytes.Contains(ordinaryRecorder.Body.Bytes(), []byte(`"code":"invalid_current_password"`)) {
		t.Fatalf("ordinary password change status = %d, body = %s", ordinaryRecorder.Code, ordinaryRecorder.Body.String())
	}
}

func TestLocalOperatorCanUseWorkspaceButCannotManageUsers(t *testing.T) {
	t.Parallel()
	authentication, err := auth.NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPassword, _, err := authentication.EnsureTemporaryAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authentication.ChangePassword("admin", temporaryPassword, "Permanent-Admin-2026!"); err != nil {
		t.Fatal(err)
	}
	registry := &fakeRegistry{kv: &fakeKV{}, clusters: []store.Cluster{
		{
			ID: "cluster-1", Name: "测试集群", Endpoints: []string{"https://etcd-1.internal:2379", "https://etcd-2.internal:2379"},
			Username: "etcd-operator", Password: "cluster-secret", TLSCAFile: "/etc/etcd/ca.pem",
		},
		{ID: "cluster-2", Name: "未授权集群", Endpoints: []string{"https://secret.internal:2379"}},
	}}
	history := &fakeHistory{}
	server := NewServer(registry, history, slog.New(slog.NewTextHandler(io.Discard, nil)), authentication).Handler()

	adminLogin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"provider":"local","username":"admin","password":"Permanent-Admin-2026!"}`))
	adminLoginRecorder := httptest.NewRecorder()
	server.ServeHTTP(adminLoginRecorder, adminLogin)
	adminCookie := adminLoginRecorder.Result().Cookies()[0]

	create := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"operator","display_name":"值班人员","password":"Operator-Password-2026!","role":"operator","active":true,"cluster_ids":["cluster-1"]}`))
	create.AddCookie(adminCookie)
	createRecorder := httptest.NewRecorder()
	server.ServeHTTP(createRecorder, create)
	if createRecorder.Code != http.StatusCreated || bytes.Contains(createRecorder.Body.Bytes(), []byte("Operator-Password-2026!")) || bytes.Contains(createRecorder.Body.Bytes(), []byte("password_hash")) {
		t.Fatalf("create user status = %d, body = %s", createRecorder.Code, createRecorder.Body.String())
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"provider":"local","username":"operator","password":"Operator-Password-2026!"}`))
	loginRecorder := httptest.NewRecorder()
	server.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginRecorder.Code, loginRecorder.Body.String())
	}
	operatorCookie := loginRecorder.Result().Cookies()[0]

	workspace := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	workspace.AddCookie(operatorCookie)
	workspaceRecorder := httptest.NewRecorder()
	server.ServeHTTP(workspaceRecorder, workspace)
	if workspaceRecorder.Code != http.StatusOK {
		t.Fatalf("operator workspace status = %d, body = %s", workspaceRecorder.Code, workspaceRecorder.Body.String())
	}
	workspaceBody := workspaceRecorder.Body.Bytes()
	for _, sensitive := range []string{"endpoints", "username", "password_configured", "tls_ca_file", "etcd-1.internal", "etcd-2.internal", "etcd-operator"} {
		if bytes.Contains(workspaceBody, []byte(sensitive)) {
			t.Fatalf("operator cluster list exposed %q: %s", sensitive, workspaceBody)
		}
	}
	for _, expected := range []string{`"id":"cluster-1"`, `"name":"测试集群"`} {
		if !bytes.Contains(workspaceBody, []byte(expected)) {
			t.Fatalf("operator cluster list missing %s: %s", expected, workspaceBody)
		}
	}
	for _, forbidden := range []string{"cluster-2", "未授权集群", "secret.internal"} {
		if bytes.Contains(workspaceBody, []byte(forbidden)) {
			t.Fatalf("operator cluster list exposed unauthorized cluster %q: %s", forbidden, workspaceBody)
		}
	}
	allowedKeys := httptest.NewRequest(http.MethodGet, "/api/v1/keys?cluster_id=cluster-1", nil)
	allowedKeys.AddCookie(operatorCookie)
	allowedKeysRecorder := httptest.NewRecorder()
	server.ServeHTTP(allowedKeysRecorder, allowedKeys)
	if allowedKeysRecorder.Code != http.StatusOK {
		t.Fatalf("assigned cluster keys status = %d, body = %s", allowedKeysRecorder.Code, allowedKeysRecorder.Body.String())
	}

	for _, denied := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/status?cluster_id=cluster-2", ""},
		{http.MethodGet, "/api/v1/keys?cluster_id=cluster-2", ""},
		{http.MethodGet, "/api/v1/key?cluster_id=cluster-2&key=demo", ""},
		{http.MethodDelete, "/api/v1/key?cluster_id=cluster-2&key=demo", ""},
		{http.MethodGet, "/api/v1/key/rollback-preview?cluster_id=cluster-2&key=demo&expected_mod_revision=2&target_mod_revision=1", ""},
		{http.MethodGet, "/api/v1/key/rollback-versions?cluster_id=cluster-2&key=demo&expected_mod_revision=2", ""},
		{http.MethodPut, "/api/v1/key", `{"cluster_id":"cluster-2","key":"demo","value":"blocked"}`},
		{http.MethodPost, "/api/v1/key/rollback", `{"cluster_id":"cluster-2","key":"demo","expected_mod_revision":2,"target_mod_revision":1}`},
	} {
		request := httptest.NewRequest(denied.method, denied.path, bytes.NewBufferString(denied.body))
		request.AddCookie(operatorCookie)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"cluster_access_denied"`)) {
			t.Errorf("unauthorized cluster %s %s status = %d, body = %s", denied.method, denied.path, recorder.Code, recorder.Body.String())
		}
	}

	allowedAudit := httptest.NewRequest(http.MethodGet, "/api/v1/audit?cluster_id=cluster-1", nil)
	allowedAudit.AddCookie(operatorCookie)
	allowedAuditRecorder := httptest.NewRecorder()
	server.ServeHTTP(allowedAuditRecorder, allowedAudit)
	if allowedAuditRecorder.Code != http.StatusOK || history.auditQuery.ClusterID != "cluster-1" ||
		history.auditQuery.Actor != "operator" || history.auditQuery.ActorType != auth.ProviderLocal {
		t.Fatalf("assigned cluster audit status = %d, body = %s", allowedAuditRecorder.Code, allowedAuditRecorder.Body.String())
	}
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit?cluster_id=cluster-2"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(operatorCookie)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("operator audit %s status = %d, body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
	connectionStatus := httptest.NewRequest(http.MethodGet, "/api/v1/status?cluster_id=cluster-1", nil)
	connectionStatus.AddCookie(operatorCookie)
	connectionStatusRecorder := httptest.NewRecorder()
	server.ServeHTTP(connectionStatusRecorder, connectionStatus)
	if connectionStatusRecorder.Code != http.StatusOK ||
		!bytes.Contains(connectionStatusRecorder.Body.Bytes(), []byte(`"endpoint":"测试集群"`)) ||
		!bytes.Contains(connectionStatusRecorder.Body.Bytes(), []byte(`"leader":"节点 1"`)) ||
		!bytes.Contains(connectionStatusRecorder.Body.Bytes(), []byte(`"member_count":2`)) ||
		bytes.Contains(connectionStatusRecorder.Body.Bytes(), []byte("etcd-1.internal")) {
		t.Fatalf("operator connection status = %d, body = %s", connectionStatusRecorder.Code, connectionStatusRecorder.Body.String())
	}
	registry.kv.(*fakeKV).healthErr = errors.New("offline")
	offlineStatus := httptest.NewRequest(http.MethodGet, "/api/v1/status?cluster_id=cluster-1", nil)
	offlineStatus.AddCookie(operatorCookie)
	offlineStatusRecorder := httptest.NewRecorder()
	server.ServeHTTP(offlineStatusRecorder, offlineStatus)
	if offlineStatusRecorder.Code != http.StatusOK || !bytes.Contains(offlineStatusRecorder.Body.Bytes(), []byte(`"endpoint":"测试集群"`)) || bytes.Contains(offlineStatusRecorder.Body.Bytes(), []byte("etcd-1.internal")) {
		t.Fatalf("operator offline status = %d, body = %s", offlineStatusRecorder.Code, offlineStatusRecorder.Body.String())
	}
	registry.kv.(*fakeKV).healthErr = nil

	historyStatus := httptest.NewRequest(http.MethodGet, "/api/v1/history-storage", nil)
	historyStatus.AddCookie(operatorCookie)
	historyStatusRecorder := httptest.NewRecorder()
	server.ServeHTTP(historyStatusRecorder, historyStatus)
	if historyStatusRecorder.Code != http.StatusOK || strings.TrimSpace(historyStatusRecorder.Body.String()) != `{"configured":true}` {
		t.Fatalf("operator history status = %d, body = %s", historyStatusRecorder.Code, historyStatusRecorder.Body.String())
	}

	for _, mutation := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/clusters", `{"name":"禁止新增","endpoints":["http://127.0.0.1:2379"]}`},
		{http.MethodPut, "/api/v1/clusters/cluster-1", `{"name":"禁止修改","endpoints":["http://127.0.0.1:2379"]}`},
		{http.MethodDelete, "/api/v1/clusters/cluster-1", ""},
		{http.MethodPost, "/api/v1/clusters/test", `{"id":"cluster-1"}`},
		{http.MethodPost, "/api/v1/history-storage/test", `{"type":"local","local_file":"/tmp/history.jsonl"}`},
		{http.MethodPut, "/api/v1/history-storage", `{"type":"local","local_file":"/tmp/history.jsonl"}`},
		{http.MethodPatch, "/api/v1/history-storage/retention", `{"retention_versions":25}`},
	} {
		request := httptest.NewRequest(mutation.method, mutation.path, bytes.NewBufferString(mutation.body))
		request.AddCookie(operatorCookie)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"admin_required"`)) {
			t.Errorf("operator %s %s status = %d, body = %s", mutation.method, mutation.path, recorder.Code, recorder.Body.String())
		}
	}

	users := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	users.AddCookie(operatorCookie)
	usersRecorder := httptest.NewRecorder()
	server.ServeHTTP(usersRecorder, users)
	if usersRecorder.Code != http.StatusForbidden || !bytes.Contains(usersRecorder.Body.Bytes(), []byte(`"code":"admin_required"`)) {
		t.Fatalf("operator users status = %d, body = %s", usersRecorder.Code, usersRecorder.Body.String())
	}

	createEmpty := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"empty-operator","display_name":"空权限用户","password":"Empty-Operator-2026!","role":"operator","active":true}`))
	createEmpty.AddCookie(adminCookie)
	createEmptyRecorder := httptest.NewRecorder()
	server.ServeHTTP(createEmptyRecorder, createEmpty)
	if createEmptyRecorder.Code != http.StatusCreated || !bytes.Contains(createEmptyRecorder.Body.Bytes(), []byte(`"cluster_ids":[]`)) {
		t.Fatalf("create empty operator status = %d, body = %s", createEmptyRecorder.Code, createEmptyRecorder.Body.String())
	}
	emptyLogin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"provider":"local","username":"empty-operator","password":"Empty-Operator-2026!"}`))
	emptyLoginRecorder := httptest.NewRecorder()
	server.ServeHTTP(emptyLoginRecorder, emptyLogin)
	if emptyLoginRecorder.Code != http.StatusOK {
		t.Fatalf("empty operator login status = %d, body = %s", emptyLoginRecorder.Code, emptyLoginRecorder.Body.String())
	}
	emptyCookie := emptyLoginRecorder.Result().Cookies()[0]
	emptyClusters := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	emptyClusters.AddCookie(emptyCookie)
	emptyClustersRecorder := httptest.NewRecorder()
	server.ServeHTTP(emptyClustersRecorder, emptyClusters)
	if emptyClustersRecorder.Code != http.StatusOK || strings.TrimSpace(emptyClustersRecorder.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty operator clusters status = %d, body = %s", emptyClustersRecorder.Code, emptyClustersRecorder.Body.String())
	}

	invalidAssignment := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"invalid-operator","password":"Invalid-Operator-2026!","role":"operator","cluster_ids":["missing-cluster"]}`))
	invalidAssignment.AddCookie(adminCookie)
	invalidAssignmentRecorder := httptest.NewRecorder()
	server.ServeHTTP(invalidAssignmentRecorder, invalidAssignment)
	if invalidAssignmentRecorder.Code != http.StatusBadRequest || !bytes.Contains(invalidAssignmentRecorder.Body.Bytes(), []byte(`"code":"invalid_cluster_permissions"`)) {
		t.Fatalf("invalid assignment status = %d, body = %s", invalidAssignmentRecorder.Code, invalidAssignmentRecorder.Body.String())
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", bytes.NewBufferString(`{}`))
	logout.AddCookie(operatorCookie)
	logoutRecorder := httptest.NewRecorder()
	server.ServeHTTP(logoutRecorder, logout)
	if logoutRecorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logoutRecorder.Code, logoutRecorder.Body.String())
	}
	workspaceAfterLogout := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	workspaceAfterLogout.AddCookie(operatorCookie)
	workspaceAfterLogoutRecorder := httptest.NewRecorder()
	server.ServeHTTP(workspaceAfterLogoutRecorder, workspaceAfterLogout)
	if workspaceAfterLogoutRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session status = %d, body = %s", workspaceAfterLogoutRecorder.Code, workspaceAfterLogoutRecorder.Body.String())
	}
}

func TestLoginAndPasswordChangeAuditAreFlushedAfterHistoryInitialization(t *testing.T) {
	t.Parallel()
	authentication, err := auth.NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPassword, _, err := authentication.EnsureTemporaryAdmin()
	if err != nil {
		t.Fatal(err)
	}
	history := &fakeHistory{unconfigured: true}
	server := NewServer(&fakeRegistry{kv: &fakeKV{}}, history, slog.New(slog.NewTextHandler(io.Discard, nil)), authentication).Handler()

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(fmt.Sprintf(
		`{"provider":"local","username":"admin","password":%q}`, temporaryPassword,
	)))
	loginRecorder := httptest.NewRecorder()
	server.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK || len(history.auditEvents) != 0 {
		t.Fatalf("login status = %d, audit events = %#v", loginRecorder.Code, history.auditEvents)
	}
	cookie := loginRecorder.Result().Cookies()[0]
	change := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", bytes.NewBufferString(
		`{"new_password":"Permanent-Admin-2026!"}`,
	))
	change.AddCookie(cookie)
	changeRecorder := httptest.NewRecorder()
	server.ServeHTTP(changeRecorder, change)
	if changeRecorder.Code != http.StatusOK || len(history.auditEvents) != 0 {
		t.Fatalf("change status = %d, audit events = %#v", changeRecorder.Code, history.auditEvents)
	}

	configure := httptest.NewRequest(http.MethodPut, "/api/v1/history-storage", bytes.NewBufferString(`{"type":"local","local_file":"/data/history.jsonl"}`))
	configure.AddCookie(cookie)
	configureRecorder := httptest.NewRecorder()
	server.ServeHTTP(configureRecorder, configure)
	if configureRecorder.Code != http.StatusOK {
		t.Fatalf("configure status = %d, body = %s", configureRecorder.Code, configureRecorder.Body.String())
	}
	if len(history.auditEvents) != 3 || history.auditEvents[0].Action != "auth.login" || history.auditEvents[1].Action != "auth.password.change" || history.auditEvents[2].Action != "history_storage.configure" {
		t.Fatalf("audit events = %#v", history.auditEvents)
	}
}

func TestHistoryStorageStatusReturnsInitializedDatabaseDetailsWithoutPassword(t *testing.T) {
	t.Parallel()
	configuredAt := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	history := &fakeHistory{statusOverride: &store.HistoryStorageStatus{
		Configured: true, ConfiguredAt: configuredAt, Type: store.HistoryStoragePostgres,
		Host: "db.internal", Port: 5432, Database: "etcd_studio", Username: "studio",
		PasswordConfigured: true, TLSMode: "require", RetentionVersions: 100,
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/history-storage", nil)
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(&fakeKV{}, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	for _, expected := range []string{`"type":"postgres"`, `"host":"db.internal"`, `"port":5432`, `"database":"etcd_studio"`, `"username":"studio"`, `"password_configured":true`, `"tls_mode":"require"`, `"configured_at":"2026-08-28T08:00:00Z"`} {
		if !bytes.Contains(body, []byte(expected)) {
			t.Fatalf("body missing %s: %s", expected, body)
		}
	}
	if bytes.Contains(body, []byte(`"password":`)) {
		t.Fatalf("status leaked password field: %s", body)
	}
}

func TestHistoryRetentionAPIUpdatesConfiguredPolicy(t *testing.T) {
	t.Parallel()
	history := &fakeHistory{}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/history-storage/retention", bytes.NewBufferString(`{"retention_versions":25}`))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(&fakeKV{}, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if history.retentionVersions != 25 || history.prunedWith != 25 {
		t.Fatalf("history retention = %d, pruned = %d", history.retentionVersions, history.prunedWith)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"retention_versions":25`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestListKeysEncodesTextAndBinaryEntries(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{page: store.Page{
		Entries: []store.Entry{
			{Key: []byte("/apps/api"), Value: []byte("enabled\ntrue"), ModRevision: 12, Version: 3},
			{Key: []byte{0xff, 0x01}, Value: []byte{0x00, 0xff}, ModRevision: 13, Version: 1},
		},
		NextCursor: []byte("/apps/next"),
	}}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/keys?cluster_id=cluster-1&prefix=/apps/&limit=10", nil)
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(response.Items))
	}
	if response.Items[0].ValuePreview != "enabled true" {
		t.Fatalf("preview = %q", response.Items[0].ValuePreview)
	}
	if response.Items[1].KeyIsUTF8 || response.Items[1].ValueIsUTF8 {
		t.Fatal("binary key and value reported as UTF-8")
	}
	wantCursor := base64.RawURLEncoding.EncodeToString([]byte("/apps/next"))
	if response.NextCursor != wantCursor {
		t.Fatalf("cursor = %q, want %q", response.NextCursor, wantCursor)
	}
}

func TestPutKeyDecodesBase64AndPreservesExpectedRevision(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{}
	body := `{"cluster_id":"cluster-1","key":"/binary","value_encoding":"base64","value_base64":"AP8=","expected_mod_revision":0}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if string(kv.putKey) != "/binary" || !bytes.Equal(kv.putValue, []byte{0x00, 0xff}) {
		t.Fatalf("put key/value = %q/%v", kv.putKey, kv.putValue)
	}
	if kv.putExpect == nil || *kv.putExpect != 0 {
		t.Fatalf("expected revision = %v, want 0", kv.putExpect)
	}
}

func TestPutKeyWritesSanitizedAuditEventForAuthenticatedUser(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{}
	history := &fakeHistory{}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(`{"cluster_id":"cluster-1","key":"/feature","value":"top-secret-value","expected_mod_revision":0}`))
	request.Header.Set("X-Authenticated-User", "alice@example.com")
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(history.auditEvents) != 1 {
		t.Fatalf("audit events = %#v", history.auditEvents)
	}
	event := history.auditEvents[0]
	if event.Actor != "alice@example.com" || event.ActorType != "authenticated_user" || event.Action != "key.create" || event.Target != "/feature" || event.ClusterName != "测试集群" {
		t.Fatalf("audit event = %#v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("top-secret-value")) {
		t.Fatalf("audit event leaked value: %s", encoded)
	}
}

func TestAuditAPIListsEventsAndReturnsRetentionPolicy(t *testing.T) {
	t.Parallel()
	history := &fakeHistory{auditEvents: []store.AuditEvent{
		{ID: "00000000000000000000000000000002", OccurredAt: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC), Actor: "alice", ActorType: "authenticated_user", Action: "key.update", ResourceType: "key", ClusterID: "cluster-1", ClusterName: "测试集群", Target: "/feature", Detail: "保存为 etcd 修订版本 #9", Result: "success"},
		{ID: "00000000000000000000000000000001", OccurredAt: time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC), Actor: "192.0.2.1", ActorType: "client_ip", Action: "key.delete", ResourceType: "key", ClusterID: "cluster-1", ClusterName: "测试集群", Target: "/old", Result: "success"},
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=1", nil)
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(&fakeKV{}, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response auditListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Actor != "alice" || response.RetentionDays != 90 || response.NextCursor == "" {
		t.Fatalf("response = %#v", response)
	}
	if history.auditQuery.Actor != "" || history.auditQuery.ActorType != "" {
		t.Fatalf("administrator audit query unexpectedly restricted = %#v", history.auditQuery)
	}
}

func TestAuditAPIAppliesDateRange(t *testing.T) {
	t.Parallel()
	history := &fakeHistory{}
	from := "2026-08-30T16:00:00Z"
	to := "2026-08-31T16:00:00Z"
	query := url.Values{"from": {from}, "to": {to}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit?"+query.Encode(), nil)
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(&fakeKV{}, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if history.auditQuery.Since.Format(time.RFC3339) != from || history.auditQuery.Until.Format(time.RFC3339) != to {
		t.Fatalf("audit query = %#v", history.auditQuery)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/audit?from="+url.QueryEscape(to)+"&to="+url.QueryEscape(from), nil)
	invalidRecorder := httptest.NewRecorder()
	newTestServerWithHistory(&fakeKV{}, &fakeHistory{}).ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusBadRequest || !bytes.Contains(invalidRecorder.Body.Bytes(), []byte(`"invalid_date_range"`)) {
		t.Fatalf("invalid status = %d, body = %s", invalidRecorder.Code, invalidRecorder.Body.String())
	}
}

func TestPutConflictReturnsHTTP409(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{putErr: store.ErrConflict}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(`{"cluster_id":"cluster-1","key":"/x","value":"new","expected_mod_revision":8}`))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"revision_conflict"`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestPutArchivesCurrentValueBeforeWriting(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{entry: store.Entry{Key: []byte("/x"), Value: []byte("old"), ModRevision: 8, Version: 2}, found: true}
	history := &fakeHistory{}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(`{"cluster_id":"cluster-1","key":"/x","value":"new","expected_mod_revision":8}`))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(history.saved) != 1 || history.saved[0].ClusterID != "cluster-1" || string(history.saved[0].Entry.Value) != "old" {
		t.Fatalf("saved snapshots = %#v", history.saved)
	}
	if string(kv.putValue) != "new" {
		t.Fatalf("put value = %q", kv.putValue)
	}
}

func TestPutStopsWhenLocalHistoryCannotBeSaved(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{entry: store.Entry{Key: []byte("/x"), Value: []byte("old"), ModRevision: 8, Version: 2}, found: true}
	history := &fakeHistory{saveErr: errors.New("disk full")}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(`{"cluster_id":"cluster-1","key":"/x","value":"new","expected_mod_revision":8}`))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError || !bytes.Contains(recorder.Body.Bytes(), []byte(`"local_history_error"`)) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.putValue != nil {
		t.Fatalf("put unexpectedly executed with value %q", kv.putValue)
	}
}

func TestPutIsBlockedUntilHistoryStorageIsConfigured(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{entry: store.Entry{Key: []byte("/x"), Value: []byte("old"), ModRevision: 8, Version: 2}, found: true}
	history := &fakeHistory{saveErr: store.ErrHistoryNotSetup, unconfigured: true}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/key", bytes.NewBufferString(`{"cluster_id":"cluster-1","key":"/x","value":"new","expected_mod_revision":8}`))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable || !bytes.Contains(recorder.Body.Bytes(), []byte(`"history_storage_not_configured"`)) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.putValue != nil {
		t.Fatalf("put unexpectedly executed with value %q", kv.putValue)
	}
}

func TestRollbackPreviewReturnsPreviousValue(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:      store.Entry{Key: []byte("/feature"), Value: []byte("broken"), ModRevision: 20, Version: 2},
		found:      true,
		historical: store.Entry{Key: []byte("/feature"), Value: []byte("working"), ModRevision: 11, Version: 1},
		historyHit: true,
	}
	key := base64.StdEncoding.EncodeToString([]byte("/feature"))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/key/rollback-preview?cluster_id=cluster-1&key_base64="+key+"&expected_mod_revision=20&target_mod_revision=11", nil)
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.historyRev != 11 {
		t.Fatalf("historical revision = %d, want 11", kv.historyRev)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"value":"working"`)) || !bytes.Contains(recorder.Body.Bytes(), []byte(`"mod_revision":11`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestRollbackVersionsMergeStorageAndEtcdHistoryWithPagination(t *testing.T) {
	t.Parallel()
	keyBytes := []byte("/feature")
	kv := &fakeKV{
		entry: store.Entry{Key: keyBytes, Value: []byte("current"), ModRevision: 40, Version: 4},
		found: true,
		historicalEntries: []store.Entry{
			{Key: keyBytes, Value: []byte("one"), ModRevision: 10, Version: 1},
			{Key: keyBytes, Value: []byte("two"), ModRevision: 20, Version: 2},
			{Key: keyBytes, Value: []byte("three"), ModRevision: 30, Version: 3},
		},
	}
	history := &fakeHistory{snapshots: []store.ValueSnapshot{
		{ClusterID: "cluster-1", Entry: store.Entry{Key: keyBytes, Value: []byte("one"), ModRevision: 10, Version: 1}},
		{ClusterID: "cluster-1", Entry: store.Entry{Key: keyBytes, Value: []byte("three"), ModRevision: 30, Version: 3}},
	}}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/key/rollback-versions?cluster_id=cluster-1&key_base64="+key+"&expected_mod_revision=40&limit=2", nil)
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response rollbackVersionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 || response.Items[0].Entry.ModRevision != 30 || response.Items[1].Entry.ModRevision != 20 {
		t.Fatalf("items = %#v", response.Items)
	}
	if response.Items[0].HistorySource != "storage" || response.Items[1].HistorySource != "etcd" {
		t.Fatalf("sources = %#v", response.Items)
	}
	if response.NextBeforeModRevision != 20 {
		t.Fatalf("next before revision = %d, want 20", response.NextBeforeModRevision)
	}

	nextRequest := httptest.NewRequest(http.MethodGet, "/api/v1/key/rollback-versions?cluster_id=cluster-1&key_base64="+key+"&expected_mod_revision=40&before_mod_revision=20&limit=2", nil)
	nextRecorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(nextRecorder, nextRequest)
	if nextRecorder.Code != http.StatusOK {
		t.Fatalf("next status = %d, body = %s", nextRecorder.Code, nextRecorder.Body.String())
	}
	response = rollbackVersionsResponse{}
	if err := json.Unmarshal(nextRecorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Entry.ModRevision != 10 || response.NextBeforeModRevision != 0 {
		t.Fatalf("next page = %#v", response)
	}
}

func TestRollbackRequiresExplicitTargetVersion(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{entry: store.Entry{Key: []byte("/feature"), Value: []byte("current"), ModRevision: 20, Version: 2}, found: true}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":20}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte(`"invalid_target_revision"`)) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.putExpect != nil {
		t.Fatal("rollback without selected target unexpectedly wrote a value")
	}
}

func TestRollbackWritesPreviousValueWithCurrentRevisionGuard(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:      store.Entry{Key: []byte("/feature"), Value: []byte("broken"), ModRevision: 20, Version: 2},
		found:      true,
		historical: store.Entry{Key: []byte("/feature"), Value: []byte("working"), ModRevision: 11, Version: 1},
		historyHit: true,
	}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":20,"target_mod_revision":11}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if string(kv.putKey) != "/feature" || string(kv.putValue) != "working" {
		t.Fatalf("rollback put = %q/%q", kv.putKey, kv.putValue)
	}
	if kv.putExpect == nil || *kv.putExpect != 20 {
		t.Fatalf("rollback expected revision = %v, want 20", kv.putExpect)
	}
}

func TestRollbackUsesLocalHistoryWhenEtcdHistoryIsCompacted(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:      store.Entry{Key: []byte("/feature"), Value: []byte("broken"), ModRevision: 20, Version: 2},
		found:      true,
		historyErr: store.ErrHistoryCompacted,
	}
	history := &fakeHistory{
		latest:      store.ValueSnapshot{ClusterID: "cluster-1", Entry: store.Entry{Key: []byte("/feature"), Value: []byte("working"), ModRevision: 11, Version: 1}},
		latestFound: true,
	}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":20,"target_mod_revision":11}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.historyRev != 0 {
		t.Fatalf("local target unexpectedly queried etcd revision %d", kv.historyRev)
	}
	if string(kv.putValue) != "working" || !bytes.Contains(recorder.Body.Bytes(), []byte(`"history_source":"storage"`)) {
		t.Fatalf("put value = %q, body = %s", kv.putValue, recorder.Body.String())
	}
	if len(history.saved) != 1 || string(history.saved[0].Entry.Value) != "broken" {
		t.Fatalf("rollback did not archive current value: %#v", history.saved)
	}
}

func TestRollbackPrefersNewerEtcdVersionOverOlderLocalSnapshot(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:      store.Entry{Key: []byte("/feature"), Value: []byte("current"), ModRevision: 30, Version: 4},
		found:      true,
		historical: store.Entry{Key: []byte("/feature"), Value: []byte("immediate-previous"), ModRevision: 25, Version: 3},
		historyHit: true,
	}
	history := &fakeHistory{
		latest:      store.ValueSnapshot{ClusterID: "cluster-1", Entry: store.Entry{Key: []byte("/feature"), Value: []byte("older-local"), ModRevision: 11, Version: 1}},
		latestFound: true,
	}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":30,"target_mod_revision":25}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if string(kv.putValue) != "immediate-previous" || !bytes.Contains(recorder.Body.Bytes(), []byte(`"history_source":"etcd"`)) {
		t.Fatalf("put value = %q, body = %s", kv.putValue, recorder.Body.String())
	}
	if len(history.saved) != 2 || string(history.saved[0].Entry.Value) != "immediate-previous" || string(history.saved[1].Entry.Value) != "current" {
		t.Fatalf("saved snapshots = %#v", history.saved)
	}
}

func TestRollbackRejectsStaleCurrentRevision(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry: store.Entry{Key: []byte("/feature"), Value: []byte("newer"), ModRevision: 21, Version: 3},
		found: true,
	}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":20,"target_mod_revision":11}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict || !bytes.Contains(recorder.Body.Bytes(), []byte(`"revision_conflict"`)) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if kv.putExpect != nil {
		t.Fatal("stale rollback unexpectedly wrote a value")
	}
}

func TestRollbackReportsCompactedHistory(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:      store.Entry{Key: []byte("/feature"), Value: []byte("broken"), ModRevision: 20, Version: 2},
		found:      true,
		historyErr: store.ErrHistoryCompacted,
	}
	body := `{"cluster_id":"cluster-1","key":"/feature","expected_mod_revision":20,"target_mod_revision":11}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/key/rollback", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict || !bytes.Contains(recorder.Body.Bytes(), []byte(`"history_compacted"`)) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeleteRequiresExistingKey(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{deleted: false}
	encodedKey := base64.StdEncoding.EncodeToString([]byte("/gone"))
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/key?cluster_id=cluster-1&key_base64="+encodedKey+"&expected_mod_revision=9", nil)
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if kv.deleteKey != nil {
		t.Fatalf("missing key unexpectedly reached delete: %q", kv.deleteKey)
	}
}

func TestDeleteArchivesCurrentValueBeforeDeleting(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{
		entry:   store.Entry{Key: []byte("/gone"), Value: []byte("keep-me"), ModRevision: 9, Version: 2},
		found:   true,
		deleted: true,
	}
	history := &fakeHistory{}
	encodedKey := base64.StdEncoding.EncodeToString([]byte("/gone"))
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/key?cluster_id=cluster-1&key_base64="+encodedKey+"&expected_mod_revision=9", nil)
	recorder := httptest.NewRecorder()
	newTestServerWithHistory(kv, history).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(history.saved) != 1 || string(history.saved[0].Entry.Value) != "keep-me" {
		t.Fatalf("saved snapshots = %#v", history.saved)
	}
	if string(kv.deleteKey) != "/gone" || kv.deleteWant == nil || *kv.deleteWant != 9 {
		t.Fatalf("delete args = %q, %v", kv.deleteKey, kv.deleteWant)
	}
}

func TestStatusReportsDisconnectedWithoutExposingError(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{healthErr: errors.New("dial tcp user:secret@example: connection refused")}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status?cluster_id=cluster-1", nil)
	recorder := httptest.NewRecorder()
	newTestServer(kv).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("secret")) {
		t.Fatalf("response leaked backend error: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"connected":false`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestStatusReportsLeaderAndDegradedMultiMemberCluster(t *testing.T) {
	t.Parallel()
	kv := &fakeKV{memberStatuses: []store.MemberStatus{
		{Endpoint: "https://etcd-1.internal:2379", MemberID: 11, LeaderID: 22, Reachable: true, Healthy: true, Version: "3.6.5"},
		{Endpoint: "https://etcd-2.internal:2379", MemberID: 22, LeaderID: 22, Reachable: true, Healthy: true, Version: "3.6.5"},
		{Endpoint: "https://etcd-3.internal:2379", Error: "connection refused"},
	}}
	registry := &fakeRegistry{
		kv: kv,
		clusters: []store.Cluster{{
			ID: "cluster-1", Name: "生产集群", Endpoints: []string{
				"https://etcd-1.internal:2379", "https://etcd-2.internal:2379", "https://etcd-3.internal:2379",
			},
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status?cluster_id=cluster-1", nil)
	recorder := httptest.NewRecorder()
	NewServer(registry, &fakeHistory{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, expected := range []string{
		`"connected":true`, `"degraded":true`, `"leader":"https://etcd-2.internal:2379"`,
		`"member_count":3`, `"healthy_member_count":2`,
	} {
		if !bytes.Contains(recorder.Body.Bytes(), []byte(expected)) {
			t.Errorf("response missing %s: %s", expected, recorder.Body.String())
		}
	}
}

func TestEmbeddedPageAndSecurityHeaders(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	newTestServer(&fakeKV{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Etcd Studio")) {
		t.Fatal("embedded index page was not served")
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`<span>删除 Key</span>`)) {
		t.Fatal("embedded editor does not expose a visible delete Key action")
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`id="validateJSONButton"`)) ||
		!bytes.Contains(recorder.Body.Bytes(), []byte(`id="jsonValidationResult"`)) {
		t.Fatal("embedded editor does not expose JSON validation controls")
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("操作员仅可查看本人在已授权集群中的操作")) {
		t.Fatal("embedded audit page does not explain operator audit visibility")
	}
	if recorder.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestListClustersNeverReturnsPassword(t *testing.T) {
	t.Parallel()
	registry := &fakeRegistry{
		kv: &fakeKV{},
		clusters: []store.Cluster{{
			ID: "production", Name: "生产集群", Endpoints: []string{"https://etcd.internal:2379"},
			Username: "operator", Password: "super-secret",
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	recorder := httptest.NewRecorder()
	NewServer(registry, &fakeHistory{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("super-secret")) {
		t.Fatalf("cluster response leaked password: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"password_configured":true`)) {
		t.Fatalf("response does not indicate configured password: %s", recorder.Body.String())
	}
}

func TestKeyOperationsRequireClusterID(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
	recorder := httptest.NewRecorder()
	newTestServer(&fakeKV{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"cluster_not_found"`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}
