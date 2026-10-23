package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflag takes the first back-quoted word in a flag's usage as the name of its
// value, so `--help` printed "--service-cidr stratum token create" for a
// string flag. A value name is one word; a space in one means prose
// was quoted by accident.
func TestNoFlagUsageQuotesProseAsItsValueName(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if name, _ := pflag.UnquoteUsage(f); strings.ContainsAny(name, " \t") {
				t.Errorf("%s --%s: help shows its value as %q; drop the backticks from its usage", c.CommandPath(), f.Name, name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRootCommand())
}
