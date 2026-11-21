"use strict";

// PanelVPS 前端
// 零构建：直接操作 DOM，不引入框架。
// 安全约定：所有动态文本走 textContent，绝不拼接 innerHTML。

const state = {
  csrf: "",
  user: null,
  initialized: true,
  view: "overview",
  timer: null,
  proxies: null,
};

const app = () => document.getElementById("app");

function cookie(name) {
  const m = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
  return m ? decodeURIComponent(m[1]) : "";
}

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v === true ? "" : v);
    }
  }
  (children || []).filter(Boolean).forEach((c) => node.appendChild(c));
  return node;
}

async function api(method, path, body) {
  const headers = { "Accept": "application/json" };
  if (method !== "GET") {
    headers["Content-Type"] = "application/json";
    headers["X-CSRF-Token"] = state.csrf || cookie("pv_csrf");
  }
  const res = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body ? JSON.stringify(body) : undefined,
  });
  let data = null;
  try { data = await res.json(); } catch (_) { /* 空响应 */ }
  if (!res.ok) {
    const msg = (data && data.msg) || ("请求失败 (" + res.status + ")");
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return data;
}

function fmtBytes(n) {
  if (!n) return "0";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(i === 0 ? 0 : 1) + " " + units[i];
}

function pct(used, total) {
  if (!total) return 0;
  return Math.min(100, Math.round((used / total) * 100));
}

function bar(percent) {
  const cls = percent >= 90 ? "bar crit" : percent >= 75 ? "bar warn" : "bar";
  const inner = el("i");
  const box = el("div", { class: cls }, [inner]);
  inner.style.width = percent + "%";
  return box;
}

function statCard(key, value, desc, percent) {
  const kids = [el("div", { class: "k", text: key }), el("div", { class: "v", text: value })];
  if (percent !== undefined) kids.push(bar(percent));
  if (desc) kids.push(el("div", { class: "d", text: desc }));
  return el("div", { class: "stat" }, kids);
}

function badge(text, kind) {
  return el("span", { class: "badge" + (kind ? " " + kind : ""), text: text });
}

function modal(title, bodyNodes, onClose) {
  const close = () => { overlay.remove(); if (onClose) onClose(); };
  const box = el("div", { class: "modal" }, [
    el("div", { class: "modal-head" }, [
      el("h3", { text: title }),
      el("button", { class: "ghost", text: "关闭", onclick: close }),
    ]),
    el("div", { class: "modal-body" }, bodyNodes),
  ]);
  const overlay = el("div", { class: "overlay", onclick: (e) => { if (e.target === overlay) close(); } }, [box]);
  document.body.appendChild(overlay);
  return { close, box };
}

// ---------- 登录 ----------

function renderLogin(message) {
  const username = el("input", { id: "u", autocomplete: "username", placeholder: "用户名" });
  const password = el("input", { id: "p", type: "password", autocomplete: "current-password", placeholder: "密码" });
  const msg = el("div", { class: "msg" + (message ? " err" : ""), text: message || "" });
  const btn = el("button", { type: "submit", text: "登录" });

  const form = el("form", {}, [
    el("label", { text: "用户名" }), username,
    el("label", { text: "密码", style: "margin-top:14px" }), password,
    btn, msg,
  ]);

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    btn.disabled = true;
    msg.className = "msg";
    msg.textContent = "验证中…";
    try {
      const data = await api("POST", "/api/v1/auth/login", {
        username: username.value.trim(),
        password: password.value,
      });
      state.user = data.user;
      if (data.must_change_password) renderChangePassword(true);
      else renderApp("overview");
    } catch (err) {
      msg.className = "msg err";
      msg.textContent = err.message;
      btn.disabled = false;
    }
  });

  app().className = "";
  app().replaceChildren(el("div", { class: "card login-card" }, [
    el("h1", { text: "PanelVPS" }),
    el("p", { class: "sub", text: "轻量运维面板 · 请登录" }),
    form,
  ]));
  setTimeout(() => username.focus(), 50);
}

// ---------- 首次改密 ----------

