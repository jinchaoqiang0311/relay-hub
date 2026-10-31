// SPDX-License-Identifier: AGPL-3.0-only
"use strict";

const $ = (id) => document.getElementById(id);

const state = {
  quotaPerUnit: 500000,
  sites: [],
  syncing: false,
  editingId: null,
  mode: "manual",
  view: "dashboard",
  trend: null,
  withdrawals: [],
  editingWD: null,
  pollTimer: null,
  fastTimer: null,
};

/* ---------- API ---------- */

async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, {
      headers: opts.body ? { "Content-Type": "application/json" } : undefined,
      ...opts,
    });
  } catch {
    throw new Error("无法连接本机服务");
  }
  let data = null;
  try { data = await res.json(); } catch { /* 非 JSON 响应 */ }
  if (!res.ok || !data || data.ok === false) {
    const err = new Error((data && data.error) || `请求失败（HTTP ${res.status}）`);
    err.status = res.status;
    throw err;
  }
  return data.data;
}

/* ---------- helpers ---------- */

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function fmtUSD(quota, perUnit) {
  const per = perUnit || state.quotaPerUnit;
  const usd = quota / per;
  const abs = Math.abs(usd);
  const digits = abs >= 100 ? 2 : abs >= 1 ? 3 : 4;
  return "$" + usd.toFixed(digits);
}

function relTime(iso) {
  const t = new Date(iso).getTime();
  if (!t) return "";
  const mins = Math.max(0, Math.round((Date.now() - t) / 60000));
  if (mins < 1) return "刚刚";
  if (mins < 60) return `${mins} 分钟前`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.round(hours / 24)} 天前`;
}

function fmtDT(iso) {
  const d = new Date(iso);
  if (!d.getTime()) return "—";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

let toastTimer = null;
function toast(msg, isError = false, ms) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.toggle("is-error", isError);
  el.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), ms || (isError ? 4200 : 2600));
}

function showErr(id, msg) {
  const el = $(id);
  el.textContent = msg;
  el.classList.remove("hidden");
}
function hideErr(id) { $(id).classList.add("hidden"); }

/* ---------- 视图切换 ---------- */

function setView(view) {
  state.view = view;
  document.querySelectorAll(".view-btn").forEach((b) => {
    b.classList.toggle("active", b.dataset.view === view);
  });
  $("view-dashboard").classList.toggle("hidden", view !== "dashboard");
  $("view-ledger").classList.toggle("hidden", view !== "ledger");
  if (view === "ledger") loadLedger();
}

document.querySelectorAll(".view-btn").forEach((b) => {
  b.addEventListener("click", () => setView(b.dataset.view));
});

/* ---------- gate（初始化 / 解锁）---------- */

async function initGate() {
  const s = await api("/api/session");
  if (s.setup_done && s.unlocked) return enterApp();
  $("gate").classList.remove("hidden");
  if (s.setup_done) {
    $("gate-title").textContent = "解锁";
    $("gate-sub").textContent = "输入主密码解锁本机密码库，密钥只保存在进程内存，重启服务后需重新解锁。";
    $("gate-btn").textContent = "解锁";
    $("gate-pass").removeAttribute("minlength");
  }
}

$("gate-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const pass = $("gate-pass").value;
  if (!pass) return;
  hideErr("gate-error");
  $("gate-btn").disabled = true;
  try {
    const s = await api("/api/session");
    if (!s.setup_done) {
      if (pass.length < 8) throw new Error("主密码至少 8 位");
      await api("/api/setup", { method: "POST", body: JSON.stringify({ password: pass }) });
    } else {
      await api("/api/unlock", { method: "POST", body: JSON.stringify({ password: pass }) });
    }
    enterApp();
  } catch (err) {
    showErr("gate-error", err.message);
  } finally {
    $("gate-btn").disabled = false;
  }
});

function enterApp() {
  $("gate").classList.add("hidden");
  $("main").classList.remove("hidden");
  loadSites();
  clearInterval(state.pollTimer);
  state.pollTimer = setInterval(loadSites, 60000);
}

/* ---------- 数据加载与渲染 ---------- */

async function loadSites() {
  let d;
  try {
    d = await api("/api/sites");
  } catch (err) {
    if (err.status === 423) { location.reload(); return; }
    toast(err.message, true);
    return;
  }
  state.quotaPerUnit = d.quota_per_unit || 500000;
  state.sites = d.sites || [];
  state.syncing = !!d.syncing;
  render();
  loadTrend();
  if (state.syncing && !state.fastTimer) {
    state.fastTimer = setTimeout(() => { state.fastTimer = null; loadSites(); }, 3000);
  }
}

function statCard(label, valHTML) {
  return `<div class="stat"><div class="stat-label">${label}</div><div class="stat-val">${valHTML}</div></div>`;
}

function render() {
  const enabled = state.sites.filter((s) => s.enabled);
  const totals = enabled.reduce(
    (acc, s) => {
      if (s.latest) {
        acc.quota += s.latest.quota;
        acc.aff += s.latest.aff_quota;
        acc.hist += s.latest.aff_history;
      }
      return acc;
    },
    { quota: 0, aff: 0, hist: 0 }
  );
  const errCount = enabled.filter((s) => s.type !== "manual" && s.last_sync_at && !s.last_sync_ok).length;

  $("stats").innerHTML =
    statCard("可用余额合计", fmtUSD(totals.quota)) +
    statCard("邀请奖励合计", fmtUSD(totals.aff)) +
    statCard("累计推广收益", fmtUSD(totals.hist)) +
    statCard("站点", `${enabled.length} 个${errCount ? ` <span class="bad">· ${errCount} 个同步异常</span>` : ""}`);

  const hasSites = state.sites.length > 0;
  $("empty").classList.toggle("hidden", hasSites);
  $("sites").innerHTML = state.sites.map(cardHTML).join("");
}

function sparkSVG(spark) {
  if (!spark || spark.length < 2) return "";
  const W = 100, H = 24, P = 2;
  const vals = spark.map((p) => p.v);
  const min = Math.min(...vals);
  const max = Math.max(...vals);
  const span = max - min || 1;
  const pts = spark
    .map((p, i) =>
      `${(P + (i * (W - 2 * P)) / (spark.length - 1)).toFixed(1)},` +
      `${(H - P - ((vals[i] - min) * (H - 2 * P)) / span).toFixed(1)}`)
    .join(" ");
  return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">
    <polyline points="${pts}" fill="none" stroke="var(--accent)" stroke-width="1.5" vector-effect="non-scaling-stroke" opacity="0.7"></polyline>
  </svg>`;
}

