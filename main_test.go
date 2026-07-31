package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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
	setCastError(dev, strings.Repeat("é", maxCastErrLen*2))
	got := castError(dev)
	if !utf8.ValidString(got) {
		t.Errorf("truncated message is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxCastErrLen+1 { // +1 for the ellipsis
		t.Errorf("truncated to %d runes, want %d", n, maxCastErrLen+1)
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
	if configWarning(DeviceConfig{Name: "Lounge", AutoCast: true}, ok) == "" {
		t.Error("an auto-cast device with no IP should be flagged")
	}
	// An IP switches it to the pychromecast helper, which reports the app id.
	if got := configWarning(DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}, ok); got != "" {
		t.Errorf("a device with an IP needs no warning, got %q", got)
	}
	// Nothing is monitoring it, so there is nothing to warn about.
	if got := configWarning(DeviceConfig{Name: "Lounge"}, ""); got != "" {
		t.Errorf("a manual-only device needs no warning, got %q", got)
	}
	// getLiveStatus already explains this one; do not pile a second line on it.
	if got := configWarning(DeviceConfig{AutoCast: true}, ok); got != "" {
		t.Errorf("an unaddressed device is already reported, got %q", got)
	}
}

// monitorDevices skips a device it has no usable URL for, and a skip is
// invisible: the card read a plain "Idle" while auto-cast was in fact never
// going to do anything with it.
func TestAutoCastWithoutUsableURLIsFlagged(t *testing.T) {
	dev := DeviceConfig{Name: "Lounge", Host: "1.2.3.4", AutoCast: true}
	if got := configWarning(dev, ""); got == "" {
		t.Error("an auto-cast device with no URL and no default should be flagged")
	}
	if got := configWarning(dev, "192.168.1.5/dash"); got == "" {
		t.Error("an auto-cast device with an unusable URL should be flagged")
	}
	// The URL problem is reported ahead of the missing IP: without a URL the
	// device is never cast to at all, so it is the more fundamental of the two.
	if got := configWarning(DeviceConfig{Name: "Lounge", AutoCast: true}, ""); !strings.Contains(got, "URL") {
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
	got := cattFailure(errors.New("exit status 1"), strings.Repeat("é", maxCastErrLen*3))
	if n := utf8.RuneCountInString(got); n != maxCastErrLen+1 {
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
