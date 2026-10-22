package main

import (
	"flag"
	"testing"
	"time"
)

func TestFlagsFromEnv(t *testing.T) {
	fs := flag.NewFlagSet("quotad", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "")
	maxHold := fs.Duration("max-hold", 30*time.Second, "")
	points := fs.Int("points", 900, "")
	env := map[string]string{"QUOTAD_LISTEN": "0.0.0.0:9000", "QUOTAD_MAX_HOLD": "1m"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	if err := flagsFromEnv(fs, lookup); err != nil {
		t.Fatal(err)
	}
	// The command line wins over the environment.
	if err := fs.Parse([]string{"-listen", "127.0.0.1:7000"}); err != nil {
		t.Fatal(err)
	}
	if *listen != "127.0.0.1:7000" || *maxHold != time.Minute || *points != 900 {
		t.Fatalf("got listen %q, max-hold %v, points %d", *listen, *maxHold, *points)
	}
}

func TestFlagsFromEnvBadValue(t *testing.T) {
	fs := flag.NewFlagSet("quotad", flag.ContinueOnError)
	fs.Int("max-in-flight", 100, "")
	lookup := func(k string) (string, bool) { return "lots", k == "QUOTAD_MAX_IN_FLIGHT" }
	if err := flagsFromEnv(fs, lookup); err == nil {
		t.Fatal("want an error for a bad value")
	}
}
