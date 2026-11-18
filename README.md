# Reasonix SelfHost

上游 `esengine/DeepSeek-Reasonix` 归档后，托管服务（`id.reasonix.io`、`remote.reasonix.io`）
一并下线。这个仓库是那部分的最小替代：**一个二进制 + 一个 SQLite 文件**，
自己跑在自己机器上，让手机在任何网络下都能连到家里的 Studio。

它不重写协议。设备码登录、`/me/devices`、`/me/remote-grants`、中继的
`relay_hello` / `controller_message` 帧，全部是已发布的客户端与内核**已经在用**的形状，
所以自托管只是换一个 base URL。

## 它是什么，不是什么

**是**：账号服务（设备码登录 + 一次性票据 + 设备吊销）和中继（只转发密文，
不持有任何密钥，握手与内容由两端以 X25519 + AES-GCM 端到端加密）。

**不是**：多租户、密码登录、邮件验证、附件存储、崩溃上报。一个人、一个账号、
一台设备或几台设备。需要这些功能时，请另找服务，而不是往这里加。

## 跑起来

```bash
go build ./cmd/reasonix-selfhost
REASONIX_SELFHOST_PUBLIC_URL=https://id.example.com \
REASONIX_SELFHOST_RELAY_URL=https://relay.example.com \
REASONIX_SELFHOST_ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '/+=') \
REASONIX_SELFHOST_ALLOWED_ORIGINS=https://id.example.com \
REASONIX_SELFHOST_DATA=/var/lib/reasonix \
  ./reasonix-selfhost
```

启动即监听 `127.0.0.1:8790`。TLS 交给反向代理：见 `deploy/Caddyfile`
（两个域名，一台进程）与 `deploy/Dockerfile`。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `REASONIX_SELFHOST_PUBLIC_URL` | ✅ | 账号服务的公网 origin，验证链接按它拼 |
| `REASONIX_SELFHOST_RELAY_URL` | ✅ | 中继的公网 origin，`/me/remote-grants` 原样返回给客户端 |
| `REASONIX_SELFHOST_ADMIN_TOKEN` | ✅ | 批准页的唯一人工凭据，至少 16 字符 |
| `REASONIX_SELFHOST_ALLOWED_ORIGINS` | | 允许带凭据跨域调用的 origin，逗号分隔。**不填就谁都不许** |
| `REASONIX_SELFHOST_ADDR` | | 默认 `127.0.0.1:8790` |
| `REASONIX_SELFHOST_DATA` | | 默认 `data`，SQLite 落在这里 |
| `REASONIX_SELFHOST_OWNER_HANDLE` | | 默认 `owner` |
| `REASONIX_SELFHOST_OWNER_EMAIL` | | 默认为空 |
| `REASONIX_SELFHOST_GRANT_TTL` | | 设备码有效期，默认 `10m` |
| `REASONIX_SELFHOST_TICKET_TTL` | | 中继票据有效期，默认 `2m` |

## 登录一次

1. 客户端调 `POST /device/start`，拿到 `userCode` 与验证链接。
2. 人在浏览器打开 `https://id.example.com/device?code=ABCD-EFGH`。
3. 页面要求管理令牌，确认后批准；客户端下一次 `POST /device/poll` 拿到
   `sessionToken`。
4. Studio 用这个 token 调 `POST /me/devices` 注册自己的 X25519 公钥，换回
   `deviceCredential`，然后以它连 `GET /v1/devices/{id}/connect`。
5. 手机调 `POST /me/remote-grants` 换一张一次性票据，以子协议
   `reasonix.auth.<ticket>` 连 `GET /v1/sessions/connect`。

票据是单次消耗的（`DELETE ... RETURNING`），所以一个被截获的子协议开不了第二条连接。

## 内核与客户端的接线

内核两个变量指向这里，其它不动：

```bash
REASONIX_ACCOUNTS_URL=https://id.example.com
REASONIX_REMOTE_URL=https://relay.example.com
```

> `REASONIX_REMOTE_URL` 需要 `wss://` 或对回环地址的 `ws://`：设备凭据不能明文上线。

## 端点

| 方法 | 路径 | 作用 |
|---|---|---|
| `POST` | `/device/start` | 开一次登录 |
| `POST` | `/device/poll` | 查询是否已批准；成功时消耗票据并发 session token |
| `GET` | `/device` | 批准页 |
| `POST` | `/device/approve` · `/device/deny` | 批准 / 拒绝（需管理令牌） |
| `GET` | `/me` | 当前账号 |
| `POST` | `/auth/logout` | 吊销当前 token |
| `GET` `POST` | `/me/devices` | 列出 / 注册 Studio |
| `DELETE` | `/me/devices/{id}` | 吊销一台 Studio |
| `POST` | `/me/remote-grants` | 换一张中继票据，并返回中继地址 |
| `GET` | `/v1/devices/{id}/connect` | Studio 的中继 socket（Bearer 设备凭据） |
| `GET` | `/v1/sessions/connect` | 手机的中继 socket（子协议票据） |
| `GET` | `/healthz` | 存活探针 |

## 测试

```bash
go test ./...
```

`internal/api/api_test.go` 跑的是完整链路：设备码、批准页、注册设备、
两端 socket 各发一帧，以及「未配置的 Origin 不得被反射」与「错的令牌不能批准」。

## 边界

- 中继只在内存里记连接，进程重启后手机需要重连（客户端的退避阶梯会处理）。
- 数据库是唯一的持久状态，备份它等于备份账号与设备列表。
- `REASONIX_SELFHOST_ALLOWED_ORIGINS` 为空时 CORS 全关：在浏览器里跑的页面
  调不通，App 与内核不受影响。
