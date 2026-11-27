# mqtt2prometheus

Subscribes to MQTT topics and exposes their values as Prometheus gauges. A YAML file decides which topics feed which metric and how labels and values are pulled out of topic names and JSON payloads.

## Running

```sh
docker run -v $PWD/config.yml:/config.yml -p 8000:8000 ghcr.io/joconcepts/mqtt2prometheus /config.yml
# or
go run . config.yml
```

| Flag | Default | |
|---|---|---|
| `-listen` | `:8000` | address for `/metrics` |
| `-debug` | off | log messages that matched a rule but produced no value |

The exporter exits if the first connection to the broker fails, for example because of wrong credentials. Once it has connected, it reconnects on its own.

## Configuration

```yaml
mqtt:
  host: localhost
  port: 1883
  user: exporter
  password: secret
  topics: ['zigbee2mqtt/#', 'tele/+/SENSOR']   # subscriptions, MQTT wildcards allowed

metrics:
  - name: power_watts
    description: 'Current power draw'
    labels: [plug]
    expire: 15m
    topics:                                    # the rules feeding this metric
      - regex: '^tele/(?P<plug>[^/]+)/SENSOR$'
        jq: '.ENERGY.Power'
        exclude: {plug: [Garage]}
```

A metric is a gauge with a fixed set of label names. Each metric has a list of rules under `topics`, and each rule answers three questions about an incoming message:

1. Does the topic match? (`topic`, `regex`, `exclude`)
2. Which labels does the series get? (`labels`, regex groups, `label_map`)
3. What is the value? (`jq`, `default`)

Every message is checked against every rule of every metric. A message can therefore feed several metrics, or the same metric several times with different labels. The only topics that arrive at all are the ones covered by `mqtt.topics`.

### Matching topics

**Exact topic.** The rule only applies to this one topic:

```yaml
- topic: 'zigbee2mqtt/Living Room Thermostat'
```

