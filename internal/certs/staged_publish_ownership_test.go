package certs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestForeignStagedKeyRefusesBeforeBundlePublication(t *testing.T) {
	for _, operation := range []string{carotateOpRotateCA, carotateOpMigrateEtcdCA, carotateOpMigrateKubeletCA} {
		for _, unreadable := range []bool{false, true} {
			name := operation
			if unreadable {
				name += "/unreadable-owner"
			}
			t.Run(name, func(t *testing.T) {
				stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
				foreignOperation := carotateOpRotateCA
				if operation == carotateOpRotateCA {
					foreignOperation = carotateOpMigrateEtcdCA
				}
				foreignKey := stagedKeySecret("foreign-key", stagedKeyOwner{operation: foreignOperation, run: "foreign-run"})
				bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "stratum-certs", Namespace: caNamespace}, Data: map[string][]byte{"certs": []byte("foreign-bundle")}}
				cs := fake.NewSimpleClientset(foreignKey, bundle)
				boom := errors.New("cannot read staged-key owner")
				if unreadable {
					cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
						if action.(k8stesting.GetAction).GetName() == caRotateKeySecret {
							return true, nil, boom
						}
						return false, nil, nil
					})
				}
				oldUpload := uploadStagedMaterial
				t.Cleanup(func() { uploadStagedMaterial = oldUpload })
				uploads := 0
				uploadStagedMaterial = func(string) error {
					uploads++
					replacement := bundle.DeepCopy()
					replacement.Data["certs"] = []byte("our-new-bundle")
					_, err := cs.CoreV1().Secrets(caNamespace).Update(context.Background(), replacement, metav1.UpdateOptions{})
					return err
				}
				var err error
				if operation == carotateOpRotateCA {
					d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs, rotationRun: "our-run"}
					_, err = d.PublishStagedKey()
				} else if operation == carotateOpMigrateKubeletCA {
					_, err = publishKubeletCAKey(context.Background(), io.Discard, cs)
				} else {
					d := &clusterDriver{w: io.Discard, ctx: context.Background(), cs: cs}
					_, err = d.PublishEtcdCA()
				}
				if err == nil {
					t.Fatal("publication accepted a foreign or unreadable staged-key owner")
				}
				if unreadable {
					if !errors.Is(err, boom) {
						t.Errorf("error = %v, want unreadable owner cause", err)
					}
				} else if !strings.Contains(err.Error(), foreignOperation) {
					t.Errorf("error = %v, want foreign operation %s", err, foreignOperation)
				}
				if uploads != 0 {
					t.Errorf("uploaded %d replacement bundles before refusing ownership", uploads)
				}
				got, getErr := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), "stratum-certs", metav1.GetOptions{})
				if getErr != nil {
					t.Fatal(getErr)
				}
				if !maps.EqualFunc(got.Data, bundle.Data, bytes.Equal) {
					t.Error("foreign bundle changed before ownership refusal: its followers cannot decrypt it")
				}
				obj, getErr := cs.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, caNamespace, caRotateKeySecret)
				if apierrors.IsNotFound(getErr) {
					t.Fatal("foreign staged key was deleted")
				}
				if getErr != nil {
					t.Fatal(getErr)
				}
				gotKey := obj.(*corev1.Secret)
				if !maps.EqualFunc(gotKey.Data, foreignKey.Data, bytes.Equal) || !maps.Equal(gotKey.Annotations, foreignKey.Annotations) {
					t.Error("foreign staged-key material or ownership changed")
				}
			})
		}
	}
}

func TestPublicationCanReplaceItsOwnRetainedFetchKey(t *testing.T) {
	for _, operation := range []string{carotateOpRotateCA, carotateOpMigrateEtcdCA, carotateOpMigrateKubeletCA} {
		t.Run(operation, func(t *testing.T) {
			stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
			oldKey := stagedKeySecret("previous-key", stagedKeyOwner{operation: operation, run: "previous-run"})
			cs := fake.NewSimpleClientset(oldKey)
			oldUpload := uploadStagedMaterial
			t.Cleanup(func() { uploadStagedMaterial = oldUpload })
			uploads := 0
			uploadStagedMaterial = func(string) error { uploads++; return nil }
			if operation == carotateOpRotateCA {
				d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs, rotationRun: "our-run"}
				if _, err := d.PublishStagedKey(); err != nil {
					t.Fatal(err)
				}
			} else if operation == carotateOpMigrateKubeletCA {
				cleanup, err := publishKubeletCAKey(context.Background(), io.Discard, cs)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
			} else {
				d := &clusterDriver{w: io.Discard, ctx: context.Background(), cs: cs}
				cleanup, err := d.PublishEtcdCA()
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
			}
			if uploads != 1 {
				t.Errorf("bundle uploads = %d, want one for the resumed operation", uploads)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data["certificate-key"]) == "previous-key" {
				t.Error("resumed operation did not replace its retained fetch key")
			}
			if got.Annotations[carotateOperationAnnotation] != operation {
				t.Errorf("new key belongs to %q, want %q", got.Annotations[carotateOperationAnnotation], operation)
			}
		})
	}
}

