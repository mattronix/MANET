package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	controlIface     = "br0"
	registryFile     = "/var/run/mesh_node_registry"
	claimedChunksFile = "/tmp/claimed_chunks.txt"
	persistentState  = "/etc/mesh_ipv4_state"
	meshConfFile     = "/etc/mesh.conf"
	forceConfFile    = "/etc/manet/mesh-ip-force.conf"
	hostsFile        = "/etc/hosts"
	hostsBegin       = "# === BEGIN MESH HOSTS ==="
	hostsEnd         = "# === END MESH HOSTS ==="
	gwStateFile      = "/var/run/mesh-gateway.state"
	myChunkFile      = "/var/run/my_ipv4_chunk"
	dnsmasqConf      = "/etc/dnsmasq.d/mesh-eud.conf"
	dnsmasqUpstream  = "/etc/dnsmasq.d/upstream-dns.conf"
	ebtablesRules    = "/etc/ebtables.rules"
	servicesReserved = 5
)

func run(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		cmd.Process.Kill()
		return "", fmt.Errorf("timeout")
	}
}

func readText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func loadKV(path string) map[string]string {
	m := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.Trim(strings.TrimSpace(line[i+1:]), "'\"")
			m[k] = v
		}
	}
	return m
}

// --- IP math ---

func ipToInt(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}

func intToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}

func cidrRange(cidr string) (min, max uint32, prefix int, ok bool) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, 0, 0, false
	}
	ones, bits := ipNet.Mask.Size()
	netAddr := ipToInt(ipNet.IP)
	hostMin := netAddr + 1
	hostMax := netAddr + (1 << uint(bits-ones)) - 2
	return hostMin, hostMax, ones, true
}

type chunkIPs struct {
	Primary, Secondary, DHCPStart, DHCPEnd net.IP
}

func getChunkIPs(network string, chunkNum, chunkSize int) (chunkIPs, bool) {
	hostMin, _, _, ok := cidrRange(network)
	if !ok {
		return chunkIPs{}, false
	}
	start := hostMin + uint32(servicesReserved) + uint32(chunkNum*chunkSize)
	return chunkIPs{
		Primary:   intToIP(start),
		Secondary: intToIP(start + 1),
		DHCPStart: intToIP(start + 2),
		DHCPEnd:   intToIP(start + uint32(chunkSize) - 1),
	}, true
}

func maxChunks(network string, chunkSize int) int {
	hostMin, hostMax, _, ok := cidrRange(network)
	if !ok {
		return 0
	}
	available := int(hostMax-hostMin+1) - servicesReserved
	return available / chunkSize
}

func ipInCIDR(ipStr, cidr string) bool {
	ip := net.ParseIP(ipStr)
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil || ip == nil {
		return false
	}
	return ipNet.Contains(ip)
}

func isServiceReserved(ipStr, network string) bool {
	hostMin, _, _, ok := cidrRange(network)
	if !ok {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	offset := int(ipToInt(ip.To4())) - int(hostMin)
	return offset >= 0 && offset < servicesReserved
}

func prefixLen(network string) string {
	if i := strings.IndexByte(network, '/'); i > 0 {
		return network[i+1:]
	}
	return "24"
}

// --- System helpers ---

func myMAC() string {
	data, _ := os.ReadFile("/sys/class/net/" + controlIface + "/address")
	return strings.TrimSpace(string(data))
}

func macIsLocal(mac string) bool {
	if mac == "" {
		return false
	}
	matches, _ := filepath.Glob("/sys/class/net/*/address")
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) == mac {
			return true
		}
	}
	return false
}

func br0IPs() []string {
	out, err := run(3*time.Second, "ip", "-4", "-o", "addr", "show", "dev", controlIface)
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`inet\s+(\d+\.\d+\.\d+\.\d+)`)
	var ips []string
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		ips = append(ips, m[1])
	}
	return ips
}

// --- Registry parsing ---

type registryNode struct {
	Hostname string
	IP       string
	MACs     string
}

