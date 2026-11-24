package host

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fakeProcMountinfo(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procMountinfo
	procMountinfo = p
	t.Cleanup(func() { procMountinfo = old })
}

func fakeMount(t *testing.T, answer func(args []string) (string, error)) *[][]string {
	t.Helper()
	var calls [][]string
	old := runMount
	runMount = func(args ...string) (string, error) {
		calls = append(calls, args)
		return answer(args)
	}
	t.Cleanup(func() { runMount = old })
	return &calls
}

const sharedRootMountinfo = "30 1 0:24 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"

const privateRootMountinfo = "30 1 0:24 / / rw,relatime - ext4 /dev/sda1 rw\n"

const slaveRootMountinfo = "30 1 0:24 / / rw,relatime master:36 - ext4 /dev/sda1 rw\n"

// The kernel writes each tag as its own optional field, and shared wins.
const slaveSharedRootMountinfo = "30 1 0:24 / / rw,relatime shared:102 master:36 - ext4 /dev/sda1 rw\n"

// A source pin: no external test can stand in for Prereqs's host stack.
func TestPrereqsCallsEnsureRootShared(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "host.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing host.go: %v", err)
	}
	var prereqs *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "Prereqs" && d.Recv == nil {
			prereqs = d
		}
	}
	if prereqs == nil {
		t.Fatal("host.go declares no func Prereqs: the host preparation runs there")
	}
	calls := false
	ast.Inspect(prereqs.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, isIdent := call.Fun.(*ast.Ident); isIdent && id.Name == "EnsureRootShared" {
			calls = true
		}
		return true
	})
	if !calls {
		t.Error("Prereqs must call EnsureRootShared: a private / leaves Cilium's host-to-container bind mounts refused and the node NotReady")
	}
}

func TestEnsureRootSharedMakesAPrivateRootShared(t *testing.T) {
	fakeProcMountinfo(t, privateRootMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if want := [][]string{{"--make-rshared", "/"}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("mount calls = %v, want %v", *calls, want)
	}
	want := "  made / a shared mount (mount --make-rshared /): containerd refuses host-path mounts with propagation, such as Cilium's, on a private /\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestEnsureRootSharedLeavesASharedRootAlone(t *testing.T) {
	fakeProcMountinfo(t, sharedRootMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if calls := *calls; len(calls) != 0 {
		t.Errorf("mount called on a shared root: %v", calls)
	}
	if out != "" {
		t.Errorf("output on a shared root = %q, want none", out)
	}
}

func TestEnsureRootSharedLeavesASlaveRootAlone(t *testing.T) {
	fakeProcMountinfo(t, slaveRootMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if calls := *calls; len(calls) != 0 {
		t.Errorf("mount called on a slave root: %v", calls)
	}
	if out != "" {
		t.Errorf("output on a slave root = %q, want none", out)
	}
}

func TestEnsureRootSharedLeavesASlaveSharedRootAlone(t *testing.T) {
	if private, ok := rootPrivate(slaveSharedRootMountinfo); !ok || private {
		t.Errorf("rootPrivate = %v, %v; want private = false, true", private, ok)
	}
	fakeProcMountinfo(t, slaveSharedRootMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if calls := *calls; len(calls) != 0 {
		t.Errorf("mount called on a slave-shared root: %v", calls)
	}
	if out != "" {
		t.Errorf("output on a slave-shared root = %q, want none", out)
	}
}

func TestEnsureRootSharedWarnsAndContinuesWhenTheMountFails(t *testing.T) {
	fakeProcMountinfo(t, privateRootMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) {
		return "mount: /: permission denied", errors.New("exit status 1")
	})
	out := quiet(t, EnsureRootShared)
	if want := [][]string{{"--make-rshared", "/"}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("mount calls = %v, want %v", *calls, want)
	}
	want := "  WARN: mount --make-rshared / failed: exit status 1: mount: /: permission denied (containerd refuses host-path mounts with propagation, such as Cilium's, on a private /)\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestEnsureRootSharedWarnsWhenMountinfoIsUnreadable(t *testing.T) {
	fakeProcMountinfo(t, "")
	if err := os.Remove(procMountinfo); err != nil {
		t.Fatal(err)
	}
	_, readErr := os.ReadFile(procMountinfo)
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if calls := *calls; len(calls) != 0 {
		t.Errorf("mount called with unreadable mountinfo: %v", calls)
	}
	want := "  WARN: could not read " + procMountinfo + " to check whether / is a shared mount: " + readErr.Error() + "\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestEnsureRootSharedWarnsWhenMountinfoHasNoRootLine(t *testing.T) {
	fakeProcMountinfo(t, "30 1 0:24 /mnt/data /mnt/data rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	calls := fakeMount(t, func(args []string) (string, error) { return "", nil })
	out := quiet(t, EnsureRootShared)
	if calls := *calls; len(calls) != 0 {
		t.Errorf("mount called with no / line in mountinfo: %v", calls)
	}
	want := "  WARN: " + procMountinfo + " has no / mount; cannot tell whether / is a shared mount\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// The mount point is the fifth field, not the mount root (the fourth).
func TestRootPrivateReadsTheMountPointNotTheMountRoot(t *testing.T) {
	mi := "30 1 0:24 / / rw,relatime - ext4 /dev/sda1 rw\n" +
		"45 30 0:40 / /sys/fs/bpf rw,relatime shared:7 - bpf bpf rw\n"
	if private, ok := rootPrivate(mi); !ok || !private {
		t.Errorf("rootPrivate = %v, %v; want private = true, true", private, ok)
	}
}

func TestRootPrivateTakesTheTopmostRootMount(t *testing.T) {
	const (
		privateBase = "30 1 0:24 / / rw,relatime - ext4 /dev/sda1 rw\n"
		sharedBase  = "30 1 0:24 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"
		privateTop  = "60 30 0:50 / / rw,relatime - overlay overlay rw\n"
		sharedTop   = "60 30 0:50 / / rw,relatime shared:9 - overlay overlay rw\n"
	)
	for _, c := range []struct {
		mi      string
		private bool
	}{
		{privateBase + sharedTop, false},
		{sharedBase + privateTop, true},
		{sharedTop + privateBase, false},
		{privateTop + sharedBase, true},
		// The root of the mount tree names itself as its parent.
		{"1 1 0:2 / / rw - rootfs rootfs rw\n", true},
		{"1 1 0:2 / / rw shared:1 - rootfs rootfs rw\n", false},
		{"1 1 0:2 / / rw - rootfs rootfs rw\n" + "60 1 0:50 / / rw,relatime shared:9 - overlay overlay rw\n", false},
	} {
		if private, ok := rootPrivate(c.mi); !ok || private != c.private {
			t.Errorf("rootPrivate(%q) = %v, %v; want private = %v, true", c.mi, private, ok, c.private)
		}
	}
}
