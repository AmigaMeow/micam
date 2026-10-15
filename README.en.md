# MiCam — Smart Home Surveillance Bridge for fnOS (Unofficial Community Edition)

<p align="center">
  <img src="platform/fnos/app/ui/images/icon_256.png" width="108" height="108" alt="MiCam Logo" />
</p>

<p align="center">
  <strong>High-performance local surveillance, 24/7 private continuous recording, and AI security suite for fnOS NAS</strong><br>
  <em>Unofficial Community Port · Zero Proprietary Binaries Bundled · 100% On-Premises Privacy · Fully Free & Open Source</em>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-AGPLv3-blue.svg" alt="License: AGPLv3"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.21+-00ADD8.svg" alt="Go 1.21+"></a>
  <a href="https://www.fnnas.com"><img src="https://img.shields.io/badge/fnOS-1.2+-green.svg" alt="fnOS Support"></a>
  <a href="DISCLAIMER.md"><img src="https://img.shields.io/badge/Legal-Disclaimer-orange.svg" alt="Legal Disclaimer"></a>
  <a href="README.md"><img src="https://img.shields.io/badge/Lang-中文-lightgrey.svg" alt="Chinese Documentation"></a>
</p>

---

> [!IMPORTANT]
> ### ⚖️ Legal Compliance Notice & Non-Affiliation
> 1. **Independent Project**: MiCam is an independent, non-commercial open-source tool developed by community volunteers. It is **not affiliated with, endorsed by, sponsored by, or associated with Xiaomi Inc. or any of its subsidiaries in any way**.
> 2. **Trademarks**: All trademarks, brand names, and model designations (such as "Xiaomi", "Mijia", "MI", CW500, CW300, etc.) are the property of their respective owners. They are referenced herein strictly for technical and hardware compatibility descriptions.
> 3. **Zero-Binary Policy (BYOB)**: This repository and official `.fpk` packages contain **no proprietary closed-source binaries** (such as `libmiot_camera_lite.so`). Dynamic runtime dependencies are retrieved directly on the user's NAS from Xiaomi's public open-source repository ([XiaoMi/xiaomi-miloco](https://github.com/XiaoMi/xiaomi-miloco)), or supplied manually offline by the user.
> 4. **Fair Use & Reverse Engineering for Interoperability**: This project is strictly non-commercial, free, and open source, complying with statutory reverse engineering exemptions for software interoperability under PRC Regulations on Computer Software Protection Art. 17, US DMCA § 1201(f), and EU Directive 2009/24/EC Art. 6. See **[DISCLAIMER.md](DISCLAIMER.md)** for details.

---

## 📸 Key Features

- **2K Low-Latency Live Stream**: Direct peer-to-peer connection via Go-based cs2 protocol, with ~300ms latency. Supports HLS (2560x1440) and MJPEG streams.
- **Dual-Lens Multi-Screen View**: Seamlessly displays both wide-angle and telephoto lenses on cameras such as the CW500 Dual.
- **24/7 Continuous Recording**: Direct stream capture (`-c copy`) with less than 2% CPU usage and zero video re-encoding loss.
- **Zero-Blackscreen Timeline Scrubbing**: Pre-generated static caches with HTTP Range partial-content support enable instantaneous timeline scrubbing across 24 hours of video footage without playback freezes or black screens.
- **PTZ Directional Control**: Smooth pan-tilt motor control with directional reversal switches.
- **Edge AI Intelligence**: Local YOLOv8/v11 object recognition, natural language semantic video search, and automated daily security morning reports.
- **Smart Home Integrations**: Out-of-the-box support for Home Assistant, Bark, Pushover, PushPlus, and custom Webhooks.

---

## 🛠️ Installation

### Option 1: fnOS App Center (Recommended)
1. Download the latest `micam_x.x.x_amd64.fpk` from the [Releases](../../releases) tab.
2. In your fnOS desktop interface, open **App Center** → **Manual Install** → Upload the `.fpk`.
3. Open MiCam, log in with the Mi Home QR code scanner, and go to **Settings → System** to automatically fetch the official streaming component in one click.

### Option 2: Build from Source
```bash
git clone https://github.com/AmigaMeow/micam.git
cd micam
bash build/build-fnos.sh
```

---

## 📄 License

This software is released under the **[GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE)**.
