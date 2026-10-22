// Command bench runs the step 1 baselines: for every strategy and demand
// level it starts a fresh fakegithub, runs loadgen against it over real
// HTTP, and repeats each setting -runs times. It prints a Markdown table of
// averages and ranges, and saves every run as a JSON line.
//
// With -proxy, quotad sits between loadgen and fakegithub. Violations are
// still counted by fakegithub, so they mean the same thing either way.
//
// With -searches, the agents also search, at the same demand multiple of
// the search budget (GitHub's 30 a minute, scaled like the rest). Search is
// its own resource, so this tests mixed traffic on one token. With
// -search-demands, the search multiple is set on its own instead, so core
// can stay under its budget while search runs out. Runs with searches also
// print the critical calls' waits split by resource.
//
// With -proxies N, each run starts N quotad copies, like N machines, and
// spreads the agents across them. Without -redis each copy counts alone;
// with -redis they share one budget through it.
//
// Build the programs first: go build -o bin/ ./cmd/...
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raghavs6/quotad/internal/metrics"
)

// run is the part of loadgen's -json output the bench reads.
type run struct {
	Strategy     string  `json:"strategy"`
	Demand       float64 `json:"demand"`
	SearchDemand float64 `json:"search_demand"`
	Seed         uint64  `json:"seed"`
	Server       struct {
		Violations   int `json:"violations"`
		Primary403   int `json:"primary_403"`
		Secondary429 int `json:"secondary_429"`
	} `json:"server"`
	Utilization float64         `json:"utilization"`
	SearchUtil  float64         `json:"search_utilization"`
	Agents      metrics.Summary `json:"agents"`
}

type setting struct {
	strategy     string
	demand       float64
	searchDemand float64
}

