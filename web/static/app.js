"use strict";

// PanelVPS 前端（M0）
// 零构建：直接操作 DOM，不引入框架。M1 接入 Vite + Vue 后替换此文件。
// 安全约定：所有动态文本走 textContent，绝不拼接 innerHTML。

const state = {
  csrf: "",
  user: null,
  initialized: true,
  timer: null,
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
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k === "html") node.innerHTML = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v);
    }
  }
  (children || []).forEach((c) => node.appendChild(c));
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
    throw new Error(msg);
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
      if (data.must_change_password) {
        renderChangePassword(true);
      } else {
        renderDashboard();
      }
    } catch (err) {
      msg.className = "msg err";
      msg.textContent = err.message;
      btn.disabled = false;
    }
  });

  const card = el("div", { class: "card login-card" }, [
    el("h1", { text: "PanelVPS" }),
    el("p", { class: "sub", text: "轻量运维面板 · 请登录" }),
    form,
  ]);

  app().className = "";
  app().replaceChildren(card);
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
  ].filter(Boolean));

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (newPwd.value !== again.value) {
      msg.className = "msg err";
      msg.textContent = "两次输入的新密码不一致";
      return;
    }
    btn.disabled = true;
    try {
      await api("POST", "/api/v1/auth/password", {
        current: oldPwd.value,
        new: newPwd.value,
      });
      msg.className = "msg ok";
      msg.textContent = "已修改，正在进入…";
      setTimeout(renderDashboard, 600);
    } catch (err) {
      msg.className = "msg err";
      msg.textContent = err.message;
      btn.disabled = false;
    }
  });

  const card = el("div", { class: "card login-card" }, [
    el("h1", { text: forced ? "设置新密码" : "修改密码" }),
    form,
  ]);

  app().className = "";
  app().replaceChildren(card);
  setTimeout(() => oldPwd.focus(), 50);
}

// ---------- 面板 ----------

function renderDashboard() {
  const banner = el("div", { class: "banner", text: "" , style: "display:none" });
  const grid = el("div", { class: "grid" });
  const infoBody = el("tbody");
  const status = el("div", { class: "msg" });

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
    el("div", {}, [el("h1", { text: "概览" }),
      el("div", { class: "who", text: "登录账号：" + (state.user ? state.user.username : "-") })]),
    el("div", { class: "row" }, [changePwd, logout]),
  ]);

  const info = el("div", { class: "card section" }, [
    el("h2", { text: "面板信息" }),
    el("table", {}, [infoBody]),
  ]);

  app().className = "";
  app().replaceChildren(head, banner, grid, info, status);

  const refresh = async () => {
    try {
      const data = await api("GET", "/api/v1/system/stat");
      const s = data.metrics;
      const memPct = pct(s.mem_used, s.mem_total);
      const diskPct = pct(s.disk_used, s.disk_total);

      grid.replaceChildren(
        statCard("CPU", s.cpu_pct ? s.cpu_pct.toFixed(1) + "%" : "—",
          "负载 " + s.load1.toFixed(2) + " / " + s.load5.toFixed(2) + " / " + s.load15.toFixed(2),
          Math.round(s.cpu_pct)),
        statCard("内存", fmtBytes(s.mem_used), "共 " + fmtBytes(s.mem_total), memPct),
        statCard("磁盘", fmtBytes(s.disk_used), "共 " + fmtBytes(s.disk_total), diskPct),
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
      if (String(err.message).indexOf("未登录") >= 0 || String(err.message).indexOf("登录") >= 0) {
        renderLogin("会话已失效，请重新登录");
        return;
      }
      status.className = "msg err";
      status.textContent = err.message;
    }
  };

  refresh();
  if (state.timer) clearInterval(state.timer);
  state.timer = setInterval(refresh, 3000);
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
    renderDashboard();
  } catch (err) {
    app().className = "";
    app().replaceChildren(el("div", { class: "card login-card" }, [
      el("h1", { text: "无法连接面板" }),
      el("p", { class: "sub", text: err.message }),
    ]));
  }
}

boot();