var regRE = regexp.MustCompile(`NODE_([A-Fa-f0-9]+)_([A-Z0-9_]+)='([^']*)'`)

func parseRegistry() map[string]registryNode {
	nodes := make(map[string]registryNode)
	data, err := os.ReadFile(registryFile)
	if err != nil {
		return nodes
	}
	for _, m := range regRE.FindAllStringSubmatch(string(data), -1) {
		id, field, val := m[1], m[2], m[3]
		n := nodes[id]
		switch field {
		case "HOSTNAME":
			n.Hostname = val
		case "IPV4_ADDRESS":
			n.IP = val
		case "MAC_ADDRESSES":
			n.MACs = val
		}
		nodes[id] = n
	}
	return nodes
}

func lookupGatewayIP(gwMAC string) string {
	nodes := parseRegistry()
	gwMAC = strings.ToLower(gwMAC)
	for _, n := range nodes {
		if strings.Contains(strings.ToLower(n.MACs), gwMAC) && n.IP != "" {
			return n.IP
		}
	}
	return ""
}

// ============================================================
// IP Manager
// ============================================================

type ipManager struct {
	network   string
	chunkSize int
	maxEUDs   int
	pIP       string
	pChunk    int
	pNetwork  string
	pValid    bool
}

func newIPManager() *ipManager {
	conf := loadKV(meshConfFile)
	maxEUDs, _ := strconv.Atoi(conf["max_euds_per_node"])
	if maxEUDs < 1 {
		maxEUDs = 1
	}
	eudMode := conf["eud"]
	if eudMode == "" {
		eudMode = "none"
	}
	if eudMode != "none" && maxEUDs < 1 {
		maxEUDs = 1
	}
	network := conf["ipv4_network"]
	if network == "" {
		network = "10.43.1.0/16"
	}

	im := &ipManager{
		network:   network,
		maxEUDs:   maxEUDs,
		chunkSize: maxEUDs + 2,
		pChunk:    -1,
	}

	// Load persistent state
	ps := loadKV(persistentState)
	if ps["PERSISTENT_IPV4"] != "" && ps["PERSISTENT_CHUNK"] != "" {
		im.pIP = ps["PERSISTENT_IPV4"]
		im.pChunk, _ = strconv.Atoi(ps["PERSISTENT_CHUNK"])
		im.pNetwork = ps["PERSISTENT_NETWORK"]
		im.pValid = true
		log.Printf("Loaded persistent state: chunk=%d ip=%s", im.pChunk, im.pIP)
	}

	// Load forced config
	if _, err := os.Stat(forceConfFile); err == nil {
		fc := loadKV(forceConfFile)
		if fc["FORCED_IPV4"] != "" && fc["FORCED_CHUNK"] != "" {
			im.pIP = fc["FORCED_IPV4"]
			im.pChunk, _ = strconv.Atoi(fc["FORCED_CHUNK"])
			im.pValid = true
			log.Printf("Forced config: chunk=%d ip=%s", im.pChunk, im.pIP)
		}
	}

	return im
}

func (im *ipManager) savePersistent() {
	content := fmt.Sprintf("PERSISTENT_IPV4=\"%s\"\nPERSISTENT_CHUNK=\"%d\"\nPERSISTENT_NETWORK=\"%s\"\n",
		im.pIP, im.pChunk, im.pNetwork)
	os.WriteFile(persistentState, []byte(content), 0644)
}

func (im *ipManager) claimedChunks() map[int]string {
	claimed := make(map[int]string)
	data, err := os.ReadFile(claimedChunksFile)
	if err != nil {
		return claimed
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ",", 2)
		if len(parts) == 2 {
			c, _ := strconv.Atoi(parts[0])
			claimed[c] = parts[1]
		}
	}
	return claimed
}

func (im *ipManager) randomAvailableChunk() (int, bool) {
	mc := maxChunks(im.network, im.chunkSize)
	if mc < 1 {
		log.Printf("Network too small for chunk_size=%d", im.chunkSize)
		return 0, false
	}
	claimed := im.claimedChunks()
	var available []int
	for i := 0; i < mc; i++ {
		if _, ok := claimed[i]; !ok {
			available = append(available, i)
		}
	}
	if len(available) == 0 {
		log.Printf("No available chunks")
		return 0, false
	}
	return available[rand.Intn(len(available))], true
}

