package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigResolvesTheProfile(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) (*ClusterConfig, error) {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}

	for _, tc := range []struct {
		name    string
		body    string
		want    string
		wantCIS bool
		wantErr string
	}{
		{"no profile", "spec: {}\n", "", false, ""},
		{"spec.profile cis", "spec:\n  profile: cis\n", "cis", true, ""},
		{"legacy spec.cis", "spec:\n  cis: true\n", "cis", true, ""},
		{"both spellings agree", "spec:\n  profile: cis\n  cis: true\n", "cis", true, ""},
		{"another profile", "spec:\n  profile: stig\n", "stig", false, ""},
		{"spec.cis false with a profile", "spec:\n  profile: stig\n  cis: false\n", "stig", false, ""},
		{"two profiles", "spec:\n  profile: stig\n  cis: true\n", "", false, "two profiles"},
		{"not a label", "spec:\n  profile: CIS Level 1\n", "", false, "DNS-1123"},
	} {
		cfg, err := load(tc.body)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: LoadConfig = %v, want an error containing %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: LoadConfig = %v", tc.name, err)
			continue
		}
		if cfg.ProfileName() != tc.want || cfg.Spec.Profile != tc.want || cfg.Spec.CIS != tc.wantCIS {
			t.Errorf("%s: profile %q (spec.profile %q, spec.cis %v); want %q, cis %v",
				tc.name, cfg.ProfileName(), cfg.Spec.Profile, cfg.Spec.CIS, tc.want, tc.wantCIS)
		}
	}

	cfg := &ClusterConfig{}
	cfg.SetProfile("cis")
	if cfg.ProfileName() != "cis" || !cfg.Spec.CIS {
		t.Errorf("SetProfile(cis) = profile %q, cis %v", cfg.ProfileName(), cfg.Spec.CIS)
	}
	cfg.SetProfile("")
	if cfg.ProfileName() != "" || cfg.Spec.CIS {
		t.Errorf("SetProfile(\"\") = profile %q, cis %v", cfg.ProfileName(), cfg.Spec.CIS)
	}
}
