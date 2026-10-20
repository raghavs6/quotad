# Step 1 baselines

First run on 2026-10-19 with `bin/bench -parallel 8` (defaults: 10 agents, 5 of them also polling, one shared token, 500 requests per 60s "hour", 10 in flight, 90 points per 1s, 100ms latency, 3 windows per run, 5 runs per row, seeds 1–5). 30% of demand is background polling, and 10% of task calls are writes. Raw runs: `results/bench.jsonl` (not committed; rerun to regenerate). Rerun in full on 2026-10-20, after utilization was changed to count only windows that fit inside the run (see below). Every other column is measured the same way as in the first run.

## Results

10 agents (5 polling), budget 500 per 1m0s, 10 in flight, 100ms latency, 3 windows per run, 5 runs per row. Cells are mean [min–max].

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |
|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] |
| 1x | backoff | 8 [1–20] | 8 [1–20] | 0 [0–0] | 6 [1–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 2 [0–4] | 100% [99%–100%] |
| 1x | header-aware | 11 [1–23] | 8 [1–14] | 4 [0–10] | 5 [1–10] | 16% [0%–42%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] |
| 1.2x | backoff | 169 [166–174] | 169 [166–174] | 0 [0–0] | 13 [13–14] | 0% [0%–0%] | 0.10 [0.10–0.10] | 17.43 [16.60–18.44] | 132 [125–140] | 100% [100%–100%] |
| 1.2x | header-aware | 175 [88–294] | 27 [27–28] | 165 [68–296] | 12 [11–12] | 25% [12%–37%] | 0.10 [0.10–0.10] | 14.96 [14.59–16.17] | 148 [138–155] | 100% [100%–100%] |
| 2x | backoff | 218 [185–253] | 202 [185–214] | 18 [0–57] | 14 [13–15] | 0% [0%–0%] | 25.09 [22.32–28.73] | 93.42 [90.02–97.85] | 839 [819–850] | 100% [100%–100%] |
| 2x | header-aware | 802 [745–842] | 33 [31–35] | 851 [790–891] | 12 [12–13] | 5% [4%–5%] | 40.48 [37.67–42.99] | 82.68 [81.77–83.93] | 1073 [1064–1085] | 100% [100%–100%] |
| 5x | backoff | 163 [157–174] | 163 [157–174] | 0 [0–0] | 15 [14–15] | 0% [0%–0%] | 56.78 [52.32–60.62] | 148.54 [144.39–152.06] | 4026 [3951–4073] | 100% [100%–100%] |
| 5x | header-aware | 838 [783–888] | 35 [33–37] | 887 [825–942] | 13 [12–14] | 5% [4%–5%] | 70.01 [68.83–71.52] | 135.79 [135.43–136.06] | 4234 [4208–4253] | 100% [100%–100%] |

## What the numbers say

**The problem is real, so the kill criterion doesn't fire.** At every demand of 1× and above, both baselines send requests while limited in every run. The best baseline's lowest count across 5 runs is 1 at 1×, 185 at 2× and 157 at 5×.

**Header-aware agents have fewer 403s but far more violations.** At 2×, header-aware cuts primary-limit 403s from ~202 to ~33. But it sends ~850 requests into secondary-limit 429s, so its total violations go from ~218 to ~800. Each agent obeys the `retry-after` it was given, but the penalty is on the shared token. Agents that didn't get that 429 keep sending into the penalty window. This is the "alone" problem in its clearest form.

**The reset stampede shows up for header-aware agents, not backoff ones.** The step 1 design predicted rejection spikes right after each reset for baseline 1. That was wrong. Backoff agents are mostly asleep in long backoffs when the window resets, and their rejections bunch up when the budget runs out instead. Header-aware agents all wake at the reset time. In a 2× run (seed 1), every second from the reset at 60s through about 75s has ~10 rejections as backlogged agents pile into the concurrency limit.

**Above 1×, critical waits grow without limit for any strategy.** Critical calls alone are 70% of demand, so at 2× they need 1.4× the budget. No strategy can serve that, so waits just grow over the run (p95 ≈ 80–150s in a 180s run, with hundreds to thousands of calls never sent). The governor can't beat that by 50% by shuffling the order alone. Waits only mean something where critical demand fits in the budget but total demand doesn't, i.e. between 1× and 1/0.7 ≈ 1.43×.

**1× to 1.43× is where the wait target is judged, and 1.2× is the bench's level for it.** At 1.2×, critical calls need 0.84× the budget, so a governor that holds background polls back could in principle serve them all. Both baselines leave critical p95 at 15–18s, and their ranges don't overlap. The best is header-aware at a mean of 14.96s (lowest run 14.59s). To meet the target, the governor needs a critical p95 of about **7.5s or less** (50% below the best mean), or 7.3s or less if every run has to beat the best baseline run by half. Header-aware violations swing widely at this level (88–294) because how badly the 429 storms build up depends on timing.

**At 1× the ranges overlap,** so the two baselines can't be told apart there. That's expected: there's barely enough load to hit the limit.

**Utilization is 100% at 1× and above for both baselines.** The first run showed 75–100% for header-aware. That was a measuring error: a window opened in the run's last seconds was charged a full window's budget. Utilization now counts only windows that reset inside the run.

## Against the step 1 checks

| Check | Result |
|---|---|
| Unit tests for the fake server's rules | Pass (`go test -race ./...`) |
| Baseline 1 violations > 0, spikes right after each reset | Violations > 0 at ≥1×. **The spikes happen when the budget runs out, not right after a reset.** |
| Baseline 2 has fewer violations than baseline 1, but still > 0 | **No at 2× and 5×**: fewer 403s but more 429s, so more violations in total. Same as baseline 1 at 1×. |
| Repeatable within ranges across 5 runs | Yes at 2× and 5× (ranges don't overlap between strategies). At 1× they overlap. |