func (im *ipManager) configureEbtables() {
	log.Printf("Configuring ebtables DHCP isolation")
	run(5*time.Second, "ebtables", "-F", "FORWARD")

	if _, err := os.Stat("/sys/class/net/bat0"); err == nil {
		run(5*time.Second, "ebtables", "-A", "FORWARD", "-o", "bat0", "-p", "IPv4",
			"--ip-protocol", "udp", "--ip-destination-port", "67:68", "-j", "DROP")
		run(5*time.Second, "ebtables", "-A", "FORWARD", "-i", "bat0", "-p", "IPv4",
			"--ip-protocol", "udp", "--ip-destination-port", "67:68", "-j", "DROP")
	}

	apIface := readText("/var/lib/ap_interface")
	matches, _ := filepath.Glob("/sys/class/net/wlan*")
	for _, path := range matches {
		iface := filepath.Base(path)
		if apIface != "" && iface == apIface {
			continue
		}
		out, err := run(3*time.Second, "ip", "link", "show", iface)
		if err == nil && strings.Contains(out, "master") {
			run(5*time.Second, "ebtables", "-A", "FORWARD", "-o", iface, "-p", "IPv4",
				"--ip-protocol", "udp", "--ip-destination-port", "67:68", "-j", "DROP")
			run(5*time.Second, "ebtables", "-A", "FORWARD", "-i", iface, "-p", "IPv4",
				"--ip-protocol", "udp", "--ip-destination-port", "67:68", "-j", "DROP")
		}
	}
	run(5*time.Second, "ebtables-save")
	// ebtables-save outputs to stdout; redirect to file
	if out, err := run(5*time.Second, "ebtables-save"); err == nil {
		os.WriteFile(ebtablesRules, []byte(out), 0644)
	}
}

func (im *ipManager) configureDnsmasq(c chunkIPs) {
	log.Printf("Configuring dnsmasq: pool=%s-%s gateway=%s",
		c.DHCPStart, c.DHCPEnd, c.Secondary)

	conf := fmt.Sprintf(`interface=br0
bind-interfaces
dhcp-range=%s,%s,4m
dhcp-option=3,%s
dhcp-option=6,%s
domain=mesh
local=/mesh/
resolv-file=/run/systemd/resolve/resolv.conf
address=/manet.mesh/%s
address=/perf.mesh/%s
log-dhcp
`, c.DHCPStart, c.DHCPEnd, c.Secondary, c.Secondary, c.Secondary, c.Secondary)

	os.MkdirAll("/etc/dnsmasq.d", 0755)
	os.WriteFile(dnsmasqConf, []byte(conf), 0644)
	os.WriteFile(dnsmasqUpstream, []byte("server=1.1.1.1\nserver=8.8.8.8\n"), 0644)

	run(5*time.Second, "systemctl", "unmask", "dnsmasq.service")
	out, _ := run(3*time.Second, "systemctl", "is-active", "--quiet", "dnsmasq.service")
	_ = out
	if exec.Command("systemctl", "is-active", "--quiet", "dnsmasq.service").Run() == nil {
		run(10*time.Second, "systemctl", "restart", "dnsmasq.service")
	} else {
		run(10*time.Second, "systemctl", "start", "dnsmasq.service")
	}

	// The web UI is restricted to whoever holds a lease from this node, so
	// the firewall rule has to follow the pool whenever it moves.
	if _, err := os.Stat("/usr/local/bin/manet-ui-firewall.sh"); err == nil {
		run(5*time.Second, "/usr/local/bin/manet-ui-firewall.sh")
	}
}

func (im *ipManager) ensureAddr(ip string) {
	if ip == "" {
		return
	}
	out, _ := run(3*time.Second, "ip", "-4", "addr", "show", "dev", controlIface)
	if !strings.Contains(out, ip) {
		run(5*time.Second, "ip", "addr", "add", ip+"/"+prefixLen(im.network), "dev", controlIface)
		log.Printf("Restored %s address: %s", controlIface, ip)
	}
}

