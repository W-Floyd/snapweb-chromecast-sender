package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// minCheckInterval is the shortest monitor interval we honour, matching the
// UI's min attribute.
const minCheckInterval = 10

// maxCheckInterval (a day) is the longest interval we honour. The ceiling is not
// cosmetic: checkInterval turns the value into `time.Duration(interval) *
// time.Second`, and past ~9.2e9 seconds that multiplication overflows int64 and
// wraps negative. A negative wait is no wait at all, so a fat-fingered paste in
// the interval box turned the monitor into a hot loop that re-probed and re-cast
// every device continuously.
const maxCheckInterval = 86400

type DeviceConfig struct {
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"` // IP address; bypasses mDNS when set
	URL      string `json:"url"`
	AutoCast bool   `json:"auto_cast"`
	// Takeover lets auto-cast reclaim a device that someone else is using.
	// Off by default: the whole point of noticing a foreign app is to not yank
	// the TV out from under whoever is watching it.
	Takeover bool `json:"takeover"`
}

type Config struct {
	CheckInterval int            `json:"check_interval"` // seconds
	DefaultURL    string         `json:"default_url"`
	Devices       []DeviceConfig `json:"devices"`
}

type DeviceStatus struct {
	Name string `json:"name"`
	// Host echoes the device this status is for. The UI pairs a status with its
	// device by index, and checks these two fields to notice when the indexes no
	// longer line up (a local add or delete that has not been saved yet). Name
	// alone was not enough to tell two devices apart: a host-only entry has none,
	// so after deleting one of two such devices the survivor was handed the
	// deleted device's status — the exact mis-attribution the check exists to
	// prevent. omitempty matches DeviceConfig.Host, so the two compare equal.
	Host  string `json:"host,omitempty"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Foreign marks a device playing an app that is not our cast — someone
	// started something on it. False whenever we cannot tell (see
	// learnedCastApp), so the UI never accuses anyone on a guess.
	Foreign bool `json:"foreign,omitempty"`
	// Warning is advisory: the device's configuration limits what we can do, but
	// nothing has failed. Kept separate from Error rather than folded into it,
	// because getDeviceStatus merges a real cast failure into Error and a
	// standing advisory there would permanently mask it.
	Warning string `json:"warning,omitempty"`
}

type DiscoveredDevice struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

var (
	cfg   Config
	cfgMu sync.RWMutex
	// cfgSaveMu serialises write-then-publish in POST /api/config so that the
	// file on disk and the config the monitor acts on cannot end up disagreeing.
	// Separate from cfgMu, which must not be held across a disk write.
	cfgSaveMu    sync.Mutex
	cfgPath      = "/config/config.json"
	staticDir    = "/static"
	statusScript = "/usr/local/lib/chromecast/cc_status.py"

	// castStates tracks devices we have actively cast to, keyed by deviceKey.
	// catt gives no signal for web-page cast state, so we track it ourselves.
	castStates = map[string]bool{}
	// castErrors holds why the most recent cast/stop attempt failed, keyed by
	// deviceKey. /api/devices/cast answers before catt has run, so without this
	// a failed cast looks identical to a successful one in the UI — the device
	// just quietly stays idle and the reason is only ever in the server log.
	castErrors = map[string]string{}
	// castURLs records the URL of the most recent cast *we* made, keyed by
	// deviceKey. Without it the isCasting skip in monitorDevices held for as long
	// as the device kept showing the old page — which for an always-on dashboard
	// is forever — so saving a new URL, or a new default_url, looked like it had
	// done nothing at all. That is the same silent no-op the interval re-read in
	// monitorLoop exists to prevent.
	//
	// An entry exists only when we know what we put on screen. A device merely
	// *observed* playing (one already showing something when the process started)
	// gets none, because re-casting on that guess would restart a dashboard that
	// was already correct.
	castURLs = map[string]string{}
	// castActions records when we last *acted* on a device (a cast or stop we
	// initiated), keyed by deviceKey. A status probe takes seconds, so a probe
	// that started before the cast can return after it and report the device as
	// still idle — last-writer-wins then erased the fresh state, the monitor
	// re-cast a device that was already playing, and a fresh cast error was
	// cleared by an observation older than the failure. Observations older than
	// the last action are stale by definition and get dropped.
	castActions = map[string]time.Time{}
	// castObserved records when the newest *applied* status poll began, keyed by
	// deviceKey. castActions above only orders polls against our own casts, and
	// polls also race each other: /api/devices/status (browser, every 30s) and the
	// monitor loop probe the same device independently, each taking up to 15s, so
	// the one that started earlier can easily finish later. Last-writer-wins then
	// republished the older view — a device seen playing flipped back to "Idle"
	// until the next tick, and the monitor could re-cast something already up.
	castObserved = map[string]time.Time{}
	// learnedCastApp is the receiver app id our own casts run under (catt's
	// cast_site uses DashCast). It is learned from the first status poll after a
	// cast we initiated rather than hardcoded: a wrong constant here would make
	// the monitor read its own dashboard as someone else's app and re-cast it on
	// every single tick. Empty until learned, and an unknown app is never
	// reported as foreign — being wrong in that direction only costs us a
	// takeover we could have done, not a dashboard that restarts forever.
	//
	// One value for all devices, not one per device: it is a property of catt,
	// so the first auto-cast after a restart re-learns it for the whole fleet.
	learnedCastApp string
	// castAppCandidate is an app id seen once after a cast of ours but not yet
	// trusted. Two separate casts must agree before it becomes learnedCastApp.
	//
	// One observation is not enough. The learn flag is claimed by whatever the
	// first poll after our cast finds running, and that poll is a whole check
	// interval later — anybody who starts their own app in that gap gets recorded
	// as us. Acting on a single sighting made that mistake permanent: the value
	// is fleet-wide, so the real dashboard then read as foreign on every device,
	// a device without takeover was never cast again, and with no further casts
	// there was no later observation that could correct it. Requiring agreement
	// makes a wrong sighting self-healing instead — it clears the trusted value,
	// casting resumes, and the next cast's poll disagrees and re-candidates.
	castAppCandidate string
	// castLearnPending marks devices we have just cast to and whose next
	// non-idle poll therefore identifies that app.
	castLearnPending = map[string]bool{}
	castStatesMu     sync.RWMutex

	// configChanged wakes monitorLoop so it re-reads the check interval. Buffered
	// and signalled non-blockingly: a coalesced wake-up is as good as several, and
	// POST /api/config must never block on the monitor, which can be mid-cast and
	// therefore tens of seconds from looking at this.
	configChanged = make(chan struct{}, 1)

	// scanInFlight serialises /api/devices/scan. A TCP scan fans out to every
	// host on the subnet; letting impatient re-clicks stack them up multiplies
	// that load for no benefit.
	scanInFlight atomic.Bool
)

// deviceKey identifies a device for cast-state tracking. Friendly names are not
// unique — two Chromecasts can legitimately share one — so prefer the IP when
// we have it, otherwise state for one device leaks onto the other.
func deviceKey(dev DeviceConfig) string {
	if dev.Host != "" {
		return "host:" + dev.Host
	}
	return "name:" + dev.Name
}

// deviceLabel names a device for a log line. A device needs only *one* of name
// and host to be castable, and a host-only entry is the configuration this
// service recommends — `catt -d <ip>` bypasses mDNS, which does not work under
// Docker bridge networking at all. Logging dev.Name alone therefore produced
// `auto-casting to ""` and `cast error for ""` for precisely the devices the
// monitor handles best, with nothing in the line to say which one it meant.
// The UI carries the same helper, for the same reason.
func deviceLabel(dev DeviceConfig) string {
	if dev.Name != "" {
		return dev.Name
	}
	if dev.Host != "" {
		return dev.Host
	}
	return "unnamed device"
}

// setCastState records the outcome of a successful cast/stop, and clears any
// error left by a previous failed attempt on the same device. url is the page
// we just put on the device, and is ignored for a stop.
func setCastState(dev DeviceConfig, playing bool, url string) {
	k := deviceKey(dev)
	castStatesMu.Lock()
	castStates[k] = playing
	castActions[k] = time.Now()
	delete(castErrors, k)
	if playing {
		// Absent rather than empty when we have no URL to record: castURLs means
		// "we know what is on screen", and an empty string there would read as a
		// known URL that no configured device can ever match, re-casting on every
		// tick.
		if url != "" {
			castURLs[k] = url
		} else {
			delete(castURLs, k)
		}
		// Whatever the next poll finds running is what our cast runs as.
		castLearnPending[k] = true
	} else {
		delete(castURLs, k)
		delete(castLearnPending, k)
	}
	castStatesMu.Unlock()
}

// lastCastURL returns the page we most recently cast to dev, and whether we
// have one at all. "No entry" is not "no URL": it means we did not put what is
// on screen there, so nothing may be concluded about whether it is current.
func lastCastURL(dev DeviceConfig) (string, bool) {
	castStatesMu.RLock()
	defer castStatesMu.RUnlock()
	u, ok := castURLs[deviceKey(dev)]
	return u, ok
}

// observeCastState records what a status poll saw, without disturbing a recorded
// cast error the way setCastState does. Polling and acting are not the same
// event: a poll that finds the device idle is usually the *consequence* of the
// failed cast, and clearing the error there erased it before
// getDeviceStatus could merge it in — every failure read as a plain "Idle".
//
// A *playing* observation does not clear it either, which is the less obvious
// half. "The device is playing something" is not evidence that our cast
// succeeded — a failed cast usually leaves the previous page up, and the poll
// that follows sees exactly that. Clearing there both wiped the reason and, worse,
// left monitorDevices with nothing to distinguish "playing the page we asked for"
// from "playing the page we failed to replace", so it skipped the device on every
// tick from then on. Only an action of ours succeeding says the error is stale, so
// only setCastState clears it. For an auto-cast device that is the very next
// tick's retry; for one nothing is monitoring, the card keeps a red error beside a
// healthy "Playing" until somebody casts to it again — an accurate report of what
// last happened, and much the cheaper of the two failure modes.
//
// observedAt is when the poll *began*, not when it finished: a probe takes
// seconds, so one started before a cast can land after it and would otherwise
// overwrite the newer truth with what it saw beforehand.
//
// Reports whether the observation was applied. A caller must not draw any other
// conclusion from a poll we dropped — see getPychromecastStatus.
func observeCastState(dev DeviceConfig, playing bool, appID string, observedAt time.Time) bool {
	k := deviceKey(dev)
	castStatesMu.Lock()
	defer castStatesMu.Unlock()
	if acted, ok := castActions[k]; ok && observedAt.Before(acted) {
		return false // this poll predates the cast/stop it would be overwriting
	}
	if prev, ok := castObserved[k]; ok && observedAt.Before(prev) {
		return false // an overlapping poll already reported a later view
	}
	castObserved[k] = observedAt
	if castLearnPending[k] {
		// The flag is consumed by the *first* applied poll after our cast, whatever
		// that poll found. Disarming unconditionally is the point: left armed it
		// never expired, and an hours-later poll — by which time somebody had
		// started their own app — claimed that app as ours. A cast can exit 0
		// without the dashboard sticking, and the next tick re-casts an idle device
		// and re-arms this anyway, so there is nothing to lose by dropping it.
		//
		// Unconditional rather than only on `!playing`, because "playing something
		// we cannot name" is no more evidence than "idle" is. That case rests
		// today on an invariant in cc_status.py (an empty app_id is reported as
		// idle); keeping the flag armed for it would make a drift over there
		// reappear here as the mislearn this exists to prevent.
		delete(castLearnPending, k)
		if playing && appID != "" {
			if castAppCandidate == appID {
				learnedCastApp = appID
			} else {
				// Disagreement, so this is a first sighting: hold it as a candidate
				// and trust nothing until a second cast confirms it. Clearing the
				// trusted value matters as much as not setting one — if the stored id
				// was the mistake, dropping it lets casting resume, which is the only
				// thing that can produce the observation that corrects it.
				castAppCandidate = appID
				learnedCastApp = ""
			}
		}
	}
	castStates[k] = playing
	return true
}

// isForeignApp reports whether appID is something other than our own cast.
// Unknown on either side means "cannot tell", which is deliberately not foreign.
func isForeignApp(appID string) bool {
	castStatesMu.RLock()
	defer castStatesMu.RUnlock()
	return learnedCastApp != "" && appID != "" && appID != learnedCastApp
}

func isCasting(dev DeviceConfig) bool {
	castStatesMu.RLock()
	defer castStatesMu.RUnlock()
	return castStates[deviceKey(dev)]
}

// maxStatusTextLen bounds any subprocess-supplied string we put in a
// DeviceStatus; a failure can print a full Python traceback, and all of it would
// otherwise end up in every status response.
const maxStatusTextLen = 400

// maxLoggedDetailLen bounds the helper's "detail" field on its way to the log.
// Matches MAX_DETAIL in cc_status.py, which is what a well-behaved helper has
// already clipped it to; the bound is here because the field arrives from a
// subprocess and nothing but this reader enforces that.
const maxLoggedDetailLen = 4000

// shortText trims and bounds a subprocess-supplied string on its way into a
// DeviceStatus. Everything we report there is repeated in every
// /api/devices/status response for as long as it stands, so an unbounded one — a
// Python traceback from a broken pychromecast install is the realistic case — is
// paid on every poll and rendered into a card sized for one line. It applies to
// the state as much as to errors: State carries the receiver app's own
// display_name, and nothing in the cast protocol bounds that.
func shortText(msg string) string {
	return boundText(msg, maxStatusTextLen)
}

// boundText trims msg and caps it at limit runes, marking it where it was cut.
func boundText(msg string, limit int) string {
	msg = strings.TrimSpace(msg)
	// Slice by runes, not bytes: a byte-slice can cut a multi-byte character in
	// half and produce invalid UTF-8 in the JSON response.
	if r := []rune(msg); len(r) > limit {
		msg = string(r[:limit]) + "…"
	}
	return msg
}

func setCastError(dev DeviceConfig, msg string) {
	msg = shortText(msg)
	k := deviceKey(dev)
	castStatesMu.Lock()
	castStates[k] = false
	castErrors[k] = msg
	// A failure is an action too: without this an in-flight poll that saw the
	// device playing could land afterwards and delete the error we just recorded.
	castActions[k] = time.Now()
	// Nothing of ours is on screen, so we no longer know what is — the same
	// reasoning that disarms the learn flag below.
	delete(castURLs, k)
	// Disarm the learn flag. Nothing of ours is running, so the next app to
	// appear on this device is somebody else's — and learning *that* as our own
	// makes the real dashboard read as foreign, fleet-wide, and re-cast on every
	// tick, which is exactly what learning the id instead of hardcoding it
	// exists to prevent.
	delete(castLearnPending, k)
	castStatesMu.Unlock()
}

func castError(dev DeviceConfig) string {
	castStatesMu.RLock()
	defer castStatesMu.RUnlock()
	return castErrors[deviceKey(dev)]
}

// pruneCastStates drops entries for devices no longer in the config, so the map
// does not grow without bound across edits over the process lifetime.
func pruneCastStates(devices []DeviceConfig) {
	keep := make(map[string]bool, len(devices))
	for _, d := range devices {
		keep[deviceKey(d)] = true
	}
	castStatesMu.Lock()
	for k := range castStates {
		if !keep[k] {
			delete(castStates, k)
		}
	}
	for k := range castErrors {
		if !keep[k] {
			delete(castErrors, k)
		}
	}
	for k := range castActions {
		if !keep[k] {
			delete(castActions, k)
		}
	}
	for k := range castObserved {
		if !keep[k] {
			delete(castObserved, k)
		}
	}
	for k := range castLearnPending {
		if !keep[k] {
			delete(castLearnPending, k)
		}
	}
	for k := range castURLs {
		if !keep[k] {
			delete(castURLs, k)
		}
	}
	castStatesMu.Unlock()
}

// normalizeConfig fills in defaults and clamps values so that what we store,
// serve from GET /api/config, and actually act on are the same thing. Without
// the clamp the UI could display check_interval: 2 while the monitor silently
// ran at 10.
func normalizeConfig(c *Config) {
	if c.Devices == nil {
		c.Devices = []DeviceConfig{}
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 60
	}
	if c.CheckInterval < minCheckInterval {
		c.CheckInterval = minCheckInterval
	}
	if c.CheckInterval > maxCheckInterval {
		c.CheckInterval = maxCheckInterval
	}
	c.DefaultURL = strings.TrimSpace(c.DefaultURL)
	// Device names must match what the Chromecast advertises exactly, so a
	// trailing space from a copy-paste silently breaks every catt call.
	for i := range c.Devices {
		c.Devices[i].Name = strings.TrimSpace(c.Devices[i].Name)
		c.Devices[i].Host = strings.TrimSpace(c.Devices[i].Host)
		c.Devices[i].URL = strings.TrimSpace(c.Devices[i].URL)
	}
}

func loadConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	fallback := Config{}
	normalizeConfig(&fallback)

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("config read error: %v", err)
		}
		cfg = fallback
		return
	}
	// Unmarshal into a scratch value so a malformed file cannot leave cfg
	// half-populated with a mix of defaults and file contents.
	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("config parse error: %v", err)
		cfg = fallback
		return
	}
	normalizeConfig(&loaded)
	cfg = loaded
}

// saveConfig writes c to cfgPath atomically. It takes the config by value
// rather than reading the global: the caller persists first and only then
// publishes, so a write that fails cannot leave the monitor acting on settings
// the disk never received.
func saveConfig(c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file and rename so a crash mid-write cannot truncate
	// the existing config. The temp name must be unique: two concurrent POSTs
	// to /api/config sharing one fixed path would interleave their writes and
	// rename a half-and-half file into place.
	f, err := os.CreateTemp(filepath.Dir(cfgPath), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Flush to disk before the rename, or a crash right after it leaves a
	// zero-length config that loadConfig will discard on next start.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the config is read by the container user.
	if err := os.Chmod(tmp, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		return err
	}
	tmp = "" // renamed away; nothing left to clean up
	// Sync the directory too. f.Sync() above only makes the *contents* durable —
	// the rename itself is a directory update, and until that is flushed a host
	// crash can leave the config back at its previous version (or, on the first
	// ever save, absent) with the caller having already been told the write
	// succeeded and the monitor already acting on the new settings.
	//
	// Best-effort: some filesystems reject fsync on a directory, and failing the
	// whole save there would reject a config already safely renamed into place.
	if dir, err := os.Open(filepath.Dir(cfgPath)); err == nil {
		if err := dir.Sync(); err != nil {
			log.Printf("config dir sync: %v", err)
		}
		dir.Close()
	}
	return nil
}

// cattDeviceArgs returns the catt flags to target a specific device.
// catt's -d accepts either a friendly name or an IP; passing the IP when we
// have one bypasses mDNS resolution.
func cattDeviceArgs(dev DeviceConfig) []string {
	if dev.Host != "" {
		return []string{"-d", dev.Host}
	}
	return []string{"-d", dev.Name}
}

// cattCmd builds the process runCatt runs. A variable for the same reason
// chromecastSetupPort is one: everything runCatt itself decides — that a killed
// subprocess is reported as a named timeout rather than as "signal: killed", that
// both streams reach the caller through one bounded buffer — and the argv and
// timeout budget each of the three catt calls below is given were otherwise only
// reachable by running catt, which no test here may do. A stand-in process is
// enough to exercise all of it, and a typo in a subcommand name is invisible
// until a device fails to cast.
//
// It takes the context so the substitute keeps the kill-on-deadline behaviour
// that the timeout naming is about.
var cattCmd = func(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "catt", args...)
}

// runCatt runs catt with a timeout. The parent ctx lets a caller abandon the
// subprocess early — an HTTP handler whose client has disconnected, say —
// instead of leaving it to run out its full timeout.
func runCatt(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := cattCmd(ctx, args...)
	// Bound the wait that follows the kill. CommandContext SIGKILLs catt when the
	// timeout fires, but Wait then also blocks until the output pipes close, and a
	// process that inherited them and outlived the kill holds this call open with
	// no deadline of its own — wedging the monitor loop, or leaking a status
	// request's goroutine, permanently.
	cmd.WaitDelay = 5 * time.Second
	// Bound what we buffer, for the same reason getPychromecastStatus does:
	// CombinedOutput grows without limit, and catt is the *likelier* of the two
	// subprocesses to produce a Python traceback — pychromecast and zeroconf both
	// log through it, and a scan that goes wrong can keep printing for the whole
	// 30s budget. Only the first few hundred bytes are ever looked at (shortText
	// caps the message at 400 runes) and the opening lines are the useful part.
	//
	// One writer for both streams, as CombinedOutput does: os/exec special-cases
	// Stdout == Stderr onto a single pipe, so there is no concurrent write to the
	// buffer and the interleaving is preserved.
	buf := &limitedBuffer{max: maxSubprocessOutput}
	cmd.Stdout, cmd.Stderr = buf, buf
	err := cmd.Run()
	out := buf.String()
	// Name the timeout. A killed subprocess usually prints nothing, so cattFailure
	// fell back to the exec error and the device card read "signal: killed" — true,
	// but it does not tell anyone that catt simply took too long. Only the error is
	// replaced: whatever catt did manage to print still wins, being the more
	// specific of the two.
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("catt timed out after %s", timeout)
	}
	return out, err
}

// cattFailure builds a human-readable reason from a failed catt invocation.
// catt exits non-zero with nothing on the pipe for some failures (and a
// subprocess killed by our timeout prints nothing at all), so falling back to
// the exec error keeps us from reporting an empty explanation.
func cattFailure(err error, out string) string {
	if msg := shortText(out); msg != "" {
		return msg
	}
	return shortText(err.Error())
}

// castableURL reports whether u is something catt's cast_site can be given.
// Pure, so it is testable without catt.
func castableURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	// Host must be present: "http:/dashboard" parses fine and is not castable.
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// effectiveURL is the URL auto-cast would use for dev: its own, or the global
// default when it has none. Single definition so monitorDevices and the warning
// the UI shows cannot disagree about which one applies.
func effectiveURL(dev DeviceConfig, defaultURL string) string {
	if dev.URL != "" {
		return dev.URL
	}
	return defaultURL
}

// duplicateDeviceKeys reports which deviceKeys appear on more than one config
// row. Two rows can legitimately be typed for one device — the same IP entered
// twice, or an IP added to a row that another row already carries — and they
// then share a single deviceKey, and with it a single cast state, cast error
// and recorded URL. See monitorDevices for what that costs.
func duplicateDeviceKeys(devices []DeviceConfig) map[string]bool {
	count := make(map[string]int, len(devices))
	for _, d := range devices {
		count[deviceKey(d)]++
	}
	dup := map[string]bool{}
	for k, n := range count {
		if n > 1 {
			dup[k] = true
		}
	}
	return dup
}

// The duplicate-entry advisory, in the two forms configWarning emits it in.
//
// Which one a row gets turns on whether auto-cast is on, because the second
// clause is the actionable half and it is only true when something is casting:
// telling a row with auto-cast *off* that only one of them is cast states a
// consequence that cannot apply to it, and reads as though enabling the checkbox
// were pointless. What is left in that case is the cost that applies either way —
// both rows show the same state and the same cast error, so a failure on one is
// rendered on both.
//
// "the first with auto-cast enabled", not "the first": autoCastTargets skips a
// row with the box unticked *before* it claims the deviceKey, so with the box
// ticked on the second row of a pair only and not the first, the second row is
// the one being cast — and it is the row this advisory is rendered on. Naming the
// first row there told the reader that the row in front of them was inert when it
// was in fact the only one doing anything.
const (
	duplicateEntryWarning       = "Another entry points at the same device — they share one cast state, and only the first with auto-cast enabled is cast."
	duplicateEntryWarningNoAuto = "Another entry points at the same device — they share one cast state, so both rows show the same status."
)

// configWarning reports an advisory problem with how a device is configured.
// url is the device's effective cast URL: "nothing to cast" is a property of the
// device and the global default together, not of the device alone. duplicate
// says another config row resolves to the same deviceKey, which is likewise a
// property of the list rather than of the device.
// Pure and free of any subprocess, so it can be tested without catt.
func configWarning(dev DeviceConfig, url string, duplicate bool) string {
	if dev.Name == "" && dev.Host == "" {
		return "" // getLiveStatus already explains this one; do not pile on
	}
	if !dev.AutoCast {
		// Reported even with nothing monitoring the device, because sharing a
		// deviceKey costs something either way: both rows report the same state
		// and the same cast error, so a failure on one is rendered on both.
		if duplicate {
			return duplicateEntryWarningNoAuto
		}
		return "" // nothing is monitoring it, so there is nothing to warn about
	}
	// monitorDevices skips both of the next two cases, and a skip is invisible:
	// from the UI it is indistinguishable from auto-cast simply not working.
	//
	// Reported *ahead* of the duplicate advisory below, unlike for a device with
	// auto-cast off: these two stop the device being cast at all, whereas the
	// duplicate note only says which row does the casting. autoCastTargets claims
	// the deviceKey for the first matching row before it looks at that row's URL,
	// so a duplicated pair whose first row has the unusable URL is never cast —
	// and leading with the duplicate advisory told the user the first row was
	// working while hiding the one problem they could actually fix.
	if url == "" {
		return "No URL for this device and no default URL — auto-cast has nothing to cast."
	}
	if !castableURL(url) {
		return "URL is not an absolute http:// or https:// address — auto-cast cannot use it."
	}
	if duplicate {
		return duplicateEntryWarning
	}
	// An auto-cast device with no IP cannot be monitored at all. `catt status`
	// reports the *media* session, and a web page cast has none, so its output is
	// byte-identical for "showing our dashboard" and "sitting idle" — the monitor
	// has nothing to poll, never notices the cast being dropped, and quietly
	// stops re-casting the device until the process restarts. Inferring idleness
	// from that output is not an option: guessing "idle" would re-cast every tick
	// and restart the dashboard forever. An IP switches the device to the
	// pychromecast helper, which does report the app id.
	if dev.Host == "" {
		return "No IP set — auto-cast cannot tell if this device drops the cast. Scan and use 'Set IP'."
	}
	return ""
}

// mergeStatusAdvisories folds onto a live status the two things the device
// itself cannot tell us: why our last cast or stop failed, and a standing
// problem with how it is configured.
//
// lastCastErr fills in only when the device has nothing to say, so that the live
// result — the newer and more specific of the two — wins. Without the fallback a
// cast that failed was indistinguishable from one that never happened: the card
// just read "Idle". Warning is a separate field rather than more text in Error
// for the opposite reason: a standing advisory recorded there would still be
// there next tick, masking every real failure that followed it.
//
// Pure, so that rule is testable without a subprocess.
func mergeStatusAdvisories(ds DeviceStatus, lastCastErr, warning string) DeviceStatus {
	if ds.Error == "" {
		ds.Error = lastCastErr
	}
	ds.Warning = warning
	return ds
}

func getDeviceStatus(ctx context.Context, dev DeviceConfig, defaultURL string, duplicate bool) DeviceStatus {
	return mergeStatusAdvisories(
		getLiveStatus(ctx, dev),
		castError(dev),
		configWarning(dev, effectiveURL(dev, defaultURL), duplicate),
	)
}

func getLiveStatus(ctx context.Context, dev DeviceConfig) DeviceStatus {
	// A row with neither identifier is what "+ Add manually" starts as. Shelling
	// out anyway ran `catt -d '' status` for the full 10s on every poll, and every
	// such row shares the deviceKey "name:", so their cast errors landed on each
	// other. Say what is missing instead.
	if dev.Name == "" && dev.Host == "" {
		// Both are empty by definition here, but set them anyway: the UI pairs a
		// status with its device by index and then checks the identity, so every row
		// it renders has to carry the fields.
		return DeviceStatus{Name: dev.Name, Host: dev.Host, State: "unknown", Error: "device has no name or IP address"}
	}
	if dev.Host != "" {
		return probeDevice(ctx, dev)
	}
	// Fall back to catt when no host IP is configured.
	out, err := cattStatus(ctx, dev)
	ds := DeviceStatus{Name: dev.Name, Host: dev.Host, State: "unknown"}
	if err != nil {
		ds.Error = cattFailure(err, out)
		return ds
	}
	ds.State = cattStatusState(out, isCasting(dev))
	return ds
}

// cattStatusState reads a device state out of `catt status` output, falling back
// to casting — our own cached cast state — when the output says nothing about it,
// which for a web-page cast is always (see the discussion in CLAUDE.md).
//
// Only "State: " is worth looking for. catt's status output has exactly six
// labels — Title, Time, Remaining time, State, Volume, Volume muted — so the
// "Content: " line this used to also parse never existed, and the URL it filled
// in was never set by anything or read by anyone.
//
// That line is narrow but real: catt includes player_state only when the media
// session reports a non-image content_type, which our own cast_site never does
// (hence the cached fallback), but a media app can. When it is there it is the
// device's own truth — PAUSED, BUFFERING — so it wins over our guess.
//
// Pure, so the parse is testable without catt.
func cattStatusState(out string, casting bool) string {
	state := "Idle"
	if casting {
		state = "Playing"
	}
	for _, line := range splitLines(out) {
		if after, ok := strings.CutPrefix(line, "State: "); ok {
			// Bounded: this comes off a 64KB buffer, and a line with no newline in
			// it is all one "value". Ignored when empty — catt printing a bare
			// "State: " would otherwise blank the card's only state text, which
			// reads as a UI fault rather than as the device saying nothing.
			if s := shortText(after); s != "" {
				state = s
			}
		}
	}
	return state
}

// splitLines splits subprocess output into lines.
//
// Deliberately not bufio.Scanner: its default token limit is 64KB, which is
// exactly maxSubprocessOutput, so output that arrives as one long line — a
// single-line Python traceback from a broken pychromecast install is the
// realistic case — made the very first Scan fail with ErrTooLong and return no
// lines at all. Both callers ignore the scanner error, so the symptom was a
// status stuck at "unknown" and a mDNS scan that found nothing, with the reason
// discarded. The output is already a bounded string in memory, so a scanner
// bought nothing to begin with.
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	// Match bufio.ScanLines and drop a trailing CR, so a \r\n stream does not
	// leave every parsed value with an invisible character stuck to the end.
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// statusQueryTimeout is the outermost layer of the budget documented in
// cc_status.py: it must stay above that script's own 12s watchdog, so the script
// gets to explain itself instead of being killed with nothing on the pipe.
const statusQueryTimeout = 15 * time.Second

// maxSubprocessOutput bounds what we buffer from a subprocess — the status
// helper here, and catt in runCatt. Their streams are read into memory, one set
// per configured device per poll, and nothing guarantees either stays quiet: a
// zeroconf or pychromecast logger stuck in a retry loop can write for the whole
// budget above. Generous enough for the JSON payload and the traceback it
// carries in "detail", which is all we would ever want to look at.
const maxSubprocessOutput = 64 << 10

// limitedBuffer collects at most max bytes and silently discards the rest,
// keeping the *first* ones: the helper writes its single JSON object before
// anything else can follow it, and a traceback's opening lines are the useful
// part of it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	// Always claim the whole write, even the part that was dropped. A short count
	// makes the copier report io.ErrShortWrite as the command's error, which would
	// replace the real result with a plumbing detail.
	total := len(p)
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return total, nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }

// statusPayload is the single JSON object cc_status.py writes to stdout.
type statusPayload struct {
	AppID       string `json:"app_id"`
	DisplayName string `json:"display_name"`
	IsIdle      bool   `json:"is_idle"`
	Error       string `json:"error"`
	// Detail is the helper's traceback, sent alongside Error whenever the failure
	// was an unexpected exception rather than a diagnosis the helper recognised.
	// It goes to the log rather than into the DeviceStatus: it is the only thing
	// that identifies a broken pychromecast install, and the helper caps it at
	// 4000 characters precisely so it survives to be read — but it is far too long
	// for a device card, and Error already carries the one-line summary of it.
	// Decoded and discarded, it was produced at that cost and never read at all.
	Detail string `json:"detail"`
}

func getPychromecastStatus(ctx context.Context, dev DeviceConfig) DeviceStatus {
	ctx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()
	// Keep stderr out of stdout: zeroconf/pychromecast log lines and Python
	// warnings land on stderr, and mixing them into stdout makes the JSON
	// unparseable even when the query itself succeeded.
	cmd := exec.CommandContext(ctx, "python3", statusScript, dev.Host)
	stdout := &limitedBuffer{max: maxSubprocessOutput}
	stderr := &limitedBuffer{max: maxSubprocessOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Bound the post-kill wait, as runCatt does: without it Wait blocks until the
	// output pipes close, which a process that inherited them and survived the
	// kill need never do — and this call has no deadline left to save it.
	cmd.WaitDelay = 5 * time.Second
	// Stamp before running: what this reports is true as of now, not as of
	// whenever the subprocess happens to finish up to statusQueryTimeout later.
	observedAt := time.Now()
	runErr := cmd.Run()
	return interpretStatusOutput(dev, stdout.Bytes(), stderr.Bytes(), runErr, ctx.Err(), observedAt)
}

// interpretStatusOutput turns one run of the status helper into a DeviceStatus,
// and applies what it learned to the cast state. Separated from the subprocess
// plumbing above so that every branch of it — the diagnostic precedence, the
// stale-poll fallback, the app-id learner, the bounding of device-supplied text
// — is assertable without running python3, which no test here may do.
//
// ctxErr is the query context's error, checked rather than derived so that a
// timeout can be told from the caller giving up. observedAt is when the query
// *began*; see observeCastState.
func interpretStatusOutput(dev DeviceConfig, stdout, stderr []byte, runErr, ctxErr error, observedAt time.Time) DeviceStatus {
	ds := DeviceStatus{Name: dev.Name, Host: dev.Host, State: "unknown"}

	// Decoded into a *pointer* so that a bare `null` on stdout is distinguishable
	// from an object, the same hazard POST /api/config guards against. Into a
	// value it decodes without error and leaves the zero payload, whose empty
	// Error and false IsIdle read as "the device is playing something" — a
	// fabricated state, applied to castStates and reported on the card, from a
	// helper that in fact told us nothing.
	var result *statusPayload
	if err := json.Unmarshal(stdout, &result); err != nil || result == nil {
		// Report the most useful diagnostic available. Previously a missing
		// python3 or an empty stdout produced an empty Error, leaving the UI
		// with no explanation at all.
		errMsg, outMsg := shortText(string(stderr)), shortText(string(stdout))
		switch {
		// Our own deadline is checked first, ahead of stderr. The script's 12s
		// watchdog fires before this one, so reaching it means the script never got
		// to speak — and stderr then holds nothing but zeroconf chatter, which was
		// being quoted onto the device card as if it were the diagnosis.
		case ctxErr == context.DeadlineExceeded:
			ds.Error = fmt.Sprintf("status query for %s timed out after %s", dev.Host, statusQueryTimeout)
		case ctxErr != nil:
			// Cancelled, not timed out — the caller (a browser that navigated away)
			// gave up on us. Nothing was learned about the device either way.
			ds.Error = "status query cancelled"
		case errMsg != "":
			ds.Error = errMsg
		case outMsg != "":
			ds.Error = outMsg
		case runErr != nil:
			ds.Error = shortText(runErr.Error())
		default:
			ds.Error = "no status output from " + statusScript
		}
		return ds
	}
	if result.Error != "" {
		ds.Error = shortText(result.Error)
		// The traceback, if there is one, goes to the log — see statusPayload.Detail.
		if detail := boundText(result.Detail, maxLoggedDetailLen); detail != "" {
			log.Printf("status helper failed for %q: %s\n%s", deviceLabel(dev), ds.Error, detail)
		}
		return ds
	}

	// A device reported as playing has to name the app that is playing. The app id
	// is the whole means of telling our own dashboard from somebody else's, and the
	// helper's own idle rule already reports a device with no app id as idle (see
	// device_is_idle in cc_status.py) — so a payload claiming otherwise describes a
	// state we cannot act on, and both ways of acting on it are wrong: recorded as
	// playing it makes the monitor skip the device for the life of the process,
	// recorded as idle it re-casts over whatever is on screen on every tick.
	// Refuse it and say so, exactly as a null payload is refused above.
	//
	// Not merely defensive against drift, though it is that too: the helper decides
	// idleness on the *raw* app id and then strips it, so a device reporting an app
	// id of nothing but whitespace arrives here as "playing" with the field empty.
	if !result.IsIdle && result.AppID == "" {
		ds.Error = "status helper reported a playing device with no app id"
		return ds
	}

	playing := !result.IsIdle
	// Only an app that is actually running can be ours. An idle device may still
	// name one — Backdrop counts as idle — and offering it to the learner would
	// teach the screensaver as our dashboard.
	appID := ""
	if playing {
		appID = result.AppID
	}
	// Observe before reporting, so a poll that has just taught us the app id does
	// not then turn round and report that same app as somebody else's.
	if !observeCastState(dev, playing, appID, observedAt) {
		// This probe began before a cast or stop of ours landed, so everything it
		// describes — the state, the app name, the app id — is the device as it
		// was beforehand; observeCastState dropped it for exactly that reason.
		// Report the state we recorded instead, which is the newer of the two.
		// Passing the stale view through made a device we had just cast to read
		// "Idle" (or carry the previous app's name) on the card, and calling its
		// app foreign made the monitor "take back" a device already showing our
		// dashboard — a second, wasted cast.
		if isCasting(dev) {
			ds.State = "Playing"
		} else {
			ds.State = "Idle"
		}
		return ds
	}
	if !playing {
		ds.State = "Idle"
		return ds
	}
	// Bounded here and not only in the helper: State is the receiver app's own
	// display_name, so its length is decided by whoever wrote that app, and it is
	// echoed in every status response for as long as the app is up.
	ds.State = shortText(result.DisplayName)
	if ds.State == "" {
		ds.State = "Playing"
	}
	ds.Foreign = isForeignApp(result.AppID)
	return ds
}

// autoCastTargets narrows a device list to the rows monitorDevices will act on
// this tick, keeping config order. It is every skip the monitor makes *before*
// touching the network, gathered into one pure function: each of them is
// invisible from the UI — a skipped device's card reads a plain "Idle", exactly
// like one auto-cast is happily managing — so the set has to be both testable
// without a subprocess and kept in step with what configWarning explains.
func autoCastTargets(devices []DeviceConfig, defaultURL string) []DeviceConfig {
	// Two config rows can resolve to one deviceKey — the same IP typed twice, or
	// an IP filled into a row that another row already carries. All the state in
	// castStates is keyed by that one key, castURLs included, so with a different
	// URL on each row the tick cast row one's page, judged it stale against row
	// two's, cast that, and did the same again next tick: an always-on dashboard
	// restarting itself forever. Act on the first such row only, and always the
	// same one, so the choice does not depend on which URL was edited last.
	handled := make(map[string]bool, len(devices))

	var targets []DeviceConfig
	for _, dev := range devices {
		if !dev.AutoCast {
			continue
		}
		if dev.Name == "" && dev.Host == "" {
			continue // nothing to target; see getLiveStatus
		}
		key := deviceKey(dev)
		if handled[key] {
			continue
		}
		handled[key] = true
		// Both of the next two skips are reported to the UI by configWarning rather
		// than through castErrors: they are standing configuration problems, so
		// recording them as a fresh cast failure on every tick would keep bumping
		// castActions and suppress every status observation of the device for good.
		//
		// The second is not cosmetic. catt reads a "-"-prefixed positional as a
		// flag, and one it accepts makes it exit 0 without casting — which we would
		// record as a success and then let the app-id learner adopt whatever is
		// really running on the device as our own, fleet-wide.
		url := effectiveURL(dev, defaultURL)
		if url == "" || !castableURL(url) {
			continue
		}
		targets = append(targets, dev)
	}
	return targets
}

// The subprocess calls the casting and discovery logic makes, behind function
// variables.
//
// monitorDevices is the only code that both probes a device and decides whether
// to cast to it, so none of its ordering — the stale-URL re-cast, the takeover
// gate, the skip on an unreachable device — could be exercised at all while it
// shelled out directly; the same went for the goroutines in /api/devices/cast and
// /stop, which are where a cast failure becomes a castErrors entry. mdnsScan is
// the same story for handleScan: an mDNS hit ends the stream early, and that
// whole branch — the found events, the count, the TCP fallback it skips — was
// unreachable in a test that may not run catt. Plain variables rather than an
// interface: there is exactly one production implementation of each, and only
// the tests ever substitute one.
var (
	probeDevice = getPychromecastStatus
	mdnsScan    = cattScan
	// tcpScanner is seamed alongside mdnsScan so that handleScan's own decisions
	// are assertable: which scanner runs for an explicit subnet, that the subnet
	// reaches it, and that a give-up reason comes back out on the done event. The
	// real one probes 254 hosts of whatever localSubnets finds, so a test that
	// drove the no-subnet path through it would scan the LAN of the machine
	// running it. tcpScan itself is exercised directly, against loopback.
	tcpScanner = tcpScan
	// cattStatus is the status query for a device with no IP, and the last
	// subprocess left in getLiveStatus. Seamed for the same reason as the rest:
	// both halves of that branch's contract — that a failure is explained rather
	// than reported as an empty error, and that the parse falls back to our own
	// cached cast state because catt says nothing about a web-page cast — were
	// unassertable in a test that may not run catt, so getting either wrong looked
	// exactly like a device that had simply gone quiet.
	cattStatus = func(ctx context.Context, dev DeviceConfig) (string, error) {
		return runCatt(ctx, 10*time.Second, append(cattDeviceArgs(dev), "status")...)
	}
	castSite = func(ctx context.Context, dev DeviceConfig, url string) (string, error) {
		return runCatt(ctx, 30*time.Second, append(cattDeviceArgs(dev), "cast_site", url)...)
	}
	stopCast = func(ctx context.Context, dev DeviceConfig) (string, error) {
		return runCatt(ctx, 15*time.Second, append(cattDeviceArgs(dev), "stop")...)
	}
)

func monitorDevices(ctx context.Context) {
	cfgMu.RLock()
	devices := append([]DeviceConfig{}, cfg.Devices...)
	defaultURL := cfg.DefaultURL
	cfgMu.RUnlock()

	for _, dev := range autoCastTargets(devices, defaultURL) {
		url := effectiveURL(dev, defaultURL)
		// Poll the device itself when we can. Relying on the cached cast state
		// alone means a device that drops the cast (reboot, someone else casts
		// to it) is never recovered, because nothing clears the flag unless a
		// browser happens to be polling /api/devices/status.
		// foreign stays false for a device with no host IP: the catt-only status
		// path cannot report an app id, so we can never tell, and the safe answer
		// is to behave exactly as before.
		foreign := false
		if dev.Host != "" {
			// A probe error means the device is off or unreachable, so a cast
			// cannot succeed. Skipping it matters: catt would spend its full 30s
			// timeout failing, serially, and a couple of powered-off TVs were
			// enough to keep the loop permanently busy and starve the devices
			// that were actually up.
			st := probeDevice(ctx, dev)
			if st.Error != "" {
				log.Printf("skipping auto-cast to %q: %s", deviceLabel(dev), st.Error)
				continue
			}
			foreign = st.Foreign
			if foreign && !dev.Takeover {
				// Somebody is watching something. Leave it alone — enable
				// takeover on the device to reclaim it instead.
				continue
			}
		}
		// A saved URL change has to reach a device that is already up. The skip
		// below is what kept it from doing so: an always-on dashboard never drops
		// the cast by itself, so editing default_url or a device's own URL applied
		// only after a restart of this service — from the UI, the save simply did
		// nothing. Only a URL we recorded ourselves counts (see castURLs); with no
		// entry we did not put the current page there and must not guess.
		stale := false
		if prev, ok := lastCastURL(dev); ok && prev != url {
			stale = true
		}
		// A cast of ours that failed has to be retried, and the skip below is what
		// stopped it being: a failed cast usually leaves whatever was on screen
		// beforehand still on screen, so the next probe reports the device as
		// playing — and setCastError has already dropped the castURLs entry that
		// would have made it read as stale. The device then matched "playing, and
		// nothing says it is stale" on every subsequent tick and was never touched
		// again: an always-on dashboard left on a page we failed to replace, with
		// the reason wiped by that same observation — which used to clear the error
		// whenever it found the device playing — and a card reading a clean
		// "Playing".
		//
		// "Playing something" is not evidence that our cast worked. Only an action
		// of ours succeeding is, and that is exactly what a castErrors entry says
		// did not happen. A retry per tick is the same cadence an idle device
		// already gets, and a success clears the entry.
		retry := castError(dev) != ""
		// A foreign app sets the cast state too (isCasting means "playing
		// something", not "playing ours"), so it must not short-circuit a
		// takeover that the check above has already approved.
		if isCasting(dev) && !foreign {
			switch {
			case stale:
				log.Printf("re-casting %q: URL changed", deviceLabel(dev))
			case retry:
				log.Printf("re-casting %q: the last cast failed", deviceLabel(dev))
			default:
				continue
			}
		}
		if foreign {
			log.Printf("taking %q back from another app", deviceLabel(dev))
		}
		log.Printf("auto-casting to %q: %s", deviceLabel(dev), url)
		out, err := castSite(ctx, dev, url)
		if err != nil {
			log.Printf("cast error for %q: %v — %s", deviceLabel(dev), err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, true, url)
		}
	}
}

// checkInterval reads the monitor interval as a Duration.
func checkInterval() time.Duration {
	cfgMu.RLock()
	interval := cfg.CheckInterval
	cfgMu.RUnlock()
	// Defensive clamp: normalizeConfig already guarantees both bounds, but
	// this is where the value becomes a Duration, and both ends of the range
	// produce a hot loop that hammers every device with status queries — too
	// small directly, too large by overflowing the multiplication below into a
	// negative duration that a sleep does not wait on at all.
	if interval < minCheckInterval {
		interval = minCheckInterval
	}
	if interval > maxCheckInterval {
		interval = maxCheckInterval
	}
	return time.Duration(interval) * time.Second
}

// remainingWait is how much of the current check interval monitorLoop still has
// to wait out, for a cycle whose wait began at waitFrom.
//
// It re-reads the interval on every call instead of sleeping on the value read
// once at the top of the cycle. The ceiling is a day, so lowering the interval
// from anywhere near it took effect only after the *old* interval had elapsed —
// up to 24h during which the save looked like it had done nothing at all.
//
// Measured from waitFrom rather than from the wake-up, so a burst of saves can
// only shorten the wait towards zero and can never postpone the next poll.
//
// Pure enough to test: no sleeping, so the 24h-to-10s case is a direct assertion.
func remainingWait(waitFrom time.Time) time.Duration {
	return time.Until(waitFrom.Add(checkInterval()))
}

// awaitNextTick blocks until the check interval that began at waitFrom has
// elapsed, re-reading it every time POST /api/config wakes us through
// configChanged. Separate from monitorLoop, which cannot be called from a test
// at all: it never returns, and the interruptibility is the part with a
// regression behind it — a save that lowered the interval from near the one-day
// ceiling otherwise took effect up to 24h later and looked like a no-op.
func awaitNextTick(waitFrom time.Time) {
	for {
		d := remainingWait(waitFrom)
		if d <= 0 {
			return
		}
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-configChanged:
			t.Stop()
		}
	}
}

func monitorLoop() {
	for {
		monitorDevices(context.Background())
		awaitNextTick(time.Now())
	}
}

// rejectBody answers a request whose body could not be decoded. A body stopped
// by http.MaxBytesReader surfaces as an ordinary decode error, so every one of
// these endpoints reported an over-long body as "400 — your JSON is malformed",
// which sends the caller looking for a syntax error that is not there. Name the
// limit and use the status that means it.
func rejectBody(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, fmt.Sprintf("request body must be at most %d bytes", tooLarge.Limit), http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// decodeOneObject decodes exactly one JSON value from body into v and refuses
// anything that follows it. Decode stops at the end of the first value and
// ignores the rest, so a body that was concatenated or double-encoded — two
// requests joined by a retrying client, a proxy appending to the stream — had
// its first half acted on and the second silently dropped, with a 200 returned
// for the whole thing. Every endpoint here replaces or acts on the *whole*
// payload, so a partial one must be refused rather than half-applied.
func decodeOneObject(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected content after the JSON object")
	}
	return nil
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		cfgMu.RLock()
		snapshot := cfg
		snapshot.Devices = append([]DeviceConfig{}, cfg.Devices...)
		cfgMu.RUnlock()
		// Encode outside the lock: a slow client would otherwise block every
		// config writer for as long as it takes to drain the response.
		json.NewEncoder(w).Encode(snapshot)
	case http.MethodPost:
		// Bound the body — this endpoint is unauthenticated on the LAN, and
		// json.Decode on an unbounded stream will happily consume all memory.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		// Decode into a *pointer* so a body of `null` is distinguishable from an
		// object. Into a Config it decodes without error and leaves the zero
		// value, which normalizeConfig then turns into a perfectly valid "no
		// devices, no default URL, 60s" config — so a client that sent `null`
		// (a serialiser given an absent value, a proxy rewriting the body) had
		// every device deleted from disk and every cast state pruned, and got a
		// 200 for it. This endpoint replaces the whole config, so it must not
		// treat "no value" as "the empty value".
		//
		// Anything after that object is refused too (see decodeOneObject): the half
		// that would be dropped here is a device list.
		var decoded *Config
		if err := decodeOneObject(r.Body, &decoded); err != nil {
			rejectBody(w, err)
			return
		}
		if decoded == nil {
			http.Error(w, "config body must be a JSON object", http.StatusBadRequest)
			return
		}
		newCfg := *decoded
		normalizeConfig(&newCfg)
		// Persist before publishing, and serialise the whole write-then-publish
		// so it stays atomic against a second POST. Applying first meant a failed
		// write left the monitor acting on a config the disk never got — the user
		// saw a 500, and a restart silently reverted behaviour to the old file.
		// Two concurrent POSTs could likewise rename in the opposite order to the
		// one they published in, leaving memory and disk permanently disagreeing.
		cfgSaveMu.Lock()
		err := saveConfig(newCfg)
		if err == nil {
			cfgMu.Lock()
			cfg = newCfg
			cfgMu.Unlock()
			// Devices can be renamed, re-addressed or deleted here; drop the cast
			// state of anything that no longer exists. After the save, so a rejected
			// write does not discard the state of devices that are still configured
			// — and inside cfgSaveMu, or a slower concurrent POST could prune
			// against its own older device list and delete the state of devices the
			// published config still has, re-casting them for no reason.
			pruneCastStates(newCfg.Devices)
		}
		cfgSaveMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// A changed interval only takes effect if the monitor stops waiting out
		// the old one; see monitorLoop.
		select {
		case configChanged <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func cattScan(ctx context.Context) []DiscoveredDevice {
	out, err := runCatt(ctx, 30*time.Second, "scan")
	if err != nil {
		log.Printf("catt scan: %v — %s", err, strings.TrimSpace(out))
	}
	return parseCattScan(out)
}

func parseCattScan(out string) []DiscoveredDevice {
	var devices []DiscoveredDevice
	var cur DiscoveredDevice
	// One entry per host. mDNS answers arrive per interface, so a host that can
	// see the LAN two ways (network_mode: host on a machine with wifi and
	// ethernet up) gets listed twice — and the UI's device list is keyed by host,
	// where a duplicate key makes Alpine throw and drop the whole list of
	// discovered devices rather than just the repeat.
	seen := map[string]bool{}
	add := func(d DiscoveredDevice) {
		if seen[d.Host] {
			return
		}
		seen[d.Host] = true
		devices = append(devices, d)
	}
	for _, raw := range splitLines(out) {
		line := strings.TrimSpace(raw)
		// Emit on whichever of the pair completes the device. Keying the append
		// off "Host:" alone assumed Name always came first; the reverse order
		// left the pair sitting in cur and the device was never reported.
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			cur.Name = strings.TrimSpace(after)
			if cur.Name != "" && cur.Host != "" {
				add(cur)
				cur = DiscoveredDevice{}
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "Host:"); ok {
			cur.Host = strings.TrimSpace(after)
			if cur.Name != "" && cur.Host != "" {
				add(cur)
				cur = DiscoveredDevice{}
			}
			continue
		}
		// catt actually prints one device per line as
		//   "<ip> - <friendly name> - <manufacturer> <model>"
		// so the labelled form above never matched and mDNS discovery always
		// came back empty, silently falling through to the TCP scan.
		host, rest, ok := strings.Cut(line, " - ")
		if !ok || net.ParseIP(strings.TrimSpace(host)) == nil {
			continue
		}
		name := rest
		// Split at the *last* separator, not the first: a friendly name may
		// itself contain " - " ("Kitchen - Nest Hub"), and cutting at the first
		// one truncated it to "Kitchen". catt is then asked to cast to a device
		// that does not exist under that name.
		if i := strings.LastIndex(rest, " - "); i != -1 {
			name = rest[:i] // trailing " - <manufacturer> <model>"
		}
		if name = strings.TrimSpace(name); name != "" {
			add(DiscoveredDevice{Name: name, Host: strings.TrimSpace(host)})
		}
	}
	return devices
}

// virtualIfacePrefixes names interfaces that cannot have a Chromecast on them:
// container and VM bridges, VPN tunnels, and virtual ethernet pairs. A host
// running Docker has one bridge per compose network, and including them turned
// an auto-detect scan into ~2800 pointless probes across eleven subnets
// instead of 254 across the one LAN the user actually cares about.
//
// "br-" and "lxcbr"/"lxdbr" rather than a bare "br": a Linux host really can
// have its LAN on br0 (libvirt, Proxmox), and filtering that out would hide the
// only subnet that matters.
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale",
	"tun", "tap", "utun", "zt", "wg",
	"vboxnet", "vmnet", "lxcbr", "lxdbr", "podman", "cali",
}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// localSubnets returns the /24 base (e.g. "192.168.1") for each usable private
// IPv4 interface, preferring physical ones.
func localSubnets() []string {
	physical := collectSubnets(true)
	// Fall back to the unfiltered set rather than returning nothing: the prefix
	// list is a heuristic, and on an unusual host it could filter out the only
	// interface there is.
	//
	// Only the virtual-interface guess is relaxed here. The private-address check
	// in collectSubnets is not part of the fallback and must never be — it is a
	// fact about the address, not a guess about a name, and undoing it is exactly
	// what would fire on the hosts that need it most (see below). A host with
	// nothing private on it correctly auto-detects nothing.
	//
	// Only walked when the filtered pass came back empty: enumerating every
	// interface and its addresses twice on every call bought nothing, and
	// /api/subnets is on the page-load path.
	if len(physical) == 0 {
		return collectSubnets(false)
	}
	return physical
}

func collectSubnets(skipVirtual bool) []string {
	ifaces, _ := net.Interfaces()
	var subnets []string
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if skipVirtual && isVirtualIface(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			v4 := net.IP(nil)
			if ip != nil {
				v4 = ip.To4()
			}
			// Skip link-local (169.254/16): an interface that failed DHCP has
			// no peers worth probing.
			if v4 == nil || v4.IsLinkLocalUnicast() {
				continue
			}
			// Auto-detect only ever probes RFC1918 space. A scan opens a TCP
			// connection to all 254 hosts in the /24 and sends an HTTP request to
			// whatever answers on 8008, and on a host carrying a routable address
			// — a VPS, or an ISP that hands out public IPv4 without NAT — that
			// port-scanned 254 machines belonging to strangers, none of which the
			// user asked for by clicking "Scan Network". IsPrivate also excludes
			// CGNAT (100.64/10), where the neighbours are other ISP customers.
			//
			// The virtual-interface filter above does not cover this: it was about
			// probe volume, so a real physical interface with a public address
			// sails straight through it.
			//
			// A subnet typed into the UI is explicit intent and stays honoured —
			// this check is only about what we go probing unasked.
			if !v4.IsPrivate() {
				continue
			}
			parts := strings.Split(v4.String(), ".")
			if len(parts) == 4 {
				// Multiple interfaces / aliases can share a /24. Deduplicate,
				// or the TCP scan probes every host twice and reports a
				// doubled host total.
				base := strings.Join(parts[:3], ".")
				if !seen[base] {
					seen[base] = true
					subnets = append(subnets, base)
				}
			}
		}
	}
	return subnets
}

type ScanEvent struct {
	Type    string            `json:"type"`
	Message string            `json:"message,omitempty"`
	Device  *DiscoveredDevice `json:"device,omitempty"`
	// No omitempty: a progress event with checked == 0 would drop the field
	// entirely, and the UI then renders "undefined / 254 hosts" and a NaN%
	// progress bar width.
	Checked int `json:"checked"`
	Total   int `json:"total"`
	Count   int `json:"count"`
}

// parseSubnet accepts any of: "192.168.1", "192.168.1.0/24", "192.168.1.x",
// "192.168.1.*", or any host address in the subnet, and returns the
// three-octet base ("192.168.1"), or "" if unparseable.
func parseSubnet(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "/"); idx != -1 {
		// Trim again after the cut: "192.168.1.0 / 24" leaves a trailing space,
		// which neither net.ParseIP nor the Atoi loop below accepts, so a subnet
		// typed with spaces around the mask was refused as "Invalid subnet".
		s = strings.TrimSpace(s[:idx]) // strip CIDR mask, leaving bare host address
	}
	if ip := net.ParseIP(s); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			parts := strings.Split(v4.String(), ".")
			return strings.Join(parts[:3], ".")
		}
	}
	parts := strings.Split(s, ".")
	// Accept a wildcard last octet: "192.168.1.x" / "192.168.1.*"
	if len(parts) == 4 && (parts[3] == "x" || parts[3] == "X" || parts[3] == "*") {
		parts = parts[:3]
	}
	// Accept a bare three-octet base like "192.168.1"
	if len(parts) == 3 {
		for _, p := range parts {
			if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 255 {
				return ""
			}
		}
		base := strings.Join(parts, ".")
		// Atoi is more permissive than Go's IP parser: it accepts a leading zero
		// ("192.168.01") and a sign ("192.168.+1"). Those got through as a base
		// that no host address built from it can be dialled, so the scan probed
		// all 254 addresses, failed every one instantly, and reported "No devices
		// found" instead of "Invalid subnet". Make the base prove itself.
		if net.ParseIP(base+".1") == nil {
			return ""
		}
		return base
	}
	return ""
}

func handleSubnets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	bases := localSubnets()
	cidrs := make([]string, len(bases)) // non-nil, so it marshals as [] not null
	for i, b := range bases {
		cidrs[i] = b + ".0/24"
	}
	json.NewEncoder(w).Encode(cidrs)
}

// maxEurekaInfo bounds the /setup/eureka_info body we are willing to read from
// a host that answered on the setup port. A genuine reply is a few KB.
const maxEurekaInfo = 64 << 10

// chromecastSetupPort is the unauthenticated HTTP port every Chromecast serves
// /setup/eureka_info on, and the port the TCP fallback looks for. A variable
// only so a test can point the scan at a local stand-in server: the confirm step
// — dial, fetch, decode, require a name — is what decides whether a responding
// host is reported as a device at all, and it cannot be reached otherwise
// without a real Chromecast on the machine running the tests.
var chromecastSetupPort = 8008

// detectSubnets is the auto-detection tcpScan falls back on when the caller named
// no subnet. A variable for the same reason tcpScanner is one: the real
// implementation returns whatever LAN the machine is on, so a test driving this
// path through it would probe 254 of the tester's neighbours — leaving the give-up
// message, which is the only thing a host with no private address ever sees, and
// the handover of a detected subnet to the probe, both asserted nowhere.
var detectSubnets = localSubnets

// tcpScan probes each host in subnets on the Chromecast setup port.
// Pass nil subnets to auto-detect from local interfaces.
// onStatus receives human-readable status lines (including subnet info).
// onFound is called immediately when a device is confirmed.
// onProgress is called every ~500ms with (checked, total) counts.
//
// The second return value is a reason the scan could not run at all, for the
// caller to put on its terminating event. It deliberately does not go out
// through onStatus: the UI overwrites the status line when the stream ends, so
// a reason reported that way was replaced by the generic "No devices found" and
// never seen — the same trap handleScan's other messages document.
func tcpScan(ctx context.Context, subnets []string, onStatus func(string), onFound func(DiscoveredDevice), onProgress func(int, int)) ([]DiscoveredDevice, string) {
	if len(subnets) == 0 {
		subnets = detectSubnets()
	}
	if len(subnets) == 0 {
		// Name the private-address requirement: auto-detect declining to guess is
		// not the same failure as having no network, and a host with only a public
		// address needs its LAN subnet typed in rather than retried.
		return nil, "No private IPv4 subnet detected — enter a subnet to scan"
	}

	total := len(subnets) * 254
	subnetLabels := make([]string, len(subnets))
	for i, s := range subnets {
		subnetLabels[i] = s + ".0/24"
	}
	onStatus(fmt.Sprintf("Probing %s — %d hosts on port %d", strings.Join(subnetLabels, ", "), total, chromecastSetupPort))

	var (
		checked int64
		devices []DiscoveredDevice
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 50)
		// Its own transport, not the default one: with Transport nil these
		// requests go through http.DefaultTransport, so a scan's connections land
		// in the process-wide idle pool and CloseIdleConnections below reaches
		// into it. Keep-alives off because we make exactly one request per host,
		// so a pooled connection is only ever waste.
		client = &http.Client{
			Timeout:   2 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		}
	)
	// A scan can open a connection to every responding host; without this they
	// sit in the idle pool until their keep-alive expires.
	defer client.CloseIdleConnections()

	// Ticker goroutine for progress events; stopped before tcpScan returns.
	stopTicker := make(chan struct{})
	var tickerWg sync.WaitGroup
	tickerWg.Add(1)
	go func() {
		defer tickerWg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				onProgress(int(atomic.LoadInt64(&checked)), total)
			case <-stopTicker:
				return
			}
		}
	}()

	for _, subnet := range subnets {
		for i := 1; i <= 254; i++ {
			host := fmt.Sprintf("%s.%d", subnet, i)
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				defer atomic.AddInt64(&checked, 1)

				// Bail out cheaply once the client has disconnected, instead of
				// probing the remaining hosts with nobody listening.
				if ctx.Err() != nil {
					return
				}

				addr := net.JoinHostPort(h, strconv.Itoa(chromecastSetupPort))
				conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
				if err != nil {
					return
				}
				conn.Close()

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/setup/eureka_info", nil)
				if err != nil {
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				defer resp.Body.Close()

				var info struct {
					Name string `json:"name"`
				}
				// Bound the body. This decodes whatever answered on port 8008 of an
				// address we picked ourselves, which is very often not a Chromecast,
				// and an unbounded Decode reads until the top-level object closes —
				// on a LAN that is tens of megabytes per host before the 2s client
				// timeout stops it, times fifty hosts in flight. A real eureka_info
				// reply is a few KB.
				if err := json.NewDecoder(io.LimitReader(resp.Body, maxEurekaInfo)).Decode(&info); err != nil || info.Name == "" {
					return
				}

				d := DiscoveredDevice{Name: info.Name, Host: h}
				mu.Lock()
				devices = append(devices, d)
				mu.Unlock()
				onFound(d)
			}(host)
		}
	}

	wg.Wait()
	close(stopTicker)
	tickerWg.Wait()
	// The ticker fires on a 500ms cadence, so the last emitted progress event is
	// almost always short of the total — leaving the UI progress bar stuck below
	// 100%. Emit one final event now that every host has been checked.
	onProgress(int(atomic.LoadInt64(&checked)), total)
	return devices, ""
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	// EventSource only ever issues a GET, and this endpoint probes every host on
	// a /24 — reject anything else rather than let an unrelated request method
	// kick off a network-wide scan.
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	var writeMu sync.Mutex
	send := func(evt ScanEvent) {
		if ctx.Err() != nil {
			return // client gone; writing to w is no longer valid
		}
		data, _ := json.Marshal(evt)
		writeMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		writeMu.Unlock()
	}

	// One scan at a time. A TCP scan probes every host on the subnet; stacking
	// them up (impatient re-clicks, a second browser tab) multiplies that load
	// on the network for no extra coverage.
	// The message rides on the "done" event, not a preceding "status" one: the
	// UI overwrites scanStatus when the stream ends, so a status-then-done pair
	// leaves the user reading "No devices found" instead of the real reason.
	if !scanInFlight.CompareAndSwap(false, true) {
		send(ScanEvent{Type: "done", Message: "A scan is already running — try again when it finishes"})
		return
	}
	defer scanInFlight.Store(false)

	// Parse optional explicit subnet (skips catt scan when provided).
	var explicitSubnets []string
	if s := r.URL.Query().Get("subnet"); s != "" {
		if base := parseSubnet(s); base != "" {
			explicitSubnets = []string{base}
		} else {
			send(ScanEvent{Type: "done", Message: "Invalid subnet — expected e.g. 192.168.1.0/24"})
			return
		}
	}

	var devices []DiscoveredDevice

	if len(explicitSubnets) == 0 {
		send(ScanEvent{Type: "status", Message: "Running mDNS scan via catt..."})
		devices = mdnsScan(ctx)
		if len(devices) > 0 {
			for i := range devices {
				send(ScanEvent{Type: "found", Device: &devices[i]})
			}
			send(ScanEvent{Type: "done", Count: len(devices)})
			return
		}
		send(ScanEvent{Type: "status", Message: "mDNS scan found nothing — starting TCP fallback"})
	}

	devices, failure := tcpScanner(
		ctx,
		explicitSubnets,
		func(msg string) { send(ScanEvent{Type: "status", Message: msg}) },
		func(d DiscoveredDevice) { send(ScanEvent{Type: "found", Device: &d}) },
		func(checked, total int) { send(ScanEvent{Type: "progress", Checked: checked, Total: total}) },
	)

	send(ScanEvent{Type: "done", Count: len(devices), Message: failure})
}

func handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	// Read-only, but not cheap: it fans out a subprocess per configured device.
	// Reject other methods rather than let, say, a stray POST spend 15s per device.
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	cfgMu.RLock()
	devices := append([]DeviceConfig{}, cfg.Devices...)
	defaultURL := cfg.DefaultURL
	cfgMu.RUnlock()

	// Computed once for the whole list rather than per device: whether a row is
	// a duplicate is a property of the list, and monitorDevices acts on only the
	// first row of each deviceKey.
	dups := duplicateDeviceKeys(devices)

	// Index-assigned rather than appended: append order is whichever device
	// answered first, which reshuffles the list on every poll.
	statuses := make([]DeviceStatus, len(devices))
	var wg sync.WaitGroup
	for i, dev := range devices {
		wg.Add(1)
		go func(i int, d DeviceConfig) {
			defer wg.Done()
			statuses[i] = getDeviceStatus(r.Context(), d, defaultURL, dups[deviceKey(d)])
		}(i, dev)
	}
	wg.Wait()
	json.NewEncoder(w).Encode(statuses)
}

func handleCast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Bound the body like /api/config does: unauthenticated on the LAN, and an
	// unbounded json.Decode will consume all available memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Name string `json:"name"`
		Host string `json:"host"`
		URL  string `json:"url"`
	}
	if err := decodeOneObject(r.Body, &req); err != nil {
		rejectBody(w, err)
		return
	}
	// Trim like normalizeConfig does. deviceKey is built from these, so an
	// untrimmed name from the UI's unsaved edits filed the cast error under a
	// key nothing else ever reads — the failure never reached the device card,
	// and pruneCastStates never collected it.
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	req.URL = strings.TrimSpace(req.URL)
	// Either identifier will do — cattDeviceArgs prefers the IP, and auto-cast
	// has always worked on a host-only device, so rejecting one here made the
	// Cast button fail with a 400 on exactly the devices the monitor handles best.
	//
	// Reported separately: one message covering both left the UI showing "name or
	// host, and url, required" for a device that has a perfectly good name and
	// only a blank URL, which reads as though the device itself were the problem.
	if req.Name == "" && req.Host == "" {
		http.Error(w, "name or host required", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	// Reject anything that is not an http(s) URL before handing it to catt as a
	// positional argument. A value starting with "-" is read by catt's argument
	// parser as a flag, and one it happens to accept ("--version") makes catt
	// exit 0 without casting anything — which we then record as a *successful*
	// cast, arming the app-id learner to adopt whatever is actually running on
	// the device as our own. That mislearning is fleet-wide (see learnedCastApp),
	// so a single bad request is worth more than the confusing failure it saves.
	if !castableURL(req.URL) {
		http.Error(w, "url must be an absolute http:// or https:// URL", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		// context.Background, not r.Context: this outlives the response we are
		// about to write, and the request context is cancelled the moment the
		// handler returns.
		out, err := castSite(context.Background(), dev, req.URL)
		if err != nil {
			log.Printf("cast %q -> %s: %v — %s", deviceLabel(dev), req.URL, err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, true, req.URL)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "casting"})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if err := decodeOneObject(r.Body, &req); err != nil {
		rejectBody(w, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	if req.Name == "" && req.Host == "" {
		http.Error(w, "name or host required", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		out, err := stopCast(context.Background(), dev)
		if err != nil {
			log.Printf("stop %q: %v — %s", deviceLabel(dev), err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, false, "")
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

func main() {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	if p := os.Getenv("STATIC_DIR"); p != "" {
		staticDir = p
	}
	if p := os.Getenv("STATUS_SCRIPT"); p != "" {
		statusScript = p
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		log.Printf("warning: could not create config dir: %v", err)
	}
	loadConfig()
	go monitorLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/subnets", handleSubnets)
	mux.HandleFunc("/api/devices/scan", handleScan)
	mux.HandleFunc("/api/devices/status", handleDeviceStatus)
	mux.HandleFunc("/api/devices/cast", handleCast)
	mux.HandleFunc("/api/devices/stop", handleStop)
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	// No WriteTimeout: /api/devices/scan is a long-lived SSE stream and a write
	// deadline would kill it mid-scan.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("chromecast-sender running on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
