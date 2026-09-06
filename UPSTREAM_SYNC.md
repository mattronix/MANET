# Syncing with upstream (very-srs/MANET)

This repo (`mattronix/MANET`) is a real GitHub fork of `very-srs/MANET`,
created 2026-08-11. On that same day the repo went through two structural
rewrites, both git-tracked as renames:

1. `a783d48` — "Restructure node_tools into semantic dirs" —
   `MANET/node_tools/*` → `MANET/scripts/{core,elections,network,radio,system}/*`
2. `5fa0be6` — "Restructure repo to mirror target filesystem layout" —
   `MANET/scripts/*`, `MANET/etc/*`, `MANET/systemd*`, `MANET/udev/*`,
   `MANET/networkd-dispatcher/*`, `MANET/root/*` → `MANET/rootfs/{etc,root,usr}/...`

Separately (not a rename — a rewrite, so git can't bridge it), several
Python/bash components were replaced with compiled Go services under
`MANET/src/`: the web server (`manet-ctrl`), `node-manager`, `node-update`,
`battery-reader`, `gps-reader`, `halow-mcs-summary`, `mesh-radio-state`,
`mesh-registry`, `gateway-manager`, `cot-emitter`, `mesh-voice`. Upstream
still has these as `.py`/`.sh` under `node_tools/` (upstream's voice PTT
daemon is `node_tools/mesh-voice.py`, Python/GStreamer/Lyra — architecturally
unrelated to the fork's compiled Go `mesh-voice`, so upstream voice fixes
need a manual read-and-translate, not a cherry-pick).

Because of this, `origin/main` and `upstream/main` can't be diffed directly
— paths (and for the Go pieces, languages) differ. But since both restructure
commits are proper git renames, **`git log --follow` on a current fork path
still bridges all the way back into the shared upstream history** for
anything that's still a script/config file.

## Checking a specific file

```
./check-upstream-sync.sh MANET/rootfs/usr/local/bin/radio-setup.sh
```

Walks the fork's rename history for that path, finds the newest hop that
still exists on `upstream/main`, and lists upstream commits on that path
since the fork date. Two outcomes:

- **Finds a matching upstream path** → still comparable. Read the listed
  commits, `git show <sha>` to see what changed, `git cherry-pick <sha>`
  to try applying it directly (expect normal merge conflicts where both
  sides evolved the file independently — resolve like any git conflict).
- **No historical path exists on upstream** → either introduced fork-side
  after 2026-08-11, or it's one of the Go rewrites above. No mechanical
  comparison possible — instead, manually check upstream's activity on the
  *old* path it replaced (e.g. `git log upstream/main -- MANET/node_tools/node-manager.sh`
  for `MANET/src/node-manager/`) and port relevant fixes by hand.

## Setup (once per clone)

```
git remote add upstream https://github.com/very-srs/MANET.git
git fetch upstream
```

## Last reviewed

Upstream commit reviewed up to: `0f9280e` (2026-09-06) — "wording edits"

### 2026-08-21 pass (up to `695ca46`)

Found one actionable, mechanically-portable fix not yet in the fork:
`70dc3c6` "Fix mesh time sync and wire GPS in as a time source" fixed real
bugs also present in the fork's `provision-mesh.sh` / `ethernet-autodetect.sh`
/ `one-shot-time-sync.sh` / `radio-setup.sh` — invalid `offline` directive in
chrony-default.conf, chrony-default.conf written to the wrong path
(`/etc/chrony-default.conf` vs `/etc/chrony/chrony-default.conf`),
`one-shot-time-sync.service` never actually enabled, and
`one-shot-time-sync.sh` bursting a source chronyd was never told about.
Ported (same day, this pass). Deliberately NOT ported: the upstream commit's
GPS-as-explicit-NTP-source marker (`/var/run/mesh-ntp-gps.state`, set from
`node_tools/node-manager.sh`) — the fork's registry publisher
(`src/mesh-registry/main.go`, `serviceActive("chrony")`) works on a
different principle than upstream's state-file marker, so a GPS marker
wouldn't plug in the same way; making a GPS-disciplined node a
preferred/discoverable mesh time source needs its own design pass, not a
port. See the corrected comment above `radio-setup.sh`'s GPS/chrony section
for the current, accurate state.

Everything else in that range (the voice PTT/Lyra feature set — fork has its
own separate Go `mesh-voice`, not upstream's Python one; journald
persistence — already fixed independently in the fork's own
`journald.conf.d/manet.conf`) needed no action.

### 2026-09-01 pass (covers `695ca46..515a3b1`, ~50 commits, plus a
re-check of everything reviewed before)

