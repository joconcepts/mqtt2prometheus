package main

import (
	"bytes"
	"errors"
	"flag"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite example/metrics.txt")

// TestExample feeds example/messages.yml through example/config.yml and
// compares the exposed metrics with example/metrics.txt.
func TestExample(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig("example/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	metrics, err := compile(cfg.Metrics, reg)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile("example/messages.yml")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []struct{ Topic, Payload string }
	if err := yaml.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		handle(metrics, m.Topic, []byte(m.Payload))
	}

	const golden = "example/metrics.txt"
	if *update {
		if err := prometheus.WriteToTextfile(golden, reg); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.GatherAndCompare(reg, bytes.NewReader(want)); err != nil {
		t.Error(err, "\n(run go test -update if the change is intended)")
	}
}

// TestLocalConfig checks that the private config.yml compiles, when present.
func TestLocalConfig(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig("config.yml")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no config.yml")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compile(cfg.Metrics, prometheus.NewRegistry()); err != nil {
		t.Fatal(err)
	}
}

func TestExpire(t *testing.T) {
	t.Parallel()

	var cfg config
	err := yaml.Unmarshal([]byte(`
metrics:
  - name: t
    labels: [sensor]
    expire: 30m
    topics:
      - regex: '^s/(?P<sensor>\w+)$'
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := compile(cfg.Metrics, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	m := metrics[0]
	if m.expire != 30*time.Minute {
		t.Fatalf("expire = %v, want 30m", m.expire)
	}

	t0 := time.Now()
	m.set(prometheus.Labels{"sensor": "a"}, 1, t0)
	m.set(prometheus.Labels{"sensor": "b"}, 2, t0.Add(20*time.Minute))

	m.sweep(t0.Add(29 * time.Minute))
	if n := testutil.CollectAndCount(m.gauge); n != 2 {
		t.Fatalf("after 29m: %d series, want 2", n)
	}
	m.sweep(t0.Add(31 * time.Minute)) // a is stale, b is not
	if n := testutil.CollectAndCount(m.gauge); n != 1 {
		t.Fatalf("after 31m: %d series, want 1", n)
	}
	if got := testutil.ToFloat64(m.gauge.With(prometheus.Labels{"sensor": "b"})); got != 2 {
		t.Errorf("b = %v, want 2", got)
	}
	m.set(prometheus.Labels{"sensor": "a"}, 3, t0.Add(32*time.Minute)) // a comes back
	if n := testutil.CollectAndCount(m.gauge); n != 2 {
		t.Errorf("after re-set: %d series, want 2", n)
	}
}
