// SPDX-License-Identifier: AGPL-3.0-only
// RelayHub Companion 后台：定时轮询各站 /api/user/self，快照存 chrome.storage.local。
// 令牌只保存在本机浏览器配置内（随浏览器配置文件加密落盘），不出设备。

const HISTORY_CAP = 200;
const ALARM_NAME = "sync";

async function getStore(keys) {
  return chrome.storage.local.get(keys);
}

function authHeaders(site) {
  const uid = String(site.userId || "");
  return {
    "Content-Type": "application/json",
    "Authorization": "Bearer " + site.token,
    "New-Api-User": uid,
    "Veloera-User": uid,
  };
}

// fetchSelf 拉取用户额度信息；401 时去掉 Bearer 前缀重试一次（兼容部分 one-api 系分叉）。
async function fetchSelf(site) {
  const hdrs = authHeaders(site);
  let res = await fetch(site.baseUrl + "/api/user/self", { headers: hdrs });
  if (res.status === 401 || res.status === 403) {
    hdrs.Authorization = site.token;
    res = await fetch(site.baseUrl + "/api/user/self", { headers: hdrs });
  }
  const env = await res.json().catch(() => null);
  if (!env) throw new Error("响应不是 JSON（该站可能不是 new-api/one-api 系）");
  if (!res.ok || env.success === false) {
    throw new Error(env.message || `HTTP ${res.status}`);
  }
  return env.data || {};
}

async function pushHistory(siteId, info) {
  const { history = {} } = await getStore(["history"]);
  const arr = history[siteId] || [];
  arr.push({
    t: Date.now(),
    quota: info.quota || 0,
    aff: info.aff || 0,
    hist: info.hist || 0,
    count: info.count || 0,
  });
  while (arr.length > HISTORY_CAP) arr.shift();
  history[siteId] = arr;
  await chrome.storage.local.set({ history });
}

async function markResult(siteId, ok, error) {
  const { lastOk = {} } = await getStore(["lastOk"]);
  lastOk[siteId] = ok ? { ok: true, at: Date.now() } : { ok: false, at: Date.now(), error: String(error) };
  await chrome.storage.local.set({ lastOk });
}

// fetchCurrent 取站点当前额度。new-api 系走 /api/user/self（Bearer+用户头），
// Sub2API 走 /api/v1/auth/me（Bearer JWT，余额为美元）。
async function fetchCurrent(site, perUnit) {
  if (site.type === "sub2api") {
    const res = await fetch(site.baseUrl + "/api/v1/auth/me", {
      headers: { Accept: "application/json", Authorization: "Bearer " + site.token },
    });
    const env = await res.json().catch(() => null);
    if (!env) throw new Error("响应不是 JSON，该站可能不是 Sub2API");
    if (res.status === 401 || env.code !== 0) {
      throw new Error(env.message || `HTTP ${res.status}（JWT 可能已过期，请重新自动获取）`);
    }
    const balance = Number(env.data && env.data.balance) || 0;
    return { quota: Math.round(balance * (perUnit || 500000)), aff: 0, hist: 0, count: 0 };
  }
  const info = await fetchSelf(site);
  return {
    quota: info.quota || 0,
    aff: info.aff_quota || 0,
    hist: info.aff_history_quota || 0,
    count: info.aff_count || 0,
  };
}

async function syncAll() {
  const { sites = [], settings = {} } = await getStore(["sites", "settings"]);
  const perUnit = settings.quotaPerUnit || 500000;
  for (const site of sites) {
    if (site.type === "manual" || site.enabled === false) continue;
    try {
      const cur = await fetchCurrent(site, perUnit);
      await pushHistory(site.id, cur);
      await markResult(site.id, true);
    } catch (err) {
      await markResult(site.id, false, err && err.message ? err.message : err);
    }
  }
}

chrome.runtime.onInstalled.addListener(async () => {
  const { settings = {} } = await getStore(["settings"]);
  chrome.alarms.create(ALARM_NAME, { periodInMinutes: Math.max(5, settings.intervalMin || 30) });
});

chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === ALARM_NAME) syncAll();
});

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "syncNow") {
    syncAll()
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true; // 异步响应
  }
  if (msg && msg.type === "reschedule") {
    chrome.alarms.create(ALARM_NAME, { periodInMinutes: Math.max(5, msg.intervalMin || 30) });
    sendResponse({ ok: true });
  }
});
