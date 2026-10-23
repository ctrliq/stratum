package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

// A worker reads no cluster config, so on a CIS cluster the join line is the
// only way it learns to run the CIS profile -- and with it the kubelet's
// seccompDefault. The worker and the control-plane join line each carry --cis
// there, and neither does on a cluster without it.
func TestJoinLinesCarryCISOnACISCluster(t *testing.T) {
	registerCISForTest(t)
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })

	for _, cis := range []bool{false, true} {
		if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Hostname: "cp1", CIS: cis}); err != nil {
			t.Fatal(err)
		}
		extra, err := profileJoinFlag()
		if err != nil {
			t.Fatal(err)
		}
		for verb, cp := range map[string]bool{"stratum up --worker": false, "stratum up --join": true} {
			var out bytes.Buffer
			var revoked []string
			c := okCmd(&out, &revoked)
			c.joinExtra = extra
			if err := createToken("https://cp:6443", cp, token.DefaultTokenTTL, c); err != nil {
				t.Fatal(err)
			}
			line := ""
			for _, l := range strings.Split(out.String(), "\n") {
				if strings.Contains(l, verb) {
					line = l
				}
			}
			if got := strings.HasSuffix(line, " --profile cis"); got != cis {
				t.Errorf("cis=%v: %s line = %q; ends with --profile cis = %v", cis, verb, line, got)
			}
		}
	}
}
