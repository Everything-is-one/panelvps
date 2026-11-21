# PanelVPS 产品需求文档（PRD）v0.3

| 项 | 内容 |
|---|---|
| 版本 | v0.3（新增安全设计、软件商店改造、代码来源纪律） |
| 日期 | 2026-11-21 |
| 状态 | 草案 — 含 7 个待决策项（见 §17），决策后升 v1.0 冻结 |
| 性质 | 单人自用工具，后续可能开源；采用洁净室实现，未复制 GPL 代码（见 ADR-11） |

## 0. 变更说明

### 0.1 v0.1 → v0.2

| v0.1 原文 | v0.2 处理 |
|---|---|
| 2C1G14G 的低配 VPS 上装低内存面板 | 量化为资源预算与硬门禁（§4、§5），并据此反推技术选型 |
| 类似宝塔的 7 项功能，功能简单些 | 拆成 M2–M8 七个模块，逐条给编号、优先级、验收口径（§7） |
| 增加 x-ui 基础功能，融合 3x-ui 的 vless/多用户 | 收敛为 4 协议 + 多用户 + 流量/到期（M9、M10），并给出**不做清单**（§8）防止范围膨胀 |
| 参考两个 GitHub 仓库 | 补充竞品横向对比、开源协议边界、可复用/不可复用部分（§2.3、§9） |

### 0.2 v0.2 → v0.3

| 本轮问题 | v0.3 处理 |
|---|---|
| 「软件管理做成软件商店，按需搜索安装」 | M8 从「组件管理」重写为**声明式软件商店**（搜索 / 分类 / 内存标注 / 一键装卸），并给出清单 DSL 与内置应用范围（§7 M8） |
| 「nf_tables 扩展模块占资源吗，效用更好吗」 | ADR-03 补资源与性能结论：**几乎不额外占内存，且大规则集下更快更省**；同时澄清 ufw 失败的真实原因是 `xt_icmp6` 缺失而非 nf_tables 不可用（§9 ADR-03） |
| 「安全还有哪些必要的，防 SSH 爆破、端口防扫描」 | 新增 **§14 安全设计**：威胁模型 + 8 层要求 + 安全默认值表（原 §14–§16 顺延为 §15–§17） |
| 「自研借最佳实践不借代码就无风险了吧」 | 新增 **ADR-11 洁净室纪律**，并落地仓库内 `PROVENANCE.md` 作为可举证材料；结论：风险可降到很低，但**不是法律意义上的零风险** |
| 建私有仓库管理代码 | 仓库初始化，`README.md` + `PROVENANCE.md` + `.gitignore` 先行入库 |

---

## 1. 一句话定义

> 在 **512MB 可用内存**的 Debian VPS 上，用一个常驻 50MB 以内的 Go 单二进制，同时提供宝塔式的日常运维和 x-ui 式的代理管理，全程不依赖 Docker。

产品的唯一卖点不是功能多，而是**在别人装不下的机器上装得下、还够用**。任何功能只要与这条冲突，一律砍掉。

---

## 2. 背景与问题定义

### 2.1 三条现实约束（需求的真正来源）

1. **宝塔 / 1Panel 在这台机器上装完就没内存了。** 宝塔是 Python 多进程加常驻任务，空载 250–400MB；1Panel 强依赖 Docker，容器层再加 300MB 起。512MB 可用内存的机器装完剩不下多少，再跑一个 Xray 和一个站点就接近 OOM。
2. **x-ui / 3x-ui 只有代理，没有运维。** 改 nginx 配置、看日志、放行端口、传文件仍要开 SSH，面板只解决了「代理节点」这一件事。
3. **现成的轻量面板缺代理。** 耗子面板（Rat Panel，Go + Vue）在 Debian 12 下面板 + LNMP 已能压到 500MB 以内，运维功能完整，但完全没有代理能力，且它的应用体系是预编译插件，扩展 Xray 需要按其插件规范重写。

结论：**「轻量运维」与「代理管理」目前没有交集，这就是本项目的位置。**

### 2.2 目标用户与场景

| 项 | 说明 |
|---|---|
| 用户 | 只有你一个（单管理员），同时是开发者与运维 |
| 机器 | 1–2 台低配境外/境内 VPS，512MB 可用内存，无容器运行时 |
| 高频场景 | 每天 1–3 次：看负载、加/改代理用户、放行端口、翻日志、改站点配置 |
| 低频场景 | 换证书、装 PHP 版本、迁移备份（月级） |
| 明确不是 | 给客户用的托管面板、多租户、计费、工单 |

### 2.3 竞品横向对比（定位依据）

| 面板 | 技术栈 | 空载内存（估） | 网站 | 代理 | 依赖 Docker | 判断 |
|---|---|---|---|---|---|---|
| 宝塔 | Python + 常驻脚本 | 250–400MB | ✅ 强 | ❌ | ❌ | 功能过剩，内存不可接受 |
| 1Panel | Go + Docker | 300–500MB | ✅ | ❌ | ✅ 强依赖 | 本机内核跑不动容器，直接出局 |
| 耗子面板 Rat Panel | Go + Vue + SQLite | 面板 + LNMP < 500MB | ✅ | ❌ | ❌ | 最接近，缺代理这一半 |
| x-ui | Go + SQLite | 20–30MB | ❌ | ✅ 基础 | ❌ | 只做代理，无运维 |
| 3x-ui | Go + SQLite | 40–60MB | ❌ | ✅ 强（协议溢出） | ❌ | 功能远超所需，含多节点/14 语言/Postgres |
| **本产品** | **Go 单二进制 + SQLite + Xray 子进程** | **目标 ≤ 50MB** | ✅ 轻 | ✅ 4 协议 | ❌ | **空白位** |

> 内存数据来自各项目公开说明与社区实测，非本机实测；M0 阶段需在本机跑一次基线测量并回填（见 §17 Q1）。

---

## 3. 目标与非目标

### 3.1 目标（全部可验收，对应 §16）

| 编号 | 目标 | 量化口径 |
|---|---|---|
| G1 | 极致轻量 | 面板空载 RSS ≤ 50MB；面板 + Xray + Nginx + PHP-FPM 合计 ≤ 320MB |
| G2 | 装得快 | 一条命令安装，从执行到可登录 ≤ 5 分钟；安装脚本幂等，可重复执行 |
| G3 | 够用 | §6 的 P0/P1 模块全部可用，任一功能 ≤ 3 次点击可达 |
| G4 | 代理不出错 | 配置变更 100% 经 `xray -test` 校验，失败自动回滚，不断流超过 3 秒 |
| G5 | 可恢复 | 面板进程崩溃 10 秒内自启；配置保留最近 5 个版本可一键回滚；DB 每日自动备份 |
| G6 | 不锁死 | 停掉面板进程，已部署的网站与代理继续正常工作（面板是管理面，不是数据面） |

### 3.2 非目标

- 不做多节点 / 集群 / 主从同步
- 不做应用商店、一键部署、插件市场
- 不做容器管理（技术上不可行，见 §4.3）
- 不做计费、工单、多租户、开放注册
- 不追求 x-ui / 3x-ui 的功能对齐率（**对齐 20% 的高频功能即可**）

---

## 4. 硬约束

### 4.1 运行环境

| 项 | 值 | 影响 |
|---|---|---|
| CPU | 2 核 | 编译产物需精简；Go 并发模型够用，但要限制监控采集频率 |
| 内存 | 512MB 可用 | **第一约束**，决定一切选型 |
| 磁盘 | 14GB | 第二约束：日志与备份必须限量，见 §5.2 |
| OS | Debian 12 (bookworm) | 主目标；兼容 Ubuntu 22/24、Alma 9 |
| 内核 | 4.x 及以上，安装时探测能力 | **第三约束**，已知坑见 §4.3 |
| 权限 | root | 面板需操作 nft、PTY、/etc，必须 root 运行 |
| 架构 | amd64（预留 arm64 交叉编译） | 单二进制发布 |

### 4.2 资源门禁（写进 CI，超了不许合）

- 面板二进制（含前端资源）≤ 25MB
- 前端产物 gzip 后 ≤ 400KB
- 空载 RSS ≤ 50MB（`/proc/<pid>/status` VmRSS 实测）
- 冷启动到可登录 ≤ 1.5s

### 4.3 内核侧已知坑（直接推出两条架构级决策）

| 坑 | 现象 | 根因 | 对本项目的约束 |
|---|---|---|---|
| Docker 容器永远停在 Created | `docker run` 后状态不前进 | 部分 4.x 内核缺少 runc 所需的 eBPF 设备过滤，`bpf_prog_query(BPF_CGROUP_DEVICE)` 返回 invalid argument | **不做容器管理，不依赖 Docker 运行自身**（ADR-02） |
| `ufw enable` 必失败 | `ip6tables-restore` 报 `Extension icmp6 revision 0 not supported` | 内核缺 IPv6 nf_tables 扩展模块 | **不调用 ufw**，防火墙自管 nftables / iptables（ADR-03） |

