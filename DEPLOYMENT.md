# Cloud Deployment

This repo now supports the single-session cloud prototype described in the plan:

- Browser audio over WebRTC/WebSocket to the Go server
- ESP32 audio over UDP to the Go server
- One browser and one ESP32 routed through one public Ubuntu host

## Server ports

- `80/tcp` for Nginx
- `443/tcp` optional if you later terminate TLS on the VM
- `5000/udp` for ESP32 audio

## 1. Build the Go server

```powershell
cd server
go build -o webrtc_server .
```

Copy the binary to the Ubuntu host, for example:

- `/opt/webrtc-client/bin/webrtc_server`

## 2. Copy web assets

Copy the contents of `web/` to:

- `/var/www/webrtc_client`

## 3. Configure systemd

Install [deploy/systemd/webrtc-client.service](deploy/systemd/webrtc-client.service) as:

- `/etc/systemd/system/webrtc-client.service`

Create the service account and enable the service:

```bash
sudo useradd --system --home /opt/webrtc-client --shell /usr/sbin/nologin webrtc
sudo systemctl daemon-reload
sudo systemctl enable --now webrtc-client
sudo systemctl status webrtc-client
```

The service starts the Go app on:

- `127.0.0.1:8080` for HTTP/WebSocket
- `0.0.0.0:5000/udp` for ESP32 traffic

## 4. Configure Nginx

Install [deploy/nginx/webrtc_client.conf](deploy/nginx/webrtc_client.conf) as:

- `/etc/nginx/sites-available/webrtc_client.conf`

Enable it:

```bash
sudo ln -s /etc/nginx/sites-available/webrtc_client.conf /etc/nginx/sites-enabled/webrtc_client.conf
sudo nginx -t
sudo systemctl reload nginx
```

## 5. Start a temporary HTTPS tunnel

Because the server has no domain, browser microphone access should use a trusted HTTPS origin.

Example with `ngrok`:

```bash
ngrok http http://127.0.0.1:80
```

Open the generated `https://...ngrok-free.app` URL on the PC browser. The web client now automatically uses `wss://` for signaling when loaded over HTTPS.

## 6. Flash the ESP32

The firmware is configured to send UDP audio to:

- `117.72.24.77:5000`

Build and flash from `mcu/` after PlatformIO is available:

```powershell
platformio run
platformio run --target upload
```

## 7. Verify end-to-end flow

Check these in order:

1. `sudo systemctl status webrtc-client` shows the Go server running.
2. `sudo journalctl -u webrtc-client -f` shows browser connect logs and ESP32 UDP endpoint discovery.
3. `sudo ss -ulpn | grep 5000` shows the UDP listener.
4. The browser page opened through the HTTPS tunnel can request microphone permission.
5. The browser can connect and stay connected.
6. ESP32 audio is heard on the browser, and browser audio packets are seen by the ESP32.

## Notes

- Raw `http://117.72.24.77` is not suitable for normal browser microphone capture.
- The current server is intentionally single-session and prototype-grade.
- No TURN, authentication, or multi-device routing is included in this phase.
