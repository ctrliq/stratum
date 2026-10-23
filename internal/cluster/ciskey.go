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

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// etcdJoinState is what the key gate needs from this node's etcd: how many
// control planes it has, and whether a control-plane join bundle (the
// stratum-certs Secret) is published that a further one could join with.
//
// "Published" is the Secret's existence, never a judgment of its entries: the
// Secret carries one ciphertext entry per live grant beside the legacy certs
// entry, and the offline reader below counts the bundle from the storage key
// alone, whatever entries it carries. Which entries a published bundle holds —
// and whether one of them carries this node's key — is the Secret owner's
// question over the API, not this etcd reader's.
type etcdJoinState struct {
	Members    int
	JoinBundle bool
	// CarriesKey is whether the published bundle authenticates THIS node's
	// at-rest key: an AES-GCM open of the encryption-key-proof entry the
	// bundle owner (ha.PublishedBundleCarriesKey) runs over the Secret API,
	// never an annotation. The offline reader below can never set it — a
	// proof is a decryption only the Secret API's owner can run — so an
	// offline or API-less answer is always unkeyed, which is what the second
	// look treats it as.
	CarriesKey bool
}

// loadNodeState is LoadState behind a seam, so a test decides whether this
// node has saved state instead of reading the host's.
var loadNodeState = LoadState

// liveEtcdJoinState reads this control plane's etcd — the offline backend read
// first, the running member when the database is locked; a var so a test can
// answer for an etcd it does not run. Its bundle answer is etcd's own, and it
// remains the gate's fallback for when the Secret API cannot be reached.
var liveEtcdJoinState = func() (etcdJoinState, error) {
	return localEtcdJoinState(version.EtcdDataDir, func() (etcdJoinState, error) {
		n, bundle, err := ha.JoinState("127.0.0.1")
		return etcdJoinState{Members: n, JoinBundle: bundle}, err
	})
}

// publishedBundleCarriesKey is the bundle owner's answer over the Secret API:
// whether a stratum-certs bundle is published, and whether it carries this
// node's at-rest key — the bytes of version.EncryptionConfigPath, nil when
// there is none yet, which is the first gate's case (it runs before any key
// exists). A var so a test answers for an apiserver this node does not run.
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

// readEtcdJoinState is the composed answer PrepareController hands the gate
// and the second look: liveJoinState, over both live legs. A var so a test can
// answer for both at once, the way a refusal on a live HA cluster has to reach
// PrepareController before anything on the host changes.
var readEtcdJoinState = liveJoinState

// liveJoinState answers the gate's question from the two live legs: the member
// count from this node's etcd, and the bundle answer from the stratum-certs
// Secret when its API answers — whether the Secret is published, and whether
// it carries this node's key. etcd's own bundle answer stands in only when the
// API cannot be reached (a first boot, a stopped apiserver), and it can never
// claim a key, because the proof is a decryption only the Secret API can run.
//
// The Secret outranks etcd's raw storage: the live Secret is what a join will
// actually download, and the storage key can trail it — a Secret deleted while
// this node was down, a write acknowledged in the last instant before an
// unclean stop. Answering from the key's existence also couples the gate to
// the apiserver's etcd layout (ha.JoinBundleEtcdKey under the default prefix),
// which the live read avoids whenever the API answers; the offline read keeps
// that coupling, because with etcd stopped there is nothing else to ask. That
// read takes neither live leg — localEtcdJoinState is called directly, with no
// bundle owner in it — and never claims a key.
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

// sharedSecretGate asks encryptionKeyRefusal exactly when PrepareController is
// about to mint a profile's shared secrets: the profile declares one that does
// not exist at at(name). It returns the secrets to mint, so PrepareController
// acts on this one answer instead of looking at the files a second time with
// different error handling. A secret already on disk -- the one a joiner
// downloaded with the shared certs, or this node's own from an earlier start --
// is used as it is, and etcd is never consulted for it. Only a successful Stat
// proves that; any other error (EACCES, EIO, a symlink loop) refuses, because
// skipping the gate on it would start an apiserver against a file it cannot
// use. The cis profile's one secret is the at-rest encryption config, which
// the refusals name.
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

// sharedSecretRefusal is encryptionKeyRefusal for whichever profile the node
// runs. The cis profile keeps that function's words: its one secret is the
// at-rest encryption key, and the remedy is the cis one, which points the
// operator at Stratum PRO's documentation. Another profile's refusal names
// the profile and the secrets it would mint, and gives the remedy in the
// profile's own terms: nothing in it says --profile cis or spec.profile: cis,
// and nothing sends the operator to the cis remedy's re-encryption, which
// covers no secret of another profile.
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

// mintSharedSecrets mints each secret the gate returned for prof, at at(name),
// 0600 and create-only, each through the second look, with confirm run before
// each write and after the last. A secret that fails takes back the ones this
// start minted before it, so a refusal leaves none of them behind.
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

