package certs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

// prepPinned is prep plus the version.PKIDir restore prep never schedules: every
// reader pinned here resolves through version.PKIDir, so without the restore the
// next test in the package would silently read this one's temp dir.
func prepPinned(t *testing.T) {
	t.Helper()
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	prep(t)
}

// hashFor produces the reference pin through token.CACertHashFor, the same
// producer the joining node's verifier consumes, so the rows compare WHICH root
// is pinned, not a hash spelling.
func hashFor(t *testing.T, name string) string {
	t.Helper()
	h, err := token.CACertHashFor(name)
	if err != nil {
		t.Fatalf("token.CACertHashFor(%q): %v", name, err)
	}
	return h
}

// TestRotationReadersAgreeAtEachPhaseBoundary pins the answer four independent
// rotation-state readers give at every boundary of a CA rotation: RotationPhase
// (certs check), signerPromoted (the rollback and reaper logic), the
// checkCARotationState guard, and the root token.CACertHash pins (join tokens).
// Different rules keep these readers, so a refactor can move one without the
// others — a promoted signer reported at trust-new, a join token pinned to the
// old root during a rotation, or a rollback that leaves the staged CA behind
// each leave nodes and joiners disagreeing about the same cluster. Every row
// states the one answer all four must give together.
func TestRotationReadersAgreeAtEachPhaseBoundary(t *testing.T) {
	// staged reaches trust-new on a fresh PKI and records both roots' pins while
	// each still has its own file: the old root in ca.crt, the new in ca-new.crt.
	staged := func(t *testing.T) map[string]string {
		prepPinned(t)
		refs := map[string]string{"old": hashFor(t, "ca")}
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		refs["new"] = hashFor(t, "ca-new")
		return refs
	}
	promoted := func(t *testing.T) {
		t.Helper()
		if err := pki.PromoteCA(); err != nil {
			t.Fatal(err)
		}
		if err := pki.PromoteSA(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name         string
		reach        func(t *testing.T) map[string]string
		wantPhase    string // prefix RotationPhase() must start with
		wantPromoted bool   // signerPromoted()
		wantRefused  bool   // checkCARotationState(CARotateOptions{}, "cp1") must refuse
		wantRoot     string // key into refs: the root token.CACertHash() must pin
	}{
		{
			name: "idle",
			reach: func(t *testing.T) map[string]string {
				prepPinned(t)
				return map[string]string{"old": hashFor(t, "ca")}
			},
			wantPhase: "idle", wantRoot: "old",
		},
		{
			name:        "trust-new staged: signer still old, tokens pin the new root",
			reach:       staged,
			wantPhase:   "trust-new",
			wantRefused: true, wantRoot: "new",
		},
		{
			name: "sign-new: signer promoted, old CA still trusted",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				return refs
			},
			wantPhase: "sign-new", wantPromoted: true, wantRefused: true, wantRoot: "new",
		},
		{
			name: "finalized: only the new CA remains",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantPhase: "idle", wantRoot: "new",
		},
		{
			// A rollback is two steps: pki.RollbackCA only
			// restores, and RemoveRotationScratch deletes the staged material
			// and the .pre-carotate backups once the leaves are repaired. The
			// callers that matter finish the second step, so every reader must
			// be back at idle only after both.
			name: "rolled back from trust-new",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RemoveRotationScratch(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantPhase: "idle", wantRoot: "old",
		},
		{
			name: "rolled back from sign-new",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RemoveRotationScratch(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantPhase: "idle", wantRoot: "old",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := tc.reach(t)
			if got := RotationPhase(); !strings.HasPrefix(got, tc.wantPhase) {
				t.Errorf("RotationPhase() = %q, want %q", got, tc.wantPhase)
			}
			if got := signerPromoted(); got != tc.wantPromoted {
				t.Errorf("signerPromoted() = %v, want %v", got, tc.wantPromoted)
			}
			err := checkCARotationState(CARotateOptions{}, "cp1")
			if refused := err != nil; refused != tc.wantRefused {
				t.Errorf("checkCARotationState refused = %v (err = %v), want %v", refused, err, tc.wantRefused)
			}
			pin, err := token.CACertHash()
			if err != nil {
				t.Fatalf("token.CACertHash: %v", err)
			}
			if want := refs[tc.wantRoot]; pin != want {
				t.Errorf("token.CACertHash() = %s, want the %s root's pin %s", pin, tc.wantRoot, want)
			}
		})
	}
}

// TestRotationReadersBetweenRollbackAndScratchRemoval pins the answers in the
// intermediate state a retried rollback observes: pki.RollbackCA only restores,
// and the .pre-carotate backups and any staged material stay until
// pki.RemoveRotationScratch runs once the leaves are repaired — the finished
// state after both steps is what TestRotationReadersAgreeAtEachPhaseBoundary
// pins. Until that second step the readers still see the rotation: from
// trust-new the staged CA keeps the phase at trust-new, the guard refusing and
// the join pin on the new root, and from sign-new the leftover backup keeps
// signerPromoted true on a node whose phase already reads idle — the one
// combination that makes the promoted signer its own anchor rather than a
// property of the phase.
func TestRotationReadersBetweenRollbackAndScratchRemoval(t *testing.T) {
	// staged is TestRotationReadersAgreeAtEachPhaseBoundary's staged: both roots'
	// pins are recorded while each still has its own file, so the rows compare
	// WHICH root is pinned after the promotion consumed ca-new.crt.
	staged := func(t *testing.T) map[string]string {
		prepPinned(t)
		refs := map[string]string{"old": hashFor(t, "ca")}
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		refs["new"] = hashFor(t, "ca-new")
		return refs
	}
	for _, tc := range []struct {
		name         string
		reach        func(t *testing.T) // the rollback, without RemoveRotationScratch
		wantPhase    string             // prefix RotationPhase() must start with
		wantPromoted bool               // signerPromoted()
		wantRefused  bool               // checkCARotationState(CARotateOptions{}, "cp1") must refuse
		wantRoot     string             // key into refs: the root token.CACertHash() must pin
	}{
		{
			name: "from trust-new: the staged CA and the new-root pin remain until the scratch goes",
			reach: func(t *testing.T) {
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
			},
			wantPhase: "trust-new", wantRefused: true, wantRoot: "new",
		},
		{
			name: "from sign-new: the backup outlives the promotion, so the phase reads idle with the signer promoted",
			reach: func(t *testing.T) {
				if err := pki.PromoteCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.PromoteSA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
			},
			wantPhase: "idle", wantPromoted: true, wantRoot: "old",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := staged(t)
			tc.reach(t)
			if got := RotationPhase(); !strings.HasPrefix(got, tc.wantPhase) {
				t.Errorf("RotationPhase() = %q, want %q", got, tc.wantPhase)
			}
			if got := signerPromoted(); got != tc.wantPromoted {
				t.Errorf("signerPromoted() = %v, want %v", got, tc.wantPromoted)
			}
			err := checkCARotationState(CARotateOptions{}, "cp1")
			if refused := err != nil; refused != tc.wantRefused {
				t.Errorf("checkCARotationState refused = %v (err = %v), want %v", refused, err, tc.wantRefused)
			}
			pin, err := token.CACertHash()
			if err != nil {
				t.Fatalf("token.CACertHash: %v", err)
			}
			if want := refs[tc.wantRoot]; pin != want {
				t.Errorf("token.CACertHash() = %s, want the %s root's pin %s", pin, tc.wantRoot, want)
			}
		})
	}
}

