package upgrade

// Nodes on the old release must hold the new images before the first control plane rolls out.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/nodejob"
)

type jobLog struct {
	mu     sync.Mutex
	events []string
}

func (l *jobLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *jobLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func recordJobs(t *testing.T, importErr map[string]error) *jobLog {
	t.Helper()
	fastOrchestrator(t, nil)
	log := &jobLog{}
	swap(t, &runImageImport, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		log.add("import:" + s.Node)
		return importErr[s.Node]
	})
	swap(t, &runNodeJob, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		log.add("upgrade:" + s.Node)
		return nil
	})
	return log
}

func TestUpgradeClusterImportsImagesOnEveryNodeBeforeTheFirstUpgrade(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, nil)

	if err := runUpgrade(t, cs); err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	got := log.all()
	if len(got) != 4 {
		t.Fatalf("jobs = %v, want an import on cp1 and w1, then the upgrades of cp1 and w1", got)
	}
	imports := slices.Sorted(slices.Values(got[:2]))
	if !slices.Equal(imports, []string{"import:cp1", "import:w1"}) {
		t.Errorf("first jobs = %v, want the image import on every node before any upgrade", got)
	}
	if !slices.Equal(got[2:], []string{"upgrade:cp1", "upgrade:w1"}) {
		t.Errorf("then = %v, want cp1's upgrade, then w1's", got[2:])
	}
}

func TestUpgradeClusterStopsBeforeAnyCordonWhenAnImportFails(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, map[string]error{"w1": errors.New("import images.tar: no space left on device")})

	err := runUpgrade(t, cs)
	for _, want := range []string{"w1", "no space left on device", "no node was cordoned or upgraded", "then re-run the cluster upgrade"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("UpgradeCluster = %v, want an error containing %q", err, want)
		}
	}
	for _, e := range log.all() {
		if strings.HasPrefix(e, "upgrade:") {
			t.Errorf("a node was upgraded after an import failed: %v", log.all())
		}
	}
	for _, name := range []string{"cp1", "w1"} {
		n, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if n.Spec.Unschedulable {
			t.Errorf("%s was cordoned after an import failed", name)
		}
	}
}

// Each import confirms both leases, so a successor stops the run before any Job.
func TestUpgradeClusterImportsNothingAfterTheMaintenanceLeaseIsTaken(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, nil)
	swap(t, &stagedImportsImages, func(context.Context, string) (bool, error) {
		takeOverTheMaintenanceLease(t, cs, "successor/1/1")
		return true, nil
	})

	if err := runUpgrade(t, cs); err == nil {
		t.Fatal("UpgradeCluster went on after another run took the maintenance lease")
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("jobs = %v, want none once the lease was taken", got)
	}
}

// A NotReady node cannot run the import's pod.
func TestImportImagesOnEveryNodeWarnsAndSkipsANotReadyNode(t *testing.T) {
	log := recordJobs(t, nil)
	nodes := []Node{{Name: "cp1", IsCP: true, Ready: true}, {Name: "w1"}}

	out, err := runWriter(t, func(w io.Writer) error {
		return importImagesOnEveryNode(context.Background(), w, fake.NewSimpleClientset(), nodes, ClusterOptions{From: "/staged/stratum"})
	})
	if err != nil {
		t.Fatalf("importImagesOnEveryNode: %v", err)
	}
	if slices.Contains(log.all(), "import:w1") {
		t.Errorf("jobs = %v, want no import on the NotReady w1", log.all())
	}
	if !slices.Contains(log.all(), "import:cp1") {
		t.Errorf("jobs = %v, want the import on cp1", log.all())
	}
	for _, want := range []string{"w1 is NotReady", "Re-run the cluster upgrade once it is Ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not warn about the NotReady w1 with %q:\n%s", want, out)
		}
	}
}

// An older staged binary rejects --import-images; the run warns and proceeds.
func TestUpgradeClusterSkipsTheImportForAnUpgradeBinaryWithoutIt(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, nil)
	swap(t, &stagedImportsImages, func(context.Context, string) (bool, error) { return false, nil })

	out, err := runWriter(t, func(w io.Writer) error {
		return UpgradeCluster(w, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs})
	})
	if err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	if got := log.all(); !slices.Equal(got, []string{"upgrade:cp1", "upgrade:w1"}) {
		t.Errorf("jobs = %v, want the upgrades of cp1 and w1 and no import", got)
	}
	if !strings.Contains(out, "has no image import step") {
		t.Errorf("output does not warn that the upgrade binary has no import step:\n%s", out)
	}
}

