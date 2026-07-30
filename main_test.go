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
	setCastState(a, true)
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
	setCastState(dev, true)
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
	castLearnPending = map[string]bool{}
	learnedCastApp, castAppCandidate = "", ""
	castStatesMu.Unlock()
}

// learnCastApp drives the two-cast agreement that teaches us our own app id.
func learnCastApp(t *testing.T, dev DeviceConfig, appID string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		setCastState(dev, true)
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
	setCastState(dev, true) // cast completes while the probe is still running
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
	setCastState(dev, true)
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
	setCastState(dev, true)
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
	setCastState(dev, true)
	observeCastState(dev, true, "84912283", time.Now())
	if isForeignApp("84912283") {
		t.Error("a disagreeing observation left the mislearned app id in place")
	}

	// Casting resumes, and the next agreeing poll settles on the right id.
	setCastState(dev, true)
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
	setCastState(dev, true)
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

	setCastState(dev, true) // arms the learn flag
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

func TestPruneCastStatesDropsActions(t *testing.T) {
	resetCastState()
	setCastState(DeviceConfig{Name: "Gone", Host: "1.2.3.4"}, true)
	pruneCastStates(nil)
	castStatesMu.RLock()
	n := len(castActions)
	castStatesMu.RUnlock()
	if n != 0 {
		t.Errorf("castActions has %d stale entries after prune", n)
	}
}

func TestCattFailureAlwaysExplains(t *testing.T) {
	if got := cattFailure(errors.New("signal: killed"), "  \n "); got != "signal: killed" {
		t.Errorf("empty output should fall back to the exec error, got %q", got)
	}
	if got := cattFailure(errors.New("exit status 1"), "\nDevice not found.\n"); got != "Device not found." {
		t.Errorf("catt output should win, got %q", got)
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
	virtual := []string{"docker0", "br-1a2b3c", "veth9f2", "virbr0", "utun3", "tailscale0"}
	for _, n := range virtual {
		if !isVirtualIface(n) {
			t.Errorf("%q should be treated as virtual", n)
		}
	}
	for _, n := range []string{"eth0", "en0", "wlan0", "enp3s0", "eno1"} {
		if isVirtualIface(n) {
			t.Errorf("%q should not be treated as virtual", n)
		}
	}
}
