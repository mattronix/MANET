#!/bin/bash
# Live fixup for an already-provisioned node whose eud= just changed, in
# either direction (see the eud apply-block in manet-ctrl's api.go).
#
# radio-setup.sh's classification (which interface is AP vs mesh) and its
# per-interface wpa_supplicant/ap-txpower.service generation all only ever
# run once, at first-ever provisioning (radio-setup-run-once, which never
# re-fires) — nothing else updates /var/lib/mesh_if or /var/lib/ap_interface
# in response to a later eud= edit. Confirmed live: setting eud=wired and
# rebooting is NOT enough on its own — /var/lib/ap_interface still names
# the original AP radio, /var/lib/mesh_if never gained it, and journalctl
# shows radio-setup.sh never re-ran that boot (only fires again if an
# unrelated interface-rename mismatch happens to trigger radio-setup-rerun).
# So a stopped-hostapd node can still be sitting on entirely stale role
# files, not just a stale wpa_supplicant/txpower config for a radio
# mesh_if already lists.
#
# This script closes both gaps and is idempotent — safe to run any time,
# does nothing once /var/lib/mesh_if, /var/lib/ap_interface, and every
# mesh interface's wpa_supplicant config/txpower all agree with the
# current eud=.
set -u
export PATH="/usr/sbin:/sbin:/usr/bin:/bin:${PATH:-}"

MESH_IF_FILE=/var/lib/mesh_if
AP_IF_FILE=/var/lib/ap_interface
AP_TXPOWER_UNIT=/etc/systemd/system/ap-txpower.service

iface_phy() {
    local iface="$1"
    iw dev "$iface" info 2>/dev/null | awk '/wiphy/ {print "phy"$2; exit}'
}

iface_driver() {
    local iface="$1" driver
    driver="$(basename "$(readlink -f /sys/class/net/$iface/device/driver 2>/dev/null)")"
    if [[ -z "$driver" || "$driver" == "." ]]; then
        driver="$(ethtool -i "$iface" 2>/dev/null | awk -F': ' '$1 == "driver" {print $2; exit}')"
    fi
    echo "$driver"
}

phys_iface() {
    local logical="$1" phys
    phys=$(grep "^${logical}:" /var/lib/iface_map 2>/dev/null | cut -d: -f2)
    echo "${phys:-$logical}"
}

iface_mesh_freq() {
    local iface="$1" phyname
    phyname="$(iface_phy "$iface")"
    [[ -z "$phyname" ]] && return
    if iw phy "$phyname" info 2>/dev/null | grep -q "2412\.0 MHz"; then
        echo "2412"
    elif iw phy "$phyname" info 2>/dev/null | grep -q "5180\.0 MHz"; then
        echo "5180"
    fi
}

