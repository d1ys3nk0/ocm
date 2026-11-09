# OCM

OCM manages an OpenConnect VPN on macOS. It runs as a root LaunchDaemon and
restores the last requested background profile after boot, before GUI login.

Install OpenConnect and its standard vpnc-script with Homebrew. Install the
release executable at `/Library/OCM/bin/ocm` and create an `ocm-hook` symlink
beside it pointing to that executable. Keep the installation paths root-owned
and writable only by root. The system LaunchDaemon runs
`/Library/OCM/bin/ocm daemon` with `RunAtLoad` and `KeepAlive`.

Create `/Library/OCM/state` and its `profiles` subdirectory as root with mode
`0700`. Set `dns-server.json` in the state directory to a JSON string with the
VPN DNS server's IP address. Store each profile as a root-owned mode-`0600` file
named for the profile, using literal `KEY=value` lines. Use `OC_HOST`, `OC_USER`,
`OC_PASS`, and `OC_GROUP`. The optional settings are `OC_RECONNECT_TIMEOUT`,
`OC_DPD_INTERVAL`, `OC_VERBOSE`, `OC_COMPRESSION`, `OC_MTU`, `OC_BASE_MTU`,
`OC_DTLS_LOCAL_PORT`, and `OC_SERVERCERT`.

```sh
sudo /Library/OCM/bin/ocm start office
sudo /Library/OCM/bin/ocm status
sudo /Library/OCM/bin/ocm stop
```

`start` saves the profile name and requests a background connection. A fresh
installation stays idle until the first start. `stop` clears saved intent, and
`toggle PROFILE` starts the named profile or stops the requested session.
`connect PROFILE` keeps a foreground session tied to the CLI and requires any
saved background session to be stopped first. `status` reports progress and
authentication failures. After correcting an authentication failure, run
`start` again. OpenConnect diagnostics remain in the root-only state directory.