function cardHTML(s) {
  const l = s.latest;
  const isManual = s.type === "manual";
  const status = isManual
    ? '<span class="dot dot-ok"></span>手动'
    : !s.last_sync_at
      ? '<span class="dot dot-idle"></span>未同步'
      : s.last_sync_ok
        ? '<span class="dot dot-ok"></span>正常'
        : '<span class="dot dot-err"></span>异常';

  const actions = isManual
    ? (s.withdraw_url ? '<button class="btn btn-sm" data-act="apply">申请提现</button>' : "") +
      '<button class="btn btn-sm" data-act="edit">更新数值</button>' +
      '<button class="btn btn-sm btn-danger" data-act="del">删除</button>'
    : (s.withdraw_url ? '<button class="btn btn-sm" data-act="apply">申请提现</button>' : "") +
      '<button class="btn btn-sm" data-act="sync">同步</button>' +
      (l && l.aff_quota > 0 ? '<button class="btn btn-sm" data-act="aff">转余额</button>' : "") +
      '<button class="btn btn-sm" data-act="edit">编辑</button>' +
      '<button class="btn btn-sm btn-danger" data-act="del">删除</button>';

  const footTime = isManual
    ? l ? "更新于 " + relTime(l.time) : "尚未记账"
    : s.last_sync_at ? "同步于 " + relTime(s.last_sync_at) : "尚未同步";

  return `
  <article class="card${!isManual && s.last_sync_at && !s.last_sync_ok ? " is-error" : ""}" data-id="${s.id}">
    <header class="card-head">
      <span class="card-name" title="${esc(s.base_url)}">${esc(s.name)}${isManual ? '<span class="badge-manual">手动</span>' : ""}</span>
      <span class="card-status${!isManual && s.last_sync_at && !s.last_sync_ok ? " bad" : ""}">${status}</span>
    </header>
    <div class="metric-hero">
      <span class="metric-label">可用余额</span>
      <span class="metric-val">${l ? fmtUSD(l.quota) : "—"}</span>
    </div>
    <div class="metric-row">
      <div class="metric"><span class="metric-label">邀请奖励</span><span class="metric-val">${l ? fmtUSD(l.aff_quota) : "—"}</span></div>
      <div class="metric"><span class="metric-label">累计收益</span><span class="metric-val">${l ? fmtUSD(l.aff_history) : "—"}</span></div>
      <div class="metric"><span class="metric-label">邀请人数</span><span class="metric-val">${l ? l.aff_count : "—"}</span></div>
    </div>
    ${sparkSVG(s.spark)}
    ${inviteRow(s)}
    ${s.last_error && !isManual ? `<div class="card-err">${s.down_count > 1 ? `已连续失败 ${s.down_count} 次：` : ""}${esc(s.last_error)}</div>` : ""}
    <footer class="card-foot">
      <span class="muted small">${footTime}</span>
      <span class="card-actions">${actions}</span>
    </footer>
  </article>`;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // 非 HTTPS/非 localhost 等不安全上下文的兜底
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

/* ---------- 邀请链接 ---------- */

function inviteLink(s) {
  if (!s.aff_code) return "";
  const base = (s.base_url || "").replace(/\/+$/, "");
  return base ? `${base}/register?aff=${encodeURIComponent(s.aff_code)}` : "";
}

function inviteRow(s) {
  const link = inviteLink(s);
  if (!link) {
    if (!s.aff_code) {
      return `<div class="invite-row">
        <span class="invite-label">邀请</span>
        <span class="invite-link is-hint">邀请码待获取：同步后自动填充，或在「编辑」里手填</span>
      </div>`;
    }
    // 有邀请码但没填站点地址（手动站）：直接展示并复制码本身
    return `<div class="invite-row" title="${esc(s.aff_code)}">
      <span class="invite-label">邀请码</span>
      <span class="invite-link" data-act="copy-invite">${esc(s.aff_code)}</span>
      <button class="btn btn-sm" data-act="copy-invite">复制</button>
    </div>`;
  }
  return `<div class="invite-row" title="${esc(link)}">
    <span class="invite-label">邀请</span>
    <span class="invite-link" data-act="copy-invite">${esc(link)}</span>
    <button class="btn btn-sm" data-act="copy-invite">复制</button>
  </div>`;
}

$("btn-copy-invites").addEventListener("click", async () => {
  const links = state.sites
    .map((s) => ({ name: s.name, link: inviteLink(s) }))
    .filter((x) => x.link);
  if (!links.length) {
    toast("还没有可复制的邀请链接（等同步拿到邀请码，或在编辑里手填）", true);
    return;
  }
  const text = links.map((x) => `${x.name} ${x.link}`).join("\n");
  if (await copyText(text)) {
    toast(`已复制 ${links.length} 条邀请链接`);
  } else {
    toast("复制失败，请手动选择文本", true);
  }
});

/* ---------- 趋势图 ---------- */

async function loadTrend() {
  try {
    state.trend = await api("/api/trend");
    renderTrend();
  } catch { /* 趋势图非关键路径，静默 */ }
}

function renderTrend() {
  const d = state.trend;
  const el = $("trend-chart");
  if (!d || !d.points || d.points.length < 2) {
    el.innerHTML = '<p class="trend-hint">数据积累中：完成两次以上同步后自动生成趋势曲线。</p>';
    $("trend-meta").textContent = "";
    return;
  }
  const pts = d.points;
  const per = d.quota_per_unit || state.quotaPerUnit;
  const W = 600, H = 150, PAD = 10;
  const vals = pts.map((p) => p.income / per);
  const min = 0;
  const max = Math.max(...vals, 1);
  const span = max - min;
  const X = (i) => PAD + (i * (W - 2 * PAD)) / (pts.length - 1);
  const Y = (v) => H - PAD - ((v - min) * (H - 2 * PAD)) / span;
  const linePts = pts.map((p, i) => `${X(i).toFixed(1)},${Y(vals[i]).toFixed(1)}`);
  const lastI = pts.length - 1;
  el.innerHTML = `
    <svg class="trend-svg" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="累计推广收益趋势">
      <path d="M${X(0).toFixed(1)},${H - PAD} L${linePts.join(" L")} L${X(lastI).toFixed(1)},${H - PAD} Z" fill="var(--accent-soft)"></path>
      <polyline points="${linePts.join(" ")}" fill="none" stroke="var(--accent)" stroke-width="2" vector-effect="non-scaling-stroke"></polyline>
      <circle cx="${X(lastI).toFixed(1)}" cy="${Y(vals[lastI]).toFixed(1)}" r="3.5" fill="var(--accent)"></circle>
    </svg>
    <div class="trend-labels">
      <span>${fmtDT(pts[0].t)} · ${fmtUSD(pts[0].income, per)}</span>
      <span>当前 ${fmtUSD(pts[lastI].income, per)}</span>
    </div>`;
  $("trend-meta").textContent = d.bucket === "day" ? "按天采样" : "按时段采样";
}

/* ---------- 站点卡片操作 ---------- */

$("sites").addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-act]");
  if (!btn) return;
  const id = Number(btn.closest(".card").dataset.id);
  const site = state.sites.find((x) => x.id === id);
  if (!site) return;
  const act = btn.dataset.act;

  if (act === "sync") {
    btn.disabled = true;
    try {
      await api(`/api/sites/${id}/sync`, { method: "POST" });
      toast("同步完成");
    } catch (err) {
      toast(err.message, true);
    }
    await loadSites();
  }

  if (act === "aff") {
    if (!confirm(`确认把「${site.name}」的邀请奖励（${fmtUSD(site.latest.aff_quota)}）转入余额？`)) return;
    btn.disabled = true;
    try {
      await api(`/api/sites/${id}/aff-transfer`, { method: "POST" });
      toast("已转入余额");
    } catch (err) {
      toast(err.message, true);
    }
    await loadSites();
  }

  if (act === "edit") openSiteDialog(site);
  if (act === "apply") openApply(site);
  if (act === "copy-invite") {
    if (await copyText(inviteLink(site))) toast("邀请链接已复制");
    else toast("复制失败，请手动选择文本", true);
  }

  if (act === "del") {
    if (!confirm(`确认删除站点「${site.name}」？它的历史数据会一并删除。`)) return;
    try {
      await api(`/api/sites/${id}`, { method: "DELETE" });
      toast("已删除");
    } catch (err) {
      toast(err.message, true);
    }
    await loadSites();
  }
});

