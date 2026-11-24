# OCM

OCM manages an OpenConnect VPN on macOS. A root LaunchDaemon restores an explicitly requested background profile before GUI login; foreground sessions run in the invoking process. A fresh installation stays disconnected.

## Installation

Install OpenConnect and its standard vpnc-script separately with Homebrew. Download the release archive for your Mac and verify its SHA-256 digest against `checksums.txt`. Preview and install the verified executable as root:

```sh
sudo /path/to/ocm install --dry-run
sudo /path/to/ocm install
```

The installer validates its dependencies, installs the executable and hook under `/opt/ocm/bin`, links `/usr/local/bin/ocm`, creates private state under `/var/lib/ocm`, and registers the `io.github.d1ys3nk0.ocm` system LaunchDaemon. It refuses unrelated destination conflicts and an active or requested session. Stop an existing session before upgrading. Installation does not start a connection.

`sudo ocm uninstall` stops the session and removes the owned installation and service. Profiles, logs, recovery records and other private data under `/var/lib/ocm` are preserved. `ocm --help` and `ocm --version` do not require root.

## Profiles

Store profiles as root-owned mode-`0600` files named `/var/lib/ocm/profiles/NAME`; the state and profiles directories must be root-owned mode `0700`. Profile names contain only ASCII letters, digits, underscores and hyphens and cannot start with a hyphen. Use literal `KEY=value` lines for `OC_HOST`, `OC_USER`, `OC_PASS` and `OC_GROUP`. Optional settings are `OC_RECONNECT_TIMEOUT`, `OC_DPD_INTERVAL`, `OC_VERBOSE`, `OC_COMPRESSION`, `OC_MTU`, `OC_BASE_MTU`, `OC_DTLS_LOCAL_PORT` and `OC_SERVERCERT`.

For split DNS, set `OC_DNS_DOMAINS` to a comma-separated list of domain suffixes and optionally set `OC_DNS_SERVER` to one IP address. OCM uses the VPN-advertised DNS servers when the address is omitted. Each selected server must be reachable through the tunnel. macOS uses these servers for the listed domains and their subdomains; other queries follow the Mac's existing DNS configuration.

```ini
OC_DNS_DOMAINS=corp.example.com,internal.example.net
OC_DNS_SERVER=192.0.2.53
```

Profiles without DNS suffixes leave DNS alone. OCM installs supplemental resolver files while connected and removes its files during disconnect and recovery. An existing resolver file for a requested suffix prevents setup. When a suffix also covers the VPN gateway, OCM looks up the gateway through the physical network's DNS before each OpenConnect launch. It passes the address to OpenConnect while retaining the original hostname for TLS verification. If the lookup fails, status reports the `gateway-dns-failed` stage and the private service log records the lookup details.

To verify connectivity through the VPN, add a ping check to the profile.

```ini
OC_HEALTH_PING=192.168.0.2
OC_HEALTH_TIMEOUT=3s
OC_HEALTH_INTERVAL=30s
```

Choose an IPv4 host that responds to ICMP through this VPN. OCM pings it through the tunnel as soon as OpenConnect is ready, then every 30 seconds by default. The default timeout is 3 seconds. Use positive whole seconds with the `s` suffix and an interval longer than the timeout. After editing a profile, run `sudo ocm check NAME` and restart that session to apply the settings.

OCM waits for the first reply before reporting the tunnel connected. One missed reply marks it unhealthy, and two in a row trigger a reconnect. OCM checks the replacement tunnel immediately and reports recovery only after a reply. If that tunnel misses two checks, the background service retries the session with backoff and a foreground session exits with an error.

## Sessions

```sh
sudo ocm profiles
sudo ocm check office
sudo ocm start office
sudo ocm status --json
sudo ocm stop
sudo ocm run office
```

`check` validates a saved profile without connecting. `start` saves background intent and returns after the service accepts it; acceptance does not mean the VPN is connected. Repeating `start` for the same background profile is safe. `run` owns a foreground session without launchd or saved boot intent; Ctrl-C and SIGTERM stop it. `stop` clears background intent and stops either kind of session. `toggle NAME` stops the matching session or requests a background session when stopped. Stop first to change profiles or modes.

`status` reports the requested profile, observed session state and health. Its JSON fields are `state`, `mode`, `profile`, `desired_profile`, `health`, `error` and `details`. The `details.stage` field gives the OpenConnect-specific phase. For a running tunnel without a ping check, health is `unknown`. Authentication failures require correcting the profile and requesting `start` again. Diagnostics and recovery records remain private under `/var/lib/ocm`.

This interface replaces `connect NAME` with `run NAME` and the old `NAME start` ordering with `start NAME`. Existing installations require a deliberate profile and service migration; do not copy transient process IDs, sockets or locks.

## macOS notifications

OCM posts macOS notifications when a tunnel connects, loses its connection, recovers, or disconnects. Each notification names the active profile. Connecting a different profile after stopping produces a disconnect for the old profile and a connect for the new one.

Both foreground and background sessions deliver alerts to the active desktop user. Events that happen with no desktop user are skipped. Open **OCM** from Applications to request notification permission. The first alert can also ask macOS to allow notifications for **OCM**. To grant permission or send a test alert without changing the VPN session, run this as your desktop user:

```sh
open -n -a /Applications/OCM.app --args --test
```

In System Settings → Notifications, select **OCM** to enable banners and Notification Center history. Focus and the sender’s settings control banner presentation and previews.

## Development and releases

On macOS, `sh scripts/build-app.sh` builds the universal desktop app at `app/build/OCM.app`. To build an installable CLI from source, run the same command before building the Go executable so it embeds the signed app. Normal Go tests can run without building the notifier.

Install the Go version specified by `go.mod` and run `go test -race ./...` and `go vet ./...`. A canonical version tag triggers the release workflow, which publishes macOS Apple Silicon and Intel archives and `checksums.txt` after checks pass.
