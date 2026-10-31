// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type app struct {
	db       *DB
	syncer   *Syncer
	notifier *Notifier
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8077", "HTTP 监听地址")
	dataDir := flag.String("data", "data", "数据目录")
	flag.Parse()

	db, err := OpenDB(filepath.Join(*dataDir, "relayhub.json"))
	if err != nil {
		log.Fatalf("打开数据文件失败: %v", err)
	}
	notifier := newNotifier(db)
	a := &app{db: db, syncer: &Syncer{db: db, client: newAPIClient(), notifier: notifier}, notifier: notifier}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", a.handleSession)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/unlock", a.handleUnlock)
	mux.HandleFunc("POST /api/lock", a.handleLock)
	mux.HandleFunc("GET /api/sites", a.requireUnlock(a.handleSites))
	mux.HandleFunc("POST /api/sites/probe", a.requireUnlock(a.handleProbe))
	mux.HandleFunc("POST /api/sites/autotoken", a.requireUnlock(a.handleAutoToken))
	mux.HandleFunc("POST /api/sites", a.requireUnlock(a.handleSiteAdd))
	mux.HandleFunc("PUT /api/sites/{id}", a.requireUnlock(a.handleSiteUpdate))
	mux.HandleFunc("DELETE /api/sites/{id}", a.requireUnlock(a.handleSiteDelete))
	mux.HandleFunc("POST /api/sites/{id}/sync", a.requireUnlock(a.handleSiteSync))
	mux.HandleFunc("POST /api/sites/{id}/aff-transfer", a.requireUnlock(a.handleAffTransfer))
	mux.HandleFunc("GET /api/sites/{id}/history", a.requireUnlock(a.handleSiteHistory))
	mux.HandleFunc("POST /api/sync", a.requireUnlock(a.handleSyncAll))
	mux.HandleFunc("GET /api/settings", a.requireUnlock(a.handleSettingsGet))
	mux.HandleFunc("POST /api/settings", a.requireUnlock(a.handleSettingsPost))
	mux.HandleFunc("POST /api/notify/tg-chatid", a.requireUnlock(a.handleTGChatID))
	mux.HandleFunc("POST /api/notify/tg-test", a.requireUnlock(a.handleTGTest))
	mux.HandleFunc("GET /api/export", a.requireUnlock(a.handleExport))
	mux.HandleFunc("POST /api/import", a.requireUnlock(a.handleImport))
	mux.HandleFunc("GET /api/trend", a.requireUnlock(a.handleTrend))
	mux.HandleFunc("GET /api/withdrawals", a.requireUnlock(a.handleWithdrawalsList))
	mux.HandleFunc("POST /api/withdrawals", a.requireUnlock(a.handleWithdrawalAdd))
	mux.HandleFunc("PUT /api/withdrawals/{id}", a.requireUnlock(a.handleWithdrawalUpdate))
	mux.HandleFunc("DELETE /api/withdrawals/{id}", a.requireUnlock(a.handleWithdrawalDelete))
	mux.Handle("GET /", a.static())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.syncer.Run(ctx)

	srv := &http.Server{Addr: *addr, Handler: withLogging(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("RelayHub 已启动: http://%s （数据目录: %s）", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务异常退出: %v", err)
	}
	log.Printf("RelayHub 已退出")
}

func (a *app) static() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内嵌资源缺失: %v", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
}

func (a *app) requireUnlock(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.syncer.vaultReady() {
			writeErr(w, http.StatusLocked, "密码库已锁定，请先解锁")
			return
		}
		next(w, r)
	}
}

func (a *app) handleSession(w http.ResponseWriter, r *http.Request) {
	set := a.db.Settings()
	writeOK(w, map[string]any{
		"setup_done":        set.SaltHex != "",
		"unlocked":          a.syncer.vaultReady(),
		"sync_interval_min": set.SyncIntervalMin,
	})
}

type credRequest struct {
	Password string `json:"password"`
}

func (a *app) handleSetup(w http.ResponseWriter, r *http.Request) {
	set := a.db.Settings()
	if set.SaltHex != "" {
		writeErr(w, http.StatusBadRequest, "已完成初始化，请使用主密码解锁")
		return
	}
	var req credRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "主密码至少 8 位，用于加密各站点的访问令牌")
		return
	}
	if err := a.initVault(req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	go a.syncer.SyncAll()
	writeOK(w, nil)
}

