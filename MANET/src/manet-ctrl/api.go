package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readBody(r *http.Request) map[string]interface{} {
	m := make(map[string]interface{})
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		return m
	}
	json.Unmarshal(body, &m)
	return m
}

func jsonStr(m map[string]interface{}, key, def string) string {
	if v, ok := m[key]; ok {
		switch s := v.(type) {
		case string:
			return s
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64)
		}
	}
	return def
}

func jsonFloat(m map[string]interface{}, key string, def float64) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case string:
			f, _ := strconv.ParseFloat(n, 64)
			return f
		}
	}
	return def
}

func jsonInt(m map[string]interface{}, key string, def int) int {
	return int(jsonFloat(m, key, float64(def)))
}

func jsonBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

var validateTargetRE = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)

// --- Status endpoints ---

func apiData(w http.ResponseWriter, r *http.Request) {
	data := cachedStatusData()
	writeJSON(w, 200, data)
}

func apiLocal(w http.ResponseWriter, r *http.Request) {
	data := cachedLocalData()
	writeJSON(w, 200, data)
}

func apiDaemons(w http.ResponseWriter, r *http.Request) {
	result := map[string]interface{}{}

	for _, d := range []struct {
		key  string
		path string
	}{
		{"gps", "/run/gps_status.json"},
		{"battery", "/run/battery_status.json"},
		{"cot_emitter", "/run/cot_emitter_status.json"},
	} {
		data, err := os.ReadFile(d.path)
		if err != nil {
			result[d.key] = map[string]interface{}{"available": false}
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) != nil {
			result[d.key] = map[string]interface{}{"available": false}
			continue
		}
		m["available"] = true
		result[d.key] = m
	}

	writeJSON(w, 200, result)
}

func apiPeer(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/peer/")
	if rest == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Missing peer IP"})
		return
	}
	peerIP := rest
	subPath := ""
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		peerIP = rest[:idx]
		subPath = rest[idx:]
	}
	if ip := net.ParseIP(peerIP); ip == nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid IP"})
		return
	}

	if subPath == "" {
		data := getPeerLocalData(peerIP, 2*time.Second)
		if data == nil {
			data = make(map[string]interface{})
		}
		writeJSON(w, 200, data)
		return
	}

	peerProxyRequest(w, r, peerIP, subPath)
}

func peerProxyRequest(w http.ResponseWriter, r *http.Request, peerIP, path string) {
	targetURL := fmt.Sprintf("https://%s%s", peerIP, path)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: peerTLSConfig,
		},
	}

	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		writeJSON(w, 502, map[string]interface{}{"ok": false, "error": "proxy request failed"})
		return
	}
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxyReq.ContentLength = r.ContentLength

	resp, err := client.Do(proxyReq)
	if err != nil {
		writeJSON(w, 502, map[string]interface{}{"ok": false, "error": "peer unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func apiVoice(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		body := readBody(r)
		action := jsonStr(body, "action", "")
		if action == "volume" {
			apiVoiceConfig(w, r, body)
			return
		}
		if !checkAuth(w, r) {
			return
		}
		apiVoiceConfig(w, r, body)
		return
	}
	writeJSON(w, 200, getVoiceStatus())
}

func apiVoiceConfig(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
	action := jsonStr(body, "action", "")

	if action == "ptt_on" || action == "ptt_off" {
		val := "0"
		if action == "ptt_on" {
			val = "1"
		}
		if err := os.WriteFile("/run/mesh-voice-ptt-remote", []byte(val), 0644); err != nil {
			writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
		return
	}

	if action == "start" || action == "stop" || action == "restart" {
		ok, errMsg := serviceAction("mesh-voice", action)
		writeJSON(w, 200, map[string]interface{}{"ok": ok, "error": errMsg})
		return
	}

	if action == "volume" {
		micVol := jsonStr(body, "mic_volume", "")
		spkVol := jsonStr(body, "speaker_volume", "")
		if micVol == "" && spkVol == "" {
			writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "no volume specified"})
			return
		}
		volUpdates := map[string]string{}
		if micVol != "" {
			volUpdates["voice_mic_volume"] = micVol
		}
		if spkVol != "" {
			volUpdates["voice_speaker_volume"] = spkVol
		}
		saveKVFile(MeshConfFile, volUpdates)
		applyVoiceVolume(loadKVFile(MeshConfFile))
		writeJSON(w, 200, map[string]interface{}{"ok": true})
		return
	}

	if action != "configure" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "unknown action"})
		return
	}

	ptt := jsonStr(body, "ptt_mode", "openvlm")
	iface := jsonStr(body, "interface", "br0")
	addr := jsonStr(body, "mcast_addr", "239.69.0.1")
	port := jsonStr(body, "port", "4370")
	micVol := jsonStr(body, "mic_volume", "")
	spkVol := jsonStr(body, "speaker_volume", "")

	validPTT := map[string]bool{"always": true, "gpio": true, "openvlm": true, "vox": true}
	if !validPTT[ptt] {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "invalid ptt_mode"})
		return
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`).MatchString(iface) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "invalid interface"})
		return
	}
	if net.ParseIP(addr) == nil || !strings.HasPrefix(addr, "239.") {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "invalid multicast address"})
		return
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1024 || portNum > 65535 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "invalid port"})
		return
	}

	// Persist PTT mode and volume to mesh.conf
	confUpdates := map[string]string{"voice_ptt_mode": ptt}
	if micVol != "" {
		confUpdates["voice_mic_volume"] = micVol
	}
	if spkVol != "" {
		confUpdates["voice_speaker_volume"] = spkVol
	}
	saveKVFile(MeshConfFile, confUpdates)
	if micVol != "" || spkVol != "" {
		applyVoiceVolume(loadKVFile(MeshConfFile))
	}

	execLine := fmt.Sprintf("/usr/local/bin/mesh-voice -iface %s -addr %s -port %s -ptt %s", iface, addr, port, ptt)

	unit := fmt.Sprintf(`[Unit]
Description=Mesh Voice PTT over multicast
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, execLine)

	if err := os.WriteFile("/etc/systemd/system/mesh-voice.service", []byte(unit), 0644); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	exec.Command("systemctl", "daemon-reload").Run()
	// mesh-voice exits cleanly on its own when voice_enabled=n, but restarting
	// a service that's about to immediately exit again is pointless churn —
	// the Config tab saves this same field via /api/admin/save in parallel
	// with this call, so mesh.conf may already reflect "disabled" here.
	if confGet(loadKVFile(MeshConfFile), "voice_enabled", "y") == "n" {
		exec.Command("systemctl", "stop", "mesh-voice").Run()
	} else {
		exec.Command("systemctl", "restart", "mesh-voice").Run()
	}

	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiAdminStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, assembleAdminStatus())
}

func apiFleetPreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, 200, loadFleetPreferences())
		return
	}
	body := readBody(r)
	prefs := loadFleetPreferences()
	if mc, ok := body["mesh_config"]; ok {
		if m, ok := mc.(map[string]interface{}); ok {
			prefs.MeshConfig = make(map[string]string, len(m))
			for k, v := range m {
				prefs.MeshConfig[k] = fmt.Sprintf("%v", v)
			}
		}
	}
	if np, ok := body["node_profiles"]; ok {
		if m, ok := np.(map[string]interface{}); ok {
			prefs.NodeProfiles = make(map[string]string, len(m))
			for k, v := range m {
				prefs.NodeProfiles[k] = fmt.Sprintf("%v", v)
			}
		}
	}
	if pr, ok := body["profiles"]; ok {
		if m, ok := pr.(map[string]interface{}); ok {
			prefs.Profiles = make(map[string]FleetProfile, len(m))
			for pid, pv := range m {
				if pm, ok := pv.(map[string]interface{}); ok {
					fp := FleetProfile{Name: fmt.Sprintf("%v", pm["name"])}
					fp.Config = make(map[string]string)
					if cfg, ok := pm["config"].(map[string]interface{}); ok {
						for k, v := range cfg {
							fp.Config[k] = fmt.Sprintf("%v", v)
						}
					}
					prefs.Profiles[pid] = fp
				}
			}
		}
	}
	if err := saveFleetPreferences(prefs); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"services": getAllServices()})
}