// encryptionKeyRefusal refuses to mint an at-rest encryption key that some
// control plane of this cluster would not have, and returns the state the
// answer came from, so the second look (mintSharedSecret) can tell a refusal
// its keyed-bundle answer corrects from one it cannot.
//
// Every apiserver of a cluster must hold the byte-identical key, and the only
// thing that hands it out is the HA join (ha/certs.go ships
// encryption-config.yaml with the shared certs). So a key minted here, on a
// cluster whose other control planes joined before CIS was on, is one only this
// apiserver has: a Secret written through it cannot be read through the others,
// and the reverse. That is refused rather than repaired, because distributing a
// key to live control planes is a coordinated change to every apiserver, not
// something one node's start can do.
//
// Counting today's members is not enough on its own: a control plane can still
// join tomorrow with a bundle uploaded before this key existed, which carries
// no key, and nothing would stop it. A join needs a published stratum-certs
// Secret, and with no key on this node every published one is keyless, so one
// being there is refused too. Once it is gone, the next `stratum token create
// --control-plane` uploads from this node, key included. What is left is a join
// that downloaded its bundle before the Secret was deleted and adds its etcd
// member after this check; a joiner with --profile cis refuses that bundle
// itself (ha.requireSharedSecrets). mintSharedSecret asks again after the key
// is written, which narrows the window between this check and the write; the
// remaining cases are the operator's to catch. The gate's
// answer also says whether the published bundle carries this node's key
// (etcdJoinState.CarriesKey, from the bundle owner over the Secret API); the
// first gate refuses a keyed bundle exactly as a keyless one, because the
// members check has already stood, and the keyed answer is the second look's to
// act on: there, sparing a key the published bundle authenticates closes the
// case where deleting the key just moves the problem to the next start.
//
// It is asked only when there is no key yet, so a control plane that received
// one at join is never consulted. A fresh install has no etcd data, which
// counts as no members and no bundle, so the first control plane mints the key
// that every later joiner receives. When etcd cannot be read the answer is a
// refusal, not a guess: a wrong "only member" here is the bug itself.
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

// mintSharedSecret writes a shared secret, then asks encryptionKeyRefusal
// again, and takes the secret back off disk if the answer has changed.
//
// The gate at the top of PrepareController runs before host prerequisites and
// PKI, so a join bundle published, or a member added, while those ran is not in
// its answer. Once the key file exists, every bundle uploaded
// from this node carries it, so this second look sees every keyless bundle and
// every member added before the key was there -- except an upload that read
// this node's certificates before the write and publishes them after this
// check, which must not be started while turning CIS on.
//
// Whether the bundle the second look sees is one this node's key decrypts is
// the bundle owner's keyed answer (etcdJoinState.CarriesKey): a bundle
// published after the write, from an upload on this node, carries exactly the
// key just written, and the rollback would remove a key that bundle hands out.
// The keyed answer closes exactly that case: when the published bundle
// authenticates the key this start wrote, the second look refuses but leaves
// the key, because a joiner downloading that bundle receives it anyway and
// taking it back only moves the problem to the next start, which would mint a
// second, different one. An answer that cannot run the decryption — the
// offline read, or an API failure fallen back to etcd — is unkeyed, and the
// rollback of today stands.
//
// Whatever goes wrong, no key this start wrote is left behind: not after a
// refusal, and not after a failed write, which could otherwise leave a partial
// file that the next start would take for a key and skip the gate on. But only
// the key this start wrote: write reports the file it created, and the rollback removes the file at path only while it is that
// same inode. A key that appeared at path some other way -- the create-only
// write refuses to replace it, with fs.ErrExist -- is left alone. Each error
// says which phase failed and what became of the key, because the operator's
// next step depends on both.
func mintSharedSecret(path string, write func() (os.FileInfo, error), read func() (etcdJoinState, error)) error {
	return mintSharedSecretFor(profile.Profile{Name: profile.CISName}, profile.CISEncryptionConfigName, path, write, read)
}

// mintSharedSecretFor is mintSharedSecret for whichever profile the node runs:
// the cis profile's words for its at-rest key, and the profile's and secret's
// own names for any other. The keyed-bundle answer that spares a key exists
// only for the cis profile's encryption config, whose key the bundle proves,
// so another profile's refusal always takes its secret back.
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
		// The keyed-bundle answer corrects one refusal only: the bundle
		// refusal on a sole control plane. A members refusal stands whatever
		// the bundle carries, because the control planes that joined before
		// the key existed hold no key to verify the bundle against — a keyed
		// bundle says only that a FUTURE join would receive this key.
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

// publishedRetains reports whether the secret minted at path must stay because
// the published bundle is proven to carry it: the keyed-bundle answer on a sole
// control plane. The proof is the at-rest key's own — its entry opens under
// the key — so it speaks for the cis profile's secret, the encryption config
// version.EncryptionConfigPath names, alone. No
// other shared secret has one: the bundle's files are sealed under a
// certificate key this node does not hold, so nothing shows that a bundle
// carries their bytes, and they are taken back as an unkeyed answer's key is.
func publishedRetains(path string, st etcdJoinState) bool {
	return filepath.Base(path) == filepath.Base(version.EncryptionConfigPath) && st.Members <= 1 && st.JoinBundle && st.CarriesKey
}

