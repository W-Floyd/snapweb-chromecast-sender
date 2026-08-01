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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseSubnet(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.168.1", "192.168.1"},
		{"192.168.1.0/24", "192.168.1"},
		{"192.168.1.5/24", "192.168.1"},
		{"192.168.1.x", "192.168.1"},
		{"192.168.1.X", "192.168.1"},
		{"192.168.1.*", "192.168.1"},
		{"10.0.0.42", "10.0.0"},
		{"  192.168.1.0/24  ", "192.168.1"},
		// Spaces around the mask: the cut leaves a trailing one, which neither
		// net.ParseIP nor the digit check accepts, so this read as "Invalid subnet".
		{"192.168.1.0 / 24", "192.168.1"},
		{"192.168.1 / 24", "192.168.1"},
		{"", ""},
		{"not-an-ip", ""},
		{"192.168", ""},
		{"192.168.1.2.3", ""},
		{"192.168.999", ""},
		{"192.168.abc", ""},
		{"fe80::1", ""},
		// Atoi accepts these; Go's IP parser does not, so every address built
		// from such a base fails to dial and the scan reports "No devices found"
		// rather than "Invalid subnet".
		{"192.168.01", ""},
		{"192.168.+1", ""},
		{"010.0.0", ""},
	}
	for _, c := range cases {
		if got := parseSubnet(c.in); got != c.want {
			t.Errorf("parseSubnet(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseCattScan(t *testing.T) {
	// The real `catt scan` format.
	out := "Scanning Chromecasts...\n" +
		"192.168.1.183 - Living Room - Google Inc. Chromecast\n" +
		"192.168.1.100 - Dining Table Display - Google Inc. Nest Hub\n"
	got := parseCattScan(out)
	want := []DiscoveredDevice{
		{Name: "Living Room", Host: "192.168.1.183"},
		{Name: "Dining Table Display", Host: "192.168.1.100"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseCattScan returned %d devices, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("device %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A friendly name containing " - " must survive intact. Cutting at the first
	// separator truncated it, and catt was then asked for a device that has no
	// such name.
	got = parseCattScan("192.168.1.7 - Kitchen - Nest Hub - Google Inc. Nest Hub\n")
	if len(got) != 1 || got[0].Name != "Kitchen - Nest Hub" {
		t.Errorf("name with a separator = %+v, want \"Kitchen - Nest Hub\"", got)
	}

	// Labelled form still works, and noise is ignored.
	got = parseCattScan("Name: Bathroom\nHost: 192.168.1.134\nsome - unrelated - text\n")
	if len(got) != 1 || got[0] != (DiscoveredDevice{Name: "Bathroom", Host: "192.168.1.134"}) {
		t.Errorf("labelled parse = %+v", got)
	}

	// ...in either field order. Emitting only on "Host:" dropped the device
	// entirely when the host line came first.
	got = parseCattScan("Host: 192.168.1.134\nName: Bathroom\n")
	if len(got) != 1 || got[0] != (DiscoveredDevice{Name: "Bathroom", Host: "192.168.1.134"}) {
		t.Errorf("host-first labelled parse = %+v", got)
	}

	if got := parseCattScan("Scanning Chromecasts...\nNo devices found.\n"); len(got) != 0 {
		t.Errorf("expected no devices, got %+v", got)
	}

	// mDNS answers arrive per interface, so a host that sees the LAN two ways can
	// list the same device twice. The UI keys its list by host and Alpine throws
	// on a duplicate key, dropping every discovered device rather than the repeat.
	got = parseCattScan("192.168.1.5 - Lounge - Google Inc. Chromecast\n" +
		"192.168.1.5 - Lounge - Google Inc. Chromecast\n" +
		"Name: Lounge\nHost: 192.168.1.5\n")
	if len(got) != 1 {
		t.Errorf("duplicate hosts not collapsed: %+v", got)
	}
}

func TestSaveConfigAtomicAndReadable(t *testing.T) {
	dir := t.TempDir()
	oldPath := cfgPath
	cfgPath = filepath.Join(dir, "config.json")
	defer func() { cfgPath = oldPath }()

	want := Config{CheckInterval: 30, DefaultURL: "http://example/", Devices: []DeviceConfig{{Name: "A", Host: "1.2.3.4"}}}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("config mode = %o, want 644", perm)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CheckInterval != 30 || len(got.Devices) != 1 || got.Devices[0].Name != "A" {
		t.Errorf("round-trip = %+v", got)
	}

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only config.json in %s, got %d entries", dir, len(entries))
	}
}

// POST /api/config replaces the whole config, so a body that carries no value
// must be refused rather than read as the empty value. `null` decodes into a
// Config without error and leaves the zero value, which normalizeConfig turns
// into a valid "no devices, no default URL, 60s" — every device deleted from
// disk, every cast state pruned, and a 200 returned for it. A body with
// content after the object is the same hazard by a different route: Decode
// stops at the first value, so a concatenated or double-encoded payload
// persisted only its first half.
func TestPostConfigRejectsBodiesThatWouldSilentlyWipeIt(t *testing.T) {
	dir := t.TempDir()
	oldPath := cfgPath
	cfgPath = filepath.Join(dir, "config.json")
	defer func() { cfgPath = oldPath }()

	original := Config{CheckInterval: 30, DefaultURL: "http://dash/", Devices: []DeviceConfig{{Name: "Lounge", Host: "1.2.3.4"}}}
	cfgMu.Lock()
	saved := cfg
	cfg = original
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfg = saved
		cfgMu.Unlock()
	}()
	if err := saveConfig(original); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	for _, body := range []string{
		"null",
		`{"check_interval":30,"devices":[]} {"devices":[]}`,
		`{"check_interval":30} trailing`,
	} {
		rec := httptest.NewRecorder()
		handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %q = %d, want 400", body, rec.Code)
		}
		cfgMu.RLock()
		devices := len(cfg.Devices)
		cfgMu.RUnlock()
		if devices != 1 {
			t.Errorf("POST %q left %d devices in the live config, want the original 1", body, devices)
		}
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var onDisk Config
		if err := json.Unmarshal(data, &onDisk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(onDisk.Devices) != 1 || onDisk.DefaultURL != "http://dash/" {
			t.Errorf("POST %q overwrote the config on disk: %+v", body, onDisk)
		}
	}

	// A well-formed replacement must still get through, trailing newline and all
	// — a JSON encoder writing to the wire adds one.
	rec := httptest.NewRecorder()
	handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"check_interval":45,"default_url":"http://new/","devices":[]}`+"\n")))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid POST = %d (%s), want 200", rec.Code, rec.Body)
	}
	cfgMu.RLock()
	got := cfg
	cfgMu.RUnlock()
	if got.CheckInterval != 45 || got.DefaultURL != "http://new/" || len(got.Devices) != 0 {
		t.Errorf("valid POST stored %+v", got)
	}
}

func TestScanEventProgressAlwaysHasCounts(t *testing.T) {
	// checked == 0 must still marshal, or the UI renders "undefined / N hosts".
	data, err := json.Marshal(ScanEvent{Type: "progress", Checked: 0, Total: 254})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["checked"]; !ok {
		t.Errorf("progress event omitted \"checked\": %s", data)
	}
}

func TestNormalizeConfig(t *testing.T) {
	c := Config{CheckInterval: 0}
	normalizeConfig(&c)
	if c.CheckInterval != 60 {
		t.Errorf("zero interval = %d, want default 60", c.CheckInterval)
	}
	if c.Devices == nil {
		t.Error("nil Devices should normalize to an empty slice")
	}

	c = Config{CheckInterval: 3}
	normalizeConfig(&c)
	if c.CheckInterval != minCheckInterval {
		t.Errorf("interval 3 = %d, want floor %d", c.CheckInterval, minCheckInterval)
	}

	// Past ~9.2e9 seconds the monitor's `Duration(interval) * time.Second`
	// overflows int64 and wraps negative, time.Sleep does not wait at all, and the
	// loop re-probes and re-casts every device continuously.
	c = Config{CheckInterval: 1 << 40}
	normalizeConfig(&c)
	if c.CheckInterval != maxCheckInterval {
		t.Errorf("huge interval = %d, want ceiling %d", c.CheckInterval, maxCheckInterval)
	}
	if d := time.Duration(c.CheckInterval) * time.Second; d <= 0 {
		t.Errorf("clamped interval still yields a non-positive sleep: %v", d)
	}

	c = Config{
		CheckInterval: 30,
		DefaultURL:    "  http://example/  ",
		Devices:       []DeviceConfig{{Name: " Living Room ", Host: " 1.2.3.4 ", URL: " http://x/ "}},
	}
	normalizeConfig(&c)
	if c.DefaultURL != "http://example/" {
		t.Errorf("DefaultURL = %q, want trimmed", c.DefaultURL)
	}
	if d := c.Devices[0]; d.Name != "Living Room" || d.Host != "1.2.3.4" || d.URL != "http://x/" {
		t.Errorf("device not trimmed: %+v", d)
	}
	if c.CheckInterval != 30 {
		t.Errorf("valid interval was altered: %d", c.CheckInterval)
	}
}

func TestCastStateKeyedByHost(t *testing.T) {
	resetCastState()

	// Two devices sharing a friendly name must not share cast state.
	a := DeviceConfig{Name: "Speaker", Host: "1.2.3.4"}
	b := DeviceConfig{Name: "Speaker", Host: "5.6.7.8"}
	setCastState(a, true, "")
	if !isCasting(a) {
		t.Error("a should be casting")
	}
	if isCasting(b) {
		t.Error("b must not inherit a's cast state")
	}

	// Devices dropped from the config must not linger in the map.
	pruneCastStates([]DeviceConfig{b})
	if isCasting(a) {
		t.Error("pruned device still marked casting")
	}
	castStatesMu.RLock()
	n := len(castStates)
	castStatesMu.RUnlock()
	if n != 0 {
		t.Errorf("castStates has %d stale entries after prune", n)
	}
}

// A device needs only one of name and host, and a host-only entry is the
// recommended configuration under Docker. Logging dev.Name alone printed
// `auto-casting to ""` for exactly those devices.
func TestDeviceLabelFallsBackToHost(t *testing.T) {
	if got := deviceLabel(DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}); got != "Lounge" {
		t.Errorf("deviceLabel = %q, want the name", got)
	}
	if got := deviceLabel(DeviceConfig{Host: "1.2.3.4"}); got != "1.2.3.4" {
		t.Errorf("deviceLabel = %q, want the host", got)
	}
	if got := deviceLabel(DeviceConfig{}); got == "" {
		t.Error("deviceLabel of an unaddressed device must still name something")
	}
}

func TestCastErrorRecordedAndCleared(t *testing.T) {
	resetCastState()

	dev := DeviceConfig{Name: "Kitchen", Host: "1.2.3.4"}
	setCastError(dev, "  Chromecast not found  ")
	if got := castError(dev); got != "Chromecast not found" {
		t.Errorf("castError = %q, want trimmed message", got)
	}
	if isCasting(dev) {
		t.Error("a failed cast must not leave the device marked as casting")
	}

	// A later success clears the stale error.
	setCastState(dev, true, "")
	if got := castError(dev); got != "" {
		t.Errorf("error not cleared after success: %q", got)
	}

	// Long output is truncated without splitting a multi-byte rune.
	setCastError(dev, strings.Repeat("é", maxStatusTextLen*2))
	got := castError(dev)
	if !utf8.ValidString(got) {
		t.Errorf("truncated message is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxStatusTextLen+1 { // +1 for the ellipsis
		t.Errorf("truncated to %d runes, want %d", n, maxStatusTextLen+1)
	}
}

func resetCastState() {
	castStatesMu.Lock()
	castStates, castErrors = map[string]bool{}, map[string]string{}
	castActions = map[string]time.Time{}
	castObserved = map[string]time.Time{}
	castLearnPending = map[string]bool{}
	castURLs = map[string]string{}
	learnedCastApp, castAppCandidate = "", ""
	castStatesMu.Unlock()
}

// learnCastApp drives the two-cast agreement that teaches us our own app id.
func learnCastApp(t *testing.T, dev DeviceConfig, appID string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		setCastState(dev, true, "")
		observeCastState(dev, true, appID, time.Now())
	}
	if isForeignApp(appID) || !isForeignApp(appID+"-other") {
		t.Fatalf("app id %q was not learned after two agreeing casts", appID)
	}
}

func TestObserveCastStateKeepsErrorWhileIdle(t *testing.T) {
	resetCastState()

	dev := DeviceConfig{Name: "Kitchen", Host: "1.2.3.4"}
	setCastError(dev, "Chromecast not found")

	// A status poll seeing the device idle is the consequence of that failure,
	// not new information — it must not erase the reason.
	observeCastState(dev, false, "", time.Now())
	if got := castError(dev); got != "Chromecast not found" {
		t.Errorf("idle observation cleared the cast error: %q", got)
	}
	if isCasting(dev) {
		t.Error("device observed idle should not be marked casting")
	}

	// Nor does seeing it *play*. A failed cast usually leaves the previous page up,
	// so the poll that follows finds the device playing — that is a consequence of
	// the failure too, not evidence against it, and clearing there did more than
	// hide the reason: monitorDevices then had nothing to tell "playing the page we
	// asked for" from "playing the page we failed to replace", and skipped the
	// device on every tick from then on. Only an action of ours succeeding clears it.
	observeCastState(dev, true, "", time.Now())
	if got := castError(dev); got != "Chromecast not found" {
		t.Errorf("playing observation cleared the cast error: %q", got)
	}
	if !isCasting(dev) {
		t.Error("device observed playing should be marked casting")
	}

	// ...and that is setCastState, which every successful retry goes through.
	setCastState(dev, true, "http://dash/")
	if got := castError(dev); got != "" {
		t.Errorf("a successful cast left the old error behind: %q", got)
	}
}

// A status probe takes seconds, so one that started before a cast/stop can
// finish after it. Applying what it saw beforehand would undo the newer truth:
// the monitor then re-casts a device that is already playing, and a just-recorded
// cast error is wiped by an observation that predates the failure.
func TestStaleObservationDoesNotOverwriteNewerAction(t *testing.T) {
	dev := DeviceConfig{Name: "Kitchen", Host: "1.2.3.4"}

	resetCastState()
	probeStarted := time.Now().Add(-time.Second)
	setCastState(dev, true, "") // cast completes while the probe is still running
	observeCastState(dev, false, "", probeStarted)
	if !isCasting(dev) {
		t.Error("stale idle observation overwrote a newer successful cast")
	}

	resetCastState()
	probeStarted = time.Now().Add(-time.Second)
	setCastError(dev, "Chromecast not found")
	observeCastState(dev, true, "", probeStarted)
	if got := castError(dev); got != "Chromecast not found" {
		t.Errorf("stale playing observation erased a newer cast error: %q", got)
	}
	if isCasting(dev) {
		t.Error("stale playing observation overwrote a newer failure")
	}

	// A probe that *starts* after the action is current, and must still be able
	// to notice that the device dropped the cast on its own.
	resetCastState()
	setCastState(dev, true, "")
	observeCastState(dev, false, "", time.Now())
	if isCasting(dev) {
		t.Error("a fresh observation should be applied")
	}
}

// The app id our casts run under is learned from the first poll after a cast we
// initiated. Hardcoding it would be worse than not knowing: a wrong constant
// makes the monitor read its own dashboard as a foreign app and re-cast it every
// tick, so "cannot tell" must never report foreign.
func TestForeignAppDetection(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	resetCastState()
	// Nothing learned yet: no app may be called foreign, however unfamiliar.
	if isForeignApp("CA5E9605") {
		t.Error("an unlearned app must not be reported as foreign")
	}

	// One sighting is only a candidate — see castAppCandidate.
	setCastState(dev, true, "")
	observeCastState(dev, true, "84912283", time.Now())
	if isForeignApp("CA5E9605") {
		t.Error("a single unconfirmed sighting must not be acted on")
	}

	learnCastApp(t, dev, "84912283")
	if isForeignApp("84912283") {
		t.Error("our own cast app reported as foreign")
	}
	// An empty app id is "cannot tell", not "someone else".
	if isForeignApp("") {
		t.Error("an empty app id must not be reported as foreign")
	}

	// Only a poll with the learn flag armed teaches us; an unsolicited poll of
	// whatever someone else started must not redefine our own cast.
	observeCastState(dev, true, "CA5E9605", time.Now())
	if !isForeignApp("CA5E9605") {
		t.Error("a later foreign observation overwrote the learned cast app")
	}
}

// A wrong app id must not be permanent. It is fleet-wide, so a device without
// takeover would never be cast again, and with no further casts there is no
// later observation that could correct it — the service would be wedged until a
// restart.
func TestMislearnedCastAppSelfHeals(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	// Somebody starts Netflix in the gap between our cast and the poll, twice,
	// so it is confirmed as "ours".
	learnCastApp(t, dev, "CA5E9605")
	if !isForeignApp("84912283") {
		t.Fatal("setup: the real dashboard should now read as foreign")
	}

	// The next cast's poll sees the real dashboard and disagrees. That alone must
	// drop the bad value, or nothing is ever cast again.
	setCastState(dev, true, "")
	observeCastState(dev, true, "84912283", time.Now())
	if isForeignApp("84912283") {
		t.Error("a disagreeing observation left the mislearned app id in place")
	}

	// Casting resumes, and the next agreeing poll settles on the right id.
	setCastState(dev, true, "")
	observeCastState(dev, true, "84912283", time.Now())
	if isForeignApp("84912283") {
		t.Error("our own app still reported as foreign after re-learning")
	}
	if !isForeignApp("CA5E9605") {
		t.Error("the interloper's app should now be the foreign one")
	}
}

// A dropped observation is not evidence of anything. Reporting the app a stale
// probe saw made the monitor "take back" a device that was already showing our
// dashboard.
func TestStaleObservationIsReportedAsNotApplied(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	probeStarted := time.Now().Add(-time.Second)
	setCastState(dev, true, "")
	if observeCastState(dev, true, "CA5E9605", probeStarted) {
		t.Error("an observation predating our cast should report as not applied")
	}
	if !observeCastState(dev, true, "84912283", time.Now()) {
		t.Error("a fresh observation should report as applied")
	}
}

// A failed cast leaves nothing of ours running, so the next app to show up on
// the device belongs to somebody else. Learning it as our own would make the
// real dashboard read as foreign for the whole fleet — learnedCastApp is one
// value — and the monitor would re-cast it on every tick.
func TestFailedCastDoesNotLearnSomeoneElsesApp(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	setCastState(dev, true, "") // arms the learn flag
	setCastError(dev, "Chromecast not found")

	// Someone starts Netflix on it afterwards.
	observeCastState(dev, true, "CA5E9605", time.Now())
	if isForeignApp("84912283") {
		t.Error("a failed cast let the next foreign app be learned as ours")
	}
	if isForeignApp("CA5E9605") {
		t.Error("nothing has been learned, so nothing may be called foreign")
	}
}

// The learn flag is armed by a cast and consumed by the next poll that finds
// something running, so a cast that exits 0 without the dashboard sticking left
// it armed indefinitely — and a poll hours later, once somebody had started their
// own app, offered that app to the learner as ours.
//
// It takes two devices to do real damage, because agreement is what promotes a
// candidate: one stale flag only mis-seeds the candidate, two agree and the wrong
// id becomes trusted fleet-wide. An applied observation of an *idle* device is
// proof that nothing of ours is running, which is exactly the reasoning
// setCastError already disarms on.
func TestIdleObservationDisarmsTheAppLearner(t *testing.T) {
	resetCastState()
	a := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	b := DeviceConfig{Name: "Kitchen", Host: "5.6.7.8"}

	for _, dev := range []DeviceConfig{a, b} {
		setCastState(dev, true, "") // arms the learn flag
		// The cast reported success, but the device is sitting idle.
		if !observeCastState(dev, false, "", time.Now()) {
			t.Fatal("a fresh observation should be applied")
		}
		// Somebody starts Netflix on it later.
		observeCastState(dev, true, "CA5E9605", time.Now())
	}
	if isForeignApp("84912283") {
		t.Error("idle polls left the learner armed, and someone else's app became ours")
	}
	if isForeignApp("CA5E9605") {
		t.Error("nothing may be called foreign while nothing has been learned")
	}

	// The flag must still do its job when the cast does stick.
	resetCastState()
	learnCastApp(t, a, "84912283")
}

// The learn flag is consumed by the first applied poll after our cast, even one
// that reports the device playing something it cannot name. cc_status.py never
// produces that pair today (an empty app_id is reported as idle), but the flag
// surviving a poll is precisely the hours-later mislearn that armed-forever
// caused, so it must not depend on an invariant in another language.
func TestUnidentifiableAppAlsoDisarmsTheLearner(t *testing.T) {
	resetCastState()
	a := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	b := DeviceConfig{Name: "Kitchen", Host: "5.6.7.8"}

	for _, dev := range []DeviceConfig{a, b} {
		setCastState(dev, true, "") // arms the learn flag
		// Playing, but with no app id to attribute it to.
		if !observeCastState(dev, true, "", time.Now()) {
			t.Fatal("a fresh observation should be applied")
		}
		// Somebody starts Netflix on it later.
		observeCastState(dev, true, "CA5E9605", time.Now())
	}
	if isForeignApp("84912283") {
		t.Error("an unnameable app left the learner armed, and someone else's app became ours")
	}
}

// bufio.Scanner's default token limit is 64KB — exactly maxSubprocessOutput —
// so subprocess output arriving as one long line failed the very first Scan
// with ErrTooLong and yielded no lines at all. Both callers ignore that error,
// so a status query silently stayed "unknown" and an mDNS scan silently found
// nothing.
func TestParsersSurviveOutputWithNoNewline(t *testing.T) {
	long := strings.Repeat("x", maxSubprocessOutput)
	if got := splitLines(long); len(got) != 1 || got[0] != long {
		t.Errorf("splitLines dropped a %d-byte unterminated line: %d lines", len(long), len(got))
	}
	// The realistic shape: a device on one line, then a flood with no newline.
	out := "192.168.1.5 - Lounge - Google Inc. Chromecast\n" + long
	if got := parseCattScan(out); len(got) != 1 || got[0].Host != "192.168.1.5" {
		t.Errorf("parseCattScan lost its devices to an overlong line: %+v", got)
	}
	// bufio.ScanLines strips a trailing CR; splitLines must too, or every parsed
	// value carries an invisible character.
	if got := splitLines("a\r\nb"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitLines(%q) = %q, want CR-stripped lines", "a\r\nb", got)
	}
}

func TestLimitedBufferKeepsThePrefixAndClaimsTheWholeWrite(t *testing.T) {
	b := &limitedBuffer{max: 8}
	// A short count would surface as io.ErrShortWrite from cmd.Run and replace the
	// real result with a plumbing detail.
	if n, err := b.Write([]byte("abcdefghij")); n != 10 || err != nil {
		t.Errorf("Write = (%d, %v), want (10, nil)", n, err)
	}
	if got := b.String(); got != "abcdefgh" {
		t.Errorf("buffered %q, want the first 8 bytes", got)
	}
	// Writes past the cap are dropped, not appended, and not an error either.
	if n, err := b.Write([]byte("klm")); n != 3 || err != nil {
		t.Errorf("over-cap Write = (%d, %v), want (3, nil)", n, err)
	}
	if got := b.String(); got != "abcdefgh" {
		t.Errorf("buffer grew past its cap: %q", got)
	}
	if len(b.Bytes()) != 8 {
		t.Errorf("Bytes() = %d bytes, want 8", len(b.Bytes()))
	}
}

// The layered budget in cc_status.py only works if the Go side outlasts the
// script's own watchdog; otherwise the script is killed with nothing on the pipe,
// which is the failure mode the watchdog exists to avoid.
//
// Read out of the script rather than hardcoded: the two halves of the budget live
// in different languages, and a comment asking the next reader to check the other
// side is exactly what does not happen. Needs no Python — it is the source file
// that is consulted, not the interpreter.
func TestStatusQueryTimeoutOutlastsTheScriptWatchdog(t *testing.T) {
	watchdog := pythonSeconds(t, "OVERALL_TIMEOUT")
	if statusQueryTimeout <= watchdog {
		t.Errorf("statusQueryTimeout = %v, must exceed the script's %v watchdog", statusQueryTimeout, watchdog)
	}
	// And the script's own layers must stay ordered inside that. The stages run one
	// after another, not in parallel — reachable(), then the connect wait, then the
	// disconnect in the finally block — so it is their *sum* that has to fit, and
	// comparing them to the watchdog one at a time passed a budget that could not
	// actually be spent. Two thresholds, because the two overruns cost different
	// things:
	//
	//   probe + connect < watchdog       the stages that produce a diagnosis must
	//                                   finish first, or the device's real problem
	//                                   is replaced by a bare "timed out"
	//   + disconnect   <= watchdog       and the cleanup must fit too, or the
	//                                   watchdog fires during it and takes the
	//                                   process down with os._exit mid-shutdown
	probe, connect := pythonSeconds(t, "PROBE_TIMEOUT"), pythonSeconds(t, "CONNECT_TIMEOUT")
	disconnect := pythonSeconds(t, "DISCONNECT_TIMEOUT")
	if !(probe < connect && probe+connect < watchdog && probe+connect+disconnect <= watchdog) {
		t.Errorf("script budget does not fit: probe %v + connect %v + disconnect %v, watchdog %v",
			probe, connect, disconnect, watchdog)
	}
}

// pythonSeconds reads an integer-seconds constant out of the status helper.
func pythonSeconds(t *testing.T, name string) time.Duration {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("scripts", "cc_status.py"))
	if err != nil {
		t.Fatalf("read the status helper: %v", err)
	}
	for _, line := range splitLines(string(src)) {
		value, ok := strings.CutPrefix(line, name+" = ")
		if !ok {
			continue
		}
		// Trailing comment, as every one of these constants carries.
		value, _, _ = strings.Cut(value, "#")
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("%s is not an integer number of seconds: %q", name, value)
		}
		return time.Duration(n) * time.Second
	}
	t.Fatalf("%s not found in the status helper", name)
	return 0
}

func TestUnaddressedDeviceIsNotProbed(t *testing.T) {
	// A blank "+ Add manually" row has neither identifier. Probing it shelled out
	// for the full timeout every poll, and every such row shares one deviceKey.
	ds := getLiveStatus(context.Background(), DeviceConfig{})
	if ds.Error == "" {
		t.Error("an unaddressed device should report why it cannot be reached")
	}
	if ds.State != "unknown" {
		t.Errorf("state = %q, want \"unknown\"", ds.State)
	}
}

// An IP is what routes a device to the pychromecast helper instead of to `catt
// status`, and it is the whole reason the config recommends setting one: catt
// reports nothing usable about a web-page cast, so only the helper can tell a
// dropped cast from a live one. Nothing asserted the routing itself, and getting
// it backwards would be invisible — both paths return a plausible DeviceStatus.
func TestGetLiveStatusRoutesAHostToTheProbe(t *testing.T) {
	probed := 0
	withProbe(t, func(_ context.Context, dev DeviceConfig) DeviceStatus {
		probed++
		return DeviceStatus{Name: dev.Name, Host: dev.Host, State: "Playing"}
	})

	ds := getLiveStatus(context.Background(), DeviceConfig{Name: "Lounge", Host: "1.2.3.4"})
	if probed != 1 || ds.State != "Playing" {
		t.Errorf("a device with an IP was not answered by the probe: %d probes, %+v", probed, ds)
	}

	// And a row with neither identifier is answered without any subprocess at all:
	// shelling out ran `catt -d '' status` for its full timeout on every poll, and
	// every such row shares the deviceKey "name:".
	if ds = getLiveStatus(context.Background(), DeviceConfig{}); probed != 1 {
		t.Errorf("an unaddressed device reached the probe: %+v", ds)
	}
}

// catt's -d takes either a friendly name or an IP, and the IP is what bypasses
// mDNS — which does not work under Docker bridge networking at all, so a device
// carrying both must be targeted by address. Nothing asserted the preference, and
// with it inverted every catt call in the container silently went through
// discovery and failed to find anything.
func TestCattDeviceArgsPrefersTheIP(t *testing.T) {
	cases := []struct {
		dev  DeviceConfig
		want []string
	}{
		{DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}, []string{"-d", "1.2.3.4"}},
		{DeviceConfig{Name: "Lounge"}, []string{"-d", "Lounge"}},
		{DeviceConfig{Host: "1.2.3.4"}, []string{"-d", "1.2.3.4"}},
	}
	for _, c := range cases {
		got := cattDeviceArgs(c.dev)
		if len(got) != len(c.want) || got[0] != c.want[0] || got[1] != c.want[1] {
			t.Errorf("cattDeviceArgs(%+v) = %q, want %q", c.dev, got, c.want)
		}
		// The result is appended to by every caller, so it must not share an array
		// with anything: two appends onto one backing store would have the second
		// subcommand overwrite the first.
		a, b := append(cattDeviceArgs(c.dev), "status"), append(cattDeviceArgs(c.dev), "stop")
		if a[2] != "status" || b[2] != "stop" {
			t.Errorf("appending to cattDeviceArgs aliased: %q / %q", a, b)
		}
	}
}

// withCattStatus stands in for the `catt status` subprocess, which is the only
// one left in getLiveStatus.
func withCattStatus(t *testing.T, fn func(context.Context, DeviceConfig) (string, error)) *[]DeviceConfig {
	t.Helper()
	var asked []DeviceConfig
	saved := cattStatus
	cattStatus = func(ctx context.Context, dev DeviceConfig) (string, error) {
		asked = append(asked, dev)
		return fn(ctx, dev)
	}
	t.Cleanup(func() { cattStatus = saved })
	return &asked
}

// A device with no IP is answered by `catt status`, and both halves of what
// getLiveStatus then does with the result matter: a failed query must carry a
// reason (an empty Error reads as success and the card shows a plain "Idle"), and
// a successful one has to fall back to our own cached cast state, because catt's
// output for "our dashboard is up" is byte-identical to a genuinely idle device.
func TestGetLiveStatusUsesCattForADeviceWithNoIP(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge"}

	// Failure: catt printed nothing and exited non-zero, so the exec error is all
	// there is to report — and report it we must.
	asked := withCattStatus(t, func(_ context.Context, _ DeviceConfig) (string, error) {
		return "", errors.New("catt timed out after 10s")
	})
	ds := getLiveStatus(context.Background(), dev)
	if len(*asked) != 1 || (*asked)[0].Name != "Lounge" {
		t.Fatalf("catt was asked about %+v, want one query for Lounge", *asked)
	}
	if ds.Error != "catt timed out after 10s" || ds.State != "unknown" {
		t.Errorf("failed query = %+v, want the reason and state \"unknown\"", ds)
	}
	if ds.Name != "Lounge" || ds.Host != "" {
		t.Errorf("failed query did not echo its device: %+v", ds)
	}

	// Success, device not being cast to by us: catt says nothing about a web-page
	// cast, so "Idle" is the cached answer, not something it told us.
	withCattStatus(t, func(_ context.Context, _ DeviceConfig) (string, error) {
		return "Volume: 40\nVolume muted: False\n", nil
	})
	if ds = getLiveStatus(context.Background(), dev); ds.State != "Idle" || ds.Error != "" {
		t.Errorf("uncast device = %+v, want a clean \"Idle\"", ds)
	}

	// ...and once we have cast to it, the same output must read as "Playing".
	// Getting this wrong re-casts the dashboard on every tick.
	setCastState(dev, true, "http://dash/")
	if ds = getLiveStatus(context.Background(), dev); ds.State != "Playing" {
		t.Errorf("cast device = %+v, want \"Playing\" from the cached cast state", ds)
	}

	// A device's own State line still wins over the cached guess.
	withCattStatus(t, func(_ context.Context, _ DeviceConfig) (string, error) {
		return "Title: Something\nState: PAUSED\n", nil
	})
	if ds = getLiveStatus(context.Background(), dev); ds.State != "PAUSED" {
		t.Errorf("device reporting its own state = %+v, want \"PAUSED\"", ds)
	}
}

// An auto-cast device with no IP is unmonitorable: `catt status` describes the
// media session, a web page cast has none, and its output is identical for "our
// dashboard is up" and "idle". The monitor therefore never notices a dropped
// cast and silently stops re-casting, so the card has to say so.
func TestAutoCastWithoutIPIsFlagged(t *testing.T) {
	const ok = "http://dash/"
	if configWarning(DeviceConfig{Name: "Lounge", AutoCast: true}, ok, false) == "" {
		t.Error("an auto-cast device with no IP should be flagged")
	}
	// An IP switches it to the pychromecast helper, which reports the app id.
	if got := configWarning(DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}, ok, false); got != "" {
		t.Errorf("a device with an IP needs no warning, got %q", got)
	}
	// Nothing is monitoring it, so there is nothing to warn about.
	if got := configWarning(DeviceConfig{Name: "Lounge"}, "", false); got != "" {
		t.Errorf("a manual-only device needs no warning, got %q", got)
	}
	// getLiveStatus already explains this one; do not pile a second line on it.
	if got := configWarning(DeviceConfig{AutoCast: true}, ok, false); got != "" {
		t.Errorf("an unaddressed device is already reported, got %q", got)
	}
}

// monitorDevices skips a device it has no usable URL for, and a skip is
// invisible: the card read a plain "Idle" while auto-cast was in fact never
// going to do anything with it.
func TestAutoCastWithoutUsableURLIsFlagged(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	if got := configWarning(dev, "", false); got == "" {
		t.Error("an auto-cast device with no URL and no default should be flagged")
	}
	if got := configWarning(dev, "192.168.1.5/dash", false); got == "" {
		t.Error("an auto-cast device with an unusable URL should be flagged")
	}
	// The URL problem is reported ahead of the missing IP: without a URL the
	// device is never cast to at all, so it is the more fundamental of the two.
	if got := configWarning(DeviceConfig{Name: "Lounge", AutoCast: true}, "", false); !strings.Contains(got, "URL") {
		t.Errorf("warning = %q, want the URL problem reported first", got)
	}
}

// A device's own URL wins over the default; the fallback is what the monitor and
// the warning must agree on.
func TestEffectiveURL(t *testing.T) {
	if got := effectiveURL(DeviceConfig{URL: "http://own/"}, "http://default/"); got != "http://own/" {
		t.Errorf("effectiveURL = %q, want the device's own URL", got)
	}
	if got := effectiveURL(DeviceConfig{}, "http://default/"); got != "http://default/" {
		t.Errorf("effectiveURL = %q, want the default", got)
	}
	if got := effectiveURL(DeviceConfig{}, ""); got != "" {
		t.Errorf("effectiveURL = %q, want empty", got)
	}
}

// Two status polls of the same device overlap routinely — /api/devices/status
// and the monitor probe independently, each taking up to 15s — so the one that
// started earlier can finish later. Applying it republished the older view: a
// device seen playing flipped back to "Idle" for a whole interval.
func TestOlderObservationDoesNotOverwriteNewerObservation(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	early, late := time.Now().Add(-2*time.Second), time.Now().Add(-time.Second)
	if !observeCastState(dev, true, "84912283", late) {
		t.Fatal("the first observation should be applied")
	}
	if observeCastState(dev, false, "", early) {
		t.Error("an observation that began earlier should report as not applied")
	}
	if !isCasting(dev) {
		t.Error("an earlier-started poll overwrote a later one's view")
	}

	// A genuinely newer poll must still get through, or the device can never be
	// seen to drop the cast.
	if !observeCastState(dev, false, "", time.Now()) {
		t.Error("a newer observation should be applied")
	}
	if isCasting(dev) {
		t.Error("newer observation not applied")
	}
}

func TestPruneCastStatesDropsActions(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Gone", Host: "1.2.3.4"}
	setCastState(dev, true, "http://dash/")
	observeCastState(dev, true, "84912283", time.Now())
	setCastError(dev, "Failed to connect.")

	pruneCastStates(nil)

	// Every map keyed by deviceKey, so a device that comes *back* under the same key
	// does not inherit the state of the one that left. The castErrors entry is the
	// visible one: a red error on the card of a device nothing has failed on yet.
	castStatesMu.RLock()
	sizes := map[string]int{
		"castStates":   len(castStates),
		"castErrors":   len(castErrors),
		"castActions":  len(castActions),
		"castObserved": len(castObserved),
		"castURLs":     len(castURLs),
	}
	castStatesMu.RUnlock()
	for name, n := range sizes {
		if n != 0 {
			t.Errorf("%s has %d stale entries after prune", name, n)
		}
	}

	// And a device still in the config keeps its state.
	resetCastState()
	kept := DeviceConfig{Name: "Kept", Host: "5.6.7.8"}
	setCastError(kept, "Failed to connect.")
	pruneCastStates([]DeviceConfig{kept})
	if castError(kept) == "" {
		t.Error("a device that is still configured lost its recorded cast error")
	}
}

// catt reads a "-"-prefixed positional argument as a flag, and one it accepts
// ("--version") makes it exit 0 without casting — recorded as a success, which
// then arms the app-id learner to adopt whatever is really running on the device
// as our own, fleet-wide.
func TestCastableURL(t *testing.T) {
	for _, u := range []string{
		"http://192.168.1.5/dashboard",
		"https://example.test/",
		"http://host:8080/a?b=c#d",
	} {
		if !castableURL(u) {
			t.Errorf("castableURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"", "--version", "-h", "dashboard", "192.168.1.5",
		"file:///etc/passwd", "ftp://example.test/",
		"http:/dashboard", // parses, but has no host
		"http://",
		"http://\x7f/", // unparseable control character
	} {
		if castableURL(u) {
			t.Errorf("castableURL(%q) = true, want false", u)
		}
	}
}

// The interval has a one-day ceiling, so a monitor that sleeps on the value it
// read at the top of the cycle applied a lowered interval up to 24h late.
func TestCheckIntervalTracksConfig(t *testing.T) {
	cfgMu.Lock()
	old := cfg
	cfg = Config{CheckInterval: 86400}
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfg = old
		cfgMu.Unlock()
	}()

	if got := checkInterval(); got != 86400*time.Second {
		t.Errorf("checkInterval = %v, want 24h", got)
	}
	cfgMu.Lock()
	cfg.CheckInterval = 10
	cfgMu.Unlock()
	if got := checkInterval(); got != 10*time.Second {
		t.Errorf("checkInterval after a config change = %v, want 10s", got)
	}

	// Clamped even if something bypasses normalizeConfig: both ends of the range
	// otherwise turn the monitor into a hot loop.
	cfgMu.Lock()
	cfg.CheckInterval = 1 << 40
	cfgMu.Unlock()
	if got := checkInterval(); got != maxCheckInterval*time.Second {
		t.Errorf("huge interval = %v, want the ceiling", got)
	}
	cfgMu.Lock()
	cfg.CheckInterval = -1
	cfgMu.Unlock()
	if got := checkInterval(); got != minCheckInterval*time.Second {
		t.Errorf("negative interval = %v, want the floor", got)
	}
}

// A scan that cannot run has to say so on the "done" event: the UI overwrites
// the status line when the stream ends, so a reason sent as a status was
// replaced by the generic "No devices found".
func TestTCPScanReportsWhyItCannotRun(t *testing.T) {
	// Auto-detection is substituted rather than skipped around: driving this path
	// through the real one probes all 254 hosts of whatever LAN the machine running
	// the tests is on, so the message a host with no private address gets — the only
	// thing it ever sees from a scan — went unasserted on every machine that has one.
	withDetectedSubnets(t, nil)

	var statuses []string
	devices, failure := tcpScan(context.Background(), nil,
		func(msg string) { statuses = append(statuses, msg) },
		func(DiscoveredDevice) {},
		func(int, int) {},
	)
	if failure == "" {
		t.Error("a scan with no detectable subnet must explain itself on done")
	}
	if len(devices) != 0 {
		t.Errorf("expected no devices, got %+v", devices)
	}
	if len(statuses) != 0 {
		t.Errorf("the reason must ride on done, not on a status event: %q", statuses)
	}

	// And the other half of the same fallback: a detected subnet has to actually
	// reach the probe, or auto-detect quietly scans nothing and reports "no devices
	// found" for a LAN full of them. Loopback, so no packet leaves the machine.
	withDetectedSubnets(t, []string{loopbackSubnet})
	_, failure = tcpScan(context.Background(), nil,
		func(msg string) { statuses = append(statuses, msg) },
		func(DiscoveredDevice) {},
		func(int, int) {},
	)
	if failure != "" {
		t.Errorf("failure = %q, want none once a subnet was detected", failure)
	}
	if len(statuses) != 1 || !strings.Contains(statuses[0], loopbackSubnet+".0/24") {
		t.Errorf("statuses = %q, want one naming the detected subnet", statuses)
	}
}

// withDetectedSubnets substitutes subnet auto-detection for the test's duration.
func withDetectedSubnets(t *testing.T, subnets []string) {
	t.Helper()
	saved := detectSubnets
	detectSubnets = func() []string { return subnets }
	t.Cleanup(func() { detectSubnets = saved })
}

// A saved URL change has to reach a device that is already showing the old
// page. monitorDevices skips anything it believes is casting, and an always-on
// dashboard never drops the cast by itself, so without this the edit applied
// only after a restart of the service and the save looked like a no-op.
func TestLastCastURLTracksOnlyWhatWeCast(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	if _, ok := lastCastURL(dev); ok {
		t.Error("a device we have never cast to must not claim a known URL")
	}
	// Observing a device playing says nothing about what put it there — this is
	// the state after a restart, and re-casting on that guess would needlessly
	// restart a dashboard that was already correct.
	observeCastState(dev, true, "84912283", time.Now())
	if _, ok := lastCastURL(dev); ok {
		t.Error("an observation must not be mistaken for a cast of ours")
	}

	setCastState(dev, true, "http://old/")
	if u, ok := lastCastURL(dev); !ok || u != "http://old/" {
		t.Errorf("lastCastURL = (%q, %v), want the URL we cast", u, ok)
	}
	// An observation of the device still playing must not disturb it: that poll
	// is how the monitor confirms our page is up, not a new claim about its URL.
	observeCastState(dev, true, "84912283", time.Now())
	if u, ok := lastCastURL(dev); !ok || u != "http://old/" {
		t.Errorf("lastCastURL after an observation = (%q, %v), want it unchanged", u, ok)
	}

	// Nothing of ours is on screen after a stop, or after a failure.
	setCastState(dev, false, "")
	if _, ok := lastCastURL(dev); ok {
		t.Error("a stop left a recorded URL behind")
	}
	setCastState(dev, true, "http://old/")
	setCastError(dev, "Chromecast not found")
	if _, ok := lastCastURL(dev); ok {
		t.Error("a failed cast left a recorded URL behind")
	}

	// An empty URL is "we do not know", not a URL. Recording it would compare
	// unequal to every configured URL and re-cast the device on every tick.
	setCastState(dev, true, "")
	if _, ok := lastCastURL(dev); ok {
		t.Error("an empty URL was recorded as a known one")
	}
}

func TestCattFailureAlwaysExplains(t *testing.T) {
	if got := cattFailure(errors.New("signal: killed"), "  \n "); got != "signal: killed" {
		t.Errorf("empty output should fall back to the exec error, got %q", got)
	}
	if got := cattFailure(errors.New("exit status 1"), "\nDevice not found.\n"); got != "Device not found." {
		t.Errorf("catt output should win, got %q", got)
	}
	// A traceback on the pipe is repeated in every /api/devices/status response
	// for as long as the failure stands, and rendered into a one-line card.
	got := cattFailure(errors.New("exit status 1"), strings.Repeat("é", maxStatusTextLen*3))
	if n := utf8.RuneCountInString(got); n != maxStatusTextLen+1 {
		t.Errorf("long catt output not bounded: %d runes", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("bounded message is not valid UTF-8: %q", got)
	}
}

func TestLocalSubnetsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range localSubnets() {
		if seen[s] {
			t.Errorf("duplicate subnet %q returned", s)
		}
		seen[s] = true
	}
}

// Auto-detect must never propose a subnet outside RFC1918. A scan connects to
// all 254 hosts in the /24, so on a machine with a routable address (a VPS, or
// an ISP handing out public IPv4 without NAT) this port-scanned strangers.
// Asserted on the real interface list, which is what the scanner actually uses.
func TestLocalSubnetsArePrivate(t *testing.T) {
	for _, base := range localSubnets() {
		// .1 stands in for the /24: IsPrivate is decided by the leading octets,
		// all of which are present in the base the scanner returns.
		ip := net.ParseIP(base + ".1")
		if ip == nil {
			t.Errorf("subnet %q is not a parseable /24 base", base)
			continue
		}
		if !ip.IsPrivate() {
			t.Errorf("auto-detect offered non-private subnet %q.0/24", base)
		}
	}
}

// The fallback in localSubnets exists to relax the interface-*name* heuristic.
// It must not also relax the private-address check, or it would re-admit public
// subnets on exactly the hosts the check is there to protect — one with nothing
// but a routable address is where filtering leaves the list empty.
func TestCollectSubnetsFiltersPublicEvenUnfiltered(t *testing.T) {
	for _, base := range collectSubnets(false) {
		ip := net.ParseIP(base + ".1")
		if ip != nil && !ip.IsPrivate() {
			t.Errorf("unfiltered collectSubnets returned public subnet %q.0/24", base)
		}
	}
}

func TestIsVirtualIface(t *testing.T) {
	virtual := []string{
		"docker0", "br-1a2b3c", "veth9f2", "virbr0", "utun3", "tailscale0",
		"vboxnet0", "vmnet8", "lxcbr0", "lxdbr0", "podman0", "cali1a2b3c",
	}
	for _, n := range virtual {
		if !isVirtualIface(n) {
			t.Errorf("%q should be treated as virtual", n)
		}
	}
	// br0 is a real LAN bridge on libvirt/Proxmox hosts, so the filter has to stay
	// narrower than a bare "br" prefix or it hides the only subnet that matters.
	for _, n := range []string{"eth0", "en0", "wlan0", "enp3s0", "eno1", "br0", "bond0"} {
		if isVirtualIface(n) {
			t.Errorf("%q should not be treated as virtual", n)
		}
	}
}

// --- shared helpers for the handler tests -----------------------------------

// withConfig installs c as the live config and restores the previous one.
func withConfig(t *testing.T, c Config) {
	t.Helper()
	cfgMu.Lock()
	saved := cfg
	cfg = c
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = saved
		cfgMu.Unlock()
	})
}

