#!/usr/bin/env bash
# Runs every Phase 0 probe (or the ones named) and writes a Markdown report.
#
# Usage: ./run-all.sh               # all probes, SE as node A and DK as node B
#        ./run-all.sh p02 p05       # only these
#        NODE_A=de NODE_B=uk ./run-all.sh   # another pair; the probes' "SE"/"DK"
#                                          # labels then mean node A / node B
set -uo pipefail
cd "$(dirname "$0")"

# A running controller replicates and creates repositories and users on other
# nodes, which changes what the probes observe (e.g. "not on DK yet").
if curl -fsS -m 2 http://localhost:8090/healthz >/dev/null 2>&1 && [ -z "${PHASE0_ALLOW_CONTROLLER:-}" ]; then
  echo "The ForgeSync controller is running and would interfere with the probes. Stop it first:" >&2
  echo "  (cd deploy/test && docker compose --profile controller stop forgesync)" >&2
  echo "or set PHASE0_ALLOW_CONTROLLER=1 to run anyway." >&2
  exit 1
fi

export NODE_A=${NODE_A:-se} NODE_B=${NODE_B:-dk}
export RUN_ID=${RUN_ID:-$(date +%m%d%H%M%S)-$NODE_A-$NODE_B}
export RESULTS=results/run-$RUN_ID.tsv
REPORT=results/run-$RUN_ID.md
mkdir -p results

node_a_url() {
  case $NODE_A in se) echo http://forgejo-se.test:3001;; dk) echo http://forgejo-dk.test:3002;;
    de) echo http://forgejo-de.test:3003;; uk) echo http://forgejo-uk.test:3004;; us) echo http://forgejo-us.test:3005;; esac
}

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
  echo "Forgejo $(curl -fsS "$(node_a_url)/api/v1/version" | jq -r .version), generated $(date '+%Y-%m-%d %H:%M')."
  echo
  echo "Node A (called SE in the checks): **$NODE_A**. Node B (called DK in the checks): **$NODE_B**."
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
