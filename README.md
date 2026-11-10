# cookie-sync

将浏览器中的 `_AJSESSIONID` 同步到运行服务的用户的 `~/.bilicookie`，支持手动同步和登录后的自动同步。需要 Go 1.21 或以上版本，无第三方依赖。

## 自动同步流程

1. 办公电脑通过 hosts 将 `dashboard-mng.biliapi.net` 指向运行本服务的 nuc。
2. 本服务在 HTTPS 443 端口代理登录站点，其他路径的请求直接转发给真实上游。
3. 浏览器 GET `/api/v4/user/dashboard_login?caller=...&path=...` 时，服务返回 302，将浏览器带到 `http://nuc.bilibili.co:7070/sync-cookie?state=...`。
4. 浏览器携带匹配 `nuc.bilibili.co` 的 `_AJSESSIONID`，服务将其原子写入 Cookie 文件。
5. 服务返回 302，将浏览器带回登录回调，校验回跳状态后恢复原始查询串并转发给上游。最终返回地址仍由上游依据 `caller/path` 决定。

回跳状态使用启动时随机生成的密钥签名，5 分钟内有效，不包含 Cookie。重启服务后，进行中的登录需要重新发起。状态仅用于保护回跳参数，不是用户认证或一次性登录凭据。保留参数 `_cookie_sync_resume` 供内部回跳使用。

仅拦截登录回调的 GET 请求，POST 等请求直接转发，以免 302 丢失请求体。上游的状态码、响应体、Location 和 Set-Cookie 保留；登录回调增加禁止缓存和禁止 Referer 的响应头。

## 启动

### 本地直接运行（无需 systemd）

项目已准备好程序和证书时，在普通用户终端执行：

```bash
# 首次运行时授予监听 443 的权限；已经设置过则无需重复执行。
sudo setcap 'cap_net_bind_service=+ep' ./cookie-sync
./run.sh
```

脚本默认加载项目内的 `certs/dashboard.crt` 和 `certs/dashboard.key`，监听 HTTPS 443 和 HTTP 7070。日志直接输出到终端，按 Ctrl+C 停止。脚本可以从任意目录通过完整路径调用，并将所有参数传给程序：

```bash
./run.sh -upstream-addr <真实上游IP>:443
./run.sh -h
```

以 `deng` 用户运行脚本，默认写入该用户的 `~/.bilicookie`，不要用 `sudo ./run.sh`，以免写入 root 的家目录。如需后台运行：

```bash
nohup ./run.sh > cookie-sync.log 2>&1 &
echo $!
```

保存输出的 PID，停止时执行 `kill <PID>`。如果之前已经通过 systemd 启动本服务，先停止该实例，避免争用端口。

脚本不会自动重新编译。源码更新后执行 `go build -o cookie-sync .`，然后重新设置 `setcap` 并启动。已有进程需要先停止，避免继续运行旧版本。

### 当前 nuc 的 systemd 部署

此节描述本机已有配置；`deploy/` 和 `certs/` 均不纳入版本控制，新克隆的仓库请使用上面的本地启动方式并自行准备证书。

本机已准备 `cookie-sync` 可执行文件、`certs/` 下的证书和 `deploy/cookie-sync.service`。在 nuc 的普通终端运行：

```bash
cd /home/deng/code/personal/cookie-sync
sudo ./deploy/install.sh
```

安装脚本会检查证书和端口冲突、安装服务、启用开机启动并重启服务。服务以 `deng` 用户运行，由 systemd 授予监听 443 的能力，无需额外执行 `setcap`。脚本仅测试本地 HTTPS 跳转和同步端口的 OPTIONS 请求，不发送 Cookie，也不覆盖 `~/.bilicookie`。占用端口的其他进程不会被自动终止。

办公电脑只需复制并信任 `certs/root-ca.crt`，不要复制 `root-ca.key` 或 `dashboard.key`。服务证书 `dashboard.crt` 包含两个域名，当前有效期为 2026-11-10 至 2027-11-10。根证书通过 DNS 名称约束限制签发范围，根私钥不对服务进程开放。

```bash
# 在办公电脑上获取公开的根证书，然后导入系统或浏览器的受信任根证书库。
scp deng@nuc.bilibili.co:/home/deng/code/personal/cookie-sync/certs/root-ca.crt .
```

安装证书后仍需按下文配置办公电脑的 hosts。不要将 nuc 上的登录域名解析指回自身。

```bash
systemctl status cookie-sync.service
journalctl -u cookie-sync.service -n 50 --no-pager

# 停止并取消开机启动。
sudo systemctl disable --now cookie-sync.service
```