// withConfigPath points cfgPath at a fresh temp file and restores it after.
func withConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	saved := cfgPath
	cfgPath = path
	t.Cleanup(func() { cfgPath = saved })
	return path
}

// sseEvents pulls the JSON payloads out of a captured text/event-stream body.
func sseEvents(t *testing.T, body string) []ScanEvent {
	t.Helper()
	var events []ScanEvent
	for _, frame := range strings.Split(body, "\n\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok {
			continue
		}
		var evt ScanEvent
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			t.Fatalf("unparseable SSE frame %q: %v", data, err)
		}
		events = append(events, evt)
	}
	return events
}

// --- stand-ins for the two subprocesses -------------------------------------

// fakeCatt records what the code under test asked catt to do, and answers with a
// canned result. Installed over the castSite/stopCast seams by withFakeCatt.
type fakeCatt struct {
	mu    sync.Mutex
	casts []castCall
	stops []string
	out   string
	err   error
}

type castCall struct {
	key string // deviceKey, so a name/host mix-up cannot pass unnoticed
	url string
}

func (f *fakeCatt) cast(_ context.Context, dev DeviceConfig, url string) (string, error) {
	f.mu.Lock()
	f.casts = append(f.casts, castCall{deviceKey(dev), url})
	f.mu.Unlock()
	return f.out, f.err
}