> 这两个坑是 v0.1 里没写、但会直接导致方案返工的部分。面板在**安装阶段就要探测**内核能力并选择后端，不能假设标准内核行为。

---

## 5. 资源预算

### 5.1 内存预算（512MB 可用）

| 组件 | 目标 RSS | 何时占用 | 控制手段 |
|---|---|---|---|
| 系统基础（systemd/journald/sshd） | 90–110MB | 常驻 | 关闭无关服务 |
| **面板主进程** | **≤ 50MB** | 常驻 | `GOMEMLIMIT=64MiB`、`GOGC=50`、SQLite cache 8MB |
| Xray-core | 25–50MB | 常驻 | 单进程；连接数增长为主因 |
| Nginx | 10–20MB | 开网站时 | worker_processes 1、worker_connections 512 |
| PHP-FPM | 40–60MB | 开 PHP 站时 | `pm=ondemand`、`pm.max_children=5` |
| MariaDB | — | **默认不装** | 需要时手动装，装前弹内存警告（+150MB） |
| **合计（含 PHP 站）** | **≈ 275–320MB** | | 留 ≥ 190MB 给业务与文件缓存 |

**硬规则**：默认安装只带「面板 + Xray」。Nginx/PHP/MariaDB 都是**按需安装**，且安装前显示预计内存增量。

### 5.2 磁盘预算（14GB 总量）

| 项 | 上限 | 策略 |
|---|---|---|
| 系统 | ~2.5GB | 不额外装编译工具链 |
| 面板 + Xray | ~60MB | 单二进制 + 一个 Xray |
| Nginx + PHP | ~300MB | 只装一个 PHP 版本 |
| 站点与业务数据 | ~10GB | 用户自管 |
| 日志 | **≤ 500MB** | logrotate 按站点/按天轮转，保留 7 天，单文件 ≤ 50MB |
| 备份 | **≤ 1GB** | 滚动保留 7 份，超过自动删最旧 |
| 安全余量 | **≥ 1GB** | 低于 1GB 时面板首页红字告警并暂停自动备份 |

---

## 6. 功能地图与优先级

```
panelvps（Go 单二进制 · 默认仅监听 127.0.0.1）
├── 安全与接入   M1  认证 / 会话 / 审计 / 限流
├── 运维         M2  概览监控 · M5 文件 · M6 终端 · M7 日志
├── 网络         M4  防火墙（nftables 优先）
├── 站点         M3  网站 / 证书 / PHP
├── 软件         M8  商店（声明式清单，搜索即装）
├── 代理         M9  入站 / 客户端 / 流量限额
└── 系统         M10 设置 / 备份 / 自更新

面板 ── 生成配置 + 调 gRPC + 读统计 ──> Xray-core（子进程）
     ── 模板渲染 + nginx -t 校验  ──> Nginx / PHP-FPM
     ── nft -f 事务式下发        ──> nftables
```

| 模块 | 名称 | 来源 | 优先级 | 一句话 |
|---|---|---|---|---|
| M1 | 认证与安全入口 | 自研 | **P0** | 面板是 root 入口，认证必须先做扎实 |
| M2 | 概览与资源监控 | 宝塔 | **P0** | CPU/内存/磁盘/网络/负载 + 进程 TOP |
| M3 | 网站管理 | 宝塔 | P1 | 静态 / PHP / 反代 + 证书 |
| M4 | 防火墙 | 宝塔 | **P0** | 端口 + IP 黑白名单，nftables 优先 |
| M5 | 文件管理 | 宝塔 | **P0** | 增删改查传 / 权限 / 压缩解压 |
| M6 | 终端 | 宝塔 | **P0** | Web 终端，xterm.js + PTY |
| M7 | 日志管理 | 宝塔 | **P0** | journald + 站点日志 tail |
| M8 | 软件商店 | 宝塔 + 商店模式 | P1 | 搜索即装，声明式清单 |
| M9 | 代理入站与客户端 | x-ui / 3x-ui | **P0** | 4 协议 + 多用户 + 流量/到期 |
| M10 | 设置 / 备份 / 自更新 | 自研 | P1 | 配置导出导入、一键回滚 |

**P0 = 第一个可用版本（MVP）必须全绿**，否则这个项目没有存在意义（少任何一个都还得开 SSH）。
**P1 = MVP 之后第二批**，缺了不致命但有明显短板。

---

## 7. 功能详细规格

### M1 认证与安全入口（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M1-01 | 单管理员账号 | 用户名 + 密码，`argon2id` 哈希；首次安装生成随机密码并强制修改 |
| M1-02 | TOTP 二次验证 | 可选开启；开启后登录需 6 位码，敏感操作（终端、防火墙、删站点）可要求二次验证 |
| M1-03 | 登录防爆破 | 连续 5 次失败锁定来源 IP 10 分钟；已登录会话全部失效可一键踢出 |
| M1-04 | 面板端口 | 安装时随机 5 位高端口（20000–60000），可改；支持只监听 `127.0.0.1` 走 Nginx 反代 |
| M1-05 | 会话管理 | HttpOnly + SameSite=Lax Cookie；空闲 2h 过期，「记住我」7d；单管理员最多 3 个并发会话 |
| M1-06 | 面板 HTTPS | 支持上传证书 / 自动申请；未配 HTTPS 时首页顶部黄色告警条 |
| M1-07 | 审计日志 | 记录登录、配置变更、文件删除、命令执行；保留 90 天 |

**不做**：多用户、RBAC 权限矩阵、LDAP/OAuth、验证码图片。

### M2 概览与资源监控（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M2-01 | 实时指标 | CPU 总/分核、内存、Swap、磁盘各分区、负载 1/5/15、网络上下行速率、TCP 连接数、运行时长 |
| M2-02 | 刷新机制 | SSE 推送，服务端 3s 一次采样；前端只画最近 5 分钟曲线，**不存全量点** |
| M2-03 | 历史趋势 | 内存环形缓冲保存 1 分钟粒度 6 小时；5 分钟粒度落库保留 7 天；表上限 50MB 自动滚动 |
| M2-04 | 进程 TOP | 按 CPU / 内存排序 TOP 10，可一键结束（需确认 + 记录审计） |
| M2-05 | 组件状态卡 | 面板 / Xray / Nginx / PHP 四个卡片：运行状态、版本、内存、重启按钮 |
| M2-06 | 阈值告警 | CPU>90% 持续 5min、内存>90%、磁盘>85%、Xray 掉线 → 通知（邮件 / TG Bot / Webhook，三选一） |
| M2-07 | 流量与端口 | 本月 Xray 总流量、当前监听端口列表（一键跳转放行） |

**不做**：Prometheus 导出、InfluxDB 时序库、自定义大盘、告警分级/值班表。

### M3 网站管理（P1）

| 编号 | 需求 | 说明 |
|---|---|---|
| M3-01 | 站点类型 | 静态、PHP、反向代理，三种；不含 Java/Node/Python 运行时管理 |
| M3-02 | 域名绑定 | 支持多域名 + 泛域名；自动生成 `server_name` |
| M3-03 | 站点 CRUD | 名称、根目录、端口、类型、备注；删除站点默认保留文件（可选一并删） |
| M3-04 | SSL 证书 | Let's Encrypt（HTTP-01 自动 / DNS-01 手动）；上传自有证书；到期前 7 天提醒；自动续期 |
| M3-05 | 强制 HTTPS | 一键开关 + HSTS 选项 |
| M3-06 | PHP 管理 | 检测系统已装版本（8.2/8.3），按站点独立 FPM pool，`pm=ondemand`；支持切换版本 |
| M3-07 | 伪静态 | 内置 WordPress / Laravel / ThinkPHP 模板，可自定义 |
| M3-08 | 配置安全 | 所有 Nginx 配置由模板生成并写库；落盘前 `nginx -t`，失败回滚不生效 |
| M3-09 | 访问日志 | 按站点分文件，logrotate 每日轮转保留 7 天，可在 M7 查看 |

**不做**：一键部署 WordPress、数据库创建向导、FTP 服务、Node/Java 项目管理、CDN 配置。

### M4 防火墙（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M4-01 | 后端自适应 | 启动探测：`nft` 可用且能建 inet 表 → nftables；否则降级 `iptables-legacy`；面板显示当前后端 |
| M4-02 | 端口规则 | 放行/拒绝，TCP/UDP，单端口/端口段，来源 IP 或 CIDR，备注，排序，启用开关 |
| M4-03 | IP 黑白名单 | 单 IP / CIDR / IP 段；黑名单优先级最高 |
| M4-04 | SSH 保护 | 安装时自动识别当前 SSH 端口并放行，**禁止删除该规则**（防锁死） |
| M4-05 | 面板端口保护 | 面板端口自动加入放行；改端口时同步更新规则 |
| M4-06 | 监听端口视图 | 列出当前 `ss -lntup` 监听端口，未放行的标红并给「一键放行」 |
| M4-07 | 自动封禁 | 登录爆破、SSH 爆破（可选读 auth.log）→ 动态加入 nftables set，带 1h 超时自动解封 |
| M4-08 | 默认策略 | 默认拒绝入站、允许出站；切换默认策略需输入面板密码二次确认 |

