package certs

import (
	"io"

	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/nodejob"
)

// kidOf and the two ids below stand for the old and the new key. They need only
// be what tokenKeyID reads back; the derivation is tested against a real token.
const (
	oldKid = "OLDKEYIDOLDKEYIDOLDKEYIDOLDKEYIDOLDKEYIDOLD"
	newKid = "NEWKEYIDNEWKEYIDNEWKEYIDNEWKEYIDNEWKEYIDNEW"
)

func jwtWithKid(kid string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"`+kid+`"}`)) + "." + enc([]byte(`{"sub":"x"}`)) + "." + enc([]byte("sig"))
}

// The apiserver names a key by the base64url SHA-256 of its PKIX DER. The fixture
// is a token a real kube-apiserver (the v1.36.4 image Stratum ships) issued, and
// the public key it verifies with; if saKeyID drifted from the apiserver's rule,
// the old key's tokens would read as clean and finalize would proceed.
func TestSAKeyIDMatchesARealApiserverToken(t *testing.T) {
	pub, err := os.ReadFile("testdata/apiserver-sa.pub")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := os.ReadFile("testdata/apiserver-sa-token.jwt")
	if err != nil {
		t.Fatal(err)
	}
	want, ok := tokenKeyID(string(tok))
	if !ok {
		t.Fatal("the fixture token has no kid")
	}
	got, err := saKeyID(pub)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("saKeyID = %s, the apiserver put %s in the token", got, want)
	}
	if !keyIDPattern.MatchString(got) {
		t.Errorf("%s is not accepted as a key id by --old-kid", got)
	}
}

func TestSAKeyIDRejectsWhatIsNotAPublicKey(t *testing.T) {
	for name, in := range map[string]string{
		"empty":       "",
		"garbage":     "not pem",
		"wrong block": "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n",
		"bad DER":     "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n",
	} {
		if kid, err := saKeyID([]byte(in)); err == nil {
			t.Errorf("%s: saKeyID = %q, want an error", name, kid)
		}
	}
}

func TestTokenKeyID(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{"a token", jwtWithKid("abc"), "abc", true},
		{"newline at the end", jwtWithKid("abc") + "\n", "abc", true},
		{"no kid", enc([]byte(`{"alg":"RS256"}`)) + ".e30.c", "", false},
		{"header is not JSON", enc([]byte("nope")) + ".e30.c", "", false},
		{"header is not base64", "!!!.e30.c", "", false},
		{"two parts", "a.b", "", false},
		{"not a token", "hello", "", false},
		{"empty", "", "", false},
	} {
		if got, ok := tokenKeyID(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("%s: tokenKeyID = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// writeMount writes a projected token as the kubelet does for a pod's volume.
func writeMount(root, podUID, volume, token string) error {
	dir := filepath.Join(root, "pods", podUID, "volumes", "kubernetes.io~projected", volume)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600)
}

func mount(t *testing.T, root, podUID, volume, token string) {
	t.Helper()
	if err := writeMount(root, podUID, volume, token); err != nil {
		t.Fatal(err)
	}
}

func TestFindStaleTokens(t *testing.T) {
	root := t.TempDir()
	mount(t, root, "uid-old", "kube-api-access-x", jwtWithKid(oldKid))
	mount(t, root, "uid-new", "kube-api-access-x", jwtWithKid(newKid))
	mount(t, root, "uid-other-issuer", "oidc", jwtWithKid("somebody-elses"))
	mount(t, root, "uid-not-a-jwt", "kube-api-access-x", "hello")
	mount(t, root, "uid-old-2", "second", jwtWithKid(oldKid))
	// A secret volume with a file called token is not a projected token.
	secret := filepath.Join(root, "pods", "uid-secret", "volumes", "kubernetes.io~secret", "s")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "token"), []byte(jwtWithKid(oldKid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The kubelet's token is a symlink into ..data; one whose pod went away is dangling.
	gone := filepath.Join(root, "pods", "uid-gone", "volumes", "kubernetes.io~projected", "v")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/token", filepath.Join(gone, "token")); err != nil {
		t.Fatal(err)
	}

	got, err := findStaleTokens(context.Background(), root, oldKid)
	if err != nil {
		t.Fatal(err)
	}
	want := []staleToken{{PodUID: "uid-old", Volume: "kube-api-access-x/token"}, {PodUID: "uid-old-2", Volume: "second/token"}}
	if !slices.Equal(got, want) {
		t.Errorf("findStaleTokens = %v, want %v", got, want)
	}
}

// A service-account token source writes to the path the pod spec gives it, nested
// or not, and the volume holds other files beside it.
func TestFindStaleTokensAnyPathInTheVolume(t *testing.T) {
	root := t.TempDir()
	vol := filepath.Join(root, "pods", "uid-1", "volumes", "kubernetes.io~projected", "creds")
	for path, tok := range map[string]string{
		"my-token":            jwtWithKid(oldKid),
		"nested/dir/sa-token": jwtWithKid(oldKid),
		"with space/token":    jwtWithKid(oldKid),
		"new-token":           jwtWithKid(newKid),
		"ca.crt":              "-----BEGIN CERTIFICATE-----",
		"nested/namespace":    "default",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(vol, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vol, path), []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findStaleTokens(context.Background(), root, oldKid)
	if err != nil {
		t.Fatal(err)
	}
	want := []staleToken{
		{PodUID: "uid-1", Volume: "creds/my-token"},
		{PodUID: "uid-1", Volume: "creds/nested/dir/sa-token"},
		{PodUID: "uid-1", Volume: "creds/with space/token"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("findStaleTokens = %v, want %v", got, want)
	}
}

// The kubelet's atomic writer: files are symlinks through ..data into a
// timestamped directory. Only what ..data points at is current; an older
// generation still beside it is not.
func TestFindStaleTokensReadsTheCurrentGeneration(t *testing.T) {
	root := t.TempDir()
	vol := filepath.Join(root, "pods", "uid-1", "volumes", "kubernetes.io~projected", "kube-api-access-x")
	for gen, kid := range map[string]string{"..2026_01": oldKid, "..2026_02": newKid} {
		if err := os.MkdirAll(filepath.Join(vol, gen), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vol, gen, "token"), []byte(jwtWithKid(kid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"..data": "..2026_02", "token": "..data/token"} {
		if err := os.Symlink(target, filepath.Join(vol, link)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := findStaleTokens(context.Background(), root, oldKid); err != nil || len(got) != 0 {
		t.Errorf("findStaleTokens = %v, %v; want nothing: the old generation is not mounted", got, err)
	}
	// ..data moves back to the old generation: now the token is stale, once.
	if err := os.Remove(filepath.Join(vol, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_01", filepath.Join(vol, "..data")); err != nil {
		t.Fatal(err)
	}
	want := []staleToken{{PodUID: "uid-1", Volume: "kube-api-access-x/token"}}
	if got, err := findStaleTokens(context.Background(), root, oldKid); err != nil || !slices.Equal(got, want) {
		t.Errorf("findStaleTokens = %v, %v; want %v", got, err, want)
	}
}

// atomicVolume is a projected volume as the kubelet writes it: generations
// beside each other, ..data pointing at the current one, token through it.
type atomicVolume struct{ root, vol string }

func newAtomicVolume(t *testing.T) atomicVolume {
	t.Helper()
	root := t.TempDir()
	return atomicVolume{root, filepath.Join(root, "pods", "uid-1", "volumes", "kubernetes.io~projected", "kube-api-access-x")}
}

func (v atomicVolume) generation(t *testing.T, name, kid string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(v.vol, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.vol, name, "token"), []byte(jwtWithKid(kid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// publish points ..data at gen, as the kubelet does: a new link renamed over the
// old one.
func (v atomicVolume) publish(t *testing.T, gen string) {
	t.Helper()
	tmp := filepath.Join(v.vol, "..data_tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(gen, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(v.vol, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(v.vol, "token")); err != nil {
		if err := os.Symlink("..data/token", filepath.Join(v.vol, "token")); err != nil {
			t.Fatal(err)
		}
	}
}

func hookOnce(t *testing.T, f func()) {
	t.Helper()
	old := scanHook
	t.Cleanup(func() { scanHook = old })
	done := false
	scanHook = func() {
		if !done {
			done = true
			f()
		}
	}
}

// The kubelet publishes a new generation and removes the one being scanned. Both
// can hold the same old-key token (a projected ConfigMap changed, the token did
// not), so reading what is left of the removed one must not read as clean.
func TestFindStaleTokensGenerationSwappedMidScan(t *testing.T) {
	v := newAtomicVolume(t)
	v.generation(t, "..2026_01", oldKid)
	v.publish(t, "..2026_01")
	hookOnce(t, func() {
		v.generation(t, "..2026_02", oldKid)
		v.publish(t, "..2026_02")
		if err := os.RemoveAll(filepath.Join(v.vol, "..2026_01")); err != nil {
			t.Fatal(err)
		}
	})
	want := []staleToken{{PodUID: "uid-1", Volume: "kube-api-access-x/token"}}
	if got, err := findStaleTokens(context.Background(), v.root, oldKid); err != nil || !slices.Equal(got, want) {
		t.Errorf("findStaleTokens = %v, %v; want %v: the new generation holds the old-key token", got, err, want)
	}
}

// A generation swap that leaves the token new is clean, and does not fail.
func TestFindStaleTokensGenerationSwappedToANewTokenMidScan(t *testing.T) {
	v := newAtomicVolume(t)
	v.generation(t, "..2026_01", oldKid)
	v.publish(t, "..2026_01")
	hookOnce(t, func() {
		v.generation(t, "..2026_02", newKid)
		v.publish(t, "..2026_02")
		if err := os.RemoveAll(filepath.Join(v.vol, "..2026_01")); err != nil {
			t.Fatal(err)
		}
	})
	if got, err := findStaleTokens(context.Background(), v.root, oldKid); err != nil || len(got) != 0 {
		t.Errorf("findStaleTokens = %v, %v; want clean", got, err)
	}
}

// A ..data that points at nothing (between generations) is retried, and one that
// never resolves fails closed: no generation was read, so none can be called clean.
func TestFindStaleTokensDanglingDataFailsClosed(t *testing.T) {
	v := newAtomicVolume(t)
	v.generation(t, "..2026_01", oldKid)
	if err := os.Symlink("..2026_gone", filepath.Join(v.vol, "..data")); err != nil {
		t.Fatal(err)
	}
	old, oldAttempts := scanHook, scanAttempts
	t.Cleanup(func() { scanHook, scanAttempts = old, oldAttempts })
	scanAttempts = 3
	read := 0
	scanHook = func() { read++ }
	if got, err := findStaleTokens(context.Background(), v.root, oldKid); err == nil {
		t.Errorf("findStaleTokens = %v, nil: a ..data that never resolves must fail closed", got)
	}
	if read != 0 {
		t.Errorf("a generation was read %d times though ..data pointed at nothing", read)
	}
}

// A volume that keeps changing, so that no complete scan can be made, fails
// closed instead of reporting what a partial one saw.
func TestFindStaleTokensKeepsChangingFailsClosed(t *testing.T) {
	v := newAtomicVolume(t)
	v.generation(t, "..2026_00", oldKid)
	v.publish(t, "..2026_00")
	n := 0
	old := scanHook
	t.Cleanup(func() { scanHook = old })
	scanHook = func() {
		n++
		gen := fmt.Sprintf("..2026_%02d", n)
		v.generation(t, gen, oldKid)
		v.publish(t, gen)
	}
	got, err := findStaleTokens(context.Background(), v.root, oldKid)
	if err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Errorf("findStaleTokens = %v, %v; want the failure to get a complete scan", got, err)
	}
	if n != scanAttempts {
		t.Errorf("tried %d times, want %d", n, scanAttempts)
	}
}

// A pod deleted mid-scan takes its volume with it: nothing is mounted, so clean.
func TestFindStaleTokensVolumeDeletedMidScanIsClean(t *testing.T) {
	v := newAtomicVolume(t)
	v.generation(t, "..2026_01", oldKid)
	v.publish(t, "..2026_01")
	hookOnce(t, func() {
		if err := os.RemoveAll(filepath.Join(v.root, "pods", "uid-1")); err != nil {
			t.Fatal(err)
		}
	})
	if got, err := findStaleTokens(context.Background(), v.root, oldKid); err != nil || len(got) != 0 {
		t.Errorf("findStaleTokens = %v, %v; want clean", got, err)
	}
}

// Expiry fails a scan: it must not return clean (the gate would proceed), not
// restart the kubelet, and must stop reading.
func TestScanExpiredContextFailsClosed(t *testing.T) {
	expired := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	t.Run("a clean node", func(t *testing.T) {
		root := t.TempDir()
		mount(t, root, "uid-new", "v", jwtWithKid(newKid))
		(&kubelet{root: root}).install(t)
		for _, restart := range []bool{false, true} {
			if got, err := scanNode(expired(), io.Discard, oldKid, restart); !errors.Is(err, context.Canceled) {
				t.Errorf("restart=%v: scanNode = %v, %v; want context.Canceled, not clean", restart, got, err)
			}
		}
	})
	t.Run("a stale node is not restarted", func(t *testing.T) {
		root := t.TempDir()
		mount(t, root, "uid-old", "v", jwtWithKid(oldKid))
		k := &kubelet{root: root, fixes: true}
		k.install(t)
		if _, err := scanNode(expired(), io.Discard, oldKid, true); err == nil {
			t.Error("scanNode on an expired context returned no error")
		}
		if k.calls != 0 {
			t.Errorf("kubelet restarted %d time(s) after expiry", k.calls)
		}
	})
	// The shared scanner enforces it, so a node with nothing to read (an empty pods
	// directory) is no exception, whoever calls it.
	t.Run("an empty pods directory, expired", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "pods"), 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got, err := findStaleTokens(ctx, root, oldKid); !errors.Is(err, context.Canceled) {
			t.Errorf("findStaleTokens = %v, %v; want context.Canceled, not clean", got, err)
		}
	})
	t.Run("expiry in the middle of the walk", func(t *testing.T) {
		v := newAtomicVolume(t)
		v.generation(t, "..2026_01", oldKid)
		v.publish(t, "..2026_01")
		ctx, cancel := context.WithCancel(context.Background())
		hookOnce(t, cancel) // after the generation is chosen, before it is read
		if got, err := findStaleTokens(ctx, v.root, oldKid); !errors.Is(err, context.Canceled) {
			t.Errorf("findStaleTokens = %v, %v; want context.Canceled", got, err)
		}
	})
	t.Run("expired after the restart: the rescan fails, not clean", func(t *testing.T) {
		root := t.TempDir()
		mount(t, root, "uid-old", "v", jwtWithKid(oldKid))
		ctx, cancel := context.WithCancel(context.Background())
		k := &kubelet{root: root, fixes: true}
		k.install(t)
		inner := restartKubelet
		restartKubelet = func() error { err := inner(); cancel(); return err }
		if got, err := scanNode(ctx, io.Discard, oldKid, true); !errors.Is(err, context.Canceled) {
			t.Errorf("scanNode = %v, %v; want context.Canceled: the tokens were re-issued but the time was up", got, err)
		}
	})
}

func TestFindStaleTokensNoKubeletDir(t *testing.T) {
	if got, err := findStaleTokens(context.Background(), filepath.Join(t.TempDir(), "absent"), oldKid); err != nil || len(got) != 0 {
		t.Errorf("findStaleTokens = %v, %v; want nothing, nil", got, err)
	}
}

// filepath.Glob drops a directory it cannot list, so a pod directory holding a
// stale token that cannot be read must not read as clean.
func TestFindStaleTokensUnreadableDirFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	for name, dir := range map[string][]string{
		"pods":              {"pods"},
		"a pod":             {"pods", "uid"},
		"its volumes":       {"pods", "uid", "volumes"},
		"the projected dir": {"pods", "uid", "volumes", "kubernetes.io~projected"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mount(t, root, "uid", "v", jwtWithKid(oldKid))
			d := filepath.Join(append([]string{root}, dir...)...)
			if err := os.Chmod(d, 0); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(d, 0o755)
			if got, err := findStaleTokens(context.Background(), root, oldKid); err == nil {
				t.Errorf("findStaleTokens = %v, nil: a directory it could not list must not read as clean", got)
			}
		})
	}
}

func TestFindStaleTokensUnreadableFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	root := t.TempDir()
	mount(t, root, "uid", "v", jwtWithKid(oldKid))
	p := filepath.Join(root, "pods", "uid", "volumes", "kubernetes.io~projected", "v", "token")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := findStaleTokens(context.Background(), root, oldKid); err == nil {
		t.Errorf("findStaleTokens = %v, nil: a token it could not read must not read as clean", got)
	}
}

// kubelet stubs the restart: it re-issues every stale token under the root with
// the new key, as a restarted kubelet does, and counts the calls.
type kubelet struct {
	root    string
	calls   int
	fixes   bool
	failure error
}

func (k *kubelet) install(t *testing.T) {
	t.Helper()
	oldRoot, oldRestart, oldWait, oldPoll := kubeletRoot, restartKubelet, kubeletReissueWait, kubeletReissuePoll
	t.Cleanup(func() {
		kubeletRoot, restartKubelet, kubeletReissueWait, kubeletReissuePoll = oldRoot, oldRestart, oldWait, oldPoll
	})
	kubeletRoot, kubeletReissueWait, kubeletReissuePoll = k.root, 50*time.Millisecond, time.Millisecond
	restartKubelet = func() error {
		k.calls++
		if k.failure != nil {
			return k.failure
		}
		if k.fixes {
			stale, _ := findStaleTokens(context.Background(), k.root, oldKid)
			for _, s := range stale {
				vol, _, _ := strings.Cut(s.Volume, "/") // these tests mount at <vol>/token
				if err := writeMount(k.root, s.PodUID, vol, jwtWithKid(newKid)); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func TestScanNode(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		stale      bool
		restart    bool
		kubeletFix bool
		restartErr error
		wantStale  int
		wantCalls  int
		wantErr    string
	}{
		{"clean, no restart asked", false, false, true, nil, 0, 0, ""},
		{"clean: the kubelet is left alone", false, true, true, nil, 0, 0, ""},
		{"stale, read-only: reported, not restarted", true, false, true, nil, 1, 0, ""},
		{"stale, restart re-issues the token", true, true, true, nil, 0, 1, ""},
		{"stale, restart does not help", true, true, false, nil, 1, 1, ""},
		{"stale, restart fails", true, true, true, errors.New("no supervisor"), 0, 1, "restart kubelet: no supervisor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mount(t, root, "uid-new", "v", jwtWithKid(newKid))
			if tc.stale {
				mount(t, root, "uid-old", "v", jwtWithKid(oldKid))
			}
			k := &kubelet{root: root, fixes: tc.kubeletFix, failure: tc.restartErr}
			k.install(t)

			got, err := scanNode(ctx, io.Discard, oldKid, tc.restart)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("scanNode error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantStale {
				t.Errorf("scanNode = %v, want %d stale", got, tc.wantStale)
			}
			if k.calls != tc.wantCalls {
				t.Errorf("kubelet restarted %d time(s), want %d", k.calls, tc.wantCalls)
			}
		})
	}
}

// ScanTokens is the Job: its answer is the error the CLI turns into the exit status.
func TestScanTokens(t *testing.T) {
	root := t.TempDir()
	mount(t, root, "uid-old", "v", jwtWithKid(oldKid))
	(&kubelet{root: root}).install(t)

	if err := ScanTokens(io.Discard, oldKid, false); !errors.Is(err, ErrStaleTokens) {
		t.Errorf("ScanTokens = %v, want ErrStaleTokens", err)
	}
	if err := ScanTokens(io.Discard, newKid, false); err != nil {
		t.Errorf("ScanTokens for a key that signed nothing here = %v, want nil", err)
	}
	for _, bad := range []string{"", "short", oldKid + "x", strings.Repeat("=", 43), "../../etc/passwd"} {
		if err := ScanTokens(io.Discard, bad, false); err == nil || errors.Is(err, ErrStaleTokens) {
			t.Errorf("ScanTokens(io.Discard, %q) = %v, want a refusal of the key id", bad, err)
		}
	}
}

func TestScanNodeArgv(t *testing.T) {
	for _, restart := range []bool{false, true} {
		argv := scanNodeArgv(oldKid, restart)
		for i, a := range argv {
			if a == "-c" || strings.Contains(a, "sh") && strings.Contains(a, "bin/") {
				t.Errorf("argv[%d] = %q: a shell is back in the payload", i, a)
			}
		}
		if !slices.Contains(argv, "--scan-tokens") || !slices.Contains(argv, "--yes") {
			t.Errorf("argv %v lacks --scan-tokens or --yes", argv)
		}
		if i := slices.Index(argv, "--old-kid"); i < 0 || argv[i+1] != oldKid {
			t.Errorf("argv %v does not carry the old key id as one element", argv)
		}
		if got := slices.Contains(argv, "--restart-kubelet"); got != restart {
			t.Errorf("--restart-kubelet present = %v, want %v", got, restart)
		}
	}
}

func TestParseStaleLines(t *testing.T) {
	log := "some noise\nstale-token uid-1 vol-a\nstale-token malformed\n  stale-token uid-2 vol-b/with space/token  \nstratum certs rotate-ca: x\n"
	want := []staleToken{{PodUID: "uid-1", Volume: "vol-a"}, {PodUID: "uid-2", Volume: "vol-b/with space/token"}}
	if got := parseStaleLines(log); !slices.Equal(got, want) {
		t.Errorf("parseStaleLines = %v, want %v", got, want)
	}
}

// stubJob replaces the Job runner and its log reader for one test.
func stubJob(t *testing.T, code int, runErr error, log string, logErr error) *[]nodejob.Spec {
	t.Helper()
	oldRun, oldLogs := runExitCode, jobLogs
	t.Cleanup(func() { runExitCode, jobLogs = oldRun, oldLogs })
	var specs []nodejob.Spec
	runExitCode = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, error) {
		specs = append(specs, s)
		return code, runErr
	}
	jobLogs = func(context.Context, kubernetes.Interface, nodejob.Spec) (string, error) { return log, logErr }
	return &specs
}

func TestScanNodeJob(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	n := cnode{Name: "worker-1"}
	for _, tc := range []struct {
		name    string
		code    int
		runErr  error
		log     string
		logErr  error
		want    []staleToken
		notWant bool // the result must not be empty
		wantErr string
	}{
		{name: "clean", code: 0},
		{name: "stale, listed", code: ExitStaleTokens, log: "stale-token u1 v1\n", want: []staleToken{{PodUID: "u1", Volume: "v1"}}},
		{name: "stale, log unreadable is still stale", code: ExitStaleTokens, logErr: errors.New("no pod"), notWant: true},
		{name: "stale, log has no offenders is still stale", code: ExitStaleTokens, log: "nothing", notWant: true},
		{name: "another exit status is an error", code: 1, wantErr: "exit status 1"},
		{name: "the Job did not run", runErr: errors.New("timed out"), wantErr: "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := stubJob(t, tc.code, tc.runErr, tc.log, tc.logErr)
			got, err := scanNodeJob(ctx, io.Discard, cs, n, oldKid, true)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil && !slices.Equal(got, tc.want) {
				t.Errorf("stale = %v, want %v", got, tc.want)
			}
			if tc.notWant && len(got) == 0 {
				t.Error("an offender-less answer from a Job that exited with stale tokens read as clean")
			}
			if tc.code == 0 && len(got) != 0 {
				t.Errorf("stale = %v, want clean", got)
			}
			s := (*specs)[0]
			if s.Node != "worker-1" || s.Namespace != caNamespace || !strings.Contains(s.Name, "scan-tokens") || !slices.Contains(s.Argv, "--restart-kubelet") {
				t.Errorf("Job spec = %+v", s)
			}
		})
	}
}

func TestNameStale(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "apipoller", Name: "apipoller-1", UID: "uid-old"}})
	stale := []staleToken{{PodUID: "uid-old", Volume: "v"}, {PodUID: "uid-unknown", Volume: "v"}}
	nameStale(context.Background(), cs, "cp2", stale)
	if got := stale[0].String(); got != "apipoller/apipoller-1 (volume v)" {
		t.Errorf("a resolved pod = %q", got)
	}
	if got := stale[1].String(); got != "pod uid uid-unknown (volume v)" {
		t.Errorf("an unresolved pod = %q, want its uid", got)
	}
}

// An offender whose uid is unknown keeps the stand-in Pod text the Job scan gave
// it; only a uid that resolves replaces it.
func TestNameStaleKeepsTheFallbackUnlessTheUIDResolves(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid-1"}})
	stale := []staleToken{
		{Pod: "(offenders not listed: no pod)"}, // no uid
		{PodUID: "uid-1", Pod: "stand-in"},      // resolves
		{PodUID: "uid-gone", Pod: "stand-in"},   // does not
	}
	nameStale(context.Background(), cs, "cp2", stale)
	for i, want := range []string{"(offenders not listed: no pod)", "ns/p", "stand-in"} {
		if stale[i].Pod != want {
			t.Errorf("stale[%d].Pod = %q, want %q", i, stale[i].Pod, want)
		}
	}
	if got := stale[0].String(); strings.Contains(got, "pod uid") {
		t.Errorf("the fallback was erased: %q", got)
	}
}

// scans is a tokenScanner over a table of what each node reports on each pass.
type scans struct {
	// pass[node] is the stale tokens the node has on its nth scan; past the end, the last.
	pass     map[string][][]staleToken
	err      map[string]error
	seen     map[string]int
	restarts map[string][]bool
}

func (s *scans) scan(_ context.Context, n cnode, restart bool) ([]staleToken, error) {
	if s.seen == nil {
		s.seen, s.restarts = map[string]int{}, map[string][]bool{}
	}
	if err := s.err[n.Name]; err != nil {
		return nil, err
	}
	i := s.seen[n.Name]
	s.seen[n.Name]++
	s.restarts[n.Name] = append(s.restarts[n.Name], restart)
	p := s.pass[n.Name]
	if len(p) == 0 {
		return nil, nil
	}
	return p[min(i, len(p)-1)], nil
}

func countTrue(b []bool) (n int) {
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

func fastGate(t *testing.T) {
	t.Helper()
	w, p, s := tokenGateWait, tokenGatePoll, tokenSettle
	t.Cleanup(func() { tokenGateWait, tokenGatePoll, tokenSettle = w, p, s })
	tokenGateWait, tokenGatePoll, tokenSettle = 40*time.Millisecond, time.Millisecond, time.Millisecond
}

func TestWaitTokensClean(t *testing.T) {
	ctx := context.Background()
	nodes := []cnode{{Name: "cp1", IsCP: true}, {Name: "cp2", IsCP: true}, {Name: "w1"}}
	stale := []staleToken{{PodUID: "u", Volume: "v", Pod: "ns/p"}}

	t.Run("every node clean on the first pass: nothing restarted, no second pass", func(t *testing.T) {
		fastGate(t)
		s := &scans{}
		if err := waitTokensClean(ctx, io.Discard, nodes, s.scan); err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if !slices.Equal(s.restarts[n.Name], []bool{false}) {
				t.Errorf("%s: scans %v, want exactly one", n.Name, s.restarts[n.Name])
			}
		}
	})

	t.Run("a stale node is restarted once, then only re-scanned until it is clean", func(t *testing.T) {
		fastGate(t)
		s := &scans{pass: map[string][][]staleToken{"cp2": {stale, stale, nil}}}
		if err := waitTokensClean(ctx, io.Discard, nodes, s.scan); err != nil {
			t.Fatal(err)
		}
		// A read-only scan finds it, one scan restarts, and after that only reads.
		if got, want := s.restarts["cp2"], []bool{false, true, false}; !slices.Equal(got, want) {
			t.Errorf("cp2 restart flag on each scan = %v, want %v (the kubelet is restarted once)", got, want)
		}
		// Every node is scanned on every pass, so none finalizes ahead of a stale one.
		if s.seen["cp1"] != 2 || s.seen["w1"] != 2 {
			t.Errorf("scans of the clean nodes = cp1 %d, w1 %d, want 2 each (the passes)", s.seen["cp1"], s.seen["w1"])
		}
	})

	// A restart that cleared the node's tokens is still the node's one restart: a
	// token that turns up later is only watched, not restarted for again.
	t.Run("a successful restart is not repeated", func(t *testing.T) {
		fastGate(t)
		s := &scans{pass: map[string][][]staleToken{
			"cp1": {stale, stale, nil},      // keeps a second pass going
			"cp2": {stale, nil, stale, nil}, // stale; restarted, clean; stale again; clean
		}}
		if err := waitTokensClean(ctx, io.Discard, nodes, s.scan); err != nil {
			t.Fatal(err)
		}
		if got := countTrue(s.restarts["cp2"]); got != 1 {
			t.Errorf("cp2's kubelet restarted %d times (scans %v), want once", got, s.restarts["cp2"])
		}
	})

	// The wait bounds the scans, not just the passes between them: a scan that
	// hangs (a Job on a node that never answers) ends the gate at the cap.
	t.Run("a scan that outlasts the cap fails the gate at the cap", func(t *testing.T) {
		fastGate(t)
		tokenGateWait = 30 * time.Millisecond
		hang := func(ctx context.Context, n cnode, _ bool) ([]staleToken, error) {
			if n.Name != "cp2" {
				return nil, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		start := time.Now()
		err := waitTokensClean(ctx, io.Discard, nodes, hang)
		if err == nil || !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("error = %v, want the cap's error naming cp2", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v is the bare deadline, not the gate's own", err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("returned after %s", took)
		}
	})

	t.Run("a node that never gets clean fails the gate, naming it and the pod", func(t *testing.T) {
		fastGate(t)
		s := &scans{pass: map[string][][]staleToken{"w1": {stale}}}
		err := waitTokensClean(ctx, io.Discard, nodes, s.scan)
		if err == nil {
			t.Fatal("the gate passed with a node still mounting a token signed by the old key")
		}
		for _, want := range []string{"w1", "ns/p", "401"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "cp1") {
			t.Errorf("error %q names a clean node", err)
		}
	})

	t.Run("a node that cannot be scanned fails the gate at once", func(t *testing.T) {
		fastGate(t)
		s := &scans{err: map[string]error{"cp2": errors.New("job failed")}}
		err := waitTokensClean(ctx, io.Discard, nodes, s.scan)
		if err == nil || !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "job failed") {
			t.Errorf("error = %v, want the scan failure of cp2", err)
		}
	})

	t.Run("the settle wait follows the last clean scan", func(t *testing.T) {
		fastGate(t)
		tokenSettle = 30 * time.Millisecond
		start := time.Now()
		if err := waitTokensClean(ctx, io.Discard, nodes, (&scans{}).scan); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took < 30*time.Millisecond {
			t.Errorf("returned after %s, before the settle wait", took)
		}
	})

	t.Run("a cancelled run stops waiting", func(t *testing.T) {
		fastGate(t)
		tokenGateWait, tokenGatePoll = time.Hour, time.Hour
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		s := &scans{pass: map[string][][]staleToken{"cp1": {stale}}}
		if err := waitTokensClean(cctx, io.Discard, nodes, s.scan); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})
}

// The single-node finalize gate on a context that is already done: refused, not
// passed, even with nothing mounted.
func TestLocalTokenGateExpiredContextRefuses(t *testing.T) {
	pub, err := os.ReadFile("testdata/apiserver-sa.pub")
	if err != nil {
		t.Fatal(err)
	}
	oldPKI, oldRoot := pkiDir, kubeletRoot
	t.Cleanup(func() { pkiDir, kubeletRoot = oldPKI, oldRoot })
	pkiDir, kubeletRoot = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(pkiDir, "sa.pre-carotate.pub"), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(kubeletRoot, "pods"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("localTokenGate = %v, want a refusal wrapping context.Canceled", err)
	}
	// And the same node on a live context is clean.
	if err := localTokenGate(context.Background(), io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Errorf("localTokenGate on a live context = %v, want nil", err)
	}
}

func TestOldSAKeyID(t *testing.T) {
	dir := t.TempDir()
	old := pkiDir
	t.Cleanup(func() { pkiDir = old })
	pkiDir = dir

	if _, err := oldSAKeyID(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("oldSAKeyID with no old key = %v, want ErrNotExist", err)
	}
	pub, err := os.ReadFile("testdata/apiserver-sa.pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sa.pre-carotate.pub"), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	want, _ := saKeyID(pub)
	if got, err := oldSAKeyID(); err != nil || got != want {
		t.Errorf("oldSAKeyID = %q, %v; want %q", got, err, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "sa.pre-carotate.pub"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := oldSAKeyID(); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("oldSAKeyID with a corrupt key = %v, want an error that is not 'no key'", err)
	}
}

func TestLocalTokenGate(t *testing.T) {
	ctx := context.Background()
	pub, err := os.ReadFile("testdata/apiserver-sa.pub")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := os.ReadFile("testdata/apiserver-sa-token.jwt")
	if err != nil {
		t.Fatal(err)
	}
	setup := func(t *testing.T, withOldKey bool, token string) {
		t.Helper()
		oldPKI, oldRoot := pkiDir, kubeletRoot
		t.Cleanup(func() { pkiDir, kubeletRoot = oldPKI, oldRoot })
		pkiDir, kubeletRoot = t.TempDir(), t.TempDir()
		if withOldKey {
			if err := os.WriteFile(filepath.Join(pkiDir, "sa.pre-carotate.pub"), pub, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if token != "" {
			mount(t, kubeletRoot, "uid-1", "kube-api-access-x", token)
		}
	}

	t.Run("a token signed by the old key refuses finalize and says how to go on", func(t *testing.T) {
		setup(t, true, string(tok))
		err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true})
		if err == nil {
			t.Fatal("finalize was not refused")
		}
		for _, want := range []string{"1 token", "uid-1", "kubelet", "--skip-token-check"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
	t.Run("--skip-token-check", func(t *testing.T) {
		setup(t, true, string(tok))
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true, SkipTokenCheck: true}); err != nil {
			t.Errorf("a skipped check refused: %v", err)
		}
	})
	t.Run("only new-key tokens", func(t *testing.T) {
		setup(t, true, jwtWithKid(newKid))
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err != nil {
			t.Errorf("a clean node refused: %v", err)
		}
	})
	t.Run("no old key: finalized already, or never signed anew", func(t *testing.T) {
		setup(t, false, string(tok))
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err != nil {
			t.Errorf("a node with no old key refused, which would break re-running a finalize whose restart failed: %v", err)
		}
	})
	// PromoteSA writes the backup only if it could read sa.pub, and promotes either
	// way: no backup with two keys still in the bundle is an unidentifiable old
	// key, not a node that finalized.
	t.Run("no old key but the bundle holds two keys refuses", func(t *testing.T) {
		setup(t, false, string(tok))
		if err := os.WriteFile(filepath.Join(pkiDir, "sa-bundle.pub"), append(append([]byte{}, pub...), pub...), 0o644); err != nil {
			t.Fatal(err)
		}
		err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), "2 keys") || !strings.Contains(err.Error(), "--skip-token-check with --force") {
			t.Errorf("error = %v, want a refusal that counts the keys and names the bypass", err)
		}
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true, SkipTokenCheck: true}); err != nil {
			t.Errorf("a skipped check refused: %v", err)
		}
	})
	// The apiserver also takes a certificate's public key into the verification
	// set, and Stratum keeps whatever encoding an existing sa.pub has: a PUBLIC KEY
	// beside a CERTIFICATE is two verifiers, not one.
	t.Run("no old key, a public key and a certificate in the bundle refuse", func(t *testing.T) {
		setup(t, false, string(tok))
		if err := os.WriteFile(filepath.Join(pkiDir, "sa-bundle.pub"), append(append([]byte{}, pub...), selfSignedCertPEM(t)...), 0o644); err != nil {
			t.Fatal(err)
		}
		err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), "2 keys") {
			t.Errorf("error = %v, want a refusal that counts 2 keys: the old verifier is still in the bundle", err)
		}
	})
	t.Run("no old key and a bundle that does not parse refuses", func(t *testing.T) {
		for name, content := range map[string]string{
			"garbage":               "not pem",
			"empty":                 "",
			"a block it cannot use": "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n",
		} {
			setup(t, false, string(tok))
			if err := os.WriteFile(filepath.Join(pkiDir, "sa-bundle.pub"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err == nil {
				t.Errorf("%s: an unparseable bundle was read as 'finalized'", name)
			}
		}
	})
	t.Run("no old key and a one-key bundle: finalized already", func(t *testing.T) {
		setup(t, false, string(tok))
		if err := os.WriteFile(filepath.Join(pkiDir, "sa-bundle.pub"), pub, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err != nil {
			t.Errorf("a finalized node refused: %v", err)
		}
	})
	t.Run("no old key and a bundle that cannot be read refuses", func(t *testing.T) {
		setup(t, false, string(tok))
		if err := os.Mkdir(filepath.Join(pkiDir, "sa-bundle.pub"), 0o755); err != nil { // reading a directory fails
			t.Fatal(err)
		}
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err == nil {
			t.Error("an unreadable bundle was read as 'finalized'")
		}
	})
	t.Run("an old key that cannot be read refuses", func(t *testing.T) {
		setup(t, true, "")
		if err := os.WriteFile(filepath.Join(pkiDir, "sa.pre-carotate.pub"), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := localTokenGate(ctx, io.Discard, CARotateOptions{Quiet: true}); err == nil || !strings.Contains(err.Error(), "--skip-token-check") {
			t.Errorf("error = %v, want a refusal that names --skip-token-check", err)
		}
	})
}

// selfSignedCertPEM is a certificate whose public key the apiserver would accept
// as a ServiceAccount verifier.
func selfSignedCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "old-sa"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}, &x509.Certificate{}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestLegacyStaleSecrets(t *testing.T) {
	secret := func(ns, name string, typ corev1.SecretType, token string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Type:       typ,
			Data:       map[string][]byte{corev1.ServiceAccountTokenKey: []byte(token)},
		}
	}
	cs := fake.NewSimpleClientset(
		secret("a", "old", saTokenSecretType, jwtWithKid(oldKid)),
		secret("a", "new", saTokenSecretType, jwtWithKid(newKid)),
		secret("b", "no-token-yet", saTokenSecretType, ""),
		secret("b", "opaque-with-an-old-token", corev1.SecretTypeOpaque, jwtWithKid(oldKid)),
	)
	got, err := legacyStaleSecrets(context.Background(), cs, oldKid)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/old"}; !slices.Equal(got, want) {
		t.Errorf("legacyStaleSecrets = %v, want %v", got, want)
	}
}

// The token gate can take an hour and restarts kubelets, so the cluster check
// runs again after it: a node that went NotReady meanwhile refuses finalize.
func TestFinalizeChecks(t *testing.T) {
	notReady := errors.New("node cp2 is not Ready")
	for _, tc := range []struct {
		name       string
		skipTokens bool
		healthErrs []error // the answer of each health check, in order
		tokenErr   error
		wantErr    error
		wantHealth int
		wantTokens int
	}{
		{"all clear", false, []error{nil, nil}, nil, nil, 2, 1},
		{"not ready before the gate", false, []error{notReady}, nil, notReady, 1, 0},
		{"not ready after the gate", false, []error{nil, notReady}, nil, notReady, 2, 1},
		{"the gate refuses", false, []error{nil}, errors.New("stale"), errors.New("stale"), 1, 1},
		{"token check skipped: one health check", true, []error{nil}, nil, nil, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var health, tokens int
			err := finalizeChecks(tc.skipTokens,
				func() error { health++; return tc.healthErrs[health-1] },
				func() error { tokens++; return tc.tokenErr })
			if (err == nil) != (tc.wantErr == nil) || (err != nil && err.Error() != tc.wantErr.Error()) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if health != tc.wantHealth || tokens != tc.wantTokens {
				t.Errorf("health checks %d, token gates %d; want %d, %d", health, tokens, tc.wantHealth, tc.wantTokens)
			}
		})
	}
}
