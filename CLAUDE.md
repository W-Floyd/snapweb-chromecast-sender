# CLAUDE.md

Guidance for working in this repo.

## What this is

A small LAN service that keeps a web page (a Snapweb dashboard) cast to Chromecast
devices. It polls each configured device and re-casts when one goes idle, and serves
a single-page web UI for managing devices and config.

## Layout

| Path | Role |
|---|---|
| [main.go](main.go) | The entire server: config, monitor loop, HTTP API, network scanning |
| [main_test.go](main_test.go) | Unit tests for the parsing and state-tracking helpers |
| [scripts/cc_status.py](scripts/cc_status.py) | pychromecast status helper, invoked as a subprocess per poll |
| [static/index.html](static/index.html) | Whole web UI — markup, styles and JS in one file |
| [Dockerfile](Dockerfile) | Two stages: Go build (alpine) → Python runtime (debian slim) |
| [.dockerignore](.dockerignore) | Keeps `__pycache__`, `config/` and the local binary out of the image |

The Go module has **no dependencies** (no `go.sum`) — standard library only. Keep it
that way unless there's a strong reason; it's what makes the build trivial and fast.

## Commands

```sh
go build -o /dev/null . && go vet ./... && go test -race ./...   # the full check
gofmt -l .                                                        # must print nothing

# Run locally. The defaults are container paths, so the env overrides are required:
CONFIG_PATH=$PWD/config/config.json STATIC_DIR=$PWD/static PORT=8083 go run .
```

`-race` matters: `castStates`, `cfg` and the scan's progress counters are all touched
from concurrent goroutines.

To exercise a local change end to end you must build the image yourself —
[docker-compose.yml](docker-compose.yml) pins a published `image:` and has no build
context, so `docker compose up --build` silently pulls `:main` instead of building:

```sh
docker build -t cc-sender-test .
docker run --rm -p 8083:8080 -v "$PWD/config:/config" cc-sender-test

# Reaching real devices needs host networking. On macOS that puts the server inside
# the Docker VM, so curl it from within the container rather than from the host:
docker run -d --name cc-test --network host -e PORT=8083 -v "$PWD/config:/config" cc-sender-test
docker exec cc-test python3 -c "import urllib.request;print(urllib.request.urlopen('http://localhost:8083/api/devices/status').read().decode())"
```

Casting during a manual test switches on a real TV. Point tests at an offline device
when you only need to exercise a failure path.

## The subprocess boundary

The server shells out for everything device-related. There is no Go Chromecast library
here.

- **`catt`** (CLI) — casting, stopping, mDNS scan. Always via `runCatt`.
- **`python3 scripts/cc_status.py <ip>`** — real device state. Contract: exactly one
  JSON object on **stdout**; stderr is captured separately because zeroconf and
  pychromecast log there and mixing the streams makes the JSON unparseable.

Always write that object through `emit()`. It flushes — stdout is a pipe, so it is
block-buffered, and a payload left in the buffer when the process is killed reaches Go
as nothing at all. It also drops everything after the first object, because the
watchdog thread can fire mid-emit and two concatenated objects are not valid JSON.

Neither is installed outside the Docker image, so `/api/devices/*` degrades to error
rows when running `go run` on a dev machine. That's expected. **The tests are pure Go
and need neither** — keep it that way; don't add tests that shell out to `catt`.

Timeout budgets are layered and must stay ordered: `PROBE_TIMEOUT` < `CONNECT_TIMEOUT`
< `OVERALL_TIMEOUT` (12s watchdog) < the Go side's 15s context in
`getPychromecastStatus`. If you change one, check the others — a subprocess killed by
Go exits with no output, which is the failure mode the watchdog exists to avoid.

The plain-TCP `reachable()` check before importing pychromecast is not redundant.
`get_chromecast_from_host` makes a blocking HTTP call to determine cast type that takes
~30s to give up on an offline host, and an unbounded `disconnect()` then joins a
retrying socket thread forever; together they blew past every timeout above. An offline
device is the common case, so it gets ruled out in ~2s with a clear message. Keep the
pre-check, and keep `disconnect(timeout=…)` bounded.

## Things that will bite you

**mDNS does not work under Docker bridge networking.** This drives much of the design:
every device should have a `host` IP set, which makes `catt -d <ip>` bypass discovery
entirely. `network_mode: host` on Linux is the only way mDNS scanning works — see the
commented service in [docker-compose.yml](docker-compose.yml).