async function fetchSiteToken(siteId) {
  const res = await api(`/api/sites/${siteId}/token`);
  return res.token;
}

$("btn-token-reveal").addEventListener("click", async () => {
  if (state.tokenRevealed) {
    state.tokenRevealed = false;
    $("token-plain").textContent = "••••••••••••";
    $("btn-token-reveal").textContent = "查看";
    return;
  }
  try {
    const token = await fetchSiteToken(state.editingId);
    state.tokenRevealed = true;
    $("token-plain").textContent = token;
    $("token-plain").title = token;
    $("btn-token-reveal").textContent = "隐藏";
  } catch (err) {
    toast(err.message, true);
  }
});

$("btn-token-copy").addEventListener("click", async () => {
  try {
    const token = state.tokenRevealed ? $("token-plain").textContent : await fetchSiteToken(state.editingId);
    if (await copyText(token)) toast("令牌已复制");
    else toast("复制失败", true);
  } catch (err) {
    toast(err.message, true);
  }
});

/* ---------- 添加 / 编辑弹窗 ---------- */

function applyTypeBlock() {
  const isManual = $("f-type").value === "manual";
  $("f-auto-block").classList.toggle("hidden", isManual);
  $("f-manual-block").classList.toggle("hidden", !isManual);
  $("btn-probe").classList.toggle("hidden", isManual || state.mode !== "manual");
}