func (a *app) initVault(password string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key, err := deriveKey(password, salt)
	if err != nil {
		return err
	}
	v := newVault(key)
	verifier, err := v.Seal([]byte(vaultCheck))
	if err != nil {
		return err
	}
	if err := a.db.SetSaltVerifier(hex.EncodeToString(salt), verifier); err != nil {
		return err
	}
	a.syncer.setVault(v)
	return nil
}

func (a *app) handleUnlock(w http.ResponseWriter, r *http.Request) {
	set := a.db.Settings()
	if set.SaltHex == "" {
		writeErr(w, http.StatusBadRequest, "尚未初始化，请先设置主密码")
		return
	}
	var req credRequest
	if !readJSON(w, r, &req) {
		return
	}
	salt, err := hex.DecodeString(set.SaltHex)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "盐值损坏")
		return
	}
	key, err := deriveKey(req.Password, salt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := newVault(key)
	if plain, err := v.Open(set.Verifier); err != nil || string(plain) != vaultCheck {
		writeErr(w, http.StatusUnauthorized, "主密码错误")
		return
	}
	a.syncer.setVault(v)
	go a.syncer.SyncAll()
	writeOK(w, nil)
}

func (a *app) handleLock(w http.ResponseWriter, r *http.Request) {
	a.syncer.setVault(nil)
	writeOK(w, nil)
}

func (a *app) handleSites(w http.ResponseWriter, r *http.Request) {
	set := a.db.Settings()
	writeOK(w, map[string]any{
		"sites":             a.db.SiteViews(),
		"quota_per_unit":    set.QuotaPerUnit,
		"sync_interval_min": set.SyncIntervalMin,
		"syncing":           a.syncer.isRunning(),
	})
}

type probeRequest struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	UserID  int64  `json:"user_id"`
}

func (a *app) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if !readJSON(w, r, &req) {
		return
	}
	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Token == "" || req.UserID == 0 {
		writeErr(w, http.StatusBadRequest, "访问令牌与数字用户 ID 均必填")
		return
	}
	info, err := a.syncer.client.Probe(base, req.Token, req.UserID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, info)
}

type autoTokenRequest struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *app) handleAutoToken(w http.ResponseWriter, r *http.Request) {
	var req autoTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "账号与密码必填（密码仅本次内存使用，不会被保存）")
		return
	}
	res, err := a.syncer.client.LoginAutoToken(base, req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, res)
}

type siteRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	UserID  int64  `json:"user_id"`
	Enabled *bool  `json:"enabled"`
	Type    string `json:"type"` // auto（new-api/one-api/one-hub 系）| manual（手动记录）

	// Type=manual 时的数值，单位美元，按全局换算率转 quota；0 表示不修改（编辑时）。
	ManualQuotaUSD      float64 `json:"manual_quota_usd"`
	ManualAffUSD        float64 `json:"manual_aff_usd"`
	ManualAffHistoryUSD float64 `json:"manual_aff_history_usd"`
}

func usdToQuota(usd, perUnit float64) int64 {
	if perUnit <= 0 {
		perUnit = 500000
	}
	return int64(math.Round(usd * perUnit))
}

func (a *app) handleSiteAdd(w http.ResponseWriter, r *http.Request) {
	var req siteRequest
	if !readJSON(w, r, &req) {
		return
	}
	siteType := req.Type
	if siteType == "" {
		siteType = "auto"
	}
	if siteType != "auto" && siteType != "manual" {
		writeErr(w, http.StatusBadRequest, "无效的站点类型")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if siteType == "manual" {
		base := strings.TrimSpace(req.BaseURL)
		name := strings.TrimSpace(req.Name)
		if name == "" {
			if base != "" {
				name = hostOf(base)
			} else {
				name = "手动站点"
			}
		}
		per := a.db.Settings().QuotaPerUnit
		site := &Site{
			Name:             name,
			BaseURL:          base,
			Type:             "manual",
			Enabled:          enabled,
			ManualQuota:      usdToQuota(req.ManualQuotaUSD, per),
			ManualAffQuota:   usdToQuota(req.ManualAffUSD, per),
			ManualAffHistory: usdToQuota(req.ManualAffHistoryUSD, per),
		}
		if err := a.db.AddSite(site); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.db.AddSnapshot(Snapshot{
			SiteID:     site.ID,
			Time:       time.Now(),
			Quota:      site.ManualQuota,
			AffQuota:   site.ManualAffQuota,
			AffHistory: site.ManualAffHistory,
		})
		view, _ := a.db.SiteView(site.ID)
		writeOK(w, view)
		return
	}

	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Token == "" || req.UserID == 0 {
		writeErr(w, http.StatusBadRequest, "访问令牌与数字用户 ID 均必填")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = hostOf(base)
	}
	v := a.syncer.currentVault()
	tokenEnc, err := v.sealString(req.Token)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密令牌失败: "+err.Error())
		return
	}
	site := &Site{
		Name:     name,
		BaseURL:  base,
		Type:     "auto",
		UserID:   req.UserID,
		TokenEnc: tokenEnc,
		Enabled:  enabled,
	}
	if err := a.db.AddSite(site); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	go a.syncer.SyncSite(site.ID)
	view, _ := a.db.SiteView(site.ID)
	writeOK(w, view)
}