**不做**：ufw 封装（内核不支持）、fail2ban 集成、地域/IP 库封禁、流量整形。

### M5 文件管理（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M5-01 | 目录浏览 | 面包屑 + 列表，显示名称/大小/权限/属主/修改时间；按名称/大小/时间排序 |
| M5-02 | 路径范围 | 默认可访问 `/`，可在设置中限定根目录（如 `/var/www`） |
| M5-03 | 文件操作 | 新建文件/目录、重命名、复制、移动、删除（进回收站或直接删，二选一可配） |
| M5-04 | 上传下载 | 支持分片上传（>100MB 不断）；下载支持断点；单文件上限 2GB |
| M5-05 | 在线编辑 | ≤ 2MB 文本文件，带行号；保存前做编码检测（UTF-8/GBK） |
| M5-06 | 权限管理 | chmod（八进制/勾选）、chown 用户与组 |
| M5-07 | 压缩解压 | zip / tar.gz 压缩与解压，显示进度；解压目标目录可选 |
| M5-08 | 安全 | 路径穿越校验（`filepath.Clean` + 前缀校验）、不跟随符号链接、上传可执行文件到 Web 目录时警示 |

**不做**：在线图片编辑器、视频预览、S3/对象存储挂载、WebDAV。

### M6 终端（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M6-01 | Web 终端 | xterm.js + WebSocket + PTY（`creack/pty`），支持颜色、resize、粘贴 |
| M6-02 | 多标签 | 最多 5 个并发会话，切换不丢状态 |
| M6-03 | 会话控制 | 空闲 30 分钟自动断开；可在设置中整体禁用终端功能 |
| M6-04 | 命令审计 | 可选记录输入命令到审计日志（不做完整回放录屏） |
| M6-05 | 安全 | 必须已登录；开启 2FA 时打开终端要求二次验证 |

**不做**：命令回放录像、堡垒机、文件传输协议（用 M5 代替）。

### M7 日志管理（P0）

| 编号 | 需求 | 说明 |
|---|---|---|
| M7-01 | 面板日志 | 面板自身日志走 stdout → journald，按 `-u panelvps` 查询 |
| M7-02 | 系统日志 | journalctl 按 unit 过滤：sshd / nginx / xray / panelvps |
| M7-03 | 站点日志 | 按站点选择 access / error 日志 |
| M7-04 | 查看方式 | `tail -n 200`，实时跟随（SSE），关键字过滤，时间范围筛选 |
| M7-05 | 安全限制 | 只读文件末尾 ≤ 2MB，禁止整文件加载（防大文件撑爆内存） |
| M7-06 | 管理 | 下载（≤ 20MB）、清空（二次确认）、跳转 logrotate 配置 |

**不做**：日志聚合分析、正则统计图表、ELK/Loki 集成。

### M8 软件商店（P1，v0.3 由「软件管理」升级）

**定位**：有搜索、有分类、有内存标注、装完能管的轻量商店。
不做成宝塔那样的几百个应用，也不是 Docker 应用市场，只收录能在 512MB 可用内存下跑、且能干净卸载的软件。

| 编号 | 需求 | 说明 |
|---|---|---|
| M8-01 | 商店首页 | 搜索框（名称/关键字/标签）+ 分类侧栏 + 应用卡片；卡片必显「内存占用 / 磁盘占用 / 已装状态」 |
| M8-02 | 分类 | Web 服务、运行时、数据库与缓存、证书与安全、网络工具、运维工具、代理 |
| M8-03 | 应用详情 | 简介、当前版本、依赖、占用端口、**安装将执行的完整步骤（明文展示）**、风险等级、内存预估 |
| M8-04 | 一键安装 | 走 job 队列，实时输出进度；装前校验内存余量，不足直接阻止并说明差多少 |
| M8-05 | 一键卸载 | 停服务 → 卸载包 → 询问是否保留配置；数据库类强制先备份再卸 |
| M8-06 | 配置管理 | 常用参数表单化（端口、根目录、最大连接、内存上限），写回前做语法校验 |
| M8-07 | 服务控制 | 启动 / 停止 / 重启 / 开机自启 / 状态 / 最近 200 行日志 |
| M8-08 | 版本管理 | 显示已装与可用版本；升级前自动快照配置到 `conf/snapshots/` |
| M8-09 | 冲突检测 | 端口占用、互斥应用（nginx / caddy / apache）、PHP 多版本共存标注 |
| M8-10 | 清单来源 | 内置一份 YAML 清单（≤ 20 个应用）；支持自定义源但默认关闭；清单带 sha256 校验 |
| M8-11 | 执行安全 | 清单**不允许任意 shell**，只允许声明式动作（见下）；执行前把动作翻译成人话展示 |

**清单只允许这 7 种声明式动作，不执行自由脚本**：

| 动作 | 参数 | 说明 |
|---|---|---|
| `apt` | `names[] / update / purge` | 只调系统包管理器，不自建仓库 |
| `download` | `url / sha256 / dest` | **强制 sha256**，校验不过即中止 |
| `extract` | `src / dest / strip` | tar.gz / zip |
| `write_file` | `path / from` | 只能从内置模板渲染，不接受内联任意内容 |
| `path` | `mkdir / remove / chmod / chown` | 目录与权限操作 |
| `service` | `name / enable / start / stop / restart` | 只允许操作清单声明的服务名 |
| `check` | `cmd[]` | 只做校验（`nginx -t` 这类），失败即回滚 |

```yaml
slug: nginx
name: Nginx
category: web
summary: 高性能 Web 服务器与反向代理
memory_mb: 15
disk_mb: 60
ports: [80, 443]
deps: []
conflicts: [apache2, caddy]
risk: low
plan:
  install:
    - apt: { names: [nginx], update: true }
    - write_file: { path: /etc/nginx/conf.d/panelvps-base.conf, from: templates/nginx-base.conf }
    - service: { name: nginx, enable: true, start: true }
    - check: { cmd: ["nginx", "-t"] }
  uninstall:
    - service: { name: nginx, stop: true, disable: true }
    - apt: { names: [nginx], purge: true }
  config_schema:
    - { key: worker_connections, label: 最大连接数, type: int, default: 512, min: 64, max: 4096 }
```

**为什么不让清单写 shell**：一旦允许，商店就变成了「任意代码执行入口」，审计、回滚、卸载全部失效。
声明式动作可以逐条翻译成界面上的「将执行」，也可以逐条回滚——这是可控性的来源。

**内置清单（16 个，全部标注内存）**

| 分类 | 应用 | 内存 | 备注 |
|---|---|---|---|
| Web 服务 | nginx / caddy | 15MB / 20MB | 二选一，冲突检测 |
| 运行时 | php 8.2 / php 8.3 | 40MB | `pm=ondemand` |
| 数据库与缓存 | sqlite3 / redis / mariadb / postgresql | 5 / 15 / **150** / **120** MB | 后两个在 512MB 可用内存下标红 |
| 证书与安全 | certbot / acme.sh | 5MB | 二选一 |
| 网络工具 | frp / rclone | 10MB | |
| 运维工具 | htop / btop / tmux / git | 2–8MB | |
| 代理 | xray | 25MB | 面板自身依赖，默认已装 |

**不做**：Docker 应用、源码编译安装、Windows/桌面软件、数百个应用的巨型市场、自建 deb 仓库。

> 与 §8「不做清单」的关系：v0.2 曾把「应用商店」列为不做，v0.3 按你的决定调整为**限定范围的商店**——
> 差别在「20 个受控清单 + 声明式动作」与「无限扩展 + 任意脚本」，前者维护成本可控，后者才是陷阱。

### M9 代理入站与客户端（P0，核心）