func (f *fakeCatt) stop(_ context.Context, dev DeviceConfig) (string, error) {
	f.mu.Lock()
	f.stops = append(f.stops, deviceKey(dev))
	f.mu.Unlock()
	return f.out, f.err
}

func (f *fakeCatt) recorded() ([]castCall, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]castCall{}, f.casts...), append([]string{}, f.stops...)
}

// castedURLs is the sequence of URLs cast so far, which is what the
// duplicate-row and stale-URL regressions are visible in.
func (f *fakeCatt) castedURLs() []string {
	casts, _ := f.recorded()
	urls := make([]string, len(casts))
	for i, c := range casts {
		urls[i] = c.url
	}
	return urls
}

func withFakeCatt(t *testing.T) *fakeCatt {
	t.Helper()
	f := &fakeCatt{}
	savedCast, savedStop := castSite, stopCast
	castSite, stopCast = f.cast, f.stop
	t.Cleanup(func() { castSite, stopCast = savedCast, savedStop })
	return f
}

func withProbe(t *testing.T, fn func(context.Context, DeviceConfig) DeviceStatus) {
	t.Helper()
	saved := probeDevice
	probeDevice = fn
	t.Cleanup(func() { probeDevice = saved })
}

// fakeProbe stands in for getPychromecastStatus, including the part that matters
// most: it *applies* the observation. The monitor's decisions read castStates
// rather than the returned struct, so a probe that only returned a state would
// exercise none of the ordering these tests are about. An error short-circuits
// both, exactly as an unreachable device does.
func fakeProbe(playing bool, appID, errMsg string, foreign bool) func(context.Context, DeviceConfig) DeviceStatus {
	return func(_ context.Context, dev DeviceConfig) DeviceStatus {
		ds := DeviceStatus{Name: dev.Name, Host: dev.Host, State: "unknown"}
		if errMsg != "" {
			ds.Error = errMsg
			return ds
		}
		observeCastState(dev, playing, appID, time.Now())
		ds.State, ds.Foreign = "Idle", foreign
		if playing {
			ds.State = "Playing"
		}
		return ds
	}
}

// echoProbe reports the device as showing whatever we last put on it, which is
// what an always-on dashboard looks like from the second tick onwards. Used for
// the multi-tick tests, where a fixed answer would beg the question.
func echoProbe(_ context.Context, dev DeviceConfig) DeviceStatus {
	playing := isCasting(dev)
	observeCastState(dev, playing, "app-ours", time.Now())
	ds := DeviceStatus{Name: dev.Name, Host: dev.Host, State: "Idle"}
	if playing {
		ds.State = "Playing"
	}
	return ds
}

// --- monitorDevices ---------------------------------------------------------

// The tick that does the actual work: probe, decide, cast. Everything below
// drives it through the seams above, because the decisions it makes are all
// invisible from the outside — a device the monitor has silently stopped
// managing looks exactly like one it is managing well.

func TestMonitorDevicesCastsAnIdleDevice(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, fakeProbe(false, "", "", false))
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())

	casts, _ := f.recorded()
	if len(casts) != 1 || casts[0].key != "host:1.2.3.4" || casts[0].url != "http://dash/" {
		t.Fatalf("casts = %+v, want one cast of the default URL to the device's IP", casts)
	}
	if !isCasting(dev) {
		t.Error("a successful cast was not recorded as playing")
	}
	// Recorded so that a later URL edit can be noticed; see lastCastURL.
	if u, ok := lastCastURL(dev); !ok || u != "http://dash/" {
		t.Errorf("lastCastURL = (%q, %v), want the URL we just cast", u, ok)
	}
	if got := castError(dev); got != "" {
		t.Errorf("a successful cast left an error: %q", got)
	}
}

// A device's own URL wins over the default, and the monitor must cast the same
// URL it records — otherwise the next tick judges its own cast stale and restarts
// the dashboard on every single tick.
func TestMonitorDevicesUsesTheDeviceURLAndThenLeavesItAlone(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, echoProbe)
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", URL: "http://own/", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	for i := 0; i < 4; i++ {
		monitorDevices(context.Background())
	}
	if got := f.castedURLs(); len(got) != 1 || got[0] != "http://own/" {
		t.Errorf("casts across four ticks = %v, want exactly one of the device's own URL", got)
	}
}

// The saved-URL-change regression: an always-on dashboard never drops the cast by
// itself, so without the recorded-URL comparison a new default_url applied only
// after a restart of the service and the save looked like a no-op.
func TestMonitorDevicesReCastsWhenTheURLChanges(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, echoProbe)
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://old/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())
	cfgMu.Lock()
	cfg.DefaultURL = "http://new/"
	cfgMu.Unlock()
	monitorDevices(context.Background())
	monitorDevices(context.Background())

	if got := f.castedURLs(); len(got) != 2 || got[0] != "http://old/" || got[1] != "http://new/" {
		t.Errorf("casts = %v, want the old URL then the new one, and no third cast", got)
	}
}

