<div align="center">

# 📡 ISP Probe

<p>

**Dual-WAN Broadband Link Monitor · ISP 探针**

Per-link connectivity monitoring and guided speed testing for dual-WAN home broadband

</p>

[![CI](https://github.com/striver2006/ISP-Probe/actions/workflows/ci.yml/badge.svg)](https://github.com/striver2006/ISP-Probe/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-macOS-lightgrey?logo=apple)](#platform-support)
[![Platform](https://img.shields.io/badge/Platform-Windows-blue?logo=windows)](#platform-support)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![CGO](https://img.shields.io/badge/CGO-disabled-success)](#deliberate-design-tradeoffs)

[简体中文](README.md) | English

</div>

---

Every probe bypasses the local clash verge TUN and its DNS hijacking — otherwise you would be measuring the proxy chain, not the broadband link.

## The problem

When two broadband lines are load-balanced into one connection, a silent failure on one of them produces no symptom you would notice: the internet still works, your bandwidth has just quietly halved. This tool watches each line continuously and alerts the moment one goes down.

## Quick start

```bash
go build -o isp-probe ./cmd/isp-probe

./isp-probe doctor   # self-check: confirm the bypass works and per-link probing is viable
./isp-probe serve    # continuous monitoring + dashboard at http://127.0.0.1:8686
```

To keep it running in the background across logins:

```bash
./isp-probe service install
```

## Commands

Run in the foreground, exit with Ctrl-C:

| Command | Description |
|---|---|
| `doctor` | Environment self-check: is the TUN bypass working, is the per-link channel viable, is DNS poisoned by fake-ip |
| `probe` | Run one per-link connectivity probe and print the result |
| `speed` | Run an up/down speed test on whichever line the current egress uses |
| `serve` | Run continuously: periodic probing plus the web dashboard |

Background service:

| Command | Description |
|---|---|
| `service install` | Install as a login-launched service and start it |
| `service uninstall` | Stop and remove |
| `service start` / `stop` | Start/stop an installed service (`stop` does not uninstall) |
| `service status` | Show installation and running state |
| `service logs [-n N]` | Print the tail of the service log |

Notifications:

| Command | Description |
|---|---|
| `notify test` | Send a test message to each configured IM channel (`--name` targets just one) |

Global flags: `-v` (debug logging), `-c <config>`. Without `-c`, `config.yaml` is looked up in the **current directory** first, then the **executable's directory** — the former preserves the habit of running straight from the repo, the latter lets you copy the whole folder anywhere.

## Running as a service

```bash
./isp-probe service install    # install + start + verify the dashboard actually came up
./isp-probe service status
./isp-probe service logs -n 50
```

`install` runs `doctor` first by default. When the TUN bypass silently fails, the program does not error — it just produces fake data, and an unattended background service producing fake data is far more dangerous than a foreground run. Use `--skip-doctor` to override. Other flags: `--no-start`, `--force`, `--log-dir`.

**Config, database and logs are all anchored to the directory containing `config.yaml`**, so the whole folder stays portable. The service definition records absolute paths resolved at install time, so re-run `install` after moving it.

**Only one instance may run at a time.** Starting `serve` while the service is running gives you a message naming the occupant rather than a bare `address already in use`. `probe` / `speed` are unaffected — taking a quick look should never be blocked.

### macOS

Installs as a **LaunchAgent** (`~/Library/LaunchAgents/com.czb.isp-probe.plist`), no sudo required. It runs inside your GUI session so desktop notifications work; the tradeoff is that it starts **after login**, not strictly at boot. Crashes restart automatically (10s minimum interval), clean exits do not — that is what makes `service stop` stick.

Do not install from an SSH session: the job may land in the wrong domain, silently breaking desktop notifications. `install` detects and blocks this; use `--force` if you really mean it.

### Windows

Installs as a Windows service (`ISPProbe`), **requires administrator** (except `status`). Startup type is "Automatic (Delayed Start)": at boot the network stack and physical NIC are not ready yet, so `iface.Detect` would fail or pick the wrong adapter — and for a once-a-minute probe, being a few dozen seconds late costs nothing.

Two known limitations:

- **Desktop notifications do not arrive.** Windows services run in session 0, isolated from your desktop. Set `notify.desktop` to `false` and configure [IM notifications](#im-notifications) instead — pushing to your phone is exactly what that feature is for.
- The service creates the database as LocalSystem, so running `probe` as a normal user may fail on permissions. Install somewhere user-writable like `C:\ISP-Probe\` rather than `C:\Program Files`.

## Two capabilities, different setup cost

### Connectivity monitoring — zero configuration

Send a DNS query to each modem's LAN IP. The packet necessarily leaves through the router's corresponding WAN port and is answered by that ISP's recursive resolver. This channel does not depend on any router-side traffic splitting configuration.

```
via 192.168.10.1 (Telecom modem) resolving www.baidu.com → 180.101.51.73   Telecom node
via 192.168.1.1  (Mobile  modem) resolving www.baidu.com → 223.109.82.16   Mobile node
```

Both lines are monitored simultaneously; a drop triggers an alert — a desktop notification, plus [IM notifications](#im-notifications) pushed to your phone.

### Per-link speed testing — one manual step

The router binds WAN policy by MAC address, and one MAC can only use one line at a time, so **one line per test**:

1. Click "Start test" on the dashboard; the program identifies the current line from the egress IP's ISP ownership
2. After the first line finishes, the dashboard shows a clickable router link and the MAC to rebind (with one-click copy)
3. You rebind to the other line on the router
4. The program polls egress ownership every 5 seconds and **starts testing the second line automatically once it detects the switch**

## How it bypasses clash

Three layers, each of which fails silently if omitted:

1. **Every outbound socket binds to the physical NIC** (`IP_BOUND_IF` / `SO_BINDTODEVICE` equivalent), so traffic never lands on the TUN.
2. **Never use the system resolver.** `net.Resolver` returns fake-ip (`198.18.0.0/15`) on this machine. All name resolution goes through DoH.
3. **`netbind.Binder.DialContext` refuses hostnames.** This is a deliberate guard rail, forcing callers to resolve explicitly rather than quietly falling back to the system resolver.

For HTTP requests the URL keeps its domain name (so SNI and the Host header stay correct) and resolution is pushed down into the dialer — replacing the domain with an IP breaks TLS, since site certificates rarely carry IP SANs.

## Configuration

See `config.yaml`. Everything environment-specific lives there; no addresses are hard-coded:

- `links[].modem_ip` — each line's modem LAN IP, the entry point for per-link probing
- `links[].expect_isp` — keywords for verifying egress ISP ownership
- `links[].wan_label` — the policy name shown in the guided prompts
- `router.admin_url` — clickable router admin link on the dashboard
- `probe.min_gap` — minimum interval between two queries to the same modem

**Do not put secrets in `config.yaml`** — it is committed to the repository. Put a `config.local.yaml` next to it (already in `.gitignore`) containing only the fields you want to override:

```yaml
# config.local.yaml — not committed
web:
  token: "some random string"
notify:
  webhooks:
    - name: "Alert group"
      kind: feishu
      url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
      enabled: true
```

The override file is only looked up **in the main config file's directory**, so it works in service mode without extra setup. Note that **lists are replaced wholesale, not appended**: writing `webhooks` in the override file replaces the main config's list entirely. A missing file is normal; a file that exists but fails to parse is a hard error — silently ignoring it would let you believe your secrets took effect while not a single alert can actually be sent.

## IM notifications

Push outage alerts to your phone. This exists for **service mode**: Windows services run in session 0 and desktop notifications simply never reach you, while "one line silently failing" is by definition an unattended scenario.

Supports WeCom, DingTalk and Feishu group bots, plus generic custom webhooks. Configure in `config.local.yaml` (above), then verify:

```bash
./isp-probe notify test          # or click "Test" on the dashboard's channel card
```

Bot URLs come from group settings (Group settings → Group bots → Add). Feishu requires at least one security setting:

- **Signature verification** — put the secret it gives you in `secret`.
- **Custom keywords** — leave `secret` empty; use `ISP探针` or `线路` as the keyword. Message bodies look like `ISP探针 · 线路故障 / 电信 线路故障：…`, which matches.
- **IP allowlist** — your home's public IP. Not recommended: residential IPs change, and when yours does, notifications fail silently.

DingTalk works the same way; with "signed" security, put the `SEC`-prefixed secret in `secret`. WeCom needs no `secret` — its key is the `key` query parameter in the URL. Custom webhooks build their body from `body_template`, with `.Kind` `.LinkID` `.Message` `.Time` available.

All three return **HTTP 200 even when the key is invalid, the bot was removed from the group, or the signature does not match** — the real error code is in the response body. `notify test` prints that code, so only a ✓ means it actually went out.

**Alert cadence**: a `down` alert fires once a line fails `probe.fail_threshold` times in a row. If it stays down, reminders follow at 5, 15, 30 and 60 minutes, then hourly, each carrying the elapsed duration. An `up` alert fires on recovery. Set `notify.repeat_alert` to `false` to disable reminders. Reminders are notification-only and never written to the events table, so one outage always shows as exactly one `down` + one `up` in the dashboard's event list.

**One explicit boundary**: when **both** lines go down simultaneously, nothing can be sent, and nothing is replayed after recovery. This feature targets "one line silently fails while the other keeps working" — which is the entire reason this tool exists. A total outage is something you will notice yourself.

## Exposing the dashboard on your LAN

It listens on `127.0.0.1` by default. To reach it from a phone or another machine, edit `config.yaml`:

```yaml
web:
  listen: "0.0.0.0:8686"
```

**Set `web.token` at the same time** (in `config.local.yaml`). Without it, any device on the same Wi-Fi — including guests' phones and IoT devices — can see your internal topology, modem addresses and NIC MAC, and can trigger a bandwidth-saturating speed test. `doctor` warns about exactly this configuration.

The token only applies to requests from other devices; loopback is always allowed (otherwise the CLI and the single-instance check would be blocked by your own auth). The browser prompts for it on first visit and remembers it in localStorage.

You also need to allow inbound connections through the system firewall: macOS prompts on first access — choose "Allow"; Windows needs an administrator to run `netsh advfirewall firewall add rule name="ISP-Probe" dir=in action=allow protocol=TCP localport=8686`. `doctor` prints the right hint for your platform.

## Deliberate design tradeoffs

**Modem DNS rate limiting must be handled.** In practice, five rapid queries make the modem time out on all of them while the line itself is perfectly fine. Failure detection is therefore tiered: DNS timeout → ping the modem to rule out a local fault → retry slowly to rule out rate limiting → only then declare a line failure.

**Anchor latency is attributed to no single line.** Which WAN a TCP connection takes is decided by the router's load balancer; the host cannot choose. That metric is recorded under a `_combined` pseudo-link rather than masquerading as per-link data.

**No ICMP-based packet loss.** Same reason: an ICMP ping to a public target cannot be pinned to a specific line. Per-link quality is measured by the success rate of DNS probes through each modem — those are the ones deterministically routed through a known WAN.

**Cross-validation that link separation actually works.** Both lines resolve the same domain and should return nodes from their respective ISPs. If the results are identical, traffic is not being split across WANs and the dashboard flags it — at that point "both lines healthy" is not trustworthy.

## Platform support

macOS and Windows. Pure-Go dependencies, cross-compiles directly with `CGO_ENABLED=0`:

```bash
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o isp-probe     ./cmd/isp-probe
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -o isp-probe     ./cmd/isp-probe
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o isp-probe.exe ./cmd/isp-probe
```

The minimum OS version follows whatever the Go toolchain itself supports (see the Go version in [go.mod](go.mod)); the project does not depend on newer system APIs. Linux builds are **deliberately not provided** — service management, NIC detection and route table parsing are all per-platform implementations, and shipping a stub that compiles but cannot run would only create a false impression.

Windows note: when mihomo has `strict-route` enabled it uses WFP to block port 53 on every non-TUN interface at the kernel level, which breaks the per-link channel (`doctor` detects and reports this). Clash Verge Rev leaves that option off by default.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). In short: `go vet ./... && GOOS=windows go vet ./... && go test ./...` must pass, and `./isp-probe doctor` must come back all green.

## License

[MIT](LICENSE)
