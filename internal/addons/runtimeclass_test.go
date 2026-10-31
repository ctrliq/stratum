package addons

import (
	"bytes"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

func TestRuntimeClassesSelectTheNodesCarryingTheirHandler(t *testing.T) {
	docs := bytes.Split(RenderRuntimeClasses([]string{"nvidia", "kata"}), []byte("---\n"))
	if len(docs) != 2 {
		t.Fatalf("rendered %d documents, want one RuntimeClass per handler", len(docs))
	}
	for i, want := range []string{"kata", "nvidia"} {
		var rc nodev1.RuntimeClass
		if err := yaml.UnmarshalStrict(docs[i], &rc); err != nil {
			t.Fatal(err)
		}
		if rc.APIVersion != "node.k8s.io/v1" || rc.Kind != "RuntimeClass" {
			t.Errorf("document %d is %s %s, want node.k8s.io/v1 RuntimeClass", i, rc.APIVersion, rc.Kind)
		}
		if rc.Name != want || rc.Handler != want {
			t.Errorf("document %d: name %q handler %q, want both %q", i, rc.Name, rc.Handler, want)
		}
		if rc.Scheduling == nil || len(rc.Scheduling.NodeSelector) != 1 || rc.Scheduling.NodeSelector[k8sutil.RuntimeHandlerLabelPrefix+want] != "true" {
			t.Errorf("%s selects %+v, want only %s%s=true", want, rc.Scheduling, k8sutil.RuntimeHandlerLabelPrefix, want)
		}
		if rc.Overhead != nil || rc.Scheduling.Tolerations != nil {
			t.Errorf("%s carries overhead or tolerations it was not asked for", want)
		}
	}
}

func TestNoRuntimeClassesRenderNoStack(t *testing.T) {
	if got := RenderRuntimeClasses(nil); got != nil {
		t.Errorf("no handlers rendered %q, want nothing", got)
	}
}