function setMode(mode) {
  state.mode = mode;
  document.querySelectorAll("#mode-switch .mode-btn").forEach((b) => {
    b.classList.toggle("active", b.dataset.mode === mode);
    b.setAttribute("aria-selected", b.dataset.mode === mode);
  });
  $("mode-auto").classList.toggle("hidden", mode !== "auto");
  $("mode-manual").classList.toggle("hidden", mode !== "manual");
  $("btn-probe").classList.toggle("hidden", $("f-type").value === "manual" || mode !== "manual");
  hideErr("site-error");
}

function openSiteDialog(site) {
  state.editingId = site ? site.id : null;
  $("site-dialog-title").textContent = site ? "编辑站点" : "添加站点";
  $("f-name").value = site ? site.name : "";
  $("f-url").value = site ? site.base_url : "";
  $("f-type").value = site ? site.type : "auto";
  $("f-token").value = "";
  $("f-token").placeholder = site ? "留空则不修改令牌" : "站点控制台生成";
  $("f-uid").value = site ? site.user_id : "";
  $("f-user").value = "";
  $("f-pass").value = "";
  const mu = site ? site.manual_usd : null;
  $("f-mquota").value = mu ? mu.quota : "";
  $("f-maff").value = mu ? mu.aff : "";
  $("f-mhist").value = mu ? mu.hist : "";
  $("f-wurl").value = site && site.withdraw_url_custom ? site.withdraw_url : "";
  $("f-wapi").value = site ? (site.withdraw_api || "") : "";
  $("f-aff").value = site ? (site.aff_code || "") : "";
  // 当前令牌揭示行：仅编辑已有令牌的站点时显示
  state.tokenRevealed = false;
  if (site && site.has_token) {
    $("token-view-row").classList.remove("hidden");
    $("token-plain").textContent = "••••••••••••";
    $("btn-token-reveal").textContent = "查看";
  } else {
    $("token-view-row").classList.add("hidden");
  }
  $("f-wurl").placeholder = site && site.withdraw_url && !site.withdraw_url_custom
    ? "当前默认：" + site.withdraw_url
    : "留空用默认 /console/wallet";
  $("f-enabled").checked = site ? site.enabled : true;
  $("probe-result").classList.add("hidden");
  hideErr("site-error");
  setMode("manual");
  applyTypeBlock();
  $("site-dialog").showModal();
}

