# 实现契约与运维

这里记录各模块实际兑现的行为边界、systemd 集成方式和常见故障的排查顺序。
README 只讲「能做什么」，本文讲「做到什么程度、为什么这样设计」。

## 资源与范围

- 面板空载 RSS ≤ 50MB，二进制 13MB（发布上限 25MB）
- 面板 + Xray + Nginx + PHP 按 320MB 以内设计，512MB 可用内存下仍有余量
- 不做容器管理、不做大而全的应用市场、不做多节点

## 安全契约

| 能力 | 说明 |
|---|---|
| 认证 | argon2id（m=19MiB，低内存档）、12 位起步的密码策略、弱密码表 |
| 会话 | 令牌只存 SHA-256、HttpOnly + SameSite=Lax、改密后踢掉所有旧会话 |
| 防爆破 | 同 IP 5 次失败封 1 小时，进程内计数 |
| CSRF | 双提交 Cookie + `X-CSRF-Token` 请求头 |
| 响应头 | CSP、HSTS、nosniff、DENY frame、no-referrer |
| 审计 | 只追加表，`UPDATE`/`DELETE` 由 SQLite 触发器拦截 |
| 监控 | 直接读 `/proc`，不依赖 cgo 与第三方库 |
| 部署 | systemd 单元带 `ProtectSystem=full`（/usr、/boot、/etc 只读）与能力收敛 |

> 前端全程零构建原生 JS（约 2000 行），目的是让 `go build` 不依赖 Node。
> 需要更强的交互时再上 Vue，二进制接口不变（ADR-05）。

## 代理（Xray）契约

| 能力 | 说明 |
|---|---|
| 协议 | VLESS（REALITY / TLS / 无加密）、VMess、Trojan、Shadowsocks（2022 多用户） |
| 传输 | TCP、WebSocket、gRPC；REALITY 限 tcp/grpc |
| 配置下发 | 生成 → `xray -test` → 备份 → 原子替换 → 重启 → 等端口就绪；任一步失败自动回滚 |
| 进程托管 | 子进程 + 指数退避重拉（1s→30s），面板退出时一并收掉，不留孤儿 |
| 流量统计 | 走 `xray api statsquery`，不引入 gRPC/protobuf 依赖；游标做差，进程重启不清零 |
| 配额 | 超额/到期自动禁用并重建配置，界面状态与实际一致 |
| 分享链接 | vless / vmess / trojan / ss 四种；入站卡片上一键复制，也可扫码；二维码服务端生成，前端零依赖 |
| 密钥 | REALITY X25519 密钥对、UUIDv4、SS-2022 定长 PSK 均由服务端生成，表单里不用填 |

### 新建入站的默认行为

端口在 20000–59999 里随机挑一个（避开面板端口、Xray 内部接口与已有入站），同时自动建一个
名为 `default` 的用户 —— 建完立刻能「复制链接」，不用再点一次「+ 用户」。REALITY 的私钥、
公钥、shortId 全部自动生成，表单只需要伪装目标与 SNI（默认 `www.cloudflare.com`）。
`xtls-rprx-vision` 只在 TCP 上生效，创建用户时自动带上；gRPC 不带（带了会让客户端握手失败）。

### NAT / 端口映射的机器

云厂商 NAT 机型、LXC、容器 VPS 要额外填「对外端口」：容器内监听 47736、商家把公网 30986
转发进来时，分享链接必须写 30986。两处可填，任选其一：

- 设置里的「公网地址」直接写 `IP:端口`，相当于全局默认
- 入站的「对外端口」，只影响这一个入站，优先级高于全局

都留空则链接用监听端口 —— 独立 IP、直接暴露端口的机器不用管。

### 连不上时的排查顺序

一步排除一层：

```bash
# 1) 服务端有没有人在听（没输出 = Xray 没起来）
ss -lntp | grep -E '47736|30986'

# 2) Xray 到底怎么了（面板托管的子进程，日志在这里，不在 journald）
tail -n 40 <data_dir>/xray.log     # 默认 /opt/panelvps/data/xray.log

# 3) 面板自己看到的 Xray 状态
systemctl is-active panelvps
```

```powershell
# 4) 在你自己电脑上测公网端口（不是服务器上）
Test-NetConnection 204.197.163.238 -Port 30986
```

第 4 步不通 = 端口映射/商家防火墙没放行，与面板和 Xray 无关；通了但客户端仍失败 =
REALITY 层，先核对客户端里的**公钥（pbk）与 shortId** 是否与面板「链接」里的一致
（手填极容易漏）。

## 运维契约

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

## 网站与软件商店契约

| 能力 | 说明 |
|---|---|
| 站点 | 站点名 `^[a-z0-9][a-z0-9-]{1,30}$`，根目录必须在 `sites_root` 下；配置写入 → `nginx -t` → reload，失败回滚旧配置 |
| 证书 | certbot webroot 模式（HTTP-01），异步任务执行；域名与邮箱先校验格式 |
| 软件商店 | 清单只允许 7 种动作：`apt / download / extract / write_file / path / service / check`，**不含自由脚本** |
| 安装前检查 | 已装则跳过、冲突与依赖校验、下载必须 https 且拒绝内网/回环地址、剩余内存不足一半直接拦截 |
| 目录预检 | 动手前先对计划写入的目录做真实写入探测，不可写就在下载安装包之前失败，并给出修复命令 |
| 回滚 | 任一步失败按逆序撤销已执行的写文件/服务动作；非 Linux 直接 501，不假装能装 |

## 备份与自更新

