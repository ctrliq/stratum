package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.etcd.io/etcd/api/v3/mvccpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

// joinState answers a fixed etcd state and records that it was asked.
func joinState(n int, bundle bool, err error, asked *bool) func() (etcdJoinState, error) {
	return func() (etcdJoinState, error) {
		*asked = true
		return etcdJoinState{Members: n, JoinBundle: bundle}, err
	}
}

// TestSharedSecretGate: enabling CIS on a control plane of a live HA
// cluster minted a key only that apiserver held, so Secrets written through it
// could not be read through the others. A key is minted only on a sole control
// plane with no outstanding join bundle; a key already on disk is never
// second-guessed.
func TestSharedSecretGate(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "encryption-config.yaml")
	if err := os.WriteFile(present, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "missing.yaml")
	// A symlink that points at itself: Stat fails with ELOOP, which is neither
	// "there" nor "not there".
	loop := filepath.Join(dir, "loop.yaml")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	const (
		members = "members"
		bundle  = "bundle"
		unread  = "unreadable"
		statErr = "stat"
	)
	for _, tc := range []struct {
		name      string
		cis       bool
		keyPath   string
		n         int
		published bool
		err       error
		refusal   string // "" for none
		wantAsked bool
	}{
		{"CIS off never asks etcd", false, absent, 3, true, nil, "", false},
		{"fresh install: first controller mints the key", true, absent, 0, false, nil, "", true},
		{"single controller enables CIS", true, absent, 1, false, nil, "", true},
		{"joiner that received the key at join", true, present, 3, true, nil, "", false},
		{"restart of a controller that already has its key", true, present, 1, true, nil, "", false},
		{"CIS enabled on a live 3-controller cluster", true, absent, 3, false, nil, members, true},
		{"CIS enabled on a live 2-controller cluster", true, absent, 2, true, nil, members, true},
		{"single controller with an outstanding join bundle", true, absent, 1, true, nil, bundle, true},
		{"etcd unreadable", true, absent, 0, false, errors.New("boom"), unread, true},
		{"a key path that cannot be stat'ed refuses, without asking etcd", true, loop, 1, false, nil, statErr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			at := func(string) string { return tc.keyPath }
			minted, err := sharedSecretGate(profiletest.For(tc.cis), at, joinState(tc.n, tc.published, tc.err, &asked))
			mint := len(minted) == 1
			if (err != nil) != (tc.refusal != "") {
				t.Fatalf("err = %v, want refusal %q", err, tc.refusal)
			}
			// The gate's answer is the one PrepareController acts on, so
			// it says "mint" exactly when CIS is on, there is no key, and etcd
			// allows it -- never on a refusal, never over a key on disk.
			if wantMint := tc.cis && tc.keyPath == absent && tc.refusal == ""; mint != wantMint {
				t.Errorf("mint = %v, want %v", mint, wantMint)
			}
			if asked != tc.wantAsked {
				t.Errorf("etcd asked = %v, want %v", asked, tc.wantAsked)
			}
			if tc.refusal == "" {
				return
			}
			if !strings.Contains(err.Error(), "at-rest encryption key") {
				t.Errorf("refusal does not name the at-rest encryption key: %v", err)
			}
			// The operator has to learn why and what to do instead.
			var wants, order []string
			switch tc.refusal {
			case unread:
				if !errors.Is(err, tc.err) {
					t.Errorf("refusal does not wrap the etcd error: %v", err)
				}
				wants = []string{
					fmt.Sprintf("127.0.0.1:%d", version.EtcdClientPort),
					version.EtcdDataDir,
					"restore the running etcd's client endpoint",
					"stop etcd so the database can be read",
				}
			case members:
				wants = []string{
					fmt.Sprintf("etcd has %d members", tc.n),
					"could not be read through the other control planes",
					"before any other joins",
					profile.ProDocsURL,
				}
				// The remedy gives the install-time way first, then
				// sends a running cluster to the documentation.
				order = []string{"Enable CIS at install time instead", "To enable it on this running cluster"}
			case statErr:
				wants = []string{"cannot tell whether an at-rest encryption key exists", loop}
			case bundle:
				wants = []string{
					"kube-system/stratum-certs",
					"uploaded before this key existed",
					"`sudo /usr/local/bin/stratum kubectl -n kube-system delete secret stratum-certs`",
					"stratum token create --control-plane",
					"remove spec.profile: cis and spec.cis: true",
				}
				// Deleting the Secret needs an apiserver, which a
				// refused start does not bring up, so the remedy starts one
				// without the profile first.
				order = []string{"start this control plane without --profile cis or --cis", "delete secret stratum-certs",
					"stop it", "start it again with --profile cis", "stratum token create --control-plane"}
			}
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q: %v", want, err)
				}
			}
			assertInOrder(t, err.Error(), order)
		})
	}
}

// assertInOrder fails unless every step appears in msg, each after the last.
func assertInOrder(t *testing.T, msg string, steps []string) {
	t.Helper()
	at := 0
	for _, step := range steps {
		i := strings.Index(msg[at:], step)
		if i < 0 {
			t.Errorf("step %q is missing, or out of order, in: %s", step, msg)
			return
		}
		at += i + len(step)
	}
}

// --- the gate asks the bundle owner ----------------------------------------
//
// The contract these tests pin:
//
//   - etcdJoinState carries CarriesKey: whether the published stratum-certs
//     Secret holds an entry whose ciphertext authenticates THIS node's at-rest
//     key — provenance from an AES-GCM open of the encryption-key-proof entry
//     the bundle owner publishes (ha), never from an annotation.
//   - liveEtcdJoinState is the live answer's etcd side: members, and etcd's own
//     bundle answer, which remains the fallback for when the Secret API cannot
//     be reached.
//   - publishedBundleCarriesKey is the bundle owner's answer over the Secret
//     API, with this node's at-rest key (version.EncryptionConfigPath's bytes,
//     nil when there is none yet) matched against the published entries.
//   - liveJoinState composes them: members from etcd; the bundle answer from
//     the Secret when the API answers — whether the Secret is published, and
//     whether it carries the key — and etcd's own answer, unkeyed, when it does
//     not. The offline read (etcd stopped) never consults the API and never
//     claims a key.
//
// When the Secret API answers, the gate does not answer "is a bundle
// published" by opening etcd's backend and looking up the apiserver's storage
// key (backendHasKey on ha.JoinBundleEtcdKey): that couples the gate to the
// apiserver's etcd layout, so etcd's answer is only the fallback.

