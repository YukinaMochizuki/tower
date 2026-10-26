#!/usr/bin/env bash
# Runs tests as the stress job (.github/workflows/stress.yml) does, on this
# machine: under the race detector, shuffled, COUNT times, on two CPUs, with
# quarantined tests included and CPU burners in the background. `make stress`
# runs it; PKG, COUNT and RUN are its variables.
#
# The burners load half the CPUs, the share stress-ng --cpu-load 50 takes in
# the job. Nothing outlives the script: on any exit the traps stop the
# burners and go test's whole process group (its test binaries included).
# Killed with SIGKILL, it runs no trap, so the burners also stop by
# themselves once it is gone, and a watcher then stops go test.
set -euo pipefail

pkgs=${PKG:-./...}
count=${COUNT:-20}
burners=${BURNERS:-$((($(nproc) + 1) / 2))}

# Job control puts each background job in a process group of its own, which
# a signal from the terminal does not reach; the traps stop them instead.
set -m
helper_pids=()
test_pid=
# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
	# SIGINT, as from a terminal: go test then waits for its test binaries.
	if [ -n "$test_pid" ]; then kill -INT -- "-$test_pid" 2>/dev/null || true; fi
	if [ ${#helper_pids[@]} -gt 0 ]; then kill "${helper_pids[@]}" 2>/dev/null || true; fi
	wait 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

self=$$
for ((i = 0; i < burners; i++)); do
	(
		trap - EXIT HUP INT TERM
		while kill -0 "$self" 2>/dev/null; do
			for ((j = 0; j < 200000; j++)); do :; done
		done
	) &
	helper_pids+=("$!")
done

# test/e2e builds under the e2e tag, so ./... leaves it out, as make test
# does. Named in PKG, its tests run one at a time under the race detector,
# as make test-e2e-race runs them ("Run in parallel" in
# docs/conventions/testing.md). The race detector is the Makefile's RACE.
args=(-race -gcflags=modernc.org/...=-race=false -shuffle=on -count="$count" -timeout 330m)
if [[ "$pkgs" == *test/e2e* ]]; then args+=(-tags e2e -parallel 1); fi
if [ -n "${RUN:-}" ]; then args+=(-run "$RUN"); fi

echo "stress: $burners CPU burners, GOMAXPROCS=2, -count=$count, $pkgs" >&2
# go test runs in the background so that a signal reaches the traps at
# once, not after it ends.
# shellcheck disable=SC2086
GOMAXPROCS=2 TOWER_RUN_QUARANTINED=1 go test "${args[@]}" $pkgs &
test_pid=$!
(
	trap - EXIT HUP INT TERM
	while kill -0 "$self" 2>/dev/null; do sleep 1; done
	kill -INT -- "-$test_pid" 2>/dev/null || true
) &
helper_pids+=("$!")
status=0
wait "$test_pid" || status=$?
test_pid=
exit "$status"
