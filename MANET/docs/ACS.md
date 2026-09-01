# Automatic Channel Selection (ACS)

Decentralized, gossip-based channel selection for the 2.4GHz and 5GHz mesh
radios. Ported from upstream (`very-srs/MANET`, see "Where this came from"
below) and substantially reworked in this fork. This doc exists because the
same ground has been re-explained across several sessions — it's the single
place to check before touching ACS again.

Static-channel provisioning still exists as a fallback (`ensureStaticChannels`,
`node-manager/main.go`) — nodes with `acs=n` in `mesh.conf` permanently park on
the lobby frequencies (`lobbyFreq24`/`lobbyFreq5`) instead of running any of
what's described below.

## Where this came from

Ported from `very-srs/MANET` (the `upstream` git remote) starting 2026-08-19,
decided explicitly because upstream's version worked and this fork's static
channel assignment didn't adapt to RF conditions. Upstream implements it as
bash (`MANET/node_tools/node-manager-acs.sh`, on the old pre-restructure
`node_tools/` layout); this fork rewrote it as Go
(`MANET/src/node-manager/*.go`) — not a port of the bash itself, a
reimplementation of the same design.

**One important negative finding from this rewrite work:** upstream's mesh
network config (`node_tools/radio-setup.sh`) is **byte-for-byte identical**
to this fork's for the dual-band mesh interfaces — just `frequency=${FREQ}`,
nothing about channel width anywhere. Upstream's ACS script has zero
mentions of width/VHT either. So the channel-width gap described below
(the current open bug) is **not a regression introduced by the port** — it's
a pre-existing gap in the original design that neither codebase ever solved.
Worth knowing before assuming "we broke something that used to work."

## Architecture

Every node runs the same deterministic computation independently — there is
no coordinator, no leader election for this purpose. `runACSTick`
(`node-manager/main.go:269`) is the top-level orchestration, called every 15s
from the main loop but internally gated to only actually do ACS work once
per `acsCycleInterval` (180s, matching upstream's scan cadence) via
`lastACSCycle`.

Each tick does, in order:

1. **Scan** (`scan.go`) — `performScan` surveys each mesh radio's candidate
   channels (`band24Channels = [2437, 2462]`, `band5Channels = [5200, 5220,
   5240, 5745, 5765, 5785, 5805, 5825]`) for noise floor and BSS count.
   Lobby frequencies (2412/5180) are deliberately excluded from the
   candidate list — if an election ever landed on the lobby pair, every
   node would flip into lobby state and elections would silently stop.
2. **Publish** (`writeChannelReport`) — the scan result is written to
   `/var/run/mesh_channel_report.json`, which `mesh-registry` picks up and
   gossips to peers via alfred as the `CHANNEL_REPORT_JSON` registry field.
3. **Aggregate** (`channel_election.go`) — `collectFreshReports` merges self
   + any peer reports newer than `reportStaleAfter` (240s) from the
   registry.
4. **Elect** (`electBand`, one call per band) — see "The election algorithm"
   below.
5. **Quorum check** (`quorum.go`) — is this node meaningfully connected to
   the mesh, or should it retreat to the lobby to try to find it again?
6. **Limp mode reconcile** (`limpmode.go`) — mesh-wide consensus on whether
   RF conditions are bad enough to throttle to legacy bitrates.
7. **Tourguide** (`tourguide.go`) — if elected and quorum holds, this node's
   turn (if it's the elected tourguide) to hop to the lobby and check for a
   foreign partition to merge with.

### The election algorithm (`electBand`, `channel_election.go:177`)

For each candidate channel with a fresh aggregated report:

- **Disqualify** if any reporting node (self or peer) saw noise worse than
  `noiseDisqualifyDBM` (-70dBm).
- **Score** survivors: `rawScore = avgNoise + totalBSS*0.1` (lower is
  better — quieter and less contended wins).
- **Vote** (`peerChannelVotes`) — for each *other* active, fresh (within
  `staleNodeThreshold`) peer in the registry, read its self-reported
  `DATA_CHANNEL_2_4`/`DATA_CHANNEL_5_0` field and count it as a vote for
  that channel. Self is excluded from its own vote count.
- **Rank**: if any votes exist at all (`totalVotes > 0`), sort by vote
  count first, score as tiebreak. If literally nobody has voted yet
  (cold start), sort by score alone, with a small `incumbentBiasDB` (4.0)
  nudge toward whatever channel is already current — just enough to damp
  scan-to-scan noise jitter (~2-4dB observed), deliberately far too small
  to ever outweigh a real peer vote once one exists.
- If the winning channel's score is still worse than
  `limpModeScoreThreshold` (-60.0), give up on the band entirely and fall
  back to the lobby frequency, flagging limp mode.

This vote-first design is deliberate and was itself a fix
(`fix/acs-channel-election-convergence`, see "Incident history" below): an
earlier version's 10dB incumbent bias was swamping the ~1.4dB real
noise-score gaps between candidate channels, which meant nodes independently
"agreed" on paper but never actually converged onto the same channel in
practice.

**Where the vote data actually comes from — important, and non-obvious:**
`DATA_CHANNEL_2_4`/`DATA_CHANNEL_5_0` are NOT the node's *elected/intended*
channel — they're written by `mesh-registry`'s `getChannel()`
(`src/mesh-registry/main.go:635`), which runs `iw dev` and reports whatever
channel is **actually live** on an interface in that frequency's band range.
So the vote mechanism is fundamentally about **live-state consensus**, not
intent-consensus — a node effectively "votes" with whatever it's really
running, not what it last decided to run. This matters a lot for the open
bug described below.

### Quorum (`quorum.go`)

`quorumOK` mirrors upstream's three-scenario `quorum-checker.sh`, comparing
batman-adv's data-plane view (`uniqueBatmanOriginators` — who can I actually
reach) against alfred's gossip view (`activeAlfredCount` — who does the mesh
believe exists). The gap between the two distinguishes genuine isolation
from being a small-but-coherent partition:

- **Solo isolation** (0 batman originators, but alfred remembers >2 other
  nodes) → not OK, retreat to lobby.
- **Small functional island** (≥2 originators, but <1/3 of alfred's active
  count) → OK, keep operating independently.
- **Barely connected** (below half of alfred's active count but ≥2
  originators) → degraded-but-real, OK; below that, not OK.

### Limp mode (`limpmode.go`)

Separate from a single node's own RF picture — `limpConsensusRatio`
requires ≥50% of active registry nodes to be self-reporting
`IS_IN_LIMP_MODE=true` before the mesh-wide bitrate throttle
(`reconcileLimpMode`, forces legacy/robust bitrates on both mesh radios)
actually engages. Entry is immediate once consensus crosses the threshold;
exit requires the consensus to have held below threshold for
`limpModeMinDuration` (300s) — asymmetric on purpose, so the mesh doesn't
flap in and out of throttled mode.

### Tourguide (`tourguide.go`)

Partition-healing. `electTourguide` picks one node (deterministic scoring
over the registry) to periodically hop to the lobby frequency and listen
for a foreign partition — a separate group of nodes running ACS
independently that never found this mesh. `analyzeForeignPartitions` +
`applyPartitionMerge` handle actually merging into (or pulling in) the
larger partition if one is found. Only the elected tourguide does this; no
one else's radios get disturbed.

## Incident history

Chronological, each with what broke and what fixed it.

**2026-08-19/20 — initial port + field test.** `feat/acs-port` implemented
and live-tested on 2 nodes: independent channel election converged, WiFi
mesh link re-established automatically. Confirmed working at small scale.

**2026-08-21 — convergence bug found and fixed
(`fix/acs-channel-election-convergence`).** The original 10dB incumbent
bias swamped real noise-score gaps (~1.4dB), blocking convergence — nodes
would each individually "elect" correctly but never actually agree in
practice. Fixed with the vote-first design described above (peer consensus
overrides incumbent bias entirely once any peer has voted).

**2026-08-21 — hostapd/wpa_supplicant race on `eud=` transitions.** Not
ACS's election logic itself, but adjacent: the wired→wireless→wired
round-trip briefly re-peered then dropped 44s later because hostapd was
still bound to the interface wpa_supplicant was trying to join the mesh on.
Fixed in `manet-wlan-reconcile.sh` by having the wired-direction step stop+
disable hostapd itself rather than trusting the caller had already done so.
Re-verified live via the real API, held stable well past the old failure
window.

**2026-08-21 — batman-adv failover/recovery timing measured.** ~4-5 seconds
in both directions (BATMAN_V, OGM-interval-driven), not itself a bug, but
useful baseline data if a future regression needs a "how fast should this
normally be" reference.

**2026-08-25 — alfred silently down breaks ACS convergence entirely
(EUD3).** Not an ACS code bug, but ACS depends completely on the registry
gossip that alfred carries. With alfred `inactive` (cleanly stopped, not
crashed — see `alfred_recurring_clean_stop_bug` memory), EUD3's own
`[acs]` elections logged `votes=0` on every cycle since boot, on both
bands — it kept re-deriving its own locally-scored channel choice with zero
peer input, so it never converged onto what EUD4 had already independently
elected. `systemctl restart alfred` fixed it within a couple of 180s ACS
cycles. **This is the third time alfred has been found silently down on a
live node** (see the alfred memory for the other two) — root cause of why
it stops is still unknown, separately tracked.

**2026-08-25 — 5GHz primary-channel mismatch between EUD3 and EUD4 (OPEN,
see below).** Found while investigating why the two nodes weren't using
their fast 5GHz link after a baseline redeploy + reboot cycle.

## Open issue: 5GHz primary channel doesn't reliably match between nodes