Found one **live bug in the fork itself**, same class as upstream's
`b0ad1a3` "Fix the auto_update gate that no node has ever satisfied":
`auto_update` is written as `y`/`n` everywhere (`admin.go`, all three
flash-time provisioning scripts, `firstrun.sh.template`), and the Go
`node-update` service's own gate (`isAffirmative`) correctly treats `y` as
on. But three other places still test the dead condition
`grep -qi '^auto_update=1' /etc/mesh.conf`, which no writer has ever
produced:
- `MANET/rootfs/etc/networkd-dispatcher/carrier:24`
- `MANET/packaging/build-rpi5-tarball.sh:144`
- `MANET/packaging/build-x86-tarball.sh:163`

Effect: the carrier-triggered "update immediately when ethernet/internet
comes up" path has never fired on any node. Not a total auto-update
failure (the routine timer-based check still runs `node-update` normally),
just this fast-path trigger. **Fixed 2026-09-01** on branch
`fix/auto-update-carrier-hook-gate` — all three files now use
`grep -qiE '^auto_update=(y|yes|1|true)[[:space:]]*$'`.

Everything else checked in this range turned out to already be independently
covered by the fork's own Go rewrite or prior ports, not because it was
mechanically applied from these specific commits:
- `f755e26` web UI localhost/DHCP firewall — already in via the fork's own
  `e5f4442`.
- `403c968` unauthenticated radio endpoints — N/A, `mesh-status.py` doesn't
  exist here; `manet-ctrl`'s `/api/control/*` routes are already behind
  `requireAuth`.
- `9824519` EU cfg80211 regdom / fatal `sae_anti_clogging_threshold` —
  `radio-setup.sh` already has the fixed logic.
- ACS election/scan-data/solo-node fixes (`fda4e21`, `0d1a31a`, `4080ac0`) —
  the Go `node-manager` already hard-errors on empty/absent scan data and
  handles solo-isolation quorum.
- `batctl o` mean-throughput column-shift parsing (`67ee2dc`, `d234974`) —
  `mesh-registry/main.go`'s `getTQAverage` already parses it correctly.
- Gateway detection on ICMP-filtered uplinks + stale route cleanup
  (`3b467f4`) — `gateway-manager` already tolerates ICMP-only flapping and
  withdraws stale default routes.
- HaLow USB recovery udev rule (`6a483c8`) — byte-identical rule and
  companion script already present.

**Needs a closer look, not cleared either way:** the ~15-commit voice
PTT/Lyra block (`b81dc2a`..`9a3e2bc`). The fork's `mesh-voice` is an
independent Go implementation, but it has zero references to "Lyra" —
codec choice differs or the codec swap was never carried over. Whether
talker-mixing / muted-presence-beacon behavior matches wasn't verified.
Do a feature-by-feature comparison if voice quality/behavior becomes a
priority.

**Structural, not directly portable** (old `node_tools/` python/bash surface
now Go — architecture differs enough that these need a manual read-and-port
if a matching symptom shows up, not a blind cherry-pick): alfred
identity/telemetry payload split (`c601555`), the `/manage`-prefixed
config apply/ACK/rollback pipeline (`323b922`, `3f80ed0`, `2f2fb2f`),
provisioning-completion tracking (`809b658`, `9fea978`), OTA-reverts-
ACS-node-to-static protection (`3355b40`, `c1d35a2`), USB-WiFi-as-uplink
prep (`ec5346c`, not reviewed in depth).

**Skipped as low value:** Windows/rpi-imager fixes, the removed RPi5
release workflow (RPi5 is a later-stage target per project priorities),
decorative UI tweaks, and ~12 version-bump-only commits.

### 2026-09-06 pass (covers `515a3b1..0f9280e`, ~40 commits)

Found one **real hardware bug, fixed this pass**: `27b0298` "Put USB back on
the DWC2 host controller instead of the 2711 XHCI" (later corrected for a
section-detection bug by `a1c1f59`). The stock CM4 image runs USB off the
2711 built-in XHCI controller (`otg_mode=1` in `[cm4]`), which upstream found
does not provide a working USB host controller on their hardware — no root
hub, so a USB MM81xx HaLow card never enumerates, so no mesh. The fix
comments out `otg_mode=1` and inserts `dtoverlay=dwc2,dr_mode=host` in the
same `[cm4]` section (section-aware, since the stock config also carries an
identical dwc2 line under `[cm5]` that a file-wide grep would false-match).