func meshMACLookup() map[string]map[string]string {
	lookup := make(map[string]map[string]string)
	reg := parseRegistry()
	for _, node := range reg {
		info := map[string]string{
			"hostname":  node["HOSTNAME"],
			"ip":        node["IPV4_ADDRESS"],
			"last_seen": node["LAST_SEEN_TIMESTAMP"],
		}
		if mac := normMAC(node["MAC_ADDRESS"]); mac != "" {
			lookup[mac] = info
		}
		for _, mac := range strings.Split(node["MAC_ADDRESSES"], ",") {
			mac = normMAC(mac)
			if mac != "" {
				lookup[mac] = info
			}
		}
	}
	return lookup
}

func apiRegistry(w http.ResponseWriter, r *http.Request) {
	registry := parseRegistry()
	myMAC := getMyMAC()

	type RegEntry struct {
		ID        string            `json:"id"`
		Fields    map[string]string `json:"fields"`
		IsMe      bool              `json:"is_me"`
	}

	var entries []RegEntry
	for id, rn := range registry {
		isMe := normMAC(rn["MAC_ADDRESS"]) == myMAC
		fields := make(map[string]string)
		for k, v := range rn {
			if k != "id" {
				fields[k] = v
			}
		}
		entries = append(entries, RegEntry{ID: id, Fields: fields, IsMe: isMe})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"nodes":     entries,
		"count":     len(entries),
		"timestamp": time.Now().Unix(),
	})
}

func apiMesh(w http.ResponseWriter, r *http.Request) {
	_, origMap := runBatctlOriginators()
	neighbors := runBatctlNeighbors()
	gateways := runBatctlGateways()
	conf := loadKVFile(MeshConfFile)
	macInfo := meshMACLookup()

	bat0 := map[string]string{"state": "unknown", "algo": "", "gw_mode": ""}
	var bat0Addrs []string

	if out, err := runCmdStdout(5*time.Second, "ip", "-br", "addr", "show", "bat0"); err == nil {
		fields := strings.Fields(out)
		if len(fields) >= 2 {
			bat0["state"] = fields[1]
		}
		for _, f := range fields[2:] {
			bat0Addrs = append(bat0Addrs, f)
		}
	}
	if out, err := runCmdStdout(5*time.Second, "batctl", "routing_algo"); err == nil {
		if m := regexp.MustCompile(`bat0:\s*(\S+)`).FindStringSubmatch(out); len(m) > 1 {
			bat0["algo"] = m[1]
		} else {
			bat0["algo"] = strings.TrimSpace(out)
		}
	}
	if out, err := runCmdStdout(5*time.Second, "batctl", "gw_mode"); err == nil {
		bat0["gw_mode"] = strings.TrimSpace(out)
	}

	enrichMAC := func(mac string) map[string]interface{} {
		m := map[string]interface{}{"mac": mac}
		if info, ok := macInfo[mac]; ok {
			m["hostname"] = info["hostname"]
			m["ip"] = info["ip"]
			m["last_seen"] = info["last_seen"]
		}
		return m
	}

	now := time.Now().Unix()

	var origList []map[string]interface{}
	for mac, o := range origMap {
		entry := enrichMAC(mac)
		entry["tq"] = o.TQ
		entry["nexthop"] = o.Nexthop
		entry["iface"] = o.Iface
		entry["last_seen"] = fmt.Sprintf("%d", now-int64(o.LastSeen))
		if nhInfo, ok := macInfo[o.Nexthop]; ok {
			entry["nexthop_hostname"] = nhInfo["hostname"]
		}
		origList = append(origList, entry)
	}

	stations := map[string]*StationLink{}
	seenIface := map[string]bool{}
	for _, n := range neighbors {
		if n.Iface == "" || seenIface[n.Iface] {
			continue
		}
		seenIface[n.Iface] = true
		for mac, st := range runStationDump(n.Iface) {
			stations[mac] = st
		}
	}
	halowBW := getHalowDriverInfo("wlan2")["halow_bw"]

	var neighList []map[string]interface{}
	for _, n := range neighbors {
		entry := enrichMAC(n.MAC)
		entry["tq"] = n.TQ
		entry["iface"] = n.Iface
		entry["last_seen"] = fmt.Sprintf("%d", now-int64(n.LastSeen))
		if st, ok := stations[n.MAC]; ok {
			entry["link"] = buildLinkBudget(st, n.Iface, halowBW)
		}
		neighList = append(neighList, entry)
	}

	var gwList []map[string]interface{}
	for _, gw := range gateways {
		entry := enrichMAC(gw.MAC)
		entry["tq"] = gw.TQ
		entry["selected"] = gw.Selected
		gwList = append(gwList, entry)
	}

	myHostname, _ := os.Hostname()
	state := loadKVFile(MeshStateFile)
	myIP := stateIP(state)

	var dnsRecords []map[string]interface{}
	reg := parseRegistry()
	for _, node := range reg {
		h := node["HOSTNAME"]
		ip := node["IPV4_ADDRESS"]
		if h == "" || ip == "" {
			continue
		}
		stale := false
		if ts := node["LAST_SEEN_TIMESTAMP"]; ts != "" {
			if seen, err := strconv.ParseInt(ts, 10, 64); err == nil {
				stale = (now - seen) > 300
			}
		}
		dnsRecords = append(dnsRecords, map[string]interface{}{
			"name": h + ".mesh", "ip": ip, "type": "node",
			"source": h, "stale": stale,
		})
	}
	dnsRecords = append(dnsRecords, map[string]interface{}{
		"name": "radio.mesh", "ip": myIP, "type": "local",
		"source": myHostname, "stale": false,
	})
	for _, rec := range collectAppletDNS(myIP, myHostname) {
		dnsRecords = append(dnsRecords, rec)
	}

	writeJSON(w, 200, map[string]interface{}{
		"bat0": map[string]interface{}{
			"state":   bat0["state"],
			"addrs":   bat0Addrs,
			"algo":    bat0["algo"],
			"gw_mode": bat0["gw_mode"],
		},
		"hostname":          myHostname,
		"halow_bw":          halowBW,
		"mesh_ssid":         conf["mesh_ssid"],
		"network":           confGet(conf, "ipv4_network", "10.30.2.0/24"),
		"originators":       origList,
		"neighbors":         neighList,
		"gateways":          gwList,
		"originator_count":  len(origMap),
		"neighbor_count":    len(neighbors),
		"gateway_count":     len(gateways),
		"dns_records":       dnsRecords,
		"euds":              getEUDs(),
	})
}

// --- Control endpoints ---

