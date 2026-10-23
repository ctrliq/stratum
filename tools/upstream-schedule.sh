#!/usr/bin/env bash
# Upcoming upstream Kubernetes releases.
#
# Patch releases are scheduled a month ahead and published as structured data.
# Reading it turns "a bump PR appeared" into "a bump PR is due Tuesday", which is
# the difference between a release captain planning and a release captain
# reacting.
#
# Usage:
#   tools/upstream-schedule.sh              upcoming dates
#   tools/upstream-schedule.sh --warn       exit 10 if something lands within 7d
set -euo pipefail
cd "$(dirname "$0")/.."

MODE=${1:---list}
export MODE REPO_ROOT="$PWD"
exec python3 - <<'PY'
import datetime, os, re, sys
sys.path.insert(0, os.path.join(os.environ["REPO_ROOT"], "tools"))
from upstream_sources import *

# kubernetes/website publishes the patch schedule as YAML data, which is the
# machine-readable source behind the human page at
# https://kubernetes.io/releases/patch-releases/
SCHEDULE = f"{RAW}/kubernetes/website/main/data/releases/schedule.yaml"

def run():
    raw = fetch(SCHEDULE)
    if raw is None:
        die("cannot read the upstream release schedule")

    today = datetime.date.today()
    supported = set(supported_minors())

    def field(block, name, indent=2):
        # Indent-aware: the minor lives at "  release:" while the next patch
        # lives at "    release:" inside the next: block, and matching \s* would
        # confuse the two.
        m = re.search(rf"^ {{{indent}}}{name}:\s*[\"']?([^\"'\n]+)", block, re.M)
        return m.group(1).strip() if m else None

    rows, stale = [], []
    # One block per supported minor. A small hand parser rather than a YAML
    # dependency: this script must run on a bare runner with nothing installed,
    # and the file's shape is stable enough that a failure here is loud.
    for block in re.split(r"\n(?=- endOfLifeDate:)", raw):
        minor = field(block, "release")
        if not minor or "." not in minor or minor.count(".") != 1:
            continue
        tracked = minor in supported
        nxt = re.search(r"next:\n((?:\s{4}.*\n)+)", block)
        if nxt:
            n = nxt.group(1)
            target = field(n, "targetDate", indent=4)
            patch = field(n, "release", indent=4)
            deadline = field(n, "cherryPickDeadline", indent=4)
            if target:
                d = datetime.date.fromisoformat(target)
                (rows if d >= today else stale).append(
                    (d, minor, f"v{patch} released", deadline, tracked))
                if deadline:
                    dd = datetime.date.fromisoformat(deadline)
                    if dd >= today:
                        rows.append((dd, minor, f"v{patch} cherry-pick deadline", None, tracked))
        for name, label in (("endOfLifeDate", "END OF LIFE"),
                            ("maintenanceModeStartDate", "maintenance mode")):
            v = field(block, name)
            if v and datetime.date.fromisoformat(v) >= today:
                rows.append((datetime.date.fromisoformat(v), minor, label, None, tracked))

    rows.sort()
    soon = False
    if rows:
        print(f"{'date':12} {'in':>6}  {'minor':7} {'event':32} tracked")
        for when, minor, event, _deadline, tracked in rows[:20]:
            days = (when - today).days
            if tracked and days <= 7:
                soon = True
            print(f"{when.isoformat():12} {str(days) + 'd':>6}  {minor:7} {event:32} "
                  f"{'yes' if tracked else 'no'}")

    # A stale schedule is itself the signal: upstream has not yet published the
    # next patch round, so nobody should be planning around a date.
    for when, minor, event, _d, tracked in sorted(stale):
        if tracked:
            print(f"note: upstream's published 'next' for {minor} ({event}, {when}) "
                  "is in the past — the schedule has not been refreshed yet")

    if not rows and not stale:
        print("upstream-schedule: no dates found; the schedule format may have changed")
        sys.exit(1)

    if os.environ["MODE"] == "--warn" and soon:
        print("\nupstream-schedule: a tracked release lands within 7 days — "
              "expect bump PRs and have a release captain ready")
        sys.exit(10)

main(run)
PY
