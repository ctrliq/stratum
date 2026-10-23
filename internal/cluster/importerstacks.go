package cluster

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/version"
)

// StackContext is the cluster an importer's stacks are rendered for.
type StackContext struct {
	ClusterDomain string
	ServiceCIDR   string
}

// ImporterStacks, when set, returns an importer's stacks as "stack/file.yaml"
// to contents; writeBundles stages them beside the bundled add-ons. Unset, a
// start removes the files an earlier start staged for an importer.
var ImporterStacks func(StackContext) (map[string][]byte, error)

// bundledStacks are the stack names writeBundles can stage itself.
var bundledStacks = []string{"crds", "node-mark", "cilium", "coredns", "local-path", "dashboard"}

// importerIndex records the files importer stacks staged, so a later start can remove the ones no longer returned.
const importerIndex = ".importer-stacks.json"

func renderImporterStacks(st *NodeState) (map[string][]byte, error) {
	if ImporterStacks == nil {
		return nil, nil
	}
	stacks, err := ImporterStacks(StackContext{ClusterDomain: st.Domain(), ServiceCIDR: cmp.Or(st.ServiceCIDR, netcfg.Default().ServiceCIDR)})
	if err != nil {
		return nil, fmt.Errorf("render the importer's stacks: %w", err)
	}
	for rel := range stacks {
		if err := checkImporterPath(rel); err != nil {
			return nil, err
		}
	}
	return stacks, nil
}

func checkImporterPath(rel string) error {
	stack, file, ok := strings.Cut(rel, "/")
	switch {
	case !ok || path.Clean(rel) != rel || strings.Contains(file, "/"):
		return fmt.Errorf("importer stack file %q: want <stack>/<file>.yaml", rel)
	case stack == "" || strings.HasPrefix(stack, ".") || strings.HasPrefix(file, "."):
		return fmt.Errorf("importer stack file %q: names may not be empty or start with a dot", rel)
	case len(validation.IsDNS1123Label(stack)) > 0:
		// The deployer labels each object with its stack name.
		return fmt.Errorf("importer stack file %q: the stack name must be a DNS-1123 label: %s",
			rel, strings.Join(validation.IsDNS1123Label(stack), "; "))
	case !strings.HasSuffix(file, ".yaml") && !strings.HasSuffix(file, ".yml"):
		return fmt.Errorf("importer stack file %q: the deployer applies only .yaml and .yml files", rel)
	case slices.Contains(bundledStacks, stack):
		return fmt.Errorf("importer stack file %q: %q is a bundled stack", rel, stack)
	}
	return nil
}

// removeStaleImporterFiles deletes files an earlier start staged for an
// importer and this one did not, then records the current set.
func removeStaleImporterFiles(current map[string][]byte) error {
	idx := filepath.Join(version.ManifestsDir, importerIndex)
	var prev []string
	switch b, err := os.ReadFile(idx); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read %s: %w", idx, err)
	default:
		if err := json.Unmarshal(b, &prev); err != nil {
			return fmt.Errorf("parse %s: %w", idx, err)
		}
	}
	for _, rel := range prev {
		if _, keep := current[rel]; keep || checkImporterPath(rel) != nil {
			continue
		}
		file := filepath.Join(version.ManifestsDir, rel)
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the dropped importer stack file %s: %w", file, err)
		}
		_ = os.Remove(filepath.Dir(file))
	}

	names := make([]string, 0, len(current))
	for rel := range current {
		names = append(names, rel)
	}
	slices.Sort(names)
	if slices.Equal(names, prev) {
		return nil
	}
	if len(names) == 0 {
		if err := os.Remove(idx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", idx, err)
		}
		return nil
	}
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return host.WriteFileAtomically(idx, append(b, '\n'), 0o644)
}
