# Per-resource budgets

Written 2026-10-22 for sign-off before any code. Raghav signed off on 2026-10-22 and confirmed from GitHub's docs that `/search/code` counts against `code_search`. This step closes the gap that the real GitHub test found. Priorities (step 4) come after it.

## Goal

quotad keeps a **separate main budget for each GitHub resource** on a token. A fleet that mixes normal calls and searches on one token gets 0 rule breaks, and a used-up search budget never holds a normal call.

## Words used in this document

- **Resource:** one of GitHub's separate budgets on the same token. Each has its own limit and its own reset time. Examples from a real `GET /rate_limit` on 2026-10-22: `core` (normal calls, 15,000 an hour for our app token), `search` (30 a minute), `code_search` (10 a minute), `graphql` (10,000 points an hour). There are 10 more rare ones, such as `dependency_snapshots` and `audit_log`.
- **`x-ratelimit-resource`:** an answer header. It names the resource that the request counted against. A real answer on `/repos/raghavs6/quotad` says `core`.
- **Main budget (primary limit):** the number of requests a resource allows per window.
- **Secondary limits:** the in-flight limit (100) and the points limit (900 a minute). They belong to the whole token, not to one resource.
- **Probe:** the one request that quotad sends first to learn a budget. Other requests wait for its headers.
- **Line (queue):** the requests that quotad holds, oldest first.

## What goes wrong today

quotad keeps one budget per token. It does not read `x-ratelimit-resource`. I ran a throwaway test against today's code with a fake clock. It found two failures.

**Failure 1: rule breaks on search.** A core answer arrives first (4,000 left, reset in 1 hour). Then search runs out (0 left, reset in 1 minute). quotad sees the search reset as "older" than the core reset, so it ignores the 0. Then it sent **5 of 5** more searches while search was at 0. Each one is a rule break.

**Failure 2: normal calls held for no reason.** Search runs out first (0 left). Then a normal call comes. quotad thinks the whole token is at 0. It turned the normal call away with "retry after 60s", even though core had thousands left.

Think of a fair with two ticket booths: one for rides and one for games. Today quotad keeps one count for both. If games sell out, it stops people who want rides. If rides show a big number, it forgets that games sold out.

## The idea

Give each resource its own count, its own reset time, and its own line. Keep one guard at the door for the whole token. The guard still counts the people inside (in-flight) and the points, and still stops everyone after a 429.

## Design decisions

### Decision 1: How does quotad know a request's resource before it sends it?

This is the most important decision. The header names the resource only in the **answer**. But quotad must pick a budget **before** it sends the request.

**Option A: guess from the URL path, then let the answer correct it.** A small table:
- `/search/code` goes to `code_search`.
- Any other `/search/...` goes to `search`.
- `/graphql` goes to `graphql`.
- Everything else goes to `core`.

When the answer comes, quotad applies its headers to the resource that `x-ratelimit-resource` names, even if the guess was different.
- Good: simple, and right for almost all agent traffic.
- Good: the answer still corrects the counts, so a wrong guess does not spoil a budget.
- Bad: the 10 rare resources (for example `dependency_snapshots`, 100 an hour) are guessed as `core`. A fleet that hammers one of them could still break its rule. Agents rarely call these.

**Option B: learn the table from answers.** quotad remembers "this kind of path went to this resource".
- Good: no table to keep up to date.
- Bad: "this kind of path" is hard to define. `/repos/a/b/issues` and `/repos/c/d/issues` are the same kind, but the path text differs. Getting this right is a project of its own.

**Option C: ask `GET /rate_limit` at start.** It lists every resource's numbers, and GitHub says it does not count against the main budget.
- Good: quotad knows every budget before the first request.
- Bad: it still does not say which path goes to which resource. So we need Option A anyway. It also adds a startup call that the fake server would need to copy.

**I recommend Option A.** It fixes the real gap (core plus search) with the least code. If quotad sees an answer whose resource is not the guessed one, it logs a warning, so we learn which rare paths matter before we build more.

