package operator

import (
	"context"
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestStagedGenerationDistinguishesRestartUpgradeAndRollback(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	old := StagedGeneration{PublicationID: "old", Digest: "sha256:old", Inputs: ClusterInputs{DNSAddress: "10.43.0.10", ClusterDomain: "cluster.local"}}
	newer := old
	newer.PublicationID = "new"
	newer.Digest = "sha256:new"
	oldPub, err := RecordStagedGeneration(ctx, cs, old, "")
	if err != nil {
		t.Fatal(err)
	}
	newPub, err := RecordStagedGeneration(ctx, cs, newer, oldPub.ID)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, previous := range []string{"", oldPub.ID} {
		if _, err := RecordStagedGeneration(ctx, cs, old, previous); err == nil {
			t.Fatal("an old first-time participant or unchanged restart replaced the new desired generation")
		}
		if got := clusterInventory(t, cs).Desired.Digest; got != newer.Digest {
			t.Fatalf("desired digest = %s, want %s", got, newer.Digest)
		}
	}
	old.PublicationID = "rollback"
	if _, err := RecordStagedGeneration(ctx, cs, old, newPub.ID); err != nil {
		t.Fatalf("changed local render authorizes rollback: %v", err)
	}
	if got := clusterInventory(t, cs).Desired.Digest; got != old.Digest {
		t.Fatalf("rollback digest = %s, want %s", got, old.Digest)
	}
}

func TestStagedGenerationPublishesInputsAtomicallyWithBundles(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	before := StagedGeneration{PublicationID: "before", Digest: "sha256:same", Inputs: ClusterInputs{DNSAddress: "10.43.0.10", ClusterDomain: "cluster.local"}}
	beforePub, err := RecordStagedGeneration(ctx, cs, before, "")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.PublicationID = "after"
	after.Inputs.ClusterDomain = "k8s.example.internal"
	if _, err := RecordStagedGeneration(ctx, cs, after, beforePub.ID); err != nil {
		t.Fatal(err)
	}
	desired := clusterInventory(t, cs).Desired
	if desired.Generation != 2 || desired.Inputs == nil || *desired.Inputs != after.Inputs {
		t.Fatalf("desired = %+v, want generation 2 with changed inputs", desired)
	}
	if _, err := RecordStagedGeneration(ctx, cs, before, beforePub.ID); err == nil {
		t.Fatal("unchanged stale inputs replaced the cluster's new inputs")
	}
}

func TestStagedGenerationRefusesLatePublicationFromOldPredecessor(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	old := StagedGeneration{PublicationID: "old", Digest: "sha256:old", Inputs: ClusterInputs{ClusterDomain: "cluster.local"}}
	oldPub, err := RecordStagedGeneration(ctx, cs, old, "")
	if err != nil {
		t.Fatal(err)
	}
	first := old
	first.PublicationID = "first"
	first.Digest = "sha256:first"
	if _, err := RecordStagedGeneration(ctx, cs, first, oldPub.ID); err != nil {
		t.Fatal(err)
	}
	late := old
	late.PublicationID = "late"
	late.Digest = "sha256:late"
	if _, err := RecordStagedGeneration(ctx, cs, late, oldPub.ID); err == nil {
		t.Fatal("late staging from an obsolete predecessor replaced the active generation")
	}
	if got := clusterInventory(t, cs).Desired.Digest; got != first.Digest {
		t.Fatalf("desired = %s, want %s", got, first.Digest)
	}
}

func TestStagedPublicationIdentitySurvivesContentRollback(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	a := StagedGeneration{PublicationID: "A-first", Digest: "sha256:A", Inputs: ClusterInputs{ClusterDomain: "cluster.local"}}
	first, err := RecordStagedGeneration(t.Context(), cs, a, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := a
	joined.PublicationID = "A-joined"
	if _, err := RecordStagedGeneration(t.Context(), cs, joined, ""); err != nil {
		t.Fatal(err)
	}
	b := a
	b.PublicationID, b.Digest = "B", "sha256:B"
	second, err := RecordStagedGeneration(t.Context(), cs, b, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.PublicationID = "A-rollback"
	rollback, err := RecordStagedGeneration(t.Context(), cs, a, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := a
	c.PublicationID, c.Digest = "C", "sha256:C"
	if _, err := RecordStagedGeneration(t.Context(), cs, c, first.ID); err == nil {
		t.Fatal("the first A publication authorized replacement of the rollback")
	}
	c.Unacknowledged = []string{first.ID}
	if _, err := RecordStagedGeneration(t.Context(), cs, c, ""); err == nil {
		t.Fatal("old uncertain publication history authorized replacement of the rollback")
	}
	c.Unacknowledged = []string{joined.PublicationID}
	if _, err := RecordStagedGeneration(t.Context(), cs, c, ""); err == nil {
		t.Fatal("an old joined publication authorized replacement of the rollback")
	}
	if _, err := RecordStagedGeneration(t.Context(), cs, c, rollback.ID); err != nil {
		t.Fatalf("current publication must authorize a new staging action: %v", err)
	}
}