func apiControlInterface(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	iface := jsonStr(body, "iface", "")
	state := jsonStr(body, "state", "")
	if iface == "" || (state != "up" && state != "down") {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid iface or state"})
		return
	}

	isHalow := false
	if out, err := runCmdStdout(3*time.Second, "ethtool", "-i", iface); err == nil {
		isHalow = strings.Contains(out, "morse_usb") || strings.Contains(out, "morse")
	}

	var cmds [][]string
	if isHalow {
		if state == "down" {
			cmds = [][]string{
				{"systemctl", "stop", "wpa_supplicant-s1g-" + iface + ".service"},
				{"ip", "link", "set", iface, "down"},
			}
		} else {
			cmds = [][]string{
				{"ip", "link", "set", iface, "up"},
				{"systemctl", "start", "wpa_supplicant-s1g-" + iface + ".service"},
			}
		}
	} else {
		if state == "down" {
			cmds = [][]string{
				{"batctl", "if", "del", iface},
				{"systemctl", "stop", "wpa_supplicant@" + iface + ".service"},
				{"ip", "link", "set", iface, "down"},
			}
		} else {
			cmds = [][]string{
				{"ip", "link", "set", iface, "up"},
				{"systemctl", "start", "wpa_supplicant@" + iface + ".service"},
			}
		}
	}

	for _, cmd := range cmds {
		runCmd(10*time.Second, cmd[0], cmd[1:]...)
	}

	if state == "up" && !isHalow {
		if out, err := runCmdStdout(5*time.Second, "batctl", "if"); err == nil {
			if !strings.Contains(out, iface) {
				runCmd(5*time.Second, "batctl", "if", "add", iface)
			}
		}
	}

	writeJSON(w, 200, map[string]interface{}{"ok": true, "iface": iface, "state": state})
}

func apiControlTxPower(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	iface := jsonStr(body, "iface", "")
	dbm := jsonFloat(body, "dbm", 0)
	if iface == "" || dbm == 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Missing iface or dbm"})
		return
	}

	cap := getIfaceTxPowerCap(iface)
	capF, _ := strconv.ParseFloat(cap, 64)
	if dbm > capF && capF > 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": fmt.Sprintf("Exceeds cap %s dBm", cap)})
		return
	}

	requested, actual, err := setIfaceTxPower(iface, dbm)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "iface": iface, "dbm": requested, "actual_dbm": actual, "cap": cap,
	})
}

func apiControlHalowChannel(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	channel := jsonInt(body, "channel", 0)
	bw := jsonStr(body, "bw", "1MHz")

	if channel < 1 || channel > 5 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid channel (1-5)"})
		return
	}

	freqKHz := HalowEUChannels[channel-1]
	bwMHz := strings.TrimSuffix(bw, "MHz")

	out, err := runCmd(5*time.Second, "morse_cli", "-i", "wlan2", "channel",
		"-c", strconv.Itoa(freqKHz), "-o", bwMHz, "-p", bwMHz)
	if err != nil {
		runCmd(5*time.Second, "systemctl", "restart", "wpa_supplicant-s1g-wlan2.service")
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": "morse_cli failed: " + strings.TrimSpace(out)})
		return
	}

	// Write override file
	os.WriteFile("/var/run/halow-channel-override", []byte(fmt.Sprintf("%d,%s", channel, bw)), 0644)

	resp := map[string]interface{}{
		"ok": true, "channel": channel, "freq_khz": freqKHz, "bw": bw,
	}

	if dbm := jsonFloat(body, "dbm", 0); dbm > 0 {
		requested, actual, _ := setIfaceTxPower("wlan2", dbm)
		resp["dbm"] = requested
		resp["actual_dbm"] = actual
	}

	writeJSON(w, 200, resp)
}

func apiControlWifiChannel(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	iface := jsonStr(body, "interface", "")
	if iface == "" {
		iface = jsonStr(body, "iface", "")
	}
	channel := jsonInt(body, "channel", 0)

	if iface == "" || channel == 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Missing interface or channel"})
		return
	}

	freq := wifiChannelToFreq(iface, channel)
	if freq == 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid channel for interface"})
		return
	}

	// Update wpa_supplicant config
	confPath := fmt.Sprintf("/etc/wpa_supplicant/wpa_supplicant-%s.conf", iface)
	if data, err := os.ReadFile(confPath); err == nil {
		text := string(data)
		freqStr := fmt.Sprintf("frequency=%d", freq)
		freqRE := regexp.MustCompile(`frequency=\d+`)
		if freqRE.MatchString(text) {
			text = freqRE.ReplaceAllString(text, freqStr)
		} else {
			text = strings.Replace(text, "}", "\t"+freqStr+"\n}", 1)
		}
		os.WriteFile(confPath, []byte(text), 0644)
	}

	runCmd(5*time.Second, "systemctl", "restart", "wpa_supplicant@"+iface+".service")

	resp := map[string]interface{}{"ok": true, "iface": iface, "channel": channel, "frequency": freq}

	if dbm := jsonFloat(body, "dbm", 0); dbm > 0 {
		requested, actual, _ := setIfaceTxPower(iface, dbm)
		resp["dbm"] = requested
		resp["actual_dbm"] = actual
	}

	writeJSON(w, 200, resp)
}

func apiControlHostname(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	prefix := jsonStr(body, "hostname", "")
	prefix = regexp.MustCompile(`[^a-zA-Z0-9_-]`).ReplaceAllString(prefix, "")
	if prefix == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Empty hostname"})
		return
	}

	conf := loadKVFile(MeshConfFile)
	meshSSID := conf["mesh_ssid"]
	macSuffix := getMACsuffix()

	full := prefix
	if meshSSID != "" {
		full += "-" + meshSSID
	}
	if macSuffix != "" {
		full += "-" + macSuffix
	}

	saveKVFile(MeshConfFile, map[string]string{"node_hostname": prefix})
	setHostname(full)

	writeJSON(w, 200, map[string]interface{}{"ok": true, "hostname": full})
}

// --- Admin endpoints ---

var saveableKeys = map[string]bool{
	"node_hostname": true, "eud": true, "lan_ap_ssid": true, "lan_ap_key": true,
	"lan_ap_channel": true, "lan_ap_bw": true,
	"max_euds_per_node": true, "mesh_ssid": true, "mesh_key": true,
	"ipv4_network": true, "regulatory_domain": true, "halow_bw": true,
	"battery_monitor": true, "admin_password": true, "require_auth": true,
	"gateway": true, "gateway_nat": true, "gateway_mss_clamp": true, "gateway_bandwidth": true,
	"multicast_mode": true,
	"voice_mic_volume": true, "voice_speaker_volume": true,
	"voice_channel": true, "voice_rx_channels": true,
	"voice_ptt_mode": true, "voice_gain": true, "voice_enabled": true,
	"voice_beep_tx_start": true, "voice_beep_rx_end": true,
	"dns_servers": true,
	"eud_bandwidth": true,
	"qos_enabled": true, "qos_voice_band": true, "qos_cot_band": true, "qos_chat_band": true,
	"auto_update": true, "update_url": true,
	"gps": true, "gps_source": true, "gps_static_lat": true, "gps_static_lon": true, "gps_static_alt": true,
	"callsign": true, "cot_type": true, "cot_team": true, "cot_role": true, "cot_icon": true,
}