| 编号 | 需求 | 说明 |
|---|---|---|
| M9-01 | 协议范围 | **VLESS（含 REALITY）、VMess、Trojan、Shadowsocks** 四种，够用即止 |
| M9-02 | 传输方式 | tcp / ws / grpc / httpupgrade；TLS 或 REALITY；其余传输不做 |
| M9-03 | 入站 CRUD | 端口、协议、传输、TLS、备注、Sniffing、总流量限额、到期时间、启用开关 |
| M9-04 | 客户端管理 | 客户端 = 用户，`email` 全局唯一作为统计主键；UUID/密码、流量限额(GB)、到期时间、IP 限制、启用开关 |
| M9-05 | 批量操作 | 批量增删客户端、批量重置流量、批量导出链接 |
| M9-06 | 分享与二维码 | 生成 `vless://` `vmess://` `trojan://` `ss://` 链接 + 二维码，支持一键复制 |
| M9-07 | 流量统计 | 通过 Xray Stats API（gRPC `127.0.0.1:10085`）按 email 采集 uplink/downlink，60s 一轮 |
| M9-08 | 限额执行 | 超流量/过期 → 自动禁用该客户端 → 重新生成配置 → 重载 Xray；解封同理 |
| M9-09 | 配置生成 | 模板 + JSON 组装，覆盖 log / api / stats / policy / inbounds / outbounds / routing |
| M9-10 | 配置校验 | 落盘前 `xray -test -config`；失败不生效并回滚上一版；保留最近 5 个版本 |
| M9-11 | 端口冲突 | 新增/修改入站前检测端口占用，冲突时明确提示占用进程 |
| M9-12 | 订阅链接 | `/sub/<token>` 输出 Base64 或 Clash 配置，可开关、可设有效期（P1） |
| M9-13 | 节点信息 | 显示 Xray 版本、运行状态、当前连接数、日志级别切换 |

**不做**：WireGuard / Hysteria / MTProto / AmneziaWG / Tun / Tunnel、多节点主从、出站负载均衡与链式代理、自定义 DNS 表拖拽、Traffic 计费。

### M10 设置 / 备份 / 自更新（P1）

| 编号 | 需求 | 说明 |
|---|---|---|
| M10-01 | 面板设置 | 端口、路径、语言（中/英）、时区、HTTPS、通知渠道、终端开关、文件根目录 |
| M10-02 | 配置导出 | 打包 DB + 面板配置 + 证书引用路径（不含私钥明文，可选包含） |
| M10-03 | 配置导入 | 上传备份包 → 校验版本 → 恢复 → 重启；版本不兼容时拒绝并提示 |
| M10-04 | 定时备份 | 每日 03:00 自动备份到本地目录，滚动保留 7 份 |
| M10-05 | 一键修复 | CLI：`panel reset-password`、`panel reset-firewall`、`panel restart`、`panel info` |
| M10-06 | 面板自更新 | 下载 → 校验签名 → 备份旧版 → 原子替换 → 失败回滚 `panelvps.prev`（见 S-F05） |

---

### M11 管理入口与内核调优（P0，v0.3 补充）

> v0.3.2 起不再单独占一个「控制」导航：访问方式与账号并入「设置」，系统信息并入「概览」，
> BBR 与重启只在命令行菜单里。控制页要拉 BBR 状态（`modprobe -n`），打开要等一两秒，
> 而这些操作一个月也用不上一次，不值得占一个一级入口。

装完之后的动作不该再回 SSH 翻配置文件，集中到「控制」页。

| 编号 | 需求 | 说明 |
|---|---|---|
| M11-01 | 访问方式 | 默认只监听 `127.0.0.1`；可在页面切到 `0.0.0.0` 并列出可用地址（请求 Host + 本机网卡 IPv4，去重），标明只有 HTTP |
| M11-02 | 端口修改 | 1–65535；顺序为「试绑新端口 → 写配置 → 按新端口重下防火墙 → 重启面板」，防火墙失败则回滚配置 |
| M11-03 | 重置密码 | 随机 20 位（仅返回一次）或自定义，复用同一套密码策略；成功后删除该账号全部会话 |
| M11-04 | 重启 | 面板 / Xray / 服务器三档；重启服务器需 `confirm=REBOOT`，用 `systemctl --no-block` 不阻塞请求 |
| M11-05 | BBR 开关 | 探测 `tcp_bbr`（可用列表或 `modprobe -n`）→ 写 `/etc/sysctl.d/99-panelvps.conf` → 交 helper 单元应用；关闭写回 `cubic` |
| M11-06 | 系统信息 | 主机名、发行版、内核、架构、CPU 核数、时区、cgroup 版本；并入「概览」页 |
| M11-07 | 命令行菜单 | `panelvps` 不带参数进菜单（无 TTY 时仍按 `serve`）：启停、状态、日志、监听与端口、重置密码、BBR、信息、重启服务器；安装脚本在 `/usr/local/bin` 建软链 |
| M11-08 | Web 终端开关 | 设置页勾选即写入 `terminal_enabled` 并立即生效，不需要改配置文件和重启 |
| M11-09 | 日志兜底 | 面板同时写 `<data_dir>/panel.log`（5MB 轮转）；日志页优先读它，绕开容器沙箱里读不到自身 journal 的问题 |

---

## 8. 明确不做清单（范围护栏）

这条清单和功能清单同等重要。**每次想加功能，先来这里对一眼。**

| 不做项 | 理由 |
|---|---|
| Docker / 容器管理 | 容器层内存开销与本项目目标冲突；部分旧内核也不支持 runc 的设备过滤 |
| 巨型应用市场（数百应用）/ 自建 deb 仓库 / Docker 应用 | 维护成本指数上升；商店只做 ≤20 个、声明式清单、走系统包管理器（见 M8） |
| 数据库可视化管理（phpMyAdmin 类） | 低频；需要时用 CLI 或临时装 Adminer 单文件 |
| 多节点 / 集群 / 主从 | 单机场景，架构复杂度指数上升 |
| 插件市场 / 扩展框架 | 会引出沙箱、权限、版本兼容三座大山 |
| 邮件服务器（Postfix/Dovecot） | 单是 Dovecot 就 80MB+，且与代理共用 IP 会互相牵连 |
| FTP 服务 | 用 M5 文件管理 + SFTP 替代，少一个守护进程 |
| 计费 / 工单 / 多租户 / 开放注册 | 个人自用，无此场景 |
| 全量 WebSocket 推送 | 每个连接占内存；改用 SSE + 3s 采样，长连接数降为 1 |
| 内置时序数据库 | 环形缓冲 + SQLite 降采样已足够，省 100MB+ |
| 3x-ui 的扩展协议族 | WireGuard/Hysteria/MTProto 属低频，且各自要额外二进制与内核模块 |
| 面板多用户与 RBAC | 单管理员即可，权限矩阵是纯负担 |

---

## 9. 关键设计决策（ADR）

### ADR-01 技术底座：自研薄内核，而非 fork 现成项目

| 方案 | 做法 | 优势 | 劣势 | 判断 |
|---|---|---|---|---|
| A. fork 3x-ui | 在 3x-ui 上加 6 个运维模块 | 代理功能零成本、成熟稳定 | 继承 14 语言、多节点、Postgres、Telegram、React+AntD 全套重量；上游迭代快，每次同步都是冲突；512MB 可用内存的目标基本无法达成 | ❌ 否决 |
| B. fork 耗子面板 | 在 Rat Panel 上加 Xray 模块 | 运维模块现成、Go+Vue 同栈、BSD 类许可友好 | 其应用体系为预编译插件，Xray 需按插件规范重写；面板 + LNMP 已 ~500MB，要压到 320MB 得动它的核心 | ⚠️ 备选（若工期极紧） |
| C. 自研 | Go 薄内核 + Xray 子进程 | 内存/体积完全可控；功能集恰好等于需求集；代码量小（粗估 1.2–1.8 万行） | 工作量大；Xray 配置生成需自己写并反复验证 | ✅ **推荐** |

**结论**：选 C。但**「借行为不借代码」**——交互与字段设计参考两家，实现全部重写（协议合规原因见 ADR-10）。
**唯一例外**：Xray 配置的字段组合逻辑可参照 x-ui 的 `xray/config.go` 理解，但需独立实现，不复制源码。

### ADR-02 不引入 Docker

面板自身以 systemd 服务方式运行，Xray / Nginx / PHP 均以系统进程运行。
理由：本机内核跑不动容器（§4.3）；容器抽象在单机单用户场景下只增加内存与排障成本。
**代价**：放弃「一键换运行环境」的能力；**收益**：省 200MB+，且不受内核 bug 影响。

### ADR-03 防火墙：nftables 优先，iptables-legacy 降级

**先纠正一个常见误判**：本机 `ufw enable` 失败的报错是
`ip6tables-restore v1.8.9 (nf_tables): Extension icmp6 revision 0 not supported`，
这说明 **`icmp6` 这个匹配扩展在当前内核/库组合里不可用**（`xt_icmp6` 内核模块或 libxtables 插件缺失），
**不是 nf_tables 子系统坏了**——同一次 `ufw enable` 里 IPv4 规则是能正常下发的，坏的只有 IPv6 的 icmp6 匹配。

#### 资源开销：几乎为零

| 项 | 实际情况 |
|---|---|
| 用户态守护进程 | **没有**。nftables 不像 ufw/firewalld 那样常驻一个进程 |
| 内核模块 | `nf_tables.ko` 数百 KB，模块代码页可共享、可回收，对 RSS 贡献可忽略 |
| 规则内存 | 单条约 100–200 字节内核对象；100 条规则 ≈ 几十 KB |
| 连接跟踪 | `nf_conntrack` 按需分配，默认上限 65536 条，满表约 20MB |
| 与 iptables 的差异 | **conntrack 是同一个子系统**，iptables 一样要用，所以这不是 nftables 的额外开销 |

