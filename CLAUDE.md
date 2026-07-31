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

Two config rows can nonetheless *share* one `deviceKey` — the same IP typed twice, or
an IP filled into a row another row already carries — and they then share every map
above, `castURLs` included. With a different URL on each row the monitor cast row
one's page, judged it stale against row two's, cast that, and repeated the pair on
every tick: an always-on dashboard restarting itself forever. `autoCastTargets` acts
on the first such row only, and `duplicateDeviceKeys` feeds `configWarning` so the
others say why they are inert.

`autoCastTargets` is also where every skip `monitorDevices` makes *before* touching
the network lives. Keeping it pure is what makes those skips testable at all — the
rest of the loop cannot run in a test, because it shells out.

Concretely (traced through catt 0.13.1, `cli.py` → `util.echo_status` →
`controllers.cast_info`): `catt status` describes the *media* session, and a web page
cast has none. DashCast does not support the Google media namespace, so `_is_idle` is
false, `title` is `None`, and `_is_audiovideo` is false — meaning no `player_state`.
Output for "our dashboard is up" is therefore byte-identical to a genuinely idle
device: two `Volume:` lines and nothing else. There is no signal to infer from, and
guessing "idle" would re-cast every tick and restart the dashboard forever. Don't build
on that path; an `auto_cast` device with no IP gets a `Warning` from `configWarning`
instead, because the monitor genuinely cannot watch it.

`configWarning` is where *every* silent `continue` in `monitorDevices` gets explained.
A skipped device is invisible — its card reads a plain "Idle" and looks identical to
one auto-cast is happily managing — so a new skip condition needs a warning alongside
it. It takes the *effective* URL (`effectiveURL`) and a `duplicate` flag, not just the
device, because "no URL and no default" and "two rows for one device" are properties
of the device *and* its context, not of the device alone. Warnings, not `castErrors`:
a standing config problem recorded as a fresh cast failure every tick would keep
bumping `castActions` and suppress every status observation of that device for good.
The implication runs one way only — a warned device may still be cast to (one with no
IP is cast blind; the first row of a duplicated pair is the row that gets cast) — so
`TestEverySkippedDeviceIsExplained` asserts only the direction that hides a problem.

The full set of labels `catt status` can print is `Title:`, `Time:`, `Remaining time:`,
`State:`, `Volume:` and `Volume muted:` — so `getLiveStatus` parses `State: ` and
nothing else. It once also looked for a `Content: ` line, which catt has never emitted;
that fed a `DeviceStatus.URL` no code ever read. `State: ` itself only appears when the
media session reports a non-image `content_type`, which our own `cast_site` never does
and a media app may, so it is narrow but not dead — keep it, and don't add parsing for
labels without checking that list first. Parse it with `splitLines`, not
`bufio.Scanner`: the scanner's default token limit is 64KB, which is exactly
`maxSubprocessOutput`, so a single unterminated line made the first `Scan` fail
with `ErrTooLong` — an error both callers discard — and the parser saw nothing
at all.

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
already playing. `castURLs` is the other half of the skip: `isCasting` only says *something* of
ours is up, not that it is the *current* page, so a device already showing the
old URL was skipped forever and editing `default_url` looked like a no-op. Only
a cast of ours writes it — a device merely *observed* playing gets no entry, so
one already up when the process starts is left alone rather than restarted.

`castObserved` does the same for polls against *each other*:
`/api/devices/status` and the monitor probe the same device independently, so
whichever started earlier can easily finish later. Any new timestamped state
belongs in `pruneCastStates` and in `resetCastState` in the tests.

**The monitor does not just sleep the interval.** `monitorLoop` waits on a timer
that `POST /api/config` interrupts through `configChanged`, and re-reads
`checkInterval()` each time round. The interval ceiling is a day, so a plain sleep
on the value read at the top of the cycle applied a *lowered* interval up to 24h
late and the save looked like it had done nothing. The deadline stays measured from
the start of the wait, so a burst of saves can only shorten it, never postpone the
next poll.

**A cast is reported before it happens.** `/api/devices/cast` and `/stop` answer
immediately and run `catt` in a goroutine, so failures can't ride on the HTTP response.
They land in `castErrors` and get merged into the next `/api/devices/status` payload.
Those goroutines deliberately use `context.Background()`, not `r.Context()`, which is
already cancelled by the time they run.

**No authentication anywhere.** This is a trusted-LAN tool. The scan endpoint probes
every host on a /24, so don't expose it. `POST /api/config` caps its body with
`http.MaxBytesReader` for this reason (an unbounded `json.Decode` will consume all
available memory), as do the cast and stop handlers. All three report the rejection
through `rejectBody`: `MaxBytesReader` surfaces an over-long body as an ordinary
decode error, so without it every one of them answered "400 — your JSON is
malformed", sending the caller after a syntax error that is not there.

That handler replaces the *whole* config, so it must never read "no value" as "the
empty value". It decodes into a `*Config` and rejects `nil`: a body of `null`
decodes into a `Config` without error, leaves the zero value, and `normalizeConfig`
turns that into a perfectly valid empty config — every device deleted from disk,
every cast state pruned, 200 returned. It also rejects anything after the object
(`dec.More()`), because `Decode` stops at the first value and a concatenated or
double-encoded body would otherwise persist only its first half.

Everything a subprocess puts in a `DeviceStatus` goes through `shortText`, `State`
included — that field carries the receiver app's own `display_name`, whose length is
decided by whoever wrote that app, and it is echoed in every status response for as
long as the app is up. Python's side of the same bound is `clip`: the reader keeps
only the first 64KB of stdout, and a *truncated* JSON object does not parse at all,
so an unbounded traceback in `detail` would take the `error` field down with it and
leave the caller with no diagnosis whatsoever.

`DeviceStatus` carries `Host` as well as `Name` so the UI can tell whether its local
device list still lines up with the server's. Pairing is by index; the identity check
is what catches an unsaved add or delete. `Name` alone was not enough — a host-only
entry has none, so two of them both read `""` and deleting the first handed the
second the deleted device's status. Both structs use `omitempty` on `Host` so the two
sides compare equal, and the UI coalesces to `''` because a field typed into and
cleared is `''` locally but absent on the wire.

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
  than a preceding `status` (the UI overwrites `scanStatus` when the stream ends —
  which is why `tcpScan` *returns* its give-up reason instead of sending it through
  `onStatus`); and the server sets no `WriteTimeout`, which would kill the stream
  mid-scan.

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
