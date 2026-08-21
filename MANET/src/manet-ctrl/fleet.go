package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	fleetMcastAddr  = "239.30.2.70:17070"
	fleetMcastIface = "br0"
)

var (
	fleetAcksMu sync.Mutex
	fleetAcks   = map[string]string{} // mac -> version
)

func fleetConfigWatcher() {
	for {
		time.Sleep(10 * time.Second)
		fleetPollAlfred()
		fleetCheckActivation()
	}
}

func fleetCheckActivation() {
	raw := getPendingConfig()
	if raw == nil {
		return
	}
	var pkg map[string]interface{}
	if json.Unmarshal(raw, &pkg) != nil {
		return
	}
	at, ok := pkg["activate_at"].(float64)
	if !ok || at == 0 {
		return
	}
	if time.Now().Unix() < int64(at) {
		return
	}
	log.Printf("fleet: activation time reached, applying config")
	fleetApplyConfig(pkg)
	clearPendingConfig()
	log.Printf("fleet: config applied and pending cleared")
}

// expandNodeTemplates replaces {{hostname}} in staged config values with this
// node's current hostname prefix, so one fleet profile can be deployed to
// every node. The fleet UI has advertised this placeholder since the start,
// but nothing expanded it — the literal braces landed in mesh.conf and the
// hostname-apply path fell back to the "node" default.
func expandNodeTemplates(updates map[string]string, conf map[string]string) {
	prefix := conf["node_hostname"]
	if prefix == "" {
		// Derive the prefix from the OS hostname by stripping the
		// generated -<ssid>-<mac> suffix.
		host, _ := os.Hostname()
		if suffix := getMACsuffix(); suffix != "" {
			host = strings.TrimSuffix(host, "-"+suffix)
		}
		if ssid := conf["mesh_ssid"]; ssid != "" {
			host = strings.TrimSuffix(host, "-"+ssid)
		}
		prefix = host
	}
	for k, v := range updates {
		if strings.Contains(v, "{{hostname}}") {
			updates[k] = strings.ReplaceAll(v, "{{hostname}}", prefix)
		}
	}
}

func fleetApplyConfig(pkg map[string]interface{}) {
	configRaw, ok := pkg["config"].(map[string]interface{})
	if !ok {
		return
	}
	updates := make(map[string]string)
	for k, v := range configRaw {
		if saveableKeys[k] {
			updates[k] = fmt.Sprintf("%v", v)
		}
	}
	if len(updates) == 0 {
		return
	}
	expandNodeTemplates(updates, loadKVFile(MeshConfFile))
	if err := saveKVFile(MeshConfFile, updates); err != nil {
		log.Printf("fleet: apply save error: %v", err)
		return
	}

	conf := loadKVFile(MeshConfFile)

	// Skip when no prefix is configured — the "node" default fallback is
	// how fleet deploys renamed nodes to node-<mac>.
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
	}
	if updates["gateway"] != "" || updates["gateway_nat"] != "" || updates["gateway_mss_clamp"] != "" || updates["gateway_bandwidth"] != "" {
		runCmd(5*time.Second, "systemctl", "reload", "gateway-manager")
	}
	if (updates["lan_ap_ssid"] != "" || updates["lan_ap_key"] != "") && eudWantsAP(conf["eud"]) {
		applyHostapdConfig(conf)
		runCmd(10*time.Second, "systemctl", "restart", "hostapd")
	}
	if updates["mesh_ssid"] != "" || updates["mesh_key"] != "" {
		applyWPAConfig(conf)
	}
	if updates["multicast_mode"] != "" {
		applyMulticastMode(updates["multicast_mode"])
	}
	if updates["voice_mic_volume"] != "" || updates["voice_speaker_volume"] != "" {
		applyVoiceVolume(conf)
	}
	if updates["voice_enabled"] != "" {
		if conf["voice_enabled"] == "n" {
			runCmd(5*time.Second, "systemctl", "stop", "mesh-voice")
		} else {
			runCmd(5*time.Second, "systemctl", "restart", "mesh-voice")
		}
	}
	if conf["voice_enabled"] != "n" && (updates["voice_ptt_mode"] != "" || updates["voice_channel"] != "") {
		txCh := int(voiceTxCh.Load())
		if txCh <= 0 {
			txCh = 1
		}
		voiceRestartDaemon(txCh)
	}
	if updates["dns_servers"] != "" {
		applyDNSServers(updates["dns_servers"])
	}
	if (updates["lan_ap_channel"] != "" || updates["lan_ap_bw"] != "") && eudWantsAP(conf["eud"]) {
		runCmd(10*time.Second, "systemctl", "restart", "hostapd")
	}
	if updates["qos_enabled"] != "" || updates["qos_voice_band"] != "" || updates["qos_cot_band"] != "" || updates["qos_chat_band"] != "" {
		applyQoSFromConf(conf)
	}
	if updates["halow_bw"] != "" {
		applyHalowBW(conf)
	}
}

