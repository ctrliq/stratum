package cli

import (
	"cmp"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// defaultProductName is the name the engine's own build gives itself in help
// and in the first line of an install command's output.
const defaultProductName = "Stratum"

// WithProductName names the product in `stratum --help` and in the line
// `up`, `token create` and `uninstall` print first, so a downstream build
// introduces itself by its own name rather than the engine's. Empty (the
// default) leaves "Stratum".
func WithProductName(name string) Option {
	return func(c *rootConfig) {
		if name != "" {
			c.productName = name
		}
	}
}

// product is the configured product name, or the engine's.
func (c rootConfig) product() string {
	return cmp.Or(c.productName, defaultProductName)
}

// headline is the first line an install command prints: the vendor, the
// product and a version a person can read, such as "CIQ Stratum X.Y.Z".
func (c rootConfig) headline() string {
	return fmt.Sprintf("CIQ %s %s", c.product(), readableVersion(c.displayVersion))
}

// printHeadline writes the headline as the command's first line.
func (c rootConfig) printHeadline(w io.Writer) {
	fmt.Fprintln(w, c.headline())
}

// releaseVersion matches a release tag: the Kubernetes version, the build's
// own suffix (stratum, or stratum- and a downstream build's name) and its
// number, then an optional release-candidate suffix.
var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)-stratum(?:-[a-z]+)*\.\d+(?:-(rc\.\d+))?$`)

// readableVersion turns a release tag into what a person reads: the
// Kubernetes version, with a release candidate named in brackets, so
// vX.Y.Z-stratum.0 reads X.Y.Z and vX.Y.Z-stratum.0-rc.2 reads
// X.Y.Z (rc.2). Anything else, such as a development build, is shown as it
// is, without a leading v.
func readableVersion(v string) string {
	m := releaseVersion.FindStringSubmatch(v)
	if m == nil {
		return strings.TrimPrefix(v, "v")
	}
	if m[2] != "" {
		return fmt.Sprintf("%s (%s)", m[1], m[2])
	}
	return m[1]
}
