# PanelVPS

> 1GB 内存 VPS 上的轻量运维面板，内置 Xray 代理管理。

## 这是什么

一台 2C1G 的 Debian VPS：装宝塔要吃掉 250–400MB，装 x-ui 又只有代理、没有运维。
PanelVPS 用一个 **≤ 50MB 的 Go 单二进制**，同时提供：

- **宝塔式的日常运维**：资源监控、文件管理、Web 终端、日志、防火墙、网站
- **x-ui 式的代理管理**：VLESS / VMess / Trojan / Shadowsocks、多用户、流量与到期限制

**不依赖 Docker**（目标机 4.14 内核跑不动 runc），**不依赖 Python / Node 运行时**。

## 当前状态

| 阶段 | 状态 |
|---|---|
| 需求（PRD v0.3） | ✅ 已成型，待锁定 Q1–Q7 |
| 技术选型 | ✅ 已定：Go 单二进制 + SQLite + Xray 子进程 + Vue 3 |
| 代码 | ⬜ 未开始 |

## 三条硬约束

1. 面板空载 RSS ≤ 50MB，二进制 ≤ 25MB
2. 面板 + Xray + Nginx + PHP ≤ 320MB（1GB 机器留 700MB 给业务）
3. 不做容器、不做无限扩展的应用市场、不做多节点

## 文档

| 文件 | 说明 |
|---|---|
| [PRDv0.3.md](PRDv0.3.md) | **当前有效版本**（含安全设计与软件商店） |
| [PRDv0.2.md](PRDv0.2.md) | 上一版 |
| [PRDv0.1.txt](PRDv0.1.txt) | 原始三句话需求 |
| [PROVENANCE.md](PROVENANCE.md) | 代码来源与许可合规记录 |

## 目录规划（代码落地时生效）

```
cmd/panelvps/          主程序入口
internal/
  auth/                认证、会话、TOTP
  monitor/             CPU/内存/磁盘/网络采集
  proxy/               Xray 编排：入站、客户端、流量
  site/                Nginx 站点、证书、PHP
  firewall/            nftables / iptables 后端
  filemgr/             文件管理
  term/                PTY Web 终端
  logview/             journald 与站点日志
  store/               软件商店（清单、安装编排）
  security/            SSH 加固、封禁、审计
  executor/            命令执行、原子落盘、校验回滚
  store(db)/           SQLite 访问层
web/                   Vue 3 + Vite 前端
docs/                  设计与运维文档
```

## 许可

待定。当前为私有仓库，未授予任何许可。
实现遵循洁净室纪律，未复制 GPL 代码，因此将来可自由选择许可；详见 [PROVENANCE.md](PROVENANCE.md)。
