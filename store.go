// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// snapshotKeepPerSite 每个站点保留的历史快照条数（30 分钟一次约 10 天）。
const snapshotKeepPerSite = 500

var errSiteNotFound = errors.New("站点不存在")
var errWithdrawalNotFound = errors.New("提现记录不存在")

type Settings struct {
	SaltHex         string  `json:"salt_hex"`
	Verifier        string  `json:"verifier,omitempty"`
	QuotaPerUnit    float64 `json:"quota_per_unit"`
	SyncIntervalMin int     `json:"sync_interval_min"`

	// Telegram 通知
	TGBotTokenEnc   string `json:"tg_bot_token_enc,omitempty"` // AES-GCM 密文，不返回给前端
	TGChatID        string `json:"tg_chat_id"`
	TGAPIBase       string `json:"tg_api_base"`                // 高级：自建 Bot API，默认官方
	TGProxyURL      string `json:"tg_proxy_url"`               // 可选代理 socks5:// 或 http://
	TGNotifyIncome  bool   `json:"tg_notify_income"`           // 进账提醒
	TGNotifyDown    bool   `json:"tg_notify_down"`             // 跑路预警与恢复
	TGDownThreshold int    `json:"tg_down_threshold"`          // 连续失败多少次触发预警
}

type Site struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Type    string `json:"type"`
	UserID  int64  `json:"user_id"`

	// TokenEnc 是访问令牌的 AES-GCM 密文（hex），不会出现在任何 API 响应里。
	TokenEnc string `json:"token_enc,omitempty"`
	Enabled  bool   `json:"enabled"`

	// Type=manual（手动记录，不接 API）时由用户维护的额度，单位 quota。
	// 数值以美元录入，按全局 QuotaPerUnit 换算；修改后会立即落一条快照。
	ManualQuota      int64 `json:"manual_quota,omitempty"`
	ManualAffQuota   int64 `json:"manual_aff_quota,omitempty"`
	ManualAffHistory int64 `json:"manual_aff_history,omitempty"`

	// 提现入口：WithdrawURL 为「申请提现」时打开的页面（默认 {base}/console/wallet）；
	// WithdrawAPI 为实验性的站方提现接口路径（个别魔改站才有，配置后一键提交申请）。
	WithdrawURL string `json:"withdraw_url,omitempty"`
	WithdrawAPI string `json:"withdraw_api,omitempty"`

	// AffCode 邀请码：自动站随同步更新，手动站可手填；邀请链接 = {base}/register?aff={code}。
	AffCode string `json:"aff_code,omitempty"`

	LastSyncAt time.Time `json:"last_sync_at"`
	LastSyncOK bool      `json:"last_sync_ok"`
	LastError  string    `json:"last_error,omitempty"`
	// DownCount 连续同步失败次数，成功后清零；达到阈值触发跑路预警。
	DownCount int64 `json:"down_count"`
}

// Snapshot 记录一次同步采集的额度数据，单位为站点原始 quota。
type Snapshot struct {
	SiteID int64     `json:"site_id"`
	Time   time.Time `json:"time"`

	Quota        int64 `json:"quota"`        // 可用余额
	UsedQuota    int64 `json:"used_quota"`   // 已用额度
	AffQuota     int64 `json:"aff_quota"`    // 邀请奖励余额（待转）
	AffHistory   int64 `json:"aff_history"`  // 累计邀请奖励
	AffCount     int64 `json:"aff_count"`    // 邀请注册人数
	RequestCount int64 `json:"request_count"`
}