function renderChangePassword(forced) {
  const oldPwd = el("input", { type: "password", autocomplete: "current-password", placeholder: "当前密码" });
  const newPwd = el("input", { type: "password", autocomplete: "new-password", placeholder: "新密码（至少 12 位，两类字符）" });
  const again = el("input", { type: "password", autocomplete: "new-password", placeholder: "再次输入" });
  const msg = el("div", { class: "msg" });
  const btn = el("button", { type: "submit", text: "保存" });

  const form = el("form", {}, [
    forced ? el("p", { class: "sub", text: "首次登录，请先设置新密码。" }) : null,
    el("label", { text: "当前密码" }), oldPwd,
    el("label", { text: "新密码", style: "margin-top:14px" }), newPwd,
    el("label", { text: "确认新密码", style: "margin-top:14px" }), again,
    btn, msg,
  ]);

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (newPwd.value !== again.value) {
      msg.className = "msg err";
      msg.textContent = "两次输入的新密码不一致";
      return;
    }
    btn.disabled = true;
    try {
      await api("POST", "/api/v1/auth/password", { current: oldPwd.value, new: newPwd.value });
      msg.className = "msg ok";
      msg.textContent = "已修改，正在进入…";
      setTimeout(() => renderApp("overview"), 600);
    } catch (err) {
      msg.className = "msg err";
      msg.textContent = err.message;
      btn.disabled = false;
    }
  });

  app().className = "";
  app().replaceChildren(el("div", { class: "card login-card" }, [
    el("h1", { text: forced ? "设置新密码" : "修改密码" }),
    form,
  ]));
  setTimeout(() => oldPwd.focus(), 50);
}

// ---------- 外壳 ----------

function renderApp(view) {
  state.view = view || state.view || "overview";
  if (state.timer) { clearInterval(state.timer); state.timer = null; }

  const navBtn = (key, label) => el("button", {
    class: "tab" + (state.view === key ? " active" : ""),
    text: label,
    onclick: () => renderApp(key),
  });

  const logout = el("button", {
    class: "ghost", text: "退出",
    onclick: async () => {
      try { await api("POST", "/api/v1/auth/logout", {}); } catch (_) { /* 忽略 */ }
      state.user = null;
      renderLogin("");
    },
  });
  const changePwd = el("button", {
    class: "ghost", text: "改密码",
    onclick: () => renderChangePassword(false),
  });

  const head = el("div", { class: "head" }, [
    el("div", {}, [
      el("h1", { text: "PanelVPS" }),
      el("div", { class: "who", text: "登录账号：" + (state.user ? state.user.username : "-") }),
    ]),
    el("div", { class: "row" }, [changePwd, logout]),
  ]);

  const nav = el("div", { class: "tabs" }, [navBtn("overview", "概览"), navBtn("proxy", "代理")]);
  const content = el("div", { id: "view" });

  app().className = "";
  app().replaceChildren(head, nav, content);

  if (state.view === "proxy") renderProxyView(content);
  else renderOverviewView(content);
}

// ---------- 概览 ----------

