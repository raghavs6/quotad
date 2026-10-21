# Step 3 results: secondary limits

Run on 2026-10-21 with `bin/bench -proxy -parallel 8`. The setup is the same as in [step 2](step2-results.md). quotad now also gets the fake server's secondary limits: 10 requests in flight, 90 points per second (GitHub's 900 per minute, scaled), and a 1-second "minute" for pauses.

## Results

Through quotad (max hold 500ms, reset guard 0, 10 in flight, 90 points per 1s). 10 agents (5 polling), budget 500 per 1m0s, 10 in flight, 100ms latency, 3 windows per run, 5 runs per row. Cells are mean [min–max].

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 3 [0–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 4 [0–8] | 100% [99%–100%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 2 [0–7] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] |
| 1.2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 12 [10–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.23 [16.60–17.60] | 135 [128–140] | 100% [100%–100%] |
| 1.2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 8 [7–9] | 0% [0%–0%] | 0.10 [0.10–0.10] | 15.41 [14.68–15.99] | 156 [154–160] | 100% [100%–100%] |
| 2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 14 [12–15] | 0% [0%–0%] | 24.73 [22.56–26.39] | 91.34 [79.77–97.13] | 852 [811–907] | 100% [100%–100%] |
| 2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–9] | 0% [0%–0%] | 23.85 [23.19–24.07] | 62.45 [61.66–63.27] | 838 [821–851] | 100% [100%–100%] |
| 5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 15 [14–15] | 0% [0%–0%] | 58.15 [54.14–61.14] | 146.41 [141.92–150.11] | 4024 [3970–4088] | 100% [100%–100%] |
| 5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–10] | 3% [0%–13%] | 63.64 [63.40–63.83] | 130.10 [129.75–130.33] | 4067 [4046–4089] | 100% [100%–100%] |

## Against the step 3 checks

| Check | Result |
|---|---|
| Unit tests for the in-flight limit, the points limit, the shared pause, the doubling pause, and the first-come line | Pass, with `go test -race ./...` |
| **0 rule breaks in every run, at every load level, for both agent types** | **Yes. 0 in all 50 runs.** |
| Main budget use at 90% or more at 1× load and above | Yes. At least 99% in every run. |

## What the numbers say

**Every rule break is gone.** No request reached the fake server while its key was limited, in any run. Before quotad, there were up to 802 rule breaks per run (header-aware agents at 2×). After step 2, there were up to 1,151.

**The rush after a reset now waits inside quotad.** All agents still come back at the same moment. But quotad lets them in only as fast as the limits allow. So the rush never reaches the server.

**Header-aware agents now wait less at high load.** At 2× load, their critical p95 wait fell from 84 seconds (after step 2) to 62 seconds. The reason: they no longer lose time to 429 storms.

**At 1.2× load, critical waits did not change.** The critical p95 is 15.41 seconds for header-aware agents and 17.23 seconds for backoff agents. The target is about 7.5 seconds. Nothing in step 3 sends important calls first. That is step 4.
