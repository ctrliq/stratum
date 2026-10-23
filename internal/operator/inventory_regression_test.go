package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestInventoryWritesIntoConfigMapWithoutData(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
	})
	if err := publishInventory(context.Background(), cs, "cp1", map[string][]objRef{"alpha": {kubeCM("kept")}}); err != nil {
		t.Fatal(err)
	}
	if got := clusterInventory(t, cs).Nodes["cp1"]["alpha"]; len(got) != 1 || got[0].Name != "kept" {
		t.Fatalf("inventory = %+v, want kept", got)
	}
}

func TestMigrationRequiresReadableLocalRecordOrExplicitRecovery(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		name := "corrupt JSON"
		if readFailure {
			name = "file cannot be read"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cs := k8sfake.NewSimpleClientset(cpNode("cp1"))
			seedInventory(t, cs, pruneInventory{})
			dir := t.TempDir()
			local := filepath.Join(dir, appliedRecordName)
			if readFailure {
				if err := os.Mkdir(local, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeManifest(t, local, "{")
			}
			if err := syncInventory(ctx, cs, dir, "cp1"); err == nil {
				t.Fatal("unreadable local inventory counted as a completed contribution")
			}
			if _, ok := clusterInventory(t, cs).Nodes["cp1"]; ok {
				t.Fatal("unreadable local inventory published an empty contribution")
			}
			if _, err := os.Stat(local); err != nil {
				t.Fatalf("unreadable inventory must remain for recovery: %v", err)
			}
			if err := writeInventory(ctx, cs, func(inv pruneInventory) pruneInventory { inv.Recovery = true; return inv }); err != nil {
				t.Fatal(err)
			}
			if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
				t.Fatalf("explicit recovery: %v", err)
			}
			if _, err := os.Stat(local); !os.IsNotExist(err) {
				t.Fatalf("explicit recovery must retire the broken local inventory: %v", err)
			}
		})
	}
}

func TestLeaderDoesNotReplaceUnreadableMigrationEvidence(t *testing.T) {
	for _, inventoryExists := range []bool{false, true} {
		name := "no cluster inventory"
		if inventoryExists {
			name = "migration already open"
		}
		t.Run(name, func(t *testing.T) {
			r := deployFixture(t)
			if inventoryExists {
				seedInventory(t, r.fakeCs, pruneInventory{})
			}
			local := filepath.Join(r.dir, appliedRecordName)
			writeManifest(t, local, "{")
			writeManifest(t, filepath.Join(r.dir, "alpha", "a.yaml"), configMap("kept", "kube-system"))
			r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")
			if b, err := os.ReadFile(local); err != nil || string(b) != "{" {
				t.Fatalf("local evidence = %q, %v; unreadable migration evidence must survive", b, err)
			}
			if inv, err := loadInventory(context.Background(), r.fakeCs); err == nil {
				if _, ok := inv.Nodes["cp1"]; ok {
					t.Fatal("leader bypassed migration validation and marked its corrupt contribution complete")
				}
			}
		})
	}
}

func TestDesiredReadFailureRefusesUnverifiedBundles(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "apiserver unavailable"
		if corrupt {
			name = "invalid inventory"
		}
		t.Run(name, func(t *testing.T) {
			r := deployFixture(t)
			writeManifest(t, filepath.Join(r.dir, "alpha", "stack.yaml"), configMap("stale", "kube-system"))
			if corrupt {
				_, err := r.fakeCs.CoreV1().ConfigMaps(inventoryNamespace).Create(context.Background(), &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
					Data:       map[string]string{inventoryDataKey: "{"},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				r.fakeCs.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				})
			}
			r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")
			if len(r.patches) != 0 || len(r.deletes) != 0 {
				t.Fatalf("patches %v, deletes %v; unreadable desired intent must suspend the pass", r.patchNames(), r.deleteNames())
			}
		})
	}
}

func TestDesiredRetiredStackRefusesStaleFollower(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	alpha := configMap("kept", "kube-system")
	retired := configMap("retired", "kube-system")
	writeManifest(t, filepath.Join(r.dir, "alpha", "stack.yaml"), alpha)
	writeManifest(t, filepath.Join(r.dir, "retired", "stack.yaml"), retired)
	before := map[string][]byte{"alpha/stack.yaml": []byte(alpha), "retired/stack.yaml": []byte(retired)}
	if _, err := RecordDesiredGeneration(ctx, r.fakeCs, DigestBundles(before), "first staging", []string{"alpha", "retired"}); err != nil {
		t.Fatal(err)
	}
	after := map[string][]byte{"alpha/stack.yaml": []byte(alpha)}
	if _, err := RecordDesiredGeneration(ctx, r.fakeCs, DigestBundles(after), "remove retired stack", []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Fatalf("patches %v, deletes %v; a follower retaining a removed staged stack must refuse stale intent", r.patchNames(), r.deleteNames())
	}
}

func TestUnresolvedSentinelProtectsObjectMovedToANewStack(t *testing.T) {
	ctx := context.Background()
	for _, cluster := range []bool{false, true} {
		name := "local record"
		if cluster {
			name = "cluster inventory"
		}
		t.Run(name, func(t *testing.T) {
			r := deployFixture(t)
			shared := kubeCM("shared")
			if cluster {
				seedInventory(t, r.fakeCs, pruneInventory{Recovery: true, Nodes: map[string]map[string][]objRef{
					"cp1": {"old-stack": {shared}},
				}})
			} else {
				seedRecord(t, r.dir, map[string][]objRef{"old-stack": {shared}})
			}
			moved := configMap("shared", "kube-system") + "  dns: \"__STRATUM_DNS_IP__\"\n"
			writeManifest(t, filepath.Join(r.dir, "coredns", "stack.yaml"), moved)
			writeManifest(t, filepath.Join(r.dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			assertApplied(t, r, "kept")
			if len(r.deletes) != 0 {
				t.Fatalf("deletes %v; another stack's old reference must not delete an object owned by a suspended stack", r.deleteNames())
			}
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			if len(r.deletes) != 0 {
				t.Fatalf("deletes %v; the suspended stack's ownership must survive inventory rewrites", r.deleteNames())
			}
		})
	}
}
