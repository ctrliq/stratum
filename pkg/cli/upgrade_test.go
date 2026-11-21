package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestUpgradeClusterRefusesAnUnstampedBuildBeforeItsOwnGates(t *testing.T) {
	old := version.K8sVersion
	version.K8sVersion = "unknown"
	t.Cleanup(func() { version.K8sVersion = old })
	oldAdmin := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "admin.conf")
	t.Cleanup(func() { version.AdminConf = oldAdmin })

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"before the confirmation gate", []string{"upgrade", "--cluster"}},
		{"with every orchestrator precondition satisfied", []string{"upgrade", "--cluster", "--yes",
			"--from", filepath.Join(t.TempDir(), "stratum-next"),
			"--target", "v1.37.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRootCommand()
			root.SetArgs(tc.args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			assertStampRefusal(t, root.Execute())
		})
	}
}

// nodeUpgradeArgv and friends are unexported in internal/upgrade, so the argv is written by hand
func TestUpgradeCommandAcceptsTheOrchestratorsArgv(t *testing.T) {
	const (
		from   = "/var/lib/stratum/upgrade/stratum-next"
		target = "v1.37.1"
		build  = "0123abcd"
		sum    = "feedface"
	)
	for _, tc := range []struct {
		name             string
		argv             []string
		wantSoak         int
		wantSkipSnapshot bool
		wantCheck        bool
		wantImportImages bool
	}{
		{
			name: "per-node upgrade Job",
			argv: []string{"--from", from, "--target", target,
				"--expect-build", build, "--expect-sha256", sum,
				"--soak", "5", "--skip-snapshot"},
			wantSoak:         5,
			wantSkipSnapshot: true,
		},
		{
			name: "build-check Job",
			argv: []string{"--from", from, "--target", target,
				"--expect-build", build, "--expect-sha256", sum,
				"--check"},
			wantCheck: true,
			// the check Job sends no --soak; it must land on the command's default, not a zero
			wantSoak: 15,
		},
		{
			name: "image-import Job",
			argv: []string{"--from", from, "--target", target,
				"--expect-build", build, "--expect-sha256", sum,
				"--import-images"},
			wantImportImages: true,
			wantSoak:         15,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newUpgradeCommand()
			if err := cmd.ParseFlags(tc.argv); err != nil {
				t.Fatalf("the upgrade command rejected the orchestrator's argv %q: %v", tc.argv, err)
			}
			fs := cmd.Flags()
			for _, check := range []struct{ flag, want string }{
				{"from", from},
				{"target", target},
				{"expect-build", build},
				{"expect-sha256", sum},
			} {
				got, err := fs.GetString(check.flag)
				if err != nil {
					t.Fatalf("read --%s: %v", check.flag, err)
				}
				if got != check.want {
					t.Errorf("--%s = %q, want %q", check.flag, got, check.want)
				}
			}
			if got, err := fs.GetInt("soak"); err != nil || got != tc.wantSoak {
				t.Errorf("--soak = %d (%v), want %d", got, err, tc.wantSoak)
			}
			if got, err := fs.GetBool("skip-snapshot"); err != nil || got != tc.wantSkipSnapshot {
				t.Errorf("--skip-snapshot = %v (%v), want %v", got, err, tc.wantSkipSnapshot)
			}
			if got, err := fs.GetBool("check"); err != nil || got != tc.wantCheck {
				t.Errorf("--check = %v (%v), want %v", got, err, tc.wantCheck)
			}
			if got, err := fs.GetBool("import-images"); err != nil || got != tc.wantImportImages {
				t.Errorf("--import-images = %v (%v), want %v", got, err, tc.wantImportImages)
			}
		})
	}
}

func TestUpgradeImportImagesRefusesTheModesItCannotCombineWith(t *testing.T) {
	for _, mode := range []string{"--cluster", "--check", "--dry-run"} {
		t.Run(mode, func(t *testing.T) {
			root := NewRootCommand()
			root.SetArgs([]string{"upgrade", "--import-images", mode, "--yes", "--from", filepath.Join(t.TempDir(), "stratum-next")})
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "cannot be combined with --cluster, --check or --dry-run") {
				t.Errorf("upgrade --import-images %s = %v, want it refused", mode, err)
			}
		})
	}
}

func TestUpgradeImportImagesIsHidden(t *testing.T) {
	f := newUpgradeCommand().Flags().Lookup("import-images")
	if f == nil || !f.Hidden {
		t.Errorf("--import-images = %+v, want a hidden flag", f)
	}
}
