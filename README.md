# quotad

A rate-limit governor for AI agent fleets.

Agents point their API base URL at a small proxy on their machine. quotad reads the provider's real remaining quota from response headers, shares one budget across all your agents, spends it on important calls first, and briefly holds calls instead of letting agents hammer a rate-limited API.

## Status

Early: good for trying out, not yet proven in production. The [Quickstart](#quickstart) gets it running. The test bench is built. quotad keeps each key under GitHub's main budgets and its secondary limits (requests in flight, points per minute), and it pauses every agent on a key after a 429. GitHub keeps a separate main budget per resource (core, search, code_search, graphql, ...), and so does quotad. In the bench, no request reaches the upstream while it is limited. Priorities come next. See [PROJECT.md](PROJECT.md) for the plan, [docs/step1-baselines.md](docs/step1-baselines.md) for the baseline numbers, [docs/step3-results.md](docs/step3-results.md) and [docs/per-resource-results.md](docs/per-resource-results.md) for the latest bench results, [docs/per-resource-waits.md](docs/per-resource-waits.md) for waits split by resource, and [docs/real-github-results.md](docs/real-github-results.md) for a first test against the real GitHub API.

## Quickstart

This gets one machine's agents going through quotad in about 10 minutes.

### 1. Install

Pick one.

- **Download** (no Go needed): get the file for your system from [Releases](https://github.com/raghavs6/quotad/releases), unpack it, and put `quotad` on your `PATH`. Downloads appear after the first tagged release.
- **Docker:** `docker run -p 8080:8080 ghcr.io/raghavs6/quotad:latest` (published with each release), or build it yourself with `docker build -t quotad .`
- **Go 1.24+:** `go install github.com/raghavs6/quotad/cmd/quotad@main` (use `@latest` once a release is tagged)

### 2. Start it

```sh
quotad                     # listens on 127.0.0.1:8080, forwards to https://api.github.com
```

Check that it is up:

```sh
curl localhost:8080/__quotad/health
```

### 3. Point your agents at it

Change only the API base URL from `https://api.github.com` to `http://localhost:8080`. Keep the token as it is. quotad passes it through and keeps one budget per token.

```sh
# curl
curl -H "Authorization: Bearer $GITHUB_TOKEN" http://localhost:8080/repos/OWNER/REPO
```

```js
// Octokit (JavaScript)
const octokit = new Octokit({ auth: process.env.GITHUB_TOKEN, baseUrl: "http://localhost:8080" });
```

```python
# PyGithub
g = Github(base_url="http://localhost:8080", auth=Auth.Token(os.environ["GITHUB_TOKEN"]))
```

```go
// go-github
client := github.NewClient(nil).WithAuthToken(token)
client.BaseURL, _ = url.Parse("http://localhost:8080/")
```

The `gh` CLI does not work through quotad yet: it only talks to github.com or to GitHub Enterprise hosts over HTTPS.

Traffic between your agents and quotad is plain HTTP and carries your token. Keep quotad on the same machine (the default) or a private network.

### 4. Watch it

```sh
curl localhost:8080/__quotad/stats
```

This shows how many requests quotad saw, sent, and turned away, and, for each token, how much of each budget (core, search, ...) is left and how many calls are waiting. Tokens are never shown, only a short hash of each one.

### Settings

Every flag can also be set by an environment variable: `QUOTAD_` plus the flag name in capitals with `-` as `_`. A flag on the command line wins. Run `quotad -h` for the full list.

| Flag | Variable | Default | What |
|---|---|---|---|
| `-listen` | `QUOTAD_LISTEN` | `127.0.0.1:8080` (`0.0.0.0:8080` in Docker) | Address to listen on |
| `-upstream` | `QUOTAD_UPSTREAM` | `https://api.github.com` | API to forward to |
| `-max-hold` | `QUOTAD_MAX_HOLD` | `30s` | Longest a call waits for budget; after that quotad answers 429 with `Retry-After` |
| `-reset-guard` | `QUOTAD_RESET_GUARD` | `1s` | Extra wait past the reset time, for clock differences |
| `-max-in-flight` | `QUOTAD_MAX_IN_FLIGHT` | `100` | Calls sent but not yet answered, per token |
| `-points` | `QUOTAD_POINTS` | `900` | Points per points window, per token (read 1, write 5) |
| `-points-window` | `QUOTAD_POINTS_WINDOW` | `1m` | The points window |
| `-min-pause` | `QUOTAD_MIN_PAUSE` | `1m` | Pause after a secondary-limit answer without `Retry-After` |

The secondary-limit defaults are GitHub's published numbers.

### Limits today

- Budgets live in memory. Each quotad process keeps its own, so two machines that share a token do not see each other's use. Shared budgets across machines (Redis) are planned, not built.
- Budgets reset when quotad restarts. It learns them again from the next response's headers.
- Priorities (`X-Governor-Priority`) are not built yet.

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

CI runs the same tests, `go vet`, and a Docker build on every pull request.

### Release

Push a version tag to publish downloads for Linux, macOS, and Windows, and a Docker image at `ghcr.io/raghavs6/quotad`:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`.goreleaser.yaml` sets up the downloads. `goreleaser release --snapshot --clean` builds them locally without publishing.

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
| `cmd/quotad` | The proxy. Flags and variables are under [Settings](#settings); `/__quotad/health` and `/__quotad/stats` are its own pages |
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