$("btn-add").addEventListener("click", () => openSiteDialog(null));
$("btn-add-empty").addEventListener("click", () => openSiteDialog(null));
$("btn-cancel").addEventListener("click", () => $("site-dialog").close());
$("f-type").addEventListener("change", () => {
  applyTypeBlock();
  $("f-user").placeholder = $("f-type").value === "sub2api" ? "注册邮箱" : "用户名 / 邮箱";
  hideErr("site-error");
  $("probe-result").classList.add("hidden");
});

document.querySelectorAll("#mode-switch .mode-btn").forEach((b) => {
  b.addEventListener("click", () => setMode(b.dataset.mode));
});

$("btn-autotoken").addEventListener("click", async () => {
  const base = $("f-url").value.trim();
  const user = $("f-user").value.trim();
  const pass = $("f-pass").value;
  if (!base || !user || !pass) {
    showErr("site-error", "请先填好站点地址、账号与密码");
    return;
  }
  hideErr("site-error");
  const btn = $("btn-autotoken");
  btn.disabled = true;
  btn.textContent = "登录获取中…";
  try {
    const res = await api("/api/sites/autotoken", {
      method: "POST",
      body: JSON.stringify({ base_url: base, username: user, password: pass, type: $("f-type").value }),
    });
    $("f-token").value = res.token;
    $("f-uid").value = res.user_id;
    if (!$("f-name").value.trim() && res.system_name) $("f-name").value = res.system_name;
    $("f-pass").value = ""; // 用完即清，不留在 DOM
    const p = $("probe-result");
    p.innerHTML =
      `✅ 已自动生成令牌并获取额度：可用余额 ${fmtUSD(res.quota)} · 邀请奖励 ${fmtUSD(res.aff_quota)} · 累计收益 ${fmtUSD(res.aff_history)}<br>` +
      `确认无误后点「保存」。若你此前在别处使用过该站的旧令牌，旧令牌已随本次重新生成而失效。`;
    p.classList.remove("hidden");
    setMode("manual");
  } catch (err) {
    showErr("site-error", err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "自动获取令牌";
  }
});

$("btn-probe").addEventListener("click", async () => {
  const base = $("f-url").value.trim();
  const token = $("f-token").value.trim();
  const uid = Number($("f-uid").value);
  if (!base || !token || !uid) {
    showErr("site-error", "测试连接前请先填好站点地址、访问令牌与数字用户 ID");
    return;
  }
  hideErr("site-error");
  const btn = $("btn-probe");
  btn.disabled = true;
  btn.textContent = "测试中…";
  try {
    const info = await api("/api/sites/probe", {
      method: "POST",
      body: JSON.stringify({ base_url: base, token, user_id: uid, type: $("f-type").value }),
    });
    const p = $("probe-result");
    p.innerHTML =
      `✅ 连接成功：<b>${esc(info.system_name || "未命名站点")}</b> ${esc(info.version || "")}<br>` +
      `可用余额 ${fmtUSD(info.quota)} · 邀请奖励 ${fmtUSD(info.aff_quota)} · 累计收益 ${fmtUSD(info.aff_history)}`;
    p.classList.remove("hidden");
    if (!$("f-name").value.trim() && info.system_name) $("f-name").value = info.system_name;
  } catch (err) {
    $("probe-result").classList.add("hidden");
    showErr("site-error", err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "测试连接";
  }
});

$("site-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const type = $("f-type").value;
  const payload = {
    name: $("f-name").value.trim(),
    base_url: $("f-url").value.trim(),
    type,
    enabled: $("f-enabled").checked,
    withdraw_url: $("f-wurl").value.trim(),
    withdraw_api: $("f-wapi").value.trim(),
    aff_code: $("f-aff").value.trim(),
  };

  if (type === "manual") {
    if (!payload.name && !payload.base_url) {
      showErr("site-error", "手动站点至少填一个备注名");
      return;
    }
    payload.manual_quota_usd = Number($("f-mquota").value) || 0;
    payload.manual_aff_usd = Number($("f-maff").value) || 0;
    payload.manual_aff_history_usd = Number($("f-mhist").value) || 0;
  } else {
    payload.user_id = Number($("f-uid").value);
    payload.token = $("f-token").value.trim();
    if (!payload.base_url || !payload.user_id) {
      showErr("site-error", "站点地址与数字用户 ID 必填");
      return;
    }
    const hasToken = state.editingId && (state.sites.find((x) => x.id === state.editingId) || {}).has_token;
    if (!payload.token && !hasToken) {
      showErr("site-error", "系统访问令牌必填；也可切换到「账号密码自动获取」");
      return;
    }
  }

  hideErr("site-error");
  try {
    if (state.editingId) {
      if (type === "auto" && !payload.token) delete payload.token;
      await api(`/api/sites/${state.editingId}`, { method: "PUT", body: JSON.stringify(payload) });
      toast("已保存");
    } else {
      await api("/api/sites", { method: "POST", body: JSON.stringify(payload) });
      toast(type === "manual" ? "已添加" : "已添加，正在同步…");
    }
    $("site-dialog").close();
    await loadSites();
  } catch (err) {
    showErr("site-error", err.message);
  }
});

/* ---------- 提现台账 ---------- */

async function loadLedger() {
  try {
    const d = await api("/api/withdrawals");
    state.withdrawals = d.withdrawals || [];
    renderLedger(d.summary || {});
  } catch (err) {
    toast(err.message, true);
  }
}

function statusLabel(s) {
  return { pending: "待到账", paid: "已到账", failed: "失败" }[s] || s;
}

function renderLedger(sum) {
  $("ledger-summary").innerHTML = `
    <span class="sumchip"><span class="sum-label">待到账</span><b>${sum.pending_count || 0} 笔 · $${(sum.pending_usd || 0).toFixed(2)}</b></span>
    <span class="sumchip"><span class="sum-label">已到账累计</span><b>$${(sum.paid_usd || 0).toFixed(2)}</b></span>
    <span class="sumchip"><span class="sum-label">失败</span><b>$${(sum.failed_usd || 0).toFixed(2)}</b></span>`;

  const has = state.withdrawals.length > 0;
  $("ledger-empty").classList.toggle("hidden", has);
  if (!has) {
    $("ledger-table").innerHTML = "";
    return;
  }
  $("ledger-table").innerHTML = `
    <table>
      <thead><tr>
        <th>站点</th><th class="num">金额</th><th>渠道</th><th>状态</th>
        <th>申请时间</th><th>到账时间</th><th>备注</th><th>操作</th>
      </tr></thead>
      <tbody>${state.withdrawals.map(wdRow).join("")}</tbody>
    </table>`;
}

function wdRow(w) {
  return `<tr data-id="${w.id}">
    <td>${esc(w.site_name)}</td>
    <td class="num">$${w.amount_usd.toFixed(2)}</td>
    <td>${esc(w.channel || "—")}</td>
    <td><span class="chip chip-${w.status}">${statusLabel(w.status)}</span></td>
    <td class="muted">${fmtDT(w.created_at)}</td>
    <td class="muted">${w.resolved_at ? fmtDT(w.resolved_at) : "—"}</td>
    <td class="wd-note muted" title="${esc(w.note || "")}">${esc(w.note || "")}</td>
    <td class="row-actions">
      ${w.status === "pending"
        ? '<button class="btn btn-sm" data-act="paid">标记到账</button><button class="btn btn-sm" data-act="failed">失败</button>'
        : ""}
      <button class="btn btn-sm" data-act="edit">编辑</button>
      <button class="btn btn-sm btn-danger" data-act="del">删除</button>
    </td>
  </tr>`;
}

$("ledger-table").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  const id = Number(btn.closest("tr").dataset.id);
  const rec = state.withdrawals.find((x) => x.id === id);
  if (!rec) return;
  const act = btn.dataset.act;

  if (act === "paid" || act === "failed") {
    const label = act === "paid" ? "已到账" : "失败";
    if (!confirm(`确认将「${rec.site_name}」的 $${rec.amount_usd.toFixed(2)} 标记为「${label}」？`)) return;
    try {
      await api(`/api/withdrawals/${id}`, { method: "PUT", body: JSON.stringify({ status: act }) });
      toast("已更新");
    } catch (err) {
      toast(err.message, true);
    }
    loadLedger();
  }
  if (act === "edit") openWD(rec);
  if (act === "del") {
    if (!confirm(`确认删除「${rec.site_name}」的这笔提现记录？`)) return;
    try {
      await api(`/api/withdrawals/${id}`, { method: "DELETE" });
      toast("已删除");
    } catch (err) {
      toast(err.message, true);
    }
    loadLedger();
  }
});

