# Hardware / Bill of Materials — Raspberry Pi 5 OpenWrt Router

This document specifies the hardware platform for the Pi 5 multi-WAN OpenWrt router,
the rationale behind each choice, the USB-Ethernet adapter requirements, and realistic
throughput expectations. Use it as the procurement reference (shopping list) and as the
ground truth for which OpenWrt target/drivers to build against.

---

## 1. Platform: Raspberry Pi 5 (4GB)

### Architecture — correct the ARMv7 misconception

> **The Raspberry Pi 5 is `aarch64` (ARMv8-A), NOT ARMv7.**

This is the single most common and most expensive mistake when building images for this
board. The Pi 5 uses the Broadcom **BCM2712** SoC: a quad-core **ARM Cortex-A76** running
at 2.4 GHz, which is a 64-bit **ARMv8-A** core.

- An ARMv7 / 32-bit image (e.g. `bcm2709`, `bcm2710`, or any `armv7` build) **will not
  boot** on a Pi 5.
- The correct OpenWrt target is **`bcm27xx` / `bcm2712`**, device profile id **`rpi-5`**.

### OpenWrt support is STABLE, not snapshot-only

Pi 5 support reached **stable** OpenWrt with **24.10.0** (released 2025-02-04). The
current stable series is **25.12.x** (25.12.4 latest at time of research). There is no
reason to run a snapshot build:

- Snapshots ship **without LuCI** preinstalled and carry **no stability guarantee**.
- Stable releases give you reproducible versions and the LuCI web UI out of the box.

**Recommended image:** current stable (25.12.x), **squashfs** flavor, target
`bcm27xx/bcm2712`, device `rpi-5`.

| Flavor       | Root           | Size    | Use it when                                            |
| ------------ | -------------- | ------- | ------------------------------------------------------ |
| **squashfs** | read-only + overlay | ~12 MB  | **Recommended.** Supports failsafe + factory-reset; ideal for an appliance. |
| ext4         | writable       | ~13.6 MB | Only if you specifically need a writable root.         |

Example filenames (25.12.4):

```
openwrt-25.12.4-bcm27xx-bcm2712-rpi-5-squashfs-factory.img.gz      # first flash
openwrt-25.12.4-bcm27xx-bcm2712-rpi-5-squashfs-sysupgrade.img.gz   # upgrades
```

