package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis, when set, makes newTestWith build budgets that count in Redis.
// Tests set it only through TestRedis, one test at a time.
var testRedis *redis.Client

// redisClient connects to the Redis named by QUOTAD_TEST_REDIS (for example
// redis://127.0.0.1:6379/15), or skips the test if it is unset.
func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("QUOTAD_TEST_REDIS")
	if url == "" {
		t.Skip("QUOTAD_TEST_REDIS not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis at %s: %v", url, err)
	}
	return rdb
}

// randomToken keeps each test's keys apart.
func randomToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "test-" + hex.EncodeToString(b)
}

// newRedisTest returns a budget on token that counts in testRedis by the
// fake clock. Leases are a day long unless cfg says otherwise, so they
// never run out in tests that aren't about them.
func newRedisTest(cfg Config, c *fakeTime, token string) *Budget {
	if cfg.Lease == 0 {
		cfg.Lease = 24 * time.Hour
	}
	return newBudget(withDefaults(cfg), c.now, c.schedule, newRedisCounts(cfg, testRedis, token, c.now, c.now))
}

func redisState(t *testing.T, rc *redisCounts, res string) snap {
	t.Helper()
	ctx := context.Background()
	rdb := rc.rdb.(*redis.Client)
	v, err := rdb.HMGet(ctx, rc.key+":r:"+res, "known", "remaining", "reserved").Result()
	if err != nil {
		t.Fatal(err)
	}
	n := func(x any) int {
		s, _ := x.(string)
		i, _ := strconv.Atoi(s)
		return i
	}
	fly, err := rdb.ZCard(ctx, rc.key+":fly").Result()
	if err != nil {
		t.Fatal(err)
	}
	paused, _ := rdb.HGet(ctx, rc.key+":st", "paused").Int64()
	s := snap{known: v[0] == "1", remaining: n(v[1]), reserved: n(v[2]), inFlight: int(fly)}
	if paused != 0 {
		s.paused = time.UnixMilli(paused)
	}
	return s
}

// TestRedis runs every budget test again with the counts in Redis: one
// quotad copy on Redis must behave exactly like one in memory.
func TestRedis(t *testing.T) {
	testRedis = redisClient(t)
	defer func() { testRedis = nil }()
	for _, tc := range []struct {
		name string
		f    func(*testing.T)
	}{
		{"LearnsFirst", TestLearnsFirst},
		{"HoldsWhenReservationsUseUpRemaining", TestHoldsWhenReservationsUseUpRemaining},
		{"StaleResponseCantRaiseCount", TestStaleResponseCantRaiseCount},
		{"ResetReleasesHeldInOrderUpToLimit", TestResetReleasesHeldInOrderUpToLimit},
		{"TooLongIsTurnedAwayAtOnce", TestTooLongIsTurnedAwayAtOnce},
		{"HeldPastMaxHoldIsTurnedAway", TestHeldPastMaxHoldIsTurnedAway},
		{"ClientGivingUpLeavesTheLine", TestClientGivingUpLeavesTheLine},
		{"FailedRequestStaysSpent", TestFailedRequestStaysSpent},
		{"FailedProbeLetsNextRequestLearn", TestFailedProbeLetsNextRequestLearn},
		{"ResponseWithoutHeadersCountsAsFailed", TestResponseWithoutHeadersCountsAsFailed},
		{"InFlightCap", TestInFlightCap},
		{"PointsCap", TestPointsCap},
		{"PointsCountUntilAWindowAfterTheAnswer", TestPointsCountUntilAWindowAfterTheAnswer},
		{"RetryAfterPausesEveryone", TestRetryAfterPausesEveryone},
		{"PauseWithoutRetryAfterDoublesAndResets", TestPauseWithoutRetryAfterDoublesAndResets},
		{"PauseLongerThanMaxHoldIsTurnedAway", TestPauseLongerThanMaxHoldIsTurnedAway},
		{"LineKeepsArrivalOrderAcrossChecks", TestLineKeepsArrivalOrderAcrossChecks},
		{"SearchAtZeroIsHeldAfterCore", TestSearchAtZeroIsHeldAfterCore},
		{"CoreGoesWhileSearchIsAtZero", TestCoreGoesWhileSearchIsAtZero},
		{"EachResourceLearnsOnItsOwn", TestEachResourceLearnsOnItsOwn},
		{"InFlightCapIsSharedAcrossResources", TestInFlightCapIsSharedAcrossResources},
		{"SecondaryPauseOnSearchPausesCore", TestSecondaryPauseOnSearchPausesCore},
		{"OldestThatCanGoGoesFirstAcrossLines", TestOldestThatCanGoGoesFirstAcrossLines},
		{"AnswerCorrectsTheResourceItNames", TestAnswerCorrectsTheResourceItNames},
	} {
		t.Run(tc.name, tc.f)
	}
}

// twoCopies returns two budgets on one token in one Redis, like two quotad
// copies on two machines, sharing a fake clock. The first has learned the
// core budget: left remaining, resetting in an hour.
func twoCopies(t *testing.T, cfg Config, left int) (*Budget, *Budget, *fakeTime) {
	t.Helper()
	testRedis = redisClient(t)
	t.Cleanup(func() { testRedis = nil })
	if cfg.MaxHold == 0 {
		cfg.MaxHold = 30 * time.Second
	}
	c := &fakeTime{t: t0}
	token := randomToken()
	a, b := newRedisTest(cfg, c, token), newRedisTest(cfg, c, token)
	probe, err := a.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(200, headers(5000, left, t0.Add(time.Hour)))
	return a, b, c
}

