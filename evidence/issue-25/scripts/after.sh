#!/bin/sh
# usage: after.sh <repo> <workdir>
cd "$1" || exit 1
uptime
for t in TestCLIGateHeldAndReleaseAndServeAutoRelease TestServeRunsAPIAndRuntimeWithGracefulShutdown TestServeWiringWithWebhookNotifier TestServeDefaultTypedDecisionEvaluatorWithTypedCriteria; do
  for v in norace:0: race:1:-race; do
    name=${v%%:*}; rest=${v#*:}; cgo=${rest%%:*}; flag=${rest#*:}
    log="$2/logs/after_${name}_$t.log"
    CGO_ENABLED=$cgo /usr/local/go/bin/go test $flag -run "^$t\$" -count=300 -timeout=30m -v ./cmd/quota-fleet >"$log" 2>&1
    rc=$?
    max=$(sed -nE 's/^--- (PASS|FAIL): .* \(([0-9.]+)s\)$/\2/p' "$log" | sort -g | tail -1)
    echo "$name $t count=300 rc=$rc pass=$(grep -c '^--- PASS' "$log") fail=$(grep -c '^--- FAIL' "$log") race=$(grep -c 'DATA RACE' "$log") max=${max}s"
  done
done
uptime