MESH_NAME=$(grep '^mesh_ssid=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
KEY=$(grep '^mesh_key=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
CFG80211_REGDOM=$(grep '^regulatory_domain=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
CFG80211_REGDOM="${CFG80211_REGDOM:-US}"
EUD=$(grep '^eud=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
AP_INTERFACE=""
[ -f "$AP_IF_FILE" ] && AP_INTERFACE=$(head -1 "$AP_IF_FILE" | tr -d '\r')
CHANGED=0

# --- 0. If eud= no longer needs an AP radio but /var/lib/ap_interface
#        still names one, that radio was never actually reclassified —
#        move it into /var/lib/mesh_if and clear ap_interface, so step 1
#        below (and every other consumer of these files) sees it as mesh.
#
#        Confirmed live: the caller (manet-ctrl's api.go) stops hostapd
#        (but does not disable it) before invoking this script on the
#        wired/none path, but this script must not depend on that
#        ordering — a stray direct invocation, a future caller, or any
#        race between the two leaves hostapd and the freshly-(re)enabled
#        wpa_supplicant@<iface> both bound to the same radio, which
#        killed a just-established mesh peering outright (`Mesh MPM:
#        failed to send peering frame`). So stop+disable hostapd here
#        ourselves whenever it's still configured for the interface
#        being reclaimed, before step 1 re-enables wpa_supplicant on it.
#        Checking is-enabled as well as is-active matters precisely
#        because the caller's own prior stop (above) leaves it inactive
#        but still enabled -- an is-active-only guard would skip the
#        disable in that exact, normal case and let hostapd come back on
#        the next reboot despite eud=wired/none.
if { [ "$EUD" = "wired" ] || [ "$EUD" = "none" ]; } && [ -n "$AP_INTERFACE" ]; then
    echo "manet-wlan-reconcile: eud=$EUD, reclassifying former AP interface $AP_INTERFACE as mesh"
    if [ "$(grep '^interface=' /etc/hostapd/hostapd.conf 2>/dev/null | cut -d= -f2-)" = "$AP_INTERFACE" ] \
        && { systemctl is-active --quiet hostapd.service || systemctl is-enabled --quiet hostapd.service; }; then
        echo "manet-wlan-reconcile: stopping hostapd (still bound to $AP_INTERFACE)"
        systemctl disable --now hostapd.service 2>/dev/null || true
    fi
    # radio-setup.sh never classifies brcmfmac as mesh-capable (see its own
    # "brcmfmac must never stay classified as a mesh radio" rescue) — it's
    # the AP radio on reference platforms precisely because it can't do
    # 802.11s. Route it back to no_mesh_if instead, matching that same
    # rule, rather than handing step 1 below an interface it'll generate a
    # doomed mode=5 wpa_supplicant config for.
    if [ "$(iface_driver "$AP_INTERFACE")" = "brcmfmac" ]; then
        echo "manet-wlan-reconcile: $AP_INTERFACE is brcmfmac, not mesh-capable — routing to no_mesh_if instead"
        if ! grep -qx "$AP_INTERFACE" /var/lib/no_mesh_if 2>/dev/null; then
            echo "$AP_INTERFACE" >> /var/lib/no_mesh_if
        fi
    elif ! grep -qx "$AP_INTERFACE" "$MESH_IF_FILE" 2>/dev/null; then
        echo "$AP_INTERFACE" >> "$MESH_IF_FILE"
    fi
    : > "$AP_IF_FILE"
    AP_INTERFACE=""
    CHANGED=1
fi

# --- 0b. The reverse: eud= now needs an AP radio but /var/lib/ap_interface
#         is empty — either this node never had one (was always wired/none)
#         or step 0 above just cleared it on a previous run. Select one
#         (mirroring radio-setup.sh's own priority: a non-mesh interface if
#         present, else the first 5GHz-capable mesh interface, else just the
#         first mesh interface), pull it out of mesh service, and regenerate
#         the three systemd units radio-setup.sh normally only writes once
#         at first provisioning (ap-interface-setup.service, hostapd.conf,
#         ap-txpower.service all bake the AP interface name in at generation
#         time, same pattern as everything else this script fixes).
if { [ "$EUD" = "wireless" ] || [ "$EUD" = "both" ] || [ "$EUD" = "auto" ]; } && [ -z "$AP_INTERFACE" ]; then
    NEW_AP=""
    NO_MESH_IF_FILE=/var/lib/no_mesh_if
    if [ -s "$NO_MESH_IF_FILE" ]; then
        NEW_AP=$(head -1 "$NO_MESH_IF_FILE")
    elif [ -s "$MESH_IF_FILE" ]; then
        for iface in $(cat "$MESH_IF_FILE"); do
            PHY="$(iface_phy "$iface")"
            if [ -n "$PHY" ] && iw phy "$PHY" info 2>/dev/null | grep -q " 5[0-9][0-9][0-9]"; then
                NEW_AP="$iface"
                break
            fi
        done
        [ -z "$NEW_AP" ] && NEW_AP=$(head -1 "$MESH_IF_FILE")
    fi

    if [ -n "$NEW_AP" ]; then
        echo "manet-wlan-reconcile: eud=$EUD, selecting $NEW_AP as the new AP interface"
        echo "$NEW_AP" > "$AP_IF_FILE"
        sed -i "/^${NEW_AP}$/d" "$MESH_IF_FILE"
        [ "$(cat /var/lib/mesh_5_if 2>/dev/null)" = "$NEW_AP" ] && : > /var/lib/mesh_5_if
        AP_INTERFACE="$NEW_AP"
        CHANGED=1

        systemctl disable --now "wpa_supplicant@$NEW_AP.service" 2>/dev/null || true
        if command -v batctl >/dev/null 2>&1 && batctl meshif bat0 if 2>/dev/null | grep -q "^${NEW_AP}:"; then
            echo "manet-wlan-reconcile: removing $NEW_AP from bat0 (now the AP, not mesh)"
            batctl meshif bat0 if del "$NEW_AP" 2>/dev/null || true
        fi

        HOST_MAC=$(ip a | grep -A1 "$(networkctl | grep -v bat | awk '/ether/ {print $2}' | head -1)" \
            | awk '/ether/ {print $2}' | cut -d':' -f 5-6 | sed 's/://g')
        LAN_AP_SSID=$(grep '^lan_ap_ssid=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
        LAN_AP_KEY=$(grep '^lan_ap_key=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
        LAN_AP_CHANNEL=$(grep '^lan_ap_channel=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
        LAN_AP_BW=$(grep '^lan_ap_bw=' /etc/mesh.conf 2>/dev/null | cut -d= -f2-)
        LAN_AP_BW="${LAN_AP_BW:-80}"

        cat <<-EOF > /etc/systemd/system/ap-interface-setup.service
	[Unit]
	Description=Set $NEW_AP to managed mode for hostapd
	Before=hostapd.service
	After=wifi-rfkill-unblock.service wpa_supplicant@${NEW_AP}.service
	Wants=wifi-rfkill-unblock.service

	[Service]
	Type=oneshot
	ExecStartPre=/usr/local/bin/unblock-wifi-rfkill.sh
	ExecStartPre=/bin/sleep 2
	ExecStartPre=-/bin/systemctl stop wpa_supplicant@${NEW_AP}.service
	ExecStart=-/usr/sbin/ip link set $NEW_AP down
	ExecStart=-/usr/sbin/iw dev $NEW_AP set type managed
	ExecStart=-/usr/sbin/ip link set $NEW_AP up
	RemainAfterExit=yes

	[Install]
	WantedBy=multi-user.target
	EOF

        cat <<-EOF > "/etc/systemd/network/30-${NEW_AP}.network"
	[Match]
	Name=$NEW_AP

	[Link]
	Unmanaged=yes
	ActivationPolicy=manual
	EOF

        # Recompute fresh for NEW_AP specifically — PHY above may be stale
        # (unset if NEW_AP came from no_mesh_if, or left over from the last
        # interface checked if NEW_AP came from the mesh_if fallback).
        PHY="$(iface_phy "$NEW_AP")"
        if [ -n "$PHY" ] && iw phy "$PHY" info 2>/dev/null | grep -q "5180\.0 MHz"; then
            AP_HW_MODE="a"
            AP_CHANNEL="${LAN_AP_CHANNEL:-36}"
            AP_80211AC="ieee80211ac=1"
        else
            AP_HW_MODE="g"
            AP_CHANNEL="${LAN_AP_CHANNEL:-11}"
            AP_80211AC=""
        fi

        vht_seg0_idx() {
            case "$1" in
                36|40|44|48) echo 42 ;;
                52|56|60|64) echo 58 ;;
                100|104|108|112) echo 106 ;;
                116|120|124|128) echo 122 ;;
                132|136|140|144) echo 138 ;;
                149|153|157|161) echo 155 ;;
                *) echo "$1" ;;
            esac
        }
        ht40_capab() {
            case "$1" in
                36|44|52|60|100|108|116|124|132|140|149|157) echo "[HT40+]" ;;
                40|48|56|64|104|112|120|128|136|144|153|161) echo "[HT40-]" ;;
                *) echo "" ;;
            esac
        }

        AP_VHT_LINES=""
        if [ "$AP_HW_MODE" = "a" ]; then
            if [ "$LAN_AP_BW" = "40" ] || [ "$LAN_AP_BW" = "80" ]; then
                _ht40_cap="$(ht40_capab "$AP_CHANNEL")"
                [ -n "$_ht40_cap" ] && AP_VHT_LINES="${AP_VHT_LINES}
