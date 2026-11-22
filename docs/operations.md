# 实现契约与运维

这里记录各模块实际兑现的行为边界、systemd 集成方式和常见故障的排查顺序。
README 只讲「能做什么」，本文讲「做到什么程度、为什么这样设计」。

## 资源与范围

- 面板空载 RSS ≤ 50MB，二进制 13MB（发布上限 25MB）
- 面板 + Xray + Nginx + PHP 按 320MB 以内设计，512MB 可用内存下仍有余量
- 不做容器管理、不做大而全的应用市场、不做多节点

## 内存口径

概览把内存切成三档，三档必然满足 `实际 + 可回收 + 空闲 = 总量`：

| 档 | 算法 | 含义 |
|---|---|---|
| 实际 | `MemTotal - MemAvailable` | 真占住了、要回收就得动手的量 |
| 可回收 | `MemAvailable - MemFree` | 内核认为能收回来还给应用的部分（页缓存 + 可回收 slab） |
| 空闲 | `MemFree` | 真正没人用的页，与 `free(1)` 的 free 列一致 |

早先「缓存」取 `Cached + Buffers + SReclaimable - Shmem`、「空闲」用减法推，两列会打架：
页缓存里有一部分页内核不会算进 `MemAvailable`（块设备元数据页、被映射钉住的页），
它既算在「实际」里、又被全量缓存算一遍，同一块内存记了两次。现场一台 960MB 的机器上
`free -m` 报空闲 327MB，面板只敢报 171MB。现在空闲直接取 `MemFree`，可回收取两者之间
余下的部分，差额另存为「页缓存（暂不可回收）」放进「实际」的构成里。

> `free -m` 的三列同样不能相加：它的 used 是 `总量 - available`，buff/cache 却是全量缓存，
> 现场那台 `291 + 327 + 499 = 1117 > 960`。这不是内存泄漏，是两列口径不同。面板做的是严格三分。

**「实际」为什么远大于前 5 名进程之和？** 因为这个数是全机的，而进程排行只列头部：

| 构成 | 来源（`/proc/meminfo`） | 说明 |
|---|---|---|
| 进程匿名 | `AnonPages` | 堆、栈、Go heap、`mmap(MAP_ANON)`，**所有进程**加总 |
| tmpfs / 共享内存 | `Shmem` | 无 swap 收不回，所以算实际占用 |
| 内核 | `SUnreclaim + KernelStack + PageTables` | slab、内核栈、页表 |
| 页缓存（暂不可回收） | 全量缓存 - 可回收 | 内核不肯算进 available 的那部分缓存，实测常是最大的一块 |
| 其它 | 余项 | per-cpu、vmalloc、驱动缓冲等 |

容器口径下这几项改读 `memory.stat`（`anon` / `shmem` / `slab_unreclaimable`），
因为 `/proc/meminfo` 在容器里报的是宿主机的数。

还有一处容易误会：**RSS 会重复计算共享页**（每个进程都把自己映射到的共享库算进
自己的 RSS），所以「所有进程 RSS 之和」不能和「实际占用」直接相减。它只适合回答
「前 5 之外的那些进程大概占多少」。实测一台机器上 RSS 合计 181MB、`AnonPages` 只有 56MB，
差的 125MB 就是被各进程重复计入的共享页。

排查用的命令：

```bash
free -m
grep -E 'MemTotal|MemAvailable|MemFree|Buffers|Cached|SReclaimable|Shmem|AnonPages|SUnreclaim|KernelStack|PageTables' /proc/meminfo
ps -eo rss= | awk '{s+=$1} END {printf "所有进程 RSS 合计 %.0f MB\n", s/1024}'
sudo slabtop -o -s c | head -12
```

### 面板自己占多少，要不要降

960MB 的机器上面板常驻 RSS 在 50–60MB 之间（含 Go runtime 的 `mheap` 元数据、栈、
GC 结构，约 10–15MB 是改不掉的地板）。想降的话，先看清楚这三件事**都不是**原因：

| 常被怀疑的项 | 实际情况 |
|---|---|
| 概览每 3 秒轮询 | 每次只读 `/proc/stat`、`/proc/meminfo`、`/proc/loadavg` 与一次 `statfs`，合计几十 KB 瞬时分配，GC 后即回收。把间隔从 3 秒调到 10 秒，稳态 RSS 基本不动 |
| 磁盘目录扫描 | **不是常驻任务**：只有鼠标停在「磁盘」卡片上才异步遍历一次，扫完即释放，3 分钟内复用结果 |
| 进程排行采样 | 2 秒 TTL + 全局互斥，同一时刻只有一个采集者；不悬停根本不跑 |