function openWD(wd) {
  state.editingWD = wd ? wd.id : null;
  state.wdApplySite = null;
  $("wd-dialog-title").textContent = wd ? "编辑提现记录" : "记一笔提现";
  const sel = $("w-site");
  sel.innerHTML = state.sites.length
    ? state.sites.map((s) => `<option value="${s.id}">${esc(s.name)}</option>`).join("")
    : '<option value="">（无站点）</option>';
  sel.disabled = !!wd;
  if (wd) {
    sel.value = wd.site_id;
    $("w-amount").value = wd.amount_usd;
    $("w-channel").value = wd.channel || "其他";
    $("w-status").value = wd.status;
    $("w-note").value = wd.note || "";
  } else {
    if (state.sites.length) sel.value = state.sites[0].id;
    $("w-amount").value = "";
    $("w-channel").value = "支付宝";
    $("w-status").value = "pending";
    $("w-note").value = "";
  }
  $("wd-open-field").classList.add("hidden");
  $("w-status-field").classList.toggle("hidden", !wd);
  hideErr("wd-error");
  $("wd-dialog").showModal();
}

// openApply 一键申请提现：金额预填站点可用余额，保存后登记台账并打开提现页
function openApply(site) {
  state.editingWD = null;
  state.wdApplySite = site;
  $("wd-dialog-title").textContent = `申请提现 · ${site.name}`;
  const sel = $("w-site");
  sel.innerHTML = `<option value="${site.id}">${esc(site.name)}</option>`;
  sel.disabled = true;
  const l = site.latest;
  $("w-amount").value = l ? (l.quota / state.quotaPerUnit).toFixed(2) : "";
  $("w-channel").value = "支付宝";
  $("w-note").value = "";
  $("wd-open-field").classList.remove("hidden");
  $("w-open").checked = true;
  $("w-status-field").classList.add("hidden");
  hideErr("wd-error");
  $("wd-dialog").showModal();
}

