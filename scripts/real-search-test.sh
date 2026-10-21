#!/bin/sh
# Runs the real GitHub test: agents sharing one token call the search API
# (30 calls a minute), directly and through quotad, and count GitHub's
# rate-limit answers. Read only: every call searches raghavs6/quotad.
#
# 2 strategies x (direct, quotad) x 2 runs of 2 minutes, with a 65s gap so
# each run starts in a fresh search window. About 25 minutes in all, at
# about one call a second.
#
# Needs Go and the gh CLI logged in (or GITHUB_TOKEN set).
# Output: results/real-search.txt
set -eu
cd "$(dirname "$0")/.."
go build -o bin/ ./cmd/...
GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}"
export GITHUB_TOKEN
mkdir -p results
out=results/real-search.txt
: > "$out"

for run in 1 2; do
	for strategy in backoff header-aware; do
		for via in direct quotad; do
			target=https://api.github.com
			if [ "$via" = quotad ]; then
				# A fresh quotad per run, so no run inherits another's state.
				bin/quotad -listen 127.0.0.1:8090 2>>results/real-quotad.log &
				qpid=$!
				sleep 1
				target=http://127.0.0.1:8090
			fi
			echo "== run $run, $strategy, $via ($(date -u +%H:%M:%S) UTC)" | tee -a "$out"
			bin/realtest -target "$target" -strategy "$strategy" -seed "$run" | tee -a "$out"
			if [ "$via" = quotad ]; then
				kill "$qpid"
			fi
			sleep 65
		done
	done
done
echo "Done. Results are in $out"
