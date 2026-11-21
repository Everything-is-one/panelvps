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

// 本文件被加载时的版本号（由服务端在 index.html 里注入 ?v=）。
// 与接口返回的面板版本不一致，说明浏览器拿的是旧脚本。
const FE_VERSION = (() => {
  try {
    const src = document.currentScript && document.currentScript.src;
    return src ? new URL(src, location.href).searchParams.get("v") || "" : "";
  } catch (e) { return ""; }
})();

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
  // 统一拆信封：调用方直接拿 data，避免到处写 res.data.xxx
  if (data && data.code === 0 && "data" in data) return data.data;
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

function statCard(key, value, desc, percent, topKind) {
  const kids = [el("div", { class: "k", text: key }), el("div", { class: "v", text: value })];
  if (percent !== undefined) kids.push(bar(percent));
  if (desc) kids.push(el("div", { class: "d", text: desc }));
  if (topKind) {
    // 给卡片一个「前 5」角标，否则用户不知道这里可以 hover
    kids[0].appendChild(el("span", { class: "stat-more", text: "前 5" }));
    const box = el("div", { class: "stat" }, kids);
    bindTopTip(box, topKind, key);
    return box;
  }
  return el("div", { class: "stat" }, kids);
}

// ---------- 资源排行浮层 ----------
//
// 概览的 CPU / 内存 / 磁盘卡片在鼠标移上去时展示「占用靠前的几名」。
// 用原生 title 做不到表格，所以自己画一个 fixed 定位的浮层：
// fixed 而不是 absolute，卡片所在容器有 overflow 时会把它裁掉。
const TOP_LABEL = { cpu: "CPU 占用", mem: "内存占用", disk: "磁盘占用" };
const topCache = new Map();
let topTipEl = null;
let topShowTimer = null;
let topHideTimer = null;
let topSeq = 0;

function topTip() {
  if (topTipEl) return topTipEl;
  topTipEl = el("div", { class: "top-tip", role: "tooltip" });
  topTipEl.style.display = "none";
  // 浮层本身可 hover：从卡片移进浮层时不能消失，否则鼠标一动就没了
  topTipEl.addEventListener("mouseenter", () => clearTimeout(topHideTimer));
  topTipEl.addEventListener("mouseleave", () => scheduleHideTip());
  document.body.appendChild(topTipEl);
  window.addEventListener("scroll", () => hideTopTip(), true);
  window.addEventListener("resize", () => hideTopTip());
  return topTipEl;
}

function hideTopTip() {
  clearTimeout(topShowTimer);
  topSeq++;
  if (topTipEl) topTipEl.style.display = "none";
}

function scheduleHideTip() {
  clearTimeout(topHideTimer);
  topHideTimer = setTimeout(hideTopTip, 180);
}

async function fetchTop(kind) {
  const hit = topCache.get(kind);
  if (hit && Date.now() - hit.at < 5000) return hit.data;
  const data = await api("GET", "/api/v1/system/top?kind=" + kind + "&limit=5");
  topCache.set(kind, { at: Date.now(), data: data });
  return data;
}

function topTipRow(name, value, ratio, sub) {
  const fill = el("i");
  fill.style.width = Math.max(2, Math.min(100, ratio)) + "%";
  return el("div", { class: "top-row" }, [
    el("div", { class: "top-row-main" }, [
      el("span", { class: "top-name", text: name, title: name }),
      el("span", { class: "top-val", text: value }),
    ]),
    el("div", { class: "top-track" }, [fill]),
    sub ? el("div", { class: "top-sub", text: sub }) : null,
  ]);
}

function topTipBody(kind, data) {
  const items = data.items || [];
  if (!data.supported || !items.length) {
    return [el("div", { class: "top-empty", text: data.reason || "暂无可展示的数据" })];
  }
  let max = 1;
  for (const it of items) {
    const v = kind === "cpu" ? it.cpu_pct : kind === "disk" ? it.pct : it.rss;
    if (v > max) max = v;
  }
  const rows = items.map((it) => {
    if (kind === "disk") {
      return topTipRow(it.path, pct(it.used, it.total) + "%", (it.pct / max) * 100,
        fmtBytes(it.used) + " / " + fmtBytes(it.total) + " · " + it.fstype);
    }
    if (kind === "mem") {
      return topTipRow(it.name, fmtBytes(it.rss), (it.rss / max) * 100,
        "PID " + it.pid + " · 占内存 " + (it.mem_pct || 0).toFixed(1) + "%");
    }
    return topTipRow(it.name, (it.cpu_pct || 0).toFixed(1) + "%", (it.cpu_pct / max) * 100,
      "PID " + it.pid + " · 内存 " + fmtBytes(it.rss));
  });
  return rows;
}

async function showTopTip(card, kind, label) {
  const seq = ++topSeq;
  const tip = topTip();
  const rect = card.getBoundingClientRect();
  tip.replaceChildren(
    el("div", { class: "top-head" }, [
      el("span", { class: "top-title", text: (TOP_LABEL[kind] || label) + "前 5" }),
      el("span", { class: "top-stamp", text: "近 5 秒" }),
    ]),
    el("div", { class: "top-body" }, [el("div", { class: "top-empty", text: "采样中…" })]),
  );
  tip.style.display = "block";
  tip.style.visibility = "hidden";
  tipPosition(tip, rect);
  tip.style.visibility = "";

  try {
    const data = await fetchTop(kind);
    if (seq !== topSeq) return;
    tip.querySelector(".top-body").replaceChildren.apply(tip.querySelector(".top-body"), topTipBody(kind, data));
    tipPosition(tip, card.getBoundingClientRect());
  } catch (e) {
    if (seq !== topSeq) return;
    tip.querySelector(".top-body").replaceChildren(el("div", { class: "top-empty", text: e.message }));
  }
}

// 优先放在卡片下方；下方放不下就翻到上方，左右夹在视口里。
function tipPosition(tip, rect) {
  const pad = 12;
  const w = tip.offsetWidth || 300;
  const h = tip.offsetHeight || 120;
  let left = rect.left;
  if (left + w > window.innerWidth - pad) left = window.innerWidth - w - pad;
  if (left < pad) left = pad;
  let top = rect.bottom + 8;
  if (top + h > window.innerHeight - pad) top = rect.top - h - 8;
  if (top < pad) top = pad;
  tip.style.left = Math.round(left) + "px";
  tip.style.top = Math.round(top) + "px";
}

