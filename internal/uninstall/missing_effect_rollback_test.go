package uninstall

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestMissingTopLevelStopRollsBackWhileSupervisorIsUp(t *testing.T) {
	_, fx := runNode(t, "controller")
	stubControlPlane(t, fx)
	supervisorUp := true
	restarted := []string{}
	fx.stopSystemdUnit = func() error { supervisorUp = false; return nil }
	fx.startUnitFn = func(name string) error {
		if !supervisorUp {
			return errors.New("supervisor socket is gone")
		}
		restarted = append(restarted, name)
		return nil
	}
	fx.stopTopLevelFn = nil
	err := prepareTeardown(io.Discard, fx, true)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "stopTopLevelFn") {
		t.Fatalf("prepare = %v; want missing stopTopLevelFn", err)
	}
	if !supervisorUp || !slices.Contains(restarted, "kubelet") || !slices.Contains(restarted, "containerd") {
		t.Fatalf("missing effect shut down rollback's supervisor: up=%v restarted=%v error=%v", supervisorUp, restarted, err)
	}
}

func TestMissingPreparationEffectsRestoreUnitAndManifests(t *testing.T) {
	for _, tc := range []struct {
		field string
		unset func(*hostEffects)
	}{
		{"suppressRestartFn", func(fx *hostEffects) { fx.suppressRestartFn = nil }},
		{"staticPodExists", func(fx *hostEffects) { fx.staticPodExists = nil }},
		{"readManifestFn", func(fx *hostEffects) { fx.readManifestFn = nil }},
		{"stopStaticPodFn", func(fx *hostEffects) { fx.stopStaticPodFn = nil }},
		{"supervisorStatus", func(fx *hostEffects) { fx.supervisorStatus = nil }},
		{"stopHostProcesses", func(fx *hostEffects) { fx.stopHostProcesses = nil }},
		{"waitHostExitFn", func(fx *hostEffects) { fx.waitHostExitFn = nil }},
		{"stopIdleShimsFn", func(fx *hostEffects) { fx.stopIdleShimsFn = nil }},
		{"staticPodRunning", func(fx *hostEffects) { fx.staticPodRunning = nil }},
		{"unmountStateFn", func(fx *hostEffects) { fx.unmountStateFn = nil }},
		{"stopSystemdUnit", func(fx *hostEffects) { fx.stopSystemdUnit = nil }},
		{"stopTopLevelFn", func(fx *hostEffects) { fx.stopTopLevelFn = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			_, fx := runNode(t, "controller")
			_, kubeletStarts := stubControlPlane(t, fx)
			enabled, suppressed := true, false
			manifests := map[string]bool{"etcd": true, "kube-apiserver": true}
			fx.disableSystemdUnit = func() error { enabled = false; return nil }
			fx.enableSystemdUnit = func() error { enabled = true; return nil }
			fx.suppressRestartFn = func() error { suppressed = true; return nil }
			fx.allowRestartFn = func() error { suppressed = false; return nil }
			fx.stopStaticPodFn = func(name string) error { manifests[name] = false; return nil }
			fx.putManifestFn = func(name string, _ []byte) error { manifests[name] = true; return nil }
			tc.unset(fx)
			err := prepareTeardown(io.Discard, fx, true)
			if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("prepare = %v; want missing %s", err, tc.field)
			}
			if !enabled || suppressed || !manifests["etcd"] || !manifests["kube-apiserver"] {
				t.Fatalf("missing effect left node changed: enabled=%v suppressed=%v manifests=%v", enabled, suppressed, manifests)
			}
			if slices.Contains([]string{"stopHostProcesses", "waitHostExitFn", "stopIdleShimsFn", "staticPodRunning", "unmountStateFn", "stopSystemdUnit", "stopTopLevelFn"}, tc.field) && *kubeletStarts == 0 {
				t.Error("missing effect after stopping kubelet did not restart it")
			}
		})
	}
}

func TestMissingRollbackEffectsRefuseBeforeTheirMutation(t *testing.T) {
	for _, tc := range []struct {
		field string
		unset func(*hostEffects)
	}{
		{"allowRestartFn", func(fx *hostEffects) { fx.allowRestartFn = nil }},
		{"enableSystemdUnit", func(fx *hostEffects) { fx.enableSystemdUnit = nil }},
		{"putManifestFn", func(fx *hostEffects) { fx.putManifestFn = nil }},
		{"startUnitFn", func(fx *hostEffects) { fx.startUnitFn = nil }},
		{"stopContainerdFn", func(fx *hostEffects) { fx.stopContainerdFn = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			events, fx := runNode(t, "controller")
			stubControlPlane(t, fx)
			enabled := true
			fx.disableSystemdUnit = func() error { enabled = false; return nil }
			fx.enableSystemdUnit = func() error { enabled = true; return nil }
			fx.unitActive = func(string) bool { return false }
			stops := 0
			fx.stopStaticPodFn = func(string) error { stops++; return nil }
			tc.unset(fx)
			err := prepareTeardown(io.Discard, fx, true)
			if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("prepare = %v; want missing %s", err, tc.field)
			}
			if !enabled || (tc.field == "putManifestFn" && stops != 0) || slices.Contains(*events, "start containerd") || slices.Contains(*events, "stop kubelet") {
				t.Fatalf("missing rollback effect allowed mutation: enabled=%v stops=%d events=%v", enabled, stops, *events)
			}
		})
	}
}

func TestMissingRemovalEffectsRefuseBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		field string
		unset func(*hostEffects)
	}{
		{"removeLegacyHostConfig", func(fx *hostEffects) { fx.removeLegacyHostConfig = nil }},
		{"reloadSystemd", func(fx *hostEffects) { fx.reloadSystemd = nil }},
		{"removeEmptyDir", func(fx *hostEffects) { fx.localPathDir = "local-path-provisioner" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			events, fx := runNode(t, "worker")
			tc.unset(fx)
			err := removeState(io.Discard, fx)
			if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("remove = %v; want missing %s", err, tc.field)
			}
			for _, event := range []string{"teardown datapath", "untrust pod cidrs", "remove state", "mark node-a"} {
				if slices.Contains(*events, event) {
					t.Errorf("missing %s allowed %s before validating cleanup effects", tc.field, event)
				}
			}
		})
	}
}
