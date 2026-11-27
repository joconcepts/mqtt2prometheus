# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single-file Go exporter (`main.go`) that subscribes to MQTT topics and exposes their values as Prometheus gauges at `/metrics` (default `:8000`). All behavior is driven by a YAML config passed as the only argument. The config schema (matchers, labels, value mapping, expiry) is documented in `README.md`; keep it in sync when changing behavior. It was a Python script until the Go rewrite; see git history for `main.py`.

`config.yml` and `metrics.txt` in the repo root are the owner's private home-automation config and a Prometheus dump. Both are gitignored: never commit them or copy their device names into tracked files. Use `example/` for anything public.

## Commands

```sh
go test -race ./...                        # TestExample (golden), TestExpire, TestLocalConfig (skips without config.yml)
go test -run TestExample -update ./...     # regenerate example/metrics.txt after changing the example or output format
golangci-lint run ./...                    # config in .golangci.yml; wsl_v5 and goconst are off on purpose
go run . -listen :8000 -debug config.yml   # -debug logs messages that matched a rule but yielded no value
```

`mqtt.host` in the private config.yml (`mosquitto.homeautomation`) only resolves inside the cluster. From a workstation use `mosquitto.home.kdl.onl`. The deployed exporter's series can be queried at `https://prometheus.home.kdl.onl` with `{job="monitoring/mqtt2prometheus"}`.

CI (`.github/workflows/docker-image.yml`) runs tests and golangci-lint (pinned to v2.15.0). The build job then makes a linux/amd64+arm64 image, which is pushed to `ghcr.io/<repo>` for pushes to `main` and `v*` tags, and only built for PRs. The Dockerfile cross-compiles on `$BUILDPLATFORM`, so arm64 builds need no QEMU.

## Architecture

- `example/`: `config.yml` plus `messages.yml` (sample messages) feed `TestExample`, which compares the result with `metrics.txt`. When adding a config feature, add it to the example and the README table.
- On startup the exporter exits if the first MQTT connect fails (bad credentials, broker down). After that, paho reconnects automatically.
- `compile` validates the config at startup. Each rule must provide exactly the metric's label set: fixed `labels` + regex named groups + `label_map` entries.
- `handle` runs every message against every rule of every metric. A message can feed several metrics.
- `rule.match` builds the labels (or skips the message), and `rule.value` extracts the number via jq.
- `metric.set` and `metric.sweep` implement per-metric `expire`. They share a mutex so a fresh update can't be deleted by a concurrent sweep. Sweeps run every `expire/10`.
