// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"sort"
	"time"
)

type trendPoint struct {
	T       time.Time `json:"t"`
	Income  int64     `json:"income"`  // 各站累计推广收益之和（快照对齐）
	Balance int64     `json:"balance"` // 各站可用余额之和（快照对齐）
}

// BuildTrend 将各站快照重采样为全局时间序列：每个时点取各站「不晚于该时点」的最新快照求和。
// 时点从最早快照均匀铺到当前时刻；目标步长 <6h 用 10 分钟、<72h 用 30 分钟、否则按天，点数上限 400。
func BuildTrend(snaps []Snapshot, now time.Time, days int) (string, []trendPoint) {
	if len(snaps) == 0 {
		return "day", nil
	}
	oldest := snaps[0].Time
	for _, sn := range snaps {
		if sn.Time.Before(oldest) {
			oldest = sn.Time
		}
	}
	// 只看最近 days 天
	if limit := now.AddDate(0, 0, -days); limit.After(oldest) {
		oldest = limit
	}
	span := now.Sub(oldest)
	if span <= 0 {
		return "hour", []trendPoint{{T: now}}
	}

	var step time.Duration
	var bucket string
	switch {
	case span < 6*time.Hour:
		step, bucket = 10*time.Minute, "hour"
	case span < 72*time.Hour:
		step, bucket = 30*time.Minute, "hour"
	default:
		step, bucket = 24*time.Hour, "day"
	}

	count := int(span/step) + 1
	if count < 2 {
		count = 2
	}
	if count > 400 {
		count = 400
	}
	stepEff := span / time.Duration(count-1)
	if stepEff <= 0 {
		stepEff = time.Second
	}
	ts := make([]time.Time, 0, count)
	for i := 0; i < count-1; i++ {
		ts = append(ts, oldest.Add(stepEff*time.Duration(i)))
	}
	ts = append(ts, now)

	bySite := make(map[int64][]Snapshot)
	for _, sn := range snaps {
		bySite[sn.SiteID] = append(bySite[sn.SiteID], sn)
	}
	type cursor struct {
		snaps []Snapshot
		i     int
	}
	cursors := make([]cursor, 0, len(bySite))
	for _, arr := range bySite {
		sort.Slice(arr, func(a, b int) bool { return arr[a].Time.Before(arr[b].Time) })
		cursors = append(cursors, cursor{snaps: arr})
	}

	points := make([]trendPoint, 0, len(ts))
	for _, t := range ts {
		var income, balance int64
		for ci := range cursors {
			c := &cursors[ci]
			for c.i < len(c.snaps) && !c.snaps[c.i].Time.After(t) {
				c.i++
			}
			if c.i > 0 {
				income += c.snaps[c.i-1].AffHistory
				balance += c.snaps[c.i-1].Quota
			}
		}
		points = append(points, trendPoint{T: t, Income: income, Balance: balance})
	}
	return bucket, points
}
