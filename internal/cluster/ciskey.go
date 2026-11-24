package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/protobuf/proto"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

type etcdJoinState struct {
	Members    int
	JoinBundle bool
	// Only the live Secret read can set it; an offline read can never prove a key.
	CarriesKey bool
}

// A seam so a test can answer for this node's saved state.
var loadNodeState = LoadState

// A var so a test can answer for an etcd this node does not run.
var liveEtcdJoinState = func() (etcdJoinState, error) {
	return localEtcdJoinState(version.EtcdDataDir, func() (etcdJoinState, error) {
		n, bundle, err := ha.JoinState("127.0.0.1")
		return etcdJoinState{Members: n, JoinBundle: bundle}, err
	})
}

// A var so a test can answer for an apiserver this node does not run.
var publishedBundleCarriesKey = func() (published, carries bool, err error) {
	config, err := os.ReadFile(version.EncryptionConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		config = nil
	} else if err != nil {
		return false, false, err
	}
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, false, err
	}
	return ha.PublishedBundleCarriesKey(cs, config)
}

// A var so a test can answer for both live legs at once.
var readEtcdJoinState = liveJoinState

// The live Secret's bundle answer outranks etcd's storage key when the API answers.
func liveJoinState() (etcdJoinState, error) {
	st, err := liveEtcdJoinState()
	if err != nil {
		return etcdJoinState{}, err
	}
	published, carries, err := publishedBundleCarriesKey()
	if err != nil {
		return etcdJoinState{Members: st.Members, JoinBundle: st.JoinBundle}, nil
	}
	return etcdJoinState{Members: st.Members, JoinBundle: published, CarriesKey: published && carries}, nil
}

// A Stat error other than not-exist refuses: the gate must not skip a file it cannot read.
func sharedSecretGate(prof profile.Profile, at func(string) string, read func() (etcdJoinState, error)) (mint []profile.Secret, err error) {
	for _, s := range prof.SharedSecrets {
		path := at(s.Name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			if prof.Name == profile.CISName {
				return nil, fmt.Errorf("--profile cis: cannot tell whether an at-rest encryption key exists at %s (%w); refusing "+
					"to start rather than skip the check that one may be created", path, err)
			}
			return nil, fmt.Errorf("--profile %s: cannot tell whether the shared secret %s exists at %s (%w); refusing "+
				"to start rather than skip the check that one may be created", prof.Name, s.Name, path, err)
		}
		mint = append(mint, s)
	}
	if len(mint) == 0 {
		return nil, nil
	}
	if _, err := sharedSecretRefusal(prof, secretNames(mint), read); err != nil {
		return nil, err
	}
	return mint, nil
}

func secretNames(secrets []profile.Secret) []string {
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		names = append(names, s.Name)
	}
	return names
}

func sharedSecretRefusal(prof profile.Profile, names []string, read func() (etcdJoinState, error)) (etcdJoinState, error) {
	if prof.Name == profile.CISName {
		return encryptionKeyRefusal(read)
	}
	secrets := strings.Join(names, ", ")
	st, err := read()
	if err != nil {
		return etcdJoinState{}, fmt.Errorf("--profile %s: the shared secret %s does not exist on this control plane yet, and its etcd could not "+
			"be read (%w), so it cannot be confirmed to be the only control plane; a secret minted here would not be "+
			"shared with any other. It is read from a running etcd on 127.0.0.1:%d, or, when etcd is "+
			"stopped, from its database under %s: restore the running etcd's client endpoint, or stop etcd so "+
			"the database can be read, and retry",
			prof.Name, secrets, err, version.EtcdClientPort, version.EtcdDataDir)
	}
	if st.Members > 1 {
		return st, fmt.Errorf("--profile %s: refusing to create the shared secret %s: this control plane's etcd has %d "+
			"members, and a profile's shared secrets reach a control plane only when it joins, so one created now would "+
			"exist on this control plane alone, and the %s profile would run with different secrets on different control planes. "+
			"Select the profile at install time instead: start the first control plane with --profile %s (or spec.profile: %s) "+
			"before any other joins, and the rest receive its secrets when they join. To select it on this running cluster, "+
			"put the same %s on every control plane under %s by hand, delete the published join bundle "+
			"(`sudo %s kubectl -n kube-system delete secret stratum-certs`; any control-plane join command already handed out stops working), "+
			"restart each control plane with --profile %s, and only then run `stratum token create --control-plane` for new joins",
			prof.Name, secrets, st.Members, prof.Name, prof.Name, prof.Name, secrets, version.PKIDir, version.StratumInstallPath, prof.Name)
	}
	if st.JoinBundle {
		return st, fmt.Errorf("--profile %s: refusing to create the shared secret %s: a control-plane join bundle (the "+
			"Secret kube-system/stratum-certs) is published, and it was uploaded before this secret existed, so a "+
			"control plane that joins with it would not have it. Deleting it needs a running apiserver, which this refusal does not start: "+
			"start this control plane without --profile %s (remove spec.profile: %s from its config if it is set there), "+
			"delete the Secret with `sudo %s kubectl -n kube-system delete secret stratum-certs` (any "+
			"control-plane join command already handed out stops working), stop it, start it again with --profile %s, and "+
			"only then run `stratum token create --control-plane` for new joins: that bundle carries the secret",
			prof.Name, secrets, prof.Name, prof.Name, version.StratumInstallPath, prof.Name)
	}
	return st, nil
}