// siteView 是返回给前端的站点视图（不含密文）。
type siteView struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	BaseURL    string    `json:"base_url"`
	Type       string    `json:"type"`
	UserID     int64     `json:"user_id"`
	Enabled    bool      `json:"enabled"`
	HasToken   bool      `json:"has_token"`
	LastSyncAt time.Time `json:"last_sync_at"`
	LastSyncOK bool      `json:"last_sync_ok"`
	LastError  string    `json:"last_error,omitempty"`
	DownCount  int64     `json:"down_count"`
	Latest     *Snapshot `json:"latest,omitempty"`
	Spark      []sparkPoint `json:"spark,omitempty"`
	ManualUSD  *manualUSD   `json:"manual_usd,omitempty"`
	// WithdrawURL 返回实际生效的提现页地址（用户配置 > 默认 {base}/console/wallet）。
	WithdrawURL       string `json:"withdraw_url,omitempty"`
	WithdrawURLCustom bool   `json:"withdraw_url_custom"`
	HasWithdrawAPI    bool   `json:"has_withdraw_api"`
	WithdrawAPI       string `json:"withdraw_api,omitempty"`
	AffCode           string `json:"aff_code,omitempty"`
}

type sparkPoint struct {
	T time.Time `json:"t"`
	V int64     `json:"v"` // aff_history
}

type manualUSD struct {
	Quota float64 `json:"quota"`
	Aff   float64 `json:"aff"`
	Hist  float64 `json:"hist"`
}