func fleetPollAlfred() {
	out, err := exec.Command("/usr/sbin/alfred", "-r", "70").Output()
	if err != nil || len(out) == 0 {
		return
	}

	// Alfred output has one line per node: { "mac", "json_payload" },
	// Parse all entries and process the most recently staged one
	myMAC := getMyMAC()
	var best []byte
	var bestStaged int64

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "\", \"")
		if idx < 0 {
			continue
		}
		mac := strings.TrimLeft(line[:idx], "{ \"")
		if strings.ReplaceAll(mac, ":", "") == strings.ReplaceAll(myMAC, ":", "") {
			continue
		}
		rest := line[idx+4:]
		end := strings.LastIndex(rest, "\"")
		if end < 0 {
			continue
		}
		payload := rest[:end]

		var raw string
		if json.Unmarshal([]byte("\""+payload+"\""), &raw) != nil {
			raw = payload
		}
		var pkg map[string]interface{}
		if json.Unmarshal([]byte(raw), &pkg) != nil {
			continue
		}
		stagedAt, _ := pkg["staged_at"].(float64)
		if int64(stagedAt) > bestStaged {
			bestStaged = int64(stagedAt)
			best = []byte(raw)
		}
	}

	if best != nil {
		fleetProcessPackage(best)
	}
}

func fleetProcessPackage(data []byte) {
	var pkg map[string]interface{}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}
	version, _ := pkg["version"].(string)
	if version == "" {
		return
	}

	// Check if we already have this version ACKed
	existing, _ := os.ReadFile(AckVersionFile)
	if strings.TrimSpace(string(existing)) == version {
		// Already ACKed — but check if remote added activate_at that we don't have yet
		if activateAt, ok := pkg["activate_at"].(float64); ok && activateAt > 0 {
			local := getPendingConfig()
			if local != nil {
				var localPkg map[string]interface{}
				if json.Unmarshal(local, &localPkg) == nil {
					if _, has := localPkg["activate_at"]; !has {
						savePendingConfig(pkg)
						log.Printf("fleet: activation received for version %s (at %d)", version, int64(activateAt))
					}
				}
			}
		}
		return
	}

	// Save as pending config
	savePendingConfig(pkg)

	// Sync profiles from the staging node so all nodes share the same view
	fleetSyncProfiles(pkg)

	// Write ACK
	os.WriteFile(AckVersionFile, []byte(version), 0644)
	log.Printf("fleet: ACKed config version %s", version)

	// Broadcast ACK via multicast for fast propagation
	fleetMcastSendAck(version)
}