Obtain images from the [OpenWrt Firmware Selector](https://firmware-selector.openwrt.org/?target=bcm27xx%2Fbcm2712&id=rpi-5)
or directly from `downloads.openwrt.org/releases/<ver>/targets/bcm27xx/bcm2712/`.
The Firmware Selector can **pre-bake packages** into a custom image — strongly recommended
here so the USB-Ethernet drivers are present on first boot (see §4 and the
[package list](#7-packages-to-pre-bake)).

### Flashing

Using Raspberry Pi Imager: choose **"Use custom"** and select the `.img.gz`. Or from a
shell (verify the target device first — this is destructive):

```sh
gzip -d openwrt-25.12.4-bcm27xx-bcm2712-rpi-5-squashfs-factory.img.gz
dd if=openwrt-25.12.4-bcm27xx-bcm2712-rpi-5-squashfs-factory.img of=/dev/<disk> bs=2M conv=fsync
```

---

## 2. Boot media: NVMe / USB-SSD strongly preferred over SD

An always-on router writes constantly: adblock blocklist refreshes (potentially 500K+
domain lists re-downloaded daily) and logging both cause **write churn** that wears out
SD cards over months. An ext4 image on an SD card is the worst case for endurance and the
most likely path to rootfs corruption.

| Option                       | Verdict        | Notes                                                                 |
| ---------------------------- | -------------- | --------------------------------------------------------------------- |
| **NVMe via PCIe Gen3 (M.2 HAT+)** | **Best**   | Dedicated PCIe lane, best endurance/reliability. Boot order can target NVMe. |
| **USB3 SSD**                 | **Good**       | Far better than SD for write endurance. Occupies a USB3 port (see §4 budget). |
| SD card (squashfs)           | Acceptable     | Workable but wears under list/log churn; squashfs (read-only root) is much safer than ext4. |
| SD card (ext4)               | Avoid          | Writable root + SD wear = corruption risk over time.                  |

4GB RAM is ample for mwan3 + adblock (even XL/XXL blocklists) + WireGuard simultaneously.

---

## 3. Power and cooling

### Power — under-powering causes phantom "WAN down" failures

The Pi 5 wants a **5V / 5A (27W) USB-C PD** supply (the official Raspberry Pi PSU).

> With a non-PD or 5V/3A supply, the Pi 5 caps **total peripheral current to ~600 mA**
> across all USB ports. That budget can **brown out bus-powered USB-Ethernet adapters**,
> causing intermittent link flaps that masquerade as `mwan3 marking WAN down` bugs.

Mitigations, in order of preference:

1. Use the **official 27W (5V/5A) PSU**.
2. If any USB NIC remains flaky, move **both adapters onto a powered USB3 hub** to take
   them off the Pi's peripheral current budget entirely.

If you investigate apparent mwan3 failover instability, **rule out power first** before
touching mwan3 timing config.

### Cooling

The Cortex-A76 at 2.4 GHz under sustained routing load benefits from active cooling. Use
the **official Active Cooler** (or a case with a fan). Routing gigabit NAT is not a heavy
CPU load (see §5), but a fanless Pi in a sealed case running 24/7 will throttle; an
inexpensive active cooler removes that variable.

### PoE / WiFi caveats

- **No onboard PoE** on the Pi 5 — PoE requires a HAT, which competes with the M.2 HAT+
  for PCIe/physical real estate. Plan accordingly if both are wanted.
- **No strong onboard WiFi AP.** The internal radio is weak for AP duty. For the optional
  WiFi AP (a secondary goal), plan a **separate dedicated AP device** rather than relying
  on the Pi 5's internal chip.

---

## 4. Network interfaces

### The single-onboard-NIC constraint → why 2 USB NICs are required

The Pi 5 has **exactly one onboard Ethernet port**. The router design needs **three**
wired interfaces:

- **WAN1** — uplink #1
- **WAN2** — uplink #2 (for mwan3 dual-WAN balancing + failover)
- **LAN** — downlink to the local switch

One onboard NIC + the need for three ports = **two USB-Ethernet adapters are mandatory**.
There is no way around this on a stock Pi 5 short of a multi-port PCIe NIC HAT (out of
scope for this build).

### Recommended port mapping

The Pi 5's **RP1 I/O controller** wires the onboard gigabit Ethernet over a **dedicated
lane** that is **NOT bandwidth-shared with the USB controller** (a real improvement over
the Pi 4, where GbE and USB3 shared one path). Exploit that:

| Role | Interface              | Port                  | Why                                                              |
| ---- | ---------------------- | --------------------- | ---------------------------------------------------------------- |
| WAN1 | **onboard GbE** (RP1)  | onboard               | Dedicated lane, best throughput — assign your **fastest uplink** here. |
| WAN2 | USB3 GbE adapter #1    | USB3 (blue)           | Gigabit-capable USB3 path.                                       |
| LAN  | USB3 GbE adapter #2    | USB3 (blue)           | Gigabit-capable USB3 path to the switch.                        |

> **Never put a gigabit interface on a USB2 (black) port.** USB2 caps at ~480 Mbps
> theoretical (~300 Mbps real-world). Both heavy NICs must be on the **USB3 (blue)** ports.

### USB3 bandwidth caveat (shared upstream)

The Pi 5's **two USB3 ports share a single ~5 Gbps upstream link** to the RP1 controller
(PCIe Gen2 x1). Consequences:

- Two USB3 NICs both saturated **contend** for that aggregate ~5 Gbps budget.
- This is **fine for two ~1 Gbps WANs** (well under 5 Gbps), but do **not** assume a full
  2 Gbps simultaneous USB throughput under worst case.
- A **USB3 SSD boot device** (§2) also consumes part of this budget if used — usually
  negligible vs. WAN traffic, but worth noting if you saturate everything at once.

### Adapter chipset recommendations and required kmod drivers

Buy by **chipset**, not by brand label. Verify the actual chipset with `lsusb` / `dmesg`
before buying in quantity — some "RTL8153" listings are counterfeit or are actually
**RTL8157 / 2.5G** variants that need a different (or absent) kmod.

| Chipset                | Speed         | OpenWrt kmod                       | Pulls / depends             | Verdict                          |
| ---------------------- | ------------- | ---------------------------------- | --------------------------- | -------------------------------- |
| **RTL8153** (USB3)     | Gigabit       | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | **Default pick.** Most broadly tested on OpenWrt/Pi. (UGREEN, Anker, TP-Link UE300, etc.) |
| RTL8152 (USB2)         | 100 Mbps      | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | Same kmod, but USB2/100M — not for a gigabit WAN. |
| **AX88179 / AX88178A** (USB3) | Gigabit | `kmod-usb-net-asix-ax88179`        | `kmod-libphy`, `kmod-usb-net` | **Solid second choice.**        |
| RTL8157 / generic 2.5G | 2.5 GbE       | (verify kmod exists first)         | varies                      | **Avoid** unless you confirm a working kmod. |

> The `kmod-usb-net-rtl8152` driver covers **both** RTL8152 (USB2 100M) and RTL8153 (USB3
> gigabit) — same module, two chips. Make sure you buy the **RTL8153** variant for gigabit.

### Pin interfaces by MAC — critical for mwan3

USB NIC **enumeration order is not guaranteed across reboots**: `eth1` and `eth2` can
swap. If that happens, mwan3 member-to-WAN mapping silently points at the wrong uplink,
breaking weighting and per-flow stickiness.

**Pin each interface by MAC address** in `/etc/config/network` (or via a hotplug rule) so
the member→WAN mapping stays stable. Example:

```
config device
	option name 'wan2'
	option macaddr 'aa:bb:cc:dd:ee:ff'   # MAC of USB3 adapter #1 — replace with real value

config device
	option name 'lan_usb'
	option macaddr 'aa:bb:cc:dd:ee:00'   # MAC of USB3 adapter #2 — replace with real value
```

---

## 5. Realistic routing throughput expectations

### CPU is not the bottleneck

The Cortex-A76 quad-core at 2.4 GHz routes/NATs **gigabit traffic comfortably** — it is
far stronger than a typical SOHO router CPU. For plain **NAT + mwan3**, expect to hit
**line-rate per gigabit uplink** with CPU headroom to spare.

### The real constraints

| Layer                          | Expectation                                                                 |
| ------------------------------ | --------------------------------------------------------------------------- |
| **NAT + mwan3**                | ~1 Gbps per WAN; CPU is a non-issue.                                         |
| **USB3 aggregate**             | Shared ~5 Gbps RP1 budget across both USB3 ports — fine for 2×1 GbE, not a guaranteed 2 Gbps simultaneous. |
| **mwan3 does NOT bond**        | A **single TCP download uses ONE WAN.** A "100+100" setup gives ~100 Mbps single-stream, **not 200**. Aggregate throughput across **many** flows can approach the sum. |
| **WireGuard (Surfshark) up**   | Encryption adds CPU cost and the tunnel **rides exactly one WAN at a time**. Expect a few hundred Mbps of encrypted throughput; the second WAN becomes **failover-only** for tunneled traffic. |

> **Honesty note (Goal 3):** When the WireGuard tunnel is up with `AllowedIPs 0.0.0.0/0`,
> tunneled traffic egresses **one WAN only**, so dual-WAN load balancing degrades to
> **failover** for that traffic. mwan3 still tracks and fails over the underlying WANs, and
> still balances any non-tunneled (split-tunnel) flows.

### mwan3 is per-flow, not per-packet

mwan3 distributes **connections** (flows), not packets, using connmark + conntrack. Every
packet of a given TCP/UDP flow exits the same WAN for the connection's lifetime — this is
**per-flow stickiness by design** and is exactly what this project wants. Per-packet
balancing would break NAT/TLS and is not how mwan3 works.

---

## 6. Concrete shopping list

| # | Item                                          | Spec / chipset                              | Required OpenWrt driver(s)               | Notes |
| - | --------------------------------------------- | ------------------------------------------- | ---------------------------------------- | ----- |
| 1 | Raspberry Pi 5                                | **4GB**, BCM2712, aarch64 Cortex-A76        | target `bcm27xx/bcm2712`, profile `rpi-5` | NOT ARMv7. |
| 2 | Official Raspberry Pi 27W PSU                 | **5V / 5A USB-C PD**                         | —                                        | Avoids the 600 mA peripheral cap that browns out USB NICs. |
| 3 | Active Cooler (or fan case)                   | Official Active Cooler                       | —                                        | Prevents throttling on a 24/7 box. |
| 4 | Boot storage — **NVMe** (preferred)           | M.2 NVMe SSD + **PCIe Gen3 M.2 HAT+**        | —                                        | Best endurance for list/log churn. |
| 4b| Boot storage — USB3 SSD (alternative)         | USB3 SSD                                     | (uses USB mass-storage, built-in)        | Good fallback; consumes a USB3 port. |
| 5 | **USB-Ethernet adapter #1 (WAN2)**            | **RTL8153** USB3 gigabit                     | `kmod-usb-net-rtl8152` (+ `r8152-firmware`) | Verify chipset via `lsusb`. |
| 6 | **USB-Ethernet adapter #2 (LAN)**             | **RTL8153** USB3 gigabit                     | `kmod-usb-net-rtl8152` (+ `r8152-firmware`) | Or AX88179 (`kmod-usb-net-asix-ax88179`). |
| 7 | Powered USB3 hub (contingency)                | Self-powered USB3                            | —                                        | Only if USB NICs flap on the Pi's own power. |
| 8 | LAN switch                                     | Gigabit unmanaged/managed                    | —                                        | Hangs off the LAN USB-Ethernet port. |
| 9 | (Optional) Dedicated WiFi AP                  | Separate AP device                           | —                                        | Internal Pi 5 radio is weak for AP duty. |

**Chipset alternative:** items 5/6 may be **AX88179/AX88178A** instead of RTL8153, using
`kmod-usb-net-asix-ax88179` (depends on `kmod-libphy` + `kmod-usb-net`). Pick one chipset
family and standardize.

---

## 7. Packages to pre-bake

Pre-select these in the Firmware Selector so the USB NICs come up on first boot and the
full stack is present:

```
# USB-Ethernet drivers (pick to match your adapters)
kmod-usb-net-rtl8152            # RTL8153 — pulls r8152-firmware
# kmod-usb-net-asix-ax88179     # alternative: AX88179/AX88178A

# Multi-WAN (Goal 1 / 1a)
mwan3
luci-app-mwan3
librespeed-cli                  # interface-bound capacity probe for the healthcheck
curl                            # fallback probe / liveness; OpenWrt ships uclient-fetch by default

# DNS filtering (Goal 2)
adblock
luci-app-adblock

# VPN (Goal 3)
wireguard-tools
kmod-wireguard
luci-app-wireguard
pbr                             # policy-based routing for split/on-demand VPN
luci-app-pbr
dnsmasq-full                    # required for domain-based VPN policies (replaces stock dnsmasq)

# Web UI
luci
```

> Note: on modern OpenWrt (fw4 / nftables), mwan3 is still connmark/iptables-based and
> pulls iptables-nft compatibility shims; the Firmware Selector resolves these
> dependencies automatically when you add `mwan3`. `dnsmasq-full` conflicts with the
> stock `dnsmasq` — the selector will swap it.

---

## 8. Hardware pitfalls checklist

- [ ] **Wrong arch:** Pulled an ARMv7/`bcm2709`/`bcm2710` image → won't boot. Use
      `bcm27xx/bcm2712` `rpi-5`.
- [ ] **Snapshot build:** No LuCI, no stability guarantee. Use stable 25.12.x.
- [ ] **Under-powered:** Non-PD/3A supply → 600 mA cap → USB NIC brownout → phantom mwan3
      "WAN down". Use the 27W PSU (or powered hub).
- [ ] **USB2 port for a gigabit WAN:** capped at ~300 Mbps real-world. Use blue USB3 ports.
- [ ] **NIC name swap on reboot:** pin USB adapters by MAC in `/etc/config/network`.
- [ ] **Counterfeit / 2.5G chipset:** verify with `lsusb`/`dmesg` before bulk buying; a
      fake RTL8153 or an RTL8157 2.5G may have no working kmod.
- [ ] **SD card wear:** prefer squashfs + NVMe/SSD; ext4-on-SD is the worst case.
- [ ] **Expecting bandwidth bonding:** mwan3 does not bond — single-stream rides one WAN.

---

## References

- Raspberry Pi 5 on OpenWrt: <https://openwrt.org/toh/raspberry_pi_foundation/raspberry_pi>
- Firmware Selector (bcm2712 / rpi-5): <https://firmware-selector.openwrt.org/?target=bcm27xx%2Fbcm2712&id=rpi-5>
- Release downloads: <https://downloads.openwrt.org/releases/>
- `kmod-usb-net-rtl8152`: <https://openwrt.org/packages/pkgdata/kmod-usb-net-rtl8152>
- `kmod-usb-net-asix-ax88179`: <https://openwrt.org/packages/pkgdata/kmod-usb-net-asix-ax88179>
- `librespeed-cli`: <https://openwrt.org/packages/pkgdata/librespeed-cli>
- mwan3: <https://openwrt.org/docs/guide-user/network/wan/multiwan/mwan3>
- Raspberry Pi hardware docs: <https://www.raspberrypi.com/documentation/computers/raspberry-pi.html>
