package cluster

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// spec.snapshots.intervalHours is what the scheduled snapshot loop turns
// straight into its ticker's interval. Unset — a missing config file, an empty
// spec, or an explicit zero — keeps the built-in default a controller runs
// (version.SnapshotIntervalHours), and an explicit value passes through as it
// is, down to the smallest whole-hour interval the loop accepts. The
// zero-keeps-default half of this is the part of the interval contract that
// must survive run-group ownership; whether a negative value is refused is that
// change's own acceptance test and is deliberately not pinned here.
func TestLoadConfigSnapshotIntervalHours(t *testing.T) {
	for _, tc := range []struct {
		name   string
		yaml   string
		absent bool
		want   int
	}{
		{name: "a missing file is pure defaults", absent: true, want: version.SnapshotIntervalHours},
		{name: "an empty spec keeps the default", yaml: "spec: {}\n", want: version.SnapshotIntervalHours},
		{name: "an explicit zero keeps the default", yaml: "spec:\n  snapshots:\n    intervalHours: 0\n", want: version.SnapshotIntervalHours},
		{name: "an explicit value passes through", yaml: "spec:\n  snapshots:\n    intervalHours: 24\n", want: 24},
		{name: "the smallest interval passes through", yaml: "spec:\n  snapshots:\n    intervalHours: 1\n", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if !tc.absent {
				if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig = %v, want nil", err)
			}
			if got := c.Spec.Snapshot.IntervalHours; got != tc.want {
				t.Errorf("spec.snapshots.intervalHours = %d, want %d", got, tc.want)
			}
		})
	}
}

// A negative spec.snapshots.intervalHours is a config error, refused at
// load like every other bad value in this file, not a value passed
// through for the scheduled snapshot loop to choke on: the loop builds
// its ticker's interval straight from the field, and time.NewTicker
// panics on a non-positive one, taking the whole `stratum up` process
// down with it. The error names the field and the value, as the vip
// refusal does. Zero is not refused — it keeps the default pinned above.
func TestLoadConfigRefusesANegativeSnapshotIntervalHours(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval int
	}{
		{name: "minus one hour", interval: -1},
		{name: "minus a hundred hours", interval: -100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			val := strconv.Itoa(tc.interval)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("spec:\n  snapshots:\n    intervalHours: "+val+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "spec.snapshots.intervalHours") || !strings.Contains(err.Error(), val) {
				t.Errorf("LoadConfig(intervalHours %s) = %v, want an error naming the field and value", val, err)
			}
		})
	}
}

// A spec.snapshots.intervalHours whose time.Duration product overflows is
// the same refusal as the negative one, reached a different way: the
// snapshot loop's ticker interval is time.Duration(hours) * time.Hour, and
// hours in the millions wrap that product to a non-positive duration —
// the exact value time.NewTicker panics on. A refusal that checked only
// the sign of the field would let ten million hours through, and the run
// group would turn the panic into a restart loop instead of the load-time
// error the negative refusal gives.
func TestLoadConfigRefusesASnapshotIntervalHoursThatOverflowsItsDuration(t *testing.T) {
	// Both wrap-to-positive values were accepted when the check was the
	// product's sign: 5124096 hours wraps to about 25 minutes and
	// 2251799813685249 wraps to exactly one hour, both positive, so a sign
	// check passed them to the snapshot ticker whole. The bound must refuse
	// every value the product cannot represent, whatever sign it wraps to.
	for _, val := range []string{"10000000", "5124096", "2251799813685249"} {
		t.Run(val, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("spec:\n  snapshots:\n    intervalHours: "+val+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "spec.snapshots.intervalHours") || !strings.Contains(err.Error(), val) {
				t.Errorf("LoadConfig(intervalHours %s) = %v, want an error naming the field and value", val, err)
			}
		})
	}
}