结论：换成 nftables 不会让内存更紧张，两者量级相同。

#### 效用：规则多时明显更好

| 维度 | iptables-legacy | nftables |
|---|---|---|
| 规则匹配 | 线性遍历，O(n) | 编译为字节码 + set/map 哈希查找 |
| 大批量 IP 封禁 | 需 `ipset` 辅助 | 原生 set，5000 条仍接近 O(1) |
| 自动解封 | 需 ipset timeout 或 cron 维护 | set 元素自带 `timeout`，到期自动移除 |
| 原子更新 | `iptables-restore` 整表替换 | `nft -f` 事务式，失败整体回滚 |
| 规则可读性 | 差 | 好（`nft list ruleset` 可读） |

**封禁场景是决定性的**：S-D02 的「扫描源短期封禁」和 S-C05 的「SSH 爆破封禁」都要求「加进去、1 小时后自动消失」。
nftables 一行 `add element ... { timeout 1h }` 就够；iptables 得靠 ipset 或定时任务，多一个组件、多一处会坏的地方。

#### 探测与降级（安装时执行，失败不静默）

```bash
# 1) 核心可用性 + icmpv6 匹配能力一起验
nft add table inet pvtest \
 && nft add chain inet pvtest c '{ type filter hook input priority 0; }' \
 && nft add rule inet pvtest c icmpv6 type echo-request accept \
 && nft delete table inet pvtest
```

- 全部成功 → 用 nftables（完整能力）
- 建表/建链成功、仅 `icmpv6` 那步失败 → 仍用 nftables，**IPv6 规则不使用 icmpv6 匹配**（改为无条件放行邻居发现所需流量）
- 建表失败 → 降级 `iptables-legacy` + `ipset`（ipset 也没有则只支持单 IP 封禁）
- 全不可用 → 防火墙模块置灰并明确报错，绝不假装成功

规则持久化：nftables 写 `/etc/nftables.d/panelvps.nft`，由 service 启动时 `nft -f` 加载；
iptables 用 `iptables-save` 落盘 + 开机恢复。

### ADR-04 存储：SQLite（纯 Go 实现）+ WAL

`modernc.org/sqlite`（无 CGO，交叉编译友好）。参数：`journal_mode=WAL`、`synchronous=NORMAL`、`cache_size=-8000`（8MB）、`busy_timeout=5000`。
单文件 `panel.db`，每日自动备份，备份即文件复制（WAL checkpoint 后）。

### ADR-05 前端：Vue 3 + 按需引入 + go:embed

Vue 3 + Vite + Naive UI（按需引入）。产物 gzip ≤ 400KB，`go:embed` 打进二进制，**运行时不依赖 Node**。
理由：开发效率与产物体积的平衡点；纯原生 JS 省 300KB 但开发成本翻倍，不值得。

> **M0/M1 实施说明（已发生两次偏离，结论见下）**：
> M0 与 M1 的前端均为 **零构建原生 JS**（当前约 700 行，gzip 后 < 10KB）。
> 原因：`go build` 不依赖 Node、CI 无前端步骤、单二进制可复现；
> 而 M1 的界面复杂度（入站表单、用户表、二维码弹窗）用原生 JS 仍在一个文件内可维护。
> **结论：暂缓引入 Vite + Vue**，等 M2/M3 的页面数量与状态复杂度真正超过原生 DOM 的舒适区再切换。
> 切换成本可控——静态资源目录与 API 契约不变，替换的只是一个文件。

### ADR-06 代理内核：复用 Xray-core 二进制，不自己实现协议

Xray-core 作为独立二进制由面板管理（下载、版本切换、启停、配置生成）。
面板只做「编排层」，**绝不实现任何协议**——这是 x-ui 系列能长期可用的根本原因。

### ADR-07 流量统计：走 Xray 官方 Stats API

配置中开启：
```json
{
  "stats": {},
  "api": { "tag": "api", "services": ["StatsService", "HandlerService"] },
  "policy": { "levels": { "0": { "statsUserUplink": true, "statsUserDownlink": true } } }
}
```
面板通过 gRPC 查询 `user>>>{email}>>>traffic>>>uplink|downlink`，60s 一轮，差值累加落库。
**代价**：需在 Xray 配置里固定开一个本地 API 端口（默认 10085，仅监听 127.0.0.1）。

### ADR-08 日志：走 stdout → journald，不自建轮转

面板自身日志直接打到 stdout，由 journald 收集（`journalctl -u panelvps`）。
理由：省一套文件轮转代码与磁盘配额逻辑；`systemd` 已带 `SystemMaxUse` 限制。
站点/系统日志的轮转交给 logrotate。

### ADR-09 面板以 root 运行（必要之恶）

需要的能力：nft/iptables、PTY、`/etc/nginx`、`/etc/xray`、systemctl、chown。
**补偿措施**：强制 2FA 可选但强烈建议、审计日志、路径限制、SSH 端口保护、默认随机端口、所有破坏性操作二次确认。
**明确记录**：面板被拿下 = 机器被拿下，这是单二进制面板的固有风险，不做虚假的安全承诺。

### ADR-10 开源协议边界（重要）

| 项目 | 许可 | 能否复制代码 |
|---|---|---|
| vaxilu/x-ui | **GPL-3.0** | 复制即传染，衍生作品必须 GPL-3.0 开源 |
| MHSanaei/3x-ui | **GPL-3.0** | 同上 |
| tnb-labs/panel（耗子面板） | BSD 类（宽松） | 可复制，保留版权声明即可 |

**结论**：
- **纯自用** → 无影响，GPL 代码可读可参考。
- **洁净室实现（本项目采用）** → 未复制 GPL 代码，许可自由；**已选定 Apache-2.0**（见 LICENSE）。
- **一旦复制** → 整个项目只能 GPL-3.0 并公开源码，没有回旋余地。
- 耗子面板（BSD 类）可安全借用代码，但仍需保留其版权声明。

### ADR-11 洁净室纪律（代码来源合规）

| 维度 | 规则 |
|---|---|
| 设计输入 | 只用**公开规范/文档**：Xray 官方 docs、nginx 文档、nftables wiki、ACME RFC 8555、各协议 URI 规范、OWASP ASVS |
| 查阅 GPL 源码 | 允许「读行为」，禁止「照着写」；确需查阅时读完不立即动手，间隔后再独立实现，避免逐行复刻 |
| 禁止复制 | `.go/.vue/.ts/.sh` 源文件、配置模板、SQL schema、UI 文案、图标、测试用例 |
| 命名 | 不使用 `x-ui` / `3x-ui` / `宝塔` 作为项目名、包名、模块名或 UI 品牌 |
| 留痕 | 每个模块在仓库根 `PROVENANCE.md` 登记「参考来源 + 仅行为参考」；保留设计草稿与提交历史 |
| 依赖 | 新增依赖登记许可，CI 用 `go-licenses` 生成清单 |

**必须说清的一点**：这不等于「法律意义上的零风险」。
著作权保护**表达**、不保护**功能**，独立实现同样功能不构成侵权——这是基本原则；
但「是否构成实质性相似」要看两边代码的实际比对结果。纪律能把概率压到很低，压不到零。

**真正的收益是许可自由**：没有复制 GPL，将来才能选 MIT/Apache 而不是被迫 GPL。
这也是把 `PROVENANCE.md` 从第一天就建起来的唯一理由——它比任何口头声明都管用。

---

### ADR-12 内核参数由独立 oneshot 单元应用，不放宽主服务沙箱

**问题**：主单元带 `ProtectKernelTunables=yes` 与 `ProtectKernelModules=yes`，进程写不了 `/proc/sys`、
也 `modprobe` 不了。而 BBR 开关恰好需要这两件事。

**被否掉的方案**：把主单元的 `ProtectKernelTunables` 改成 `no` 或加 `ReadWritePaths=/proc/sys`。
代价是进程从此能改**任意**内核参数——为一个小功能拆掉整层加固，不划算。

**采用方案**：面板只写 `/etc/sysctl.d/99-panelvps.conf`（`ReadWritePaths` 里放行这一条），
再由 `panelvps-sysctl.service` 这个 oneshot 单元执行：

```
ExecStartPre=-/sbin/modprobe tcp_bbr
ExecStart=/sbin/sysctl -p /etc/sysctl.d/99-panelvps.conf
```

面板通过 `systemctl restart panelvps-sysctl` 触发，不开机也无需 reboot。

**约束与代价**