func (im *ipManager) cleanupStaleAddrs() {
	if !im.pValid || im.pChunk < 0 {
		return
	}
	c, ok := getChunkIPs(im.network, im.pChunk, im.chunkSize)
	if !ok {
		return
	}
	keep := map[string]bool{c.Primary.String(): true, c.Secondary.String(): true}
	pLen := prefixLen(im.network)
	for _, ip := range br0IPs() {
		if keep[ip] || !ipInCIDR(ip, im.network) || isServiceReserved(ip, im.network) {
			continue
		}
		run(5*time.Second, "ip", "addr", "del", ip+"/"+pLen, "dev", controlIface)
		log.Printf("Removed stale %s alias: %s", controlIface, ip)
	}
}

func (im *ipManager) run() {
	mac := myMAC()
	if mac == "" {
		log.Printf("Cannot read MAC from %s", controlIface)
		return
	}

	im.cleanupStaleAddrs()

	currentIP := ""
	for _, ip := range br0IPs() {
		currentIP = ip
		break
	}

	configured := currentIP != ""
	if configured && !im.pValid && isServiceReserved(currentIP, im.network) {
		configured = false
		currentIP = ""
	}

	pLen := prefixLen(im.network)

	if !configured {
		// UNCONFIGURED — select and claim a chunk
		chunk := -1
		usePersistent := false

		if im.pValid && im.pIP != "" {
			if im.pNetwork != "" && im.pNetwork != im.network {
				log.Printf("Network changed %s -> %s, selecting new chunk", im.pNetwork, im.network)
				im.pValid = false
				im.savePersistent()
			} else if ipInCIDR(im.pIP, im.network) {
				chunk = im.pChunk
				usePersistent = true
			} else {
				im.pValid = false
				im.savePersistent()
			}
		}

		if chunk < 0 {
			var ok bool
			chunk, ok = im.randomAvailableChunk()
			if !ok {
				return
			}
		}

		if !usePersistent {
			claimed := im.claimedChunks()
			if _, taken := claimed[chunk]; taken {
				log.Printf("Chunk %d in use, will retry", chunk)
				return
			}
		}

		c, ok := getChunkIPs(im.network, chunk, im.chunkSize)
		if !ok {
			return
		}

		log.Printf("Claiming chunk %d: primary=%s gateway=%s dhcp=%s-%s",
			chunk, c.Primary, c.Secondary, c.DHCPStart, c.DHCPEnd)

		run(5*time.Second, "ip", "addr", "add", c.Primary.String()+"/"+pLen, "dev", controlIface)
		run(5*time.Second, "ip", "addr", "add", c.Secondary.String()+"/"+pLen, "dev", controlIface)

		im.configureEbtables()
		im.configureDnsmasq(c)

		im.pIP = c.Primary.String()
		im.pChunk = chunk
		im.pNetwork = im.network
		im.pValid = true
		im.savePersistent()

		os.WriteFile(myChunkFile, []byte(strconv.Itoa(chunk)), 0644)
		log.Printf("Successfully claimed chunk %d", chunk)
		go meshHook("ip-change", "IP="+c.Primary.String(), "GATEWAY="+c.Secondary.String())
	} else {
		// CONFIGURED — check for conflicts
		claimed := im.claimedChunks()
		for chunkNum, claimedMAC := range claimed {
			c, ok := getChunkIPs(im.network, chunkNum, im.chunkSize)
			if !ok {
				continue
			}
			if c.Primary.String() == currentIP && !macIsLocal(claimedMAC) {
				log.Printf("CONFLICT for %s! MAC: %s chunk %d", currentIP, claimedMAC, chunkNum)
				if mac > claimedMAC {
					log.Printf("Won tie-breaker, defending chunk")
				} else {
					log.Printf("Lost tie-breaker, releasing chunk")
					if im.pValid {
						pc, ok := getChunkIPs(im.network, im.pChunk, im.chunkSize)
						if ok {
							run(5*time.Second, "ip", "addr", "del", pc.Primary.String()+"/"+pLen, "dev", controlIface)
							run(5*time.Second, "ip", "addr", "del", pc.Secondary.String()+"/"+pLen, "dev", controlIface)
						}
					}
					im.pIP = ""
					im.pChunk = -1
					im.pNetwork = ""
					im.pValid = false
					im.savePersistent()
					os.Remove(myChunkFile)
				}
				return
			}
		}

		// No conflict — ensure addresses and dnsmasq are correct
		if im.pValid && im.pChunk >= 0 {
			os.WriteFile(myChunkFile, []byte(strconv.Itoa(im.pChunk)), 0644)
			c, ok := getChunkIPs(im.network, im.pChunk, im.chunkSize)
			if ok {
				im.ensureAddr(c.Primary.String())
				im.ensureAddr(c.Secondary.String())

				needsUpdate := false
				data, err := os.ReadFile(dnsmasqConf)
				if err != nil {
					needsUpdate = true
				} else {
					text := string(data)
					if !strings.Contains(text, "dhcp-range="+c.DHCPStart.String()) ||
						!strings.Contains(text, "dhcp-option=3,"+c.Secondary.String()) {
						needsUpdate = true
					}
				}
				if needsUpdate {
					log.Printf("DHCP config changed, reconfiguring")
					im.configureEbtables()
					im.configureDnsmasq(c)
				}
			}
		}
	}
}

