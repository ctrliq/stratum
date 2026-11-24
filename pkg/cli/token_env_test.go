package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pins the precedence and, more importantly, that the env var is read at all.
func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		flag, file string
		env        string
		want       string
	}{
		{"flag alone", "flag-token", "", "", "flag-token"},
		{"file alone, trimmed", "", file, "", "file-token"},
		{"env alone", "", "", "env-token", "env-token"},
		{"flag beats file", "flag-token", file, "", "flag-token"},
		{"flag beats env", "flag-token", "", "env-token", "flag-token"},
		{"file beats env", "", file, "env-token", "file-token"},
		{"env is trimmed", "", "", "  env-token\n", "env-token"},
		{"nothing set is not an error", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tokenEnvVar, tc.env)
			got, err := resolveToken(tc.flag, tc.file)
			if err != nil {
				t.Fatalf("resolveToken: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveToken(%q, %q) with %s=%q = %q, want %q",
					tc.flag, tc.file, tokenEnvVar, tc.env, got, tc.want)
			}
		})
	}
}

// An unreadable --token-file must fail rather than silently join with a different credential.
func TestResolveTokenUnreadableFileIsAnError(t *testing.T) {
	t.Setenv(tokenEnvVar, "env-token")
	got, err := resolveToken("", filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatalf("accepted an unreadable --token-file, returning %q", got)
	}
	if !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("error %q does not name the flag that failed", err)
	}
}

// Every mechanism the guidance recommends must actually be implemented.
func TestTokenGuidanceNamesOnlyWiredMechanisms(t *testing.T) {
	// The env var the guidance names must be the one resolveToken reads.
	t.Setenv(tokenEnvVar, "wired")
	got, err := resolveToken("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wired" {
		t.Fatalf("guidance names $%s but resolveToken does not read it (got %q)", tokenEnvVar, got)
	}

	// The other mechanism it names, --token-file, must be a real flag on `up`.
	if f := newUpCommand(rootConfig{}).Flags().Lookup("token-file"); f == nil {
		t.Error("guidance recommends --token-file but `stratum up` defines no such flag")
	}
}