func main() {
	bin := flag.String("bin", "bin", "directory holding the fakegithub and loadgen binaries")
	runs := flag.Int("runs", 5, "runs per setting")
	strategies := flag.String("strategies", "backoff,header-aware", "comma-separated strategies")
	demands := flag.String("demands", "0.5,1,1.2,2,5", "comma-separated demand multiples")
	budget := flag.Int("budget", 500, "budget per window")
	window := flag.Duration("window", 60*time.Second, "window (GitHub: 1h)")
	windows := flag.Int("windows", 3, "run length, in windows")
	concurrency := flag.Int("concurrency", 10, "max requests in flight")
	latency := flag.Duration("latency", 100*time.Millisecond, "server service time")
	nAgents := flag.Int("agents", 10, "agents")
	pollers := flag.Int("pollers", 5, "agents that also poll")
	parallel := flag.Int("parallel", 1, "runs at once, each against its own server")
	port := flag.Int("port", 18100, "first port; each parallel slot uses its own")
	out := flag.String("out", "results/bench.jsonl", "where to save every run")
	viaProxy := flag.Bool("proxy", false, "put quotad between the agents and the fake server")
	maxHold := flag.Duration("max-hold", 0, "quotad's max hold (0 = window/120, GitHub's 30s per hour scaled)")
	searches := flag.Bool("searches", false, "agents also search, at the same demand multiple of the search budget")
	nProxies := flag.Int("proxies", 1, "quotad copies per run, like machines (implies -proxy); agents are spread across them")
	redisURL := flag.String("redis", "", "Redis URL the quotad copies share budgets through (implies -proxy); each run uses a fresh token")
	searchDemands := flag.String("search-demands", "", "comma-separated search demand multiples, each run against every core demand (implies -searches)")
	flag.Parse()

	if *searchDemands != "" {
		*searches = true
	}
	if *nProxies > 1 || *redisURL != "" {
		*viaProxy = true
	}
	var settings []setting
	for _, d := range parseList(*demands) {
		// Without -search-demands, search runs at the core multiple (or 0).
		sds := []float64{0}
		if *searches {
			sds = []float64{d}
		}
		if *searchDemands != "" {
			sds = parseList(*searchDemands)
		}
		for _, sd := range sds {
			for _, s := range strings.Split(*strategies, ",") {
				settings = append(settings, setting{strings.TrimSpace(s), d, sd})
			}
		}
	}

	hold := *maxHold
	if hold == 0 {
		hold = *window / 120
	}
	minute := *window / 60 // GitHub's minute, scaled like the hour
	searchBudget := max(1, 30**budget/5000)

	type job struct {
		setting
		seed uint64
	}
	jobs := make(chan job)
	results := make(chan run)
	var wg sync.WaitGroup
	for slot := range *parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := fmt.Sprintf("127.0.0.1:%d", *port+slot)
			for j := range jobs {
				server := exec.Command(filepath.Join(*bin, "fakegithub"),
					"-addr", addr, "-budget", strconv.Itoa(*budget), "-window", window.String(),
					"-concurrency", strconv.Itoa(*concurrency), "-latency", latency.String(),
					"-search-budget", strconv.Itoa(searchBudget), "-search-window", minute.String())
				server.Stderr = os.Stderr
				if err := server.Start(); err != nil {
					log.Fatal(err)
				}
				waitForPort(addr)
				target := "http://" + addr
				var proxies []*exec.Cmd
				var targets []string
				for k := range *nProxies {
					if !*viaProxy {
						break
					}
					// Both programs share this machine's clock, so no reset
					// guard. The secondary limits are the fake server's, as
					// GitHub's published numbers would be in production.
					// Each copy is told the token's full limits, as each
					// machine's would be.
					proxyAddr := fmt.Sprintf("127.0.0.1:%d", *port+*parallel+slot**nProxies+k)
					args := []string{"-listen", proxyAddr, "-upstream", target,
						"-max-hold", hold.String(), "-reset-guard", "0",
						"-max-in-flight", strconv.Itoa(*concurrency),
						"-points", strconv.Itoa(max(5, 900**budget/5000)), "-points-window", minute.String(),
						"-min-pause", minute.String()}
					if *redisURL != "" {
						args = append(args, "-redis", *redisURL)
					}
					proxy := exec.Command(filepath.Join(*bin, "quotad"), args...)
					proxy.Stderr = os.Stderr
					if err := proxy.Start(); err != nil {
						log.Fatal(err)
					}
					waitForPort(proxyAddr)
					proxies = append(proxies, proxy)
					targets = append(targets, "http://"+proxyAddr)
				}
				if len(targets) > 0 {
					target = strings.Join(targets, ",")
				}
				// A fresh token per run, so nothing in Redis carries over.
				token := fmt.Sprintf("bench-%d-%d", time.Now().UnixNano(), slot)

				var stdout bytes.Buffer
				lg := exec.Command(filepath.Join(*bin, "loadgen"), "-json",
					"-search-demand", strconv.FormatFloat(j.searchDemand, 'g', -1, 64),
					"-search-budget", strconv.Itoa(searchBudget), "-search-window", minute.String(),
					"-target", target, "-stats", "http://"+addr+"/__stats", "-token", token, "-strategy", j.strategy,
					"-demand", strconv.FormatFloat(j.demand, 'g', -1, 64),
					"-budget", strconv.Itoa(*budget), "-window", window.String(), "-latency", latency.String(),
					"-windows", strconv.Itoa(*windows), "-agents", strconv.Itoa(*nAgents),
					"-pollers", strconv.Itoa(*pollers), "-seed", strconv.FormatUint(j.seed, 10))
				lg.Stdout, lg.Stderr = &stdout, os.Stderr
				err := lg.Run()
				for _, proxy := range proxies {
					proxy.Process.Kill()
					proxy.Wait()
				}
				server.Process.Kill()
				server.Wait()
				if err != nil {
					log.Fatalf("loadgen %s %.2gx (search %.2gx) seed %d: %v", j.strategy, j.demand, j.searchDemand, j.seed, err)
				}
				var r run
				if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
					log.Fatalf("parsing loadgen output: %v", err)
				}
				r.SearchDemand = j.searchDemand // loadgen omits 0
				results <- r
			}
		}()
	}
	go func() {
		for _, s := range settings {
			for i := range *runs {
				jobs <- job{s, uint64(i + 1)}
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	bySetting := map[setting][]run{}
	total := len(settings) * *runs
	for r := range results {
		json.NewEncoder(w).Encode(r)
		key := setting{r.Strategy, r.Demand, r.SearchDemand}
		bySetting[key] = append(bySetting[key], r)
		done := 0
		for _, rs := range bySetting {
			done += len(rs)
		}
		log.Printf("%d/%d done: %s %.2gx (search %.2gx) seed %d: %d violations, critical p95 %.2fs",
			done, total, r.Strategy, r.Demand, r.SearchDemand, r.Seed, r.Server.Violations, r.Agents.CriticalP95)
	}

	if *nProxies > 1 {
		shared := "each counting alone"
		if *redisURL != "" {
			shared = "sharing one budget through Redis"
		}
		fmt.Printf("%d quotad copies, %s, agents spread across them. ", *nProxies, shared)
	}
	if *viaProxy {
		fmt.Printf("Through quotad (max hold %v, reset guard 0, %d in flight, %d points per %v). ", hold, *concurrency, max(5, 900**budget/5000), minute)
	}
	fmt.Printf("%d agents (%d polling), budget %d per %v, %d in flight, %v latency, %d windows per run, %d runs per row. Cells are mean [min–max].\n\n",
		*nAgents, *pollers, *budget, *window, *concurrency, *latency, *windows, *runs)
	searchCol, searchSep := "", ""
	if *searches {
		if *searchDemands == "" {
			fmt.Printf("Agents also search: %d searches per %v, at the same demand multiple. Utilization is core's.\n\n", searchBudget, minute)
		} else {
			fmt.Printf("Agents also search: %d searches per %v, at the search demand shown. Utilization is core's.\n\n", searchBudget, minute)
		}
		searchCol, searchSep = " search demand | search utilization |", "---|---|"
	}
	fmt.Println("| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |" + searchCol)
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|" + searchSep)
	for _, s := range settings {
		col := colFor(bySetting[s])
		search := ""
		if *searches {
			search = fmt.Sprintf(" %.2gx | %s |", s.searchDemand, col(func(r run) float64 { return 100 * r.SearchUtil }, "%.0f%%"))
		}
		fmt.Printf("| %.2gx | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |%s\n", s.demand, s.strategy,
			col(func(r run) float64 { return float64(r.Server.Violations) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Server.Primary403) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Server.Secondary429) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Agents.PeakRejectionsPerS) }, "%.0f"),
			col(func(r run) float64 { return 100 * r.Agents.PostResetShare }, "%.0f%%"),
			col(func(r run) float64 { return r.Agents.CriticalP50 }, "%.2f"),
			col(func(r run) float64 { return r.Agents.CriticalP95 }, "%.2f"),
			col(func(r run) float64 { return float64(r.Agents.CriticalUnfinished) }, "%.0f"),
			col(func(r run) float64 { return 100 * r.Utilization }, "%.0f%%"),
			search,
		)
	}
	if *searches {
		printByResource(settings, bySetting)
	}
}

