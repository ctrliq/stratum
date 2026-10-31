package runtime

import (
	"testing"

	"sigs.k8s.io/yaml"
)

// The node's taints reach kubelet.conf as registerWithTaints, read back the
// way the kubelet reads it: each key, value and effect in its own field.
func TestRegisterWithTaintsLine(t *testing.T) {
	if got := registerWithTaintsLine(nil); got != "" {
		t.Errorf("no taints rendered %q, want nothing", got)
	}
	line := registerWithTaintsLine([]string{"stratum.ciq.com/unverified=true:NoSchedule", "dedicated:NoExecute"})
	want := `registerWithTaints: [{key: "stratum.ciq.com/unverified", value: "true", effect: NoSchedule}, {key: "dedicated", value: "", effect: NoExecute}]` + "\n"
	if line != want {
		t.Errorf("rendered\n%s\nwant\n%s", line, want)
	}
	var conf struct {
		RegisterWithTaints []struct {
			Key, Value, Effect string
		} `json:"registerWithTaints"`
	}
	if err := yaml.Unmarshal([]byte(line), &conf); err != nil {
		t.Fatalf("the line does not parse: %v", err)
	}
	if len(conf.RegisterWithTaints) != 2 ||
		conf.RegisterWithTaints[0].Key != "stratum.ciq.com/unverified" || conf.RegisterWithTaints[0].Value != "true" || conf.RegisterWithTaints[0].Effect != "NoSchedule" ||
		conf.RegisterWithTaints[1].Key != "dedicated" || conf.RegisterWithTaints[1].Value != "" || conf.RegisterWithTaints[1].Effect != "NoExecute" {
		t.Errorf("registerWithTaints read back as %+v", conf.RegisterWithTaints)
	}
}