**Cast state is process-local and inferred.** `catt` reports nothing useful about
web-page casts, so `castStates` tracks it in memory. It is keyed by `deviceKey` —
IP when available, name otherwise — because friendly names are *not* unique and state
leaks between devices otherwise. It's lost on restart (devices get re-cast on the next
tick) and pruned on config save. Never key new state by name alone.

`isCasting` means "playing *something*", not "playing ours" — a person casting
Netflix sets it too. Telling the two apart needs the `app_id` the status helper
reports, and `learnedCastApp` picks that up from the first poll after a cast we
initiated rather than hardcoding DashCast's id. Don't be tempted to hardcode it:
if the constant is wrong the monitor reads its own dashboard as a foreign app and
re-casts it on every tick, so an app we cannot place deliberately reports *not*
foreign. A device's `takeover` flag is what then lets auto-cast reclaim it.

A status poll takes seconds, so one that started before a cast can land after it.
`castActions` timestamps our own casts and `observeCastState` drops any
observation older than that — without it the poll's stale view overwrote the
newer truth, which both erased fresh cast errors and re-cast devices that were
already playing.

**A cast is reported before it happens.** `/api/devices/cast` and `/stop` answer
immediately and run `catt` in a goroutine, so failures can't ride on the HTTP response.
They land in `castErrors` and get merged into the next `/api/devices/status` payload.
Those goroutines deliberately use `context.Background()`, not `r.Context()`, which is
already cancelled by the time they run.

**No authentication anywhere.** This is a trusted-LAN tool. The scan endpoint probes
every host on a /24, so don't expose it. `POST /api/config` caps its body with
`http.MaxBytesReader` for this reason (an unbounded `json.Decode` will consume all
available memory); the cast/stop handlers do not yet, and should if they grow.

### The scanner

Discovery is two-stage — `catt scan` (mDNS) first, a TCP probe of port 8008 as
fallback — and streams progress over SSE. Four things to know:

- **`catt scan` output is line-based**, `"<ip> - <name> - <mfr> <model>"`, not the
  labelled `Name:` / `Host:` form. `parseCattScan` handles both; a wrong guess here
  silently returns zero devices and falls through to the slow TCP path.
- **Auto-detection deliberately skips interfaces.** `localSubnets` filters container
  bridges, `veth` pairs and VPN tunnels (`virtualIfacePrefixes`) — a Docker host has one
  bridge per compose network, and unfiltered it probed ~2800 hosts across eleven subnets
  instead of 254 across the one LAN that matters. Trade-off: devices genuinely behind
  such an interface need the subnet typed in by hand. Falls back to the unfiltered set
  if filtering leaves nothing.
- **One scan at a time**, guarded by the `scanInFlight` atomic; a second caller is
  turned away on the `done` event. A TCP scan touches every host on the subnet, so
  re-clicks would multiply the load for no extra coverage.
- **The SSE stream has its own rules.** `ScanEvent.Checked`/`Total` must not get
  `omitempty` (a zero renders as `undefined / 254 hosts`); errors ride on `done` rather
  than a preceding `status` (the UI overwrites `scanStatus` when the stream ends); and
  the server sets no `WriteTimeout`, which would kill the stream mid-scan.

## Conventions

- **Comments explain *why*, not what** — most existing ones record the regression that
  motivated the code (an empty error string reading as success, a 500ms ticker leaving
  the progress bar short of 100%). Preserve them; that rationale is not recoverable
  from the code. Write new ones in the same register.
- `normalizeConfig` is the single place defaults, clamps and trimming happen, so what
  gets stored, what `GET /api/config` returns, and what the monitor acts on can't
  disagree. Put new validation there, not in the handler.
- `/api/devices/status` fans out one goroutine per device and writes results **by
  index**, not `append` — appending ordered them by whichever device answered first, so
  the UI list reshuffled on every poll. `getDeviceStatus` is the layer that merges a
  stale `castErrors` entry over `getLiveStatus`; keep the live result winning when both
  have something to say.
- The frontend has **no build step**. Alpine.js and Tailwind load from CDNs, so the page
  needs internet access even though the service is LAN-only. Adding tooling here would
  be a significant change in direction — ask first.