// A failed probe says nothing about the step; stop rather than run without the images.
func TestUpgradeClusterStopsWhenTheProbeFails(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, nil)
	swap(t, &stagedImportsImages, func(context.Context, string) (bool, error) {
		return false, errors.New("ask the upgrade binary /staged/stratum whether it can import images: signal: killed")
	})

	err := runUpgrade(t, cs)
	for _, want := range []string{"signal: killed", "no node was cordoned or upgraded"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("UpgradeCluster = %v, want an error containing %q", err, want)
		}
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("jobs = %v, want none after the probe failed", got)
	}
}

func TestStagedImportsImagesAsksTheBinary(t *testing.T) {
	for _, tc := range []struct {
		name, script  string
		want, wantErr bool
	}{
		{"has the step", "#!/bin/sh\n[ \"$*\" = \"upgrade --import-images --help\" ]\n", true, false},
		{"predates the step", "#!/bin/sh\necho 'Error: unknown flag: --import-images' >&2\nexit 1\n", false, false},
		{"fails another way", "#!/bin/sh\necho 'Killed' >&2\nexit 137\n", false, true},
		{"cannot run", "not a program\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "stratum-next")
			if err := os.WriteFile(p, []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := stagedImportsImages(context.Background(), p)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("stagedImportsImages = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestUpgradeClusterDryRunListsTheImport(t *testing.T) {
	cs := fake.NewSimpleClientset(upgradeNodesFixture()...)
	log := recordJobs(t, nil)

	out, err := runWriter(t, func(w io.Writer) error {
		return UpgradeCluster(w, ClusterOptions{Target: "v1.37.1", From: "/staged/stratum", Client: cs, DryRun: true})
	})
	if err != nil {
		t.Fatalf("UpgradeCluster --dry-run: %v", err)
	}
	if !strings.Contains(out, "import the upgrade binary's images on every Ready node") {
		t.Errorf("the dry run does not list the image import:\n%s", out)
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("the dry run dispatched jobs: %v", got)
	}
}

func TestNodeImportImagesArgvAsksTheStagedBinaryToImportImages(t *testing.T) {
	o := ClusterOptions{From: "/var/lib/stratum/upgrade/stratum-next", Target: "v1.37.1",
		expectBuild: "build-1", expectSHA256: "feedface", arch: "amd64"}
	n := Node{Name: "w1", Arch: "amd64"}
	got := nodeImportImagesArgv(o, n)
	want := append(stagedUpgradeArgv(o, n), "--import-images")
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func importFixture(t *testing.T, imagesDir string) *[]string {
	t.Helper()
	resumeFixture(t)
	swap(t, &resolvePayload, func(io.Writer, string, []string) (Source, error) {
		return Source{Dir: t.TempDir(), Images: imagesDir}, nil
	})
	var imported []string
	swap(t, &importImages, func(_ context.Context, _ io.Writer, dir string) error {
		imported = append(imported, dir)
		return nil
	})
	return &imported
}

func TestUpgradeNodeImportImagesImportsAndChangesNothingElse(t *testing.T) {
	images := t.TempDir()
	imported := importFixture(t, images)

	if err := UpgradeNode(io.Discard, NodeOptions{ImportImages: true}); err != nil {
		t.Fatalf("UpgradeNode --import-images: %v", err)
	}
	if !slices.Equal(*imported, []string{images}) {
		t.Errorf("imported %v, want the payload's images dir %s", *imported, images)
	}
	if _, err := os.Stat(stratumInstallPath); !os.IsNotExist(err) {
		t.Errorf("stratum was installed at %s (stat: %v); an import changes nothing but the images", stratumInstallPath, err)
	}
	if m, err := readUpgradeMarker(); err != nil || m != nil {
		t.Errorf("upgrade marker = %+v (%v), want none", m, err)
	}
	if sets, err := scanBackupSets(); err != nil || len(sets) != 0 {
		t.Errorf("backup sets = %v (%v), want none", sets, err)
	}
}

func TestUpgradeNodeImportImagesWithoutImagesImportsNothing(t *testing.T) {
	imported := importFixture(t, "")

	if err := UpgradeNode(io.Discard, NodeOptions{ImportImages: true}); err != nil {
		t.Fatalf("UpgradeNode --import-images: %v", err)
	}
	if len(*imported) != 0 {
		t.Errorf("imported %v from a payload that carries no images", *imported)
	}
}

func TestUpgradeNodeImportImagesRefusesWhileAnotherOperationHoldsTheNode(t *testing.T) {
	imported := importFixture(t, t.TempDir())
	release, err := lockNode("rollback")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := UpgradeNode(io.Discard, NodeOptions{ImportImages: true}); err == nil {
		t.Fatal("UpgradeNode --import-images ran while another operation held the node lock")
	}
	if len(*imported) != 0 {
		t.Errorf("imported %v while the node was locked", *imported)
	}
}
