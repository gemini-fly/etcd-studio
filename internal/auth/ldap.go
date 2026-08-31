package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
)

const (
	LDAPTLSStartTLS = "starttls"
	LDAPTLSLDAPS    = "ldaps"
	LDAPTLSPlain    = "plain"
)

func (m *Manager) AuthenticateLDAP(ctx context.Context, username, password string) (Principal, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return Principal{}, ErrInvalidCredentials
	}
	m.mu.RLock()
	if !m.configured {
		m.mu.RUnlock()
		return Principal{}, ErrNotConfigured
	}
	if !modeIncludes(m.config.Mode, ProviderLDAP) {
		m.mu.RUnlock()
		return Principal{}, ErrProviderDisabled
	}
	config := m.config.LDAP
	m.mu.RUnlock()

	connection, err := dialLDAP(ctx, config, m.timeout)
	if err != nil {
		return Principal{}, fmt.Errorf("connect LDAP: %w", err)
	}
	defer connection.Close()
	userDN := ""
	displayName := username
	if config.UserDNTemplate != "" {
		userDN = strings.ReplaceAll(config.UserDNTemplate, "{{username}}", ldap.EscapeDN(username))
	} else {
		if config.BindDN != "" {
			if err := connection.Bind(config.BindDN, config.BindPassword); err != nil {
				return Principal{}, fmt.Errorf("LDAP service bind: %w", err)
			}
		}
		filter := strings.ReplaceAll(config.UserFilter, "{{username}}", ldap.EscapeFilter(username))
		request := ldap.NewSearchRequest(
			config.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2,
			max(1, int(m.timeout.Seconds())), false, filter,
			[]string{config.UsernameAttribute, config.DisplayNameAttribute}, nil,
		)
		result, err := connection.Search(request)
		if err != nil {
			return Principal{}, fmt.Errorf("LDAP user search: %w", err)
		}
		if len(result.Entries) != 1 {
			return Principal{}, ErrInvalidCredentials
		}
		userDN = result.Entries[0].DN
		if value := strings.TrimSpace(result.Entries[0].GetAttributeValue(config.DisplayNameAttribute)); value != "" {
			displayName = value
		}
	}
	if err := connection.Bind(userDN, password); err != nil {
		return Principal{}, ErrInvalidCredentials
	}
	role := RoleOperator
	if containsFold(config.AdminUsernames, username) {
		role = RoleAdmin
	}
	return Principal{Username: username, DisplayName: displayName, Provider: ProviderLDAP, Role: role, ClusterIDs: []string{}}, nil
}

func (m *Manager) TestLDAP(ctx context.Context, input LDAPSettingsInput) error {
	m.mu.RLock()
	current := m.config.LDAP
	m.mu.RUnlock()
	config, err := normalizeLDAPSettings(input, current, true)
	if err != nil {
		return err
	}
	connection, err := dialLDAP(ctx, config, m.timeout)
	if err != nil {
		return err
	}
	defer connection.Close()
	if config.BindDN != "" {
		if err := connection.Bind(config.BindDN, config.BindPassword); err != nil {
			return fmt.Errorf("LDAP bind failed: %w", err)
		}
	}
	return nil
}

func normalizeLDAPSettings(input LDAPSettingsInput, current ldapSettings, required bool) (ldapSettings, error) {
	config := ldapSettings{
		Host: strings.TrimSpace(input.Host), Port: input.Port,
		TLSMode: strings.ToLower(strings.TrimSpace(input.TLSMode)), CAFile: strings.TrimSpace(input.CAFile),
		ServerName: strings.TrimSpace(input.ServerName), BaseDN: strings.TrimSpace(input.BaseDN),
		BindDN: strings.TrimSpace(input.BindDN), UserFilter: strings.TrimSpace(input.UserFilter),
		UserDNTemplate: strings.TrimSpace(input.UserDNTemplate), UsernameAttribute: strings.TrimSpace(input.UsernameAttribute),
		DisplayNameAttribute: strings.TrimSpace(input.DisplayNameAttribute),
	}
	if input.BindPassword == nil {
		config.BindPassword = current.BindPassword
	} else {
		config.BindPassword = *input.BindPassword
	}
	seenAdmins := make(map[string]struct{})
	for _, username := range input.AdminUsernames {
		username = strings.TrimSpace(username)
		if username == "" {
			continue
		}
		key := strings.ToLower(username)
		if _, exists := seenAdmins[key]; exists {
			continue
		}
		seenAdmins[key] = struct{}{}
		config.AdminUsernames = append(config.AdminUsernames, username)
	}
	if !required && config.Host == "" {
		return current, nil
	}
	if config.TLSMode == "" {
		config.TLSMode = LDAPTLSStartTLS
	}
	if config.Port == 0 {
		if config.TLSMode == LDAPTLSLDAPS {
			config.Port = 636
		} else {
			config.Port = 389
		}
	}
	if config.UserFilter == "" {
		config.UserFilter = "(uid={{username}})"
	}
	if config.UsernameAttribute == "" {
		config.UsernameAttribute = "uid"
	}
	if config.DisplayNameAttribute == "" {
		config.DisplayNameAttribute = "cn"
	}
	if config.Host == "" || config.Port < 1 || config.Port > 65535 {
		return ldapSettings{}, fmt.Errorf("%w: LDAP 地址和有效端口不能为空", ErrInvalidInput)
	}
	if config.TLSMode != LDAPTLSStartTLS && config.TLSMode != LDAPTLSLDAPS && config.TLSMode != LDAPTLSPlain {
		return ldapSettings{}, fmt.Errorf("%w: LDAP 加密方式无效", ErrInvalidInput)
	}
	if config.UserDNTemplate == "" {
		if config.BaseDN == "" || !strings.Contains(config.UserFilter, "{{username}}") {
			return ldapSettings{}, fmt.Errorf("%w: LDAP 搜索模式需要 Base DN，用户过滤器必须包含 {{username}}", ErrInvalidInput)
		}
	} else if !strings.Contains(config.UserDNTemplate, "{{username}}") {
		return ldapSettings{}, fmt.Errorf("%w: 用户 DN 模板必须包含 {{username}}", ErrInvalidInput)
	}
	if config.BindDN != "" && config.BindPassword == "" {
		return ldapSettings{}, fmt.Errorf("%w: 配置 Bind DN 时必须填写 Bind 密码", ErrInvalidInput)
	}
	return config, nil
}

func dialLDAP(ctx context.Context, config ldapSettings, timeout time.Duration) (*ldap.Conn, error) {
	tlsConfig, err := ldapTLSConfig(config)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}
	scheme := "ldap"
	options := []ldap.DialOpt{ldap.DialWithDialer(dialer)}
	if config.TLSMode == LDAPTLSLDAPS {
		scheme = "ldaps"
		options = append(options, ldap.DialWithTLSConfig(tlsConfig))
	}
	address := scheme + "://" + net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	connection, err := ldap.DialURL(address, options...)
	if err != nil {
		return nil, err
	}
	connection.SetTimeout(timeout)
	if config.TLSMode == LDAPTLSStartTLS {
		if err := connection.StartTLS(tlsConfig); err != nil {
			connection.Close()
			return nil, err
		}
	}
	return connection, nil
}

func ldapTLSConfig(config ldapSettings) (*tls.Config, error) {
	serverName := config.ServerName
	if serverName == "" {
		serverName = config.Host
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if config.CAFile == "" {
		return tlsConfig, nil
	}
	data, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read LDAP CA file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("LDAP CA file does not contain a valid PEM certificate")
	}
	tlsConfig.RootCAs = roots
	return tlsConfig, nil
}
