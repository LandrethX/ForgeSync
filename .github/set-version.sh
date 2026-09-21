#!/usr/bin/env bash
# Writes a release version into the places the documentation names it.
#
#   .github/set-version.sh v1.2.3        # an example; give the real version
#
# Run it before tagging, so the tag carries instructions that point at
# itself. The release workflow also runs it after a tag, which catches the
# time you forget, at the cost of the tag naming the release before it.
#
# Every pattern below has to match exactly once. A pattern that matches
# nothing means the documentation moved and this script did not, which is
# how a version-bumping script quietly stops bumping anything; it is a
# failure here rather than a surprise later.
set -Eeuo pipefail
cd "$(dirname "$0")/.."

V=${1:-}
case "$V" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: $0 vMAJOR.MINOR.PATCH" >&2; exit 2 ;;
esac

# file : a sed address matching the one line : what that line becomes
rewrite() { # rewrite <file> <match> <replacement>
  local file=$1 match=$2 replace=$3 n
  n=$(grep -cE "$match" "$file" || true)
  [ "$n" = 1 ] || {
    echo "$file: expected one line matching /$match/, found $n" >&2
    echo "  the documentation has moved; fix this script rather than the count" >&2
    exit 1
  }
  # The replacement is written whole, so nothing depends on the old version.
  python3 - "$file" "$match" "$replace" <<'PY'
import re, sys
path, match, replace = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(path).read().split("\n")
out = [replace if re.search(match, line) else line for line in lines]
open(path, "w").write("\n".join(out))
PY
}

R=https://github.com/LandrethX/ForgeSync

rewrite README.md \
  '^Current release: \*\*\[v[0-9]' \
  "Current release: **[$V]($R/releases/tag/$V)**."

rewrite README.md \
  '^curl -fsSLO https://raw\.githubusercontent\.com/LandrethX/ForgeSync/v[0-9]' \
  "curl -fsSLO https://raw.githubusercontent.com/LandrethX/ForgeSync/$V/deploy/prod/install.sh"

rewrite README.md \
  '^bash install\.sh --first --binary --ref v[0-9]' \
  "bash install.sh --first --binary --ref $V"

rewrite README.md \
  '^The current one is \*\*v[0-9]' \
  "The current one is **$V**, and it is not a 1.0 on purpose: everything in it has been"

rewrite deploy/prod/README.md \
  '^V=v[0-9]' \
  "V=$V                                   # the current release"

echo "documentation now names $V"
