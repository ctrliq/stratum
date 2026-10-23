package certs

import (
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/token"
)

// KubeletCAPhases are the forward phases of `certs migrate-kubelet-client-ca`,
// in order.
var KubeletCAPhases = []string{pki.KubeletPhaseTrustBoth, pki.KubeletPhaseIssueNew, pki.KubeletPhaseDropRoot}

// KubeletCARollbackPhases undo them, in order.
var KubeletCARollbackPhases = []string{pki.KubeletPhaseReAddRoot, pki.KubeletPhaseReissueRoot, pki.KubeletPhaseRemoveCA}

// KubeletCAOptions configures one invocation.
type KubeletCAOptions struct {
	Phase       string
	Rollback    bool
	Force       bool
	FetchShared bool
	Quiet       bool
}

// KubeletCAFlags is the flag set `certs migrate-kubelet-client-ca` was given.
type KubeletCAFlags struct {
	Cluster     bool
	Phase       string
	Rollback    bool
	Force       bool
	FetchShared bool
}

// Validate refuses contradictory flags.
func (f KubeletCAFlags) Validate() error {
	const cmd = pki.KubeletMigrateCommand
	switch {
	case f.Cluster && f.Phase != "":
		return fmt.Errorf("%s: use either --cluster (every phase on every node) or --phase (one phase on this node), not both", cmd)
	case !f.Cluster && f.Phase == "":
		return fmt.Errorf("%s: pass --cluster, or --phase to run one phase on this node", cmd)
	case f.Rollback && !f.Cluster:
		return fmt.Errorf("%s: --rollback only applies to --cluster; by hand, run --phase=%s, then %s, then %s on every node in turn", cmd,
			pki.KubeletPhaseReAddRoot, pki.KubeletPhaseReissueRoot, pki.KubeletPhaseRemoveCA)
	case f.Phase != "" && !slices.Contains(KubeletCAPhases, f.Phase) && !slices.Contains(KubeletCARollbackPhases, f.Phase):
		return fmt.Errorf("%s: unknown phase %q (want one of %v or %v)", cmd, f.Phase, KubeletCAPhases, KubeletCARollbackPhases)
	case f.Force && !f.Rollback && f.Phase != pki.KubeletPhaseReAddRoot:
		return fmt.Errorf("%s: --force only applies to a rollback, where it re-adds the cluster root to kubelets that completed %s", cmd, pki.KubeletPhaseDropRoot)
	case f.FetchShared && f.Phase != pki.KubeletPhaseTrustBoth:
		return fmt.Errorf("%s: --fetch-shared only applies to --phase=%s", cmd, pki.KubeletPhaseTrustBoth)
	}
	return nil
}

// Options turns validated flags into options.
func (f KubeletCAFlags) Options() KubeletCAOptions {
	return KubeletCAOptions{Phase: f.Phase, Rollback: f.Rollback, Force: f.Force, FetchShared: f.FetchShared}
}

// kubeletNodeOps are the effects one phase has on a node.
type kubeletNodeOps struct {
	controlPlane     func() bool
	ident            func() (nodeIdent, error)
	fetchShared      func() error
	fetchPublished   func() (*x509.Certificate, error)
	restartKubelet   func() error
	restartAPIServer func() error
	republish        func() error
}

// MigrateKubeletClientCA runs one phase on this node. Its progress goes
// through the writer the caller hands it — os.Stdout for the CLI, the MCP
// handler's result buffer for the tool.
func MigrateKubeletClientCA(w io.Writer, o KubeletCAOptions) error {
	release, err := acquireNodeLock("certs migrate-kubelet-client-ca", false)
	if err != nil {
		return err
	}
	defer release()
	if err := RequireNodeProfile("migrate-kubelet-client-ca"); err != nil {
		return err
	}
	return migrateKubeletClientCAWith(w, o, kubeletNodeOps{
		controlPlane: func() bool { return staticpod.Exists("kube-apiserver") },
		ident: func() (nodeIdent, error) {
			h, ip, sans, _, err := hostNodeIP()
			return nodeIdent{h, ip, sans}, err
		},
		fetchShared:    fetchStagedMaterial,
		fetchPublished: fetchPublishedKubeletClientCA,
		restartKubelet: restartKubelet,
		restartAPIServer: func() error {
			if err := staticpod.Restart("kube-apiserver"); err != nil {
				return err
			}
			return waitBounced("kube-apiserver")
		},
		republish: token.RepinCA,
	})
}

