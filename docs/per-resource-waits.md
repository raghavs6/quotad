# Per-resource waits: results

Run on 2026-10-22 with `bin/bench -proxy -runs 5 -demands 0.5,1 -search-demands 0,1,2,5 -parallel 8`. It builds on [per-resource budgets](per-resource-results.md). The setup is the same: 10 agents (5 also poll), core budget 500 per 60-second "hour", search budget 3 per 1-second "minute", 10 in flight, 100ms latency, 3 windows per run, 5 runs per row. All runs go through the new quotad.

## The claim

"Core calls stay fast even when search is used up." In other words, quotad never makes a core call wait because the search budget is empty.

## What changed in the bench

- Every critical call now records the resource it spends (core or search) and when the agent first sent it.
- Per resource, the bench reports two numbers:
  - **Wait**: from when the agent wanted the call to when it succeeded. This is the same wait as before. It also counts time the agent spent stuck on its own earlier calls, because an agent sends one task call at a time.
  - **Call time**: from when the agent first sent the call to when it succeeded. This is only what happened to that one call: a hold in quotad, any rejections and retries, and the server's latency.
- `-search-demands` sets the search demand on its own. So core can stay under its budget (0.5×) or at it (1×) while search runs far over (2×, 5×).
- Calls the schedule wanted but never sent before the run ended are drawn as core or search the same way as sent calls, so they count under a resource too.

## Waits by resource

| demand | search demand | strategy | core wait p50 (s) | core wait p95 (s) | core call p95 (s) | unfinished core | search wait p95 (s) | search call p95 (s) | unfinished search |
|---|---|---|---|---|---|---|---|---|---|
| 0.5x | 0x | backoff | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 0.00 [0.00–0.00] | 0.00 [0.00–0.00] | 0 [0–0] |
| 0.5x | 0x | header-aware | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 0.00 [0.00–0.00] | 0.00 [0.00–0.00] | 0 [0–0] |
| 0.5x | 1x | backoff | 0.10 [0.10–0.10] | 7.60 [1.84–16.02] | 0.10 [0.10–0.10] | 9 [0–27] | 10.24 [4.73–16.66] | 3.66 [2.02–5.02] | 14 [2–28] |
| 0.5x | 1x | header-aware | 0.10 [0.10–0.10] | 4.78 [1.12–9.10] | 0.10 [0.10–0.10] | 2 [0–5] | 6.57 [3.11–11.47] | 3.11 [2.10–4.11] | 7 [0–15] |
| 0.5x | 2x | backoff | 36.43 [29.36–41.24] | 103.27 [94.77–112.57] | 0.10 [0.10–0.10] | 258 [244–271] | 103.41 [92.31–126.20] | 10.85 [10.27–11.11] | 534 [489–557] |
| 0.5x | 2x | header-aware | 32.77 [28.87–35.44] | 117.94 [111.32–122.05] | 0.10 [0.10–0.10] | 257 [245–271] | 119.06 [112.06–124.08] | 12.92 [10.62–17.59] | 528 [492–557] |
| 0.5x | 5x | backoff | 71.25 [66.03–75.53] | 137.06 [134.72–139.71] | 0.10 [0.10–0.10] | 417 [401–436] | 137.82 [136.31–139.40] | 11.36 [10.90–12.00] | 2167 [2144–2193] |
| 0.5x | 5x | header-aware | 68.06 [64.67–72.51] | 146.65 [142.35–151.21] | 0.10 [0.10–0.10] | 420 [407–426] | 147.85 [145.43–150.95] | 9.11 [6.11–12.12] | 2166 [2137–2195] |
| 1x | 0x | backoff | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 4 [0–8] | 0.00 [0.00–0.00] | 0.00 [0.00–0.00] | 0 [0–0] |
| 1x | 0x | header-aware | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 0.00 [0.00–0.00] | 0.00 [0.00–0.00] | 0 [0–0] |
| 1x | 1x | backoff | 0.10 [0.10–0.10] | 9.39 [4.69–14.10] | 0.10 [0.10–0.10] | 29 [16–45] | 11.83 [8.28–16.62] | 4.70 [3.92–5.17] | 17 [9–29] |
| 1x | 1x | header-aware | 0.10 [0.10–0.10] | 7.92 [1.97–14.31] | 0.10 [0.10–0.10] | 25 [13–34] | 10.62 [4.11–15.67] | 4.11 [2.11–6.11] | 19 [6–23] |
| 1x | 2x | backoff | 37.11 [32.85–42.36] | 101.32 [92.41–104.79] | 0.10 [0.10–0.10] | 512 [484–534] | 104.52 [96.42–111.38] | 11.58 [10.90–11.85] | 538 [525–566] |
| 1x | 2x | header-aware | 34.55 [27.34–41.63] | 109.84 [90.27–133.57] | 0.10 [0.10–0.10] | 506 [473–555] | 111.16 [92.57–132.63] | 14.44 [10.11–19.59] | 531 [519–557] |
| 1x | 5x | backoff | 72.26 [69.59–74.13] | 137.75 [135.89–140.01] | 0.10 [0.10–0.10] | 840 [803–872] | 138.07 [135.86–140.27] | 11.33 [11.00–11.81] | 2167 [2136–2198] |
| 1x | 5x | header-aware | 69.58 [67.78–72.24] | 151.16 [142.64–161.01] | 0.10 [0.10–0.10] | 842 [814–858] | 150.66 [143.10–158.46] | 8.75 [6.31–12.11] | 2158 [2134–2188] |

