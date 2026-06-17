# Hardware / Bill of Materials — Raspberry Pi 5 OpenWrt Router

This document specifies the hardware platform for the Pi 5 multi-WAN OpenWrt router,
the rationale behind each choice, the USB-Ethernet adapter requirements, and realistic
throughput expectations. Use it as the procurement reference (shopping list) and as the
ground truth for which OpenWrt target/drivers to build against.

---

## 1. Platform: Raspberry Pi 5 (4GB)

### Architecture and OpenWrt target

> **The Raspberry Pi 5 is 64-bit `aarch64` (ARMv8-A).**

The Pi 5 uses the Broadcom **BCM2712** SoC: a quad-core **ARM Cortex-A76** running at
2.4 GHz, which is a 64-bit **ARMv8-A** core.

- The correct OpenWrt target is **`bcm27xx` / `bcm2712`**, device profile id **`rpi-5`**.
- Build only `aarch64` images for this target. An image built for a different Raspberry Pi
  target (e.g. a 32-bit `bcm2709`/`bcm2710` build) **will not boot** on a Pi 5.

### OpenWrt support is STABLE, not snapshot-only

Pi 5 support reached **stable** OpenWrt with the **24.10** series (24.10.0 released
2025-02-06). Always build against the **current stable release** — check the live source
for the exact version, as point releases ship regularly (verify against
<https://downloads.openwrt.org/releases/>). There is no reason to run a snapshot build:

- Snapshots ship **without LuCI** preinstalled and carry **no stability guarantee**.
- Stable releases give you reproducible versions and the LuCI web UI out of the box.

**Recommended image:** current stable, **squashfs** flavor, target
`bcm27xx/bcm2712`, device `rpi-5`.

| Flavor       | Root           | Size    | Use it when                                            |
| ------------ | -------------- | ------- | ------------------------------------------------------ |
| **squashfs** | read-only + overlay | (verify) | **Recommended.** Supports failsafe + factory-reset; ideal for an appliance. |
| ext4         | writable       | (verify) | Only if you specifically need a writable root.         |

Example filenames (substitute the current stable `<ver>`, e.g. `24.10.x`):

```
openwrt-<ver>-bcm27xx-bcm2712-rpi-5-squashfs-factory.img.gz      # first flash
openwrt-<ver>-bcm27xx-bcm2712-rpi-5-squashfs-sysupgrade.img.gz   # upgrades
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
gzip -d openwrt-<ver>-bcm27xx-bcm2712-rpi-5-squashfs-factory.img.gz
dd if=openwrt-<ver>-bcm27xx-bcm2712-rpi-5-squashfs-factory.img of=/dev/<disk> bs=2M conv=fsync
```

---

## 2. Boot media: NVMe / USB-SSD strongly preferred over SD

An always-on router writes constantly: adblock blocklist refreshes (potentially 500K+
domain lists re-downloaded daily) and logging both cause **write churn** that wears out
SD cards over months. An ext4 image on an SD card is the worst case for endurance and the
most likely path to rootfs corruption.

| Option                       | Verdict        | Notes                                                                 |
| ---------------------------- | -------------- | --------------------------------------------------------------------- |
| **NVMe via M.2 HAT+ (PCIe)**  | **Best**   | Dedicated PCIe lane direct from BCM2712 (separate from RP1/USB). Certified at **PCIe 2.0 x1**; Gen3 speeds work via the unofficial `dtparam=pciex1_gen=3` override (not certified). Best endurance/reliability; boot order can target NVMe. |
| **USB3 SSD**                 | **Good**       | Far better than SD for write endurance. Occupies a USB3 port (see §4 budget). |
| SD card (squashfs)           | Acceptable     | Workable but wears under list/log churn; squashfs (read-only root) is much safer than ext4. |
| SD card (ext4)               | Avoid          | Writable root + SD wear = corruption risk over time.                  |

4GB RAM is ample for mwan3 + adblock (even XL/XXL blocklists) + WireGuard simultaneously.

---

## 3. Power and cooling

### Power — under-powering causes phantom "WAN down" failures

The Pi 5 wants a **5V / 5A (27W) USB-C PD** supply (the official Raspberry Pi PSU).

> With a 5V/3A (15W, non-PD) supply, the Pi 5 caps **total USB peripheral current to
> 600 mA** (official figure). With the official 27W (5V/5A) PD supply, the firmware raises
> the peripheral budget substantially (commonly cited as ~1.6 A, but Raspberry Pi does not
> publish an exact figure — verify against the live source). The 600 mA cap can **brown out
> bus-powered USB-Ethernet adapters**, causing intermittent link flaps that masquerade as
> `mwan3 marking WAN down` bugs.

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

- **No onboard PoE** on the Pi 5 — PoE requires a HAT. A PoE+ HAT uses the dedicated 4-pin
  PoE header and GPIO (not the PCIe lane), so it does not electrically conflict with an M.2
  HAT+, but the two compete for **physical/vertical stacking space** and airflow. Plan the
  mechanical layout if both are wanted.
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

The Pi 5's onboard gigabit Ethernet is driven by a **dedicated MAC** inside the RP1 I/O
controller (a Cadence GEM over RGMII), **separate from the two USB3 xHCI controllers**. It
is therefore independent of the USB ports — it does not contend with USB traffic at the
controller level. (Note: GbE, both USB controllers, MIPI, etc. all share RP1's single
PCIe 2.0 x4 uplink to the BCM2712 SoC, but at ~16 Gbps that link has ample headroom for
1 GbE + 2×5 Gbps USB; this is not a Pi 4–style contention point.) Exploit the separate
controllers:

| Role | Interface              | Port                  | Why                                                              |
| ---- | ---------------------- | --------------------- | ---------------------------------------------------------------- |
| WAN1 | **onboard GbE**        | onboard               | Dedicated RP1 MAC, off the USB controllers — assign your **fastest uplink** here. |
| WAN2 | USB3 GbE adapter #1    | USB3 (blue)           | Gigabit-capable USB3 path.                                       |
| LAN  | USB3 GbE adapter #2    | USB3 (blue)           | Gigabit-capable USB3 path to the switch.                        |

> **Never put a gigabit interface on a USB2 (black) port.** USB2 caps at ~480 Mbps
> theoretical (~300 Mbps real-world). Both heavy NICs must be on the **USB3 (blue)** ports.

### USB3 bandwidth (improved over Pi 4)

The Pi 5 has **two independent USB3 xHCI controllers** in RP1, each rated for **5 Gbps
simultaneously** (the RP1 datasheet states the two controllers together support "more than
10 Gbps of downstream USB traffic"). This is a real improvement over the Pi 4, where both
USB3 ports shared a single 5 Gbps path. Consequences:

- Each USB3 port has its own 5 Gbps; two USB3 GbE NICs at ~1 Gbps each do **not** contend
  at the USB-controller level. Two ~1 Gbps WANs are comfortably within budget.
- The shared resource is RP1's **PCIe 2.0 x4 uplink to the SoC** (~16 Gbps raw), carrying
  GbE + both USB controllers + camera/display. At gigabit NIC speeds this uplink is far
  from saturated; it only matters if you simultaneously max NVMe, both USB3 ports, and GbE.
- A **USB3 SSD boot device** (§2) shares only with the *other* USB3 controller's port via
  that uplink — negligible vs. WAN traffic at gigabit speeds.

### Adapter chipset recommendations and required kmod drivers

Buy by **chipset**, not by brand label. Verify the actual chipset with `lsusb` / `dmesg`
before buying in quantity — some "RTL8153" listings are counterfeit or are actually a
**different speed-class chip** (e.g. an RTL8156 2.5GbE or RTL8157 5GbE part) that you did
not intend to buy.

| Chipset                | Speed         | OpenWrt kmod                       | Pulls / depends             | Verdict                          |
| ---------------------- | ------------- | ---------------------------------- | --------------------------- | -------------------------------- |
| **RTL8153** (USB3)     | Gigabit       | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | **Default pick.** Most broadly tested on OpenWrt/Pi. (UGREEN, Anker, TP-Link UE300, etc.) |
| RTL8152 (USB2)         | 100 Mbps      | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | Same kmod, but USB2/100M — not for a gigabit WAN. |
| **AX88179 / AX88178A** (USB3) | Gigabit | `kmod-usb-net-asix-ax88179`        | `kmod-libphy`, `kmod-usb-net` | **Solid second choice.** (OpenWrt page lists AX88179 explicitly; AX88178A is covered by the same kernel driver.) |
| RTL8156 (USB3)         | 2.5 GbE       | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | Works (same r8152 driver), but overkill — both WAN/LAN here are 1 GbE. |
| RTL8157 (USB3.2)       | 5 GbE         | `kmod-usb-net-rtl8152`             | pulls `r8152-firmware`      | **Not a 2.5G part** — this is the 5GbE chip. Unnecessary for this build. |

> The `kmod-usb-net-rtl8152` driver (kernel `r8152`) covers the whole Realtek USB family:
> **RTL8152** (USB2 100M), **RTL8153** (USB3 gigabit), and the newer **RTL8156** (2.5G) and
> **RTL8157** (5G) parts — one module, same `r8152-firmware` dependency. (The OpenWrt/kernel
> Kconfig text only advertises 8152/8153, but the driver code includes `r8156_*`/`r8157_*`
> support.) For this gigabit build, make sure you buy the **RTL8153** variant.

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
| **USB3 ports**                 | Two independent 5 Gbps USB3 controllers (not a shared 5 Gbps path like the Pi 4); 2×1 GbE NICs fit comfortably. RP1's PCIe 2.0 x4 SoC uplink (~16 Gbps) is the only shared resource and is far from saturated at gigabit speeds. |
| **mwan3 does NOT bond**        | A **single TCP download uses ONE WAN.** A "100+100" setup gives ~100 Mbps single-stream, **not 200**. Aggregate throughput across **many** flows can approach the sum. |
| **WireGuard (Surfshark) up**   | Encryption adds CPU cost and the tunnel **rides exactly one WAN at a time**. Expect a few hundred Mbps of encrypted throughput (WireGuard uses ChaCha20-Poly1305, not AES; a single flow is core-bound on one A76 core — verify on your hardware); the second WAN becomes **failover-only** for tunneled traffic. |

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
| 1 | Raspberry Pi 5                                | **4GB**, BCM2712, aarch64 Cortex-A76        | target `bcm27xx/bcm2712`, profile `rpi-5` | 64-bit aarch64. |
| 2 | Official Raspberry Pi 27W PSU                 | **5V / 5A USB-C PD**                         | —                                        | Avoids the 600 mA peripheral cap that browns out USB NICs. |
| 3 | Active Cooler (or fan case)                   | Official Active Cooler                       | —                                        | Prevents throttling on a 24/7 box. |
| 4 | Boot storage — **NVMe** (preferred)           | M.2 NVMe SSD + **M.2 HAT+** (PCIe 2.0 x1; Gen3 via override) | —                          | Best endurance for list/log churn. |
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

- [ ] **Wrong target:** Pulled an image for a different Raspberry Pi target (e.g. a 32-bit
      `bcm2709`/`bcm2710` build) → won't boot. Use `bcm27xx/bcm2712` `rpi-5` (aarch64).
- [ ] **Snapshot build:** No LuCI, no stability guarantee. Use the current stable release
      (24.10 series or later — verify the exact version against the live source; see §1).
- [ ] **Under-powered:** Non-PD/3A supply → 600 mA cap → USB NIC brownout → phantom mwan3
      "WAN down". Use the 27W PSU (or powered hub).
- [ ] **USB2 port for a gigabit WAN:** capped at ~300 Mbps real-world. Use blue USB3 ports.
- [ ] **NIC name swap on reboot:** pin USB adapters by MAC in `/etc/config/network`.
- [ ] **Wrong-chip / wrong-speed adapter:** verify with `lsusb`/`dmesg` before bulk buying;
      a listing sold as "RTL8153" may actually ship an RTL8156 (2.5G) or RTL8157 (5G) part.
      All are driven by `kmod-usb-net-rtl8152`, but you want the gigabit RTL8153 for this build.
- [ ] **SD card wear:** prefer squashfs + NVMe/SSD; ext4-on-SD is the worst case.
- [ ] **Expecting bandwidth bonding:** mwan3 does not bond — single-stream rides one WAN.
- [ ] **VPN expected to load-balance:** with `AllowedIPs 0.0.0.0/0`, tunneled traffic rides
      one WAN; dual-WAN degrades to **failover** for that traffic (see §5 Honesty note).

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
