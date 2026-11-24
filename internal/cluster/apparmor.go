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

// Vars so a test can shorten, answer for and read the loop's seams.
var (
	appArmorProfileInterval           = time.Minute
	watchAppArmorPolicy               = apparmor.WatchPolicy
	appArmorSettle                    = 100 * time.Millisecond
	reconcileAppArmor                 = apparmor.Reconcile
	appArmorLog             io.Writer = os.Stderr
)

func appArmorProfileLoop(ctx context.Context) {
	t := time.NewTicker(appArmorProfileInterval)
	defer t.Stop()
	changes := watchAppArmorPolicy(ctx)
	var failing string
	backedOff := false
	look := func() {
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
	// The watcher counts only changes after it starts, so look once now.
	look()
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
		look()
	}
}
