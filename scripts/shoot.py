# -*- coding: utf-8 -*-
"""对本地测试面板截取指定视图，用于改版前后对比。

用法：
    PV_PASSWORD=初始密码 python scripts/shoot.py overview proxy relay
首次登录会先把密码改成 Test123456，之后的截图都用新密码。
"""
import os
import sys

import requests
from playwright.sync_api import sync_playwright

BASE = os.environ.get("PV_BASE", "http://127.0.0.1:20977")
OLD = os.environ.get("PV_PASSWORD", "")
NEW = "TestPass123!"

LABELS = {
    "overview": "概览", "proxy": "代理", "relay": "中转", "sites": "网站",
    "files": "文件", "terminal": "终端", "firewall": "防火墙", "logs": "日志",
    "store": "商店", "backup": "备份", "settings": "设置",
}


def prepare_password():
    """用初始密码登录并把密码改成 NEW，返回浏览器登录要用的密码。"""
    s = requests.Session()
    s.get(BASE + "/api/v1/auth/state")
    h = {"X-CSRF-Token": s.cookies.get("pv_csrf", "")}
    r = s.post(BASE + "/api/v1/auth/login",
               json={"username": "admin", "password": OLD}, headers=h)
    if r.status_code != 200:
        # 可能已经改过了，直接用新密码
        r2 = s.post(BASE + "/api/v1/auth/login",
                    json={"username": "admin", "password": NEW}, headers=h)
        if r2.status_code == 200:
            return NEW
        sys.exit("登录失败: %s %s" % (r.status_code, r.text[:200]))
    body = r.json()
    data = body.get("data") or body
    if data.get("must_change_password"):
        r3 = s.post(BASE + "/api/v1/auth/password",
                    json={"current": OLD, "new": NEW}, headers=h)
        if r3.status_code != 200:
            sys.exit("改密失败: %s %s" % (r3.status_code, r3.text[:200]))
        return NEW
    return OLD


def main(views):
    pwd = prepare_password()
    with sync_playwright() as p:
        b = p.chromium.launch()
        pg = b.new_page(viewport={"width": 1440, "height": 900}, device_scale_factor=2)
        pg.goto(BASE + "/", wait_until="networkidle")
        pg.fill("#u", "admin")
        pg.fill("#p", pwd)
        pg.click('button[type="submit"]')
        pg.wait_for_timeout(1400)
        if pg.query_selector(".login-card"):
            b.close()
            sys.exit("仍在登录页，密码可能不对")
        for v in views:
            label = LABELS.get(v, v)
            pg.click('.nav-item[title="%s"]' % label)
            pg.wait_for_timeout(700)
            path = "data/test-run/ui-%s.png" % v
            pg.screenshot(path=path, full_page=True)
            print("shot", v, path)
        b.close()


if __name__ == "__main__":
    main(sys.argv[1:] or ["overview"])