func (a *app) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	var req siteRequest
	if !readJSON(w, r, &req) {
		return
	}
	var newBase string
	if req.BaseURL != "" {
		b, err := normalizeBaseURL(req.BaseURL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		newBase = b
	}
	var tokenEnc string
	if req.Token != "" {
		v := a.syncer.currentVault()
		enc, err := v.sealString(req.Token)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密令牌失败: "+err.Error())
			return
		}
		tokenEnc = enc
	}
	if req.Type != "" && req.Type != "auto" && req.Type != "manual" {
		writeErr(w, http.StatusBadRequest, "无效的站点类型")
		return
	}
	manualUpdated := false
	per := a.db.Settings().QuotaPerUnit // 预读，避免在持有 db 锁的闭包内再取锁
	err := a.db.UpdateSite(id, func(st *Site) error {
		if req.Type != "" {
			st.Type = req.Type
		}
		if name := strings.TrimSpace(req.Name); name != "" {
			st.Name = name
		}
		if newBase != "" {
			st.BaseURL = newBase
		}
		if tokenEnc != "" {
			st.TokenEnc = tokenEnc
		}
		if req.UserID != 0 {
			st.UserID = req.UserID
		}
		if req.Enabled != nil {
			st.Enabled = *req.Enabled
		}
		if st.Type == "manual" {
			// 0 视为「未提供」，不做清零（手动数值极少需要清零，避免误操作）。
			if req.ManualQuotaUSD != 0 {
				st.ManualQuota = usdToQuota(req.ManualQuotaUSD, per)
				manualUpdated = true
			}
			if req.ManualAffUSD != 0 {
				st.ManualAffQuota = usdToQuota(req.ManualAffUSD, per)
				manualUpdated = true
			}
			if req.ManualAffHistoryUSD != 0 {
				st.ManualAffHistory = usdToQuota(req.ManualAffHistoryUSD, per)
				manualUpdated = true
			}
		}
		return nil
	})
	if errors.Is(err, errSiteNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if manualUpdated {
		if st, err := a.db.GetSite(id); err == nil {
			a.db.AddSnapshot(Snapshot{
				SiteID:     id,
				Time:       time.Now(),
				Quota:      st.ManualQuota,
				AffQuota:   st.ManualAffQuota,
				AffHistory: st.ManualAffHistory,
			})
		}
	}
	if tokenEnc != "" {
		go a.syncer.SyncSite(id)
	}
	view, _ := a.db.SiteView(id)
	writeOK(w, view)
}

func (a *app) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	err := a.db.DeleteSite(id)
	if errors.Is(err, errSiteNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, nil)
}

func (a *app) handleSiteSync(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	site, err := a.db.GetSite(id)
	if errors.Is(err, errSiteNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !site.Enabled {
		writeErr(w, http.StatusBadRequest, "站点已停用，请先启用")
		return
	}
	if site.Type == "manual" {
		writeErr(w, http.StatusBadRequest, "手动站点不参与同步，请通过「编辑」更新数值")
		return
	}
	if err := a.syncer.SyncSite(id); err != nil {
		view, _ := a.db.SiteView(id)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "data": map[string]any{"site": view}})
		return
	}
	view, _ := a.db.SiteView(id)
	writeOK(w, map[string]any{"site": view})
}

