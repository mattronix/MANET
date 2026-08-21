package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	alfredBin    = "/usr/sbin/alfred"
	batctlBin    = "/usr/sbin/batctl"
	alfredType   = "68"
	registryFile = "/var/run/mesh_node_registry"
	stateFile    = "/var/lib/manet/state.json"
	nodesFile    = "/var/lib/manet/known_nodes.json"
	confFile     = "/etc/mesh.conf"
	appletsDir   = "/usr/local/share/manet/applets"
	interval     = 15 * time.Second
)

type NodeInfo struct {
	Hostname               string `json:"hostname"`
	MAC                    string `json:"mac"`
	MACAddresses           string `json:"mac_addresses"`
	IPv4                   string `json:"ipv4"`
	IPv4Chunk              string `json:"ipv4_chunk"`
	Uptime                 string `json:"uptime_seconds"`
	Battery                string `json:"battery_percentage"`
	CPULoad                string `json:"cpu_load"`
	IsGateway              string `json:"is_gateway"`
	GatewayIface           string `json:"gateway_iface"`
	IsNTP                  string `json:"is_ntp"`
	GPSLat                 string `json:"gps_lat"`
	GPSLon                 string `json:"gps_lon"`
	GPSAlt                 string `json:"gps_alt"`
	Ch2G                   string `json:"ch_2g"`
	Ch5G                   string `json:"ch_5g"`
	IsLimp                 string `json:"is_limp"`
	Timestamp              string `json:"timestamp"`
	Applets                string `json:"applets,omitempty"`
	HalowTxMCS             string `json:"halow_tx_mcs,omitempty"`
	HalowRxMCS             string `json:"halow_rx_mcs,omitempty"`
	HalowMCSPeer           string `json:"halow_mcs_peer,omitempty"`
	Wifi24TxMCS            string `json:"wifi_24_tx_mcs,omitempty"`
	Wifi24RxMCS            string `json:"wifi_24_rx_mcs,omitempty"`
	Wifi5TxMCS             string `json:"wifi_5_tx_mcs,omitempty"`
	Wifi5RxMCS             string `json:"wifi_5_rx_mcs,omitempty"`
	TQAverage              string `json:"tq_average,omitempty"`
	Neighbors              string `json:"neighbors,omitempty"`
	ConfigAck              string `json:"config_ack,omitempty"`
	ChannelReport          string `json:"channel_report,omitempty"`
	LastTourguideTimestamp string `json:"last_tourguide_timestamp,omitempty"`
	LastTourguideRadio     string `json:"last_tourguide_radio,omitempty"`
	PartitionSize          string `json:"partition_size,omitempty"`
}

var Version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(Version)
		return
	}

	log.SetFlags(log.Ldate | log.Ltime)
	log.Printf("mesh-registry starting (version %s)", Version)

	loadKnownNodes()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(interval)
	defer tick.Stop()

	run()
	for {
		select {
		case <-tick.C:
			run()
		case <-sig:
			log.Println("shutting down")
			saveKnownNodes()
			return
		}
	}
}

func run() {
	info := collectLocal()
	publish(info)
	peers := readPeers()
	writeRegistry(info, peers)
}

