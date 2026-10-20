# Governor Step 2: The First Proxy (Primary Budget Only)

Written 2026-10-20 for sign-off before any code. It's the first build step of chunk 2 (v0.1 on one machine) in PROJECT.md.

## Goal

Put a real proxy between the agents and the fake server, and get **primary-limit 403s to 0** in every bench run at every load level. Nothing else yet.

Step 1 showed two separate problems:
1. **Primary budget** (403 at `remaining: 0`): agents keep sending after the budget is gone.
2. **Secondary limits** (429 + `retry-after`): agents stampede the concurrency limit, and one agent's `retry-after` doesn't stop the others.

Following "one new thing per step", this step fixes only #1. Secondary limits are step 3 and priorities are step 4. So at the end of this step, **violations will still be above 0** from 429s. That's expected, and the step's check says so up front.

An analogy: the proxy is a bouncer at a club with a fixed number of wristbands per hour. It counts the wristbands from what the club tells it, hands them out, and makes people wait in line when they run out. It doesn't yet care how crowded the dance floor is (concurrency) or who's a VIP (priority).

## What gets built

```text
cmd/quotad/         # the proxy program: -listen, -upstream, flags below
internal/budget/    # the budget rules, pure logic with an injected clock (unit-tested)
internal/proxy/     # HTTP: hold, forward, read headers, answer
cmd/bench           # + a -proxy flag that starts quotad between loadgen and fakegithub
```

Agents don't change. As in step 1's Decision 3, loadgen just points `-target` at quotad instead of fakegithub.

## How a request flows

1. An agent's request arrives. The proxy finds the budget for its token (the `Authorization` header).
2. **Reserve:** if the budget has room (`remaining − reserved > 0`), take one unit and forward the request right away.
3. **Hold:** if not, wait in line until the reset. If that wait is longer than the max hold, answer `429` with `Retry-After` straight away, without contacting the upstream.
4. **Correct:** when the upstream answers, read `x-ratelimit-remaining`, `-limit` and `-reset`, update the budget, release the reservation, and pass the response back unchanged.

## Design decisions

### Decision 1: How to forward requests

| Option | Upside | Downside |
|---|---|---|
| **A. `net/http/httputil.ReverseProxy`** | Standard library. Handles streaming bodies, hop-by-hop headers and `X-Forwarded-For`. `ModifyResponse` is a hook that runs on each upstream response, which is exactly where we read headers | Its error handling has to be learned (`ErrorHandler`) |
| B. Our own `http.Client` loop | Every line is ours, easy to read | We'd re-implement header copying and streaming, and get some of it wrong |

**Recommend A.** It's what AGENTS.md's "standard library first" points to, and the hold logic sits in a small handler in front of it.

### Decision 2: What to do before we've seen any headers

When the proxy starts, it doesn't know the budget. Ten agents might send at once.

| Option | Upside | Downside |
|---|---|---|
| **A. Learn first:** let one request through and hold the rest until its headers come back | Can never overshoot, even if the token is already near 0 | The first burst waits one round trip (~100ms in the bench) |
| B. Let everything through until the first response | No added wait | If the token is already at 0 (another machine, or a restart), the whole burst is violations |
| C. Configure a starting budget by hand | No wait | Exactly the hand-typed limits quotad exists to avoid |

**Recommend A.** One round trip at startup is cheap, and "no overshoot" is the product's promise.

### Decision 3: Reserve, then correct (the budget rule)

Per token, the proxy keeps `limit`, `remaining` (from headers), `reset`, and `reserved` (requests in flight).

- **Admit** only if `remaining − reserved > 0`, then `reserved++`.
- **On a response**, `reserved--`. Then:
  - If the response is for the **same window** (same `reset`), set `remaining = min(remaining, header)`. Within a window GitHub's `used` only goes up, so a smaller `remaining` is always the newer truth. Responses that arrive out of order then can't push our count back up.
  - If it's a **new window** (`reset` moved forward), take the header as is.