function bindTopTip(card, kind, label) {
  card.classList.add("stat-has-top");
  card.tabIndex = 0;
  card.setAttribute("aria-label", label + "：悬停或聚焦查看占用排行");
  const show = () => {
    clearTimeout(topShowTimer);
    clearTimeout(topHideTimer);
    topShowTimer = setTimeout(() => showTopTip(card, kind, label), 120);
  };
  card.addEventListener("mouseenter", show);
  card.addEventListener("mouseleave", () => { clearTimeout(topShowTimer); scheduleHideTip(); });
  card.addEventListener("focus", show);
  card.addEventListener("blur", () => scheduleHideTip());
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
    field("用户名", username),
    field("密码", password),
    el("div", { class: "login-actions" }, [btn]),
    msg,
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

  app().className = "login-wrap";
  app().replaceChildren(el("div", { class: "card login-card" }, [
    el("div", { class: "login-brand" }, [
      el("span", { class: "brand-mark", text: "PV" }),
      el("div", { class: "brand-text" }, [
        el("strong", { text: "PanelVPS" }),
        el("span", { text: "服务器面板" }),
      ]),
    ]),
    el("h1", { text: "登录" }),
    el("p", { class: "sub", text: "输入账号密码继续" }),
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
    field("当前密码", oldPwd),
    field("新密码", newPwd, "至少 12 位，且包含两类字符"),
    field("确认新密码", again),
    el("div", { class: "login-actions" }, [btn]),
    msg,
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

  app().className = "login-wrap";
  app().replaceChildren(el("div", { class: "card login-card" }, [
    el("h1", { text: forced ? "设置新密码" : "修改密码" }),
    form,
  ]));
  setTimeout(() => oldPwd.focus(), 50);
}

// ---------- 外壳 ----------

// 导航图标：单路径线性图标，统一 24 视窗、1.7 描边。
// 关键约束：所有图形的墨迹范围都控制在 x/y 的 4–20 之间（即 16×16 的
// 中心方框），否则同一列图标的视觉宽度会差 1–2px，排起来就"不齐"。
// 新增条目照这个尺子画，别再引入 3..21 那种 18 宽的图标。
const NAV_ICONS = {
  overview: "M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z",
  proxy: "M4 8h16M8 4 4 8l4 4M20 16H4M16 12l4 4-4 4",
  relay: "M4 4h5v5H4zM15 15h5v5h-5zM9 6.5h6a2 2 0 0 1 2 2V15",
  sites: "M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16M4 12h16M12 4c2.1 2.2 3.2 4.9 3.2 8S14.1 17.8 12 20c-2.1-2.2-3.2-4.9-3.2-8S9.9 6.2 12 4",
  users: "M9 11.5a3.4 3.4 0 1 0 0-6.8 3.4 3.4 0 0 0 0 6.8M3.8 19.2v-1.1a4.9 4.9 0 0 1 4.9-4.9h1.1a4.9 4.9 0 0 1 4.9 4.9v1.1M15.8 4.9a3.3 3.3 0 0 1 0 6.4M16.4 13.3a4.5 4.5 0 0 1 3.8 4.5v1.4",
  files: "M4 6.5A1.5 1.5 0 0 1 5.5 5h3.8l2 2h6.2A2.5 2.5 0 0 1 20 9.5V17a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z",
  terminal: "M4 5.5A1.5 1.5 0 0 1 5.5 4h13A1.5 1.5 0 0 1 20 5.5v13A1.5 1.5 0 0 1 18.5 20h-13A1.5 1.5 0 0 1 4 18.5zM8 10l3 2.5L8 15M13 15h4",
  firewall: "M12 4l7.8 2.9v5.2c0 4.6-3.2 7.7-7.8 8.8-4.6-1.1-7.8-4.2-7.8-8.8V6.9z",
  logs: "M4 6h1.6M8 6h12M4 12h1.6M8 12h12M4 18h1.6M8 18h6",
  store: "M4.5 8h15l-1 12h-13zM8.5 8V6.5a3.5 3.5 0 0 1 7 0V8",
  backup: "M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3m0 0v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3",
  settings: "M4 7h4M12 7h8M4 17h8M16 17h4M10 5a2 2 0 1 0 0 4 2 2 0 0 0 0-4M14 15a2 2 0 1 0 0 4 2 2 0 0 0 0-4",
};

// 导航分组：条目多了以后平铺成一长列很难扫，按「网络 / 系统」分两段，
// 段标题用小号灰字，视觉上形成节奏。
const NAV_GROUPS = [
  ["", [["overview", "概览"]]],
  ["网络", [["proxy", "代理"], ["users", "用户"], ["relay", "中转"], ["sites", "网站"], ["firewall", "防火墙"]]],
  ["系统", [["files", "文件"], ["terminal", "终端"], ["logs", "日志"],
            ["store", "商店"], ["backup", "备份"], ["settings", "设置"]]],
];

const VIEW_META = {
  overview: ["概览", "资源占用、系统环境与常用入口"],
  proxy: ["代理", "Xray 入站、用户与订阅"],
  users: ["用户", "所有入站下的账号：流量、到期与状态"],
  relay: ["中转", "端口转发：把本机流量透传到目标地址"],
  sites: ["网站", "Nginx 站点与证书"],
  files: ["文件", "浏览、编辑服务器文件"],
  terminal: ["终端", "以 root 身份执行命令"],
  firewall: ["防火墙", "规则、封禁与 SSH 保护"],
  logs: ["日志", "面板运行日志与系统日志"],
  store: ["商店", "一键安装常用服务"],
  backup: ["备份", "数据库、配置与站点打包"],
  settings: ["设置", "访问方式、账号与运行参数"],
};

function icon(name, size) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", size || 18);
  svg.setAttribute("height", size || 18);
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.7");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", NAV_ICONS[name] || NAV_ICONS.overview);
  svg.appendChild(p);
  return svg;
}

function renderApp(view) {
  state.view = view || state.view || "overview";
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  // 上一个视图注册的清理钩子（终端要断开 socket、释放 xterm 实例）
  if (typeof state.cleanup === "function") {
    try { state.cleanup(); } catch (_) { /* 清理失败不影响切换 */ }
    state.cleanup = null;
  }

  const logout = async () => {
    try { await api("POST", "/api/v1/auth/logout", {}); } catch (_) { /* 忽略 */ }
    state.user = null;
    renderLogin("");
  };

  const navButton = ([key, label]) => el("button", {
    class: "nav-item" + (state.view === key ? " active" : ""),
    onclick: () => renderApp(key),
    title: label,
  }, [icon(key), el("span", { text: label })]);
  const navItems = NAV_GROUPS.map(([label, items]) => el("div", { class: "nav-group" },
    (label ? [el("div", { class: "nav-label", text: label })] : []).concat(items.map(navButton))));

  const username = state.user ? state.user.username : "admin";
  const sidebar = el("aside", { class: "sidebar" }, [
    el("div", { class: "brand" }, [
      el("span", { class: "brand-mark", text: "PV" }),
      el("div", { class: "brand-text" }, [
        el("strong", { text: "PanelVPS" }),
        el("span", { text: "服务器面板" }),
      ]),
    ]),
    el("nav", { class: "nav" }, navItems),
    el("div", { class: "sidebar-foot" }, [
      el("div", { class: "user-chip" }, [
        el("span", { class: "avatar", text: username.slice(0, 1).toUpperCase() }),
        el("div", { class: "user-meta" }, [
          el("strong", { text: username }),
          el("span", { text: "已登录" }),
        ]),
      ]),
      el("button", { class: "ghost block", text: "退出登录", onclick: logout }),
    ]),
  ]);

  const meta = VIEW_META[state.view] || VIEW_META.overview;
  const topbar = el("header", { class: "topbar" }, [
    el("div", {}, [
      el("h1", { text: meta[0] }),
      el("p", { class: "sub", text: meta[1] }),
    ]),
    el("div", { class: "row" }, [
      el("button", { class: "ghost", text: "改密码", onclick: () => renderChangePassword(false) }),
    ]),
  ]);

  const content = el("div", { id: "view", class: "view" });
  app().className = "shell";
  app().replaceChildren(sidebar, el("main", { class: "main" }, [
    el("div", { class: "main-inner" }, [topbar, content]),
  ]));

  switch (state.view) {
    case "proxy": renderProxyView(content); break;
    case "users": renderUsersView(content); break;
    case "relay": renderRelayView(content); break;
    case "sites": renderSitesView(content); break;
    case "files": renderFilesView(content); break;
    case "terminal": renderTerminalView(content); break;
    case "firewall": renderFirewallView(content); break;
    case "logs": renderLogsView(content); break;
    case "store": renderStoreView(content); break;
    case "backup": renderBackupView(content); break;
    case "settings": renderSettingsView(content); break;
    default: renderOverviewView(content);
  }
}

// ---------- 概览 ----------

function renderOverviewView(root) {
  const banner = el("div", { class: "banner", text: "", style: "display:none" });
  const grid = el("div", { class: "grid stat-grid" });
  const infoBody = el("tbody");
  const sysBody = el("tbody");
  const status = el("div", { class: "msg" });

  root.replaceChildren(
    banner,
    grid,
    // 面板信息与系统环境并排：两块都是键值表，条目数接近，
    // 单列铺开会在 1440 以上的屏幕上留出一大片空白。
    el("div", { class: "grid g2 overview-meta" }, [
      el("div", { class: "card section" }, [el("h2", { text: "面板信息" }), el("table", {}, [infoBody])]),
      el("div", { class: "card section" }, [el("h2", { text: "系统环境" }), el("table", {}, [sysBody])]),
    ]),
    status,
  );

  // 主机信息基本不变，只取一次；资源指标才需要轮询
  const loadHost = async () => {
    try {
      const d = await api("GET", "/api/v1/system/info");
      const o = d.os || {};
      const rows = [
        ["主机名", o.hostname],
        ["发行版", o.distro],
        ["内核", o.kernel],
        ["架构", o.arch],
        ["CPU 核数", o.cpu_cores ? String(o.cpu_cores) : "-"],
        ["时区", d.timezone || "-"],
        ["运行环境", d.cgroup ? "cgroup " + d.cgroup : "-"],
      ];
      sysBody.replaceChildren.apply(sysBody, rows.map(([k, v]) =>
        el("tr", {}, [el("th", { text: k }), el("td", { class: "mono", text: String(v || "-") })])));
    } catch (e) {
      if (e.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      sysBody.replaceChildren(el("tr", {}, [el("td", { text: e.message })]));
    }
  };

  const refresh = async () => {
    try {
      const data = await api("GET", "/api/v1/system/stat");
      const s = data.metrics;
      grid.replaceChildren(
        statCard("CPU", s.cpu_pct ? s.cpu_pct.toFixed(1) + "%" : "—",
          (s.container ? "宿主负载 " : "负载 ") + s.load1.toFixed(2) + " / " + s.load5.toFixed(2) + " / " + s.load15.toFixed(2),
          Math.round(s.cpu_pct), "cpu"),
        statCard("内存", fmtBytes(s.mem_used), (s.container ? "cgroup 限额 " : "共 ") + fmtBytes(s.mem_total), pct(s.mem_used, s.mem_total), "mem"),
        statCard("磁盘", fmtBytes(s.disk_used), "共 " + fmtBytes(s.disk_total), pct(s.disk_used, s.disk_total), "disk"),
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
        ["采集口径", s.container ? "cgroup 限额（容器内准确）" : "宿主机 /proc"],
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

  loadHost();
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
      el("p", { class: "sub", text: "客户端连接用的公网地址。默认取当前访问域名，VPS 上建议填真实 IP 或域名。NAT / 端口映射的机器可以连端口一起填，例如 1.2.3.4:30986。" }),
      el("div", { class: "row wrap" }, [
        hostInput,
        el("button", { class: "ghost", text: "保存", onclick: async () => {
          hostMsg.className = "msg";
          hostMsg.textContent = "保存中…";
          try {
            const r = await api("PUT", "/api/v1/proxies/preferences", { public_host: hostInput.value.trim() });
            hostMsg.className = "msg ok";
            hostMsg.textContent = "已保存：" + r.host;
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
        el("button", { class: "ghost", text: "+ 新建入站", onclick: () => inboundForm(null) }),
      ]),
      list,
    ]),
    err,
  );

  const refresh = async () => {
    try {
      const data = await api("GET", "/api/v1/proxies");
      state.proxies = data;
      paintStatus(statusLine, data.status, banner);
      if (document.activeElement !== hostInput) hostInput.value = data.host || "";
      paintInbounds(list, data);
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

function paintInbounds(root, data) {
  if (!data.inbounds.length) {
    root.replaceChildren(el("p", { class: "sub", text: "还没有入站。点击右上角「新建入站」开始。" }));
    return;
  }
  root.replaceChildren.apply(root, data.inbounds.map((ib) => inboundCard(ib, data)));
}

// ---------- 用户相关的展示小件 ----------

function fmtDate(d) {
  if (!(d instanceof Date) || isNaN(d.getTime())) return "—";
  const p = (n) => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
}

// 到期日不能只显示一个 - 号：用户最常问的就是「还剩几天」。
// 这里同时给日期和剩余天数，过期用红色划掉，快到期用琥珀色。
function expiryCell(ts, tag) {
  const td = el("td", { class: "expiry" });
  if (!ts) {
    td.appendChild(el("span", { class: "when", text: "永久" }));
    return td;
  }
  const d = new Date(ts);
  if (isNaN(d.getTime())) {
    td.appendChild(el("span", { class: "when", text: "—" }));
    return td;
  }
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000);
  const left = days < 0 ? "已过期 " + (-days) + " 天" : days === 0 ? "今天到期" : "剩 " + days + " 天";
  if (days < 0) td.className = "expiry expired";
  else if (days <= 7) td.className = "expiry soon";
  td.appendChild(el("span", { class: "when", text: fmtDate(d) }));
  td.appendChild(el("span", { class: "left", text: left }));
  if (tag) td.title = tag;
  return td;
}

// active 已经把「停用 / 过期 / 超额」三种情况都算进去了，但用户需要知道
// 具体是哪一种，否则一排「受限」等于没说。
function statusBadge(c) {
  if (!c.enabled) return badge("已停用", "warn");
  if (c.expires_at && new Date(c.expires_at).getTime() < Date.now()) return badge("已过期", "err");
  if (c.total_bytes > 0 && c.up + c.down >= c.total_bytes) return badge("已超额", "err");
  return c.active ? badge("正常", "ok") : badge("受限", "warn");
}

function usedCell(c) {
  const used = fmtBytes(c.up + c.down);
  return el("td", { class: "mono", text: c.total_bytes > 0 ? used + " / " + fmtBytes(c.total_bytes) : used });
}

function inboundCard(ib, data) {
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
      el("button", { class: "link", text: "编辑", onclick: () => clientForm(ib, c) }),
      el("button", { class: "link", text: c.enabled ? "禁用" : "启用", onclick: () => toggleClient(c) }),
      el("button", { class: "link danger", text: "删除", onclick: () => removeClient(c) }),
    ]);
    return el("tr", {}, [
      el("td", {}, [
        el("div", { text: c.email }),
        el("div", { class: "mono muted", text: c.secret.slice(0, 18) + (c.secret.length > 18 ? "…" : "") }),
      ]),
      el("td", { class: "mono", text: quota }),
      expiryCell(c.expires_at),
      el("td", {}, [statusBadge(c)]),
      el("td", { class: "right" }, [actions]),
    ]);
  });

  const table = el("table", { class: "clients" }, [
    el("thead", {}, [el("tr", {}, [
      el("th", { text: "用户" }), el("th", { text: "已用流量" }), el("th", { text: "到期" }),
      el("th", { text: "状态" }), el("th", { text: "" }),
    ])]),
    el("tbody", {}, rows.length ? rows : [el("tr", {}, [el("td", { colspan: "5", class: "muted", text: "暂无用户" })])]),
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
        el("div", {
          class: "mono muted",
          text: (() => {
            // 入站自己的对外端口优先，其次是设置里的全局端口
            const pub = ib.public_port > 0 ? ib.public_port : (data.public_port || 0);
            return (pub > 0 ? ":" + ib.port + " → 公网 " + pub : ":" + ib.port)
              + "  " + detail + "  流量 ↑" + fmtBytes(ib.up) + " ↓" + fmtBytes(ib.down);
          })(),
        }),
      ]),
      el("div", { class: "row actions-inline" }, [
        el("button", { class: "ghost", text: "编辑", onclick: () => inboundForm(ib) }),
        el("button", { class: "ghost", text: "复制链接", onclick: (ev) => copyInboundLink(ib, ev.target) }),
        el("button", { class: "ghost", text: "+ 用户", onclick: () => clientForm(ib, null) }),
        el("button", { class: "ghost", text: ib.enabled ? "停用" : "启用", onclick: () => toggleInbound(ib) }),
        el("div", { class: "tool-divider" }),
        el("button", { class: "ghost danger", text: "删除", onclick: () => removeInbound(ib) }),
      ]),
    ]),
    table,
  ]);
}

// 复制文本。面板一般是 http://IP:端口 打开的，navigator.clipboard 在
// 非安全上下文里根本不存在，所以老办法 execCommand 才是主路径。
async function copyText(text) {
  if (!text) return false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) { /* 落到下面 */ }
  const ta = el("textarea", { text: text });
  ta.style.position = "fixed";
  ta.style.top = "-1000px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

function copyWithFeedback(btn, text, doneText) {
  copyText(text).then((ok) => {
    const old = btn.textContent;
    btn.textContent = ok ? (doneText || "已复制") : "复制失败";
    setTimeout(() => { btn.textContent = old; }, 2000);
  });
}

async function copyInboundLink(ib, btn) {
  const list = ib.clients || [];
  const c = list.find((x) => x.enabled) || list[0];
  if (!c) {
    alert("这个入站还没有用户，先点「+ 用户」创建一个。");
    return;
  }
  if (!c.link) {
    alert("还没填服务器公网地址，无法生成链接。请在上方「公网地址」里填 IP 或域名后保存；NAT 机器要写 IP:对外端口。");
    return;
  }
  copyWithFeedback(btn, c.link, "已复制连接");
}