**Confirmed:** Raghav checked GitHub's docs on 2026-10-22. `/search/code` counts against `code_search`, and other `/search/...` paths count against `search`.

### Decision 2: Which checks belong to a resource, and which to the whole token?

| Check | Belongs to | Why |
|---|---|---|
| Main budget (remaining, reset) | Resource | GitHub counts each resource on its own. |
| Probe (learn before a burst) | Resource | Each resource has its own numbers to learn. The first search sends its own probe, even if core is known. |
| In-flight limit (100) | Token | GitHub's in-flight limit is shared across REST and GraphQL. |
| Points limit (900 a minute) | Token | GitHub counts points for the whole token. |
| Pause after a 429 | Token | A 429 means a secondary limit, and those belong to the token. |
| 403 with remaining 0 | Resource | That is the main budget of one resource running out. |

### Decision 3: One line, or one line per resource?

Today there is one line per token. Nobody jumps ahead.

**Option A: keep one line.** A search at the front, waiting 50 seconds for search to reset, would block every normal call behind it. That is Failure 2 again, in a new form. (This is called head-of-line blocking: the first in line holds up everyone behind it.)

**Option B: one line per resource.** Each resource keeps first-come order inside its own line. When a shared slot frees up (an in-flight slot, or points), it goes to the oldest waiting request among the lines whose resource has budget.

**I recommend Option B.** A request waits only for its own resource and for the shared limits. Order stays fair: among requests that can go, the oldest goes first.

### Decision 4: An answer with no `x-ratelimit-resource` header

The fake server does not send it today. Some older GitHub Enterprise servers may not either. **Recommendation:** use the guessed resource. The fake server will also start to send the header (see the check below).

### Decision 5: GraphQL

GraphQL gets its own budget from Decision 1. But a GraphQL call can cost many points of that budget, not 1. quotad still reserves 1 and the answer corrects it. **Recommendation:** leave real GraphQL costs for later. quotad handles REST first, as PROJECT.md says.

## What gets built

```text
internal/budget/   # split: a token part (in-flight, points, pause) and one part per resource (budget, probe, line)
internal/proxy/    # guess the resource from the path; pass it to the budget
internal/fake/     # + a "search" resource with its own small limit, and the x-ratelimit-resource header
internal/agents/   # + a setting for the share of calls that are searches
cmd/bench/         # + passes the search share through
```

That is two steps, one commit each, in one PR:
1. The budget split, with unit tests.
2. The fake server's search resource and a mixed-traffic bench run.

## Failure cases

- **An agent crashes or gives up while it waits:** it leaves its resource's line. Nothing else changes.
- **An agent gives up after its request is sent:** the shared in-flight slot stays taken until the upstream answers, as in step 3.
- **A burst right after a reset:** each resource resets on its own. A search reset frees only searches. The shared limits still pace the burst.
- **A wrong guess:** the answer's headers update the right resource. The warning in the log shows the path.
- **No answer at all (timeout):** the guessed resource keeps the unit spent, as today.

## How we will know it is done

1. **Unit tests, with a fake clock:**
   - Failure 1 and Failure 2 from above now pass: 0 searches go out at 0, and a normal call goes at once while search is at 0.
   - The first search sends its own probe while core is already known.
   - With an in-flight limit of 2, one core call and one search fill it, and a third call of either kind waits.
   - A 429 on a search pauses core calls too.
   - Each line keeps first-come order, and the oldest request that can go goes first.
   - An answer whose header names another resource updates that resource, not the guess.
2. **The bench, 5 runs per setting:** a fleet where 20% of calls are searches, at 1x and 2x load. **0 rule breaks in every run.** Budget use stays at 90% or more on core. Core waits do not grow when search is used up.
3. **Optional real check on your laptop:** a short mixed run (core calls well under the limit, plus searches at 2x). 0 rule breaks, and core calls are not held. 2 runs only, to keep load on your account small.

## Sign-off

Raghav gave his go on 2026-10-22, with the path table above. Results: [per-resource-results.md](per-resource-results.md).