// Withdrawal 是一笔现金提现台账记录（中转站多为人工打款，无法 API 化，故只做记账与提醒）。
type Withdrawal struct {
	ID         int64     `json:"id"`
	SiteID     int64     `json:"site_id"`
	SiteName   string    `json:"site_name"`
	AmountUSD  float64   `json:"amount_usd"`
	Channel    string    `json:"channel"`
	Note       string    `json:"note,omitempty"`
	Status     string    `json:"status"` // pending 待到账 | paid 已到账 | failed 失败
	CreatedAt  time.Time `json:"created_at"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

type dbFile struct {
	Settings    Settings     `json:"settings"`
	NextID      int64        `json:"next_id"`
	NextWID     int64        `json:"next_wid"`
	Sites       []*Site      `json:"sites"`
	Snapshots   []Snapshot   `json:"snapshots"`
	Withdrawals []Withdrawal `json:"withdrawals"`
}

// DB 是一个互斥锁保护的 JSON 文件存储，写入采用「临时文件 + rename」保证原子性。
type DB struct {
	mu   sync.Mutex
	path string
	f    dbFile
}

func OpenDB(path string) (*DB, error) {
	db := &DB{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		db.f = dbFile{Settings: Settings{
			QuotaPerUnit:    500000,
			SyncIntervalMin: 30,
			TGDownThreshold: 3,
			TGNotifyIncome:  true,
			TGNotifyDown:    true,
		}}
		return db, db.Save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &db.f); err != nil {
		return nil, fmt.Errorf("数据文件解析失败 %s: %w", path, err)
	}
	if db.f.Settings.QuotaPerUnit <= 0 {
		db.f.Settings.QuotaPerUnit = 500000
	}
	if db.f.Settings.SyncIntervalMin <= 0 {
		db.f.Settings.SyncIntervalMin = 30
	}
	if db.f.Settings.TGDownThreshold <= 0 {
		// 引入通知功能时对既有数据文件的迁移默认值；用户保存过设置（阈值>=1）后不再触发。
		db.f.Settings.TGDownThreshold = 3
		db.f.Settings.TGNotifyIncome = true
		db.f.Settings.TGNotifyDown = true
		_ = db.saveLocked()
	}
	// 站点类型从单一 "new-api" 迁移为 "auto"（兼容 new-api/one-api/one-hub 系）。
	migrated := false
	for _, s := range db.f.Sites {
		if s.Type == "new-api" {
			s.Type = "auto"
			migrated = true
		}
	}
	if migrated {
		_ = db.saveLocked()
	}
	return db, nil
}

func (db *DB) Save() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.saveLocked()
}

func (db *DB) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(db.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(db.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, db.path)
}

func (db *DB) Settings() Settings {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.f.Settings
}

func (db *DB) SetSaltVerifier(saltHex, verifier string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.f.Settings.SaltHex = saltHex
	db.f.Settings.Verifier = verifier
	return db.saveLocked()
}

func (db *DB) UpdateSettings(apply func(*Settings)) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	apply(&db.f.Settings)
	if db.f.Settings.QuotaPerUnit <= 0 {
		db.f.Settings.QuotaPerUnit = 500000
	}
	if db.f.Settings.SyncIntervalMin < 5 {
		db.f.Settings.SyncIntervalMin = 5
	}
	return db.saveLocked()
}

func (db *DB) findLocked(id int64) *Site {
	for _, s := range db.f.Sites {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// Path 返回数据文件路径（导出用）。
func (db *DB) Path() string { return db.path }

// Replace 用导入的数据整体替换当前内容，并做基本规范化。
// 调用方负责随后清空内存中的密钥（令牌是用导出方主密码加密的）。
func (db *DB) Replace(f dbFile) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if f.Settings.QuotaPerUnit <= 0 {
		f.Settings.QuotaPerUnit = 500000
	}
	if f.Settings.SyncIntervalMin <= 0 {
		f.Settings.SyncIntervalMin = 30
	}
	if f.Settings.TGDownThreshold <= 0 {
		f.Settings.TGDownThreshold = 3
		f.Settings.TGNotifyIncome = true
		f.Settings.TGNotifyDown = true
	}
	db.f = f
	return db.saveLocked()
}

func (db *DB) AddSite(s *Site) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.f.NextID++
	s.ID = db.f.NextID
	db.f.Sites = append(db.f.Sites, s)
	return db.saveLocked()
}

// GetSite 返回站点副本，避免外部直接改动内部状态。
func (db *DB) GetSite(id int64) (Site, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	s := db.findLocked(id)
	if s == nil {
		return Site{}, errSiteNotFound
	}
	return *s, nil
}

func (db *DB) UpdateSite(id int64, apply func(*Site) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s := db.findLocked(id)
	if s == nil {
		return errSiteNotFound
	}
	if err := apply(s); err != nil {
		return err
	}
	return db.saveLocked()
}

func (db *DB) DeleteSite(id int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	idx := -1
	for i, s := range db.f.Sites {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errSiteNotFound
	}
	db.f.Sites = append(db.f.Sites[:idx], db.f.Sites[idx+1:]...)
	kept := db.f.Snapshots[:0]
	for _, sn := range db.f.Snapshots {
		if sn.SiteID != id {
			kept = append(kept, sn)
		}
	}
	db.f.Snapshots = kept
	return db.saveLocked()
}

// ListSites 返回所有站点的副本列表。
func (db *DB) ListSites() []Site {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]Site, 0, len(db.f.Sites))
	for _, s := range db.f.Sites {
		out = append(out, *s)
	}
	return out
}

func (db *DB) AddSnapshot(snap Snapshot) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.f.Snapshots = append(db.f.Snapshots, snap)
	// 快照按时间追加，从旧到新遍历，超出每站点配额的旧数据直接丢弃。
	counts := make(map[int64]int)
	kept := make([]Snapshot, 0, len(db.f.Snapshots))
	for _, sn := range db.f.Snapshots {
		counts[sn.SiteID]++
		if counts[sn.SiteID] > snapshotKeepPerSite {
			continue
		}
		kept = append(kept, sn)
	}
	db.f.Snapshots = kept
	_ = db.saveLocked()
}

// AllSnapshots 返回全部快照的副本（供趋势重采样）。
func (db *DB) AllSnapshots() []Snapshot {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]Snapshot, len(db.f.Snapshots))
	copy(out, db.f.Snapshots)
	return out
}

func (db *DB) SiteViews() []siteView {
	db.mu.Lock()
	defer db.mu.Unlock()
	latest := make(map[int64]*Snapshot)
	sparks := make(map[int64][]sparkPoint)
	for i := range db.f.Snapshots {
		sn := &db.f.Snapshots[i]
		if cur := latest[sn.SiteID]; cur == nil || sn.Time.After(cur.Time) {
			latest[sn.SiteID] = sn
		}
		// 迷你趋势只保留最近 30 个点（快照按时间追加）。
		arr := sparks[sn.SiteID]
		if len(arr) >= 30 {
			copy(arr, arr[1:])
			arr[len(arr)-1] = sparkPoint{T: sn.Time, V: sn.AffHistory}
			sparks[sn.SiteID] = arr
		} else {
			sparks[sn.SiteID] = append(arr, sparkPoint{T: sn.Time, V: sn.AffHistory})
		}
	}
	per := db.f.Settings.QuotaPerUnit
	if per <= 0 {
		per = 500000
	}
	out := make([]siteView, 0, len(db.f.Sites))
	for _, s := range db.f.Sites {
		v := siteView{
			ID:         s.ID,
			Name:       s.Name,
			BaseURL:    s.BaseURL,
			Type:       s.Type,
			UserID:     s.UserID,
			Enabled:    s.Enabled,
			HasToken:   s.TokenEnc != "",
			LastSyncAt: s.LastSyncAt,
			LastSyncOK: s.LastSyncOK,
			LastError:  s.LastError,
			DownCount:  s.DownCount,
			Spark:      sparks[s.ID],
		}
		if sn := latest[s.ID]; sn != nil {
			cp := *sn
			v.Latest = &cp
		}
		if s.Type == "manual" {
			v.ManualUSD = &manualUSD{
				Quota: float64(s.ManualQuota) / per,
				Aff:   float64(s.ManualAffQuota) / per,
				Hist:  float64(s.ManualAffHistory) / per,
			}
		}
		if s.WithdrawURL != "" {
			v.WithdrawURL = s.WithdrawURL
			v.WithdrawURLCustom = true
		} else if s.BaseURL != "" {
			v.WithdrawURL = strings.TrimRight(s.BaseURL, "/") + "/console/wallet"
		}
		v.HasWithdrawAPI = s.WithdrawAPI != ""
		v.WithdrawAPI = s.WithdrawAPI
		v.AffCode = s.AffCode
		out = append(out, v)
	}
	return out
}

func (db *DB) SiteView(id int64) (siteView, bool) {
	for _, v := range db.SiteViews() {
		if v.ID == id {
			return v, true
		}
	}
	return siteView{}, false
}

// SiteHistory 返回某站点最近的快照，新的在前。
func (db *DB) SiteHistory(id int64, limit int) []Snapshot {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]Snapshot, 0, limit)
	for i := len(db.f.Snapshots) - 1; i >= 0 && len(out) < limit; i-- {
		if db.f.Snapshots[i].SiteID == id {
			out = append(out, db.f.Snapshots[i])
		}
	}
	return out
}

func (db *DB) AddWithdrawal(wd *Withdrawal) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.f.NextWID++
	wd.ID = db.f.NextWID
	db.f.Withdrawals = append(db.f.Withdrawals, *wd)
	return db.saveLocked()
}

// UpdateWithdrawal 更新记录并返回更新后的副本。
func (db *DB) UpdateWithdrawal(id int64, apply func(*Withdrawal) error) (Withdrawal, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.f.Withdrawals {
		if db.f.Withdrawals[i].ID == id {
			if err := apply(&db.f.Withdrawals[i]); err != nil {
				return Withdrawal{}, err
			}
			cp := db.f.Withdrawals[i]
			return cp, db.saveLocked()
		}
	}
	return Withdrawal{}, errWithdrawalNotFound
}

func (db *DB) DeleteWithdrawal(id int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	idx := -1
	for i, wd := range db.f.Withdrawals {
		if wd.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errWithdrawalNotFound
	}
	db.f.Withdrawals = append(db.f.Withdrawals[:idx], db.f.Withdrawals[idx+1:]...)
	return db.saveLocked()
}

// ListWithdrawals 返回全部提现记录，新的在前。
func (db *DB) ListWithdrawals() []Withdrawal {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]Withdrawal, 0, len(db.f.Withdrawals))
	for i := len(db.f.Withdrawals) - 1; i >= 0; i-- {
		out = append(out, db.f.Withdrawals[i])
	}
	return out
}