ht_capab=$_ht40_cap"
            fi
            if [ "$LAN_AP_BW" = "80" ]; then
                AP_VHT_LINES="${AP_VHT_LINES}
vht_oper_chwidth=1
vht_oper_centr_freq_seg0_idx=$(vht_seg0_idx "$AP_CHANNEL")"
            else
                AP_VHT_LINES="${AP_VHT_LINES}
vht_oper_chwidth=0"
            fi
        fi

        cat <<-EOF > /etc/hostapd/hostapd.conf
	interface=$NEW_AP
	bridge=br0
	driver=nl80211
	ssid=${LAN_AP_SSID}-${HOST_MAC}
	country_code=$CFG80211_REGDOM
	ieee80211d=1

	hw_mode=$AP_HW_MODE
	channel=$AP_CHANNEL
	ieee80211n=1
	$AP_80211AC$AP_VHT_LINES
	wmm_enabled=1

	auth_algs=1
	wpa=2
	wpa_key_mgmt=WPA-PSK
	wpa_pairwise=CCMP
	rsn_pairwise=CCMP
	wpa_passphrase=$LAN_AP_KEY
	EOF

        cat <<-EOF > /etc/systemd/system/ap-txpower.service
	[Unit]
	Description=Set low TX power on AP interface
	After=ap-interface-setup.service
	Wants=ap-interface-setup.service

	[Service]
	Type=oneshot
	ExecStartPre=/bin/sleep 2
	ExecStart=-/bin/sh -c '/usr/sbin/iw phy "\$(cat /sys/class/net/$NEW_AP/phy80211/name)" set txpower fixed 500'
	RemainAfterExit=yes

	[Install]
	WantedBy=multi-user.target
	EOF

        systemctl daemon-reload
        systemctl unmask hostapd.service
        systemctl unmask dnsmasq.service
        systemctl enable ap-interface-setup.service hostapd.service ap-txpower.service dnsmasq.service
        systemctl restart ap-interface-setup.service 2>/dev/null || true
        systemctl restart hostapd.service 2>/dev/null || true
        systemctl restart dnsmasq.service 2>/dev/null || true
        systemctl restart ap-txpower.service 2>/dev/null || true
        echo "manet-wlan-reconcile: AP configuration complete for $NEW_AP"
    else
        echo "manet-wlan-reconcile: WARNING eud=$EUD needs an AP interface but none is available" >&2
    fi
