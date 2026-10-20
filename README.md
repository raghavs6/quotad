# quotad

A rate-limit governor for AI agent fleets.

Agents point their API base URL at a small proxy on their machine. quotad reads the provider's real remaining quota from response headers, shares one budget across all your agents, spends it on important calls first, and briefly holds calls instead of letting agents hammer a rate-limited API.

## Status

Early, not ready for production. The test bench is built, and the first proxy step works: quotad holds requests once a token's main budget is gone, so no request reaches the upstream while that limit is in force. Secondary limits (concurrency, `retry-after`) and priorities come next. See [PROJECT.md](PROJECT.md) for the plan, [docs/step1-baselines.md](docs/step1-baselines.md) for the baseline numbers, and [docs/step2-results.md](docs/step2-results.md) for the proxy's first results.

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

# The same, with quotad between the agents and the fake server.
bin/bench -proxy -parallel 8 > results/quotad.md
```

`curl -i -H 'Authorization: token x' localhost:8081/repos/o/r` shows the `x-ratelimit-*` headers; `curl localhost:8081/__stats` shows violation counts.

| Path | What |
|---|---|
| `cmd/quotad` | The proxy: `-upstream`, `-listen`, `-max-hold`, `-reset-guard` |
| `cmd/fakegithub` | Fake GitHub REST API with GitHub's primary and secondary limits, scaled by flags |
| `cmd/loadgen` | Simulated agents sharing one token |
| `cmd/bench` | Runs every setting 5 times against fresh servers and reports ranges |
| `internal/budget` | One token's budget: reserve, then correct from headers; hold and release |
| `internal/proxy` | quotad's HTTP layer around `httputil.ReverseProxy` |
| `internal/fake` | The rate-limit rules |
| `internal/agents` | Agent behavior and the two baseline strategies |
| `internal/metrics` | Percentiles, rejections over time, ranges |

## License

Apache-2.0. See [LICENSE](LICENSE).
