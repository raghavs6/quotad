package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis keeps one token's counts in Redis, so every quotad copy that uses
// the same Redis shares them. Each copy keeps its own lines.
//
// Each change is one Lua script, which Redis runs whole before anything
// else, so two copies can never both take the last unit. The scripts use
// Redis's clock, so the copies' own clocks don't matter.
//
// Keys (P is quotad:{hash of the token}; the token itself is never stored):
//
//	P:st          hash: paused (ms), streak, ptotal (points counted now), seq
//	P:r:<name>    hash per resource: known, probing (lease id), limit,
//	              remaining, reset (unix seconds), reserved
//	P:fly         sorted set of leases, "id|resource|cost", scored by when
//	              the lease runs out (ms)
//	P:pts         sorted set of answered requests' points, "id|cost",
//	              scored by when they were answered (ms)
//
// A lease is how a crashed copy's reservations end: if no answer comes
// before it runs out, admit counts the request as failed.
type redisCounts struct {
	cfg   Config
	rdb   redis.Scripter
	key   string           // P above
	clock func() time.Time // nil: Redis's own clock (tests pass a fake one)

	// Until Redis answers again, the token is counted in memory alone,
	// like a single quotad. That can overspend; it never stops serving.
	mem       *memory
	downUntil time.Time // real time; don't try Redis again before it
}

// redisLease is a lease in Redis: its member in P:fly.
type redisLease string

const (
	// redisPoll is how soon a held request asks again when what holds it
	// is an answer that another copy may get at any moment (an in-flight
	// slot, or another copy's probe).
	redisPoll = 50 * time.Millisecond
	// redisRetry is how long a copy counts alone after a failed Redis call
	// before it tries Redis again.
	redisRetry = time.Second
	// redisTTL drops an idle token's keys. Every window that matters is
	// shorter.
	redisTTL = 24 * time.Hour
	// DefaultLease is how long an admitted request may go unanswered before
	// it counts as failed.
	DefaultLease = 2 * time.Minute
)

// NewRedis returns a budget whose counts live in rdb, shared by every
// quotad copy on token. Config fields left at zero get GitHub's numbers,
// and Lease gets DefaultLease. The token is hashed before it is used in a
// key.
func NewRedis(cfg Config, rdb redis.Scripter, token string, schedule func(time.Duration, func())) *Budget {
	return newBudget(withDefaults(cfg), time.Now, schedule, newRedisCounts(cfg, rdb, token, nil, time.Now))
}

func newRedisCounts(cfg Config, rdb redis.Scripter, token string, clock, now func() time.Time) *redisCounts {
	cfg = withDefaults(cfg)
	if cfg.Lease == 0 {
		cfg.Lease = DefaultLease
	}
	sum := sha256.Sum256([]byte(token))
	// The braces make Redis Cluster keep all of one token's keys on one
	// node, which the scripts need.
	key := "quotad:{" + hex.EncodeToString(sum[:16]) + "}"
	return &redisCounts{cfg: cfg, rdb: rdb, key: key, clock: clock, mem: newMemory(cfg, now)}
}

// nowArg is the time the scripts use: 0 tells them to read Redis's clock.
func (rc *redisCounts) nowArg() int64 {
	if rc.clock == nil {
		return 0
	}
	return rc.clock().UnixMilli()
}

func (rc *redisCounts) ok(err error) bool {
	if err == nil {
		return true
	}
	if time.Now().After(rc.downUntil) {
		log.Printf("quotad: Redis unreachable (%v); counting %s in this copy alone for now", err, rc.key)
	}
	rc.downUntil = time.Now().Add(redisRetry)
	return false
}

func (rc *redisCounts) admit(wants []want) grant {
	if time.Now().Before(rc.downUntil) {
		return rc.mem.admit(wants)
	}
	args := []any{rc.nowArg(), rc.cfg.ResetGuard.Milliseconds(), rc.cfg.MaxInFlight, rc.cfg.Points,
		rc.cfg.PointsWindow.Milliseconds(), rc.cfg.Lease.Milliseconds(), redisPoll.Milliseconds(), redisTTL.Milliseconds()}
	for _, w := range wants {
		args = append(args, w.res, w.cost)
	}
	v, err := admitScript.Run(context.Background(), rc.rdb, []string{rc.key}, args...).Slice()
	if err == nil && len(v) < 1+len(wants) {
		err = fmt.Errorf("admit: short reply %v", v)
	}
	if !rc.ok(err) {
		return rc.mem.admit(wants)
	}
	g := grant{leases: make([]lease, len(wants)), release: map[string]time.Duration{}, wake: ms(v[0])}
	for i := range wants {
		if s, _ := v[1+i].(string); s != "" {
			g.leases[i] = redisLease(s)
		}
	}
	for i := 1 + len(wants); i+1 < len(v); i += 2 {
		name, _ := v[i].(string)
		g.release[name] = ms(v[i+1])
	}
	return g
}

func ms(v any) time.Duration {
	n, _ := v.(int64)
	return time.Duration(n) * time.Millisecond
}

