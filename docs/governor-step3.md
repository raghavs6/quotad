# Governor Step 3: Secondary Limits

Written 2026-10-21 for sign-off before any code. This is the second build step of chunk 2 (v0.1 on one machine).

## Goal

Get **all** rule breaks to 0, for both agent types, at every load level.

After step 2, quotad stops every 403. But some agents still break the rules with 429s. Step 3 removes those 429s.

## Words used in this document

- **Upstream:** the real API behind quotad. In the bench, it is the fake GitHub server.
- **Main budget (primary limit):** the number of requests a key may send per hour. Step 2 handles it.
- **Secondary limits:** two extra GitHub rules on top of the main budget:
  - **In-flight limit:** "in flight" means a request was sent and its answer has not come back yet. GitHub allows only a fixed number in flight at the same time.
  - **Points limit:** each request costs points. A read costs 1 point, and a write costs 5. GitHub allows only a fixed number of points per minute.
- **429:** the answer GitHub sends when a request breaks a secondary limit.
- **Retry-after:** a wait time that comes with a 429. The key must not send anything until that time is over.
- **Rule break (violation):** a request that reaches the upstream while the key is limited.

## What causes the 429s today

I ran a test to find out. It used header-aware agents at 2× load, through quotad, for 3 windows. I ran it three ways:

| Fake server setting | Rule breaks |
|---|---|
| Normal (in-flight limit 10, points limit on) | 937 |
| Points limit off | 906 |
| In-flight limit off | 117 |

So the in-flight limit causes most of the 429s. The points limit causes the rest.

The 429s also come in long storms. After a reset, there were about 11 rule breaks every second for about 30 seconds. Here is why. Agents with a backlog send requests back to back. More than 10 requests are in flight, so GitHub sends a 429. Only that one agent waits. The other agents keep sending during the wait, and each of those requests is a rule break. Then the next 429 starts the same thing again.

## The idea of step 3

Think of a small shop with a door guard. The shop allows only 10 people inside at a time. It also allows only so many purchases per minute.

Today the guard counts only daily tickets (the main budget). Step 3 teaches the guard two more things:
1. Count the people inside. Let a new person in only when someone leaves.
2. If the shop ever shouts "stop!" (a 429), stop **everyone** at the door, not only the person who heard it.

## What gets built

Everything goes into the code that step 2 built:

```text
internal/budget/   # + in-flight count, points count, shared pause
internal/proxy/    # + read the request type (read or write), pass 429s to the budget
cmd/quotad/        # + flags for the two limits
cmd/bench          # + passes the bench's limits to quotad
```

Agents do not change.

## Design decisions

### Decision 1: How does quotad know the in-flight limit and the points limit?

This is the most important decision.

GitHub's answer headers carry the main budget. They do **not** carry the in-flight limit or the points limit. So quotad cannot read these two numbers. It must get them another way.

**Option A: a setting with GitHub's own number as the default.** quotad has two flags: `-max-in-flight` and `-points-per-minute`. Their defaults are the numbers that GitHub publishes: 100 in flight, and 900 points per minute. The bench sets them to the bench's smaller numbers.
- Good: quotad never breaks the rule to find the number. So 0 rule breaks is possible.
- Good: simple to build and to test.
- Bad: if GitHub changes its numbers, someone must change the setting.

**Option B: learn the number by testing it.** quotad starts high. After each 429, it cuts its limit in half. After a period with no 429s, it raises the limit by one. (This is how internet traffic control finds a safe speed.)
- Good: no setting to keep up to date.
- Bad: quotad learns the number only by breaking the rule. Each 429 is a rule break. So the goal of 0 rule breaks can never be met.
- Bad: the limit keeps going up and down, and that is harder to test.

**I recommend Option A.** Our goal is 0 rule breaks, and Option B cannot reach that goal. Option A is also different from the "limits you type in by hand" that we criticize in other tools. Those tools ask the user to type in the main budget, which GitHub already sends in every answer. Here, GitHub does not send these two numbers at all, so a published default is the best that anyone can do.