| 项 | 处理 |
|---|---|
| 覆盖他人配置 | 文件首行必须是 `# Managed by PanelVPS`，否则拒绝写入并报错 |
| helper 不存在 | 退化为直接 `sysctl -p`（例如手工在终端跑二进制），仍失败则回滚文件 |
| 卸载 | `install.sh --uninstall` 一并停用单元、删除 conf；BBR 在跑则先写回 `cubic` |
| 失败回滚 | `sysctl -p` 失败时恢复原文件；已生效的内核参数留到下次重启自然恢复 |

---

### ADR-13 沙箱从 strict 退到 full：文件管理必须能写

**问题**：v0.3.0 的主单元用 `ProtectSystem=strict` + `ReadWritePaths` 白名单，只放行了数据目录、
`/etc/nginx/conf.d` 等几处。上线后用户在文件管理器里 `mkdir /home/aaa` 直接报
`read-only file system`，上传同样失败——面板最核心的功能之一不可用。

**被否掉的方案**：继续加白名单。`/home`、`/root`、`/srv`、`/mnt`、`/opt`、`/var` 都要写，
加到等于没加，而且用户永远会在白名单外踩到只读。

**采用方案**：`ProtectSystem=full`（继续保护 `/usr`、`/boot`、`/etc`），
去掉 `ProtectHome=read-only` 与 `PrivateTmp`，`/etc` 下需要的子目录仍由 `ReadWritePaths` 精确放行。

| 项 | 变化 | 理由 |
|---|---|---|
| `ProtectSystem` | strict → full | `/usr`、`/boot`、`/etc` 仍只读，挡住替换系统二进制这类持久化 |
| `ProtectHome` | read-only → 取消 | 站点、备份、用户目录都在 `/home` 与 `/root` 下 |
| `PrivateTmp` | yes → 取消 | 文件管理器看到的 `/tmp` 与 SSH 里不是同一个目录，上传完找不到文件 |
| `MemoryMax` | 192M → 256M | Xray 是子进程，也吃同一份 cgroup 限额 |

**为什么这不是「放弃加固」**：主单元是 `User=root` 且保留 `CAP_DAC_OVERRIDE`（读任意文件是设计的一部分），
一个已经能改 `/etc/systemd/system` 和 `/usr/local/bin` 的进程，把 `/home` 设成只读并不能阻止它持久化。
真正的边界是「不要把面板裸奔在公网」和「用强密码 / 二次验证」，而不是给文件管理加一把假锁。

**代价**：面板进程被攻破后可以直接写 `/home` 与 `/root`。与其已有的能力相比没有实质变化，
但确实少了「误操作影响系统盘」的一层缓冲。

---

## 10. 技术架构

### 10.1 分层

```
┌──────────────────────────────────────────────────────────────┐
│ 浏览器 (Vue3 SPA, embed 静态资源)                              │
└───────────────┬──────────────────────────────────────────────┘
                │ HTTP/JSON + SSE + WebSocket(终端)
┌───────────────▼──────────────────────────────────────────────┐
│ 接入层  net/http + chi  中间件: 认证/CSRF/限流/审计/panic恢复    │
├──────────────────────────────────────────────────────────────┤
│ 业务层  handler → service → 领域逻辑                          │
│  auth │ monitor │ site │ firewall │ file │ term │ log │ proxy │
├──────────────────────────────────────────────────────────────┤
│ 适配层  executor: 统一封装 命令执行/配置渲染/原子落盘/回滚       │
│         xrayctl │ nginxctl │ nftctl │ systemdctl │ ptmx       │
├──────────────────────────────────────────────────────────────┤
│ 存储层  SQLite (WAL)   +   内存环形缓冲 (监控/日志 tail)        │
└──────────────────────────────────────────────────────────────┘
                │ 子进程 / 文件 / 系统调用
┌───────────────▼──────────────────────────────────────────────┐
│ Xray-core │ Nginx │ PHP-FPM │ nftables │ systemd             │
└──────────────────────────────────────────────────────────────┘
```

### 10.2 关键机制

| 机制 | 做法 |
|---|---|
| 配置原子落盘 | 写 `.tmp` → 语法校验 → `rename` 覆盖 → 保留 `.bak.N`（N=1..5） |
| 失败回滚 | 校验失败则删除 tmp，不触碰运行中的配置，接口返回具体错误行 |
| 命令执行 | 统一 `exec.CommandContext`，带 30s 超时、输出捕获上限 1MB、禁止 shell 拼接 |
| 监控采集 | 3s 采样一次读 `/proc`（不引入 cgo），写环形缓冲；前端 SSE 拉取 |
| 长任务 | 安装/解压/备份走 job 队列，前端轮询进度（避免请求超时） |
| 并发控制 | 配置类写操作加全局互斥锁，防止同时改 Xray/Nginx 造成中间态 |

### 10.3 目录结构（建议）

```
/opt/panelvps/
├── panelvps              # 主二进制 (≤25MB)
├── bin/xray              # Xray 内核，可多版本
├── data/panel.db         # SQLite
├── data/backup/          # 备份归档
├── conf/panel.yaml       # 启动配置（端口/路径/DB 路径）
├── conf/xray/config.json # 生成物，勿手改
├── conf/xray/history/    # 最近 5 版
└── logs/                 # 仅长任务临时日志，主日志走 journald
/etc/systemd/system/panelvps.service
/usr/local/bin/pv         # CLI 别名
```

---

## 11. 数据模型（初版）

```sql
-- 认证
users(id, username, password_hash, totp_secret, totp_enabled, created_at, last_login_at, last_login_ip)
sessions(id, user_id, token_hash, ip, user_agent, created_at, expires_at)

-- 系统
settings(key TEXT PRIMARY KEY, value TEXT, updated_at)
audit_logs(id, username, action, target, detail, ip, created_at)   -- 索引 created_at
metrics(id, ts, cpu, mem_used, mem_total, swap_used, disk_used, disk_total, load1, net_in, net_out)

-- 代理
inbounds(id, tag, remark, protocol, listen, port, transport, security,
         settings_json, stream_json, total_bytes, up_bytes, down_bytes,
         expiry_at, enable, created_at, updated_at)
clients(id, inbound_id, email UNIQUE, uuid, password, flow, limit_ip,
        total_bytes, up_bytes, down_bytes, expiry_at, enable, sub_token, created_at)
traffic_daily(id, client_id, day, up_bytes, down_bytes)            -- 日聚合，便于出账单视图

-- 网站
sites(id, name, domains, type, root_path, listen_port, ssl_mode,
      cert_path, key_path, php_version, extra_json, enable, created_at)

-- 防火墙
firewall_rules(id, action, family, protocol, port, source, comment, priority, enable)
firewall_bans(id, ip, reason, expire_at, created_at)

-- 软件商店
app_catalog(id, slug UNIQUE, name, category, summary, readme_md, icon,
            version, memory_mb, disk_mb, ports, deps, conflicts,
            plan_json, config_schema_json, risk_level, source_id, updated_at)
app_installed(id, slug, version, status, service_name, config_json, installed_at)

-- 运维
jobs(id, type, status, progress, message, created_at, finished_at)
backups(id, filename, size, kind, created_at)
```

设计要点：
- `clients.email` 是 Xray 流量统计的唯一主键，**必须全局唯一**，创建时校验。
- 流量双写：`clients` 存累计值（快查），`traffic_daily` 存日聚合（出趋势图、算账单）。
- 所有时间统一 UTC 存储，展示按设置时区转换。

---

## 12. API 约定

- 风格：REST + JSON，前缀 `/api/v1`
- 鉴权：Cookie 会话（浏览器）或 `X-API-Token`（脚本/CLI）
- 响应：`{ "code": 0, "msg": "ok", "data": {...} }`，`code != 0` 为业务错误
- 错误码：`1001` 未登录 / `1002` 无权限 / `1003` 需二次验证 / `2001` 参数错误 / `2002` 资源不存在 / `3001` 命令执行失败 / `3002` 配置校验失败 / `5001` 内部错误
- 破坏性操作（删站点、改防火墙默认策略、清日志、结束进程）必须带 `confirm: true` 且记录审计
- 长任务返回 `job_id`，前端轮询 `GET /api/v1/jobs/{id}`

---

## 13. 非功能需求

| 类别 | 要求 |
|---|---|
| 性能 | 本地接口 P95 < 150ms；首页首屏 < 800ms；100 个客户端配置生成 < 2s |
| 内存 | 空载 ≤ 50MB；100 客户端 + 20 并发 ≤ 70MB（`GOMEMLIMIT=64MiB` 兜底） |
| 启动 | 冷启动 ≤ 1.5s；崩溃后 systemd 10s 内拉起 |
| 兼容 | Debian 11/12、Ubuntu 22/24、Alma 9；amd64 必测，arm64 交叉编译 |
| 可靠性 | 配置变更零静默失败；DB 损坏可用 `.bak` 恢复；面板挂了不影响数据面 |
| 安全 | 见 §14 安全设计（威胁模型 + 8 层要求 + 安全默认值） |
| 可维护 | 单二进制部署；CLI 提供 reset/restart/info；日志结构化（JSON 可选） |
| 可观测 | `/healthz` 健康检查；`/metrics`（可选，Prometheus 文本格式，默认关闭） |
| 国际化 | 中/英双语，文案抽离为 JSON |

