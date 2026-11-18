# Deploying without Docker

English | [中文](DEPLOY-SYSTEMD.zh-CN.md) | [Back to the main guide](../README.md)

The main guide installs two containers. This one runs the same service from a
static binary under systemd, with Caddy on the host terminating TLS. Choose it
if you would rather not run Docker, or if the machine is already serving other
sites through a host Caddy.

Everything else — the two hostnames, the admin token, the `.env` contents, the
desktop and phone steps, and the verification checklist — is identical to the
main guide. The build, the service user, the unit, and where Caddy runs are
what this page replaces; the rest you can follow there verbatim.

**Why this is a good fit here:** the binary has no cgo and no libc dependency
(SQLite is the pure-Go `modernc.org/sqlite`), so it is a single file that runs
on any Linux the same way, and a host Caddy can serve it alongside your other
sites without a port conflict.

## Prerequisites

- A Linux machine with root, reachable on 80 and 443.
- Go 1.26 or newer to build it, **or** build it elsewhere and copy the binary.
- Caddy installed on the host (`caddy --version`).

## 1. Build

On the server, or on any machine with the same architecture:

```bash
sudo mkdir -p /opt/reasonix-selfhost
cd /opt/reasonix-selfhost
git clone https://github.com/f1shsmell/Reasonix-SelfHost.git app
cd app
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/reasonix-selfhost ./cmd/reasonix-selfhost
```

Check it runs and refuses a half-read config, which proves the binary and the
validator both work:

```bash
reasonix-selfhost
# selfhost: REASONIX_SELFHOST_PUBLIC_URL is required (e.g. https://id.example.com)
```

That error is the expected answer here. The service refuses to start without a
complete configuration, rather than starting half-configured.

## 2. Create the service user and directories

The service should not run as root, and it needs its own data directory:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin reasonix-selfhost
sudo mkdir -p /var/lib/reasonix-selfhost
sudo chown reasonix-selfhost:reasonix-selfhost /var/lib/reasonix-selfhost
sudo chmod 700 /var/lib/reasonix-selfhost
```

## 3. Write the environment file

Identical in content to the main guide; only the path differs. It lives in
`/etc` because it holds the admin token:

```bash
sudo install -d -m 700 /etc/reasonix-selfhost
sudo tee /etc/reasonix-selfhost/env >/dev/null <<'EOF'
REASONIX_SELFHOST_PUBLIC_URL=https://id.example.com
REASONIX_SELFHOST_RELAY_URL=https://relay.example.com
REASONIX_SELFHOST_ADMIN_TOKEN=paste-the-token-from-the-main-guide
REASONIX_SELFHOST_ALLOWED_ORIGINS=https://id.example.com
REASONIX_SELFHOST_OWNER_HANDLE=owner
REASONIX_SELFHOST_DATA=/var/lib/reasonix-selfhost
EOF
sudo chown root:reasonix-selfhost /etc/reasonix-selfhost/env
sudo chmod 640 /etc/reasonix-selfhost/env
```

The file is `root:` owned and readable only by the service group: systemd reads
it as root, and the service user needs no more than that. A `600 root:root` here
is the classic mistake — the unit starts, systemd cannot hand the variables to a
user that cannot read the file, and the service exits complaining that
`PUBLIC_URL` is missing.

`ADDR` is left at its default `127.0.0.1:8790`: the host Caddy reaches it there,
and nothing else on the machine can. Do not change it to `0.0.0.0` — that would
expose the plain-HTTP port directly.

Generate the admin token with the same command as the main guide:

```bash
head -c 32 /dev/urandom | base64 | tr -d '/+=' | cut -c1-40
```

## 4. Write the unit

```bash
sudo tee /etc/systemd/system/reasonix-selfhost.service >/dev/null <<'EOF'
[Unit]
Description=Reasonix self-hosted accounts and relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=reasonix-selfhost
Group=reasonix-selfhost
EnvironmentFile=/etc/reasonix-selfhost/env
ExecStart=/usr/local/bin/reasonix-selfhost
Restart=on-failure
RestartSec=2s

# The service needs its data directory and nothing else. These make a bug in
# the service fail loudly here instead of touching the rest of the machine.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/reasonix-selfhost

[Install]
WantedBy=multi-user.target
EOF
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now reasonix-selfhost
sudo systemctl status reasonix-selfhost
```

Expect `active (running)` and a log line naming your two origins.

## 5. Point Caddy at it

The repository's `deploy/Caddyfile` works unchanged — it already reverse
proxies to `127.0.0.1:8790`. Copy it, then set your names as in the main guide:

```bash
sudo cp /opt/reasonix-selfhost/app/deploy/Caddyfile /etc/caddy/Caddyfile
sudo $EDITOR /etc/caddy/Caddyfile
sudo grep -nE 'example\.com' /etc/caddy/Caddyfile    # expect no output
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

If the host Caddy already serves other sites, **append** the two site blocks
to the existing file rather than replacing it — a replacement would silently
drop whichever sites were configured there.

`caddy validate` catches a syntax error before the reload does, and a reload
that fails leaves the old configuration serving, so a mistake here does not
take your other sites down.

## 6. Verify

Same five checks as the main guide, with two substitutions:

```bash
curl -I http://id.example.com
curl -s https://id.example.com/healthz
curl -s -X POST https://id.example.com/device/start
sudo journalctl -u caddy -n 50 | grep -iE 'certificate|error'
```

Then complete the desktop and phone steps, and the off-Wi-Fi check, exactly as
the main guide describes them.

## Operating it

**Logs.** `sudo journalctl -u reasonix-selfhost -f` — one line per request.

**Restart.** `sudo systemctl restart reasonix-selfhost`. Relay sockets are held
in memory, so this drops every connected phone; they reconnect with backoff.

**Upgrade:**

```bash
cd /opt/reasonix-selfhost/app
sudo -u reasonix-selfhost git pull     # or clone as root and chown
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/reasonix-selfhost ./cmd/reasonix-selfhost
sudo systemctl restart reasonix-selfhost
```

A binary replaced while the service runs stays in use until the restart, so the
two steps belong together.

**Back up.** Same rule as the main guide, applied to a different path:

```bash
sudo systemctl stop reasonix-selfhost
sudo cp /var/lib/reasonix-selfhost/accounts.db "/var/backups/accounts-$(date +%F).db"
sudo systemctl start reasonix-selfhost
```

Stopping first matters: SQLite in WAL mode keeps a journal beside the file, and
copying it during a write produces a backup that may not open.

## Troubleshooting

Everything in the main guide's troubleshooting applies. Three failures are
specific to this path:

**`status=203/EXEC`.** systemd cannot execute the binary. Usually a missing
`chmod +x`, a wrong path, or — on a machine with a different architecture —
a binary built for the wrong one.

**`status=209/STDOUT` or the service exits immediately with no log line.**
The environment file is not readable. Check `EnvironmentFile=` points at
`/etc/reasonix-selfhost/env` and that the unit's user can read it.

**`ProtectSystem=strict` blocked the write.** The data directory must be in
`ReadWritePaths=`. If you moved `REASONIX_SELFHOST_DATA`, update the unit
with it — the service will otherwise fail to create `accounts.db` and exit.

**Caddy reload fails after editing.** The old configuration keeps serving, so
your existing sites stay up; `caddy validate` reports the line that is wrong.