// colFor returns a function that formats one number across rs as
// mean [min–max].
func colFor(rs []run) func(f func(run) float64, format string) string {
	return func(f func(run) float64, format string) string {
		xs := make([]float64, len(rs))
		for i, r := range rs {
			xs[i] = f(r)
		}
		g := metrics.RangeOf(xs)
		return fmt.Sprintf(format+" ["+format+"–"+format+"]", g.Mean, g.Min, g.Max)
	}
}

// printByResource prints the critical calls' waits split by resource.
func printByResource(settings []setting, bySetting map[setting][]run) {
	fmt.Println("\nCritical calls by resource. Wait is from when the agent wanted the call; it includes time stuck behind the agent's own earlier calls. Call time is from when the agent first sent it: quotad's hold, rejections and retries, and the server's latency.")
	fmt.Println()
	fmt.Println("| demand | search demand | strategy | core wait p50 (s) | core wait p95 (s) | core call p95 (s) | unfinished core | search wait p95 (s) | search call p95 (s) | unfinished search |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range settings {
		col := colFor(bySetting[s])
		res := func(name string, f func(metrics.ResourceSummary) float64) func(run) float64 {
			return func(r run) float64 { return f(r.Agents.Resources[name]) }
		}
		fmt.Printf("| %.2gx | %.2gx | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.demand, s.searchDemand, s.strategy,
			col(res("core", func(x metrics.ResourceSummary) float64 { return x.WaitP50 }), "%.2f"),
			col(res("core", func(x metrics.ResourceSummary) float64 { return x.WaitP95 }), "%.2f"),
			col(res("core", func(x metrics.ResourceSummary) float64 { return x.CallP95 }), "%.2f"),
			col(res("core", func(x metrics.ResourceSummary) float64 { return float64(x.Unfinished) }), "%.0f"),
			col(res("search", func(x metrics.ResourceSummary) float64 { return x.WaitP95 }), "%.2f"),
			col(res("search", func(x metrics.ResourceSummary) float64 { return x.CallP95 }), "%.2f"),
			col(res("search", func(x metrics.ResourceSummary) float64 { return float64(x.Unfinished) }), "%.0f"),
		)
	}
}

func parseList(list string) []float64 {
	var xs []float64
	for _, x := range strings.Split(list, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			log.Fatalf("bad number %q: %v", x, err)
		}
		xs = append(xs, f)
	}
	return xs
}

// waitForPort blocks until something listens on addr.
func waitForPort(addr string) {
	for range 100 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("fakegithub never listened on %s", addr)
}