// ============================================================
// Hosts Updater
// ============================================================

var lastHostsBlock string

func updateHosts() {
	nodes := parseRegistry()
	if len(nodes) == 0 {
		return
	}

	// Sort by id: map iteration order is randomized, and an unsorted block
	// would defeat the change-gate below by "changing" on every call even
	// when the node set is identical.
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var block strings.Builder
	block.WriteString(hostsBegin + "\n")
	count := 0
	for _, id := range ids {
		n := nodes[id]
		if n.Hostname != "" && n.IP != "" {
			fmt.Fprintf(&block, "%s    %s %s.mesh\n", n.IP, n.Hostname, n.Hostname)
			count++
		}
	}
	block.WriteString(hostsEnd)

	blockStr := block.String()
	if blockStr == lastHostsBlock {
		return
	}

	data, err := os.ReadFile(hostsFile)
	if err != nil {
		data = []byte{}
	}
	text := string(data)

	if strings.Contains(text, hostsBegin) {
		// Replace existing block
		re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(hostsBegin) + `.*?` + regexp.QuoteMeta(hostsEnd))
		text = re.ReplaceAllString(text, blockStr)
	} else {
		text = text + "\n" + blockStr + "\n"
	}

	tmp := hostsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0644); err != nil {
		log.Printf("write hosts: %v", err)
		return
	}
	if err := os.Rename(tmp, hostsFile); err != nil {
		log.Printf("rename hosts: %v", err)
		return
	}
	lastHostsBlock = blockStr
	log.Printf("Updated %d mesh host entries", count)
}

// ============================================================
// Mesh DNS (dnsmasq address records for EUDs)
// ============================================================

const (
	dnsNamesFile = "/etc/dnsmasq.d/mesh-names.conf"
	appletsDir   = "/usr/local/share/manet/applets"
)

var lastDNSHash string

