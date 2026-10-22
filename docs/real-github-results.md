# Real GitHub results

Run on 2026-10-22 by Raghav on his laptop with `scripts/real-search-test.sh`, against the real `api.github.com`, with his own `gh` login. Before this, quotad was only tested against the fake server.

## Setup

- 6 agents share one token. Every call is the same read-only search: `GET /search/issues?q=repo:raghavs6/quotad&per_page=1`.
- Search has its own small limit: 30 calls a minute. That makes it a safe limit to test against. It takes little load to reach, and it resets every minute.
- The fleet wants 60 calls a minute (2× the limit), for 2 minutes per run.
- 2 strategies × (direct, through a fresh quotad) × 2 runs, with a 65-second gap so every run starts in a fresh search window.
- Only 2 runs per row, not the bench's 5, to keep load on the real account small. So this is a real-world check of the bench results, not a new finding on its own.

## Results

| strategy | via | rule breaks | sent to GitHub | ok | quotad 429s | critical p50 (s) | critical p95 (s) | unfinished critical |
|---|---|---|---|---|---|---|---|---|
| backoff | direct | 52, 47 | 112, 107 | 60, 60 | 0, 0 | 18.35, 14.63 | 72.90, 78.79 | 59, 60 |
| backoff | quotad | **0, 0** | 60, 60 | 60, 60 | 29, 32 | 17.37, 15.57 | 53.26, 53.44 | 61, 58 |
| header-aware | direct | 80, 75 | 140, 135 | 60, 60 | 0, 0 | 17.84, 15.84 | 54.56, 53.01 | 58, 58 |
| header-aware | quotad | **0, 0** | 60, 60 | 60, 60 | 5, 6 | 15.43, 16.32 | 52.77, 53.98 | 59, 58 |

"Rule breaks" are GitHub's own rate-limit answers. Every one was a `403` with `x-ratelimit-remaining: 0` on the `search` resource, which is the answer quotad's budget expects. "Sent to GitHub" through quotad is responses minus quotad's own 429s, which never leave the machine. The backoff runs through quotad also had 6 and 4 calls cut off by the end of the run.

## What the numbers say

**quotad stopped every rule break on the real API.** Without it, GitHub refused 47 to 80 calls per run. Through it, GitHub refused none. Every call quotad sent got a 200.

**No budget was lost.** Every run, direct or not, got all 30 calls in each minute. quotad sent exactly the 60 calls that fit, and nothing more.

**Header-aware agents break more rules alone than backoff agents** (75–80 vs 47–52). They all wait for the same reset time and then send together, so the rush right after a reset overshoots. The bench showed the same pattern.

**Backoff agents wait less through quotad.** Their critical p95 fell from 73–79s to 53s, and the two runs don't overlap. Header-aware p95 didn't change (53–55s both ways). At 2× load, half the calls can't fit, so about 60 calls stay unfinished in every run, with or without quotad. Sending important calls first is step 4.

**quotad learned the real limit from headers.** It read `x-ratelimit-limit: 30` for search with no limit typed in.

## Limits of this test

- Only the search resource. quotad keeps one budget per token, but GitHub keeps a separate budget per resource (`core`, `search`, `graphql`, ...; see `x-ratelimit-resource`). A fleet that mixes normal calls and searches on one token would be miscounted. That needs per-resource budgets before real mixed use.
- The secondary limits (100 in flight, 900 points a minute) were not pushed on the real API. Breaking them on purpose risks the account being flagged.
- A smoke test from the cloud session on `/repos/raghavs6/quotad` (core, 15,000 an hour), at 2 calls a second for 45s, well under every limit: 0 rule breaks both ways, and p50 0.37s both ways. So quotad added no delay we could measure.
