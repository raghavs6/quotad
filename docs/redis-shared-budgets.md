# Redis shared budgets (v0.2)

Written 2026-10-22 for sign-off before any code. Raghav signed off on all four questions on 2026-10-22. Results: [redis-results.md](redis-results.md). PR #6 (per-resource budgets) and PR #7 (waits split by resource) are merged, so this builds on `main`.

## Goal

Many quotad copies, on different machines, share **one budget per token and per resource**. Together they break 0 rules, just like one quotad does today. Redis is optional: with no Redis setting, quotad works in memory exactly as it does now.

**Success check (the bench):** 3 quotad copies, one token, one fake server.
- Without Redis: they break rules (each copy thinks it owns the whole budget).
- With Redis: 0 rule breaks in every run, and budget use of 90% or more.
- 5 runs per setting, average and range, as AGENTS.md says.

## Words used in this document

- **Redis:** a small, fast database that keeps data in memory. Many programs can talk to one Redis over the network. We use it as a shared notebook that every quotad copy reads and writes.
- **Copy (instance):** one running quotad process. Each machine runs its own copy.
- **Atomic:** a change that happens all at once. No other copy can see it half done or change the same data in the middle.
- **Lua script:** a short program that we send to Redis. Redis runs the whole script before it does anything else, so the script is atomic.
- **Lease:** a reservation with an end time. If the owner does not give it back in time, it ends on its own.
- **Docker:** a tool that runs a program in a sealed box (a container) with everything it needs. You can start Redis with one command and throw it away after.
- **Round trip:** one message to Redis and its answer. On the same machine it takes well under a millisecond. Across a data center it takes about 1 ms.

## What goes wrong today

Each quotad copy keeps its counts in its own memory. Two copies use one token. GitHub says 10 calls are left. Copy A believes it can send 10. Copy B also believes it can send 10. Together they send 20, and 10 of them break the rule.

It is like two people who share one bank account but keep separate notebooks. Each person sees $100 and spends $100.

PROJECT.md says Redis is "not needed for the hourly budget". I think that is wrong, for the reason above: response headers correct each copy only *after* it has sent its calls. The bench will show this with numbers (the "without Redis" rows).

## The Docker question

**Yes, use Docker for Redis in development and tests. quotad itself does not need Docker.**

- quotad takes one setting, `-redis redis://host:6379`. Any Redis works: one in Docker on a laptop, a managed one in the cloud (for example AWS ElastiCache or Upstash), or one installed by hand.
- For the bench and the tests we start Redis with one line: `docker run --rm -p 6379:6379 redis:7-alpine`. Nothing to install, and nothing left behind after.
- The coordinator suggested a docker-compose file that also runs 2 or 3 quotad copies. **I suggest we do not do that yet.** The bench already starts quotad as normal processes on localhost, which AGENTS.md asks for. Putting quotad in containers means we rebuild an image after every code change, and it adds no new test. The bench can start 3 quotad processes on 3 ports that share one Redis. That is "many machines" for our purpose: what matters is 3 separate processes with separate memory.
- A Dockerfile for people who deploy quotad is a good idea later. It is not part of this step.

## The idea

Move the **counts** into Redis. Keep the **lines** (held requests) on each machine.

- Counts: each resource's remaining, reset time and reserved units; requests in flight; points; the pause after a 429. Every copy must see the same counts, so they live in Redis.
- Lines: the requests that a copy is holding. Only that copy can wake them, so they stay in its memory, as today.

When a held request reaches the front of its line, the copy asks Redis: "may I send one request on resource X at cost Y?" Redis checks all four rules and answers yes (and reserves) or no (and says when to ask again). When the answer comes back from GitHub, the copy tells Redis the headers, as today's `Done` does.

## Design decisions

### Decision 1: How do we make each update atomic?

Two copies may ask "may I send?" at the same moment. If both read "1 left" and both write "0 left", we send 2. So "check and reserve" must be one atomic step.

**Option A: Lua scripts.** One script checks all four rules and reserves, in one round trip. Another script applies an answer's headers.
- Good: one round trip per step. Redis runs it whole, so no other copy can slip in.
- Good: the logic is the same as today's Go code, just written in Lua.
- Bad: some logic lives in a second language. We test it against a real Redis to keep it honest.

**Option B: WATCH and MULTI (try, and retry if someone else changed it).** Read the counts, then write them only if nobody changed them in between.
- Bad: under load, many copies collide and retry again and again. That is slowest exactly when the budget is tight.

**Option C: a lock in Redis.** Take the lock, read, write, give it back.
- Bad: 3 or more round trips per request. If a copy dies while it holds the lock, everyone waits for the lock to time out.

**I recommend Option A (Lua).** Two scripts: `admit` and `answer`. Each is about 60 lines.

### Decision 2: Whose clock?

Resets, points windows and pauses all compare times. Three machines have three slightly different clocks.

- **I recommend Redis's clock.** Each script reads the time from Redis (`TIME`), so all copies use one clock. GitHub's reset times are absolute (seconds since 1970), so they compare fine against Redis's clock. We keep today's `-reset-guard` for the gap between Redis's clock and GitHub's.
- For tests, the script takes an optional "now" from the caller. Tests pass a fake time, so they stay fast and exact without sleeping (AGENTS.md test rule).

### Decision 3: What if a copy crashes while its requests are in flight?

A copy reserves a unit and a points slot, then dies before the answer. Nobody gives them back. After a few crashes, the in-flight count is stuck high and the token stops.

**Leases.** Each in-flight request is one entry in a Redis sorted set (a list kept in order by a number), scored by its lease end time. The `admit` script first throws out expired entries. An expired entry counts like today's `Failed`: the unit stays spent (GitHub may have counted it), and its points stay for one more window.