func fleetSyncProfiles(pkg map[string]interface{}) {
	prefs := loadFleetPreferences()

	if profiles, ok := pkg["profiles"].(map[string]interface{}); ok {
		synced := make(map[string]FleetProfile)
		for pid, pv := range profiles {
			pm, _ := pv.(map[string]interface{})
			name, _ := pm["name"].(string)
			cfg := make(map[string]string)
			if cfgRaw, ok := pm["config"].(map[string]interface{}); ok {
				for k, v := range cfgRaw {
					cfg[k] = fmt.Sprintf("%v", v)
				}
			}
			synced[pid] = FleetProfile{Name: name, Config: cfg}
		}
		if len(synced) > 0 {
			prefs.Profiles = synced
		}
	}

	if np, ok := pkg["node_profiles"].(map[string]interface{}); ok {
		synced := make(map[string]string)
		for mac, pid := range np {
			synced[mac], _ = pid.(string)
		}
		prefs.NodeProfiles = synced
	}

	if config, ok := pkg["config"].(map[string]interface{}); ok {
		mc := make(map[string]string)
		for k, v := range config {
			mc[k] = fmt.Sprintf("%v", v)
		}
		prefs.MeshConfig = mc
	}

	saveFleetPreferences(prefs)
	log.Printf("fleet: synced profiles from staged package")
}

func fleetMcastSendActivation(version string, activateAt int64) {
	addr, err := net.ResolveUDPAddr("udp4", fleetMcastAddr)
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	msg := map[string]interface{}{
		"type":        "fleet_activate",
		"version":     version,
		"activate_at": activateAt,
	}
	data, _ := json.Marshal(msg)
	conn.Write(data)
}

func fleetMcastSendAck(version string) {
	addr, err := net.ResolveUDPAddr("udp4", fleetMcastAddr)
	if err != nil {
		return
	}
	iface, _ := net.InterfaceByName(fleetMcastIface)
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = iface // used for receive side

	hostname, _ := os.Hostname()
	mac := getMyMAC()
	msg := map[string]string{
		"type":     "fleet_ack",
		"version":  version,
		"hostname": hostname,
		"mac":      mac,
	}
	data, _ := json.Marshal(msg)
	conn.Write(data)
}

func fleetMcastListener() {
	addr, err := net.ResolveUDPAddr("udp4", fleetMcastAddr)
	if err != nil {
		log.Printf("fleet mcast: resolve error: %v", err)
		return
	}
	iface, err := net.InterfaceByName(fleetMcastIface)
	if err != nil {
		log.Printf("fleet mcast: interface %s not found, retrying in 30s", fleetMcastIface)
		time.Sleep(30 * time.Second)
		iface, err = net.InterfaceByName(fleetMcastIface)
		if err != nil {
			log.Printf("fleet mcast: giving up on %s", fleetMcastIface)
			return
		}
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, addr)
	if err != nil {
		log.Printf("fleet mcast: listen error: %v", err)
		return
	}
	defer conn.Close()
	conn.SetReadBuffer(4096)

	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var msg map[string]interface{}
		if json.Unmarshal(buf[:n], &msg) != nil {
			continue
		}
		msgType, _ := msg["type"].(string)
		if msgType == "fleet_stage" {
			fleetProcessPackage(buf[:n])
		} else if msgType == "fleet_ack" {
			mac, _ := msg["mac"].(string)
			version, _ := msg["version"].(string)
			if mac != "" && version != "" {
				fleetAcksMu.Lock()
				fleetAcks[normMAC(mac)] = version
				fleetAcksMu.Unlock()
			}
		} else if msgType == "fleet_activate" {
			version, _ := msg["version"].(string)
			activateAt, _ := msg["activate_at"].(float64)
			if version != "" && activateAt > 0 {
				local := getPendingConfig()
				if local != nil {
					var localPkg map[string]interface{}
					if json.Unmarshal(local, &localPkg) == nil {
						localVer, _ := localPkg["version"].(string)
						if localVer == version {
							if _, has := localPkg["activate_at"]; !has {
								localPkg["activate_at"] = activateAt
								savePendingConfig(localPkg)
								log.Printf("fleet: mcast activation for version %s", version)
							}
						}
					}
				}
			}
		}
	}
}
