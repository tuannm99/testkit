#!/bin/sh
# Phase 0 acceptance: on a host with only Docker, `up` then `down` three
# times in a row; after every `down` nothing of the project may remain.
# Uses ./tk (containerised CLI) so no Go toolchain is needed.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
PROJECT=${TESTKIT_PROJECT:-testkit}
NETWORK=${TESTKIT_NETWORK:-testkit_net}
SERVICES=${SERVICES:-order-worker}

leftovers() {
  c=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter "label=testkit.project=$PROJECT" | wc -l)
  c2=$(docker ps -aq --filter "label=testkit.project=$PROJECT" | wc -l)
  v=$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT" | wc -l)
  n=$(docker network ls -q --filter "name=^${NETWORK}\$" | wc -l)
  echo "containers=$((c + c2)) volumes=$v networks=$n"
}

./tk doctor
for i in 1 2 3; do
  echo "=== iteration $i: up"
  start=$(date +%s)
  ./tk up --services "$SERVICES"
  ./tk status
  echo "=== iteration $i: down (up took $(( $(date +%s) - start ))s)"
  ./tk down
  state=$(leftovers)
  echo "=== iteration $i: after down: $state"
  [ "$state" = "containers=0 volumes=0 networks=0" ] || { echo "FAIL: leftovers after iteration $i"; exit 1; }
done
echo "PHASE0 ACCEPTANCE: PASS (3/3 up+down cycles clean)"
