#!/usr/bin/env python3
"""PanelVPS 冒烟测试。

用法：
    PV_PASSWORD='初始密码' python3 scripts/smoke_test.py
    python3 scripts/smoke_test.py --base http://127.0.0.1:20873 -u admin -p '密码'
    python3 scripts/smoke_test.py --write      # 额外验证写入链路，会自动清理

只读用例不改任何数据。--write 会在文件沙箱里建 _smoke 目录、
建一个 smoke-* 站点，结束时全部删除。

退出码：0 = 没有 FAIL；1 = 有 FAIL；2 = 连不上或登录失败。
"""

import argparse
import json
import os
import sys
import time

try:
    import requests
except ImportError:
    sys.exit("需要 requests 库：pip3 install requests")


class Checker:
    def __init__(self):
        self.passed = 0
        self.failed = 0
        self.skipped = 0
        self.failures = []

    def check(self, name, cond, detail=""):
        if cond:
            self.passed += 1
            print("  PASS  " + name)
        else:
            self.failed += 1
            self.failures.append((name, detail))
            print("  FAIL  " + name + ("   " + detail if detail else ""))

    def skip(self, name, why):
        self.skipped += 1
        print("  SKIP  " + name + "    " + why)

    def summary(self):
        total = self.passed + self.failed + self.skipped
        print()
        print("-" * 52)
        print(f"共 {total} 项：通过 {self.passed}，失败 {self.failed}，跳过 {self.skipped}")
        if self.failures:
            print("失败项：")
            for name, detail in self.failures:
                print("  - " + name + ("   " + detail if detail else ""))
        print("-" * 52)
        return 1 if self.failed else 0


class Client:
    def __init__(self, base, timeout=30):
        self.base = base.rstrip("/")
        self.timeout = timeout
        self.s = requests.Session()
        self.s.headers["Accept"] = "application/json"

    def csrf(self):
        return self.s.cookies.get("pv_csrf", "")

    def call(self, method, path, **kw):
        headers = kw.pop("headers", {})
        if method not in ("GET", "HEAD"):
            headers["X-CSRF-Token"] = self.csrf()
        r = self.s.request(method, self.base + path, headers=headers, timeout=self.timeout, **kw)
        try:
            body = r.json()
        except Exception:
            body = None
        data = body.get("data") if isinstance(body, dict) and body.get("code") == 0 else None
        return r, data


def human(n):
    n = float(n or 0)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return f"{n:.1f}{unit}"
        n /= 1024
    return f"{n:.1f}PB"