// TestReadCARotationAtEachPhaseBoundary pins pki.ReadCARotation, the one
// rotation-state read that RotationPhase, signerPromoted, checkCARotationState
// and token.CACertHash all consume, at the same phase boundaries the
// reader-agreement pin above walks. Each row states what the struct must
// report — whether a new root is staged, whether the promotion backup is
// present, how many anchors the live trust bundle carries, which root a new
// pin must name — and resolves that anchor through the pin producer the join
// verifier consumes, so the reader cannot name a root the pin would not accept.
func TestReadCARotationAtEachPhaseBoundary(t *testing.T) {
	// staged reaches trust-new on a fresh PKI and records both roots' pins while
	// each still has its own file: the old root in ca.crt, the new in ca-new.crt.
	staged := func(t *testing.T) map[string]string {
		prepPinned(t)
		refs := map[string]string{"old": hashFor(t, "ca")}
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		refs["new"] = hashFor(t, "ca-new")
		return refs
	}
	promoted := func(t *testing.T) {
		t.Helper()
		if err := pki.PromoteCA(); err != nil {
			t.Fatal(err)
		}
		if err := pki.PromoteSA(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name       string
		reach      func(t *testing.T) map[string]string
		wantStaged bool   // a new root is staged but not yet promoted
		wantProm   bool   // the live root's promotion backup is present
		wantBundle int    // CA anchors in the live trust bundle
		wantRoot   string // key into refs: the root the named anchor must pin
		wantNoPKI  bool   // no PKI written: the absent bundle is the read error
	}{
		{
			name: "idle",
			reach: func(t *testing.T) map[string]string {
				prepPinned(t)
				return map[string]string{"old": hashFor(t, "ca")}
			},
			wantStaged: false, wantProm: false, wantBundle: 1, wantRoot: "old",
		},
		{
			name:       "trust-new: staged root, trust wide, the pin names the staged root",
			reach:      staged,
			wantStaged: true, wantProm: false, wantBundle: 2, wantRoot: "new",
		},
		{
			name: "sign-new: the promotion consumed the staged file, trust still wide",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				return refs
			},
			wantStaged: false, wantProm: true, wantBundle: 2, wantRoot: "new",
		},
		{
			name: "finalized: only the new CA remains",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantStaged: false, wantProm: false, wantBundle: 1, wantRoot: "new",
		},
		{
			name: "rolled back from trust-new",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RemoveRotationScratch(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantStaged: false, wantProm: false, wantBundle: 1, wantRoot: "old",
		},
		{
			name: "rolled back from sign-new",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RemoveRotationScratch(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantStaged: false, wantProm: false, wantBundle: 1, wantRoot: "old",
		},
		{
			// The intermediate state a retried rollback observes: pki.RollbackCA
			// restores but leaves the staged material and the promotion backup
			// until RemoveRotationScratch, and the struct must still see them —
			// this is what keeps the readers' answers in the intermediate rows
			// of the pin above true once they read the struct.
			name: "rollback from trust-new, before the scratch is removed",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantStaged: true, wantProm: false, wantBundle: 1, wantRoot: "new",
		},
		{
			name: "rollback from sign-new, before the scratch is removed",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promoted(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantStaged: false, wantProm: true, wantBundle: 1, wantRoot: "old",
		},
		{
			// An uninitialized node: the absent bundle must read as a not-exist
			// error — the one read error checkCARotationState may permit under
			// --force — never as an idle trust bundle of zero anchors.
			name: "no PKI written yet",
			reach: func(t *testing.T) map[string]string {
				old := version.PKIDir
				t.Cleanup(func() { version.PKIDir = old })
				version.PKIDir = t.TempDir()
				return nil
			},
			wantStaged: false, wantProm: false, wantBundle: 0, wantRoot: "", wantNoPKI: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := tc.reach(t)
			rot := pki.ReadCARotation()
			if rot.Staged != tc.wantStaged {
				t.Errorf("ReadCARotation().Staged = %v, want %v", rot.Staged, tc.wantStaged)
			}
			if rot.Promoted != tc.wantProm {
				t.Errorf("ReadCARotation().Promoted = %v, want %v", rot.Promoted, tc.wantProm)
			}
			if rot.BundleCAs != tc.wantBundle {
				t.Errorf("ReadCARotation().BundleCAs = %d, want %d", rot.BundleCAs, tc.wantBundle)
			}
			if tc.wantNoPKI {
				if !errors.Is(rot.Err, os.ErrNotExist) {
					t.Errorf("ReadCARotation().Err = %v, want a not-exist error for the absent bundle", rot.Err)
				}
				return
			}
			if rot.Err != nil {
				t.Fatalf("ReadCARotation(): %v", rot.Err)
			}
			// The named anchor must resolve to the root this phase's join pin
			// records, through the same producer token.CACertHash consumes.
			pin, err := token.CACertHashFor(rot.Anchor)
			if err != nil {
				t.Fatalf("token.CACertHashFor(%q): %v", rot.Anchor, err)
			}
			if want := refs[tc.wantRoot]; pin != want {
				t.Errorf("anchor %q pins %s, want the %s root's pin %s", rot.Anchor, pin, tc.wantRoot, want)
			}
		})
	}
}

// liveCACrt reads this node's live root certificate, for the byte-identical
// assertion that a skipped or resumed phase never touched it.
func liveCACrt(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertCACrtUnchanged(t *testing.T, before []byte) {
	t.Helper()
	if after := liveCACrt(t); !bytes.Equal(before, after) {
		t.Error("ca.crt changed during a phase that must skip or resume it")
	}
}

// TestTrustNewAppliedOnlyPastTheRotation pins caPhaseApplied(trust-new) at the
// states where one of its inputs can mislead it. The .pre-carotate backup is
// not proof of promotion on its own — a rollback whose leaf repair or scratch
// removal failed leaves it behind on a node that holds one root again — and
// the trust bundle may be absent or unreadable on a node that never promoted,
// which checkCARotationState, not the applied check, has to judge (it lets
// --force through a missing bundle and a follower through anything). A wrong
// "applied" skips trust-new on a node that then never trusts the new root; a
// wrong "not applied" on a promoted node mints a second, divergent root.
func TestTrustNewAppliedOnlyPastTheRotation(t *testing.T) {
	steps := func(t *testing.T, fs ...func() error) {
		t.Helper()
		for _, f := range fs {
			if err := f(); err != nil {
				t.Fatal(err)
			}
		}
	}
	stage := func() error { return pki.RotateCAMaterial(false) }
	writeBundle := func(b []byte) func() error {
		return func() error { return os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), b, 0o644) }
	}
	removeBundle := func() error { return os.Remove(filepath.Join(version.PKIDir, "ca-bundle.crt")) }
	collapseBundle := func() error {
		// FinalizeCA's first write: the bundle holds only the live (new)
		// root, before the finalized marker and the scratch removal.
		b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
		if err != nil {
			return err
		}
		return writeBundle(b)()
	}
	for _, tc := range []struct {
		name    string
		reach   func(t *testing.T)
		want    bool
		wantErr bool
	}{
		{"idle", func(t *testing.T) { prepPinned(t) }, false, false},
		{"staged: AT trust-new, not past it", func(t *testing.T) { prepPinned(t); steps(t, stage) }, false, false},
		{"never promoted, bundle missing: the guard decides, not the applied check",
			func(t *testing.T) { prepPinned(t); steps(t, removeBundle) }, false, false},
		{"never promoted, bundle corrupt: the guard decides, not the applied check",
			func(t *testing.T) { prepPinned(t); steps(t, writeBundle([]byte("not pem"))) }, false, false},
		{"mid-sign-new: promoted, no marker, trust still old‖new",
			func(t *testing.T) { prepPinned(t); steps(t, stage, pki.PromoteCA, pki.PromoteSA) }, true, false},
		{"sign-new recorded", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted)
		}, true, false},
		{"sign-new recorded, bundle unreadable: the marker is the proof", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted, removeBundle)
		}, true, false},
		{"mid-finalize: bundle collapsed to the new root, marker not yet written", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted, collapseBundle)
		}, true, false},
		{"finalized", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted, pki.FinalizeCA)
		}, true, false},
		{"rolled back from sign-new, scratch not removed: the backup outlived the rotation", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.RollbackCA)
		}, false, false},
		{"a new rotation staged on a node whose rollback left its backup: AT trust-new", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.RollbackCA, stage)
		}, false, false},
		{"promoted without a marker and the bundle unreadable: cannot tell sign-new from a rollback", func(t *testing.T) {
			prepPinned(t)
			steps(t, stage, pki.PromoteCA, pki.PromoteSA, removeBundle)
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.reach(t)
			applied, err := caPhaseApplied(PhaseTrustNew)
			if (err != nil) != tc.wantErr {
				t.Fatalf("caPhaseApplied(trust-new) err = %v, want error %v", err, tc.wantErr)
			}
			if applied != tc.want {
				t.Errorf("caPhaseApplied(trust-new) = %v, want %v", applied, tc.want)
			}
		})
	}

	t.Run("a follower whose trust bundle is corrupt reaches the fetch that rewrites it", func(t *testing.T) {
		// checkCARotationState lets every --fetch-shared follower through:
		// the initiator's material it fetches rewrites the bundle. The
		// applied check must not refuse ahead of it on the bundle it is about
		// to replace. The fake cluster publishes no staged key, so the run
		// stops at the fetch — which is the point: it got there.
		kubeVIPNode(t)
		steps(t, writeBundle([]byte("not pem")))
		err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, RunID: "run-1", Quiet: true, SkipWorkloadCheck: true})
		if err == nil || !strings.Contains(err.Error(), "fetch staged CA material") {
			t.Fatalf("follower trust-new = %v, want it to reach the fetch of the initiator's material", err)
		}
	})
	t.Run("a forced recovery on a node predating the bundle files stages the new root", func(t *testing.T) {
		// checkCARotationState permits --force through a missing root
		// bundle for compromise recovery; the applied check must leave
		// that decision to it.
		kubeVIPNode(t)
		steps(t, removeBundle, func() error {
			if err := os.Remove(pki.KubeletClientBundlePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Force: true, RunID: "run-1", Quiet: true, SkipWorkloadCheck: true}); err != nil {
			t.Fatalf("forced trust-new without a trust bundle: %v", err)
		}
		if !pki.CARotationStaged() {
			t.Error("the forced recovery staged no new root")
		}
	})
	t.Run("a new rotation on a node whose rollback left its backup stages, not skips", func(t *testing.T) {
		kubeVIPNode(t)
		steps(t, stage, pki.PromoteCA, pki.PromoteSA, pki.RollbackCA)
		o := CARotateOptions{Phase: PhaseTrustNew, RunID: "brand-new-run", SkipWorkloadCheck: true}
		var out bytes.Buffer
		if err := cpTrustNew(&out, o); err != nil {
			t.Fatalf("trust-new of a new rotation: %v", err)
		}
		if strings.Contains(out.String(), "already applied") {
			t.Errorf("trust-new skipped on a rolled-back node: %q", out.String())
		}
		if !pki.CARotationStaged() {
			t.Error("trust-new staged nothing: this node would never trust the rotation's new root")
		}
		// The resume of that trust-new — its restart failed, say — must
		// re-run it too: the node is AT trust-new, and the stale backup beside
		// its widened bundle is not a promotion. A bundle the first run left
		// narrow is what the skip would strand for the rest of the rotation.
		fp, err := os.ReadFile(filepath.Join(version.PKIDir, "front-proxy-ca.crt"))
		if err != nil {
			t.Fatal(err)
		}
		steps(t, func() error {
			return os.WriteFile(filepath.Join(version.PKIDir, "front-proxy-ca-bundle.crt"), fp, 0o644)
		})
		out.Reset()
		if err := cpTrustNew(&out, o); err != nil {
			t.Fatalf("the resumed trust-new: %v", err)
		}
		if strings.Contains(out.String(), "already applied") {
			t.Errorf("the resumed trust-new skipped on a node only staged for the rotation: %q", out.String())
		}
		if n, err := pki.CABundleCertCount("front-proxy-ca"); err != nil || n != 2 {
			t.Errorf("after the resumed trust-new the front-proxy bundle holds %d CA(s) (%v), want old‖new", n, err)
		}
	})
}