function renderOverviewView(root) {
  const banner = el("div", { class: "banner", text: "", style: "display:none" });
  const grid = el("div", { class: "grid" });
  const infoBody = el("tbody");
  const status = el("div", { class: "msg" });

  root.replaceChildren(
    banner,
    grid,
    el("div", { class: "card section" }, [el("h2", { text: "面板信息" }), el("table", {}, [infoBody])]),
    status,
  );

  const refresh = async () => {
    try {
      const data = await api("GET", "/api/v1/system/stat");
      const s = data.metrics;
      grid.replaceChildren(
        statCard("CPU", s.cpu_pct ? s.cpu_pct.toFixed(1) + "%" : "—",
          "负载 " + s.load1.toFixed(2) + " / " + s.load5.toFixed(2) + " / " + s.load15.toFixed(2),
          Math.round(s.cpu_pct)),
        statCard("内存", fmtBytes(s.mem_used), "共 " + fmtBytes(s.mem_total), pct(s.mem_used, s.mem_total)),
        statCard("磁盘", fmtBytes(s.disk_used), "共 " + fmtBytes(s.disk_total), pct(s.disk_used, s.disk_total)),
        statCard("面板占用", fmtBytes(s.panel_rss), s.goroutines + " goroutines"),
      );

      if (!s.supported) {
        banner.style.display = "block";
        banner.textContent = "当前系统不是 Linux，系统指标不可用（仅用于本地开发调试）。";
      }

      const rows = [
        ["面板端口", data.listen],
        ["监听范围", data.loopback ? "仅本机（安全）" : "对外网开放"],
        ["数据目录", data.data_dir],
        ["数据库", data.db_path],
        ["审计条数", String(data.audit_count)],
        ["版本", data.version],
        ["运行时长", Math.floor(s.uptime_sec / 3600) + " 小时 " + Math.floor((s.uptime_sec % 3600) / 60) + " 分"],
      ];
      infoBody.replaceChildren.apply(infoBody, rows.map(([k, v]) =>
        el("tr", {}, [el("th", { text: k }), el("td", { class: "mono", text: v })])));
      status.textContent = "";
    } catch (err) {
      if (err.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      status.className = "msg err";
      status.textContent = err.message;
    }
  };

  refresh();
  state.timer = setInterval(refresh, 3000);
}

// ---------- 代理 ----------

function renderProxyView(root) {
  const banner = el("div", { class: "banner", text: "", style: "display:none" });
  const statusLine = el("div", { class: "row wrap" });
  const hostInput = el("input", { placeholder: "服务器公网 IP 或域名", style: "max-width:280px" });
  const hostMsg = el("div", { class: "msg" });
  const list = el("div", {});
  const err = el("div", { class: "msg err" });

  root.replaceChildren(
    banner,
    el("div", { class: "card" }, [
      el("div", { class: "card-head" }, [
        el("h2", { text: "Xray 状态" }),
        el("div", { class: "row" }, [
          el("button", { class: "ghost", text: "重新下发配置", onclick: () => applyConfig(root) }),
          el("button", { class: "ghost", text: "刷新流量", onclick: () => collectTraffic(root) }),
        ]),
      ]),
      statusLine,
    ]),
    el("div", { class: "card section" }, [
      el("h2", { text: "分享链接地址" }),
      el("p", { class: "sub", text: "客户端连接用的公网地址。默认取当前访问域名，VPS 上建议填真实 IP 或域名。" }),
      el("div", { class: "row wrap" }, [
        hostInput,
        el("button", { class: "ghost", text: "保存", onclick: async () => {
          hostMsg.className = "msg";
          hostMsg.textContent = "保存中…";
          try {
            const r = await api("PUT", "/api/v1/proxies/preferences", { public_host: hostInput.value.trim() });
            hostMsg.className = "msg ok";
            hostMsg.textContent = "已保存：" + r.data.host;
            if (window.__proxyRefresh) window.__proxyRefresh();
          } catch (e) {
            hostMsg.className = "msg err";
            hostMsg.textContent = e.message;
          }
        } }),
      ]),
      hostMsg,
    ]),
    el("div", { class: "section card" }, [
      el("div", { class: "card-head" }, [
        el("h2", { text: "入站" }),
        el("button", { class: "ghost", text: "+ 新建入站", onclick: () => inboundForm(root) }),
      ]),
      list,
    ]),
    err,
  );

  const refresh = async () => {
    try {
      const data = (await api("GET", "/api/v1/proxies")).data;
      state.proxies = data;
      paintStatus(statusLine, data.status, banner);
      if (document.activeElement !== hostInput) hostInput.value = data.host || "";
      paintInbounds(list, data, root);
      err.textContent = "";
    } catch (e) {
      if (e.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      err.textContent = e.message;
    }
  };

  window.__proxyRefresh = refresh;
  refresh();
  state.timer = setInterval(refresh, 5000);
}

function paintStatus(box, st, banner) {
  if (!st.available) {
    banner.style.display = "block";
    banner.textContent = "未安装 Xray：代理功能不可用。安装后重启面板即可（或在配置文件里指定 xray_bin）。";
    box.replaceChildren(badge("不可用", "warn"));
    return;
  }
  banner.style.display = st.running ? "none" : "block";
  if (!st.running) banner.textContent = "Xray 未运行：" + (st.last_error || "点击「重新下发配置」尝试启动");

  const up = st.uptime_sec || 0;
  box.replaceChildren(
    badge(st.running ? "运行中" : "已停止", st.running ? "ok" : "warn"),
    el("span", { class: "mono", text: "PID " + (st.pid || "-") }),
    el("span", { class: "mono", text: "运行 " + Math.floor(up / 3600) + "h" + Math.floor((up % 3600) / 60) + "m" }),
    el("span", { class: "mono", text: st.version ? st.version.split(" ").slice(0, 2).join(" ") : "" }),
    el("span", { class: "mono", text: "API :" + st.api_port }),
  );
}

function paintInbounds(root, data, pageRoot) {
  if (!data.inbounds.length) {
    root.replaceChildren(el("p", { class: "sub", text: "还没有入站。点击右上角「新建入站」开始。" }));
    return;
  }
  root.replaceChildren.apply(root, data.inbounds.map((ib) => inboundCard(ib, data, pageRoot)));
}

function inboundCard(ib, data, pageRoot) {
  const meta = [
    ib.protocol.toUpperCase(),
    ib.network,
    ib.security === "none" ? "无加密" : ib.security.toUpperCase(),
  ].join(" · ");

  const rows = ib.clients.map((c) => {
    const quota = c.total_bytes > 0
      ? fmtBytes(c.up + c.down) + " / " + fmtBytes(c.total_bytes)
      : fmtBytes(c.up + c.down);
    const actions = el("div", { class: "row" }, [
      el("button", { class: "link", text: "链接", onclick: () => showLink(c) }),
      el("button", { class: "link", text: c.enabled ? "禁用" : "启用", onclick: () => toggleClient(c, pageRoot) }),
      el("button", { class: "link danger", text: "删除", onclick: () => removeClient(c, pageRoot) }),
    ]);
    return el("tr", {}, [
      el("td", {}, [
        el("div", { text: c.email }),
        el("div", { class: "mono muted", text: c.secret.slice(0, 18) + (c.secret.length > 18 ? "…" : "") }),
      ]),
      el("td", { class: "mono", text: quota }),
      el("td", {}, [c.active ? badge("正常", "ok") : badge(c.enabled ? "受限" : "已禁用", "warn")]),
      el("td", { class: "right" }, [actions]),
    ]);
  });

  const table = el("table", { class: "clients" }, [
    el("thead", {}, [el("tr", {}, [
      el("th", { text: "用户" }), el("th", { text: "已用流量" }), el("th", { text: "状态" }), el("th", { text: "" }),
    ])]),
    el("tbody", {}, rows.length ? rows : [el("tr", {}, [el("td", { colspan: "4", class: "muted", text: "暂无用户" })])]),
  ]);

  const detail = ib.security === "reality"
    ? "SNI " + (ib.options.server_names || []).join(", ") + " · 目标 " + (ib.options.dest || "-")
    : ib.security === "tls"
      ? "证书 " + (ib.options.cert_file || "-")
      : ib.network === "ws" ? "路径 " + (ib.options.ws_path || "/") : "";

  return el("div", { class: "inbound" }, [
    el("div", { class: "inbound-head" }, [
      el("div", {}, [
        el("div", { class: "row" }, [
          el("strong", { text: ib.remark || ib.tag }),
          badge(meta, "plain"),
          ib.enabled ? null : badge("已停用", "warn"),
        ]),
        el("div", { class: "mono muted", text: ":" + ib.port + "  " + detail + "  流量 ↑" + fmtBytes(ib.up) + " ↓" + fmtBytes(ib.down) }),
      ]),
      el("div", { class: "row" }, [
        el("button", { class: "ghost", text: "+ 用户", onclick: () => clientForm(ib, pageRoot) }),
        el("button", { class: "ghost", text: ib.enabled ? "停用" : "启用", onclick: () => toggleInbound(ib, pageRoot) }),
        el("button", { class: "ghost danger", text: "删除", onclick: () => removeInbound(ib, pageRoot) }),
      ]),
    ]),
    table,
  ]);
}

function showLink(client) {
  const link = client.link || "";
  const box = el("div", {}, [
    el("p", { class: "sub", text: "把链接导入客户端，或用手机扫码。" }),
    el("textarea", { class: "link-box", rows: "4", readonly: true, text: link }),
    el("div", { class: "row", style: "margin-top:12px" }, [
      el("button", { class: "ghost", text: "复制链接", onclick: async () => {
        try { await navigator.clipboard.writeText(link); } catch (_) {
          const ta = document.querySelector(".link-box"); ta.select(); document.execCommand("copy");
        }
      } }),
    ]),
    el("div", { class: "qr" }, [
      el("img", { src: "/api/v1/proxies/clients/" + client.id + "/qr", alt: "二维码", width: "240", height: "240" }),
    ]),
  ]);
  if (!link) {
    box.insertBefore(el("div", { class: "msg err", text: "尚未配置公网地址，无法生成链接。" }), box.firstChild);
  }
  modal(client.email + " 的连接信息", [box]);
}

async function applyConfig(root) {
  try {
    const r = await api("POST", "/api/v1/proxies/apply", {});
    const a = r.data;
    if (a.ok) {
      modal("配置已下发", [el("p", { class: "mono", text: a.message })]);
      if (window.__proxyRefresh) window.__proxyRefresh();
    } else {
      modal("下发失败", [
        el("p", { class: "msg err", text: a.message }),
        el("pre", { class: "log", text: a.output || "" }),
      ]);
    }
  } catch (e) {
    modal("下发失败", [el("p", { class: "msg err", text: e.message })]);
  }
}

async function collectTraffic(root) {
  try {
    await api("POST", "/api/v1/proxies/traffic/collect", {});
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) {
    modal("刷新失败", [el("p", { class: "msg err", text: e.message })]);
  }
}

function field(label, node, hint) {
  return el("div", { class: "field" }, [
    el("label", { text: label }),
    node,
    hint ? el("div", { class: "hint", text: hint }) : null,
  ]);
}

function select(options, value) {
  const s = el("select", {});
  options.forEach((o) => {
    const opt = el("option", { value: o, text: o });
    if (o === value) opt.selected = true;
    s.appendChild(opt);
  });
  return s;
}

function inboundForm(root) {
  const protocol = select(["vless", "vmess", "trojan", "shadowsocks"], "vless");
  const network = select(["tcp", "ws", "grpc"], "tcp");
  const security = select(["none", "tls", "reality"], "reality");
  const listen = el("input", { value: "0.0.0.0" });
  const port = el("input", { value: "443", type: "number", min: "1", max: "65535" });
  const remark = el("input", { placeholder: "例如：东京节点" });
  const dest = el("input", { value: "www.cloudflare.com:443", placeholder: "REALITY 伪装目标 host:port" });
  const sni = el("input", { value: "www.cloudflare.com", placeholder: "serverNames，多个用逗号分隔" });
  const wsPath = el("input", { value: "/", placeholder: "WebSocket 路径" });
  const certFile = el("input", { placeholder: "/etc/ssl/fullchain.pem" });
  const keyFile = el("input", { placeholder: "/etc/ssl/privkey.pem" });
  const ssMethod = select([
    "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
  ], "2022-blake3-aes-128-gcm");
  const msg = el("div", { class: "msg" });

  const realityBox = el("div", {}, [
    field("伪装目标（dest）", dest, "建议用大站 443，且目标站支持 TLS1.3"),
    field("SNI / serverNames", sni, "与目标站域名一致"),
  ]);
  const tlsBox = el("div", {}, [field("证书路径", certFile), field("私钥路径", keyFile)]);
  const wsBox = el("div", {}, [field("WebSocket 路径", wsPath)]);
  const ssBox = el("div", {}, [field("加密方式", ssMethod, "2022 系列支持多用户，密钥自动生成")]);

  const syncVisible = () => {
    realityBox.style.display = security.value === "reality" ? "block" : "none";
    tlsBox.style.display = security.value === "tls" ? "block" : "none";
    wsBox.style.display = network.value === "ws" && protocol.value !== "shadowsocks" ? "block" : "none";
    ssBox.style.display = protocol.value === "shadowsocks" ? "block" : "none";
    // SS 只能是 tcp + none
    if (protocol.value === "shadowsocks") {
      network.value = "tcp";
      security.value = "none";
      network.disabled = true;
      security.disabled = true;
    } else {
      network.disabled = false;
      security.disabled = false;
    }
    if (protocol.value !== "vless" && security.value === "reality") security.value = "none";
  };
  [protocol, network, security].forEach((s) => s.addEventListener("change", syncVisible));
  syncVisible();

  const submit = el("button", { text: "创建并入站", onclick: async () => {
    submit.disabled = true;
    msg.className = "msg";
    msg.textContent = "创建中…";
    const options = {};
    if (security.value === "reality") {
      options.dest = dest.value.trim();
      options.server_names = sni.value.split(",").map((s) => s.trim()).filter(Boolean);
    }
    if (security.value === "tls") {
      options.cert_file = certFile.value.trim();
      options.key_file = keyFile.value.trim();
    }
    if (network.value === "ws") options.ws_path = wsPath.value.trim() || "/";
    if (protocol.value === "shadowsocks") options.method = ssMethod.value;

    try {
      const r = await api("POST", "/api/v1/proxies/inbounds", {
        remark: remark.value.trim(),
        protocol: protocol.value,
        listen: listen.value.trim(),
        port: parseInt(port.value, 10),
        network: network.value,
        security: security.value,
        options,
      });
      const a = r.data.apply;
      if (a && !a.ok) {
        msg.className = "msg err";
        msg.textContent = "已保存但下发失败：" + a.message;
        submit.disabled = false;
        return;
      }
      closeModal();
      if (window.__proxyRefresh) window.__proxyRefresh();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
      submit.disabled = false;
    }
  } });

  const m = modal("新建入站", [
    field("备注", remark),
    field("协议", protocol),
    el("div", { class: "row" }, [field("监听地址", listen), field("端口", port)]),
    field("传输方式", network),
    wsBox,
    field("安全类型", security),
    realityBox,
    tlsBox,
    ssBox,
    submit,
    msg,
  ]);
  function closeModal() { m.close(); }
}

function clientForm(ib, root) {
  const email = el("input", { placeholder: "用户标识，例如 alice" });
  const quota = el("input", { type: "number", min: "0", placeholder: "0 表示不限" });
  const expiry = el("input", { type: "date" });
  const msg = el("div", { class: "msg" });

  const submit = el("button", { text: "创建用户", onclick: async () => {
    submit.disabled = true;
    msg.className = "msg";
    msg.textContent = "创建中…";
    const body = { email: email.value.trim(), total_bytes: 0 };
    const gb = parseFloat(quota.value);
    if (!isNaN(gb) && gb > 0) body.total_bytes = Math.round(gb * 1024 * 1024 * 1024);
    if (expiry.value) body.expires_at = new Date(expiry.value + "T23:59:59").toISOString();
    try {
      const r = await api("POST", "/api/v1/proxies/inbounds/" + ib.id + "/clients", body);
      const a = r.data.apply;
      if (a && !a.ok) {
        msg.className = "msg err";
        msg.textContent = "已创建但下发失败：" + a.message;
        submit.disabled = false;
        return;
      }
      closeModal();
      if (window.__proxyRefresh) window.__proxyRefresh();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
      submit.disabled = false;
    }
  } });

  const m = modal("为 " + (ib.remark || ib.tag) + " 添加用户", [
    field("用户标识", email, "全局唯一；Xray 用它区分流量归属"),
    field("流量配额（GB）", quota),
    field("到期日期", expiry, "留空表示永久"),
    submit,
    msg,
  ]);
  function closeModal() { m.close(); }
}

async function toggleClient(c, root) {
  try {
    await api("PATCH", "/api/v1/proxies/clients/" + c.id, { email: c.email, enabled: !c.enabled, total_bytes: c.total_bytes });
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function removeClient(c, root) {
  if (!confirm("确定删除用户 " + c.email + "？该操作会立刻断开其连接。")) return;
  try {
    await api("DELETE", "/api/v1/proxies/clients/" + c.id, {});
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function toggleInbound(ib, root) {
  try {
    await api("PATCH", "/api/v1/proxies/inbounds/" + ib.id, { enabled: !ib.enabled });
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function removeInbound(ib, root) {
  if (!confirm("确定删除入站 " + (ib.remark || ib.tag) + "？其下所有用户会一并删除。")) return;
  try {
    await api("DELETE", "/api/v1/proxies/inbounds/" + ib.id, {});
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

// ---------- 启动 ----------

async function boot() {
  try {
    const data = await api("GET", "/api/v1/auth/state");
    state.csrf = data.csrf;
    state.initialized = data.initialized;

    if (!data.authenticated) {
      renderLogin(data.initialized ? "" : "尚未初始化账号，请查看安装日志中的初始密码");
      return;
    }
    state.user = data.user;
    if (data.user.must_change_password) {
      renderChangePassword(true);
      return;
    }
    renderApp("overview");
  } catch (err) {
    app().className = "";
    app().replaceChildren(el("div", { class: "card login-card" }, [
      el("h1", { text: "无法连接面板" }),
      el("p", { class: "sub", text: err.message }),
    ]));
  }
}

boot();
