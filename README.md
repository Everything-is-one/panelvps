# PanelVPS

> 用 Go 写的单二进制 VPS 管理面板，带 Xray 代理管理，512MB 可用内存就能跑。

[![CI](https://github.com/Everything-is-one/panelvps/actions/workflows/ci.yml/badge.svg)](https://github.com/Everything-is-one/panelvps/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8.svg)](https://go.dev/)
[![Platform](https://img.shields.io/badge/linux-amd64%20%7C%20arm64-lightgrey.svg)](#一键安装)

## 这是什么

PanelVPS 把 VPS 上常做的几件事放进一个网页：看资源、改文件、开终端、查日志、配防火墙、建站，
另外带一套 Xray 代理管理（VLESS / VMess / Trojan / Shadowsocks，多用户、流量统计、配额与到期），
不用再单独装 x-ui。

只有一个二进制，前端资源在编译时打包进去，数据存 SQLite，运行时不依赖 Docker、Python 或 Node。
空载内存 50MB 左右，512MB 可用内存的机器上还能留出空间跑站点和代理。

## 当前状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| M0 | 骨架：CLI、SQLite、认证、会话、安全默认值、embed 前端、healthz | ✅ 已完成 |
| M1 | 代理：Xray 编排、入站/客户端、流量统计、分享链接 | ✅ 已完成 |
| M2 | 运维：监控、文件、终端、防火墙、日志 | ✅ 已完成 |
| M3 | 网站与商店：站点/证书/PHP、声明式软件商店 | ✅ 已完成 |
| M4 | 打磨：备份、告警、自更新、签名发布 | ✅ 已完成（签名发布未做） |
| M5 | 控制：公网访问与端口、重置密码、BBR、重启 | ✅ 已完成 |

## 资源与范围

- 面板空载 RSS ≤ 50MB，二进制 13MB（发布上限 25MB）
- 面板 + Xray + Nginx + PHP 按 320MB 以内设计，512MB 可用内存下仍有余量
- 不做容器管理、不做大而全的应用市场、不做多节点

## 功能一览

| 页面 | 能做什么 |
|---|---|
| 概览 | CPU / 内存 / 磁盘 / 面板占用，地址、数据库、审计条数、运行时长 |
| 控制 | 访问方式与端口、重置密码、BBR 开关、重启面板 / Xray / 服务器、系统信息 |
| 代理 | 入站与用户管理、分享链接与二维码、流量采样、配额与到期 |
| 网站 | 建站（静态 / PHP）、生成并下发 Nginx 配置、申请证书、预览配置 |
| 文件 | 浏览、编辑、上传下载、打包解压、重命名/复制/删除（沙箱限定根目录） |
| 终端 | 浏览器里的 PTY，支持 Ctrl 组合键与粘贴，会话 30 分钟、并发 2 |
| 防火墙 | nftables / iptables 双后端、规则编辑、一键下发、临时封禁、SSH 爆破榜 |
| 日志 | 面板 / Xray / SSH / Nginx / syslog 六个固定来源，可按行数尾部读取 |
| 商店 | 11 个软件的声明式清单，可预览每一步再安装，失败自动回滚 |
| 备份 | 一键备份（含数据库一致性快照）、下载、恢复（二次确认 + 安全备份）、自动清理 |
| 设置 | 公网地址、告警 Webhook、SSH 防护开关与阈值、自更新检查与应用 |

命令行另有一个管理菜单，敲 `panelvps` 就能启停服务、改端口、重置密码、开 BBR、重启服务器。

## 快速开始

### 从源码构建

```bash
go build -trimpath -ldflags "-s -w" -o bin/panelvps ./cmd/panelvps
```

交叉编译两个架构：

```bash
./scripts/build.sh
```

### 本地运行

```bash
./bin/panelvps serve
```

首次启动会自动创建管理员账号，**初始密码只打印在 stdout**（不落盘），形如：

```
========================================
  首次启动，已创建管理员账号
  用户名: admin
  密  码: XXXXXXXXXXXXXXXX
========================================
```

默认只监听 `127.0.0.1:20873`，浏览器直接访问即可。

### 一键安装

Debian 12 / Ubuntu 22.04+，amd64 与 arm64 均支持。自动判断架构、下载对应二进制、校验 SHA256、
装好 systemd 服务，最后直接把初始密码打印出来。

```bash
# 最新版
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh | sudo bash
```

生产环境建议锁定版本，让脚本和二进制取同一个 tag，避免 `main` 更新后错配：

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/v0.3.0/deploy/bootstrap.sh \
    | sudo bash -s -- --ref v0.3.0
```

只体检、不安装（逐项报告能力，不改动系统）：

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh \
    | sudo bash -s -- --check
```

`bootstrap.sh` 的常用参数（透传给 `install.sh`）：

| 参数 | 作用 |
|---|---|
| `--ref TAG` | 锁定版本，脚本与二进制取同一个 tag |
| `--base URL` | 换分发源，见下表 |
| `--public` | 装完直接监听 `0.0.0.0`，可用 `IP:端口` 访问 |
| `--check` | 只体检，不安装 |

卸载走 `deploy/` 目录里的安装脚本，数据目录会保留：

```bash
sudo ./deploy/install.sh --uninstall
```

`bootstrap.sh` 取文件的来源按优先级：

| 来源 | 用法 | 适用 |
|---|---|---|
| GitHub Release | 默认，`--ref` 可锁定 tag | 首选 |
| `--base URL` | `--base https://dl.example.com/panelvps` | 国内访问 GitHub 慢，自建分发 |
| 本地文件 | `sudo ./deploy/install.sh` | 已手工上传安装包 |

跑脚本本身走 `raw.githubusercontent.com`，下二进制走 Release。这两条路在国内都可能被干扰，
所以 `bootstrap.sh` 内置了回退：直链返回非 200 时自动改用 `api.github.com` 取同一份资产，
两条都不通才会报错。实测 `github.com/releases/download` 会间歇性返回 502，而 API 通道稳定。

仍拉不动时，用发布脚本把安装包推到自己有 HTTPS 的目录，再带 `--base` 安装：

```bash
# 本机执行：交叉编译 → 生成 SHA256SUMS → 上传
./scripts/publish.sh root@1.2.3.4:/www/wwwroot/dl.example.com/panelvps \
    --base https://dl.example.com/panelvps

# 目标机器
curl -fsSL https://dl.example.com/panelvps/bootstrap.sh | sudo bash
```

### 安装后访问

面板默认只监听 `127.0.0.1:20873`，不暴露公网。用 SSH 隧道访问：

```bash
ssh -L 20873:127.0.0.1:20873 root@<服务器IP>
# 浏览器打开 http://127.0.0.1:20873
```

想直接用 `IP:端口` 打开，安装时加 `--public`：

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh \
    | sudo bash -s -- --public
```

装完会打印访问地址，并提醒去云厂商安全组放行该端口。已经装好的也不用重装：
登录后进「控制 → 访问方式」，勾上「允许从公网访问」并保存，面板会改监听地址、同步防火墙规则、
然后自己重启，用新地址就能打开。

同一个地方还能改端口（1–65535）。保存顺序是先试绑新端口 → 更新防火墙 → 重启面板，
任何一步失败都回滚，不会出现「新端口没放行、旧端口已关」把自己锁在门外。

> 面板自己不做 HTTPS，公网直连就是明文 HTTP。要么在前面套一层 TLS 反向代理，
> 要么至少用 16 位以上随机密码，并且只放行自己的出口 IP。

### 在 LXC / 容器里安装

LXC 小鸡共享宿主内核，容器内的 systemd 通常**没有** `CAP_NET_ADMIN` / `CAP_SYS_ADMIN`，
主单元里的 `AmbientCapabilities`、`MemoryMax`、`ProtectSystem=strict` 会让服务直接启动失败。
安装脚本会自动识别容器并装上降级覆盖（`deploy/panelvps-container.conf`），不需要手工改。

先体检，看清楚这台机器能用到哪些功能：

```bash
sudo ./deploy/install.sh --check
```

再安装：

```bash
sudo ./deploy/install.sh
sudo journalctl -u panelvps | grep 初始管理员     # 取初始密码
```

`--check` 会逐项报告：

| 输出 | 含义 | 非特权 LXC 的典型结果 |
|---|---|---|
| `CAP_NET_ADMIN` | 防火墙能否下发规则、能否封禁 IP | ❌ 只能查看，不能下发 |
| `CAP_NET_BIND_SERVICE` / `ip_unprivileged_port_start` | 能否监听 80 / 443 | 视商家配置而定，常见 ❌ |
| 防火墙后端 | 实际走 nftables 还是 iptables-legacy | ❌ none |
| PTY | Web 终端能否连接 | ✅ 一般可用（需挂载 devpts） |
| journald | 日志页与初始密码 | ✅ |
| nginx / certbot / xray | 站点、证书、代理 | 需自行安装 |

**能力不足时的替代路径**

- **绑不了 80/443**：让宿主设置 `net.ipv4.ip_unprivileged_port_start=0`
  （LXC 配置 `lxc.sysctl.net.ipv4.ip_unprivileged_port_start = 0`），
  或让 Nginx 听 8080、由宿主 Nginx / 端口映射转发 80 → 8080。
  站点配置里的 `listen` 目前固定 80/443，走这条路需要先在容器内用
  `sysctl -w net.ipv4.ip_unprivileged_port_start=0` 验证是否允许。
- **防火墙不可下发**：安全边界退化为「只监听 127.0.0.1 + SSH 隧道」。
  在容器里刷 iptables 会和宿主规则打架，商家一般也不允许，不建议强求。
- **想在 NAT 小鸡上直连面板**：进「控制 → 访问方式」勾上公网访问，或把 `conf/panel.yaml`
  的 `listen` 改成 `0.0.0.0:20873`，再用商家给的端口映射访问；只做临时测试，测完改回 `127.0.0.1`。

**只想先跑起来看看**（不装 systemd 服务）：

```bash
mkdir -p /opt/panelvps/{bin,conf,data}
install -m 0755 panelvps-linux-amd64 /opt/panelvps/bin/panelvps
/opt/panelvps/bin/panelvps serve -config /opt/panelvps/conf/panel.yaml
```

首次启动会打印初始密码；`Ctrl-C` 即停，数据留在 `/opt/panelvps/data`。

### 管理菜单

装完不用记子命令，敲 `panelvps` 回车就是个数字菜单：

```
PanelVPS 0.2.0  管理菜单
────────────────────────────────────────────────────
 服务 : 运行中 (pid 2314) · 开机自启
 监听 : 127.0.0.1:20873（仅本机）
 BBR  : 未开启（cubic）
────────────────────────────────────────────────────
  1) 启动面板
  2) 停止面板
  3) 重启面板
  4) 查看运行状态
  5) 查看最近日志
  6) 监听地址与端口（IP:端口 外网访问）
  7) 重置账号密码
  8) BBR 加速开关
  9) 显示面板信息
 10) 重启服务器
  0) 退出
