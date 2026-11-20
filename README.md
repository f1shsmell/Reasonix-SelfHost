# Reasonix SelfHost

English | [中文](README.zh-CN.md)

A self-hosted replacement for the account service and relay that Reasonix
used to depend on. One binary, one SQLite file, and your own two hostnames: a
phone can then reach the Studio on your computer from any network, without
either side having to know a vendor's address.

This is a deployment guide. If you want to know what the service *is* before
installing it, read [What this is](#what-this-is) first — it takes one minute
and tells you whether you want this at all.

- [What this is](#what-this-is)
- [Before you start](#before-you-start)
- [Deploy: Docker + Caddy](#deploy-docker--caddy) ← the default path
- [Connect the desktop](#connect-the-desktop)
- [Connect the phone](#connect-the-phone)
- [Verify it end to end](#verify-it-end-to-end)
- [Operating it](#operating-it)
- [Troubleshooting](#troubleshooting)
- [Deploying without Docker](docs/DEPLOY-SYSTEMD.md)
- [Reference](#reference)

## What this is

Two things a self-hosted install has to provide, in one process:

**An account service.** A device flow that lets a desktop prove who it is and
register itself, and that lets a phone ask for permission to connect to that
desktop. One person, one account, one admin token — not multi-tenant, not
email-and-password, not a user directory.

**A relay.** A WebSocket both sides dial when a direct connection is not
possible. It forwards sealed frames and holds no key: the handshake is X25519
and the content is AES-GCM, both end to end between the desktop and the phone.
The relay can read the routing, never the conversation.

It speaks the shapes the released client and kernel already call — the device
code flow, `/me/devices`, `/me/remote-grants`, and the framed relay sockets. So
self-hosting is a base-URL change on both sides, not a second protocol.

**Not included, by design:** multi-tenancy, password sign-in, email
verification, attachment storage, crash upload, push notifications over APNs
or FCM. If you need those, this is the wrong service to add them to.

## Before you start

You need four things. Three of them are free, one costs money:

1. **A machine with a public entry point.** A VPS with root is the simplest.
   It only needs to run Docker and accept inbound 80 and 443.
2. **A domain you control**, with two hostnames pointed at that machine:

   ```
   id.example.com     A   203.0.113.10
   relay.example.com  A   203.0.113.10
   ```

   Use subdomains, not your apex. Adding two records alongside an existing
   site does not touch that site: only the names you create are affected.
3. **An email address** for Let's Encrypt. It is not an account and appears
   nowhere public — it is only where a certificate renewal failure would be
   reported. Any address you can read works.
4. **Roughly 15 minutes.**

One regional note: if the server is inside mainland China, the domain needs
ICP filing before it resolves publicly, and the certificate cannot be issued
until it does. Overseas and Hong Kong servers need nothing. See
[Troubleshooting](#troubleshooting) if the ACME challenge fails.

> **Ports 80 and 443 must actually be open.** Cloud providers frequently block
> them by default at the security-group level even when the host has no
> firewall. This is the single most common reason the first deployment fails,
> and it is worth checking before you start rather than after.

## Deploy: Docker + Caddy

Caddy terminates TLS for both hostnames and forwards to the service over the
internal network. The service itself never sees a certificate and never speaks
plain HTTP to a client.

Everything lives in one directory. Nothing is written into the source tree.

### 1. Get the code

```bash
sudo mkdir -p /srv/reasonix-selfhost
sudo chown -R "$USER" /srv/reasonix-selfhost
cd /srv/reasonix-selfhost
git clone https://github.com/f1shsmell/Reasonix-SelfHost.git app
```

The layout you are building:

```
/srv/reasonix-selfhost/
├── app/               the repository (git)
├── data/              SQLite lives here — back this up
├── caddy/Caddyfile    your two hostnames
├── selfhost.env       the admin token and the two origins
└── docker-compose.yml
```

Data and configuration sit *outside* `app/` on purpose: that keeps a `git pull`
from touching your deployment, and keeps the database out of the repository.

### 2. Generate the admin token

This is the only human credential the whole deployment has. It approves a
device code; it is not a password and there is no username beside it.

```bash
head -c 32 /dev/urandom | base64 | tr -d '/+=' | cut -c1-40
```

Copy the output somewhere safe before continuing — the service will not start
with a token shorter than 16 characters, and a lost token means re-deploying
rather than recovering.

### 3. Write the environment file

```bash
cat > /srv/reasonix-selfhost/selfhost.env <<'EOF'
REASONIX_SELFHOST_PUBLIC_URL=https://id.example.com
REASONIX_SELFHOST_RELAY_URL=https://relay.example.com
REASONIX_SELFHOST_ADMIN_TOKEN=paste-the-token-from-step-2
REASONIX_SELFHOST_ALLOWED_ORIGINS=https://id.example.com
REASONIX_SELFHOST_OWNER_HANDLE=owner
EOF
chmod 600 /srv/reasonix-selfhost/selfhost.env
```

Replace `id.example.com` and `relay.example.com` with your own names in **both
lines**. `PUBLIC_URL` and `RELAY_URL` are what the service hands back to clients
— a leftover `example.com` here produces a deployment that looks healthy and
whose approval links go to somebody else's domain.

`REASONIX_SELFHOST_ALLOWED_ORIGINS` lists the browser origins allowed to call
the service with credentials. Leaving it empty does not lock it down to "your
host" — it turns CORS off entirely. Fill it in.

### 4. Write the Caddyfile

Caddy runs in its own container here, so it cannot reach the service on
`127.0.0.1` — that address is Caddy itself. It reaches the service by its
compose service name instead, and that is the one line that differs from the
generic file in `deploy/Caddyfile`. Write the whole thing rather than copying:

```bash
mkdir -p /srv/reasonix-selfhost/caddy/{data,config}
cat > /srv/reasonix-selfhost/caddy/Caddyfile <<'EOF'
{
	email you@example.com
}

id.example.com {
	encode zstd gzip
	reverse_proxy selfhost:8790
}

relay.example.com {
	reverse_proxy selfhost:8790 {
		flush_interval -1
	}
}
EOF
$EDITOR /srv/reasonix-selfhost/caddy/Caddyfile
```

Three things to change in it, and nothing else:

- `email you@example.com` → your own address;
- `id.example.com {` → your account-service hostname;
- `relay.example.com {` → your relay hostname.

Then confirm no placeholder survived:

```bash
grep -nE 'example\.com' /srv/reasonix-selfhost/caddy/Caddyfile
```

Expect **no output**. If any line prints, it is still a placeholder and Caddy
will try to obtain a certificate for a domain you do not own.

The relay block carries `flush_interval -1`; keep it — an idle WebSocket is
normal there, and buffering it breaks the connection. Both blocks must name
`selfhost:8790`: swapping that for `127.0.0.1` is the single mistake that makes
every request return 502 while the logs look otherwise healthy.

### 5. Write the compose file

```yaml
# /srv/reasonix-selfhost/docker-compose.yml
services:
  selfhost:
    build:
      context: ./app
      dockerfile: deploy/Dockerfile
      args:
        # Module downloads inside the build. Go's own proxy is unreachable from
        # many networks; goproxy.cn is the mirror most Chinese hosts can reach.
        # Outside that situation, delete this `args:` block and the Dockerfile
        # default applies instead.
        GOPROXY: https://goproxy.cn,direct
    image: reasonix-selfhost:latest
    restart: unless-stopped
    env_file: ./selfhost.env
    volumes:
      - ./data:/data
    networks: [internal]

  caddy:
    image: caddy:2-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./caddy/Caddyfile:/etc/caddy/Caddyfile:ro
      - ./caddy/data:/data
      - ./caddy/config:/config
    networks: [internal]

networks:
  internal:
```

The service listens on `0.0.0.0:8790` inside its container — the image sets
that already, so `selfhost.env` should not override `ADDR`. Only `caddy`
publishes ports; the service is reachable as `selfhost:8790` from the Caddy
container and from nowhere else.

#### Or pull a prebuilt image

This repository builds the image on GitHub's runners (`.github/workflows/image.yml`)
and publishes it to GHCR, so nothing has to be compiled here.

```bash
docker pull ghcr.io/f1shsmell/reasonix-selfhost:latest
```

In `docker-compose.yml`, drop the whole `build:` block and keep only the image
line:

```yaml
  selfhost:
    image: ghcr.io/f1shsmell/reasonix-selfhost:latest
```

`docker compose up -d` then fetches it. Tags: a `v1.2.3` release publishes
`1.2.3`, `1.2`, `latest` and `sha-<commit>`; a manual run of the workflow
publishes only `latest` and `sha-<commit>`, so it can never overwrite a
released version number.

Two things worth knowing if the pull does not work:

- **The path is lowercase.** GHCR rejects uppercase, so `…-SelfHost` in the
  repository name becomes `…-selfhost` in the image name.
- **A package's visibility can differ from its repository's.** The published
  package is anonymous-readable, which is what makes the bare `docker pull`
  above work. If a future publish comes back `unauthorized`, open the
  repository's *Packages*, then the package's settings, and set visibility to
  **Public** — GHCR's default for a newly created package is private, and only
  the first publish inherits the repository's.

### 6. Make the data directory writable

The image runs as UID `10001`. A bind mount keeps the host's ownership, so the
directory has to agree:

```bash
mkdir -p /srv/reasonix-selfhost/data
sudo chown 10001:10001 /srv/reasonix-selfhost/data
```

### 7. Start it

```bash
cd /srv/reasonix-selfhost
docker compose up -d --build
docker compose ps
```

Both services should report `running`.

## Connect the desktop

The desktop's kernel reaches both services through two environment variables.
Nothing else changes:

```bash
REASONIX_ACCOUNTS_URL=https://id.example.com
REASONIX_REMOTE_URL=https://relay.example.com
```

`REASONIX_REMOTE_URL` may be written as `https://` or `wss://`; the kernel
rewrites the first form to the second itself. An `https://` URL is the one the
account service hands out, so writing it the same way here is the least
surprising choice. A `wss://` URL for a **public** host works too; a plaintext
`ws://` is accepted only on loopback.

Restart Studio, then sign in once with the account service. On the first start
it calls `POST /me/devices`, registers its X25519 public key, and keeps the
device credential it receives. That credential is what proves the desktop at
the relay from then on.

## Connect the phone

1. **Settings → Account → Service address**: enter `https://id.example.com`
   and save. This is the only place a phone learns where your service lives.
2. Tap **sign in**. The service returns a short code.
3. Approve it. Two ways, both need the admin token:
   - in the app, type the token into the field beside the code;
   - or tap "approve in the browser", open
     `https://id.example.com/device?code=XXXX-XXXX`, and enter it there.
4. The phone completes the device-flow poll on its own and keeps a session
   token. Relay connections use the address the account service returns.

The app's account pane and the approval page share one credential, so the two
paths cannot disagree about who may approve.

## Verify it end to end

Work down this list; stop at the first failure and see
[Troubleshooting](#troubleshooting).

**1. Ports are reachable** — from your own machine, not the server:

```bash
curl -I http://id.example.com
```

A redirect to `https://` means 80 is open. A timeout means the port is blocked.

**2. Certificates were issued:**

```bash
docker compose logs caddy | grep -iE 'certificate|error'
```

Look for `certificate obtained successfully` twice, once per hostname.

**3. The service answers:**

```bash
curl -s https://id.example.com/healthz
```

Expect `{"ok":true}`.

**4. The device flow starts:**

```bash
curl -s -X POST https://id.example.com/device/start
```

Expect JSON with `userCode`, `deviceCode` and a `verificationUriComplete` that
**starts with your own hostname**. If it says `example.com`, step 3's
`PUBLIC_URL` did not take effect.

**5. The phone reaches the desktop off Wi-Fi.** Turn the phone's Wi-Fi off,
confirm the connection shows as cloud, and send one message. This is the only
check that exercises the relay rather than the account service.

## Operating it

**Logs.** `docker compose logs -f selfhost` — one line per request, never a
token or a code.

**Restart.** `docker compose restart selfhost`. Relay sockets are held in
memory, so a restart drops every connected phone; the app reconnects on its own
with backoff.

**Back up.** The database is the only durable state — the account, the devices,
the credentials and the enrolled phones all live in `data/accounts.db`:

```bash
docker compose stop selfhost
cp data/accounts.db "accounts-$(date +%F).db"
docker compose start selfhost
```

Stopping first matters: SQLite in WAL mode has a journal beside the file, and
copying it while the service writes produces a backup that may not open.

**Upgrade:**

```bash
git -C app pull
docker compose up -d --build
```

That rebuilds from source, which needs the Go toolchain and peaks at 1.5–2 GB
of RAM. On a small host — one that shares its memory with other services — the
build can starve the machine until sshd stops answering. Build the binary on
your workstation instead and copy it in; `deploy/Dockerfile.runtime` does
nothing but place it.

Build from the checkout **on your workstation** — that is the tree the binary
comes from, so pull there, not on the server:

```bash
cd app                                   # the clone on your workstation
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
  -o reasonix-selfhost ./cmd/reasonix-selfhost
scp reasonix-selfhost you@server:/srv/reasonix-selfhost/
```

`GOARCH=arm64` instead of `amd64` on an ARM host — check with `uname -m` on the
server. The image is `FROM alpine`, so a wrong architecture shows up as
`exec format error` the moment the container starts.

Then copy `deploy/Dockerfile.runtime` from the clone to the deployment
directory beside the binary, and point the `selfhost` service's `build:` block
at it — **dropping its `args:`**, since the runtime Dockerfile declares none:

```bash
scp app/deploy/Dockerfile.runtime you@server:/srv/reasonix-selfhost/
```

```yaml
    build:
      context: .
      dockerfile: Dockerfile.runtime
```

`dockerfile:` resolves relative to `context`, so keeping both files at the
deployment root is what makes this one line work. (Putting it at
`dockerfile: app/deploy/Dockerfile.runtime` also works — the path just has to
stay inside the context.)

The context is now the deployment directory, which also holds `data/` and
`selfhost.env`. Keep them out of it with a `.dockerignore` beside the binary:

```
# /srv/reasonix-selfhost/.dockerignore
data
selfhost.env
app
*.db*
```

An allowlist (`*` then `!reasonix-selfhost`) sends less data, but it fails with
`COPY ... not found` if the binary is ever renamed. Either works; the explicit
list above is the forgiving one.

```bash
docker compose build selfhost     # seconds, no toolchain
docker compose up -d              # build alone leaves the old container running
```

Step 5 above builds from source, which is fine on a host with memory to spare;
this is the path to switch to when the server has little.

**Rotate the admin token.** Change `REASONIX_SELFHOST_ADMIN_TOKEN` in
`selfhost.env` and restart. Sessions already issued keep working; only future
approvals need the new token.

## Troubleshooting

**`challenge failed` or repeated ACME errors in Caddy's log.**
Port 80 is not reachable from the internet. Check the cloud provider's security
group first — a host firewall being open is not enough. Inside mainland China,
an unfiled domain also fails here.

**Caddy starts, the site never loads, Caddy shows nothing.**
The domain does not resolve to this machine yet. `nslookup id.example.com` from
outside the server, and remember DNS caching can outlast the record's TTL.

**`{"ok":true}` works but `/device/start` returns a link to `example.com`.**
`REASONIX_SELFHOST_PUBLIC_URL` is still the placeholder, or the container
did not pick up the edited `selfhost.env` — `docker compose up -d` after any
change to it.

**The service refuses to start: `SELFHOST_ADMIN_TOKEN is required`.**
The token is missing, shorter than 16 characters, or the env file is not being
read (`env_file` path, or a `chmod 600` on the wrong owner).

**The phone connects but every request fails.**
The relay address the account service returned is not reachable from the phone,
or `REASONIX_SELFHOST_RELAY_URL` does not match a hostname Caddy serves.
Check `docker compose logs selfhost` for `/v1/sessions/connect` entries.

**Studio says "the relay address is not recognised".**
The value in `REASONIX_REMOTE_URL` is not a bare origin. It must be
`https://host` or `wss://host` with no path, query, fragment or `user:pass@`.

**Studio says "no relay address yet".**
`REASONIX_REMOTE_URL` is unset. This is a distinct message from an
unreachable relay on purpose: the fix is the variable, not the network.

## Reference

### Environment variables

| Variable | Required | Meaning |
|---|---|---|
| `REASONIX_SELFHOST_PUBLIC_URL` | ✅ | The account service's public origin; approval links are built from it |
| `REASONIX_SELFHOST_RELAY_URL` | ✅ | The relay's public origin, returned to clients by `/me/remote-grants` |
| `REASONIX_SELFHOST_ADMIN_TOKEN` | ✅ | The only human credential; at least 16 characters |
| `REASONIX_SELFHOST_ALLOWED_ORIGINS` | | Origins allowed to call with credentials, comma-separated. **Empty means CORS is off entirely** |
| `REASONIX_SELFHOST_ADDR` | | Listen address, default `127.0.0.1:8790` (the image sets `0.0.0.0:8790`) |
| `REASONIX_SELFHOST_DATA` | | Data directory, default `data` (the image sets `/data`) |
| `REASONIX_SELFHOST_OWNER_HANDLE` | | Owner handle, default `owner` |
| `REASONIX_SELFHOST_OWNER_EMAIL` | | Owner email, default empty |
| `REASONIX_SELFHOST_GRANT_TTL` | | Device code lifetime, default `10m` |
| `REASONIX_SELFHOST_TICKET_TTL` | | Relay ticket lifetime, default `2m` |

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness probe |
| `POST` | `/device/start` | Open a sign-in; returns the codes |
| `POST` | `/device/poll` | Ask whether it was approved; a completed sign-in returns a session token |
| `GET` | `/device` | The approval page |
| `POST` | `/device/approve` · `/device/deny` | Approve or deny (admin token) |
| `GET` | `/me` | The current account |
| `POST` | `/auth/logout` | Revoke the current token |
| `GET` `POST` | `/me/devices` | List / register a Studio |
| `DELETE` | `/me/devices/{id}` | Revoke a Studio |
| `POST` | `/me/remote-grants` | Mint a one-time relay ticket and return the relay address |
| `POST` | `/host/push/notify` | A desktop reporting that something is waiting (device credential) |
| `GET` | `/host/controllers` | The phones enrolled against this desktop |
| `POST` | `/host/controllers/{id}/resolve` | Answer a waiting phone: `add`, `replace` or `reject` |
| `POST` | `/host/controllers/{id}/revoke` | End an enrolled phone |
| `GET` | `/host/controllers/{id}/report` | What is waiting for a phone, if anything |
| `POST` | `/host/controllers/{id}/seen` | Mark a phone's report as read |
| `GET` | `/v1/devices/{id}/connect` | The Studio's relay socket (Bearer device credential) |
| `GET` | `/v1/sessions/connect` | The phone's relay socket (ticket subprotocol) |

### Limits worth knowing

- **The relay keeps connections in memory.** A restart drops every phone; they
  reconnect with backoff, and there is no lost-message queue to reconcile.
- **The database is the only durable state.** Back it up as above, or lose the
  account and every registered device.
- **No push notifications.** A phone in the background learns that something is
  waiting by polling `/host/controllers/{id}/report`, not by being pushed to.
- **No account recovery.** The admin token is the last word. Lose it and the
  only path back is redeploying with a fresh database.

### Testing

```bash
go test ./...
```

`internal/api/api_test.go` runs the whole chain against a real SQLite file: the
device code flow, the approval page, device registration, both relay sockets,
plus the refusals that matter — an unconfigured origin is never reflected, and
a wrong token never approves.