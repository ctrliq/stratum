// Package rollout restarts the in-cluster workloads Stratum owns, so that every
// command that has to do it (snapshot restore, and rotate-ca) rolls one list.
package rollout

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Workload is a set of in-cluster workloads that hold watches or credentials
// from before an operation, and are restarted together as one entry in its
// report.
type Workload struct {
	Name      string   // as reported to the operator
	Namespace string   // where its targets live
	Probe     string   // an object whose presence says the cluster runs it
	Targets   []string // what is rolled
}

// Workloads are restarted after the control plane's own watchers. Cilium's
// service table and endpoint state, and CoreDNS's Service and EndpointSlice
// view (headless-Service names answer from it), are built from watches that now
// point past etcd's restored resourceVersions.
// The local-path provisioner watches PersistentVolumeClaims, so a claim restored
// from the snapshot stays unprovisioned until it has restarted. It is there only
// when the cluster's default storage is local-path, and each workload is found by
// its own namespace and skipped when absent.
//
// What is not here, and why: cilium-envoy has no ClusterRole, takes its config
// from a mounted ConfigMap and the agent, and opens no API watch; containerd
// and the cluster controller (`stratum up`) hold no long watch; and user
// workloads, and third-party controllers and operators, are never restarted by
// the restore, since that would widen a destructive command to what it does not
// own (snapshot-restore.md gives the command to restart them by hand).
var Workloads = []Workload{
	{"cilium agent + operator", "kube-system", "ds/cilium", []string{"ds/cilium", "deploy/cilium-operator"}},
	{"coredns", "kube-system", "deploy/coredns", []string{"deploy/coredns"}},
	{"local-path provisioner", "local-path-storage", "deploy/local-path-provisioner", []string{"deploy/local-path-provisioner"}},
	{"dashboard", "stratum-dashboard", "deploy/stratum-dashboard", []string{"deploy/stratum-dashboard"}},
}

// Owned reports whether kind (a Kubernetes kind: Deployment or DaemonSet) named
// name in namespace is one of the Targets Stratum rolls itself, so a caller
// that looks for workloads Stratum does not own can leave these out.
func Owned(namespace, kind, name string) bool {
	short := map[string]string{"Deployment": "deploy", "DaemonSet": "ds"}[kind]
	if short == "" {
		return false
	}
	for _, w := range Workloads {
		if w.Namespace == namespace && slices.Contains(w.Targets, short+"/"+name) {
			return true
		}
	}
	return false
}

// RestartCommands is one `rollout restart` command per namespace for the
// workloads the restore rolls, each prefixed with kubectl, for the guidance that
// tells an operator to restart them by hand or to repeat the restore's restarts.
// One list, so the guidance cannot drift from what the restore does.
func RestartCommands(kubectl string) []string {
	var cmds []string
	var namespaces []string
	targets := map[string][]string{}
	for _, w := range Workloads {
		if _, ok := targets[w.Namespace]; !ok {
			namespaces = append(namespaces, w.Namespace)
		}
		targets[w.Namespace] = append(targets[w.Namespace], w.Targets...)
	}
	for _, ns := range namespaces {
		cmds = append(cmds, kubectl+" -n "+ns+" rollout restart "+strings.Join(targets[ns], " "))
	}
	return cmds
}

// Result is what Roller.Restart reports for one Workload.
// Err joins the restarts that failed: those components still hold their old
// watches. Warn joins the rollouts that did not finish in time. They were
// restarted and may yet finish, so that is a warning, not a failure: the
// operator's recovery steps must not wait on it.
type Result struct {
	Name      string
	Present   bool // false when the cluster does not run it
	Warn, Err error
}

// Roller restarts Workloads through the apiserver its Kubectl talks to.
type Roller struct {
	// Kubectl runs one kubectl call under its own deadline and returns its
	// output (combined with stderr when combined is set). A call that gets no
	// answer in time must be killed and reported, never waited on.
	Kubectl func(ctx context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error)
	// Hint is the command line an operator runs by hand against the same
	// apiserver, for args.
	Hint func(args string) string
	// CallTimeout bounds one discovery or restart call.
	CallTimeout time.Duration
	// Wait is how long Restart waits for all the rollouts it started, in total.
	Wait time.Duration
}

// Restart rolls every Workload the cluster runs. Each target is always
// restarted, and every rollout that started is checked.
func (r Roller) Restart() []Result { return r.RestartOf(Workloads) }

// RestartOf is Restart for ws, so a caller can roll workloads that are not
// Stratum's own (rotate-ca's --restart-workloads) with the same waiting and
// reporting.
func (r Roller) RestartOf(ws []Workload) []Result {
	results := make([]Result, len(ws))
	restartErrs := make([][]error, len(ws))
	type rollout struct {
		group     int
		namespace string
		target    string
		err       error
	}
	var started []*rollout
	for i, w := range ws {
		results[i].Name = w.Name
		out, err := r.Kubectl(context.Background(), r.CallTimeout, false, "-n", w.Namespace, "get", w.Probe, "-o", "name", "--ignore-not-found")
		if err != nil {
			results[i].Present = true
			results[i].Err = fmt.Errorf("look for %s: %w", w.Probe, err)
			continue
		}
		if strings.TrimSpace(string(out)) == "" {
			continue
		}
		results[i].Present = true
		for _, t := range w.Targets {
			if out, err := r.Kubectl(context.Background(), r.CallTimeout, true, "-n", w.Namespace, "rollout", "restart", t); err != nil {
				restartErrs[i] = append(restartErrs[i], fmt.Errorf("restart %s: %w: %s", t, err, strings.TrimSpace(string(out))))
				continue
			}
			started = append(started, &rollout{group: i, namespace: w.Namespace, target: t})
		}
	}
	// The rollouts, of every workload, are waited on together, so Wait is
	// one budget for all of them, not one each. Any may not finish while a node's
	// kubelet still holds a pre-restore watch, which the operator only fixes after
	// the restore returns.
	//
	// The budget is also a deadline on the calls themselves, one shared by all of
	// them, because `--timeout` does not bound a first request that never gets
	// an answer. A call killed at the deadline is a warning like a timed-out one.
	ctx, cancel := context.WithTimeout(context.Background(), r.Wait)
	defer cancel()
	var wg sync.WaitGroup
	for _, ro := range started {
		wg.Go(func() {
			wait := fmt.Sprintf("--timeout=%ds", max(1, int(r.Wait/time.Second)))
			if out, err := r.Kubectl(ctx, r.Wait, true, "-n", ro.namespace, "rollout", "status", ro.target, wait); err != nil {
				ro.err = fmt.Errorf("%s did not finish rolling out in %s (on the seed: `%s`): %w: %s",
					ro.target, r.Wait, r.Hint("-n "+ro.namespace+" rollout status "+ro.target), err, strings.TrimSpace(string(out)))
			}
		})
	}
	wg.Wait()
	warns := make([][]error, len(ws))
	for _, ro := range started {
		warns[ro.group] = append(warns[ro.group], ro.err) // errors.Join drops the nils
	}
	for i := range results {
		results[i].Warn = errors.Join(warns[i]...)
		results[i].Err = errors.Join(append([]error{results[i].Err}, restartErrs[i]...)...)
	}
	return results
}
