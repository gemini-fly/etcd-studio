package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	ModeLocal = "local"
	ModeLDAP  = "ldap"
	ModeBoth  = "both"

	ProviderLocal = "local"
	ProviderLDAP  = "ldap"

	RoleAdmin    = "admin"
	RoleOperator = "operator"

	SessionCookieName     = "etcd_studio_session"
	SessionDuration       = 12 * time.Hour
	MinimumPasswordLength = 10

	authConfigVersion = 1
)

var (
	ErrNotConfigured             = errors.New("authentication is not configured")
	ErrAlreadyConfigured         = errors.New("authentication is already configured")
	ErrInvalidInput              = errors.New("invalid authentication input")
	ErrInvalidCredentials        = errors.New("invalid username or password")
	ErrProviderDisabled          = errors.New("authentication provider is disabled")
	ErrUserExists                = errors.New("local user already exists")
	ErrUserNotFound              = errors.New("local user not found")
	ErrLastAdmin                 = errors.New("at least one active local administrator is required")
	ErrPasswordUnchanged         = errors.New("new password must differ from current password")
	ErrPasswordChangeNotRequired = errors.New("temporary password change is not required")
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{1,63}$`)

type Principal struct {
	Username           string   `json:"username"`
	DisplayName        string   `json:"display_name"`
	Provider           string   `json:"provider"`
	Role               string   `json:"role"`
	ClusterIDs         []string `json:"cluster_ids"`
	MustChangePassword bool     `json:"must_change_password,omitempty"`
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

func (p Principal) CanAccessCluster(clusterID string) bool {
	if p.IsAdmin() {
		return true
	}
	clusterID = strings.TrimSpace(clusterID)
	for _, allowed := range p.ClusterIDs {
		if allowed == clusterID {
			return true
		}
	}
	return false
}

type User struct {
	Username           string    `json:"username"`
	DisplayName        string    `json:"display_name"`
	Role               string    `json:"role"`
	Active             bool      `json:"active"`
	ClusterIDs         []string  `json:"cluster_ids"`
	MustChangePassword bool      `json:"must_change_password,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type LocalUserInput struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Password    string   `json:"password"`
	Role        string   `json:"role"`
	Active      *bool    `json:"active,omitempty"`
	ClusterIDs  []string `json:"cluster_ids"`
}

type LDAPSettingsInput struct {
	Host                 string   `json:"host"`
	Port                 int      `json:"port"`
	TLSMode              string   `json:"tls_mode"`
	CAFile               string   `json:"ca_file"`
	ServerName           string   `json:"server_name"`
	BaseDN               string   `json:"base_dn"`
	BindDN               string   `json:"bind_dn"`
	BindPassword         *string  `json:"bind_password,omitempty"`
	UserFilter           string   `json:"user_filter"`
	UserDNTemplate       string   `json:"user_dn_template"`
	UsernameAttribute    string   `json:"username_attribute"`
	DisplayNameAttribute string   `json:"display_name_attribute"`
	AdminUsernames       []string `json:"admin_usernames"`
}

type LDAPSettingsStatus struct {
	Host                   string   `json:"host"`
	Port                   int      `json:"port"`
	TLSMode                string   `json:"tls_mode"`
	CAFile                 string   `json:"ca_file"`
	ServerName             string   `json:"server_name"`
	BaseDN                 string   `json:"base_dn"`
	BindDN                 string   `json:"bind_dn"`
	BindPasswordConfigured bool     `json:"bind_password_configured"`
	UserFilter             string   `json:"user_filter"`
	UserDNTemplate         string   `json:"user_dn_template"`
	UsernameAttribute      string   `json:"username_attribute"`
	DisplayNameAttribute   string   `json:"display_name_attribute"`
	AdminUsernames         []string `json:"admin_usernames"`
}

type SettingsInput struct {
	Mode string            `json:"mode"`
	LDAP LDAPSettingsInput `json:"ldap"`
}

type SettingsStatus struct {
	ConfiguredAt time.Time          `json:"configured_at"`
	Mode         string             `json:"mode"`
	LDAP         LDAPSettingsStatus `json:"ldap"`
}

