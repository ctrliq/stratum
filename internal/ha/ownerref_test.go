package ha

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// a new token adds an owner; GC reaps the Secret only when every owner is gone
func TestMergeOwnerRef(t *testing.T) {
	ref := func(uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: "bootstrap-token-" + uid, UID: types.UID(uid)}
	}
	a, b := ref("a"), ref("b")

	got := mergeOwnerRef([]metav1.OwnerReference{a}, b)
	if len(got) != 2 {
		t.Fatalf("merge added=%d, want 2 (earlier owner kept): %+v", len(got), got)
	}

	got = mergeOwnerRef([]metav1.OwnerReference{a, b}, a)
	if len(got) != 2 {
		t.Errorf("re-adding an existing UID must be idempotent, got %d: %+v", len(got), got)
	}

	if got := mergeOwnerRef(nil, a); len(got) != 1 || got[0].UID != "a" {
		t.Errorf("merge into empty = %+v, want [a]", got)
	}
}