// A device already playing when the process starts gets no castURLs entry, so
// nothing may be concluded about whether its page is current — re-casting on that
// guess would restart a dashboard that was already correct.
func TestMonitorDevicesLeavesADeviceItDidNotCastAlone(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, fakeProbe(true, "app-ours", "", false))
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())
	if got := f.castedURLs(); len(got) != 0 {
		t.Errorf("casts = %v, want none: the device was already playing something we did not put there", got)
	}
}

// Somebody is watching something. Leave it alone unless takeover says otherwise —
// the whole point of noticing a foreign app is to not yank the TV out from under
// whoever is using it.
func TestMonitorDevicesRespectsTheTakeoverFlag(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		resetCastState()
		f := withFakeCatt(t)
		withProbe(t, fakeProbe(true, "app-theirs", "", true))
		dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true, Takeover: takeover}
		withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

		monitorDevices(context.Background())

		got := f.castedURLs()
		if takeover && len(got) != 1 {
			t.Errorf("takeover on: casts = %v, want the device reclaimed", got)
		}
		if !takeover && len(got) != 0 {
			t.Errorf("takeover off: casts = %v, want the other app left alone", got)
		}
	}
}

// A foreign app sets the cast state too — isCasting means "playing something",
// not "playing ours" — so it must not short-circuit an approved takeover.
func TestMonitorDevicesTakesOverADeviceItBelievesIsCasting(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true, Takeover: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	// Our own cast is up and current, so the stale-URL check cannot be what
	// triggers the re-cast; only the foreign app may.
	withProbe(t, echoProbe)
	monitorDevices(context.Background())
	withProbe(t, fakeProbe(true, "app-theirs", "", true))
	monitorDevices(context.Background())

	if got := f.castedURLs(); len(got) != 2 {
		t.Errorf("casts = %v, want a second one to reclaim the device", got)
	}
}

// A probe error means the device is off or unreachable, so a cast cannot succeed.
// Skipping matters: catt would spend its full 30s timeout failing, serially, and
// a couple of powered-off TVs kept the loop permanently busy.
func TestMonitorDevicesSkipsAnUnreachableDevice(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, fakeProbe(false, "", "1.2.3.4 is not reachable on port 8009", false))
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())

	if got := f.castedURLs(); len(got) != 0 {
		t.Errorf("casts = %v, want none for an unreachable device", got)
	}
	// The live probe is what reports this on the card; a standing unreachability
	// must not also be recorded as a cast failure, which would bump castActions
	// every tick and suppress every observation of the device.
	if got := castError(dev); got != "" {
		t.Errorf("castError = %q, want the skip left it clear", got)
	}
}

// A cast that fails has to leave a reason behind: /api/devices/status merges it
// onto the card, and without it the device just quietly stays idle.
func TestMonitorDevicesRecordsAndRetriesAFailedCast(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	f.err, f.out = errors.New("exit status 1"), "Device not found.\n"
	withProbe(t, echoProbe)
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())
	if got := castError(dev); got != "Device not found." {
		t.Errorf("castError = %q, want catt's own explanation", got)
	}
	if isCasting(dev) {
		t.Error("a failed cast left the device marked as playing")
	}
	if _, ok := lastCastURL(dev); ok {
		t.Error("a failed cast recorded a URL as being on screen")
	}
	// And the next tick tries again rather than giving up on it.
	monitorDevices(context.Background())
	if got := f.castedURLs(); len(got) != 2 {
		t.Errorf("casts = %v, want the failure retried on the next tick", got)
	}
}

// The retry above is the easy case: the device read idle afterwards, so the
// ordinary "idle, so cast" rule covered it. The hard one is a cast that fails and
// leaves the *previous* page on screen, which is what actually happens to an
// always-on dashboard — catt's cast_site fails, the page it was replacing is still
// up, and the next probe duly reports the device as playing.
//
// Everything then conspired to make the monitor walk away from it for good:
// setCastError drops the castURLs entry, so nothing says the page is stale; the
// probe's playing observation cleared the cast error, so nothing says our last
// attempt failed; and "playing, not foreign, not stale" is the skip. The device sat
// on the page we failed to replace until the process restarted, with a card reading
// a clean "Playing" and no reason recorded anywhere.
func TestMonitorDevicesRetriesACastThatFailedOverAPageStillOnScreen(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	// The probe answers with what is on screen, which once the dashboard is up
	// never changes again by itself — not even when a cast fails, which is the
	// whole point. Deliberately not echoProbe: that reports our *cast state*, so it
	// would follow setCastError back to "idle" and the ordinary idle-device rule
	// would cover the retry, leaving the bug invisible.
	onScreen := false
	withProbe(t, func(_ context.Context, dev DeviceConfig) DeviceStatus {
		observeCastState(dev, onScreen, "app-ours", time.Now())
		ds := DeviceStatus{Name: dev.Name, Host: dev.Host, State: "Idle"}
		if onScreen {
			ds.State = "Playing"
		}
		return ds
	})

	// Get the dashboard up the ordinary way, so there is a recorded URL.
	monitorDevices(context.Background())
	if got := f.castedURLs(); len(got) != 1 {
		t.Fatalf("casts = %v, want the initial cast", got)
	}
	onScreen = true

	// Now the URL changes, so the monitor tries — and the cast fails, leaving the
	// page it was replacing still up.
	f.err, f.out = errors.New("exit status 1"), "Failed to connect.\n"
	cfgMu.Lock()
	cfg.DefaultURL = "http://new/"
	cfgMu.Unlock()
	monitorDevices(context.Background())
	if got := f.castedURLs(); len(got) != 2 || got[1] != "http://new/" {
		t.Fatalf("casts = %v, want the URL change attempted once", got)
	}

	// The reason has to survive the probe that follows it, or nothing is left to
	// say the device needs another attempt.
	for tick := 3; tick <= 5; tick++ {
		monitorDevices(context.Background())
		if got := castError(dev); got != "Failed to connect." {
			t.Fatalf("tick %d: castError = %q, want the failure still recorded", tick, got)
		}
		if got := f.castedURLs(); len(got) != tick {
			t.Fatalf("tick %d: casts = %v, want one attempt per tick", tick, got)
		}
	}

	// And once catt works again the retry sticks, the reason is dropped, and the
	// monitor goes quiet instead of re-casting a device that is now correct.
	f.err, f.out = nil, ""
	monitorDevices(context.Background())
	if got := castError(dev); got != "" {
		t.Errorf("a successful retry left the error behind: %q", got)
	}
	before := len(f.castedURLs())
	monitorDevices(context.Background())
	monitorDevices(context.Background())
	if got := f.castedURLs(); len(got) != before {
		t.Errorf("casts = %v, want no further casts once the retry succeeded", got)
	}
}

// Two rows for one device share a single deviceKey, and with it a single
// castURLs entry. With a different URL on each row the monitor cast row one's
// page, judged it stale against row two's, cast that, and repeated the pair on
// every tick: an always-on dashboard restarting itself forever.
func TestMonitorDevicesDoesNotAlternateBetweenDuplicateRows(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, echoProbe)
	withConfig(t, Config{CheckInterval: 60, Devices: []DeviceConfig{
		{Name: "First", Host: "1.2.3.4", URL: "http://a/", AutoCast: true},
		{Name: "Second", Host: "1.2.3.4", URL: "http://b/", AutoCast: true},
	}})

	for i := 0; i < 5; i++ {
		monitorDevices(context.Background())
	}
	if got := f.castedURLs(); len(got) != 1 || got[0] != "http://a/" {
		t.Errorf("casts across five ticks = %v, want a single cast of the first row's URL", got)
	}
}

// A device with no IP cannot be probed at all: `catt status` describes the media
// session and a web page cast has none. It is cast blind, once, and then left
// alone — guessing "idle" from that output would restart the dashboard every tick.
func TestMonitorDevicesCastsBlindWithoutAnIP(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, func(_ context.Context, dev DeviceConfig) DeviceStatus {
		t.Errorf("a device with no IP must not be probed: %+v", dev)
		return DeviceStatus{}
	})
	dev := DeviceConfig{Name: "Lounge", AutoCast: true}
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{dev}})

	monitorDevices(context.Background())
	monitorDevices(context.Background())

	casts, _ := f.recorded()
	if len(casts) != 1 || casts[0].key != "name:Lounge" {
		t.Errorf("casts = %+v, want exactly one, keyed by name", casts)
	}
}

// The skips autoCastTargets makes are asserted directly elsewhere; this is the
// end-to-end version, so that a future change which moves a decision back into
// the loop cannot quietly start casting to a row that should be inert.
func TestMonitorDevicesActsOnlyOnAutoCastTargets(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	withProbe(t, fakeProbe(false, "", "", false))
	withConfig(t, Config{CheckInterval: 60, DefaultURL: "http://dash/", Devices: []DeviceConfig{
		{Name: "Off", Host: "1.1.1.1"},
		{AutoCast: true},
		{Name: "BadURL", Host: "2.2.2.2", URL: "--version", AutoCast: true},
		{Name: "Fine", Host: "3.3.3.3", AutoCast: true},
	}})

	monitorDevices(context.Background())

	casts, _ := f.recorded()
	if len(casts) != 1 || casts[0].key != "host:3.3.3.3" {
		t.Errorf("casts = %+v, want only the one usable row", casts)
	}
}

// --- /api/devices/cast and /stop: the part that runs after the response ------