// stubGateVars saves and restores the three live-leg seams a test stubs.
func stubGateVars(t *testing.T, etcd func() (etcdJoinState, error), api func() (bool, bool, error)) {
	t.Helper()
	savedEtcd, savedAPI := liveEtcdJoinState, publishedBundleCarriesKey
	t.Cleanup(func() { liveEtcdJoinState, publishedBundleCarriesKey = savedEtcd, savedAPI })
	liveEtcdJoinState, publishedBundleCarriesKey = etcd, api
}

func TestKeyGateAsksTheBundleSecret(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "encryption-config.yaml") // no key on disk
	// The refusal TestEncryptionKeyGate pins, verbatim in kind: the gate
	// must answer "a bundle is published" from the Secret the same way, with
	// the same words, whatever source answered it.
	bundleWants := []string{
		"kube-system/stratum-certs",
		"uploaded before this key existed",
		"`sudo /usr/local/bin/stratum kubectl -n kube-system delete secret stratum-certs`",
		"stratum token create --control-plane",
		"remove spec.profile: cis and spec.cis: true",
	}

	// The gate reaches the bundle owner's answer through the live leg, so a
	// wiring regression here silences everything below. Read from source,
	// because reaching the live leg's body in a test means running an etcd and
	// an apiserver. The same reading TestPrepareControllerMintsThroughTheSecondLook
	// takes for the write it pins.
	t.Run("the live leg asks the bundle owner, not etcd's storage key", func(t *testing.T) {
		src, err := os.ReadFile("ciskey.go")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "ha.PublishedBundleCarriesKey") {
			t.Fatal("the gate's live leg does not ask the stratum-certs owner " +
				"(ha.PublishedBundleCarriesKey) whether a keyed bundle is published")
		}
		if !strings.Contains(string(src), "publishedBundleCarriesKey()") {
			t.Fatal("the live leg never consults the bundle owner's answer")
		}
	})

	t.Run("with etcd running, the Secret's answer decides, whatever etcd's raw storage holds", func(t *testing.T) {
		// etcd's own answer (and its backend: the key bucket holds no bundle
		// key here) says nothing is published; the Secret over the API says a
		// bundle is. The live leg must believe the Secret, not etcd's storage
		// key.
		stubGateVars(t,
			func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil },
			func() (bool, bool, error) { return true, false, nil })
		mint, err := cisGateMints(absent, liveJoinState)
		if err == nil || mint {
			t.Fatalf("gate = (%v, %v); want the published-bundle refusal", mint, err)
		}
		for _, want := range bundleWants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal is missing %q: %v", want, err)
			}
		}
	})

	t.Run("a bundle deleted while etcd's backend still shows it does not refuse", func(t *testing.T) {
		// The reverse ranking, and the reason the gate reads the Secret: etcd's
		// backend can trail what the Secret API answers (an unclean stop, a
		// deleted Secret), and the live Secret is what a join will actually
		// download.
		stubGateVars(t,
			func() (etcdJoinState, error) { return etcdJoinState{Members: 1, JoinBundle: true}, nil },
			func() (bool, bool, error) { return false, false, nil })
		mint, err := cisGateMints(absent, liveJoinState)
		if err != nil || !mint {
			t.Fatalf("gate = (%v, %v); want the mint the live Secret allows", mint, err)
		}
	})

	t.Run("a keyed bundle is refused by the first gate exactly as a keyless one is", func(t *testing.T) {
		// TestEncryptionKeyGate's rule, preserved: any published bundle
		// refuses a key mint on a sole control plane. The gate never mints
		// because a bundle is keyed — the keyed answer exists for the second
		// look, which must spare the key it wrote, not for the first gate.
		stubGateVars(t,
			func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil },
			func() (bool, bool, error) { return true, true, nil })
		mint, err := cisGateMints(absent, liveJoinState)
		if err == nil || mint {
			t.Fatalf("gate = (%v, %v); want the published-bundle refusal even for a keyed bundle", mint, err)
		}
		for _, want := range bundleWants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal is missing %q: %v", want, err)
			}
		}
	})

	t.Run("an API failure falls back to etcd's own answer, unkeyed", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			etcd     etcdJoinState
			wantMint bool
		}{
			// The apiserver answers nothing (a first boot, a stopped
			// apiserver), etcd is up: the gate refuses on what etcd says, and
			// no key it cannot verify is ever read as present.
			{"etcd sees no bundle: the mint goes ahead", etcdJoinState{Members: 1}, true},
			{"etcd sees a bundle: the refusal stands", etcdJoinState{Members: 1, JoinBundle: true}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stubGateVars(t,
					func() (etcdJoinState, error) { return tc.etcd, nil },
					func() (bool, bool, error) { return false, false, errors.New("apiserver down") })
				mint, err := cisGateMints(absent, liveJoinState)
				if (err == nil) != tc.wantMint || mint != tc.wantMint {
					t.Fatalf("gate = (%v, %v); want mint=%v from etcd's fallback answer", mint, err, tc.wantMint)
				}
			})
		}
	})

	t.Run("with etcd stopped, the offline read answers and no API is asked", func(t *testing.T) {
		saved := loadNodeState
		t.Cleanup(func() { loadNodeState = saved })
		loadNodeState = func() (*NodeState, error) { return nil, fs.ErrNotExist }
		dir := t.TempDir()
		writeBackend(t, dir, backend{
			members: []string{member("cp1")},
			revs:    []rev{{key: ha.JoinBundleEtcdKey}},
		})
		st, err := localEtcdJoinState(dir, func() (etcdJoinState, error) {
			t.Error("asked the live leg for an unlocked backend")
			return etcdJoinState{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The offline read can say a bundle is published but can never say it
		// is keyed: the proof is a decryption only the Secret API's owner can
		// run. The second look treats an offline answer as unkeyed and keeps
		// today's rollback.
		want := etcdJoinState{Members: 1, JoinBundle: true}
		if st != want {
			t.Fatalf("got %+v, want %+v", st, want)
		}
	})
}

// TestSecondLookSparesAKeyedBundle: the second look's refusal does not take
// the key back when the published bundle authenticates it, because deleting
// just moves the problem — the bundle hands the key out, so a joiner already
// holds exactly the key the rollback would remove, and the next start would
// mint a second, different one. The key stays, and the refusal says so
// instead of claiming a removal that did not happen.
func TestSecondLookSparesAKeyedBundle(t *testing.T) {
	savedSync := syncKeyDir
	t.Cleanup(func() { syncKeyDir = savedSync })
	var synced []string
	syncKeyDir = func(d string) error { synced = append(synced, d); return nil }
	path := filepath.Join(t.TempDir(), "encryption-config.yaml")
	write := func() (os.FileInfo, error) {
		if err := os.WriteFile(path, []byte("k"), 0o600); err != nil {
			return nil, err
		}
		return os.Stat(path)
	}
	err := mintSharedSecret(path, write, func() (etcdJoinState, error) {
		return etcdJoinState{Members: 1, JoinBundle: true, CarriesKey: true}, nil
	})
	if err == nil {
		t.Fatal("the second look allowed a mint while a bundle is published: the published-bundle refusal rule is gone")
	}
	for _, want := range []string{
		"at-rest encryption key",
		"a second check of etcd, made after the at-rest key was written",
		"carries this key",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
	for _, lie := range []string{"the key was removed from", "none was left behind"} {
		if strings.Contains(err.Error(), lie) {
			t.Errorf("refusal claims %q, but the key was spared: %v", lie, err)
		}
	}
	if len(synced) != 0 {
		t.Errorf("the spared key's directory was synced as if for a removal: %v", synced)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the key this start wrote is gone from %s (%v): the second look deleted a key the published "+
			"bundle authenticates", path, statErr)
	}
}

// --- the bundle owner's answer, and what authenticates it ---

// sealProof seals plaintext under the raw 32 bytes a base64 at-rest key
// encodes, as the bundle owner's proof entry is sealed.
func sealProof(t *testing.T, keyB64 string, plaintext []byte) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(raw) != 32 {
		t.Fatalf("at-rest key %q is not base64 of 32 bytes (%v)", keyB64, err)
	}
	blk, err := aes.NewCipher(raw)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil)
}

