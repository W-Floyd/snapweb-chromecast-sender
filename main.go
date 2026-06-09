package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	cfg     Config
	cfgMu   sync.RWMutex
	cfgPath = "/config/config.json"
)

func loadConfig() {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("config read error: %v", err)
		}
		cfg = Config{CheckInterval: 60}
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("config parse error: %v", err)
	}
	if cfg.Devices == nil {
		cfg.Devices = []DeviceConfig{}
	}
}

func saveConfig() error {
	cfgMu.RLock()
	data, err := json.MarshalIndent(cfg, "", "  ")
	cfgMu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0644)
}

// cattDeviceArgs returns the catt flags to target a specific device.
// Uses --host <ip> when available (bypasses mDNS), otherwise -d <name>.
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
	args := append(cattDeviceArgs(dev), "status")
	out, err := runCatt(10*time.Second, args...)
	ds := DeviceStatus{Name: dev.Name, State: "unknown"}
	if err != nil {
		ds.Error = strings.TrimSpace(out)
		return ds
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

func monitorDevices() {
	cfgMu.RLock()
	interval := cfg.CheckInterval
	devices := append([]DeviceConfig{}, cfg.Devices...)
	defaultURL := cfg.DefaultURL
	cfgMu.RUnlock()

	if interval <= 0 {
		interval = 60
	}

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
		status := getDeviceStatus(dev)
		if strings.EqualFold(status.State, "idle") {
			log.Printf("auto-casting to %q: %s", dev.Name, url)
			args := append(cattDeviceArgs(dev), "cast_site", url)
			out, err := runCatt(30*time.Second, args...)
			if err != nil {
				log.Printf("cast error for %q: %v — %s", dev.Name, err, strings.TrimSpace(out))
			}
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
	var devices []DiscoveredDevice
	var cur DiscoveredDevice
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			cur.Name = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "Host:"); ok {
			cur.Host = strings.TrimSpace(after)
			if cur.Name != "" {
				devices = append(devices, cur)
				cur = DiscoveredDevice{}
			}
		}
	}
	return devices
}

// localSubnets returns the /24 base (e.g. "192.168.1") for each non-loopback IPv4 interface.
func localSubnets() []string {
	ifaces, _ := net.Interfaces()
	var subnets []string
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
			parts := strings.Split(ip.String(), ".")
			if len(parts) == 4 {
				subnets = append(subnets, strings.Join(parts[:3], "."))
			}
		}
	}
	return subnets
}

type ScanEvent struct {
	Type    string            `json:"type"`
	Message string            `json:"message,omitempty"`
	Device  *DiscoveredDevice `json:"device,omitempty"`
	Checked int               `json:"checked,omitempty"`
	Total   int               `json:"total,omitempty"`
	Count   int               `json:"count"`
}

// parseSubnet accepts "192.168.1", "192.168.1.0/24", or "192.168.1.x" and
// returns the three-octet base ("192.168.1"), or "" if unparseable.
// parseSubnet accepts any of: "192.168.1", "192.168.1.0/24", "192.168.1.x",
// or any host address in the subnet, and returns the three-octet base ("192.168.1").
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
	// Accept bare three-octet base like "192.168.1"
	parts := strings.Split(s, ".")
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
		return s
	}
	return ""
}

func handleSubnets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	bases := localSubnets()
	cidrs := make([]string, len(bases))
	for i, b := range bases {
		cidrs[i] = b + ".0/24"
	}
	if cidrs == nil {
		cidrs = []string{}
	}
	json.NewEncoder(w).Encode(cidrs)
}

// tcpScan probes each host in subnets on port 8008.
// Pass nil subnets to auto-detect from local interfaces.
// onStatus receives human-readable status lines (including subnet info).
// onFound is called immediately when a device is confirmed.
// onProgress is called every ~500ms with (checked, total) counts.
func tcpScan(subnets []string, onStatus func(string), onFound func(DiscoveredDevice), onProgress func(int, int)) []DiscoveredDevice {
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

				conn, err := net.DialTimeout("tcp", h+":8008", 400*time.Millisecond)
				if err != nil {
					return
				}
				conn.Close()

				resp, err := client.Get("http://" + h + ":8008/setup/eureka_info")
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

	var writeMu sync.Mutex
	send := func(evt ScanEvent) {
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
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

func main() {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
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
	mux.Handle("/", http.FileServer(http.Dir("/static")))

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	log.Printf("chromecast-sender running on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
