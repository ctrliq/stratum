package ha

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// A published node.yaml must not outlive compensated membership. If syncing
// it fails, keep both the state and journal for reconciliation on the next run.
var errJoinStatePublished = errors.New("published join state could not be made durable")

var errJoinIntentStale = errors.New("recorded join member is absent; repeat online preparation")

func saveJoinState(st *nodestate.NodeState) error {
	return saveJoinStateWithSync(st, syncJoinFile)
}

func saveJoinStateWithSync(st *nodestate.NodeState, syncPath func(string) error) error {
	if err := nodestate.SaveState(st); err != nil {
		// A previous failed save may already have published node.yaml. Do
		// not compensate membership while that record can still survive.
		if _, stateErr := os.Stat(nodestate.StatePath()); !errors.Is(stateErr, fs.ErrNotExist) {
			return fmt.Errorf("%w: save state: %w", errJoinStatePublished, err)
		}
		return err
	}
	for _, path := range []string{nodestate.StatePath(), filepath.Dir(nodestate.StatePath())} {
		if err := syncPath(path); err != nil {
			return fmt.Errorf("%w: sync %s: %w", errJoinStatePublished, path, err)
		}
	}
	return nil
}

func syncJoinFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Sync the local preparation before recording its durable snapshot. Resuming
// without the API depends on retaining shared keys and cluster configuration.
func persistJoinPreparation() error {
	return persistJoinPreparationWithSync(syncJoinFile)
}

func persistJoinPreparationWithSync(syncPath func(string) error) error {
	for _, root := range []string{version.PKIDir, version.K8sConfDir, version.KubeconfigDir} {
		var dirs []string
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				return syncPath(path)
			}
			if entry.IsDir() {
				dirs = append(dirs, path)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("sync retained preparation in %s: %w", root, err)
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			if err := syncPath(dirs[i]); err != nil {
				return err
			}
		}
	}
	// Persist the names of directories freshly created by host preparation,
	// including DataDir itself, before any member can be added remotely.
	for _, dir := range []string{version.DataDir, filepath.Dir(version.DataDir)} {
		if err := syncPath(dir); err != nil {
			return err
		}
	}
	return nil
}

func verifyJoinResumeTrust(pin string) error {
	if err := clusterinfo.ValidatePin(pin); err != nil {
		return err
	}
	matched := false
	for _, name := range []string{pki.RootCAName, pki.StagedRootCAName, "front-proxy-ca", pki.EtcdCAName} {
		if name == pki.StagedRootCAName {
			_, certErr := os.Stat(filepath.Join(version.PKIDir, name+".crt"))
			_, keyErr := os.Stat(filepath.Join(version.PKIDir, name+".key"))
			if errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist) {
				continue
			}
		}
		pair, err := tls.LoadX509KeyPair(filepath.Join(version.PKIDir, name+".crt"), filepath.Join(version.PKIDir, name+".key"))
		if err != nil {
			return fmt.Errorf("retained %s certificate/key pair: %w", name, err)
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		if (name == pki.RootCAName || name == pki.StagedRootCAName) && clusterinfo.PinFor(cert) == pin {
			matched = true
		}
	}
	if !matched {
		return errors.New("retained cluster CA does not match the recorded discovery pin")
	}
	for _, name := range []string{"sa.key", "sa.pub"} {
		if b, err := os.ReadFile(filepath.Join(version.PKIDir, name)); err != nil || len(b) == 0 {
			return fmt.Errorf("missing or unreadable retained service-account material %s", name)
		}
	}
	return nil
}

// Prefer a quorum-backed view; fall back to a survivor's own membership only
// for positive adoption. Callers must not infer absence from that stale view.
func joinMembershipVia(ips []string, read func(string) (etcdMembership, error)) (etcdMembership, error) {
	var fallback *etcdMembership
	var last error
	for _, ip := range ips {
		m, err := read(ip)
		if err != nil {
			last = err
			continue
		}
		if m.Quorum {
			return m, nil
		}
		if fallback == nil {
			fallback = &m
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	if last == nil {
		last = errors.New("no etcd endpoints")
	}
	return etcdMembership{}, last
}