func TestRedisCopiesShareOneBudget(t *testing.T) {
	a, b, _ := twoCopies(t, Config{}, 4)
	// The second copy learned nothing itself, but knows the budget at once.
	for i, x := range []*Budget{b, a, b, a} {
		if _, err := x.Acquire(context.Background(), "core", false); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Together they have used all 4. Neither may send a fifth: both know
	// the budget is out until the reset, an hour away.
	for i, x := range []*Budget{a, b} {
		_, err := x.Acquire(context.Background(), "core", false)
		var tl *TooLongError
		if !errors.As(err, &tl) || tl.RetryAfter != 3600 {
			t.Fatalf("copy %d: err = %v, want TooLongError with RetryAfter 3600", i, err)
		}
	}
}

func TestRedisRaceNeverOverspends(t *testing.T) {
	const left = 30
	a, b, _ := twoCopies(t, Config{}, left)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var sent atomic.Int64
	var wg sync.WaitGroup
	for i := range 100 {
		x := a
		if i%2 == 1 {
			x = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := x.Acquire(ctx, "core", false); err == nil {
				sent.Add(1)
			} else if !errors.Is(err, ErrTooLong) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := sent.Load(); n != left {
		t.Fatalf("100 racing requests on two copies sent %d, want exactly %d", n, left)
	}
}

func TestRedisCrashedCopysLeaseRunsOut(t *testing.T) {
	a, b, c := twoCopies(t, Config{MaxHold: 5 * time.Minute, MaxInFlight: 1, Lease: 2 * time.Minute}, 10)
	// Copy a sends a request, then crashes: no answer ever comes.
	if _, err := a.Acquire(context.Background(), "core", false); err != nil {
		t.Fatal(err)
	}
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, held)

	c.advance(time.Minute)
	mustBeHeld(t, held) // the lease still runs
	c.advance(time.Minute)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("after the crashed copy's lease ran out: %v", r.err)
	}
	// The lost request may have counted upstream, so its unit stays spent:
	// 10 - 1 (lost) - 1 (b's, reserved).
	if s := state(t, b, "core"); s.remaining != 9 || s.reserved != 1 {
		t.Fatalf("remaining %d reserved %d, want 9 and 1", s.remaining, s.reserved)
	}
}

func TestRedisLateAnswerAfterLeaseIsNotCountedTwice(t *testing.T) {
	a, b, c := twoCopies(t, Config{MaxHold: 5 * time.Minute, Lease: time.Minute}, 10)
	slow, err := a.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := b.Acquire(context.Background(), "core", false); err != nil { // runs the lease out
		t.Fatal(err)
	}
	slow.Done(200, headers(5000, 8, t0.Add(time.Hour)))
	if s := state(t, a, "core"); s.remaining != 8 || s.reserved != 1 || s.inFlight != 1 {
		t.Fatalf("remaining %d reserved %d in flight %d, want 8, 1 and 1", s.remaining, s.reserved, s.inFlight)
	}
}

func TestRedisDownCountsAlone(t *testing.T) {
	// Nothing listens on port 1. The copy must keep serving, as one quotad.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	defer rdb.Close()
	c := &fakeTime{t: t0}
	b := newBudget(withDefaults(Config{MaxHold: 30 * time.Second}), c.now, c.schedule,
		newRedisCounts(Config{}, rdb, randomToken(), c.now, c.now))
	probe, err := b.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(200, headers(5000, 1, t0.Add(10*time.Second)))
	if _, err := b.Acquire(context.Background(), "core", false); err != nil {
		t.Fatal(err)
	}
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, held)
}

func TestRedisUsesRedisClock(t *testing.T) {
	rdb := redisClient(t)
	b := NewRedis(Config{MaxHold: time.Second}, rdb, randomToken(), AfterFunc)
	probe, err := b.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(200, headers(5000, 1, time.Now().Add(time.Hour)))
	if _, err := b.Acquire(context.Background(), "core", false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(context.Background(), "core", false); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}
}

func TestRedisSnapshotShowsEveryCopysCounts(t *testing.T) {
	a, b, c := twoCopies(t, Config{}, 4)
	if _, err := b.Acquire(context.Background(), "core", true); err != nil {
		t.Fatal(err)
	}
	s := a.Snapshot()
	// The probe (1 point, answered) and b's write (5 points, in flight).
	if s.InFlight != 1 || s.Points != 6 || len(s.Resources) != 1 {
		t.Fatalf("snapshot = %+v, want 1 in flight, 6 points, one resource", s)
	}
	r := s.Resources[0]
	if r.Name != "core" || !r.Known || r.Limit != 5000 || r.Remaining != 4 || r.Reserved != 1 || r.Reset == nil || !r.Reset.Equal(t0.Add(time.Hour)) {
		t.Fatalf("core = %+v, want known, 5000, 4 left, 1 reserved, reset in an hour", r)
	}
	// A window later, the probe's point has left it.
	c.advance(time.Minute)
	if s := a.Snapshot(); s.Points != 5 {
		t.Fatalf("points a window later = %d, want 5", s.Points)
	}
}
