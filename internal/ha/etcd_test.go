package ha

import "testing"

func TestPeerURLMatchComparesHostAndPort(t *testing.T) {
	const listed = "https://[fd00::10]:2380" // the URL the member was added under
	m := etcdMembership{Members: []etcdMember{{ID: 7, Name: "cp2", PeerURLs: []string{listed}}}}

	for _, form := range []string{
		"https://[fd00::10]:2380",            // byte-identical
		"https://[FD00::10]:2380",            // hex-digit case
		"https://[fd00:0:0:0:0:0:0:10]:2380", // zero runs expanded, not compressed
	} {
		got := m.memberByPeerURL(form)
		if got == nil || got.ID != 7 {
			t.Errorf("memberByPeerURL(%q) = %v, want member 7 (listed as %q)", form, got, listed)
		}
	}

	for _, form := range []string{
		"https://[fd00::10]:2381", // another port is another member
		"https://[fd00::11]:2380", // another host is another member
	} {
		if got := m.memberByPeerURL(form); got != nil {
			t.Errorf("memberByPeerURL(%q) = %v, want nil (listed as %q)", form, got, listed)
		}
	}
}

func TestMemberHostsNamesAnUnstartedMemberByItsPeerAddress(t *testing.T) {
	got, err := memberHosts([]etcdMember{
		{ID: 1, Name: "cp1", PeerURLs: []string{"https://10.0.0.1:2380"},
			ClientURLs: []string{"https://127.0.0.1:2379", "https://10.0.0.1:2379"}},
		{ID: 2, PeerURLs: []string{"https://10.0.0.2:2380"}},
		{ID: 3, PeerURLs: []string{"https://[fd00::3]:2380"}, IsLearner: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1", "10.0.0.2", "fd00::3"}
	if len(got) != len(want) {
		t.Fatalf("memberHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("memberHosts = %v, want %v", got, want)
		}
	}
}

func TestMemberHostsRefusesAMemberItCannotPlace(t *testing.T) {
	if _, err := memberHosts([]etcdMember{{ID: 9}}); err == nil {
		t.Fatal("a member with no URL was accepted, so a run could leave it out")
	}
}