// The handlers answer immediately and run catt in a goroutine, so a failure
// cannot ride on the HTTP response — it has to land in castErrors and be merged
// into the next status poll.
func TestCastHandlerRecordsItsOutcomeAfterResponding(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	f.err, f.out = errors.New("exit status 1"), "Device not found.\n"

	rec := httptest.NewRecorder()
	handleCast(rec, httptest.NewRequest(http.MethodPost, "/api/devices/cast",
		strings.NewReader(`{"name":" Lounge ","host":" 1.2.3.4 ","url":" http://dash/ "}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("cast = %d (%s)", rec.Code, rec.Body)
	}

	// Trimmed before the key is built: an untrimmed name filed the error under a
	// key nothing else ever reads, so it never reached the device card.
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	waitFor(t, "the cast failure to be recorded", func() bool { return castError(dev) != "" })
	if got := castError(dev); got != "Device not found." {
		t.Errorf("castError = %q, want catt's explanation filed under the trimmed key", got)
	}
	casts, _ := f.recorded()
	if len(casts) != 1 || casts[0].url != "http://dash/" {
		t.Errorf("casts = %+v, want one cast of the trimmed URL", casts)
	}
}

func TestCastAndStopHandlersRecordSuccess(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	dev := DeviceConfig{Host: "1.2.3.4"}

	rec := httptest.NewRecorder()
	handleCast(rec, httptest.NewRequest(http.MethodPost, "/api/devices/cast",
		strings.NewReader(`{"host":"1.2.3.4","url":"http://dash/"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("cast = %d (%s)", rec.Code, rec.Body)
	}
	waitFor(t, "the cast to be recorded", func() bool { return isCasting(dev) })
	if u, ok := lastCastURL(dev); !ok || u != "http://dash/" {
		t.Errorf("lastCastURL = (%q, %v), want the manually cast URL", u, ok)
	}

	rec = httptest.NewRecorder()
	handleStop(rec, httptest.NewRequest(http.MethodPost, "/api/devices/stop",
		strings.NewReader(`{"host":"1.2.3.4"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d (%s)", rec.Code, rec.Body)
	}
	waitFor(t, "the stop to be recorded", func() bool { return !isCasting(dev) })
	if _, ok := lastCastURL(dev); ok {
		t.Error("a stop left a recorded URL behind — nothing of ours is on screen")
	}
	_, stops := f.recorded()
	if len(stops) != 1 || stops[0] != "host:1.2.3.4" {
		t.Errorf("stops = %v, want one, keyed by host", stops)
	}
}

// A stop fails the same way a cast does — the response has already gone out, so
// the only place the reason can land is castErrors, to be merged into the next
// status. Nothing else reports it: the card would otherwise read a clean "Idle"
// for a device still showing whatever it was showing.
func TestStopHandlerRecordsAFailure(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)
	f.err, f.out = errors.New("exit status 1"), "Failed to connect.\n"
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	setCastState(dev, true, "http://dash/")

	rec := httptest.NewRecorder()
	handleStop(rec, httptest.NewRequest(http.MethodPost, "/api/devices/stop",
		strings.NewReader(`{"name":" Lounge ","host":" 1.2.3.4 "}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d (%s)", rec.Code, rec.Body)
	}
	waitFor(t, "the stop failure to be recorded", func() bool { return castError(dev) != "" })
	if got := castError(dev); got != "Failed to connect." {
		t.Errorf("castError = %q, want catt's explanation filed under the trimmed key", got)
	}
	// Nothing of ours is known to be on screen after a stop we could not confirm,
	// so the recorded URL goes with it: keeping it would let the monitor conclude
	// the current page is the one it asked for.
	if _, ok := lastCastURL(dev); ok {
		t.Error("a failed stop left a recorded URL behind")
	}
}

// waitFor polls until cond holds. /api/devices/cast and /stop answer *before*
// catt has run, so their outcome is recorded after the response — and recording
// it is the last thing those goroutines do, which makes waiting for the outcome
// the only signal that also proves the goroutine has finished with the seams
// t.Cleanup is about to restore under it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- duplicate config rows --------------------------------------------------

// Two rows can name one device — the same IP typed twice, or an IP filled into a
// row that another row already carries. Everything in castStates is keyed by the
// single deviceKey they share, castURLs included, so with a different URL on each
// row the monitor cast row one's page, judged it stale against row two's, cast
// that, and repeated the pair on every tick: an always-on dashboard restarting
// itself forever.
func TestDuplicateDeviceKeys(t *testing.T) {
	devices := []DeviceConfig{
		{Name: "Lounge", Host: "1.2.3.4"},
		{Name: "Renamed", Host: "1.2.3.4"}, // same IP, so the same key
		{Name: "Kitchen", Host: "5.6.7.8"},
		{Name: "Speaker"}, // no IP, keyed by name
		{Name: "Speaker"},
	}
	dup := duplicateDeviceKeys(devices)
	if !dup["host:1.2.3.4"] {
		t.Error("two rows with the same IP were not reported as duplicates")
	}
	if !dup["name:Speaker"] {
		t.Error("two host-less rows with the same name were not reported as duplicates")
	}
	if dup["host:5.6.7.8"] {
		t.Error("a device that appears once was reported as a duplicate")
	}
	// A name and an IP that happen to read the same must not collide — the keys
	// carry a prefix precisely so that they cannot.
	dup = duplicateDeviceKeys([]DeviceConfig{{Name: "1.2.3.4"}, {Host: "1.2.3.4"}})
	if len(dup) != 0 {
		t.Errorf("name and host namespaces collided: %v", dup)
	}
	if len(duplicateDeviceKeys(nil)) != 0 {
		t.Error("an empty list has no duplicates")
	}
}

func TestConfigWarningFlagsDuplicateEntries(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	if got := configWarning(dev, "http://dash/", true); got == "" {
		t.Error("a duplicated device entry should be flagged")
	}
	// Sharing a key costs something whether or not auto-cast is on: both rows
	// report the same state and the same cast error, so a failure on one is
	// rendered on both.
	//
	// But it is a *different* advisory. Naming which row is cast states a
	// consequence that cannot apply to a row with auto-cast off, and reads as
	// though ticking the box would be pointless; what is left is the shared status.
	manual := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	got := configWarning(manual, "", true)
	if got == "" {
		t.Error("a duplicate should be flagged even without auto-cast")
	}
	if strings.Contains(got, "auto-cast") {
		t.Errorf("warning = %q, want no auto-cast claim on a row that has it switched off", got)
	}
	if !strings.Contains(got, "same device") || !strings.Contains(got, "same status") {
		t.Errorf("warning = %q, want the shared-status advisory", got)
	}
	// The auto-cast row keeps the clause that is actionable for it: which of the
	// two rows the monitor is actually casting from.
	if got := configWarning(dev, "http://dash/", true); !strings.Contains(got, "only the first with auto-cast enabled is cast") {
		t.Errorf("warning = %q, want it to say which row is acted on", got)
	}
	// And it has to name that row the way autoCastTargets picks it. A row with the
	// box unticked is skipped *before* the deviceKey is claimed, so ticking it on
	// the second row of a pair only makes the second row the one being cast — which
	// is the row this advisory is rendered on. Saying "the first" there told the
	// reader the row in front of them was inert when it was the only one working.
	pair := []DeviceConfig{
		{Name: "Manual", Host: "1.2.3.4"},
		{Name: "Managed", Host: "1.2.3.4", URL: "http://dash/", AutoCast: true},
	}
	targets := autoCastTargets(pair, "")
	if len(targets) != 1 || targets[0].Name != "Managed" {
		t.Fatalf("targets = %+v, want the second row, which is the first with auto-cast on", targets)
	}
	got = configWarning(pair[1], pair[1].URL, true)
	if strings.Contains(got, "only the first is") {
		t.Errorf("warning = %q, but this *is* the row being cast — it is only the second in the list", got)
	}
	if !strings.Contains(got, "only the first with auto-cast enabled is cast") {
		t.Errorf("warning = %q, want the advisory to name the row autoCastTargets actually picks", got)
	}
	// Still nothing to pile on for a row with no identifier at all: two blank
	// rows share the key "name:", and getLiveStatus already explains each.
	if got := configWarning(DeviceConfig{}, "", true); got != "" {
		t.Errorf("an unaddressed device is already reported, got %q", got)
	}
}

// autoCastTargets claims a deviceKey for the first matching row before it looks
// at that row's URL, so a duplicated pair whose *first* row has the unusable URL
// is never cast at all. Leading with the duplicate advisory then told the user
// that the first row was the one being cast — hiding the one problem they could
// actually fix — so for an auto-cast row the URL comes first.
func TestConfigWarningReportsAnUnusableURLAheadOfTheDuplicateNote(t *testing.T) {
	first := DeviceConfig{Name: "First", Host: "1.2.3.4", URL: "not-a-url", AutoCast: true}
	if got := configWarning(first, first.URL, true); !strings.Contains(got, "URL") {
		t.Errorf("warning = %q, want the unusable URL named", got)
	}
	noURL := DeviceConfig{Name: "First", Host: "1.2.3.4", AutoCast: true}
	if got := configWarning(noURL, "", true); !strings.Contains(got, "URL") {
		t.Errorf("warning = %q, want the missing URL named", got)
	}
	// The row that is genuinely only inert because another row won the key still
	// says so, which is what points at the row above.
	second := DeviceConfig{Name: "Second", Host: "1.2.3.4", URL: "http://b/", AutoCast: true}
	if got := configWarning(second, second.URL, true); !strings.Contains(got, "same device") {
		t.Errorf("warning = %q, want the duplicate advisory", got)
	}
	// A no-IP auto-cast row that is *also* duplicated hears about the duplicate
	// first: unlike the URL problems it is the reason nothing happens at all.
	blind := DeviceConfig{Name: "Speaker", URL: "http://b/", AutoCast: true}
	if got := configWarning(blind, blind.URL, true); !strings.Contains(got, "same device") {
		t.Errorf("warning = %q, want the duplicate advisory ahead of the missing IP", got)
	}
	if got := configWarning(blind, blind.URL, false); !strings.Contains(got, "No IP") {
		t.Errorf("warning = %q, want the missing IP once it is not duplicated", got)
	}
}

// --- autoCastTargets: every pre-network skip the monitor makes --------------

func TestAutoCastTargetsSkips(t *testing.T) {
	const def = "http://default/"
	devices := []DeviceConfig{
		{Name: "Off", Host: "1.1.1.1"},                   // auto-cast disabled
		{Host: "2.2.2.2", AutoCast: true},                // uses the default URL
		{AutoCast: true},                                 // no identifier at all
		{Name: "NoURL", Host: "3.3.3.3", AutoCast: true}, // default covers it
		{Name: "BadURL", Host: "4.4.4.4", URL: "--version", AutoCast: true},
		{Name: "Own", Host: "5.5.5.5", URL: "http://own/", AutoCast: true},
	}
	got := autoCastTargets(devices, def)
	want := []string{"2.2.2.2", "3.3.3.3", "5.5.5.5"}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want hosts %v", got, want)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("target %d = %q, want %q (config order must be preserved)", i, got[i].Host, h)
		}
	}

	// With no default URL, the two rows that relied on it drop out as well.
	got = autoCastTargets(devices, "")
	if len(got) != 1 || got[0].Host != "5.5.5.5" {
		t.Errorf("with no default URL, targets = %+v, want only the row with its own URL", got)
	}
}

// Only the first row of a duplicated pair is acted on, and always the same one:
// alternating between them is what restarted the dashboard on every tick.
func TestAutoCastTargetsActsOnOneRowPerDevice(t *testing.T) {
	devices := []DeviceConfig{
		{Name: "First", Host: "1.2.3.4", URL: "http://a/", AutoCast: true},
		{Name: "Second", Host: "1.2.3.4", URL: "http://b/", AutoCast: true},
	}
	for i := 0; i < 3; i++ {
		got := autoCastTargets(devices, "")
		if len(got) != 1 {
			t.Fatalf("run %d: targets = %+v, want exactly one row for one device", i, got)
		}
		if got[0].URL != "http://a/" {
			t.Errorf("run %d: acted on %q, want the first row every time", i, got[0].URL)
		}
	}
	// A duplicate keyed by name behaves the same way.
	got := autoCastTargets([]DeviceConfig{
		{Name: "Speaker", URL: "http://a/", AutoCast: true},
		{Name: "Speaker", URL: "http://b/", AutoCast: true},
	}, "")
	if len(got) != 1 || got[0].URL != "http://a/" {
		t.Errorf("name-keyed duplicate = %+v, want only the first row", got)
	}
}

// The whole point of configWarning is that no skip is silent: a skipped device's
// card reads a plain "Idle", identical to one auto-cast is happily managing. So
// every addressable row the monitor drops must come with an explanation. The
// converse does not hold — a device with no IP is cast to blind and warned about
// anyway — so this only asserts the direction that hides a problem.
func TestEverySkippedDeviceIsExplained(t *testing.T) {
	const def = "http://default/"
	devices := []DeviceConfig{
		{Name: "Off", Host: "1.1.1.1"},
		{Name: "NoIP", URL: "http://a/", AutoCast: true},
		{Name: "BadURL", Host: "2.2.2.2", URL: "ftp://x/", AutoCast: true},
		{Name: "Dup", Host: "3.3.3.3", URL: "http://a/", AutoCast: true},
		{Name: "Dup2", Host: "3.3.3.3", URL: "http://b/", AutoCast: true},
		{Name: "Fine", Host: "4.4.4.4", AutoCast: true},
		{AutoCast: true},
	}
	for _, defaultURL := range []string{def, ""} {
		targets := map[string]bool{}
		for _, d := range autoCastTargets(devices, defaultURL) {
			targets[deviceKey(d)+"|"+d.Name] = true
		}
		dups := duplicateDeviceKeys(devices)
		for _, d := range devices {
			if targets[deviceKey(d)+"|"+d.Name] {
				// Acted on, so nothing to check: a device the monitor is casting to
				// can still carry an advisory (it is cast blind without an IP, and the
				// first row of a duplicated pair is the one that gets cast).
				continue
			}
			if !d.AutoCast || (d.Name == "" && d.Host == "") {
				continue // not monitored, or already reported by getLiveStatus
			}
			if configWarning(d, effectiveURL(d, defaultURL), dups[deviceKey(d)]) == "" {
				t.Errorf("default %q: device %+v was skipped with no explanation", defaultURL, d)
			}
		}
	}
}

// --- GET/POST /api/config ---------------------------------------------------

func TestHandleConfigGET(t *testing.T) {
	withConfig(t, Config{CheckInterval: 45, DefaultURL: "http://dash/", Devices: []DeviceConfig{}})

	rec := httptest.NewRecorder()
	handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	// devices must marshal as [] and never null: the UI reads config.devices.length
	// straight out of this response.
	if !strings.Contains(rec.Body.String(), `"devices":[]`) {
		t.Errorf("empty device list did not marshal as []: %s", rec.Body)
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		handleConfig(rec, httptest.NewRequest(method, "/api/config", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/config = %d, want 405", method, rec.Code)
		}
	}
}

// GET must not hand out the live slice: the caller would then be reading the
// device list while a POST reslices it.
func TestHandleConfigGETCopiesTheDeviceSlice(t *testing.T) {
	withConfig(t, Config{CheckInterval: 60, Devices: []DeviceConfig{{Name: "Lounge"}}})

	rec := httptest.NewRecorder()
	handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	got.Devices[0].Name = "mutated"
	cfgMu.RLock()
	live := cfg.Devices[0].Name
	cfgMu.RUnlock()
	if live != "Lounge" {
		t.Errorf("live config was mutated through the response: %q", live)
	}
}

// An over-long body is a different failure from a malformed one. MaxBytesReader
// surfaces it as an ordinary decode error, so it used to be reported as "400 —
// your JSON is bad", sending the caller after a syntax error that is not there.
func TestOversizedBodiesAreRejectedAsTooLarge(t *testing.T) {
	withConfigPath(t)
	withConfig(t, Config{CheckInterval: 60, Devices: []DeviceConfig{}})

	big := `{"default_url":"` + strings.Repeat("x", 2<<20) + `"}`
	cases := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"config", "/api/config", handleConfig},
		{"cast", "/api/devices/cast", handleCast},
		{"stop", "/api/devices/stop", handleStop},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.handler(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(big)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: oversized body = %d, want 413", c.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "at most") {
			t.Errorf("%s: 413 body does not name the limit: %s", c.name, rec.Body)
		}
	}
}

// A rejected write must not be published: applying first left the monitor acting
// on a config the disk never received, so the user saw a 500 and a restart
// silently reverted the behaviour they thought they had lost.
func TestFailedSaveNeitherPublishesNorPrunes(t *testing.T) {
	// A path inside a directory that does not exist, so CreateTemp fails.
	saved := cfgPath
	cfgPath = filepath.Join(t.TempDir(), "missing-dir", "config.json")
	defer func() { cfgPath = saved }()

	kept := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	withConfig(t, Config{CheckInterval: 30, Devices: []DeviceConfig{kept}})
	resetCastState()
	setCastState(kept, true, "http://dash/")

	rec := httptest.NewRecorder()
	handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"check_interval":99,"devices":[]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable config = %d, want 500", rec.Code)
	}
	cfgMu.RLock()
	got := cfg
	cfgMu.RUnlock()
	if got.CheckInterval != 30 || len(got.Devices) != 1 {
		t.Errorf("a failed save was published anyway: %+v", got)
	}
	if !isCasting(kept) {
		t.Error("a failed save pruned the cast state of a device that is still configured")
	}
}

// A successful save is what prunes: devices can be renamed, re-addressed or
// deleted here, and their state would otherwise linger for the process lifetime.
func TestSuccessfulSavePrunesRemovedDevices(t *testing.T) {
	withConfigPath(t)
	gone := DeviceConfig{Name: "Gone", Host: "1.2.3.4"}
	stays := DeviceConfig{Name: "Stays", Host: "5.6.7.8"}
	withConfig(t, Config{CheckInterval: 30, Devices: []DeviceConfig{gone, stays}})
	resetCastState()
	setCastState(gone, true, "http://dash/")
	setCastState(stays, true, "http://dash/")

	rec := httptest.NewRecorder()
	handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"check_interval":30,"devices":[{"name":"Stays","host":"5.6.7.8"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d (%s)", rec.Code, rec.Body)
	}
	if isCasting(gone) {
		t.Error("a removed device kept its cast state")
	}
	if !isCasting(stays) {
		t.Error("a device that is still configured lost its cast state")
	}
}

// The interval is re-read only when the monitor is woken, and POST must never
// block on that wake-up: the monitor can be mid-cast and tens of seconds away
// from looking at the channel.
func TestSavingSignalsTheMonitorWithoutBlocking(t *testing.T) {
	withConfigPath(t)
	withConfig(t, Config{CheckInterval: 60, Devices: []DeviceConfig{}})
	// Start with the signal already pending, which is the case that would block
	// on an unbuffered or unguarded send.
	select {
	case configChanged <- struct{}{}:
	default:
	}
	t.Cleanup(func() {
		select {
		case <-configChanged:
		default:
		}
	})

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config",
			strings.NewReader(`{"check_interval":20,"devices":[]}`)))
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("save = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("POST /api/config blocked waiting for the monitor to notice")
	}
}

// --- loadConfig -------------------------------------------------------------