function showLink(client) {
  const link = client.link || "";
  const box = el("div", {}, [
    el("p", { class: "sub", text: "把链接导入客户端，或用手机扫码。" }),
    el("textarea", { class: "link-box", rows: "4", readonly: true, text: link }),
    el("div", { class: "row", style: "margin-top:12px" }, [
      el("button", { class: "ghost", text: "复制链接", onclick: (ev) => copyWithFeedback(ev.target, link, "已复制") }),
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
    const a = r;
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
    const val = Array.isArray(o) ? o[0] : o;
    const label = Array.isArray(o) ? o[1] : o;
    const opt = el("option", { value: val, text: label });
    if (val === value) opt.selected = true;
    s.appendChild(opt);
  });
  return s;
}

// 端口随机：20xxx–59xxx，避开常见服务与面板自身端口段，撞了后端会提示
function randomPort() {
  return 20000 + Math.floor(Math.random() * 40000);
}

function inboundForm(existing) {
  const o = (existing && existing.options) || {};
  const protocol = select(["vless", "vmess", "trojan", "shadowsocks"], existing ? existing.protocol : "vless");
  const network = select(["tcp", "ws", "grpc"], existing ? existing.network : "tcp");
  const security = select(["none", "tls", "reality"], existing ? existing.security : "reality");
  const listen = el("input", { value: existing ? (existing.listen || "0.0.0.0") : "0.0.0.0" });
  const port = el("input", {
    type: "number", min: "1", max: "65535", style: "flex:1;min-width:0",
    value: String(existing ? existing.port : randomPort()),
  });
  const portRoll = el("button", {
    class: "ghost", text: "随机", style: "flex:0 0 auto",
    onclick: () => { port.value = String(randomPort()); },
  });
  const publicPort = el("input", {
    type: "number", min: "1", max: "65535",
    value: existing && existing.public_port ? String(existing.public_port) : "",
    placeholder: "留空 = 与监听端口相同",
  });
  const remark = el("input", { placeholder: "例如：东京节点", value: existing ? (existing.remark || "") : "" });
  const dest = el("input", { value: o.dest || "www.cloudflare.com:443", placeholder: "REALITY 伪装目标 host:port" });
  const sni = el("input", {
    value: (o.server_names && o.server_names.length ? o.server_names.join(", ") : "www.cloudflare.com"),
    placeholder: "serverNames，多个用逗号分隔",
  });
  const wsPath = el("input", { value: o.ws_path || "/", placeholder: "WebSocket 路径" });
  const certFile = el("input", { value: o.cert_file || "", placeholder: "/etc/ssl/fullchain.pem" });
  const keyFile = el("input", { value: o.key_file || "", placeholder: "/etc/ssl/privkey.pem" });
  const ssMethod = select([
    "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
  ], o.method || "2022-blake3-aes-128-gcm");
  const msg = el("div", { class: "msg" });

  const realityBox = el("div", {}, [
    field("伪装目标（dest）", dest, "建议用大站 443，且目标站支持 TLS1.3；只填域名会自动补 :443"),
    field("SNI / serverNames", sni, "必须与目标站同域，客户端靠它伪装"),
    el("div", { class: "hint", text: "私钥、公钥、shortId 都由服务端自动生成，不用填，公钥会自动写进分享链接。流控 xtls-rprx-vision 只在 TCP 上生效，创建用户时会自动带上；gRPC 不支持流控，留空即可。" }),
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

  const submit = el("button", { text: existing ? "保存" : "创建入站", onclick: async () => {
    submit.disabled = true;
    msg.className = "msg";
    msg.textContent = existing ? "保存中…" : "创建中…";
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
      const body = {
        remark: remark.value.trim(),
        protocol: protocol.value,
        listen: listen.value.trim(),
        port: parseInt(port.value, 10),
        public_port: parseInt(publicPort.value, 10) || 0,
        network: network.value,
        security: security.value,
        options,
      };
      const r = existing
        ? await api("PATCH", "/api/v1/proxies/inbounds/" + existing.id, body)
        : await api("POST", "/api/v1/proxies/inbounds", body);
      const a = r.apply;
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

  const m = modal(existing ? "编辑入站" : "新建入站", [
    field("备注", remark, "只影响显示与分享链接里的备注名"),
    field("协议", protocol),
    el("div", { class: "row" }, [
      field("监听地址", listen, "0.0.0.0 = IPv4 + IPv6 同时监听"),
      field("端口", el("div", { class: "row", style: "gap:6px" }, [port, portRoll]), "默认随机，被占用会提示换一个"),
    ]),
    field("对外端口", publicPort,
      "只有 NAT / 端口映射的机器才要填：商家把公网 30986 转发到容器内 47736 时填 30986。独立 IP 或走 IPv6 时留空——填了会把错误端口写进分享链接。"),
    field("传输方式", network, "REALITY 选 tcp 或 grpc；ws 一般配合 CDN"),
    wsBox,
    field("安全类型", security, "reality 不需要证书，也最省事"),
    realityBox,
    tlsBox,
    ssBox,
    submit,
    msg,
  ]);
  function closeModal() { m.close(); }
}

function clientForm(ib, existing) {
  const isEdit = !!existing;
  const email = el("input", {
    value: isEdit ? existing.email : "",
    placeholder: "用户标识，例如 alice",
  });
  const quota = el("input", {
    type: "number", min: "0", step: "0.1",
    value: isEdit && existing.total_bytes > 0
      ? String(Math.round(existing.total_bytes / 1073741824 * 100) / 100)
      : "",
    placeholder: "0 表示不限",
  });
  const expiry = el("input", { type: "date" });
  const forever = el("input", { type: "checkbox" });
  const enabled = el("input", { type: "checkbox" });

  if (isEdit && existing.expires_at) {
    const d = new Date(existing.expires_at);
    if (!isNaN(d.getTime())) expiry.value = fmtDate(d);
  }
  forever.checked = !(isEdit && existing.expires_at);
  enabled.checked = isEdit ? !!existing.enabled : true;

  const syncForever = () => {
    expiry.disabled = forever.checked;
    if (forever.checked) expiry.value = "";
  };
  forever.addEventListener("change", syncForever);
  syncForever();

  const msg = el("div", { class: "msg" });

  const submit = el("button", { text: isEdit ? "保存" : "创建用户", onclick: async () => {
    if (!forever.checked && !expiry.value) {
      msg.className = "msg err";
      msg.textContent = "请选择到期日期，或勾选「永久有效」";
      return;
    }
    submit.disabled = true;
    msg.className = "msg";
    msg.textContent = isEdit ? "保存中…" : "创建中…";

    const body = {
      email: email.value.trim(),
      total_bytes: 0,
      enabled: enabled.checked,
    };
    const gb = parseFloat(quota.value);
    if (!isNaN(gb) && gb > 0) body.total_bytes = Math.round(gb * 1024 * 1024 * 1024);
    if (!forever.checked && expiry.value) {
      body.expires_at = new Date(expiry.value + "T23:59:59").toISOString();
    } else if (isEdit) {
      body.clear_expiry = true;
    }

    try {
      const url = isEdit
        ? "/api/v1/proxies/clients/" + existing.id
        : "/api/v1/proxies/inbounds/" + ib.id + "/clients";
      const r = await api(isEdit ? "PATCH" : "POST", url, body);
      const a = r && r.apply;
      if (a && !a.ok) {
        msg.className = "msg err";
        msg.textContent = "已保存但下发失败：" + a.message;
        submit.disabled = false;
        return;
      }
      m.close();
      if (window.__proxyRefresh) window.__proxyRefresh();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
      submit.disabled = false;
    }
  } });

  const resetTraffic = isEdit ? el("button", { class: "ghost", text: "重置已用流量", onclick: async () => {
    if (!confirm("把 " + existing.email + " 的上下行流量清零？配额与到期时间不变。")) return;
    try {
      await api("POST", "/api/v1/proxies/clients/" + existing.id + "/reset", {});
      msg.className = "msg ok";
      msg.textContent = "已清零";
      if (window.__proxyRefresh) window.__proxyRefresh();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  } }) : null;

  const title = isEdit
    ? "编辑用户 · " + existing.email
    : "为 " + (ib.remark || ib.tag) + " 添加用户";
  const m = modal(title, [
    field("用户标识", email, "全局唯一；Xray 用它区分流量归属"),
    el("div", { class: "row" }, [
      field("流量配额（GB）", quota, "留空或 0 表示不限"),
      field("到期日期", el("div", { class: "row", style: "gap:12px" }, [
        expiry,
        el("label", { class: "check-inline" }, [forever, el("span", { text: "永久有效" })]),
      ]), "到期后 Xray 拒绝握手，可随时改"),
    ]),
    el("label", { class: "row", style: "margin-top:4px" }, [enabled, el("span", { text: "启用该用户" })]),
    el("div", { class: "actions" }, [submit, resetTraffic]),
    msg,
  ]);
}

async function toggleClient(c) {
  try {
    await api("PATCH", "/api/v1/proxies/clients/" + c.id, {
      email: c.email, enabled: !c.enabled, total_bytes: c.total_bytes,
      expires_at: c.expires_at || undefined,
    });
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function removeClient(c) {
  if (!confirm("确定删除用户 " + c.email + "？该操作会立刻断开其连接。")) return;
  try {
    await api("DELETE", "/api/v1/proxies/clients/" + c.id, {});
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function toggleInbound(ib) {
  try {
    await api("PATCH", "/api/v1/proxies/inbounds/" + ib.id, { enabled: !ib.enabled });
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

async function removeInbound(ib) {
  if (!confirm("确定删除入站 " + (ib.remark || ib.tag) + "？其下所有用户会一并删除。")) return;
  try {
    await api("DELETE", "/api/v1/proxies/inbounds/" + ib.id, {});
    if (window.__proxyRefresh) window.__proxyRefresh();
  } catch (e) { alert(e.message); }
}

// ---------- 用户管理 ----------

// 代理页是「按入站看用户」，用户多了以后没人愿意一个一个入站点开找。
// 这里把所有入站下的用户摊平成一张表，流量 / 到期 / 状态 / 编辑都在一处。
function renderUsersView(root) {
  const summary = el("div", { class: "grid stat-grid" });
  const search = el("input", { placeholder: "搜索用户或入站…", style: "max-width:240px" });
  const filter = select([
    ["all", "全部用户"], ["active", "正常"], ["expiring", "7 天内到期"],
    ["expired", "已过期"], ["disabled", "已停用"],
  ], "all");
  filter.style.maxWidth = "170px";
  const wrap = el("div", { class: "table-wrap freeze" });
  const msg = el("div", { class: "msg" });
  const foot = el("div", { class: "hint" });
  let data = { inbounds: [] };

  const dayLeft = (ts) => ts ? Math.ceil((new Date(ts).getTime() - Date.now()) / 86400000) : null;

  const paint = () => {
    const q = search.value.trim().toLowerCase();
    const mode = filter.value;
    const rows = [];

    let total = 0, expired = 0, soon = 0, disabled = 0, traffic = 0;
    for (const ib of data.inbounds) {
      for (const c of ib.clients) {
        total++;
        traffic += (c.up || 0) + (c.down || 0);
        const left = dayLeft(c.expires_at);
        if (!c.enabled) disabled++;
        else if (left !== null && left < 0) expired++;
        else if (left !== null && left <= 7) soon++;

        if (q && !(c.email + " " + (ib.remark || "") + " " + ib.tag).toLowerCase().includes(q)) continue;
        const isExpired = left !== null && left < 0;
        if (mode === "active" && (!c.enabled || isExpired)) continue;
        if (mode === "expiring" && !(c.enabled && left !== null && left >= 0 && left <= 7)) continue;
        if (mode === "expired" && !isExpired) continue;
        if (mode === "disabled" && c.enabled) continue;

        rows.push(el("tr", {}, [
          el("td", {}, [
            el("div", { class: "app-name", text: c.email }),
            el("div", { class: "mono muted", text: c.secret.slice(0, 16) + (c.secret.length > 16 ? "…" : "") }),
          ]),
          el("td", {}, [
            el("div", { text: ib.remark || ib.tag }),
            el("div", { class: "hint mono", text: ib.protocol.toUpperCase() + " · :" + ib.port }),
          ]),
          usedCell(c),
          expiryCell(c.expires_at),
          el("td", {}, [statusBadge(c)]),
          el("td", {}, [el("div", { class: "row" }, [
            el("button", { class: "link", text: "链接", onclick: () => showLink(c) }),
            el("button", { class: "link", text: "编辑", onclick: () => clientForm(ib, c) }),
            el("button", { class: "link", text: c.enabled ? "禁用" : "启用", onclick: () => toggleClient(c) }),
            el("button", { class: "link danger", text: "删除", onclick: () => removeClient(c) }),
          ])]),
        ]));
      }
    }

    summary.replaceChildren(
      statCard("用户总数", String(total), data.inbounds.length + " 个入站"),
      statCard("7 天内到期", String(soon), soon ? "记得提前续期" : "暂无"),
      statCard("已过期", String(expired), expired ? "这些用户已无法连接" : "暂无"),
      statCard("已停用", String(disabled), "手动关闭的账号"),
    );

    wrap.replaceChildren(rows.length
      ? el("table", { class: "data store-table" }, [
        el("thead", {}, [el("tr", {}, ["用户", "所属入站", "已用流量", "到期", "状态", "操作"]
          .map((h) => el("th", { text: h })))]),
        el("tbody", {}, rows),
      ])
      : emptyHint(data.inbounds.length ? "没有符合条件的用户" : "还没有用户：先到「代理」页建一个入站，再给入站加用户"));
    foot.textContent = "共 " + total + " 位用户 · 累计流量 " + fmtBytes(traffic)
      + (rows.length !== total ? " · 当前筛出 " + rows.length + " 位" : "");
  };

  const load = async () => {
    try {
      data = await api("GET", "/api/v1/proxies");
      msg.textContent = "";
      paint();
    } catch (e) {
      if (e.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  search.addEventListener("input", paint);
  filter.addEventListener("change", paint);
  window.__proxyRefresh = load;

  root.replaceChildren(
    summary,
    card("用户", [
      el("div", { class: "row wrap toolbar" }, [
        search, filter,
        el("div", { class: "spacer" }),
        el("button", { text: "+ 新建用户", onclick: () => quickUserForm() }),
        el("button", { class: "ghost", text: "刷新", onclick: load }),
      ]),
      msg, wrap, foot,
    ]),
  );
  load();
}

// 用户页的「新建」不先选入站会很别扭，所以先让用户挑一个入站再开表单。
function quickUserForm() {
  api("GET", "/api/v1/proxies").then((d) => {
    if (!(d.inbounds || []).length) {
      modal("先创建入站", [el("p", { class: "sub", text: "还没有任何入站，用户在入站下创建。去「代理」页新建一个入站吧。" })]);
      return;
    }
    const known = d.inbounds.filter((ib) => ib.enabled);
    const list = known.length ? known : d.inbounds;
    const sel = select(list.map((ib) => [String(ib.id), (ib.remark || ib.tag) + " · " + ib.protocol.toUpperCase() + " :" + ib.port]), String(list[0].id));
    const m = modal("选择入站", [
      field("入站", sel, "用户归属哪个入站，决定协议与端口"),
      el("div", { class: "row" }, [
        el("button", { text: "下一步", onclick: () => {
          const ib = list.find((x) => String(x.id) === sel.value);
          m.close();
          clientForm(ib, null);
        } }),
      ]),
    ]);
  }).catch((e) => alert(e.message));
}

// ---------- 通用小部件 ----------

function card(title, nodes, extra) {
  const head = el("div", { class: "card-head" }, [
    el("h2", { text: title }),
    extra ? el("div", { class: "row" }, extra) : null,
  ]);
  return el("div", { class: "card section" }, [head].concat(nodes));
}

function fmtTime(s) {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d.getTime())) return String(s);
  const p = (n) => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) +
    " " + p(d.getHours()) + ":" + p(d.getMinutes());
}

function simpleTable(headers, rows) {
  return el("table", { class: "data" }, [
    el("thead", {}, [el("tr", {}, headers.map((h) => el("th", { text: h })))]),
    el("tbody", {}, rows),
  ]);
}

function emptyHint(text) {
  return el("div", { class: "empty", text: text });
}

// ---------- 文件管理 ----------

function renderFilesView(root) {
  let cwd = "";
  const pathBar = el("div", { class: "path-bar mono", text: "/" });
  const listBox = el("div", { class: "table-wrap freeze" });
  const msg = el("div", { class: "msg" });
  const fileInput = el("input", { type: "file", multiple: true, style: "display:none" });

  // 批量选择状态：selected 存路径，rowCbs/rowEls 让「全选」「shift 连选」
  // 能直接回写行内复选框与高亮，不必整表重绘。
  const selected = new Set();
  const rowCbs = new Map();
  const rowEls = new Map();
  let listed = [];
  let lastPick = -1;

  const headCb = el("input", { type: "checkbox", title: "全选当前目录" });
  const selBar = el("div", { class: "selection-bar", style: "display:none" });
  const selText = el("span", { class: "selection-text" });
  const selBytes = el("span", { class: "selection-size" });

  const syncSelection = () => {
    let bytes = 0, files = 0;
    for (const e of listed) {
      const on = selected.has(e.path);
      const cb = rowCbs.get(e.path);
      if (cb) cb.checked = on;
      const tr = rowEls.get(e.path);
      if (tr) tr.classList.toggle("selected", on);
      if (on && !e.is_dir) { bytes += e.size || 0; files++; }
    }
    const n = selected.size;
    selBar.style.display = n ? "" : "none";
    selText.textContent = "已选 " + n + " 项";
    const parts = [];
    if (bytes) parts.push(fmtBytes(bytes));
    if (files < n) parts.push("含目录");
    selBytes.textContent = parts.join(" · ");
    headCb.checked = n > 0 && n === listed.length;
    headCb.indeterminate = n > 0 && n < listed.length;
  };

  const packDownload = () => {
    const paths = Array.from(selected);
    if (!paths.length) return alert("请先勾选文件");
    if (paths.length > 200) return alert("一次最多打包 200 项，请分批下载");
    const q = paths.map((p) => "path=" + encodeURIComponent(p)).join("&");
    msg.className = "msg ok";
    msg.textContent = "正在打包 " + paths.length + " 项，浏览器随后开始下载…";
    window.location.href = "/api/v1/files/batch-download?" + q;
  };

  const packToServer = async () => {
    const paths = Array.from(selected);
    if (!paths.length) return alert("请先勾选文件");
    const dest = prompt("压缩包保存路径", joinPath(cwd, "archive-" + Date.now() + ".tar.gz"));
    if (!dest) return;
    try {
      await api("POST", "/api/v1/files/archive", { paths: paths, dest: dest });
      refresh();
    } catch (e) { alert(e.message); }
  };

  const refresh = async () => {
    msg.className = "msg";
    msg.textContent = "读取中…";
    try {
      const data = await api("GET", "/api/v1/files?path=" + encodeURIComponent(cwd) + "&hidden=1");
      pathBar.textContent = "/" + (data.path || "");
      selected.clear();
      renderList(data);
      msg.textContent = "";
    } catch (err) {
      if (err.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      msg.className = "msg err";
      msg.textContent = err.message;
    }
  };

  const openFile = async (path) => {
    try {
      const data = await api("GET", "/api/v1/files/content?path=" + encodeURIComponent(path));
      if (data.binary) {
        window.location.href = "/api/v1/files/download?path=" + encodeURIComponent(path);
        return;
      }
      const ta = el("textarea", { class: "editor", spellcheck: "false" });
      ta.value = data.content;
      const info = el("div", { class: "hint", text: data.size + " 字节 · UTF-8" });
      const save = el("button", { text: "保存" });
      const m = modal("编辑 " + path, [info, ta, el("div", { class: "row", style: "margin-top:12px" }, [save])]);
      ta.style.height = "50vh";
      save.addEventListener("click", async () => {
        save.disabled = true;
        try {
          await api("PUT", "/api/v1/files/content", { path: path, content: ta.value });
          m.close();
          refresh();
        } catch (e) {
          alert(e.message);
        } finally { save.disabled = false; }
      });
      setTimeout(() => ta.focus(), 50);
    } catch (e) { alert(e.message); }
  };

  const rename = async (path) => {
    const to = prompt("新路径（可含子目录）", path);
    if (!to || to === path) return;
    try {
      await api("POST", "/api/v1/files/rename", { from: path, to: to, overwrite: false });
      refresh();
    } catch (e) { alert(e.message); }
  };

  const removePaths = (paths) => {
    if (!paths.length) return alert("请先勾选文件");
    const permanent = el("input", { type: "checkbox" });
    const go = el("button", { text: "移入回收站" });
    const tip = el("div", { class: "hint", text: "默认移入回收站，保留 7 天，期间可以在「回收站」里原样恢复。" });
    const m = modal("删除 " + paths.length + " 项", [
      el("div", { class: "warn-box", text: "删除的是服务器上的真实文件。系统关键目录、挂载点、面板自身的数据与配置目录会被服务端直接拒绝，勾选也没用。" }),
      el("pre", { class: "log", text: paths.join("\n") }),
      el("label", { class: "row", style: "margin-top:10px" }, [permanent, el("span", { text: "彻底删除（不放进回收站，不可恢复）" })]),
      tip,
      el("div", { class: "actions" }, [
        go, el("button", { class: "ghost", text: "取消", onclick: () => m.close() }),
      ]),
    ]);
    permanent.addEventListener("change", () => {
      go.className = permanent.checked ? "danger" : "";
      go.textContent = permanent.checked ? "彻底删除" : "移入回收站";
      tip.textContent = permanent.checked
        ? "彻底删除不经过回收站，删除后无法恢复，点下去还会再要求输入一次 DELETE。"
        : "默认移入回收站，保留 7 天，期间可以在「回收站」里原样恢复。";
    });
    go.addEventListener("click", async () => {
      if (permanent.checked) {
        const w = prompt("彻底删除后无法恢复。输入 DELETE 继续：");
        if (w !== "DELETE") return;
      }
      go.disabled = true;
      try {
        const res = await api("POST", "/api/v1/files/delete", {
          paths: paths, confirm: "DELETE", permanent: permanent.checked,
        });
        m.close();
        msg.className = "msg ok";
        msg.textContent = res.trashed
          ? "已移入回收站 " + res.trashed + " 项"
          : "已彻底删除 " + res.deleted + " 项";
        refresh();
      } catch (e) {
        alert(e.message);
      } finally {
        go.disabled = false;
      }
    });
  };

  const openTrash = async () => {
    const body = el("div");
    modal("回收站", [body]);

    const act = async (fn) => {
      try { await fn(); await loadTrash(); await refresh(); }
      catch (e) { alert(e.message); }
    };

    const paint = (d) => {
      if (!d.items.length) {
        body.replaceChildren(
          el("div", { class: "empty", text: "回收站是空的" }),
          el("div", { class: "hint", style: "margin-top:8px", text: "目录：" + d.dir }),
        );
        return;
      }
      const rows = d.items.map((it) => {
        const restore = el("button", { class: "link", text: "恢复", onclick: () => act(() =>
          api("POST", "/api/v1/trash/restore", { ids: [it.id] })) });
        const kill = el("button", { class: "link danger", text: "彻底删除", onclick: () => {
          const w = prompt("彻底删除「" + it.name + "」后不可恢复，输入 DELETE 继续：");
          if (w !== "DELETE") return;
          act(() => api("POST", "/api/v1/trash/delete", { ids: [it.id], confirm: "DELETE" }));
        } });
        return el("tr", {}, [
          el("td", { text: it.name + (it.is_dir ? "/" : "") }),
          el("td", { class: "mono", text: "/" + it.original }),
          el("td", { class: "mono", text: it.is_dir ? "—" : fmtBytes(it.size) }),
          el("td", { class: "mono", text: fmtTime(it.deleted_at) }),
          el("td", {}, [el("div", { class: "row" }, [restore, kill])]),
        ]);
      });
      body.replaceChildren(
        el("div", { class: "row spread" }, [
          el("span", { class: "hint", text: d.count + " 项 · " + fmtBytes(d.bytes) + " · 自动保留 7 天" }),
          el("button", { class: "ghost danger", text: "清空回收站", onclick: () => {
            const w = prompt("清空回收站后不可恢复，输入 DELETE 继续：");
            if (w !== "DELETE") return;
            act(() => api("POST", "/api/v1/trash/delete", { all: true, confirm: "DELETE" }));
          } }),
        ]),
        el("div", { class: "table-wrap", style: "margin-top:10px" },
          [simpleTable(["名称", "原路径", "大小", "删除时间", "操作"], rows)]),
        el("div", { class: "hint", style: "margin-top:10px", text: "目录：" + d.dir }),
      );
    };

    const loadTrash = async () => {
      body.replaceChildren(el("div", { class: "hint", text: "读取中…" }));
      try { paint(await api("GET", "/api/v1/trash")); }
      catch (e) { body.replaceChildren(el("div", { class: "msg err", text: e.message })); }
    };
    loadTrash();
  };

  const renderList = (data) => {
    listed = data.entries || [];
    rowCbs.clear();
    rowEls.clear();
    lastPick = -1;

    const rows = [];
    if (data.parent !== "" || data.path !== "") {
      const up = el("button", { class: "link", text: ".. 上一级", onclick: () => { cwd = data.parent; refresh(); } });
      rows.push(el("tr", { class: "row-up" }, [el("td", { colSpan: 5 }, [up])]));
    }
    for (const e of listed) {
      const cb = el("input", { type: "checkbox", "aria-label": "选择 " + e.name });
      const onToggle = (ev) => {
        const idx = listed.findIndex((x) => x.path === e.path);
        if (ev.shiftKey && lastPick >= 0 && idx >= 0) {
          const a = Math.min(lastPick, idx), b = Math.max(lastPick, idx);
          for (let i = a; i <= b; i++) selected.add(listed[i].path);
        } else if (cb.checked) {
          selected.add(e.path);
        } else {
          selected.delete(e.path);
        }
        lastPick = idx;
        syncSelection();
      };
      cb.addEventListener("click", onToggle);
      const checkCell = el("td", { class: "check" }, [cb]);
      // 点整格都能选中，避免只有 15px 的小方块可点
      checkCell.addEventListener("click", (ev) => {
        if (ev.target === cb) return;
        cb.checked = !cb.checked;
        onToggle(ev);
      });

      const name = el("span", { class: "file-name" + (e.is_dir ? " dir" : ""), text: e.name + (e.is_link ? " ↗" : "") });
      name.addEventListener("click", () => {
        if (e.is_dir) { cwd = e.path; refresh(); } else { openFile(e.path); }
      });
      const actions = el("div", { class: "row" }, [
        e.is_dir ? null : el("button", { class: "link", text: "下载", onclick: () => {
          window.location.href = "/api/v1/files/download?path=" + encodeURIComponent(e.path);
        } }),
        el("button", { class: "link", text: "重命名", onclick: () => rename(e.path) }),
        el("button", { class: "link danger", text: "删除", onclick: () => removePaths([e.path]) }),
      ]);
      const tr = el("tr", {}, [
        checkCell,
        el("td", {}, [name]),
        el("td", { class: "mono", text: e.is_dir ? "—" : fmtBytes(e.size) }),
        el("td", { class: "mono", text: fmtTime(e.mod_time) }),
        el("td", {}, [actions]),
      ]);
      rowCbs.set(e.path, cb);
      rowEls.set(e.path, tr);
      rows.push(tr);
    }

    syncSelection();
    if (!listed.length) {
      listBox.replaceChildren(el("div", { class: "hint", style: "padding:28px;text-align:center", text: "空目录" }));
      return;
    }
    listBox.replaceChildren(el("table", { class: "data files" }, [
      el("thead", {}, [el("tr", {}, [
        el("th", { class: "check" }, [headCb]),
        el("th", { text: "名称" }),
        el("th", { text: "大小" }),
        el("th", { text: "修改时间" }),
        el("th", { text: "操作" }),
      ])]),
      el("tbody", {}, rows),
    ]));
  };

  headCb.addEventListener("change", () => {
    if (headCb.checked) listed.forEach((e) => selected.add(e.path));
    else selected.clear();
    lastPick = -1;
    syncSelection();
  });

  selBar.replaceChildren(
    el("div", { class: "selection-info" }, [selText, selBytes]),
    el("div", { class: "spacer" }),
    el("button", { class: "ghost", text: "打包下载", onclick: packDownload }),
    el("button", { class: "ghost", text: "打包到服务器", onclick: packToServer }),
    el("button", { class: "ghost danger", text: "删除所选", onclick: () => removePaths(Array.from(selected)) }),
    el("button", { class: "link", text: "取消选择", onclick: () => { selected.clear(); lastPick = -1; syncSelection(); } }),
  );

  // 工具栏按「新建 / 批量 / 目录」分组，中间用竖线断开：
  // 六个同等权重的按钮排成一排会糊成一条，分组后一眼能找到主操作。
  const toolbar = el("div", { class: "row wrap toolbar" }, [
    el("button", { text: "上传文件", onclick: () => fileInput.click() }),
    el("button", { class: "ghost", text: "新建目录", onclick: async () => {
      const name = prompt("目录名");
      if (!name) return;
      try { await api("POST", "/api/v1/files/mkdir", { path: joinPath(cwd, name) }); refresh(); }
      catch (e) { alert(e.message); }
    } }),
    el("button", { class: "ghost", text: "新建文件", onclick: async () => {
      const name = prompt("文件名");
      if (!name) return;
      try { await api("PUT", "/api/v1/files/content", { path: joinPath(cwd, name), content: "" }); refresh(); }
      catch (e) { alert(e.message); }
    } }),
    el("div", { class: "tool-divider" }),
    el("button", { class: "ghost", text: "回收站", onclick: openTrash }),
    el("div", { class: "spacer" }),
    el("button", { class: "ghost", text: "刷新", onclick: refresh }),
  ]);

  fileInput.addEventListener("change", async () => {
    if (!fileInput.files.length) return;
    const fd = new FormData();
    for (const f of fileInput.files) fd.append("file", f);
    msg.className = "msg";
    msg.textContent = "上传中…";
    try {
      const res = await fetch("/api/v1/files/upload?path=" + encodeURIComponent(cwd), {
        method: "POST",
        body: fd,
        credentials: "same-origin",
        headers: { "X-CSRF-Token": state.csrf || cookie("pv_csrf") },
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.msg || "上传失败");
      msg.textContent = "已上传 " + data.data.files.length + " 个文件";
      fileInput.value = "";
      refresh();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  });

  root.replaceChildren(
    card("文件管理", [
      el("div", { class: "path-row" }, [pathBar]),
      toolbar,
      selBar,
      msg,
      listBox,
      fileInput,
    ]),
  );
  refresh();
}

function joinPath(base, name) {
  return (base ? base.replace(/\/$/, "") + "/" : "") + name.replace(/^\/+/, "");
}

// ---------- Web 终端 ----------

function stripAnsi(s) {
  return s
    .replace(/\x1b\][^\x07]*\x07/g, "")
    .replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "")
    .replace(/\x1b[()][A-Z0-9]/g, "")
    .replace(/\r/g, "");
}

function renderTerminalView(root) {
  if (typeof window.Terminal !== "function" || !(window.FitAddon && window.FitAddon.FitAddon)) {
    renderTerminalFallback(root);
    return;
  }

  const host = el("div", { class: "term-host" });
  const msg = el("div", { class: "msg" });
  const status = el("span", { class: "badge plain", text: "未连接" });
  const dimLbl = el("span", { class: "hint mono", text: "—" });
  const connectBtn = el("button", { text: "连接" });
  const clearBtn = el("button", { class: "ghost", text: "清屏" });
  let ws = null;

  const setStatus = (text, cls) => { status.className = "badge " + (cls || "plain"); status.textContent = text; };
  const send = (s) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "input", data: s })); };
  const sendResize = (c, r) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: c, rows: r })); };

  // xterm 负责 ANSI 解析、光标定位、退格/全角宽度与 UTF-8 流式解码，
  // 不再由前端拼字符串——之前的「光标不跟随、ping 后面出乱码」都是那套做法带来的。
  const term = new Terminal({
    cursorBlink: true,
    cursorStyle: "block",
    fontSize: 13,
    fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, "Cascadia Code", Consolas, monospace',
    lineHeight: 1.3,
    scrollback: 5000,
    convertEol: false,
    theme: {
      background: "#0d1117", foreground: "#d1d7e0", cursor: "#7aa2f7", cursorAccent: "#0d1117",
      selectionBackground: "rgba(122,162,247,.30)",
      black: "#1b1f27", red: "#f7768e", green: "#9ece6a", yellow: "#e0af68",
      blue: "#7aa2f7", magenta: "#bb9af7", cyan: "#7dcfff", white: "#c0caf5",
      brightBlack: "#5a6478", brightRed: "#ff9aae", brightGreen: "#b9f27c", brightYellow: "#ffd68a",
      brightBlue: "#9ab8ff", brightMagenta: "#d4b0ff", brightCyan: "#a4e4ff", brightWhite: "#ffffff",
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);

  const doFit = () => {
    try { fit.fit(); } catch (_) { /* 容器还没布局好，下次再算 */ }
    dimLbl.textContent = term.cols + " × " + term.rows;
  };

  const connect = async () => {
    if (ws) { try { ws.close(); } catch (_) {} ws = null; }
    msg.className = "msg";
    msg.textContent = "正在申请终端票据…";
    setStatus("连接中", "warn");
    try {
      const t = await api("POST", "/api/v1/terminal/ticket", {});
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const url = proto + "://" + location.host + "/api/v1/terminal/ws?ticket=" +
        encodeURIComponent(t.ticket) + "&cols=" + term.cols + "&rows=" + term.rows;
      ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      ws.onopen = () => {
        msg.className = "msg ok";
        msg.textContent = "已连接";
        setStatus("已连接", "ok");
        term.focus();
        doFit();
        sendResize(term.cols, term.rows);
      };
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const m = JSON.parse(ev.data);
            if (m.type === "exit") {
              term.write("\r\n\x1b[2m[会话结束]\x1b[0m\r\n");
              msg.textContent = "会话已结束";
              setStatus("已结束", "plain");
            } else if (m.type === "error") {
              msg.className = "msg err";
              msg.textContent = m.data;
              setStatus("错误", "err");
            }
          } catch (_) {
            term.write(ev.data);
          }
          return;
        }
        // 二进制帧原样交给 xterm：ANSI 序列与多字节字符由它自己处理
        term.write(new Uint8Array(ev.data));
      };
      ws.onclose = () => {
        setStatus("已断开", "plain");
        if (msg.textContent === "已连接") { msg.className = "msg"; msg.textContent = "连接已断开"; }
      };
      ws.onerror = () => {
        msg.className = "msg err";
        msg.textContent = "WebSocket 连接失败";
        setStatus("失败", "err");
      };
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
      setStatus("未连接", "plain");
    }
  };

  connectBtn.addEventListener("click", connect);
  clearBtn.addEventListener("click", () => term.clear());

  root.replaceChildren(card("Web 终端", [
    // 一行只放控件，说明文字另起一行：原来五个元素挤在一条线上，
    // 后半句在 1280 宽度以下一定会被挤成两行，按钮也跟着移位。
    el("div", { class: "row wrap toolbar" }, [
      connectBtn,
      clearBtn,
      status,
      dimLbl,
    ]),
    el("div", { class: "hint", style: "margin-top:10px", text: "以 root 运行，不需要 sudo（sudo 用的是 setuid 二进制，已被沙箱挡住）；Ctrl+C 中断，Ctrl+Shift+V 粘贴" }),
    msg,
    host,
    el("div", { class: "actions", style: "justify-content:space-between" }, [
      el("span", { class: "hint", text: "终端默认关闭，到「设置 → 基本设置」勾选「启用 Web 终端」即可生效，不用重启。会话最长 30 分钟，同时最多 2 个。" }),
      el("button", { class: "ghost", text: "去设置", onclick: () => renderApp("settings") }),
    ]),
  ]));

  term.open(host);
  requestAnimationFrame(doFit);
  term.onData(send);
  term.onResize(({ cols, rows }) => { dimLbl.textContent = cols + " × " + rows; sendResize(cols, rows); });
  // 选中即复制：剪贴板 API 只在 HTTPS / localhost 下可用，失败就算了，不打断使用
  term.onSelectionChange(() => {
    const sel = term.getSelection();
    if (sel && navigator.clipboard) navigator.clipboard.writeText(sel).catch(() => {});
  });
  term.focus();

  const ro = new ResizeObserver(() => { try { fit.fit(); } catch (_) {} });
  ro.observe(host);

  state.cleanup = () => {
    try { if (ws) ws.close(); } catch (_) {}
    ro.disconnect();
    try { term.dispose(); } catch (_) {}
  };

  setTimeout(connect, 120);
}

// 兜底渲染：vendor 里的 xterm 没加载成功时仍能连着看输出（功能有限）。
function renderTerminalFallback(root) {
  const out = el("pre", { class: "term", tabindex: "0" });
  const msg = el("div", { class: "msg" });
  const cols = el("span", { class: "mono", text: "—" });
  let ws = null;
  let buffer = "";

  const size = () => {
    const rect = out.getBoundingClientRect();
    const c = Math.max(40, Math.floor(rect.width / 7.4));
    const r = Math.max(10, Math.floor(rect.height / 17));
    cols.textContent = c + " × " + r;
    return { c: c, r: r };
  };

  const append = (text) => {
    buffer += text;
    if (buffer.length > 300000) buffer = buffer.slice(-200000);
    out.textContent = buffer;
    out.scrollTop = out.scrollHeight;
  };
  const send = (s) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "input", data: s })); };

  const connect = async () => {
    try {
      const t = await api("POST", "/api/v1/terminal/ticket", {});
      const dim = size();
      const proto = location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(proto + "://" + location.host + "/api/v1/terminal/ws?ticket=" +
        encodeURIComponent(t.ticket) + "&cols=" + dim.c + "&rows=" + dim.r);
      ws.binaryType = "arraybuffer";
      ws.onopen = () => { msg.className = "msg ok"; msg.textContent = "已连接（简化渲染）"; out.focus(); };
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const m = JSON.parse(ev.data);
            if (m.type === "exit") append("\n[会话结束]\n");
            if (m.type === "error") { msg.className = "msg err"; msg.textContent = m.data; }
          } catch (_) { append(ev.data); }
          return;
        }
        append(stripAnsi(new TextDecoder("utf-8").decode(new Uint8Array(ev.data))));
      };
      ws.onclose = () => { msg.className = "msg"; msg.textContent = "连接已断开"; };
      ws.onerror = () => { msg.className = "msg err"; msg.textContent = "WebSocket 连接失败"; };
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  const keyMap = {
    Enter: "\r", Backspace: "\x7f", Tab: "\t", Escape: "\x1b",
    ArrowUp: "\x1b[A", ArrowDown: "\x1b[B", ArrowRight: "\x1b[C", ArrowLeft: "\x1b[D",
    Home: "\x1b[H", End: "\x1b[F", Delete: "\x1b[3~", PageUp: "\x1b[5~", PageDown: "\x1b[6~",
  };
  out.addEventListener("keydown", (e) => {
    if (e.ctrlKey && e.key.length === 1) {
      const code = e.key.toUpperCase().charCodeAt(0) - 64;
      if (code >= 0 && code < 32) { e.preventDefault(); send(String.fromCharCode(code)); return; }
    }
    if (keyMap[e.key]) { e.preventDefault(); send(keyMap[e.key]); return; }
    if (e.key.length === 1) { e.preventDefault(); send(e.key); }
  });
  out.addEventListener("paste", (e) => { e.preventDefault(); send(e.clipboardData.getData("text")); });

  root.replaceChildren(card("Web 终端", [
    el("div", { class: "term-bar" }, [
      el("button", { text: "连接", onclick: connect }),
      el("button", { class: "ghost", text: "清屏", onclick: () => { buffer = ""; out.textContent = ""; } }),
      el("span", { class: "hint", text: "尺寸 " }, [cols]),
      el("span", { class: "hint", text: "简化渲染：未加载到终端组件，光标与全角字符可能不准" }),
    ]),
    msg,
    out,
  ]));
  setTimeout(connect, 100);
}

// ---------- 防火墙 ----------

function renderFirewallView(root) {
  const statusBox = el("div");
  const rulesBox = el("div");
  const bansBox = el("div");
  const sshBox = el("div");
  const msg = el("div", { class: "msg" });

  const load = async () => {
    try {
      const data = await api("GET", "/api/v1/firewall");
      const st = data.status;
      statusBox.replaceChildren(
        el("div", { class: "grid stat-grid" }, [
          statCard("后端", st.backend === "nft" ? "nftables" : st.backend === "iptables" ? "iptables-legacy" : "不可用",
            st.supported ? "已就绪" : (st.reason || "")),
          statCard("模式", st.whitelist ? "白名单（默认拒绝）" : "黑名单（默认放行）",
            st.whitelist ? "仅放行已添加规则" : "未匹配流量放行"),
          statCard("SSH 端口", (data.ssh_ports || []).join(", "), "始终优先放行"),
          statCard("封禁数", String((st.bans || []).length), "nft set 自动到期"),
        ]),
        el("div", { class: "hint mono", text: "持久化文件：" + (data.persist_path || "-") }),
      );

      const rows = (data.rules || []).map((r) => {
        const del = el("button", { class: "link danger", text: "删除", onclick: async () => {
          if (!confirm("删除规则 #" + r.id + "？")) return;
          try { await api("DELETE", "/api/v1/firewall/rules/" + r.id, {}); load(); }
          catch (e) { alert(e.message); }
        } });
        return el("tr", {}, [
          el("td", { class: "mono", text: "#" + r.id }),
          el("td", {}, [badge(r.action === "accept" ? "放行" : "拒绝", r.action === "accept" ? "ok" : "err")]),
          el("td", { text: r.proto }),
          el("td", { class: "mono", text: r.port || "*" }),
          el("td", { class: "mono", text: r.source || "任意" }),
          el("td", { text: r.comment || "" }),
          el("td", {}, [del]),
        ]);
      });
      rulesBox.replaceChildren(rows.length ? simpleTable(["ID", "动作", "协议", "端口", "来源", "备注", ""], rows) : emptyHint("还没有规则"));

      const bans = st.bans || [];
      const banRows = bans.map((b) => el("tr", {}, [
        el("td", { class: "mono", text: b.ip }),
        el("td", { class: "mono", text: b.expires_at ? fmtTime(b.expires_at) : "手动" }),
        el("td", {}, [el("button", { class: "link", text: "解封", onclick: async () => {
          try { await api("DELETE", "/api/v1/firewall/bans/" + encodeURIComponent(b.ip), {}); load(); }
          catch (e) { alert(e.message); }
        } })]),
      ]));
      bansBox.replaceChildren(banRows.length ? simpleTable(["IP", "到期", ""], banRows) : emptyHint("当前没有封禁记录"));
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  const loadSSH = async () => {
    try {
      const d = await api("GET", "/api/v1/security/ssh?minutes=10");
      if (!d.supported) {
        sshBox.replaceChildren(emptyHint(d.reason || "SSH 日志不可用（非 Linux 或没有 journalctl）"));
        return;
      }
      const rows = (d.counts || []).slice(0, 10).map((c) => el("tr", {}, [
        el("td", { class: "mono", text: c.ip }),
        el("td", {}, [badge(String(c.count), c.count >= d.threshold ? "err" : "warn")]),
        el("td", {}, [el("button", { class: "link", text: "封禁 " + d.ban_minutes + " 分钟", onclick: async () => {
          try { await api("POST", "/api/v1/firewall/bans", { ip: c.ip, minutes: d.ban_minutes }); load(); }
          catch (e) { alert(e.message); }
        } })]),
      ]));
      sshBox.replaceChildren(
        el("div", { class: "hint", text: "最近 10 分钟失败次数，阈值 " + d.threshold + " 次自动封禁 " + d.ban_minutes + " 分钟；防护当前" + (d.guard ? "已开启" : "已关闭") }),
        rows.length ? simpleTable(["来源 IP", "失败次数", "操作"], rows) : emptyHint("最近没有 SSH 失败记录"),
      );
    } catch (e) {
      sshBox.replaceChildren(emptyHint(e.message));
    }
  };

  // 五个控件 + 按钮挤在一行时，每项只能分到 100 多像素，
  // 标签全靠 placeholder，填到一半就不知道自己在填什么。
  const addForm = el("form", { class: "form-grid" }, [
    field("动作", select([["accept", "放行"], ["drop", "拒绝"]], "accept")),
    field("协议", select([["tcp", "TCP"], ["udp", "UDP"], ["any", "全部"]], "tcp")),
    field("端口", el("input", { placeholder: "80 或 8000-8100" })),
    field("来源", el("input", { placeholder: "IP / CIDR，可空" })),
    field("备注", el("input", { placeholder: "可空" })),
    el("div", { class: "form-actions" }, [
      el("span", { class: "hint", text: "新增后需点上方「下发规则到内核」才会生效" }),
      el("button", { type: "submit", text: "添加规则" }),
    ]),
  ]);
  addForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const kids = addForm.querySelectorAll("select, input");
    try {
      await api("POST", "/api/v1/firewall/rules", {
        action: kids[0].value, proto: kids[1].value,
        port: kids[2].value.trim(), source: kids[3].value.trim(), comment: kids[4].value.trim(),
      });
      kids[2].value = ""; kids[3].value = ""; kids[4].value = "";
      load();
    } catch (err) { alert(err.message); }
  });

  const applyBtn = el("button", { text: "下发规则到内核", onclick: async () => {
    if (!confirm("将重建 panelvps 表并立即生效。\nSSH 与面板端口会被自动放行。继续？")) return;
    try {
      const r = await api("POST", "/api/v1/firewall/apply", {});
      msg.className = "msg ok";
      msg.textContent = "已下发（" + r.backend + "），共 " + (r.ssh_ports || []).length + " 个 SSH 端口被强制放行";
      load();
    } catch (err) {
      msg.className = "msg err";
      msg.textContent = err.message;
    }
  } });

  root.replaceChildren(
    card("防火墙", [msg, statusBox, el("div", { class: "actions" }, [applyBtn])]),
    card("规则", [addForm, rulesBox]),
    card("手动封禁", [
      (() => {
        const ip = el("input", { placeholder: "IP 地址", style: "max-width:200px" });
        const minutes = el("input", { placeholder: "分钟", value: "60", style: "max-width:100px" });
        const btn = el("button", { text: "封禁", onclick: async () => {
          try { await api("POST", "/api/v1/firewall/bans", { ip: ip.value.trim(), minutes: parseInt(minutes.value, 10) || 60 }); ip.value = ""; load(); }
          catch (e) { alert(e.message); }
        } });
        return el("div", { class: "row wrap" }, [ip, minutes, btn]);
      })(),
      bansBox,
    ]),
    card("SSH 爆破防护", [sshBox]),
  );
  load();
  loadSSH();
}

// ---------- 日志 ----------

function renderLogsView(root) {
  const sourceSel = select([], "panel");
  const linesSel = select([["100", "100 行"], ["200", "200 行"], ["500", "500 行"], ["1000", "1000 行"]], "200");
  const pre = el("pre", { class: "log tall", text: "" });
  const msg = el("div", { class: "msg" });
  const auto = el("input", { type: "checkbox" });
  let timer = null;

  const load = async () => {
    msg.className = "msg";
    msg.textContent = "读取中…";
    try {
      const d = await api("GET", "/api/v1/logs?source=" + encodeURIComponent(sourceSel.value) + "&lines=" + linesSel.value);
      pre.textContent = (d.lines || []).join("\n") || "（无内容）";
      pre.scrollTop = pre.scrollHeight;
      msg.textContent = d.note || (d.has_more ? "已显示末尾 " + linesSel.value + " 行" : "");
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  const setupAuto = () => {
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    if (auto.checked) state.timer = setInterval(load, 3000);
  };

  api("GET", "/api/v1/logs/sources").then((d) => {
    sourceSel.replaceChildren.apply(sourceSel, (d.items || []).map((s) =>
      el("option", { value: s.key, text: s.label + (s.present ? "" : "（不可用）") })));
    load();
  }).catch((e) => { msg.className = "msg err"; msg.textContent = e.message; });

  sourceSel.addEventListener("change", load);
  linesSel.addEventListener("change", load);
  auto.addEventListener("change", setupAuto);

  root.replaceChildren(card("日志", [
    el("div", { class: "row wrap toolbar" }, [
      sourceSel, linesSel,
      el("label", { class: "row check-inline" }, [auto, el("span", { text: "自动刷新" })]),
      el("div", { class: "spacer" }),
      el("button", { text: "刷新", onclick: load }),
    ]),
    msg, pre,
  ]));
}

// ---------- 中转 ----------

function renderRelayView(root) {
  const list = el("div", {});
  const err = el("div", { class: "msg err" });
  const total = el("div", { class: "mono muted" });

  root.replaceChildren(
    el("div", { class: "card section" }, [
      el("h2", { text: "中转（端口转发）" }),
      el("p", { class: "sub", text: "把本机端口收到的字节原样转发到目标地址。规则跑在「中转机」上——也就是你能直连的那台机器，通常和跑代理的机器不是同一台。" }),
      el("p", { class: "sub", text: "协议内容全程透传：REALITY、TLS、gRPC 都不受影响，服务端不用改任何配置。客户端只要把地址换成中转机的地址和端口。" }),
      el("p", { class: "hint", text: "命令行也能跑：panelvps relay -listen :8443 -to 目标IP:端口（不需要装面板、不建数据库）。" }),
    ]),
    el("div", { class: "card section" }, [
      el("div", { class: "card-head" }, [
        el("div", {}, [el("h2", { text: "规则" }), total]),
        el("button", { class: "ghost", text: "+ 新建中转", onclick: () => relayForm(null, refresh) }),
      ]),
      list,
    ]),
    err,
  );

  const refresh = async () => {
    try {
      const data = await api("GET", "/api/v1/relays");
      paintRelays(list, data.rules || []);
      const sum = (data.rules || []).reduce((a, r) => ({
        up: a.up + (r.stats.up || 0), down: a.down + (r.stats.down || 0),
        active: a.active + (r.stats.active || 0),
      }), { up: 0, down: 0, active: 0 });
      total.textContent = "共 " + (data.rules || []).length + " 条 · 上行 " + fmtBytes(sum.up)
        + " · 下行 " + fmtBytes(sum.down) + " · 活跃连接 " + sum.active;
      err.textContent = "";
    } catch (e) {
      if (e.status === 401) { renderLogin("会话已失效，请重新登录"); return; }
      err.textContent = e.message;
    }
  };

  window.__relayRefresh = refresh;
  refresh();
  state.timer = setInterval(refresh, 5000);
}

function paintRelays(box, rules) {
  if (!rules.length) {
    box.replaceChildren(el("p", { class: "sub", text: "还没有中转规则。点击右上角「新建中转」开始。" }));
    return;
  }
  box.replaceChildren.apply(box, rules.map(relayCard));
}

function relayCard(r) {
  const st = r.stats || {};
  const netLabel = r.network === "both" ? "TCP + UDP" : r.network.toUpperCase();
  const statusBadge = r.enabled
    ? (st.running ? badge("运行中", "ok") : badge("未监听", "warn"))
    : badge("已停用", "warn");

  const detail = el("div", { class: "mono muted" },
    [el("span", { text: ":" + r.listen_port + " → " + r.target + "   " + netLabel
      + "   ↑" + fmtBytes(st.up || 0) + " ↓" + fmtBytes(st.down || 0)
      + "   连接 " + (st.active || 0) + " / " + (st.total || 0) })]);

  const body = [
    el("div", { class: "row" }, [
      el("strong", { text: r.name || ("中转-" + r.listen_port) }),
      statusBadge,
    ]),
    detail,
  ];
  if (st.last_error) {
    body.push(el("div", { class: "hint", style: "color:var(--danger)", text: st.last_error }));
  }

  return el("div", { class: "inbound" }, [
    el("div", { class: "inbound-head" }, [
      el("div", {}, body),
      el("div", { class: "row" }, [
        el("button", { class: "ghost", text: "编辑", onclick: () => relayForm(r, () => window.__relayRefresh && window.__relayRefresh()) }),
        el("button", {
          class: "ghost", text: r.enabled ? "停用" : "启用",
          onclick: () => toggleRelay(r),
        }),
        el("button", { class: "ghost danger", text: "删除", onclick: () => removeRelay(r) }),
      ]),
    ]),
  ]);
}

async function toggleRelay(r) {
  try {
    await api("POST", "/api/v1/relays/" + r.id + "/toggle", {});
    if (window.__relayRefresh) window.__relayRefresh();
  } catch (e) {
    modal("操作失败", [el("p", { class: "msg err", text: e.message })]);
  }
}

async function removeRelay(r) {
  const del = async () => {
    await api("DELETE", "/api/v1/relays/" + r.id, {});
    m.close();
    if (window.__relayRefresh) window.__relayRefresh();
  };
  const delBtn = el("button", { class: "danger", text: "删除规则", onclick: async () => {
    delBtn.disabled = true;
    try { await del(); } catch (e) { msg.className = "msg err"; msg.textContent = e.message; delBtn.disabled = false; }
  } });
  const msg = el("div", { class: "msg" });
  const m = modal("删除中转规则", [
    el("p", { text: "确定删除「" + (r.name || r.listen_port) + "」吗？删除后监听端口会立即释放。" }),
    el("div", { class: "row" }, [
      el("button", { class: "ghost", text: "取消", onclick: () => m.close() }),
      delBtn,
    ]),
    msg,
  ]);
}

function relayForm(existing, after) {
  const name = el("input", { placeholder: "例如：回家中转", value: existing ? (existing.name || "") : "" });
  const listen = el("input", { value: existing ? (existing.listen || "0.0.0.0") : "0.0.0.0" });
  const port = el("input", {
    type: "number", min: "1", max: "65535",
    value: existing ? String(existing.listen_port) : String(randomPort()),
    style: "flex:1;min-width:0",
  });
  const roll = el("button", { class: "ghost", text: "随机", style: "flex:0 0 auto", onclick: () => { port.value = String(randomPort()); } });
  const target = el("input", { placeholder: "目标IP:端口，例如 204.197.163.238:47736", value: existing ? (existing.target || "") : "" });
  const network = select([["tcp", "TCP"], ["udp", "UDP"], ["both", "TCP + UDP"]], existing ? existing.network : "tcp");
  const msg = el("div", { class: "msg" });

  const submit = el("button", { text: existing ? "保存" : "创建", onclick: async () => {
    submit.disabled = true;
    msg.className = "msg";
    msg.textContent = existing ? "保存中…" : "创建中…";
    const body = {
      name: name.value.trim(),
      listen: listen.value.trim() || "0.0.0.0",
      listen_port: parseInt(port.value, 10) || 0,
      target: target.value.trim(),
      network: network.value,
    };
    try {
      if (existing) await api("PATCH", "/api/v1/relays/" + existing.id, body);
      else await api("POST", "/api/v1/relays", body);
      m.close();
      if (after) after();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
      submit.disabled = false;
    }
  } });

  const m = modal(existing ? "编辑中转" : "新建中转", [
    field("名称", name, "只影响显示"),
    el("div", { class: "row" }, [
      field("监听地址", listen, "0.0.0.0 = 全网卡（含 IPv6）"),
      field("监听端口", el("div", { class: "row", style: "gap:6px" }, [port, roll]), "客户端连这个端口"),
    ]),
    field("目标地址", target, "中转机要能访问到的地址：host:port"),
    field("协议", network, "代理流量选 TCP；游戏/UDP 应用才需要 UDP"),
    el("div", { class: "hint", text: "填完后客户端连接「中转机 IP:监听端口」即可，协议的其余参数一律不变。" }),
    submit,
    msg,
  ]);
}

// ---------- 网站 ----------

function renderSitesView(root) {
  const banner = el("div", { class: "banner", style: "display:none" });
  const listBox = el("div");
  const msg = el("div", { class: "msg" });
  let meta = { nginx: {}, root: "", email: "" };

  const load = async () => {
    try {
      const d = await api("GET", "/api/v1/sites");
      meta = d;
      const n = d.nginx || {};
      if (!n.ok) {
        banner.style.display = "block";
        banner.textContent = "Nginx 不可用：" + (n.message || "未安装或配置校验未通过") +
          "。可到「商店」安装 Nginx 后再试。";
      } else {
        banner.style.display = "none";
      }
      const cards = (d.items || []).map((s) => siteCard(s, load));
      listBox.replaceChildren(cards.length ? el("div", { class: "grid g2" }, cards) : emptyHint("还没有站点"));
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  const newBtn = el("button", { text: "新建站点", onclick: () => siteForm(load, meta) });

  root.replaceChildren(
    card("网站", [
      banner,
      el("div", { class: "row wrap toolbar" }, [
        newBtn,
        el("button", { class: "ghost", text: "刷新", onclick: load }),
        el("div", { class: "spacer" }),
        el("span", { class: "hint mono", text: "站点根目录：" + (meta.root || "/var/www") }),
      ]),
      msg,
      listBox,
    ]),
  );
  load();
}

function siteCard(s, reload) {
  const sslBadge = s.ssl
    ? badge("HTTPS · 到期 " + (s.cert_expiry || "?"), "ok")
    : badge("HTTP", "warn");
  const actions = el("div", { class: "row wrap" }, [
    el("button", { class: "ghost", text: "重新下发", onclick: async () => {
      try { await api("POST", "/api/v1/sites/" + s.id + "/apply", {}); reload(); }
      catch (e) { alert(e.message); }
    } }),
    el("button", { class: "ghost", text: "查看配置", onclick: async () => {
      try {
        const d = await api("GET", "/api/v1/sites/" + s.id + "/preview");
        const pre = el("pre", { class: "log tall", text: d.config });
        modal("Nginx 配置 · " + d.path, [pre]);
      } catch (e) { alert(e.message); }
    } }),
    s.ssl ? null : el("button", { class: "ghost", text: "申请证书", onclick: async () => {
      if (!confirm("将为 " + s.domain + " 申请 Let's Encrypt 证书并启用 HTTPS。\n请确认域名已解析到本机且 80 端口可访问。")) return;
      try {
        const job = await api("POST", "/api/v1/sites/" + s.id + "/ssl", {});
        watchJob(job, reload);
      } catch (e) { alert(e.message); }
    } }),
    el("button", { class: "link danger", text: "删除", onclick: async () => {
      if (!confirm("删除站点 " + s.domain + " 的 Nginx 配置？网站文件会保留。")) return;
      try { await api("DELETE", "/api/v1/sites/" + s.id + "?confirm=1", {}); reload(); }
      catch (e) { alert(e.message); }
    } }),
  ]);

  return el("div", { class: "card site-card" }, [
    el("div", { class: "row spread" }, [
      el("div", {}, [el("h3", { text: s.domain }), el("div", { class: "hint mono", text: s.root })]),
      sslBadge,
    ]),
    el("div", { class: "row wrap", style: "margin:10px 0" }, [
      badge(s.kind === "php" ? "PHP " + s.php_version : "静态", "muted"),
      badge("端口 " + s.port, "muted"),
      badge(s.enabled ? "已启用" : "已停用", s.enabled ? "ok" : "warn"),
    ]),
    actions,
  ]);
}

function siteForm(reload, meta) {
  const name = el("input", { placeholder: "站点名（小写字母/数字/-）" });
  const domain = el("input", { placeholder: "example.com" });
  const kind = select([["static", "静态站点"], ["php", "PHP 站点"]], "static");
  const php = el("input", { placeholder: "PHP 版本，如 8.2", value: "8.2" });
  const port = el("input", { placeholder: "80", value: "80" });
  const createRoot = el("input", { type: "checkbox" });
  createRoot.checked = true;

  const body = [
    field("站点名", name, "会生成 panelvps-<name>.conf"),
    field("域名", domain, "多个域名请分别建站"),
    field("类型", kind),
    field("PHP 版本", php, "仅 PHP 站点需要；需先安装对应 php-fpm"),
    field("HTTP 端口", port),
    el("label", { class: "row" }, [createRoot, el("span", { text: "同时创建站点目录与占位首页" })]),
    el("div", { class: "hint", text: "站点根目录会落在 " + (meta.root || "/var/www") + " 之下；证书申请前请先让域名解析到本机。" }),
    el("div", { class: "msg err" }),
  ];
  const submit = el("button", { text: "创建并下发" });
  const m = modal("新建站点", body.concat([el("div", { class: "actions" }, [submit])]));
  const errBox = body[body.length - 1];

  submit.addEventListener("click", async () => {
    submit.disabled = true;
    try {
      await api("POST", "/api/v1/sites", {
        name: name.value.trim(), domain: domain.value.trim(), kind: kind.value,
        php_version: php.value.trim(), port: parseInt(port.value, 10) || 80,
        create_root: createRoot.checked,
      });
      m.close();
      reload();
    } catch (e) {
      errBox.textContent = e.message;
    } finally { submit.disabled = false; }
  });
}

// ---------- 软件商店 ----------

// 软件 logo：品牌色圆角瓷片 + 白色线性图形（24 网格，统一 1.9 描边）。
// 不引外链图片：面板经常部署在没外网 DNS 的机器上，图片挂了整列都会变成碎图标。
const APP_LOGOS = {
  nginx: { bg: "#009639", d: "M6 18.5V5.5l12 13V5.5" },
  docker: {
    bg: "#2496ED",
    d: "M4 15h16c0 2.8-2.4 5-5.6 5H9.6C6.4 20 4 17.8 4 15zM6.5 11.8h3v3h-3zM10.5 11.8h3v3h-3zM14.5 11.8h3v3h-3z",
  },
  redis: { bg: "#D63A2F", d: "M4 8.5 12 4.5l8 4-8 4zM4 12.2l8 4 8-4M4 15.9l8 4 8-4" },
  mariadb: {
    bg: "#003545",
    d: "M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3m0 0v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3",
  },
  php: { bg: "#777BB4", d: "M8.5 7.5 4.5 12l4 4.5M15.5 7.5l4 4.5-4 4.5M13 5.5 11 18.5" },
  git: {
    bg: "#F05032",
    d: "M7 6.7v10.6M8.7 14.5h4.3a4 4 0 0 0 4-4V8.7M5.3 5a1.7 1.7 0 1 0 3.4 0 1.7 1.7 0 1 0-3.4 0M5.3 17a1.7 1.7 0 1 0 3.4 0 1.7 1.7 0 1 0-3.4 0M15.3 7a1.7 1.7 0 1 0 3.4 0 1.7 1.7 0 1 0-3.4 0",
  },
  htop: { bg: "#0F766E", d: "M5 18V10M10 18V5M15 18v-5M20 18V8M4 20.5h16" },
  curl: { bg: "#1B4B66", d: "M12 4v12M8 12l4 4 4-4M4 20h16" },
  certbot: {
    bg: "#2C6ECB",
    d: "M12 4l7 2.6v5c0 4.3-2.9 7.2-7 8.3-4.1-1.1-7-4-7-8.3v-5zM9 11.5l2.2 2.2 4.3-4.3",
  },
  unzip: { bg: "#C2801A", d: "M5 4.5h14v15H5zM12 4.5v15M12 7.5h2M12 10.5h-2M12 13.5h2M12 16.5h-2" },
  xray: { bg: "#5B4BE0", d: "M5 5l14 14M19 5 5 19" },
  sqlite3: { bg: "#0F4C81", d: "M5 19c6.5 1 12.5-4 14-14-8.5 0-13.5 5-14 14zM5 19l7.5-7.5" },
};

function appLogo(slug, size) {
  const s = size || 36;
  const meta = APP_LOGOS[slug] || { bg: "#64748b", d: "M7 7h10v10H7z" };
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 36 36");
  svg.setAttribute("width", s);
  svg.setAttribute("height", s);
  svg.setAttribute("class", "app-logo");
  svg.setAttribute("aria-hidden", "true");

  const rect = document.createElementNS(ns, "rect");
  rect.setAttribute("width", "36");
  rect.setAttribute("height", "36");
  rect.setAttribute("rx", "10");
  rect.setAttribute("fill", meta.bg);

  const g = document.createElementNS(ns, "g");
  g.setAttribute("transform", "translate(6 6)");
  g.setAttribute("fill", "none");
  g.setAttribute("stroke", "#fff");
  g.setAttribute("stroke-width", "1.9");
  g.setAttribute("stroke-linecap", "round");
  g.setAttribute("stroke-linejoin", "round");
  g.setAttribute("opacity", ".96");
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", meta.d);
  g.appendChild(p);

  svg.appendChild(rect);
  svg.appendChild(g);
  return svg;
}

function renderStoreView(root) {
  const search = el("input", { placeholder: "搜索软件…", style: "max-width:240px" });
  const cat = select([["", "全部类别"]], "");
  const viewSel = select([["list", "列表"], ["card", "卡片"]], localStorage.getItem("pv-store-view") || "list");
  cat.style.maxWidth = "160px";
  viewSel.style.maxWidth = "110px";
  const grid = el("div");
  const listBox = el("div", { class: "table-wrap freeze" });
  const msg = el("div", { class: "msg" });
  let all = [];
  let supported = true;

  const matches = () => {
    const q = search.value.trim().toLowerCase();
    const c = cat.value;
    return all.filter((m) =>
      (!c || m.category === c) &&
      (!q || (m.name + " " + m.slug + " " + m.summary).toLowerCase().includes(q)));
  };

  const paint = () => {
    const items = matches();
    // 已安装永远排在最上面：用户回商店十次有九次是来看「我装了什么 / 能不能卸」，
    // 混在一起按 slug 排序会让已安装项散落在中间。
    const installed = items.filter((m) => m.installed);
    const rest = items.filter((m) => !m.installed);
    const listMode = viewSel.value === "list";
    grid.style.display = listMode ? "none" : "";
    listBox.style.display = listMode ? "" : "none";
    localStorage.setItem("pv-store-view", viewSel.value);
    if (!items.length) {
      grid.replaceChildren();
      listBox.replaceChildren(emptyHint("没有匹配的软件"));
      return;
    }
    if (listMode) {
      listBox.replaceChildren(storeTable(installed, rest, supported, () => load()));
    } else {
      grid.replaceChildren.apply(grid, [
        storeGroup("已安装", installed, "ok", supported, () => load()),
        storeGroup("可安装", rest, "muted", supported, () => load()),
      ].filter(Boolean));
    }
  };

  const load = async () => {
    try {
      const d = await api("GET", "/api/v1/store");
      all = d.items || [];
      supported = d.supported;
      if (!supported) {
        msg.className = "msg err";
        msg.textContent = d.reason;
      }
      const cats = Array.from(new Set(all.map((m) => m.category)));
      cat.replaceChildren(el("option", { value: "", text: "全部类别" }),
        ...cats.map((k) => el("option", { value: k, text: k })));
      paint();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  search.addEventListener("input", paint);
  cat.addEventListener("change", paint);
  viewSel.addEventListener("change", paint);

  root.replaceChildren(card("软件商店", [
    el("div", { class: "hint", text: "清单是声明式的（apt / 下载 / 解压 / 写文件 / 路径 / 服务 / 校验），不含自由脚本。安装会真实调用 apt 或下载二进制，可能需要几分钟。" }),
    el("div", { class: "row wrap toolbar" }, [search, cat, viewSel,
      el("div", { class: "spacer" }),
      el("button", { class: "ghost", text: "刷新", onclick: load })]),
    msg, listBox, grid,
  ]));
  load();
}

// 卡片视图的分组：标题 + 数量，下面接一组卡片。
function storeGroup(label, items, kind, supported, reload) {
  if (!items.length) return null;
  return el("div", { class: "store-group" }, [
    el("div", { class: "store-group-head" }, [
      el("h2", { text: label }),
      badge(String(items.length), kind),
      el("div", { class: "sep" }),
    ]),
    el("div", { class: "grid g2" }, items.map((m) => storeCard(m, supported, reload))),
  ]);
}

// storeTable 用表格展示，适合一眼扫过全部软件与安装状态。
// 已安装 / 未安装用一条组内分隔行分开，表头只有一份且冻结在容器顶部。
function storeTable(installed, rest, supported, reload) {
  const head = el("tr", {}, ["软件", "类别", "说明", "内存", "磁盘", "状态", "操作"]
    .map((h) => el("th", { text: h })));
  const body = el("tbody");

  const group = (label, items, kind) => {
    if (!items.length) return;
    body.appendChild(el("tr", { class: "store-group-row" }, [
      el("td", { colspan: "7" }, [
        el("span", { class: "store-group-name", text: label }),
        badge(String(items.length), kind),
      ]),
    ]));
    items.forEach((m) => body.appendChild(storeRow(m, supported, reload)));
  };
  group("已安装", installed, "ok");
  group("未安装", rest, "muted");

  return el("table", { class: "store-table" }, [el("thead", {}, [head]), body]);
}

function storeRow(m, supported, reload) {
  const acts = storeActions(m, supported, reload);
  return el("tr", {}, [
    el("td", {}, [el("div", { class: "app-cell" }, [
      appLogo(m.slug),
      el("div", { class: "app-text" }, [
        el("div", { class: "app-name", text: m.name }),
        el("div", { class: "mono app-slug", text: m.slug + " · " + m.version }),
      ]),
    ])]),
    el("td", { text: m.category || "-" }),
    el("td", { class: "sub", text: m.summary }),
    el("td", { class: "mono", text: m.memory_mb + "MB" }),
    el("td", { class: "mono", text: m.disk_mb + "MB" }),
    el("td", {}, [
      m.installed
        ? badge(m.installed_version ? "已装 " + m.installed_version : "已安装", "ok")
        : badge(riskText[m.risk] || m.risk, m.risk === "high" ? "err" : m.risk === "medium" ? "warn" : "muted"),
    ]),
    el("td", {}, [el("div", { class: "row wrap" }, acts)]),
  ]);
}

const riskText = { low: "低风险", medium: "中风险", high: "高风险" };

function storeActions(m, supported, reload) {
  const acts = [
    el("button", { class: "ghost", text: "查看步骤", onclick: async () => {
      try {
        const d = await api("GET", "/api/v1/store/" + m.slug + "/plan?mode=install");
        const lines = (d.steps || []).map((s) => "• " + s).join("\n");
        modal(m.name + " · 安装步骤", [
          el("pre", { class: "log", text: lines || "（无）" }),
          el("div", { class: "hint", text: "这些步骤会以 root 身份真实执行；失败时自动尝试回滚。" }),
        ]);
      } catch (e) { alert(e.message); }
    } }),
  ];

  if (m.installed) {
    acts.push(el("button", { class: "ghost danger", text: "卸载", onclick: async () => {
      if (!confirm("卸载 " + m.name + "？会停止服务并 purge 相关软件包。")) return;
      try {
        const job = await api("POST", "/api/v1/store/" + m.slug + "/uninstall", {});
        watchJob(job, reload);
      } catch (e) { alert(e.message); }
    } }));
  } else if (supported) {
    acts.push(el("button", { text: "安装", onclick: async () => {
      if (m.risk === "high" && !confirm("该软件标记为高风险：" + (m.warning || "") + "\n\n仍要继续？")) return;
      else if (m.risk !== "high" && !confirm("安装 " + m.name + "？预计占用内存 " + m.memory_mb + "MB。")) return;
      try {
        const job = await api("POST", "/api/v1/store/" + m.slug + "/install", { force: false });
        watchJob(job, reload);
      } catch (e) { alert(e.message); }
    } }));
  } else {
    // 不支持时不是一个「动作」，用标签而不是按钮：整列 12 个同样的靛蓝按钮
    // 会把页面压成一堵色墙，也让真正可点的「安装」失去重点。
    acts.push(el("span", { class: "badge muted", text: "仅 Linux 可安装" }));
  }
  return acts;
}

function storeCard(m, supported, reload) {
  const acts = el("div", { class: "row wrap" }, storeActions(m, supported, reload));

  return el("div", { class: "card store-card" }, [
    el("div", { class: "row spread" }, [
      el("div", { class: "app-cell" }, [
        appLogo(m.slug, 40),
        el("div", { class: "app-text" }, [
          el("h3", { text: m.name }),
          el("div", { class: "hint mono", text: m.slug + " · " + m.version }),
        ]),
      ]),
      m.installed ? badge("已安装", "ok") : badge(riskText[m.risk] || m.risk, m.risk === "high" ? "err" : m.risk === "medium" ? "warn" : "muted"),
    ]),
    el("p", { class: "sub", text: m.summary }),
    el("div", { class: "row wrap", style: "margin:8px 0" }, [
      badge("内存 " + m.memory_mb + "MB", "muted"),
      badge("磁盘 " + m.disk_mb + "MB", "muted"),
      m.ports && m.ports.length ? badge("端口 " + m.ports.join("/"), "muted") : null,
      m.installed && m.installed_version ? badge("版本 " + m.installed_version, "muted") : null,
    ]),
    m.warning ? el("div", { class: "warn-box", text: m.warning }) : null,
    acts,
  ]);
}

// ---------- 备份 ----------

function renderBackupView(root) {
  const listBox = el("div");
  const msg = el("div", { class: "msg" });
  const note = el("input", { placeholder: "备注（可选）", style: "max-width:280px" });

  const load = async () => {
    try {
      const d = await api("GET", "/api/v1/backups");
      const rows = (d.items || []).map((f) => el("tr", {}, [
        el("td", { class: "mono", text: f.name }),
        el("td", { class: "mono", text: fmtBytes(f.size) }),
        el("td", { class: "mono", text: fmtTime(f.mod_time) }),
        el("td", {}, [el("div", { class: "row wrap" }, [
          el("button", { class: "link", text: "下载", onclick: () => {
            window.location.href = "/api/v1/backups/" + encodeURIComponent(f.name) + "/download";
          } }),
          el("button", { class: "link", text: "恢复", onclick: async () => {
            if (!confirm("恢复会用备份中的数据库覆盖当前数据，并重启面板。\n当前状态会先自动备份一份。\n\n确定恢复 " + f.name + "？")) return;
            try {
              const r = await api("POST", "/api/v1/backups/" + encodeURIComponent(f.name) + "/restore", { confirm: "RESTORE" });
              msg.className = "msg ok";
              msg.textContent = r.message || "已开始恢复";
              if (r.restart_required) setTimeout(() => location.reload(), 8000);
            } catch (e) { alert(e.message); }
          } }),
          el("button", { class: "link danger", text: "删除", onclick: async () => {
            if (!confirm("删除备份 " + f.name + "？")) return;
            try { await api("DELETE", "/api/v1/backups/" + encodeURIComponent(f.name), {}); load(); }
            catch (e) { alert(e.message); }
          } }),
        ])]),
      ]));
      listBox.replaceChildren(
        rows.length ? simpleTable(["文件", "大小", "时间", "操作"], rows) : emptyHint("还没有备份"),
        el("div", { class: "hint", text: "备份目录：" + (d.dir || "-") + "，自动保留最近 " + (d.keep || 30) + " 份" }),
      );
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  root.replaceChildren(card("备份与恢复", [
    el("div", { class: "row wrap toolbar" }, [
      note,
      el("button", { text: "立即备份", onclick: async () => {
        try {
          const job = await api("POST", "/api/v1/backups", { note: note.value.trim() });
          note.value = "";
          watchJob(job, load);
        } catch (e) { alert(e.message); }
      } }),
      el("div", { class: "spacer" }),
      el("button", { class: "ghost", text: "刷新", onclick: load }),
    ]),
    el("div", { class: "hint", text: "备份包含数据库一致性快照（VACUUM INTO）、配置文件与版本信息；不包含网站文件与 Xray 二进制。" }),
    msg, listBox,
  ]));
  load();
}

// ---------- 设置 ----------

function renderSettingsView(root) {
  const msg = el("div", { class: "msg" });
  const host = el("input", { placeholder: "服务器公网 IP 或域名" });
  const webhook = el("input", { placeholder: "https://example.com/webhook" });
  const guard = el("input", { type: "checkbox" });
  const threshold = el("input", { placeholder: "5", style: "max-width:100px" });
  const terminalChk = el("input", { type: "checkbox" });
  const info = el("tbody");
  const updateBox = el("div");

  // 访问方式（原「控制」页，收进设置）
  const accessPort = el("input", { type: "number", min: "1", max: "65535", style: "max-width:130px" });
  const accessPublic = el("input", { type: "checkbox" });
  const accessInfo = el("div");
  const accessMsg = el("div", { class: "msg" });

  // 账号
  const accountBox = el("div");

  const paintAccess = (a) => {
    accessPort.value = a.port ? String(a.port) : "";
    accessPublic.checked = !!a.public;
    const kids = [
      el("div", { class: "row wrap" }, [
        badge(a.public ? "外网可访问" : "仅本机", a.public ? "warn" : "ok"),
        el("span", { class: "mono", text: a.listen || "-" }),
      ]),
    ];
    (a.urls || []).forEach((u) => kids.push(el("div", { style: "margin-top:6px" }, [
      el("a", { class: "mono", href: u, target: "_blank", rel: "noreferrer", text: u }),
    ])));
    if (!(a.urls || []).length && a.public) {
      kids.push(el("div", { class: "hint", text: "没有探测到可用地址，请用服务器公网 IP:" + a.port }));
    }
    if ((a.local_ips || []).length) {
      kids.push(el("div", { class: "hint", text: "本机网卡地址：" + a.local_ips.join("、") }));
    }
    kids.push(a.public
      ? el("div", { class: "banner", style: "margin-top:12px", text: "面板对外开放，而且只提供 HTTP。请使用强密码，并确认云厂商安全组放行了 " + a.port + "/tcp。" })
      : el("div", { class: "hint", text: "只监听 127.0.0.1，需要通过 SSH 隧道访问。" }));
    accessInfo.replaceChildren.apply(accessInfo, kids);
  };

  const saveAccess = async () => {
    const port = parseInt(accessPort.value, 10);
    if (!port || port < 1 || port > 65535) {
      accessMsg.className = "msg err"; accessMsg.textContent = "端口应在 1–65535 之间"; return;
    }
    const pub = accessPublic.checked;
    if (!confirm("面板会重启，当前页面断开约 3 秒。" + (pub ? "" : "\n改为仅本机后，只能通过 SSH 隧道访问。"))) return;
    accessMsg.className = "msg"; accessMsg.textContent = "保存中…";
    try {
      const r = await api("PUT", "/api/v1/control/access", { port, public: pub, confirm: "ACCESS" });
      const extra = (r.warnings || []).length ? " " + r.warnings.join("；") : "";
      accessMsg.className = "msg ok";
      accessMsg.textContent = "已保存" + (r.restarting ? "，面板重启中…" : "") + extra;
      if (!r.restarting) return;
      const h = location.hostname;
      if (r.public && location.protocol === "http:" && h !== "127.0.0.1" && h !== "localhost" && h !== "::1") {
        setTimeout(() => { location.href = location.protocol + "//" + h + ":" + r.port + "/"; }, 4500);
      } else if (!r.public) {
        accessMsg.textContent = "已保存，服务重启后请通过 SSH 隧道访问 127.0.0.1:" + r.port;
      }
    } catch (e) {
      accessMsg.className = "msg err"; accessMsg.textContent = e.message;
    }
  };

  const showPasswordModal = (pwd) => {
    const input = el("input", { readonly: true, class: "mono" });
    input.value = pwd;
    const m = modal("新密码（只显示一次）", [
      el("div", { class: "hint", text: "请立即保存。关闭后无法再次查看，只能重新重置。" }),
      input,
      el("div", { class: "actions" }, [
        el("button", { text: "复制", onclick: async () => {
          try { await navigator.clipboard.writeText(pwd); }
          catch (_) { input.select(); document.execCommand("copy"); }
        } }),
        el("button", { class: "ghost", text: "我已保存，去登录", onclick: () => m.close() }),
      ]),
    ], () => {
      state.user = null;
      renderLogin("密码已重置，请用新密码登录");
    });
  };

  const resetRandom = async () => {
    if (!confirm("生成新的随机密码，并踢掉所有已登录会话？")) return;
    try {
      const r = await api("POST", "/api/v1/control/password", { mode: "random", confirm: "RESET" });
      showPasswordModal(r.password);
    } catch (e) { alert(e.message); }
  };

  const paintAccount = (a) => {
    accountBox.replaceChildren(
      el("div", { class: "row wrap" }, [
        badge(a.username || "admin", "plain"),
        el("span", { class: "hint", text: a.last_login_at
          ? "上次登录 " + fmtTime(a.last_login_at) + (a.last_login_ip ? " · " + a.last_login_ip : "")
          : "还没有登录记录" }),
      ]),
      el("div", { class: "actions" }, [
        el("button", { class: "ghost", text: "修改密码（需旧密码）", onclick: () => renderChangePassword(false) }),
        el("button", { class: "ghost danger", text: "重置为随机密码", onclick: resetRandom }),
      ]),
      el("div", { class: "hint", text: "重置会立即踢掉所有会话，新密码只显示一次。" }),
    );
  };

  const load = async () => {
    try {
      const d = await api("GET", "/api/v1/settings");
      host.value = d.public_host || "";
      webhook.value = d.alert_webhook || "";
      guard.checked = !!(d.ssh_guard && d.ssh_guard.enabled);
      threshold.value = d.ssh_guard ? d.ssh_guard.threshold : 5;
      paintAccess(d.access || {});
      paintAccount(d.account || {});
      const p = d.panel || {};
      terminalChk.checked = !!p.terminal_enabled;
      const rows = [
        ["面板版本", p.version], ["监听地址", p.listen + (p.loopback ? "（仅本机）" : "（对外）")],
        ["数据目录", p.data_dir], ["数据库", p.db_path],
        ["文件根目录", p.file_root], ["站点根目录", p.sites_root],
        ["Nginx 配置", p.nginx_conf], ["备份目录", p.backup_dir],
        ["证书邮箱", p.cert_email || "未设置"], ["Web 终端", p.terminal_enabled ? "已启用" : "已关闭"],
        ["更新仓库", p.update_repo], ["配置文件", p.config_path || "-"],
      ];
      info.replaceChildren.apply(info, rows.map(([k, v]) =>
        el("tr", {}, [el("th", { text: k }), el("td", { class: "mono", text: String(v || "-") })])));
      paintUpdate(d.update);
      loadUnit();
      if (FE_VERSION && p.version && p.version !== FE_VERSION) {
        const stale = el("div", { class: "msg err", style: "margin-bottom:12px" }, [
          el("span", { text: "当前页面用的是旧版前端（" + FE_VERSION + "），服务端已是 " + p.version + "，部分设置项可能显示不全。" }),
          el("button", { class: "ghost", text: "刷新页面", style: "margin-left:10px", onclick: () => location.reload() }),
        ]);
        root.insertBefore(stale, root.firstChild);
      }
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  };

  const paintUpdate = (u) => {
    if (!u) {
      updateBox.replaceChildren(el("div", { class: "hint", text: "尚未检查更新" }));
      return;
    }
    const body = [
      el("div", { class: "row wrap" }, [
        badge("当前 " + u.current, "muted"),
        u.available ? badge("可更新到 " + u.latest, "ok") : badge("已是最新", "ok"),
        el("span", { class: "hint", text: "检查于 " + fmtTime(u.checked_at) }),
      ]),
    ];
    if (u.notes) body.push(el("pre", { class: "log", text: u.notes }));
    if (u.available) {
      body.push(el("div", { class: "actions" }, [
        el("button", { text: "下载并替换二进制", onclick: async () => {
          if (!confirm("将下载 " + u.asset_name + "（" + fmtBytes(u.asset_size) + "）并替换当前二进制。\n替换后需要重启面板才能生效。")) return;
          try {
            const job = await api("POST", "/api/v1/updates/apply", { tag: u.latest });
            watchJob(job, null);
          } catch (e) { alert(e.message); }
        } }),
      ]));
    }
    updateBox.replaceChildren.apply(updateBox, body);
  };

  // 服务单元自检：设置页的「版本更新」只替换二进制，不换 /etc/systemd 下的单元。
  // 旧单元里的沙箱策略会挡住新功能（v0.3.5 之前装 Xray 报 read-only 就是这个）。
  const unitBox = el("div", { class: "hint", text: "检查中…" });
  const loadUnit = async () => {
    let u;
    try {
      u = await api("GET", "/api/v1/system/unit");
    } catch (e) {
      unitBox.replaceChildren(el("div", { class: "hint", text: "无法读取服务单元状态：" + e.message }));
      return;
    }
    if (!u.present) {
      unitBox.replaceChildren(el("div", { class: "hint", text: u.reason || "未找到服务单元" }));
      return;
    }
    if (u.match) {
      unitBox.replaceChildren(el("div", { class: "row wrap" }, [
        badge("与服务版本一致", "ok"),
        el("span", { class: "hint mono", text: u.path }),
      ]));
      return;
    }
    unitBox.replaceChildren(
      el("div", { class: "row wrap" }, [
        badge("需要更新", "warn"),
        el("span", { class: "hint mono", text: u.path }),
      ]),
      el("div", { class: "warn-box", style: "margin-top:10px", text: "磁盘上的单元是旧版本：升级只换了二进制，没换 systemd 单元。旧单元的沙箱策略会挡住新功能，比如软件商店装 Xray 报 read-only file system。" }),
      el("div", { class: "actions" }, [
        el("button", { text: "用内置版本覆盖", onclick: async () => {
          if (!confirm("将用当前版本内置的单元覆盖：\n" + u.path + "\n\n旧文件会先备份为 .bak-时间戳，然后执行 systemd daemon-reload。覆盖后需要重启面板才生效。")) return;
          try {
            const r = await api("POST", "/api/v1/system/unit/sync", {});
            unitBox.replaceChildren(el("div", { class: "msg ok", text: (r.backup ? "已更新，旧单元备份在 " + r.backup + "。" : "已更新。") + "现在点下面的「重启面板」让它生效。" }));
          } catch (e) { alert(e.message); }
        } }),
      ]),
    );
  };

  const save = el("button", { text: "保存设置", onclick: async () => {
    msg.className = "msg";
    msg.textContent = "保存中…";
    try {
      await api("PUT", "/api/v1/settings", {
        public_host: host.value.trim(),
        alert_webhook: webhook.value.trim(),
        ssh_guard_enabled: guard.checked,
        ssh_guard_threshold: parseInt(threshold.value, 10) || 5,
        terminal_enabled: terminalChk.checked,
      });
      msg.className = "msg ok";
      msg.textContent = "已保存";
      load();
    } catch (e) {
      msg.className = "msg err";
      msg.textContent = e.message;
    }
  } });

  const restart = el("button", { class: "ghost danger", text: "重启面板", onclick: async () => {
    if (!confirm("重启 panelvps 服务？网站与代理不会中断，面板会断开约 3 秒。")) return;
    try {
      await api("POST", "/api/v1/system/restart", { confirm: "RESTART" });
      msg.className = "msg ok";
      msg.textContent = "正在重启，5 秒后自动刷新…";
      setTimeout(() => location.reload(), 5000);
    } catch (e) { alert(e.message); }
  } });

  root.replaceChildren(
    card("访问方式", [
      accessInfo,
      el("label", { class: "row", style: "margin-top:12px" }, [
        accessPublic, el("span", { text: "允许通过 IP + 端口访问" }),
      ]),
      el("label", { class: "row", style: "margin-top:8px" }, [
        el("span", { text: "端口" }), accessPort,
        el("span", { class: "hint", text: "1–65535，保存后面板自动重启" }),
      ]),
      el("div", { class: "actions" }, [
        el("button", { text: "保存并重启面板", onclick: saveAccess }),
        el("span", { class: "hint", text: "保存后服务端会重启，页面约 3 秒后自动重连" }),
      ]),
      accessMsg,
    ]),
    card("账号", [accountBox]),
    card("基本设置", [
      field("公网地址", host, "用于生成分享链接；可填 IP、域名，NAT 机器可写 IP:端口"),
      field("告警 Webhook", webhook, "留空则关闭。事件以 JSON POST 推送，5 分钟内相同标题只发一次"),
      el("label", { class: "row" }, [guard, el("span", { text: "启用 SSH 爆破自动封禁" })]),
      field("失败次数阈值", threshold, "窗口 10 分钟（配置项 ssh_guard_window）"),
      el("label", { class: "row", style: "margin-top:8px" }, [terminalChk, el("span", { text: "启用 Web 终端" })]),
      el("div", { class: "hint", text: "打开后立即生效，不需要重启；写入配置文件 terminal_enabled。终端以 root 身份执行命令，请确保面板本身足够安全。" }),
      el("div", { class: "actions" }, [save, restart]),
      msg,
    ]),
    card("运行参数", [el("table", {}, [info])]),
    card("服务单元", [unitBox]),
    card("版本更新", [
      el("div", { class: "actions", style: "margin-top:0" }, [
        el("button", { text: "检查更新", onclick: async () => {
          updateBox.replaceChildren(el("div", { class: "hint", text: "检查中…" }));
          try { paintUpdate(await api("POST", "/api/v1/updates/check", {})); }
          catch (e) { updateBox.replaceChildren(el("div", { class: "msg err", text: e.message })); }
        } }),
        el("span", { class: "hint", text: "从 GitHub Release 下载，校验 sha256 后原子替换，保留上一版" }),
      ]),
      updateBox,
    ]),
  );
  load();
}

// ---------- 任务进度 ----------

function watchJob(job, onDone) {
  const logBox = el("pre", { class: "log tall", text: "排队中…" });
  const m = modal("执行中 · " + (job.target || job.kind), [logBox]);
  const timer = setInterval(async () => {
    try {
      const j = await api("GET", "/api/v1/system/jobs/" + job.id);
      logBox.textContent = j.log || "…";
      logBox.scrollTop = logBox.scrollHeight;
      if (j.status !== "running") {
        clearInterval(timer);
        const h = m.box.querySelector("h3");
        if (h) h.textContent = j.status === "ok" ? "完成" : "失败";
        if (j.error) logBox.textContent += "\n\n错误：" + j.error;
        if (onDone) onDone(j);
      }
    } catch (e) {
      clearInterval(timer);
      if (e.status === 401) renderLogin("会话已失效，请重新登录");
    }
  }, 1200);
  return m;
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
    app().className = "login-wrap";
    app().replaceChildren(el("div", { class: "card login-card" }, [
      el("h1", { text: "无法连接面板" }),
      el("p", { class: "sub", text: err.message }),
    ]));
  }
}

boot();