func (a *app) handleSyncAll(w http.ResponseWriter, r *http.Request) {
	if a.syncer.isRunning() {
		writeErr(w, http.StatusConflict, "已有同步任务进行中")
		return
	}
	go a.syncer.SyncAll()
	writeOK(w, map[string]any{"started": true})
}

func (a *app) handleAffTransfer(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	site, err := a.db.GetSite(id)
	if errors.Is(err, errSiteNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := a.syncer.currentVault()
	token, err := v.openString(site.TokenEnc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取令牌失败: "+err.Error())
		return
	}
	if err := a.syncer.client.TransferAff(site.BaseURL, token, site.UserID); err != nil {
		writeErr(w, http.StatusBadGateway, "转余额失败: "+err.Error())
		return
	}
	_ = a.syncer.SyncSite(id) // 刷新余额视图；失败不打断，等下轮同步
	view, _ := a.db.SiteView(id)
	writeOK(w, map[string]any{"site": view})
}

func (a *app) handleSiteHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	writeOK(w, map[string]any{"snapshots": a.db.SiteHistory(id, 200)})
}

func (a *app) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	set := a.db.Settings()
	writeOK(w, map[string]any{
		"quota_per_unit":    set.QuotaPerUnit,
		"sync_interval_min": set.SyncIntervalMin,
		"tg": map[string]any{
			"has_bot_token":  set.TGBotTokenEnc != "",
			"chat_id":        set.TGChatID,
			"api_base":       set.TGAPIBase,
			"proxy_url":      set.TGProxyURL,
			"notify_income":  set.TGNotifyIncome,
			"notify_down":    set.TGNotifyDown,
			"down_threshold": set.TGDownThreshold,
		},
	})
}

type tgSettings struct {
	BotToken      string `json:"bot_token"`
	ChatID        string `json:"chat_id"`
	APIBase       string `json:"api_base"`
	ProxyURL      string `json:"proxy_url"`
	NotifyIncome  bool   `json:"notify_income"`
	NotifyDown    bool   `json:"notify_down"`
	DownThreshold int    `json:"down_threshold"`
}

type settingsRequest struct {
	QuotaPerUnit    float64    `json:"quota_per_unit"`
	SyncIntervalMin int        `json:"sync_interval_min"`
	TG              tgSettings `json:"tg"`
}

func (a *app) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.QuotaPerUnit <= 0 {
		writeErr(w, http.StatusBadRequest, "quota_per_unit 必须为正数")
		return
	}
	if req.SyncIntervalMin < 5 {
		writeErr(w, http.StatusBadRequest, "同步间隔不能低于 5 分钟")
		return
	}
	if req.TG.DownThreshold < 1 {
		writeErr(w, http.StatusBadRequest, "连续失败阈值至少为 1 次")
		return
	}
	var tokenEnc string
	if tok := strings.TrimSpace(req.TG.BotToken); tok != "" {
		v := a.syncer.currentVault()
		enc, err := v.sealString(tok)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密 Bot Token 失败: "+err.Error())
			return
		}
		tokenEnc = enc
	}
	if err := a.db.UpdateSettings(func(s *Settings) {
		s.QuotaPerUnit = req.QuotaPerUnit
		s.SyncIntervalMin = req.SyncIntervalMin
		s.TGChatID = strings.TrimSpace(req.TG.ChatID)
		s.TGAPIBase = strings.TrimSpace(req.TG.APIBase)
		s.TGProxyURL = strings.TrimSpace(req.TG.ProxyURL)
		s.TGNotifyIncome = req.TG.NotifyIncome
		s.TGNotifyDown = req.TG.NotifyDown
		s.TGDownThreshold = req.TG.DownThreshold
		if tokenEnc != "" {
			s.TGBotTokenEnc = tokenEnc
		}
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleSettingsGet(w, r)
}

type tgChatIDRequest struct {
	BotToken string `json:"bot_token"`
}

func (a *app) handleTGChatID(w http.ResponseWriter, r *http.Request) {
	var req tgChatIDRequest
	if !readJSON(w, r, &req) {
		return
	}
	chatID, err := a.notifier.GetChatID(a.syncer.currentVault(), req.BotToken)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"chat_id": chatID})
}

func (a *app) handleTGTest(w http.ResponseWriter, r *http.Request) {
	if err := a.notifier.Test(a.syncer.currentVault()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, nil)
}

