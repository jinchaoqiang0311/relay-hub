// SPDX-License-Identifier: AGPL-3.0-only
"use strict";

const $ = (id) => document.getElementById(id);
const QUOTA_PER_UNIT_DEFAULT = 500000;

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function fmtUSD(quota, per) {
  const usd = quota / (per || QUOTA_PER_UNIT_DEFAULT);
  const abs = Math.abs(usd);
  const digits = abs >= 100 ? 2 : abs >= 1 ? 3 : 4;
  return "$" + usd.toFixed(digits);
}

let toastTimer = null;

// 令牌揭示状态（按站点 ID），仅影响当前弹窗显示
const revealedToks = new Set();
let currentSites = [];
function toast(msg, isError = false) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.toggle("is-error", isError);
  el.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), 2600);
}

function relTime(ts) {
  const mins = Math.max(0, Math.round((Date.now() - ts) / 60000));
  if (mins < 1) return "刚刚";
  if (mins < 60) return `${mins} 分钟前`;
  const h = Math.round(mins / 60);
  if (h < 24) return `${h} 小时前`;
  return `${Math.round(h / 24)} 天前`;
}

async function loadSites() {
  const { sites = [], history = {}, lastOk = {}, settings = {} } =
    await chrome.storage.local.get(["sites", "history", "lastOk", "settings"]);
  currentSites = sites;
  render(sites, history, lastOk, settings.quotaPerUnit);
}

function render(sites, history, lastOk, perUnit) {
  const per = perUnit || QUOTA_PER_UNIT_DEFAULT;
  let totQuota = 0, totAff = 0;
  for (const s of sites) {
    const arr = history[s.id] || [];
    const last = arr[arr.length - 1];
    if (last) { totQuota += last.quota; totAff += last.aff; }
  }
  $("totals").innerHTML = `
    <div class="t"><div class="t-label">余额合计</div><div class="t-val">${fmtUSD(totQuota, per)}</div></div>
    <div class="t"><div class="t-label">邀请奖励</div><div class="t-val">${fmtUSD(totAff, per)}</div></div>`;

  $("empty").classList.toggle("hidden", sites.length > 0);
  $("sites").innerHTML = sites.map((s) => siteHTML(s, history[s.id] || [], lastOk[s.id], per)).join("");
}

function siteHTML(s, arr, ok, per) {
  const last = arr[arr.length - 1];
  const status = s.type === "manual"
    ? '<span class="dot dot-ok"></span>手动'
    : !ok
      ? '<span class="dot dot-idle"></span>待同步'
      : ok.ok
        ? '<span class="dot dot-ok"></span>正常'
        : '<span class="dot dot-err"></span>异常';
  const link = s.baseUrl
    ? `<a href="${esc(s.baseUrl)}" target="_blank" rel="noreferrer">${esc(s.name)}</a>`
    : esc(s.name);
  return `
  <article class="site" data-id="${s.id}">
    <div class="site-head">
      <span class="site-name">${link}${s.type === "manual" ? ' <span class="status">· 手动</span>' : ""}</span>
      <span class="status${ok && !ok.ok ? " bad" : ""}">${status}</span>
    </div>
    <div class="site-nums">
      <div class="n"><div class="n-label">可用余额</div><div class="n-val main">${last ? fmtUSD(last.quota, per) : "—"}</div></div>
      <div class="n"><div class="n-label">邀请奖励</div><div class="n-val">${last ? fmtUSD(last.aff, per) : "—"}</div></div>
      <div class="n"><div class="n-label">累计收益</div><div class="n-val">${last ? fmtUSD(last.hist, per) : "—"}</div></div>
    </div>
    ${ok && !ok.ok && ok.error ? `<div class="err-text">${esc(ok.error)}</div>` : ""}
    ${revealedToks.has(s.id) ? `<div class="tok-plain" title="点「令牌」可隐藏">${esc(s.token)}</div>` : ""}
    <div class="site-foot">
      <span class="muted">${last ? relTime(last.t) : "尚无数据"}</span>
      <span>
        ${s.token ? `<button class="btn" data-act="tok">${revealedToks.has(s.id) ? "隐藏" : "令牌"}</button><button class="btn" data-act="copy-tok">复制令牌</button>` : ""}
        <button class="btn btn-danger" data-act="del">删除</button>
      </span>
    </div>
  </article>`;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

$("sites").addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-act]");
  if (!btn) return;
  const id = Number(btn.closest(".site").dataset.id);
  const site = currentSites.find((x) => x.id === id);
  if (!site) return;
  const act = btn.dataset.act;

  if (act === "tok") {
    if (revealedToks.has(id)) revealedToks.delete(id);
    else revealedToks.add(id);
    loadSites();
    return;
  }
  if (act === "copy-tok") {
    if (await copyText(site.token)) toast("令牌已复制");
    else toast("复制失败", true);
    return;
  }
  if (act === "del") {
    if (!confirm("确认删除该站点及其本地历史？")) return;
    const { sites = [], history = {}, lastOk = {} } =
      await chrome.storage.local.get(["sites", "history", "lastOk"]);
    await chrome.storage.local.set({
      sites: sites.filter((s) => s.id !== id),
      history: { ...history, [id]: undefined },
      lastOk: { ...lastOk, [id]: undefined },
    });
    revealedToks.delete(id);
    loadSites();
  }
});