**2026-08-27 update — fix found, built, and hardware-confirmed, see
[`wpa-supplicant-mesh-noscan.md`](wpa-supplicant-mesh-noscan.md).** The
"Fix design" and gate-run sections below (2026-08-26) concluded no
config-level fix exists in mainline wpa_supplicant and this section's
"Decision" chose 20MHz-only mesh instead. That conclusion has been
superseded: OpenWrt maintains a small, already-written patch pair
(`300-noscan.patch` + `301-mesh-noscan.patch`) that adds a real `noscan`
`wpa_ssid` field and wires it into the exact mesh coex-scan function
responsible for this bug (`ibss_mesh_setup_freq()` in
`wpa_supplicant/wpa_supplicant.c` — not `hostapd`'s
`ieee80211n_check_40mhz()` as originally traced below, a related but
separate function this fork's own mesh path never reaches). A patched
arm64 binary was built and tested live on EUD3/EUD4 the same day: 20/20
independent restarts (VHT80 and HT40-only, 5 each × 2 nodes) landed on
the identical channel every time, zero deviation, for both widths — the
symptom below is fixed. Read the linked doc before repeating any of the
"no fix exists" research below — it's still accurate as a historical
record of why 20MHz-only was chosen at the time, but is no longer the
last word on whether a fix is possible. Nothing has shipped to the fleet
yet — that test was fully reverted, and `mesh_5ghz_bw` stays `20`/`80`-only
until a deployment decision (packaging, which width to default to) is
made.

**Symptom:** EUD3 and EUD4 both correctly elect the same channel (both
logged `elected channel 5220`/channel 44, matching votes), but EUD3's
*actual live radio* lands on channel 48 instead — same 80MHz spectrum block
(`center1: 5210MHz` on both), different primary 20MHz sub-channel. Reproduced
twice via clean `wpa_supplicant@wlan1` restarts with completely unchanged
config, so it's deterministic given the environment, not a random race.

**Root cause, traced via wpa_supplicant journal + live `iw dev wlan1 info`
on both nodes:** wpa_supplicant logs `Switch own primary and secondary
channel to get secondary channel with no Beacons from other BSSes` on
restart — its own environment-scan-based primary-channel reselection is
overriding the requested `frequency=5220`. `/etc/wpa_supplicant/
wpa_supplicant-wlan1.conf` is byte-identical on both nodes (just
`frequency=5220`, nothing else) — this isn't a config divergence between
the two nodes, it's non-deterministic driver/wpa_supplicant behavior given
an underspecified config. **Consequence:** EUD3 still detects EUD4
(`new peer notification` for EUD4's wlan1 MAC — plausible since they share
the same 80MHz occupied spectrum) but the SAE handshake then fails
(`MESH-SAE-AUTH-FAILURE`), most likely because EUD3's unicast auth frames go
out on its own (wrong) primary and never land on EUD4's receiver. Only the
2.4GHz (~73-87 Mbit/s) and HaLow (~7 Mbit/s) links carry traffic between
them as a result — down from the ~340 Mbit/s the 5GHz VHT80 link hit
earlier the same day, before this regression appeared. It does **not**
self-heal: `electBand` only re-triggers `setIfaceFrequency` when its own
elected value *changes* — once the conf file already says `frequency=5220`,
ACS considers the job done and never notices the live radio has silently
diverged, no matter how many more 180s cycles pass.

**Why nobody set channel width deliberately (see "Where this came from"
above):** it never has been, in this fork or upstream. Nothing in
`radio-setup.sh`'s mesh network block, `node-manager`'s runtime rewrite, or
any `mesh.conf` key controls mesh-radio channel width — only the AP
interface (`lan_ap_bw`) and HaLow (`halow_bw`) have that lever.
80MHz is simply whatever the mt7915e driver defaults to when nothing
constrains it.

**Why "just pin a static channel" is not what's being proposed:** channel
*width* (how many 20MHz channels get bonded together) and channel
*election* (which candidate channel wins, via the peer-consensus voting
above) are orthogonal. Pinning width doesn't touch election — ACS keeps
freely electing among candidates exactly as it does today, each one is
just narrower. This is not a revert of the convergence-fix work.

### Fix design — validated by manet-architect 2026-08-26

The first pass at this (below, "possible solutions") named `max_oper_chwidth`
as the primary lever. **That was wrong**, caught during design validation
before any code was written — recorded here rather than silently corrected,
since the reasoning matters for whoever implements this.

**1. Deterministic width pinning — corrected lever.** The
`Switch own primary and secondary channel...` log line comes from hostapd's
`ieee80211n_switch_pri_sec()` (`src/ap/hw_features.c` upstream), reached via
the **HT40 20/40 co-existence scan**, which wpa_supplicant's mesh mode pulls
in through the same hostapd AP code `mesh.c` links against. That path is
gated on `iface->conf->noscan`, **not** on VHT width — so `max_oper_chwidth`
alone constrains the VHT field but doesn't stop the scan that's actually
doing the swap. The real fix, in priority order:
- `noscan=1` — stops the coex scan (the actual fix)
- `ht40=1` — explicit secondary-channel offset instead of scan-derived
- `max_oper_chwidth=1` — keeps VHT80 explicit, makes the seg0 computation
  deterministic once the scan is out of the way (still worth keeping)

**Do not use `disable_ht40`/`disable_vht`** — those "fix" the mismatch by
giving up the 80MHz link entirely (trading ~340 Mbit/s for roughly half or
less), which is a regression, not a fix. `fixed_freq` is IBSS-oriented and
doesn't stop the coex scan either.

**Mandatory gate before writing any code:** confirm `noscan` is actually
present in the deployed wpa_supplicant binary (`strings $(command -v
wpa_supplicant) | grep -x -E 'noscan|ht40|max_oper_chwidth'`) — the
original `strings` pass that found `max_oper_chwidth`/`disable_ht40`/etc.
was searching for hostapd-style keys and never checked for `noscan`
specifically. If it's not there, this whole approach needs to be
reconsidered before touching EUD3/EUD4 again.

**2026-08-26 — gate run, FAILED. Implementation did not proceed.** Ran the
gate command on EUD4 (`192.168.1.183`, the only directly-reachable node)
before writing any code:

```
strings $(command -v wpa_supplicant) | grep -x -E 'noscan|ht40|max_oper_chwidth'
# => max_oper_chwidth
```

Only `max_oper_chwidth` matched. Follow-up (non-anchored) greps to rule out
a quoting fluke in the gate command itself:

- `strings ... | grep -i noscan` → **zero matches**, not even as a
  substring anywhere in the binary.
- `strings ... | grep -i ht40` → matches exist, but every one is
  `p2p_go_ht40`, `disable_ht40`, `ht40_intolerant`, or hostapd/ACS log
  strings (`HT40: control channel...`, `nl80211: ACS Params: ... HT40:
  %d ...`) — there is no plain `ht40=%d`/standalone `ht40` network-block
  config key exposed anywhere. The one bare `" ht40"` string found is part
  of a longer flag name, not an assignable key.
- `strings ... | grep -i chwidth` → only `max_oper_chwidth` and
  `vht_oper_chwidth`, both VHT-width fields, neither one a scan-suppression
  lever.

Binary identity: `/usr/sbin/wpa_supplicant`, `wpa_supplicant v2.10`
(2003-2022 build), confirmed present via `command -v` (not a stale PATH
hit).

**Conclusion: the core premise of the "Fix design — validated by
manet-architect 2026-08-26" section above does not hold on this fleet's
actual wpa_supplicant build.** `noscan` is not a recognized config key in
this binary at all — it isn't that the coex-scan-suppression *behavior* is
absent, the *config key* itself was never compiled in. `ht40=1` is
similarly not available as a plain mesh/AP network-block key on this
build. Only `max_oper_chwidth` — the lever the design doc already
identified as *not* actually stopping the coex scan — is present.
Implementation (radio-setup.sh/manet-wlan-reconcile.sh template changes,
`ensureMeshConfDefaults`, `setIfaceFrequency` verify-after-apply,
`waitForFrequency` fix, `scanIface` EU-domain fix) was **not started** as a
result — per this doc's own gate instructions, those all depend on
`noscan` actually working, and none of them were written.

**What needs to happen next, before this can be re-attempted:**
- Determine why `noscan` isn't compiled in — check the OpenWrt/buildroot
  `.config` used to build this `wpa_supplicant` (likely
  `CONFIG_NO_SCAN_PROCESSING` or a stripped mesh/AP feature set) against
  what upstream wpa_supplicant 2.10 actually supports for `noscan` in a
  `mode=5` (mesh) `network={}` block — this repo's binary may simply have
  had that support built out, or `noscan` may never have applied to mesh
  mode in 2.10 regardless of build config and the design's premise (that
  it's gated purely on `iface->conf->noscan` reachable from `mesh.c`) needs
  re-verification against the actual 2.10 source, not just the changelog
  reasoning that produced this design.
- If `noscan` truly isn't available on this build, the width-pinning
  approach needs a different lever than the one in "Fix design" above —
  this has NOT been researched yet, don't assume `max_oper_chwidth` alone
  is good enough just because it's what's left; the design doc already
  explains why that alone doesn't stop the coex scan.
- Rebuilding/patching wpa_supplicant with `noscan` support (if feasible
  given this fleet's toolchain) is one option but hasn't been scoped —
  would need to confirm this doesn't regress `MANET/binaries_arm64/`'s
  other consumers of the same prebuilt binary.
- The parts of this design that are independent of `noscan`'s availability
  (the EU-domain `scanIface` fake-noise-floor fix, `waitForFrequency`'s
  string-match bug) are still valid problems on their own, just not the
  reason this task was opened — worth doing separately if wanted, but they
  don't fix the primary/secondary channel mismatch by themselves.

**2026-08-26, later same day — root cause fully traced against real
wpa_supplicant/hostap source, question above answered.** Downloaded and
grepped the actual source directly (`src/ap/hw_features.c`,
`wpa_supplicant/mesh.c`, `wpa_supplicant/config.c`,
`wpa_supplicant/config_ssid.h` — read myself, not summarized) rather than
continuing to guess. Conclusive findings:

- The scan-and-switch behavior lives in `ieee80211n_check_40mhz()`
  (`src/ap/hw_features.c`): `if (!iface->conf->secondary_channel ||
  iface->conf->no_pri_sec_switch || iface->conf->noscan) return 0;` — so
  `noscan` genuinely is a real gate, exactly as originally claimed. But
  it's a field on `struct hostapd_config` (`iface->conf`) — hostapd's
  **AP-side** config struct — not a field on `struct wpa_ssid`, which is
  what a wpa_supplicant.conf `network={}` block actually populates. This
  is the exact same category of mistake as the original `vht_oper_chwidth`
  confusion, one layer deeper: a real, correctly-identified C struct
  field that is simply not reachable from wpa_supplicant.conf text at all.
- Confirmed via `config.c`'s `ssid_fields[]` table (the actual key-to-field
  parser mapping) that neither `noscan` nor a bare `ht40` exist as
  parseable network-block keys anywhere in wpa_supplicant, in any
  version — not stripped from this build, never existed as text-config
  options in the first place. (`disable_ht40`, `disable_vht`,
  `max_oper_chwidth` are real and do exist — confirmed both in source and
  via `strings` on the live binary — they just don't touch this code
  path.)
- Confirmed via direct grep of `mesh.c`'s full source: **it never sets
  `conf->noscan` or `conf->no_pri_sec_switch` from any `wpa_ssid` field,
  anywhere.** `mesh.c` builds its own internal `struct hostapd_config` via
  `hostapd_config_defaults()` for each mesh interface (there's no real
  hostapd.conf file involved for mesh at all), and both fields are left at
  their zero-initialized default — meaning the scan-based primary/
  secondary reselection is unconditionally active for every 40MHz+-wide
  wpa_supplicant mesh interface, with **no way to configure it off via
  wpa_supplicant.conf, in any version.** This is a genuine, unconditional
  gap in wpa_supplicant's own mesh implementation, not a fleet-specific
  build issue and not something more `strings`-searching would ever have
  found.

**What this means for the fix:** every variant of "add a config key"
(`noscan`, `ht40`, `max_oper_chwidth`, or any combination) was chasing a
lever that structurally cannot exist for mesh mode in mainline
wpa_supplicant. Three real options remain, none yet attempted:

1. **Patch wpa_supplicant/mesh.c itself** to set `conf->noscan = 1`
   unconditionally (or wire a new toggle through from `ssid`), and build/
   vendor a custom binary for this fleet — the same category of solution
   that HaLow already required (`MANET/binaries_arm64/wpa_supplicant_s1g`
   exists specifically because mainline wpa_supplicant has zero 802.11ah
   support at all; this would be the same pattern applied to a smaller,
   more contained gap in mesh mode). Real work: a toolchain to build
   arm64 wpa_supplicant, a patch, and ongoing maintenance of a fork.
2. **Work around it post-hoc, outside wpa_supplicant entirely** — after
   `wpa_supplicant@wlan1` starts and joins the mesh, issue a direct `iw
   dev wlan1 set channel <N> <width>` (or equivalent netlink call) from
   `node-manager` to force the actual radio channel, overriding whatever
   primary wpa_supplicant's internal scan picked. Doesn't fix
   wpa_supplicant's own internal bookkeeping/beacon IEs, so needs
   verification that this doesn't create a mismatch between what
   wpa_supplicant *believes* it's running and what the radio actually
   does — untested, but far less work than (1) and fits naturally as an
   extension of the verify-after-apply piece of this design (same
   `setIfaceFrequency` call site could issue the correction directly
   instead of just detecting the mismatch).
3. **Accept 20MHz-only mesh links** via `disable_ht40=1` (the one lever
   that's both real and confirmed present in the deployed binary) — this
   is the option explicitly rejected earlier in this doc for trading away
   the ~340 Mbit/s VHT80 link. Re-surfaced here as the only *zero-new-code*
   option now that (1) and (2) are both known to be real engineering
   effort — worth the user explicitly weighing the tradeoff now that the
   alternatives' actual cost is known, rather than assuming a config-only
   fix was always going to be available.

**2026-08-26, later still — option 3 live-benchmarked on EUD3/EUD4.**
`disable_ht40=1` alone **broke mesh join entirely** on both nodes
(`wlan1: mesh join error=-1`) — VHT80 is built from HT40 segments, so
disabling HT40 while VHT capability stays enabled is an inconsistent
state the driver rejects outright, not a graceful step-down. Adding
`disable_vht=1` alongside it fixed this: both nodes then joined cleanly
(`mesh plink established` within ~2s) and landed on **identical**
`channel 44, width: 20 MHz` — deterministic, unlike the 80MHz case. Real
iperf3 numbers, same link, same session, for a fair comparison:

| Width | Throughput (TCP, 10s) | Retransmits |
|---|---|---|
| 80MHz (VHT80, current default) | 505 Mbit/s | 24 |
| 20MHz (`disable_ht40=1` + `disable_vht=1`) | 144 Mbit/s (142 receiver-side) | 0 |

~28-29% of the 80MHz throughput (close to the naive 1/4-bandwidth
expectation) but zero retransmits over the full window vs. 24 at 80MHz —
some support for the narrower-channel-is-more-reliable theory, though
this is one 10s sample on one link, not a proper characterization. Both
nodes were restored to their original (unpatched, VHT80-default) config
immediately after this test — this was a benchmark to inform the decision
below, not a deployed fix.

**Correction to the option-3 lever named above:** `disable_ht40=1` alone
is **not sufficient and breaks the mesh** — it requires `disable_vht=1`
as well. Both are real, confirmed-present, unconditional config keys (not
gated behind a build flag either — `CONFIG_HT_OVERRIDES`/
`CONFIG_VHT_OVERRIDES` are both compiled into this fleet's binary, per the
successful test above).

None of these have been implemented or tested **as a deployed fix** (the
above was a live benchmark only, reverted immediately). This is a genuine
fork in the road, not a continuation of the previous "just add the right
key" approach — needs a decision on which direction before any more
implementation work.

## Decision: 20MHz-only 5GHz mesh (chosen 2026-08-26)

Option 3 chosen over options 1 (custom wpa_supplicant fork) and 2
(post-hoc `iw` correction). Reasoning, from the user directly: real
deployments are geographically spread out, and 5GHz is the shortest-range
of the three radios (basic RF: higher frequency = more path loss) — so
it's the first link to drop as node separation grows, not the backbone.
2.4GHz has been stable throughout this session's testing with no
reproduction of the primary-channel bug. HaLow (sub-1GHz) is the
long-range fallback by design — this repo's own
[`halow-range-calc.md`](halow-range-calc.md) documents km-scale HaLow
range vs. WiFi's much shorter reach, and explicitly states the general
principle "wider channels increase throughput but reduce range... each
3dB sensitivity loss halves the power margin, cutting range by ~30%" —
the same physics applies to 5GHz, so going 20MHz-only likely *improves*
5GHz's usable range margin, not just fixes the determinism bug. Any two
nodes close enough to use 5GHz at all will route their traffic over it in
preference to HaLow (batman-adv picks the highest-throughput path,
observed directly in this session's own `batctl o` output), which
offloads HaLow — the shared, most range-constrained resource across a
spread-out mesh. Net: reliability and a probable range improvement, in
exchange for a throughput ceiling drop on a link that's already the
least critical/most marginal one in a realistic deployment. Chosen over
options 1/2 specifically because it's a genuine root-cause fix (the
buggy code path structurally cannot run once VHT/HT40 are both off) with
no new runtime state machine, no custom binary to build/maintain, and no
standing correction loop — see the live-benchmark section above for why
options 1 and 2 both carry real ongoing cost that this doesn't.

### Implementation plan

**Scope: 5GHz mesh interface (wlan1-class) only.** Not 2.4GHz (already
stable, doesn't negotiate VHT, no change needed — same template loop,
different band, leave its block untouched). Not the AP interface
(client-facing `hostapd`, already correctly configured with `vht_oper_
chwidth`/`vht_oper_centr_freq_seg0_idx` for its own VHT80 — completely
separate config path from the mesh interfaces, untouched by this). Not
HaLow (separate `-s1g` config path entirely, own driver, own binary).

**Config keys**: `disable_ht40=1` **and** `disable_vht=1` together — the
live benchmark showed `disable_ht40` alone breaks mesh join outright
(`error=-1`), not a graceful step-down; both are required.

**Files to change:** (see "Implementation — 2026-08-26, fleet-wide toggle"
below for what actually shipped — a `mesh_5ghz_bw` mesh.conf toggle rather
than the hard-coded switch sketched here, and a two-way reconciler
instead of item 3's one-way `ensureMeshConfDefaults`.)
1. `MANET/rootfs/usr/local/bin/radio-setup.sh` — the mesh network block
   heredoc (~line 1012-1028, inside the `for WLAN in $(cat
   /var/lib/mesh_if)` loop). `FREQ` is already computed per-interface via
   `iface_mesh_freq "$WLAN"` *before* this heredoc, in plain MHz — add
   `if [[ "$FREQ" -ge 5000 ]]; then` around the two new lines so only the
   5GHz interface gets them, 2.4GHz's block is untouched by the same loop
   iteration.
2. `MANET/rootfs/usr/local/bin/manet-wlan-reconcile.sh` — the mirrored
   mesh lobby template heredoc (~line 319-335), same conditional, kept
   byte-consistent with radio-setup.sh's block (established convention
   from the earlier design work).
3. `MANET/src/node-manager/main.go` — new idempotent
   `ensureMeshConfDefaults(iface)` (same shape/reasoning as the earlier
   design's migration function, but simpler — no rate-limiting or retry
   state needed here, since a mismatch now fails loudly at join time
   rather than silently diverging): inserts the two keys into the
   `network={}` block of **both** the live `wpa_supplicant-<iface>.conf`
   **and** the `-lobby.conf` if missing, gated to 5GHz interfaces only.
   Required for the same reason as before: `mesh-boot-lobby.service`
   re-copies the lobby conf over the live conf on every boot, and
   `manet-wlan-reconcile.sh` only regenerates the lobby template when the
   conf is *missing* — so the shell-template changes alone reach new
   provisions only, not EUD1-4's existing lobby confs. This Go-side
   migration is the only path that's both idempotent and cleanly
   redeployable via a normal software update.

**What's different from the old design, now unnecessary:** no
verify-after-apply/rate-limiting logic in `setIfaceFrequency` — that
existed to catch *silent* divergence between elected and applied channel,
which was specifically the `noscan`-approach's failure mode. This
approach either joins cleanly at the deterministic target channel or
fails loudly (`mesh join error=-1`, visible in the journal, no silent
wrong-channel state possible). `waitForFrequency`'s existing string-match
bug and the EU-regulatory `scan.go` fake-noise-floor issue are still real
and still worth fixing, but are unrelated to this specific change —
optional to bundle, not required by it.

**New failure mode to watch for, not present in the old design:** a
node that fails to join the 20MHz-only mesh for an unrelated local reason
(driver quirk, RF issue) has **no fallback and no silent partial state**
— it's fully isolated on 5GHz until whatever's wrong with that node
specifically is fixed. This is arguably easier to notice (loud journal
error, node genuinely absent from `batctl o`'s wlan1 routes) than the old
bug's silent wrong-channel state, but there's no existing alert for
"wpa_supplicant is running but never joined its mesh group" — worth a
cheap health check (e.g. surfaced in the same `mesh status`/web UI radio
info view) rather than assuming journal-log visibility is enough in
practice.

### Testing checklist for this specific plan

- [ ] EUD3 + EUD4: clean join, identical channel both sides, confirmed
      across **multiple independent restarts** (not just the one live
      benchmark already done).
- [ ] **Cold reboot**, not just service restart — the determinism claim
      depends on the lobby-conf migration actually landing; a boot-time
      test is the only way to catch `mesh-boot-lobby.service` silently
      reverting it, same lesson as the earlier design.
- [ ] Migration test on nodes with pre-existing `-lobby.conf` files (the
      current fleet) via a normal software update, no re-provision.
- [ ] Longer-duration stability run (10+ minutes, several iperf3 samples,
      not the single 10s sample from tonight's benchmark) to build real
      confidence in the 144 Mbit/s number and its consistency.
- [ ] Confirm 2.4GHz and the AP-facing radio are genuinely untouched —
      diff their configs before/after, don't just assume the conditional
      is correct.
- [ ] EUD1/EUD2 (HaLow-only, no 5GHz mesh radio) — confirm the migration
      is a clean no-op for them (`meshIfaces()` should already exclude
      them from having a 5GHz interface to touch at all).
- [ ] Range/reliability claim — if practical, a real distance/obstruction
      test comparing 20MHz vs. 80MHz link margin at the edge of range,
      not just the close-range throughput number from tonight.

### Mixed-width peering — tested live, per-node toggle is not a clean option

User asked whether the old (80MHz, can mismatch) and new (20MHz-only,
deterministic) behavior could coexist as a per-node setting rather than a
fleet-wide switch. Tested directly: EUD3 set to
`disable_ht40=1`+`disable_vht=1`, EUD4 left at default 80MHz, both
restarted.

**They do peer** — `mesh plink established` on both sides, no
capability-mismatch rejection. But each side kept its *own* configured
width independently rather than negotiating down to a common one: EUD3
showed `width: 20 MHz, center1: 5220 MHz`, EUD4 simultaneously showed
`width: 80 MHz, center1: 5210 MHz` — same peering, two different
self-reported widths. They shared channel 44 as primary only because it
happens to fall inside both configurations (EUD4's 80MHz block spans
36-48, EUD3's 20MHz-only channel is 44 itself).

**Real throughput, same link, immediately after peering:** 100 Mbit/s
(98.6 Mbit/s receiver-side), zero retransmits — clean and stable, but
**worse than both-sides-20MHz (144 Mbit/s)**, despite one side running at
80MHz. The narrower side becomes the pairwise bottleneck as expected, but
the mismatch itself appears to cost something extra on top of just "as
slow as the narrower side" — not confirmed why (frame-format overhead
from the wider side, protection/RTS-CTS behavior under a capability
mismatch, and batman's own throughput estimator briefly reporting an even
lower 14.8 Mbit/s before the real number settled are all plausible
partial explanations, none confirmed).

**Conclusion: a per-node toggle is not a clean option.** It technically
works (peers, doesn't break), but: (1) it underperforms the uniform
20MHz configuration on the very link it's meant to protect, so there's no
throughput upside to justify the mixed state; (2) critically, **any node
left at 80MHz is still fully exposed to the original bug** for every one
of *its* other 80MHz peers — a per-node toggle doesn't shrink the bug's
blast radius, it just adds a third, worse-performing link type on top of
the two already measured. If this direction is pursued at all, a
fleet-wide switch (all nodes same mode, chosen deliberately, never mixed
in normal operation) is the only version of "keep both as a setting"
that's actually well-supported by what's been tested.

**2. The fix won't reach already-provisioned nodes as designed — new
finding, not in the original solutions list.** `radio-setup.sh`'s
`mesh-boot-lobby.service` copies `wpa_supplicant-wlanX-lobby.conf` over
`wpa_supplicant-wlanX.conf` on **every boot**. Any key added only to the
live `.conf` (e.g. via `rewriteFrequencyLine`) is erased on the next
reboot — this is *why* "does the fix survive a cold boot" needed to be a
separate test item, not an incidental extra check. Worse:
`manet-wlan-reconcile.sh` only regenerates the lobby template when `.conf`
is *missing*, so a software update alone does not rewrite existing
`-lobby.conf` files on the current fleet — EUD1-4 already have theirs, and
updating only the shell templates fixes new provisions, not these four
nodes. Generated units like `mesh-boot-lobby.service` are also confirmed
not tarball-redeployable (see `upstream_sync_2026-08-21_pr12` memory).
**Required:** an idempotent `ensureMeshConfDefaults(iface)` in
`node-manager` (Go, cleanly redeployable) that inserts the new keys into
the `network={}` block of *both* the live conf and the lobby conf if
they're missing, run once per loop tick per mesh iface. Cheap, no-op after
the first pass on any given node.

**3. Verify-after-apply — bounded, not a blind retry loop.** Put the check
inside `setIfaceFrequency` (`node-manager/main.go:210`), replacing the
blind `time.Sleep(5*time.Second)` with a poll — reuse `waitForFrequency`
(`tourguide.go:232`) rather than writing a second poller, but fix a real
bug in it first: it does `strings.Contains(out, targetFreq+" MHz")` against
full `iw dev info` output, which also contains lines like `width: 80 MHz`
and `center1: 5210 MHz` — parse the `channel N (FFFF MHz)` line specifically
instead, or a future candidate value could collide and produce a false
pass. Two hard constraints, both required:
- **No retry inside `setIfaceFrequency` itself.** It's called synchronously
  from `runACSTick` for both bands and from `applyPartitionMerge` inside
  tourguide's dwell window — a multi-attempt retry there can overrun the
  15s tick and the tourguide dwell timer.
- **Rate-limit the self-heal.** `setIfaceFrequency` currently returns early
  whenever the conf already matches the target — fixing that means
  deciding based on *live* radio state instead, but `ensureStaticIfaceChannel`
  calls this every 15s. A node that can never reach its requested frequency
  (e.g. the EU-regulatory case below) would otherwise restart
  `wpa_supplicant` every 15 seconds forever — strictly worse than today's
  bug. Cap it at one corrective restart per `acsCycleInterval` (180s) per
  interface, and stop + log once after N consecutive failures on the same
  target rather than looping forever.

Convention to follow: **verify and report, don't retry** — matching
`setIfaceTxPower`'s existing poll-then-report shape
(`manet-ctrl/collect.go:1176-1194`, 6×250ms poll, returns requested *and*
actual, no retry), the closest existing precedent in this codebase, even
though that function is itself separately documented as unreliable
(`tx_power_confirmation_unreliable` memory) — the *pattern* is right, that
instance's bug is a different, narrower issue (polling window too short).
On mismatch: log under the existing `[acs] ` sub-prefix, write one marker
via `writeStateFile` under `/var/run/`, matching the existing
`mesh_limp_mode`/`tourguide_state` convention — no gossip schema change,
no retry state machine.

**Tourguide needs a narrower related fix.** `hopFrequency` automatically
inherits the new keys for free (same conf file, no change needed there),
but its two failure modes aren't handled today: a failed hop *to* the
lobby produces a false "no foreign partition found" with no log line, and
a failed hop *back* to the data channel leaves the node stranded off its
own mesh's channel until the next 180s cycle. The return-hop case is worth
a bounded escalation to the full `setIfaceFrequency` restart path on
failure — the outbound lobby-hop case is lower stakes (just a wasted scan
attempt) and can be a log-only fix.

**New risk found during validation, not previously documented: a
regulatory-domain interaction that would turn verify-after-apply into a
restart-loop generator on EU nodes.** `scan.go`'s `band5Channels` hardcodes
`{5200, 5220, 5240, 5745, 5765, 5785, 5805, 5825}` — the 5745-5825 range
(UNII-3) is US-only, illegal under ETSI, and never filtered against what
the phy/regulatory domain actually allows (`radio-setup.sh`'s
`iface_supports_freq` already does this exact check elsewhere and is the
convention to reuse). Separately, `scanIface` synthesizes
`NoiseFloor = -100` for any candidate with no survey entry (e.g. an
unscannable/illegal channel) — better than any real measurement, so
`rawScore` picks it as the winner outright. On an EU node today this just
fails quietly; **once verify-after-apply exists, it becomes a genuine
restart loop** unless the rate-limit above is actually in place. Minimum
required fix alongside this work: `scanIface` should drop missing-survey
results instead of synthesizing a fake winning score, so
`aggregateChannelReports` correctly returns "no data" and skips the
candidate. Filtering `band5Channels` by phy capability up front is the
fuller fix and can land separately.

**2026-08-27 — both of the above implemented, then a near-miss caught in
review before either could reach a real node.** `node-manager/scan.go`:
`scanIface` no longer synthesizes the fake `-100` noise floor (a
one-line `continue` when a candidate has no real survey entry — see
`channel_election.go`'s existing `ok=false` skip path, no new mechanism
needed); a new `activeBand5Channels(iface5)` derives the 5GHz candidate
list live from `iw phy <phy> info` instead of the hardcoded
US-domain-only `band5Channels`, excluding anything flagged `(disabled)`,
`(no IR)`, or `(radar detection)` — the last one because a DFS channel
pending Channel Availability Check isn't flagged disabled but is
equally unusable right now, and this same function is reused verbatim
by the planned self-heal (below) as its "is this frequency legal right
now" guard, so it can't silently pass a CAC-pending channel.

**First review pass found a defect that would have put the entire US
fleet into permanent 5GHz lobby+limp mode, not just fixed the EU edge
case this was written for.** `iw phy info` prints frequencies with a
fractional part (`"5180.0 MHz"`, confirmed against this repo's own
`radio-setup.sh`/`manet-wlan-reconcile.sh` greps for exactly that dotted
form) — the first parsing pass used `strconv.Atoi`, which fails on every
single line, silently returning an empty usable-frequency set on every
real node regardless of regulatory domain. Combined with the first
review's second finding — `electBand` conflated "zero candidates had
any data" (an outage) with "every candidate was disqualified by noise"
(a real RF problem), both falling to `lobbyFreq`+`limp: true` — this
would have restarted `wpa_supplicant` and throttled the whole mesh to
legacy bitrates on every single node, every tick, the moment this
landed. Caught entirely by review against real code, before any
hardware or deployment was involved.

**Fixed and independently re-confirmed** (a second review pass stubbed
`iw` with realistic fixtures — US-domain, EU-domain, malformed, and a
DFS/radar-flagged line — and ran the actual parsing code against them,
not just read it):
- `phyUsableFreqs` (`scan.go`) now parses via `strconv.ParseFloat` +
  `math.Round`, and treats a zero-length parse result as an error
  (distinguishes "the parser broke" from "this phy legitimately has
  nothing usable," loud instead of silent).
- `electBand` (`channel_election.go`) now tracks `hadAnyData` across the
  candidate loop, separate from `len(scored)`; zero data on either band
  returns `electionResult{freq: currentFreq, limp: false}` (hold, no
  restart, no mesh-wide disruption) instead of lobby+limp. Confirmed
  this also covers the 2.4GHz case (a failed/cold-boot scan with no
  survey data yet), not just 5GHz.
- `activeBand5Channels` resolves the phy once per tick and does map
  lookups (`phyUsableFreqsForIface`, new), rather than once per
  candidate — confirmed via instrumented stub to have dropped from ~16
  subprocess spawns per tick to 2, while `freqAvailableOnPhy`'s own
  external signature/behavior (the function the planned self-heal will
  actually call) is unchanged.
- `(radar detection)` added to the same exclusion check as
  `(disabled)`/`(no IR)` — confirmed same code path, not a parallel one.

**Not fixed, deliberately deferred, logged so it isn't lost:**
frequencies are rounded to the nearest whole MHz, which would
round-collide for HaLow/S1G's half-MHz-spaced channels (e.g. 903.5 vs
904) — unreachable today since only the 5GHz mesh interface calls this,
but flagged directly in the code comment so nobody points this function
at a HaLow interface without switching to integer-kHz keys first. Also
noted but not built: a phy where every candidate is legitimately
disabled (a real regulatory-domain-00 state) currently produces the same
"parse failure" error as an actual parsing bug, which is behaviorally
harmless today (both routes land on the same hold) but would need
separating if a future consumer needs to tell "couldn't determine" apart
from "confirmed illegal"; and `electBand`'s new hold branch logs once
per 180s cycle with no escalation if it persists for many cycles in a
row — better than the old permanent lobby+limp, but a silently-stuck
hold is still only visible in the journal. Worth folding into the
self-heal's own circuit-breaker state below rather than building a
second, separate consecutive-failure tracker for the same underlying
condition.

**2026-08-27 — re-scoped against the current actual code, and repositioned
now that `noscan` exists (see
[`wpa-supplicant-mesh-noscan.md`](wpa-supplicant-mesh-noscan.md)).** This
mechanism was originally the primary fix candidate for the 5GHz mismatch;
it's no longer that — `noscan` fixes the root cause (a restart now
reliably reconverges both nodes), so this is now a **defense-in-depth
safety net** for the remaining ways `runACSTick`'s elected value can
silently diverge from live radio state (a failed join, a driver hiccup, a
race during restart) that have nothing to do with the coex-scan bug.
Still worth building, just no longer urgent in the way it was when it was
the only lever.

Confirmed directly against current source, not re-derived from memory:

- `setIfaceFrequency` (`node-manager/main.go:214-229`) calls
  `rewriteFrequencyLine` (`main.go:177-205`), which does exactly
  `if freq == targetFreq { return false }` — a pure text compare against
  the **config file**, zero awareness of live radio state. This is the
  precise gap: when the elected value doesn't change cycle-to-cycle,
  `rewriteFrequencyLine` returns `false` and `setIfaceFrequency` exits
  immediately, never even reaching the `radioIfaceEnabled`/restart branch
  — so a corrective restart genuinely cannot happen today no matter how
  long a live divergence persists.
- `waitForFrequency` (`tourguide.go:232-241`) confirmed to have exactly
  the substring bug already described above: `strings.Contains(out,
  targetFreq+" MHz")` against the full `iw dev info` output. Returns
  `void`, not `bool` — callers can't act on failure even if they wanted
  to today.
- `runACSTick` (`main.go:434-496`) calls `setIfaceFrequency` for each band
  once per tick unconditionally (`main.go:462`, `:472`) — the hook point
  for a live-state check is right after that call, per band, only when
  `setIfaceFrequency` reports "no config change was needed" (i.e. this is
  specifically the silent-divergence case, not a fresh election).

**New interaction risk, not in the original design — found by reading the
main loop, not by testing:** `reconcile5GHzWidth(iface5)`
(`main.go:387-412`) runs once per 15s tick, **immediately before**
`runACSTick()` in the same `loop` closure (`main.go:53-68`). It has its
own independent `systemctl restart wpa_supplicant@<iface>` path (when the
`disable_ht40`/`disable_vht` keys need to change) with **no settle delay
after the restart** — unlike `setIfaceFrequency`, which sleeps 5s after
its own restart. If a live-divergence check inside `runACSTick` runs
right after `reconcile5GHzWidth` just restarted the same interface for an
unrelated reason (a width-toggle change), the radio may not have
re-joined yet, and the check would see a false mismatch and needlessly
fire its own corrective restart on top of one that was already in
flight. **Fix: track one shared per-interface "last restarted at"
timestamp** (not two independent trackers, one per reconciler) and skip
the live-divergence check entirely for an interface that either
reconciler restarted within the last tick — the next 15s tick re-checks
cleanly once the radio has actually had time to settle.

**Implementation-ready plan, not built yet:**
1. Fix `waitForFrequency` to parse the `channel N (FFFF MHz)` line via
   regex (not substring match) and return `bool`. Reuse it (or a sibling
   helper built the same way) for the new live-check, per the original
   design's own instruction not to write a second poller.
2. Add a small package-level `map[string]time.Time` (iface → last
   restart, covering both `setIfaceFrequency` and `reconcile5GHzWidth`'s
   restart paths) near the existing `lastACSCycle` state.
3. In `runACSTick`, after each band's `setIfaceFrequency` call: if it
   returned `false` (no config change) **and** the interface wasn't
   restarted by either reconciler within the last tick, check live state
   against the target. On mismatch: log under `[acs]`, write a
   `/var/run/` marker (matching `mesh_limp_mode`/`tourguide_state`
   convention, per the original design), and force one restart — capped
   at once per `acsCycleInterval` (180s) per interface, exactly as
   originally specified, with the EU-regulatory-domain fix above
   (`scanIface` no longer synthesizing a fake winning score for
   unscannable channels) landing first or alongside, since this is what
   turns that existing quiet failure into an actual restart loop.

**Not yet done: validating this design with `manet-architect`** before
writing code, matching this project's own convention for ACS/radio-
config changes (see "Fix design — validated by manet-architect
2026-08-26" above) — this is a re-scope of prior work, not fresh design,
but it does touch the same hot loop and deserves the same gate before
implementation starts.

### 2026-08-27 — validated by `manet-architect`, verdict: approved with changes

Five blocking corrections to the re-scope above, all found by reading the
actual current code, not by re-deriving from this doc. **Implementation
has not started** — this is a design correction, same convention as the
2026-08-26 `manet-architect` pass earlier in this doc.

**1. (Blocking) The shared per-interface timestamp map is the wrong
mechanism — use systemd, not in-process state.** The map only sees
`node-manager`'s own two restart paths (`setIfaceFrequency`,
`reconcile5GHzWidth`). At least six other things on a live node restart
`wpa_supplicant@<iface>`, all invisible to an in-process map:
`sae-watchdog.sh:60` (restarts *every* mesh iface on
`MESH-SAE-AUTH-BLOCKED` — the exact symptom class this self-heal
targets, two healers now racing with zero shared state), `manet-ctrl/
api.go` (`apiControlWifiChannel`, radio enable/disable, SSID/key change),
`batman-if-setup.sh`, `radio-setup.sh:2146`, and critically
**`mesh-radio-state/main.go`'s `applyIface`, invoked via
`radioStateSync()` at `main.go:54` — in the same 15s tick, immediately
before `runACSTick`, from a separate process.** This is the exact
`reconcile5GHzWidth` race the design already worried about, one line
earlier in the loop, uncovered by the proposed fix. **Fix: query systemd
instead** — `systemctl show -p ActiveEnterTimestampMonotonic --value
wpa_supplicant@<iface>.service`, predicate "unit has been continuously
`active` for ≥ N seconds", not "did I restart it." Existing in-repo
precedent for exactly this: `manet-ctrl/collect.go:842`
(`isUnitRestarting`), `collect.go:1329`, `applets.go:130`. Also gate on
`ActiveState == "active"` — skip the check entirely if `activating`/
`failed`, don't interpret that as divergence.

**2. Restart sources the design missed, and one item to delete from the
plan.** Tourguide's `hopFrequency` (`tourguide.go:221-230`) uses `wpa_cli
reconfigure`, not a unit restart — invisible to systemd timestamps too.
Saved only by call order: `maybeRunTourguide` runs at `main.go:492`,
*after* both `setIfaceFrequency` calls (`:462`/`:472`). **This ordering
is now a hard constraint, not an accident** — the live-check must stay
immediately after each `setIfaceFrequency` call, never moved to the end
of `runACSTick` or into the 15s `loop` closure, or it will generate
guaranteed false mismatches during tourguide's ~12s dwell.
`applyPartitionMerge` (`tourguide.go:351-352`) calls `setIfaceFrequency`
twice inside that same dwell window — covered fine by the systemd-
timestamp fix in #1, just flagging it's in-scope. Separately: **delete
the "tourguide return-hop escalation" item from this doc's plan
(originally at what's now roughly line 682-685)** — a stranded radio
after a failed return hop is exactly a live-vs-elected divergence the
self-heal already catches next cycle; building both is redundant.

**3. (Blocking) The rate limit as specified is a no-op, and the original
circuit breaker was silently dropped during the re-scope — restore it.
`scan.go`'s fix must land strictly *before*, not "first or alongside."**
`runACSTick` already early-returns unless `acsCycleInterval` has elapsed
(`main.go:403-405` in this worktree) — "cap at one restart per
`acsCycleInterval`" is exactly the rate the code already runs at, so it
bounds nothing new. Worse: even if it did something, "one restart every
180s" is not a bound, it's a **permanent recurring outage** — every
restart tears down the mesh plink and flaps batman-adv TQ for every
peer, not just the local node; on an EU node electing an illegal channel
this turns today's one-time quiet failure into mesh-wide route-flapping
forever. The original 2026-08-26 design's second constraint — "stop +
log once after N consecutive failures on the same target rather than
looping forever" — is the actual bound and was dropped from the
2026-08-27 re-scope above without a stated reason. **Restore it as a
required circuit breaker.** It also matters for a second reason: a node
restarting every 180s republishes an oscillating `DATA_CHANNEL_5_0` vote
(this is *live* state, per the "Where the vote data actually comes
from" note earlier in this doc), which can drag peers around — stable-
but-wrong beats oscillating. Additionally, dropping the `-100` noise
synthesis in `scan.go` is necessary but **not sufficient as the only
gate** — `iw` commonly still lists supported-but-regdomain-forbidden
frequencies even when a survey entry exists. **The self-heal needs its
own independent guard**: before firing any corrective restart, confirm
the target frequency is actually permitted on the phy right now
(reusing/fixing `radio-setup.sh`'s `iface_supports_freq` convention —
note that function as written greps for `"<freq>.0 MHz"` in `iw phy
info` without excluding `(disabled)`/no-IR entries, so the Go port must
filter those out, not copy the bug). Reviewer's broader recommendation,
treated as a requirement of this change rather than deferred: derive
`band5Channels` from live phy capability instead of a hardcoded
US-domain list — `node-manager` currently reads no regulatory domain
anywhere despite `radio-setup.sh` already doing so, and once a self-heal
can actively restart on a bad election, the hardcoding is what makes an
EU misconfiguration actively harmful rather than passively wrong.
HaLow confirmed unaffected either way — `meshIfaces()` only reads
`/var/lib/mesh_if`, and `setIfaceFrequency` only ever drives
`wpa_supplicant@` units.

**4. Reading live `iw` state from the 15s tick path — four fixes.**
(a) **Timeout**: `waitForFrequency` uses a bare, unbounded
`exec.Command(...).Output()` — use `exec.CommandContext` with a 2-3s
timeout, matching `scan.go:53`'s existing convention (established
specifically because `iw` can hang). (b) **Don't reuse the 10s poller at
the decision point** — `runACSTick` already spends up to 10s across
`setIfaceFrequency`'s two 5s post-restart sleeps plus up to 10s in
`performScan`, against a 15s tick; budget is already tight. Factor a new
single-shot `readIfaceFreq(iface) (string, bool)`, with
`waitForFrequency` becoming a thin loop over it instead of a second,
independent poller. (c) **"Wrong channel" ≠ "no channel"**: a mesh iface
that never joined (the `mesh join error=-1` case this doc already
documents) prints no `channel` line in `iw dev <iface> info` at all —
treating that as a mismatch would restart forever a node that
structurally cannot join, and a restart cannot fix a config rejection.
Only "channel line present and wrong" should ever fire a corrective
restart; "no channel line" is log + marker only. (d) **Gate on
`radioIfaceEnabled(iface)`** — `setIfaceFrequency` already skips its own
restart when the radio is desired-down but still returns `true` in that
case; in the self-heal's actual trigger condition (config already
matches, so `setIfaceFrequency` returned `false`), a downed radio would
report no channel line, get misread as a mismatch, and get restarted
against the operator's own intent. Also: any `iw` error or empty output
(USB adapter unplugged, driver reload, `mesh-radio-state` mid-flight) —
skip silently, never restart on an error.

**5. (Blocking) Marker file must clear on recovery, not just get
written on fault.** `setLimpMode` (`main.go:505-512`) is the in-repo
pattern to copy: write on fault, `os.Remove` on recovery. A marker that
never clears is the same failure class as this project's own recurring
"alfred silently down for 8 days" incident. Content should be
`writeStateFile`-style `KEY=value` lines (iface, target, actual,
timestamp) matching `tourguide_state`'s shape, not existence-only. And a
`/var/run/` file nobody looks at won't get noticed in practice — surface
it in `mesh status`/the manet-ctrl radio view, which is the same missing
alert this doc already flags elsewhere ("wpa_supplicant is running but
never joined its mesh group") — one implementation covers both gaps.

**6. Static mode (`acs=n`) silently loses the self-heal entirely — this
needs a deliberate decision, not an implicit one.** The original design
put the check inside `setIfaceFrequency` (covering both ACS and static
modes); the re-scope moved it into `runACSTick` only, so
`ensureStaticChannels` nodes get no self-heal at all. Either state that
scope reduction explicitly and drop the corresponding item from this
doc's testing checklist below, or move the check into a helper both
`runACSTick` and `ensureStaticChannels` call — don't leave this
unresolved by accident.

**7. `waitForFrequency`'s substring bug is real but not currently live —
calibrate the fix's urgency honestly.** With today's candidate sets
(`scan.go`'s `band24Channels`/`band5Channels`), no `width:`/`center1:`
value happens to collide with any candidate frequency, so the bug can
currently only produce a false *pass*, never a false failure. It becomes
genuinely load-bearing the moment any check built on it drives a restart
decision — worth fixing as part of this work regardless, just don't
describe it as an active bug today.

**8. This doc's file:line references have drifted (~32 lines) from the
current worktree — implementer should locate every symbol by name, not
trust line numbers written here.** Confirmed via direct comparison:
`runACSTick` is actually `main.go:402-496` (this doc says 434-496),
`reconcile5GHzWidth` is `:355-380` (doc says 387-412),
`setIfaceFrequency` is `:213-228` (doc says 214-229), `desiredMeshWidth`
is `:269` (`wpa-supplicant-mesh-noscan.md` §6 says 301).
`waitForFrequency:232-241` and the `:462`/`:472`/`:53-68` call sites
above are all still correct as written.

**Testing additions from this review, on top of the checklist already in
this doc:** a negative test forcing sustained divergence to confirm the
circuit breaker actually trips and *stops* restarting (watch ≥30 min,
not 10 — this is the least likely path to get exercised by accident); a
marker-clears-on-recovery test; the EU-regulatory test split into two
independent layers (does ACS avoid electing the illegal channel at all,
*and separately* does the phy-capability guard block a restart even if
it did); a radio-disabled-node test (zero corrective restarts over 10+
min); and — flagged as the single highest-value new test, not previously
in this doc — a **concurrent-healer test**: trigger `sae-watchdog.sh`'s
`MESH-SAE-AUTH-BLOCKED` path while an ACS cycle is mid-flight, confirm
`node-manager` backs off (via the systemd-timestamp check from #1)
rather than racing it, and confirm the interface actually ends up back
in `bat0` afterward either way.

**Blocking items before implementation starts:** #1 (systemd timestamps,
not the in-process map), #3 (`scan.go` fix strictly first, independent
phy-capability guard, circuit breaker restored), #4's sub-points (b)
`radioIfaceEnabled` gate, (c) "no channel" ≠ mismatch, (a) `CommandContext`
timeout, and #5 (marker clears on recovery). #2's ordering constraint
must also be preserved, not just noted.

### 2026-08-27 — implemented on `feat/acs-verify-after-apply-selfheal`, all blocking items addressed

**Not yet deployed or hardware-tested.** Build/vet/gofmt clean, two
independent review passes (one full, one focused re-verification after
this session's own reviewer agent hit an infrastructure rate limit and
the checks were completed directly instead), no blocking findings
remaining. Nothing here has touched a real node.

**New file `node-manager/acs_selfheal.go`** — all of the new machinery:
- `readIfaceFreq(iface) (string, bool)` — single-shot live-frequency
  read via `iw dev <iface> info`, regex-matched against the actual
  `channel N (FFFF MHz)` line (not a substring match), 3s
  `CommandContext` timeout. `ok=false` covers both "no channel line"
  (never joined) and any command failure — both handled identically by
  every caller (log + marker, never a restart), satisfying point 4's
  "never restart on an error" requirement even though the two cases
  aren't distinguished from each other in the return value itself.
- `unitActiveFor(unit) (time.Duration, bool)` — replaces the
  originally-proposed in-process restart-tracking map entirely, per
  point 1. Queries `systemctl show -p ActiveState -p
  ActiveEnterTimestampMonotonic --value` and compares against
  `/proc/uptime` (both boot-relative `CLOCK_MONOTONIC`-derived, so this
  is immune to wall-clock/NTP skew — deliberately chosen given this
  project's own past mesh-time-sync incidents). Correct regardless of
  *who* restarted the unit — `reconcile5GHzWidth`, `sae-watchdog.sh`,
  `mesh-radio-state`'s `applyIface`, manet-ctrl's API, anything.
- `acsHealMinUnitUptime = 20s` — the settle window before a live-state
  read is trusted. Verified this is meaningful, not dead weight: since
  `runACSTick` only does real work once per `acsCycleInterval` (180s,
  gated internally — confirmed unchanged), a restart from anything else
  landing in the *same or immediately preceding* 15s tick is exactly
  what 20s (a bit more than one tick) is sized to catch; a restart from
  much earlier trivially clears 20s on its own.
- `acsHealTripThreshold = 3`, `acsHealState{target, consecFail}`,
  `acsHealStates` (per-iface, unsynchronized — verified safe, everything
  in this file runs from `node-manager`'s single sequential loop, never
  concurrently) — the restored circuit breaker. Traced cycle-by-cycle:
  cycles 1-3 each fire one restart (`consecFail` 0→1→2→3, checked
  *before* incrementing); cycle 4 sees `consecFail(3) >=
  acsHealTripThreshold(3)` and stops — exactly 3 restart attempts, no
  off-by-one. A new elected target (`st.target != targetFreq`) resets
  the counter, per point 1's "a fresh situation, not a continuation."
- `freqAvailableOnPhy` (from stage 1, `scan.go`) reused verbatim as the
  independent legality guard before ever restarting toward a target —
  confirmed all three of its failure modes (parse error, query error,
  confirmed-illegal) return early without ever reaching
  `acsHealStates`/`restartWpaSupplicant`, no fallthrough bug.
- Marker file `/var/run/mesh_acs_divergence_<iface>`
  (`writeAcsDivergence`/`clearAcsDivergence`) — `KEY=value`
  (`IFACE`/`TARGET_FREQ`/`ACTUAL_FREQ`/`TIMESTAMP`/`CONSEC_FAIL`) via
  the existing `writeStateFile` helper, matching `tourguide_state`'s
  shape; cleared via `os.Remove` on recovery, matching `setLimpMode`'s
  write-on-fault/remove-on-recovery convention — satisfies point 5.
- A second, deliberately separate small counter/marker
  (`acsHoldStreaks`, `acsTrackHold`,
  `/var/run/mesh_acs_divergence_<iface>_hold`) escalates stage 1's
  `electBand` "hold" result (zero scan data at election time) after 3
  consecutive cycles, rather than folding it into the same state as the
  live-frequency divergence above — a deliberate choice (they trigger at
  different points in the tick and neither's resolution affects the
  other), not an oversight; the two share only their marker-file
  *shape*, not their tracking state.

**`main.go`**: `restartWpaSupplicant` factored out of `setIfaceFrequency`
so the self-heal's corrective restart is the literal same code path, not
a reimplementation. `setIfaceFrequency` now returns whether the config
actually changed. `runACSTick`'s two per-band blocks call
`acsVerifyAfterApply`/`acsTrackHold` immediately after their
`setIfaceFrequency` call and before `maybeRunTourguide` — confirmed by
reading the final function top to bottom, satisfying point 2's ordering
constraint (this must never move to the end of the tick or into the 15s
loop closure, or it will false-trigger during tourguide's ~12s dwell).
`ensureStaticIfaceChannel` also calls `acsVerifyAfterApply` — the point-6
decision, made explicitly: static (`acs=n`) nodes get the same self-heal
coverage rather than silently losing it.

**`tourguide.go`**: `waitForFrequency` rewritten as a thin retry loop
over `readIfaceFreq`, now returns `bool` — fixes the substring-match bug
(point 7) as a side effect of the refactor rather than a separate patch.
The tourguide return-hop escalation item from the original design was
deliberately **not built** — a stranded radio after a failed hop is
exactly the divergence this self-heal already catches next cycle, per
the review's point 2; `hopFrequency` just logs a failed hop now.

**`channel_election.go`**: `electionResult` gained a `hold bool` field,
set precisely when stage 1's `!hadAnyData` branch fires — gives
`acsTrackHold` a clean signal instead of inferring "was this a hold"
from a score-based heuristic.

**`manet-ctrl/collect.go`**: `acsDivergenceFault`/`acsHoldFault` read
both marker files (no shared package between the two binaries, so the
path/format is kept in sync by convention and comments, not by import)
and surface them as `Faults` entries in the existing per-interface
health view — reachable via `mesh radio-info`/`mesh status` with zero
new UI plumbing. Confirmed independent of the pre-existing `"Inactive in
batman-adv"`/`"No wpa_supplicant"` checks: only ever upgrades
`ok`→`warn`, never downgrades an existing `fault` — severity ordering
preserved.

**One asymmetry worth knowing about, not a bug**: `ensureStaticChannels`
(static mode) is called every 15s tick, unlike `runACSTick`'s internal
180s gate — so in static mode, a corrective restart's very next tick
(15s later) gets skipped by the 20s settle window, then evaluated again
one tick after that (~30s after the restart). Static mode's effective
restart cadence during a sustained divergence is therefore closer to
~30s than the nominal 15s tick, versus ACS mode's ~180s. Not incorrect —
both modes still terminate via the same 3-attempt circuit breaker — just
a real difference in how long that takes wall-clock-wise between the
two modes, worth knowing before being surprised by it during testing.

**Not yet done — required before this is considered field-ready, not
just code-complete:**
- Hardware test: force a sustained divergence on a real node, confirm
  the breaker actually trips and stops (watch ≥30 min, not 10, per the
  testing-additions list above), confirm the marker appears/clears
  correctly, confirm it's visible via `mesh radio-info`.
- The concurrent-healer test (`sae-watchdog.sh` racing an ACS cycle) —
  the single highest-value test named above, not yet run.
- Confirm `iw dev <iface> info`'s `channel N (FFFF MHz)` line format
  against this fleet's actual driver/`iw` version live — the regex was
  traced against the documented format, not run against real output.
- EU-regulatory-domain node test of the `freqAvailableOnPhy` guard
  actually blocking a restart toward an illegal target.

**Related future work, flagged by the user 2026-08-26, not yet
scoped/implemented:** `regulatory_domain` is currently one combined
mesh.conf key, but WiFi and HaLow hardware have different real-world
constraints — HaLow varies by chip (MM6108 is US-only, MM8108 is
US+EU-capable) while the WiFi cards can likely do both, independent of
which HaLow chip a given node has. A separate `halow_regulatory_domain`
key already exists in `radio-setup.sh` for exactly this, but has a real
bug: `uses_eu_halow_region("$REGULATORY_DOMAIN")` unconditionally forces
HaLow to `EU` whenever the general/WiFi domain is an EU country, even if
`halow_regulatory_domain` was explicitly set to `US` — there's no
hardware-capability check (MM6108 vs MM8108) at all, and no way today to
run WiFi on EU bands while keeping HaLow pinned to US. Full detail in the
`regulatory_domain_wifi_halow_split` memory. Not part of this fix's scope,
but touches the same `radio-setup.sh` regulatory-domain code the EU-domain
fix above lives in — worth doing together if either is picked up.

**Channel-flapping risk, also newly identified:** because
`DATA_CHANNEL_5_0` reports *live* state and `electBand` ranks by votes
first, a node stuck on the wrong primary **votes for its own wrong
channel**. In a two-node case (EUD3/EUD4) each side sees exactly one peer
vote, so it's a tie broken by score/channel-ascending — meaning EUD4 could
in principle get dragged onto EUD3's wrong channel rather than the other
way around, and EUD3's next restart could shift again. This is why the
mismatch marker from part (3) matters even before part (1) is field-proven:
it's the only signal that distinguishes "the mesh genuinely agreed to
move" from "one node's broken state is dragging its peers."

### Files/functions that need to change (per the validation pass)

- `node-manager/main.go` — `setIfaceFrequency` (:210, verify-after-apply +
  rate limit); new `ensureMeshConfDefaults(iface)` (migration, called from
  the loop closure per mesh iface); new package-level per-iface
  mismatch/cooldown state near `lastACSCycle`.
- `node-manager/tourguide.go` — `waitForFrequency` (:232, fix the string-
  match bug, return `bool`); `hopFrequency` (:221, propagate the result);
  the return-to-data hop (:114, escalate on failure).
- `node-manager/scan.go` — `scanIface` (:64-74, stop synthesizing `-100`
  for missing survey data); `band5Channels` (:18, at minimum comment that
  it's US-domain-only, ideally filter by phy capability).
- `rootfs/usr/local/bin/radio-setup.sh` — mesh lobby template heredoc
  (~:1012-1028), add the new keys.
- `rootfs/usr/local/bin/manet-wlan-reconcile.sh` — mesh lobby template
  heredoc (~:319-335), same keys, byte-consistent with radio-setup.sh.
- `manet-ctrl/api.go` — `apiControlWifiChannel` (~:829-873) writes
  `frequency=` and restarts without verifying, worth the same treatment
  for consistency (its sibling `setIfaceTxPower` two lines below already
  verifies-and-reports) — not required for this fix, but flagged as an
  inconsistency worth closing in the same pass.

**Explicitly out of scope, don't touch:** `mesh-boot-lobby.service` itself
(generated, not tarball-redeployable — fix via the Go migration instead);
the HaLow `-s1g` config path and `halow_regulatory_domain` handling —
`setIfaceFrequency` only ever drives `wpa_supplicant@` units, and
`meshIfaces()` already excludes HaLow.

### Open question, not yet resolved — flagging honestly rather than guessing

Live registry dump from EUD4 (2026-08-26) shows `NODE_..._DATA_CHANNEL_5_0`
populated for EUD1, which per the fleet topology docs is a HaLow-only +
onboard-WiFi-AP node with no 5GHz mesh radio at all. `getChannel()` matches
*any* interface in the 5GHz frequency range via a loose `iw dev` scan, not
specifically the mesh-point interface — so if a node's AP-mode radio is
dual-band-capable and happens to be sitting on a 5GHz frequency (e.g. from
AP operation, unrelated to mesh), it could pollute `peerChannelVotes` with
a vote that has nothing to do with actual mesh-radio consensus. This was
noticed while re-verifying the vote counts (`votes=3` in a 4-node mesh where
only 2 nodes are believed to have 5GHz mesh radios doesn't add up under a
strict one-vote-per-mesh-radio reading) but **not chased down** — could be
this AP/mesh radio conflation, could be something else about how
`registry` entries are populated. Worth resolving before trusting vote
counts precisely in future ACS work.

## Open issue: cold-boot 5GHz election races ahead of peer gossip

**Found 2026-08-27/28, live, during the reboot verification of the
`noscan`+self-heal deployment above** (see
[`wpa-supplicant-mesh-noscan.md`](wpa-supplicant-mesh-noscan.md) §9 for
that deployment's own status — this bug is unrelated to it and would
exist with or without `noscan`; it was simply never exercised by a real
cold reboot until tonight). **Implemented 2026-08-28, hardware-verified
2026-08-28** — deployed to EUD3+EUD4 and confirmed via a real sequential
reboot of both nodes: no outage, correct channel elected on both nodes'
own cold starts, mesh re-established in the same cycle instead of ~180s
later (see "Implementation — 2026-08-28" at the end of this section, and
the "What needs testing" checklist above for the exact verification
results).

**Symptom, reproduced on EUD3:** cold-rebooted a node that was already
correctly meshed with EUD4 on 5GHz (channel 44/5220). Result: a genuine
**3.5-4 minute 5GHz mesh outage** before it self-corrected. 2.4GHz and
HaLow stayed healthy throughout — this is a 5GHz-electBand-specific gap,
not a general mesh-recovery failure, and EUD4 was never affected (kept
correctly electing 5220 with `votes=3` the entire time — the existing
vote-first convergence design held up under a peer temporarily voting
wrong).

**Root cause, traced via `journalctl -b`:** `mesh-boot-lobby.service`
resets the live `wpa_supplicant-wlan1.conf`'s `frequency=` to the lobby
value (5180) before `wpa_supplicant@wlan1` even starts — this is correct,
by-design behavior. `node-manager` then runs its first `runACSTick`
~20s later, before alfred/mesh-registry has had any chance to gossip a
peer's channel vote (`elected channel 5745 (score -92.00, votes 0)` — a
true cold start, zero votes). With zero votes, `electBand`
(`channel_election.go`) falls back to raw noise score with a small
`incumbentBiasDB` (4.0) nudge toward whatever `currentCh` is — but
`currentCh` comes from `cur := getConfFreq(wpaConfPath(iface5))`
(`main.go:487`), which by this point already reads back the
just-written *lobby* frequency (5180) — a channel number
(36) that isn't even in `band5Channels`'s candidate set at all. So the
incumbent bias silently applies to nothing, and the winner is chosen by
raw local noise score alone: EUD3 picked channel 149/5745 (-92.00dBm)
over the mesh's actual already-converged channel 44/5220 (-90.50dBm) —
a 1.5dB gap that `incumbentBiasDB` (4.0) would easily have overridden
*if it had been biasing toward the right channel*. ~180s later (the next
ACS cycle), EUD4's gossiped vote arrived, `votes` went from 0→3, and
EUD3 correctly re-converged to 5220 — self-healing, but only after a
real multi-minute outage.

**Why the self-heal above doesn't catch this**: `acsVerifyAfterApply`
is scoped to *intra-node* live-radio-vs-own-elected-value divergence,
and by design skips its check entirely on any tick where
`setIfaceFrequency` just changed the config itself (`configChanged`) —
which is exactly this tick, since the mis-election *is* the config
change. It has no visibility into *inter-node* disagreement (this node
elected something different from what its peer is actually using) —
a structurally different problem from what it was built to catch.

**Proposed fix, being validated before implementation**: seed the
cold-start incumbent-bias tiebreak from a small node-manager-owned
persisted state file (e.g. `/var/lib/mesh_last_5ghz_freq`, mirrored for
2.4GHz) — written every time `electBand` produces a genuine winning
election (`result.winnerCh != 0`, not a lobby/limp fallback) — read back
as `cur` instead of `getConfFreq(wpaConfPath(iface))`. Because this
lives under `/var/lib/` rather than the wpa_supplicant conf that
`mesh-boot-lobby.service` resets every boot by design, a freshly-booted
node's cold-start tiebreak would correctly bias toward its own
last-known-good channel instead of nothing. Bounded, not a staleness
risk: the moment any real peer vote exists (same cycle or the very next
one), the existing vote-first design completely supersedes the
incumbent bias regardless of what it's seeded with — this fix can only
ever affect the narrow window before any peer vote has arrived, never
beyond it.

### Validated by `manet-architect`, 2026-08-27/28 — verdict: approved with changes

Two corrections are **blocking** — the fix as first proposed would have
introduced a new regression and a new data-corruption path. Both fixed
in the design below before any code was written.

**Blocking correction 1 — the persisted value must never feed the
`hold` path's returned frequency, only the bias comparator.**
`currentFreq` (the parameter originally proposed to swap wholesale) is
not just a bias input — it's the literal value `electBand`'s `hold`
branch returns, which `runACSTick` then applies via
`setIfaceFrequency`, restarting `wpa_supplicant`. Swapping it for the
persisted value would turn "no scan data, change nothing" into
"rewrite the conf to a remembered frequency and restart" — exactly the
mesh-wide disruption the `hold` branch exists to avoid, and worse on a
cold boot with a failed first scan (it would yank the radio off the
lobby before any peer could find it). **Fix: `electBand` takes a
separate `biasFreq` parameter**, used only inside the `totalVotes == 0`
comparator; the `hold` return keeps using the real `getConfFreq`-derived
`currentFreq` as before. `biasFreq` selection rule: use the live-conf
value when it's actually in the candidate list; fall back to the
persisted value only when it isn't (i.e. only in the lobby-reset
cold-start case this fix targets).

**Blocking correction 2 — `result.winnerCh != 0` is not the "genuine
election" signal.** The `hold` return also sets `winnerCh: currentCh`
(non-zero whenever the conf frequency merely parses), so a first-ever
boot with no persisted file and a failed first scan would persist the
*lobby* frequency (5180/2412 — not even a real candidate) as "last known
good," cementing this exact bug permanently. **Fix: gate the write on
`quorum && !result.hold && result.winnerCh != 0`** — the `quorum` term
matters too, since `runACSTick` overrides to the lobby frequency
whenever `!quorum` regardless of what `electBand` returned; the file
must mean "the frequency this node last actually ran as a data
channel," not "the frequency electBand most recently computed."

**Point 1 (layer choice) — confirmed, both alternatives correctly
rejected, and a stronger argument found for why.** Removing
`mesh-boot-lobby.service`'s frequency reset isn't safe on its own terms
— that template also carries `country=`, SSID, SAE password, and the
5GHz width lines, so it's a whole-file mechanism for several unrelated
settings, not something to special-case one line out of. Delaying the
first `runACSTick` for a gossip round-trip doesn't just add latency, it
fails outright in the realistic worst case: **site-wide power loss**,
where every node cold-starts simultaneously and there is no peer vote
for anyone to wait for, ever. Persisted state is the only mechanism
that works in that exact scenario — every node biases back to the same
pre-outage channel independently and the mesh re-forms in the first
cycle, with no coordinator and no waiting **when every node actually
persisted the same value** — see the follow-up correction below, found
during code review, for the case where that assumption doesn't hold.

**Follow-up correction, found during code review (not design review) —
the simultaneous-cold-start claim above is stronger than what's
actually guaranteed.** Persisted values can genuinely diverge across
nodes: the write is gated on each node's *own* tick, and one node's
tick can run up to a full `acsCycleInterval` (180s) later than another's
— during a real mid-flight channel migration (not rare here;
`noiseDisqualifyDBM` alone can force one), a power loss landing in that
window leaves two nodes with *different* last-persisted values from the
same pre-outage mesh. At cold start this can make a split *marginally
more likely* than today in that narrow case, not less — today's
undirected local-noise-score pick is at least *correlated* between
co-located nodes (shared RF environment), where two different 4.0dB
biases toward two different remembered channels deliberately overrides
that correlation. Still net-positive and still bounded to one cycle in
a mesh of 3+ nodes, because gossip doesn't depend on the 5GHz link at
all — HaLow stays on `bat0` throughout on a static, config-driven
channel, so alfred/mesh-registry keeps flowing even if both WiFi bands
temporarily split, and the majority's vote count resolves the minority
within one cycle via the existing `votes desc` comparator.

**One case doesn't self-resolve, and it's pre-existing, not introduced
by this fix**: in an exactly-2-node mesh split A-on-X/B-on-Y,
`peerChannelVotes` excludes self, so A tallies one vote for Y and B
tallies one vote for X — both take the `votes desc` branch and both
move to *the other's* channel, swapping rather than converging, and
swap back again next cycle. This only resolves if the two nodes'
ticks happen to be far enough apart in phase that one moves before the
other reads the registry — and a site-wide power loss is exactly the
scenario that tends to align both nodes' `lastACSCycle` phase (stamped
on each one's first tick after `bat0` comes up, at a similar point
post-boot for both). The EUD3 incident this whole fix is for only
self-healed within one cycle because the fleet has four nodes
(`votes=3` on the correcting cycle) — a real two-node deployment
wouldn't get that for free. A future fix (out of scope here) would be
counting a node's own current channel as one self-vote, so both sides'
tallies end up identical and the existing lowest-channel-number tiebreak
converges — a different mechanism from the additive incumbent *bonus*
already rejected earlier in this doc, not a revival of it.

**Point 2 (staleness) — the original "provably bounded" framing was
overstated; corrected here.** A node stranded on a wrong persisted
channel for a long absence receives no peer votes *specifically
because* it's on the wrong channel (alfred/registry gossip requires an
actual mesh link) — `quorumOK` doesn't rescue this either, since a
long-absent node's empty registry means `active == 0`, which falls
through to `return true` (not a lobby retreat). **This is not a new
stranding class, though** — today's unfixed behavior also parks
indefinitely on one channel in this scenario (raw noise score is stable
cycle to cycle), just a locally-scored one instead of a
previously-real one. The fix changes *which* wrong channel a truly
stranded node parks on (better prior: "where it last worked" beats
"whatever scored best just now"), not whether it can get stranded at
all — recovery in both cases is the same existing tourguide lobby-dwell
+ `analyzeForeignPartitions`/`applyPartitionMerge` path. Where the bound
*does* hold exactly: the instant any real peer vote exists,
`totalVotes > 0` and the comparator never touches the bias at all —
confirmed by re-reading the sort comparator directly.

**Point 3 (tourguide) — confirmed insulated by construction, not just
assumption; no extra write needed.** Traced directly: `hopFrequency`
and `applyPartitionMerge` both call `rewriteFrequencyLine`/
`setIfaceFrequency` directly, never through `electBand` — neither can
produce an `electionResult`, so neither can touch the persisted file.
Explicitly **do not** add a persistence write to `applyPartitionMerge`
despite it being a genuine channel decision: its frequency comes from
`wifiChannelFreq` resolved against the *unfiltered* `band5Channels`
superset (including UNII-3), so on an EU node it could persist an
ETSI-illegal value; the omission self-corrects within one cycle anyway,
since the next election runs with the merged partition's votes and
persists the real result through the normal path. Write placement:
after both bands' blocks in `runACSTick`, before `maybeRunTourguide` —
keeps the insulation structural, not incidental.

**Point 4 (static mode) — no change needed, one explicit guard added.**
`ensureStaticChannels` is deterministic/config-driven, immune by
construction. Explicitly never write the persisted file from the
static path — doing so would store a lobby frequency that becomes a
useless bias value if the node is later switched to `acs=y`.

**Point 5 (2.4GHz) — included, not just 5GHz.** Same code path, same
lobby-reset race, and `band24Channels` (`{2437, 2462}`, lobby `2412`
outside the set) has the identical structural gap — "historically more
RF-stable" is a claim about noise, not immunity to this specific race.
Both bands get the same fix in the same change.

**Point 6 (format) — changed from two separate files to one, KEY=value,
matching existing conventions.** One file,
`/var/lib/mesh_acs_last_channels`, keys `LAST_FREQ_2_4`/`LAST_FREQ_5_0`
(mirroring the registry's own `DATA_CHANNEL_2_4`/`DATA_CHANNEL_5_0`
naming) plus `TIMESTAMP` (meaning "when the value last *changed*", see
point 7) — via the existing `writeStateFile` helper, one write, one
atomic rename for both bands. **Keyed by band, never by interface
name** — `meshIfaces()` derives band from position in `/var/lib/mesh_if`,
not name, and this project has live history of wlan naming/ordering
churn across boots; a band-keyed file survives that, an iface-keyed one
wouldn't. `writeStateFile` does tmp+write+rename with no fsync, so a
brownout mid-write can leave a zero-length file — the reader must treat
missing, empty, and unparseable identically (clean fallback to
`getConfFreq`), documented as a named failure mode on this hardware,
not an edge case to ignore.

**Point 7 (write cadence) — changed from unconditional-every-cycle to
write-on-change.** An unconditional ~180s write to `/var/lib` would be
a genuinely new pattern this project has already implicitly rejected
once — every existing high-frequency node-manager state write goes to
`/var/run` (tmpfs); every `/var/lib`/`/etc` writer elsewhere in this
codebase is change-driven (`mesh-manager`'s `savePersistent()`,
`mesh-registry`'s `maybeSaveKnownNodes` — the latter's own history
explicitly cites avoiding hitting the SD card every 15s regardless of
whether anything changed). Match that: compare against the in-memory
last-written value, skip the write when unchanged. Steady state settles
to zero writes.

**Regulatory-domain safety, made an explicit invariant, not just an
observation**: the persisted value is only ever compared against
`activeBand5Channels`'s live phy-filtered candidate set — a stale or
cross-domain value simply matches nothing and degrades to today's
behavior. **The persisted value must never be passed to
`setIfaceFrequency`/`rewriteFrequencyLine`, and must never be restored
directly into a wpa_supplicant conf** — stated as a hard constraint on
the read helper itself (comment, not just this doc), specifically so a
future "restore last channel at boot" feature doesn't casually bypass
the ETSI filter and write a UNII-3 frequency onto an EU node.

**Testing required once implemented** (beyond re-running tonight's
exact repro): first-ever-boot with no persisted file and a failed first
scan must not write the lobby frequency (blocking correction 2); a
forced no-scan-data cycle with a stale persisted value must produce
zero restarts and zero conf rewrites (blocking correction 1); a
simulated simultaneous multi-node cold start (the actual brownout case
this design is strongest for) should show every node re-converging on
the pre-outage channel in the first cycle; a deliberately wrong
persisted value must yield immediately the first cycle any peer vote
exists; tourguide dwell must never change the file; both bands: static
mode must never create/touch the file; a truncated/garbage file must
fall back cleanly with a log line, no panic; an EU-domain node with a
persisted UNII-3 value must have it silently ignored, never reaching
the conf.

### Implementation — 2026-08-28

Shipped in `node-manager` per the validated design above, both blocking
corrections applied. Not hardware-verified yet — see the checklist this
adds to "What needs testing" below.

- `channel_election.go:190` — `electBand` gains a `biasFreq` parameter,
  inserted between `currentFreq` and `lobbyFreq`. `currentFreq`/`currentCh`
  keep their existing job untouched: they're still what the `hold` branch
  returns and what `runACSTick` applies via `setIfaceFrequency`. A new
  `biasCh` (`channel_election.go:192`) is parsed from `biasFreq` and used
  only inside the `totalVotes == 0` comparator (`channel_election.go:253,
  256`, replacing the old `currentCh` comparisons there) — the moment any
  peer vote exists, this parameter plays no role, same as before.
- `acs_channel_persist.go` (new file) — the persisted state:
  - `lastElectedChannelsFile = "/var/lib/mesh_acs_last_channels"`
    (`:21`) — one file, `LAST_FREQ_2_4`/`LAST_FREQ_5_0`/`TIMESTAMP` keys,
    band-keyed per point 6, written via the existing `writeStateFile`.
  - `readLastElectedFreq(band string) string` (`:42`) — missing, empty,
    and unparseable file all fall back to `""` identically, each logged.
    Carries the hard invariant from this section as a comment on the
    function itself: this value must never reach `setIfaceFrequency`,
    `rewriteFrequencyLine`, or a wpa_supplicant conf.
  - `selectBiasFreq(confFreq string, candidates []int, band string) string`
    (`:83`) — live-conf value if it's in `candidates`, else the persisted
    value if one exists, else the live-conf value anyway.
  - `maybeWriteLastElectedFreq(freq24, freq5 string)` (`:110`) — write-on
    -change against what's currently on disk (read back per band, not an
    in-memory cache, so it stays correct across a node-manager restart);
    `""` for a band means "leave that band's persisted value alone this
    cycle."