// keyedSecret is a stratum-certs Secret carrying the given data entries, as
// the bundle owner publishes it.
func keyedSecret(data map[string][]byte, annotations map[string]string) *corev1.Secret {
	if annotations == nil {
		annotations = map[string]string{}
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "stratum-certs",
			Namespace:   "kube-system",
			Annotations: annotations,
		},
		Data: data,
	}
}

// TestPublishedBundleCarriesKeyAuthenticatesByCiphertext: the owner answers
// whether a published bundle carries an at-rest key from the proof entry's
// ciphertext — an AES-GCM open under that exact key — and from nothing else.
// An annotation is writable by anyone who can write the Secret, so it
// authenticates nothing: provenance must not rest on an unauthenticated
// marker.
func TestPublishedBundleCarriesKeyAuthenticatesByCiphertext(t *testing.T) {
	keyB64 := testEncryptionKey(t)
	otherB64 := testEncryptionKey(t)
	proof := sealProof(t, keyB64, []byte("stratum at-rest key proof"))

	t.Run("a proof entry sealed under this node's key answers keyed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(keyedSecret(
			map[string][]byte{"certs": []byte("x"), "encryption-key-proof": proof}, nil))
		published, carries, err := ha.PublishedBundleCarriesKey(cs, []byte(testEncryptionConfig(keyB64)))
		if err != nil || !published || !carries {
			t.Fatalf("got (%v, %v, %v); want the published bundle answering keyed", published, carries, err)
		}
	})
	t.Run("a proof sealed under another key answers published and unkeyed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(keyedSecret(
			map[string][]byte{"certs": []byte("x"), "encryption-key-proof": sealProof(t, otherB64, []byte("p"))}, nil))
		published, carries, err := ha.PublishedBundleCarriesKey(cs, []byte(testEncryptionConfig(keyB64)))
		if err != nil || !published || carries {
			t.Fatalf("got (%v, %v, %v); want published, not this node's key", published, carries, err)
		}
	})
	t.Run("an annotation is not provenance", func(t *testing.T) {
		cs := fake.NewSimpleClientset(keyedSecret(
			map[string][]byte{"certs": []byte("x")},
			map[string]string{
				"stratum.ciq.com/certificate-key-set-at": time.Now().UTC().Format(time.RFC3339),
				"stratum.ciq.com/carries-encryption-key": "true",
			}))
		published, carries, err := ha.PublishedBundleCarriesKey(cs, []byte(testEncryptionConfig(keyB64)))
		if err != nil || !published || carries {
			t.Fatalf("got (%v, %v, %v); an unauthenticated annotation must never answer keyed", published, carries, err)
		}
	})
	t.Run("no Secret is not published, and no error", func(t *testing.T) {
		published, carries, err := ha.PublishedBundleCarriesKey(fake.NewSimpleClientset(),
			[]byte(testEncryptionConfig(keyB64)))
		if err != nil || published || carries {
			t.Fatalf("got (%v, %v, %v); want an unpublished cluster", published, carries, err)
		}
	})
	t.Run("no key on disk answers published and unkeyed", func(t *testing.T) {
		// The first gate runs before any key exists: it can ask whether a
		// bundle is published, and has nothing to match the proof against.
		cs := fake.NewSimpleClientset(keyedSecret(
			map[string][]byte{"certs": []byte("x"), "encryption-key-proof": proof}, nil))
		published, carries, err := ha.PublishedBundleCarriesKey(cs, nil)
		if err != nil || !published || carries {
			t.Fatalf("got (%v, %v, %v); want published, unkeyed with no key to match", published, carries, err)
		}
	})
	t.Run("a Secret that cannot be read is an error, never an answer", func(t *testing.T) {
		boom := errors.New("etcdserver: request timed out")
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, boom
		})
		published, carries, err := ha.PublishedBundleCarriesKey(cs, []byte(testEncryptionConfig(keyB64)))
		if !errors.Is(err, boom) || published || carries {
			t.Fatalf("got (%v, %v, %v); want the read error, fail-closed", published, carries, err)
		}
	})
}

