package mcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/comps"
)

func TestNoteStaleSupervisor(t *testing.T) {
	stale := func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{PID: 9, Exe: "/usr/local/bin/stratum (deleted)"}, nil
	}
	current := func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{PID: 9, Exe: "/usr/local/bin/stratum", Installed: true}, nil
	}
	unreadable := func() (comps.RunningSupervisor, error) { return comps.RunningSupervisor{}, errors.New("not root") }

	out := ClusterStatusOut{Healthy: true}
	noteStaleSupervisor(&out, stale)
	if !out.SupervisorStale || !strings.HasPrefix(out.Note, "The supervisor (pid 9) runs /usr/local/bin/stratum (deleted)") {
		t.Errorf("stale: %+v", out)
	}
	if !out.Healthy {
		t.Error("a stale supervisor changed the health verdict, which is about the processes it runs")
	}

	out = ClusterStatusOut{Note: "Wanted but not running: kubelet"}
	noteStaleSupervisor(&out, stale)
	if !strings.HasPrefix(out.Note, "Wanted but not running: kubelet. The supervisor") {
		t.Errorf("an existing note was not kept first: %q", out.Note)
	}

	for name, read := range map[string]func() (comps.RunningSupervisor, error){"current": current, "unreadable": unreadable} {
		out = ClusterStatusOut{}
		noteStaleSupervisor(&out, read)
		if out.SupervisorStale || out.Note != "" {
			t.Errorf("%s: %+v, want nothing added", name, out)
		}
	}
}
