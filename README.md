# quotad

A rate-limit governor for AI agent fleets.

Agents point their API base URL at a small proxy on their machine. quotad reads the provider's real remaining quota from response headers, shares one budget across all your agents, spends it on important calls first, and briefly holds calls instead of letting agents hammer a rate-limited API.

## Status

Early, not ready for production. The test bench is built. quotad keeps each key under GitHub's main budgets and its secondary limits (requests in flight, points per minute), and it pauses every agent on a key after a 429. GitHub keeps a separate main budget per resource (core, search, code_search, graphql, ...), and so does quotad. In the bench, no request reaches the upstream while it is limited. Priorities come next. See [PROJECT.md](PROJECT.md) for the plan, [docs/step1-baselines.md](docs/step1-baselines.md) for the baseline numbers, [docs/step3-results.md](docs/step3-results.md) and [docs/per-resource-results.md](docs/per-resource-results.md) for the latest bench results, and [docs/real-github-results.md](docs/real-github-results.md) for a first test against the real GitHub API.

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

# Mixed traffic: agents also search, which GitHub counts apart from core.
bin/bench -proxy -searches -parallel 8 > results/quotad-searches.md
```

`curl -i -H 'Authorization: token x' localhost:8081/repos/o/r` shows the `x-ratelimit-*` headers; `curl localhost:8081/__stats` shows violation counts.

| Path | What |
|---|---|
| `cmd/quotad` | The proxy: `-upstream`, `-listen`, `-max-hold`, `-reset-guard`, and the secondary limits `-max-in-flight`, `-points`, `-points-window`, `-min-pause` (defaults are GitHub's numbers) |
| `cmd/fakegithub` | Fake GitHub REST API with GitHub's primary and secondary limits, scaled by flags. `/search/...` has its own primary limit, like GitHub's search resource |
| `cmd/loadgen` | Simulated agents sharing one token |
| `cmd/realtest` | The same agents against the real GitHub API, directly or through quotad; counts every answer itself. `scripts/real-search-test.sh` runs the full real test |
| `cmd/bench` | Runs every setting 5 times against fresh servers and reports ranges |
| `internal/budget` | One token's limits: a primary budget and line per resource (reserve, then correct from headers), and the token's in-flight cap, points, shared pause; hold and release |
| `internal/proxy` | quotad's HTTP layer around `httputil.ReverseProxy`; guesses each request's resource from its path |
| `internal/fake` | The rate-limit rules |
| `internal/agents` | Agent behavior and the two baseline strategies |
| `internal/metrics` | Percentiles, rejections over time, ranges |

## License

Apache-2.0. See [LICENSE](LICENSE).