- `main.go:487-553` (`runACSTick`) — per band: computes `biasFreq` via
  `selectBiasFreq` before calling `electBand` (`:497-498` for 2.4GHz,
  `:528-529` for 5GHz); after each band's `setIfaceFrequency`/
  `acsVerifyAfterApply`/`acsTrackHold` calls, gates that band's
  `writeFreq24`/`writeFreq5` on `quorum && !result.hold &&
  result.winnerCh != 0` (`:523`, `:542` — blocking correction 2, verbatim);
  `maybeWriteLastElectedFreq(writeFreq24, writeFreq5)` (`:553`) runs after
  both bands' blocks, before `setLimpMode`/`writePartitionSize`/the
  `if quorum { maybeRunTourguide(...) }` block — per point 3, keeping
  tourguide's insulation structural.
- `ensureStaticChannels`/`ensureStaticIfaceChannel`/`hopFrequency`/
  `applyPartitionMerge` — unchanged; confirmed by inspection none of them
  call `electBand`, so none can produce an `electionResult` to gate a
  write on, and none reference the new persisted-state functions at all.

**Verification status:** `go build`/`go vet` (via `-C <absolute-path>`,
not `cd &&`, per this session's own earlier miscompile incident) and
`gofmt -l` clean on `node-manager`. `golangci-lint run ./...` shows the
same pre-existing errcheck findings as before this change
(`limpmode.go`, `main.go`'s `setLimpMode`, `tourguide.go` — all in
untouched lines) and zero findings in the two changed/new files. Not yet
hardware-verified — added to "What needs testing" below.

### Re-implementation — 2026-09-01: the 2026-08-30 supersession was wrong

The checklist item below marked this fix **SUPERSEDED** on the argument
that `mesh-boot-lobby.service` resetting every node to the lobby frequency
means a simultaneous mesh-wide cold start still converges on its own —
"everyone meshes at the lobby, gossips, gets votes, and elects together on
the next tick" — making persisted state unnecessary. That argument only
holds for the *sequential*-reboot case the 2026-08-30 hardware test
actually exercised (EUD3 reboots while EUD4 is already up and already has
a real elected vote to gossip back in). It does not hold for a genuine
*simultaneous* cold start, and the code makes this a certainty, not a risk:
`peerChannelVotes` (`channel_election.go`) only counts a peer's vote for a
channel that's actually in `candidates`, which deliberately excludes the
lobby frequency (`band24Channels`/`band5Channels` vs `lobbyFreq24`/
`lobbyFreq5`, `scan.go`). While every node in the mesh is still sitting at
lobby, every peer's "vote" is for a channel that structurally can never be
counted — `totalVotes` cannot leave 0 by gossip alone, no matter how long
everyone waits or how fast `coldStart`'s retry polls. There is no tick at
which "everyone meshes, gossips, gets votes" actually produces a non-zero
vote, because nobody can ever cast the first one. The fast-retry fix
(`5d9a5ec`) makes the *sequential*-reboot case fast; it does nothing for
this case, since polling faster doesn't help when the value being polled
for is definitionally never going to arrive.

**Confirmed live, not simulated, 2026-09-01:** EUD4 held continuously for
30+ minutes (118 consecutive `acsTrackHold` cycles) and EUD3 for its whole
reachable uptime window, both logging "no peer votes yet (cold start or
isolated)" every cycle for *both* bands. Registry dumps on both nodes
agreed: all reporting nodes' `DATA_CHANNEL_2_4`/`DATA_CHANNEL_5_0` were
`1`/`36` — the lobby channel numbers — confirming the whole reachable mesh
was parked at lobby simultaneously with no path out. Diagnosed via
`manet-node-debug` (batman-adv, alfred, and mesh-registry all confirmed
healthy — the break was purely in `electBand`'s vote-counting logic, not
the gossip layer underneath it), not via code inspection alone.

