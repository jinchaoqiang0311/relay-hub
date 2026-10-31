// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"errors"
	"log"
	"math"
	"sync"
	"time"
)

// Syncer 负责定时与手动同步，并持有解锁后的密码库。
type Syncer struct {
	db       *DB
	client   *apiClient
	notifier *Notifier

	mu      sync.Mutex
	vault   *Vault
	running bool
}

func (s *Syncer) setVault(v *Vault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vault = v
}

func (s *Syncer) vaultReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vault.ready()
}

func (s *Syncer) currentVault() *Vault {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vault
}

func (s *Syncer) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Syncer) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Syncer) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
}

// SyncAll 同步所有启用站点，串行执行以避免给站方压力。
func (s *Syncer) SyncAll() (okCount, failCount int) {
	if !s.vaultReady() || !s.begin() {
		return 0, 0
	}
	defer s.finish()
	for _, st := range s.db.ListSites() {
		if !st.Enabled || st.Type == "manual" {
			continue
		}
		if err := s.SyncSite(st.ID); err != nil {
			failCount++
			log.Printf("[sync] %s(#%d) 同步失败: %v", st.Name, st.ID, err)
		} else {
			okCount++
		}
	}
	return
}

// SyncSite 同步单个站点：探测接口 -> 更新状态 -> 落一条快照。
func (s *Syncer) SyncSite(id int64) error {
	v := s.currentVault()
	if !v.ready() {
		return errors.New("密码库未解锁")
	}
	site, err := s.db.GetSite(id)
	if err != nil {
		return err
	}
	if site.Type == "manual" {
		return errors.New("手动站点不参与自动同步，请通过「编辑」更新数值")
	}
	token, err := v.openString(site.TokenEnc)
	if err != nil {
		s.markFailure(id, "读取令牌失败: "+err.Error())
		return err
	}
	if site.Type == "sub2api" {
		return s.syncSub2Api(site, token)
	}
	info, err := s.client.Probe(site.BaseURL, token, site.UserID)
	if err != nil {
		s.markFailure(id, err.Error())
		return err
	}
	// 通知逻辑基于「上一条快照」做差值，取数要在落新快照之前。
	prevSnaps := s.db.SiteHistory(id, 1)
	var prev *Snapshot
	if len(prevSnaps) > 0 {
		prev = &prevSnaps[0]
	}
	wasDown := site.DownCount > 0 && site.DownCount >= int64(s.db.Settings().TGDownThreshold)

	now := time.Now()
	if err := s.db.UpdateSite(id, func(st *Site) error {
		st.LastSyncAt = now
		st.LastSyncOK = true
		st.LastError = ""
		st.DownCount = 0
		if info.AffCode != "" {
			st.AffCode = info.AffCode
		}
		return nil
	}); err != nil {
		return err
	}
	snap := Snapshot{
		SiteID:       id,
		Time:         now,
		Quota:        info.Quota,
		UsedQuota:    info.UsedQuota,
		AffQuota:     info.AffQuota,
		AffHistory:   info.AffHistory,
		AffCount:     info.AffCount,
		RequestCount: info.RequestCount,
	}
	s.db.AddSnapshot(snap)
	if wasDown {
		s.notifier.NotifyRecover(v, site, site.DownCount)
	}
	if prev != nil {
		s.notifier.NotifyIncome(v, site, *prev, snap)
	}
	return nil
}

// syncSub2Api 同步 Sub2API 站点：auth/me 取美元余额换算 quota，顺带尽力取邀请码。
// 注意：Sub2API 的邀请收益不在 auth/me 里，暂无 aff_history 来源，进账提醒对这类站不生效。
func (s *Syncer) syncSub2Api(site Site, token string) error {
	me, err := s.client.Sub2ApiMe(site.BaseURL, token)
	if err != nil {
		s.markFailure(site.ID, err.Error())
		return err
	}
	wasDown := site.DownCount > 0 && site.DownCount >= int64(s.db.Settings().TGDownThreshold)
	per := s.db.Settings().QuotaPerUnit
	affCode := s.client.Sub2ApiAffCode(site.BaseURL, token) // 网络调用放在拿锁之前
	now := time.Now()
	if err := s.db.UpdateSite(site.ID, func(st *Site) error {
		st.LastSyncAt = now
		st.LastSyncOK = true
		st.LastError = ""
		st.DownCount = 0
		if affCode != "" {
			st.AffCode = affCode
		}
		return nil
	}); err != nil {
		return err
	}
	s.db.AddSnapshot(Snapshot{
		SiteID: site.ID,
		Time:   now,
		Quota:  int64(math.Round(me.Balance * per)),
	})
	if wasDown {
		s.notifier.NotifyRecover(s.currentVault(), site, site.DownCount)
	}
	return nil
}

func (s *Syncer) markFailure(id int64, msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	var updated Site
	_ = s.db.UpdateSite(id, func(st *Site) error {
		st.LastSyncAt = time.Now()
		st.LastSyncOK = false
		st.LastError = msg
		st.DownCount++
		updated = *st
		return nil
	})
	// 恰好达到阈值时预警一次，之后不再重复，恢复后重新计数。
	set := s.db.Settings()
	if set.TGNotifyDown && set.TGDownThreshold > 0 && updated.DownCount == int64(set.TGDownThreshold) {
		if v := s.currentVault(); v.ready() {
			s.notifier.NotifyDown(v, updated, updated.DownCount, updated.LastError)
		}
	}
}

// Run 周期性自动同步，仅在解锁状态下工作。
func (s *Syncer) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var lastRun time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !s.vaultReady() {
			continue
		}
		set := s.db.Settings()
		interval := time.Duration(max(set.SyncIntervalMin, 5)) * time.Minute
		if !lastRun.IsZero() && time.Since(lastRun) < interval {
			continue
		}
		lastRun = time.Now()
		okN, failN := s.SyncAll()
		if okN+failN > 0 {
			log.Printf("[sync] 自动同步完成: %d 成功, %d 失败", okN, failN)
		}
	}
}