func apiAdminSave(w http.ResponseWriter, r *http.Request) {
	if getPendingConfig() != nil {
		writeJSON(w, 409, map[string]interface{}{"ok": false, "error": "Cannot save while fleet config is staged — activate or cancel it first"})
		return
	}
	body := readBody(r)
	configRaw, ok := body["config"]
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Missing config"})
		return
	}
	configMap, ok := configRaw.(map[string]interface{})
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid config format"})
		return
	}

	updates := make(map[string]string)
	var saved []string
	for k, v := range configMap {
		if saveableKeys[k] {
			updates[k] = fmt.Sprintf("%v", v)
			saved = append(saved, k)
		}
	}

	if len(updates) == 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "No valid keys"})
		return
	}

	expandNodeTemplates(updates, loadKVFile(MeshConfFile))
	if err := saveKVFile(MeshConfFile, updates); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	applied := make(map[string]interface{})
	conf := loadKVFile(MeshConfFile)

	// Apply hostname. Skip when no prefix is configured — falling back to
	// the "node" default here is how nodes ended up renamed to node-<mac>.
	if (updates["node_hostname"] != "" || updates["mesh_ssid"] != "") && conf["node_hostname"] != "" {
		prefix := conf["node_hostname"]
		meshSSID := conf["mesh_ssid"]
		macSuffix := getMACsuffix()
		full := prefix
		if meshSSID != "" {
			full += "-" + meshSSID
		}
		if macSuffix != "" {
			full += "-" + macSuffix
		}
		setHostname(full)
		applied["hostname"] = full
	}

	// Apply gateway config
	if updates["gateway"] != "" || updates["gateway_nat"] != "" || updates["gateway_mss_clamp"] != "" || updates["gateway_bandwidth"] != "" {
		runCmd(5*time.Second, "systemctl", "reload", "gateway-manager")
		applied["gateway_reloaded"] = true
	}

	// Apply EUD bandwidth cap
	if updates["eud_bandwidth"] != "" {
		applyEUDBandwidth(conf["eud_bandwidth"])
		applied["eud_bandwidth_applied"] = true
	}

	// Apply EUD mode changes
	if updates["eud"] != "" {
		eud := conf["eud"]
		if eudWantsAP(eud) {
			// The reconcile script itself selects/regenerates the AP
			// interface (hostapd.conf, ap-interface-setup.service,
			// ap-txpower.service) and stops any stale mesh
			// wpa_supplicant on it — must run before hostapd is
			// (re)started so it picks up a config that actually
			// targets the current AP interface, not a stale one.
			// 60s budget: on this path the script itself restarts 4
			// services sequentially, two of them oneshot units with a
			// 2s ExecStartPre sleep each — a shorter timeout risks
			// SIGKILLing it mid-sequence, leaving e.g. ap-txpower.service
			// never applied while api.go's own follow-up calls still
			// report success.
			if out, err := runCmd(60*time.Second, "manet-wlan-reconcile.sh"); err != nil {
				log.Printf("manet-wlan-reconcile: %v (%s)", err, strings.TrimSpace(out))
			} else if strings.TrimSpace(out) != "" {
				log.Printf("manet-wlan-reconcile: %s", strings.TrimSpace(out))
			}
			runCmd(5*time.Second, "systemctl", "enable", "hostapd")
			runCmd(5*time.Second, "systemctl", "start", "hostapd")
		} else if eud == "wired" || eud == "none" {
			runCmd(5*time.Second, "systemctl", "stop", "hostapd")
			// The radio that was AP just got reclassified as mesh in
			// /var/lib/mesh_if, but never had a wpa_supplicant config
			// generated (that only happens once, at first provisioning)
			// and ap-txpower.service still holds its txpower fixed low —
			// reconcile both now rather than leaving it a non-functional
			// mesh radio until the node is fully re-provisioned.
			if out, err := runCmd(60*time.Second, "manet-wlan-reconcile.sh"); err != nil {
				log.Printf("manet-wlan-reconcile: %v (%s)", err, strings.TrimSpace(out))
			} else if strings.TrimSpace(out) != "" {
				log.Printf("manet-wlan-reconcile: %s", strings.TrimSpace(out))
			}
		}
		applied["eud_mode_applied"] = true
	}

	// Apply AP settings. Gated on eud actually wanting an AP: the web UI's
	// Config tab always resends the node's current (unchanged) lan_ap_*
	// values on every save, not just when the user edited them, so this
	// would otherwise fire on an eud=wired/none save too -- restarting
	// hostapd right after the eud block above may have just stopped and
	// disabled it.
	apChanged := updates["lan_ap_ssid"] != "" || updates["lan_ap_key"] != "" ||
		updates["lan_ap_channel"] != "" || updates["lan_ap_bw"] != ""
	if apChanged && eudWantsAP(conf["eud"]) {
		applyHostapdConfig(conf)
		runCmd(10*time.Second, "systemctl", "restart", "hostapd")
		applied["ap_restarted"] = true
	}

	// Apply DHCP pool changes
	if updates["max_euds_per_node"] != "" || updates["ipv4_network"] != "" {
		runCmd(10*time.Second, "systemctl", "restart", "mesh-manager")
		applied["mesh_manager_restarted"] = true
	}

	// Apply HaLow bandwidth
	if updates["halow_bw"] != "" || updates["regulatory_domain"] != "" {
		applyHalowBW(conf)
		applied["halow_bw_applied"] = true
	}

	// Apply mesh key/SSID changes to wpa_supplicant configs
	if updates["mesh_ssid"] != "" || updates["mesh_key"] != "" {
		applyWPAConfig(conf)
		applied["mesh_updated"] = true
	}

	// Apply multicast mode
	if updates["multicast_mode"] != "" {
		applyMulticastMode(updates["multicast_mode"])
		applied["multicast_applied"] = true
	}

	// Apply voice volume
	if updates["voice_mic_volume"] != "" || updates["voice_speaker_volume"] != "" {
		applyVoiceVolume(conf)
		applied["voice_volume_applied"] = true
	}

	// Apply voice_enabled: stop the daemon outright on disable (mesh-voice
	// exits cleanly on its own if restarted while disabled, but stopping it
	// directly avoids a pointless restart cycle and updates the UI's
	// running-state immediately rather than after RestartSec).
	if updates["voice_enabled"] != "" {
		if conf["voice_enabled"] == "n" {
			runCmd(5*time.Second, "systemctl", "stop", "mesh-voice")
		} else {
			runCmd(5*time.Second, "systemctl", "restart", "mesh-voice")
		}
		applied["voice_enabled_applied"] = true
	}

	// Apply gps: stop gpsd/gps-reader outright on disable — this hardware
	// has no GPS module, no point leaving either running. cot-emitter stays
	// untouched; its EUD relay is GPS-independent. Enabling on a node that
	// was originally provisioned with gps=n needs gpsd installed first —
	// radio-setup.sh only apt-installs it when gps=y at boot.
	//
	// enable/disable alongside stop/restart: radio-setup.sh sets the
	// boot-time enabled state once, at first provisioning, and never
	// re-runs on a live gps= change — a stop/restart-only toggle here
	// looks like it worked but silently reverts on the node's next
	// reboot in both directions.
	//
	// gps_source=static (a node with no receiver reporting a fixed
	// position, e.g. a stationary gateway) never needs gpsd at all --
	// gps-reader itself re-reads gps_source/gps_static_* on every poll
	// tick and writes the configured position straight into
	// /run/gps_status.json, so no restart is needed here for lat/lon/alt
	// edits alone, only for gps or gps_source actually changing.
	if updates["gps"] != "" || updates["gps_source"] != "" {
		if conf["gps"] == "n" {
			runCmd(5*time.Second, "systemctl", "disable", "--now", "gps-reader")
			runCmd(5*time.Second, "systemctl", "disable", "--now", "gpsd")
		} else if conf["gps_source"] == "static" {
			runCmd(5*time.Second, "systemctl", "disable", "--now", "gpsd")
			runCmd(5*time.Second, "systemctl", "enable", "--now", "gps-reader")
		} else {
			if _, err := exec.LookPath("gpsd"); err != nil {
				runCmd(60*time.Second, "apt-get", "install", "-y", "gpsd", "gpsd-clients")
			}
			runCmd(5*time.Second, "systemctl", "enable", "--now", "gpsd")
			runCmd(5*time.Second, "systemctl", "enable", "--now", "gps-reader")
		}
		applied["gps_applied"] = true
	}

	// Apply CoT identity changes (callsign/type/team/role/icon) — cot-emitter
	// reads these once at startup, so a live edit needs a restart to take
	// effect. configSave() always submits every field on the Config page,
	// including ones intentionally cleared back to blank (e.g. reverting
	// cot_team to "no team affiliation"), so check key presence in updates
	// rather than non-blank value — a blank submission is still a real edit.
	cotIdentityKeys := []string{"callsign", "cot_type", "cot_team", "cot_role", "cot_icon"}
	cotIdentityChanged := false
	for _, k := range cotIdentityKeys {
		if _, ok := updates[k]; ok {
			cotIdentityChanged = true
			break
		}
	}
	if cotIdentityChanged {
		runCmd(5*time.Second, "systemctl", "restart", "cot-emitter")
		applied["cot_identity_applied"] = true
	}

	// Apply voice PTT mode / channel changes
	if conf["voice_enabled"] != "n" && (updates["voice_ptt_mode"] != "" || updates["voice_channel"] != "" || updates["voice_gain"] != "" ||
		updates["voice_beep_tx_start"] != "" || updates["voice_beep_rx_end"] != "") {
		txCh := int(voiceTxCh.Load())
		if txCh <= 0 {
			txCh = 1
		}
		voiceRestartDaemon(txCh)
		applied["voice_restarted"] = true
	}

	// Apply DNS servers
	if updates["dns_servers"] != "" {
		applyDNSServers(updates["dns_servers"])
		applied["dns_applied"] = true
	}

	// Apply QoS
	if updates["qos_enabled"] != "" || updates["qos_voice_band"] != "" || updates["qos_cot_band"] != "" || updates["qos_chat_band"] != "" {
		applyQoSFromConf(conf)
		applied["qos_applied"] = true
	}

	if updates["auto_update"] != "" || updates["update_url"] != "" {
		runCmd(5*time.Second, "systemctl", "reload", "node-update")
		applied["node_update_reloaded"] = true
	}

	go func() {
		args := []string{"config-change"}
		for _, k := range saved {
			args = append(args, "KEY="+k)
		}
		exec.Command("/usr/local/bin/mesh-hook", args...).Run()
	}()

	writeJSON(w, 200, map[string]interface{}{"ok": true, "saved": saved, "applied": applied})
}