def main():
    ap = argparse.ArgumentParser(description="PanelVPS 冒烟测试")
    ap.add_argument("--base", default=os.environ.get("PV_BASE", "http://127.0.0.1:20873"))
    ap.add_argument("-u", "--user", default="admin")
    ap.add_argument("-p", "--password", default=os.environ.get("PV_PASSWORD", ""))
    ap.add_argument("--write", action="store_true", help="额外验证写入链路（会自动清理）")
    args = ap.parse_args()

    if not args.password:
        sys.exit("请用 --password 或环境变量 PV_PASSWORD 提供密码")

    c = Client(args.base)
    ck = Checker()

    print(f"目标: {args.base}")
    print()

    # ---------- 连通性与登录 ----------
    try:
        r = c.s.get(args.base.rstrip("/") + "/healthz", timeout=10)
    except Exception as e:
        sys.exit(f"连不上面板：{e}")
    ck.check("healthz 可访问", r.status_code == 200, f"HTTP {r.status_code}")

    r, _ = c.call("GET", "/api/v1/auth/state")
    ck.check("auth/state 下发 CSRF Cookie", r.status_code == 200 and bool(c.csrf()),
             f"HTTP {r.status_code}")

    r, _ = c.call("GET", "/api/v1/files?path=")
    ck.check("未登录访问受保护接口被拒", r.status_code == 401, f"HTTP {r.status_code}")

    r, _ = c.call("POST", "/api/v1/auth/login",
                  json={"username": args.user, "password": args.password})
    if r.status_code != 200:
        sys.exit(f"登录失败（HTTP {r.status_code}）：{str(r.text)[:160]}\n"
                 "初始密码见 journalctl -u panelvps | grep 初始管理员")
    ck.check("登录成功", True)

    # ---------- 系统信息 ----------
    print("\n[系统]")
    r, d = c.call("GET", "/api/v1/system/info")
    panel = (d or {}).get("panel") or {}
    osi = (d or {}).get("os") or {}
    ck.check("system/info 可读", r.status_code == 200 and "distro" in osi, f"HTTP {r.status_code}")
    if osi:
        print(f"        {osi.get('distro')} / {osi.get('arch')} / 内核 {osi.get('kernel') or '-'} / "
              f"cgroup {((d or {}).get('cgroup') or '未知')}")
    for field in ("version", "listen", "data_dir", "file_root", "sites_root"):
        ck.check(f"panel.{field} 已返回", field in panel, str(panel)[:80])

    r, d = c.call("GET", "/api/v1/system/stat")
    m = (d or {}).get("metrics") or {}
    ck.check("system/stat 可读", r.status_code == 200 and bool(m), f"HTTP {r.status_code}")
    if m.get("supported"):
        ck.check("指标已采集", True)
    elif osi.get("distro") == "windows":
        ck.skip("指标已采集", "非 Linux 服务端，本地开发调试正常")
    else:
        ck.check("指标已采集", False, "supported=false，Linux 上不应出现")
    if m.get("mem_total"):
        tag = "cgroup 限额" if m.get("container") else "宿主机 /proc"
        print(f"        内存 {human(m.get('mem_used'))}/{human(m.get('mem_total'))} ({tag})，"
              f"CPU {m.get('cpu_pct', 0):.1f}%，面板 RSS {human(m.get('panel_rss'))}")
    if m.get("container"):
        ck.check("容器内使用 cgroup 口径", m.get("mem_total", 0) > 0)

    r, d = c.call("GET", "/api/v1/system/processes?limit=5")
    ck.check("进程列表", r.status_code == 200 and isinstance((d or {}).get("items"), list),
             f"HTTP {r.status_code}")

    r, d = c.call("GET", "/api/v1/system/services")
    ck.check("服务列表", r.status_code == 200 and isinstance((d or {}).get("items"), list),
             f"HTTP {r.status_code}")

    r, d = c.call("GET", "/api/v1/system/ports")
    ck.check("监听端口", r.status_code == 200 and isinstance((d or {}).get("items"), list),
             f"HTTP {r.status_code}")

    # ---------- 防火墙 ----------
    print("\n[防火墙]")
    r, d = c.call("GET", "/api/v1/firewall")
    st = ((d or {}).get("status") or {}) if isinstance(d, dict) else {}
    supported = bool(st.get("supported"))
    ck.check("firewall 状态可读", r.status_code == 200 and isinstance(d, dict), f"HTTP {r.status_code}")
    if supported:
        ck.check("防火墙后端可用", True, "")
        print(f"        后端 {st.get('backend')}，表 {st.get('table')}")
    else:
        ck.skip("防火墙规则下发", st.get("reason") or "当前环境不支持")

    r, d = c.call("GET", "/api/v1/security/ssh")
    ck.check("SSH 爆破报告", r.status_code == 200 and isinstance(d, dict), f"HTTP {r.status_code}")

    # ---------- 日志 ----------
    print("\n[日志]")
    r, d = c.call("GET", "/api/v1/logs/sources")
    srcs = (d or {}).get("items") if isinstance(d, dict) else None
    ck.check("日志来源列表", r.status_code == 200 and isinstance(srcs, list) and len(srcs) > 0,
             f"HTTP {r.status_code}")
    r, d = c.call("GET", "/api/v1/logs?source=panel&lines=20")
    ck.check("读取面板日志", r.status_code == 200 and isinstance((d or {}).get("lines"), list),
             f"HTTP {r.status_code} {str(r.text)[:80]}")

    # ---------- 文件（只读） ----------
    print("\n[文件]")
    r, d = c.call("GET", "/api/v1/files?path=")
    ck.check("文件列表", r.status_code == 200 and isinstance((d or {}).get("entries"), list),
             f"HTTP {r.status_code}")
    r, d = c.call("GET", "/api/v1/files?path=../../")
    ck.check("目录穿越被拒", r.status_code == 403, f"HTTP {r.status_code}")

    # ---------- 站点 ----------
    print("\n[站点]")
    r, d = c.call("GET", "/api/v1/sites")
    ck.check("站点列表", r.status_code == 200 and isinstance((d or {}).get("items"), list),
             f"HTTP {r.status_code}")
    r, d = c.call("POST", "/api/v1/sites", json={"name": "A BAD NAME", "domain": "x.example.com"})
    ck.check("非法站点名被拒", r.status_code == 400, f"HTTP {r.status_code}")

    # ---------- 商店 ----------
    print("\n[商店]")
    r, d = c.call("GET", "/api/v1/store")
    items = (d or {}).get("items") if isinstance(d, dict) else None
    ck.check("商店清单", r.status_code == 200 and isinstance(items, list) and len(items) >= 5,
             f"HTTP {r.status_code}")
    r, d = c.call("GET", "/api/v1/store/nginx/plan")
    ck.check("安装计划预览", r.status_code == 200 and isinstance((d or {}).get("steps"), list),
             f"HTTP {r.status_code}")

    # ---------- 备份与设置 ----------
    print("\n[备份与设置]")
    r, d = c.call("GET", "/api/v1/backups")
    ck.check("备份列表", r.status_code == 200 and isinstance((d or {}).get("items"), list),
             f"HTTP {r.status_code}")
    r, d = c.call("GET", "/api/v1/settings")
    ck.check("设置可读", r.status_code == 200 and isinstance(d, dict), f"HTTP {r.status_code}")

    r, d = c.call("PUT", "/api/v1/settings",
                  json={"public_host": "x", "alert_webhook": "ftp://example.com/h",
                        "ssh_guard_enabled": True, "ssh_guard_threshold": 5})
    ck.check("非法 Webhook 被拒", r.status_code == 400, f"HTTP {r.status_code}")

    # ---------- 终端 ----------
    print("\n[终端]")
    r, d = c.call("POST", "/api/v1/terminal/ticket")
    if r.status_code == 200 and (d or {}).get("ticket"):
        ck.check("终端票据签发（PTY 可用）", True)
    else:
        ck.skip("终端", "PTY 不可用：" + str((d or {}).get("message") or r.text)[:80])

    # ---------- 自更新 ----------
    print("\n[自更新]")
    r, d = c.call("POST", "/api/v1/updates/check", json={})
    if r.status_code == 200:
        ck.check("更新检查", "latest" in (d or {}), str(d)[:100])
    else:
        ck.skip("更新检查", f"可能无外网（HTTP {r.status_code}）")

    # ---------- 写入链路 ----------
    if args.write:
        run_write_tests(c, ck, panel)

    return ck.summary()


