#!/usr/bin/env bash
# Runs every Phase 0 probe (or the ones named) and writes a Markdown report.
#
# Usage: ./run-all.sh               # all probes
#        ./run-all.sh p02 p05       # only these
set -uo pipefail
cd "$(dirname "$0")"

export RUN_ID=${RUN_ID:-$(date +%m%d%H%M%S)}
export RESULTS=results/run-$RUN_ID.tsv
REPORT=results/run-$RUN_ID.md
mkdir -p results

probes=$(ls p[0-9][0-9]-*.sh)
if [ $# -gt 0 ]; then
  probes=$(for p in "$@"; do ls "$p"-*.sh; done)
fi

for p in $probes; do
  echo
  if ! PROBE=$(basename "$p" .sh) bash "$p"; then
    printf '%s\t%s\t%s\t%s\t%s\n' "$RUN_ID" "$(basename "$p" .sh)" ERROR "probe stopped early" "later checks in this probe did not run" >> "$RESULTS"
  fi
done

{
  echo "# ForgeSync Phase 0 results, run $RUN_ID"
  echo
  echo "Forgejo $(curl -fsS http://forgejo-se.test:3001/api/v1/version | jq -r .version), generated $(date '+%Y-%m-%d %H:%M')."
  echo
  echo "CONFIRMED / REFUTED: whether the stated hypothesis held. INFO: an observed fact. ERROR: the probe itself failed."
  echo
  awk -F'\t' '{print $3}' "$RESULTS" | sort | uniq -c | awk '{printf "- %s: %s\n", $2, $1}'
  current=""
  while IFS=$'\t' read -r _ probe verdict check detail; do
    if [ "$probe" != "$current" ]; then
      printf '\n## %s\n\n| Verdict | Check | Detail |\n|---|---|---|\n' "$probe"
      current=$probe
    fi
    printf '| %s | %s | %s |\n' "$verdict" "${check//|/\\|}" "${detail//|/\\|}"
  done < "$RESULTS"
} > "$REPORT"

echo
echo "Report: phase0/$REPORT"
awk -F'\t' '{print $3}' "$RESULTS" | sort | uniq -c
