package token

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// TestBootstrapExpiration pins the RFC3339 trap: a positive ttl
// yields a value that round-trips through the SAME parser the bootstrap-token
// controller uses (so it is never treated as already-expired), and ttl<=0 opts
// out with an empty string.
func TestBootstrapExpiration(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	for _, ttl := range []time.Duration{0, -time.Hour} {
		got, err := bootstrapExpiration(now, ttl)
		if err != nil {
			t.Fatalf("ttl=%s: %v", ttl, err)
		}
		if got != "" {
			t.Errorf("ttl=%s: got %q, want empty (non-expiring)", ttl, got)
		}
	}

	got, err := bootstrapExpiration(now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("emitted expiration %q is not RFC3339 the controller can parse: %v", got, err)
	}
	if want := now.Add(24 * time.Hour); !parsed.Equal(want) {
		t.Errorf("expiration = %s, want %s", parsed, want)
	}
}

// TestBootstrapOwnerRef: an empty UID yields no ownerRef (never emit one the GC
// cannot resolve — it would delete the dependent immediately); a real UID yields a
// Secret ref by the token's Secret name.
func TestBootstrapOwnerRef(t *testing.T) {
	if got := bootstrapOwnerRef(mintID, ""); got != nil {
		t.Errorf("empty uid produced an ownerRef: %+v", got)
	}
	got := bootstrapOwnerRef(mintID, "uid-123")
	if got == nil {
		t.Fatal("nil ownerRef for a real uid")
	}
	if got.Kind != "Secret" || got.Name != mintName || string(got.UID) != "uid-123" || got.APIVersion != "v1" {
		t.Errorf("ownerRef = %+v, want Secret/%s/uid-123", got, mintName)
	}
}

// TestEnsureBootstrapSecretStampsExpiration: ttl>0 stamps a parseable expiration
// and returns the created UID; ttl==0 omits the key (opt-in non-expiring).
func TestEnsureBootstrapSecretStampsExpiration(t *testing.T) {
	t.Run("ttl stamps a valid expiration", func(t *testing.T) {
		f := &fakeSecrets{uid: "uid-xyz"}
		created, uid, err := ensureBootstrapSecret(context.Background(), f, mintID, mintSecret, 24*time.Hour, false)
		if err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		if string(uid) != "uid-xyz" {
			t.Errorf("uid = %q, want the created Secret's UID", uid)
		}
		exp := f.lastCreated.StringData["expiration"]
		if exp == "" {
			t.Fatal("no expiration stamped for ttl>0")
		}
		if _, err := time.Parse(time.RFC3339, exp); err != nil {
			t.Errorf("expiration %q not RFC3339: %v", exp, err)
		}
	})

	t.Run("ttl 0 omits expiration", func(t *testing.T) {
		f := &fakeSecrets{}
		if _, _, err := ensureBootstrapSecret(context.Background(), f, mintID, mintSecret, 0, false); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.lastCreated.StringData["expiration"]; ok {
			t.Error("expiration stamped for ttl=0 (should be non-expiring)")
		}
	})
}

// TestEnsureBootstrapSecretScopesTheGroupsByRole: every token carries the
// shared node group the CSR bindings name, and only a control-plane token
// carries the control-plane group, the one stratum:certs-reader grants, so a
// worker token cannot read the stratum-certs ciphertext.
func TestEnsureBootstrapSecretScopesTheGroupsByRole(t *testing.T) {
	for _, tc := range []struct {
		name         string
		controlPlane bool
		want         []string
	}{
		{"worker", false, []string{k8sutil.BootstrapGroup}},
		{"control plane", true, []string{k8sutil.BootstrapGroup, k8sutil.ControlPlaneBootstrapGroup}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSecrets{}
			if _, _, err := ensureBootstrapSecret(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, tc.controlPlane); err != nil {
				t.Fatal(err)
			}
			got := strings.Split(f.lastCreated.StringData["auth-extra-groups"], ",")
			if !slices.Equal(got, tc.want) {
				t.Errorf("auth-extra-groups = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCreateTokenMintsTheRequestedShape: createToken hands controlPlane to the
// mint, so Create(..., true) is what puts a token in the control-plane group.
func TestCreateTokenMintsTheRequestedShape(t *testing.T) {
	for _, controlPlane := range []bool{false, true} {
		f := &fakeSecrets{}
		if _, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, controlPlane, okSteps()); err != nil {
			t.Fatal(err)
		}
		groups := f.lastCreated.StringData["auth-extra-groups"]
		if has := slices.Contains(strings.Split(groups, ","), k8sutil.ControlPlaneBootstrapGroup); has != controlPlane {
			t.Errorf("controlPlane=%v: auth-extra-groups = %q, carries the control-plane group = %v", controlPlane, groups, has)
		}
	}
}

// TestCreateTokenReturnsOwnerRef: the --control-plane path needs an ownerRef to the
// minted token's Secret so the stratum-certs Secret is GC'd with the token.
func TestCreateTokenReturnsOwnerRef(t *testing.T) {
	f := &fakeSecrets{uid: "uid-abc"}
	_, _, owner, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		t.Fatal("no ownerRef returned on success")
	}
	if owner.Name != mintName || string(owner.UID) != "uid-abc" || owner.Kind != "Secret" {
		t.Errorf("ownerRef = %+v, want Secret/%s/uid-abc", owner, mintName)
	}
}

// TestTokenInfos pins List's classification: a real RFC3339
// expiration parses; a non-empty but malformed one is Malformed (NOT "never",
// which is what the bootstrap-token controller would disagree with); an absent
// expiration is non-expiring; entries without a token-id are skipped; output is
// id-sorted.
func TestTokenInfos(t *testing.T) {
	sec := func(id, exp string) corev1.Secret {
		d := map[string][]byte{}
		if id != "" {
			d["token-id"] = []byte(id)
		}
		if exp != "" {
			d["expiration"] = []byte(exp)
		}
		return corev1.Secret{Data: d}
	}
	items := []corev1.Secret{
		sec("cccccc", "2099-01-02T03:04:05Z"), // valid, far future
		sec("aaaaaa", ""),                     // non-expiring
		sec("bbbbbb", "not-a-timestamp"),      // malformed
		sec("", "2099-01-02T03:04:05Z"),       // no id: skipped
	}
	got := tokenInfos(items)
	if len(got) != 3 {
		t.Fatalf("got %d infos, want 3 (empty-id entry skipped): %+v", len(got), got)
	}
	// Sorted by id: aaaaaa, bbbbbb, cccccc.
	if got[0].ID != "aaaaaa" || got[0].Expires != nil || got[0].Malformed {
		t.Errorf("aaaaaa should be non-expiring: %+v", got[0])
	}
	if got[1].ID != "bbbbbb" || !got[1].Malformed || got[1].Expires != nil {
		t.Errorf("bbbbbb should be Malformed, not never: %+v", got[1])
	}
	if got[2].ID != "cccccc" || got[2].Expires == nil || got[2].Malformed {
		t.Errorf("cccccc should have a parsed expiration: %+v", got[2])
	}
}
