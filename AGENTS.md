# AGENTS.md

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

This project uses AI for implementation without outsourcing engineering understanding: Claude writes the code; I must understand every design decision in it.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

## Who I am (calibrate to this)

Sophomore CS student. Comfortable with Go, Python, Docker, Postgres, Redis, gRPC. Background in systems research: formal verification of HBase's region split protocol (TLA+, invariants, rollback correctness) and TLA+ specs for the CORFU shared log at NYU.

So: I understand distributed systems concepts and protocol reasoning reasonably well. I have less experience with production-grade Go services, HTTP proxies, and benchmarking methodology. Explain build/tooling decisions more than protocol concepts. Don't hand me finished code without explaining the design choice behind it — I'm building this to learn.

## 0. What quotad is (keep every change consistent with this)

An open-source rate-limit governor for AI agent fleets. Agents point their API base URL at a small proxy on their machine. The proxy learns the provider's **real** remaining quota from response headers, shares one budget across agents, spends it on critical calls first, and holds calls instead of rejecting them. It is a product, not a research project: **no formal verification or TLA+.**

Settled decisions (argue against them per section 3 if they're wrong, but don't silently change them):
- **Go, one module.** Layout: `cmd/` for programs (`cmd/fakegithub`, `cmd/loadgen`, later the proxy), `internal/` for packages (`internal/fake`, `internal/agents`, `internal/metrics`, …).
- **Per-machine proxy, in-memory by default.** Solo mode must never need Redis or any other infrastructure.
- **Redis is optional**, only for sharing budgets across machines (v0.2). If Redis is down, fall back to a local share (last known remaining ÷ number of proxies), never stop serving.
- **Reserve, then correct.** Reserve the estimated cost before sending; correct from response headers. GitHub `used` only goes up within a window (take the max). LLM refilling buckets: headers may only *lower* our estimate.
- **Priorities.** `X-Governor-Priority: critical`; background calls stop at 20% remaining.
- **Out of budget.** Hold up to 30s and release in priority order; otherwise an immediate `429` with `Retry-After`.
- **Upstreams in order:** GitHub REST first, then LLM providers after the many-machine version.

## Go conventions

- Standard library first (`net/http`, `net/http/httputil`, `context`, `sync`). Every new dependency needs a stated reason and my sign-off.
- Before any commit: `gofmt -l .` is empty, `go vet ./...` is clean, `go test ./...` passes. Run concurrency code under `go test -race`.
- Every goroutine has a clear owner and a way to stop (`context` cancellation). Every held request respects its client's context, so a caller that gives up frees its slot.
- Window length, budget, and limits are config flags, never hard-coded.

## Test bench rules

- **Real clock, shrunken window** (e.g. "hour" = 60s, budget = 500). No simulated clock: we test the product, not a model of it.
- **Real HTTP on localhost, separate processes.** The fake server and the load generator are two programs, so the proxy slots in between by changing only a base URL.
- **5 runs per configuration, report average and range.** A result counts only if the ranges don't overlap. Never report a single run as a finding.
- Unit tests for rate-limit rules must not sleep for real windows; inject time (pass a `now func() time.Time` or similar) so they're fast and exact. The real clock is for the end-to-end bench.
- Success targets: 0 requests sent while limited, ≥90% of quota used, critical-call p95 wait ≥50% below the best baseline. **Kill criterion:** if the best baseline already gets 0 violations with low critical waits, stop and tell me.

## 1. How we work: explain → agree → build

**No code appears until I've seen the reasoning and signed off on it.** In that order, every step.

- Before major implementation, make sure I understand the goal, inputs, outputs, state, dependencies, and important failure cases.
- Explain the step first: what it does, every design decision in it, the alternatives, why this one.
- Name tradeoffs on both sides, then recommend. Don't pick silently.
- Let me participate in architecture and important design decisions.
- Wait for my go. Then write the code — implement aggressively; don't make me type code or boilerplate for learning's sake.
- Verify against the step's stated check. Then commit and open a PR. Then the next step.
- If an explanation is too big to hold in my head at once, the step is too big — split it.

Section 7 covers *how* to explain. This section is about *when*.

## 2. Tiny, goal-driven, verifiable steps

**Define success criteria. Loop until verified.**

- One new thing per step. If a step has two new things in it, it's two steps.
- Write the success check before the code. Transform tasks into verifiable goals:
  - "Add validation" → "Write tests for invalid inputs, then make them pass"
  - "Fix the bug" → "Write a test that reproduces it, then make it pass"
  - "Refactor X" → "Ensure tests pass before and after"
- For multi-step tasks, state a brief plan:
  ```
  1. [Step] → verify: [check]
  2. [Step] → verify: [check]
  3. [Step] → verify: [check]
  ```
- Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.
- Commit often: one commit per verified step. The message says *why*, not just what.
- One PR per chunk of work (e.g. "fake GitHub + baselines"), not per step: batch the step commits on one branch. Never commit directly to `main`. The PR description explains the why, the design decisions, and how each step was verified.
- Never batch several steps into one turn, even when they're each small.

## 3. Think before coding; push back

**Don't assume. Don't hide confusion. Surface tradeoffs.**

- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them — don't pick silently.
- If something is unclear, stop. Name what's confusing. Ask.
- If a simpler approach exists, say so. Push back when warranted.
- Argue against the plan file or PROJECT.md when they're wrong. An approved plan is not evidence.
- Flag speculative abstractions even when a plan calls for them.
- Push back if I'm scoping badly or building the wrong thing next.
- Correct me clearly when my technical reasoning is wrong.

## 4. Simplicity first

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 5. Surgical changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it — don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 6. Verify claims, don't assert them

- Prove tool, API, and library behavior in the terminal before I act on it.
- Prefer a temporary experiment over an assertion — a throwaway flag, a test pointed at a dead port, a `curl -i` against the fake server to show the actual `x-ratelimit-*` headers.
- Claims about GitHub's or an LLM provider's rate-limit behavior need a source (docs link) or a check against the real API; the fake server is only as good as those claims.
- When a check contradicts you, say so plainly, correct the record, and move on.

## 7. Explain like I'm learning

**The user is learning. Explain every concept at a child-friendly level.**

When introducing or touching any technical concept:
- Use plain, everyday language. Avoid jargon; when a technical term is unavoidable, define it in one short sentence.
- Use simple analogies to ground abstract ideas.
- Break explanations into small steps. One idea per sentence when possible.
- Show *why* something matters before *how* it works.
- After explaining, give a tiny concrete example the user can picture.
- It is okay to be repetitive across sessions — assume the user is still building intuition.

Apply this whenever you:
- Introduce a new library, framework, or pattern.
- Use a term like "state", "API", "async", "component", "hook", "endpoint", etc.
- Make a design decision the user might not have seen before.

## 8. After building: understanding mode

After substantial implementation, switch from building to understanding:
- Trace the important path end-to-end with me.
- Examine relevant failure paths, concurrency issues, and performance implications. For quotad that always includes: a crashed agent, a client that gives up mid-hold, a burst right after a window reset, and (from v0.2) Redis going away.

## 9. Where things live

- `PROJECT.md` — the brief: problem, architecture, decisions, build plan (chunks), success targets, open questions. Seed it from `governor-design.md` and `governor-handoff.md`.
- `docs/` — per-step design write-ups (e.g. `governor-step1.md`), written for sign-off before the code.
- `UPDATES.md` — a log, newest first, gitignored (not committed). Every few steps, add an entry with the design decisions we made and any numbers we ran.
- Longer explanations go in a file, not in chat.