func (rc *redisCounts) answer(l lease, a answer) {
	rl, ok := l.(redisLease)
	if !ok {
		rc.mem.answer(l, a) // admitted while Redis was away
		return
	}
	var flags, reset int64
	if a.backoff {
		flags |= 1
	}
	if a.ok {
		flags |= 2
		reset = a.reset.Unix()
	}
	err := answerScript.Run(context.Background(), rc.rdb, []string{rc.key},
		rc.nowArg(), rc.cfg.MinPause.Milliseconds(), redisTTL.Milliseconds(), string(rl),
		a.status, a.pause.Milliseconds(), flags, a.resource, a.limit, a.remaining, reset).Err()
	// If Redis missed this answer, the lease runs out and counts it as failed.
	rc.ok(err)
}

func (rc *redisCounts) cancel(l lease) {
	rl, ok := l.(redisLease)
	if !ok {
		rc.mem.cancel(l)
		return
	}
	rc.ok(cancelScript.Run(context.Background(), rc.rdb, []string{rc.key}, rc.nowArg(), redisTTL.Milliseconds(), string(rl)).Err())
}

// luaNow is the start of every script: ARGV[1] is the time in ms, or 0 for
// Redis's clock.
const luaNow = `
local P = KEYS[1]
local now = tonumber(ARGV[1])
if now == 0 then
  local t = redis.call('TIME')
  now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local st, fly, pts = P .. ':st', P .. ':fly', P .. ':pts'
`

// admitScript is memory.admit in Lua. ARGV: now, reset guard, max in
// flight, points, points window, lease, poll, key TTL (times in ms), then
// a resource and a cost per want. It returns the wake time, a lease (or
// "") per want, then a resource name and release time for each resource
// with a want left over (times relative to now, in ms).
var admitScript = redis.NewScript(luaNow + `
local guard, maxfly, maxpts = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local win, lease, poll, ttl = tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7]), tonumber(ARGV[8])

local res = {}
local function load(name)
  local r = res[name]
  if r then return r end
  local v = redis.call('HMGET', P .. ':r:' .. name, 'known', 'probing', 'limit', 'remaining', 'reset', 'reserved')
  r = {known = v[1] == '1', probing = v[2] or '', limit = tonumber(v[3]) or 0,
       remaining = tonumber(v[4]) or 0, reset = tonumber(v[5]) or 0, reserved = tonumber(v[6]) or 0}
  -- Start a new window once the old one's reset (plus the guard) has
  -- passed. Assume the full limit until a response says otherwise.
  if r.known and r.reset ~= 0 and now >= r.reset * 1000 + guard then
    r.remaining = r.limit
    r.reset = 0
  end
  res[name] = r
  return r
end

-- Leases that ran out: a copy crashed or lost Redis. The upstream may have
-- counted the request, so the unit stays spent, like Failed.
local dead = redis.call('ZRANGEBYSCORE', fly, '-inf', now, 'WITHSCORES')
for i = 1, #dead, 2 do
  local m = dead[i]
  local id, name, cost = string.match(m, '^(%d+)|(.+)|(%d+)$')
  redis.call('ZREM', fly, m)
  redis.call('ZADD', pts, dead[i + 1], id .. '|' .. cost)
  local r = load(name)
  r.reserved = r.reserved - 1
  if r.known then r.remaining = r.remaining - 1 end
  if r.probing == id then r.probing = '' end
end

local s = redis.call('HMGET', st, 'paused', 'ptotal')
local paused, ptotal = tonumber(s[1]) or 0, tonumber(s[2]) or 0
-- Forget points of requests answered more than a window ago.
local old = redis.call('ZRANGEBYSCORE', pts, '-inf', now - win)
for _, m in ipairs(old) do
  ptotal = ptotal - tonumber(string.match(m, '|(%d+)$'))
end
if #old > 0 then redis.call('ZREMRANGEBYSCORE', pts, '-inf', now - win) end
local nfly = redis.call('ZCARD', fly)

local n = (#ARGV - 8) / 2
local out, blocked, stopped, unknown = {}, {}, false, false
for i = 1, n do
  local name, cost = ARGV[7 + 2 * i], tonumber(ARGV[8 + 2 * i])
  local r = load(name)
  out[i] = ''
  if stopped or blocked[name] then
  elseif (not r.known and r.probing ~= '') or (r.known and r.remaining - r.reserved <= 0) then
    blocked[name] = true
    if not r.known then unknown = true end -- a probe's answer comes when it comes
  elseif now < paused or nfly >= maxfly or ptotal + cost > maxpts then
    stopped = true
    if nfly >= maxfly then unknown = true end -- so do in-flight answers
  else
    local id = tostring(redis.call('HINCRBY', st, 'seq', 1))
    if not r.known then r.probing = id end -- learn first
    r.reserved = r.reserved + 1
    nfly = nfly + 1
    ptotal = ptotal + cost
    out[i] = id .. '|' .. name .. '|' .. cost
    redis.call('ZADD', fly, now + lease, out[i])
  end
end

local wake = 0
local function consider(t)
  if t > now and (wake == 0 or t < wake) then wake = t end
end
local release, held = {}, false
for i = 1, n do
  if out[i] == '' then
    held = true
    local name = ARGV[7 + 2 * i]
    local r = res[name]
    local rel = paused
    if r.known and r.remaining - r.reserved <= 0 and r.reset ~= 0 then
      rel = math.max(rel, r.reset * 1000 + guard)
    end
    release[name] = rel - now
    if r.known and r.reset ~= 0 then consider(r.reset * 1000 + guard) end
  end
end
if held then
  consider(paused)
  local first = redis.call('ZRANGE', pts, 0, 0, 'WITHSCORES')
  if #first > 0 then consider(tonumber(first[2]) + win) end
  if unknown then consider(now + poll) end
end

redis.call('HSET', st, 'ptotal', ptotal)
for name, r in pairs(res) do
  local k = P .. ':r:' .. name
  redis.call('HSET', k, 'known', r.known and 1 or 0, 'probing', r.probing, 'limit', r.limit,
    'remaining', r.remaining, 'reset', r.reset, 'reserved', r.reserved)
  redis.call('PEXPIRE', k, ttl)
end
for _, k in ipairs({st, fly, pts}) do redis.call('PEXPIRE', k, ttl) end

local reply = {0}
if wake ~= 0 then reply[1] = wake - now end
for i = 1, n do reply[#reply + 1] = out[i] end
for name, rel in pairs(release) do
  reply[#reply + 1] = name
  reply[#reply + 1] = rel
end
return reply
`)

