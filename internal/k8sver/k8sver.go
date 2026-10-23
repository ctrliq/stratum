// Package k8sver parses, validates and orders Kubernetes version strings.
//
// It draws a deliberate line between two classes of input:
//
//   - A REQUESTED TARGET is untrusted. It arrives from a CLI flag or from
//     spec.targetKubernetesVersion on a ClusterUpgrade, and it goes on to be
//     written into node annotations and command arguments. ParseTarget holds it
//     to an exact, fully-anchored vMAJOR.MINOR.PATCH.
//
//   - An OBSERVED VERSION is reported by a kubelet and is not attacker-chosen.
//     Real ones legitimately carry distribution suffixes — v1.31.2+k3s1,
//     v1.28.3-eks-a1b2c3, v1.36.2+build. Parse accepts those.
//
// Holding observed versions to ParseTarget would be actively harmful, not
// merely strict: callers skip their downgrade and version-skew gates when no
// node version parses, so a strict parser here would make those clusters fail
// OPEN. Parse must stay lenient for that reason.
//
// It lives in internal/ rather than pkg/: the operator is in this binary, so
// there is no cross-module consumer and nothing to export. Still stdlib-only,
// which costs nothing and keeps a parsing helper from acquiring dependencies.
package k8sver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TargetPattern is the anchored form a requested target must match. It is
// exported so the ClusterUpgrade CRD's OpenAPI `pattern:` can be asserted
// byte-identical to what the Go validator enforces, keeping admission-time and
// controller-time validation from drifting apart.
//
// Anchoring is load-bearing. OpenAPI `pattern:` is a search rather than a full
// match, so an unanchored form would match happily INSIDE "v1.36.2; curl ...".
// Neither Go's RE2 nor ECMA-262 lets an unflagged `$` match before a trailing
// newline, so the anchors also reject "v1.36.2\nrm -rf /".
//
// Leading zeros are rejected so v01.36.2 cannot alias v1.36.2.
const TargetPattern = `^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`

var targetRe = regexp.MustCompile(TargetPattern)

// Version is a major.minor.patch triple. The zero value is 0.0.0.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare orders two versions, returning -1, 0 or +1. All three components
// participate: comparing on major and minor alone would treat v1.36.1 and
// v1.36.2 as equivalent and let a patch downgrade through a downgrade gate.
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

// Less reports whether v orders before o.
func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

// ValidTarget reports whether v is shaped like an acceptable upgrade target:
// exactly vMAJOR.MINOR.PATCH, no prefix, no suffix, no build metadata, no
// leading zeros.
//
// It bounds the SHAPE of a target but not the magnitude of its components, so a
// value with a 20-digit component satisfies it while overflowing an int. Prefer
// ParseTarget, which reports both. This exists for callers that want the
// predicate alone, and to keep the pattern and the parser testable separately.
func ValidTarget(v string) bool { return targetRe.MatchString(v) }

// ParseTarget validates AND parses a requested target in one step. ok is false
// unless v is exact vMAJOR.MINOR.PATCH and every component fits in an int.
//
// Use this rather than pairing ValidTarget with Parse. The pattern bounds digit
// shape, not digit count, so "v99999999999999999999.1.1" passes ValidTarget and
// then overflows strconv.Atoi — a caller that ignored the parse result would
// silently compare against 0.0.0.
func ParseTarget(v string) (Version, bool) {
	m := targetRe.FindStringSubmatch(v)
	// len(m) != 4 means TargetPattern's capture groups were edited without
	// updating this function. Fail closed rather than silently reading the
	// wrong captures (or panicking on m[3]).
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

// Parse extracts a Version from an OBSERVED version string such as a kubelet's
// status.nodeInfo.kubeletVersion. It tolerates a missing "v", a missing patch,
// and any distribution suffix ("+k3s1", "-eks-a1b2c3", "+build"), because those
// are all real values a healthy node reports. A missing or unreadable patch
// yields 0.
//
// It reports ok=false only when there is no usable MAJOR.MINOR to read. Do not
// use Parse to validate user input — use ParseTarget for that. (Not ValidTarget
// followed by Parse: the pattern bounds digit shape but not digit count, so
// pairing them is exactly the overflow gap ParseTarget exists to close.)
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
	patch := 0 // absent or unreadable patch ("1.36", "v1.36.x") reads as .0
	if len(parts) == 3 {
		if p, ok := leadingInt(parts[2]); ok {
			patch = p
		}
	}
	return Version{Major: major, Minor: minor, Patch: patch}, true
}

// leadingInt reads the leading digit run of s ("36+k3s1" -> 36). ok is false
// when s does not start with a digit, or when the run overflows an int.
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