func apiAdminStage(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	configRaw, ok := body["config"]
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Missing config"})
		return
	}
	configMap, ok := configRaw.(map[string]interface{})
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid config format"})
		return
	}

	currentConf := loadKVFile(MeshConfFile)
	strConf := make(map[string]string)
	for k, v := range configMap {
		strConf[k] = fmt.Sprintf("%v", v)
	}

	version := makeConfigVersion(strConf)

	dangerous := strConf["mesh_ssid"] != currentConf["mesh_ssid"] ||
		strConf["mesh_key"] != currentConf["mesh_key"] ||
		strConf["ipv4_network"] != confGet(currentConf, "ipv4_network", "10.30.2.0/24")

	prefs := loadFleetPreferences()

	pkg := map[string]interface{}{
		"version":       version,
		"config":        configMap,
		"profiles":      prefs.Profiles,
		"node_profiles": prefs.NodeProfiles,
		"staged_by":     getMyHostname(),
		"staged_at":     time.Now().Unix(),
	}

	savePendingConfig(pkg)
	os.WriteFile(AckVersionFile, []byte(version), 0644)
	broadcastConfigPackage(pkg)
	go fleetMcastSendAck(version)

	writeJSON(w, 200, map[string]interface{}{"ok": true, "version": version, "dangerous": dangerous})
}

func apiAdminActivate(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	force := jsonBool(body, "force")

	pending := getPendingConfig()
	if pending == nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "No pending config"})
		return
	}

	var pkg map[string]interface{}
	json.Unmarshal(pending, &pkg)

	if !force {
		version := jsonStr(pkg, "version", "")
		registry := parseRegistry()
		var notAcked []string
		for _, rn := range registry {
			if rn["CONFIG_ACK_VERSION"] != version {
				name := rn["HOSTNAME"]
				if name == "" {
					name = rn["IPV4_ADDRESS"]
				}
				notAcked = append(notAcked, name)
			}
		}
		if len(notAcked) > 0 {
			writeJSON(w, 400, map[string]interface{}{
				"ok":    false,
				"error": fmt.Sprintf("%d nodes have not ACKed: %s", len(notAcked), strings.Join(notAcked, ", ")),
			})
			return
		}
	}

	activateAt := time.Now().Add(60 * time.Second).Unix()
	pkg["activate_at"] = activateAt
	savePendingConfig(pkg)
	broadcastConfigPackage(pkg)
	version := jsonStr(pkg, "version", "")
	go fleetMcastSendActivation(version, activateAt)

	writeJSON(w, 200, map[string]interface{}{"ok": true, "activate_at": activateAt})
}

func apiAdminCancel(w http.ResponseWriter, r *http.Request) {
	clearPendingConfig()
	// Keep AckVersionFile so fleetPollAlfred won't re-stage the same version from Alfred
	// Clear the Alfred slot to stop other nodes from picking it up
	cmd := exec.Command("alfred", "-s", "70")
	cmd.Stdin = strings.NewReader("")
	cmd.Run()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiAdminDeleteNode(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	nodeID, _ := body["id"].(string)
	if nodeID == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "missing node id"})
		return
	}
	nodeID = strings.ReplaceAll(strings.ToLower(nodeID), ":", "")

	data, err := os.ReadFile(RegistryFile)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": "cannot read registry"})
		return
	}

	prefix := "NODE_" + nodeID + "_"
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}

	if err := os.WriteFile(RegistryFile, []byte(strings.Join(kept, "\n")), 0644); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]interface{}{"ok": true, "deleted": nodeID})
}

// --- Service action ---

func apiServiceAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "missing service id"})
		return
	}
	serviceID := parts[3]
	body := readBody(r)
	action := jsonStr(body, "action", "")
	if action == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "missing action"})
		return
	}
	ok, errMsg := serviceAction(serviceID, action)
	writeJSON(w, 200, map[string]interface{}{"ok": ok, "error": errMsg})
}

// --- Perf endpoints ---

var (
	activeStreams   = make(map[string]*exec.Cmd)
	activeStreamMu sync.Mutex
)

func killStream(key string) {
	activeStreamMu.Lock()
	defer activeStreamMu.Unlock()
	if cmd, ok := activeStreams[key]; ok {
		cmd.Process.Kill()
		cmd.Wait()
		delete(activeStreams, key)
	}
}

func apiIperfServerStart(w http.ResponseWriter, r *http.Request) {
	exec.Command("pkill", "-f", "iperf3 -s").Run()
	cmd := exec.Command("iperf3", "-s", "--one-off", "-J", "--logfile", "/tmp/iperf3-server.log")
	if err := cmd.Start(); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiIperfServerStop(w http.ResponseWriter, r *http.Request) {
	exec.Command("pkill", "-f", "iperf3 -s").Run()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiIperfClientRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverIP := jsonStr(body, "server_ip", "")
	testType := jsonStr(body, "test_type", "tcp_1stream")
	duration := jsonInt(body, "duration", 30)
	bitrate := jsonStr(body, "bitrate", "4M")
	parallel := jsonInt(body, "parallel", 1)
	reverse := jsonBool(body, "reverse")

	if serverIP == "" || !validateTargetRE.MatchString(serverIP) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid server_ip"})
		return
	}

	args := []string{"-c", serverIP, "-t", strconv.Itoa(duration), "-J"}
	if strings.HasPrefix(testType, "udp") {
		args = append(args, "-u", "-b", bitrate)
	}
	if parallel > 1 {
		args = append(args, "-P", strconv.Itoa(parallel))
	}
	if reverse {
		args = append(args, "-R")
	}

	timeout := time.Duration(duration+15) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "iperf3", args...).CombinedOutput()
	var result interface{}
	json.Unmarshal(out, &result)

	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"ok": false, "error": err.Error(), "result": result})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "result": result})
}