---

## 14. 安全设计

### 14.1 先定威胁模型

| 威胁源 | 能力 | 最可能路径 | 优先级 |
|---|---|---|---|
| 自动化扫描 / 撞库僵尸网络 | 低，但每天都有 | 扫端口 → 撞面板密码 / SSH 弱口令 | **高**（每天都在发生） |
| 机会型攻击者 | 中 | 已知 CVE、默认配置、暴露的数据库 | 高 |
| 定向攻击者 | 高 | 供应链、社工、0day | 低（个人 VPS 不值得） |
| **自己（误操作）** | — | 防火墙改错锁死、删错文件、覆盖配置 | **高**（实际最高频） |

诚实结论：个人 VPS 上 90% 的真实风险是「扫描撞库」和「自己手抖」，不是 APT。
安全投入按这个排序做，不做表演性的安全。

### 14.2 八层要求

#### A 暴露面（收益最高，先做这个）

| 编号 | 要求 | 说明 |
|---|---|---|
| S-A01 | 面板默认只监听 `127.0.0.1` | 装完从公网**扫不到**面板端口，这是最有效的一招 |
| S-A02 | 三种访问模式，安装时三选一 | ① SSH 隧道（默认）② 白名单 IP 直连 ③ Nginx 反代 + TLS + 强制 2FA |
| S-A03 | IP 白名单 | 面板与 SSH 都支持「仅允许列表内 IP」，安装时自动加入当前出口 IP |
| S-A04 | 非标端口 + 随机路径 | **只作降噪，不作为安全边界**（UI 上写明，避免虚假安全感） |
| S-A05 | 最小服务集 | 默认只装面板 + Xray，其余按需；不监听任何无用端口 |
| S-A06 | 端口暴露清单 | 首页常驻「当前对外监听端口」表，新增监听时高亮 |

#### B 认证与会话

| 编号 | 要求 | 说明 |
|---|---|---|
| S-B01 | 密码 | argon2id；≥12 位；拒绝内置 top 10000 弱密码表 |
| S-B02 | 2FA | TOTP；**面板一旦监听公网就强制开启**；提供 10 个一次性恢复码 |
| S-B03 | 登录限流 | 同 IP 5 次失败封 1h；同账号 10 次锁 30min（两套独立计数） |
| S-B04 | 会话 | 登录后轮换 session id；HttpOnly + Secure + SameSite=Lax；空闲 2h；并发上限 3 |
| S-B05 | 敏感操作 sudo mode | 终端、防火墙默认策略、删除站点、改密码、关 2FA → 需重输密码或 TOTP，5 分钟内免重复 |
| S-B06 | 登录通知 | 新 IP 首次登录 → 邮件/TG 告警（含 IP、归属地、UA） |
| S-B07 | 恢复码 | 10 个一次性码，哈希存储，用掉即失效，可重新生成 |

#### C SSH 加固（面板管 SSH，但不替代 SSH）

| 编号 | 要求 | 说明 |
|---|---|---|
| S-C01 | 配置体检 | 检查密码登录、root 登录、端口、是否仅 IPv4、是否有可用公钥、`PermitEmptyPasswords` |
| S-C02 | 一键禁密码登录 | **前置门禁：必须检测到 ≥1 个可用公钥，且当前会话就是密钥登录**，否则拒绝执行 |
| S-C03 | 禁止 root 直接登录 | 可选；要求已存在可 sudo 的普通用户，否则拒绝 |
| S-C04 | 改 SSH 端口 | 同步防火墙规则；旧端口保留 5 分钟再关；改完展示「断开后如何用 VNC/救援模式回滚」 |
| S-C05 | 爆破封禁 | 读 `journalctl -u ssh`，同 IP 5 次失败 → nft set 封 1h；**不装 fail2ban**（一个 goroutine 就够） |
| S-C06 | 变更保险 | 每次 SSH 变更前生成 `/root/ssh-rollback-<ts>.sh` 并打印一行救援命令 |

> S-C02 的门禁是这一节最重要的设计。太多人关掉密码登录后才发现自己没配公钥，直接锁死在门外。

#### D 端口扫描防护（先说清它能干什么）

**判断：扫描挡不住，也不值得挡。** 公网 VPS 每天被扫几千次是常态，扫描源是分布式的，封 IP 只是打地鼠。
真正有效的只有三件事：

1. **不监听**（S-A01，唯一根治）
2. **默认拒绝 + 白名单**（把「被扫到」变成「扫到也没用」）
3. **降噪**：对异常源做短期封禁，减少日志噪音和撞库尝试

| 编号 | 要求 | 说明 |
|---|---|---|
| S-D01 | 默认策略 | 入站 DROP、出站 ACCEPT；改默认策略必须 sudo mode |
| S-D02 | 扫描降噪 | nft meter 统计每源 IP 60s 新建连接数；>20 且命中 >10 个不同端口 → 加入 ban set 10min |
| S-D03 | 不落盘 | 被 DROP 的包**只计数不写日志**，避免日志写满磁盘 |
| S-D04 | 可见性 | 首页显示「24h 封禁 IP 数 / TOP 来源」，定位为健康指标而非安全指标 |
| S-D05 | 不做 | 地理封禁、IP 信誉库订阅、蜜罐、IDS——收益远低于维护成本 |

#### E 应用层

| 编号 | 要求 | 说明 |
|---|---|---|
| S-E01 | CSRF | SameSite=Lax + 双提交 Token；所有写操作校验 |
| S-E02 | CSP | `default-src 'self'`；禁 inline script；构建产物用 nonce/hash |
| S-E03 | XSS | 前端禁用 `v-html`；日志详情做白名单过滤 |
| S-E04 | 命令注入 | 一律 `exec.Command` 数组传参、**永不经 shell**；可执行白名单；30s 超时；输出截断 1MB |
| S-E05 | 路径穿越 | `filepath.Clean` + 根目录前缀校验 + 拒绝 `..` 与空字节 + 不跟随符号链接 |
| S-E06 | 文件上传 | 大小限制、扩展名白名单、随机文件名、不落可执行目录、PHP 目录禁执行 |
| S-E07 | SSRF | 反代目标、Webhook、DNS-01 目标做黑名单：`127/8`、`169.254/16`、`::1`、私网段、云元数据地址 |
| S-E08 | 请求限制 | JSON body ≤ 8MB（上传接口单独 2GB）；并发连接上限；全局令牌桶限流 |
| S-E09 | 响应头 | HSTS、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer` |
| S-E10 | 错误处理 | 不外泄堆栈与绝对路径；统一错误码；panic 中间件兜底并记审计 |
| S-E11 | 依赖 | `govulncheck` + `npm audit` 进 CI；锁定版本；生成 SBOM |

#### F 主机与进程

| 编号 | 要求 | 说明 |
|---|---|---|
| S-F01 | systemd 沙箱 | `ProtectSystem=full`（/usr、/boot、/etc 只读）、`ReadWritePaths` 放行 /etc 下的配置目录、`ProtectKernelTunables`、`RestrictSUIDSGID`、`LockPersonality`、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK` |
| S-F02 | 权限收敛 | `CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_CHOWN CAP_DAC_OVERRIDE CAP_KILL CAP_SETUID CAP_SETGID`；不用 `NoNewPrivileges`（会阻断子进程切换用户） |
| S-F03 | 文件权限 | 私钥 / DB / 恢复码 0600，数据目录 0700，禁止 world-readable |
| S-F04 | 发布完整性 | 二进制附 sha256 + minisign 签名；安装脚本先校验再执行 |
| S-F05 | 自更新 | 校验签名 → 备份旧版 → 原子替换 → 失败回滚 `panelvps.prev` |
| S-F06 | 备份加密 | 备份包默认 age 加密，可选关闭 |

#### G 检测与响应

| 编号 | 要求 | 说明 |
|---|---|---|
| S-G01 | 审计不可篡改 | 审计表禁止 UPDATE/DELETE（SQLite 触发器），保留 90 天，可选同步远端 syslog |
| S-G02 | 关键告警 | 登录失败激增、新 IP 登录、配置变更、Xray 异常退出、磁盘 >85%、内存 >90% |
| S-G03 | 紧急模式 | 一键：关公网监听 + 踢所有会话 + 停 Xray（保留 Nginx），可一键恢复 |
| S-G04 | 变更留 diff | 记录「谁、何时、哪个文件、旧值 → 新值」，配置回滚基于此 |

#### H 明确的安全边界（不吹牛）