当前受限会话无法连接 systemd 或监听网络端口，因此这里只完成了部署文件准备和证书验证；服务是否已启动，以普通终端执行安装脚本后的检查结果为准。

### 手动启动

准备 PEM 证书和私钥，证书 SAN 必须包含 `dashboard-mng.biliapi.net`，并在办公电脑上信任对应签发机构或证书。证书和私钥放到 `certs/` 下，该目录已加入 Git 忽略规则。

```bash
go build -o cookie-sync .

# Linux 普通用户监听 443，需要授予绑定低端口的权限。
sudo setcap 'cap_net_bind_service=+ep' ./cookie-sync

./cookie-sync \
  -tls-cert ./certs/dashboard.crt \
  -tls-key ./certs/dashboard.key \
  -cookie-file /home/deng/.bilicookie
```

以 `deng` 用户运行服务，确保文件属主正确。每次重新构建二进制后，需要重新设置绑定端口的权限。仅修改 hosts 无法改变端口，办公电脑使用默认 HTTPS 地址时，nuc 必须在 443 端口提供此代理。

办公电脑的 hosts 增加以下条目，将占位符换成 nuc 的实际 IP：

```text
<NUC_IP> dashboard-mng.biliapi.net
```

办公电脑还需要能将 `nuc.bilibili.co` 解析到 nuc。只在办公电脑修改登录站点的 hosts；nuc 上应继续解析到真实上游。

如果 nuc 上也存在 hosts 覆盖，可以指定真实上游 IP：

```bash
./cookie-sync \
  -tls-cert ./certs/dashboard.crt \
  -tls-key ./certs/dashboard.key \
  -upstream-addr <真实上游IP>:443
```

指定 IP 只改变 TCP 连接目标，HTTP Host、TLS SNI 和证书校验仍使用 `dashboard-mng.biliapi.net`。上游请求不使用环境中的 HTTP(S) 代理。检测到代理请求返回自身时响应 508，避免递归转发；指定的真实 IP 发生变化后需要更新配置。

## Cookie 条件与 HTTPS 同步

默认沿用 HTTP 同步地址，因此 `_AJSESSIONID` 必须满足浏览器的发送条件：Domain 匹配 `nuc.bilibili.co`（通常为 `.bilibili.co`）、Path 匹配 `/sync-cookie`、未设置 Secure，且 SameSite 策略允许此次跨站顶层导航。`dashboard-mng.biliapi.net` 下的 Cookie 不会因为 302 自动跨域传给 nuc。

若 Cookie 设置了 Secure，需为证书 SAN 增加 `nuc.bilibili.co`，并同时使用：

```bash
./cookie-sync \
  -tls-cert ./certs/dashboard.crt \
  -tls-key ./certs/dashboard.key \
  -sync-tls \
  -sync-url https://nuc.bilibili.co:7070/sync-cookie
```

HTTP 模式会在办公电脑与 nuc 之间明文传输 Cookie。服务用于个人内网机器，应限制两个监听端口的访问范围，避免暴露给不相关的设备。

同步时若没有收到有效 Cookie、状态过期或写入失败，会显示错误并停止回跳，保留原有 Cookie 文件。文件采用临时文件加原子替换写入，权限为 `0600`，日志不输出凭证内容。多个登录同时发生时，以最后完成写入的请求为准。

## 手动同步与参数

不提供证书时，仍可用 `go run .` 仅启动原来的 HTTP 同步接口。手动 GET/POST `/sync-cookie` 携带 `_AJSESSIONID` 后返回 JSON；自动同步无需页面 JavaScript。手动接口保留原有的跨域行为，因此应通过主机防火墙限制访问来源。

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `-addr` | `:7070` | Cookie 同步监听地址 |
| `-proxy-addr` | `:443` | HTTPS 登录代理监听地址 |
| `-sync-url` | `http://nuc.bilibili.co:7070/sync-cookie` | 浏览器的同步目标 |
| `-tls-cert` / `-tls-key` | 空 | 证书及私钥，成对提供后开启代理 |
| `-sync-tls` | `false` | 同步端口启用 HTTPS |
| `-upstream-addr` | 空 | 可选的真实上游 IP:端口 |
| `-cookie-file` | `~/.bilicookie` | Cookie 保存路径 |

## 验证

```bash
go test -race -buildvcs=false ./...
go vet ./...
```

测试使用模拟上游和内存 TLS 连接，不访问真实登录服务。部署后在浏览器开发者工具的 Network 中开启 Preserve log，重新登录，确认跳转链为“登录回调 → nuc 同步 → 登录回调 → 原页面”，并确认 Cookie 文件更新时间变化。不要分享包含 Cookie 请求头的完整网络日志。
