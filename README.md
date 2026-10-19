# quotad

A rate-limit governor for AI agent fleets.

Agents point their API base URL at a small proxy on their machine. quotad reads the provider's real remaining quota from response headers, shares one budget across all your agents, spends it on important calls first, and briefly holds calls instead of letting agents hammer a rate-limited API.

## Status

Early. Nothing usable yet. We're building the test bench (a fake GitHub server and a load generator) to measure the problem before building the proxy. See [PROJECT.md](PROJECT.md) for the plan.

## How it will work

```text
agent ──> quotad (local proxy) ──> GitHub / LLM API
            │
            └── shared budget (in-memory; Redis when spanning machines)
```

- **No code changes in agents:** point their API base URL at quotad.
- **Learns limits from headers:** no hand-typed limits to keep in sync.
- **Priorities:** tag a call with `X-Governor-Priority: critical`; background calls stop at 20% remaining.
- **Pacing, not rejecting:** when the budget is out, calls wait up to 30s and are released in priority order.

First upstream is GitHub REST. OpenAI and Anthropic come later.

## Development

Requires Go.

```sh
go test ./...
```
