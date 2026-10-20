// Command bench runs the step 1 baselines: for every strategy and demand
// level it starts a fresh fakegithub, runs loadgen against it over real
// HTTP, and repeats each setting -runs times. It prints a Markdown table of
// averages and ranges, and saves every run as a JSON line.
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
	Strategy string  `json:"strategy"`
	Demand   float64 `json:"demand"`
	Seed     uint64  `json:"seed"`
	Server   struct {
		Violations   int `json:"violations"`
		Primary403   int `json:"primary_403"`
		Secondary429 int `json:"secondary_429"`
	} `json:"server"`
	Utilization float64         `json:"utilization"`
	Agents      metrics.Summary `json:"agents"`
}

type setting struct {
	strategy string
	demand   float64
}

func main() {
	bin := flag.String("bin", "bin", "directory holding the fakegithub and loadgen binaries")
	runs := flag.Int("runs", 5, "runs per setting")
	strategies := flag.String("strategies", "backoff,header-aware", "comma-separated strategies")
	demands := flag.String("demands", "0.5,1,2,5", "comma-separated demand multiples")
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
	flag.Parse()

	var settings []setting
	for _, d := range strings.Split(*demands, ",") {
		demand, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
		if err != nil {
			log.Fatalf("bad demand %q: %v", d, err)
		}
		for _, s := range strings.Split(*strategies, ",") {
			settings = append(settings, setting{strings.TrimSpace(s), demand})
		}
	}

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
					"-concurrency", strconv.Itoa(*concurrency), "-latency", latency.String())
				server.Stderr = os.Stderr
				if err := server.Start(); err != nil {
					log.Fatal(err)
				}
				waitForPort(addr)

				var stdout bytes.Buffer
				lg := exec.Command(filepath.Join(*bin, "loadgen"), "-json",
					"-target", "http://"+addr, "-strategy", j.strategy,
					"-demand", strconv.FormatFloat(j.demand, 'g', -1, 64),
					"-budget", strconv.Itoa(*budget), "-window", window.String(), "-latency", latency.String(),
					"-windows", strconv.Itoa(*windows), "-agents", strconv.Itoa(*nAgents),
					"-pollers", strconv.Itoa(*pollers), "-seed", strconv.FormatUint(j.seed, 10))
				lg.Stdout, lg.Stderr = &stdout, os.Stderr
				err := lg.Run()
				server.Process.Kill()
				server.Wait()
				if err != nil {
					log.Fatalf("loadgen %s %.2gx seed %d: %v", j.strategy, j.demand, j.seed, err)
				}
				var r run
				if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
					log.Fatalf("parsing loadgen output: %v", err)
				}
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
		bySetting[setting{r.Strategy, r.Demand}] = append(bySetting[setting{r.Strategy, r.Demand}], r)
		done := 0
		for _, rs := range bySetting {
			done += len(rs)
		}
		log.Printf("%d/%d done: %s %.2gx seed %d: %d violations, critical p95 %.2fs",
			done, total, r.Strategy, r.Demand, r.Seed, r.Server.Violations, r.Agents.CriticalP95)
	}

	fmt.Printf("%d agents (%d polling), budget %d per %v, %d in flight, %v latency, %d windows per run, %d runs per row. Cells are mean [min–max].\n\n",
		*nAgents, *pollers, *budget, *window, *concurrency, *latency, *windows, *runs)
	fmt.Println("| demand | strategy | violations | 403s | 429s | peak rejections/s | rejections ≤2s after reset | critical p50 (s) | critical p95 (s) | unfinished critical | utilization |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range settings {
		rs := bySetting[s]
		col := func(f func(run) float64, format string) string {
			xs := make([]float64, len(rs))
			for i, r := range rs {
				xs[i] = f(r)
			}
			g := metrics.RangeOf(xs)
			return fmt.Sprintf(format+" ["+format+"–"+format+"]", g.Mean, g.Min, g.Max)
		}
		fmt.Printf("| %.2gx | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.demand, s.strategy,
			col(func(r run) float64 { return float64(r.Server.Violations) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Server.Primary403) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Server.Secondary429) }, "%.0f"),
			col(func(r run) float64 { return float64(r.Agents.PeakRejectionsPerS) }, "%.0f"),
			col(func(r run) float64 { return 100 * r.Agents.PostResetShare }, "%.0f%%"),
			col(func(r run) float64 { return r.Agents.CriticalP50 }, "%.2f"),
			col(func(r run) float64 { return r.Agents.CriticalP95 }, "%.2f"),
			col(func(r run) float64 { return float64(r.Agents.CriticalUnfinished) }, "%.0f"),
			col(func(r run) float64 { return 100 * r.Utilization }, "%.0f%%"),
		)
	}
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
