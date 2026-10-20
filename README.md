# quotad

A rate-limit governor for AI agent fleets.

Agents point their API base URL at a small proxy on their machine. quotad reads the provider's real remaining quota from response headers, shares one budget across all your agents, spends it on important calls first, and briefly holds calls instead of letting agents hammer a rate-limited API.

## Status

Early. Nothing usable yet. The test bench (a fake GitHub server and a load generator) is built and has measured the problem; the proxy comes next. See [PROJECT.md](PROJECT.md) for the plan, [docs/governor-step1.md](docs/governor-step1.md) for the bench design, and [docs/step1-baselines.md](docs/step1-baselines.md) for the baseline numbers.

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
go test -race ./...
```

### Run the bench

```sh
go build -o bin/ ./cmd/...

# One run, by hand:
bin/fakegithub &                          # 500 requests per 60s "hour"
bin/loadgen -strategy header-aware -demand 2

# The full baseline: 2 strategies × 5 demand levels × 5 runs, as a Markdown table.
bin/bench -parallel 8 > results/baseline.md
```

`curl -i -H 'Authorization: token x' localhost:8081/repos/o/r` shows the `x-ratelimit-*` headers; `curl localhost:8081/__stats` shows violation counts.

| Path | What |
|---|---|
| `cmd/fakegithub` | Fake GitHub REST API with GitHub's primary and secondary limits, scaled by flags |
| `cmd/loadgen` | Simulated agents sharing one token |
| `cmd/bench` | Runs every setting 5 times against fresh servers and reports ranges |
| `internal/fake` | The rate-limit rules |
| `internal/agents` | Agent behavior and the two baseline strategies |
| `internal/metrics` | Percentiles, rejections over time, ranges |
