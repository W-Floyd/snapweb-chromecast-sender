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

	// castStates tracks devices we have actively cast to.
	// catt gives no signal for web-page cast state, so we track it ourselves.
	castStates   = map[string]bool{}
	castStatesMu sync.RWMutex
)

func setCastState(name string, playing bool) {
	castStatesMu.Lock()
	castStates[name] = playing
	castStatesMu.Unlock()
}

func isCasting(name string) bool {
	castStatesMu.RLock()
	defer castStatesMu.RUnlock()
	return castStates[name]
}

func loadConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("config read error: %v", err)
		}
		cfg = Config{CheckInterval: 60, Devices: []DeviceConfig{}}
		return
	}
	// Unmarshal into a scratch value so a malformed file cannot leave cfg
	// half-populated with a mix of defaults and file contents.
	loaded := Config{CheckInterval: 60}
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("config parse error: %v", err)
		cfg = Config{CheckInterval: 60, Devices: []DeviceConfig{}}
		return
	}
	if loaded.Devices == nil {
		loaded.Devices = []DeviceConfig{}
	}
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

func runCatt(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "catt", args...).CombinedOutput()
	return string(out), err
}

func getDeviceStatus(dev DeviceConfig) DeviceStatus {
	if dev.Host != "" {
		return getPychromecastStatus(dev)
	}
	// Fall back to catt when no host IP is configured.
	args := append(cattDeviceArgs(dev), "status")
	out, err := runCatt(10*time.Second, args...)
	ds := DeviceStatus{Name: dev.Name, State: "unknown"}
	if err != nil {
		ds.Error = strings.TrimSpace(out)
		return ds
	}
	if isCasting(dev.Name) {
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

func getPychromecastStatus(dev DeviceConfig) DeviceStatus {
	ds := DeviceStatus{Name: dev.Name, State: "unknown"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Keep stderr out of stdout: zeroconf/pychromecast log lines and Python
	// warnings land on stderr, and mixing them into stdout makes the JSON
	// unparseable even when the query itself succeeded.
	cmd := exec.CommandContext(ctx, "python3", statusScript, dev.Host)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var result struct {
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
		setCastState(dev.Name, false)
	} else {
		ds.State = result.DisplayName
		if ds.State == "" {
			ds.State = "Playing"
		}
		setCastState(dev.Name, true)
	}
	return ds
}

func monitorDevices() {
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
		if dev.Host != "" {
			getPychromecastStatus(dev)
		}
		if isCasting(dev.Name) {
			continue
		}
		log.Printf("auto-casting to %q: %s", dev.Name, url)
		args := append(cattDeviceArgs(dev), "cast_site", url)
		out, err := runCatt(30*time.Second, args...)
		if err != nil {
			log.Printf("cast error for %q: %v — %s", dev.Name, err, strings.TrimSpace(out))
		} else {
			setCastState(dev.Name, true)
		}
	}
}

func monitorLoop() {
	for {
		monitorDevices()
		cfgMu.RLock()
		interval := cfg.CheckInterval
		cfgMu.RUnlock()
		if interval <= 0 {
			interval = 60
		}
		// Floor the interval: a small or hand-edited value would otherwise
		// hammer every device with status queries in a near-hot loop.
		if interval < minCheckInterval {
			interval = minCheckInterval
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		json.NewEncoder(w).Encode(cfg)
	case http.MethodPost:
		var newCfg Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if newCfg.Devices == nil {
			newCfg.Devices = []DeviceConfig{}
		}
		cfgMu.Lock()
		cfg = newCfg
		cfgMu.Unlock()
		if err := saveConfig(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func cattScan() []DiscoveredDevice {
	out, err := runCatt(30*time.Second, "scan")
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
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			cur.Name = strings.TrimSpace(after)
			continue
		}
		if after, ok := strings.CutPrefix(line, "Host:"); ok {
			cur.Host = strings.TrimSpace(after)
			if cur.Name != "" {
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

// localSubnets returns the /24 base (e.g. "192.168.1") for each non-loopback IPv4 interface.
func localSubnets() []string {
	ifaces, _ := net.Interfaces()
	var subnets []string
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
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
			if ip == nil || ip.To4() == nil {
				continue
			}
			parts := strings.Split(ip.To4().String(), ".")
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

	// Parse optional explicit subnet (skips catt scan when provided).
	var explicitSubnets []string
	if s := r.URL.Query().Get("subnet"); s != "" {
		if base := parseSubnet(s); base != "" {
			explicitSubnets = []string{base}
		} else {
			send(ScanEvent{Type: "status", Message: "Invalid subnet — expected e.g. 192.168.1.0/24"})
			send(ScanEvent{Type: "done", Count: 0})
			return
		}
	}

	var devices []DiscoveredDevice

	if len(explicitSubnets) == 0 {
		send(ScanEvent{Type: "status", Message: "Running mDNS scan via catt..."})
		devices = cattScan()
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

	statuses := make([]DeviceStatus, 0, len(devices))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, dev := range devices {
		wg.Add(1)
		go func(d DeviceConfig) {
			defer wg.Done()
			s := getDeviceStatus(d)
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		}(dev)
	}
	wg.Wait()
	json.NewEncoder(w).Encode(statuses)
}

func handleCast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
		Host string `json:"host"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.URL == "" {
		http.Error(w, "name and url required", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		args := append(cattDeviceArgs(dev), "cast_site", req.URL)
		out, err := runCatt(30*time.Second, args...)
		if err != nil {
			log.Printf("cast %q -> %s: %v — %s", req.Name, req.URL, err, strings.TrimSpace(out))
		} else {
			setCastState(req.Name, true)
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
	var req struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	go func() {
		dev := DeviceConfig{Name: req.Name, Host: req.Host}
		args := append(cattDeviceArgs(dev), "stop")
		out, err := runCatt(15*time.Second, args...)
		if err != nil {
			log.Printf("stop %q: %v — %s", req.Name, err, strings.TrimSpace(out))
		} else {
			setCastState(req.Name, false)
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
