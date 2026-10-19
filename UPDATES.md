# Updates

## 2026-10-19

Built step 1: the fake GitHub server, the simulated agents, and the bench. At 2× demand, backoff agents send ~218 requests while limited per 3-window run, and header-aware agents send ~748 (mostly into secondary-limit 429s), so the kill criterion doesn't fire. See [docs/step1-baselines.md](docs/step1-baselines.md).