// A secret that fails takes back the ones this start minted before it.
func mintSharedSecrets(prof profile.Profile, secrets []profile.Secret, at func(string) string, confirm func() error, read func() (etcdJoinState, error)) error {
	type minted struct {
		path    string
		created os.FileInfo
	}
	var done []minted
	for _, s := range secrets {
		path := at(s.Name)
		b, err := s.Mint()
		if err == nil {
			var created os.FileInfo
			err = mintSharedSecretFor(prof, s.Name, path, func() (os.FileInfo, error) {
				if err := confirm(); err != nil {
					return nil, err
				}
				fi, err := host.CreateFileExclusively(path, b, 0o600)
				created = fi
				return fi, err
			}, read)
			if err == nil {
				done = append(done, minted{path, created})
				continue
			}
		} else {
			err = fmt.Errorf("mint the shared secret %s: %w", s.Name, err)
		}
		errs := []error{err}
		var st etcdJoinState
		if len(done) > 0 {
			if got, rerr := read(); rerr == nil {
				st = got
			}
		}
		for _, m := range done {
			if publishedRetains(m.path, st) {
				errs = append(errs, fmt.Errorf("the at-rest encryption key this start wrote at %s was left in place: "+
					"the published bundle carries it, so taking it back would not unpublish it", m.path))
				continue
			}
			errs = append(errs, removeMinted(m.path, m.created, fmt.Errorf("a later shared secret of this start was refused")))
		}
		return errors.Join(errs...)
	}
	if len(done) == 0 {
		return nil
	}
	return confirm()
}

// Refused rather than repaired: distributing a key to live control planes takes more than one node.
func encryptionKeyRefusal(read func() (etcdJoinState, error)) (etcdJoinState, error) {
	st, err := read()
	if err != nil {
		return etcdJoinState{}, fmt.Errorf("--profile cis: no at-rest encryption key exists on this control plane, and its etcd could not "+
			"be read (%w), so it cannot be confirmed to be the only control plane; a key minted here would not be "+
			"shared with any other. It is read from a running etcd on 127.0.0.1:%d, or, when etcd is "+
			"stopped, from its database under %s: restore the running etcd's client endpoint, or stop etcd so "+
			"the database can be read, and retry",
			err, version.EtcdClientPort, version.EtcdDataDir)
	}
	if st.Members > 1 {
		return st, fmt.Errorf("--profile cis: refusing to create an at-rest encryption key: this control plane's etcd has %d "+
			"members, and the key is shared with a control plane only when it joins, so a key created now would "+
			"exist on this apiserver alone — Secrets written through it could not be read through the other control "+
			"planes, and theirs not through it. Enable CIS at install time instead: start the first "+
			"control plane with --profile cis (or spec.profile: cis) before any other joins, and the rest receive its key "+
			"when they join. To enable it on this running cluster, see the Stratum PRO documentation: %s",
			st.Members, profile.ProDocsURL)
	}
	if st.JoinBundle {
		return st, fmt.Errorf("--profile cis: refusing to create an at-rest encryption key: a control-plane join bundle (the "+
			"Secret kube-system/stratum-certs) is published, and it was uploaded before this key existed, so a "+
			"control plane that joins with it would have no key — Secrets written through this apiserver could not "+
			"be read through it. Deleting it needs a running apiserver, which this refusal does not start: "+
			"start this control plane without --profile cis or --cis (remove spec.profile: cis and spec.cis: true from its config if they are set there), "+
			"delete the Secret with `sudo %s kubectl -n kube-system delete secret stratum-certs` (any "+
			"control-plane join command already handed out stops working), stop it, start it again with --profile cis, and "+
			"only then run `stratum token create --control-plane` for new joins: that bundle carries the key",
			version.StratumInstallPath)
	}
	return st, nil
}

