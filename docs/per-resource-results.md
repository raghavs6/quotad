# Per-resource budgets: results

Run on 2026-10-22 with `bin/bench -proxy -searches -parallel 8`. The design is in [per-resource-budgets.md](per-resource-budgets.md). The setup is the same as in [step 3](step3-results.md), plus searches.

## What changed in the bench

- The fake server now has a **search** resource, like GitHub. Paths under `/search/` count against their own main budget: 3 searches per 1-second "minute" (GitHub's 30 a minute, scaled like the rest). Core keeps 500 per 60-second "hour". The in-flight limit and the points limit stay shared by the token.
- With `-searches`, agents mix searches into their task calls. Searches run at the same demand multiple of the search budget as core does of the core budget. So at 2×, the fleet wants twice the core budget **and** twice the search budget. That makes about a third of the task calls searches. (The design said 20%. Tying search to the same multiple was simpler to reason about.)
- "Utilization" is core's. "Search utilization" is the same number for search.

## Through the new quotad

Agents also search: 3 searches per 1s, at the same demand multiple. Utilization is core's.

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization | search utilization |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 1 [0–2] | 80% [0%–100%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 51% [50%–52%] | 60% [58%–61%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 1 [0–2] | 80% [0%–100%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 51% [50%–52%] | 61% [59%–62%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [5–8] | 100% [100%–100%] | 0.16 [0.10–0.39] | 13.11 [5.00–22.12] | 48 [21–73] | 98% [96%–100%] | 96% [95%–98%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [6–7] | 100% [100%–100%] | 0.10 [0.10–0.10] | 9.23 [3.09–15.71] | 49 [21–75] | 97% [95%–100%] | 96% [93%–98%] |
| 1.2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 11 [10–12] | 99% [98%–100%] | 6.31 [3.49–9.19] | 68.84 [57.50–94.49] | 391 [376–416] | 100% [99%–100%] | 98% [97%–100%] |
| 1.2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–11] | 100% [100%–100%] | 6.75 [4.11–9.76] | 50.98 [46.72–56.82] | 350 [309–387] | 98% [96%–100%] | 100% [99%–100%] |
| 2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 10 [9–11] | 94% [93%–96%] | 47.80 [46.00–51.91] | 110.54 [106.04–116.10] | 1866 [1840–1888] | 100% [100%–100%] | 96% [94%–98%] |
| 2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 12 [11–13] | 100% [100%–100%] | 42.92 [40.36–47.05] | 107.08 [97.36–114.54] | 1708 [1674–1731] | 100% [100%–100%] | 99% [99%–100%] |
| 5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 11 [9–12] | 84% [83%–86%] | 77.97 [77.46–78.76] | 150.24 [149.21–151.76] | 7017 [6989–7056] | 100% [100%–100%] | 94% [93%–96%] |
| 5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 13 [13–13] | 100% [99%–100%] | 73.10 [72.51–74.01] | 147.32 [144.15–150.47] | 6747 [6716–6771] | 100% [100%–100%] | 99% [99%–99%] |

## Through the old quotad (one budget per token), for comparison

Same bench, with quotad built from `main` before this change. Only the middle demand levels, to save time.

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization | search utilization |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1x | backoff | 292 [253–323] | 292 [253–323] | 0 [0–0] | 8 [7–11] | 100% [99%–100%] | 0.45 [0.10–0.95] | 31.36 [22.45–40.68] | 130 [93–169] | 95% [92%–97%] | 92% [89%–94%] |
| 1x | header-aware | 694 [448–940] | 694 [448–940] | 0 [0–0] | 9 [9–10] | 100% [99%–100%] | 0.34 [0.10–0.95] | 6.93 [2.86–10.38] | 47 [17–69] | 96% [93%–100%] | 96% [94%–98%] |
| 1.2x | backoff | 389 [367–411] | 389 [367–411] | 0 [0–0] | 8 [7–10] | 98% [95%–100%] | 9.26 [6.07–13.83] | 80.36 [66.79–97.69] | 469 [443–494] | 98% [97%–99%] | 94% [92%–95%] |
| 1.2x | header-aware | 1300 [1213–1382] | 1300 [1213–1382] | 0 [0–0] | 11 [11–12] | 100% [100%–100%] | 10.92 [9.74–12.69] | 41.94 [36.67–44.21] | 356 [326–380] | 98% [97%–100%] | 100% [99%–100%] |
| 2x | backoff | 333 [303–360] | 333 [303–360] | 0 [0–0] | 9 [8–10] | 86% [84%–89%] | 48.32 [44.25–51.39] | 126.71 [117.58–147.12] | 1954 [1907–1991] | 100% [100%–100%] | 95% [94%–97%] |
| 2x | header-aware | 1360 [1282–1406] | 1360 [1282–1406] | 0 [0–0] | 11 [11–12] | 100% [100%–100%] | 46.81 [45.11–49.45] | 96.49 [95.08–97.32] | 1741 [1719–1771] | 100% [99%–100%] | 100% [99%–100%] |

## Against the checks

| Check | Result |
|---|---|
| Unit tests: search at 0 is held after a core answer; core goes while search is at 0; each resource learns on its own; in-flight cap and 429 pause shared across resources; oldest request that can go goes first across lines; an answer updates the resource it names | Pass, with `go test -race ./...` |
| **0 rule breaks in every run with mixed traffic** | **Yes. 0 in all 50 runs.** The old quotad had 253 to 1,406 per run. |
| Core budget use at 90% or more at 1× and above | Yes. At least 95% in every run. |
| Core waits do not grow when search is used up | Not measured directly. The bench reports waits for all critical calls together, and searches are critical calls here. See below. |

## What the numbers say

**The gap is closed.** With mixed traffic, the old quotad sent hundreds of searches while search was at 0. Every one was a 403 on search. The new quotad sent none. It used 96–100% of the core budget and 93–100% of the search budget at 1× and above.

**Why the old quotad broke the rules.** Core's reset is about a minute away, and search's is about a second away. The old quotad kept one reset time. A core answer moved it a minute ahead, so it ignored search's "0 left" as an answer from an older window. Header-aware runs had more rule breaks than backoff runs. I have not checked why.

**Waits are longer than in step 3**, but this is not a fair comparison. The fleet now wants more calls in total (core plus search), and an agent waits for its search before its next call. At 1×, critical p95 is 9–13s here, against 0.10s in step 3 without searches. A search budget of 3 per second at exactly 1× demand often has more searches in one second than fit, and those wait for the next second or come back after quotad's 429. The bench can't yet split waits by resource. Step 4 (priorities) will need waits per call type anyway, so I suggest adding that there.

## Limits

- The 10 rare GitHub resources (for example `dependency_snapshots`) are guessed as core. If an answer names another resource, quotad updates that resource and logs a warning with the path.
- GraphQL gets its own budget, but quotad still reserves 1 per call. Real GraphQL costs are left for later.
- Not yet checked on the real API with mixed traffic. A short run on Raghav's laptop (core calls plus searches) would confirm it.
