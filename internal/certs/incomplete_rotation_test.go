package certs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// A rotation that fails after reissuing the leaves leaves fresh leaves on disk
// beside kubeconfigs and processes still on the old ones. The next timer run
// must not read the fresh leaves as nothing to do: it rotates again until a
// run completes.
func TestRotationThatFailsAfterReissueIsRetriedByTheTimer(t *testing.T) {
	timer := Options{Quiet: true, IfExpiringDays: 45}

	failKubeconfigRewrite := func(t *testing.T) (undo func()) {
		if err := os.RemoveAll(version.KubeconfigDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version.KubeconfigDir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(version.KubeconfigDir); err != nil {
				t.Fatal(err)
			}
		}
	}
	failEtcdRestart := func(t *testing.T) (undo func()) {
		if err := os.MkdirAll(staticpod.Path("etcd"), 0o755); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(staticpod.Path("etcd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	unauthorized := func(t *testing.T) {
		cs := fakeAdminClientset(t)
		cs.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewUnauthorized("x509: certificate has expired")
		})
	}

	for _, tc := range []struct {
		name    string
		fail    func(*testing.T) func()
		expired bool
	}{
		{name: "kubeconfig rewrite fails", fail: failKubeconfigRewrite},
		{name: "control-plane restart fails", fail: failEtcdRestart},
		{name: "rotation from expired leaves fails, API still rejects admin.conf", fail: failKubeconfigRewrite, expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verify := kubeVIPNode(t)
			if tc.expired {
				unauthorized(t)
				expireLeaf(t)
			}
			undo := tc.fail(t)
			before := readLeaf(t, "kube-vip.crt")
			if _, err := RotateContext(context.Background(), &bytes.Buffer{}, Options{Quiet: true}); err == nil {
				t.Fatal("a rotation that failed after reissuing reported success")
			}
			reissued := readLeaf(t, "kube-vip.crt")
			if bytes.Equal(before, reissued) {
				t.Fatal("the failing rotation did not get as far as reissuing the leaves")
			}

			undo()
			var out bytes.Buffer
			rotated, err := RotateContext(context.Background(), &out, timer)
			if err != nil {
				t.Fatalf("the retry failed: %v", err)
			}
			if !rotated {
				t.Fatal("the timer read the fresh leaves of an incomplete rotation as nothing to rotate")
			}
			if !strings.Contains(out.String(), "did not restart every component") {
				t.Errorf("the retry did not say why it rotated: %q", out.String())
			}
			if bytes.Equal(reissued, readLeaf(t, "kube-vip.crt")) {
				t.Fatal("the retry did not reissue the leaves")
			}
			verify("after the retry", 1)

			rotated, err = RotateContext(context.Background(), &bytes.Buffer{}, timer)
			if err != nil || rotated {
				t.Fatalf("after a completed rotation the timer must find nothing to do: rotated=%v err=%v", rotated, err)
			}
			if _, err := os.Stat(incompleteRotationPath()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the incomplete-rotation marker outlived a completed rotation: %v", err)
			}
		})
	}
}
