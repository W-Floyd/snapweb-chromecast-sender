package main

import (
	"context"
	"encoding/json"
	"errors"
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
	learnedCastApp = ""
	castStatesMu.Unlock()
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

	setCastState(dev, true)
	observeCastState(dev, true, "84912283", time.Now()) // first poll after our cast
	if isForeignApp("84912283") {
		t.Error("our own cast app reported as foreign")
	}
	if !isForeignApp("CA5E9605") {
		t.Error("a different app should be reported as foreign")
	}
	// An empty app id is "cannot tell", not "someone else".
	if isForeignApp("") {
		t.Error("an empty app id must not be reported as foreign")
	}

	// Only the first poll after a cast teaches us; later polls of whatever
	// someone else started must not redefine what our own cast looks like.
	observeCastState(dev, true, "CA5E9605", time.Now())
	if !isForeignApp("CA5E9605") {
		t.Error("a later foreign observation overwrote the learned cast app")
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
