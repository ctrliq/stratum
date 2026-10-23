// npm.go checks the npm packages bundled into the dashboard.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// npmPackage is one entry of the list dash/bundled-packages.js writes.
type npmPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	License string `json:"license"`
}

func checkNpm(file string, rep *reporter, stderr io.Writer) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var pkgs []npmPackage
	if err := json.Unmarshal(data, &pkgs); err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	if pkgs == nil {
		return fmt.Errorf("reading %s: want a JSON array of {name, version, license}", file)
	}
	// The dashboard bundles react at least, so an empty list means the
	// listing broke, and passing it would check nothing.
	if len(pkgs) == 0 {
		return fmt.Errorf("reading %s: lists no packages", file)
	}
	for i, p := range pkgs {
		if p.Name == "" {
			return fmt.Errorf("reading %s: entry %d has no name", file, i)
		}
	}
	sort.SliceStable(pkgs, func(i, j int) bool {
		if pkgs[i].Name != pkgs[j].Name {
			return pkgs[i].Name < pkgs[j].Name
		}
		return pkgs[i].Version < pkgs[j].Version
	})
	for _, p := range pkgs {
		name := p.Name
		if p.Version != "" {
			name += "@" + p.Version
		}
		rep.report(name, npmVerdict(p))
	}
	rep.summary(stderr, fmt.Sprintf("%d npm packages in %s", len(pkgs), file))
	return nil
}

// npmVerdict checks the SPDX expression in a package's license field. An
// empty field, or one that is not an SPDX expression (such as "SEE LICENSE
// IN LICENSE.md"), is an unknown license.
func npmVerdict(p npmPackage) verdict {
	license := strings.TrimSpace(p.License)
	if license == "" || noLicense(license) {
		return verdict{levelWarn, fmt.Sprintf("no license declared (license %q)", license)}
	}
	e, err := parseExpr(license)
	if err != nil {
		return verdict{levelWarn, fmt.Sprintf("unknown license %q: %v", license, err)}
	}
	return judge(p.Name, e, fmt.Sprintf("license %q", license))
}