// TestPrepareControllerRefusesASplitKey pins the gate to PrepareController, ahead
// of any host change: without it a live HA cluster's controller goes on to mint
// its own key.
func TestPrepareControllerRefusesASplitKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root PrepareController would change this host if the gate were missing")
	}
	if _, err := os.Stat(version.EncryptionConfigPath); !os.IsNotExist(err) {
		t.Skipf("%s exists on this host", version.EncryptionConfigPath)
	}
	saved := readEtcdJoinState
	t.Cleanup(func() { readEtcdJoinState = saved })
	readEtcdJoinState = func() (etcdJoinState, error) { return etcdJoinState{Members: 3}, nil }

	cfg := &ClusterConfig{}
	cfg.Spec.CIS = true
	_, err := PrepareController(cfg, "", nil)
	if err == nil || !strings.Contains(err.Error(), "at-rest encryption key") {
		t.Fatalf("PrepareController = %v, want the at-rest encryption key refusal", err)
	}
}

// rev is one revision in etcd's "key" bucket.
type rev struct {
	key       string
	tombstone bool
}

// backend describes an etcd-shaped database to write: the "members" bucket's
// values, and the "key" bucket's history, one revision per entry in order.
type backend struct {
	members   []string
	revs      []rev
	noMembers bool // leave out the members bucket
	noKeys    bool // leave out the key bucket
}