// handleExport 下载数据文件。令牌在文件中是密文，但没有主密码无法解密。
func (a *app) handleExport(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(a.db.Path())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取数据文件失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="relayhub-export.json"`)
	_, _ = w.Write(raw)
}

// handleImport 整体导入数据文件（站点/历史/台账/设置）。
// 导入后密码库立即锁定：令牌是用「导出方」的主密码加密的，需用该密码重新解锁。
func (a *app) handleImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传内容失败: "+err.Error())
		return
	}
	var f dbFile
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadRequest, "文件不是有效的 RelayHub 数据（JSON 解析失败）")
		return
	}
	if f.Settings.SaltHex == "" || f.Settings.Verifier == "" {
		writeErr(w, http.StatusBadRequest, "文件缺少主密码盐值/校验值，不是有效的导出文件")
		return
	}
	if err := a.db.Replace(f); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.syncer.setVault(nil)
	writeOK(w, nil)
}

func (a *app) handleTrend(w http.ResponseWriter, r *http.Request) {
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			days = min(max(n, 7), 90)
		}
	}
	bucket, points := BuildTrend(a.db.AllSnapshots(), time.Now(), days)
	writeOK(w, map[string]any{
		"bucket":         bucket,
		"points":         points,
		"quota_per_unit": a.db.Settings().QuotaPerUnit,
	})
}

type withdrawalRequest struct {
	SiteID    int64   `json:"site_id"`
	AmountUSD float64 `json:"amount_usd"`
	Channel   string  `json:"channel"`
	Note      string  `json:"note"`
	Status    string  `json:"status"`
}

func (a *app) handleWithdrawalsList(w http.ResponseWriter, r *http.Request) {
	list := a.db.ListWithdrawals()
	var pendingCount int64
	var pendingUSD, paidUSD, failedUSD float64
	for _, wd := range list {
		switch wd.Status {
		case "pending":
			pendingCount++
			pendingUSD += wd.AmountUSD
		case "paid":
			paidUSD += wd.AmountUSD
		case "failed":
			failedUSD += wd.AmountUSD
		}
	}
	writeOK(w, map[string]any{
		"withdrawals": list,
		"summary": map[string]any{
			"pending_count": pendingCount,
			"pending_usd":   pendingUSD,
			"paid_usd":      paidUSD,
			"failed_usd":    failedUSD,
		},
	})
}

func (a *app) handleWithdrawalAdd(w http.ResponseWriter, r *http.Request) {
	var req withdrawalRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.AmountUSD <= 0 {
		writeErr(w, http.StatusBadRequest, "金额必须大于 0")
		return
	}
	site, err := a.db.GetSite(req.SiteID)
	if errors.Is(err, errSiteNotFound) {
		writeErr(w, http.StatusBadRequest, "站点不存在，请先添加站点")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	wd := &Withdrawal{
		SiteID:    site.ID,
		SiteName:  site.Name,
		AmountUSD: req.AmountUSD,
		Channel:   strings.TrimSpace(req.Channel),
		Note:      strings.TrimSpace(req.Note),
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	if err := a.db.AddWithdrawal(wd); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, wd)
}

func (a *app) handleWithdrawalUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	var req withdrawalRequest
	if !readJSON(w, r, &req) {
		return
	}
	wd, err := a.db.UpdateWithdrawal(id, func(wd *Withdrawal) error {
		if req.AmountUSD > 0 {
			wd.AmountUSD = req.AmountUSD
		}
		if ch := strings.TrimSpace(req.Channel); ch != "" {
			wd.Channel = ch
		}
		if nt := strings.TrimSpace(req.Note); nt != "" {
			wd.Note = nt
		}
		switch req.Status {
		case "", wd.Status:
			// 未提供或未变化
		case "pending", "paid", "failed":
			wd.Status = req.Status
			if req.Status == "pending" {
				wd.ResolvedAt = time.Time{}
			} else {
				wd.ResolvedAt = time.Now()
			}
		default:
			return errors.New("无效的状态，仅支持 pending/paid/failed")
		}
		return nil
	})
	if errors.Is(err, errWithdrawalNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, wd)
}

func (a *app) handleWithdrawalDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	err := a.db.DeleteWithdrawal(id)
	if errors.Is(err, errWithdrawalNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, nil)
}

func (a *app) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的站点 ID")
		return 0, false
	}
	return id, true
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "请求需为 JSON")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