func apiIperfClientStream(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverIP := jsonStr(body, "server_ip", "")
	testType := jsonStr(body, "test_type", "tcp_1stream")
	duration := jsonInt(body, "duration", 30)
	bitrate := jsonStr(body, "bitrate", "4M")

	if !validateTargetRE.MatchString(serverIP) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid server_ip"})
		return
	}

	killStream("iperf")
	args := []string{"-c", serverIP, "-t", strconv.Itoa(duration), "--forceflush"}
	if strings.HasPrefix(testType, "udp") {
		args = append(args, "-u", "-b", bitrate)
	}

	cmd := exec.Command("iperf3", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	activeStreamMu.Lock()
	activeStreams["iperf"] = cmd
	activeStreamMu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()
	killStream("iperf")
}

func apiIperfStop(w http.ResponseWriter, r *http.Request) {
	killStream("iperf")
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiPingRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	target := jsonStr(body, "target", "")
	count := jsonInt(body, "count", 100)
	interval := jsonFloat(body, "interval", 0.2)

	if !validateTargetRE.MatchString(target) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid target"})
		return
	}

	timeout := time.Duration(float64(count)*interval+10) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, _ := exec.CommandContext(ctx, "ping", "-c", strconv.Itoa(count),
		"-i", fmt.Sprintf("%.1f", interval), target).CombinedOutput()

	output := string(out)
	result := map[string]interface{}{"output": output}

	if m := regexp.MustCompile(`(\d+)% packet loss`).FindStringSubmatch(output); len(m) > 1 {
		v, _ := strconv.Atoi(m[1])
		result["loss_pct"] = v
	}
	if m := regexp.MustCompile(`([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)`).FindStringSubmatch(output); len(m) > 4 {
		result["rtt_min"], _ = strconv.ParseFloat(m[1], 64)
		result["rtt_avg"], _ = strconv.ParseFloat(m[2], 64)
		result["rtt_max"], _ = strconv.ParseFloat(m[3], 64)
		result["rtt_mdev"], _ = strconv.ParseFloat(m[4], 64)
	}

	writeJSON(w, 200, map[string]interface{}{"ok": true, "result": result})
}

func apiPingStream(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	target := jsonStr(body, "target", "")
	count := jsonInt(body, "count", 0)

	if !validateTargetRE.MatchString(target) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid target"})
		return
	}

	killStream("ping")
	args := []string{target}
	if count > 0 {
		args = []string{"-c", strconv.Itoa(count), target}
	}

	cmd := exec.Command("ping", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	activeStreamMu.Lock()
	activeStreams["ping"] = cmd
	activeStreamMu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()
	killStream("ping")
}

func apiPingStop(w http.ResponseWriter, r *http.Request) {
	killStream("ping")
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func apiTracerouteStream(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	target := jsonStr(body, "target", "")

	if !validateTargetRE.MatchString(target) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid target"})
		return
	}

	killStream("traceroute")

	cmd := exec.Command("bash", "-c",
		`TARGET="$1"
MAC=$(ip neigh show dev bat0 "$TARGET" 2>/dev/null | awk '{print $5}' | head -1)
if [ -n "$MAC" ] && command -v batctl >/dev/null 2>&1; then
  echo "=== Mesh Route (batctl traceroute $MAC) ==="
  batctl traceroute "$MAC" 2>&1 || true
  echo ""
fi
if command -v traceroute >/dev/null 2>&1; then
  echo "=== IP Traceroute ==="
  traceroute -n -w 3 -m 15 "$TARGET" 2>&1 || true
else
  echo "=== IP Route ==="
  ip route get "$TARGET" 2>&1 || true
fi`, "--", target)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	activeStreamMu.Lock()
	activeStreams["traceroute"] = cmd
	activeStreamMu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()
	killStream("traceroute")
}

func apiTracerouteStop(w http.ResponseWriter, r *http.Request) {
	killStream("traceroute")
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// --- Terminal HTTP fallback ---

var blockedCmdRE = regexp.MustCompile(`(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|shutdown|halt|poweroff)\b`)

func apiTerminalExec(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	cmd := strings.TrimSpace(jsonStr(body, "cmd", ""))
	target := jsonStr(body, "target", "")
	user := jsonStr(body, "user", "root")
	password := jsonStr(body, "password", "")

	if cmd == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Empty command"})
		return
	}
	if blockedCmdRE.MatchString(cmd) {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "error": "Command blocked"})
		return
	}

	var shellCmd string
	if target != "" {
		if !validateTargetRE.MatchString(target) {
			writeJSON(w, 400, map[string]interface{}{"ok": false, "error": "Invalid target"})
			return
		}
		sshOpts := "-o StrictHostKeyChecking=no -o ConnectTimeout=5"
		if password != "" {
			shellCmd = fmt.Sprintf("sshpass -p %s ssh %s %s@%s bash -l -c %s",
				shellQuote(password), sshOpts, shellQuote(user), shellQuote(target), shellQuote(cmd))
		} else {
			shellCmd = fmt.Sprintf("ssh %s %s@%s bash -l -c %s",
				sshOpts, shellQuote(user), shellQuote(target), shellQuote(cmd))
		}
	} else {
		shellCmd = fmt.Sprintf("bash -l -c %s", shellQuote(cmd))
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	proc := exec.Command("bash", "-c", shellCmd)
	proc.Stdout = w
	proc.Stderr = w
	flusher, _ := w.(http.Flusher)
	proc.Run()
	if flusher != nil {
		flusher.Flush()
	}
}

func apiTerminalComplete(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	line := jsonStr(body, "line", "")
	pos := jsonInt(body, "pos", len(line))

	textBefore := line[:pos]
	parts := strings.Fields(textBefore)
	word := ""
	if len(parts) > 0 {
		word = parts[len(parts)-1]
	}
	isFirst := len(parts) <= 1

	compType := "file"
	if isFirst {
		compType = "command"
	}
	compCmd := fmt.Sprintf("compgen -A %s -- %s", compType, shellQuote(word))

	out, _ := runCmdStdout(3*time.Second, "bash", "-l", "-c", compCmd)
	seen := make(map[string]bool)
	var matches []string
	for _, m := range strings.Split(strings.TrimSpace(out), "\n") {
		if m != "" && !seen[m] {
			seen[m] = true
			matches = append(matches, m)
		}
	}

	writeJSON(w, 200, map[string]interface{}{"matches": matches, "word": word})
}

func apiTerminalReboot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"ok": true, "message": "Rebooting..."})
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("systemctl", "reboot").Run()
	}()
}

// --- Auth ---

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	conf := loadKVFile(MeshConfFile)
	ra := strings.ToLower(conf["require_auth"])
	if ra != "y" && ra != "yes" && ra != "1" {
		return true
	}
	pw := getProvisionedPassword(conf)
	if pw == "" {
		return true
	}
	cookie, err := r.Cookie(PerfAuthCookie)
	if err != nil || cookie.Value != getPerfAuthToken() {
		writeJSON(w, 401, map[string]interface{}{"ok": false, "error": "Authentication required"})
		return false
	}
	return true
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		next(w, r)
	}
}

func apiAuthStatus(w http.ResponseWriter, r *http.Request) {
	conf := loadKVFile(MeshConfFile)
	ra := strings.ToLower(conf["require_auth"])
	pw := getProvisionedPassword(conf)
	required := pw != "" && (ra == "y" || ra == "yes" || ra == "1")
	authenticated := !required
	if required {
		cookie, err := r.Cookie(PerfAuthCookie)
		if err == nil && cookie.Value == getPerfAuthToken() {
			authenticated = true
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"required":      required,
		"authenticated": authenticated,
	})
}

