# 不用 Docker 部署

[English](DEPLOY-SYSTEMD.md) | 中文 | [回到主指南](../README.zh-CN.md)

主指南装的是两个容器。这一份用 systemd 跑同一份服务的静态二进制，Caddy 装在宿主机上
终结 TLS。不想跑 Docker，或者这台机器已经在用宿主 Caddy 服务别的站点，就选这条。

其余一切——两个主机名、管理员令牌、`.env` 的内容、电脑与手机那几步、以及验证清单——
与主指南完全一致。这份文档替换的是构建、服务用户、unit，以及 Caddy 跑在哪里；
其余照着主指南做即可。

**为什么这条路在这里很合适：**这个二进制不依赖 cgo，也不依赖 libc（SQLite 用的是纯 Go 的
`modernc.org/sqlite`），所以它是一个文件、在任何 Linux 上行为一致；而宿主 Caddy 可以
和你的其它站点一起服务它，不抢端口。

## 前置条件

- 一台有 root 的 Linux 机器，80 和 443 可访问。
- Go 1.26 或更新版本用来构建，**或者**在别处构建好再把二进制拷过来。
- 宿主机上装了 Caddy（`caddy --version`）。

## 1. 构建

在服务器上，或在任何同架构的机器上：

```bash
sudo mkdir -p /opt/reasonix-selfhost
cd /opt/reasonix-selfhost
git clone https://github.com/f1shsmell/Reasonix-SelfHost.git app
cd app
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/reasonix-selfhost ./cmd/reasonix-selfhost
```

跑一下，看它拒绝半份配置——这同时证明二进制和校验都在工作：

```bash
reasonix-selfhost
# selfhost: REASONIX_SELFHOST_PUBLIC_URL is required (e.g. https://id.example.com)
```

这个报错在这里就是正确答案。配置不全时服务拒绝启动，而不是带着半份配置跑起来。

## 2. 建立服务用户与目录

服务不该以 root 运行，也需要自己的数据目录：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin reasonix-selfhost
sudo mkdir -p /var/lib/reasonix-selfhost
sudo chown reasonix-selfhost:reasonix-selfhost /var/lib/reasonix-selfhost
sudo chmod 700 /var/lib/reasonix-selfhost
```

## 3. 写环境变量文件

内容与主指南一致，只是路径不同。放在 `/etc` 下是因为它装着管理员令牌：

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

这个文件属主是 `root:`，只有服务组可读：systemd 以 root 读它，服务用户不需要更多权限。
这里写成 `600 root:root` 是经典错误——unit 起来了，systemd 却没法把变量交给一个
读不了该文件的用户，服务随即退出并抱怨 `PUBLIC_URL` 缺失。

`ADDR` 保持默认 `127.0.0.1:8790`：宿主 Caddy 在那里访问它，机器上别的东西够不着。
不要改成 `0.0.0.0`——那会把明文端口直接暴露出去。

管理员令牌与主指南同一条命令生成：

```bash
head -c 32 /dev/urandom | base64 | tr -d '/+=' | cut -c1-40
```

## 4. 写 unit

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

# 服务只需要自己的数据目录，别处都要不可写。这样服务里的 bug 会在这里响亮地
# 失败，而不是去碰机器上的其它东西。
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

期望 `active (running)`，并有一行日志报出你的两个 origin。

## 5. 让 Caddy 指向它

仓库里的 `deploy/Caddyfile` 原样可用——它已经反代到 `127.0.0.1:8790`。复制过去，
然后按主指南改成你自己的名字：

```bash
sudo cp /opt/reasonix-selfhost/app/deploy/Caddyfile /etc/caddy/Caddyfile
sudo $EDITOR /etc/caddy/Caddyfile
sudo grep -nE 'example\.com' /etc/caddy/Caddyfile    # 期望无输出
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

如果宿主 Caddy 已经在服务别的站点，要**追加**这两个站点块，而不是替换整个文件——
替换会静默丢掉那里原本配置的所有站点。

`caddy validate` 在 reload 之前就抓出语法错误；而 reload 失败时旧配置继续服务，
所以这里写错不会把你别的站点一起带下线。

## 6. 验证

与主指南同样的五项检查，只是两条命令换了：

```bash
curl -I http://id.example.com
curl -s https://id.example.com/healthz
curl -s -X POST https://id.example.com/device/start
sudo journalctl -u caddy -n 50 | grep -iE 'certificate|error'
```

然后按主指南完成电脑与手机那几步，以及关掉 Wi-Fi 的检查。

## 日常运维

**日志。** `sudo journalctl -u reasonix-selfhost -f` —— 每个请求一行。

**重启。** `sudo systemctl restart reasonix-selfhost`。中继 socket 都在内存里，
重启会断开每一台连着的手机；它们会退避重连。

**升级：**

```bash
cd /opt/reasonix-selfhost/app
sudo -u reasonix-selfhost git pull     # 或者用 root 拉再 chown
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/reasonix-selfhost ./cmd/reasonix-selfhost
sudo systemctl restart reasonix-selfhost
```

服务运行期间替换二进制不会生效，直到重启——所以这两步是一对。

**备份。** 与主指南同一条规矩，只是换了路径：

```bash
sudo systemctl stop reasonix-selfhost
sudo cp /var/lib/reasonix-selfhost/accounts.db "/var/backups/accounts-$(date +%F).db"
sudo systemctl start reasonix-selfhost
```

先停再拷很重要：WAL 模式的 SQLite 在文件旁边还有日志，写入过程中拷贝，
备份可能打不开。

## 故障排查

主指南故障排查里的每一条都适用。还有三条是这条路特有的：

**`status=203/EXEC`。** systemd 执行不了这个二进制。通常是少了 `chmod +x`、路径不对，
或者——在同架构不同的机器上——构建出来的二进制架构不匹配。

**`status=209/STDOUT` 或服务立刻退出却没有任何日志。**
环境变量文件读不到。检查 `EnvironmentFile=` 是否指向 `/etc/reasonix-selfhost/env`，
以及 unit 的用户能不能读它。

**`ProtectSystem=strict` 挡住了写入。** 数据目录必须在 `ReadWritePaths=` 里。
如果你改过 `REASONIX_SELFHOST_DATA`，要把它一起更新到 unit 里——否则服务建不出
`accounts.db` 然后退出。

**改完之后 Caddy reload 失败。** 旧配置继续服务，你现有的站点不受影响；
`caddy validate` 会指出是哪一行错了。