// A second look after the write takes the key back, unless the published bundle carries it.
func mintSharedSecret(path string, write func() (os.FileInfo, error), read func() (etcdJoinState, error)) error {
	return mintSharedSecretFor(profile.Profile{Name: profile.CISName}, profile.CISEncryptionConfigName, path, write, read)
}

func mintSharedSecretFor(prof profile.Profile, name, path string, write func() (os.FileInfo, error), read func() (etcdJoinState, error)) error {
	cis := prof.Name == profile.CISName
	what := fmt.Sprintf("--profile %s: write the shared secret %s", prof.Name, name)
	if cis {
		what = "--profile cis: write the at-rest encryption key"
	}
	created, err := write()
	if err != nil {
		cause := fmt.Errorf("%s: %w", what, err)
		if errors.Is(err, fs.ErrExist) {
			err := keyInTheWay(path, cause)
			if errors.Is(err, host.ErrStrayCopy) {
				return fmt.Errorf("%w. Also delete the temporary file named above: it holds all or part of the "+
					"key this start generated", err)
			}
			return err
		}
		return removeMinted(path, created, cause)
	}
	st, err := sharedSecretRefusal(prof, []string{name}, read)
	if err != nil {
		cause := fmt.Errorf("--profile %s: a second check of etcd, made after the shared secret %s "+
			"was written, refused it: %w", prof.Name, name, err)
		if cis {
			cause = fmt.Errorf("--profile cis: a second check of etcd, made after the at-rest key "+
				"was written, refused it: %w", err)
		}
		// A keyed bundle spares the key only here: a members refusal stands whatever it carries.
		if publishedRetains(path, st) {
			return fmt.Errorf("%w. The published bundle carries this key — its proof entry opens under "+
				"exactly the key this start wrote — so a control plane that joins with it receives this "+
				"key, and the key was left in place: taking it back would not unpublish the bundle, and "+
				"the next start would mint a second, different one. Follow the refusal's remedy above "+
				"before starting again", cause)
		}
		return removeMinted(path, created, cause)
	}
	return nil
}

// Only the at-rest key has a proof the bundle carries; other secrets are taken back unkeyed.
func publishedRetains(path string, st etcdJoinState) bool {
	return filepath.Base(path) == filepath.Base(version.EncryptionConfigPath) && st.Members <= 1 && st.JoinBundle && st.CarriesKey
}

func keyInTheWay(path string, cause error) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w; %s is a symlink, which the at-rest key is never written through: remove it, "+
			"or point it at this cluster's key, before starting again", cause, path)
	}
	return fmt.Errorf("%w; a key appeared at %s during this start and was left in place, because it may be "+
		"one the cluster already uses. The next start uses it as it is, without consulting etcd: before "+
		"starting again, confirm it is the key every other control plane holds (compare sha256sum), or remove it",
		cause, path)
}

// A seam so a test can fail the rollback's directory sync.
var syncKeyDir = host.SyncDir

// The removal is synced to the directory: an unsynced remove can be undone by a power loss.
func removeMinted(path string, created os.FileInfo, cause error) error {
	if created == nil {
		if errors.Is(cause, host.ErrStrayCopy) {
			return fmt.Errorf("%w; no key was written to %s, but the temporary file named above holds all or "+
				"part of the key this start generated: delete that file before retrying", cause, path)
		}
		return fmt.Errorf("%w; no key was written to %s", cause, path)
	}
	cur, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w; no key is left at %s", cause, path)
	}
	if err != nil {
		return fmt.Errorf("%w; could not inspect %s to remove the key this start wrote (%v): check it before "+
			"retrying, or the next start may take it for a key this cluster shares", cause, path, err)
	}
	if !os.SameFile(cur, created) {
		return fmt.Errorf("%w; the file now at %s is not the key this start wrote, so it was left in place", cause, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w; the key could NOT be removed from %s (%v): delete it before retrying, or the "+
			"next start will take it for a key this cluster shares", cause, path, err)
	}
	if err := syncKeyDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w; the key was removed from %s, but the removal could not be made durable (%v): "+
			"the rollback is incomplete, and after a power loss the key could return -- check that %s is "+
			"absent before retrying", cause, path, err, path)
	}
	if errors.Is(cause, host.ErrStrayCopy) {
		return fmt.Errorf("%w; the key was removed from %s, but the temporary file named above still holds it: "+
			"delete that file before retrying", cause, path)
	}
	return fmt.Errorf("%w; the key was removed from %s, so none was left behind", cause, path)
}