// TestSignNewIsSkippedWhenAlreadyApplied pins caPhaseApplied, the per-phase
// "applied" test, modelled on pki.EtcdPhaseApplied: it reports whether a
// phase's material work is already present on this node, so a resumed
// rotation and a re-run `rotate-ca --cluster` SKIP finished work instead of
// tripping a guard written for a deliberate re-invocation.
//
// The contract caPhaseApplied (carotate.go) keeps, stated so the check and
// the pins cannot drift:
//
//   - trust-new is applied once the node is PAST it: the staged root was
//     promoted (the promotion backup pki.ReadCARotation reports) or the
//     rotation ended in finalize. A node that is merely AT trust-new —
//     material staged, bundles written — is not applied: re-running
//     RotateCAMaterial reuses the staged material and rewrites the same
//     bundles, which is the idempotent re-run the etcd reader also re-runs
//     rather than skips.
//   - sign-new is applied once its material work is recorded: the
//     carotate-promoted marker (pki.CAPromoted) or the carotate-finalized
//     marker (pki.CAFinalized). A node whose promotion stopped before the
//     marker — RotateCAMaterial+PromoteCA with the leaves never reissued, or
//     a sign-new whose restart failed — is NOT applied: its sign-new must
//     resume (pki.SignNewResumable, pki.FinishPromotion), and skipping it
//     would strand leaves the old CA signed and leave finalize refusing.
//     RotateCAMaterial+PromoteCA in particular reads as not applied: that
//     state has a forward resume path, which TestSignNewRestartFailureResumes
//     depends on.
//   - finalize is never applied, like pki.EtcdPhaseApplied's drop-root: a
//     re-run re-verifies and re-carries the restart a node whose finalize
//     restart failed still owes.
//
// The phase steps below run the REAL control-plane paths (cpSignNew,
// cpTrustNew) on the kubeVIPNode fixture: the consult must sit at the entry
// of each phase, BEFORE the guards and refusals it supersedes, the way
// migrateEtcdCAWith decides applied before it fetches (etcdca.go:205-235).
func TestSignNewIsSkippedWhenAlreadyApplied(t *testing.T) {
	// completed reaches sign-new's recorded end: promotion, SA promotion and
	// the marker signNewRestartThen writes once the restart succeeded.
	completed := func(t *testing.T) {
		t.Helper()
		prepPinned(t)
		for _, step := range []func() error{
			func() error { return pki.RotateCAMaterial(false) },
			pki.PromoteCA,
			pki.PromoteSA,
			pki.MarkCAPromoted,
		} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name         string
		reach        func(t *testing.T)
		wantTrustNew bool // caPhaseApplied(PhaseTrustNew)
		wantSignNew  bool // caPhaseApplied(PhaseSignNew)
	}{
		{
			name:  "idle: nothing applied",
			reach: prepPinned,
		},
		{
			name: "trust-new: material staged, the phase is AT, not past, itself",
			reach: func(t *testing.T) {
				prepPinned(t)
				if err := pki.RotateCAMaterial(false); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "sign-new: the promotion consumed the staged root but wrote no marker",
			reach: func(t *testing.T) {
				prepPinned(t)
				if err := pki.RotateCAMaterial(false); err != nil {
					t.Fatal(err)
				}
				if err := pki.PromoteCA(); err != nil {
					t.Fatal(err)
				}
			},
			wantTrustNew: true,
		},
		{
			name:         "sign-new: completed and recorded",
			reach:        completed,
			wantTrustNew: true,
			wantSignNew:  true,
		},
		{
			name: "finalized: the rotation is over but both earlier phases read applied",
			reach: func(t *testing.T) {
				completed(t)
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
			},
			wantTrustNew: true,
			wantSignNew:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.reach(t)
			for phase, want := range map[string]bool{
				PhaseTrustNew: tc.wantTrustNew,
				PhaseSignNew:  tc.wantSignNew,
				PhaseFinalize: false, // never skipped: a re-run re-verifies and carries the restart
			} {
				applied, err := caPhaseApplied(phase)
				if err != nil {
					t.Fatalf("caPhaseApplied(%s): %v", phase, err)
				}
				if applied != want {
					t.Errorf("caPhaseApplied(%s) = %v, want %v", phase, applied, want)
				}
			}
		})
	}
	t.Run("an unknown phase is an error, never a silent skip", func(t *testing.T) {
		if _, err := caPhaseApplied("no-such-phase"); err == nil {
			t.Error("caPhaseApplied on an unknown phase returned no error")
		}
	})

	// The steps below drive the real phase paths. A node that promoted and
	// stopped resumes its sign-new; a retried phase on a node that already
	// applied it skips rather than refuses, because a refusal there leaves a
	// halted `rotate-ca --cluster` with no forward path.
	t.Run("a retried sign-new finishes a node that promoted and stopped", func(t *testing.T) {
		kubeVIPNode(t)
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		if err := pki.PromoteCA(); err != nil {
			t.Fatal(err)
		}
		live := liveCACrt(t)
		if err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true}); err != nil {
			t.Fatalf("re-running sign-new after the promotion stopped it: %v", err)
		}
		assertCACrtUnchanged(t, live)
		if applied, err := caPhaseApplied(PhaseSignNew); err != nil || !applied {
			t.Errorf("after the resume, caPhaseApplied(sign-new) = %v, %v, want applied", applied, err)
		}
	})
	t.Run("a second re-run of a completed sign-new is skipped, not refused", func(t *testing.T) {
		kubeVIPNode(t)
		completed(t)
		live := liveCACrt(t)
		if err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true}); err != nil {
			t.Fatalf("a completed sign-new must skip on a re-run, not refuse: %v", err)
		}
		assertCACrtUnchanged(t, live)
	})
	t.Run("a re-run of trust-new past a promoted signer is skipped", func(t *testing.T) {
		// The trust-new re-run on a node that already promoted is the one
		// that would mint a DIVERGENT root (nothing is staged any more), so
		// the applied check must skip it before the mid-rotation guard fires.
		completed(t)
		live := liveCACrt(t)
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); err != nil {
			t.Fatalf("re-running trust-new on a node past it must skip, not refuse: %v", err)
		}
		assertCACrtUnchanged(t, live)
		if pki.CARotationStaged() {
			t.Error("the re-run staged a second, divergent root CA")
		}
	})
	t.Run("a re-run of trust-new on a staged node reuses the staged material", func(t *testing.T) {
		// A trust-new halt leaves the initiator staged; its re-run must be
		// the idempotent one — RotateCAMaterial re-uses the staged root and
		// rewrites the same bundles — not a refusal and not a second root.
		// This is the arm of the resume the promoted-state skip above cannot
		// cover: a staged node is AT trust-new, not past it, so no skip
		// applies and the guard must let the same rotation's re-run through.
		kubeVIPNode(t)
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		stagedRoot := liveCACrt(t) // still the old root: trust-new only stages
		stagedNew, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-new.crt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); err != nil {
			t.Fatalf("re-running trust-new on a staged node must reuse the staged material, not refuse: %v", err)
		}
		assertCACrtUnchanged(t, stagedRoot)
		after, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-new.crt"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stagedNew, after) {
			t.Error("the re-run staged a second, divergent root CA over the first run's material")
		}
	})
}