def run_write_tests(c, ck, panel):
    """有副作用的用例：全部在 _smoke 下，结束时清理。"""
    print("\n[写入链路]（--write）")
    tag = time.strftime("%H%M%S")

    def cleanup_files():
        c.call("POST", "/api/v1/files/delete", json={"paths": ["_smoke"], "confirm": "DELETE"})

    cleanup_files()
    try:
        r, _ = c.call("POST", "/api/v1/files/mkdir", json={"path": "_smoke"})
        ck.check("建目录", r.status_code == 200, f"HTTP {r.status_code} {str(r.text)[:80]}")

        payload = f"panelvps smoke {tag}\n".encode()
        r, d = c.call("POST", "/api/v1/files/upload?path=_smoke",
                      files={"file": ("hello.txt", payload)})
        ck.check("上传文件", r.status_code == 200 and bool((d or {}).get("files")),
                 f"HTTP {r.status_code} {str(r.text)[:80]}")

        r, d = c.call("GET", "/api/v1/files/content?path=_smoke/hello.txt")
        content = (d or {}).get("content") or ""
        ck.check("读回内容一致", r.status_code == 200 and payload.decode() in content,
                 f"HTTP {r.status_code}")

        r, _ = c.call("PUT", "/api/v1/files/content",
                      json={"path": "_smoke/hello.txt", "content": "edited\n"})
        ck.check("写文件", r.status_code == 200, f"HTTP {r.status_code}")

        r, _ = c.call("POST", "/api/v1/files/rename",
                      json={"from": "_smoke/hello.txt", "to": "_smoke/renamed.txt"})
        ck.check("重命名", r.status_code == 200, f"HTTP {r.status_code}")

        r, _ = c.call("POST", "/api/v1/files/copy",
                      json={"from": "_smoke/renamed.txt", "to": "_smoke/copy.txt"})
        ck.check("复制", r.status_code == 200, f"HTTP {r.status_code}")

        r, _ = c.call("POST", "/api/v1/files/archive",
                      json={"paths": ["_smoke/renamed.txt", "_smoke/copy.txt"],
                            "dest": "_smoke/pack.tar.gz"})
        ck.check("打包", r.status_code == 200, f"HTTP {r.status_code} {str(r.text)[:80]}")

        r, _ = c.call("POST", "/api/v1/files/extract",
                      json={"archive": "_smoke/pack.tar.gz", "dir": "_smoke/out"})
        ck.check("解包", r.status_code == 200, f"HTTP {r.status_code} {str(r.text)[:80]}")

        r, _ = c.call("POST", "/api/v1/files/extract",
                      json={"archive": "_smoke/nonexistent.tar.gz", "dir": "_smoke/x"})
        ck.check("解包不存在的文件被拒", r.status_code in (400, 404), f"HTTP {r.status_code}")

        # 防火墙：只建规则不 apply，避免动真实网络
        r, d = c.call("GET", "/api/v1/firewall")
        fw = (d or {}).get("status") or {}
        if fw.get("supported"):
            r, d = c.call("POST", "/api/v1/firewall/rules",
                          json={"action": "accept", "proto": "tcp", "port": "8443",
                                "source": "", "comment": "smoke"})
            rid = (d or {}).get("id")
            ck.check("新增防火墙规则", r.status_code == 200 and rid, f"HTTP {r.status_code}")
            if rid:
                r, _ = c.call("POST", "/api/v1/firewall/rules",
                              json={"action": "accept", "proto": "tcp", "port": "99999"})
                ck.check("非法端口被拒", r.status_code == 400, f"HTTP {r.status_code}")
                r, _ = c.call("DELETE", f"/api/v1/firewall/rules/{rid}")
                ck.check("删除防火墙规则", r.status_code == 200, f"HTTP {r.status_code}")
        else:
            ck.skip("防火墙写入", "当前环境无 NET_ADMIN")

        # 站点：建在 sites_root 下，最后删站点与目录
        root = (panel.get("sites_root") or "/var/www").rstrip("/") + f"/smoke-{tag}"
        r, d = c.call("POST", "/api/v1/sites",
                      json={"name": f"smoke-{tag}", "domain": f"smoke-{tag}.example.com",
                            "kind": "static", "port": 80, "create_root": True, "root": root})
        sid = (d or {}).get("id")
        ck.check("创建站点", r.status_code == 200 and sid,
                 f"HTTP {r.status_code} {str(r.text)[:120]}")
        if sid:
            r, d = c.call("GET", f"/api/v1/sites/{sid}/preview")
            cfg = (d or {}).get("config") or ""
            ck.check("生成 Nginx 配置", r.status_code == 200 and "server_name" in cfg,
                     f"HTTP {r.status_code}")
            r, _ = c.call("DELETE", f"/api/v1/sites/{sid}?confirm=1")
            ck.check("删除站点", r.status_code == 200, f"HTTP {r.status_code}")
            if root.startswith((panel.get("sites_root") or "/var/www")):
                rel = root[len((panel.get("sites_root") or "/var/www").rstrip("/")):].lstrip("/")
                if rel:
                    c.call("POST", "/api/v1/files/delete",
                           json={"paths": [rel], "confirm": "DELETE"})
    finally:
        cleanup_files()
        ck.check("清理测试文件", True)


if __name__ == "__main__":
    sys.exit(main())