func collectLocal() NodeInfo {
	hostname, _ := os.Hostname()
	mac := getMyMAC()
	allMACs := getAllMACs()
	ip := getIPv4()
	uptime := getUptimeSeconds()
	battery := getBatteryPct()
	cpu := getCPULoad()
	isGW, gwIface := getGatewayInfo()
	gpsLat, gpsLon, gpsAlt := getGPS()

	mcs := collectMCS()
	tourguideState := loadKV("/var/run/tourguide_state")

	return NodeInfo{
		Hostname:               hostname,
		MAC:                    mac,
		MACAddresses:           allMACs,
		IPv4:                   ip,
		Uptime:                 uptime,
		Battery:                battery,
		CPULoad:                cpu,
		IsGateway:              isGW,
		GatewayIface:           gwIface,
		IsNTP:                  boolStr(serviceActive("ntp") || serviceActive("chrony") || serviceActive("systemd-timesyncd")),
		GPSLat:                 gpsLat,
		GPSLon:                 gpsLon,
		GPSAlt:                 gpsAlt,
		Ch2G:                   getChannel("2.4"),
		Ch5G:                   getChannel("5"),
		IsLimp:                 boolStr(fileExists("/var/run/mesh_limp_mode")),
		Timestamp:              fmt.Sprintf("%d", time.Now().Unix()),
		Applets:                scanApplets(),
		HalowTxMCS:             mcs["WLAN2_TX_MCS"],
		HalowRxMCS:             mcs["WLAN2_RX_MCS"],
		HalowMCSPeer:           mcs["WLAN2_MCS_PEER"],
		Wifi24TxMCS:            mcs["WLAN0_TX_MCS"],
		Wifi24RxMCS:            mcs["WLAN0_RX_MCS"],
		Wifi5TxMCS:             mcs["WLAN1_TX_MCS"],
		Wifi5RxMCS:             mcs["WLAN1_RX_MCS"],
		TQAverage:              getTQAverage(),
		Neighbors:              getDirectNeighbors(),
		ConfigAck:              readFileStr("/var/run/mesh_config_ack_version"),
		ChannelReport:          readFileStr("/var/run/mesh_channel_report.json"),
		LastTourguideTimestamp: tourguideState["LAST_TOURGUIDE_TIME"],
		LastTourguideRadio:     tourguideState["LAST_TOURGUIDE_RADIO"],
		PartitionSize:          readFileStr("/var/run/mesh_partition_size"),
	}
}

func publish(info NodeInfo) {
	data, err := json.Marshal(info)
	if err != nil {
		log.Printf("marshal: %v", err)
		return
	}
	cmd := exec.Command(alfredBin, "-s", alfredType)
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("alfred publish: %v: %s", err, out)
	}
}

var alfredRE = regexp.MustCompile(`\{\s*"([^"]+)",\s*"((?:[^"\\]|\\.)*)"\s*\}`)

func readPeers() map[string]NodeInfo {
	peers := make(map[string]NodeInfo)
	out, err := exec.Command(alfredBin, "-r", alfredType).Output()
	if err != nil {
		return peers
	}
	for _, m := range alfredRE.FindAllStringSubmatch(string(out), -1) {
		mac := m[1]
		payload := unescapeAlfred(m[2])
		var info NodeInfo
		if json.Unmarshal([]byte(payload), &info) == nil {
			peers[mac] = info
		}
	}
	return peers
}