真正的上限是服务单元里的两个环境变量：

```ini
Environment=GOMEMLIMIT=64MiB   # 堆的目标上限，不是硬上限
Environment=GOGC=50            # 每增长 50% 触发一次 GC
```

`GOMEMLIMIT` 给的是「堆可以长到多大」的预算，RSS 贴近 64MB 说明它在按设计工作。
想再省一点，把它调到 `48MiB`：

```bash
sudo systemctl edit panelvps
# 写入：
# [Service]
# Environment=GOMEMLIMIT=48MiB
sudo systemctl restart panelvps
```

预期收益 5–10MB RSS，代价是 GC 更频繁（1 核机器上 CPU 会略升）。**960MB 的机器上省这
10MB 换不来什么**，除非你同时在跑数据库或编译任务。真要压内存，收益大得多的是：

- `journalctl --vacuum-size=50M` ＋ `/etc/systemd/journald.conf` 里 `SystemMaxUse=50M`
  （日志常驻 15MB 起，还会涨）；
- 别让面板去扫全盘目录（那是悬停才发生的一次性开销，不是泄漏）；
- 页缓存高不是问题 —— 它在内存紧张时会被内核直接丢弃，`free` 的 `buff/cache` 列不参与压力判断。

## 安全契约

| 能力 | 说明 |
|---|---|
| 认证 | argon2id（m=19MiB，低内存档）、12 位起步的密码策略、弱密码表 |
| 会话 | 令牌只存 SHA-256、HttpOnly + SameSite=Lax、改密后踢掉所有旧会话 |
| 防爆破 | 同 IP 10 分钟内失败 5 次封 1 小时；达到 `ssh_guard_perma_threshold`（默认 10）直接永久封禁 |
| 暴露面 | 可设访问后缀 `access_path`，不带前缀的路径（含 `/api/v1/auth/login`）一律 404 |
| 归属地 | 攻击记录按需查询来源国家，结果缓存 12 小时，查询失败只留空 |
| CSRF | 双提交 Cookie + `X-CSRF-Token` 请求头 |
| 响应头 | CSP、HSTS、nosniff、DENY frame、no-referrer |
| 审计 | 只追加表，`UPDATE`/`DELETE` 由 SQLite 触发器拦截 |
| 监控 | 直接读 `/proc`，不依赖 cgo 与第三方库 |
| 部署 | systemd 单元默认放开写权限、只读点名 `/boot` `/efi`，外加能力收敛与 `MemoryMax` |

> 前端全程零构建原生 JS（约 2000 行），目的是让 `go build` 不依赖 Node。
> 需要更强的交互时再上 Vue，二进制接口不变（ADR-05）。

## 安全运维：端口、封禁与访问后缀

### 改 SSH 端口（`panelvps` 第 11 项）

默认 22 端口每天要吃几百到几千次爆破尝试。改端口的顺序是固定的，任一步失败整份回滚：

1. 备份 `sshd_config` 与所有 `sshd_config.d/*.conf`
2. 注释掉所有生效的 `Port` 行 —— sshd 的 `Port` 是**累加**的，留着旧值就会同时监听两个端口
3. 在主配置最前面写 `Port <新端口>`（排在 `Include` 之前，即使 drop-in 里又冒出一条也压得住）
4. `sshd -t` 校验语法与 host key
5. 放行 ufw（若启用）与面板自己的规则链，并把新端口写进 `ssh_ports`
6. `systemctl reload ssh`（RHEL 是 `sshd`），失败再退到 `pkill -HUP sshd`

**只 reload 不 restart**：reload 不会掐断已建立的连接，而 restart 会让当前会话断在改配置的
中途。改完先别关当前窗口，另开一个终端用 `ssh -p <新端口>` 验证成功，再去云厂商安全组
放行同一个端口。

### 封禁的生命周期

| 场景 | 触发 | 时长 |
|---|---|---|
| 自动临时封禁 | 窗口内失败次数 ≥ `ssh_guard_threshold`（默认 5） | `ssh_guard_ban`（默认 1h） |
| 自动永久封禁 | 窗口内失败次数 ≥ `ssh_guard_perma_threshold`（默认 10） | 十年（见下） |
| 手动封禁 | 安全页「封禁 N 分钟」或「永久封禁」 | 自选 / 永久 |

