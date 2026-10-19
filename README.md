# quotad

A rate-limit governor for AI agent fleets. Agents send their API traffic through a small local proxy that learns the provider's real remaining quota from response headers, shares one budget across agents, and spends it on critical calls first.

**Status:** step 1 of chunk 1. There's no proxy yet; this is the test bench it will be judged on: a fake GitHub API and simulated agents running two baseline retry strategies. See [docs/governor-step1.md](docs/governor-step1.md) for the design and [docs/step1-baselines.md](docs/step1-baselines.md) for the baseline numbers.

## Run the bench

```sh
go build -o bin/ ./cmd/...

# One run, by hand:
bin/fakegithub &                          # 500 requests per 60s "hour"
bin/loadgen -strategy header-aware -demand 2

# The full baseline: 2 strategies × 4 demand levels × 5 runs, as a Markdown table.
bin/bench -parallel 8 > results/baseline.md
```

`curl -i -H 'Authorization: token x' localhost:8081/repos/o/r` shows the `x-ratelimit-*` headers; `curl localhost:8081/__stats` shows violation counts.

## Layout

| Path | What |
|---|---|
| `cmd/fakegithub` | Fake GitHub REST API with GitHub's primary and secondary limits, scaled by flags |
| `cmd/loadgen` | Simulated agents sharing one token |
| `cmd/bench` | Runs every setting 5 times against fresh servers and reports ranges |
| `internal/fake` | The rate-limit rules |
| `internal/agents` | Agent behavior and the two baseline strategies |
| `internal/metrics` | Percentiles, rejections over time, ranges |

## Tests

```sh
go test -race ./...
```