func unescapeAlfred(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\x0a`, "\n")
	s = strings.ReplaceAll(s, `\x09`, "\t")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

var knownNodes = make(map[string]NodeInfo)
var prevLive = make(map[string]bool)

// Offline entries older than this are dropped from the registry entirely.
const offlineMaxAge = 7 * 24 * time.Hour

func normMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(mac), "-", ":"))
}

// macSet returns every MAC identifying a node (bat0 + physical interfaces),
// normalized. Physical MACs are burned-in, so they identify the same box even
// after the bat0 MAC changes across a reboot or reflash.
func macSet(n NodeInfo) map[string]bool {
	set := make(map[string]bool)
	if m := normMAC(n.MAC); m != "" {
		set[m] = true
	}
	for _, m := range strings.Split(n.MACAddresses, ",") {
		if m = normMAC(m); m != "" {
			set[m] = true
		}
	}
	return set
}

func loadKnownNodes() {
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		return
	}
	var nodes map[string]NodeInfo
	if json.Unmarshal(data, &nodes) == nil {
		// Normalize keys so entries written by older builds (mixed case /
		// dash-separated MACs) can't coexist with their normalized twin.
		for mac, n := range nodes {
			knownNodes[normMAC(mac)] = n
		}
		log.Printf("loaded %d known nodes from disk", len(knownNodes))
	}
}

const knownNodesSaveInterval = 5 * time.Minute

var (
	lastSavedNodeSet string
	lastSaveTime     time.Time
)

// maybeSaveKnownNodes persists knownNodes to disk only when the set of known
// MACs has changed, or every knownNodesSaveInterval as a fallback so
// persisted LAST_SEEN timestamps don't go stale for too long across a crash.
// A per-tick full-content diff isn't useful here: every live node's own
// Timestamp/Uptime/telemetry fields change on nearly every call, so it would
// almost never gate anything and this file was hitting the SD card every
// 15s regardless of whether the mesh actually changed.
func maybeSaveKnownNodes() {
	macs := make([]string, 0, len(knownNodes))
	for mac := range knownNodes {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	nodeSet := strings.Join(macs, ",")

	if nodeSet == lastSavedNodeSet && time.Since(lastSaveTime) < knownNodesSaveInterval {
		return
	}
	lastSavedNodeSet = nodeSet
	lastSaveTime = time.Now()
	saveKnownNodes()
}

func saveKnownNodes() {
	data, err := json.Marshal(knownNodes)
	if err != nil {
		log.Printf("marshal known nodes: %v", err)
		return
	}
	os.MkdirAll(filepath.Dir(nodesFile), 0755)
	tmp := nodesFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("write known nodes: %v", err)
		return
	}
	os.Rename(tmp, nodesFile)
}

func writeRegistry(self NodeInfo, peers map[string]NodeInfo) {
	// Build set of live MACs from current alfred data
	selfMAC := normMAC(self.MAC)
	liveMACs := make(map[string]bool)
	liveMACs[selfMAC] = true
	for _, p := range peers {
		if m := normMAC(p.MAC); m != "" {
			liveMACs[m] = true
		}
	}

	// Merge current peers into known nodes, keyed by normalized NodeInfo.MAC.
	knownNodes[selfMAC] = self
	for _, p := range peers {
		if m := normMAC(p.MAC); m != "" && m != selfMAC {
			knownNodes[m] = p
		}
	}

	// Purge stale entries that are old identities of a live node: same box
	// after a bat0 MAC change (any shared physical MAC) or same claimed IP.
	// Also expire offline entries not seen for offlineMaxAge.
	now := time.Now().Unix()
	for mac, stale := range knownNodes {
		if liveMACs[mac] {
			continue
		}
		staleMACs := macSet(stale)
		drop := false
		for liveMac := range liveMACs {
			live, ok := knownNodes[liveMac]
			if !ok || liveMac == mac {
				continue
			}
			if stale.IPv4 != "" && live.IPv4 == stale.IPv4 {
				drop = true
				break
			}
			for m := range macSet(live) {
				if staleMACs[m] {
					drop = true
					break
				}
			}
			if drop {
				break
			}
		}
		if !drop {
			if ts, err := strconv.ParseInt(stale.Timestamp, 10, 64); err == nil && now-ts > int64(offlineMaxAge.Seconds()) {
				drop = true
			}
		}
		if drop {
			log.Printf("purging stale node %s (%s / %s)", mac, stale.Hostname, stale.IPv4)
			delete(knownNodes, mac)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Mesh Node Registry - Generated %s\n", time.Now().Format(time.RFC1123))
	fmt.Fprintln(&b, "# Sourced by other scripts to get network state.")
	fmt.Fprintln(&b)

	for mac, n := range knownNodes {
		isLive := liveMACs[mac]
		if !isLive {
			n.IsLimp = "false"
		}
		writeNodeWithState(&b, n, isLive)
	}

	tmp := registryFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0644); err != nil {
		log.Printf("write registry: %v", err)
		return
	}
	os.Rename(tmp, registryFile)
	maybeSaveKnownNodes()

	// Emit peer-join / peer-leave events
	for mac := range liveMACs {
		if mac == selfMAC {
			continue
		}
		if !prevLive[mac] {
			n := knownNodes[mac]
			go meshHook("peer-join", "MAC="+mac, "IP="+n.IPv4, "HOSTNAME="+n.Hostname)
		}
	}
	for mac := range prevLive {
		if !liveMACs[mac] {
			n := knownNodes[mac]
			go meshHook("peer-leave", "MAC="+mac, "IP="+n.IPv4, "HOSTNAME="+n.Hostname)
		}
	}
	prevLive = make(map[string]bool)
	for mac := range liveMACs {
		if mac != selfMAC {
			prevLive[mac] = true
		}
	}
}

func writeNodeWithState(b *strings.Builder, n NodeInfo, isLive bool) {
	if n.MAC == "" {
		return
	}
	prefix := "NODE_" + strings.ReplaceAll(strings.ReplaceAll(n.MAC, ":", ""), "-", "")
	w := func(field, val string) {
		fmt.Fprintf(b, "%s_%s='%s'\n", prefix, field, val)
	}
	w("HOSTNAME", n.Hostname)
	w("MAC_ADDRESS", n.MAC)
	w("MAC_ADDRESSES", n.MACAddresses)
	w("IPV4_ADDRESS", n.IPv4)
	w("IPV4_CHUNK", n.IPv4Chunk)
	w("UPTIME_SECONDS", n.Uptime)
	w("BATTERY_PERCENTAGE", n.Battery)
	w("CPU_LOAD_AVERAGE", n.CPULoad)
	w("IS_GATEWAY", n.IsGateway)
	w("GATEWAY_IFACE", n.GatewayIface)
	w("IS_NTP_SERVER", n.IsNTP)
	w("GPS_LATITUDE", n.GPSLat)
	w("GPS_LONGITUDE", n.GPSLon)
	w("GPS_ALTITUDE", n.GPSAlt)
	w("DATA_CHANNEL_2_4", n.Ch2G)
	w("DATA_CHANNEL_5_0", n.Ch5G)
	w("IS_IN_LIMP_MODE", n.IsLimp)
	w("LAST_SEEN_TIMESTAMP", n.Timestamp)
	state := "ACTIVE"
	if !isLive {
		state = "OFFLINE"
	}
	w("NODE_STATE", state)
	w("APPLETS", n.Applets)
	w("HALOW_TX_MCS", n.HalowTxMCS)
	w("HALOW_RX_MCS", n.HalowRxMCS)
	w("HALOW_MCS_PEER", n.HalowMCSPeer)
	w("WIFI_24_TX_MCS", n.Wifi24TxMCS)
	w("WIFI_24_RX_MCS", n.Wifi24RxMCS)
	w("WIFI_5_TX_MCS", n.Wifi5TxMCS)
	w("WIFI_5_RX_MCS", n.Wifi5RxMCS)
	w("TQ_AVERAGE", n.TQAverage)
	w("DIRECT_NEIGHBORS", n.Neighbors)
	w("CONFIG_ACK_VERSION", n.ConfigAck)
	w("CHANNEL_REPORT_JSON", n.ChannelReport)
	w("LAST_TOURGUIDE_TIMESTAMP", n.LastTourguideTimestamp)
	w("LAST_TOURGUIDE_RADIO", n.LastTourguideRadio)
	w("PARTITION_SIZE", n.PartitionSize)
	fmt.Fprintln(b)
}

// --- System data collection ---

func meshHook(event string, args ...string) {
	cmdArgs := append([]string{event}, args...)
	exec.Command("/usr/local/bin/mesh-hook", cmdArgs...).Run()
}

func getMyMAC() string {
	for _, name := range []string{"bat0", "br0"} {
		iface, err := net.InterfaceByName(name)
		if err == nil && len(iface.HardwareAddr) > 0 {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}

func getAllMACs() string {
	var macs []string
	for _, name := range []string{"bat0", "br0", "wlan0", "wlan1", "wlan2", "wlan3"} {
		iface, err := net.InterfaceByName(name)
		if err == nil && len(iface.HardwareAddr) > 0 {
			macs = append(macs, iface.HardwareAddr.String())
		}
	}
	return strings.Join(macs, ",")
}

func getIPv4() string {
	// Try state file first
	if data, err := os.ReadFile(stateFile); err == nil {
		var state map[string]string
		if json.Unmarshal(data, &state) == nil {
			if ip := state["CURRENT_IPV4"]; ip != "" {
				return ip
			}
			if ip := state["PERSISTENT_IPV4"]; ip != "" {
				return ip
			}
		}
	}
	// Try reading state file as KV
	if kv := loadKV(stateFile); kv["CURRENT_IPV4"] != "" {
		return kv["CURRENT_IPV4"]
	}
	// Fall back to br0 address
	iface, err := net.InterfaceByName("br0")
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func getUptimeSeconds() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// battery-reader (src/battery-reader) writes its own status file directly —
// it doesn't register as a kernel power_supply device, so there's no
// /sys/class/power_supply/BAT0 on these boards. This previously read that
// path anyway (a leftover from a generic/laptop assumption), which meant
// BATTERY_PERCENTAGE was silently empty for every node, always — nothing
// ever broadcast a peer's battery status to the rest of the mesh.
//
// run() ticks every 15s for the rest of the registry (neighbors, uptime,
// etc.), but battery percentage doesn't need to be that fresh and reading
// it more often than necessary just adds needless I2C bus traffic on top
// of battery-reader's own polling — cache it and only re-read every
// batteryCacheDuration. collectLocal() runs on a single goroutine (the
// main ticker loop), so no locking is needed for the cache.
const batteryCacheDuration = 5 * time.Minute

var (
	batteryCacheVal  string
	batteryCacheTime time.Time
)

func getBatteryPct() string {
	if !batteryCacheTime.IsZero() && time.Since(batteryCacheTime) < batteryCacheDuration {
		return batteryCacheVal
	}
	batteryCacheTime = time.Now()

	data, err := os.ReadFile("/run/battery_status.json")
	if err != nil {
		batteryCacheVal = ""
		return batteryCacheVal
	}
	var bs struct {
		Percentage *int `json:"percentage"`
	}
	if json.Unmarshal(data, &bs) != nil || bs.Percentage == nil {
		batteryCacheVal = ""
		return batteryCacheVal
	}
	batteryCacheVal = strconv.Itoa(*bs.Percentage)
	return batteryCacheVal
}

func getCPULoad() string {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func getGatewayInfo() (string, string) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "false", ""
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "00000000" {
			iface := fields[0]
			if iface == "br0" || iface == "bat0" {
				continue
			}
			return "true", iface
		}
	}
	return "false", ""
}

// gpspipe normally exits after -n 5 reports; if gpsd is present but stuck
// (no fix, wedged receiver) it can instead block forever, and since this
// runs synchronously at the top of every run(), a hung gpspipe freezes the
// entire registry daemon indefinitely — confirmed live on a node where
// mesh-registry never wrote /var/run/mesh_node_registry even once across
// 2+ hours of uptime, with a single gpspipe child stuck the whole time.
const gpsTimeout = 5 * time.Second

func getGPS() (string, string, string) {
	conf := loadKV(confFile)
	if conf["gps"] == "n" {
		return "", "", ""
	}
	if conf["gps_source"] == "static" {
		// No receiver to query — report the configured fixed position
		// directly, so peers gossiped this node's registry entry see the
		// same static location gps-reader is writing locally.
		return conf["gps_static_lat"], conf["gps_static_lon"], conf["gps_static_alt"]
	}

	ctx, cancel := context.WithTimeout(context.Background(), gpsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gpspipe", "-w", "-n", "5").Output()
	if err != nil {
		return "", "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, `"class":"TPV"`) {
			continue
		}
		var tpv map[string]interface{}
		if json.Unmarshal([]byte(line), &tpv) != nil {
			continue
		}
		mode, _ := tpv["mode"].(float64)
		if mode < 2 {
			continue
		}
		lat := fmt.Sprintf("%f", tpv["lat"])
		lon := fmt.Sprintf("%f", tpv["lon"])
		alt := ""
		if a, ok := tpv["altMSL"].(float64); ok {
			alt = fmt.Sprintf("%.1f", a)
		} else if a, ok := tpv["alt"].(float64); ok {
			alt = fmt.Sprintf("%.1f", a)
		}
		return lat, lon, alt
	}
	return "", "", ""
}

func getChannel(band string) string {
	out, err := exec.Command("iw", "dev").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if strings.Contains(line, "channel") {
			ch := strings.TrimSpace(line)
			freq := 0
			if parts := strings.Fields(ch); len(parts) >= 2 {
				if f, err := strconv.Atoi(parts[1]); err == nil {
					freq = f
				}
				// Try extracting from parenthetical
				for _, p := range parts {
					p = strings.Trim(p, "()")
					if f, err := strconv.Atoi(p); err == nil && f > 100 {
						freq = f
					}
				}
			}
			if band == "2.4" && freq >= 2400 && freq <= 2500 {
				return extractChannelNum(lines, i)
			}
			if band == "5" && freq >= 5000 && freq <= 6000 {
				return extractChannelNum(lines, i)
			}
		}
	}
	return ""
}

func extractChannelNum(lines []string, idx int) string {
	line := strings.TrimSpace(lines[idx])
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

func serviceActive(name string) bool {
	err := exec.Command("systemctl", "is-active", "--quiet", name).Run()
	return err == nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFileStr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func collectMCS() map[string]string {
	result := make(map[string]string)
	for _, iface := range []string{"wlan0", "wlan1", "wlan2"} {
		if !fileExists("/sys/class/net/" + iface) {
			continue
		}
		out, err := exec.Command("/usr/local/bin/halow-mcs-summary", "--iface", iface, "--shell").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				result[k] = strings.Trim(v, "'")
			}
		}
	}
	return result
}

// getDirectNeighbors formats each entry as MAC[=tq_or_throughput[=iface]].
// The iface segment lets the topology UI show which physical radio (HaLow
// vs standard WiFi mesh) actually carries the link between two *other*
// nodes, not just links to this node itself — "batctl n" already reports
// it per neighbor, e.g. "9c:04:b6:a0:aa:13    0.040s (  32.5) [ wlan2]".
func getDirectNeighbors() string {
	out, err := exec.Command(batctlBin, "n").Output()
	if err != nil {
		return ""
	}
	var entries []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.Contains(fields[0], ":") {
			entry := fields[0]
			if len(fields) >= 4 {
				speed := strings.Trim(fields[3], "()")
				if _, err := strconv.ParseFloat(speed, 64); err == nil {
					entry += "=" + speed
					if len(fields) >= 6 {
						if iface := strings.Trim(fields[5], "[]"); iface != "" {
							entry += "=" + iface
						}
					}
				}
			}
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, ",")
}

// tqLineRE matches a `batctl o` originator row and captures the leading
// selected-route marker, the originator MAC, and the parenthesised
// throughput value. A fixed field index doesn't work here for two
// independent reasons: the "*" marking the batman-selected route shifts
// every later column right by one, and this batctl version pads the
// throughput value with a leading space inside the parens
// ("(        3.1)"), which splits "(" into its own whitespace-delimited
// field even on unstarred rows — so strings.Fields()[2] never lands on the
// number either way. A regex on the parenthesised value sidesteps both.
var tqLineRE = regexp.MustCompile(`^\s*(\*?)\s*([0-9a-fA-F:]{17})\s+[\d.]+s\s*\(\s*([\d.]+)\)`)

func getTQAverage() string {
	out, err := exec.Command("/usr/sbin/batctl", "o").Output()
	if err != nil {
		return "0"
	}

	// batctl o lists every candidate next-hop path per originator; only the
	// "*"-marked row is the one batman actually routes through. Average
	// just the selected route per originator — averaging in unselected
	// alternate paths would distort what's meant to be "mean throughput to
	// reachable neighbors".
	best := make(map[string]float64)
	for _, line := range strings.Split(string(out), "\n") {
		m := tqLineRE.FindStringSubmatch(line)
		if m == nil || m[1] != "*" {
			continue
		}
		if v, err := strconv.ParseFloat(m[3], 64); err == nil {
			best[m[2]] = v
		}
	}

	if len(best) == 0 {
		return "0"
	}
	var sum float64
	for _, v := range best {
		sum += v
	}
	return fmt.Sprintf("%.2f", sum/float64(len(best)))
}

func loadKV(path string) map[string]string {
	m := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			v = strings.Trim(v, "'\"")
			m[k] = v
		}
	}
	return m
}

func scanApplets() string {
	entries, err := os.ReadDir(appletsDir)
	if err != nil {
		return ""
	}
	var parts []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(appletsDir, e.Name(), "applet.json"))
		if err != nil {
			continue
		}
		var m struct {
			Name    string `json:"name"`
			Label   string `json:"label"`
			Backend struct {
				Service string `json:"service"`
			} `json:"backend"`
		}
		if json.Unmarshal(data, &m) != nil || m.Name == "" {
			continue
		}
		label := m.Label
		if label == "" {
			label = m.Name
		}
		svc := m.Backend.Service
		if svc == "" {
			svc = m.Name + ".service"
		}
		status := "unknown"
		if exec.Command("systemctl", "is-active", "--quiet", svc).Run() == nil {
			status = "running"
		} else {
			status = "stopped"
		}
		parts = append(parts, m.Name+"|"+label+"|"+status)
	}
	return strings.Join(parts, ",")
}