nftables 后端的 set **不再声明默认 timeout**，每条元素自己带 `timeout`。之前 set 上写了
`timeout 1h`，任何不显式指定超时的元素都会被压成一小时，永久封禁也就名存实亡。

「永久」在两个后端都实现为十年：nft 没有「永不过期」这个取值，十年对一台 VPS 等同于永久，
好处是不必为永久封禁单独建集合、单独改链，升级时规则不会对不上。界面上显示为「永久」。

iptables 后端没有超时能力，到期时刻写进规则注释（`panelvps-ban:<unix>`，0 表示永久），
由每分钟的后台任务 `SweepExpiredBans` 清理。**0.3.16 及更早这里是个 bug**：注释是固定
字符串，没有任何清理逻辑，所谓「封 60 分钟」在 iptables 模式下永远不会解除。

`Apply`（点「下发规则到内核」）会 flush 整张 nft 表，集合里的元素也会被清空。现在会在
flush 前记下现有封禁、重建后按剩余时间补回，避免一次下发把所有攻击者放出来。

### 访问后缀

设置 `access_path` 后，只有 `/<后缀>/...` 能进来：

- 根路径、`/.env`、`/wp-login.php`、`/api/v1/auth/login` 全部 404，且不跳转、不返回 403
  —— 任何「这里有个面板」的信号都会让扫描器换条路继续试
- `/healthz` 保持原样，监控脚本不受影响
- 前端通过 `index.html` 里的 `<meta name="pv-base">` 得到前缀，接口、静态资源、WebSocket
  与下载链接都会自动带上（内联 `<script>` 会被 CSP 的 `script-src 'self'` 拦掉，所以用 meta）
- 改完立即生效，页面会自动跳到新地址；忘了后缀可用 `panelvps` 第 12 项查看或修改

这是**收窄暴露面**，不是认证手段：真正的防线仍然是强密码、登录限流与 TLS。

### IP 归属地

安全页的「归属地」列走在线查询（ip-api.com，中文国名），结果在内存里缓存 12 小时。
只发送攻击来源 IP 本身，不发送任何面板数据。查询失败（机器不出网、接口不可用）时该列
显示「—」，不影响其余功能；也可以关掉 `geoip_enabled` 省下这几秒。

### 默认放行与默认拒绝

面板的防火墙有两种链尾策略，安全页「模式」卡片显示当前是哪一种：

| 模式 | 内核里的链尾 | 实际效果 |
|---|---|---|
| **默认放行**（黑名单，出厂值） | `policy accept` | 规则列表里没有的端口**本来就能连通**，规则只用来拦你显式加进去的 IP 或端口 |
| **默认拒绝**（白名单） | `counter drop` | 除已放行的端口外一律拒绝 |

这里有个容易误解的地方：出厂是「默认放行」，此时你新建中转、新建入站、装个 Redis
都能直接连上——**不是因为防火墙放行了它们，而是因为它根本没在拦**。以为自己在用
防火墙挡端口、实际全开，是这类面板最常见的安全错觉。安全页顶部的橙色提示条就是
在讲这件事。

点「切换为默认拒绝」时：

- 面板先扫一遍 `/proc/net/*`，把**当前对外监听**（非 `127.0.0.1`）的端口逐个补一条
  `accept` 规则，再改链尾为 `drop`。不这么做，点下去的瞬间站点、宝塔、Docker 的端口
  会一起断掉
- 确认框会先把这批端口列出来，看清楚再点
- SSH 端口与面板端口由 `Render` 强制排在最前（`ssh (panelvps safety)` /
  `panel (panelvps safety)`），任何模式下都不会把自己锁在外面
- 只看回环的端口不加规则——它们本来就不该对外

之后新建入站、中转时会自动补对应端口的放行规则，备注带 `[auto]` 前缀；删除或改端口
时收回。手工加的规则不带这个前缀，永远不会被自动清理。

切回「默认放行」只删掉那条链尾 `drop`，已放行的规则全部保留。

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

## 忘记密码：先确认服务在用哪个库

重置密码本身只有一条命令，但「重置成功却还是登不上」几乎都是同一个原因：
**CLI 改的库和服务读的库不是同一个文件**。

```bash
sudo /opt/panelvps/bin/panelvps info                      # 服务实际打开哪个库
sudo /opt/panelvps/bin/panelvps reset-password -user admin # 重置为随机 20 位
```