This directly applies to the fork: `firstrun.sh.template` already detects a
USB MM81xx HaLow card via sysfs (`_halow_on_spi=0` path,
`MANET/provisioning/firstrun.sh.template:283-300`) and skips the SPI-hat
`config.txt` plumbing for it — but never had *any* CM4 DWC2/otg_mode
handling, on either side of the fork's 2026-08-11 split. `27b0298` landed
2026-08-30, inside the range the 2026-09-01 pass already covered, but wasn't
individually caught — it fell into that pass's "Windows/rpi-imager, skipped
as low value" bucket by association with the surrounding flasher-rewrite
commits, when it's actually a `firstrun.sh.template` provisioning fix
unrelated to the flasher GUI. **Ported 2026-09-06** (final, section-aware
form) directly onto `MANET/provisioning/firstrun.sh.template`, right after
the existing `[cm4]` `pcie-32bit-dma` block. Not yet hardware-verified on a
CM4 + USB MM81xx board — our fleet's HaLow boards have all been SPI/Seeed-HAT
so far (see `[[halow_spi_clock_speed_fix]]`), so this path has had no live
coverage either upstream's way or ours.

Also checked, no action needed:
- `06e462b` "Document how a node discovers claimed IP chunks via Alfred" —
  docs-only on upstream's old `node_tools/README.md`, describing the
  claimed-chunk allocation invariants (random pick, 300s stale timeout,
  persisted state overrides live registry data to avoid false-conflict
  churn). Cross-checked against the fork's Go `mesh-manager`
  (`MANET/src/mesh-manager/main.go`, `ipManager.run()` ~line 453-535): the Go
  implementation already matches this invariant exactly — a valid persisted
  chunk (`im.pValid`) is reasserted directly without consulting
  `claimed_chunks.txt` (`usePersistent` gate at line 503). Confirms the
  design is correct; no gap.
- `92ae222`/`45faf87`/`bfcbb34`/`8d60d0b` (openvlm/Lyra voice pipeline
  work, ~5 commits) — Python/GStreamer/Lyra, architecturally unrelated to
  the fork's own Go `mesh-voice` (Opus-based). `45faf87` is a real bug
  pattern worth knowing about even though it's not portable code: adapted
  loss-recovery state (frames-per-packet) was being silently reset to the
  configured default whenever the GStreamer pipeline rebuilt on a SIGHUP
  retune, and cumulative loss counters weren't reset alongside it, producing
  a bogus negative delta right after retune. Worth checking for an analogous
  "adaptive state clobbered by pipeline/session rebuild" bug in the fork's
  Go mesh-voice *if* a feature-by-feature comparison is ever prioritized
  (still an open item from the 2026-09-01 pass, not resolved this pass
  either).
- `build-cm4-tarball.sh` / `build-r3a-tarball.sh` / `build-rpi5-tarball.sh` —
  one-line `openvlm` binary packaging additions, N/A (same Lyra-voice
  architecture boundary as above).

**New idea, not implemented — needs a product decision, not a port:** the
Windows flasher gained an "additional scripts" feature (new
`provisioning/additional-scripts/` dir + GUI checks: syntax-check scripts
before running them, measure real script size instead of trusting the
directory entry, auto-correct CRLF-mangled Windows-authored scripts instead
of rejecting them, size list columns to content). The fork has no equivalent
— nothing under `MANET/provisioning/` runs arbitrary user-supplied setup
scripts during provisioning today. This is a genuinely new capability
(let an installer bundle custom node setup steps), not a bugfix, so it
wasn't ported; flag if there's a use case for user-extensible provisioning.

**Skipped as low value, consistent with prior passes:** the rest of the
Windows GUI flasher rewrite (~30 commits: DPI scaling, rpiboot visibility,
progress bars, quoting fixes, build-stamp display, settings-page defaults) —
the fork's own `windows.ps1`/`linux.sh`/`mac.sh` are independent
implementations, not upstream's rpi-imager-wrapper GUI. US-spelling and
version-bump-only commits.

Update this line after each review pass so `git log upstream/main --oneline
<last-reviewed-sha>..upstream/main` shows only what's new.