func updateMeshDNS() {
	nodes := parseRegistry()
	myIP := getLocalBr0IP()

	// Sort by id: map iteration order is randomized, and an unsorted block
	// would defeat the change-gate below by "changing" on every call even
	// when the node set is identical (same fix as updateHosts above).
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString("# Auto-generated mesh DNS records\n")
	b.WriteString("local=/mesh/\n")

	b.WriteString(fmt.Sprintf("address=/radio.mesh/%s\n", myIP))

	for _, id := range ids {
		n := nodes[id]
		if n.Hostname != "" && n.IP != "" {
			b.WriteString(fmt.Sprintf("address=/%s.mesh/%s\n", n.Hostname, n.IP))
		}
	}

	entries, _ := os.ReadDir(appletsDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(appletsDir, e.Name(), "applet.json"))
		if err != nil {
			continue
		}
		var m struct {
			DNS []struct {
				Name  string `json:"name"`
				Scope string `json:"scope"`
			} `json:"dns"`
		}
		if json.Unmarshal(data, &m) != nil || len(m.DNS) == 0 {
			continue
		}
		for _, d := range m.DNS {
			if d.Scope == "local" {
				b.WriteString(fmt.Sprintf("address=/%s/%s\n", d.Name, myIP))
			}
		}
	}

	content := b.String()
	if content == lastDNSHash {
		return
	}
	lastDNSHash = content

	if err := os.WriteFile(dnsNamesFile, []byte(content), 0644); err != nil {
		log.Printf("Failed to write mesh DNS: %v", err)
		return
	}
	run(10*time.Second, "systemctl", "restart", "dnsmasq")
	log.Printf("Updated mesh DNS records")
}