```

- **6) 监听地址**：选「公网直连」再填端口，配置落盘后自动重启，并打印可用地址。已下发过防火墙规则的会按新端口重下一遍，失败则回滚配置——不会留下「新端口没放行、旧端口已关」的状态
- **7) 重置密码**：随机 20 位（只显示一次）或自己输入，改完踢掉该账号所有会话
- **8) BBR**：先显示当前的拥塞控制与队列算法，开启后写入 `/etc/sysctl.d/99-panelvps.conf`，由 helper 单元立即应用，重启后仍然有效
- **10) 重启服务器**：要输入 `REBOOT` 确认

没有 systemd 的环境（非特权容器、前台跑二进制）里，服务相关的项会说明不可用，改配置和重置密码照常可用。

### 常用命令

```bash
panelvps                  # 管理菜单（默认）
panelvps serve            # 前台启动服务
panelvps version          # 版本
panelvps info             # 配置、路径、账号数
panelvps init             # 创建管理员
panelvps reset-password   # 重置为随机密码并踢掉所有会话
```

安装脚本会在 `/usr/local/bin/panelvps` 建一个软链，所以任何目录下敲 `panelvps` 都行；
systemd 单元用的是 `/opt/panelvps/bin/panelvps serve` 绝对路径，不受影响。

### 验收测试

装完先跑一遍冒烟测试，它会逐项报告哪些能力在这台机器上**真的**可用：

```bash
pip3 install requests          # 唯一依赖
PV_PASSWORD='初始密码' python3 scripts/smoke_test.py
```

只读模式 31 项，不改任何数据。加 `--write` 会额外验证上传 / 编辑 / 打包 / 解包、
防火墙规则增删、建站与删站；文件放在 `_smoke` 临时目录、站点用 `smoke-*` 命名，结束时全部清理。

```
共 31 项：通过 27，失败 0，跳过 4
```

**SKIP 不是错误**：它表示该能力在当前环境不可用，例如非特权容器拿不到 `CAP_NET_ADMIN`、
Windows 上无法创建 PTY。有 FAIL 时脚本以退出码 1 结束，可用于 CI。

## M0 已实现的契约

| 能力 | 说明 |
|---|---|
| 认证 | argon2id（m=19MiB，低内存档）、12 位起步的密码策略、弱密码表 |
| 会话 | 令牌只存 SHA-256、HttpOnly + SameSite=Lax、改密后踢掉所有旧会话 |
| 防爆破 | 同 IP 5 次失败封 1 小时，进程内计数 |
| CSRF | 双提交 Cookie + `X-CSRF-Token` 请求头 |
| 响应头 | CSP、HSTS、nosniff、DENY frame、no-referrer |
| 审计 | 只追加表，`UPDATE`/`DELETE` 由 SQLite 触发器拦截 |
| 监控 | 直接读 `/proc`，不依赖 cgo 与第三方库 |
| 部署 | systemd 单元带 `ProtectSystem=strict` 与能力收敛 |

> 前端全程零构建原生 JS（约 2000 行），目的是让 `go build` 不依赖 Node。
> 需要更强的交互时再上 Vue，二进制接口不变（ADR-05）。

## M1 已实现的契约

| 能力 | 说明 |
|---|---|
| 协议 | VLESS（REALITY / TLS / 无加密）、VMess、Trojan、Shadowsocks（2022 多用户） |
| 传输 | TCP、WebSocket、gRPC；REALITY 限 tcp/grpc |
| 配置下发 | 生成 → `xray -test` → 备份 → 原子替换 → 重启 → 等端口就绪；任一步失败自动回滚 |
| 进程托管 | 子进程 + 指数退避重拉（1s→30s），面板退出时一并收掉，不留孤儿 |
| 流量统计 | 走 `xray api statsquery`，不引入 gRPC/protobuf 依赖；游标做差，进程重启不清零 |
| 配额 | 超额/到期自动禁用并重建配置，界面状态与实际一致 |
| 分享链接 | vless / vmess / trojan / ss 四种；二维码服务端生成，前端零依赖 |
| 密钥 | REALITY X25519 密钥对、UUIDv4、SS-2022 定长 PSK 均由服务端生成 |

**M1 实测结论**（2026-11-21，Xray 26.11.9）：

- 五种入站组合生成的配置全部通过真实 `xray -test`
- 客户端经 VLESS 入站访问 `gstatic/generate_204` 返回 204、访问 `api.github.com` 返回 200
- 流量准确记到对应用户名下（`alice` 独占总量，另两人为 0）
- 把配额改成 1000 字节后，下一次采样自动禁用该用户并把其从配置中剔除

## M2 已实现的契约（运维）

| 能力 | 说明 |
|---|---|
| 文件沙箱 | 所有路径先 `Clean` + 相对化 + 对最近已存在祖先做 `EvalSymlinks`，越界直接 403；写盘走临时文件 rename |
| 上传 / 下载 | 流式，单文件上限 512MB；响应只回文件名与相对路径，不回绝对路径 |
| 归档 | tar.gz / zip 打包与解包；zip slip 拦截、20000 条与 2GB 解压上限（压缩炸弹） |
| 服务与进程 | 服务名正则白名单、动作白名单（start/stop/restart/reload/enable/disable） |
| Web 终端 | PTY + WebSocket；一次性票据（2 分钟、单次使用），不把 Cookie 放进 URL；shell 必须是绝对路径 |
| 防火墙 | `nft` 建表探测失败自动降级 `iptables-legacy`；SSH 与面板端口**永远排在用户 drop 规则之前**；nft set 自带 timeout 实现到期自动解封 |
| 日志 | 六个固定来源，不接受任意路径；只 seek 文件末尾 512KB，不会把整个日志读进内存 |
| 异步任务 | 内存态执行器，并发 2、单任务 15 分钟超时、日志上限 256KB，避免 apt/证书撑爆 HTTP 超时 |

## M3 已实现的契约（网站与商店）

| 能力 | 说明 |
|---|---|
| 站点 | 站点名 `^[a-z0-9][a-z0-9-]{1,30}$`，根目录必须在 `sites_root` 下；配置写入 → `nginx -t` → reload，失败回滚旧配置 |
| 证书 | certbot webroot 模式（HTTP-01），异步任务执行；域名与邮箱先校验格式 |
| 软件商店 | 清单只允许 7 种动作：`apt / download / extract / write_file / path / service / check`，**不含自由脚本** |
| 安装前检查 | 已装则跳过、冲突与依赖校验、下载必须 https 且拒绝内网/回环地址、剩余内存不足一半直接拦截 |
| 回滚 | 任一步失败按逆序撤销已执行的写文件/服务动作；非 Linux 直接 501，不假装能装 |

## M4 已实现的契约（备份与自更新）

| 能力 | 说明 |
|---|---|
| 备份 | `VACUUM INTO` 取数据库一致性快照 + 配置 + 版本信息，打包 tar.gz；文件名白名单正则防穿越 |
| 恢复 | 需 `confirm=RESTORE`；先做安全备份，再校验 SQLite 头，替换后重启面板 |
| 清理 | 保留最近 N 份（默认 30），按文件名时间戳排序 |
| 告警 | 单一通用 Webhook（POST JSON），5 分钟内相同标题去重，避免告警风暴 |
| 自更新 | 只信任 https + GitHub 域名的 release 资源；下载后校验 sha256，替换前校验 ELF 头，保留 `.old-<时间戳>` |

**M2–M4 端到端实测**（2026-11-21，本地实例 + 假 nginx，82 项）：

- 认证：未登录 401、错密码 401、缺 CSRF 403、弱密码 400、改密后旧会话失效
- 文件：穿越 403、绝对路径按根相对、上传下载字节一致、打包解压往返、删除需 `confirm=DELETE`
- 防火墙：端口 70000 与 `1-2; rm -rf /`、来源 `1.2.3.4; reboot` 全部 400；无后端时 503
- 站点：`../evil` 与非法域名 400、重名/重域名 409、预览含 `server_name` 与点文件拒绝、删除后配置同步清理
- 商店：11 个清单可读，步骤可预览，非 Linux 安装/卸载 501
- 备份：任务是异步的、产物是合法 tar.gz 且含 `panel.db`、`../` 名称 400、恢复需确认词
- 更新：非 GitHub 来源 400

## 控制页与内核调优

「控制」页把装完之后才需要的操作集中起来，省得为了改个端口去翻配置文件。

| 能力 | 说明 |
|---|---|
| 访问方式 | 默认只监听 `127.0.0.1`；切到公网后页面列出所有可用地址，标出是否只有 HTTP |
| 改端口 | 先 `net.Listen` 试绑 → 改配置 → 按新端口重新下发防火墙 → 重启；防火墙失败则回滚配置并报错 |
| 重置密码 | 随机（20 位，只显示一次）或自定义；走同一套密码策略，改完删掉该账号的全部会话 |
| 重启 | 面板 / Xray / 整机三档；整机重启要输入 `REBOOT` 确认，用 `systemctl --no-block` 不卡住请求 |
| BBR | 读写 `/etc/sysctl.d/99-panelvps.conf`；先探测内核有没有 `tcp_bbr`，没有就置灰并说明原因 |

BBR 这一项没有为了它放宽主服务的沙箱。主单元带 `ProtectKernelTunables=yes`，进程本身写不了 `/proc/sys`，
与其为了一个开关拆掉加固，不如让一个独立的 oneshot 单元去干这件事：

```ini
# /etc/systemd/system/panelvps-sysctl.service
ExecStartPre=-/sbin/modprobe tcp_bbr
ExecStart=/sbin/sysctl -p /etc/sysctl.d/99-panelvps.conf
```

面板只负责写文件，再 `systemctl restart panelvps-sysctl` 让配置立即生效，不用 reboot。
配置文件首行带 `# Managed by PanelVPS`，不是本面板生成的文件（比如你自己手写的 tuning）会拒绝覆盖，
避免把别人的内核参数一起冲掉。