func apiPerfAuth(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	password := jsonStr(body, "password", "")

	conf := loadKVFile(MeshConfFile)
	expected := getProvisionedPassword(conf)
	if expected == "" || password != expected {
		writeJSON(w, 401, map[string]interface{}{"ok": false, "error": "Invalid password"})
		return
	}

	token := getPerfAuthToken()
	http.SetCookie(w, &http.Cookie{
		Name:     PerfAuthCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   PerfAuthMaxAge,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func setHostname(name string) {
	if _, err := runCmd(5*time.Second, "hostnamectl", "set-hostname", name); err == nil {
		return
	}
	os.WriteFile("/etc/hostname", []byte(name+"\n"), 0644)

	if data, err := os.ReadFile("/etc/hosts"); err == nil {
		text := string(data)
		hostRE := regexp.MustCompile(`(?m)^127\.0\.1\.1\s+.*$`)
		if hostRE.MatchString(text) {
			text = hostRE.ReplaceAllString(text, "127.0.1.1\t"+name)
		} else {
			text += "\n127.0.1.1\t" + name
		}
		os.WriteFile("/etc/hosts", []byte(text), 0644)
	}

	if _, err := runCmd(3*time.Second, "hostname", name); err != nil {
		runCmd(3*time.Second, "hostnamectl", "--transient", "set-hostname", name)
	}
}

func applyWPAConfig(conf map[string]string) {
	ssid := conf["mesh_ssid"]
	key := conf["mesh_key"]
	if ssid == "" {
		return
	}

	wpaDir := "/etc/wpa_supplicant"
	entries, err := os.ReadDir(wpaDir)
	if err != nil {
		return
	}

	ssidRE := regexp.MustCompile(`ssid="[^"]*"`)
	pskRE := regexp.MustCompile(`psk="[^"]*"`)
	saeRE := regexp.MustCompile(`sae_password="[^"]*"`)

	restartS1G := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "wpa_supplicant-wlan") || !strings.HasSuffix(name, ".conf") {
			continue
		}

		path := wpaDir + "/" + name
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)
		text = ssidRE.ReplaceAllString(text, fmt.Sprintf(`ssid="%s"`, ssid))
		if key != "" {
			if strings.Contains(name, "s1g") {
				text = saeRE.ReplaceAllString(text, fmt.Sprintf(`sae_password="%s"`, key))
				restartS1G = true
			} else {
				text = pskRE.ReplaceAllString(text, fmt.Sprintf(`psk="%s"`, key))
			}
		}
		os.WriteFile(path, []byte(text), 0644)
		log.Printf("wpa config updated: %s", name)
	}

	runCmd(10*time.Second, "bash", "-c", "systemctl restart 'wpa_supplicant@wlan*.service' 2>/dev/null || true")
	if restartS1G {
		runCmd(10*time.Second, "bash", "-c", "systemctl restart 'wpa_supplicant-s1g-wlan*.service' 2>/dev/null || true")
	}
}

func halowBWParams(bw, regDomain string) (opClass, channel, primChwidth, txpowerMBM string) {
	if regDomain == "US" {
		switch bw {
		case "1MHz":
			return "68", "11", "0", "2400"
		case "2MHz":
			return "69", "10", "1", "2400"
		case "8MHz":
			// op_class 72 / channel 8 is rejected outright by
			// wpa_supplicant_s1g ("error determining S1G operating channel
			// width from operating class") — same bug as radio-setup.sh's
			// table, fixed there but missed here since this is a separate
			// duplicate lookup used when the UI/fleet push updates
			// halow_bw on an already-provisioned node. op_class 71 /
			// channel 12 is the confirmed-working pair for 8MHz.
			return "71", "12", "1", "2000"
		default:
			return "71", "12", "1", "2200"
		}
	}
	switch bw {
	case "2MHz":
		return "67", "2", "1", "2400"
	default:
		return "66", "1", "0", "2400"
	}
}

func applyHalowBW(conf map[string]string) {
	bw := conf["halow_bw"]
	if bw == "" {
		return
	}
	regDomain := confGet(conf, "regulatory_domain", "US")
	opClass, ch, chwidth, txMBM := halowBWParams(bw, regDomain)

	opClassRE := regexp.MustCompile(`op_class=(\d+)`)
	channelRE := regexp.MustCompile(`(^|\s)channel=\d+`)
	chwidthRE := regexp.MustCompile(`s1g_prim_chwidth=\d+`)
	txpowerRE := regexp.MustCompile(`txpower fixed \d+`)

	wpaDir := "/etc/wpa_supplicant"
	entries, _ := os.ReadDir(wpaDir)
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, "s1g") {
			continue
		}
		path := wpaDir + "/" + name
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)

		if m := opClassRE.FindStringSubmatch(text); len(m) > 1 && m[1] == opClass {
			continue
		}

		text = opClassRE.ReplaceAllString(text, "op_class="+opClass)
		text = channelRE.ReplaceAllStringFunc(text, func(m string) string {
			prefix := ""
			if len(m) > 0 && m[0] != 'c' {
				prefix = string(m[0])
			}
			return prefix + "channel=" + ch
		})
		text = chwidthRE.ReplaceAllString(text, "s1g_prim_chwidth="+chwidth)
		os.WriteFile(path, []byte(text), 0644)
		log.Printf("halow bw updated: %s -> %s (op_class=%s ch=%s)", name, bw, opClass, ch)
		changed = true
	}

	if !changed {
		return
	}

	svcDir := "/etc/systemd/system"
	svcEntries, _ := os.ReadDir(svcDir)
	for _, entry := range svcEntries {
		name := entry.Name()
		if !strings.HasPrefix(name, "halow-txpower-") {
			continue
		}
		path := svcDir + "/" + name
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := txpowerRE.ReplaceAllString(string(data), "txpower fixed "+txMBM)
		os.WriteFile(path, []byte(text), 0644)
	}
	runCmd(5*time.Second, "systemctl", "daemon-reload")
	runCmd(10*time.Second, "bash", "-c", "systemctl restart 'wpa_supplicant-s1g-wlan*.service' 2>/dev/null || true")
}

// eudWantsAP reports whether the given eud= mode requires an AP interface
// (hostapd running), as opposed to wired/none which only use the mesh
// radios and must keep hostapd stopped.
func eudWantsAP(eud string) bool {
	return eud == "wireless" || eud == "both" || eud == "auto"
}

func applyHostapdConfig(conf map[string]string) {
	apf := "/etc/hostapd/hostapd.conf"
	data, err := os.ReadFile(apf)
	if err != nil {
		return
	}
	text := string(data)

	if apSSID := conf["lan_ap_ssid"]; apSSID != "" {
		macSuffix := getMACsuffix()
		fullSSID := apSSID
		if macSuffix != "" {
			fullSSID += "-" + macSuffix
		}
		text = regexp.MustCompile(`(?m)^ssid=.*`).ReplaceAllString(text, "ssid="+fullSSID)
	}
	if apKey := conf["lan_ap_key"]; apKey != "" {
		text = regexp.MustCompile(`(?m)^wpa_passphrase=.*`).ReplaceAllString(text, "wpa_passphrase="+apKey)
	}
	if apCh := conf["lan_ap_channel"]; apCh != "" {
		text = regexp.MustCompile(`(?m)^channel=.*`).ReplaceAllString(text, "channel="+apCh)
	}
	if apBw := conf["lan_ap_bw"]; apBw != "" {
		bwInt, _ := strconv.Atoi(apBw)
		if bwInt >= 40 {
			text = regexp.MustCompile(`(?m)^#?ht_capab=.*`).ReplaceAllString(text, "ht_capab=[HT40+][SHORT-GI-40]")
		} else {
			text = regexp.MustCompile(`(?m)^ht_capab=.*`).ReplaceAllString(text, "#ht_capab=")
		}
		if bwInt >= 80 {
			text = regexp.MustCompile(`(?m)^#?vht_oper_chwidth=.*`).ReplaceAllString(text, "vht_oper_chwidth=1")
		} else if regexp.MustCompile(`(?m)^vht_oper_chwidth=`).MatchString(text) {
			text = regexp.MustCompile(`(?m)^vht_oper_chwidth=.*`).ReplaceAllString(text, "vht_oper_chwidth=0")
		}
	}

	os.WriteFile(apf, []byte(text), 0644)
	log.Printf("hostapd config updated")
}

