package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveToken pins the precedence and, more importantly, that the
// environment variable is read at all. The original defect was not a wrong
// precedence: it was a documented mechanism that no code implemented, so an
// operator who followed the printed advice got an empty token and a failed
// join.
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

// An unreadable --token-file must fail loudly rather than falling through to the
// environment: silently joining with a different credential than the operator
// named is worse than refusing.
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

// TestTokenGuidanceNamesOnlyWiredMechanisms is the guard that would have caught
// the unwired-env-var defect. Every mechanism the worker guidance recommends
// must actually be implemented — the defect was a true-looking sentence with
// nothing behind it.
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