// writeBackend creates the database the way etcd lays it out.
func writeBackend(t *testing.T, dataDir string, b backend) {
	t.Helper()
	snap := filepath.Join(dataDir, "member", "snap")
	if err := os.MkdirAll(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(snap, "db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bolt.Tx) error {
		if !b.noMembers {
			mb, err := tx.CreateBucket([]byte("members"))
			if err != nil {
				return err
			}
			for i, v := range b.members {
				if err := mb.Put(fmt.Appendf(nil, "%016x", i+1), []byte(v)); err != nil {
					return err
				}
			}
		}
		if b.noKeys {
			return nil
		}
		kb, err := tx.CreateBucket([]byte("key"))
		if err != nil {
			return err
		}
		for i, r := range b.revs {
			// 8-byte main revision, '_', 8-byte sub, and 't' for a delete.
			k := binary.BigEndian.AppendUint64(nil, uint64(i+2))
			k = append(k, '_')
			k = binary.BigEndian.AppendUint64(k, 0)
			kv := &mvccpb.KeyValue{Key: []byte(r.key), ModRevision: int64(i + 2)}
			if r.tombstone {
				k = append(k, 't')
			} else {
				kv.Value = []byte("v")
			}
			v, err := kv.Marshal()
			if err != nil {
				return err
			}
			if err := kb.Put(k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func member(name string) string {
	return fmt.Sprintf(`{"id":1,"peerURLs":["https://10.0.0.1:2380"],"name":%q}`, name)
}

func TestLocalEtcdJoinState(t *testing.T) {
	saved := loadNodeState
	t.Cleanup(func() { loadNodeState = saved })
	loadNodeState = func() (*NodeState, error) { return nil, fs.ErrNotExist }
	noLive := func() (etcdJoinState, error) {
		t.Helper()
		t.Error("asked the running etcd although the backend was readable")
		return etcdJoinState{}, nil
	}
	read := func(t *testing.T, b backend) (etcdJoinState, error) {
		t.Helper()
		dir := t.TempDir()
		writeBackend(t, dir, b)
		return localEtcdJoinState(dir, noLive)
	}
	other := "/registry/secrets/kube-system/something-else"
	bundle := ha.JoinBundleEtcdKey

	t.Run("an empty etcd data directory is a fresh install", func(t *testing.T) {
		st, err := localEtcdJoinState(t.TempDir(), noLive)
		if err != nil || st != (etcdJoinState{}) {
			t.Fatalf("got %+v, %v; want nothing, nil", st, err)
		}
	})
	t.Run("no etcd data directory is a fresh install", func(t *testing.T) {
		st, err := localEtcdJoinState(filepath.Join(t.TempDir(), "etcd"), noLive)
		if err != nil || st != (etcdJoinState{}) {
			t.Fatalf("got %+v, %v; want nothing, nil", st, err)
		}
	})
	// Saved node state without etcd data is a controller that
	// lost its etcd directory, not a fresh install, and unreadable state cannot
	// be ruled out as one.
	for _, tc := range []struct {
		name, want string
		load       func() (*NodeState, error)
	}{
		{"saved node state with no etcd data", "has saved node state",
			func() (*NodeState, error) { return &NodeState{Role: "controller"}, nil }},
		{"unreadable node state with no etcd data", "read node state",
			func() (*NodeState, error) { return nil, errors.New("yaml: bad") }},
	} {
		t.Run(tc.name+" is an error, not a fresh install", func(t *testing.T) {
			loadNodeState = tc.load
			t.Cleanup(func() { loadNodeState = func() (*NodeState, error) { return nil, fs.ErrNotExist } })
			st, err := localEtcdJoinState(t.TempDir(), noLive)
			if err == nil {
				t.Fatalf("got %+v, nil; want an error", st)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is missing %q: %v", tc.want, err)
			}
		})
	}
	// Only an absent or empty data directory is fresh. Etcd
	// history without its database is a node whose membership cannot be read.
	t.Run("a WAL without its database is an error, not a fresh install", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "member", "wal"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "member", "wal", "0000000000000000-0000000000000000.wal"), []byte("w"), 0o600); err != nil {
			t.Fatal(err)
		}
		if st, err := localEtcdJoinState(dir, noLive); err == nil {
			t.Fatalf("got %+v, nil; want an error", st)
		}
	})
	for _, tc := range []struct {
		name string
		b    backend
		want etcdJoinState
	}{
		{"one member, nothing published",
			backend{members: []string{member("cp1")}, revs: []rev{{key: other}}},
			etcdJoinState{Members: 1}},
		{"three members read offline",
			backend{members: []string{member("cp1"), member("cp2"), member("cp3")}},
			etcdJoinState{Members: 3}},
		{"a published join bundle",
			backend{members: []string{member("cp1")}, revs: []rev{{key: other}, {key: bundle}}},
			etcdJoinState{Members: 1, JoinBundle: true}},
		{"a deleted join bundle is not published",
			backend{members: []string{member("cp1")}, revs: []rev{{key: bundle}, {key: other}, {key: bundle, tombstone: true}}},
			etcdJoinState{Members: 1}},
		{"a join bundle re-created after a delete is published",
			backend{members: []string{member("cp1")}, revs: []rev{{key: bundle}, {key: bundle, tombstone: true}, {key: bundle}}},
			etcdJoinState{Members: 1, JoinBundle: true}},
		{"a key that merely starts with the bundle's name is not it",
			backend{members: []string{member("cp1")}, revs: []rev{{key: bundle + "-old"}}},
			etcdJoinState{Members: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := read(t, tc.b)
			if err != nil || st != tc.want {
				t.Fatalf("got %+v, %v; want %+v, nil", st, err, tc.want)
			}
		})
	}
	t.Run("a running etcd's lock defers to the live answer", func(t *testing.T) {
		dir := t.TempDir()
		writeBackend(t, dir, backend{members: []string{member("cp1")}})
		// A writable open takes the exclusive lock, as the running etcd does.
		db, err := bolt.Open(filepath.Join(dir, "member", "snap", "db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		want := etcdJoinState{Members: 3, JoinBundle: true}
		st, err := localEtcdJoinState(dir, func() (etcdJoinState, error) { return want, nil })
		if err != nil || st != want {
			t.Fatalf("got %+v, %v; want the live answer %+v, nil", st, err, want)
		}
	})
	// A failed live read says the database was there but locked, and
	// which endpoint was tried instead.
	t.Run("a failed live read names the lock and the endpoint", func(t *testing.T) {
		dir := t.TempDir()
		writeBackend(t, dir, backend{members: []string{member("cp1")}})
		db, err := bolt.Open(filepath.Join(dir, "member", "snap", "db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		boom := errors.New("connection refused")
		_, err = localEtcdJoinState(dir, func() (etcdJoinState, error) { return etcdJoinState{}, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the live read's error wrapped", err)
		}
		for _, want := range []string{"is locked by a running etcd", fmt.Sprintf("127.0.0.1:%d", version.EtcdClientPort)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
	})
	// Every one of these is the layout changing under this code, and each one
	// has to be an error: a zero would read as "only member, nothing published".
	for _, tc := range []struct {
		name string
		b    backend
	}{
		{"no members bucket", backend{noMembers: true}},
		{"an empty members bucket", backend{}},
		{"a member value that is not JSON", backend{members: []string{member("cp1"), "not json"}}},
		{"no key bucket", backend{members: []string{member("cp1")}, noKeys: true}},
	} {
		t.Run(tc.name+" is an error", func(t *testing.T) {
			if st, err := read(t, tc.b); err == nil {
				t.Fatalf("got %+v, nil; want an error", st)
			}
		})
	}
	t.Run("a revision key without etcd's separator is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeBackend(t, dir, backend{members: []string{member("cp1")}})
		db, err := bolt.Open(filepath.Join(dir, "member", "snap", "db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		// 17 bytes, the right length for a live revision, with 'x' where
		// etcd puts '_'.
		k := binary.BigEndian.AppendUint64(nil, 9)
		k = append(k, 'x')
		k = binary.BigEndian.AppendUint64(k, 0)
		v, err := (&mvccpb.KeyValue{Key: []byte(ha.JoinBundleEtcdKey)}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("key")).Put(k, v) }); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if st, err := localEtcdJoinState(dir, noLive); err == nil {
			t.Fatalf("got %+v, nil; want an error", st)
		}
	})
	t.Run("a revision key of an unknown length is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeBackend(t, dir, backend{members: []string{member("cp1")}})
		db, err := bolt.Open(filepath.Join(dir, "member", "snap", "db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("key")).Put([]byte("short"), []byte{})
		}); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if st, err := localEtcdJoinState(dir, noLive); err == nil {
			t.Fatalf("got %+v, nil; want an error", st)
		}
	})
}

// TestPublishedCheckCountsTheBundleWhateverItsEntries: the offline published
// check answers from the stratum-certs storage key alone, never from the
// Secret's contents. The Secret carries one ciphertext entry per live grant
// beside the legacy certs entry, and every such state is a published bundle;
// a check that opened the stored value — the apiserver's own encoding, opaque
// here — would read the degenerate empty value below as "nothing published"
// and misanswer the gate.
func TestPublishedCheckCountsTheBundleWhateverItsEntries(t *testing.T) {
	saved := loadNodeState
	t.Cleanup(func() { loadNodeState = saved })
	loadNodeState = func() (*NodeState, error) { return nil, fs.ErrNotExist }
	dir := t.TempDir()
	writeBackend(t, dir, backend{members: []string{member("cp1")}, revs: []rev{{key: ha.JoinBundleEtcdKey}}})
	// A live revision whose value is empty: nothing to decode, still published.
	db, err := bolt.Open(filepath.Join(dir, "member", "snap", "db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	kv := &mvccpb.KeyValue{Key: []byte(ha.JoinBundleEtcdKey)}
	v, err := kv.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		k := binary.BigEndian.AppendUint64(nil, 99)
		k = append(k, '_')
		k = binary.BigEndian.AppendUint64(k, 0)
		return tx.Bucket([]byte("key")).Put(k, v)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := localEtcdJoinState(dir, func() (etcdJoinState, error) {
		t.Error("asked a live etcd for an unlocked database")
		return etcdJoinState{}, nil
	})
	if err != nil || st != (etcdJoinState{Members: 1, JoinBundle: true}) {
		t.Fatalf("got %+v, %v; want one member with the bundle published whatever entries it carries", st, err)
	}
}

// TestMintEncryptionKeyLooksAgainAfterTheWrite: the first check
// runs before host prerequisites and PKI, so a bundle published or a member
// added meanwhile is missed. The second look, after the key is on disk, takes
// the key back off when the answer changed -- only the key this start wrote
// and durably.
func TestMintEncryptionKeyLooksAgainAfterTheWrite(t *testing.T) {
	savedSync := syncKeyDir
	t.Cleanup(func() { syncKeyDir = savedSync })
	// write creates the key at path and reports it, as the create-only writer does.
	write := func(path string) func() (os.FileInfo, error) {
		return func() (os.FileInfo, error) {
			if err := os.WriteFile(path, []byte("k"), 0o600); err != nil {
				return nil, err
			}
			return os.Stat(path)
		}
	}
	for _, tc := range []struct {
		name    string
		state   etcdJoinState
		err     error
		refuse  bool
		keyLeft bool
	}{
		{"still the only member, nothing published", etcdJoinState{Members: 1}, nil, false, true},
		{"a join bundle published meanwhile", etcdJoinState{Members: 1, JoinBundle: true}, nil, true, false},
		{"a member added meanwhile", etcdJoinState{Members: 2}, nil, true, false},
		{"etcd unreadable on the second look", etcdJoinState{}, errors.New("boom"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "encryption-config.yaml")
			var synced []string
			syncKeyDir = func(d string) error { synced = append(synced, d); return nil }
			asked := false
			read := func() (etcdJoinState, error) {
				if _, err := os.Stat(path); err != nil {
					t.Error("asked etcd before the key was written")
				}
				asked = true
				return tc.state, tc.err
			}
			err := mintSharedSecret(path, write(path), read)
			if (err != nil) != tc.refuse {
				t.Fatalf("err = %v, want refusal: %v", err, tc.refuse)
			}
			if !asked {
				t.Error("never looked again after the write")
			}
			if tc.refuse && !strings.Contains(err.Error(), "at-rest encryption key") {
				t.Errorf("refusal does not name the at-rest encryption key: %v", err)
			}
			// The second look's refusal names its phase and says the
			// key it rolled back is gone, rather than repeating the first
			// gate's words as if nothing had been written.
			if tc.refuse {
				for _, want := range []string{"a second check of etcd, made after the at-rest key was written",
					"the key was removed from " + path} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal is missing %q: %v", want, err)
					}
				}
				// The removal is synced, or a power loss can
				// bring the refused key back.
				if len(synced) != 1 || synced[0] != filepath.Dir(path) {
					t.Errorf("rollback synced %v, want exactly %s", synced, filepath.Dir(path))
				}
			}
			_, statErr := os.Stat(path)
			if left := statErr == nil; left != tc.keyLeft {
				t.Errorf("key left on disk = %v, want %v", left, tc.keyLeft)
			}
		})
	}
	syncKeyDir = savedSync
	t.Run("a failed write that created nothing asks nothing and touches nothing", func(t *testing.T) {
		boom := errors.New("disk full")
		err := mintSharedSecret(filepath.Join(t.TempDir(), "k"), func() (os.FileInfo, error) { return nil, boom },
			func() (etcdJoinState, error) { t.Error("asked etcd after a failed write"); return etcdJoinState{}, nil })
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "no key was written") {
			t.Fatalf("err = %v, want the write error and that no key was written", err)
		}
	})
	// A write that leaves a partial file and then fails must not
	// leave it there, or the next start takes it for a key and skips the gate.
	t.Run("a write that fails after creating a partial file leaves no file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		boom := errors.New("short write")
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			if err := os.WriteFile(path, []byte("apiVersion: apiserver.con"), 0o600); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			return fi, boom
		}, func() (etcdJoinState, error) { t.Error("asked etcd after a failed write"); return etcdJoinState{}, nil })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the write error", err)
		}
		for _, want := range []string{"write the at-rest encryption key", "the key was removed from " + path} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a partial key file is left at %s (%v)", path, err)
		}
	})
	// A key that appeared at the path after the gate looked is
	// not replaced by the create-only write, and not removed by the rollback.
	t.Run("a key that appeared meanwhile is left in place", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			return nil, &fs.PathError{Op: "link", Path: path, Err: fs.ErrExist}
		}, func() (etcdJoinState, error) { t.Error("asked etcd after a failed write"); return etcdJoinState{}, nil })
		if err == nil {
			t.Fatal("a key that appeared meanwhile was accepted")
		}
		for _, want := range []string{
			"was left in place",
			"The next start uses it as it is, without consulting etcd",
			"compare sha256sum",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "so the check runs against it") {
			t.Errorf("error promises a check the next start does not run: %v", err)
		}
		if b, _ := os.ReadFile(path); string(b) != "theirs" {
			t.Fatalf("the key that appeared now holds %q", b)
		}
	})
	t.Run("a dangling symlink at the path is named, not taken for a key that appeared", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "encryption-config.yaml")
		if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
			t.Fatal(err)
		}
		none := func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil }
		minted, err := sharedSecretGate(profiletest.Profile(), func(string) string { return path }, none)
		mint := len(minted) == 1
		if err != nil || !mint {
			t.Fatalf("gate = (%v, %v), want it to see no key behind a dangling symlink", mint, err)
		}
		err = mintSharedSecret(path, func() (os.FileInfo, error) {
			return host.CreateFileExclusively(path, []byte("k"), 0o600)
		}, none)
		if err == nil || !strings.Contains(err.Error(), path+" is a symlink") {
			t.Fatalf("err = %v, want it to name the symlink", err)
		}
		if strings.Contains(err.Error(), "a key appeared") {
			t.Errorf("error says a key appeared at a dangling symlink: %v", err)
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("the symlink was replaced (%v, %v)", fi, err)
		}
	})
	t.Run("the rollback removes only the inode this start created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			fi, err := write(path)()
			if err != nil {
				return nil, err
			}
			// Someone replaces the file before the second look. The
			// replacement exists before the original goes, as with a rename
			// over it, so it cannot reuse the original's inode number (Linux
			// reuses a freed one at once, which made a remove-then-create
			// replacement look like the same file).
			theirs := path + ".theirs"
			if err := os.WriteFile(theirs, []byte("theirs"), 0o600); err != nil {
				return nil, err
			}
			return fi, os.Rename(theirs, path)
		}, func() (etcdJoinState, error) { return etcdJoinState{Members: 2}, nil })
		if err == nil || !strings.Contains(err.Error(), "is not the key this start wrote, so it was left in place") {
			t.Fatalf("err = %v, want it to leave the replacement alone", err)
		}
		if b, _ := os.ReadFile(path); string(b) != "theirs" {
			t.Fatalf("the replacement now holds %q", b)
		}
	})
	t.Run("an unsynced removal is reported as an incomplete rollback", func(t *testing.T) {
		syncKeyDir = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { syncKeyDir = savedSync })
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		err := mintSharedSecret(path, write(path), func() (etcdJoinState, error) { return etcdJoinState{Members: 2}, nil })
		if err == nil || !strings.Contains(err.Error(), "the rollback is incomplete") {
			t.Fatalf("err = %v, want an incomplete rollback", err)
		}
		if strings.Contains(err.Error(), "so none was left behind") {
			t.Fatalf("an unsynced removal is reported as complete: %v", err)
		}
	})
	t.Run("a key left under its temporary name is never reported gone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			fi, err := write(path)()
			if err != nil {
				return nil, err
			}
			return fi, fmt.Errorf("%s.tmp-1: %w", path, host.ErrStrayCopy)
		}, func() (etcdJoinState, error) { t.Error("asked etcd after a failed write"); return etcdJoinState{}, nil })
		if err == nil || strings.Contains(err.Error(), "so none was left behind") {
			t.Fatalf("err = %v, want it not to call the rollback complete", err)
		}
		for _, want := range []string{path + ".tmp-1", "still holds it", "the key was removed from " + path} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the key is still at %s (%v)", path, err)
		}
	})
	t.Run("a partial key left under its temporary name before the link is named", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "encryption-config.yaml")
		noEtcd := func() (etcdJoinState, error) { t.Error("asked etcd after a failed write"); return etcdJoinState{}, nil }
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			return nil, fmt.Errorf("EIO; %s.tmp-1: %w", path, host.ErrStrayCopy)
		}, noEtcd)
		for _, want := range []string{path + ".tmp-1", "no key was written to " + path, "delete that file before retrying"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		err = mintSharedSecret(path, func() (os.FileInfo, error) {
			return nil, fmt.Errorf("%w; %s.tmp-1: %w", fs.ErrExist, path, host.ErrStrayCopy)
		}, noEtcd)
		for _, want := range []string{path + ".tmp-1", "was left in place", "Also delete the temporary file named above"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
	})
	t.Run("a key that cannot be removed says so", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root removes files from a read-only directory")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "encryption-config.yaml")
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		err := mintSharedSecret(path, func() (os.FileInfo, error) {
			fi, err := write(path)()
			if err != nil {
				return nil, err
			}
			return fi, os.Chmod(dir, 0o500)
		}, func() (etcdJoinState, error) { return etcdJoinState{Members: 2}, nil })
		if err == nil || !strings.Contains(err.Error(), "the key could NOT be removed from "+path) {
			t.Fatalf("err = %v, want it to say the key is still there", err)
		}
	})
}