func applyMulticastMode(mode string) {
	if mode == "optimized" {
		runCmd(3*time.Second, "batctl", "bat0", "multicast_forceflood", "0")
		os.WriteFile("/sys/devices/virtual/net/br0/bridge/multicast_snooping", []byte("1"), 0644)
		os.WriteFile("/sys/devices/virtual/net/br0/bridge/multicast_querier", []byte("1"), 0644)
	} else {
		runCmd(3*time.Second, "batctl", "bat0", "multicast_forceflood", "1")
		os.WriteFile("/sys/devices/virtual/net/br0/bridge/multicast_snooping", []byte("0"), 0644)
		os.WriteFile("/sys/devices/virtual/net/br0/bridge/multicast_querier", []byte("0"), 0644)
	}
}

func applyDNSServers(csv string) {
	servers := strings.Split(csv, ",")
	var lines []string
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s != "" && net.ParseIP(s) != nil {
			lines = append(lines, "nameserver "+s)
		}
	}
	if len(lines) == 0 {
		lines = []string{"nameserver 8.8.8.8", "nameserver 8.8.4.4"}
	}
	os.WriteFile("/etc/resolv.conf", []byte(strings.Join(lines, "\n")+"\n"), 0644)
	runCmd(5*time.Second, "systemctl", "restart", "dnsmasq")
}

func findOpenVLMCard() string {
	matches, _ := filepath.Glob("/proc/asound/card*/usbid")
	target := fmt.Sprintf("%04x:%04x", 0x0D8C, 0x0012)
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(strings.ToLower(string(data))) != target {
			continue
		}
		cardDir := filepath.Base(filepath.Dir(path))
		return strings.TrimPrefix(cardDir, "card")
	}
	return ""
}

func applyVoiceVolume(conf map[string]string) {
	card := findOpenVLMCard()
	if card == "" {
		return
	}
	mic := confGet(conf, "voice_mic_volume", "")
	spk := confGet(conf, "voice_speaker_volume", "")
	if mic != "" {
		v, err := strconv.Atoi(mic)
		if err == nil && v >= 0 && v <= 100 {
			pct := fmt.Sprintf("%d%%", v)
			runCmd(3*time.Second, "amixer", "-c", card, "set", "Mic", pct)
		}
	}
	if spk != "" {
		v, err := strconv.Atoi(spk)
		if err == nil && v >= 0 && v <= 100 {
			pct := fmt.Sprintf("%d%%", v)
			runCmd(3*time.Second, "amixer", "-c", card, "set", "PCM", pct)
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func apiATAKPackage(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "MANET"
	}

	uid := "manet-mesh-" + hostname

	pref := `<?xml version='1.0' standalone='yes'?>
<preferences>
    <preference version="1" name="cot_streams">
        <entry key="count" class="class java.lang.Integer">1</entry>
        <entry key="description0" class="class java.lang.String">MANET Mesh</entry>
        <entry key="enabled0" class="class java.lang.Boolean">true</entry>
        <entry key="connectString0" class="class java.lang.String">239.2.3.1:6969:udp</entry>
    </preference>
</preferences>`

	manifest := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MissionPackageManifest version="2">
    <Configuration>
        <Parameter name="uid" value="%s"/>
        <Parameter name="name" value="MANET Mesh CoT"/>
    </Configuration>
    <Contents>
        <Content ignore="false" zipEntry="config/manet-mesh.pref">
            <Parameter name="name" value="MANET Mesh Network Input"/>
        </Content>
    </Contents>
</MissionPackageManifest>`, uid)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	mf, _ := zw.Create("MANIFEST/manifest.xml")
	mf.Write([]byte(manifest))

	pf, _ := zw.Create("config/manet-mesh.pref")
	pf.Write([]byte(pref))

	zw.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="MANET-Mesh-CoT.zip"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
}

func applyEUDBandwidth(capMbit string) {
	iface := "br0"
	ifbDev := "ifb0"

	runCmd(3*time.Second, "tc", "qdisc", "del", "dev", iface, "root")
	runCmd(3*time.Second, "tc", "qdisc", "del", "dev", iface, "ingress")
	runCmd(3*time.Second, "tc", "qdisc", "del", "dev", ifbDev, "root")

	if capMbit == "" || capMbit == "0" {
		runCmd(3*time.Second, "ip", "link", "set", ifbDev, "down")
		return
	}

	rate := capMbit + "mbit"
	euds := getEUDs()
	if len(euds) == 0 {
		return
	}

	// Download shaping (br0 egress)
	runCmd(3*time.Second, "tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "99")
	runCmd(3*time.Second, "tc", "class", "add", "dev", iface, "parent", "1:", "classid", "1:99", "htb", "rate", "1000mbit")

	for i, eud := range euds {
		classID := fmt.Sprintf("1:%d", 10+i)
		handleID := fmt.Sprintf("%d:", 10+i)
		runCmd(3*time.Second, "tc", "class", "add", "dev", iface, "parent", "1:", "classid", classID, "htb", "rate", rate, "ceil", rate)
		runCmd(3*time.Second, "tc", "qdisc", "add", "dev", iface, "parent", classID, "handle", handleID, "sfq", "perturb", "10")
		runCmd(3*time.Second, "tc", "filter", "add", "dev", iface, "parent", "1:", "protocol", "ip", "prio", "1", "u32", "match", "ip", "dst", eud.IP+"/32", "flowid", classID)
	}

	// Upload shaping (br0 ingress via IFB redirect)
	runCmd(3*time.Second, "modprobe", "ifb")
	runCmd(3*time.Second, "ip", "link", "add", ifbDev, "type", "ifb")
	runCmd(3*time.Second, "ip", "link", "set", ifbDev, "up")
	runCmd(3*time.Second, "tc", "qdisc", "add", "dev", iface, "handle", "ffff:", "ingress")
	runCmd(3*time.Second, "tc", "filter", "add", "dev", iface, "parent", "ffff:", "protocol", "ip", "u32",
		"match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifbDev)

	runCmd(3*time.Second, "tc", "qdisc", "add", "dev", ifbDev, "root", "handle", "1:", "htb", "default", "99")
	runCmd(3*time.Second, "tc", "class", "add", "dev", ifbDev, "parent", "1:", "classid", "1:99", "htb", "rate", "1000mbit")

	for i, eud := range euds {
		classID := fmt.Sprintf("1:%d", 10+i)
		handleID := fmt.Sprintf("%d:", 10+i)
		runCmd(3*time.Second, "tc", "class", "add", "dev", ifbDev, "parent", "1:", "classid", classID, "htb", "rate", rate, "ceil", rate)
		runCmd(3*time.Second, "tc", "qdisc", "add", "dev", ifbDev, "parent", classID, "handle", handleID, "sfq", "perturb", "10")
		runCmd(3*time.Second, "tc", "filter", "add", "dev", ifbDev, "parent", "1:", "protocol", "ip", "prio", "1", "u32", "match", "ip", "src", eud.IP+"/32", "flowid", classID)
	}
}

func apiDownloadAPK(w http.ResponseWriter, r *http.Request) {
	apkPath := "/usr/local/share/manet/mesh-ctrl.apk"
	info, err := os.Stat(apkPath)
	if err != nil {
		writeJSON(w, 404, map[string]interface{}{"error": "APK not available on this node"})
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="mesh-ctrl.apk"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	http.ServeFile(w, r, apkPath)
}