$("btn-wd-add").addEventListener("click", () => openWD(null));
$("btn-wd-add-empty").addEventListener("click", () => openWD(null));
$("btn-wd-cancel").addEventListener("click", () => $("wd-dialog").close());

$("wd-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const payload = {
    site_id: Number($("w-site").value),
    amount_usd: Number($("w-amount").value),
    channel: $("w-channel").value,
    note: $("w-note").value.trim(),
  };
  if (!payload.site_id) {
    showErr("wd-error", "请先添加站点再记账");
    return;
  }
  if (!(payload.amount_usd > 0)) {
    showErr("wd-error", "金额必须大于 0");
    return;
  }
  hideErr("wd-error");

  // 一键申请模式：直接向站方（如已配置接口）提交并登记台账
  if (state.wdApplySite) {
    try {
      const res = await api(`/api/sites/${state.wdApplySite.id}/withdraw`, {
        method: "POST",
        body: JSON.stringify(payload),
      });
      if ($("w-open").checked && res.withdraw_url) {
        window.open(res.withdraw_url, "_blank");
      }
      toast(res.auto_submitted
        ? `站方已受理：${res.station_message || "申请已提交"}（已在台账登记）`
        : "已登记台账，请在打开的页面完成申请，到账后回来标记", false, 4200);
      $("wd-dialog").close();
      await loadSites();
      if (state.view === "ledger") await loadLedger();
    } catch (err) {
      showErr("wd-error", err.message);
    }
    return;
  }

  try {
    if (state.editingWD) {
      payload.status = $("w-status").value;
      await api(`/api/withdrawals/${state.editingWD}`, { method: "PUT", body: JSON.stringify(payload) });
    } else {
      await api("/api/withdrawals", { method: "POST", body: JSON.stringify(payload) });
      if ($("w-open").checked) {
        const site = state.sites.find((x) => x.id === payload.site_id);
        if (site && site.withdraw_url) window.open(site.withdraw_url, "_blank");
      }
    }
    $("wd-dialog").close();
    toast("已保存");
    loadLedger();
  } catch (err) {
    showErr("wd-error", err.message);
  }
});

