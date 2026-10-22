# Redis results: many copies, one budget

Run on 2026-10-22 with `bin/bench -proxies 3 -parallel 6`, once without Redis and once with `-redis` (Redis 7.0 on the same machine). The design is in [redis-shared-budgets.md](redis-shared-budgets.md).

## The setup

Each run starts 3 quotad copies, like 3 machines, in front of one fake server. The 10 agents are spread across the copies (agent 1 to copy 1, agent 2 to copy 2, and so on). All agents use one token. Each copy is told the token's full limits: 500 per 60s "hour", 10 in flight, 90 points per 1s "minute". That is what each machine would be told in real life, because each machine sees the same headers.

- **Alone:** each copy keeps its counts in its own memory, as quotad did before this change.
- **Redis:** the 3 copies share their counts through one Redis.

Everything else is the same as in [step 3](step3-results.md): 5 load levels, 2 agent types, 5 runs per row, 3 windows per run, a fresh token per run.

## Results

### 3 copies, each counting alone

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 3 [1–6] | 3 [1–6] | 0 [0–0] | 4 [1–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 5 [0–8] | 100% [99%–100%] |
| 1x | header-aware | 3 [1–5] | 3 [1–5] | 0 [0–0] | 3 [1–7] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] |
| 1.2x | backoff | 6 [6–7] | 6 [6–7] | 0 [0–0] | 12 [10–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.24 [16.71–17.60] | 134 [126–143] | 100% [100%–100%] |
| 1.2x | header-aware | 123 [97–174] | 8 [6–12] | 157 [126–205] | 11 [11–12] | 3% [2%–4%] | 0.10 [0.10–0.10] | 16.51 [14.17–18.10] | 143 [124–149] | 100% [100%–100%] |
| 2x | backoff | 12 [7–21] | 8 [7–9] | 6 [0–18] | 14 [12–15] | 0% [0%–0%] | 25.42 [23.11–29.15] | 91.68 [81.65–95.67] | 854 [820–912] | 100% [100%–100%] |
| 2x | header-aware | 391 [298–477] | 14 [11–19] | 467 [373–546] | 12 [12–13] | 3% [2%–4%] | 40.05 [39.19–40.90] | 83.14 [82.26–85.04] | 1070 [1050–1094] | 100% [100%–100%] |
| 5x | backoff | 14 [8–28] | 9 [8–10] | 8 [0–29] | 15 [14–15] | 0% [0%–0%] | 57.50 [55.02–61.28] | 145.30 [142.13–149.84] | 4005 [3929–4096] | 100% [100%–100%] |
| 5x | header-aware | 460 [318–656] | 16 [10–22] | 550 [393–763] | 13 [12–14] | 3% [2%–4%] | 70.90 [70.39–71.63] | 136.27 [135.66–137.17] | 4240 [4222–4263] | 100% [100%–100%] |

### 3 copies, sharing one budget through Redis

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 3 [0–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 4 [0–8] | 100% [99%–100%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 2 [0–7] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] |
| 1.2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 12 [10–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.24 [16.71–17.60] | 134 [126–143] | 100% [100%–100%] |
| 1.2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 8 [8–9] | 1% [0%–4%] | 0.10 [0.10–0.10] | 15.34 [14.64–15.87] | 156 [149–164] | 100% [100%–100%] |
| 2x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 14 [12–15] | 0% [0%–0%] | 24.57 [22.74–26.30] | 91.69 [79.01–98.36] | 845 [801–912] | 100% [100%–100%] |
| 2x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–10] | 5% [0%–12%] | 23.57 [23.31–24.16] | 62.37 [61.68–63.45] | 837 [817–849] | 100% [100%–100%] |
| 5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 15 [14–15] | 0% [0%–0%] | 57.27 [54.08–60.94] | 148.31 [145.31–150.04] | 4014 [3961–4085] | 100% [100%–100%] |
| 5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 10 [9–12] | 6% [2%–10%] | 63.33 [62.94–63.80] | 130.03 [129.58–130.43] | 4060 [4043–4071] | 100% [100%–100%] |

## Against the checks

| Check | Result |
|---|---|
| All 24 budget tests pass with the counts in a real Redis | Yes (`QUOTAD_TEST_REDIS=... go test -race ./...`) |
| Two copies never take the same last unit | Yes. 100 requests race on two copies with 30 left: exactly 30 go, every time |
| A crashed copy's slot ends | Yes. Its lease runs out, and the request counts as failed (the unit stays spent) |
| Redis down: the copy keeps serving | Yes. It counts alone and tries Redis again every second |
| **3 copies alone break rules** | **Yes. 5,058 rule breaks over 50 runs; 40 of 50 runs had at least one (all runs at 1x load and above)** |
| **3 copies on Redis: 0 rule breaks** | **Yes. 0 in all 50 runs** |
| Budget use of 90% or more at 1x and above | Yes. 99% to 100% in every run, both ways |

## What the numbers say

**Without Redis, the copies break rules at every load of 1x and above.** At 2x, header-aware agents caused 298 to 477 rule breaks per run. The ranges with and without Redis never overlap.

**Most of these rule breaks are 429s from the secondary limits, not 403s.** Each copy allows 10 requests in flight and 90 points per second. Three copies together allow 30 and 270. The server allows 10 and 90. Header-aware agents send as fast as quotad lets them, so they hit this hardest.

**The main budget breaks too, but less.** There were 1 to 22 403s per run at 1x and above. Each copy learns the true remaining count from every answer, so it overspends only by what the other copies have in flight at that moment. This is why PROJECT.md was wrong to say Redis is "not needed for the hourly budget". It is needed, though the secondary limits are the bigger problem.

**With Redis, 3 copies act like one quotad.** The Redis table is almost the same as step 3's single quotad. For example, header-aware critical p95 at 2x: 62.37s on Redis, 62.45s for one quotad in step 3. Without Redis it is 83.14s, because agents lose time to 429s.

**Redis cost no visible time.** Critical p50 at 1x and below is 0.10s either way, which is the server's own latency. Redis ran on the same machine, so each round trip was well under a millisecond. Across a data center, expect about 1 to 2 ms per request (two round trips).

**Redis stayed small.** After all 50 runs, Redis held 134 keys. Each run's keys expire one day after their last use.

## Not tested yet

- **Redis going away during a run.** The unit test shows that a copy keeps serving. But in that case it counts alone with the full budget, so it can overspend. The 1/N local share and the merge on return are the next PR.
- **Real machines.** The 3 copies ran on one machine. They had separate memory, but they shared one clock and a fast link to Redis.
