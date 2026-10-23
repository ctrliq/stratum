package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// An MCP rollout has each node check the staged binary before running it; a
// CLI rollout runs --from as given.
func TestStagedUpgradeArgvVerifiesOnlyWhenAsked(t *testing.T) {
	o := ClusterOptions{From: "/var/lib/stratum/upgrade/stratum", Target: "v1.37.2", expectSHA256: "abc", arch: "amd64"}
	cli := stagedUpgradeArgv(o, Node{Arch: "amd64"})
	if slices.Contains(cli, "/bin/sh") {
		t.Errorf("a CLI rollout runs the verify script: %q", cli)
	}
	o.VerifyStagedOnNode = true
	same := stagedUpgradeArgv(o, Node{Arch: "amd64"})
	i := slices.Index(same, "/bin/sh")
	if i < 0 || same[i+2] != verifyStagedScript {
		t.Fatalf("an MCP rollout does not run the verify script first: %q", same)
	}
	if got := same[i+4 : i+7]; !slices.Equal(got, []string{o.From, "0", "abc"}) {
		t.Errorf("verify arguments %q, want [path 0 sha256]", got)
	}
	if got := same[i+7 : i+9]; !slices.Equal(got, []string{o.From, "upgrade"}) {
		t.Errorf("after the check the node runs %q, want the staged binary's upgrade", got)
	}
	other := stagedUpgradeArgv(o, Node{Arch: "arm64"})
	j := slices.Index(other, "/bin/sh")
	if other[j+6] != "" {
		t.Errorf("a node of another architecture is held to this build's sha256 %q", other[j+6])
	}
}

// The script, run as a node would: it execs the binary only when the file and
// its directory are what an operator staged.
func TestVerifyStagedScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script uses GNU stat, as on the nodes")
	}
	owner := strconv.Itoa(os.Getuid())
	stage := func(t *testing.T) (dir, bin, sum string) {
		dir = filepath.Join(t.TempDir(), "upgrade")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		bin = filepath.Join(dir, "stratum")
		body := []byte("#!/bin/sh\necho ran \"$@\"\n")
		if err := os.WriteFile(bin, body, 0o755); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(body)
		return dir, bin, hex.EncodeToString(h[:])
	}
	run := func(path, want string) (string, error) {
		out, err := exec.Command("/bin/sh", "-c", verifyStagedScript, "stratum-verify-staged",
			path, owner, want, path, "upgrade").CombinedOutput()
		return string(out), err
	}

	_, bin, sum := stage(t)
	if out, err := run(bin, sum); err != nil || !strings.Contains(out, "ran upgrade") {
		t.Errorf("a staged binary with the right hash was not run: %v\n%s", err, out)
	}
	if out, err := run(bin, ""); err != nil || !strings.Contains(out, "ran upgrade") {
		t.Errorf("a staged binary with no hash to check was not run: %v\n%s", err, out)
	}
	for name, setup := range map[string]func(t *testing.T) (string, string){
		"wrong hash": func(t *testing.T) (string, string) { _, b, _ := stage(t); return b, strings.Repeat("0", 64) },
		"group-writable file": func(t *testing.T) (string, string) {
			_, b, s := stage(t)
			if err := os.Chmod(b, 0o775); err != nil {
				t.Fatal(err)
			}
			return b, s
		},
		"world-writable directory": func(t *testing.T) (string, string) {
			d, b, s := stage(t)
			if err := os.Chmod(d, 0o777); err != nil {
				t.Fatal(err)
			}
			return b, s
		},
		"symlinked binary": func(t *testing.T) (string, string) {
			d, b, s := stage(t)
			l := filepath.Join(d, "link")
			if err := os.Symlink(b, l); err != nil {
				t.Fatal(err)
			}
			return l, s
		},
		"relative path": func(t *testing.T) (string, string) { return "stratum", "" },
	} {
		t.Run(name, func(t *testing.T) {
			path, want := setup(t)
			out, err := run(path, want)
			if err == nil || strings.Contains(out, "ran upgrade") {
				t.Errorf("the binary ran: %v\n%s", err, out)
			}
			if !strings.Contains(out, "refusing the staged upgrade binary") {
				t.Errorf("no refusal reason:\n%s", out)
			}
		})
	}
}
