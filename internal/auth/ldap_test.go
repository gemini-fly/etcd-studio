package auth

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLDAPTLSConfigRejectsCAFileOutsideManagedDirectory(t *testing.T) {
	t.Parallel()
	managedDirectory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "ldap-ca.pem")
	_, err := ldapTLSConfig(ldapSettings{Host: "ldap.internal", CAFile: outside}, managedDirectory)
	if err == nil || !strings.Contains(err.Error(), "must be inside") {
		t.Fatalf("outside LDAP CA error = %v", err)
	}
}
