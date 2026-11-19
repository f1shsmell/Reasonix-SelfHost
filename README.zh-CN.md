# Reasonix SelfHost

[English](README.md) | 中文

Reasonix 原先依赖的账号服务与中继，自托管替代品。一个二进制、一个 SQLite 文件、
两个你自己的域名：手机从此在任何网络下都能连到家里的 Studio，两端都不必再知道厂商的地址。

这是一份部署指南。想知道这个服务**是什么**再决定装不装，先读[它是什么](#它是什么)，
一分钟就够。

- [它是什么](#它是什么)
- [开工之前](#开工之前)
- [部署：Docker + Caddy](#部署docker--caddy) ← 默认路径
- [接上电脑](#接上电脑)
- [接上手机](#接上手机)
- [端到端验证](#端到端验证)
- [日常运维](#日常运维)
- [故障排查](#故障排查)
- [不用 Docker 部署](docs/DEPLOY-SYSTEMD.zh-CN.md)
- [参考](#参考)

## 它是什么

自托管需要提供两样东西，它们跑在同一个进程里：

**账号服务。** 一套设备码流程，让电脑证明自己是谁并完成注册，让手机申请连接这台
电脑的权限。一个人、一个账号、一个管理员令牌——不是多租户，不是邮箱密码，不是用户目录。

**中继。** 直连不可用时两端都会去拨的 WebSocket。它只转发密封帧，不持有任何密钥：
握手是 X25519，内容是 AES-GCM，端到端都在电脑与手机之间。中继能读到路由，永远读不到对话。

它说的是已发布的客户端与内核**已经在调**的那些形状——设备码流程、`/me/devices`、
`/me/remote-grants`、以及带帧的中继 socket。所以自托管在两端都只是换一个 base URL，
不是第二套协议。

**有意不包含**：多租户、密码登录、邮件验证、附件存储、崩溃上报、APNs / FCM 推送。
需要这些，说明你不该往这个服务里加，而是该另找一个。

## 开工之前

你需要四样东西，三样免费，一样要花钱：

1. **一台有公网入口的机器。** 有 root 的 VPS 最省事。它只要能跑 Docker，并接受
   入站 80 和 443。
2. **一个你控制的域名**，两个主机名指向那台机器：

   ```
   id.example.com     A   203.0.113.10
   relay.example.com  A   203.0.113.10
   ```

   用子域，不要用根域。在现有站点旁边加两条记录不会动到它：只有你新建的名字受影响。

3. **一个邮箱地址**给 Let's Encrypt。它不是账号，也不会出现在任何公开位置——它只是
   证书续期失败时的通知去处。你能收到信的任意地址都可以。
4. **大约 15 分钟。**

一条地域提醒：服务器在中国大陆境内时，域名必须先完成 ICP 备案才能公网解析，
而证书在解析生效前签发不出来。境外与香港的机器不需要这一步。ACME 挑战失败时
见[故障排查](#故障排查)。

> **80 和 443 端口必须真的通。** 云厂商常在安全组层面默认封禁，即使主机本身没有
> 防火墙。这是首次部署失败最常见的原因，值得在开工前就查，而不是出了问题再查。

## 部署：Docker + Caddy

Caddy 为两个主机名终结 TLS，并通过内部网络转发给服务。服务自身永远看不到证书，
也永远不对客户端说明文 HTTP。

所有东西都在一个目录里，不会写进源码树。

### 1. 取代码

```bash
sudo mkdir -p /srv/reasonix-selfhost
sudo chown -R "$USER" /srv/reasonix-selfhost
cd /srv/reasonix-selfhost
git clone https://github.com/f1shsmell/Reasonix-SelfHost.git app
```

你要搭出来的布局：

```
/srv/reasonix-selfhost/
├── app/               仓库（git）
├── data/              SQLite 落在这里 —— 备份的就是它
├── caddy/Caddyfile    你的两个域名
├── selfhost.env       管理员令牌与两个 origin
└── docker-compose.yml
```

数据与配置放在 `app/` **外面**是有意的：这样 `git pull` 不会碰到你的部署，
数据库也不会进仓库。

### 2. 生成管理员令牌

这是整个部署**唯一**的人工凭据。它用来批准设备码；它不是密码，旁边也没有用户名。

```bash
head -c 32 /dev/urandom | base64 | tr -d '/+=' | cut -c1-40
```

继续之前先把输出存到安全的地方——令牌短于 16 字符服务不会启动，而令牌丢了
只能重新部署，不能找回。

### 3. 写环境变量文件

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

把 `id.example.com` 与 `relay.example.com` 换成你自己的名字，**两行都要换**。
`PUBLIC_URL` 与 `RELAY_URL` 是服务交还给客户端的东西——这里留下 `example.com`，
会得到一个看起来健康、而批准链接指向别人域名的部署。

`REASONIX_SELFHOST_ALLOWED_ORIGINS` 列出允许带凭据调用服务的浏览器 origin。
把它留空并不等于"只允许你自己的主机"，而是把 CORS 整个关掉。要填。

### 4. 写 Caddyfile

这里 Caddy 跑在自己的容器里，所以它够不到 `127.0.0.1` 上的服务——那个地址是 Caddy
自己。它改按 compose 的服务名找服务，这也是它和 `deploy/Caddyfile` 里那份通用文件
唯一不同的一行。别复制，直接把整份写出来：

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

里面只改三处，别的不动：

- `email you@example.com` → 你自己的地址；
- `id.example.com {` → 你的账号服务主机名；
- `relay.example.com {` → 你的中继主机名。

然后确认占位符一个不剩：

```bash
grep -nE 'example\.com' /srv/reasonix-selfhost/caddy/Caddyfile
```

期望**没有任何输出**。如果还有行打出来，那就是占位符，Caddy 会去为你并不拥有的
域名申请证书。

中继那段带着 `flush_interval -1`，保留它——那里的 WebSocket 空闲是正常的，
对它做缓冲会断连接。两个块都必须写 `selfhost:8790`：把它换成 `127.0.0.1` 正是
那个让每个请求都返回 502、而日志看起来一切正常的错误。

### 5. 写 compose 文件

```yaml
# /srv/reasonix-selfhost/docker-compose.yml
services:
  selfhost:
    build:
      context: ./app
      dockerfile: deploy/Dockerfile
      args:
        # 构建时的模块下载。Go 官方代理在很多网络下连不上；goproxy.cn 是国内
        # 大多数机器能访问的镜像。不在这种情况，把整个 `args:` 块删掉，
        # Dockerfile 里的默认值会生效。
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

服务在容器内监听 `0.0.0.0:8790`——镜像里已经设好了，所以 `selfhost.env` 不要覆盖
`ADDR`。只有 `caddy` 对外发布端口；服务只能从 Caddy 容器以 `selfhost:8790` 访问，
别处够不着。

### 6. 让数据目录可写

镜像以 UID `10001` 运行。绑定挂载会保留宿主的所有权，所以目录得对得上：

```bash
mkdir -p /srv/reasonix-selfhost/data
sudo chown 10001:10001 /srv/reasonix-selfhost/data
```

### 7. 启动

```bash
cd /srv/reasonix-selfhost
docker compose up -d --build
docker compose ps
```

两个服务都应显示 `running`。

## 接上电脑

电脑上的内核通过两个环境变量访问这两个服务。别的不动：

```bash
REASONIX_ACCOUNTS_URL=https://id.example.com
REASONIX_REMOTE_URL=https://relay.example.com
```

`REASONIX_REMOTE_URL` 写成 `https://` 或 `wss://` 都行，内核会自己把前者改写成
后者。账号服务交出去的本来就是 `https://`，这里写成同样的形式最不容易出岔子。
**公网**主机写成 `wss://` 也可以；明文 `ws://` 只在本机回环上被接受。

重启 Studio，然后用账号服务登录一次。首次启动它会调 `POST /me/devices`，注册
自己的 X25519 公钥，并保存拿到的设备凭据。此后它就是电脑在中继上的身份证明。

## 接上手机

1. **设置 → 账号 → 服务地址**：填 `https://id.example.com` 并保存。这是手机唯一
   一次得知你的服务在哪。
2. 点**登录**，服务返回一个短码。
3. 批准它。两种方式，都需要管理员令牌：
   - 在 App 里把令牌填进码旁边的输入框；
   - 或点「在网页中批准」，打开 `https://id.example.com/device?code=XXXX-XXXX`
     在那里输入。
4. 手机自己完成设备码轮询，保存会话令牌。中继连接使用账号服务返回的地址。

App 的账号面板与批准页共用同一个凭据，所以两条路径不会在「谁可以批准」上出现分歧。

## 端到端验证

按顺序往下走，哪一步失败就停下看[故障排查](#故障排查)。

**1. 端口通** —— 在你自己的电脑上，不是服务器上：

```bash
curl -I http://id.example.com
```

跳转到 `https://` 说明 80 通。超时说明端口被封。

**2. 证书签出来了：**

```bash
docker compose logs caddy | grep -iE 'certificate|error'
```

应当看到两次 `certificate obtained successfully`，每个主机名一次。

**3. 服务在应答：**

```bash
curl -s https://id.example.com/healthz
```

期望 `{"ok":true}`。

**4. 设备码流程能启动：**

```bash
curl -s -X POST https://id.example.com/device/start
```

期望 JSON 里含 `userCode`、`deviceCode`，且 `verificationUriComplete` **以你自己的
主机名开头**。如果写着 `example.com`，说明第 3 步的 `PUBLIC_URL` 没生效。

**5. 手机能离开 Wi-Fi 连上电脑。** 关掉手机 Wi-Fi，确认连接显示为云端，发一条消息。
这是唯一一条真正跑中继、而不只是账号服务的检查。

## 日常运维

**日志。** `docker compose logs -f selfhost` —— 每个请求一行，永远不会有令牌或验证码。

**重启。** `docker compose restart selfhost`。中继 socket 都在内存里，重启会断开每一台
连着的手机；App 会自己退避重连。

**备份。** 数据库是唯一的持久状态——账号、设备、凭据、已登记的手机都在
`data/accounts.db` 里：

```bash
docker compose stop selfhost
cp data/accounts.db "accounts-$(date +%F).db"
docker compose start selfhost
```

先停再拷很重要：WAL 模式的 SQLite 文件旁边还有日志，服务正在写时拷贝，
备份可能打不开。

**升级：**

```bash
git -C app pull
docker compose up -d --build
```

**轮换管理员令牌。** 改 `selfhost.env` 里的 `REASONIX_SELFHOST_ADMIN_TOKEN` 后重启。
已发出的会话继续有效；只有之后的批准需要新令牌。

## 故障排查

**Caddy 日志里 `challenge failed` 或反复的 ACME 报错。**
80 端口从公网不通。先查云厂商的安全组——主机防火墙开着是不够的。在中国大陆，
域名未备案也会卡在这里。

**Caddy 起来了，站点打不开，Caddy 日志又什么都没有。**
域名还没解析到这台机器。从服务器外面 `nslookup id.example.com`，并记住 DNS 缓存
可能活得比记录的 TTL 更久。

**`{"ok":true}` 正常，但 `/device/start` 返回的链接指向 `example.com`。**
`REASONIX_SELFHOST_PUBLIC_URL` 还是占位符，或者容器没读到改过的 `selfhost.env`——
改过之后要 `docker compose up -d`。

**服务起不来：`SELFHOST_ADMIN_TOKEN is required`。**
令牌缺失、短于 16 字符，或者环境变量文件根本没被读（`env_file` 路径不对，
或者 `chmod 600` 给了错误的属主）。

**手机连上了但每个请求都失败。**
账号服务返回的中继地址手机够不着，或者 `REASONIX_SELFHOST_RELAY_URL` 与 Caddy 服务的
主机名不一致。去 `docker compose logs selfhost` 里看 `/v1/sessions/connect` 的记录。

**Studio 提示「中继地址无法识别」。**
`REASONIX_REMOTE_URL` 里不是一个纯 origin。它必须是 `https://主机` 或 `wss://主机`，
不带路径、查询串、fragment 或 `user:pass@`。

**Studio 提示「还没有配置中继地址」。**
`REASONIX_REMOTE_URL` 为空。这条消息与「中继连不上」是刻意分开的：要改的是变量，
不是网络。

## 参考

### 环境变量

| 变量 | 必填 | 含义 |
|---|---|---|
| `REASONIX_SELFHOST_PUBLIC_URL` | ✅ | 账号服务的公网 origin，批准链接按它拼 |
| `REASONIX_SELFHOST_RELAY_URL` | ✅ | 中继的公网 origin，`/me/remote-grants` 原样返回给客户端 |
| `REASONIX_SELFHOST_ADMIN_TOKEN` | ✅ | 唯一的人工凭据，至少 16 字符 |
| `REASONIX_SELFHOST_ALLOWED_ORIGINS` | | 允许带凭据跨域调用的 origin，逗号分隔。**留空等于整个关掉 CORS** |
| `REASONIX_SELFHOST_ADDR` | | 监听地址，默认 `127.0.0.1:8790`（镜像设为 `0.0.0.0:8790`） |
| `REASONIX_SELFHOST_DATA` | | 数据目录，默认 `data`（镜像设为 `/data`） |
| `REASONIX_SELFHOST_OWNER_HANDLE` | | 账号句柄，默认 `owner` |
| `REASONIX_SELFHOST_OWNER_EMAIL` | | 账号邮箱，默认空 |
| `REASONIX_SELFHOST_GRANT_TTL` | | 设备码有效期，默认 `10m` |
| `REASONIX_SELFHOST_TICKET_TTL` | | 中继票据有效期，默认 `2m` |

### 端点

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/healthz` | 存活探针 |
| `POST` | `/device/start` | 开一次登录，返回两个码 |
| `POST` | `/device/poll` | 查询是否已批准；完成后返回会话令牌 |
| `GET` | `/device` | 批准页 |
| `POST` | `/device/approve` · `/device/deny` | 批准 / 拒绝（管理员令牌） |
| `GET` | `/me` | 当前账号 |
| `POST` | `/auth/logout` | 吊销当前令牌 |
| `GET` `POST` | `/me/devices` | 列出 / 注册 Studio |
| `DELETE` | `/me/devices/{id}` | 吊销一台 Studio |
| `POST` | `/me/remote-grants` | 换一张一次性中继票据，并返回中继地址 |
| `POST` | `/host/push/notify` | 电脑上报"有东西在等"（设备凭据） |
| `GET` | `/host/controllers` | 登记在这台电脑上的手机 |
| `POST` | `/host/controllers/{id}/resolve` | 答复一台等待中的手机：`add`、`replace` 或 `reject` |
| `POST` | `/host/controllers/{id}/revoke` | 结束一台已登记的手机 |
| `GET` | `/host/controllers/{id}/report` | 这台手机有什么在等，若没有则为空 |
| `POST` | `/host/controllers/{id}/seen` | 标记手机已读过上报 |
| `GET` | `/v1/devices/{id}/connect` | Studio 的中继 socket（Bearer 设备凭据） |
| `GET` | `/v1/sessions/connect` | 手机的中继 socket（票据子协议） |

### 值得知道的边界

- **中继只在内存里记连接。** 重启会断开每一台手机；它们会退避重连，而且没有
  待补发的消息队列需要对账。
- **数据库是唯一的持久状态。** 按上面的方法备份它，否则账号与所有已注册设备一起丢。
- **没有推送通知。** 后台的手机靠轮询 `/host/controllers/{id}/report` 得知有东西在等，
  而不是被推送。
- **没有账号找回。** 管理员令牌是最后一句话。丢了它，唯一的路是用新数据库重新部署。

### 测试

```bash
go test ./...
```

`internal/api/api_test.go` 对着真实的 SQLite 文件跑完整链路：设备码流程、批准页、
设备注册、两端 socket，以及真正要紧的那些拒绝——未配置的 origin 永远不被反射，
错的令牌永远批准不了。