package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadableVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.37.2-stratum.0":          "1.37.2",
		"v1.36.6-stratum.0-rc.2":     "1.36.6 (rc.2)",
		"v1.36.6-stratum-pro.0-rc.2": "1.36.6 (rc.2)",
		"v1.37.2-stratum-pro.1":      "1.37.2",
		"v0.0.0-dev":                 "0.0.0-dev",
		"1.2.3":                      "1.2.3",
	} {
		if got := readableVersion(in); got != want {
			t.Errorf("readableVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadlineNamesTheProduct(t *testing.T) {
	cfg := rootConfig{displayVersion: "v1.37.2-stratum.0"}
	if got, want := cfg.headline(), "CIQ Stratum 1.37.2"; got != want {
		t.Errorf("default headline = %q, want %q", got, want)
	}
	WithProductName("Stratum Pro")(&cfg)
	WithProductName("")(&cfg) // empty leaves the name already set
	if got, want := cfg.headline(), "CIQ Stratum Pro 1.37.2"; got != want {
		t.Errorf("headline with a product name = %q, want %q", got, want)
	}
}

// TestHelpGroupsEveryEngineCommand: every command NewRootCommand adds is
// listed under a named group, so none falls into "Additional Commands",
// which is left for importers' own commands.
func TestHelpGroupsEveryEngineCommand(t *testing.T) {
	root := NewRootCommand(WithProductName("Stratum Pro"))
	for _, c := range root.Commands() {
		if c.GroupID == "" && c.Name() != "help" && c.Name() != "completion" {
			t.Errorf("command %q has no help group", c.Name())
		}
	}
	if !strings.HasPrefix(root.Long, "CIQ Stratum Pro ") {
		t.Errorf("help does not open with the product's name:\n%s", root.Long)
	}
	if !strings.Contains(root.Long, "sudo /usr/local/bin/stratum up") {
		t.Errorf("help does not show the first command to run, as root and by full path:\n%s", root.Long)
	}
}

func TestReadableTTL(t *testing.T) {
	for in, want := range map[time.Duration]string{
		24 * time.Hour:   "24 hours",
		time.Hour:        "1 hour",
		90 * time.Minute: "90 minutes",
		time.Minute:      "1 minute",
		90 * time.Second: "1m30s",
	} {
		if got := readableTTL(in); got != want {
			t.Errorf("readableTTL(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSudoCommandQuotesOnlyWhatNeedsIt(t *testing.T) {
	got := sudoCommand([]string{"up", "--config", "/etc/my config.yaml"})
	if !strings.HasPrefix(got, "sudo ") || !strings.HasSuffix(got, " up --config '/etc/my config.yaml'") {
		t.Errorf("sudoCommand = %q", got)
	}
}

func TestNeedsRoot(t *testing.T) {
	other := errors.New("boom")
	if got := needsRoot(other); got != other {
		t.Errorf("a failure that is not a permission error changed: %v", got)
	}
	perm := fmt.Errorf("open node-start lock /run/stratum-start.lock: %w", fs.ErrPermission)
	got := needsRoot(perm)
	if os.Geteuid() == 0 {
		if got != perm {
			t.Errorf("as root, a permission error changed: %v", got)
		}
		return
	}
	if !errors.Is(got, fs.ErrPermission) {
		t.Errorf("the original error is lost: %v", got)
	}
	for _, want := range []string{"needs root", "sudo ", "node-start lock"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("message lacks %q:\n%v", want, got)
		}
	}
}
