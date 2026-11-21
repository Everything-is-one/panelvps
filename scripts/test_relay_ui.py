"""v0.3.9 自测：中转规则 + 入站编辑。

不依赖 Xray：本机没有 xray 时 apply 会返回「已保存但下发失败」，
这是预期行为，测试只校验数据库与转发链路。
"""
import json
import os
import socket
import sys
import time

import requests
from playwright.sync_api import sync_playwright

BASE = "http://127.0.0.1:20977"
OLD_PWD = os.environ.get("PV_PASSWORD", "")  # 用面板初始化时打印的初始密码
NEW_PWD = "TestPass123!"

ok_count = 0
fail_count = 0


def check(name, cond, extra=""):
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  PASS {name}")
    else:
        fail_count += 1
        print(f"  FAIL {name} {extra}")


s = requests.Session()
s.get(BASE + "/api/v1/auth/state")
csrf = s.cookies.get("pv_csrf")
H = {"X-CSRF-Token": csrf}

r = s.post(BASE + "/api/v1/auth/login", json={"username": "admin", "password": OLD_PWD}, headers=H)
print("login:", r.status_code, r.text[:120])
if r.status_code != 200:
    sys.exit("登录失败（先设置环境变量 PV_PASSWORD）")
r = s.post(BASE + "/api/v1/auth/password",
           json={"current": OLD_PWD, "new": NEW_PWD}, headers=H)
print("改密:", r.status_code, r.text[:120])

# 预置一个入站，供编辑测试（本机无 xray，apply 会失败但记录会落库）
r = s.post(BASE + "/api/v1/proxies/inbounds", json={
    "remark": "编辑前", "protocol": "vless", "listen": "0.0.0.0", "port": 39003,
    "public_port": 39004, "network": "tcp", "security": "none", "options": {},
}, headers=H)
print("建入站:", r.status_code, r.text[:200])
inbound_id = (r.json().get("data") or {}).get("id")

with sync_playwright() as p:
    b = p.chromium.launch()
    page = b.new_page(viewport={"width": 1440, "height": 980})

    page.goto(BASE, wait_until="networkidle")
    page.fill("#u", "admin")
    page.fill("#p", NEW_PWD)
    page.click("button[type=submit]")
    page.wait_for_selector(".sidebar", timeout=10000)
    print("已登录")

    # ---- 中转页 ----
    page.click(".nav-item:has-text('中转')")
    page.wait_for_selector("text=中转（端口转发）", timeout=8000)
    check("中转页可打开", page.locator(".card h2:has-text('规则')").count() == 1)

    page.click("button:has-text('+ 新建中转')")
    page.wait_for_selector(".modal", timeout=5000)

    def fill(label, value):
        box = page.locator(".modal .field").filter(has_text=label).first
        box.locator("input").first.fill(value)

    fill("名称", "本机自测")
    fill("监听地址", "127.0.0.1")
    fill("监听端口", "39001")
    fill("目标地址", "127.0.0.1:39002")
    page.click(".modal button:has-text('创建')")
    page.wait_for_timeout(1200)

    body = page.inner_text("body")
    check("规则出现在列表", "本机自测" in body, body[:200])
    check("卡片显示目标", "127.0.0.1:39002" in body)

    # ---- 转发链路实测 ----
    time.sleep(0.5)
    try:
        c = socket.create_connection(("127.0.0.1", 39001), timeout=3)
        c.sendall(b"ping")
        data = c.recv(64)
        c.close()
        check("TCP 转发可用", data == b"ECHO:ping", f"收到 {data!r}")
    except Exception as e:  # noqa: BLE001
        check("TCP 转发可用", False, str(e))

    page.screenshot(path="data/test-run/relay.png", full_page=True)

    # ---- 入站编辑 ----
    page.click(".nav-item:has-text('代理')")
    page.wait_for_selector(".inbound", timeout=8000)
    check("入站卡片有编辑按钮", page.locator(".inbound button:has-text('编辑')").count() >= 1)

    page.click(".inbound button:has-text('编辑')")
    page.wait_for_selector(".modal", timeout=5000)
    port_val = page.locator(".modal .field").filter(has_text="端口").first.locator("input").first.input_value()
    remark_val = page.locator(".modal .field").filter(has_text="备注").first.locator("input").first.input_value()
    pp_val = page.locator(".modal .field").filter(has_text="对外端口").first.locator("input").first.input_value()
    check("编辑表单回填监听端口", port_val == "39003", port_val)
    check("编辑表单回填备注", remark_val == "编辑前", remark_val)
    check("编辑表单回填对外端口", pp_val == "39004", pp_val)

    page.locator(".modal .field").filter(has_text="备注").first.locator("input").first.fill("编辑后")
    page.click(".modal button:has-text('保存')")
    page.wait_for_timeout(1200)
    page.screenshot(path="data/test-run/proxy.png", full_page=True)

    if page.locator(".modal").count():
        page.click(".modal button:has-text('关闭')")

    r = s.get(BASE + "/api/v1/proxies")
    rows = (r.json().get("data") or {}).get("inbounds") or []
    target = next((x for x in rows if x.get("id") == inbound_id), None)
    check("编辑已落库", target and target.get("remark") == "编辑后", json.dumps(target, ensure_ascii=False)[:200] if target else "未找到")

    # 清空对外端口：验证指针语义（0 能覆盖旧值）
    if target:
        r = s.patch(f"{BASE}/api/v1/proxies/inbounds/{inbound_id}", json={
            "remark": "编辑后", "protocol": target["protocol"], "listen": target["listen"],
            "port": target["port"], "public_port": 0, "network": target["network"],
            "security": target["security"], "options": {},
        }, headers=H)
        r2 = s.get(BASE + "/api/v1/proxies")
        rows = (r2.json().get("data") or {}).get("inbounds") or []
        t2 = next((x for x in rows if x.get("id") == inbound_id), None)
        check("对外端口可清空", t2 and t2.get("public_port") == 0,
              f"patch={r.status_code} public_port={t2.get('public_port') if t2 else None}")

    b.close()

print(f"\n结果：{ok_count} 通过 / {fail_count} 失败")
sys.exit(0 if fail_count == 0 else 1)
