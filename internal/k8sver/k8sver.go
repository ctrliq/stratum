// Package k8sver parses, validates and orders Kubernetes version strings.
package k8sver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Exported to pin the CRD's OpenAPI pattern; OpenAPI patterns search, so anchors are load-bearing.
const TargetPattern = `^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`

var targetRe = regexp.MustCompile(TargetPattern)

type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Patch participates: a major/minor-only compare would let a patch downgrade through.
func (v Version) Compare(o Version) int {
	for _, p := range [3][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

// Bounds the shape of a target, not component magnitude; prefer ParseTarget.
func ValidTarget(v string) bool { return targetRe.MatchString(v) }

// Validates and parses in one step; ValidTarget plus Parse misses int overflow.
func ParseTarget(v string) (Version, bool) {
	m := targetRe.FindStringSubmatch(v)
	// Fail closed rather than read the wrong captures if TargetPattern is edited.
	if len(m) != 4 {
		return Version{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, false
		}
		out[i] = n
	}
	return Version{Major: out[0], Minor: out[1], Patch: out[2]}, true
}

// For observed kubelet versions; untrusted input goes through ParseTarget.
func Parse(v string) (Version, bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return Version{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return Version{}, false
	}
	minor, ok := leadingInt(parts[1])
	if !ok {
		return Version{}, false
	}
	patch := 0
	if len(parts) == 3 {
		if p, ok := leadingInt(parts[2]); ok {
			patch = p
		}
	}
	return Version{Major: major, Minor: minor, Patch: patch}, true
}

func leadingInt(s string) (int, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}