## The full table

| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization | search demand | search utilization |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] | 0x | 0% [0%–0%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 0 [0–1] | 50% [49%–50%] | 0x | 0% [0%–0%] |
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 5 [3–8] | 100% [100%–100%] | 0.15 [0.10–0.24] | 9.07 [3.53–16.02] | 23 [2–55] | 50% [48%–52%] | 1x | 96% [93%–97%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 6 [6–7] | 100% [100%–100%] | 0.10 [0.10–0.10] | 5.92 [2.58–10.88] | 9 [0–20] | 51% [50%–52%] | 1x | 96% [93%–98%] |
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [7–8] | 100% [100%–100%] | 38.02 [31.42–41.92] | 102.71 [93.54–119.84] | 792 [749–808] | 33% [32%–35%] | 2x | 100% [100%–100%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–10] | 100% [100%–100%] | 34.26 [31.84–36.61] | 118.69 [111.37–123.59] | 785 [752–805] | 33% [32%–34%] | 2x | 100% [100%–100%] |
| 0.5x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 8 [7–8] | 99% [99%–99%] | 71.35 [70.44–72.14] | 137.71 [136.40–139.40] | 2584 [2561–2614] | 22% [22%–24%] | 5x | 100% [100%–100%] |
| 0.5x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–9] | 100% [100%–100%] | 68.57 [65.26–70.85] | 147.63 [145.84–150.31] | 2585 [2563–2617] | 22% [21%–23%] | 5x | 100% [100%–100%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 3 [0–11] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 4 [0–8] | 100% [99%–100%] | 0x | 0% [0%–0%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 2 [0–7] | 0% [0%–0%] | 0.10 [0.10–0.10] | 0.10 [0.10–0.10] | 3 [0–5] | 100% [99%–100%] | 0x | 0% [0%–0%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [5–9] | 100% [100%–100%] | 0.12 [0.10–0.18] | 10.32 [6.51–14.81] | 46 [26–74] | 98% [96%–100%] | 1x | 96% [95%–97%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [6–8] | 100% [100%–100%] | 0.10 [0.10–0.10] | 9.09 [3.11–15.09] | 43 [22–57] | 97% [96%–100%] | 1x | 96% [93%–98%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 7 [6–8] | 100% [100%–100%] | 38.27 [35.02–42.36] | 103.50 [94.71–108.07] | 1050 [1015–1097] | 66% [64%–68%] | 2x | 100% [100%–100%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–9] | 100% [100%–100%] | 35.87 [27.82–42.89] | 110.65 [91.78–133.10] | 1037 [1004–1112] | 65% [62%–66%] | 2x | 100% [100%–100%] |
| 1x | backoff | 0 [0–0] | 0 [0–0] | 0 [0–0] | 8 [6–9] | 99% [99%–99%] | 71.19 [69.85–72.04] | 137.94 [135.90–140.27] | 3006 [2967–3028] | 44% [43%–46%] | 5x | 100% [100%–100%] |
| 1x | header-aware | 0 [0–0] | 0 [0–0] | 0 [0–0] | 9 [9–10] | 100% [100%–100%] | 69.03 [66.42–71.98] | 150.89 [143.10–158.92] | 3000 [2975–3028] | 43% [41%–43%] | 5x | 100% [100%–100%] |

## Against the checks

| Check | Result |
|---|---|
| Unit tests: waits and call times split by resource; unfinished calls counted; the task stream labels each call with its resource | Pass, with `go test -race ./...` |
| 0 rule breaks | Yes, 0 in all 80 runs. |
| quotad never holds a core call because search is used up | **Yes.** Core call p95 is 0.10s (the server's latency) in every run, at every search demand. |
| An agent's core work stays fast when search is used up | **No.** Core wait p95 goes from 0.10s with no searches to 101–151s at 2× and 5× search. Core budget use falls from 50% to 22% (at 0.5× core) and from 100% to 43% (at 1× core). |

## What the numbers say

**quotad keeps the two budgets apart.** Once an agent sends a core call, it comes back in 0.10s, every time. That is the server's latency and nothing more. This holds even at 5× search, where four out of five searches can never fit.

**But the agents' core work still slows down a lot.** The extra wait is all before the core call is sent. Each agent sends one task call at a time. When its search is held, the agent's next core call waits behind it. At 2× search and above, most agents are stuck on a search most of the time. So they send fewer core calls, and core budget use falls.

Backoff agents ignore headers, and they show the same knock-on wait as header-aware agents. So the cause is the one-call-at-a-time queue, not an agent pausing on search's "0 left" header.

This is not quotad holding core calls. It is the agent waiting for its own search. A real agent that needs a search result before its next step would wait too. So I do not count it as a quotad bug. But it means we must be careful with the claim. "Core calls are never delayed by quotad" is true. "Agents keep doing core work at full speed when search runs out" is not true in this bench.

**Search's own calls take about 10s at p95** when search demand is 2× or more. quotad holds a search for at most 0.5s (GitHub's 30s, scaled), then answers 429 with Retry-After. The agent waits and tries again. Most of the 10s is that retry loop.

## Limits

- Agents here send one task call at a time. A fleet where agents search in parallel with their other work would show less of the knock-on wait. I did not test that.
- Only through quotad. Without quotad, header-aware agents keep one "remaining" for all resources, so a search 0 would also pause their core calls. That comparison is not in this run.