/* ---------- 当前标签页网址自动填充 ---------- */

// useCurrentTab 取当前活动标签页的网址（origin）填入站点地址。
// 依赖 activeTab 权限：用户点开插件图标即临时授权，无需常驻「读取浏览记录」权限。
async function useCurrentTab() {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.url) return false;
    const u = new URL(tab.url);
    if (!/^https?:$/.test(u.protocol)) return false;
    $("f-url").value = u.origin;
    return true;
  } catch {
    return false;
  }
}

$("btn-current-tab").addEventListener("click", async () => {
  const ok = await useCurrentTab();
  toast(ok ? "已填入当前标签页网址" : "当前标签页不是 http/https 网址", !ok);
});

/* ---------- 添加站点 ---------- */

$("btn-add").addEventListener("click", async () => {
  $("add-form").classList.remove("hidden");
  $("add-error").classList.add("hidden");
  const filled = await useCurrentTab(); // 打开即预填当前站点
  if (!filled) $("f-url").focus();
});
$("btn-cancel").addEventListener("click", () => $("add-form").classList.add("hidden"));
$("f-manual").addEventListener("change", () => {
  const manual = $("f-manual").checked;
  $("f-token").disabled = manual;
  $("f-uid").disabled = manual;
});

$("btn-save").addEventListener("click", async () => {
  const errEl = $("add-error");
  errEl.classList.add("hidden");
  const showErr = (m) => { errEl.textContent = m; errEl.classList.remove("hidden"); };

  let baseUrl = $("f-url").value.trim();
  const manual = $("f-manual").checked;
  const token = $("f-token").value.trim();
  const userId = Number($("f-uid").value);
  if (!baseUrl && !manual) return showErr("站点地址必填");
  if (!manual && (!token || !userId)) return showErr("令牌与数字用户 ID 必填");

  let origin = null;
  if (baseUrl) {
    if (!baseUrl.includes("://")) baseUrl = "https://" + baseUrl;
    let u;
    try { u = new URL(baseUrl); } catch { return showErr("站点地址无效"); }
    if (!/^https?:$/.test(u.protocol)) return showErr("仅支持 http/https");
    origin = u.origin + "/*";
    baseUrl = u.origin;
  }

  const site = {
    id: Date.now(),
    baseUrl,
    type: manual ? "manual" : "auto",
    token: manual ? "" : token,
    userId: manual ? 0 : userId,
    enabled: true,
  };
  try {
    site.name = baseUrl ? new URL(baseUrl).hostname : (prompt("站点备注名：") || "手动站点");
  } catch {
    site.name = "站点";
  }

  if (origin) {
    // 按站点申请 host 权限（用户手势内调用）
    const granted = await chrome.permissions.request({ origins: [origin] });
    if (!granted) return showErr("未授权访问该站点，无法同步");
  }

  const { sites = [] } = await chrome.storage.local.get("sites");
  sites.push(site);
  await chrome.storage.local.set({ sites });
  $("add-form").classList.add("hidden");
  $("f-url").value = ""; $("f-token").value = ""; $("f-uid").value = ""; $("f-manual").checked = false;
  $("f-token").disabled = false; $("f-uid").disabled = false;
  loadSites();
  if (!manual) {
    toast("已添加，正在同步…");
    chrome.runtime.sendMessage({ type: "syncNow" }, () => setTimeout(loadSites, 1500));
  } else {
    toast("已添加");
  }
});