fi

# --- 1. Generate a missing mesh wpa_supplicant config for any interface
#        /var/lib/mesh_if now claims but never got one written for, AND
#        make sure the service is actually enabled+running either way —
#        not just when a config was freshly generated. A radio that went
#        through an AP round-trip (step 0b stops its wpa_supplicant but
#        deliberately leaves the mesh config file in place, matching
#        radio-setup.sh's own "disable stale service, don't delete
#        config" philosophy) still has its config file sitting there on
#        the way back, which would otherwise make this loop skip it
#        entirely and leave it disabled forever.
[ -f "$MESH_IF_FILE" ] || exit 0
for WLAN in $(cat "$MESH_IF_FILE"); do
    [ -n "$AP_INTERFACE" ] && [ "$WLAN" = "$AP_INTERFACE" ] && continue

    if [ ! -e "/etc/wpa_supplicant/wpa_supplicant-$WLAN.conf" ]; then
        FREQ=$(iface_mesh_freq "$WLAN")
        if [ -z "$FREQ" ]; then
            echo "manet-wlan-reconcile: WARNING cannot determine band for $WLAN, skipping" >&2
            continue
        fi

        CHANGED=1
        echo "manet-wlan-reconcile: generating mesh config for $WLAN (${FREQ} MHz)"

        cat <<-EOF > "/etc/wpa_supplicant/wpa_supplicant-$WLAN-lobby.conf"
		ctrl_interface=/var/run/wpa_supplicant
		country=$CFG80211_REGDOM
		update_config=1
		sae_pwe=0
		ap_scan=2
		network={
		    ssid="$MESH_NAME"
		    mode=5
		    frequency=${FREQ}
		    key_mgmt=SAE
		    sae_password="$KEY"
		    ieee80211w=2
		    mesh_fwding=0
		    group_rekey=0
		}
		EOF

        cat <<-EOF > "/etc/systemd/network/30-$WLAN.network"
		[Match]
		MACAddress=$(ip a | grep -A1 "$(phys_iface "$WLAN")" | awk '/ether/ {print $2}')

		[Network]

		[Link]
		RequiredForOnline=no
		MTUBytes=1432
		EOF

        cp "/etc/wpa_supplicant/wpa_supplicant-$WLAN-lobby.conf" "/etc/wpa_supplicant/wpa_supplicant-$WLAN.conf"
    fi

    if ! systemctl is-active --quiet "wpa_supplicant@$WLAN.service"; then
        CHANGED=1
        echo "manet-wlan-reconcile: (re)enabling wpa_supplicant for $WLAN"
        systemctl enable --now "wpa_supplicant@$WLAN.service"
    fi
