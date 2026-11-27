// Command mqtt2prometheus subscribes to MQTT topics and exposes their values
// as Prometheus gauges, driven by a YAML config.
package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/itchyny/gojq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

type config struct {
	MQTT struct {
		Host     string   `yaml:"host"`
		Port     int      `yaml:"port"`
		User     string   `yaml:"user"`
		Password string   `yaml:"password"`
		Topics   []string `yaml:"topics"`
	} `yaml:"mqtt"`
	Metrics []metricConfig `yaml:"metrics"`
}

type metricConfig struct {
	Name        string                  `yaml:"name"`
	Description string                  `yaml:"description"`
	Labels      []string                `yaml:"labels"`
	LabelMap    map[string]labelMapping `yaml:"label_map"`
	Topics      []ruleConfig            `yaml:"topics"`
	Expire      time.Duration           `yaml:"expire"`
}

// labelMapping derives a label by looking up the value of label From in Values.
type labelMapping struct {
	From   string            `yaml:"from"`
	Values map[string]string `yaml:"values"`
}

// ruleConfig matches either an exact Topic or a Regex whose named groups
// become labels. Default replaces a missing, null or failed jq result.
type ruleConfig struct {
	Topic   string              `yaml:"topic"`
	Regex   string              `yaml:"regex"`
	Labels  map[string]string   `yaml:"labels"`
	Exclude map[string][]string `yaml:"exclude"`
	JQ      string              `yaml:"jq"`
	Default *float64            `yaml:"default"`
}

type rule struct {
	ruleConfig
	re *regexp.Regexp
	jq *gojq.Code
}

type metric struct {
	gauge      *prometheus.GaugeVec
	labelNames []string
	labelMap   map[string]labelMapping
	rules      []rule
	expire     time.Duration

	mu       sync.Mutex // makes set and sweep atomic, guards lastSeen
	lastSeen map[string]seenSeries
}

type seenSeries struct {
	labels prometheus.Labels
	at     time.Time
}

var errNoValue = errors.New("no value")

func main() {
	listen := flag.String("listen", ":8000", "address to serve /metrics on")
	debug := flag.Bool("debug", false, "log messages that yield no value")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] config.yml\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := run(flag.Arg(0), *listen); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(configPath, listen string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	metrics, err := compile(cfg.Metrics, prometheus.DefaultRegisterer)
	if err != nil {
		return err
	}

	filters := make(map[string]byte, len(cfg.MQTT.Topics))
	for _, t := range cfg.MQTT.Topics {
		filters[t] = 0
	}
	onMessage := func(_ mqtt.Client, msg mqtt.Message) {
		handle(metrics, msg.Topic(), msg.Payload())
	}

	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", cfg.MQTT.Host, cfg.MQTT.Port)).
		SetClientID("mqtt2prom-" + uuid.New().String()).
		SetUsername(cfg.MQTT.User).
		SetPassword(cfg.MQTT.Password).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("mqtt connection lost", "err", err)
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			slog.Info("mqtt connected", "broker", cfg.MQTT.Host)
			if t := c.SubscribeMultiple(filters, onMessage); t.Wait() && t.Error() != nil {
				slog.Error("mqtt subscribe failed", "err", t.Error())
			}
		})

	// Fail fast on the first connect so bad credentials show up as an exit error
	// (paho only logs failed retries at debug level). Reconnects stay automatic.
	if t := mqtt.NewClient(opts).Connect(); t.Wait() && t.Error() != nil {
		return fmt.Errorf("mqtt connect: %w", t.Error())
	}

	for _, m := range metrics {
		if m.expire > 0 {
			go func() {
				// Checking every expire/10 keeps a stale series around for at most 1.1x expire.
				for now := range time.Tick(m.expire / 10) {
					m.sweep(now)
				}
			}()
		}
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("serving metrics", "addr", listen)

	return srv.ListenAndServe()
}

func loadConfig(path string) (*config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own CLI argument
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func compile(cfgs []metricConfig, reg prometheus.Registerer) ([]*metric, error) {
	metrics := make([]*metric, 0, len(cfgs))
	for _, mc := range cfgs {
		m := &metric{
			gauge:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: mc.Name, Help: mc.Description}, mc.Labels),
			labelNames: mc.Labels,
			labelMap:   mc.LabelMap,
			expire:     mc.Expire,
			lastSeen:   map[string]seenSeries{},
		}
		if err := reg.Register(m.gauge); err != nil {
			return nil, fmt.Errorf("metric %s: %w", mc.Name, err)
		}
		for i, rc := range mc.Topics {
			r, err := compileRule(rc, mc)
			if err != nil {
				return nil, fmt.Errorf("metric %s, topic #%d: %w", mc.Name, i+1, err)
			}
			m.rules = append(m.rules, r)
		}
		metrics = append(metrics, m)
	}
	return metrics, nil
}

