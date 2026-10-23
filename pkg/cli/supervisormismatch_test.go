package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/comps"
)

func TestSupervisorMismatch(t *testing.T) {
	saved := runningSupervisor
	t.Cleanup(func() { runningSupervisor = saved })
	for _, c := range []struct {
		name string
		cur  comps.RunningSupervisor
		err  error
		warn bool
	}{
		{name: "stale", cur: comps.RunningSupervisor{PID: 9, Exe: "/usr/local/bin/stratum (deleted)"}, warn: true},
		{name: "current", cur: comps.RunningSupervisor{PID: 9, Exe: "/usr/local/bin/stratum", Installed: true}},
		{name: "unreadable", err: errors.New("permission denied")},
	} {
		runningSupervisor = func() (comps.RunningSupervisor, error) { return c.cur, c.err }
		got := supervisorMismatch()
		if c.warn != (got != "") {
			t.Errorf("%s: %q", c.name, got)
		}
		if c.warn && (!strings.Contains(got, "pid 9") || !strings.Contains(got, "(deleted)") || !strings.Contains(got, "restart `stratum up`")) {
			t.Errorf("%s: warning does not say what runs and what to do: %q", c.name, got)
		}
	}
}