done

# --- 2. If ap-txpower.service is still holding a radio's txpower fixed
#        low for a radio that isn't the AP anymore, reset it and retire
#        the stale unit.
if [ -f "$AP_TXPOWER_UNIT" ]; then
    STALE_TARGET=$(grep -oP '(?<=/sys/class/net/)[a-zA-Z0-9]+(?=/phy80211/name)' "$AP_TXPOWER_UNIT" | head -1)
    if [ -n "$STALE_TARGET" ] && [ "$STALE_TARGET" != "$AP_INTERFACE" ]; then
        STALE_PHY=$(iface_phy "$STALE_TARGET")
        if [ -n "$STALE_PHY" ]; then
            echo "manet-wlan-reconcile: resetting txpower on $STALE_TARGET ($STALE_PHY), no longer the AP interface"
            iw phy "$STALE_PHY" set txpower auto
        fi
        systemctl disable --now ap-txpower.service 2>/dev/null || true
    fi
fi

# --- 3. batman-enslave.service is ALSO a first-boot-only oneshot unit
#        (radio-setup.sh's generated ExecStart=/usr/local/bin/batman-if-setup.sh
#        start, WantedBy=multi-user.target, never re-fires) — a radio can end
#        up with a real working 802.11s mesh-point peer link (steps 1-2 above)
#        and still never actually carry mesh traffic, because batman-adv was
#        never told to add it to bat0. Confirmed live: wlan1 had an
#        established mesh plink on both sides while `batctl bat0 if` still
#        only listed the two radios enslaved at first boot.
#        batman-if-setup.sh's own start() is idempotent — it re-reads
#        /var/lib/mesh_if/halow_if fresh every call and only adds interfaces
#        not already enslaved — so it's safe to re-run any time something
#        here actually changed.
if [ "$CHANGED" -eq 1 ] && [ -x /usr/local/bin/batman-if-setup.sh ]; then
    echo "manet-wlan-reconcile: re-running batman-if-setup.sh to enslave newly-configured interfaces"
    /usr/local/bin/batman-if-setup.sh start
fi

exit 0
