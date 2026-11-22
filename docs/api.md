# HTTP 接口

面板前端只用这些接口，没有隐藏入口。所有写操作都要 `X-CSRF-Token`，敏感动作还要
`confirm=<关键字>` 二次确认。

## 控制

```
GET    /api/v1/control                  面板 / 账号 / BBR / Xray / 防火墙 / 系统信息
PUT    /api/v1/control/access           改监听地址与端口（confirm=ACCESS）
POST   /api/v1/control/password         重置密码（confirm=RESET，mode=random|custom）
POST   /api/v1/control/bbr              开 / 关 BBR（confirm=BBR）
POST   /api/v1/control/xray/restart     重启 Xray
POST   /api/v1/control/reboot           重启服务器（confirm=REBOOT）
```

## 代理

```
GET    /api/v1/proxies                     概览（状态 + 入站 + 用户 + 链接）
POST   /api/v1/proxies/apply               重建并下发配置
POST   /api/v1/proxies/traffic/collect     立即采样一次流量
PUT    /api/v1/proxies/preferences         设置分享链接用的公网地址
POST   /api/v1/proxies/keys/reality        生成 REALITY 密钥对
POST   /api/v1/proxies/inbounds            新建入站
PATCH  /api/v1/proxies/inbounds/{id}       修改入站（部分字段）
DELETE /api/v1/proxies/inbounds/{id}       删除入站（级联删除用户）
POST   /api/v1/proxies/inbounds/{id}/clients  新建用户
PATCH  /api/v1/proxies/clients/{id}        修改用户
DELETE /api/v1/proxies/clients/{id}        删除用户
POST   /api/v1/proxies/clients/{id}/reset  重置流量
GET    /api/v1/proxies/clients/{id}/links  分享链接
GET    /api/v1/proxies/clients/{id}/qr     二维码 PNG
```

Xray 可执行文件按以下顺序探测：`PANELVPS_XRAY_BIN` 环境变量 → 配置项 `xray_bin`
→ `/usr/local/bin/xray` → `/usr/bin/xray` → 面板同目录 → `PATH`。找不到时面板照常启动，
只是代理页显示「不可用」。

## 中转（端口转发）

```
GET    /api/v1/relays              规则列表（含实时上下行与连接数）
POST   /api/v1/relays              新建规则
PATCH  /api/v1/relays/{id}         修改规则
DELETE /api/v1/relays/{id}         删除规则
POST   /api/v1/relays/{id}/toggle  启用 / 停用
```

规则字段：`name`、`listen`（IP，默认 `0.0.0.0`）、`listen_port`、`target`（`host:port`）、
`network`（`tcp` / `udp` / `both`）。转发引擎不经过 Xray，改规则不会重启 Xray，不会掐断
在线的代理连接；统计只存在内存里，面板重启后清零。

中转机没有装面板也能用同一个二进制跑：

```bash
panelvps relay -listen :8443 -to 1.2.3.4:443        # 只转 TCP
panelvps relay -listen :8443 -to 1.2.3.4:443 -udp   # TCP + UDP
```

协议内容全程透传（REALITY / TLS / gRPC 都不受影响），客户端只改地址与端口，其余参数不动。

## 系统与运维

```
GET    /api/v1/system/stat              资源指标（CPU / 内存 / 磁盘 / 负载）
GET    /api/v1/system/info              系统环境（发行版 / 内核 / cgroup / 时区）
GET    /api/v1/system/disk-usage        磁盘占用扫描（path=&refresh=1，异步任务）
GET    /api/v1/files                    目录列表
GET    /api/v1/files/search             按文件名搜索（q=&limit=，子目录递归）
GET    /api/v1/security/records         外网攻击记录（minutes=）
GET    /api/v1/updates/state            版本检查结果
```

磁盘扫描是异步的：首次请求返回 `state=running`，前端轮询同一路径直到 `done`，
结果缓存 3 分钟，同一路径同时只跑一个任务、全局最多 2 个并发。
