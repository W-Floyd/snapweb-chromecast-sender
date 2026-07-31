package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	// Seeing it play means the error is stale.
	observeCastState(dev, true, "", time.Now())
	if got := castError(dev); got != "" {
		t.Errorf("playing observation left a stale error: %q", got)
	}
	if !isCasting(dev) {
		t.Error("device observed playing should be marked casting")
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
func TestStatusQueryTimeoutOutlastsTheScriptWatchdog(t *testing.T) {
	if statusQueryTimeout <= 12*time.Second {
		t.Errorf("statusQueryTimeout = %v, must exceed the script's 12s watchdog", statusQueryTimeout)
	}
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
	pruneCastStates(nil)
	castStatesMu.RLock()
	nActions, nObserved := len(castActions), len(castObserved)
	nURLs := len(castURLs)
	castStatesMu.RUnlock()
	if nURLs != 0 {
		t.Errorf("castURLs has %d stale entries after prune", nURLs)
	}
	if nActions != 0 {
		t.Errorf("castActions has %d stale entries after prune", nActions)
	}
	if nObserved != 0 {
		t.Errorf("castObserved has %d stale entries after prune", nObserved)
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
	// Only exercise the give-up path. With a private subnet detected, tcpScan
	// would go and probe all 254 hosts of the machine's real LAN, which a unit
	// test has no business doing.
	if len(localSubnets()) > 0 {
		t.Skip("host has a private subnet; reaching this path would scan the real LAN")
	}
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
	manual := DeviceConfig{Name: "Lounge", Host: "1.2.3.4"}
	if got := configWarning(manual, "", true); got == "" {
		t.Error("a duplicate should be flagged even without auto-cast")
	}
	// Still nothing to pile on for a row with no identifier at all: two blank
	// rows share the key "name:", and getLiveStatus already explains each.
	if got := configWarning(DeviceConfig{}, "", true); got != "" {
		t.Errorf("an unaddressed device is already reported, got %q", got)
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