Re-implemented substantially as originally designed above, plus fixes
found by `manet-reviewer` against a first re-implementation pass that
also needed correcting to actually match this design:

- New file `acs_last_channels.go` (renamed from the deleted
  `acs_channel_persist.go`): `acsLastChannelsFile`, `readACSLastChannels`,
  `acsBiasFreq`, `updateACSLastChannels` — same shape and invariants as
  `selectBiasFreq`/`maybeWriteLastElectedFreq`/`readLastElectedFreq`
  above, renamed. `readACSLastChannels` logs a non-missing-file read error
  and drops (with a log line) any value that doesn't parse as an int,
  rather than passing garbage through silently.
- **The comparator is now an additive nudge, not an absolute override.**
  A first re-implementation pass made `ch == biasCh` the primary sort key
  in `electColdStart`'s comparator — i.e. any bias match wins regardless
  of how much worse it scores. That's a materially different mechanism
  from what this design record actually approved and reasoned about: the
  split-risk analysis above (the "4.0dB" paragraphs) explicitly depends on
  the bias being a *bounded* nudge that a large enough real noise/BSS gap
  can still override, not an unconditional trump card. Fixed: restored
  `incumbentBiasDB = 4.0` (the same constant name/value the original,
  pre-`7ce1df7` incumbent-bias design used, scoped here only inside
  `electColdStart`) as a subtracted adjustment to `rawScore` before
  comparing, never a primary sort key.
