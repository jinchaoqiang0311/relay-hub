// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultTGAPIBase = "https://api.telegram.org"

// Notifier 发送 Telegram 通知。配置缺失或网络故障只记日志，绝不影响同步主流程。
type Notifier struct {
	db *DB
}

func newNotifier(db *DB) *Notifier { return &Notifier{db: db} }

func (n *Notifier) httpClient(proxyURL string) (*http.Client, error) {
	t := &http.Transport{}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("代理地址无效: %w", err)
		}
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: t}, nil
}

func (n *Notifier) apiBase(set Settings) string {
	if b := strings.TrimRight(strings.TrimSpace(set.TGAPIBase), "/"); b != "" {
		return b
	}
	return defaultTGAPIBase
}

// tgCall 调用 Bot API 并返回 result 原始 JSON。
func tgCall(cl *http.Client, apiBase, botToken, method string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", apiBase, botToken, method)
	resp, err := cl.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("无法连接 Telegram API: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("Telegram API 响应异常（HTTP %d）", resp.StatusCode)
	}
	if !out.OK {
		desc := out.Description
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, errors.New("Telegram API: " + desc)
	}
	return out.Result, nil
}

// send 读取设置并投递一条消息；未配置或未解锁时返回错误由调用方决定是否忽略。
func (n *Notifier) send(v *Vault, text string) error {
	set := n.db.Settings()
	if set.TGBotTokenEnc == "" || set.TGChatID == "" {
		return errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	if !v.ready() {
		return errors.New("密码库未解锁")
	}
	token, err := v.openString(set.TGBotTokenEnc)
	if err != nil {
		return fmt.Errorf("读取 Bot Token 失败: %w", err)
	}
	cl, err := n.httpClient(set.TGProxyURL)
	if err != nil {
		return err
	}
	_, err = tgCall(cl, n.apiBase(set), token, "sendMessage", map[string]any{
		"chat_id": set.TGChatID,
		"text":    text,
	})
	return err
}

// GetChatID 调 getUpdates，取最近一次对话的 chat_id（用户先给 Bot 发条消息）。
func (n *Notifier) GetChatID(v *Vault, botToken string) (string, error) {
	set := n.db.Settings()
	if botToken == "" {
		if set.TGBotTokenEnc == "" {
			return "", errors.New("请先填写 Bot Token（@BotFather 创建）")
		}
		if !v.ready() {
			return "", errors.New("密码库未解锁")
		}
		t, err := v.openString(set.TGBotTokenEnc)
		if err != nil {
			return "", err
		}
		botToken = t
	}
	cl, err := n.httpClient(set.TGProxyURL)
	if err != nil {
		return "", err
	}
	res, err := tgCall(cl, n.apiBase(set), botToken, "getUpdates", map[string]any{"limit": 100})
	if err != nil {
		return "", err
	}
	var updates []struct {
		Message struct {
			Chat struct {
				ID float64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}
	if err := json.Unmarshal(res, &updates); err != nil {
		return "", errors.New("无法解析 getUpdates 结果")
	}
	for i := len(updates) - 1; i >= 0; i-- {
		id := updates[i].Message.Chat.ID
		if id != 0 {
			return strconv.FormatInt(int64(id), 10), nil
		}
	}
	return "", errors.New("未找到对话：请先在 Telegram 中给你的 Bot 发送任意一条消息，再点「自动获取 Chat ID」")
}

// Test 发送测试消息，错误直接返回给前端展示。
func (n *Notifier) Test(v *Vault) error {
	return n.send(v, "✅ RelayHub 测试通知：连接正常。进账提醒与跑路预警将发送到这里。")
}

// NotifyCheckIn 自动签到成功后推送提醒（归入进账类通知开关）。
func (n *Notifier) NotifyCheckIn(v *Vault, siteName, detail string) {
	set := n.db.Settings()
	if !set.TGNotifyIncome {
		return
	}
	n.deliver(v, siteName, "📅 自动签到\n站点："+siteName+"\n"+detail)
}

func usd(quota int64, perUnit float64) string {
	if perUnit <= 0 {
		perUnit = 500000
	}
	v := float64(quota) / perUnit
	digits := 4
	if math.Abs(v) >= 1 {
		digits = 3
	}
	if math.Abs(v) >= 100 {
		digits = 2
	}
	return fmt.Sprintf("$%.*f", digits, v)
}

// NotifyIncome 累计收益或邀请人数增长时推送进账提醒。
func (n *Notifier) NotifyIncome(v *Vault, site Site, prev, cur Snapshot) {
	set := n.db.Settings()
	if !set.TGNotifyIncome {
		return
	}
	delta := cur.AffHistory - prev.AffHistory
	affDelta := cur.AffCount - prev.AffCount
	if delta <= 0 && affDelta <= 0 {
		return
	}
	var b strings.Builder
	b.WriteString("💰 推广收益进账\n")
	b.WriteString("站点：" + site.Name + "\n")
	if delta > 0 {
		fmt.Fprintf(&b, "累计收益：%s → %s（+%s）\n",
			usd(prev.AffHistory, set.QuotaPerUnit), usd(cur.AffHistory, set.QuotaPerUnit), usd(delta, set.QuotaPerUnit))
	}
	if affDelta > 0 {
		fmt.Fprintf(&b, "邀请人数：%d → %d（+%d）\n", prev.AffCount, cur.AffCount, affDelta)
	}
	fmt.Fprintf(&b, "可用余额：%s ｜ 待转奖励：%s",
		usd(cur.Quota, set.QuotaPerUnit), usd(cur.AffQuota, set.QuotaPerUnit))
	n.deliver(v, site.Name, b.String())
}

// NotifyDown 站点连续失败达到阈值时推送跑路预警（每次故障只推一次）。
func (n *Notifier) NotifyDown(v *Vault, site Site, count int64, lastErr string) {
	set := n.db.Settings()
	if !set.TGNotifyDown {
		return
	}
	var b strings.Builder
	b.WriteString("🚨 跑路预警：站点连续同步失败\n")
	b.WriteString("站点：" + site.Name + "（" + site.BaseURL + "）\n")
	fmt.Fprintf(&b, "已连续 %d 次同步失败（阈值 %d）\n", count, set.TGDownThreshold)
	b.WriteString("最近错误：" + lastErr + "\n\n")
	b.WriteString("建议尽快登录站点确认状态；若站点仍在运营，先到「钱包」查看余额与提现入口，必要时把邀请奖励转余额止损。")
	n.deliver(v, site.Name, b.String())
}

// NotifyRecover 站点从持续失败中恢复时推送通知。
func (n *Notifier) NotifyRecover(v *Vault, site Site, failedCount int64) {
	set := n.db.Settings()
	if !set.TGNotifyDown {
		return
	}
	n.deliver(v, site.Name, fmt.Sprintf(
		"✅ 站点恢复\n站点：%s\n连续失败 %d 次后已恢复同步。", site.Name, failedCount))
}

func (n *Notifier) deliver(v *Vault, siteName, text string) {
	go func() {
		if err := n.send(v, text); err != nil {
			log.Printf("[notify] %s 通知发送失败: %v", siteName, err)
		} else {
			log.Printf("[notify] 已发送通知: %s", siteName)
		}
	}()
}
