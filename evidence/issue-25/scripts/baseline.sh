#!/bin/sh
# usage: baseline.sh <package dir> <workdir>
cd "$1" || exit 1
for t in TestCLIGateHeldAndReleaseAndServeAutoRelease TestServeRunsAPIAndRuntimeWithGracefulShutdown TestServeWiringWithWebhookNotifier TestServeDefaultTypedDecisionEvaluatorWithTypedCriteria; do
  for v in base:300 base.race:150; do
    b=${v%:*}; n=${v#*:}
    log="$2/logs/${b}_$t.log"
    "$2/bin/$b.test" -test.run "^$t\$" -test.count="$n" -test.v -test.timeout=30m >"$log" 2>&1
    rc=$?
    echo "$b $t count=$n rc=$rc pass=$(grep -c '^--- PASS' "$log") fail=$(grep -c '^--- FAIL' "$log") race=$(grep -c 'DATA RACE' "$log")"
  done
done
uptime