- 面板以 root 运行 = **面板被拿下就是机器被拿下**，这是单二进制面板的固有代价，不做虚假承诺。
- 不做 seccomp 白名单与 WAF——与「需要执行任意系统命令」直接冲突。
- 不做入侵检测 / 蜜罐 / 流量分析。
- 安全默认值优先于便利性：默认不外网、默认白名单、默认强密码、公网时默认强制 2FA。

### 14.3 安全默认值（安装完的初始状态）

| 项 | 默认值 | 理由 |
|---|---|---|
| 面板监听 | `127.0.0.1:随机端口` | 公网扫不到 |
| 访问方式 | SSH 隧道（文档给一行命令） | 零暴露 |
| 公网直连 | 默认关闭，需 `--public` 或控制页显式开启 | 默认零暴露，开的时候明确告知只有 HTTP |
| 面板密码 | 随机 20 位，首次登录强制改 | 杜绝弱口令 |
| 2FA | 检测到监听公网则强制开启 | 唯一的第二因子 |
| 防火墙默认策略 | 入站 DROP | 白名单思维 |
| SSH 密码登录 | **不自动关闭**（只告警） | 自动关会锁死人，交给用户显式操作 |
| 终端功能 | 开启，但需 sudo mode | 高频且高危 |
| 面板日志 | journald，`SystemMaxUse=200M` | 磁盘保护 |
| 自动备份 | 开，滚动 7 份，age 加密 | 可恢复性 |

---

## 15. 交付里程碑

按「先能替代 SSH，再谈好用」的顺序切。

| 阶段 | 内容 | 粗估 | 出口标准 |
|---|---|---|---|
| **M0 骨架** | 项目结构、CLI、systemd 沙箱、SQLite 迁移、登录鉴权 + 安全默认值、embed 前端、healthz | 3–5 人日 | 能登录、能改密码、**公网扫不到面板端口**、空载 RSS ≤ 50MB |
| **M1 代理核心** | Xray 集成、入站/客户端 CRUD、配置生成+校验+回滚、Stats API 流量、分享链接/二维码 | 8–10 人日 | 建 VLESS 入站 + 3 用户，客户端连通且计数正确 |
| **M2 运维核心** | M2 监控、M5 文件、M6 终端、M4 防火墙、M7 日志、SSH 加固与封禁 | 10–12 人日 | 不开 SSH 能完成日常 90% 操作；爆破封禁生效 |
| **M3 网站与商店** | M3 站点/证书/PHP、M8 软件商店（清单 + 安装编排） | 8–10 人日 | 静态站 + PHP 站跑通；商店装 nginx/redis 成功并可干净卸载 |
| **M4 打磨** | M10 备份/导入导出/自更新、告警、紧急模式、签名发布、安装脚本、双语文案 | 5–7 人日 | 全新机器 5 分钟内装完可用，二进制可验签 |
| **M5 管理入口** | M11 访问方式与端口、重置密码、Web 终端开关、命令行菜单与 BBR helper 单元 | 4–5 人日 | 浏览器或 `panelvps` 菜单都能把面板从本机切到公网，改完端口仍进得来 |
| 合计 | | **约 36–47 人日（7–9 周，单人业余）** | |

> 估算是按「熟悉 Go + 熟悉 Linux 运维」的前提给的，不含反复调 Xray 配置的踩坑时间。**M1 是最可能超期的一段**。

**建议的 MVP 边界**：M0 + M1 + M2 就能上线（约 3 周），M3 网站管理可以延后——因为你日常最痛的其实是「改代理用户」和「看日志/传文件」。

---

## 16. 验收清单

上线前逐条打勾：

**资源**
- [ ] 面板空载 RSS ≤ 50MB（`grep VmRSS /proc/$(pgrep panelvps)/status`）
- [ ] 面板 + Xray + Nginx + PHP 合计 ≤ 320MB
- [ ] 二进制 ≤ 25MB，前端 gzip ≤ 400KB
- [ ] 磁盘占用（日志 + 备份）≤ 1.5GB

**功能**
- [ ] 建 VLESS+REALITY 入站，3 个用户，手机客户端可连通
- [ ] 流量统计与实际使用误差 < 5%
- [ ] 超流量自动禁用生效，重置后恢复
- [ ] 文件上传 500MB 不中断；编辑器保存不破坏中文编码
- [ ] 终端可正常 vim / top / 中文显示
- [ ] 防火墙放行端口后立即生效，且重启后规则仍在
- [ ] 静态站 + PHP 站可访问，证书自动续期成功
- [ ] 面板进程 kill 后 10s 内自启，网站与代理不中断

**软件商店**
- [ ] 搜索「redis」能在 3 个字符内出结果
- [ ] 安装前能看到完整步骤（apt 包名、要写的文件、要启动的服务）
- [ ] 内存不足时阻止安装 mariadb，并显示「预计 150MB，当前可用 90MB」
- [ ] 卸载后端口回收、进程消失、配置可选保留
- [ ] 清单中出现 `shell:` 动作时，加载直接报错（不允许任意脚本）

**安全**
- [ ] 未登录访问任何 API 返回 1001
- [ ] 路径 `../../etc/passwd` 被拒绝
- [ ] 命令参数含 `; rm -rf` 不生效（无 shell 拼接）
- [ ] 连续 5 次错误密码后 IP 被临时封禁，1h 后自动解封
- [ ] 所有破坏性操作在审计日志中有记录，且审计表无法 UPDATE/DELETE
- [ ] 从外网 `nmap -p-` 扫不到面板端口（默认 127.0.0.1）
- [ ] 面板监听公网时，未开 2FA 无法完成登录流程
- [ ] 一键禁 SSH 密码登录：无公钥时被拒绝执行
- [ ] SSH 改端口后旧端口保留 5 分钟，防火墙规则同步
- [ ] 模拟端口扫描（60s 内连 20 个端口）后，源 IP 进入 ban set 并自动解封
- [ ] `ProtectSystem=full` 下写 `/usr/bin` 失败、写 `/home` 与 `/opt/panelvps/data` 成功
- [ ] 反代目标填 `http://169.254.169.254` 被 SSRF 黑名单拦截

---

## 17. 待决策项（阻塞 v1.0 冻结）

| # | 问题 | 选项 | 默认建议 |
|---|---|---|---|
| Q1 | 目标机器是 512MB 可用内存的 Debian 12 吗？还是也要兼容其他配置？ | A 只针对该机 B 同时兼容标准内核 | **B**：探测式适配成本不高，兼容面更宽 |
| Q2 | 网站管理要不要 PHP + 数据库？ | A 只做静态 + 反代（省 100MB+） B 带 PHP（不带 MySQL） C 带 PHP + 可选 MariaDB | **B**：PHP 站是刚需，MySQL 手动装 |
| Q3 | 代理协议范围？ | A 只 VLESS + REALITY（最省） B + VMess/Trojan/SS（4 协议） C 对齐 3x-ui 全协议 | **B**：4 协议覆盖 95% 场景，C 是范围陷阱 |
| Q4 | 是否只自用、永不分发？ | A 纯自用 B 将来可能开源 C 将来可能商用 | ✅ **已定 B**：按洁净室实现，保留许可自由（ADR-11） |
| Q5 | 交付节奏偏好？ | A 一次性做完 5 个模块 B 先 MVP（M0–M2）再迭代 | **B**：3 周能看到东西，避免半途而废 |
| Q6 | 面板怎么访问？ | A 仅 SSH 隧道 B 白名单 IP 直连 C 公网 + Nginx 反代 + 强制 2FA | **B**：比 A 少一步、比 C 安全得多 |
| Q7 | 商店清单规模？ | A 内置 16 个且封闭 B 支持第三方源 | **A**：先封闭，跑顺了再谈扩展 |

---

## 附录 A：可直接复用的外部能力

| 能力 | 用什么 | 说明 |
|---|---|---|
| 代理内核 | Xray-core 官方 release | 面板只做编排，不编译 |
| PTY | `github.com/creack/pty` | 无 CGO |
| 终端前端 | `xterm.js` | 成熟稳定 |
| SQLite | `modernc.org/sqlite` | 纯 Go，免 CGO |
| 系统指标 | `github.com/shirou/gopsutil/v4` | 纯 Go，或直接读 /proc |
| 证书 | `certbot` / `acme.sh` | 不自己实现 ACME |
| 二维码 | `github.com/skip2/go-qrcode` | 纯 Go |

## 附录 B：术语

| 术语 | 含义 |
|---|---|
| 入站 Inbound | Xray 的监听端点（端口 + 协议 + 传输 + 安全层） |
| 客户端 Client | 挂在入站下的一个用户身份，用 email 唯一标识 |
| Stats API | Xray 内置 gRPC 统计服务，按 tag/email 提供流量计数 |
| REALITY | Xray 的无证书 TLS 伪装方案，steal 目标站点证书 |
| 数据面 / 管理面 | Xray+Nginx 承载业务 vs 面板只负责配置它们 |