控制接口：

```
GET    /api/v1/control                  面板 / 账号 / BBR / Xray / 防火墙 / 系统信息
PUT    /api/v1/control/access           改监听地址与端口（confirm=ACCESS）
POST   /api/v1/control/password         重置密码（confirm=RESET，mode=random|custom）
POST   /api/v1/control/bbr              开 / 关 BBR（confirm=BBR）
POST   /api/v1/control/xray/restart     重启 Xray
POST   /api/v1/control/reboot           重启服务器（confirm=REBOOT）
```

## 代理接口

```
GET    /api/v1/proxies                     概览（状态 + 入站 + 用户 + 链接）
POST   /api/v1/proxies/apply               重建并下发配置
POST   /api/v1/proxies/traffic/collect     立即采样一次流量
PUT    /api/v1/proxies/preferences          设置分享链接用的公网地址
POST   /api/v1/proxies/keys/reality         生成 REALITY 密钥对
POST   /api/v1/proxies/inbounds             新建入站
PATCH  /api/v1/proxies/inbounds/{id}        修改入站（部分字段）
DELETE /api/v1/proxies/inbounds/{id}        删除入站（级联删除用户）
POST   /api/v1/proxies/inbounds/{id}/clients 新建用户
PATCH  /api/v1/proxies/clients/{id}         修改用户
DELETE /api/v1/proxies/clients/{id}         删除用户
POST   /api/v1/proxies/clients/{id}/reset   重置流量
GET    /api/v1/proxies/clients/{id}/links   分享链接
GET    /api/v1/proxies/clients/{id}/qr      二维码 PNG
```