`info` 会打印两件事：配置解析出的数据库路径，以及从 `/proc/<pid>/fd` 反查到的
**服务真正打开的文件**。两者不一致时会直接给出结论，并列出其它位置上的
「影子库」——那些库通常是 CLI 在别的目录里跑出来的，里面也有 admin，看起来
一切正常，实际上和 Web 端毫无关系。

指定密码（仍走同一套策略校验）：

```bash
sudo /opt/panelvps/bin/panelvps reset-password -user admin -password '你的新密码'
```

配置读不出来、或者要越过配置直接改某个文件时用 `-db`：

```bash
sudo /opt/panelvps/bin/panelvps reset-password -db /opt/panelvps/data/panel.db -user admin
```

### 为什么会出现两个库

历史版本按**进程工作目录**解析 `data_dir`（默认 `./data`）。服务由 systemd
拉起，工作目录固定 `/opt/panelvps`，解析成 `/opt/panelvps/data`；而人在 `/root`
里敲 `panelvps`，同一个 `./data` 解析成 `/root/data`。CLI 发现库里没有账号会
新建一份，于是：

- 菜单提示「已重置密码」，Web 端始终 `账号或密码错误`；
- 服务日志里还留着一条首次启动时打印的初始密码；
- 两边都不报错，所以极难自查。

从 0.4.3 起，相对路径以**配置文件所在的安装前缀**为基准（`<prefix>/conf/panel.yaml`
→ `<prefix>`），与工作目录无关。升级不会改变既有安装的数据库位置：新解析出的
目录不存在、而按工作目录解析出的目录确实存在时，仍回落到旧位置。

### 重置后的自检

`reset-password` / 菜单第 7 项在写完后会**回读账号并重新校验明文密码**，
校验不过直接报错，而不是打一句「已重置」就结束——写进影子库、库是只读副本、
事务没提交，这三种失败以前都是静默的。

## 网站与软件商店契约

| 能力 | 说明 |
|---|---|
| 站点 | 站点名 `^[a-z0-9][a-z0-9-]{1,30}$`，根目录必须在 `sites_root` 下；配置写入 → `nginx -t` → reload，失败回滚旧配置 |
| 证书 | certbot webroot 模式（HTTP-01），异步任务执行；域名与邮箱先校验格式 |
| 软件商店 | 清单只允许 7 种动作：`apt / download / extract / write_file / path / service / check`，**不含自由脚本** |
| 安装前检查 | 已装则跳过、冲突与依赖校验、下载必须 https 且拒绝内网/回环地址、剩余内存不足一半直接拦截 |
| 包管理 | `apt-get install -y --no-install-recommends`；回滚时 `stop`/`disable` 对**不存在的单元按成功处理**（apt 没装上就不该再报一次 `Unit not loaded`，那会把真正的失败原因盖住） |
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

**当前写法（v0.4.6 起）：默认放开，只读点名。**

```ini
ProtectSystem=no
ReadOnlyPaths=-/boot -/efi
```

原来不是这样，而是「`ProtectSystem=full` + `ReadWritePaths` 白名单」。这条路线被坑了三轮，
每轮都只是把洞补上、没解决产生洞的机制：

| 版本 | 写法 | 撞上的事 |
|---|---|---|
| ≤ v0.3.4 | `strict` + 五条白名单 | 文件管理器 `mkdir /home/aaa` 报 read-only |
| v0.3.5 | `full` + `/etc/nginx/conf.d` 等零散白名单 | `apt-get` 写 `/etc/nginx`、`/var/lib/dpkg` 全线失败 |
| v0.4.0 | `full` + `-/etc -/usr -/var` + 点名商店目录 | 旧单元升级后仍报 read-only；装 Nginx 时 dpkg 在 `/etc/default/nginx` 失败 |

三个结构性原因让白名单路线注定反复：

1. **枚举总是漏。** 面板的本职就是替用户改系统——装包写 `/usr` `/etc` `/var`、建站写
   `/etc/nginx`、防火墙写 `/etc/nftables.conf`、调内核写 `/etc/sysctl.d`。清单以外的新功能
   一定会写到新目录。
2. **同一份单元在不同机器上行为不同。** systemd 对 `ReadWritePaths` 里带 `-` 前缀且
   **不存在**的路径是静默跳过的：目录没建，bind mount 不发生，那里就仍是只读。装 Xray 时
   `mkdir /usr/local/share/xray` 失败，正是这个机制。