func TestStagedKeyReplacementConflictsAfterOwnershipSnapshotChanges(t *testing.T) {
	for _, operation := range []string{carotateOpRotateCA, carotateOpMigrateEtcdCA, carotateOpMigrateKubeletCA} {
		t.Run(operation, func(t *testing.T) {
			original := stagedKeySecret("original-key", stagedKeyOwner{operation: operation, run: "our-old-run"})
			original.ResourceVersion = "10"
			replacement := stagedKeySecret("foreign-key", stagedKeyOwner{operation: "foreign-operation", run: "foreign-run"})
			replacement.ResourceVersion = "11"
			cs := fake.NewSimpleClientset(original)
			resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			cs.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				// A successor replaced the Secret after our ownership GET. Model the
				// apiserver's Secret update semantics: missing resourceVersion permits
				// an unconditional overwrite; a stale nonempty version conflicts.
				if err := cs.Tracker().Update(resource, replacement, caNamespace); err != nil {
					t.Fatal(err)
				}
				incoming := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
				if incoming.ResourceVersion != original.ResourceVersion {
					t.Errorf("update resourceVersion = %q, want ownership snapshot %q", incoming.ResourceVersion, original.ResourceVersion)
				}
				if incoming.ResourceVersion != "" && incoming.ResourceVersion != replacement.ResourceVersion {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, caRotateKeySecret, errors.New("ownership snapshot changed"))
				}
				return false, nil, nil
			})
			err := putStagedKey(context.Background(), cs, "our-new-key", stagedKeyOwner{operation: operation, run: "our-new-run"})
			if !apierrors.IsConflict(err) {
				t.Errorf("put error = %v, want conflict after ownership changed", err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !maps.EqualFunc(got.Data, replacement.Data, bytes.Equal) || !maps.Equal(got.Annotations, replacement.Annotations) {
				t.Error("put overwrote the successor's staged key after inspecting its predecessor")
			}
		})
	}
}

func TestMigrationPreservesLegacyRotationKeyBeforePublishing(t *testing.T) {
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	legacy := stagedKeySecret("legacy-rotation-key", rotationStagedKeyOwner("legacy-rotation-run"))
	delete(legacy.Annotations, carotateOperationAnnotation)
	bundle := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "stratum-certs", Namespace: caNamespace}, Data: map[string][]byte{"certs": []byte("legacy-rotation-ciphertext")}}
	cs := fake.NewSimpleClientset(legacy, bundle)
	oldUpload := uploadStagedMaterial
	t.Cleanup(func() { uploadStagedMaterial = oldUpload })
	uploads := 0
	uploadStagedMaterial = func(string) error {
		uploads++
		next := bundle.DeepCopy()
		next.Data["certs"] = []byte("migration-ciphertext")
		_, err := cs.CoreV1().Secrets(caNamespace).Update(context.Background(), next, metav1.UpdateOptions{})
		return err
	}
	d := &clusterDriver{w: io.Discard, ctx: context.Background(), cs: cs}
	cleanup, err := d.PublishEtcdCA()
	if err == nil {
		t.Error("migration accepted the legacy rotation's retained key")
	}
	// Do not run returned cleanup until after inspecting the erroneous success:
	// the existing legacy key must survive both publication and cleanup.
	if cleanup != nil {
		defer cleanup()
	}
	if uploads != 0 {
		t.Errorf("migration uploaded %d times before checking legacy rotation ownership", uploads)
	}
	got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(got.Data, legacy.Data, bytes.Equal) || !maps.Equal(got.Annotations, legacy.Annotations) {
		t.Error("migration replaced the legacy rotation key")
	}
	gotBundle, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), "stratum-certs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(gotBundle.Data, bundle.Data, bytes.Equal) {
		t.Error("migration replaced the legacy rotation ciphertext")
	}
}

