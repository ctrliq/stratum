// Package rollout restarts the in-cluster workloads Stratum owns, as one list.
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

// Workload is a set of workloads holding pre-operation watches, restarted together.
type Workload struct {
	Name      string
	Namespace string
	Probe     string // an object whose presence says the cluster runs it
	Targets   []string
}

// Workloads' watches would point past etcd's restored resourceVersions.
var Workloads = []Workload{
	{"cilium agent + operator", "kube-system", "ds/cilium", []string{"ds/cilium", "deploy/cilium-operator"}},
	{"coredns", "kube-system", "deploy/coredns", []string{"deploy/coredns"}},
	{"local-path provisioner", "local-path-storage", "deploy/local-path-provisioner", []string{"deploy/local-path-provisioner"}},
	{"dashboard", "stratum-dashboard", "deploy/stratum-dashboard", []string{"deploy/stratum-dashboard"}},
}

// Owned reports whether a workload is one of the Targets Stratum rolls itself.
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

// RestartCommands is the guidance's copy of what the restore rolls, one list.
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

// Result is what Roller.Restart reports for one Workload; Warn is not a failure.
type Result struct {
	Name      string
	Present   bool
	Warn, Err error
}

// Roller restarts Workloads through the apiserver its Kubectl talks to.
type Roller struct {
	// A Kubectl call that gets no answer in time must be killed, never waited on.
	Kubectl func(ctx context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error)
	// Hint is the command line an operator runs by hand for the same args.
	Hint func(args string) string
	// CallTimeout bounds one discovery or restart call.
	CallTimeout time.Duration
	// Wait is how long Restart waits for all the rollouts it started, in total.
	Wait time.Duration
}

// Restart rolls every Workload the cluster runs.
func (r Roller) Restart() []Result { return r.RestartOf(Workloads) }

// RestartOf is Restart for workloads that are not Stratum's own.
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
	// One Wait budget covers all rollouts; `--timeout` does not bound a first request.
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
		warns[ro.group] = append(warns[ro.group], ro.err)
	}
	for i := range results {
		results[i].Warn = errors.Join(warns[i]...)
		results[i].Err = errors.Join(append([]error{results[i].Err}, restartErrs[i]...)...)
	}
	return results
}
