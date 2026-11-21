# PanelVPS

> 1GB 内存 VPS 上的轻量运维面板，内置 Xray 代理管理。

## 这是什么

一台 2C1G 的 Debian VPS：装宝塔要吃掉 250–400MB，装 x-ui 又只有代理、没有运维。
PanelVPS 用一个 **≤ 50MB 的 Go 单二进制**，同时提供：

- **宝塔式的日常运维**：资源监控、文件管理、Web 终端、日志、防火墙、网站
- **x-ui 式的代理管理**：VLESS / VMess / Trojan / Shadowsocks、多用户、流量与到期限制

**不依赖 Docker**（目标机 4.14 内核跑不动 runc），**不依赖 Python / Node 运行时**。

## 当前状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| M0 | 骨架：CLI、SQLite、认证、会话、安全默认值、embed 前端、healthz | ✅ 已完成 |
| M1 | 代理：Xray 编排、入站/客户端、流量统计、分享链接 | ⬜ 未开始 |
| M2 | 运维：监控、文件、终端、防火墙、日志 | ⬜ 未开始 |
| M3 | 网站与商店：站点/证书/PHP、声明式软件商店 | ⬜ 未开始 |
| M4 | 打磨：备份、告警、自更新、签名发布 | ⬜ 未开始 |

## 三条硬约束

1. 面板空载 RSS ≤ 50MB，二进制 ≤ 25MB（当前 12MB）
2. 面板 + Xray + Nginx + PHP ≤ 320MB（1GB 机器留 700MB 给业务）
3. 不做容器、不做无限扩展的应用市场、不做多节点

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

### 安装到服务器

```bash
sudo ./deploy/install.sh
```

安装后通过 SSH 隧道访问（推荐，面板不暴露公网）：

```bash
ssh -L 20873:127.0.0.1:20873 root@<服务器IP>
```

### 常用命令

```bash
panelvps serve            # 启动（默认子命令）
panelvps version          # 版本
panelvps info             # 配置、路径、账号数
panelvps init             # 创建管理员
panelvps reset-password   # 重置为随机密码并踢掉所有会话
```

## M0 已实现的契约

| 能力 | 说明 |
|---|---|
| 认证 | argon2id（m=19MiB，为 1GB 机器选的低内存档）、12 位起步的密码策略、弱密码表 |
| 会话 | 令牌只存 SHA-256、HttpOnly + SameSite=Lax、改密后踢掉所有旧会话 |
| 防爆破 | 同 IP 5 次失败封 1 小时，进程内计数 |
| CSRF | 双提交 Cookie + `X-CSRF-Token` 请求头 |
| 响应头 | CSP、HSTS、nosniff、DENY frame、no-referrer |
| 审计 | 只追加表，`UPDATE`/`DELETE` 由 SQLite 触发器拦截 |
| 监控 | 直接读 `/proc`，不依赖 cgo 与第三方库 |
| 部署 | systemd 单元带 `ProtectSystem=strict` 与能力收敛 |

> M0 的前端是零构建原生 JS（约 300 行），目的是让 `go build` 不依赖 Node。
> M1 接入 Vite + Vue 3 后产物仍 embed 到同一位置，二进制接口不变。

## 目录结构

```
cmd/panelvps/        CLI 入口（serve / init / info / reset-password）
internal/
  auth/              密码哈希、密码策略、会话令牌、登录限流
  config/            YAML 配置与校验
  metrics/           /proc 指标采集
  server/            路由、中间件、API 处理器
  store/             SQLite 访问层与迁移
  version/           构建期注入的版本信息
web/                 embed 的前端静态资源
deploy/              systemd 单元与安装脚本
scripts/             构建脚本
```

## 文档

| 文件 | 说明 |
|---|---|
| [PRDv0.3.md](PRDv0.3.md) | **当前有效版本**：需求、架构、安全设计、ADRs |
| [PRDv0.2.md](PRDv0.2.md) | 上一版 |
| [PRDv0.1.txt](PRDv0.1.txt) | 原始三句话需求 |
| [PROVENANCE.md](PROVENANCE.md) | 代码来源与许可合规记录 |

## 许可

待定。当前为私有仓库，未授予任何许可。
实现遵循洁净室纪律，未复制 GPL 代码，因此将来可自由选择许可；详见 [PROVENANCE.md](PROVENANCE.md)。
