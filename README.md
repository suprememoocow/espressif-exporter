# espressif-exporter

A Prometheus exporter for ESPHome and Shelly devices. It finds devices with mDNS and
exports their readings under one metric namespace, so a single query covers the whole
fleet regardless of vendor.

Built for a home network of 100 or more devices, running on TrueNAS SCALE in a container
on the default bridge network.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Prometheus configuration](#prometheus-configuration)
- [Configuration reference](#configuration-reference)
- [Metrics](#metrics)
- [Terminology](#terminology)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## How it works

The exporter runs three stages.

1. **Discovery** finds devices with mDNS and records each one under a stable identifier.
2. **The registry** merges observations, tracks addresses, and publishes an immutable
   snapshot.
3. **Collection** reads each device and renders Prometheus metrics.

### Discovery in a bridged container

A bridged container cannot send or receive multicast, so it cannot do mDNS itself. On
TrueNAS SCALE the host's `avahi-daemon` also holds port 5353 on every interface and you
cannot disable it.

The exporter solves both problems the same way: it browses through the host's
`avahi-daemon` over the system D-Bus socket. It never binds port 5353. Only discovery
needs the host. Unicast HTTP and TCP to the devices work from the bridge network.

Four discovery backends implement one interface. Select them with `discovery.sources`.

| Backend | Use it when | Limits |
|---------|-------------|--------|
| `avahi` | Production on TrueNAS SCALE, or any host that runs `avahi-daemon`. | Needs the host D-Bus socket. |
| `zeroconf` | A Linux host with `network_mode: host` and no `avahi-daemon`. | Binds port 5353. Finds nothing on TrueNAS SCALE or macOS, because another daemon already holds that port. |
| `static` | VLANs that do not forward mDNS, and as a fallback when Avahi is down. | You maintain the list. |
| `shelly_cloud` | Shelly devices on VLANs that do not forward mDNS, without maintaining a list. | Shelly only. Needs outbound HTTPS and a cloud key. Addresses are the last ones the devices reported to the cloud, polled every 5 minutes. Devices the cloud lists as offline are included, because Gen1 devices often drop their cloud connection while still reachable on the LAN. See [Shelly Cloud](#shelly-cloud). |

Configure `static` alongside `avahi`. An Avahi outage then reduces coverage instead of
removing it. Put every device you alert on in the static list.

### Collection differs by vendor

**ESPHome** uses the native API on port 6053, the same protocol Home Assistant speaks.
The protocol pushes state, so the exporter holds one connection per device and keeps a
cached snapshot. A probe reads that cache and makes no network request.

**Shelly** uses HTTP. A probe fetches `/rpc/Shelly.GetStatus` on Gen2 and later, or
`/status` on Gen1, within the scrape deadline. Steady state is one request per device per
scrape on a keep-alive connection.

### Identity is stable across DHCP changes

Prometheus derives the `instance` label from the scrape target. The exporter therefore
gives Prometheus a stable device identifier, not an IP address, and resolves the
identifier to a current address on each probe.

Without this, every DHCP lease change renames `instance` on every series for that device.
Counters restart, `rate()` breaks, and dashboards lose history.

## Quick start

### Run on TrueNAS SCALE

Check the host first:

```bash
ls -l /run/dbus/system_bus_socket
systemctl is-active dbus avahi-daemon
busctl --system list | grep -i avahi
getent passwd 568
avahi-browse -rt _shelly._tcp
avahi-browse -rt _esphomelib._tcp
```

`avahi-browse` takes one service type per invocation. Two types in one command fail with
`Too many arguments`.

`getent passwd 568` must print a line. `dbus-daemon` looks the container's uid up in the
host's passwd database before it accepts the connection, so the uid in
`deploy/docker-compose.yml` has to exist on the host. 568 is TrueNAS SCALE's `apps` user.

Copy `configs/config.example.yaml` to `deploy/config.yaml` and edit it. Then start the
exporter:

```bash
docker compose -f deploy/docker-compose.yml up -d
docker compose logs -f espressif-exporter
```

Check that discovery works:

```bash
curl -s truenas:9826/debug/devices | jq 'length'
curl -s truenas:9826/metrics | grep espressif_exporter_avahi_up
```

`/debug/devices` returns the full snapshot as JSON: every address, TXT record, and
identifier. It answers most deployment questions on its own.

### Run without Avahi

Use `deploy/docker-compose.hostnet.yml`. It sets `network_mode: host` and selects the
`zeroconf` backend. This fails on any host where another daemon holds port 5353.

## Prometheus configuration

The exporter serves Prometheus HTTP service discovery at `/sd` and per-device metrics at
`/probe`. Add these jobs:

```yaml
scrape_configs:
  - job_name: shelly
    scrape_interval: 30s
    scrape_timeout: 20s
    metrics_path: /probe
    http_sd_configs:
      - url: http://espressif-exporter:9826/sd?kind=shelly
        refresh_interval: 60s
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: espressif-exporter:9826
      - source_labels: [__meta_device_kind]
        target_label: kind
      - source_labels: [__meta_device_stale]
        regex: "true"
        action: drop

  - job_name: esphome
    scrape_interval: 30s
    scrape_timeout: 20s
    metrics_path: /probe
    http_sd_configs:
      - url: http://espressif-exporter:9826/sd?kind=esphome
        refresh_interval: 60s
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: espressif-exporter:9826
      - source_labels: [__meta_device_kind]
        target_label: kind

  - job_name: espressif-exporter
    static_configs:
      - targets: ["espressif-exporter:9826"]
```

Do not promote `__meta_device_name` or `__meta_device_model` to labels. Device names are
editable, so renaming a device in the vendor app forks every series for it. Join them in
a query instead:

```promql
espressif_power_watts * on (instance) group_left (device_name, model) espressif_device_info
```

### Endpoints

| Path | Returns |
|------|---------|
| `/sd` | Prometheus target list. Accepts `?kind=shelly` or `?kind=esphome`. |
| `/probe?target=<id>` | One device's metrics. Returns 404 for an unknown identifier. |
| `/metrics` | The exporter's own metrics. No per-device labels appear here. |
| `/healthz` | Liveness. Always 200 once the server listens. |
| `/readyz` | Readiness. 503 until the registry holds at least one device. |
| `/debug/devices` | The discovery snapshot as JSON. |
| `/-/loglevel?level=debug` | Changes the log level. Accepts POST. |

## Configuration reference

Configuration layers in this order, with later layers winning: defaults, then the YAML
file, then environment variables, then flags.

Environment variables use the prefix `EE_` and `__` for nesting. `EE_SERVER__LISTEN`
maps to `server.listen`.

Every secret accepts a `_file` variant that reads the value from a path, so no secret
needs to appear in a compose file:

```yaml
shelly:
  auth:
    password_file: /run/secrets/shelly_password
esphome:
  encryption_key_file: /run/secrets/esphome_psk
```

See `configs/config.example.yaml` for every option with its default.

### ESPHome authentication

Set one shared Noise key for the fleet:

```yaml
esphome:
  encryption_key: "base64-encoded-32-bytes"
```

The exporter tries the encrypted handshake first and falls back to plaintext once per
device. Set `require_encryption: true` to remove the fallback.

Nodes on ESPHome firmware older than 2026.1.0 may use an API password instead. Such a
node accepts the connection, answers the device-information request, then closes the
connection. Set `esphome.password` for those nodes, or migrate them to an encryption key.

### Shelly authentication

Set one credential and override it per device. The exporter reads overrides top to
bottom and uses the first match:

```yaml
shelly:
  auth:
    username: admin
    password_file: /run/secrets/shelly_password
    overrides:
      - match: {hostname: "shelly1-*"}
        password_file: /run/secrets/shelly_legacy_password
      - match: {device_id: "mac:a8032ab1c2d3"}
        password: "per-device"
```

Match on `device_id`, `mac`, `hostname`, or `gen`. Gen2 and later accept only the
username `admin`, so the exporter overrides any other value and logs a warning once.

### Shelly Cloud

One Shelly Cloud account key enables two independent features:

- the `shelly_cloud` discovery backend, which lists every device on the account with its
  LAN address
- `shelly.cloud.names`, which takes device names, channel names and rooms from the Shelly
  app

You can enable either without the other. For example, keep `avahi` for discovery and take
only the names from the cloud.

```yaml
shelly:
  cloud:
    server: https://shelly-58-eu.shelly.cloud
    auth_key_file: /run/secrets/shelly_cloud_key
    names: true
discovery:
  sources: [avahi, static, shelly_cloud]
```

In the Shelly app, open User settings → Authorization cloud key. Copy the key, and the
server shown with it, into the config.

The key grants control of every device on the account, not just read access. Mount it as a
file, as above, rather than putting it in a compose file.

The key changes whenever you change the account password, and Shelly may move the account
to a different server. Either change stops discovery and freezes names. Alert on
`espressif_exporter_discovery_source_up{source="shelly_cloud"}` and
`espressif_exporter_shelly_cloud_names_last_success_timestamp_seconds` to catch it.

The cloud limits each account to one request per second, and it counts a request that
overlaps another against that limit. The exporter therefore sends one request at a time,
with a two-second pause after each. In steady state it makes one request every 5 minutes
for discovery and two every 15 minutes for names.

The Shelly app and other integrations on the same account share the limit. When the cloud
refuses a request with `max_req`, the exporter waits 10 seconds and tries again, up to
twice. If the room list fails, only the `area` label is affected, and the previous room
names stay in use. If a names refresh fails, the exporter retries it after 30 seconds,
doubling the delay up to `refresh_interval`.

At startup the exporter waits up to 30 seconds for the first names refresh before it
starts discovery, so that no device is scraped under its own names first.

The device-list and room-list endpoints that names depend on are the ones the Shelly app
itself uses. Shelly does not document them. If names don't appear, capture the response
and compare it with `internal/shellycloud/testdata/device_list.json`:

```bash
curl -s -d "auth_key=$KEY" https://shelly-58-eu.shelly.cloud/interface/device/list | jq .
```

## Metrics

Both vendors write to one namespace with one label set, so one query covers the fleet:

```promql
sum(espressif_power_watts)
```

### Labels

Every value series carries these labels. An absent label holds an empty string, which
Prometheus treats as absent when matching.

| Label | ESPHome | Shelly |
|-------|---------|--------|
| `device` | Stable identifier. Never an IP address. | Same. |
| `kind` | `esphome` | `shelly` |
| `component` | Entity domain, such as `sensor`. | Component type, such as `switch`. |
| `id` | Entity object identifier. | Component instance index. |
| `name` | Entity name. | Component name. |
| `device_class` | From the entity metadata. | Derived from the metric family. |
| `phase` | Empty. | `a`, `b`, `c`, `n`, or `total` on polyphase meters. |
| `area` | From the device metadata. | The device's room in the Shelly app, with [`shelly.cloud.names`](#shelly-cloud). Otherwise empty. |

### Units

The exporter converts every reading to a Prometheus base unit.

| Quantity | Metric | Unit |
|----------|--------|------|
| Power | `espressif_power_watts` | watts |
| Energy | `espressif_energy_joules_total` | joules, as a counter |
| Voltage | `espressif_voltage_volts` | volts |
| Current | `espressif_current_amperes` | amperes |
| Temperature | `espressif_temperature_celsius` | degrees Celsius |
| Humidity | `espressif_humidity_ratio` | ratio from 0 to 1 |
| Signal strength | `espressif_wifi_rssi_dbm` | dBm |

Energy uses joules because the Prometheus naming guide names it as the base unit. To
display kilowatt-hours in Grafana, divide by `3.6e6` and set the panel unit to `kWh`.

Percentages become ratios, so humidity reads `0.482`. Grafana renders this correctly with
the `percentunit` panel setting. Set `metrics.percent_as_ratio: false` to keep raw
percentages.

A reading whose unit the exporter does not recognise goes to `espressif_sensor_value`
with a `unit` label. This is the only metric family that carries a unit label.

### Missing readings produce gaps, not zeros

A device reports a missing reading as JSON `null`, `is_valid: false`, `missing_state`, or
`NaN`. The exporter emits no value for these. It emits
`espressif_entity_unavailable{reason="..."}` instead.

A zero for a faulted current clamp looks plausible and is wrong. A `NaN` silently
corrupts `avg`, `sum`, and `rate`.

### Probe metrics

Every probe emits these, whatever the outcome:

```
probe_success 0|1
probe_duration_seconds <float>
probe_http_status_code <int>
probe_failure_reason{reason="..."} 0|1
```

`probe_failure_reason` emits every reason on every scrape. Exactly one holds 1 when
`probe_success` is 0. Emitting only the active reason would leave the previous one to
expire five minutes later, so a query would report two reasons at once.

### Freshness

ESPHome state arrives by push, so a cached value can be old. The exporter exports the age
rather than hiding it:

```promql
time() - espressif_entity_updated_timestamp_seconds{device_class="temperature"} > 600
```

The exporter does not suppress old values by default. A `total_daily_energy` sensor
updates hourly and a Wi-Fi SSID sensor updates once per boot. Suppressing them would
break `rate()` on energy counters. Set `esphome.max_state_age` to opt in to suppression.

When a device has no live connection, the exporter emits no entity values at all, only
device-level metrics. Prometheus staleness then applies.

### Uncurated Shelly components

Shelly ships new component types often. The exporter has curated extractors for the known
types. It exports an unknown type under `espressif_raw_*` as an untyped gauge with no unit
suffix.

Treat `espressif_raw_*` names as unstable. `espressif_exporter_shelly_unknown_components_total`
counts each unknown type, which tells you when a component needs a curated extractor. Set
`shelly.generic_fallback: false` to disable the fallback.

### Shelly names

Each Shelly series carries a `name` label taken from the device's own configuration — the
per-component name you set in the app, such as a switch named "Water Heater". The exporter
reads it from `Shelly.GetConfig` on Gen2+ and `/settings` on Gen1, refreshed on the
`shelly.identity_ttl` timer rather than on every scrape. Set `shelly.fetch_config: false`
to skip the request; series then carry an empty `name`.

The `device_name` label on `espressif_device_info` comes from the same request on Gen1.
Gen1 firmware omits the name from `/shelly`, so `/settings` is the only endpoint that
reports it. With `shelly.fetch_config: false`, a Gen1 device falls back to its mDNS
hostname, such as `shelly1-c45bbe7891bf`. Gen2+ report the name on `/shelly` and are
unaffected.

With `shelly.cloud.names: true`, names from the Shelly app take precedence. The device
falls back to its own name wherever the app has none. The order is cloud, then device,
then mDNS:

- **Device name.** A single-channel device takes its name from the app. A multi-channel
  device keeps its own name, because the app names each channel separately and has no name
  for the device as a whole.
- **Channel names.** Each channel in the app has a category, and channel *n* names
  component *n* of the matching type. The name covers every series that component emits,
  including power, energy, voltage and current, not just its on/off state.

  | Category | Components |
  |----------|------------|
  | `relay` | `switch` |
  | `roller`, `cover` | `cover` |
  | `light` | `light`, `rgb`, `rgbw`, `cct` |
  | `emeter` | `em1`, `pm1` |

  On a Shelly EM or Pro EM-50, the channels are the meters, so the full meter names replace
  the truncated ones stored on the device, and the relay keeps its own name.
  - Inputs and sensors keep the device's own names.
  - `em` is never named from the app, because three-phase readings are one component split
    by `phase`.
- **Room.** The room becomes the `area` label.

Renaming a device or moving it to another room in the app changes label values, which
starts new series. The exporter keeps the last good names through a cloud outage. After a
restart, discovery waits for the first refresh, so restarting does not rename series
either.

## Terminology

| Concept | Approved term | Do not use |
|---------|---------------|------------|
| A physical ESPHome or Shelly unit | device | node, unit, target |
| One scrape of one device | probe | poll, query, sample |
| Stable device identifier | device identifier | device ID, target ID |
| Finding devices with mDNS | discovery | detection, scanning |
| A discovery implementation | backend | provider, driver |
| The set of known devices | registry | inventory, cache |
| Shelly measurement block | component | channel, sensor |
| ESPHome measurement | entity | sensor, reading |
| One metric name and its labels | metric family | metric type |
| Shared Noise key | encryption key | PSK, secret, token |

## Troubleshooting

### The exporter finds no devices

Check that Avahi works on the host, one service type per invocation:

```bash
avahi-browse -rt _shelly._tcp
avahi-browse -rt _esphomelib._tcp
```

Then check what the exporter sees:

```bash
curl -s localhost:9826/metrics | grep espressif_exporter_avahi_up
curl -s localhost:9826/debug/devices | jq 'length'
```

`espressif_exporter_avahi_up 0` means the exporter cannot reach `avahi-daemon`. The
`avahi session failed` log line classifies why, in its `cause` field, and its `hint` field
names the command to run or the setting to change:

| `cause` | Meaning |
|---------|---------|
| `socket_missing` | The bus socket is not in the container. The mount is missing. |
| `not_a_socket` | The path exists but is not a socket. Docker creates a directory when the host side of a bind mount is missing. |
| `connection_refused` | The socket is stale: `dbus-daemon` restarted after the container started. |
| `permission_denied` | The container's uid cannot write to the socket. |
| `auth_rejected` | The host refused the uid the kernel reports for the socket, which points at a user-namespace remap. |
| `hello_dropped` | The host cannot resolve the container's uid. Run `getent passwd` for that uid on the host. |
| `policy_denied` | The host's D-Bus policy refuses this client access to Avahi. |

### The exporter crash-loops with `D-Bus Hello: ... broken pipe`

```
WARN  avahi session failed  cause=hello_dropped uid=65532
ERROR discovery backend failed  source=avahi error="avahi was unreachable for 30s: ..."
```

The host is healthy: the compose file mounts the socket, `dbus-daemon` and `avahi-daemon`
run, and `avahi-browse` resolves devices. `dbus-daemon` authenticates the connection, then
closes it while handling `Hello`, without sending a D-Bus error. It does that when it
cannot look the peer's uid up in the host's passwd database, which it does before it
accepts any connection. It records the refusal at verbose level only, so the host journal
stays silent.

Check the uid the container presents:

```bash
getent passwd 65532
```

No output means the host cannot resolve it. Run the container as a uid that exists on the
host: `user: "568:568"` on TrueNAS SCALE, or `user: "65534:65534"` on most Linux hosts.

### Avahi is the only source and the exporter exits

`discovery.avahi.required: true` stops the process when Avahi is the only live backend,
which is deliberate: a discovery-only exporter that discovers nothing has nothing to
export. Note that `sources: [avahi, static]` with an empty `static:` list is still one
live backend, because a static backend with no entries never starts.

Add `discovery.static` entries, or set `discovery.avahi.required: false`.

### A device has no address

`/debug/devices` lists filtered addresses under `rejected_addrs`. The exporter rejects
link-local, loopback, and multicast addresses always, and IPv6 unless you set
`discovery.ipv6: true`.

A narrow `discovery.allow_cidrs` is the most common cause. The exporter also logs a
warning that names the rejected addresses.

### A probe returns `mac_mismatch`

The device at that address reports a different MAC address than the identifier expects.
DHCP reassigned the address to another device. The exporter emits no metrics rather than
attributing one device's readings to another. Discovery corrects this on the next
resolve.

### An ESPHome node connects, then fails with `not_connected`

The node requires an API password. ESPHome before 2026.1.0 answers the device-information
request without authentication, then closes the connection on the next request. A node
with encryption enabled refuses plaintext outright instead, so this pattern identifies a
password specifically. The log names the node and the fix.

Set `esphome.password`, or migrate the node to an encryption key. ESPHome removed
password authentication in 2026.1.0.

A log line reading `node did not accept the encrypted handshake; using plaintext` is
expected and harmless. The mDNS record does not reliably say whether a node is
encrypted, so the exporter tries the handshake and falls back once.

### A probe returns `auth`

The exporter reached the device, and the device rejected the credential. Compare this
with `espressif_auth_required`, which reports whether the device has authentication
enabled. An offline device returns `timeout` or `conn_refused` instead.

Authentication failures use a longer retry interval, from 1 minute to 15 minutes.
Retrying a wrong password more often does not help, and some Shelly firmware rate-limits
repeated failures.

### A probe returns `backoff`

The exporter stopped contacting an unreachable device so often. It still emits
`probe_success 0` on every scrape, so alerts continue to work.
`probe_backoff_remaining_seconds` reports the wait. A device that re-announces itself on
mDNS clears its backoff at once.

### Avahi restarts and discovery goes quiet

The exporter detects this three ways: a D-Bus name-owner change, a 30-second watchdog,
and a periodic rebrowse. The watchdog covers the case where `avahi-daemon` dies and the
D-Bus connection stays healthy, so the browsers go silent without any error.

The rebrowse matters for more than recovery. Avahi replays its record cache only to a
newly created browser, and stays silent about services it already knows, so for a device
that is quiet on mDNS the rebrowse is the only thing that re-observes it. Keep
`registry.endpoint_ttl` above `discovery.avahi.rebrowse_interval`: if it is shorter, every
device loses its address partway through each cycle and its metrics vanish until the next
rebrowse. The exporter refuses to start on that combination.

Alert on a stalled backend. The threshold has to stay well clear of
`rebrowse_interval`, or a healthy rebrowse trips it:

```promql
time() - espressif_exporter_discovery_last_event_timestamp_seconds{source="avahi"} > 3600
```

## Development

`mise.toml` pins every tool:

```bash
mise install
mise run test     # go test with the race detector
mise run lint     # golangci-lint, hadolint, actionlint
mise run build
mise run dev      # runs against configs/dev.yaml
mise run docker
```

The `zeroconf` backend finds nothing on macOS, because `mDNSResponder` owns port 5353
exclusively. The exporter detects this at startup and logs the reason. Generate a static
device list from Bonjour instead:

```bash
scripts/macos-static-config.sh 10 > devices.yaml
```

Append the result to your config and set `discovery.sources: [static]`. Regenerate it
after a DHCP change.

### Layout

| Path | Holds |
|------|-------|
| `internal/discovery/` | The `Discoverer` interface and its three backends. |
| `internal/registry/` | Device identity, address selection, and snapshots. |
| `internal/probe/` | Concurrency limits, request de-duplication, and backoff. |
| `internal/metrics/` | Metric families, units, and the emitter. |
| `internal/shelly/` | The Shelly collector for both generations. |
| `internal/esphome/` | The ESPHome native API client and connection manager. |
| `internal/server/` | The HTTP endpoints. |

## Licence

MIT.