// TestPrepareControllerMintsThroughTheSecondLook pins PrepareController's
// shared-secret writes to mintSharedSecrets, whose one write is inside
// mintSharedSecret's: a direct write would mint without the second look. Read
// from source, because reaching that line in a test means running the host
// preparation before it.
func TestPrepareControllerMintsThroughTheSecondLook(t *testing.T) {
	src, err := os.ReadFile("prepare.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "mintSharedSecrets(prof, mint, version.ProfileSecretFile,") {
		t.Fatal("PrepareController does not write its shared secrets through mintSharedSecrets")
	}
	if strings.Contains(string(src), "CreateFileExclusively(") || strings.Contains(string(src), ".Mint()") {
		t.Fatal("prepare.go mints a shared secret directly, bypassing the second look")
	}
	// Whether to mint is the gate's answer, not a second look at the
	// file with different error handling.
	if strings.Contains(string(src), "os.Stat(version.ProfileSecretFile") || strings.Contains(string(src), "os.Stat(version.EncryptionConfigPath") {
		t.Fatal("prepare.go checks a secret file again instead of acting on the gate's mint answer")
	}
	key, err := os.ReadFile("ciskey.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(key), "host.CreateFileExclusively("); n != 1 {
		t.Fatalf("ciskey.go writes a shared secret %d times; the one write belongs inside mintSharedSecret's", n)
	}
	if !strings.Contains(string(key), "err = mintSharedSecretFor(prof, s.Name, path, func() (os.FileInfo, error) {\n\t\t\t\tif err := confirm(); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n\t\t\t\tfi, err := host.CreateFileExclusively(") {
		t.Fatal("mintSharedSecrets does not write through the second look (mintSharedSecretFor)")
	}
}

// mintSharedSecrets writes each secret 0600 and create-only: the gate asks
// only whether the file exists, so a write must never replace a secret that
// appeared since the gate looked. A secret refused after another was minted
// takes the earlier one back.
func TestMintSharedSecretsCreatesOnlyAndTakesBackTheEarlierOnes(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	one := func() (etcdJoinState, error) { return etcdJoinState{Members: 1}, nil }
	secret := func(name, body string) profile.Secret {
		return profile.Secret{Name: name, Mint: func() ([]byte, error) { return []byte(body), nil }}
	}

	if err := mintSharedSecrets(profiletest.Profile(), []profile.Secret{secret("a.yaml", "A")}, at, noConfirm, one); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(at("a.yaml"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("a.yaml = %v, %v; want it at 0600", fi, err)
	}
	err = mintSharedSecrets(profiletest.Profile(), []profile.Secret{secret("a.yaml", "B")}, at, noConfirm, one)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a second mint = %v, want fs.ErrExist", err)
	}
	if b, _ := os.ReadFile(at("a.yaml")); string(b) != "A" {
		t.Fatalf("a second mint replaced the secret: %q", b)
	}

	boom := errors.New("no entropy")
	err = mintSharedSecrets(profiletest.Profile(), []profile.Secret{secret("b.yaml", "B"), {Name: "c.yaml", Mint: func() ([]byte, error) { return nil, boom }}}, at, noConfirm, one)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "c.yaml") {
		t.Fatalf("mintSharedSecrets = %v, want the failure of c.yaml", err)
	}
	if _, err := os.Stat(at("b.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("b.yaml, minted before c.yaml failed, is still there (%v)", err)
	}
}

func TestLocalEtcdJoinStateOnRealEtcdDatabases(t *testing.T) {
	for _, tc := range []struct {
		file    string
		members int
		bundle  bool
	}{
		{"published.db", 1, true},
		{"deleted.db", 1, false},
		{"recreated.db", 1, true},
		{"members.db", 2, false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "etcd", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			snap := filepath.Join(dir, "member", "snap")
			if err := os.MkdirAll(snap, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(snap, "db"), src, 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := localEtcdJoinState(dir, func() (etcdJoinState, error) {
				t.Error("asked a live etcd for an unlocked database")
				return etcdJoinState{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := (etcdJoinState{Members: tc.members, JoinBundle: tc.bundle}); st != want {
				t.Fatalf("got %+v, want %+v", st, want)
			}
		})
	}
}

// TestRealEtcdDatabasesAreFromThePinnedEtcd fails a bump of the etcd image to a
// new minor version until the fixtures are regenerated, so
// TestLocalEtcdJoinStateOnRealEtcdDatabases reads databases written by the
// etcd minor the cluster runs. It reads the pin generate.sh writes the fixtures
// with: the registry.k8s.io/etcd image in build/images.txt, not etcd_version in
// build/versions.mk, which pins only the host etcdctl and etcdutl. etcd records
// a storage version of major.minor.0, so a patch bump is not caught, and needs
// no new fixtures.
func TestRealEtcdDatabasesAreFromThePinnedEtcd(t *testing.T) {
	images, err := os.ReadFile(filepath.Join("..", "..", "build", "images.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var pinned string
	for line := range strings.Lines(string(images)) {
		if ref, ok := strings.CutPrefix(line, "registry.k8s.io/etcd:"); ok {
			tag, _, _ := strings.Cut(ref, "@")
			pinned, _, _ = strings.Cut(strings.TrimSpace(tag), "-")
		}
	}
	if pinned == "" {
		t.Fatal("build/images.txt pins no registry.k8s.io/etcd image")
	}
	files, err := filepath.Glob(filepath.Join("testdata", "etcd", "*.db"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in testdata/etcd (%v)", err)
	}
	for _, f := range files {
		db, err := bolt.Open(f, 0o400, &bolt.Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("meta"))
			if b == nil {
				return errors.New("no meta bucket")
			}
			got = string(b.Get([]byte("storageVersion")))
			return nil
		})
		db.Close()
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if majorMinor(got) != majorMinor(pinned) {
			t.Errorf("%s was written by etcd %q, but build/images.txt pins etcd %s: "+
				"run internal/cluster/testdata/etcd/generate.sh", f, got, pinned)
		}
	}
}

func majorMinor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// cisGateMints is sharedSecretGate for the cis profile with its key at path,
// answered as whether it mints.
func cisGateMints(path string, read func() (etcdJoinState, error)) (bool, error) {
	minted, err := sharedSecretGate(profiletest.Profile(), func(string) string { return path }, read)
	return len(minted) > 0, err
}

func noConfirm() error { return nil }

// The keyed-bundle answer proves the at-rest key alone: its proof opens under
// that key. A profile's other shared secret is taken back on the second
// look's refusal even when the bundle is keyed, because nothing shows the
// bundle carries its bytes.
func TestSecondLookSparesOnlyTheKeyTheProofAuthenticates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.yaml")
	err := mintSharedSecret(path, func() (os.FileInfo, error) {
		if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
			return nil, err
		}
		return os.Stat(path)
	}, func() (etcdJoinState, error) {
		return etcdJoinState{Members: 1, JoinBundle: true, CarriesKey: true}, nil
	})
	if err == nil {
		t.Fatal("the second look allowed a mint while a bundle is published")
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("a.yaml was kept on the at-rest key's proof (%v): %v", serr, err)
	}
}

// A batch that fails on a later secret keeps an at-rest key it minted earlier
// once the published bundle is proven to carry it, and takes back the rest.
func TestMintSharedSecretsKeepsAnEarlierKeyTheBundleCarries(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	reads := 0
	read := func() (etcdJoinState, error) {
		reads++
		if reads == 1 {
			return etcdJoinState{Members: 1}, nil
		}
		return etcdJoinState{Members: 1, JoinBundle: true, CarriesKey: true}, nil
	}
	key := filepath.Base(version.EncryptionConfigPath)
	secret := func(name, body string) profile.Secret {
		return profile.Secret{Name: name, Mint: func() ([]byte, error) { return []byte(body), nil }}
	}
	err := mintSharedSecrets(profiletest.Profile(), []profile.Secret{secret(key, "K"), secret("b.yaml", "B")}, at, noConfirm, read)
	if err == nil {
		t.Fatal("mintSharedSecrets = nil, want b.yaml's refusal")
	}
	if b, rerr := os.ReadFile(at(key)); rerr != nil || string(b) != "K" {
		t.Errorf("the earlier key the bundle carries was taken back (%q, %v): %v", b, rerr, err)
	}
	if !strings.Contains(err.Error(), "was left in place") {
		t.Errorf("the refusal does not say the key was kept: %v", err)
	}
	if _, serr := os.Stat(at("b.yaml")); !os.IsNotExist(serr) {
		t.Errorf("b.yaml survived its refusal: %v", serr)
	}
}
