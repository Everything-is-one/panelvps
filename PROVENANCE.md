# 来源与合规记录（Provenance）

> 本文件记录本项目每个模块的**设计输入来源**与**实现方式**，用于将来开源、分发或商用时的合规举证。
> **规则：任何新模块在合并前，必须在此登记一行。**

## 一、结论先行

- 本项目所有代码为**独立实现**（clean-room implementation）。
- **未复制、未改写**任何 GPL-3.0 项目的源代码、配置文件模板、SQL schema、UI 文案或图标。
- 第三方依赖仅通过包管理器 / Go module / npm 引入，各自遵守原许可。

## 二、参考项目与许可

| 项目 | 许可 | 本项目的使用方式 | 是否复制代码 |
|---|---|---|---|
| [vaxilu/x-ui](https://github.com/vaxilu/x-ui) | **GPL-3.0** | 仅阅读 README 与公开说明，确认「代理面板需要哪些功能」 | ❌ 否 |
| [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui) | **GPL-3.0** | 仅阅读 README/文档，确认协议支持范围与产品边界 | ❌ 否 |
| [tnb-labs/panel](https://github.com/tnb-labs/panel)（耗子面板） | BSD-3-Clause | 仅参考产品形态与功能取舍 | ❌ 否 |
| [XTLS/Xray-core](https://github.com/XTLS/Xray-core) | **MPL-2.0** | 作为**独立子进程**调用（配置文件 + gRPC API），**不 import 其 Go 包** | ❌ 否 |

### 关于 MPL-2.0 的说明

Xray-core 采用 MPL-2.0，属于**文件级** copyleft：修改其源文件需公开该文件的修改，但不要求衍生作品整体开源。
本项目以「独立进程 + 配置文件 + gRPC」方式集成，**不链接其代码**，因此不受 MPL 约束。
若将来需要 import 其 Go 包（例如复用配置结构体），相关文件须遵守 MPL-2.0 并单独标注。

## 三、实现依据（只用公开规范）

| 模块 | 设计输入 |
|---|---|
| Xray 配置结构（log/api/stats/policy/inbounds/outbounds/routing） | Xray-core 官方文档 |
| 流量统计 | Xray 官方 `StatsService` gRPC 接口定义 |
| VLESS/VMess/Trojan/SS 分享链接格式 | 公开 URI 规范与各协议文档 |
| Nginx 站点/反代/PHP 配置 | nginx 官方文档 |
| nftables 规则与 set/timeout 用法 | nftables wiki、`man nft` |
| systemd 沙箱与加固参数 | `systemd.exec(5)`、`systemd.resource-control(5)` |
| Let's Encrypt 签发 | ACME RFC 8555、certbot / acme.sh 官方文档 |
| 面板 API 与安全基线 | OWASP ASVS 4.0（选取 L1 相关条目） |

## 四、红线（每次提交前自查）

- [ ] 没有 `.go` / `.vue` / `.ts` / `.sh` 文件是从 GPL 项目复制或「改名改写」而来
- [ ] 没有复制对方的配置文件模板、SQL schema、UI 文案、图标、测试用例
- [ ] 没有使用 `x-ui` / `3x-ui` / `宝塔` 作为项目名、模块名、包名或 UI 品牌
- [ ] commit message 中不出现 `ported from` / `copied from` / `based on <GPL repo>` 等表述
- [ ] 新增第三方依赖已登记在第五节，并确认许可兼容

## 五、第三方依赖清单

> 直接依赖人工核对（`go list -m` 的模块目录下 LICENSE 文件），间接依赖均为上述依赖带入的
> 宽松许可（BSD-3-Clause / MIT / Apache-2.0），CI 接入 `go-licenses` 后自动复核。

| 依赖 | 版本 | 许可 | 用途 |
|---|---|---|---|
| modernc.org/sqlite | v1.39.1 | BSD-3-Clause | 纯 Go SQLite 驱动，避免 cgo |
| golang.org/x/crypto | v0.45.0 | BSD-3-Clause | argon2id 密码哈希 |
| golang.org/x/sys | v0.38.0 | BSD-3-Clause | 系统调用（终端、防火墙相关） |
| gopkg.in/yaml.v3 | v3.0.1 | MIT + Apache-2.0（双许可） | 配置文件与软件清单解析 |
| github.com/creack/pty | v1.1.24 | MIT | Web 终端的伪终端 |
| github.com/gorilla/websocket | v1.5.3 | BSD-3-Clause | 终端 WebSocket 传输 |
| github.com/skip2/go-qrcode | v0.0.0-20200617 | MIT | 分享链接二维码，服务端生成 |

**外部程序**（不作为代码链接，仅以子进程调用，各自遵守原许可）：

| 程序 | 许可 | 用途 |
|---|---|---|
| Xray-core | MPL-2.0 | 代理能力；面板只生成配置并通过 CLI/API 调用，不链接其代码 |
| nginx / certbot / apt / nftables | 各自发行版许可 | 站点下发、证书、软件安装、防火墙 |

MPL-2.0 是文件级 copyleft：只要不修改并分发 Xray 自身文件，就不影响本项目的许可选择。
面板从不内嵌或改写 Xray 源码，只写 JSON 配置并调用其二进制。

## 六、为什么这样做

著作权保护的是**表达**，不是**功能**。独立实现同样功能不构成侵权——这是基本原则。
真正的风险在于「**实质性相似**」：如果一边看 GPL 源码一边写，函数拆分、变量命名、注释、配置模板、UI 文案都可能被认定为衍生作品。

所以纪律比声明更重要：

1. **输入换源**：以官方文档为设计输入，而不是以别人的源码为输入。
2. **时间隔离**：确需查阅 GPL 源码确认行为时，读完不要立刻写代码，间隔一段时间再实现，避免逐行复刻。
3. **留痕**：保留设计草稿与提交历史，证明独立演进过程。

**洁净室实现的回报是许可自由**：没有复制 GPL 代码，就可以自由选择 MIT / Apache-2.0 / GPL-3.0。
本项目最终选用 **Apache-2.0**（2026-11-21 开源时确定），兼顾专利授权与商业友好。
一旦复制了，整个项目就只能 GPL-3.0——这才是真正的成本。
