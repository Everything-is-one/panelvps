# 安装

Debian 12 / Ubuntu 22.04+，amd64 与 arm64 均支持。安装脚本自动判断架构、下载对应
二进制、校验 SHA256、装好 systemd 服务，最后把初始密码打印出来。

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh | sudo bash
```

生产环境建议锁定版本，让脚本和二进制取同一个 tag，避免 `main` 更新后错配：

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/v0.3.15/deploy/bootstrap.sh \
    | sudo bash -s -- --ref v0.3.15
```

只体检、不安装（逐项报告能力，不改动系统）：

```bash
curl -fsSL https://raw.githubusercontent.com/Everything-is-one/panelvps/main/deploy/bootstrap.sh \
    | sudo bash -s -- --check
```

## 参数

| 参数 | 作用 |
|---|---|
| `--ref TAG` | 锁定版本，脚本与二进制取同一个 tag |
| `--base URL` | 换分发源，见「自建分发」 |
| `--public` | 装完直接监听 `0.0.0.0`，可用 `IP:端口` 访问 |
| `--check` | 只体检，不安装 |

卸载走 `deploy/` 目录里的安装脚本，数据目录会保留：

```bash
sudo ./deploy/install.sh --uninstall
```

## 安装后访问

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

装完会打印访问地址，并提醒去云厂商安全组放行该端口。已经装好的不用重装：登录后进
「设置 → 访问方式」，勾上「允许通过 IP + 端口访问」并保存，面板会改监听地址、同步防火墙
规则，然后自己重启。

同一个地方还能改端口（1–65535）。保存顺序是先试绑新端口 → 更新防火墙 → 重启面板，
任何一步失败都回滚，不会出现「新端口没放行、旧端口已关」把自己锁在门外。

> 面板自己不做 HTTPS，公网直连就是明文 HTTP。要么在前面套一层 TLS 反向代理，要么至少用
> 16 位以上随机密码，并且只放行自己的出口 IP。

## 在 LXC / 容器里安装

LXC 小鸡共享宿主内核，容器内的 systemd 通常**没有** `CAP_NET_ADMIN` / `CAP_SYS_ADMIN`，
主单元里的 `AmbientCapabilities`、`MemoryMax`、`ProtectSystem` 会让服务直接启动失败。
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

### 能力不足时的替代路径

- **绑不了 80/443**：让宿主设置 `net.ipv4.ip_unprivileged_port_start=0`
  （LXC 配置 `lxc.sysctl.net.ipv4.ip_unprivileged_port_start = 0`），或让 Nginx 听 8080、
  由宿主 Nginx / 端口映射转发 80 → 8080。
- **防火墙不可下发**：安全边界退化为「只监听 127.0.0.1 + SSH 隧道」。在容器里刷 iptables
  会和宿主规则打架，商家一般也不允许，不建议强求。
- **想在 NAT 小鸡上直连面板**：进「设置 → 访问方式」勾上公网访问，或把 `conf/panel.yaml`
  的 `listen` 改成 `0.0.0.0:20873`，再用商家给的端口映射访问；只做临时测试，测完改回
  `127.0.0.1`。

### 只想先跑起来看看

不装 systemd 服务：

```bash
mkdir -p /opt/panelvps/{bin,conf,data}
install -m 0755 panelvps-linux-amd64 /opt/panelvps/bin/panelvps
/opt/panelvps/bin/panelvps serve -config /opt/panelvps/conf/panel.yaml
```

首次启动会打印初始密码；`Ctrl-C` 即停，数据留在 `/opt/panelvps/data`。

## 自建分发（国内加速）

跑脚本走 `raw.githubusercontent.com`，下二进制走 Release。这两条路在国内都可能被干扰，
所以 `bootstrap.sh` 内置了回退：直链返回非 200 时自动改用 `api.github.com` 取同一份资产。

仍拉不动时，用发布脚本把安装包推到自己有 HTTPS 的目录，再带 `--base` 安装：

```bash
# 本机执行：交叉编译 → 生成 SHA256SUMS → 上传
./scripts/publish.sh root@1.2.3.4:/www/wwwroot/dl.example.com/panelvps \
    --base https://dl.example.com/panelvps

# 目标机器
curl -fsSL https://dl.example.com/panelvps/bootstrap.sh | sudo bash
```

`SHA256SUMS` 里的路径写成 `bin/<文件名>`，与分发目录的 `bin/` 结构一致；面板按该路径
定位条目，格式不对会直接报校验失败。

## 管理菜单

装完不用记子命令，敲 `panelvps` 回车就是个数字菜单：

```
PanelVPS x.y.z  管理菜单
────────────────────────────────────────────────────
 服务 : 运行中 (pid 2314) · 开机自启
 监听 : 127.0.0.1:20873（仅本机）
 入口 : /a1b2c3d4e5f6a7b8/
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
 11) 修改 SSH 端口
 12) 面板访问后缀（隐藏登录入口）
  0) 退出
```

- **6) 监听地址**：选「公网直连」再填端口，配置落盘后自动重启，并打印可用地址。已下发过
  防火墙规则的会按新端口重下一遍，失败则回滚配置
- **7) 重置密码**：随机 20 位（只显示一次）或自己输入，改完踢掉该账号所有会话
- **8) BBR**：先显示当前的拥塞控制与队列算法，开启后写入 `/etc/sysctl.d/99-panelvps.conf`
- **10) 重启服务器**：要输入 `REBOOT` 确认
- **11) 修改 SSH 端口**：备份 `sshd_config` → 注释掉所有 `Port` 行（含
  `sshd_config.d/*.conf`）→ 在最前面写新端口 → `sshd -t` 校验 → 放行 ufw 与面板规则 →
  `systemctl reload`。任何一步失败都整份回滚。只 reload 不 restart，当前 SSH 会话不会断
- **12) 访问后缀**：给登录入口加一个只有你知道的前缀（如 `/a1b2c3d4/`），不带的路径一律
  404；留空即关闭。也可以用「设置 → 访问后缀」在网页里改

没有 systemd 的环境（非特权容器、前台跑二进制）里，服务相关的项会说明不可用，改配置和
重置密码照常可用。

## 常用命令

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

## 验收测试

装完先跑一遍冒烟测试，它会逐项报告哪些能力在这台机器上**真的**可用：

```bash
pip3 install requests          # 唯一依赖
PV_PASSWORD='初始密码' python3 scripts/smoke_test.py
```

只读模式 31 项，不改任何数据。加 `--write` 会额外验证上传 / 编辑 / 打包 / 解包、防火墙
规则增删、建站与删站；文件放在 `_smoke` 临时目录、站点用 `smoke-*` 命名，结束时全部清理。

```
共 31 项：通过 27，失败 0，跳过 4
```

**SKIP 不是错误**：它表示该能力在当前环境不可用，例如非特权容器拿不到 `CAP_NET_ADMIN`、
Windows 上无法创建 PTY。有 FAIL 时脚本以退出码 1 结束，可用于 CI。