type Status struct {
	Configured    bool       `json:"configured"`
	LocalEnabled  bool       `json:"local_enabled"`
	LDAPEnabled   bool       `json:"ldap_enabled"`
	Authenticated bool       `json:"authenticated"`
	User          *Principal `json:"user,omitempty"`
}

type persistedUser struct {
	User
	PasswordHash string `json:"password_hash"`
}

type ldapSettings struct {
	Host                 string   `json:"host"`
	Port                 int      `json:"port"`
	TLSMode              string   `json:"tls_mode"`
	CAFile               string   `json:"ca_file"`
	ServerName           string   `json:"server_name"`
	BaseDN               string   `json:"base_dn"`
	BindDN               string   `json:"bind_dn"`
	BindPassword         string   `json:"bind_password"`
	UserFilter           string   `json:"user_filter"`
	UserDNTemplate       string   `json:"user_dn_template"`
	UsernameAttribute    string   `json:"username_attribute"`
	DisplayNameAttribute string   `json:"display_name_attribute"`
	AdminUsernames       []string `json:"admin_usernames"`
}

type persistedConfig struct {
	Version      int             `json:"version"`
	ConfiguredAt time.Time       `json:"configured_at"`
	Mode         string          `json:"mode"`
	LDAP         ldapSettings    `json:"ldap"`
	Users        []persistedUser `json:"users"`
}

type session struct {
	Principal Principal
	ExpiresAt time.Time
}

type Manager struct {
	mu         sync.RWMutex
	filePath   string
	timeout    time.Duration
	config     persistedConfig
	configured bool
	sessionMu  sync.Mutex
	sessions   map[string]session
}

func NewManager(filePath string, timeout time.Duration) (*Manager, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("auth config file cannot be empty")
	}
	if timeout <= 0 {
		return nil, errors.New("auth connection timeout must be positive")
	}
	manager := &Manager{filePath: filePath, timeout: timeout, sessions: make(map[string]session)}
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Status(principal *Principal) Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := Status{Configured: m.configured}
	if m.configured {
		status.LocalEnabled = modeIncludes(m.config.Mode, ProviderLocal)
		status.LDAPEnabled = modeIncludes(m.config.Mode, ProviderLDAP)
	}
	if principal != nil {
		copy := *principal
		copy.ClusterIDs = append([]string{}, principal.ClusterIDs...)
		status.Authenticated = true
		status.User = &copy
	}
	return status
}

func (m *Manager) Bootstrap(input LocalUserInput) (User, error) {
	return m.bootstrap(input, false)
}

// EnsureTemporaryAdmin creates the one-time local administrator used on first
// process startup. The generated password is returned exactly once and is not
// persisted in plaintext.
func (m *Manager) EnsureTemporaryAdmin() (password string, created bool, err error) {
	m.mu.RLock()
	configured := m.configured
	m.mu.RUnlock()
	if configured {
		return "", false, nil
	}
	password, err = generateTemporaryPassword(24)
	if err != nil {
		return "", false, err
	}
	_, err = m.bootstrap(LocalUserInput{
		Username: "admin", DisplayName: "系统管理员", Password: password, Role: RoleAdmin,
	}, true)
	if errors.Is(err, ErrAlreadyConfigured) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return password, true, nil
}

