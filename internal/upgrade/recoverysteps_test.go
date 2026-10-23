package upgrade

import (
	"errors"
	"strings"
	"testing"
)

// The printed recovery steps: every command runs the --from binary (the node has
// no kubectl on its PATH, and a bare stratum may be an older release), a missing
// Job is explained, the uncordon is qualified, and a path that is not a plain
// word is quoted.
func TestRecoveryStepsRunTheFromBinary(t *testing.T) {
	for _, from := range []string{"/var/lib/stratum/upgrade/stratum-v1.37.1", "/opt/my stratum/it's"} {
		out := recoverySteps(Node{Name: "cp1", IsCP: true}, "stratum-upgrade-cp1", ClusterOptions{Target: "v1.37.1", From: from})
		q := shellQuote(from)
		for _, want := range []string{
			q + " kubectl -n kube-system get job/stratum-upgrade-cp1",
			`"not found" means it`,
			"already finished and was cleaned up",
			q + " upgrade --cluster --target v1.37.1 --from " + q + " --yes",
			"this run cordoned it",
			q + " kubectl uncordon cp1",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("from=%q: recovery steps missing %q:\n%s", from, want, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "kubectl") && !strings.Contains(line, q+" kubectl") {
				t.Errorf("from=%q: a bare kubectl: %q", from, line)
			}
		}
	}
	for in, want := range map[string]string{
		"/a/b-c_d.e": "/a/b-c_d.e", "": "''", "a b": "'a b'", "it's": `'it'\''s'`, "$x": "'$x'", "a;b": "'a;b'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// A worker's Job never uncordons it, so a halted run leaves it cordoned. A
// controller's Job uncordons it once its upgrade passes, so its cordon is not
// stated as fact.
func TestRecoveryStepsStateTheCordonOnlyOfAWorker(t *testing.T) {
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-v1.37.1"}
	for _, c := range []struct {
		n    Node
		want string
	}{
		{Node{Name: "w1"}, "  w1 is left cordoned, and its upgrade Job may still be running. To recover:\n"},
		{Node{Name: "cp1", IsCP: true}, "  cp1 may be left cordoned (its upgrade Job uncordons it once upgraded), and the Job may still be running. To recover:\n"},
	} {
		if got := recoverySteps(c.n, "stratum-upgrade-"+c.n.Name, o); !strings.HasPrefix(got, c.want) {
			t.Errorf("recovery steps for %s start:\n%s\nwant:\n%s", c.n.Name, got, c.want)
		}
	}
}

// The Job's backstop warning names a command that runs on the node and under
// sudo: the installed binary's absolute path (Rocky's secure_path has no
// /usr/local/bin) and its bundled kubectl. It also says to uncordon only a
// cordon the cluster upgrade made, since it runs when ownership could not be read.
func TestUncordonWarningRunsAnAbsoluteBundledKubectl(t *testing.T) {
	swap(t, &stratumInstallPath, "/usr/local/bin/stratum")
	got := uncordonWarning("cp1", errors.New("connection refused"))
	for _, want := range []string{
		"run: /usr/local/bin/stratum kubectl uncordon cp1\n",
		"connection refused",
		"if the cluster upgrade cordoned it (you did not cordon it yourself before the upgrade)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ReplaceAll(got, "/usr/local/bin/stratum kubectl", ""), "kubectl uncordon") {
		t.Errorf("a bare kubectl in %q", got)
	}
	swap(t, &stratumInstallPath, "/opt/my stratum/stratum")
	if got := uncordonWarning("cp1", errors.New("x")); !strings.Contains(got, "run: '/opt/my stratum/stratum' kubectl uncordon cp1") {
		t.Errorf("a path with a space is not quoted: %q", got)
	}
}