func getLocalBr0IP() string {
	iface, err := net.InterfaceByName("br0")
	if err != nil {
		return "127.0.0.1"
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}

// ============================================================
// Gateway Route Manager
// ============================================================

func getGatewayMAC() string {
	out, err := run(5*time.Second, "batctl", "gwl")
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?m)^\s*(?:=>)?\s*([0-9a-f:]{17})`)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "=>") || strings.HasPrefix(strings.TrimSpace(line), "*") {
			if m := re.FindStringSubmatch(line); m != nil {
				return strings.ToLower(m[1])
			}
		}
	}
	// No selected gateway, try first one
	if m := re.FindStringSubmatch(out); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func gatewayRouteLoop(stop <-chan struct{}) {
	log.Printf("Gateway route manager started (10s poll)")

	for {
		select {
		case <-stop:
			return
		default:
		}

		if _, err := os.Stat(gwStateFile); err == nil {
			// Local gateway mode — remove mesh default route on br0
			out, _ := run(3*time.Second, "ip", "route", "show", "default")
			if strings.Contains(out, "dev br0") {
				run(5*time.Second, "ip", "route", "del", "default", "dev", "br0")
				log.Printf("Gateway mode: removed mesh default route")
			}
			time.Sleep(10 * time.Second)
			continue
		}

		gwMAC := getGatewayMAC()
		if gwMAC == "" {
			time.Sleep(10 * time.Second)
			continue
		}

		gwIP := lookupGatewayIP(gwMAC)
		if gwIP == "" {
			time.Sleep(10 * time.Second)
			continue
		}

		localIP := ""
		for _, ip := range br0IPs() {
			localIP = ip
			break
		}
		if localIP == "" {
			time.Sleep(10 * time.Second)
			continue
		}

		curRoute, _ := run(3*time.Second, "ip", "route", "show", "default")
		if exec.Command("ping", "-c", "1", "-W", "1", gwIP).Run() == nil {
			if !strings.Contains(curRoute, "via "+gwIP+" dev br0") {
				run(5*time.Second, "ip", "route", "replace", "default", "via", gwIP, "dev", "br0", "src", localIP)
				log.Printf("Default route: via %s src %s", gwIP, localIP)
			}
		}

		time.Sleep(10 * time.Second)
	}
}

func defaultRouteFix() {
	// Skip for gateway nodes
	out, _ := run(5*time.Second, "batctl", "gw_mode")
	if strings.Contains(out, "server") {
		return
	}
	if _, err := os.Stat(gwStateFile); err == nil {
		return
	}

	for i := 0; i < 30; i++ {
		out, _ := run(3*time.Second, "ip", "route", "show", "default")
		if strings.Contains(out, "default") {
			log.Printf("Default route confirmed")
			return
		}

		gwMAC := getGatewayMAC()
		if gwMAC == "" {
			time.Sleep(2 * time.Second)
			continue
		}
		gwIP := lookupGatewayIP(gwMAC)
		localIP := ""
		for _, ip := range br0IPs() {
			localIP = ip
			break
		}
		if gwIP != "" && localIP != "" {
			if exec.Command("ping", "-c", "1", "-W", "1", gwIP).Run() == nil {
				run(5*time.Second, "ip", "route", "replace", "default", "via", gwIP, "dev", "br0", "src", localIP)
				log.Printf("Default route set: via %s src %s", gwIP, localIP)
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// ============================================================
// Voice QoS (tc + DSCP prioritization)
// ============================================================

func setupVoiceQoS() {
	ifaces := []string{"br0"}
	if data, err := os.ReadFile("/var/lib/mesh_if"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if f := strings.TrimSpace(line); f != "" {
				ifaces = append(ifaces, f)
			}
		}
	}

	for _, iface := range ifaces {
		exec.Command("/usr/sbin/tc", "qdisc", "del", "dev", iface, "root").Run()

		if out, err := run(5*time.Second, "/usr/sbin/tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "prio",
			"bands", "3", "priomap",
			"1", "2", "2", "2", "1", "2", "0", "0",
			"1", "1", "1", "1", "1", "1", "1", "1"); err != nil {
			log.Printf("QoS: tc qdisc on %s failed: %s %v", iface, out, err)
			continue
		}

		// DSCP EF (0xb8) → band 0 (highest priority)
		if out, err := run(5*time.Second, "/usr/sbin/tc", "filter", "add", "dev", iface, "parent", "1:0",
			"protocol", "ip", "prio", "1", "u32",
			"match", "ip", "tos", "0xb8", "0xfc",
			"flowid", "1:1"); err != nil {
			log.Printf("QoS: tc filter EF on %s failed: %s %v", iface, out, err)
		}

		// Voice multicast port 4370 → band 0 (catch unmarked voice too)
		if out, err := run(5*time.Second, "/usr/sbin/tc", "filter", "add", "dev", iface, "parent", "1:0",
			"protocol", "ip", "prio", "2", "u32",
			"match", "ip", "dport", "4370", "0xffff",
			"flowid", "1:1"); err != nil {
			log.Printf("QoS: tc filter port on %s failed: %s %v", iface, out, err)
		}

		// CoT multicast port 6969 → band 1 (normal)
		if out, err := run(5*time.Second, "/usr/sbin/tc", "filter", "add", "dev", iface, "parent", "1:0",
			"protocol", "ip", "prio", "3", "u32",
			"match", "ip", "dport", "6969", "0xffff",
			"flowid", "1:2"); err != nil {
			log.Printf("QoS: tc filter CoT on %s failed: %s %v", iface, out, err)
		}

		// Mesh Chat port 9800 → band 2 (bulk)
		if out, err := run(5*time.Second, "/usr/sbin/tc", "filter", "add", "dev", iface, "parent", "1:0",
			"protocol", "ip", "prio", "4", "u32",
			"match", "ip", "dport", "9800", "0xffff",
			"flowid", "1:3"); err != nil {
			log.Printf("QoS: tc filter chat on %s failed: %s %v", iface, out, err)
		}

		log.Printf("QoS: priority active on %s (voice=high, cot=normal, chat=bulk)", iface)
	}
}

// ============================================================
// Main
// ============================================================

var Version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(Version)
		return
	}

	log.SetFlags(0)
	log.SetPrefix("[mesh-manager] ")
	log.Printf("Starting mesh manager (version %s)", Version)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down")
		close(stop)
	}()

	im := newIPManager()

	// Initial IP allocation
	im.run()

	// Initial hosts + DNS + QoS
	updateHosts()
	updateMeshDNS()
	setupVoiceQoS()

	// One-shot default route fix
	go defaultRouteFix()

	// Gateway route loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		gatewayRouteLoop(stop)
	}()

	// Periodic IP check + hosts update (every 30s)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				im.run()
				updateHosts()
				updateMeshDNS()
			}
		}
	}()

	wg.Wait()
}

func meshHook(event string, args ...string) {
	cmdArgs := append([]string{event}, args...)
	exec.Command("/usr/local/bin/mesh-hook", cmdArgs...).Run()
}
