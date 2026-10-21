# Step 2 results: the first proxy

Run on 2026-10-20 with `bin/bench -proxy -parallel 8`. The setup is the same as the [step 1 baselines](step1-baselines.md) (10 agents, 5 also polling, 500 requests per 60s "hour", 10 in flight, 3 windows per run, 5 runs per row), with quotad between the agents and the fake server (max hold 500ms, reset guard 0). Violations are still counted by the fake server, so these numbers compare directly with the baselines. The agents are unchanged: each still runs its own retry strategy, now against quotad.

## Results

Through quotad (max hold 500ms, reset guard 0). 10 agents (5 polling), budget 500 per 1m0s, 10 in flight, 100ms latency, 3 windows per run, 5 runs per row. Cells are mean [min–max].

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 3 [0–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 4 [0–8] | 100% [99%–100%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 2 [0–7] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] |
| 1.2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 12 [10–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.23 [16.60–17.60] | 135 [128–140] | 100% [100%–100%] |
| 1.2x | header-aware | 392 [259–496] | 0 [0–0] | 435 [287–550] | 11 [11–11] | 4% [2%–6%] | 0.55 [0.10–2.31] | 16.91 [15.16–19.10] | 145 [136–157] | 100% [100%–100%] |
| 2x | backoff | 10 [0–22] | 0 [0–0] | 11 [0–24] | 14 [13–15] | 0% [0%–0%] | 24.49 [22.67–25.80] | 93.00 [89.68–95.42] | 853 [817–907] | 100% [100%–100%] |
| 2x | header-aware | 924 [885–966] | 0 [0–0] | 1016 [972–1062] | 12 [12–12] | 3% [2%–3%] | 40.03 [37.64–43.00] | 84.23 [82.28–85.91] | 1072 [1045–1097] | 100% [100%–100%] |
| 5x | backoff | 46 [0–140] | 0 [0–0] | 50 [0–152] | 15 [14–15] | 0% [0%–0%] | 57.87 [54.19–60.62] | 147.40 [140.55–150.39] | 4023 [3924–4078] | 100% [100%–100%] |
| 5x | header-aware | 1151 [936–1450] | 0 [0–0] | 1266 [1030–1594] | 12 [12–12] | 3% [2%–4%] | 71.49 [70.14–72.05] | 136.67 [136.54–137.02] | 4252 [4226–4269] | 100% [100%–100%] |

## Against the step 2 checks

| Check | Result |
|---|---|
| Unit tests for `internal/budget` (learn first, holding at `remaining − reserved = 0`, stale responses, release order at reset, turning away waits past max hold, client giving up, failed requests) | Pass, with `go test -race ./...` |
| **0 primary 403s in every run at every demand** | **Yes: 0 in all 50 runs.** Baselines had 8–202 per run at 1× and above. |
| Utilization ≥ 90% at 1× and above | Yes: at least 99% in every run. |
| 429 violations still above 0 (step 3's job) | Yes, for header-aware agents at every level from 1.2× up, and for backoff agents at 2× and 5×. |

## What the numbers say

**The primary budget is solved.** No request reached the fake server after its budget was gone, at any load. quotad held calls or answered with its own 429, and those never touch the upstream.

**For backoff agents, quotad removes almost every violation.** At 1× and 1.2×, violations went from 8 and 169 to **0 in every run**. At 2× and 5×, a few 429s are left (10 and 46 on average, against 218 and 163 before).

**For header-aware agents, violations got worse, as step 3 expects.** At 2×, violations went from 802 [745–842] to 924 [885–966], and the ranges don't overlap. All of them are 429s. quotad's own 429 tells every turned-away agent to retry at the same moment, the reset. Header-aware agents obey it, wake together, and the burst trips the upstream's concurrency limit (10 in flight). That's the "burst right after a reset" failure case from the step 2 design, and it's what step 3 (concurrency and shared `retry-after`) is for.

**Critical waits didn't change, as expected.** At 1.2×, critical p95 is 17.23s for backoff and 16.91s for header-aware, against 17.43s and 14.96s in the baselines. Nothing in step 2 puts critical calls first. That's step 4, and the target there is still about 7.5s.
