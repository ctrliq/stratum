package cluster

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

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

func TestLoadConfigRefusesASnapshotIntervalHoursThatOverflowsItsDuration(t *testing.T) {
	// Both values wrap to positive durations, so the bound must refuse them too.
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

func TestLoadConfigSnapshotRetain(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{name: "an empty spec keeps the default", yaml: "spec: {}\n", want: version.SnapshotRetainDefault},
		{name: "an explicit zero keeps the default", yaml: "spec:\n  snapshots:\n    retain: 0\n", want: version.SnapshotRetainDefault},
		{name: "an explicit value passes through", yaml: "spec:\n  snapshots:\n    retain: 3\n", want: 3},
		{name: "the smallest retention passes through", yaml: "spec:\n  snapshots:\n    retain: 1\n", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig = %v, want nil", err)
			}
			if got := c.Spec.Snapshot.Retain; got != tc.want {
				t.Errorf("spec.snapshots.retain = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLoadConfigRefusesANegativeSnapshotRetain(t *testing.T) {
	// t.TempDir() puts the test name in the path the error quotes, so the name must not hold the value.
	for _, tc := range []struct {
		name string
		val  string
	}{
		{name: "minus one", val: "-1"},
		{name: "minus a hundred", val: "-100"},
	} {
		val := tc.val
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("spec:\n  snapshots:\n    retain: "+val+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "spec.snapshots.retain") || !strings.Contains(err.Error(), val) {
				t.Errorf("LoadConfig(retain %s) = %v, want an error naming the field and value", val, err)
			}
		})
	}
}
