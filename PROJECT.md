# quotad

**One sentence:** A small per-machine proxy that learns each API provider's real remaining quota from response headers, shares one budget across every agent, and spends it on critical calls first, so agents never send requests while rate limited.

## Problem

Many AI agents often share one token (a GitHub App, an LLM API key). Each agent retries on its own, so when the quota runs out they all hit the limit together, retry together right after the reset, and keep sending requests while limited. That wastes quota, risks bans, and makes important calls wait behind background polling.

Existing tools don't solve this:
- Gateways (agentgateway, Zuplo, Kong) enforce limits you type in by hand.
- Truto normalizes rate-limit headers but leaves coordination to you.
- LiteLLM shares LLM counters and has priority reservations, but its limits are hand-configured and it can overshoot.

**quotad's edge:** budgets learned from headers, no overshoot, pacing instead of rejecting, and one budget across an agent's LLM and tool traffic.

## Architecture

```text
agents ──base URL──> quotad proxy (one per machine) ──> upstream API
                          │
                     budget store
              in-memory (solo, default)
              Redis (many machines, v0.2)
```

Agents need no code changes: they point their API base URL at the local proxy.

## Decisions

| Topic | Decision |
|---|---|
| Style | Open-source product, not research. No formal verification or TLA+ |
| License | Apache-2.0 (confirmed 2026-10-20) |
| Language | Go, one module (`cmd/` programs, `internal/` packages) |
| Where it runs | A proxy on each machine. Solo mode (in-memory, no Redis) by default |
| Sharing a budget | **Reserve, then correct:** reserve the estimated cost before sending, correct from headers. GitHub: within a window `used` only goes up (take the max). LLMs (refilling buckets): headers may only lower our estimate |
| Many machines | Redis. Concurrency leases in a sorted set with deadlines so a crashed agent's slot expires. Not needed for the hourly budget |
| Redis down | Fall back to a local share (last known remaining ÷ number of proxies) |
| Priorities | `X-Governor-Priority: critical`; background calls stop at 20% remaining |
| No budget left | Hold up to 30s, release in priority order; longer waits get an immediate `429` with `Retry-After` |
| Duplicate reads | Merge identical in-flight GETs per machine; use ETags (a GitHub `304` doesn't count against the primary limit) |
| LLM limits | Built into quotad, not contributed to LiteLLM |
| Test bench | Real clock with a shrunken window (e.g. "hour" = 60s), real HTTP between separate processes, 5 runs per configuration with ranges reported |

## Build plan

1. **Prove the problem:** fake GitHub server + load generator + two baseline retry strategies (backoff with jitter; header-aware but alone). Talk to 3–5 teams running many agents on one token.
2. **v0.1, one machine:** proxy that reads headers and holds requests at 0 remaining; priority headroom; dashboard and Prometheus-style `/metrics`; binary, Docker image, README quickstart; release.
3. **v0.2, many machines:** shared budget in Redis; concurrency leases and points per minute; Redis-outage fallback.
4. **LLM limits:** fake LLM provider; token reserve-then-correct; read usage from the end of streams.
5. **v0.3:** merge duplicate reads + ETags; real GitHub test on a throwaway account; launch post.

## Success targets

- 0 requests sent while limited.
- At least 90% of the quota used.
- Critical-call p95 wait at least 50% below the best baseline.

**Kill criterion:** if the best baseline already gets 0 violations with low critical waits, stop.

## Open questions

1. Why Go is not written down. Likely reasons: goroutines handle many held requests cheaply, the standard library has a production HTTP server and reverse proxy, and it builds to a single binary.
2. LLM details (not blocking until step 4): which provider first (suggested Anthropic), how much to reserve per call, and whether to add OpenAI's `stream_options.include_usage` to streamed requests.
