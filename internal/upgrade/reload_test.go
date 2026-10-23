package upgrade

import (
	"io"

	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/supervisor"
)

func TestHostReloadResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		fatal bool
	}{
		{"success", nil, false},
		{"old supervisor", fmt.Errorf("reload containerd: %w", supervisor.ErrReloadUnsupported), false},
		{"old PID still running after timeout", errors.New("containerd did not come back within 1m30s of reload"), true},
		{"status failure", errors.New("status unavailable"), true},
		{"transport failure", errors.New("connection reset"), true},
		{"other rejection", errors.New("reload rejected"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := hostReloadResult(io.Discard, tc.err)
			if tc.fatal {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want cause %v preserved", err, tc.err)
				}
				if !strings.Contains(err.Error(), "new version remains installed; no rollback attempted") {
					t.Fatalf("missing recovery context: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
