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

Profiles without DNS suffixes leave DNS alone. OCM installs supplemental resolver files while connected and removes its files during disconnect and recovery. An existing resolver file for a requested suffix prevents setup.

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

`status` reports the requested profile, observed session state and health. Its JSON fields are `state`, `mode`, `profile`, `desired_profile`, `health`, `error` and `details`; `details.stage` gives the OpenConnect-specific phase. A running tunnel does not by itself verify end-to-end connectivity, so health is `unknown`. Authentication failures require correcting the profile and requesting `start` again. Diagnostics and recovery records remain private under `/var/lib/ocm`.

This interface replaces `connect NAME` with `run NAME` and the old `NAME start` ordering with `start NAME`. Existing installations require a deliberate profile and service migration; do not copy transient process IDs, sockets or locks.

## Development and releases

Install the Go version specified by `go.mod` and run `go test -race ./...` and `go vet ./...`. A canonical version tag triggers the release workflow, which publishes macOS Apple Silicon and Intel archives and `checksums.txt` after checks pass.
