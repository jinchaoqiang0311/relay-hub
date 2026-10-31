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
    <div class="site-foot">
      <span class="muted">${last ? relTime(last.t) : "尚无数据"}</span>
      <button class="btn btn-danger" data-act="del">删除</button>
    </div>
  </article>`;
}

$("sites").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act='del']");
  if (!btn) return;
  const id = Number(btn.closest(".site").dataset.id);
  if (!confirm("确认删除该站点及其本地历史？")) return;
  const { sites = [], history = {}, lastOk = {} } =
    await chrome.storage.local.get(["sites", "history", "lastOk"]);
  await chrome.storage.local.set({
    sites: sites.filter((s) => s.id !== id),
    history: { ...history, [id]: undefined },
    lastOk: { ...lastOk, [id]: undefined },
  });
  loadSites();
});

/* ---------- 添加站点 ---------- */

$("btn-add").addEventListener("click", () => {
  $("add-form").classList.remove("hidden");
  $("add-error").classList.add("hidden");
  $("f-url").focus();
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
