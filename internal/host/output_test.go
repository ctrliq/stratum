package host

import (
	"bytes"
	"strings"
	"testing"
)

func TestUntrustProgressUsesOperationWriter(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed source", true: "interface warning"}[fail], func(t *testing.T) {
			f := newFakeFirewalld(t)
			seedLegacySource(t, f, "10.245.0.0/16")
			if fail {
				f.ifaceRuntime[ciliumPodInterface] = firewalldZone
				r, err := readFirewalldRecord()
				if err != nil {
					t.Fatal(err)
				}
				r.TrustedInterfaces = []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true}}
				if err := writeFirewalldRecord(r); err != nil {
					t.Fatal(err)
				}
				f.failQuery = true
			}
			var progress bytes.Buffer
			var cleanupErr error
			stdout := quiet(t, func() { cleanupErr = UntrustPodCIDRsWithWriter(&progress) })
			if fail && cleanupErr == nil {
				t.Fatal("failed query must report cleanup failure")
			}
			if !fail && cleanupErr != nil {
				t.Fatal(cleanupErr)
			}
			if stdout != "" {
				t.Errorf("firewall cleanup leaked to process stdout: %q", stdout)
			}
			want := "removed trusted source"
			if fail {
				want = "cannot check interface"
			}
			if !strings.Contains(progress.String(), want) {
				t.Errorf("operation progress missing %q: %q", want, progress.String())
			}
		})
	}
}