// The backend can trail the WAL after an unclean stop; failing closed would block crash recovery.
func localEtcdJoinState(dataDir string, live func() (etcdJoinState, error)) (etcdJoinState, error) {
	path := filepath.Join(dataDir, "member", "snap", "db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		entries, err := os.ReadDir(dataDir)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0) {
			if _, err := loadNodeState(); err == nil {
				return etcdJoinState{}, fmt.Errorf("this node has saved node state (%s) but no etcd data in %s, "+
					"so it is not a fresh install and its membership cannot be read", StatePath(), dataDir)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return etcdJoinState{}, fmt.Errorf("read node state %s: %w", StatePath(), err)
			}
			return etcdJoinState{}, nil
		}
		if err != nil {
			return etcdJoinState{}, fmt.Errorf("inspect etcd data %s: %w", dataDir, err)
		}
		return etcdJoinState{}, fmt.Errorf("etcd data %s has contents but no backend database %s, so this "+
			"node's membership cannot be read", dataDir, path)
	} else if err != nil {
		return etcdJoinState{}, err
	}
	db, err := bolt.Open(path, 0o400, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if errors.Is(err, berrors.ErrTimeout) {
		st, err := live()
		if err != nil {
			// Say the database was locked: that is what sent this to the live read.
			return etcdJoinState{}, fmt.Errorf("etcd database %s is locked by a running etcd, so it was read "+
				"live from 127.0.0.1:%d instead, and that failed: %w", path, version.EtcdClientPort, err)
		}
		return st, nil
	}
	if err != nil {
		return etcdJoinState{}, fmt.Errorf("open etcd backend %s: %w", path, err)
	}
	defer db.Close()
	n, err := countBackendMembers(db)
	if err != nil {
		return etcdJoinState{}, err
	}
	bundle, err := backendHasKey(db, []byte(ha.JoinBundleEtcdKey))
	if err != nil {
		return etcdJoinState{}, err
	}
	return etcdJoinState{Members: n, JoinBundle: bundle}, nil
}

// A database always has this node as a member, so a zero count means the read is incomplete.
func countBackendMembers(db *bolt.DB) (int, error) {
	n := 0
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("members"))
		if b == nil {
			return fmt.Errorf("etcd backend %s has no members bucket", db.Path())
		}
		return b.ForEach(func(k, v []byte) error {
			// A non-JSON value means an unknown layout; a miscount must not be returned.
			if !json.Valid(v) {
				return fmt.Errorf("etcd backend %s: member %x is not JSON", db.Path(), k)
			}
			n++
			return nil
		})
	})
	if err == nil && n == 0 {
		return 0, fmt.Errorf("etcd backend %s records no members", db.Path())
	}
	return n, err
}

// etcd's MVCC: revision keys iterate in order; compaction never drops a live key's latest revision.
func backendHasKey(db *bolt.DB, key []byte) (bool, error) {
	exists := false
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("key"))
		if b == nil {
			return fmt.Errorf("etcd backend %s has no key bucket", db.Path())
		}
		return b.ForEach(func(rev, v []byte) error {
			if (len(rev) != 17 && !(len(rev) == 18 && rev[17] == 't')) || rev[8] != '_' {
				return fmt.Errorf("etcd backend %s: revision %x is not in the layout this code knows", db.Path(), rev)
			}
			var kv mvccpb.KeyValue
			if err := proto.Unmarshal(v, &kv); err != nil {
				return fmt.Errorf("etcd backend %s: revision %x: %w", db.Path(), rev, err)
			}
			if bytes.Equal(kv.Key, key) {
				exists = len(rev) == 17
			}
			return nil
		})
	})
	return exists, err
}
