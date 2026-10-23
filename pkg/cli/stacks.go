package cli

import (
	"fmt"

	"github.com/ctrliq/stratum/internal/cluster"
)

// StackContext is the cluster an importer's stacks are rendered for.
type StackContext struct {
	ClusterDomain string
	ServiceCIDR   string
}

// A StackFunc returns manifest stacks keyed "<stack>/<file>.yaml" (or .yml):
// one directory level, a stack name that is a DNS-1123 label and not a bundled
// one, and no file name starting with a dot. On every control-plane `stratum up`
// they are staged beside the bundled add-ons, which the operator applies and
// prunes; a file no longer returned is removed, and its objects pruned. Only
// the operator leader's copy is applied, so return the same bytes on every
// control plane. An error, or a key that breaks these rules, fails the start.
type StackFunc func(StackContext) (map[string][]byte, error)

// WithStacks adds an importer's manifest stacks to every control-plane start.
func WithStacks(f StackFunc) Option {
	return func(c *rootConfig) {
		if f != nil {
			c.stacks = append(c.stacks, f)
		}
	}
}

func importerStacks(fs []StackFunc) func(cluster.StackContext) (map[string][]byte, error) {
	if len(fs) == 0 {
		return nil
	}
	return func(c cluster.StackContext) (map[string][]byte, error) {
		all := map[string][]byte{}
		for _, f := range fs {
			stacks, err := f(StackContext(c))
			if err != nil {
				return nil, err
			}
			for rel, content := range stacks {
				if _, dup := all[rel]; dup {
					return nil, fmt.Errorf("two importer stack functions return %s", rel)
				}
				all[rel] = content
			}
		}
		return all, nil
	}
}
