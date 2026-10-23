#!/usr/bin/env bash
# Days behind upstream, per branch.
#
# The evidence that we track upstream: for each supported minor, the gap
# between the newest upstream patch release and what we pin.
#
# Usage: tools/days-behind.sh [--json]
set -euo pipefail
cd "$(dirname "$0")/.."

FORMAT=${1:---text}
export FORMAT REPO_ROOT="$PWD"
exec python3 - <<'PY'
import datetime, json, os, subprocess, sys
sys.path.insert(0, os.path.join(os.environ["REPO_ROOT"], "tools"))
from upstream_sources import *

def run():
    today = datetime.date.today()
    rows = []
    for minor in supported_minors():
        branch = f"release-{minor}"
        # What that branch pins. main is the exception: it tracks the next minor.
        try:
            mk = subprocess.run(["git", "show", f"{branch}:build/versions.mk"],
                                capture_output=True, text=True, check=True).stdout
        except subprocess.CalledProcessError:
            mk = open("build/versions.mk").read()
            branch += " (local, branch not found)"
        pinned = next(l.split("=")[1].strip() for l in mk.splitlines()
                      if l.startswith("kubernetes_version"))
        newest = latest_patch(minor)

        pinned_date = tag_release_date("kubernetes/kubernetes", f"v{pinned}")
        newest_date = tag_release_date("kubernetes/kubernetes", f"v{newest}")
        behind = None
        if newest_date:
            behind = (today - datetime.date.fromisoformat(newest_date)).days if pinned == newest \
                     else (datetime.date.fromisoformat(newest_date) - datetime.date.fromisoformat(pinned_date)).days
        rows.append({
            "branch": branch, "pinned": pinned, "upstream_latest": newest,
            "pinned_released": pinned_date, "upstream_released": newest_date,
            "up_to_date": pinned == newest,
            # For an up-to-date branch this is the age of the pin, which is the
            # honest number: "we are current, and current is N days old".
            "days": behind,
        })

    if os.environ["FORMAT"] == "--json":
        print(json.dumps({"generated": today.isoformat(), "branches": rows}, indent=2))
        return

    print(f"{'branch':22} {'pinned':10} {'upstream':10} {'status'}")
    for r in rows:
        status = (f"up to date (pin is {r['days']}d old)" if r["up_to_date"]
                  else f"BEHIND by {r['days']} days")
        print(f"{r['branch']:22} {r['pinned']:10} {r['upstream_latest']:10} {status}")

main(run)
PY
