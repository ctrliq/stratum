package selinux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemExecLabel(t *testing.T) {
	const shell = "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023\x00"
	for _, tc := range []struct {
		name     string
		mode     Mode
		labelled bool
		current  string
		want     string
	}{
		{"first start from a shell, the node just labelled", Enforcing, true, shell, systemRuntimeContext},
		{"later start from a shell", Enforcing, true, "unconfined_u:unconfined_r:container_runtime_t:s0-s0:c0.c1023\x00", systemRuntimeContext},
		{"permissive, labelled", Permissive, true, shell, systemRuntimeContext},
		{"started by systemd", Enforcing, true, "system_u:system_r:container_runtime_t:s0\x00", ""},
		{"opted out or not yet labelled", Enforcing, false, shell, ""},
		{"selinux disabled", Disabled, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, tc.mode)
			if tc.labelled {
				writeStamp(t)
			}
			old := procCurrent
			t.Cleanup(func() { procCurrent = old })
			procCurrent = filepath.Join(t.TempDir(), "current")
			if tc.current != "" {
				if err := os.WriteFile(procCurrent, []byte(tc.current), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := SystemExecLabel(); got != tc.want {
				t.Errorf("SystemExecLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}
