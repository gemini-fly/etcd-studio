package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gemini-fly/etcd-studio/internal/auth"
	"github.com/gemini-fly/etcd-studio/internal/store"
)

type authPrincipalContextKey struct{}

type loginRequest struct {
	Provider string `json:"provider"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) requireAuthentication(next http.Handler) http.Handler {
	if s.auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || publicAuthRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.auth.Status(nil).Configured {
			writeError(w, http.StatusServiceUnavailable, "auth_setup_required", "认证初始化未完成，请重启服务")
			return
		}
		principal, ok := s.requestPrincipal(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "登录已失效，请重新登录")
			return
		}
		ctx := context.WithValue(r.Context(), authPrincipalContextKey{}, principal)
		if principal.MustChangePassword && !passwordChangeAllowedRoute(r) {
			writeError(w, http.StatusForbidden, "password_change_required", "首次登录必须先修改临时密码")
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func publicAuthRoute(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/auth/status", "POST /api/v1/auth/login":
		return true
	default:
		return false
	}
}

func passwordChangeAllowedRoute(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "POST /api/v1/auth/change-password", "POST /api/v1/auth/logout":
		return true
	default:
		return false
	}
}

func (s *Server) requestPrincipal(r *http.Request) (auth.Principal, bool) {
	if principal, ok := authPrincipalFromContext(r.Context()); ok {
		return principal, true
	}
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return auth.Principal{}, false
	}
	return s.auth.Session(cookie.Value)
}

func authPrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(authPrincipalContextKey{}).(auth.Principal)
	return principal, ok
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusOK, auth.Status{Configured: false})
		return
	}
	var principal *auth.Principal
	if current, ok := s.requestPrincipal(r); ok {
		principal = &current
	}
	writeJSON(w, http.StatusOK, s.auth.Status(principal))
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "认证服务未启用")
		return
	}
	var request loginRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	var principal auth.Principal
	var err error
	switch strings.ToLower(strings.TrimSpace(request.Provider)) {
	case auth.ProviderLocal:
		principal, err = s.auth.AuthenticateLocal(request.Username, request.Password)
	case auth.ProviderLDAP:
		principal, err = s.auth.AuthenticateLDAP(ctx, request.Username, request.Password)
	default:
		writeError(w, http.StatusBadRequest, "invalid_provider", "请选择本地账户或 LDAP 登录")
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		case errors.Is(err, auth.ErrProviderDisabled):
			writeError(w, http.StatusBadRequest, "provider_disabled", "所选登录方式未启用")
		case errors.Is(err, auth.ErrNotConfigured):
			writeError(w, http.StatusPreconditionRequired, "auth_setup_required", "请先创建首个本地管理员")
		default:
			s.logger.Warn("authentication failed", "provider", request.Provider, "username", request.Username, "error", err)
			writeError(w, http.StatusBadGateway, "authentication_backend_error", "认证服务暂时不可用，请稍后重试")
		}
		return
	}
	if err := s.startSession(w, r, principal); err != nil {
		s.logger.Error("create login session", "error", err)
		writeError(w, http.StatusInternalServerError, "session_error", "创建登录会话失败")
		return
	}
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "auth.login", ResourceType: "session", Target: principal.Username,
		Detail: providerLabel(principal.Provider) + "登录成功",
	})
	writeJSON(w, http.StatusOK, principal)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	principal, ok := authPrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "请重新登录")
		return
	}
	if principal.Provider != auth.ProviderLocal {
		writeError(w, http.StatusBadRequest, "local_account_required", "LDAP 密码需要在企业目录中修改")
		return
	}
	var request changePasswordRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var updated auth.Principal
	var err error
	if principal.MustChangePassword {
		updated, err = s.auth.CompleteTemporaryPasswordChange(principal.Username, request.NewPassword)
	} else {
		updated, err = s.auth.ChangePassword(principal.Username, request.CurrentPassword, request.NewPassword)
	}
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid_current_password", "当前密码错误")
		case errors.Is(err, auth.ErrPasswordUnchanged):
			writeError(w, http.StatusBadRequest, "password_unchanged", "新密码不能与当前密码相同")
		case errors.Is(err, auth.ErrPasswordChangeNotRequired):
			writeError(w, http.StatusConflict, "password_change_not_required", "临时密码已修改，请刷新页面后重试")
		default:
			s.writeAuthError(w, "change password", err)
		}
		return
	}
	s.recordAuditForPrincipal(r, updated, store.AuditEvent{
		Action: "auth.password.change", ResourceType: "user", Target: updated.Username, Detail: "修改本地账户密码",
	})
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	principal, _ := authPrincipalFromContext(r.Context())
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		s.auth.DeleteSession(cookie.Value)
	}
	s.clearSessionCookie(w, r)
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "auth.logout", ResourceType: "session", Target: principal.Username, Detail: "退出登录",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

func (s *Server) handleAuthSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	settings, err := s.auth.Settings()
	if err != nil {
		s.writeAuthError(w, "read auth settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateAuthSettings(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input auth.SettingsInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	settings, err := s.auth.UpdateSettings(input)
	if err != nil {
		s.writeAuthError(w, "update auth settings", err)
		return
	}
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "auth.settings.update", ResourceType: "system_setting", Target: "登录与认证",
		Detail: "认证方式设置为 " + authModeLabel(settings.Mode),
	})
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleTestLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var input auth.LDAPSettingsInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := s.auth.TestLDAP(ctx, input); err != nil {
		if errors.Is(err, auth.ErrInvalidInput) {
			s.writeAuthError(w, "test LDAP", err)
			return
		}
		s.logger.Warn("LDAP connection test failed", "host", input.Host, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"connected": false, "message": "连接失败，请检查地址、TLS、Bind 账号和目录配置"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connected": true, "message": "LDAP 连接和 Bind 验证成功"})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.auth.ListUsers()
	if err != nil {
		s.writeAuthError(w, "list users", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input auth.LocalUserInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.validateUserClusterPermissions(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cluster_permissions", err.Error())
		return
	}
	user, err := s.auth.CreateUser(input)
	if err != nil {
		s.writeAuthError(w, "create user", err)
		return
	}
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "user.create", ResourceType: "user", Target: user.Username,
		Detail: fmt.Sprintf("创建本地%s账户，集群权限：%s", roleLabel(user.Role), userClusterAccessLabel(user)),
	})
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	username := r.PathValue("username")
	var input auth.LocalUserInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.EqualFold(principal.Username, username) && principal.Provider == auth.ProviderLocal &&
		((input.Active != nil && !*input.Active) || input.Role != auth.RoleAdmin) {
		writeError(w, http.StatusConflict, "cannot_disable_self", "不能停用自己的账户或移除自己的管理员角色")
		return
	}
	if err := s.validateUserClusterPermissions(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cluster_permissions", err.Error())
		return
	}
	user, err := s.auth.UpdateUser(username, input)
	if err != nil {
		s.writeAuthError(w, "update user", err)
		return
	}
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "user.update", ResourceType: "user", Target: user.Username,
		Detail: fmt.Sprintf("更新为%s，状态：%s，集群权限：%s", roleLabel(user.Role), activeLabel(user.Active), userClusterAccessLabel(user)),
	})
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	username := r.PathValue("username")
	if principal.Provider == auth.ProviderLocal && strings.EqualFold(principal.Username, username) {
		writeError(w, http.StatusConflict, "cannot_delete_self", "不能删除当前登录账户")
		return
	}
	if err := s.auth.DeleteUser(username); err != nil {
		s.writeAuthError(w, "delete user", err)
		return
	}
	s.recordAuditForPrincipal(r, principal, store.AuditEvent{
		Action: "user.delete", ResourceType: "user", Target: username, Detail: "删除本地账户",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.auth == nil {
		return auth.Principal{Role: auth.RoleAdmin}, true
	}
	principal, ok := authPrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "请重新登录")
		return auth.Principal{}, false
	}
	if !principal.IsAdmin() {
		writeError(w, http.StatusForbidden, "admin_required", "只有管理员可以管理用户、集群和系统设置")
		return auth.Principal{}, false
	}
	return principal, true
}

func (s *Server) requestIsAdmin(r *http.Request) bool {
	if s.auth == nil {
		return true
	}
	principal, ok := authPrincipalFromContext(r.Context())
	return ok && principal.IsAdmin()
}

func (s *Server) requestCanAccessCluster(r *http.Request, clusterID string) bool {
	if s.auth == nil {
		return true
	}
	principal, ok := authPrincipalFromContext(r.Context())
	return ok && principal.CanAccessCluster(clusterID)
}

func (s *Server) requireClusterAccess(w http.ResponseWriter, r *http.Request, clusterID string) bool {
	if s.requestCanAccessCluster(r, clusterID) {
		return true
	}
	writeError(w, http.StatusForbidden, "cluster_access_denied", "没有该集群的访问权限")
	return false
}

func (s *Server) validateUserClusterPermissions(input auth.LocalUserInput) error {
	if strings.EqualFold(strings.TrimSpace(input.Role), auth.RoleAdmin) {
		return nil
	}
	available := make(map[string]struct{})
	for _, cluster := range s.clusters.ListClusters() {
		available[cluster.ID] = struct{}{}
	}
	for _, clusterID := range input.ClusterIDs {
		clusterID = strings.TrimSpace(clusterID)
		if clusterID == "" {
			continue
		}
		if _, exists := available[clusterID]; !exists {
			return fmt.Errorf("集群 %q 不存在，请刷新页面后重新选择", clusterID)
		}
	}
	return nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, principal auth.Principal) error {
	token, expiresAt, err := s.auth.CreateSession(principal)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: token, Path: "/", Expires: expiresAt,
		MaxAge: int(auth.SessionDuration.Seconds()), HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: "", Path: "/", Expires: time.Unix(1, 0),
		MaxAge: -1, HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (s *Server) writeAuthError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_auth_input", strings.TrimPrefix(err.Error(), auth.ErrInvalidInput.Error()+": "))
	case errors.Is(err, auth.ErrAlreadyConfigured):
		writeError(w, http.StatusConflict, "auth_already_configured", "认证已经初始化")
	case errors.Is(err, auth.ErrNotConfigured):
		writeError(w, http.StatusPreconditionRequired, "auth_setup_required", "请先创建首个本地管理员")
	case errors.Is(err, auth.ErrUserExists):
		writeError(w, http.StatusConflict, "user_exists", "本地用户名已存在")
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user_not_found", "本地用户不存在")
	case errors.Is(err, auth.ErrLastAdmin):
		writeError(w, http.StatusConflict, "last_admin_required", "必须保留至少一个启用的本地管理员")
	default:
		s.logger.Error("authentication operation failed", "operation", operation, "error", err)
		writeError(w, http.StatusInternalServerError, "auth_storage_error", "认证配置保存失败，请检查数据目录权限")
	}
}

func providerLabel(provider string) string {
	if provider == auth.ProviderLDAP {
		return "LDAP"
	}
	return "本地账户"
}

func authModeLabel(mode string) string {
	switch mode {
	case auth.ModeLDAP:
		return "仅 LDAP"
	case auth.ModeBoth:
		return "本地账户 + LDAP"
	default:
		return "仅本地账户"
	}
}

func roleLabel(role string) string {
	if role == auth.RoleAdmin {
		return "管理员"
	}
	return "操作员"
}

func userClusterAccessLabel(user auth.User) string {
	if user.Role == auth.RoleAdmin {
		return "全部集群"
	}
	if len(user.ClusterIDs) == 0 {
		return "未分配"
	}
	return fmt.Sprintf("%d 个集群", len(user.ClusterIDs))
}

func activeLabel(active bool) string {
	if active {
		return "启用"
	}
	return "停用"
}
