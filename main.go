package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
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
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
	// Foreign marks a device playing an app that is not our cast — someone
	// started something on it. False whenever we cannot tell (see
	// learnedCastApp), so the UI never accuses anyone on a guess.
	Foreign bool `json:"foreign,omitempty"`
}

type DiscoveredDevice struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

var (
	cfg          Config
	cfgMu        sync.RWMutex
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
	// castActions records when we last *acted* on a device (a cast or stop we
	// initiated), keyed by deviceKey. A status probe takes seconds, so a probe
	// that started before the cast can return after it and report the device as
	// still idle — last-writer-wins then erased the fresh state, the monitor
	// re-cast a device that was already playing, and a fresh cast error was
	// cleared by an observation older than the failure. Observations older than
	// the last action are stale by definition and get dropped.
	castActions = map[string]time.Time{}
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
	// castLearnPending marks devices we have just cast to and whose next
	// non-idle poll therefore identifies that app.
	castLearnPending = map[string]bool{}
	castStatesMu     sync.RWMutex

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

// setCastState records the outcome of a successful cast/stop, and clears any
// error left by a previous failed attempt on the same device.
func setCastState(dev DeviceConfig, playing bool) {
	k := deviceKey(dev)
	castStatesMu.Lock()
	castStates[k] = playing
	castActions[k] = time.Now()
	delete(castErrors, k)
	if playing {
		// Whatever the next poll finds running is what our cast runs as.
		castLearnPending[k] = true
	} else {
		delete(castLearnPending, k)
	}
	castStatesMu.Unlock()
}

// observeCastState records what a status poll saw, without disturbing a recorded
// cast error the way setCastState does. Polling and acting are not the same
// event: a poll that finds the device idle is usually the *consequence* of the
// failed cast, and clearing the error there erased it before
// getDeviceStatus could merge it in — every failure read as a plain "Idle".
// An observed *playing* device does mean the last error is stale, so drop it.
//
// observedAt is when the poll *began*, not when it finished: a probe takes
// seconds, so one started before a cast can land after it and would otherwise
// overwrite the newer truth with what it saw beforehand.
func observeCastState(dev DeviceConfig, playing bool, appID string, observedAt time.Time) {
	k := deviceKey(dev)
	castStatesMu.Lock()
	defer castStatesMu.Unlock()
	if acted, ok := castActions[k]; ok && observedAt.Before(acted) {
		return // this poll predates the cast/stop it would be overwriting
	}
	if playing && appID != "" && castLearnPending[k] {
		// Overwrite rather than keep the first value ever seen: if someone cast
		// to the device in the gap between our cast and this poll we learn the
		// wrong app, and only a later cast can correct it.
		learnedCastApp = appID
		delete(castLearnPending, k)
	}
	castStates[k] = playing
	if playing {
		delete(castErrors, k)
	}
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

// maxCastErrLen bounds what we keep from catt's output; a failure can print a
// full Python traceback, and all of it would end up in every status response.
const maxCastErrLen = 400

func setCastError(dev DeviceConfig, msg string) {
	msg = strings.TrimSpace(msg)
	// Slice by runes, not bytes: a byte-slice can cut a multi-byte character in
	// half and produce invalid UTF-8 in the JSON response.
	if r := []rune(msg); len(r) > maxCastErrLen {
		msg = string(r[:maxCastErrLen]) + "…"
	}
	k := deviceKey(dev)
	castStatesMu.Lock()
	castStates[k] = false
	castErrors[k] = msg
	// A failure is an action too: without this an in-flight poll that saw the
	// device playing could land afterwards and delete the error we just recorded.
	castActions[k] = time.Now()
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
	for k := range castLearnPending {
		if !keep[k] {
			delete(castLearnPending, k)
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

func saveConfig() error {
	cfgMu.RLock()
	data, err := json.MarshalIndent(cfg, "", "  ")
	cfgMu.RUnlock()
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

// runCatt runs catt with a timeout. The parent ctx lets a caller abandon the
// subprocess early — an HTTP handler whose client has disconnected, say —
// instead of leaving it to run out its full timeout.
func runCatt(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "catt", args...).CombinedOutput()
	return string(out), err
}

// cattFailure builds a human-readable reason from a failed catt invocation.
// catt exits non-zero with nothing on the pipe for some failures (and a
// subprocess killed by our timeout prints nothing at all), so falling back to
// the exec error keeps us from reporting an empty explanation.
func cattFailure(err error, out string) string {
	if msg := strings.TrimSpace(out); msg != "" {
		return msg
	}
	return err.Error()
}

func getDeviceStatus(ctx context.Context, dev DeviceConfig) DeviceStatus {
	ds := getLiveStatus(ctx, dev)
	// Surface the last failed cast/stop when the device itself has nothing to
	// report. Otherwise a cast that failed is indistinguishable from one that
	// never happened: the card just reads "Idle".
	if ds.Error == "" {
		ds.Error = castError(dev)
	}
	return ds
}

func getLiveStatus(ctx context.Context, dev DeviceConfig) DeviceStatus {
	if dev.Host != "" {
		return getPychromecastStatus(ctx, dev)
	}
	// Fall back to catt when no host IP is configured.
	args := append(cattDeviceArgs(dev), "status")
	out, err := runCatt(ctx, 10*time.Second, args...)
	ds := DeviceStatus{Name: dev.Name, State: "unknown"}
	if err != nil {
		ds.Error = cattFailure(err, out)
		return ds
	}
	if isCasting(dev) {
		ds.State = "Playing"
	} else {
		ds.State = "Idle"
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "State: "); ok {
			ds.State = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "Content: "); ok {
			ds.URL = strings.TrimSpace(after)
		}
	}
	return ds
}

func getPychromecastStatus(ctx context.Context, dev DeviceConfig) DeviceStatus {
	ds := DeviceStatus{Name: dev.Name, State: "unknown"}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Keep stderr out of stdout: zeroconf/pychromecast log lines and Python
	// warnings land on stderr, and mixing them into stdout makes the JSON
	// unparseable even when the query itself succeeded.
	cmd := exec.CommandContext(ctx, "python3", statusScript, dev.Host)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Stamp before running: what this reports is true as of now, not as of
	// whenever the subprocess happens to finish up to 15s later.
	observedAt := time.Now()
	runErr := cmd.Run()

	var result struct {
		AppID       string `json:"app_id"`
		DisplayName string `json:"display_name"`
		IsIdle      bool   `json:"is_idle"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		// Report the most useful diagnostic available. Previously a missing
		// python3 or an empty stdout produced an empty Error, leaving the UI
		// with no explanation at all.
		switch {
		case strings.TrimSpace(stderr.String()) != "":
			ds.Error = strings.TrimSpace(stderr.String())
		case strings.TrimSpace(stdout.String()) != "":
			ds.Error = strings.TrimSpace(stdout.String())
		case runErr != nil:
			ds.Error = runErr.Error()
		default:
			ds.Error = "no status output from " + statusScript
		}
		return ds
	}
	if result.Error != "" {
		ds.Error = result.Error
		return ds
	}
	if result.IsIdle {
		ds.State = "Idle"
		observeCastState(dev, false, "", observedAt)
	} else {
		ds.State = result.DisplayName
		if ds.State == "" {
			ds.State = "Playing"
		}
		observeCastState(dev, true, result.AppID, observedAt)
		// After observing, so a poll that has just taught us the app id does not
		// then report that same app as somebody else's.
		ds.Foreign = isForeignApp(result.AppID)
	}
	return ds
}

func monitorDevices(ctx context.Context) {
	cfgMu.RLock()
	devices := append([]DeviceConfig{}, cfg.Devices...)
	defaultURL := cfg.DefaultURL
	cfgMu.RUnlock()

	for _, dev := range devices {
		if !dev.AutoCast {
			continue
		}
		url := dev.URL
		if url == "" {
			url = defaultURL
		}
		if url == "" {
			continue
		}
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
			st := getPychromecastStatus(ctx, dev)
			if st.Error != "" {
				log.Printf("skipping auto-cast to %q: %s", dev.Name, st.Error)
				continue
			}
			foreign = st.Foreign
			if foreign && !dev.Takeover {
				// Somebody is watching something. Leave it alone — enable
				// takeover on the device to reclaim it instead.
				continue
			}
		}
		// A foreign app sets the cast state too (isCasting means "playing
		// something", not "playing ours"), so it must not short-circuit a
		// takeover that the check above has already approved.
		if isCasting(dev) && !foreign {
			continue
		}
		if foreign {
			log.Printf("taking %q back from another app", dev.Name)
		}
		log.Printf("auto-casting to %q: %s", dev.Name, url)
		args := append(cattDeviceArgs(dev), "cast_site", url)
		out, err := runCatt(ctx, 30*time.Second, args...)
		if err != nil {
			log.Printf("cast error for %q: %v — %s", dev.Name, err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, true)
		}
	}
}

func monitorLoop() {
	for {
		monitorDevices(context.Background())
		cfgMu.RLock()
		interval := cfg.CheckInterval
		cfgMu.RUnlock()
		// Defensive floor: normalizeConfig already guarantees this, but a hot
		// loop here would hammer every device with status queries.
		if interval < minCheckInterval {
			interval = minCheckInterval
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
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
		var newCfg Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		normalizeConfig(&newCfg)
		cfgMu.Lock()
		cfg = newCfg
		cfgMu.Unlock()
		// Devices can be renamed, re-addressed or deleted here; drop the cast
		// state of anything that no longer exists.
		pruneCastStates(newCfg.Devices)
		if err := saveConfig(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
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
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Emit on whichever of the pair completes the device. Keying the append
		// off "Host:" alone assumed Name always came first; the reverse order
		// left the pair sitting in cur and the device was never reported.
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			cur.Name = strings.TrimSpace(after)
			if cur.Name != "" && cur.Host != "" {
				devices = append(devices, cur)
				cur = DiscoveredDevice{}
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "Host:"); ok {
			cur.Host = strings.TrimSpace(after)
			if cur.Name != "" && cur.Host != "" {
				devices = append(devices, cur)
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
		if before, _, ok := strings.Cut(rest, " - "); ok {
			name = before // trailing " - <manufacturer> <model>"
		}
		if name = strings.TrimSpace(name); name != "" {
			devices = append(devices, DiscoveredDevice{Name: name, Host: strings.TrimSpace(host)})
		}
	}
	return devices
}

// virtualIfacePrefixes names interfaces that cannot have a Chromecast on them:
// container and VM bridges, VPN tunnels, and virtual ethernet pairs. A host
// running Docker has one bridge per compose network, and including them turned
// an auto-detect scan into ~2800 pointless probes across eleven subnets
// instead of 254 across the one LAN the user actually cares about.
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale",
	"tun", "tap", "utun", "zt", "wg",
}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// localSubnets returns the /24 base (e.g. "192.168.1") for each usable
// non-loopback IPv4 interface, preferring physical ones.
func localSubnets() []string {
	physical, all := collectSubnets(true), collectSubnets(false)
	// Fall back to the unfiltered set rather than returning nothing: the prefix
	// list is a heuristic, and on an unusual host it could filter out the only
	// interface there is.
	if len(physical) == 0 {
		return all
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
		s = s[:idx] // strip CIDR mask, leaving bare host address
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
		return strings.Join(parts, ".")
	}
	return ""
}

func handleSubnets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	bases := localSubnets()
	cidrs := make([]string, len(bases)) // non-nil, so it marshals as [] not null
	for i, b := range bases {
		cidrs[i] = b + ".0/24"
	}
	json.NewEncoder(w).Encode(cidrs)
}

// tcpScan probes each host in subnets on port 8008.
// Pass nil subnets to auto-detect from local interfaces.
// onStatus receives human-readable status lines (including subnet info).
// onFound is called immediately when a device is confirmed.
// onProgress is called every ~500ms with (checked, total) counts.
func tcpScan(ctx context.Context, subnets []string, onStatus func(string), onFound func(DiscoveredDevice), onProgress func(int, int)) []DiscoveredDevice {
	if len(subnets) == 0 {
		subnets = localSubnets()
	}
	if len(subnets) == 0 {
		onStatus("No local IPv4 interfaces found")
		return nil
	}

	total := len(subnets) * 254
	subnetLabels := make([]string, len(subnets))
	for i, s := range subnets {
		subnetLabels[i] = s + ".0/24"
	}
	onStatus(fmt.Sprintf("Probing %s — %d hosts on port 8008", strings.Join(subnetLabels, ", "), total))

	var (
		checked int64
		devices []DiscoveredDevice
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 50)
		client  = &http.Client{Timeout: 2 * time.Second}
	)
	// A scan can open a connection to every responding host; without this they
	// sit in the shared idle pool until their keep-alive expires.
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

				conn, err := net.DialTimeout("tcp", h+":8008", 400*time.Millisecond)
				if err != nil {
					return
				}
				conn.Close()

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h+":8008/setup/eureka_info", nil)
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
				if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Name == "" {
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
	return devices
}

func handleScan(w http.ResponseWriter, r *http.Request) {
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
		devices = cattScan(ctx)
		if len(devices) > 0 {
			for i := range devices {
				send(ScanEvent{Type: "found", Device: &devices[i]})
			}
			send(ScanEvent{Type: "done", Count: len(devices)})
			return
		}
		send(ScanEvent{Type: "status", Message: "mDNS scan found nothing — starting TCP fallback"})
	}

	devices = tcpScan(
		ctx,
		explicitSubnets,
		func(msg string) { send(ScanEvent{Type: "status", Message: msg}) },
		func(d DiscoveredDevice) { send(ScanEvent{Type: "found", Device: &d}) },
		func(checked, total int) { send(ScanEvent{Type: "progress", Checked: checked, Total: total}) },
	)

	send(ScanEvent{Type: "done", Count: len(devices)})
}

func handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	cfgMu.RLock()
	devices := append([]DeviceConfig{}, cfg.Devices...)
	cfgMu.RUnlock()

	// Index-assigned rather than appended: append order is whichever device
	// answered first, which reshuffles the list on every poll.
	statuses := make([]DeviceStatus, len(devices))
	var wg sync.WaitGroup
	for i, dev := range devices {
		wg.Add(1)
		go func(i int, d DeviceConfig) {
			defer wg.Done()
			statuses[i] = getDeviceStatus(r.Context(), d)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Trim like normalizeConfig does. deviceKey is built from these, so an
	// untrimmed name from the UI's unsaved edits filed the cast error under a
	// key nothing else ever reads — the failure never reached the device card,
	// and pruneCastStates never collected it.
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" || req.URL == "" {
		http.Error(w, "name and url required", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		args := append(cattDeviceArgs(dev), "cast_site", req.URL)
		// context.Background, not r.Context: this outlives the response we are
		// about to write, and the request context is cancelled the moment the
		// handler returns.
		out, err := runCatt(context.Background(), 30*time.Second, args...)
		if err != nil {
			log.Printf("cast %q -> %s: %v — %s", req.Name, req.URL, err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, true)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		args := append(cattDeviceArgs(dev), "stop")
		out, err := runCatt(context.Background(), 15*time.Second, args...)
		if err != nil {
			log.Printf("stop %q: %v — %s", req.Name, err, strings.TrimSpace(out))
			setCastError(dev, cattFailure(err, out))
		} else {
			setCastState(dev, false)
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