// keyInTheWay explains a create-only key write that found path taken. The next
// start trusts whatever key file is there without consulting etcd
// (sharedSecretGate), so the operator has to check it first. A symlink is
// named as such: the gate follows it and sees no key when its target is
// missing, and the write refuses to replace it, so every start would end here.
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

// syncKeyDir is host.SyncDir behind a seam, so a test can fail the rollback's
// directory sync.
var syncKeyDir = host.SyncDir

// removeMinted takes back the key mintSharedSecret's write created, and says
// in cause what became of it. created is nil when the write created nothing;
// a file at path that is not created is someone else's and stays. The removal
// is synced to the directory: an unsynced remove can be undone
// by a power loss, bringing back a refused key that the next start would trust.
// A write that left the key under a second, temporary name as well
// (host.ErrStrayCopy) never reports the key gone.
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

// localEtcdJoinState reads the etcd join state recorded in this node's etcd
// data.
//
// Offline first, from the backend database, because the common way to turn CIS
// on is a restart and etcd is not necessarily running at this point: after a
// reboot nothing is. When etcd IS running (its containers outlive a supervisor
// restart) it holds the database's exclusive lock, so the read-only open times
// out, and the running member answers instead.
//
// Only an absent or empty etcd data directory on a node with no saved node
// state is a fresh install, with no members and nothing published. Saved state
// with no etcd data is a controller that lost its etcd directory, whose saved
// membership PrepareController goes on to keep: a key minted there would be one
// the restored members lack. So existing state, or state that cannot be read,
// is an error here. A directory with other contents but no database -- the
// database lost or moved, a WAL left behind -- belongs to a node with etcd
// history this code cannot read, and is an error: calling it fresh would mint a
// key that survives the backend coming back.
//
// The offline read is the backend alone, and after an unclean stop the backend
// can trail the WAL: etcd commits it in batches, every 100ms or so, and replays
// the rest from the WAL when it starts. So a member-add or a stratum-certs
// write acknowledged in the last instant before a crash can be missing here.
// That is accepted rather than refused: failing closed would refuse every `up
// --profile cis` after an unclean stop, blocking crash recovery to guard a
// window that also needs a CIS enable to follow that exact crash.
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
			// Name the path taken: "etcd could not be read" alone does not say
			// that the database was there but locked, which is what sent this
			// to the live read.
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

// countBackendMembers counts the entries of etcd's "members" bucket: one per
// member, keyed by member ID, valued with its JSON. Like backendHasKey this is
// etcd's internal layout (storage/schema/membership.go), checked against real
// etcd databases by TestLocalEtcdJoinStateOnRealEtcdDatabases. A
// learner is an entry like any other member, and a removed member is deleted
// from it (and recorded in "members_removed"), so this is the current set. A
// database that exists always has this node as a member, so zero is an
// incomplete or unknown backend and an error, never the "fresh install" answer
// that only a missing data directory gives.
func countBackendMembers(db *bolt.DB) (int, error) {
	n := 0
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("members"))
		if b == nil {
			return fmt.Errorf("etcd backend %s has no members bucket", db.Path())
		}
		return b.ForEach(func(k, v []byte) error {
			// Every value is a member's JSON; anything else means this is not
			// the layout this code knows, and a miscount is the one answer
			// that must not be returned.
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

// backendHasKey reports whether key currently exists in etcd's keyspace, read
// from the backend's "key" bucket.
//
// The answer is the key's existence alone. The stored value is the apiserver's
// own encoding of the Secret, and whether a bundle is published stays a fact
// about the Secret's existence, never about its contents: under the
// multi-entry grant Secret the Secret carries one ciphertext entry per live
// grant beside the legacy one, and every one of those states — and any value
// this reader cannot decode — counts as published, because the Secret is
// there.
//
// That bucket is etcd's MVCC history: one entry per revision of every key,
// keyed by the revision (8-byte main, '_', 8-byte sub, big-endian so the bucket
// iterates in revision order), with an 18th byte 't' when the revision is a
// delete. Anything else, a missing separator included, is refused as etcd's
// own decoder refuses it, rather than read as a revision. So the key exists when its latest revision is not a tombstone. Compaction drops
// older revisions but never a key's latest one while the key exists.
//
// This is etcd's internal storage layout (server/storage/mvcc and
// storage/schema in the etcd source), not an API, and nothing in etcd promises
// to keep it. It is coupled on two sides: to etcd for the bucket and the
// revision encoding, and to kube-apiserver for the key's name
// (ha.JoinBundleEtcdKey). Each side has a test against the real thing:
// TestLocalEtcdJoinStateOnRealEtcdDatabases reads databases written by the etcd
// this repository ships, and staticpod's TestApiserverKeepsTheDefaultEtcdPrefix
// pins the prefix. Moving the etcd image in build/images.txt to a new minor
// version fails TestRealEtcdDatabasesAreFromThePinnedEtcd until the fixtures
// are regenerated (testdata/etcd/generate.sh), and a layout change in the new
// etcd then fails the first.
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
			if err := kv.Unmarshal(v); err != nil {
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
