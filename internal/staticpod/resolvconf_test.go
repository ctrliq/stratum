package staticpod

import "testing"

// TestStaticPodsKeepTheKubeletsResolver: the control plane resolves through
// the node's own resolv.conf, as the kubelet hands it to host-network pods.
// Only CoreDNS forwards through a chosen upstream, so no static pod
// mounts one over /etc/resolv.conf or carries a digest that would recreate
// it.
func TestStaticPodsKeepTheKubeletsResolver(t *testing.T) {
	st := testNode(t)
	st.VIP.Address = "192.0.2.100"
	for _, name := range Names {
		pod, err := render(name, st)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.MountPath == "/etc/resolv.conf" {
				t.Errorf("%s mounts %q over /etc/resolv.conf", name, m.Name)
			}
		}
		if v, ok := pod.Annotations["stratum.ciq.com/resolv-conf-sha256"]; ok {
			t.Errorf("%s carries a resolv.conf digest %q: a resolver change would recreate it", name, v)
		}
	}
}