Raghav checked these numbers against [GitHub's documentation](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) on 2026-10-21. They are correct, with three details:
- The 100 in-flight limit is shared by the REST and GraphQL APIs. quotad handles only REST for now, so this does not change anything yet.
- Most reads cost 1 point and most writes cost 5, but some endpoints differ. quotad uses 1 and 5 for every request. If an endpoint costs more, the shared pause (Decision 3) is the safety net.
- GitHub has other hidden limits too (for example, on server work time). quotad cannot count those, so the shared pause is the safety net for them as well.

### Decision 2: How to count points over time

GitHub counts points "per minute". We do not know when GitHub's minute starts.

**Option A: a sliding window.** quotad remembers each request's time and cost for the last minute. It lets a request through only if the points in **any** one-minute stretch stay under the limit.
- Good: safe no matter when GitHub's minute starts.
- Bad: sometimes slightly slower than needed.

**Option B: a fixed window.** quotad resets its point count at the start of each minute on its own clock.
- Good: simpler.
- Bad: if GitHub's minute starts at a different second, quotad can let through almost double the points in a short stretch. That can cause 429s.

**I recommend Option A.** It is the only option that is safe without knowing GitHub's clock.

### Decision 3: What to do when a 429 arrives anyway

With the right limits, 429s should not happen. But a 429 can still come. For example, another program might use the same key.

**Recommendation: one shared pause.** When any request on a key gets a 429, quotad stops all requests on that key until the retry-after time is over. Requests that arrive during the pause wait in the line. If the pause is longer than the maximum wait, quotad answers them with its own 429 at once.

quotad follows GitHub's order for how long to wait:
1. If the 429 has a retry-after, wait that long.
2. Otherwise, if the main budget is at 0, wait until the reset.
3. Otherwise, wait one "minute". In the bench, a minute is 1 second.
4. If 429s keep coming, double the wait each time (for example 60, 120, then 240 seconds). This is called exponential backoff. The wait goes back to normal after a request succeeds.

The other option is to keep today's behavior, where only one agent waits. Today's numbers show that this causes the storms, so I do not recommend it.

### Decision 4: Spread out the agents' return times?

In my last message, I said that quotad must spread out the agents' return times. I now think this is **not needed**, and I want to tell you why.

Today, many agents come back at the same second, and they all go to the upstream together. After step 3, they still come back together. But they wait in quotad's line, and quotad lets them in one at a time, within the limits. So the rush stays inside quotad and never reaches GitHub.

So I recommend that we do **not** add random spreading. That keeps step 3 smaller.

### Decision 5: One line or several lines?

A request now must pass four checks: main budget, in-flight count, points, and no pause.

**Recommendation: one line per key, first come, first served.** The first request in the line goes when all four checks pass. No request jumps ahead. This keeps the order fair now, and step 4 can add priorities to this one line.

### Left for later: spacing out writes

GitHub also asks clients to send frequent writes at least one second apart. This is part of GitHub's limits on creating content, and those are not in the fake server yet. So I suggest that this goes into a later step, together with those limits.

## Failure cases

- **An agent crashes, or gives up while it waits:** it leaves the line and frees nothing, because it held nothing. This already works from step 2.
- **An agent gives up after its request is sent:** the request is still in flight at GitHub until GitHub answers. So quotad keeps it in the in-flight count until the upstream answers or the connection closes.
- **A burst right after a reset:** the in-flight limit and the points limit make the burst wait in quotad's line.
- **The upstream does not answer:** the in-flight count goes down when the connection fails or times out. quotad still counts the points as spent.

## How we will know step 3 is done

1. **Unit tests** for the new rules, with a fake clock:
   - With an in-flight limit of 2, the 3rd request waits until one answer comes back.
   - With 7 points per minute, a write (5) and two reads (1 each) pass, and a third read waits.
   - Points from more than one minute ago no longer count.
   - A 429 with retry-after 3 seconds stops all requests for 3 seconds.
   - A 429 without retry-after stops all requests for one "minute".
   - A second 429 in a row without retry-after doubles the pause, and a success resets it.
   - The line stays in first-come order across all four checks.
2. **The bench through quotad, 5 runs per setting:** **0 rule breaks in every run**, at every load level, for both agent types. The main budget use stays at 90% or more at 1× load and above.
3. **If any rule break is left,** I find the cause before I call step 3 done.

## Sign-off (2026-10-21)

Raghav approved the step and all five recommendations, and he checked GitHub's numbers against GitHub's documentation (see Decision 1).

## A change made while building

The first test runs still had a few points 429s. quotad counted a request's points from the moment it **sent** the request. But GitHub counts them when the request **arrives**, which is a little later, and the delay changes from request to request. So now a request's points count from when quotad sends it until one full window after its answer comes back. GitHub counted the request somewhere in between, so this is always safe.
