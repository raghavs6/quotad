# Governor Step 1: Fake GitHub and Baseline Numbers

Written 2026-10-18 for sign-off before any code. This is build step 1 in [governor-design.md](governor-design.md).

## Goal

Recreate the "stampede" on a laptop and get numbers to beat. By the end of this step we can say something like: "10 agents sharing one token sent X requests while rate limited, and important calls waited Y seconds (p95)." Every later step has to improve those numbers.

Nothing in this step is the product yet. It's the test bench the product will be judged on. Like a wind tunnel built before the plane.

## What gets built

Everything is in Go, in one module:

```text
cmd/fakegithub/     # a fake GitHub API server
cmd/loadgen/        # simulated agents that hit it
internal/fake/      # the rate-limit rules (unit-tested)
internal/agents/    # agent behavior + the two baseline retry strategies
internal/metrics/   # counting and percentiles
```

### 1. Fake GitHub server

A small HTTP server that answers like GitHub and **enforces GitHub's rules**, but with numbers we choose:
- **Primary limit:** N requests per window, per token. Every response carries the real header names: `x-ratelimit-limit`, `-remaining`, `-used`, `-reset`, `-resource`. When the budget is gone it answers `403` with `x-ratelimit-remaining: 0`, which is what GitHub does.
- **Secondary limits:** max concurrent requests, and points per minute (read = 1, write = 5). Breaking them gets a `429` with `retry-after`.
- **Violation counter:** every request that arrives while the caller is limited (budget at 0 before reset, or inside a `retry-after` window) gets counted. This is the "ban risk" number from the design. It's exposed at `GET /__stats` as JSON.
- **Not in step 1:** ETags and caching, the content-creation limit, GraphQL. They come in later steps.

### 2. Simulated agents (load generator)

N agents run at once as goroutines (Go's lightweight threads), all sharing one token:
- Each agent runs a **task stream**: mostly reads plus some writes, with a short pause between them, like a coding agent reading files and leaving a comment. These calls are **critical**.
- Some agents also run a **poller**: the same "is CI done?" read every few seconds. These calls are **background**.
- Fixed random seeds, so we can repeat a run.

Each agent uses one of two retry strategies, the baselines from the design:
1. **Backoff with jitter:** on a 403 or 429, wait 1s, 2s, 4s… plus a random extra. This is what most SDKs do.
2. **Header-aware, but alone:** also obey `retry-after` and pause until `reset` when `remaining` hits 0. This is the best an agent can do on its own, and it shows the stampede still happens without sharing.

### 3. Numbers it prints

| Metric | Meaning |
|---|---|
| Violations | Requests sent while limited (from the fake server's counter) |
| Rejections | Count of 403/429 responses, and **rejections per second over time**, so we can see bursts |
| Critical wait p50 / p95 | From when an agent *wanted* to send a critical call to when it succeeded |
| Utilization | Successful requests ÷ the budget available during the run |

## Design decisions in this step

### Decision 1: Real time, but a shorter "hour"

GitHub's window is an hour. Waiting an hour per test run is too slow.

| Option | Upside | Downside |
|---|---|---|
| **A. Real clock, shrunk window** (e.g. "hour" = 60 seconds, budget = 500) | Real HTTP and real timing. **The same fake server later tests the real proxy unchanged** | Runs aren't perfectly repeatable, since real timing varies a little |
| B. Simulated clock, like ToolScale's event simulator | Perfectly repeatable and very fast | Can't put the real proxy in it. We'd test a model of the product, not the product |

**Recommend A.** In ToolScale we simulated because there was no real system. Here the product **is** a real proxy, so the test bench should be real too. The window and budget are config flags, and we keep the ratios close to GitHub's.

### Decision 2: How "repeatable" do we promise?

Real timing means two runs with the same seed won't match exactly. So each configuration runs **5 times**, and we report the average and the range. A result counts only if the ranges don't overlap. This is the honest version of "same seed, same answer."

### Decision 3: One process or two?

The load generator talks to the fake server over real HTTP on localhost, as two separate programs. That means step 2's proxy can sit between them with no code changes: we only point the load generator at a different address. This is the same base-URL trick real agents will use.

## How we'll know step 1 is done

1. **Unit tests for the fake server's rules**, each checked by hand:
   - With a budget of 5, the 6th request in a window gets a 403, `remaining` goes 4, 3, 2, 1, 0, and `used` goes 1…5.
   - After the window resets, requests succeed again and `remaining` is back to 5.
   - A request inside a `retry-after` window is counted as a violation.
   - The 11th concurrent request when the limit is 10 gets a 429.
   - A write costs 5 points, a read 1.
2. **Baseline 1 shows the problem:** with 10 agents, violations are above 0 and the rejections-per-second chart shows **spikes right after each reset** (everyone retrying at once).
3. **Baseline 2 is better but not fixed:** fewer violations than baseline 1, but still above 0, or important calls stuck behind background polling.
4. **Repeatable within the ranges** from Decision 2, across 5 runs.

If baseline 2 already gets 0 violations **and** low critical waits, that's an early warning that the governor may not be needed. I'd tell you right away rather than keep building.

## Things I need from you

1. **Your go** on the step and the three decisions.
2. **A repo.** No repo for this exists yet, and creating one has to be your call. Please create an empty repo (public is fine for an open-source tool) and tell me its name; I'll attach it and push there. Name ideas: `quotad` (my pick), `fairshare`, `tokenpool`. Until it exists, I can build in my workspace and push once it's ready.
3. **The LLM questions** (which provider first, reservation size, stream flag) can wait until the LLM chunk. They don't block step 1.

## Changes agreed before building (2026-10-19)

Five questions came up in review. Here is what each one changed.

1. **Shrinking the hour would have distorted the per-minute limit.** GitHub's points limit is per minute. If the "hour" became 60s but the "minute" stayed 60s, both limits would run on the same clock. So every window shrinks by the same factor: the points window defaults to `window / 60`, and the points budget to `900 × budget / 5000`, GitHub's real ratio.
2. **Reset times are whole seconds.** GitHub sends `x-ratelimit-reset` as whole epoch seconds. Rounding error that's tiny over an hour is ~2% of a 60s window. The fake server starts each window on a whole second, so the reset header is exact. `retry-after` is still whole seconds, like GitHub's, so with a sub-second points window a 429 makes callers wait at least 1s; that's a known distortion of the secondary limit.
3. **Load is swept, not fixed.** `-demand` sets how much the fleet wants per window as a multiple of the budget. The bench runs 0.5×, 1×, 2× and 5×. At 0.5× there is nothing to fix; that row is a sanity check.
4. **Violations aren't the only number.** The bench also reports 403s and 429s separately, peak rejections per second, and the share of rejections within 2s after a reset (the stampede). What counts as a win was fixed before any numbers came in: the success targets and kill criterion in AGENTS.md, judged per demand level, with ranges that don't overlap.
5. **Calls that never succeed still count.** A critical call still waiting when the run ends counts with a wait of "run end − when it was wanted", and the number of unfinished calls is reported.

Two other choices made while building:
- **Concurrency limit defaults to 10**, not GitHub's 100. With 10 agents and 5 pollers, at most 15 calls are in flight, so a limit of 100 could never be reached. It's a flag.
- **Utilization divides by the full budget of every window started.** Windows start on the first request after a reset, so they drift with traffic. Counting a window opened near the end in full keeps utilization at or below 100%, at the cost of looking a little worse on short runs.
