# Step 1 baselines

Run on 2026-10-19 with `bin/bench -parallel 8` (defaults: 10 agents, 5 of them also polling, one shared token, 500 requests per 60s "hour", 10 in flight, 90 points per 1s, 100ms latency, 3 windows per run, 5 runs per row, seeds 1–5). 30% of demand is background polling, and 10% of task calls are writes. Raw runs: `results/bench.jsonl` (not committed; rerun to regenerate). The 1.2× rows were added on 2026-10-20 with `bin/bench -parallel 8 -demands 1.2`, on the same code.

## Results

10 agents (5 polling), budget 500 per 1m0s, 10 in flight, 100ms latency, 3 windows per run, 5 runs per row. Cells are mean [min–max].

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 8 [1–20] | 8 [1–20] | 0 [0–0] | 6 [1–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 2 [0–4] | 100% [99%–100%] |
| 1x | header-aware | 10 [1–21] | 7 [1–14] | 3 [0–8] | 5 [1–9] | 14% [0%–36%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 95% [75%–100%] |
| 1.2x | backoff | 169 [166–174] | 169 [166–174] | 0 [0–0] | 13 [13–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.43 [16.60–18.44] | 132 [125–140] | 100% [100%–100%] |
| 1.2x | header-aware | 211 [80–478] | 27 [27–27] | 204 [59–500] | 11 [11–12] | 30% [8%–44%] | 0.58 [0.10–2.50] | 14.45 [12.89–15.48] | 145 [134–155] | 80% [75%–100%] |
| 2x | backoff | 218 [185–253] | 202 [185–215] | 18 [0–57] | 14 [13–15] | 0% [0%–0%] | 25.01 [22.32–28.44] | 93.35 [89.64–97.86] | 837 [819–848] | 100% [100%–100%] |
| 2x | header-aware | 748 [700–808] | 33 [31–36] | 791 [739–854] | 13 [12–14] | 5% [4%–5%] | 38.59 [36.49–40.01] | 81.64 [80.85–82.15] | 1061 [1051–1073] | 80% [75%–100%] |
| 5x | backoff | 173 [155–221] | 161 [155–169] | 13 [0–66] | 15 [14–15] | 0% [0%–0%] | 57.24 [54.92–61.14] | 148.40 [144.23–150.68] | 4012 [3908–4085] | 100% [100%–100%] |
| 5x | header-aware | 864 [731–1081] | 36 [34–39] | 915 [771–1149] | 14 [13–14] | 5% [4%–5%] | 69.90 [68.08–71.11] | 135.63 [134.78–136.32] | 4231 [4188–4254] | 80% [75%–100%] |

## What the numbers say

**The problem is real, so the kill criterion doesn't fire.** At every demand of 1× and above, both baselines send requests while limited in every run. The best baseline's lowest count across 5 runs is 1 at 1×, 185 at 2× and 155 at 5×.

**Header-aware agents have fewer 403s but far more violations.** At 2×, header-aware cuts primary-limit 403s from ~202 to ~33. But it sends ~790 requests into secondary-limit 429s, so its total violations go from ~218 to ~748. Each agent obeys the `retry-after` it was given, but the penalty is on the shared token. Agents that didn't get that 429 keep sending into the penalty window. This is the "alone" problem in its clearest form.

**The reset stampede shows up for header-aware agents, not backoff ones.** The step 1 design predicted rejection spikes right after each reset for baseline 1. That was wrong. Backoff agents are mostly asleep in long backoffs when the window resets, and their rejections bunch up when the budget runs out instead. Header-aware agents all wake at the reset time. In a 2× run (seed 1), every second from the reset at 60s through about 75s has ~10 rejections as backlogged agents pile into the concurrency limit.

**Above 1×, critical waits grow without limit for any strategy.** Critical calls alone are 70% of demand, so at 2× they need 1.4× the budget. No strategy can serve that, so waits just grow over the run (p95 ≈ 80–150s in a 180s run, with hundreds to thousands of calls never sent). The governor can't beat that by 50% by shuffling the order alone. Waits only mean something where critical demand fits in the budget but total demand doesn't, i.e. between 1× and 1/0.7 ≈ 1.43×.

**1× to 1.43× is where the wait target is judged, and 1.2× is the bench's level for it.** At 1.2×, critical calls need 0.84× the budget, so a governor that holds background polls back could in principle serve them all. Both baselines leave critical p95 at 13–18s, and their ranges don't overlap. The best is header-aware at a mean of 14.45s (lowest run 12.89s). To meet the target, the governor needs a critical p95 of about **7.2s or less** (50% below the best mean), or 6.4s or less if every run has to beat the best baseline run by half. Header-aware violations swing widely at this level (80–478) because how badly the 429 storms build up depends on timing.

**At 1× the ranges overlap,** so the two baselines can't be told apart there. That's expected: there's barely enough load to hit the limit.

**Utilization of 75–100% for header-aware** is from window drift: a window opened just before a run ends counts in full (see the step 1 doc).

## Against the step 1 checks

| Check | Result |
|---|---|
| Unit tests for the fake server's rules | Pass (`go test -race ./...`) |
| Baseline 1 violations > 0, spikes right after each reset | Violations > 0 at ≥1×. **The spikes happen when the budget runs out, not right after a reset.** |
| Baseline 2 has fewer violations than baseline 1, but still > 0 | **No at 2× and 5×**: fewer 403s but more 429s, so more violations in total. Same as baseline 1 at 1×. |
| Repeatable within ranges across 5 runs | Yes at 2× and 5× (ranges don't overlap between strategies). At 1× they overlap. |