3. **升级只换二进制、不换单元。** 「设置 → 版本更新」不动
   `/etc/systemd/system/panelvps.service`，于是二进制已经修好、沙箱仍是旧的，症状和没修一样。

翻转成黑名单后，故障面从「N 个可能遗漏的白名单」缩成「2 个明确的只读路径」，而且错误
直白：真的写到 `/boot` 才报 read-only。写保护对 root 面板的边际收益本来就低（它能写
systemd 单元、带 `CAP_DAC_OVERRIDE`）；真正挡提权与横向移动的是同一单元里的能力集、
`RestrictAddressFamilies`、`ProtectKernelTunables` / `ProtectKernelModules`、`MemoryMax`——
那些一律保留。

**单元不匹配时的修法**：「设置 → 服务单元」会比对磁盘上的单元与二进制内置版本，不一致
显示「需要更新」，点「用内置版本覆盖」（旧文件备份为 `.bak-时间戳`）再点「重启面板」。
也可以走命令行：

```bash
sudo install -m644 panelvps.service /etc/systemd/system/panelvps.service
sudo systemctl daemon-reload && sudo systemctl restart panelvps
```

`sudo ./install.sh --check` 会复核单元写法，旧写法（`strict` / `full`）会明确告警。

装包前面板会先探 `/etc` `/usr` `/var` 三个根目录（`internal/softstore/preflight.go`），
写不进去就在下载安装包之前停下并说明原因，不会走到 dpkg 半装那一步。如果已经半装，
先 `apt-get -f install` 收尾。

注意：面板自带的 Web 终端不能用来修这类问题 —— 它是面板的子进程，继承同一份只读 mount
namespace；`sudo` 也会失败（`setresuid(...): Operation not permitted`），因为单元里开了
`RestrictSUIDSGID`。终端本身已经是 root，不需要 `sudo`。

### 能力集：CAP_FOWNER 与 CAP_NET_BIND_SERVICE

单元用 `CapabilityBoundingSet` 把能力收窄到面板真正需要的几项。少一个就会撞出一类症状，
而且报错位置往往离真因很远。已经踩过的两个：

**`CAP_FOWNER` —— apt 的一串 warning。** apt 的 sandbox 用 `_apt` 用户下载，落到
`lists/partial` 的文件属主是 `_apt`，root 再去改权限时**对不属于自己的文件 chmod 需要
`CAP_FOWNER`**（`CAP_CHOWN` 只管改属主，不管改权限位）。少了它会看到：

```
W: chmod 0600 of file /var/lib/apt/lists/partial/..._InRelease failed
   - Item::QueueURI (1: Operation not permitted)
```

只是 warning，apt 不会中断，所以很容易被当成噪音忽略 —— 但同样的权限检查会被文件管理器
改权限、站点部署 `chown/chmod` 用到。

**`CAP_NET_BIND_SERVICE` —— 装 Nginx「装成功却判失败」。** 表现为最后一步校验炸掉：

```
[3/3] 校验 nginx -t
nginx: [emerg] bind() to 0.0.0.0:80 failed (13: Permission denied)
安装失败，开始回滚
```

`nginx -t` 除了查语法，还会**真的 bind 一遍每个 listen 指令**。它是面板的子进程，继承
单元的能力边界集；内核在 `inet_bind` 里先查端口权限（`snum < 1024` 且无
`CAP_NET_BIND_SERVICE` → `EACCES`），再查是否被占用。所以哪怕 nginx 自己（独立单元、
能力完整）早就正常监听 80，面板这边也只拿到 EACCES。

内核在 `EACCES` 与 `EADDRINUSE` 之间有确定的先后：**权限先判**，因此缺能力时永远看不到
「端口被占用」这条更准确的诊断。

从 v0.4.7 起两层都做了：

- 单元补上 `CAP_NET_BIND_SERVICE`（面板也要直接绑低端口：中转规则常写 `:443`，
  自身 `listen` 也可能配成 `:80`）；
- 校验逻辑按 errno 分流，缺能力与「端口被自己占用」都不再判成安装失败
  （`internal/bindfail`）。所以**即使单元还是旧的，0.4.7 也能把 Nginx 装上**，
  只是日志里会带一行提示。

`install.sh --check` 两项都核对。注意它先查「当前 shell」的能力（`CapEff`），再查
**单元文件**里写的能力 —— 前者是 SSH 会话的，看不出单元有没有放行，两者的差别正是
上面那个故障。

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
