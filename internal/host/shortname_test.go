package host

import "testing"

// The short name keeps a hostname no Node can take, lowercased, so what is
// named by it (snapshots, their prune) carries a name rather than "".
func TestShortNameOfKeepsANameNoNodeCanTake(t *testing.T) {
	for in, want := range map[string]string{
		"K8S_Ctrl.example.com": "k8s_ctrl",
		"HK-k8s-ctrl-01":       "hk-k8s-ctrl-01",
	} {
		if got := shortNameOf(in); got != want {
			t.Errorf("shortNameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