/* ---------- 设置 ---------- */

$("btn-settings").addEventListener("click", async () => {
  hideErr("settings-error");
  try {
    const s = await api("/api/settings");
    $("s-interval").value = s.sync_interval_min;
    $("s-quota").value = s.quota_per_unit;
    const tg = s.tg || {};
    $("s-tg-income").checked = !!tg.notify_income;
    $("s-tg-down").checked = !!tg.notify_down;
    $("s-tg-chat").value = tg.chat_id || "";
    $("s-tg-proxy").value = tg.proxy_url || "";
    $("s-tg-api").value = tg.api_base || "";
    $("s-tg-threshold").value = tg.down_threshold || 3;
    $("s-tg-token").value = "";
    $("s-tg-token").placeholder = tg.has_bot_token ? "已保存，留空不修改" : "如 123456:ABC-DEF...，@BotFather 创建";
    $("settings-dialog").showModal();
  } catch (err) {
    toast(err.message, true);
  }
});

$("btn-settings-cancel").addEventListener("click", () => $("settings-dialog").close());

$("btn-tg-chatid").addEventListener("click", async () => {
  try {
    const res = await api("/api/notify/tg-chatid", {
      method: "POST",
      body: JSON.stringify({ bot_token: $("s-tg-token").value.trim() }),
    });
    $("s-tg-chat").value = res.chat_id;
    toast("已获取 Chat ID：" + res.chat_id);
  } catch (err) {
    toast(err.message, true);
  }
});

$("btn-tg-test").addEventListener("click", async () => {
  const btn = $("btn-tg-test");
  btn.disabled = true;
  try {
    await api("/api/notify/tg-test", { method: "POST" });
    toast("测试消息已发送，请在 Telegram 查看");
  } catch (err) {
    toast(err.message, true);
  } finally {
    btn.disabled = false;
  }
});

$("settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/settings", {
      method: "POST",
      body: JSON.stringify({
        sync_interval_min: Number($("s-interval").value),
        quota_per_unit: Number($("s-quota").value),
        tg: {
          notify_income: $("s-tg-income").checked,
          notify_down: $("s-tg-down").checked,
          chat_id: $("s-tg-chat").value.trim(),
          proxy_url: $("s-tg-proxy").value.trim(),
          api_base: $("s-tg-api").value.trim(),
          down_threshold: Number($("s-tg-threshold").value) || 3,
          bot_token: $("s-tg-token").value.trim(),
        },
      }),
    });
    $("settings-dialog").close();
    toast("设置已保存");
    await loadSites();
  } catch (err) {
    showErr("settings-error", err.message);
  }
});

/* ---------- 数据备份 ---------- */

$("btn-export").addEventListener("click", () => {
  window.open("/api/export");
  toast("已开始下载导出文件");
});

$("btn-import").addEventListener("click", () => $("import-file").click());

$("import-file").addEventListener("change", async (e) => {
  const file = e.target.files[0];
  e.target.value = "";
  if (!file) return;
  if (!confirm("导入将覆盖当前全部站点、历史与提现台账数据，确认继续？")) return;
  try {
    const text = await file.text();
    await api("/api/import", { method: "POST", body: text });
    toast("导入成功，页面即将刷新，请用导出时的主密码解锁");
    setTimeout(() => location.reload(), 1200);
  } catch (err) {
    toast(err.message, true);
  }
});

/* ---------- 顶栏 ---------- */

$("btn-sync-all").addEventListener("click", async () => {
  try {
    await api("/api/sync", { method: "POST" });
    toast("已开始同步全部站点");
    await loadSites();
  } catch (err) {
    toast(err.message, true);
  }
});

$("btn-lock").addEventListener("click", async () => {
  try {
    await api("/api/lock", { method: "POST" });
  } finally {
    location.reload();
  }
});

/* ---------- 启动 ---------- */

initGate().catch((err) => {
  $("gate").classList.remove("hidden");
  showErr("gate-error", err.message);
});
