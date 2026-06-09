# snapweb-chromecast-sender

Monitors Chromecast devices and automatically casts a web page to them when idle. Useful for always-on dashboards.

## Features

- Auto-cast a URL to idle devices on a configurable interval
- Per-device IP address to bypass mDNS (required in some network environments)
- Network scanner with live progress (mDNS via catt, falls back to TCP probe on port 8008)
- Real device state via pychromecast when a host IP is set
- Web UI to manage devices and config (Alpine.js + Tailwind CSS)

## Usage

```yaml
# docker-compose.yml
services:
  chromecast-sender:
    image: ghcr.io/w-floyd/snapweb-chromecast-sender:main
    ports:
      - "8083:8083"
    volumes:
      - ./config:/config
    environment:
      PORT: "8083"
      CONFIG_PATH: /config/config.json
    restart: unless-stopped
```

```sh
docker compose up -d
# open http://localhost:8083
```

Use **Scan Network** to discover devices, add them to config, set their IP in the host field, then save.

## Config

`config/config.json` is created on first run and editable via the UI.

```json
{
  "check_interval": 60,
  "default_url": "http://192.168.1.x/dashboard",
  "devices": [
    {
      "name": "Living Room",
      "host": "192.168.1.50",
      "url": "",
      "auto_cast": true
    }
  ]
}
```

| Field | Description |
|---|---|
| `check_interval` | Seconds between idle checks |
| `default_url` | Fallback URL for devices with no URL set |
| `host` | Device IP — required in Docker (bypasses mDNS) |
| `auto_cast` | Cast automatically when the device is idle |

## Notes

- `host` is required when running in Docker with bridge networking — mDNS discovery does not work in that context
- For mDNS scanning to work, deploy on Linux with `network_mode: host` (see commented section in `docker-compose.yml`)
- Cast state survives as long as the container runs; a restart will re-cast to auto-cast devices on the next check

## Screenshot

![alt text](image.png)