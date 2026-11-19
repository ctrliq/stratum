package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ctrliq/stratum/internal/apparmor"
)

// The AppArmor runc profile loop's seams: how often it looks, what tells it
// to look sooner, how long it lets a policy change settle, what it does each
// time, and where it logs. Vars so a test can shorten, answer for and read
// them.
var (
	appArmorProfileInterval           = time.Minute
	watchAppArmorPolicy               = apparmor.WatchPolicy
	appArmorSettle                    = 100 * time.Millisecond
	reconcileAppArmor                 = apparmor.Reconcile
	appArmorLog             io.Writer = os.Stderr
)

// appArmorProfileLoop keeps the kernel's runc profile covering Stratum's runc
// while the node runs. The host's AppArmor replaces it with the host's own
// whenever it reloads /etc/apparmor.d, and every container created after that
// is stacked for good. So it looks as soon as the kernel's policy changes,
// once the change has settled, as a reload of every profile changes it many
// times, and every interval besides. A look loads only when runc is not
// covered, so its own load does not make the next one load again; but a load
// that leaves runc uncovered, as when another profile attaches to it first,
// would, so after one the loop waits for the interval. A failure is logged
// when it first happens and when it changes, not on every look.
func appArmorProfileLoop(ctx context.Context) {
	t := time.NewTicker(appArmorProfileInterval)
	defer t.Stop()
	changes := watchAppArmorPolicy(ctx)
	var failing string
	backedOff := false
	for {
		wake := changes
		if backedOff {
			wake = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(appArmorSettle):
			}
		}
		loaded, err := reconcileAppArmor(ctx)
		backedOff = loaded && errors.Is(err, apparmor.ErrNotCovered)
		switch {
		case err != nil:
			if err.Error() != failing {
				failing = err.Error()
				fmt.Fprintf(appArmorLog, "stratum: WARN: %v\n", err)
			}
		case loaded:
			failing = ""
			fmt.Fprintln(appArmorLog, "stratum: the kernel's runc AppArmor profile no longer covered Stratum's runc; loaded it again")
		case failing != "":
			failing = ""
			fmt.Fprintln(appArmorLog, "stratum: the kernel's runc AppArmor profile covers Stratum's runc again")
		}
	}
}