// TestStagedKeyMetadataNeverCarriesTheKey: the staged key Secret holds the
// certificate-key that decrypts every CA private key in stratum-certs, so the
// key may live only in its data. `kubectl describe secret` hides data values
// but prints annotations and labels in full, and metadata-only readers
// (PartialObjectMetadata, inventory exporters) see them too. Each staging
// path is driven the way production calls it.
func TestStagedKeyMetadataNeverCarriesTheKey(t *testing.T) {
	oldUpload := uploadStagedMaterial
	t.Cleanup(func() { uploadStagedMaterial = oldUpload })
	uploadStagedMaterial = func(string) error { return nil }
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	for name, stage := range map[string]func(cs kubernetes.Interface) error{
		"rotate-ca --cluster": func(cs kubernetes.Interface) error {
			return stageCarotateKey(io.Discard, context.Background(), cs, strings.Repeat("cafebabe", 8), run1)
		},
		"migrate-etcd-ca": func(cs kubernetes.Interface) error {
			d := &clusterDriver{w: io.Discard, ctx: context.Background(), cs: cs}
			_, err := d.PublishEtcdCA()
			return err
		},
		"migrate-kubelet-client-ca": func(cs kubernetes.Interface) error {
			_, err := publishKubeletCAKey(context.Background(), io.Discard, cs)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if err := stage(cs); err != nil {
				t.Fatal(err)
			}
			sec, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			key := string(sec.Data["certificate-key"])
			if key == "" {
				t.Fatal("the staged Secret holds no certificate-key")
			}
			if sec.Annotations[carotateRunAnnotation] == "" {
				t.Error("the staged key carries no run annotation: its owner cannot be told from a leftover")
			}
			for _, meta := range []map[string]string{sec.Annotations, sec.Labels} {
				for k, v := range meta {
					if strings.Contains(v, key) {
						t.Errorf("metadata %s carries the staged certificate-key, which `kubectl describe` prints in full", k)
					}
				}
			}
		})
	}
}

func TestForeignStagedKeyDiagnosticsDoNotDiscloseMigrationKey(t *testing.T) {
	certKey := strings.Repeat("cafebabe", 8)
	secret := stagedKeySecret(certKey, migrationStagedKeyOwner(certKey))
	cs := fake.NewSimpleClientset(secret)
	for name, check := range map[string]func() error{
		"publication guard": func() error { return checkStagedKeyOwner(context.Background(), cs, carotateOpRotateCA) },
		"staged-key put": func() error {
			return putStagedKey(context.Background(), cs, "our-key", rotationStagedKeyOwner("our-run"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := check()
			if err == nil {
				t.Fatal("expected foreign ownership refusal")
			}
			if strings.Contains(err.Error(), certKey) {
				t.Error("ownership refusal discloses the migration's certificate-key through its run annotation")
			}
		})
	}
}

func TestUnannotatedLegacyStagedKeyRemainsReplaceable(t *testing.T) {
	for _, operation := range []string{carotateOpRotateCA, carotateOpMigrateEtcdCA, carotateOpMigrateKubeletCA} {
		t.Run(operation, func(t *testing.T) {
			legacy := stagedKeySecret("legacy-key", stagedKeyOwner{})
			legacy.Annotations = nil
			cs := fake.NewSimpleClientset(legacy)
			if err := checkStagedKeyOwner(context.Background(), cs, operation); err != nil {
				t.Fatal(err)
			}
			if err := putStagedKey(context.Background(), cs, "new-key", stagedKeyOwner{operation: operation, run: "new-run"}); err != nil {
				t.Fatal(err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data["certificate-key"]) != "new-key" || got.Annotations[carotateOperationAnnotation] != operation {
				t.Error("unannotated legacy key was not replaced by the current operation")
			}
		})
	}
}

func TestEtcdMigrationReaperPreservesEveryForeignOperation(t *testing.T) {
	for _, operation := range []string{carotateOpRotateCA, carotateOpMigrateKubeletCA, "future-migration"} {
		t.Run(operation, func(t *testing.T) {
			sec := stagedKeySecret("foreign-key", stagedKeyOwner{operation: operation, run: "foreign-run"})
			cs := fake.NewSimpleClientset(sec)
			deletes := preconditionedDeletes(t, cs)
			var out bytes.Buffer
			d := &clusterDriver{ctx: context.Background(), cs: cs, w: &out}
			d.ReapStaleFetchKey()
			if !hasSecret(t, cs, caRotateKeySecret) || len(*deletes) != 0 {
				t.Fatal("etcd migration reaped a foreign operation's fetch key")
			}
			if !strings.Contains(out.String(), operation) || !strings.Contains(out.String(), "Leaving it") {
				t.Fatalf("missing ownership guidance: %q", out.String())
			}
		})
	}
}

func TestRootRotationRefusesForeignKeyBeforeCreatingCheckpoint(t *testing.T) {
	for _, operation := range []string{carotateOpMigrateEtcdCA, carotateOpMigrateKubeletCA} {
		t.Run(operation, func(t *testing.T) {
			sec := stagedKeySecret("foreign-key", stagedKeyOwner{operation: operation, run: "foreign-run"})
			cs := fake.NewSimpleClientset(sec)
			d := &prodCADriver{ctx: context.Background(), cs: cs, w: io.Discard}
			_, _, err := d.ResolveRotation()
			if err == nil || !strings.Contains(err.Error(), operation) {
				t.Errorf("root rotation accepted foreign owner: %v", err)
			}
			_, err = cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("rejected root rotation created a checkpoint that blocks foreign recovery: %v", err)
			}
		})
	}
}