- **`electColdStart` only runs when the node itself is still at the lobby
  frequency** (`currentFreq == lobbyFreq`, checked in `electBand` before
  calling it). The first pass ran it for every `totalVotes == 0` case,
  which also fires for an already-converged node whose peer's vote merely
  aged out or temporarily doesn't map into this node's candidate set — a
  case `electBand`'s own doc comment already called out as needing to stay
  a quiet hold, not a scored re-election, specifically so a rebooting
  peer still finds this node parked at its expected rendezvous frequency.
- `electColdStart`'s two "all disqualified"/"still poor" lobby-fallback
  returns now carry `coldStart: currentFreq == lobbyFreq` through, same as
  the plain hold return — a first pass left `coldStart` unset on these,
  which would have silently reintroduced the exact 180s-stall bug the
  `5d9a5ec` fast-retry fix (below) exists to prevent, just reached from a
  different branch.
- `main.go`'s `runACSTick` re-reads each band's conf file after
  `setIfaceFrequency` and only marks that band eligible for
  `updateACSLastChannels` if it reads back as the elected frequency —
  `setIfaceFrequency`'s bool return can't be used for this since it
  conflates "already matched" with "write/restart failed"
  (`rewriteFrequencyLine` returns `false` for both). Matches this
  section's own invariant: the persisted file must mean "the frequency
  this node last actually ran," not "the frequency `electBand` most
  recently computed." `rewriteFrequencyLine`'s two previously-silent
  `os.ReadFile`/`os.WriteFile` error returns now log.