func TestLoadConfig(t *testing.T) {
	path := withConfigPath(t)
	withConfig(t, Config{})

	// No file yet: usable defaults, and a non-nil device list.
	loadConfig()
	cfgMu.RLock()
	got := cfg
	cfgMu.RUnlock()
	if got.CheckInterval != 60 || got.Devices == nil || len(got.Devices) != 0 {
		t.Errorf("missing config = %+v, want defaults", got)
	}

	// A malformed file must not leave cfg half-populated with a mix of defaults
	// and file contents — it is decoded into a scratch value for that reason.
	if err := os.WriteFile(path, []byte(`{"check_interval": 30, "devices": [{"name":`), 0644); err != nil {
		t.Fatal(err)
	}
	loadConfig()
	cfgMu.RLock()
	got = cfg
	cfgMu.RUnlock()
	if got.CheckInterval != 60 || len(got.Devices) != 0 {
		t.Errorf("malformed config = %+v, want defaults only", got)
	}

	// A valid file is normalized on the way in, so what the monitor acts on and
	// what GET /api/config reports cannot disagree with the stored value.
	if err := os.WriteFile(path, []byte(`{"check_interval":2,"default_url":"  http://dash/  ","devices":[{"name":" Lounge ","host":" 1.2.3.4 ","url":" http://x/ "}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	loadConfig()
	cfgMu.RLock()
	got = cfg
	cfgMu.RUnlock()
	if got.CheckInterval != minCheckInterval {
		t.Errorf("interval = %d, want the floor %d", got.CheckInterval, minCheckInterval)
	}
	if got.DefaultURL != "http://dash/" {
		t.Errorf("default URL = %q, want trimmed", got.DefaultURL)
	}
	if len(got.Devices) != 1 {
		t.Fatalf("devices = %+v", got.Devices)
	}
	if d := got.Devices[0]; d.Name != "Lounge" || d.Host != "1.2.3.4" || d.URL != "http://x/" {
		t.Errorf("device not trimmed on load: %+v", d)
	}
}

// --- /api/devices/cast and /stop -------------------------------------------

// Neither handler may reach catt without an addressable device and, for a cast,
// a URL catt's cast_site can actually be given.
func TestCastAndStopValidation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		for name, h := range map[string]func(http.ResponseWriter, *http.Request){"cast": handleCast, "stop": handleStop} {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(method, "/api/devices/"+name, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, name, rec.Code)
			}
		}
	}

	castCases := []struct{ body, wantIn string }{
		// Reported separately: one message covering both read as though the device
		// itself were the problem when only the URL was blank.
		{`{"url":"http://dash/"}`, "name or host required"},
		{`{"name":"   ","host":"  ","url":"http://dash/"}`, "name or host required"},
		{`{"name":"Lounge"}`, "url required"},
		{`{"name":"Lounge","url":"   "}`, "url required"},
		// catt reads a "-"-prefixed positional as a flag, and one it accepts makes
		// it exit 0 without casting — recorded as a success, which then arms the
		// app-id learner to adopt whatever is really running on the device.
		{`{"name":"Lounge","url":"--version"}`, "absolute http"},
		{`{"name":"Lounge","url":"file:///etc/passwd"}`, "absolute http"},
		{`{"name":"Lounge","url":"dashboard"}`, "absolute http"},
		{`not json`, ""},
		{`null`, "name or host required"},
	}
	for _, c := range castCases {
		rec := httptest.NewRecorder()
		handleCast(rec, httptest.NewRequest(http.MethodPost, "/api/devices/cast", strings.NewReader(c.body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cast %s = %d, want 400", c.body, rec.Code)
		}
		if c.wantIn != "" && !strings.Contains(rec.Body.String(), c.wantIn) {
			t.Errorf("cast %s said %q, want it to mention %q", c.body, strings.TrimSpace(rec.Body.String()), c.wantIn)
		}
	}

	for _, body := range []string{`{}`, `{"name":"  "}`, `null`, `not json`} {
		rec := httptest.NewRecorder()
		handleStop(rec, httptest.NewRequest(http.MethodPost, "/api/devices/stop", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("stop %s = %d, want 400", body, rec.Code)
		}
	}
}

// Decode stops at the end of the first value and ignores the rest, so a
// concatenated body — two requests joined by a retrying client, a proxy appending
// to the stream — acted on its first half only and answered 200 for the whole
// thing: one device cast, the other silently skipped, with nothing anywhere to
// say so. The same hazard POST /api/config already refuses.
func TestCastAndStopRefuseAConcatenatedBody(t *testing.T) {
	resetCastState()
	f := withFakeCatt(t)

	rec := httptest.NewRecorder()
	handleCast(rec, httptest.NewRequest(http.MethodPost, "/api/devices/cast", strings.NewReader(
		`{"host":"1.2.3.4","url":"http://a/"} {"host":"5.6.7.8","url":"http://b/"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cast with a trailing object = %d (%s), want 400", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	rec = httptest.NewRecorder()
	handleStop(rec, httptest.NewRequest(http.MethodPost, "/api/devices/stop", strings.NewReader(
		`{"host":"1.2.3.4"} trailing`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("stop with trailing content = %d (%s), want 400", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// Refused before the goroutine starts, so nothing reached catt. Both handlers
	// answer before acting, so a partial application would be invisible otherwise.
	if casts, stops := f.recorded(); len(casts) != 0 || len(stops) != 0 {
		t.Errorf("a refused body still reached catt: casts %+v, stops %v", casts, stops)
	}

	// A single object with the trailing newline an encoder writes must still work.
	rec = httptest.NewRecorder()
	handleCast(rec, httptest.NewRequest(http.MethodPost, "/api/devices/cast",
		strings.NewReader(`{"host":"1.2.3.4","url":"http://a/"}`+"\n")))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid cast = %d (%s), want 200", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	waitFor(t, "the cast to be recorded", func() bool {
		casts, _ := f.recorded()
		return len(casts) == 1
	})
}

// --- /api/devices/status ----------------------------------------------------

func TestHandleDeviceStatus(t *testing.T) {
	withConfig(t, Config{CheckInterval: 60, Devices: []DeviceConfig{}})

	for _, method := range []string{http.MethodPost, http.MethodPut} {
		rec := httptest.NewRecorder()
		handleDeviceStatus(rec, httptest.NewRequest(method, "/api/devices/status", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}

	// An empty list must marshal as [] and never null — the UI iterates it.
	rec := httptest.NewRecorder()
	handleDeviceStatus(rec, httptest.NewRequest(http.MethodGet, "/api/devices/status", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("no devices = %q, want []", got)
	}
}

// One entry per configured device, in config order, each carrying the identity
// of the device it describes. The UI pairs them up by index and checks that
// identity to notice when its local list has drifted.
//
// Every row here has a host, so each is answered by the pychromecast helper and
// never by catt — which the tests must not invoke. The helper is pointed at a
// path that does not exist, so the probes fail immediately and identically.
func TestHandleDeviceStatusIsOrderedAndIdentified(t *testing.T) {
	savedScript := statusScript
	statusScript = filepath.Join(t.TempDir(), "does-not-exist.py")
	defer func() { statusScript = savedScript }()

	resetCastState()
	devices := []DeviceConfig{
		{Name: "A", Host: "127.0.0.1"},
		{Host: "127.0.0.2"}, // host-only, so its Name is ""
		{Name: "C", Host: "127.0.0.3"},
		{Name: "Dup", Host: "127.0.0.3", AutoCast: true, URL: "http://dash/"},
	}
	withConfig(t, Config{CheckInterval: 60, Devices: devices})

	rec := httptest.NewRecorder()
	handleDeviceStatus(rec, httptest.NewRequest(http.MethodGet, "/api/devices/status", nil))
	var got []DeviceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if len(got) != len(devices) {
		t.Fatalf("got %d statuses for %d devices", len(got), len(devices))
	}
	for i, d := range devices {
		if got[i].Name != d.Name || got[i].Host != d.Host {
			t.Errorf("status %d = %+v, want the identity of %+v", i, got[i], d)
		}
		if got[i].Error == "" {
			t.Errorf("status %d has no explanation for an unreachable probe: %+v", i, got[i])
		}
	}
	// The last two rows share one IP, so both are told so — the flag is computed
	// from the whole list, which is why handleDeviceStatus and not getDeviceStatus
	// works it out.
	for _, i := range []int{2, 3} {
		if !strings.Contains(got[i].Warning, "same device") {
			t.Errorf("status %d = %+v, want the duplicate advisory", i, got[i])
		}
	}
	if got[0].Warning != "" {
		t.Errorf("status 0 = %+v, want no advisory for a device that appears once", got[0])
	}
}

// The two advisories the device cannot report for itself are merged on top of
// the live status, and which one wins where is the whole point of the layer.
func TestMergeStatusAdvisories(t *testing.T) {
	live := DeviceStatus{Name: "Lounge", Host: "1.2.3.4", State: "Idle"}

	// A device with nothing to say gets the last cast failure, or a failed cast
	// is indistinguishable from one that never happened: the card reads "Idle".
	got := mergeStatusAdvisories(live, "Chromecast not found", "")
	if got.Error != "Chromecast not found" {
		t.Errorf("error = %q, want the recorded cast failure", got.Error)
	}

	// The live result wins when both have something to say: it is the newer and
	// the more specific of the two.
	withLive := live
	withLive.Error = "192.168.1.5 is not reachable on port 8009"
	got = mergeStatusAdvisories(withLive, "Chromecast not found", "")
	if got.Error != withLive.Error {
		t.Errorf("error = %q, want the live result to win", got.Error)
	}

	// A warning never lands in Error. Recorded there it would still be there next
	// tick, masking every real failure that followed it.
	got = mergeStatusAdvisories(live, "", "No IP set")
	if got.Error != "" {
		t.Errorf("a warning leaked into error: %q", got.Error)
	}
	if got.Warning != "No IP set" {
		t.Errorf("warning = %q", got.Warning)
	}
	// And it is set unconditionally: a device whose problem has been fixed must
	// lose the advisory rather than keep the previous poll's.
	got = mergeStatusAdvisories(DeviceStatus{Warning: "stale"}, "", "")
	if got.Warning != "" {
		t.Errorf("warning = %q, want it cleared", got.Warning)
	}
	// The rest of the live status passes through untouched.
	if got = mergeStatusAdvisories(live, "e", "w"); got.Name != "Lounge" || got.Host != "1.2.3.4" || got.State != "Idle" {
		t.Errorf("merge altered the live status: %+v", got)
	}
}

// getDeviceStatus wires that merge to the live probe, the recorded cast error and
// the config warning. Exercised on an unaddressed device, which is the one shape
// getLiveStatus answers without a subprocess.
func TestGetDeviceStatusUsesTheLiveResult(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{}
	setCastError(dev, "Chromecast not found")

	ds := getDeviceStatus(context.Background(), dev, "http://dash/", false)
	if !strings.Contains(ds.Error, "no name or IP") {
		t.Errorf("error = %q, want the live explanation rather than the stale cast failure", ds.Error)
	}
	if ds.Warning != "" {
		t.Errorf("warning = %q; getLiveStatus already explains this device", ds.Warning)
	}
}

// The status helper may be missing, unreadable, or python3 itself may not be
// installed. Every one of those has to produce a message: an empty Error reads
// as success, and the device would be reported as playing.
func TestPychromecastStatusAlwaysExplainsItself(t *testing.T) {
	saved := statusScript
	statusScript = filepath.Join(t.TempDir(), "does-not-exist.py")
	defer func() { statusScript = saved }()

	resetCastState()
	ds := getPychromecastStatus(context.Background(), DeviceConfig{Name: "Lounge", Host: "127.0.0.1"})
	if ds.Error == "" {
		t.Error("a helper that cannot run must still explain itself")
	}
	if ds.State != "unknown" {
		t.Errorf("state = %q, want \"unknown\"", ds.State)
	}
	if ds.Host != "127.0.0.1" || ds.Name != "Lounge" {
		t.Errorf("status lost the device identity: %+v", ds)
	}
	// Nothing was learned about the device, so no cast state may be invented.
	if isCasting(DeviceConfig{Name: "Lounge", Host: "127.0.0.1"}) {
		t.Error("a failed probe marked the device as casting")
	}
}

// --- interpretStatusOutput: everything the status helper's output decides -----

// One run of the helper, without running it. getPychromecastStatus is only the
// plumbing around this; the diagnostics, the cast-state observation and the
// bounding of device-supplied text all live here, and none of it could be
// exercised while it was welded to a python3 subprocess no test may start.

// interpret runs interpretStatusOutput with the boring arguments filled in.
func interpret(dev DeviceConfig, stdout string) DeviceStatus {
	return interpretStatusOutput(dev, []byte(stdout), nil, nil, nil, time.Now())
}

// Every diagnostic branch, in the order it is consulted. An empty Error reads as
// success on the far side, which would report the device as playing.
func TestInterpretStatusOutputAlwaysExplainsAFailure(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	// zeroconf chatter, which is what stderr holds when the script never got to
	// speak. It must lose to both of the context errors below.
	const chatter = "zeroconf: retrying"

	cases := []struct {
		name           string
		stdout, stderr string
		runErr, ctxErr error
		wantIn         string
	}{
		// The script's 12s watchdog fires first, so our deadline being the one that
		// expired means the script produced nothing — and stderr is then noise.
		{"our deadline", "", chatter, errors.New("signal: killed"), context.DeadlineExceeded, "timed out after " + statusQueryTimeout.String()},
		{"caller gave up", "", chatter, errors.New("signal: killed"), context.Canceled, "cancelled"},
		{"stderr", "", "Traceback: no module named pychromecast", errors.New("exit status 1"), nil, "pychromecast"},
		// Unparseable output beats the bare exec error: it is the more specific of
		// the two.
		{"stdout", "some tool printed this", "", errors.New("exit status 1"), nil, "some tool printed this"},
		{"exec error", "", "", errors.New("exec: \"python3\": executable file not found"), nil, "not found"},
		{"nothing at all", "", "", nil, nil, "no status output"},
		// A reported error is the script explaining itself, and needs no fallback.
		{"reported error", `{"error":"1.2.3.4 is not reachable on port 8009"}`, chatter, errors.New("exit status 1"), nil, "not reachable"},
	}
	for _, c := range cases {
		resetCastState()
		ds := interpretStatusOutput(dev, []byte(c.stdout), []byte(c.stderr), c.runErr, c.ctxErr, time.Now())
		if !strings.Contains(ds.Error, c.wantIn) {
			t.Errorf("%s: error = %q, want it to mention %q", c.name, ds.Error, c.wantIn)
		}
		if c.name != "stderr" && strings.Contains(ds.Error, chatter) {
			t.Errorf("%s: zeroconf chatter was quoted as the diagnosis: %q", c.name, ds.Error)
		}
		if ds.State != "unknown" {
			t.Errorf("%s: state = %q, want \"unknown\" — nothing was learned", c.name, ds.State)
		}
		if ds.Name != dev.Name || ds.Host != dev.Host {
			t.Errorf("%s: status lost the device identity: %+v", c.name, ds)
		}
		// A failed query is not an observation. Inventing one would clear a
		// recorded cast error and let the monitor act on a state nobody reported.
		if isCasting(dev) {
			t.Errorf("%s: a failed query marked the device as casting", c.name)
		}
	}
	// The timeout message names the device, because it is the only thing on the
	// card that says which query timed out.
	ds := interpretStatusOutput(dev, nil, nil, nil, context.DeadlineExceeded, time.Now())
	if !strings.Contains(ds.Error, dev.Host) {
		t.Errorf("timeout error = %q, want it to name the host", ds.Error)
	}
}

// A bare `null` on stdout decodes into a value payload without error and leaves
// the zero value — an empty error and is_idle false, which reads as "the device
// is playing something". That is a state nobody reported, applied to castStates
// and rendered on the card, so the payload is decoded into a pointer and nil is
// refused. Same hazard, same fix, as POST /api/config's body.
func TestInterpretStatusOutputRefusesANonObjectPayload(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	for _, stdout := range []string{"null", "", "  ", "not json", `{"app_id":`} {
		resetCastState()
		ds := interpret(dev, stdout)
		if ds.Error == "" {
			t.Errorf("stdout %q produced no error", stdout)
		}
		if ds.State != "unknown" {
			t.Errorf("stdout %q = state %q, want \"unknown\" rather than an invented one", stdout, ds.State)
		}
		if isCasting(dev) {
			t.Errorf("stdout %q was read as the device playing something", stdout)
		}
	}
}

// The helper pays to produce a traceback — it is the only thing that identifies a
// broken pychromecast install, and it is capped at 4000 characters precisely so it
// survives to be read. Decoded and dropped on the floor, that cost bought nothing:
// the card shows the one-line summary and the reason it happened went nowhere.
func TestInterpretStatusOutputLogsTheHelpersTraceback(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	logged := captureLog(t)

	resetCastState()
	ds := interpret(dev, `{"error":"OSError","detail":"Traceback (most recent call last):\n  ImportError"}`)
	if ds.Error != "OSError" {
		t.Errorf("error = %q, want the helper's summary", ds.Error)
	}
	// The traceback is far too long for a card sized for one line, and Error is
	// repeated in every status response for as long as it stands.
	if strings.Contains(ds.Error, "Traceback") {
		t.Errorf("error = %q, want the traceback kept out of the status", ds.Error)
	}
	out := logged.String()
	for _, want := range []string{"Lounge", "OSError", "ImportError"} {
		if !strings.Contains(out, want) {
			t.Errorf("log = %q, want it to mention %q", out, want)
		}
	}

	// Nothing at all when there is no traceback, which is every failure the
	// helper recognised for itself.
	logged.Reset()
	interpret(dev, `{"error":"1.2.3.4 is not reachable on port 8009"}`)
	if logged.Len() != 0 {
		t.Errorf("log = %q, want silence for a diagnosis the helper made itself", logged)
	}
}

// captureLog redirects the standard logger into a buffer for the test's duration.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

func TestInterpretStatusOutputReportsAndRecordsWhatItSaw(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	// Idle. The app id an idle device names — Backdrop, the screensaver — must not
	// reach the learner, or the screensaver becomes our dashboard.
	resetCastState()
	setCastState(dev, true, "http://dash/") // arms the learn flag
	ds := interpret(dev, `{"app_id":"E8C28D3C","display_name":"Backdrop","is_idle":true}`)
	if ds.State != "Idle" || ds.Error != "" {
		t.Errorf("idle device = %+v, want a clean \"Idle\"", ds)
	}
	if isCasting(dev) {
		t.Error("an idle observation was not applied to the cast state")
	}
	if isForeignApp("84912283") {
		t.Error("an idle device's app id was offered to the learner")
	}

	// Playing, and the receiver app's own name is the state.
	resetCastState()
	ds = interpret(dev, `{"app_id":"84912283","display_name":"DashCast","is_idle":false}`)
	if ds.State != "DashCast" {
		t.Errorf("state = %q, want the receiver app's display name", ds.State)
	}
	if !isCasting(dev) {
		t.Error("a playing observation was not applied to the cast state")
	}
	// Nothing learned yet, so nothing may be called foreign.
	if ds.Foreign {
		t.Error("an app was called foreign before anything was learned")
	}

	// Playing but unnamed: the card needs *something* on its state line.
	resetCastState()
	if ds = interpret(dev, `{"app_id":"84912283","is_idle":false}`); ds.State != "Playing" {
		t.Errorf("state = %q, want the \"Playing\" fallback for an app with no name", ds.State)
	}

	// display_name is written by whoever wrote the receiver app and is echoed in
	// every status response for as long as it is up, so it is bounded here too and
	// not only in the helper.
	resetCastState()
	ds = interpret(dev, `{"app_id":"x","display_name":"`+strings.Repeat("é", maxStatusTextLen*2)+`","is_idle":false}`)
	if n := utf8.RuneCountInString(ds.State); n != maxStatusTextLen+1 {
		t.Errorf("an overlong display name was not bounded: %d runes", n)
	}
	if !utf8.ValidString(ds.State) {
		t.Errorf("bounded state is not valid UTF-8: %q", ds.State)
	}
}

// The learner runs before the report, so a poll that has just taught us the app
// id does not then turn round and accuse that same app of being someone else's.
func TestInterpretStatusOutputReportsAForeignApp(t *testing.T) {
	resetCastState()
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	// Two agreeing casts teach us our own app id — the second of them through
	// this very function, which is the ordering being asserted.
	setCastState(dev, true, "http://dash/")
	interpret(dev, `{"app_id":"84912283","display_name":"DashCast","is_idle":false}`)
	setCastState(dev, true, "http://dash/")
	if ds := interpret(dev, `{"app_id":"84912283","display_name":"DashCast","is_idle":false}`); ds.Foreign {
		t.Error("the poll that learned the app id reported it as foreign")
	}

	// Somebody else's app on the same device now is.
	if ds := interpret(dev, `{"app_id":"CA5E9605","display_name":"Netflix","is_idle":false}`); !ds.Foreign {
		t.Error("an app that is not ours was not reported as foreign")
	}
	// An app we cannot name is "cannot tell", not "someone else".
	if ds := interpret(dev, `{"display_name":"Mystery","is_idle":false}`); ds.Foreign {
		t.Error("an app with no id was accused of being foreign")
	}
}

// A probe that began before a cast of ours landed describes the device as it was
// beforehand. Passing that through made a device we had just cast to read "Idle",
// and calling its app foreign made the monitor take back a device that was
// already showing our dashboard — a second, wasted cast.
func TestInterpretStatusOutputPrefersTheRecordedStateOverAStalePoll(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}

	resetCastState()
	probeStarted := time.Now().Add(-time.Second)
	setCastState(dev, true, "http://dash/") // the cast lands while the probe is out
	ds := interpretStatusOutput(dev, []byte(`{"is_idle":true}`), nil, nil, nil, probeStarted)
	if ds.State != "Playing" {
		t.Errorf("state = %q, want the newer recorded \"Playing\"", ds.State)
	}
	if !isCasting(dev) {
		t.Error("a stale poll overwrote the cast state")
	}

	// And the other way round, after a stop: the stale poll saw someone's app, so
	// neither its state nor its foreignness may be believed.
	resetCastState()
	probeStarted = time.Now().Add(-time.Second)
	setCastState(dev, false, "")
	ds = interpretStatusOutput(dev, []byte(`{"app_id":"CA5E9605","display_name":"Netflix","is_idle":false}`), nil, nil, nil, probeStarted)
	if ds.State != "Idle" {
		t.Errorf("state = %q, want the newer recorded \"Idle\"", ds.State)
	}
	if ds.Foreign {
		t.Error("a dropped observation still reported a foreign app")
	}
}

// A cancelled probe is not a timeout and not a device fault. Quoting zeroconf
// chatter from stderr as the diagnosis is what this ordering prevents.
func TestPychromecastStatusReportsCancellation(t *testing.T) {
	saved := statusScript
	statusScript = filepath.Join(t.TempDir(), "does-not-exist.py")
	defer func() { statusScript = saved }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ds := getPychromecastStatus(ctx, DeviceConfig{Name: "Lounge", Host: "127.0.0.1"})
	if !strings.Contains(ds.Error, "cancelled") {
		t.Errorf("error = %q, want it to name the cancellation", ds.Error)
	}
}

// --- /api/subnets -----------------------------------------------------------

func TestHandleSubnets(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		handleSubnets(rec, httptest.NewRequest(method, "/api/subnets", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handleSubnets(rec, httptest.NewRequest(http.MethodGet, "/api/subnets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	// Never null: the UI feeds this straight into an x-for over the datalist.
	if strings.TrimSpace(rec.Body.String()) == "null" {
		t.Error("no detected subnets marshalled as null, want []")
	}
	var cidrs []string
	if err := json.Unmarshal(rec.Body.Bytes(), &cidrs); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	for _, c := range cidrs {
		base, ok := strings.CutSuffix(c, ".0/24")
		if !ok {
			t.Errorf("suggested subnet %q is not a /24", c)
			continue
		}
		// The suggestion has to survive the round trip back through parseSubnet,
		// or picking it from the datalist yields "Invalid subnet".
		if parseSubnet(c) != base {
			t.Errorf("suggested subnet %q does not parse back to %q", c, base)
		}
		if ip := net.ParseIP(base + ".1"); ip == nil || !ip.IsPrivate() {
			t.Errorf("suggested subnet %q is not private", c)
		}
	}
}

// --- /api/devices/scan ------------------------------------------------------

// This endpoint probes every host on a /24, so nothing but a GET may start it.
func TestHandleScanRejectsNonGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodHead} {
		rec := httptest.NewRecorder()
		handleScan(rec, httptest.NewRequest(method, "/api/devices/scan", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}

// The reason has to ride on the terminating event: the UI overwrites its status
// line when the stream ends, so a reason sent as a "status" was replaced by the
// generic "No devices found" and never seen.
func TestHandleScanReportsRefusalsOnTheDoneEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan?subnet=192.168.999", nil))
	events := sseEvents(t, rec.Body.String())
	if len(events) != 1 || events[0].Type != "done" {
		t.Fatalf("invalid subnet produced %+v, want a single done event", events)
	}
	if !strings.Contains(events[0].Message, "Invalid subnet") {
		t.Errorf("done message = %q, want it to name the bad subnet", events[0].Message)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// An invalid subnet must not leave the one-scan-at-a-time latch held, or the
	// next attempt is turned away for the rest of the process lifetime.
	if scanInFlight.Load() {
		t.Error("a refused scan left scanInFlight set")
	}
}

func TestHandleScanRefusesASecondScan(t *testing.T) {
	scanInFlight.Store(true)
	defer scanInFlight.Store(false)

	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan", nil))
	events := sseEvents(t, rec.Body.String())
	if len(events) != 1 || events[0].Type != "done" {
		t.Fatalf("events = %+v, want a single done event", events)
	}
	if !strings.Contains(events[0].Message, "already running") {
		t.Errorf("done message = %q, want it to say a scan is in flight", events[0].Message)
	}
	// The latch belongs to the scan that is still running; refusing must not
	// release it on that scan's behalf.
	if !scanInFlight.Load() {
		t.Error("refusing a second scan cleared the in-flight latch")
	}
}

// --- the catt status parse --------------------------------------------------

// `catt status` describes the *media* session, and a web page cast has none, so
// for our own casts the output carries no state at all and the cached one is the
// only answer there is. When the device does report one it is the device's own
// truth and wins.
func TestCattStatusState(t *testing.T) {
	// The byte-identical output for "our dashboard is up" and "genuinely idle":
	// two Volume lines and nothing else.
	const webCast = "Volume: 100\nVolume muted: False\n"
	if got := cattStatusState(webCast, true); got != "Playing" {
		t.Errorf("state = %q, want the cached \"Playing\"", got)
	}
	if got := cattStatusState(webCast, false); got != "Idle" {
		t.Errorf("state = %q, want the cached \"Idle\"", got)
	}
	// A media app does report one, and it beats our guess.
	media := "Title: Something\nState: PAUSED\nVolume: 100\n"
	if got := cattStatusState(media, true); got != "PAUSED" {
		t.Errorf("state = %q, want the device's own PAUSED", got)
	}
	if got := cattStatusState(media, false); got != "PAUSED" {
		t.Errorf("state = %q, want the device's own PAUSED even when we think it is idle", got)
	}
	// A bare "State: " says nothing. Taking it blanked the card's only state
	// text, which reads as a UI fault rather than as the device saying nothing.
	if got := cattStatusState("State: \nVolume: 100\n", true); got != "Playing" {
		t.Errorf("state = %q, want an empty State line ignored", got)
	}
	// The label is matched with its space, so "State:" alone and the other five
	// labels catt can print are all left alone.
	for _, out := range []string{"State:\n", "Volume muted: False\n", "Remaining time: 12\n"} {
		if got := cattStatusState(out, false); got != "Idle" {
			t.Errorf("cattStatusState(%q) = %q, want the fallback", out, got)
		}
	}
	// \r\n output must not leave the state with an invisible character stuck to
	// the end, and the value is bounded like every other subprocess string.
	if got := cattStatusState("State: BUFFERING\r\n", false); got != "BUFFERING" {
		t.Errorf("state = %q, want the CR stripped", got)
	}
	long := cattStatusState("State: "+strings.Repeat("é", maxStatusTextLen*2), false)
	if n := utf8.RuneCountInString(long); n != maxStatusTextLen+1 {
		t.Errorf("an overlong state was not bounded: %d runes", n)
	}
	if !utf8.ValidString(long) {
		t.Errorf("bounded state is not valid UTF-8: %q", long)
	}
}

// --- the monitor's wait ------------------------------------------------------

// The interval ceiling is a day, so a monitor that waits on the value read at the
// top of the cycle applied a lowered interval up to 24h late and the save looked
// like it had done nothing.
func TestRemainingWaitTracksALoweredInterval(t *testing.T) {
	withConfig(t, Config{CheckInterval: maxCheckInterval})

	waitFrom := time.Now()
	if d := remainingWait(waitFrom); d < 23*time.Hour {
		t.Fatalf("remaining wait = %v, want most of a day", d)
	}
	cfgMu.Lock()
	cfg.CheckInterval = minCheckInterval
	cfgMu.Unlock()
	if d := remainingWait(waitFrom); d > minCheckInterval*time.Second {
		t.Errorf("remaining wait after lowering the interval = %v, want at most %ds", d, minCheckInterval)
	}

	// Measured from the start of the wait, so a burst of saves can only shorten
	// it: once the interval has elapsed the answer stays non-positive however
	// many times it is asked, and monitorLoop polls instead of waiting again.
	if d := remainingWait(time.Now().Add(-time.Hour)); d > 0 {
		t.Errorf("remaining wait for an elapsed interval = %v, want <= 0", d)
	}
}

// --- the TCP scan ------------------------------------------------------------

// The loopback /24: every host in it is local, so a probe to an unbound port is
// refused instantly and no packet reaches the LAN. It exercises the fan-out, the
// progress ticker and the final progress event without scanning anyone.
const loopbackSubnet = "127.0.0"

func TestTCPScanCompletesAndReportsEveryHost(t *testing.T) {
	var (
		mu       sync.Mutex
		statuses []string
		progress [][2]int
	)
	devices, failure := tcpScan(context.Background(), []string{loopbackSubnet},
		func(msg string) { mu.Lock(); statuses = append(statuses, msg); mu.Unlock() },
		func(DiscoveredDevice) {},
		func(checked, total int) { mu.Lock(); progress = append(progress, [2]int{checked, total}); mu.Unlock() },
	)
	if failure != "" {
		t.Errorf("failure = %q, want none for an explicit subnet", failure)
	}
	// Loopback has no Chromecast on it; anything found would be a real service on
	// 127.0.0.x:8008 answering eureka_info, which is not this test's business.
	for _, d := range devices {
		if !strings.HasPrefix(d.Host, loopbackSubnet+".") {
			t.Errorf("scan reported a device outside the subnet it was given: %+v", d)
		}
	}
	if len(statuses) != 1 || !strings.Contains(statuses[0], loopbackSubnet+".0/24") {
		t.Errorf("statuses = %q, want one naming the subnet and the host count", statuses)
	}
	if !strings.Contains(statuses[0], "254") {
		t.Errorf("status %q does not name the number of hosts", statuses[0])
	}
	// The ticker fires every 500ms, so the last event it emits is almost always
	// short of the total — leaving the UI progress bar stuck below 100%. The
	// final event is emitted explicitly once every host has been checked.
	if len(progress) == 0 {
		t.Fatal("no progress events at all")
	}
	last := progress[len(progress)-1]
	if last[0] != 254 || last[1] != 254 {
		t.Errorf("final progress = %d/%d, want 254/254", last[0], last[1])
	}
	for _, p := range progress {
		if p[0] > p[1] {
			t.Errorf("progress %d/%d exceeds the total", p[0], p[1])
		}
	}
}

// The confirm step: a host answering on the setup port is only reported once it
// has served an eureka_info document with a name in it. Nothing exercised it
// before — it needs something listening on that port — so a stand-in server is
// put on loopback and the scan pointed at its port. Every host but 127.0.0.1 has
// nothing there, which is also what makes this the negative case for free.
func TestTCPScanConfirmsADeviceOnTheSetupPort(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // the device name expected, or "" for "must not be reported"
	}{
		{"a real reply", `{"name":"Living Room","ssid":"home"}`, "Living Room"},
		// Whatever else is on the LAN on that port is not a Chromecast. Reporting
		// it would put a nameless card in the scan results.
		{"no name", `{"name":"","ssid":"home"}`, ""},
		{"not json", `<html>router login</html>`, ""},
		// The body is read through a LimitReader, so a host that answers with
		// megabytes leaves the JSON truncated and unparseable rather than being
		// read to the end — which on a LAN is tens of megabytes per host, fifty
		// hosts at a time, before the 2s client timeout stops it. The name here is
		// deliberately past the cap, so a bound that did not bite would find it.
		{"oversized", `{"padding":"` + strings.Repeat("x", maxEurekaInfo) + `","name":"Too Big"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, c.body)
			}))
			defer srv.Close()

			_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatal(err)
			}
			saved := chromecastSetupPort
			chromecastSetupPort = port
			defer func() { chromecastSetupPort = saved }()

			var (
				mu    sync.Mutex
				found []DiscoveredDevice
				lines []string
			)
			devices, failure := tcpScan(context.Background(), []string{loopbackSubnet},
				func(msg string) { mu.Lock(); lines = append(lines, msg); mu.Unlock() },
				func(d DiscoveredDevice) { mu.Lock(); found = append(found, d); mu.Unlock() },
				func(int, int) {},
			)
			if failure != "" {
				t.Errorf("failure = %q, want none", failure)
			}
			// onFound is what the SSE stream is built on, so it must agree with the
			// returned slice rather than being a separate accounting.
			if len(found) != len(devices) {
				t.Errorf("%d found callbacks for %d returned devices", len(found), len(devices))
			}
			if c.want == "" {
				if len(devices) != 0 {
					t.Errorf("devices = %+v, want none: the reply is not a usable eureka_info", devices)
				}
				return
			}
			want := DiscoveredDevice{Name: c.want, Host: loopbackSubnet + ".1"}
			if len(devices) != 1 || devices[0] != want {
				t.Fatalf("devices = %+v, want exactly %+v", devices, want)
			}
			if path != "/setup/eureka_info" {
				t.Errorf("scan requested %q, want /setup/eureka_info", path)
			}
			// The status line names the port, so a scan that found nothing says
			// which port it was looking on.
			if len(lines) != 1 || !strings.Contains(lines[0], portStr) {
				t.Errorf("statuses = %q, want one naming port %s", lines, portStr)
			}
		})
	}
}

// A client that navigates away cancels the request, and the remaining hosts must
// not go on being probed with nobody listening.
func TestTCPScanStopsWhenTheClientGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	devices, failure := tcpScan(ctx, []string{loopbackSubnet},
		func(string) {}, func(DiscoveredDevice) {}, func(int, int) {})
	if len(devices) != 0 || failure != "" {
		t.Errorf("cancelled scan = (%+v, %q), want nothing and no failure", devices, failure)
	}
}

// The whole SSE contract in one pass: the ordered event types, counts that are
// always present, and a terminating event the UI can read a result off.
func TestHandleScanStreamsTheWholeTCPPath(t *testing.T) {
	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan?subnet="+loopbackSubnet+".0/24", nil))

	events := sseEvents(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("events = %+v, want at least a status and a done", events)
	}
	first, last := events[0], events[len(events)-1]
	if first.Type != "status" {
		t.Errorf("first event = %+v, want the probing status", first)
	}
	if last.Type != "done" {
		t.Errorf("last event = %+v, want done", last)
	}
	if last.Message != "" {
		t.Errorf("done carried a failure for an explicit subnet: %q", last.Message)
	}
	var progressed, found int
	for _, e := range events {
		switch e.Type {
		case "progress":
			progressed++
			if e.Total != 254 {
				t.Errorf("progress event total = %d, want 254", e.Total)
			}
		case "found":
			found++
			if e.Device == nil || e.Device.Host == "" {
				t.Errorf("found event with no device: %+v", e)
			}
		}
	}
	if progressed == 0 {
		t.Error("no progress events — the UI bar would never move")
	}
	if last.Count != found {
		t.Errorf("done count = %d but %d found events were sent", last.Count, found)
	}
	// The latch must be released whatever happened, or every later scan is
	// refused for the rest of the process lifetime.
	if scanInFlight.Load() {
		t.Error("a completed scan left scanInFlight set")
	}
	// An explicit subnet skips catt entirely — which is why this test can run at
	// all, and why it must never be given one that reaches the real LAN.
	if strings.Contains(rec.Body.String(), "mDNS") {
		t.Error("an explicit subnet should not have run the mDNS scan")
	}
}

// --- handleScan's two-stage discovery ---------------------------------------

// withScanners substitutes both discovery stages. The mDNS one shells out to
// catt, and the TCP one probes 254 hosts of whatever localSubnets finds — which
// for the auto-detect path is the LAN of the machine running the tests.
func withScanners(t *testing.T, mdns func(context.Context) []DiscoveredDevice,
	tcp func(context.Context, []string, func(string), func(DiscoveredDevice), func(int, int)) ([]DiscoveredDevice, string)) {
	t.Helper()
	savedMDNS, savedTCP := mdnsScan, tcpScanner
	if mdns != nil {
		mdnsScan = mdns
	}
	if tcp != nil {
		tcpScanner = tcp
	}
	t.Cleanup(func() { mdnsScan, tcpScanner = savedMDNS, savedTCP })
}

// An mDNS hit ends the stream there: it is the fast and accurate answer, and the
// TCP fallback exists only for the bridge-networking case where mDNS cannot work
// at all. Running it anyway would probe 254 hosts for nothing.
func TestHandleScanStopsAtAnMDNSHit(t *testing.T) {
	withScanners(t,
		func(context.Context) []DiscoveredDevice {
			return []DiscoveredDevice{{Name: "Lounge", Host: "1.2.3.4"}, {Name: "Kitchen", Host: "5.6.7.8"}}
		},
		func(context.Context, []string, func(string), func(DiscoveredDevice), func(int, int)) ([]DiscoveredDevice, string) {
			t.Error("the TCP fallback ran even though mDNS found devices")
			return nil, ""
		})

	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan", nil))

	events := sseEvents(t, rec.Body.String())
	var found []DiscoveredDevice
	for _, e := range events {
		if e.Type == "found" && e.Device != nil {
			found = append(found, *e.Device)
		}
	}
	if len(found) != 2 || found[0].Name != "Lounge" || found[1].Host != "5.6.7.8" {
		t.Errorf("found events = %+v, want both discovered devices in order", found)
	}
	last := events[len(events)-1]
	if last.Type != "done" || last.Count != 2 || last.Message != "" {
		t.Errorf("last event = %+v, want a clean done with a count of 2", last)
	}
	if scanInFlight.Load() {
		t.Error("the mDNS path left scanInFlight set")
	}
}

// mDNS does not work under Docker bridge networking, which is the deployment this
// service is written for, so the fall-through is the path that normally runs.
func TestHandleScanFallsThroughToTheTCPScan(t *testing.T) {
	var gotSubnets []string
	called := false
	withScanners(t,
		func(context.Context) []DiscoveredDevice { return nil },
		func(_ context.Context, subnets []string, onStatus func(string), onFound func(DiscoveredDevice), onProgress func(int, int)) ([]DiscoveredDevice, string) {
			called, gotSubnets = true, subnets
			onStatus("Probing 254 hosts")
			onProgress(0, 254) // zero must still reach the UI; see ScanEvent
			onFound(DiscoveredDevice{Name: "Lounge", Host: "1.2.3.4"})
			onProgress(254, 254)
			return []DiscoveredDevice{{Name: "Lounge", Host: "1.2.3.4"}}, ""
		})

	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan", nil))

	if !called {
		t.Fatal("mDNS found nothing and the TCP fallback never ran")
	}
	// nil, not an empty slice: that is what tells tcpScan to auto-detect rather
	// than to scan no subnets at all.
	if gotSubnets != nil {
		t.Errorf("TCP scan given subnets %+v, want nil so that it auto-detects", gotSubnets)
	}
	events := sseEvents(t, rec.Body.String())
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Type)
	}
	want := []string{"status", "status", "status", "progress", "found", "progress", "done"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("event types = %v, want %v", kinds, want)
	}
	// The user has to be told why a second, slower stage started.
	if !strings.Contains(events[1].Message, "mDNS scan found nothing") {
		t.Errorf("second status = %q, want it to explain the fallback", events[1].Message)
	}
	if last := events[len(events)-1]; last.Count != 1 || last.Message != "" {
		t.Errorf("done = %+v, want a count of 1 and no failure", last)
	}
}

// An explicit subnet is the answer to mDNS not working, so it must skip that
// stage rather than pay for it first — and it has to actually arrive at the
// scanner, parsed to the three-octet base.
func TestHandleScanPassesAnExplicitSubnetStraightToTheTCPScan(t *testing.T) {
	var gotSubnets []string
	withScanners(t,
		func(context.Context) []DiscoveredDevice {
			t.Error("an explicit subnet must not run the mDNS scan")
			return nil
		},
		func(_ context.Context, subnets []string, _ func(string), _ func(DiscoveredDevice), _ func(int, int)) ([]DiscoveredDevice, string) {
			gotSubnets = subnets
			return nil, ""
		})

	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan?subnet=192.168.7.0%2F24", nil))
	if len(gotSubnets) != 1 || gotSubnets[0] != "192.168.7" {
		t.Errorf("subnets = %+v, want the parsed base [192.168.7]", gotSubnets)
	}
	if events := sseEvents(t, rec.Body.String()); events[len(events)-1].Type != "done" {
		t.Errorf("events = %+v, want a terminating done", events)
	}
}

// A scan that could not run says why on the terminating event, because the UI
// overwrites its status line when the stream ends.
func TestHandleScanPutsTheTCPFailureOnDone(t *testing.T) {
	const reason = "No private IPv4 subnet detected — enter a subnet to scan"
	withScanners(t,
		func(context.Context) []DiscoveredDevice { return nil },
		func(context.Context, []string, func(string), func(DiscoveredDevice), func(int, int)) ([]DiscoveredDevice, string) {
			return nil, reason
		})

	rec := httptest.NewRecorder()
	handleScan(rec, httptest.NewRequest(http.MethodGet, "/api/devices/scan", nil))
	events := sseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Type != "done" || last.Message != reason {
		t.Errorf("last event = %+v, want the give-up reason on done", last)
	}
	for _, e := range events[:len(events)-1] {
		if e.Message == reason {
			t.Error("the reason was also sent as a status, where the UI overwrites it")
		}
	}
}

// --- the monitor's interruptible wait ---------------------------------------

// The interval ceiling is a day, so the wait has to be woken by a save and
// re-measured, or lowering the interval from anywhere near that ceiling took
// effect up to 24h later and the save looked like it had done nothing.
// monitorLoop itself cannot be called from a test — it never returns — which is
// why the wait is its own function.
func TestAwaitNextTickIsWokenByAConfigChange(t *testing.T) {
	withConfig(t, Config{CheckInterval: maxCheckInterval})
	// A wait that began just over the eventual (floored) interval ago, so the
	// lowered value is already elapsed and the wake-up returns rather than
	// re-arming for another 10s.
	waitFrom := time.Now().Add(-(minCheckInterval + 1) * time.Second)
	t.Cleanup(func() {
		select {
		case <-configChanged:
		default:
		}
	})

	done := make(chan struct{})
	go func() { awaitNextTick(waitFrom); close(done) }()

	// Still a day to go, so it must not return on its own.
	select {
	case <-done:
		t.Fatal("the wait returned before the interval elapsed")
	case <-time.After(50 * time.Millisecond):
	}

	cfgMu.Lock()
	cfg.CheckInterval = minCheckInterval
	cfgMu.Unlock()
	select {
	case configChanged <- struct{}{}:
	default:
		t.Fatal("could not signal the monitor")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a lowered interval did not wake the wait")
	}
}

// A wake-up that does not shorten the interval past zero must go back to waiting,
// not fall through and poll every device on every save.
func TestAwaitNextTickResumesWaitingAfterAnIrrelevantWakeUp(t *testing.T) {
	withConfig(t, Config{CheckInterval: maxCheckInterval})
	t.Cleanup(func() {
		select {
		case <-configChanged:
		default:
		}
	})

	// As above: begun long enough ago that the lowered interval at the end has
	// already elapsed, so the wait ends promptly instead of re-arming for another.
	waitFrom := time.Now().Add(-(minCheckInterval + 1) * time.Second)
	done := make(chan struct{})
	go func() { awaitNextTick(waitFrom); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case configChanged <- struct{}{}:
		case <-done:
			t.Fatal("the wait returned on a wake-up that left a day still to run")
		}
	}
	select {
	case <-done:
		t.Fatal("the wait returned on a wake-up that left a day still to run")
	case <-time.After(50 * time.Millisecond):
	}
	// Leave nothing blocked behind: an elapsed interval returns immediately.
	cfgMu.Lock()
	cfg.CheckInterval = minCheckInterval
	cfgMu.Unlock()
	select {
	case configChanged <- struct{}{}:
	default:
	}
	<-done
}

// --- identity and text helpers ---------------------------------------------

// The prefixes are what stop a device named after an IP from sharing state with
// the device at that IP.
func TestDeviceKeyNamespacesNameAndHost(t *testing.T) {
	byHost := deviceKey(DeviceConfig{Name: "Lounge", Host: "1.2.3.4"})
	if byHost != "host:1.2.3.4" {
		t.Errorf("deviceKey with a host = %q, want the IP to win", byHost)
	}
	if got := deviceKey(DeviceConfig{Name: "Lounge"}); got != "name:Lounge" {
		t.Errorf("deviceKey without a host = %q", got)
	}
	if deviceKey(DeviceConfig{Name: "1.2.3.4"}) == deviceKey(DeviceConfig{Host: "1.2.3.4"}) {
		t.Error("a name and an IP that read alike must not share a key")
	}
}

func TestShortText(t *testing.T) {
	if got := shortText("  spaced  "); got != "spaced" {
		t.Errorf("shortText = %q, want it trimmed", got)
	}
	if got := shortText(""); got != "" {
		t.Errorf("shortText(\"\") = %q", got)
	}
	// Exactly at the cap: no ellipsis, nothing dropped.
	exact := strings.Repeat("a", maxStatusTextLen)
	if got := shortText(exact); got != exact {
		t.Errorf("a message exactly at the cap was altered: %d runes", utf8.RuneCountInString(got))
	}
	// One past it, sliced by runes so the result stays valid UTF-8.
	long := strings.Repeat("é", maxStatusTextLen+1)
	got := shortText(long)
	if !utf8.ValidString(got) {
		t.Errorf("truncated text is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxStatusTextLen+1 {
		t.Errorf("truncated to %d runes, want %d including the ellipsis", n, maxStatusTextLen+1)
	}
}

// The UI compares a status against its device to notice that its local list has
// drifted from the server's. That only works while both sides agree on how an
// empty host is encoded — omitted on both, so the two read as equal.
func TestHostIsOmittedConsistently(t *testing.T) {
	cfgJSON, err := json.Marshal(DeviceConfig{Name: "Lounge"})
	if err != nil {
		t.Fatal(err)
	}
	statusJSON, err := json.Marshal(DeviceStatus{Name: "Lounge", State: "Idle"})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{cfgJSON, statusJSON} {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["host"]; ok {
			t.Errorf("an empty host was emitted rather than omitted: %s", data)
		}
	}
	// And a set host appears on both, so they can be compared at all.
	statusJSON, _ = json.Marshal(DeviceStatus{Name: "Lounge", Host: "1.2.3.4"})
	if !strings.Contains(string(statusJSON), `"host":"1.2.3.4"`) {
		t.Errorf("status did not carry the device host: %s", statusJSON)
	}
}

// A warning is advisory and an error is a failure; folding them into one field
// would let a standing config problem mask every real cast failure that follows.
func TestWarningAndErrorAreSeparateFields(t *testing.T) {
	data, err := json.Marshal(DeviceStatus{Name: "A", State: "Idle", Error: "boom", Warning: "advice"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["error"] != "boom" || m["warning"] != "advice" {
		t.Errorf("status did not carry both fields: %s", data)
	}
	// Both omitted when empty, so the UI's x-show tests are false rather than
	// showing an empty amber or red line.
	data, _ = json.Marshal(DeviceStatus{Name: "A", State: "Idle"})
	if strings.Contains(string(data), "error") || strings.Contains(string(data), "warning") {
		t.Errorf("empty advisories were emitted: %s", data)
	}
}

// --- runCatt, and the argv each catt call is built with ----------------------
//
// The subprocess itself is this test binary, re-invoked to run TestCattStandIn
// and nothing else. That is what makes runCatt's own decisions assertable without
// running catt, which no test here may do: the deadline it names rather than
// letting the caller read "signal: killed", the two streams it merges into one
// bounded buffer, and — through the cattCmd seam recording what it was asked for —
// the subcommand and timeout budget each of the three catt calls is given. A typo
// in one of those subcommand names is otherwise invisible until a device fails to
// cast, with catt's usage message on the card as the only clue.

// cattStandInEnv selects what the re-invoked binary should do. Passed in the
// environment, not on the command line: catt's own arguments would reach the
// child's flag parser as unknown flags ("-d") and kill it before it ran.
const cattStandInEnv = "CATT_STAND_IN_MODE"

func TestCattStandIn(t *testing.T) {
	mode := os.Getenv(cattStandInEnv)
	if mode == "" {
		t.Skip("only meaningful as the stand-in subprocess")
	}
	switch mode {
	case "quiet": // exits 0 with nothing on either stream, like a successful cast
	case "scan":
		fmt.Fprintln(os.Stdout, "Scanning Chromecasts...")
		fmt.Fprintln(os.Stdout, "192.168.1.5 - Kitchen - Nest Hub - Google Inc. Nest Hub")
	case "scan-then-fail":
		fmt.Fprintln(os.Stdout, "192.168.1.5 - Kitchen - Nest Hub - Google Inc. Nest Hub")
		fmt.Fprint(os.Stderr, "Failed to connect.")
		os.Exit(1)
	case "fail":
		fmt.Fprint(os.Stderr, "Failed to connect.")
		os.Exit(1)
	case "hang":
		time.Sleep(time.Minute) // killed by runCatt's deadline
	default:
		fmt.Fprintf(os.Stderr, "unknown stand-in mode %q", mode)
		os.Exit(2)
	}
	// os.Exit rather than returning: the testing framework would otherwise write
	// its own "PASS" onto the pipe the caller is about to parse.
	os.Exit(0)
}

type cattStandIn struct {
	mu    sync.Mutex
	calls [][]string
	// budgets is how long each call had left when the process was built, which is
	// the per-command timeout runCatt was given.
	budgets []time.Duration
}

func (s *cattStandIn) recorded() ([][]string, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string{}, s.calls...), append([]time.Duration{}, s.budgets...)
}

// withCattStandIn points the catt subprocess at this test binary for the duration
// of the test, and records what each invocation was asked to run.
func withCattStandIn(t *testing.T, mode string) *cattStandIn {
	t.Helper()
	s := &cattStandIn{}
	saved := cattCmd
	cattCmd = func(ctx context.Context, args ...string) *exec.Cmd {
		s.mu.Lock()
		s.calls = append(s.calls, append([]string{}, args...))
		budget := time.Duration(0)
		if deadline, ok := ctx.Deadline(); ok {
			budget = time.Until(deadline)
		}
		s.budgets = append(s.budgets, budget)
		s.mu.Unlock()
		// Built from ctx, so the substitute keeps the kill-on-deadline behaviour the
		// timeout naming is about.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCattStandIn$")
		cmd.Env = append(os.Environ(), cattStandInEnv+"="+mode)
		return cmd
	}
	t.Cleanup(func() { cattCmd = saved })
	return s
}

func TestCattCallsPassTheRightSubcommandAndBudget(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	cases := []struct {
		name   string
		run    func(context.Context)
		args   []string
		budget time.Duration
	}{
		{"status", func(ctx context.Context) { cattStatus(ctx, dev) },
			[]string{"-d", "1.2.3.4", "status"}, 10 * time.Second},
		{"cast_site", func(ctx context.Context) { castSite(ctx, dev, "http://dash/") },
			[]string{"-d", "1.2.3.4", "cast_site", "http://dash/"}, 30 * time.Second},
		{"stop", func(ctx context.Context) { stopCast(ctx, dev) },
			[]string{"-d", "1.2.3.4", "stop"}, 15 * time.Second},
		// The mDNS scan takes no device: it is asking who is out there.
		{"scan", func(ctx context.Context) { cattScan(ctx) },
			[]string{"scan"}, 30 * time.Second},
	}
	for _, c := range cases {
		s := withCattStandIn(t, "quiet")
		c.run(context.Background())
		calls, budgets := s.recorded()
		if len(calls) != 1 {
			t.Fatalf("%s: ran catt %d times, want once", c.name, len(calls))
		}
		if got := calls[0]; !slicesEqual(got, c.args) {
			t.Errorf("%s: argv = %v, want %v", c.name, got, c.args)
		}
		// Generous: the assertion is which budget was chosen, not how fast the
		// machine got from the deadline to here.
		if d := budgets[0]; d > c.budget || d < c.budget-2*time.Second {
			t.Errorf("%s: budget = %s, want ~%s", c.name, d, c.budget)
		}
	}
}

// A subprocess killed by our own deadline prints nothing, so cattFailure falls
// back to the exec error — and the device card then read "signal: killed", which
// is true and tells nobody that catt simply took too long.
func TestRunCattNamesItsOwnTimeout(t *testing.T) {
	withCattStandIn(t, "hang")
	start := time.Now()
	out, err := runCatt(context.Background(), 200*time.Millisecond, "-d", "1.2.3.4", "status")
	if err == nil {
		t.Fatal("a subprocess that never returns produced no error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("runCatt took %s to give up on a hung subprocess", elapsed)
	}
	msg := cattFailure(err, out)
	if !strings.Contains(msg, "timed out after 200ms") {
		t.Errorf("failure = %q, want the timeout named", msg)
	}
	if strings.Contains(msg, "killed") {
		t.Errorf("failure = %q, want the plumbing detail replaced by the reason", msg)
	}
}

// catt's own words win over anything we could say: it exits non-zero with the
// explanation on stderr, and stderr shares the buffer for exactly that reason.
func TestRunCattReportsWhatCattPrinted(t *testing.T) {
	withCattStandIn(t, "fail")
	out, err := runCatt(context.Background(), 10*time.Second, "-d", "1.2.3.4", "stop")
	if err == nil {
		t.Fatal("a non-zero exit produced no error")
	}
	if got := cattFailure(err, out); got != "Failed to connect." {
		t.Errorf("failure = %q, want catt's own explanation", got)
	}
}

// The mDNS path end to end, minus catt itself: a wrong guess at the output format
// silently returns zero devices and falls through to the slow TCP scan, which is
// indistinguishable from there being nothing on the network.
func TestCattScanParsesWhatCattPrints(t *testing.T) {
	withCattStandIn(t, "scan")
	got := cattScan(context.Background())
	want := []DiscoveredDevice{{Name: "Kitchen - Nest Hub", Host: "192.168.1.5"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("cattScan = %+v, want %+v", got, want)
	}
}

// A failing scan is logged and its output still parsed. catt can name devices and
// *then* exit non-zero — a second interface it could not query, an mDNS socket it
// could not close — and returning nothing there would send handleScan down the
// 254-host TCP fallback for devices it had already been told about.
func TestCattScanKeepsWhatAFailedScanPrinted(t *testing.T) {
	withCattStandIn(t, "scan-then-fail")
	logged := captureLog(t)
	got := cattScan(context.Background())
	if len(got) != 1 || got[0].Host != "192.168.1.5" {
		t.Errorf("cattScan = %+v, want the device catt named before it failed", got)
	}
	// The exit status is not silently swallowed either: it is the only record that
	// the list may be short.
	if !strings.Contains(logged.String(), "Failed to connect.") {
		t.Errorf("log = %q, want the scan failure recorded", logged)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Once the browser has gone the ResponseWriter is no longer valid to write to, and
// a TCP scan keeps producing events for as long as it takes to finish — including
// the found and progress callbacks, which run on the scan's own goroutines.
func TestHandleScanWritesNothingOnceTheClientIsGone(t *testing.T) {
	withScanners(t,
		func(context.Context) []DiscoveredDevice { return nil },
		func(_ context.Context, _ []string, onStatus func(string), onFound func(DiscoveredDevice), onProgress func(int, int)) ([]DiscoveredDevice, string) {
			onStatus("probing")
			onFound(DiscoveredDevice{Name: "Kitchen", Host: "192.168.7.5"})
			onProgress(254, 254)
			return []DiscoveredDevice{{Name: "Kitchen", Host: "192.168.7.5"}}, ""
		})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/devices/scan?subnet=192.168.7.0/24", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handleScan(rec, req)
	if body := rec.Body.String(); body != "" {
		t.Errorf("wrote %q to a client that had gone", body)
	}
	if scanInFlight.Load() {
		t.Error("an abandoned scan left scanInFlight set")
	}
}

// The virtual-interface filter is a guess about a name, so an unusual host — one
// where every private address is on an interface the list happens to match, which
// is any container on a Docker bridge — must fall back to the unfiltered set
// rather than auto-detect nothing at all. Asserted as the relationship between the
// two passes, because which of them is empty depends on the machine running this.
func TestLocalSubnetsFallsBackToTheUnfilteredSet(t *testing.T) {
	physical, all := collectSubnets(true), collectSubnets(false)
	want := physical
	if len(physical) == 0 {
		want = all
	}
	got := localSubnets()
	if !slicesEqual(got, want) {
		t.Errorf("localSubnets = %v, want %v (physical %v, unfiltered %v)", got, want, physical, all)
	}
	// The fallback relaxes the interface-name guess and nothing else: the
	// private-address requirement is a fact about the address, and undoing it is
	// what would port-scan 254 strangers on a host with a routable IPv4.
	for _, base := range got {
		if ip := net.ParseIP(base + ".1"); ip == nil || !ip.IsPrivate() {
			t.Errorf("auto-detected subnet %q is not private", base)
		}
	}
}

// Port 8008 on a LAN is not only ever a Chromecast, and a host can answer the
// dial and then say nothing that resembles HTTP. The confirm step has to drop it
// rather than report a device, and it must not take the scan down with it.
func TestTCPScanIgnoresAHostThatAnswersWithoutSpeakingHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close() // accepted, then hung up mid-request
		}
	}()

	saved := chromecastSetupPort
	chromecastSetupPort = ln.Addr().(*net.TCPAddr).Port
	defer func() { chromecastSetupPort = saved }()

	devices, failure := tcpScan(context.Background(), []string{loopbackSubnet},
		func(string) {}, func(d DiscoveredDevice) { t.Errorf("reported %+v, which never served eureka_info", d) },
		func(int, int) {})
	if failure != "" {
		t.Errorf("failure = %q, want none", failure)
	}
	if len(devices) != 0 {
		t.Errorf("devices = %+v, want none", devices)
	}
}