// answerScript is memory.answer in Lua. ARGV: now, min pause, key TTL,
// lease, status, pause (ms, 0 = none), flags (1 backoff, 2 headers ok),
// then the headers: resource ("" = the admitted one), limit, remaining,
// reset (unix seconds).
var answerScript = redis.NewScript(luaNow + `
local minpause, ttl, m = tonumber(ARGV[2]), tonumber(ARGV[3]), ARGV[4]
local status, pause, flags = tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7])
local backoff, ok = flags % 2 == 1, flags >= 2
local id, name, cost = string.match(m, '^(%d+)|(.+)|(%d+)$')

if pause > 0 then
  local p = tonumber(redis.call('HGET', st, 'paused')) or 0
  redis.call('HSET', st, 'paused', math.max(p, now + pause))
elseif backoff then
  local s = redis.call('HMGET', st, 'paused', 'streak')
  local p, streak = tonumber(s[1]) or 0, tonumber(s[2]) or 0
  redis.call('HSET', st, 'paused', math.max(p, now + minpause * 2 ^ math.min(streak, 10)))
  redis.call('HINCRBY', st, 'streak', 1)
end

-- Not live: the lease ran out first, and admit already counted it failed.
local live = redis.call('ZREM', fly, m) == 1
local rk = P .. ':r:' .. name
if live then
  redis.call('ZADD', pts, now, id .. '|' .. cost)
  redis.call('HINCRBY', rk, 'reserved', -1)
end
if not ok then
  if live then
    local v = redis.call('HMGET', rk, 'known', 'probing')
    if v[1] == '1' then
      redis.call('HINCRBY', rk, 'remaining', -1)
    elseif v[2] == id then
      redis.call('HSET', rk, 'probing', '')
    end
  end
else
  if live then redis.call('HSET', rk, 'probing', '') end
  if status < 300 then redis.call('HSET', st, 'streak', 0) end
  local tk = rk
  if ARGV[8] ~= '' then tk = P .. ':r:' .. ARGV[8] end
  local limit, remaining, reset = tonumber(ARGV[9]), tonumber(ARGV[10]), tonumber(ARGV[11])
  local v = redis.call('HMGET', tk, 'reset', 'remaining')
  local cur = tonumber(v[1]) or 0
  redis.call('HSET', tk, 'known', 1, 'limit', limit)
  if cur == 0 or reset > cur then
    -- A window we haven't seen yet: take its count as is.
    redis.call('HSET', tk, 'reset', reset, 'remaining', remaining)
  elseif reset == cur then
    redis.call('HSET', tk, 'remaining', math.min(tonumber(v[2]), remaining))
  end
  -- Else a late answer from an older window says nothing about this one.
  redis.call('PEXPIRE', tk, ttl)
end
for _, k in ipairs({st, fly, pts, rk}) do redis.call('PEXPIRE', k, ttl) end
return 1
`)

// cancelScript is memory.cancel in Lua. ARGV: now, key TTL, lease.
var cancelScript = redis.NewScript(luaNow + `
local ttl, m = tonumber(ARGV[2]), ARGV[3]
local id, name, cost = string.match(m, '^(%d+)|(.+)|(%d+)$')
if redis.call('ZREM', fly, m) == 1 then
  local rk = P .. ':r:' .. name
  redis.call('ZADD', pts, now, id .. '|' .. cost) -- never sent; its points simply expire
  redis.call('HINCRBY', rk, 'reserved', -1)
  local v = redis.call('HMGET', rk, 'known', 'probing')
  if v[1] ~= '1' and v[2] == id then redis.call('HSET', rk, 'probing', '') end
end
return 1
`)