- **At the reset time**, set `remaining = limit` and release held requests, in arrival order, up to that limit.

This is the PROJECT.md rule ("GitHub: within a window `used` only goes up, take the max"). `min(remaining)` is the same thing as `max(used)`.

### Decision 4: When the upstream fails without answering

A request might time out, or its connection might drop after GitHub has already counted it.

| Option | Upside | Downside |
|---|---|---|
| **A. Count it as spent** (don't give the unit back until the next response corrects us) | Never overshoots | Undercounts by one until the next response fixes it |
| B. Refund it | Uses every unit | Can overshoot by the number of failed requests |

**Recommend A.** The next successful response corrects the count anyway, so the cost is tiny.

### Decision 5: The max hold, at the bench's shrunken time scale

PROJECT.md says to hold up to 30s. That's 30s out of a 1-hour window, or 0.8%. In the bench, 30s is half of the 60s "hour". If we keep 30s, nearly every out-of-budget call gets held instead of rejected, so the bench would test a different product from the real one.

| Option | Upside | Downside |
|---|---|---|
| **A. Scale it like every other limit:** `-max-hold` defaults to 30s, and the bench passes `window/120` (0.5s for a 60s "hour") | Keeps real-world ratios, consistent with step 1's fix for points per minute | A 0.5s hold looks small, so most out-of-budget calls get the immediate 429 |
| B. Keep 30s in the bench | Matches the number written in PROJECT.md | Holds are 60× longer, relative to the window, than in production |

**Recommend A**, for the same reason we scaled the points window.

### Decision 6: Clock skew at the reset

`reset` comes from GitHub's clock, and the proxy uses its own. If the proxy's clock runs ahead, it releases requests a moment before GitHub resets, and they're violations.

**Recommend a `-reset-guard` flag**, default 1s: wait until `reset + guard`. On a real hour that's negligible. In the bench both programs share one clock, so the bench passes `-reset-guard 0` and doesn't lose 1/60 of every window.

## Failure cases (AGENTS.md section 8)

- **Client gives up mid-hold:** the held request watches its client's context. When the client disconnects, it leaves the line and never reserves anything.
- **Crashed agent:** the proxy sees the same thing as a client giving up. Anything already forwarded stays counted, per Decision 4.
- **Burst right after a reset:** held requests are released up to the budget all at once. That will trip the concurrency limit, and that's step 3's problem. The step 2 bench will show it as 429s.
- **Proxy restarts:** it learns the budget again (Decision 2).

## Known gaps, on purpose

- **Only one bucket per token.** GitHub has separate buckets (`core`, `search`, `graphql`, named by `x-ratelimit-resource`). The fake server only has `core`. Per-resource buckets come before the real-GitHub test in chunk 4.
- **No priorities and no secondary limits** (steps 3 and 4).

## How we'll know step 2 is done

1. **Unit tests for `internal/budget`**, with an injected clock:
   - A fresh budget admits one request and holds the second until the first response arrives (Decision 2).
   - With `remaining: 3` and 3 requests in flight, a 4th is held.
   - An older response with a larger `remaining` doesn't raise the count, and a new window's header replaces it.
   - At the reset, held requests are released in arrival order, up to the limit.
   - A wait longer than the max hold gets an immediate 429 with the right `Retry-After`.
   - A held request whose client gives up is removed and reserves nothing.
2. **Bench through the proxy, 5 runs per setting:** **0 primary 403s in every run at every demand**, and utilization ≥ 90% at 1× and above.
3. **Honest about what's left:** 429 violations will still be above 0. If they come out at 0 anyway, I'll find out why before calling that a win.

## Sign-off (2026-10-20)

Raghav approved the step and all six decisions, including scaling the max hold to `window/120` in the bench and splitting the work into steps 2–4. The proxy binary is `cmd/quotad`.