func (m *Manager) bootstrap(input LocalUserInput, mustChangePassword bool) (User, error) {
	username, displayName, role, err := normalizeUserFields(input.Username, input.DisplayName, RoleAdmin)
	if err != nil {
		return User{}, err
	}
	if role != RoleAdmin {
		return User{}, fmt.Errorf("%w: 首个账户必须是管理员", ErrInvalidInput)
	}
	hash, err := hashPassword(input.Password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	user := User{
		Username: username, DisplayName: displayName, Role: RoleAdmin, Active: true,
		ClusterIDs: []string{}, MustChangePassword: mustChangePassword, CreatedAt: now, UpdatedAt: now,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured {
		return User{}, ErrAlreadyConfigured
	}
	m.config = persistedConfig{
		Version: authConfigVersion, ConfiguredAt: now, Mode: ModeLocal,
		Users: []persistedUser{{User: user, PasswordHash: hash}},
	}
	if err := m.saveLocked(); err != nil {
		m.config = persistedConfig{}
		return User{}, err
	}
	m.configured = true
	return user, nil
}

func (m *Manager) Settings() (SettingsStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.configured {
		return SettingsStatus{}, ErrNotConfigured
	}
	return settingsStatus(m.config), nil
}

func (m *Manager) UpdateSettings(input SettingsInput) (SettingsStatus, error) {
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode != ModeLocal && mode != ModeLDAP && mode != ModeBoth {
		return SettingsStatus{}, fmt.Errorf("%w: 认证方式必须是本地账户、LDAP 或两者同时启用", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured {
		return SettingsStatus{}, ErrNotConfigured
	}
	ldapConfig, err := normalizeLDAPSettings(input.LDAP, m.config.LDAP, modeIncludes(mode, ProviderLDAP))
	if err != nil {
		return SettingsStatus{}, err
	}
	if modeIncludes(mode, ProviderLocal) && activeAdminCount(m.config.Users) == 0 {
		return SettingsStatus{}, ErrLastAdmin
	}
	if mode == ModeLDAP && len(ldapConfig.AdminUsernames) == 0 {
		return SettingsStatus{}, fmt.Errorf("%w: 仅启用 LDAP 时至少配置一个 LDAP 管理员用户名", ErrInvalidInput)
	}
	previousMode, previousLDAP := m.config.Mode, m.config.LDAP
	m.config.Mode, m.config.LDAP = mode, ldapConfig
	if err := m.saveLocked(); err != nil {
		m.config.Mode, m.config.LDAP = previousMode, previousLDAP
		return SettingsStatus{}, err
	}
	m.revokeProviderSessionsLocked(mode)
	return settingsStatus(m.config), nil
}

func (m *Manager) ListUsers() ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.configured {
		return nil, ErrNotConfigured
	}
	users := make([]User, 0, len(m.config.Users))
	for _, user := range m.config.Users {
		copy := user.User
		copy.ClusterIDs = append([]string{}, user.ClusterIDs...)
		users = append(users, copy)
	}
	sort.Slice(users, func(i, j int) bool { return strings.ToLower(users[i].Username) < strings.ToLower(users[j].Username) })
	return users, nil
}

func (m *Manager) CreateUser(input LocalUserInput) (User, error) {
	username, displayName, role, err := normalizeUserFields(input.Username, input.DisplayName, input.Role)
	if err != nil {
		return User{}, err
	}
	hash, err := hashPassword(input.Password)
	if err != nil {
		return User{}, err
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	now := time.Now().UTC()
	user := User{
		Username: username, DisplayName: displayName, Role: role, Active: active,
		ClusterIDs: clusterIDsForRole(role, input.ClusterIDs), CreatedAt: now, UpdatedAt: now,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured {
		return User{}, ErrNotConfigured
	}
	if findUserIndex(m.config.Users, username) >= 0 {
		return User{}, ErrUserExists
	}
	m.config.Users = append(m.config.Users, persistedUser{User: user, PasswordHash: hash})
	if err := m.saveLocked(); err != nil {
		m.config.Users = m.config.Users[:len(m.config.Users)-1]
		return User{}, err
	}
	return user, nil
}

func (m *Manager) UpdateUser(username string, input LocalUserInput) (User, error) {
	displayName := strings.TrimSpace(input.DisplayName)
	role := strings.ToLower(strings.TrimSpace(input.Role))
	if displayName == "" || len([]rune(displayName)) > 100 || (role != RoleAdmin && role != RoleOperator) {
		return User{}, fmt.Errorf("%w: 显示名称不能为空，角色必须是管理员或操作员", ErrInvalidInput)
	}
	var hash string
	var err error
	if input.Password != "" {
		hash, err = hashPassword(input.Password)
		if err != nil {
			return User{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured {
		return User{}, ErrNotConfigured
	}
	index := findUserIndex(m.config.Users, username)
	if index < 0 {
		return User{}, ErrUserNotFound
	}
	previous := m.config.Users[index]
	updated := previous
	updated.DisplayName = displayName
	updated.Role = role
	updated.ClusterIDs = clusterIDsForRole(role, input.ClusterIDs)
	if input.Active != nil {
		updated.Active = *input.Active
	}
	if hash != "" {
		updated.PasswordHash = hash
	}
	updated.UpdatedAt = time.Now().UTC()
	m.config.Users[index] = updated
	if modeIncludes(m.config.Mode, ProviderLocal) && activeAdminCount(m.config.Users) == 0 {
		m.config.Users[index] = previous
		return User{}, ErrLastAdmin
	}
	if err := m.saveLocked(); err != nil {
		m.config.Users[index] = previous
		return User{}, err
	}
	m.revokeSessions(ProviderLocal, previous.Username)
	return updated.User, nil
}

func (m *Manager) DeleteUser(username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured {
		return ErrNotConfigured
	}
	index := findUserIndex(m.config.Users, username)
	if index < 0 {
		return ErrUserNotFound
	}
	previous := append([]persistedUser(nil), m.config.Users...)
	m.config.Users = append(m.config.Users[:index], m.config.Users[index+1:]...)
	if modeIncludes(m.config.Mode, ProviderLocal) && activeAdminCount(m.config.Users) == 0 {
		m.config.Users = previous
		return ErrLastAdmin
	}
	if err := m.saveLocked(); err != nil {
		m.config.Users = previous
		return err
	}
	m.revokeSessions(ProviderLocal, username)
	return nil
}

// RemoveClusterPermissions clears a deleted cluster from every local
// operator so stale grants do not remain in the authentication file.
func (m *Manager) RemoveClusterPermissions(clusterID string) error {
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return fmt.Errorf("%w: 集群 ID 不能为空", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured {
		return ErrNotConfigured
	}
	previous := make([][]string, len(m.config.Users))
	previousUpdatedAt := make([]time.Time, len(m.config.Users))
	changed := false
	for index := range m.config.Users {
		previous[index] = append([]string{}, m.config.Users[index].ClusterIDs...)
		previousUpdatedAt[index] = m.config.Users[index].UpdatedAt
		filtered := m.config.Users[index].ClusterIDs[:0]
		userChanged := false
		for _, allowed := range m.config.Users[index].ClusterIDs {
			if allowed != clusterID {
				filtered = append(filtered, allowed)
			} else {
				changed = true
				userChanged = true
			}
		}
		m.config.Users[index].ClusterIDs = filtered
		if userChanged {
			m.config.Users[index].UpdatedAt = time.Now().UTC()
		}
	}
	if !changed {
		return nil
	}
	if err := m.saveLocked(); err != nil {
		for index := range m.config.Users {
			m.config.Users[index].ClusterIDs = previous[index]
			m.config.Users[index].UpdatedAt = previousUpdatedAt[index]
		}
		return err
	}
	return nil
}

func (m *Manager) AuthenticateLocal(username, password string) (Principal, error) {
	m.mu.RLock()
	if !m.configured {
		m.mu.RUnlock()
		return Principal{}, ErrNotConfigured
	}
	if !modeIncludes(m.config.Mode, ProviderLocal) {
		m.mu.RUnlock()
		return Principal{}, ErrProviderDisabled
	}
	index := findUserIndex(m.config.Users, strings.TrimSpace(username))
	if index < 0 {
		m.mu.RUnlock()
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOe5u5.jiA4V7Q6r5QkdOqC9D1sN3Aq6u"), []byte(password))
		return Principal{}, ErrInvalidCredentials
	}
	user := m.config.Users[index]
	m.mu.RUnlock()
	if !user.Active || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return Principal{}, ErrInvalidCredentials
	}
	return Principal{
		Username: user.Username, DisplayName: user.DisplayName, Provider: ProviderLocal, Role: user.Role,
		ClusterIDs: append([]string{}, user.ClusterIDs...), MustChangePassword: user.MustChangePassword,
	}, nil
}

// ChangePassword verifies a local user's current password, applies the strong
// password policy, and clears the first-login password-change requirement.
func (m *Manager) ChangePassword(username, currentPassword, newPassword string) (Principal, error) {
	if err := validateStrongPassword(newPassword); err != nil {
		return Principal{}, err
	}
	m.mu.RLock()
	if !m.configured {
		m.mu.RUnlock()
		return Principal{}, ErrNotConfigured
	}
	index := findUserIndex(m.config.Users, username)
	if index < 0 {
		m.mu.RUnlock()
		return Principal{}, ErrInvalidCredentials
	}
	current := m.config.Users[index]
	m.mu.RUnlock()
	if !current.Active || bcrypt.CompareHashAndPassword([]byte(current.PasswordHash), []byte(currentPassword)) != nil {
		return Principal{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(current.PasswordHash), []byte(newPassword)) == nil {
		return Principal{}, ErrPasswordUnchanged
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return Principal{}, fmt.Errorf("hash password: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	index = findUserIndex(m.config.Users, username)
	if index < 0 || m.config.Users[index].PasswordHash != current.PasswordHash {
		return Principal{}, ErrInvalidCredentials
	}
	previous := m.config.Users[index]
	m.config.Users[index].PasswordHash = string(newHash)
	m.config.Users[index].MustChangePassword = false
	m.config.Users[index].UpdatedAt = time.Now().UTC()
	if err := m.saveLocked(); err != nil {
		m.config.Users[index] = previous
		return Principal{}, err
	}
	updated := m.config.Users[index]
	return Principal{
		Username: updated.Username, DisplayName: updated.DisplayName,
		Provider: ProviderLocal, Role: updated.Role, ClusterIDs: append([]string{}, updated.ClusterIDs...),
	}, nil
}

// CompleteTemporaryPasswordChange replaces the one-time password after the
// user has already authenticated with it. It cannot be used for an ordinary
// password change after the first-login requirement has been cleared.
func (m *Manager) CompleteTemporaryPasswordChange(username, newPassword string) (Principal, error) {
	if err := validateStrongPassword(newPassword); err != nil {
		return Principal{}, err
	}
	m.mu.RLock()
	if !m.configured {
		m.mu.RUnlock()
		return Principal{}, ErrNotConfigured
	}
	index := findUserIndex(m.config.Users, username)
	if index < 0 {
		m.mu.RUnlock()
		return Principal{}, ErrInvalidCredentials
	}
	current := m.config.Users[index]
	m.mu.RUnlock()
	if !current.Active || !current.MustChangePassword {
		return Principal{}, ErrPasswordChangeNotRequired
	}
	if bcrypt.CompareHashAndPassword([]byte(current.PasswordHash), []byte(newPassword)) == nil {
		return Principal{}, ErrPasswordUnchanged
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return Principal{}, fmt.Errorf("hash password: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	index = findUserIndex(m.config.Users, username)
	if index < 0 || m.config.Users[index].PasswordHash != current.PasswordHash {
		return Principal{}, ErrInvalidCredentials
	}
	if !m.config.Users[index].Active || !m.config.Users[index].MustChangePassword {
		return Principal{}, ErrPasswordChangeNotRequired
	}
	previous := m.config.Users[index]
	m.config.Users[index].PasswordHash = string(newHash)
	m.config.Users[index].MustChangePassword = false
	m.config.Users[index].UpdatedAt = time.Now().UTC()
	if err := m.saveLocked(); err != nil {
		m.config.Users[index] = previous
		return Principal{}, err
	}
	updated := m.config.Users[index]
	return Principal{
		Username: updated.Username, DisplayName: updated.DisplayName,
		Provider: ProviderLocal, Role: updated.Role, ClusterIDs: append([]string{}, updated.ClusterIDs...),
	}, nil
}

func (m *Manager) CreateSession(principal Principal) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)
	expiresAt := time.Now().UTC().Add(SessionDuration)
	m.sessionMu.Lock()
	m.pruneSessionsLocked(time.Now().UTC())
	m.sessions[sessionKey(token)] = session{Principal: principal, ExpiresAt: expiresAt}
	m.sessionMu.Unlock()
	return token, expiresAt, nil
}

func (m *Manager) Session(token string) (Principal, bool) {
	key := sessionKey(token)
	m.sessionMu.Lock()
	sessionValue, exists := m.sessions[key]
	if !exists || !sessionValue.ExpiresAt.After(time.Now().UTC()) {
		delete(m.sessions, key)
		m.sessionMu.Unlock()
		return Principal{}, false
	}
	m.sessionMu.Unlock()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.configured || !modeIncludes(m.config.Mode, sessionValue.Principal.Provider) {
		m.DeleteSession(token)
		return Principal{}, false
	}
	principal := sessionValue.Principal
	if principal.Provider == ProviderLocal {
		index := findUserIndex(m.config.Users, principal.Username)
		if index < 0 || !m.config.Users[index].Active {
			m.DeleteSession(token)
			return Principal{}, false
		}
		principal.DisplayName = m.config.Users[index].DisplayName
		principal.Role = m.config.Users[index].Role
		principal.ClusterIDs = append([]string{}, m.config.Users[index].ClusterIDs...)
		principal.MustChangePassword = m.config.Users[index].MustChangePassword
	} else {
		principal.Role = RoleOperator
		principal.ClusterIDs = []string{}
		if containsFold(m.config.LDAP.AdminUsernames, principal.Username) {
			principal.Role = RoleAdmin
		}
	}
	return principal, true
}

func (m *Manager) DeleteSession(token string) {
	m.sessionMu.Lock()
	delete(m.sessions, sessionKey(token))
	m.sessionMu.Unlock()
}

func (m *Manager) load() error {
	file, err := os.Open(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open auth config: %w", err)
	}
	defer file.Close()
	var config persistedConfig
	decoder := json.NewDecoder(io.LimitReader(file, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode auth config: %w", err)
	}
	if config.Version != authConfigVersion || config.ConfiguredAt.IsZero() {
		return errors.New("unsupported or incomplete auth config")
	}
	if config.Mode != ModeLocal && config.Mode != ModeLDAP && config.Mode != ModeBoth {
		return errors.New("invalid stored authentication mode")
	}
	for index, user := range config.Users {
		if _, _, _, err := normalizeUserFields(user.Username, user.DisplayName, user.Role); err != nil || user.PasswordHash == "" {
			return errors.New("invalid stored local user")
		}
		config.Users[index].ClusterIDs = clusterIDsForRole(user.Role, user.ClusterIDs)
	}
	m.config = config
	m.configured = true
	return nil
}

func (m *Manager) saveLocked() error {
	m.config.Version = authConfigVersion
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode auth config: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(m.filePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create auth directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary auth config: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure auth config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write auth config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync auth config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close auth config: %w", err)
	}
	if err := os.Rename(temporaryPath, m.filePath); err != nil {
		cleanup()
		return fmt.Errorf("replace auth config: %w", err)
	}
	if err := os.Chmod(m.filePath, 0o600); err != nil {
		return fmt.Errorf("secure auth config permissions: %w", err)
	}
	return nil
}

func settingsStatus(config persistedConfig) SettingsStatus {
	ldapConfig := config.LDAP
	return SettingsStatus{
		ConfiguredAt: config.ConfiguredAt,
		Mode:         config.Mode,
		LDAP: LDAPSettingsStatus{
			Host: ldapConfig.Host, Port: ldapConfig.Port, TLSMode: ldapConfig.TLSMode,
			CAFile: ldapConfig.CAFile, ServerName: ldapConfig.ServerName, BaseDN: ldapConfig.BaseDN,
			BindDN: ldapConfig.BindDN, BindPasswordConfigured: ldapConfig.BindPassword != "",
			UserFilter: ldapConfig.UserFilter, UserDNTemplate: ldapConfig.UserDNTemplate,
			UsernameAttribute: ldapConfig.UsernameAttribute, DisplayNameAttribute: ldapConfig.DisplayNameAttribute,
			AdminUsernames: append([]string(nil), ldapConfig.AdminUsernames...),
		},
	}
}

func normalizeUserFields(username, displayName, role string) (string, string, string, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	role = strings.ToLower(strings.TrimSpace(role))
	if !usernamePattern.MatchString(username) {
		return "", "", "", fmt.Errorf("%w: 用户名需为 2–64 位字母、数字或 . _ @ -", ErrInvalidInput)
	}
	if displayName == "" {
		displayName = username
	}
	if len([]rune(displayName)) > 100 {
		return "", "", "", fmt.Errorf("%w: 显示名称不能超过 100 个字符", ErrInvalidInput)
	}
	if role == "" {
		role = RoleOperator
	}
	if role != RoleAdmin && role != RoleOperator {
		return "", "", "", fmt.Errorf("%w: 角色必须是管理员或操作员", ErrInvalidInput)
	}
	return username, displayName, role, nil
}

func hashPassword(password string) (string, error) {
	if err := validateStrongPassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

func validateStrongPassword(password string) error {
	if len([]rune(password)) < MinimumPasswordLength {
		return fmt.Errorf("%w: 密码至少需要 %d 个字符", ErrInvalidInput, MinimumPasswordLength)
	}
	if len([]byte(password)) > 72 {
		return fmt.Errorf("%w: 密码不能超过 72 字节", ErrInvalidInput)
	}
	var upper, lower, digit, special bool
	for _, character := range password {
		switch {
		case unicode.IsSpace(character), unicode.IsControl(character):
			return fmt.Errorf("%w: 密码不能包含空格或控制字符", ErrInvalidInput)
		case unicode.IsUpper(character):
			upper = true
		case unicode.IsLower(character):
			lower = true
		case unicode.IsDigit(character):
			digit = true
		case unicode.IsPunct(character), unicode.IsSymbol(character):
			special = true
		}
	}
	if !upper || !lower || !digit || !special {
		return fmt.Errorf("%w: 密码必须同时包含大写字母、小写字母、数字和特殊字符", ErrInvalidInput)
	}
	return nil
}

func generateTemporaryPassword(length int) (string, error) {
	if length < 4 {
		return "", errors.New("temporary password length must be at least 4")
	}
	const uppercase = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const lowercase = "abcdefghijkmnopqrstuvwxyz"
	const digits = "23456789"
	const symbols = "!@#$%&*+-_=.?"
	const all = uppercase + lowercase + digits + symbols
	password := make([]byte, 0, length)
	for _, alphabet := range []string{uppercase, lowercase, digits, symbols} {
		character, err := randomCharacter(alphabet)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for len(password) < length {
		character, err := randomCharacter(all)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for index := len(password) - 1; index > 0; index-- {
		position, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return "", fmt.Errorf("shuffle temporary password: %w", err)
		}
		swap := int(position.Int64())
		password[index], password[swap] = password[swap], password[index]
	}
	return string(password), nil
}

func randomCharacter(alphabet string) (byte, error) {
	position, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
	if err != nil {
		return 0, fmt.Errorf("generate temporary password: %w", err)
	}
	return alphabet[position.Int64()], nil
}

func findUserIndex(users []persistedUser, username string) int {
	for index := range users {
		if strings.EqualFold(users[index].Username, strings.TrimSpace(username)) {
			return index
		}
	}
	return -1
}

func clusterIDsForRole(role string, clusterIDs []string) []string {
	if role == RoleAdmin {
		return []string{}
	}
	unique := make(map[string]struct{}, len(clusterIDs))
	for _, clusterID := range clusterIDs {
		clusterID = strings.TrimSpace(clusterID)
		if clusterID != "" {
			unique[clusterID] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(unique))
	for clusterID := range unique {
		normalized = append(normalized, clusterID)
	}
	sort.Strings(normalized)
	return normalized
}

func activeAdminCount(users []persistedUser) int {
	count := 0
	for _, user := range users {
		if user.Active && user.Role == RoleAdmin {
			count++
		}
	}
	return count
}

func modeIncludes(mode, provider string) bool {
	return mode == ModeBoth || mode == provider
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func sessionKey(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (m *Manager) pruneSessionsLocked(now time.Time) {
	for key, current := range m.sessions {
		if !current.ExpiresAt.After(now) {
			delete(m.sessions, key)
		}
	}
}

func (m *Manager) revokeSessions(provider, username string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	for key, current := range m.sessions {
		if current.Principal.Provider == provider && strings.EqualFold(current.Principal.Username, username) {
			delete(m.sessions, key)
		}
	}
}

func (m *Manager) revokeProviderSessionsLocked(mode string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	for key, current := range m.sessions {
		if !modeIncludes(mode, current.Principal.Provider) {
			delete(m.sessions, key)
		}
	}
}