Xray 可执行文件按以下顺序探测：`PANELVPS_XRAY_BIN` 环境变量 → 配置项 `xray_bin`
→ `/usr/local/bin/xray` → `/usr/bin/xray` → 面板同目录 → `PATH`。
找不到时面板照常启动，只是代理页显示「不可用」。


## 目录结构

```
cmd/panelvps/        CLI 入口（serve / init / info / reset-password）
internal/
  auth/              密码哈希、密码策略、会话令牌、登录限流
  backup/            备份打包、恢复、保留策略
  config/            YAML 配置与校验
  filemgr/           文件沙箱、浏览、读写、归档解包
  firewall/          nftables / iptables 双后端、规则渲染与封禁
  jobs/              异步任务执行器
  logs/              固定来源的日志尾部读取、SSH 失败解析
  metrics/           /proc 指标采集
  notify/            Webhook 告警
  selfupdate/        GitHub release 检查、校验与替换
  server/            路由、中间件、API 处理器
  sites/             Nginx 站点渲染、校验、证书
  softstore/         声明式软件清单与安装器（manifests/*.yaml）
  store/             SQLite 访问层与迁移（v1 认证 / v2 代理 / v3 运维）
  sysinfo/           进程、服务、端口、网卡
  terminal/          PTY 会话
  version/           构建期注入的版本信息
  xray/              配置生成、进程托管、统计读取、分享链接与二维码
web/                 embed 的前端静态资源（零构建原生 JS）
deploy/              systemd 单元、容器覆盖、安装与一键引导脚本
scripts/             构建、发布（publish.sh）与冒烟测试
```

## 文档

| 文件 | 说明 |
|---|---|
| [PRDv0.3.md](PRDv0.3.md) | **当前有效版本**：需求、架构、安全设计、ADRs |
| [PRDv0.2.md](PRDv0.2.md) | 上一版 |
| [PRDv0.1.txt](PRDv0.1.txt) | 原始三句话需求 |
| [PROVENANCE.md](PROVENANCE.md) | 代码来源与许可合规记录 |

## 许可

[Apache License 2.0](LICENSE)。可以商用、修改、闭源分发，需保留版权与许可声明，并附带 NOTICE 类声明；
贡献者授予专利许可，同时不得用专利起诉本项目使用者。

本项目采用洁净室实现，未复制 GPL 代码，因此许可选择不受上游传染；详见 [PROVENANCE.md](PROVENANCE.md)。

## 参与

- **安全问题**：请不要开 public issue，走 GitHub 的私有漏洞报告（Security → Report a vulnerability）
- **功能与缺陷**：开 issue 时附上发行版、架构、`panelvps info` 输出与相关日志
- **代码**：fork → 分支 → 提 PR；提交前跑 `go vet ./...`、`go test ./...`、`python -m py_compile scripts/smoke_test.py`

