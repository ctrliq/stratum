package upgrade

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestJobName(t *testing.T) {
	const prefix = "stratum-upgrade-"
	shared := strings.Repeat("a", 63)
	names := []string{
		"worker-1",
		strings.Repeat("b", 63-len(prefix)),
		strings.Repeat("b", 64-len(prefix)),
		shared + ".worker-1",
		shared + ".worker-2",
		strings.Repeat("c", 30) + ".worker-1.example.com",
		strings.Repeat("d", 30) + "-worker-1.example.com",
		strings.Repeat(shared+".", 3) + strings.Repeat("e", 61),
	}
	seen := make(map[string]string)
	for _, node := range names {
		t.Run(node, func(t *testing.T) {
			if errs := validation.IsDNS1123Subdomain(node); len(errs) != 0 {
				t.Fatalf("invalid fixture: %v", errs)
			}
			got := jobName(node)
			if len(got) > 63 {
				t.Errorf("Job name length %d exceeds 63", len(got))
			}
			if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
				t.Errorf("invalid Job name %q: %v", got, errs)
			}
			if !strings.HasPrefix(got, prefix) {
				t.Errorf("lost prefix: %q", got)
			}
			if len(prefix+node) <= 63 && got != prefix+node {
				t.Errorf("short name changed: %q", got)
			}
			if again := jobName(node); again != got {
				t.Errorf("name is not stable: %q vs %q", got, again)
			}
			if other, ok := seen[got]; ok {
				t.Errorf("nodes %q and %q share Job name %q", other, node, got)
			}
			seen[got] = node
		})
	}
}

func TestValidateUpgradeJobNames(t *testing.T) {
	const prefix = "stratum-upgrade-"
	long := strings.Repeat("a", 63) + ".worker-1"
	if errs := validation.IsDNS1123Subdomain(long); len(errs) != 0 {
		t.Fatalf("long node name is invalid: %v", errs)
	}
	short := strings.TrimPrefix(jobName(long), prefix)
	if len(prefix+short) > maxJobNameLen || jobName(short) != jobName(long) {
		t.Fatalf("fixture does not collide: %q and %q", long, short)
	}
	if errs := validation.IsDNS1123Subdomain(short); len(errs) != 0 {
		t.Fatalf("short node name is invalid: %v", errs)
	}

	err := validateUpgradeJobNames([]Node{{Name: long}, {Name: "worker-2"}, {Name: short}})
	if err == nil || !strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), short) {
		t.Fatalf("collision was not identified: %v", err)
	}
	if err := validateUpgradeJobNames([]Node{{Name: long}, {Name: "worker-2"}}); err != nil {
		t.Fatalf("distinct Job names rejected: %v", err)
	}
}