// compileRule also checks that the rule yields exactly the metric's labels,
// so a typo fails at startup instead of panicking on the first message.
func compileRule(rc ruleConfig, mc metricConfig) (rule, error) {
	r := rule{ruleConfig: rc}
	if (rc.Topic == "") == (rc.Regex == "") {
		return r, errors.New("set exactly one of topic or regex")
	}

	have := slices.Collect(maps.Keys(rc.Labels))
	if rc.Regex != "" {
		re, err := regexp.Compile(rc.Regex)
		if err != nil {
			return r, err
		}
		r.re = re
		for _, name := range re.SubexpNames() {
			if name != "" {
				have = append(have, name)
			}
		}
	}
	for label, lm := range mc.LabelMap {
		if slices.Contains(have, lm.From) && !slices.Contains(have, label) {
			have = append(have, label)
		}
	}
	slices.Sort(have)
	if want := slices.Sorted(slices.Values(mc.Labels)); !slices.Equal(have, want) {
		return r, fmt.Errorf("rule yields labels %v, metric has %v", have, want)
	}

	if rc.JQ != "" {
		q, err := gojq.Parse(rc.JQ)
		if err != nil {
			return r, fmt.Errorf("jq: %w", err)
		}
		if r.jq, err = gojq.Compile(q); err != nil {
			return r, fmt.Errorf("jq: %w", err)
		}
	}
	return r, nil
}

func handle(metrics []*metric, topic string, payload []byte) {
	for _, m := range metrics {
		for _, r := range m.rules {
			labels, ok := r.match(topic, m.labelMap)
			if !ok {
				continue
			}
			v, err := r.value(payload)
			switch {
			case errors.Is(err, errNoValue):
				slog.Debug("skipped", "topic", topic, "jq", r.JQ, "err", err)
			case err != nil:
				slog.Warn("bad value", "topic", topic, "jq", r.JQ, "err", err, "payload", string(payload))
			default:
				m.set(labels, v, time.Now())
			}
		}
	}
}

func (r *rule) match(topic string, labelMap map[string]labelMapping) (prometheus.Labels, bool) {
	var groups []string
	if r.re == nil {
		if topic != r.Topic {
			return nil, false
		}
	} else if groups = r.re.FindStringSubmatch(topic); groups == nil {
		return nil, false
	}

	labels := prometheus.Labels{}
	maps.Copy(labels, r.Labels)
	if r.re != nil {
		for i, name := range r.re.SubexpNames() {
			if name != "" {
				labels[name] = groups[i]
			}
		}
	}
	for label, values := range r.Exclude {
		if slices.Contains(values, labels[label]) {
			return nil, false
		}
	}
	for label, lm := range labelMap {
		if _, ok := labels[label]; ok {
			continue
		}
		mapped, ok := lm.Values[labels[lm.From]]
		if !ok {
			return nil, false // the series would miss a label
		}
		labels[label] = mapped
	}
	return labels, true
}

// value returns the raw payload as a number, or the first result of the jq
// expression. jq gets the parsed JSON, or the trimmed text as a string when the
// payload is not JSON (e.g. ON), so plain-text states can be mapped too.
func (r *rule) value(payload []byte) (float64, error) {
	if r.jq == nil {
		return toFloat(strings.TrimSpace(string(payload)))
	}
	// Device firmware is not always strict about JSON.
	var doc any
	lenient := json.JoinOptions(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if err := json.Unmarshal(payload, &doc, lenient); err != nil {
		doc = strings.TrimSpace(string(payload))
	}
	out, _ := r.jq.Run(doc).Next() // no result comes back as nil, like null
	if err, isErr := out.(error); out == nil || isErr {
		switch {
		case r.Default != nil:
			return *r.Default, nil
		case isErr:
			return 0, fmt.Errorf("%w: %w", errNoValue, err)
		default:
			return 0, errNoValue
		}
	}
	return toFloat(out)
}

func toFloat(v any) (float64, error) {
	switch v := v.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case *big.Int:
		f, _ := v.Float64()
		return f, nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("not a number: %v (%T)", v, v)
	}
}

func (m *metric) set(labels prometheus.Labels, v float64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauge.With(labels).Set(v)
	if m.expire == 0 {
		return
	}
	key := make([]string, len(m.labelNames))
	for i, name := range m.labelNames {
		key[i] = labels[name]
	}
	m.lastSeen[strings.Join(key, "\xff")] = seenSeries{labels, now}
}

// sweep deletes series that have not been set within m.expire.
func (m *metric) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.lastSeen, func(_ string, s seenSeries) bool {
		stale := now.Sub(s.at) > m.expire
		if stale {
			m.gauge.Delete(s.labels)
		}
		return stale
	})
}