- Lease length: a new setting, `-lease`, default 2 minutes.
- So that no real request outlives its lease, the proxy gets an upstream timeout of the same length. (Today the proxy has no upstream timeout at all.)

PROJECT.md already planned this ("concurrency leases in a sorted set with deadlines").

### Decision 4: How does a held request learn that budget is free again?

Today a held request wakes when a local answer comes back, or at a timer (reset, end of pause, points leaving the window). With Redis, budget can free up because of an answer on **another** machine.

**Option A: the `admit` script says when to ask again.** If the answer is "no", it also returns the earliest time that a rule can start passing: the reset, the end of the pause, or the moment the oldest points leave the window. The copy sets a timer for that time. If the only thing blocking is in-flight requests (their answers come at unknown times), it says "ask again in 50 ms".
- Good: no extra Redis feature. Each wait costs at most one Redis call per 50 ms per token (not per request: only the head of each line asks).
- Bad: up to 50 ms extra wait when the in-flight cap is the thing that blocks. That cap (100) rarely blocks.

**Option B: Redis pub/sub.** Each answer sends a "something freed up" message to all copies.
- Good: wake at once.
- Bad: a second kind of connection to manage, and messages are lost if a copy is briefly disconnected, so we would need Option A's timers anyway.

**I recommend Option A.** Simple, and it cannot miss a wake-up.

### Decision 5: What happens if Redis is down?

AGENTS.md settles this: **never stop serving; fall back to a local share.**

- Each copy writes "I am alive" to Redis every 2 seconds. So each copy knows how many copies there are (call it N).
- If a Redis call fails or takes longer than `-redis-timeout` (default 100 ms), the copy switches to **local mode**: its own in-memory budget, with 1/N of the last known remaining on each resource, 1/N of the in-flight cap, and 1/N of the points. Any pause it knows about still holds.
- It tries Redis again every second.
- **Merge on return.** When Redis answers again, the copy writes what it learned while it was alone: for each resource, the lowest remaining it saw for that window (the "lowest is newest" rule still holds), the points it spent in the last window, and any pause. Then it switches back to shared mode.

Example: 3 copies, Redis says 300 left, then Redis dies. Each copy may send 100. Together they send at most 300, so 0 rule breaks, but a copy that is busy cannot borrow a quiet copy's share. That is the cost of being safe.

**I suggest we build this in a second PR**, after the shared budget works. Until then, if Redis is down, a copy acts like today's solo quotad and logs a loud warning. That can overspend, but it never stops serving.

### Decision 6: Which Redis library? (new dependency, needs your sign-off)

quotad has no dependencies today.

**Option A: `github.com/redis/go-redis/v9`.** The official Go client for Redis.
- Good: connection pool, reconnects, timeouts, and a helper that sends a script once and then calls it by its short hash.
- Bad: one dependency (plus two small ones it pulls in).

**Option B: write our own small client.** Redis's wire format is simple text.
- Good: no dependency.
- Bad: about 200 lines that are not quotad's job: a pool, timeouts, reconnects, parsing replies. Easy to get subtly wrong.

**I recommend Option A.** It is the standard choice, and the code we would write ourselves would be worse.

### Decision 7: Fairness across machines

Today, one copy serves its oldest held request first. Across copies there is no shared line: the copy whose `admit` reaches Redis first wins.

- Good: no shared line to keep in Redis. A dead copy cannot block a shared line.
- Bad: a copy that asks more often can get a little more than its fair share during a tight moment.

**I recommend we accept this.** Within each copy the order stays fair. Priorities (critical first) come in a later step, and they are a per-copy rule too.

### Decision 8: How the code changes

- `budget.Budget` keeps its lines and timers. The counts move behind a small interface with four methods: try to admit, apply an answer, mark a failure, and pause. Two versions: memory (today's code, moved, not changed) and Redis (the Lua scripts).
- All of today's budget tests must pass unchanged against the memory version. The same tests run against the Redis version when a test Redis is available (`QUOTAD_TEST_REDIS=redis://127.0.0.1:6379`), and are skipped otherwise.
- Redis keys use a hash of the token, not the token itself, so no secret is stored in Redis. All of one token's keys share a `{hash}` tag, which keeps them on one server if someone uses Redis Cluster (the scripts need that).
- New flags on `quotad`: `-redis` (empty = memory only), `-lease`, `-redis-timeout`.

## Cost per request

Two Redis round trips per request: one to admit, one to apply the answer. That is well under a millisecond on localhost and about 1 to 2 ms in one data center. GitHub calls take 100 ms or more, so this is small.

## Plan

1. Move the counts behind the interface; memory version only. → check: every existing test passes, `go test -race` clean.
2. Redis version with the two Lua scripts and leases; `-redis` flag. → check: the same budget tests pass against a real Redis; a test where two `Budget`s share one Redis and together never pass the remaining count; a crashed-copy test where a lease runs out.
3. Bench: `-proxies N` starts N quotad copies, and loadgen spreads its agents across them. `-redis` on or off. → check: the success check at the top (3 copies, demands 0.5, 1, 2, 5; 5 runs each).
4. (Second PR) Redis-down fallback and merge on return. → check: a bench run that stops Redis in the middle; 0 rule breaks while it is down, and no request turned away only because Redis is down.

## Questions for you

1. Docker only for Redis (one `docker run` line), quotad as normal processes: OK?
2. Lua scripts, Redis's clock, leases, and "ask again at" timers: OK?
3. May we add `go-redis` as a dependency?
4. Redis-down fallback in a second PR: OK?