/* ---------- 从登录态自动获取令牌 + 用户 ID ---------- */

// getJSON 调站点接口并解析统一信封；credentials 带上浏览器 Cookie（需已授权该站点）。
async function getJSON(url, opts = {}) {
  let res;
  try {
    res = await fetch(url, { credentials: "include", headers: { Accept: "application/json", ...opts.headers } });
  } catch {
    throw new Error("无法连接站点");
  }
  const env = await res.json().catch(() => null);
  if (!env) throw new Error(`响应不是 JSON（HTTP ${res.status}），该站可能不是 new-api/one-api 系`);
  return env;
}

function popupErr(msg) {
  const el = $("add-error");
  el.textContent = msg;
  el.classList.remove("hidden");
}

$("btn-auto").addEventListener("click", async () => {
  const errEl = $("add-error");
  errEl.classList.add("hidden");
  const showErr = (m) => { errEl.textContent = m; errEl.classList.remove("hidden"); };

  let raw = $("f-url").value.trim();
  if (!raw) return showErr("请先填站点地址");
  if (!raw.includes("://")) raw = "https://" + raw;
  let u;
  try { u = new URL(raw); } catch { return showErr("站点地址无效"); }
  if (!/^https?:$/.test(u.protocol)) return showErr("仅支持 http/https");
  const base = u.origin;

  if ($("f-manual").checked) return showErr("手动记录模式不接 API，无需获取令牌");

  const btn = $("btn-auto");
  btn.disabled = true;
  btn.textContent = "获取中…";
  try {
    // 按站点申请 Cookie 访问权限（用户手势内调用）
    const granted = await chrome.permissions.request({ origins: [u.origin + "/*"] });
    if (!granted) throw new Error("未授权访问该站点，无法读取登录态");

    // 1) 用登录态调 /api/user/self：拿数字 ID 与已生成的令牌
    const selfEnv = await getJSON(base + "/api/user/self");
    if (!selfEnv.success || !selfEnv.data || !selfEnv.data.id) {
      const msg = selfEnv.message || "";
      throw new Error(
        /无权|未登录|token/i.test(msg) || !selfEnv.success
          ? "未读取到登录态：请先在浏览器的标签页里登录该站（若刚登录，刷新一下站点页面），再点本按钮"
          : msg || "站点返回异常"
      );
    }
    const uid = selfEnv.data.id;
    let token = selfEnv.data.access_token || "";
    let regenerated = false;

    // 2) 从未生成过令牌才生成（复用现有令牌不会影响别处的使用）
    if (!token) {
      const tEnv = await getJSON(base + "/api/user/token", {
        headers: { "New-Api-User": String(uid), "Veloera-User": String(uid) },
      });
      if (!tEnv.success || !tEnv.data || typeof tEnv.data !== "string") {
        throw new Error("生成令牌失败：" + (tEnv.message || "站点未返回令牌"));
      }
      token = tEnv.data;
      regenerated = true;
    }

    $("f-token").value = token;
    $("f-uid").value = uid;
    errEl.classList.remove("hidden");
    errEl.textContent = regenerated
      ? "✅ 已自动生成令牌并获取用户 ID，点「保存」完成添加（该站旧令牌已失效）"
      : "✅ 已从登录态读取现有令牌与用户 ID，点「保存」完成添加";
  } catch (err) {
    showErr(err && err.message ? err.message : String(err));
  } finally {
    btn.disabled = false;
    btn.textContent = "🔑 从登录态自动获取令牌 + 用户 ID";
  }
});

/* ---------- 同步按钮 ---------- */

$("btn-sync").addEventListener("click", () => {
  const btn = $("btn-sync");
  btn.disabled = true;
  chrome.runtime.sendMessage({ type: "syncNow" }, (res) => {
    btn.disabled = false;
    if (res && res.ok) {
      toast("同步完成");
      loadSites();
    } else {
      toast((res && res.error) || "同步失败", true);
    }
  });
});

loadSites();