**Verification status:** `go build`/`go vet`/`gofmt -l` clean. Reviewed by
`manet-reviewer` (two critical findings above, both fixed in this pass;
three warnings below also addressed — read-error logging, the
already-read snapshot now threaded into `updateACSLastChannels` instead of
re-read, and the type rename `candidate` → `scoredCandidate` to stop
colliding with this file's existing `candidates []int` naming). Not yet
hardware-verified — added to "What needs testing" below, alongside the
still-open items from the 2026-08-28 pass this supersedes.

## What needs testing

**Before any code is written:**
- [ ] `strings` gate confirming `noscan` (not just `max_oper_chwidth`) is
      present in the deployed wpa_supplicant binary — see "Fix design"
      above. Do not proceed to implementation if it's missing.

**Once implemented:**
- [ ] EUD3 + EUD4: `iw dev wlan1 info` shows the same `channel N (FFFF MHz)`
      on both sides, matching `width`/`center1`. `batctl o` shows a wlan1
      route again, and throughput recovers toward ~340 Mbit/s — the
      pre-regression number is the pass bar, not just "a route exists."
- [ ] Cold reboot on both nodes, not just `systemctl restart
      wpa_supplicant@wlan1` — specifically to prove `mesh-boot-lobby`
      didn't strip the new keys back out.
- [ ] **Migration test**: deploy to a node that already has old
      `-lobby.conf` files (i.e. the current fleet) via a normal software
      update with no re-provision, and confirm both the live conf and the
      lobby conf actually gained the new keys.
- [ ] Negative test for verify-after-apply: force a mismatch (e.g.
      reproduce tonight's bug, or set a frequency the radio won't honor),
      confirm the log line + `/var/run/` marker appear, and confirm no
      restart storm over a 10-minute window in static mode (`acs=n`,
      ticks 4x more often than ACS) — the rate-limit is the thing most
      likely to be under-tested since it only shows up in a sustained
      failure case.
- [ ] **EU-domain regression** (`regulatory_domain=EU`): confirm ACS does
      *not* elect 5745-5825 and does not enter a restart loop. Flagged as
      the test most likely to get skipped and most likely to actually
      break a real deployment if it is.
- [ ] Tourguide round trip with the new keys: lobby hop lands on the exact
      lobby primary, and a forced failed return-hop escalates to a full
      restart instead of leaving the radio stranded off-channel.
- [ ] The `DATA_CHANNEL_5_0`/AP-radio-conflation open question (see above),
      on a node confirmed to have a dual-band-capable AP radio.
- [ ] Whether the alfred-down failure mode (separately tracked) can be
      reproduced/caught live with logging now in place
      (`fix/alfred-service-action-logging`, in progress in a different
      session as of 2026-08-26), to finally learn whether ACS-breaking
      alfred outages come from the admin API or somewhere else.
- [ ] General tourguide/partition-merge behavior hasn't been re-verified
      since the hostapd race fix and the convergence fix — worth a real
      foreign-partition-merge test now that both of those are settled.
- [ ] Full `eud=` round-trip (wired→wireless→both→auto→none→wired) was
      last fully verified 2026-08-21, before this session's baseline
      redeploy — worth re-confirming on current `release-v0.1.3` code,
      especially since `eud=wired` was re-tested on EUD3 this session and
      the hostapd-disable fix held across reboot (partial re-verification,
      not the full 5-value round trip).
- [ ] **Cold-boot bias fix (`mesh_acs_last_channels`)** — **RE-OPENED
      2026-09-01: the "SUPERSEDED" verdict below was wrong for the
      simultaneous-cold-start case, confirmed live on EUD3+EUD4 (30+ min
      continuous deadlock, not simulated). See "Re-implementation —
      2026-09-01" above for the corrected implementation and why the
      argument below doesn't cover that case. Needs the full hardware
      test pass below, plus these new cases this re-implementation pass
      specifically targets:**
      - [x] **Simultaneous power-cycle — verified live 2026-09-01, EUD3+EUD4
        concurrent boot (PR #30, commit `5ce5cb5`).** Both booted within
        ~7 minutes of each other (effectively concurrent relative to the
        ~3min `acsCycleInterval`), both hit `electColdStart` immediately
        with no deadlock and no repeat of the old "holding forever"
        symptom. Initial picks briefly diverged on 2.4GHz (EUD4 → ch11
        from its own bias/scan, EUD3 → ch6 from its own) while 5GHz
        matched from cold start on both (ch149/5745MHz) — the divergent
        band self-corrected at EUD3's next periodic cycle (~2.5min later)
        once EUD4's vote gossiped in, confirmed by direct `iw dev` re-query
        showing both nodes on identical channels on both bands, and
        `batctl n`/`o` (via `sudo`) showing real batman-adv peering over
        both wlan0 and wlan1, not just interfaces up. EUD1/EUD2 (HaLow-only,
        no 2.4/5GHz mesh radio) don't exercise this path and were still on
        the pre-fix build at test time — not part of this verification,
        need a routine update before they can be.
      - [x] **Converged node loses a peer — verified live 2026-09-01,
        EUD4 while EUD3 was powered off (not just rebooted).** `batctl`
        showed EUD3 gone from the data plane almost immediately, but its
        registry vote stayed `ACTIVE` for the full `staleNodeThreshold`
        (600s) before aging out — a real ~10min lag between batman losing
        the peer and ACS's vote aging out, by design, not a bug. Once the
        vote actually aged out, both bands logged the plain `no peer
        votes yet (cold start or isolated) — holding current channel`
        line, never `electColdStart`'s scored-election line — correct,
        since EUD4's live channels (2462/5745) never equaled the lobby
        frequency. `iw dev` confirmed both radios held their exact
        channel throughout, zero `wpa_supplicant`/`node-manager`
        restarts. Also confirmed no quorum-driven lobby retreat occurred
        (EUD4 still had 2 other batman originators via HaLow, so
        `quorumOK`'s solo-isolation branch never triggered) — this
        exercised the cold-start-hold gate specifically, not the separate
        quorum-retreat path.
      - A bias-eligible channel that scores clearly worse (>4dB) than an
        alternative real candidate: confirm the alternative still wins —
        i.e. the nudge is bounded, not an override.
      - [x] **Corrupted persisted-state file — verified live 2026-09-01,
        EUD4.** Overwrote `/var/lib/mesh_acs_last_channels` with
        `LAST_FREQ_2_4=garbage`, `LAST_FREQ_5_0=` (empty),
        `TIMESTAMP=notanumber`. Next cycle logged each field individually
        (`persisted last-elected channels: LAST_FREQ_2_4="garbage"
        unparseable, ignoring`, same for the other two), election
        proceeded normally in the same cycle with no crash and no restart
        loop (`systemctl is-active` stayed `active` throughout). Restored
        to original content, confirmed byte-identical afterward.
      - [x] **Forced conf-write failure — verified live 2026-09-01, EUD4,
        two attempts.** First attempt (`chmod 444` on
        `wpa_supplicant-wlan1.conf`) was a false negative: `node-manager`
        runs as root with no `User=` directive, so root bypassed the
        permission bits entirely and the write silently succeeded —
        proved nothing. Retried with `chattr +i` (immutable attribute),
        which does block root; confirmed blocked via a direct `sudo`
        write attempt returning `Operation not permitted` before touching
        node-manager. With a real frequency mismatch forced into the conf
        (5745→5805) and the file immutable, three consecutive cycles
        logged `rewrite 5 GHz (ACS) frequency: write
        .../wpa_supplicant-wlan1.conf: open ...: operation not
        permitted` — the new error path fires, not swallowed.
        `/var/lib/mesh_acs_last_channels` stayed at `LAST_FREQ_5_0=5745`
        unchanged across all three failed-write cycles — it never
        recorded 5805, the frequency the radio never actually reached,
        which is the exact invariant this fix exists to hold. Live radio
        (`iw dev wlan1 info`) stayed at the real 5745 MHz throughout,
        since the disk write failed before any `wpa_supplicant` restart
        was attempted. `node-manager` had 0 restarts and stayed `active`
        the whole test. Fully restored (`chattr -i`, original content
        rewritten, md5sum-verified byte-identical) and confirmed clean on
        one more post-restore cycle.
      Historical context on the deletion/re-supersession this re-opens:
- [x] **Cold-boot bias fix (`mesh_acs_last_channels`), 2026-08-28 through
      2026-08-30 history** — **an external review of this doc (a
      different MANET fork's maintainer)**
      argued that persisting a last-known-good channel bias was fixing the
      symptom rather than the race: `electBand` now holds outright when
      `totalVotes == 0` instead of running any election
      (`channel_election.go`), removing this failure mode structurally
      rather than biasing it, and `acs_channel_persist.go`/
      `mesh_acs_last_channels` were deleted as no longer needed. The
      argument: `mesh-boot-lobby.service` already resets every node to the
      lobby frequency at boot, so a simultaneous mesh-wide power loss
      still converges (everyone meshes at the lobby, gossips, gets votes,
      elects together) without persisted state, and a truly isolated node
      has nothing to optimize for by biasing toward a remembered channel
      anyway.
      **Confirmed on real hardware, 2026-08-30, EUD3+EUD4 sequential
      reboot (same methodology as the original verification below):** the
      flagged risk materialized exactly as predicted, on both nodes.
      Neither recovered same-cycle — both waited for the *next*
      `acsCycleInterval` tick. EUD3 (rebooted first): reboot 17:17:23 BST
      → first tick 17:18:30 (`no peer votes yet — holding`) → second tick
      17:21:31 (`elected channel 5745, votes 1`) → plink to EUD4 up
      17:21:44 — **4m21s outage.** EUD4 (rebooted second): reboot 17:25:13
      → first tick 17:25:55 (hold) → second tick 17:29:15 (elected) →
      plink up 17:29:20 — **4m7s outage.** Both at or past the *original*
      bug's reported 3.5-4 minute window — this reproduced the outage the
      fix lineage exists to eliminate, not the same-cycle recovery below.
      Also directly observed: during EUD3's downtime, EUD4's own next tick
      logged the same "no peer votes yet — holding" for its *own*,
      already-correct channel (harmless — hold just re-echoes the current
      value — but confirms the two nodes have no third party to shorten
      convergence through).
      **Root cause and fix, same session:** the `acsCycleInterval` (180s)
      throttle in `runACSTick` exists to stop a *converged* mesh from
      rescanning too often — it was never meant to make a node with
      nothing elected yet wait a full cycle between attempts, but that's
      exactly what it did here. Fix: `electionResult.coldStart` (set only
      when the node is still sitting on the lobby frequency, never on an
      already-converged node with a momentary vote gap — see
      `channel_election.go`) tells `runACSTick` to rewind `lastACSCycle`
      to zero, so the very next 15s `loopInterval` tick retries the
      election immediately instead of waiting out the full interval.
      Tourguide is also skipped on a `coldStart` tick (`quorum &&
      !coldStart`), since dwelling at the lobby doesn't make a vote arrive
      faster and would otherwise risk yanking the node's *other*,
      already-working band to the lobby every ~15s — exactly the gossip
      path the cold-starting band is waiting on.
      **Fast-retry fix (`5d9a5ec`) hardware-verified 2026-08-30, same
      EUD3+EUD4 sequential-reboot methodology.** EUD3's first post-boot
      tick already found `votes 1` for both bands (EUD4's vote had
      already gossiped in) and elected immediately — outage (reboot →
      5GHz plink up) **~79.6s**, down from the regression build's 4m21s.
      EUD4 did exercise the hold path — `journalctl -u node-manager
      -o short-monotonic` shows holds at boot+26s and retries landing at
      boot+41.5s and boot+56.5s (~15s apart, matching `loopInterval`,
      confirming the `lastACSCycle` rewind fires on schedule instead of
      the old 180s wall) before electing both bands and re-establishing
      the plink — outage **~80.8s**, down from 4m7s. Neither figure lands
      in the ideal 15-45s band, but the remainder is legitimate boot time
      plus gossip-propagation latency plus sequential per-band
      wpa_supplicant reassociation, not the 180s throttle bug recurring —
      confirmed by the ~15s-spaced retry log lines above. Also confirmed:
      no tourguide/coldStart bleed onto the other, already-converged node
      in either direction (neither node's healthy band was disturbed
      during the other's downtime), and `iperf3` post-recovery held at
      437/435 Mbit/s, in line with the previously documented healthy
      range. If ~80s outages are still too slow for the target use case,
      the next lever is registry/alfred gossip propagation speed or how
      early node-manager starts its first ACS tick relative to boot — not
      this commit. The true-first-boot and EU-regulatory-domain cases the
      original fix also left untested remain untested here too.
- [x] **Cold-boot bias fix (`mesh_acs_last_channels`)** — **hardware-
      verified 2026-08-28** (historical — see superseded note above.)
      Deployed the fix and rebooted both EUD3 and
      EUD4 sequentially (EUD3 first — the node that broke originally —
      confirmed fully re-meshed before rebooting EUD4). Both nodes'
      first post-boot election was still a true cold start (`votes 0`,
      gossip hadn't caught up yet) but now correctly elected 5220 via the
      persisted bias instead of the original bug's wrong 5745 — mesh
      plink re-established in the *same* cycle on both reboots, not ~180s
      later. `/var/lib/mesh_acs_last_channels` survived EUD3's reboot
      byte-identical (`TIMESTAMP` unchanged). iperf3 post-reboot: 387/385
      Mbit/s (EUD3 reboot), 412/411 Mbit/s (EUD4 reboot) — consistent
      with the 380-444 Mbit/s range from the original deploy, no
      degradation. No `mesh_acs_divergence_*` fault ever appeared on
      either node. **Not exercised by this test** (fleet is 4 nodes, US
      regdomain, already running with existing persisted state): a true
      first-ever-boot-with-no-file node, static mode (`acs=n`), an
      EU-domain node with a stale/cross-domain persisted value, and the
      exactly-2-node-mesh vote-swap edge case noted in this doc's own
      "Follow-up correction" above (doesn't apply to this fleet's size).
- [~] **EU-domain phy-capability filtering** — tested 2026-08-28 on
      EUD4 (`iw reg set DE`/`GB`/`FR`/`NL`/`SE`, live, no reboot, no
      `mesh.conf` change — EUD3 left untouched as reference), and it
      found a real gap in a claim this doc has repeated since
      2026-08-26, not a code bug. **Marked partial, not passed** — see
      the dedicated writeup below.

## Finding, 2026-08-28: EU/ETSI does not disable UNII-3 at the per-frequency level this code reads — the safety claim needs correcting

This doc has stated, since the original "New risk found during
validation" paragraph (2026-08-26) through `activeBand5Channels`'s own
code comments (`scan.go`) and the `manet-architect`-validated self-heal
design, that 5745-5825 (UNII-3) is "illegal under ETSI" and would show
up as `(disabled)` in `iw phy info` under a European regulatory domain
— the entire premise `activeBand5Channels`/`freqAvailableOnPhy` were
built on. **Tested directly on real hardware for the first time
2026-08-28 (EUD4, `iw reg set DE`/`GB`/`FR`/`NL`/`SE`) and this premise
does not hold.**

**What actually happens**: under every ETSI country tested, all 8 of
`band5Channels`'s candidates (including all five UNII-3 entries)
remained listed as usable in `iw phy info`, just at reduced transmit
power (13dBm most ETSI countries, 23dBm GB, vs 30dBm under US) — never
flagged `(disabled)`, `(no IR)`, or `(radar detection)`. The `[acs]
5GHz candidates this cycle: [...]` log line, watched live during the
test, stayed the full 8-candidate list the entire time the node was
set to an ETSI domain. Real-world ETSI restricts UNII-3 to
**indoor-only use** (a `NO-OUTDOOR` rule) — this is a country-level
rule visible only in `iw reg get`'s per-country rule table, structurally
invisible to `iw phy info`'s per-frequency flag listing, which is the
only thing `phyUsableFreqs` (`scan.go`) parses. `noscan`/`disable_vht`
and everything else built and tested this week works correctly and
safely regardless of this finding — nothing malfunctioned, no restart
fired toward anything, the mesh was never disrupted. The gap is narrower
and more specific: **the phy-capability filter cannot see or enforce
the specific kind of EU restriction (indoor-only, not outright
prohibition) that actually applies to UNII-3**, so on a real EU
deployment this code would currently let outdoor mesh nodes elect and
transmit on UNII-3 channels that are restricted to indoor use under
real ETSI rules — a genuine compliance gap this project's own outdoor
MANET use case would trigger, not a hypothetical.

**Also confirmed, so the guard's underlying mechanism isn't a total
unknown**: the parsing itself is correct where a real per-frequency
`(disabled)` flag does exist — 5845/5865/5885 (outside `band5Channels`,
not real ACS candidates, but present in the phy's frequency list) *did*
correctly show `(disabled)` under DE/GB and would correctly be excluded
by this same code. The gap is specifically "ETSI's actual UNII-3
restriction shape doesn't map to any per-frequency flag `iw phy info`
exposes," not "the parser is broken."

**Not independently testable as a result**: the self-heal's
`freqAvailableOnPhy` legality guard (`acs_selfheal.go`) and the
cold-boot bias fix's cross-domain-ignore property both depend on the
same phy-capability primitive to ever see an illegal target in the
first place. Since no ETSI country in this hardware's actual regulatory
database disables any of `band5Channels`'s 8 candidates or either lobby
frequency, there is currently no real, black-box-reproducible path to
get `electBand` to elect an "illegal" target on this hardware at all —
with or without `iw reg set`. Both of those guards' code was already
read and confirmed structurally correct during earlier design/code
review (they correctly consult whatever `freqAvailableOnPhy` reports),
but the specific end-to-end "EU domain → illegal target elected → guard
blocks it" scenario remains unverified, and per this finding, may not
be reproducible until the underlying filter is extended.

**Decision needed, not made here**: either (a) treat this as an
accepted, documented limitation — HaLow/2.4GHz already exist as
lower-bandwidth EU-legal fallbacks, and if this fleet's real EU
deployments are expected to stay indoors or simply not use 5GHz
outdoors in the EU by policy rather than by code enforcement, this may
be an acceptable gap to leave open with clear documentation (this
section) rather than more code — or (b) extend `phyUsableFreqs`/
`activeBand5Channels` to also parse `iw reg get`'s per-country rule
table for `NO-OUTDOOR`(and any other rule-level, not frequency-level,
restrictions) and cross-reference it against the candidate list — real
additional work, not yet scoped. This is a compliance/policy call as
much as an engineering one — worth a decision from the user before
either path is taken, not something to resolve unilaterally given the
real-world regulatory stakes.

### Correction, same day — the root cause above was narrower than first framed

Pulled the actual `wireless-regdb` source (`db.txt`, the literal file
`iw`/cfg80211's regulatory database compiles from) for the countries
tested, rather than continuing to reason from the live `iw phy info`
output alone:

| Country | Rule for 5725-5875 MHz |
|---|---|
| DE, FR, NL, SE | `25 mW`, **no flags at all** — no `NO-OUTDOOR`, no restriction beyond the power cap |
| GB | `200 mW`, **`NO-OUTDOOR`** |
| US | `30 dBm (1000mW)`, `AUTO-BW`, no outdoor restriction |

**This changes the finding's scope, and the earlier framing above
overstated it.** Mainland EU member states (DE/FR/NL/SE — the ones
actually tested, and representative of ETSI-harmonized EU regulation
generally) do **not** restrict this band to indoor-only at all — they
simply cap transmit power far lower than the US (25mW/~14dBm vs
1000mW/30dBm), with no indoor/outdoor distinction whatsoever. The
`NO-OUTDOOR` restriction is specifically a **UK/Ofcom** rule — and the
UK is not in the EU, not bound by ETSI/CEPT harmonization, and sets its
own national rules independently.

**And the power cap itself is very likely already correctly enforced,
with no code change needed**: Linux's cfg80211 regulatory framework
computes the actual maximum transmit power for the current
channel+country and enforces it in the driver/hardware directly — this
is the whole purpose of the regulatory-domain mechanism, not an
informational-only field. `radio-setup.sh` already sets the correct
country code via `regulatory_domain` at boot. So as long as that's
configured correctly, the radio physically cannot exceed 25mW on this
band while set to a mainland-EU country, regardless of what
`activeBand5Channels`/`electBand` elect — the kernel itself is the
enforcement layer here, independent of and underneath anything in
`node-manager`.

**Revised bottom line**: there is likely no real compliance gap for
mainland EU deployments from this finding at all — the original
"outdoor MANET nodes could violate ETSI" framing above was based on an
assumption (broad EU indoor-only restriction) that this authoritative
source doesn't support. The one remaining edge worth naming explicitly:
a **UK-specific** deployment, where `NO-OUTDOOR` is real and does apply
— worth confirming directly whether `iw phy info`'s human-readable
output actually surfaces a `NO-OUTDOOR`-equivalent flag string the way
it does `(disabled)`/`(no IR)`/`(radar detection)` (not confirmed either
way — today's GB test noted the higher power ceiling but didn't
specifically check for or rule out an additional flag string on that
band's line). If this project has no near-term UK deployment plans,
this is now a much smaller, better-understood, arguably-not-worth-
building item than the original finding suggested — worth confirming
that's an acceptable read before spending more effort here.

## Implementation — 2026-08-26, fleet-wide toggle (`mesh_5ghz_bw`)

The plan above (`disable_ht40=1`+`disable_vht=1`, unconditional) shipped
with one refinement not in the original decision: a fleet-wide **mesh.conf
toggle**, `mesh_5ghz_bw` (values `20`/`80`, default `20` when the key is
absent), rather than a hard-coded always-on switch. Rationale: the
"Mixed-width peering" finding above already established this can never be
a *per-node* setting (any node left at 80MHz stays fully exposed to the
mismatch bug for all its 80MHz peers) — but a fleet can still legitimately
want a single, deliberate, all-nodes-at-once choice between the two widths
(e.g. rolling back to 80MHz once a real fix for the underlying
wpa_supplicant gap ships, without a re-provision). The default resolves to
`20` (safe/deterministic) whether the key is present-and-set-to-20 or
absent entirely — an existing node's unset key must not silently mean
"stay on legacy 80MHz."

**What actually shipped, file:line:**

- `MANET/rootfs/usr/local/bin/radio-setup.sh`:389-396 — reads
  `mesh_5ghz_bw` from `/etc/mesh.conf` (`grep`+`cut`, same pattern as
  `regulatory_domain`/`halow_regulatory_domain` immediately above it),
  defaults `MESH_5GHZ_BW` to `20`. The mesh network heredoc (originally
  ~1012-1028, now shifted by the added lines) builds a `WIDTH_LINES`
  string gated on `FREQ -ge 5000 && MESH_5GHZ_BW != "80"` and splices it
  into the `network={}` block before the closing brace — 2.4GHz and the
  AP interface's separate hostapd config path are untouched either way.
- `MANET/rootfs/usr/local/bin/manet-wlan-reconcile.sh`:62-67, ~330-345 —
  identical read (`grep '^mesh_5ghz_bw='`) and identical `WIDTH_LINES`
  gating in its own mesh lobby template heredoc, kept byte-consistent
  with radio-setup.sh's block per this doc's established convention.
- `MANET/src/node-manager/main.go` — new functions, all added after
  `wpaConfPath`:
  - `wpaLobbyConfPath(iface)` (:252) — path helper for the `-lobby.conf`
    companion to `wpaConfPath`.
  - `desiredMeshWidth()` (:269) — reads `mesh_5ghz_bw` via the existing
    `loadConf`, returns `"80"` only on an exact match, `"20"` otherwise
    (covers absent, empty, and any unrecognized value as safe-default).
  - `setMeshWidthKeys(path string, want20 bool) bool` (:281) — the
    two-way reconciler: walks the file's `network={}` block, adds
    `disable_ht40=1`/`disable_vht=1` before the closing brace if
    `want20` and they're missing, drops them if `!want20` and they're
    present. Returns whether it actually changed the file; a genuine
    no-op (not just "no restart") when already correct. Verified with a
    throwaway local test (add → idempotent-no-op → remove →
    idempotent-no-op → byte-exact round trip back to the original file)
    during implementation, not committed — this repo has no test suite
    convention to add it to.
  - `reconcile5GHzWidth(iface string)` (:355) — calls
    `setMeshWidthKeys` on both `wpaConfPath(iface)` and
    `wpaLobbyConfPath(iface)`; if either changed, restarts
    `wpa_supplicant@<iface>.service` once (guarded by the existing
    `radioIfaceEnabled` check, same as `setIfaceFrequency`). No
    rate-limiting/retry state — per the original plan's own reasoning,
    this only ever compares config text to a fixed target, there's no
    failure mode to back off from the way `setIfaceFrequency`'s ACS
    self-heal needs.
  - Wired into `main.go`'s `loop` closure (~line 53 area): `if _, iface5
    := meshIfaces(); iface5 != "" { reconcile5GHzWidth(iface5) }`, called
    unconditionally before the `acsEnabled` branch — both ACS and static
    mode get it, matching the plan ("it's about width, not about which
    channel ACS elects").
- `MANET/src/manet-ctrl/api.go` — `mesh_5ghz_bw` added to `saveableKeys`
  (~line 908) and `keyDescriptions` (~line 933). **No explicit
  `apiAdminSave` apply block added** — deliberate, and the one place this
  implementation deviates in shape from a literal reading of the task:
  surveying the existing apply blocks in `apiAdminSave` showed the
  pattern is "add an apply block only when something needs to happen
  *faster* than the relevant reconciler's own next pass" (e.g. `eud`
  triggers `manet-wlan-reconcile.sh` immediately because EUD mode
  changes are user-facing and visible instantly; keys like
  `admin_password`/`battery_monitor`/`require_auth` have no apply block
  at all and just ride on the next natural read). `mesh_5ghz_bw` fits the
  second category: `reconcile5GHzWidth` already runs every 15s
  regardless of ACS/static mode and does a cheap read-and-compare when
  nothing changed, so worst case a saved setting takes one loop tick
  (≤15s) to apply — not worth a special-cased immediate trigger.
- `MANET/rootfs/usr/local/bin/mesh` — `mesh_5ghz_bw` added to the `Config
  keys` help block, next to `halow_bw`.

**Deviations from the original sketch, noted per this doc's own
convention:**
- The task's suggested signature `reconcile5GHzWidth(iface, confPath)`
  wasn't used as-written — the shipped version takes just `iface` and
  derives both `wpaConfPath(iface)` and `wpaLobbyConfPath(iface)`
  internally, since the function always needs both paths together (that
  pairing is the entire point of the two-way, both-files reconcile) and
  passing one in while deriving the other from it would be redundant.
- The original plan's `ensureMeshConfDefaults(iface)` (one-way,
  insert-only migration) is superseded by `setMeshWidthKeys`/
  `reconcile5GHzWidth` (two-way) — the fleet-wide toggle requirement
  didn't exist when `ensureMeshConfDefaults` was scoped, and a one-way
  insert-only function can't support switching back to 80MHz. No
  `ensureMeshConfDefaults` function exists in the shipped code; treat
  every earlier reference to it in this doc as superseded by the above.
- No `/var/run/` marker file was added for a width mismatch, unlike the
  verify-after-apply design for `setIfaceFrequency` elsewhere in this
  doc — not needed here, since this reconcile has no "silently diverged
  and nobody notices" failure mode the way frequency drift did; it's a
  synchronous compare against mesh.conf, always eventually consistent
  within one loop tick.

**Verification status:** `go build ./...` and `go vet ./...` clean on
both `node-manager` and `manet-ctrl`; `gofmt -l` clean on the changed
node-manager files (this session found `manet-ctrl`'s `api.go` and
several sibling files already gofmt-non-canonical on `main` before this
change — pre-existing repo drift, not touched, out of scope);
`golangci-lint run` on both modules shows no new findings introduced by
this change (baseline errcheck/staticcheck findings pre-date it, confirmed
by line-number cross-check); `shellcheck` on all three touched shell
files (`radio-setup.sh`, `manet-wlan-reconcile.sh`, `mesh`) shows no new
findings (diffed against each file's pre-change shellcheck output — only
line-number shifts from added comments).

**2026-08-26, later same day — live hardware verification completed,
committed, and rolled out fleet-wide.** The implementer's live-check
couldn't reach the fleet; done directly in the follow-up session instead.
Found one real gap in the process along the way: the implementer's live
check would have needed to deploy **both** `node-manager` *and*
`manet-ctrl` — `mesh config set mesh_5ghz_bw 80` failed with `"No valid
keys"` after deploying only `node-manager`, because `saveableKeys`/
`keyDescriptions` live in `manet-ctrl`, which was still the old binary.
Cross-compiled and deployed both to EUD3, then it worked. Full round trip
confirmed on real hardware:

- [x] `mesh config set mesh_5ghz_bw 80`: both `wpa_supplicant-wlan1.conf`
      and `-lobby.conf` lost the `disable_` keys, `wpa_supplicant@wlan1`
      restarted once, `iw dev wlan1 info` confirmed `width: 80 MHz`.
- [x] `mesh config set mesh_5ghz_bw 20`: the reverse, both files gained
      the keys back, `iw dev wlan1 info` confirmed `width: 20 MHz`.
- [x] Default (key absent): confirmed independently on both EUD3 and
      EUD4 — `reconcile5GHzWidth` added the keys with zero explicit
      config, both nodes converged to `width: 20 MHz` automatically.

Committed as `8d28272` on `fix/acs-5ghz-channel-width` (5 code files +
this doc; the pre-existing unrelated `.gitignore` change on the working
tree was deliberately excluded from the commit).

**Then rolled out to the full fleet** (cross-compiled binaries + the
three shell scripts, deployed via the same atomic-swap SSH procedure used
throughout this session — not from an official release tarball, a
hand-rolled build from this branch):

- **EUD3, EUD4** (dual-band): both converged to the 20MHz default
  automatically, no manual `mesh config set` needed. Peering confirmed
  healthy post-rollout (`batctl o` showing an active wlan1 route at
  ~51 Mbit/s, consistent with the 144 Mbit/s real throughput measured
  earlier in this doc).
- **EUD1, EUD2** (HaLow + onboard WiFi, no 5GHz mesh radio — see
  `radio info` output: `wlan2` is HaLow/mesh, `wlan3` is the onboard
  brcmfmac radio in **AP role**, not mesh): user explicitly asked whether
  this change could interfere with their onboard WiFi. Verified directly,
  not just architecturally — captured `md5sum /etc/hostapd/hostapd.conf`
  before deployment on both nodes, redeployed, re-hashed: **byte-identical
  before and after on both nodes.** `node-manager`'s journal shows zero
  `[acs]` 5GHz-width log lines on either node post-deploy, confirming
  `reconcile5GHzWidth` correctly no-ops when `meshIfaces()` reports no
  5GHz interface. Onboard WiFi confirmed structurally unreachable by this
  code path, not merely assumed safe.

**Fleet state as of this rollout:** all 4 nodes running this branch's
code (ahead of `release-v0.1.3`, not yet cut as an official release) —
worth a proper release once this is considered final rather than leaving
it as a live-patched state.

## 2026-09-01 — `activeBand5Channels` phy-capability filter now fails open

Flagged by an independent third-party MANET fork reviewer on 2026-08-30
(see `acs_review_followups_2026-08-30` memory) and by a same-day
comparison against upstream (`very-srs/MANET`)'s equivalent
`phy_usable_freqs` in `node-manager-acs.sh`, which fails open by explicit
design ("filtering to an empty set is far worse than not filtering — it
would take a whole band off the air on every node at once"). Our
`activeBand5Channels` (`scan.go:265`) previously failed *closed* on a
transient `iw phy info` error — returning zero candidates for the cycle,
which excludes the entire 5GHz band from that tick's election on every
node hitting the same transient failure simultaneously. Fixed to match
upstream's fail-open behavior: on error, return the full unfiltered
`band5Channels` superset instead of `nil`. Downstream safety is unchanged
— `freqAvailableOnPhy` (`acs_selfheal.go`) still independently guards
against firing a corrective restart toward an actually-illegal frequency,
and `electBand`'s noise/vote scoring still disqualifies a genuinely bad
channel — this filter was only ever a fast-path optimization, not the
sole safety net. Build-verified (`go build`/`go vet` clean); no test
suite exists for this package to run. Not yet hardware-verified — the
failure mode this guards is a transient `iw` error, which is hard to
reproduce on demand.

## Related docs and memory

- [`wpa-supplicant-mesh-noscan.md`](wpa-supplicant-mesh-noscan.md) — the
  2026-08-27 finding that a real fix for the "Open issue" above exists,
  plus its build plan. Read this before redoing the "no fix exists"
  research in that section.
- `node-architecture.md` — general node architecture, doesn't currently
  cover ACS (this doc fills that gap).
- `VERSIONING.md` — unrelated to ACS, but referenced by the auto-update doc
  which shares this docs directory.
- Auto-memory (`/root/.claude/projects/-root-MANET/memory/`, not in this
  repo): `acs_port_in_progress`, `combined_test_branch`,
  `eud3_eud4_5ghz_primary_channel_mismatch`,
  `alfred_recurring_clean_stop_bug`, `tx_power_confirmation_unreliable` —
  session-level detail this doc summarizes but doesn't fully replace.