func migrateKubeletClientCAWith(w io.Writer, o KubeletCAOptions, ops kubeletNodeOps) error {
	cp := ops.controlPlane()
	t, err := pki.ReadKubeletClientTrust()
	if err != nil {
		return fmt.Errorf("certs: read this node's kubelet client trust: %w", err)
	}
	state := t.State()
	skip := func(why string) error {
		kubeletstep(w, o, fmt.Sprintf("Skip %s: %s", o.Phase, why))
		return nil
	}
	switch o.Phase {
	case pki.KubeletPhaseTrustBoth:
		if state == pki.KubeletStateMigrated || cp && state == pki.KubeletStateIssueNew {
			return skip("already past it on this node")
		}
		if pki.CARotationStaged() {
			return fmt.Errorf("certs: refusing %s: a CA rotation is staged on this node; finish it or roll it back first", o.Phase)
		}
		if cp {
			if o.FetchShared {
				kubeletstep(w, o, "Fetch the cluster's kubelet-client CA from the initiator")
				if err := ops.fetchShared(); err != nil {
					return fmt.Errorf("certs: fetch the shared kubelet-client CA: %w", err)
				}
			}
			kubeletstep(w, o, "Trust "+pki.KubeletClientCAName+" beside the cluster root")
			if err := pki.KubeletClientTrustBoth(!o.FetchShared); err != nil {
				return err
			}
		} else if state == pki.KubeletStateNotMigrated {
			kubeletstep(w, o, "Trust the cluster's published kubelet-client CA beside the cluster root")
			ca, err := ops.fetchPublished()
			if err != nil {
				return err
			}
			if err := pki.AddKubeletClientAnchor(ca); err != nil {
				return err
			}
		}
		return restartAfter(w, o, cp, ops, ops.restartKubelet, "kubelet")
	case pki.KubeletPhaseIssueNew, pki.KubeletPhaseReissueRoot:
		if !cp {
			return skip("workers hold no apiserver-kubelet-client")
		}
		toCA := o.Phase == pki.KubeletPhaseIssueNew
		if toCA && state == pki.KubeletStateMigrated || !toCA && !t.LeafOnCA {
			return skip("apiserver-kubelet-client is already signed by the right CA")
		}
		id, err := ops.ident()
		if err != nil {
			return err
		}
		if toCA {
			kubeletstep(w, o, "Reissue apiserver-kubelet-client under "+pki.KubeletClientCAName)
			err = pki.KubeletClientIssueNew(id.Hostname, id.NodeIP, id.APISANs)
		} else {
			kubeletstep(w, o, "Reissue apiserver-kubelet-client under the cluster root")
			err = pki.KubeletClientReissueRoot(id.Hostname, id.NodeIP, id.APISANs)
		}
		if err != nil {
			return err
		}
		return restartAfter(w, o, false, ops, ops.restartAPIServer, "kube-apiserver")
	case pki.KubeletPhaseDropRoot:
		kubeletstep(w, o, "Trust "+pki.KubeletClientCAName+" alone for kubelet clients")
		if err := pki.KubeletClientDropRoot(); err != nil {
			return err
		}
		return restartAfter(w, o, cp, ops, ops.restartKubelet, "kubelet")
	case pki.KubeletPhaseReAddRoot:
		if state == pki.KubeletStateNotMigrated {
			return skip("this node's kubelet trusts the cluster root already")
		}
		if state == pki.KubeletStateMigrated && !o.Force {
			return fmt.Errorf("certs: refusing %s without --force: this node's kubelet trusts %s alone, and re-adding the cluster root lets every root-signed client certificate reach it again",
				o.Phase, pki.KubeletClientCAName)
		}
		kubeletstep(w, o, "Trust the cluster root again beside "+pki.KubeletClientCAName)
		if err := pki.KubeletClientReAddRoot(); err != nil {
			return err
		}
		return restartAfter(w, o, cp, ops, ops.restartKubelet, "kubelet")
	case pki.KubeletPhaseRemoveCA:
		if state == pki.KubeletStateNotMigrated && !t.HasCA {
			return skip("this node holds no kubelet-client CA")
		}
		kubeletstep(w, o, "Trust the cluster root alone for kubelet clients and remove "+pki.KubeletClientCAName)
		if err := pki.KubeletClientRemoveCA(); err != nil {
			return err
		}
		return restartAfter(w, o, false, ops, ops.restartKubelet, "kubelet")
	}
	return fmt.Errorf("certs: unknown kubelet client-CA migration phase %q", o.Phase)
}

// restartAfter restarts what a phase changed and, on a control plane whose
// kubelet trust moved, republishes the kubelet client-CA bundle.
func restartAfter(w io.Writer, o KubeletCAOptions, republish bool, ops kubeletNodeOps, restart func() error, name string) error {
	kubeletstep(w, o, fmt.Sprintf("Restart %s (phase %s)", name, o.Phase))
	if err := restart(); err != nil {
		return fmt.Errorf("certs: %s wrote its change but %s did not restart onto it: %w", o.Phase, name, err)
	}
	if !republish {
		return nil
	}
	kubeletstep(w, o, "Republish the kubelet client-CA bundle")
	if err := ops.republish(); err != nil {
		return fmt.Errorf("certs: %s: republish the kubelet client-CA bundle: %w", o.Phase, err)
	}
	return nil
}

// fetchPublishedKubeletClientCA returns the one kubelet-client CA the cluster
// publishes beside the root this worker trusts, fetched over TLS verified by
// that root.
func fetchPublishedKubeletClientCA() (*x509.Certificate, error) {
	p := workerTrustPaths()
	server, err := kubeconfig.CurrentServer(p.kubeletKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("worker: read kubelet kubeconfig endpoint: %w", err)
	}
	roots, source, err := readTrustAnchor(p)
	if err != nil {
		return nil, err
	}
	fetched, found, err := fetchKubeletClientCA(server, roots)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("certs: %s publishes no kubelet client-CA bundle; run %s on the control planes first", server, pki.KubeletPhaseTrustBoth)
	}
	onDiskPEM, err := os.ReadFile(p.kubeletClientCA)
	if err != nil {
		return nil, err
	}
	onDisk, err := parseTrustBundle("kubelet client-CA bundle "+p.kubeletClientCA, onDiskPEM)
	if err != nil {
		return nil, err
	}
	want, err := parseTrustBundle("published kubelet client-CA bundle from "+server, fetched)
	if err != nil {
		return nil, err
	}
	added, _, err := checkTrustTransition(onDisk, want, trustWiden)
	if err != nil {
		return nil, fmt.Errorf("kubelet client-CA bundle (checked against %s): %w", source, err)
	}
	if len(added) != 1 {
		return nil, fmt.Errorf("certs: the published kubelet client-CA bundle adds %d CAs to this worker's kubelet trust, want exactly one", len(added))
	}
	return added[0], nil
}

func kubeletstep(w io.Writer, o KubeletCAOptions, msg string) {
	if !o.Quiet {
		k8sutil.Step(w, msg)
	}
}
