package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

func makeConfigVersion(config map[string]string) string {
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf(`"%s":"%s"`, k, config[k])
	}
	data := fmt.Sprintf("{%s}@%d", strings.Join(parts, ","), time.Now().UnixNano())
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", h)[:8]
}

func getPendingConfig() json.RawMessage {
	data, err := os.ReadFile(PendingConfFile)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

func savePendingConfig(pkg map[string]interface{}) error {
	data, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	return os.WriteFile(PendingConfFile, data, 0644)
}

func clearPendingConfig() {
	os.Remove(PendingConfFile)
}

func broadcastConfigPackage(pkg map[string]interface{}) bool {
	data, err := json.Marshal(pkg)
	if err != nil {
		return false
	}
	cmd := exec.Command("alfred", "-s", "70")
	cmd.Stdin = strings.NewReader(string(data))
	err = cmd.Run()
	return err == nil
}

func loadFleetPreferences() FleetPreferences {
	var prefs FleetPreferences
	data, err := os.ReadFile(FleetPrefsFile)
	if err == nil {
		json.Unmarshal(data, &prefs)
	}
	if prefs.Profiles == nil || len(prefs.Profiles) == 0 {
		prefs.Profiles = map[string]FleetProfile{
			"default": {Name: "Default", Config: map[string]string{}},
		}
	}
	if prefs.NodeProfiles == nil {
		prefs.NodeProfiles = map[string]string{}
	}
	if prefs.MeshConfig == nil {
		prefs.MeshConfig = map[string]string{}
	}
	return prefs
}

func saveFleetPreferences(prefs FleetPreferences) error {
	data, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	return os.WriteFile(FleetPrefsFile, data, 0644)
}

func assembleAdminStatus() AdminStatus {
	conf := loadKVFile(MeshConfFile)
	registry := parseRegistry()
	pending := getPendingConfig()
	prefs := loadFleetPreferences()

	currentConfig := map[string]string{
		"node_hostname":     conf["node_hostname"],
		"eud":               confGet(conf, "eud", "wired"),
		"lan_ap_ssid":       conf["lan_ap_ssid"],
		"lan_ap_key":        conf["lan_ap_key"],
		"max_euds_per_node": confGet(conf, "max_euds_per_node", "0"),
		"mesh_ssid":         conf["mesh_ssid"],
		"mesh_key":          conf["mesh_key"],
		"ipv4_network":      confGet(conf, "ipv4_network", "10.30.2.0/24"),
		"regulatory_domain": confGet(conf, "regulatory_domain", "US"),
		"admin_password":    conf["admin_password"],
		"gateway":           confGet(conf, "gateway", "y"),
		"gateway_nat":       confGet(conf, "gateway_nat", "y"),
		"gateway_mss_clamp": confGet(conf, "gateway_mss_clamp", "y"),
		"gateway_bandwidth": confGet(conf, "gateway_bandwidth", ""),
		"halow_bw":          confGet(conf, "halow_bw", ""),
		"multicast_mode":    confGet(conf, "multicast_mode", "flood"),
		"lan_ap_channel":      conf["lan_ap_channel"],
		"lan_ap_bw":           confGet(conf, "lan_ap_bw", "20"),
		"voice_mic_volume":     confGet(conf, "voice_mic_volume", "80"),
		"voice_speaker_volume": confGet(conf, "voice_speaker_volume", "80"),
		"voice_channel":        confGet(conf, "voice_channel", "1"),
		"voice_rx_channels":    confGet(conf, "voice_rx_channels", "1"),
		"voice_ptt_mode":       confGet(conf, "voice_ptt_mode", "openvlm"),
		"voice_enabled":        confGet(conf, "voice_enabled", "y"),
		"dns_servers":          confGet(conf, "dns_servers", "8.8.8.8,8.8.4.4"),
		"qos_enabled":          confGet(conf, "qos_enabled", "y"),
		"qos_voice_band":       confGet(conf, "qos_voice_band", "0"),
		"qos_cot_band":         confGet(conf, "qos_cot_band", "1"),
		"qos_chat_band":        confGet(conf, "qos_chat_band", "2"),
		"require_auth":         confGet(conf, "require_auth", "n"),
		"auto_update":          confGet(conf, "auto_update", "n"),
		"update_url":           confGet(conf, "update_url", ""),
		"eud_bandwidth":        confGet(conf, "eud_bandwidth", "0"),
		"battery_monitor":      confGet(conf, "battery_monitor", "y"),
		"voice_beep_tx_start":  confGet(conf, "voice_beep_tx_start", "y"),
		"voice_beep_rx_end":    confGet(conf, "voice_beep_rx_end", "y"),
		"voice_gain":           confGet(conf, "voice_gain", "3.0"),
		"gps":                  confGet(conf, "gps", "y"),
		"gps_source":           confGet(conf, "gps_source", "receiver"),
		"gps_static_lat":       conf["gps_static_lat"],
		"gps_static_lon":       conf["gps_static_lon"],
		"gps_static_alt":       conf["gps_static_alt"],
		"callsign":             conf["callsign"],
		"cot_type":             conf["cot_type"],
		"cot_team":             conf["cot_team"],
		"cot_role":             conf["cot_role"],
		"cot_icon":             conf["cot_icon"],
	}

	if currentConfig["halow_bw"] == "" {
		info := getHalowDriverInfo("wlan2")
		if bw, ok := info["halow_bw"]; ok {
			currentConfig["halow_bw"] = bw
		} else {
			currentConfig["halow_bw"] = "8MHz"
		}
	}

	myMAC := getMyMAC()
	localAck := ""
	if b, err := os.ReadFile(AckVersionFile); err == nil {
		localAck = strings.TrimSpace(string(b))
	}

	var adminNodes []AdminNode
	activeCount := 0
	for _, rn := range registry {
		state := rn["NODE_STATE"]
		if state == "ACTIVE" {
			activeCount++
		}
		mac := normMAC(rn["MAC_ADDRESS"])
		profile := prefs.NodeProfiles[mac]
		if profile == "" {
			profile = "default"
		}
		ack := rn["CONFIG_ACK_VERSION"]
		if mac == myMAC && localAck != "" {
			ack = localAck
		}
		fleetAcksMu.Lock()
		if mcastAck, ok := fleetAcks[mac]; ok && mcastAck != "" && ack == "" {
			ack = mcastAck
		}
		fleetAcksMu.Unlock()
		adminNodes = append(adminNodes, AdminNode{
			Hostname:  rn["HOSTNAME"],
			IP:        rn["IPV4_ADDRESS"],
			MAC:       rn["MAC_ADDRESS"],
			Ack:       ack,
			LastSeen:  rn["LAST_SEEN_TIMESTAMP"],
			NodeState: state,
			Profile:   profile,
		})
	}

	return AdminStatus{
		CurrentConfig: currentConfig,
		Pending:       pending,
		Nodes:         adminNodes,
		TotalNodes:    len(registry),
		ActiveNodes:   activeCount,
		MyHostname:    getMyHostname(),
		Preferences:   prefs,
	}
}

func getMACsuffix() string {
	out, err := runCmdStdout(3*time.Second, "ip", "-br", "link", "show")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && (strings.HasPrefix(fields[0], "eth") || strings.HasPrefix(fields[0], "end")) {
			mac := fields[2]
			parts := strings.Split(mac, ":")
			if len(parts) >= 4 {
				return strings.Join(parts[len(parts)-4:], "")
			}
		}
	}
	return ""
}