**Regex.** A Go regular expression ([RE2 syntax](https://github.com/google/re2/wiki/Syntax)). It's unanchored, so write `^…$` to match the whole topic. Named groups become labels (see below):

```yaml
- regex: '^tele/(?P<plug>[^/]+)/SENSOR$'           # any Tasmota device
- regex: '^zigbee2mqtt/Window (?P<window>.+)$'     # every device whose name starts with "Window "
- regex: '^home/(?P<room>kitchen|bath)/humidity$'  # an explicit list
```

Each rule has exactly one of `topic` or `regex`.

**Exclude.** Skips a matched message when a label has one of the listed values. This is useful when a broad regex catches a device that sends something different:

```yaml
- regex: '^tele/(?P<plug>[^/]+)/SENSOR$'
  exclude:
    plug: [Garage, Workshop]
```

### Labels

A series' labels come from three sources, applied in this order:

| Source | Example | |
|---|---|---|
| `labels` | `labels: {pump: circulation}` | fixed values, the usual choice for exact-topic rules |
| regex named groups | `(?P<device>\w+)` | the matched text becomes the label value |
| `label_map` | see below | derives a label from one of the above |

`label_map` is set on the metric, not the rule. It looks up the value of one label in a table and puts the result in another. Use it when the topic only carries an ID:

```yaml
- name: radiator_valve_percent
  labels: [device, room]
  label_map:
    room:
      from: device          # take the value of "device"…
      values:               # …and look it up here
        trv01: kitchen
        trv02: bedroom
  topics:
    - regex: '^home/valves/(?P<device>\w+)/position$'
```

`home/valves/trv01/position` becomes `radiator_valve_percent{device="trv01",room="kitchen"}`. An ID missing from the table skips the message, because the series would be missing a label. A rule that already sets the label, through `labels` or a regex group, is left alone. That lets you mix mapped regex rules with exact-topic rules on the same metric.

Every rule must produce exactly the metric's label names. This is checked at startup, and any mismatch stops the exporter with an error naming the metric and rule. The order of labels doesn't matter.

### Values

**Without `jq`**, the payload itself must be a number, e.g. `21.5` or `1`.

**With `jq`**, the payload is parsed as JSON and the [jq](https://jqlang.org/manual/) expression runs on it. The first result is converted like this:

| jq result | Value |
|---|---|
| number | as-is |
| `true` / `false` | `1` / `0` |
| numeric string, e.g. `"21.5"` | parsed |
| nothing, `null`, or a jq error | `default` if set, otherwise the message is skipped |
| anything else (text, object, array) | logged as a warning and skipped |

Because booleans become 1/0, most "map this to a number" needs are a single jq expression:

```yaml
jq: '.temperature'                              # plain field
jq: '.ENERGY.Power'                             # nested field
jq: '.thermostats[0].pos'                       # array element
jq: '."DS18B20-1".Temperature'                  # key with a dash needs quotes
jq: '{"heat": 1, "idle": 0}[.running_state]'    # strings to digits, unknown values become null
jq: '{"off": 0, "low": 1, "high": 2}[.fan]'     # any number of states
jq: '.state == "ON"'                            # one string to 1, everything else to 0
jq: '.state | ascii_downcase == "on"'           # same, ignoring case
jq: '.contact | not'                            # invert a boolean (contact=true means closed)
jq: '.battery < 20'                             # threshold to 1/0
jq: '(.temp_f - 32) * 5 / 9'                    # unit conversion
jq: '.power // .ENERGY.Power'                   # fall back to another field
```

**`default`** fills in when the field is missing or the mapping returns `null`:

```yaml
- topic: 'zigbee2mqtt/Office Thermostat'
  jq: '{"heat": 1, "idle": 0}[.running_state]'
  default: 0               # a message without running_state still reports 0
```

Leave `default` out when a missing field should leave the last value in place. That's usually right for devices that send partial updates.

Payloads that aren't JSON, such as a plain `ON`, can't go through jq. Without jq they must be numeric.

### Expiry

A gauge keeps its last value until a new message for that series arrives. For a device that has died, that means a flat line that looks perfectly healthy. Set `expire` on the metric to delete series that haven't been updated for that long:

```yaml
- name: power_watts
  expire: 15m              # Go duration: 90s, 15m, 6h, …
```

Expired series disappear from `/metrics`, so Prometheus shows a gap and `absent()` alerts work. Each series comes back with the device's next message. Choose a value comfortably above the device's reporting interval. Leave `expire` out for event-driven values that legitimately stay quiet for days, such as switch states or window contacts.

## Example

[`example/config.yml`](example/config.yml) uses every feature above. Feeding it the messages in [`example/messages.yml`](example/messages.yml) produces [`example/metrics.txt`](example/metrics.txt):

| Topic | Payload | Result |
|---|---|---|
| `home/boiler/pump` | `1` | `pump_running{pump="circulation"} 1` |
| `home/valves/trv01/position` | `35` | `radiator_valve_percent{device="trv01",room="kitchen"} 35` |
| `home/valves/trv99/position` | `80` | skipped: `trv99` not in `label_map` |
| `tele/Desk/SENSOR` | `{"ENERGY":{"Power":42,…}}` | `power_watts{plug="Desk"} 42` |
| `tele/Garage/SENSOR` | `{"ENERGY":{"Power":1200}}` | skipped: excluded |
| `zigbee2mqtt/Living Room Thermostat` | `{"running_state":"heat",…}` | `thermostat_heating{thermostat="living_room"} 1` |
| `zigbee2mqtt/Office Thermostat` | `{"local_temperature":21}` | `thermostat_heating{thermostat="office"} 0` (default) |
| `zigbee2mqtt/Window Kitchen` | `{"contact":false,…}` | `window_open{window="Kitchen"} 1` |
| `home/weather` | `{"temp_f":41}` | `outdoor_temperature_celsius 5` |

`go test` checks this output, so the example stays correct. After changing the example, run `go test -update` to regenerate `metrics.txt`.

## Development

```sh
go test -race ./...
golangci-lint run ./...
```

If a `config.yml` exists in the repo root (it's gitignored), `go test` also checks that it compiles.
