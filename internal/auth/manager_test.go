package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerCreatesOneTimeTemporaryAdminAndPersistsOnlyHash(t *testing.T) {
	t.Parallel()
	filePath := filepath.Join(t.TempDir(), "auth.json")
	manager, err := NewManager(filePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Status(nil).Configured {
		t.Fatal("new manager unexpectedly configured")
	}
	temporaryPassword, created, err := manager.EnsureTemporaryAdmin()
	if err != nil || !created {
		t.Fatalf("temporary admin created = %v, err = %v", created, err)
	}
	if err := validateStrongPassword(temporaryPassword); err != nil {
		t.Fatalf("generated temporary password is not strong: %v", err)
	}
	if password, created, err := manager.EnsureTemporaryAdmin(); err != nil || created || password != "" {
		t.Fatalf("second ensure = password %q, created %v, err %v", password, created, err)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(temporaryPassword)) || !bytes.Contains(data, []byte("password_hash")) {
		t.Fatalf("auth file did not contain only a password hash: %s", data)
	}
	principal, err := manager.AuthenticateLocal("ADMIN", temporaryPassword)
	if err != nil || principal.Username != "admin" || !principal.IsAdmin() || !principal.MustChangePassword {
		t.Fatalf("principal = %#v, err = %v", principal, err)
	}
	if _, err := manager.AuthenticateLocal("admin", "wrong-password"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password error = %v", err)
	}
	reloaded, err := NewManager(filePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Status(nil).Configured {
		t.Fatal("reloaded manager lost configuration")
	}
	reloadedPrincipal, err := reloaded.AuthenticateLocal("admin", temporaryPassword)
	if err != nil || !reloadedPrincipal.MustChangePassword {
		t.Fatalf("reloaded principal = %#v, err = %v", reloadedPrincipal, err)
	}
}

func TestChangePasswordEnforcesPolicyAndClearsTemporaryFlag(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPassword, _, err := manager.EnsureTemporaryAdmin()
	if err != nil {
		t.Fatal(err)
	}
	principal, err := manager.AuthenticateLocal("admin", temporaryPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := manager.CreateSession(principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, weak := range []string{
		"Short-1!", "onlylowercasepassword1!", "ONLYUPPERCASEPASSWORD1!", "No-Digits-Password!", "NoSpecialPassword123",
	} {
		if _, err := manager.ChangePassword("admin", temporaryPassword, weak); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("weak password %q error = %v", weak, err)
		}
	}
	if _, err := manager.ChangePassword("admin", "Wrong-Current-2026!", "Permanent-Admin-2026!"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password error = %v", err)
	}
	if _, err := manager.ChangePassword("admin", temporaryPassword, temporaryPassword); !errors.Is(err, ErrPasswordUnchanged) {
		t.Fatalf("unchanged password error = %v", err)
	}
	updated, err := manager.ChangePassword("admin", temporaryPassword, "Permanent-Admin-2026!")
	if err != nil || updated.MustChangePassword {
		t.Fatalf("updated principal = %#v, err = %v", updated, err)
	}
	if _, err := manager.AuthenticateLocal("admin", temporaryPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("temporary password remained valid: %v", err)
	}
	if current, ok := manager.Session(token); !ok || current.MustChangePassword {
		t.Fatalf("existing session = %#v, valid = %v", current, ok)
	}
}

func TestPasswordPolicyRequiresTenCharacters(t *testing.T) {
	t.Parallel()
	if err := validateStrongPassword("Aa1!xxxxx"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nine-character password error = %v", err)
	}
	if err := validateStrongPassword("Aa1!xxxxxx"); err != nil {
		t.Fatalf("ten-character password error = %v", err)
	}
}

func TestManagerMaintainsAnActiveAdminAndRevokesUpdatedUserSessions(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := manager.Bootstrap(LocalUserInput{Username: "admin", Password: "Administrator-Password-2026!", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	active := true
	operator, err := manager.CreateUser(LocalUserInput{Username: "operator", DisplayName: "值班人员", Password: "Operator-Password-2026!", Role: RoleOperator, Active: &active})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := manager.CreateSession(Principal{Username: operator.Username, DisplayName: operator.DisplayName, Provider: ProviderLocal, Role: operator.Role})
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := manager.UpdateUser(operator.Username, LocalUserInput{DisplayName: operator.DisplayName, Role: operator.Role, Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Session(token); ok {
		t.Fatal("disabled user session remained valid")
	}
	if err := manager.DeleteUser(admin.Username); err != ErrLastAdmin {
		t.Fatalf("delete last admin error = %v", err)
	}
}

func TestLocalUserClusterPermissionsPersistAndDefaultToEmpty(t *testing.T) {
	t.Parallel()
	filePath := filepath.Join(t.TempDir(), "auth.json")
	manager, err := NewManager(filePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Bootstrap(LocalUserInput{
		Username: "admin", Password: "Administrator-Password-2026!", Role: RoleAdmin,
		ClusterIDs: []string{"ignored-for-admin"},
	}); err != nil {
		t.Fatal(err)
	}
	active := true
	operator, err := manager.CreateUser(LocalUserInput{
		Username: "operator", DisplayName: "值班人员", Password: "Operator-Password-2026!",
		Role: RoleOperator, Active: &active, ClusterIDs: []string{"cluster-b", " cluster-a ", "cluster-b", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(operator.ClusterIDs, ","); got != "cluster-a,cluster-b" {
		t.Fatalf("normalized cluster IDs = %q", got)
	}
	empty, err := manager.CreateUser(LocalUserInput{
		Username: "empty-operator", Password: "Empty-Operator-2026!", Role: RoleOperator, Active: &active,
	})
	if err != nil || empty.ClusterIDs == nil || len(empty.ClusterIDs) != 0 {
		t.Fatalf("empty operator = %#v, err = %v", empty, err)
	}
	principal, err := manager.AuthenticateLocal("operator", "Operator-Password-2026!")
	if err != nil || !principal.CanAccessCluster("cluster-a") || principal.CanAccessCluster("cluster-c") {
		t.Fatalf("operator principal = %#v, err = %v", principal, err)
	}
	admin, err := manager.AuthenticateLocal("admin", "Administrator-Password-2026!")
	if err != nil || !admin.CanAccessCluster("any-cluster") || len(admin.ClusterIDs) != 0 {
		t.Fatalf("admin principal = %#v, err = %v", admin, err)
	}

	reloaded, err := NewManager(filePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reloadedPrincipal, err := reloaded.AuthenticateLocal("operator", "Operator-Password-2026!")
	if err != nil || strings.Join(reloadedPrincipal.ClusterIDs, ",") != "cluster-a,cluster-b" {
		t.Fatalf("reloaded principal = %#v, err = %v", reloadedPrincipal, err)
	}
	if err := reloaded.RemoveClusterPermissions("cluster-a"); err != nil {
		t.Fatal(err)
	}
	prunedPrincipal, err := reloaded.AuthenticateLocal("operator", "Operator-Password-2026!")
	if err != nil || strings.Join(prunedPrincipal.ClusterIDs, ",") != "cluster-b" {
		t.Fatalf("pruned principal = %#v, err = %v", prunedPrincipal, err)
	}
}

func TestSettingsNeverReturnLDAPBindPassword(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(filepath.Join(t.TempDir(), "auth.json"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Bootstrap(LocalUserInput{Username: "admin", Password: "Administrator-Password-2026!", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	password := "ldap-service-secret"
	settings, err := manager.UpdateSettings(SettingsInput{Mode: ModeBoth, LDAP: LDAPSettingsInput{
		Host: "ldap.internal", Port: 389, TLSMode: LDAPTLSStartTLS, BaseDN: "dc=example,dc=com",
		BindDN: "cn=reader,dc=example,dc=com", BindPassword: &password,
		UserFilter: "(uid={{username}})", AdminUsernames: []string{"ldap-admin"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !settings.LDAP.BindPasswordConfigured {
		t.Fatal("bind password was not reported as configured")
	}
	encoded := []byte(settings.LDAP.Host + settings.LDAP.BindDN + settings.LDAP.UserFilter)
	if bytes.Contains(encoded, []byte(password)) {
		t.Fatal("settings leaked LDAP bind password")
	}
}
