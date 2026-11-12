# OCM

OCM manages an OpenConnect VPN on macOS. It runs as a root LaunchDaemon and restores the last requested background profile after boot, before GUI login.

Install OpenConnect and its standard vpnc-script with Homebrew. Install the release executable at `/Library/OCM/bin/ocm` and create an `ocm-hook` symlink beside it pointing to that executable. Keep the installation paths root-owned and writable only by root. The system LaunchDaemon runs `/Library/OCM/bin/ocm daemon` with `RunAtLoad` and `KeepAlive`.

Create `/Library/OCM/state` and its `profiles` subdirectory as root with mode `0700`. Store each profile as a root-owned mode-`0600` file named for the profile, using literal `KEY=value` lines. Use `OC_HOST`, `OC_USER`, `OC_PASS`, and `OC_GROUP`. The optional settings are `OC_RECONNECT_TIMEOUT`, `OC_DPD_INTERVAL`, `OC_VERBOSE`, `OC_COMPRESSION`, `OC_MTU`, `OC_BASE_MTU`, `OC_DTLS_LOCAL_PORT`, and `OC_SERVERCERT`.

For split DNS, set `OC_DNS_DOMAINS` to a comma-separated list of domain suffixes and optionally set `OC_DNS_SERVER` to one IP address. OCM uses the VPN-advertised DNS servers when the address is omitted. Each selected server must be reachable through the tunnel. macOS uses these servers for the listed domains and their subdomains. Other queries follow the Mac's existing DNS configuration.

```ini
OC_DNS_DOMAINS=corp.example.com,internal.example.net
OC_DNS_SERVER=192.0.2.53
```

Profiles without DNS suffixes leave DNS alone. OCM installs supplemental resolver files while connected and removes its files during disconnect and recovery. An existing resolver file for a requested suffix prevents setup. Upgrades from the previous global DNS configuration should specify suffixes in each profile that needs VPN DNS. The old `dns-server.json` is obsolete.

```sh
sudo /Library/OCM/bin/ocm start office
sudo /Library/OCM/bin/ocm status
sudo /Library/OCM/bin/ocm stop
```

`start` saves the profile name and requests a background connection. A fresh installation stays idle until the first start. `stop` clears saved intent, and `toggle PROFILE` starts the named profile or stops the requested session. `connect PROFILE` keeps a foreground session tied to the CLI and requires any saved background session to be stopped first. `status` reports progress and authentication failures. After correcting an authentication failure, run `start` again. OpenConnect diagnostics remain in the root-only state directory.