| 能力 | 说明 |
|---|---|
| 备份 | `VACUUM INTO` 取数据库一致性快照 + 配置 + 版本信息，打包 tar.gz；文件名白名单正则防穿越 |
| 恢复 | 需 `confirm=RESTORE`；先做安全备份，再校验 SQLite 头，替换后重启面板 |
| 清理 | 保留最近 N 份（默认 30），按文件名时间戳排序 |
| 告警 | 单一通用 Webhook（POST JSON），5 分钟内相同标题去重，避免告警风暴 |
| 自更新 | 只信任 https + GitHub 域名的 release 资源；下载后校验 sha256，替换前校验 ELF 头，保留 `.old-<时间戳>`；打开面板与设置页自动检查，服务端缓存 10 分钟避免打满 GitHub 未认证限流 |

## systemd 沙箱与可写目录

从 v0.3.5 起，单元用 `ProtectSystem=full` 并明确放开 `/usr`、`/etc`、`/var`，`/boot` 与
`/efi` 仍然只读。之前是「full + 零散白名单」，踩过两次坑：

- 白名单只放行了 `/etc/nginx/conf.d` 等几个子目录，`apt-get` 装包时写 `/etc/nginx`、
  `/var/lib/dpkg` 全线失败；
- 白名单里的路径**不存在时 systemd 会静默跳过**（`-` 前缀），目录没建就没有 bind mount，
  面板再往里 `mkdir` 就报 `read-only file system` —— 软件商店装 Xray 时
  `mkdir /usr/local/share/xray` 失败正是这个原因。

老版本（≤ v0.3.4）原地修复：

```bash
# 补目录后重启，让 bind mount 重新解析；不升级也能装上 Xray
sudo mkdir -p /usr/local/share/xray && sudo systemctl restart panelvps
```

升级到 v0.3.5+ 会重写单元、预创建常用目录，以上都不必手动做。
`sudo ./install.sh --check` 可复核当前单元与目录状态。

从 v0.3.6 起，「设置 → 服务单元」会比对磁盘上的单元与当前二进制内置的版本：不一致时显示
「需要更新」，点「用内置版本覆盖」即可写入并 `daemon-reload`（旧文件备份为 `.bak-时间戳`），
再点「重启面板」生效。这条路径解决的是**「设置 → 版本更新」只换二进制、不换 systemd 单元**
的问题。

注意：面板自带的 Web 终端不能用来修这类问题 —— 它是面板的子进程，继承同一份只读 mount
namespace；`sudo` 也会失败（`setresuid(...): Operation not permitted`），因为单元里开了
`RestrictSUIDSGID`。终端本身已经是 root，不需要 `sudo`。

## 入口与内核调优

装完之后才需要的操作分两处：**网页在「设置」页**，**命令行敲 `panelvps` 进菜单**。
不再单独占一个导航标签——控制页要拉 BBR 状态，打开得等一两秒，而这些操作本来一个月也用不上一次。

| 能力 | 网页 | 命令行 |
|---|---|---|
| 访问方式与端口 | 设置 → 访问方式 | 菜单第 6 项 |
| 重置密码 | 设置 → 账号 | 菜单第 7 项 |
| BBR 开关 | — | 菜单第 8 项 |
| 重启面板 / Xray / 服务器 | 设置 → 重启面板；代理页重启 Xray | 菜单第 3 项 / 第 10 项 |

网页改端口同样是「先 `net.Listen` 试绑 → 改配置 → 按新端口重新下发防火墙 → 重启」，防火墙
失败则回滚配置并报错；命令行菜单走的是同一套 `internal/manage` 实现。

BBR 这一项没有为了它放宽主服务的沙箱。主单元带 `ProtectKernelTunables=yes`，进程本身写不了
`/proc/sys`，与其为了一个开关拆掉加固，不如让一个独立的 oneshot 单元去干这件事：

```ini
# /etc/systemd/system/panelvps-sysctl.service
ExecStartPre=-/sbin/modprobe tcp_bbr
ExecStart=/sbin/sysctl -p /etc/sysctl.d/99-panelvps.conf
```

面板只负责写文件，再 `systemctl restart panelvps-sysctl` 让配置立即生效，不用 reboot。
配置文件首行带 `# Managed by PanelVPS`，不是本面板生成的文件（比如你自己手写的 tuning）会
拒绝覆盖，避免把别人的内核参数一起冲掉。

## 实测记录

**M1（2026-11-21，Xray 26.11.9）**

- 五种入站组合生成的配置全部通过真实 `xray -test`
- 客户端经 VLESS 入站访问 `gstatic/generate_204` 返回 204、访问 `api.github.com` 返回 200
- 流量准确记到对应用户名下（`alice` 独占总量，另两人为 0）
- 把配额改成 1000 字节后，下一次采样自动禁用该用户并把其从配置中剔除

**M2–M4 端到端（2026-11-21，本地实例 + 假 nginx，82 项）**

- 认证：未登录 401、错密码 401、缺 CSRF 403、弱密码 400、改密后旧会话失效
- 文件：穿越 403、绝对路径按根相对、上传下载字节一致、打包解压往返、删除需 `confirm=DELETE`
- 防火墙：端口 70000 与 `1-2; rm -rf /`、来源 `1.2.3.4; reboot` 全部 400；无后端时 503
- 站点：`../evil` 与非法域名 400、重名/重域名 409、预览含 `server_name` 与点文件拒绝
- 商店：12 个清单可读，步骤可预览，非 Linux 安装/卸载 501
- 备份：任务是异步的、产物是合法 tar.gz 且含 `panel.db`、`../` 名称 400、恢复需确认词
- 更新：非 GitHub 来源 400
