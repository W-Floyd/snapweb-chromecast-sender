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
| [scripts/test_cc_status.py](scripts/test_cc_status.py) | Its tests — no pychromecast, no device; kept out of the image by `.dockerignore` |
| [static/index.html](static/index.html) | Whole web UI — markup, styles and JS in one file |
| [Dockerfile](Dockerfile) | Two stages: Go build (alpine) → Python runtime (debian slim) |
| [.dockerignore](.dockerignore) | Keeps `__pycache__`, `config/` and the local binary out of the image |

The Go module has **no dependencies** (no `go.sum`) — standard library only. Keep it
that way unless there's a strong reason; it's what makes the build trivial and fast.

## Commands

```sh
go build -o /dev/null . && go vet ./... && go test -race ./...   # the full check
gofmt -l .                                                        # must print nothing
python3 -m unittest discover -s scripts                           # the status helper

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
rows when running `go run` on a dev machine. That's expected. **No test may shell out
to `catt` or to the status helper** — keep it that way.

The rest of the logic is still tested through function variables the tests substitute:

| Seam | Substituted by | Because |
|---|---|---|
| `probeDevice`, `castSite`, `stopCast` | `withProbe`, `withFakeCatt` | `monitorDevices` is the only code that both probes a device and decides whether to cast to it, and the `/cast` and `/stop` goroutines are where a failure *becomes* a `castErrors` entry |
| `mdnsScan`, `tcpScanner` | `withScanners` | an mDNS hit ends the SSE stream early, and the no-subnet fallback probes 254 hosts of the tester's own LAN |
| `cattCmd` | `withCattStandIn` | `runCatt`'s own decisions, and the argv each catt call is built with |
| `detectSubnets` | `withDetectedSubnets` | same as `tcpScanner`, one layer down: which subnet auto-detection hands to the probe, and the give-up message when it finds none |
| `chromecastSetupPort` | (set directly) | the confirm step — dial, fetch `/setup/eureka_info`, decode, require a name — otherwise needs a real Chromecast |

A new subprocess call belongs behind one of these, and a new decision in the loop
belongs in a test that drives it. The fake probe deliberately *applies* the observation
the real one does, because the loop reads `castStates`, not the returned struct.

`cattCmd` points at this test binary re-invoked to run `TestCattStandIn` (its mode
arrives in the environment, because catt's own `-d` would reach the child's flag parser
as an unknown flag). That is not a way of running catt: it is how the deadline `runCatt`
*names* — so a killed subprocess does not reach the card as "signal: killed" — and the
subcommand and timeout budget of `cattStatus`, `castSite`, `stopCast` and `cattScan` get
asserted. A typo in one of those subcommand names is otherwise invisible until a device
fails to cast.

`interpretStatusOutput` is the same split one layer down: `getPychromecastStatus` is
only the subprocess plumbing, and everything that *reads* the result — the diagnostic
precedence, the stale-poll fallback, the app-id learner, the bounding of device-supplied
text — is a pure-ish function a test can call directly. It decodes the payload into a
**pointer** and refuses `nil`, exactly as `POST /api/config` does with its body and for
the same reason: a bare `null` decodes into a value without error and leaves an empty
`error` and `is_idle: false`, which reads as "the device is playing something" — a state
nobody reported, written into `castStates` and rendered on the card.

Timeout budgets are layered and must stay ordered: `PROBE_TIMEOUT` < `CONNECT_TIMEOUT`
< `OVERALL_TIMEOUT` (12s watchdog) < the Go side's 15s context in
`getPychromecastStatus`. A subprocess killed by Go exits with no output, which is the
failure mode the watchdog exists to avoid. The ordering is asserted rather than
commented — `TestStatusQueryTimeoutOutlastsTheScriptWatchdog` reads the constants
straight out of `cc_status.py`, no interpreter needed, because a comment asking the next
reader to go and check the other language is exactly what does not happen.

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
every tick: an always-on dashboard restarting itself forever. `autoCastTargets` acts on
the first such row with auto-cast enabled and no other, and `duplicateDeviceKeys` feeds
`configWarning` so the rest say why they are inert.

`autoCastTargets` is also where every skip `monitorDevices` makes *before* touching the
network lives. Keeping it pure keeps those skips assertable one row at a time; the rest
of the loop is covered through the subprocess seams described above. A skip is invisible
from the UI — the card reads a plain "Idle", identical to a device auto-cast is happily
managing — which is what `configWarning` below exists to fix.

Concretely (traced through catt 0.13.1, `cli.py` → `util.echo_status` →
`controllers.cast_info`): `catt status` describes the *media* session, and a web page
cast has none. DashCast does not support the Google media namespace, so `_is_idle` is
false, `title` is `None`, and `_is_audiovideo` is false — meaning no `player_state`.
Output for "our dashboard is up" is therefore byte-identical to a genuinely idle
device: two `Volume:` lines and nothing else. There is no signal to infer from, and
guessing "idle" would re-cast every tick and restart the dashboard forever. Don't build
on that path; an `auto_cast` device with no IP gets a `Warning` from `configWarning`
instead, because the monitor genuinely cannot watch it.

`configWarning` is where every one of those skips gets explained, so a new skip
condition needs a warning alongside it. It takes the *effective* URL (`effectiveURL`)
and a `duplicate` flag, not just the device, because "no URL and no default" and "two
rows for one device" are properties of the device *and* its context. Warnings, not
`castErrors`: a standing config problem recorded as a fresh cast failure every tick
would keep bumping `castActions` and suppress every status observation of that device
for good.

Order and wording matter inside it, and both hinge on the same fact:
`autoCastTargets` claims a `deviceKey` for the first row *with auto-cast enabled*,
before it looks at that row's URL.

- The two URL problems are reported *ahead* of the duplicate advisory, because they
  stop the device being cast at all whereas the duplicate note only says which row does
  the casting. A duplicated pair whose first row has the unusable URL is never cast, so
  leading with the duplicate advisory hid the one problem the user could fix.
- The advisory says "only the first **with auto-cast enabled**", not "only the first".
  Tick the box on the second row of a pair and not the first, and the second row is the
  one being cast — and it is the row the advisory is rendered on, so naming the first
  told the reader that the row in front of them was inert when it was the only one
  doing anything.
- A row with auto-cast off skips the URL gates entirely and keeps the advisory, but a
  *different* one (`duplicateEntryWarningNoAuto`): naming the row that gets cast is a
  consequence that cannot apply to a row with the box unticked, and reads as though
  ticking it would be pointless. What is left is the cost that applies either way —
  both rows show the same state and the same cast error.

The implication runs one way only — a warned device may still be cast to (one with no
IP is cast blind; the first row of a duplicated pair is the row that gets cast) — so
`TestEverySkippedDeviceIsExplained` asserts only the direction that hides a problem.

The full set of labels `catt status` can print is `Title:`, `Time:`, `Remaining time:`,
`State:`, `Volume:` and `Volume muted:` — so `cattStatusState` parses `State: ` and
nothing else, and ignores it when empty (a bare `State: ` blanked the card's only state
line, which reads as a UI fault rather than as the device saying nothing). It once also
parsed a `Content: ` line, which catt has never emitted, feeding a `DeviceStatus.URL` no
code ever read. `State: ` appears only when the media session reports a non-image
`content_type`, which our own `cast_site` never does and a media app may — narrow but
not dead. Don't add parsing for labels without checking that list first. Parse with
`splitLines`, not `bufio.Scanner`: the scanner's default token limit is 64KB, exactly
`maxSubprocessOutput`, so a single unterminated line made the first `Scan` fail with
`ErrTooLong` — an error both callers discard — and the parser saw nothing at all.

The five maps under `castStatesMu`, and the regression each one exists for:

| Map | Holds | Without it |
|---|---|---|
| `castStates` | is the device playing *something* — not necessarily ours | — |
| `castURLs` | the page **we** put on screen, when we know it | `isCasting` says something of ours is up, not that it is the *current* page, so a device showing the old URL was skipped forever and editing `default_url` looked like a no-op |
| `castErrors` | why our last cast or stop failed | a cast that failed over a page still on screen is never retried (see below) |
| `castActions` | when we last cast or stopped | a poll that started before the cast lands after it and overwrites the newer truth — erasing fresh errors, re-casting devices already playing |
| `castObserved` | when the newest applied poll began | `/api/devices/status` and the monitor probe independently, so the poll that started earlier can finish later and republish the older view |

Only a cast of *ours* writes `castURLs`: a device merely observed playing gets no
entry, so one already up when the process starts is left alone rather than restarted.

A live `castErrors` entry is the *third* reason to cast, alongside "idle" and "stale
URL", and it covers the normal failure shape for an always-on dashboard: `cast_site`
fails, the page it was replacing is still up, the next probe reports the device as
playing, and `setCastError` has already dropped the `castURLs` entry — so "playing, not
foreign, not stale" skipped the device for the life of the process. Which is also why an
observation *never* clears an error, playing or idle: "playing something" is not evidence
that our cast worked. Only `setCastState` clears one. The cost is that a device nothing
is auto-casting keeps a red error beside a healthy "Playing" until someone casts to it
again — the accurate report of what happened, and much the cheaper of the two failure
modes.

`isCasting` means "playing *something*" — a person casting Netflix sets it too. Telling
the two apart needs the `app_id` the status helper reports, and `learnedCastApp` picks
that up from the first poll after a cast we initiated rather than hardcoding DashCast's
id. Don't be tempted to hardcode it: if the constant is wrong the monitor reads its own
dashboard as a foreign app and re-casts it every tick, so an app we cannot place
deliberately reports *not* foreign. A device's `takeover` flag is what then lets
auto-cast reclaim it.

Any new timestamped state belongs in `pruneCastStates` and in `resetCastState` in the
tests.

**The monitor does not just sleep the interval.** `monitorLoop` waits on a timer that
`POST /api/config` interrupts through `configChanged`, and re-reads `checkInterval()`
each time round through `remainingWait`. The ceiling is a day, so sleeping on the value
read at the top of the cycle applied a *lowered* interval up to 24h late and the save
looked like it had done nothing. The deadline stays measured from the start of the wait,
so a burst of saves can only shorten it, never postpone the next poll. `remainingWait`
and `awaitNextTick` are separate functions so that is a direct assertion rather than a
test that waits out a real interval — `monitorLoop` cannot be called from a test at all,
since it never returns.

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
so one unbounded field takes the whole payload down with it — an unbounded traceback
in `detail`, or an unbounded `app_id`, and the caller gets neither a status nor a
diagnosis. Every device-supplied string in that payload is clipped for that reason,
`app_id` included even though nothing renders it: it is compared against, and stored
as, the id our own casts run under. `device_is_idle` is applied to the *raw* id, before
the clip, so truncating cannot turn one id into another by accident.

`detail` is the exception to "nothing renders it": it goes to the **log**, not into the
`DeviceStatus`. It is the only thing that identifies a broken pychromecast install, and
`error` already carries the one-line summary that fits on a card.

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

- **`catt scan` output is line-based**, `"<ip> - <name> - <mfr> <model>"`. A wrong
  guess at this format silently returns zero devices and falls through to the slow
  TCP path — which is exactly what a labelled `Name:` / `Host:` parser, written on a
  guess and kept for years after the real format was added, did nothing to prevent.
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
