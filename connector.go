// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SelfInfo 汇总 /api/user/self 与 /api/status 的关键信息。
// 各分叉字段命名略有差异，缺失的字段保持零值。
type SelfInfo struct {
	SystemName   string `json:"system_name"`
	Version      string `json:"version"`
	Username     string `json:"username"`
	Quota        int64  `json:"quota"`
	UsedQuota    int64  `json:"used_quota"`
	RequestCount int64  `json:"request_count"`
	AffQuota     int64  `json:"aff_quota"`
	AffHistory   int64  `json:"aff_history_quota"`
	AffCount     int64  `json:"aff_count"`
	AffCode      string `json:"aff_code"`
}

type apiEnvelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type apiClient struct {
	http *http.Client
}

func newAPIClient() *apiClient {
	return &apiClient{http: &http.Client{Timeout: 20 * time.Second}}
}

// normalizeBaseURL 补全协议并去掉结尾斜杠。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("站点地址不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("站点地址无效: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("站点地址仅支持 http/https")
	}
	if u.Host == "" {
		return "", errors.New("站点地址缺少域名")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// request 发起请求并解析 new-api 系统一的 {success, message, data} 信封。
func (c *apiClient) request(method, endpoint string, hdrs map[string]string) (*apiEnvelope, int, error) {
	return doEnvelope(c.http, method, endpoint, hdrs, "{}")
}

// doEnvelope 发起请求并解析统一信封；body 仅对 POST 生效，传空串时使用 "{}"。
func doEnvelope(cl *http.Client, method, endpoint string, hdrs map[string]string, body string) (*apiEnvelope, int, error) {
	var rd io.Reader
	if method == http.MethodPost {
		if body == "" {
			body = "{}"
		}
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接站点: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败: %w", err)
	}
	env := &apiEnvelope{}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("响应不是 JSON（HTTP %d），该站可能不是 new-api/one-api 系", resp.StatusCode)
	}
	return env, resp.StatusCode, nil
}

// authHeaders 生成兼容 new-api / Veloera 分叉的鉴权头。
func authHeaders(token string, userID int64) map[string]string {
	uid := strconv.FormatInt(userID, 10)
	return map[string]string{
		"Authorization": "Bearer " + token,
		"New-Api-User":  uid,
		"Veloera-User":  uid,
	}
}

func (c *apiClient) fetchSelf(baseURL, token string, userID int64) (*SelfInfo, error) {
	endpoint := baseURL + "/api/user/self"
	hdrs := authHeaders(token, userID)
	env, status, err := c.request(http.MethodGet, endpoint, hdrs)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 个别 one-api 系分叉不识别 Bearer 前缀，去掉后重试一次
		hdrs["Authorization"] = token
		env, status, err = c.request(http.MethodGet, endpoint, hdrs)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("用户信息接口返回 HTTP %d（请检查访问令牌与用户 ID）", status)
	}
	if !env.Success {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "站点未返回错误原因"
		}
		return nil, errors.New("站点返回: " + msg)
	}
	var info SelfInfo
	if err := json.Unmarshal(env.Data, &info); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %w", err)
	}
	return &info, nil
}

// Probe 拉取用户额度信息，并尽力补充站点名称与版本。
func (c *apiClient) Probe(baseURL, token string, userID int64) (*SelfInfo, error) {
	info, err := c.fetchSelf(baseURL, token, userID)
	if err != nil {
		return nil, err
	}
	if env, status, err := c.request(http.MethodGet, baseURL+"/api/status", nil); err == nil && status == http.StatusOK && env.Success {
		var meta struct {
			SystemName string `json:"system_name"`
			Version    string `json:"version"`
		}
		if json.Unmarshal(env.Data, &meta) == nil {
			if meta.SystemName != "" {
				info.SystemName = meta.SystemName
			}
			if meta.Version != "" {
				info.Version = meta.Version
			}
		}
	}
	return info, nil
}

// autoTokenResult 是「账号密码自动获取令牌」的返回结果。
type autoTokenResult struct {
	SelfInfo
	Token  string `json:"token"`
	UserID int64  `json:"user_id"`
}

// LoginAutoToken 用账号密码登录站点并重新生成系统访问令牌，返回令牌、用户 ID 与额度信息。
// 密码只在本次请求的内存中使用，不落盘、不写日志。
// 注意：生成是「重新生成」语义，站点上已有的旧访问令牌会随之失效。
func (c *apiClient) LoginAutoToken(baseURL, username, password string) (*autoTokenResult, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	sess := &http.Client{Timeout: 20 * time.Second, Jar: jar}

	// 1. 站点状态：启用人机验证的站无法自动登录
	env, status, err := doEnvelope(c.http, http.MethodGet, baseURL+"/api/status", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("站点状态接口返回 HTTP %d", status)
	}
	var sys struct {
		TurnstileCheck bool `json:"turnstile_check"`
	}
	_ = json.Unmarshal(env.Data, &sys)
	if sys.TurnstileCheck {
		return nil, errors.New("该站登录启用了人机验证（Turnstile），无法自动获取；请在站点「个人设置」里手动生成令牌")
	}

	// 2. 登录（用户名或邮箱，取决于站方配置）
	loginBody, _ := json.Marshal(map[string]string{"username": username, "password": password})
	env, status, err = doEnvelope(sess, http.MethodPost, baseURL+"/api/user/login", nil, string(loginBody))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("登录接口返回 HTTP %d", status)
	}
	if !env.Success {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "用户名或密码错误"
		}
		return nil, errors.New("登录失败: " + msg)
	}
	var lu struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &lu); err != nil || lu.ID == 0 {
		return nil, errors.New("登录成功但未能解析用户 ID，请改用手动方式")
	}

	// 3. 生成系统访问令牌（旧令牌随之失效）
	env, status, err = doEnvelope(sess, http.MethodGet, baseURL+"/api/user/token", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || !env.Success {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return nil, errors.New("生成令牌失败: " + msg)
	}
	var token string
	_ = json.Unmarshal(env.Data, &token)
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("站点未返回令牌，请改用手动方式")
	}

	// 4. 用新令牌拉取额度信息（Probe 顺带带出站名/版本），并验证令牌可用
	info, err := c.Probe(baseURL, token, lu.ID)
	if err != nil {
		return nil, fmt.Errorf("令牌已生成但拉取用户信息失败: %w", err)
	}
	return &autoTokenResult{SelfInfo: *info, Token: token, UserID: lu.ID}, nil
}

// ---------- 签到（new-api 系 + Sub2API，端点参考 all-api-hub 适配层） ----------

// NewApiCheckInEnabled 读站点公开状态里的签到开关。
// new-api 用 checkin_enabled，Veloera 用 check_in_enabled（下划线位置不同，参考其 controller/misc.go）。
func (c *apiClient) NewApiCheckInEnabled(baseURL string) bool {
	env, status, err := c.request(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/status", nil)
	if err != nil || status != http.StatusOK || !env.Success {
		return false
	}
	var st struct {
		CheckinEnabled *bool `json:"checkin_enabled"`
		CheckInEnabled *bool `json:"check_in_enabled"`
	}
	if json.Unmarshal(env.Data, &st) != nil {
		return false
	}
	if st.CheckinEnabled != nil {
		return *st.CheckinEnabled
	}
	if st.CheckInEnabled != nil {
		return *st.CheckInEnabled
	}
	return false
}

// NewApiCheckInAlready 查询今日是否已签（GET /api/user/checkin?month=YYYY-MM）。
func (c *apiClient) NewApiCheckInAlready(baseURL, token string, userID int64) (bool, error) {
	month := time.Now().Format("2006-01")
	endpoint := strings.TrimRight(baseURL, "/") + "/api/user/checkin?month=" + month
	hdrs := authHeaders(token, userID)
	env, status, err := c.request(http.MethodGet, endpoint, hdrs)
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		hdrs["Authorization"] = token
		env, status, err = c.request(http.MethodGet, endpoint, hdrs)
		if err != nil {
			return false, err
		}
	}
	if status != http.StatusOK || !env.Success {
		return false, fmt.Errorf("签到状态接口异常（HTTP %d）", status)
	}
	var st struct {
		Stats struct {
			CheckedInToday bool `json:"checked_in_today"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(env.Data, &st); err != nil {
		return false, fmt.Errorf("解析签到状态失败: %w", err)
	}
	return st.Stats.CheckedInToday, nil
}

// NewApiCheckIn 执行签到（POST /api/user/checkin），返回奖励 quota 与站方消息。
// 站方对"今日已签"可能仍返回非 success，按消息内容识别为幂等成功。
func (c *apiClient) NewApiCheckIn(baseURL, token string, userID int64) (quota int64, msg string, err error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/user/checkin"
	hdrs := authHeaders(token, userID)
	env, status, err := c.request(http.MethodPost, endpoint, hdrs)
	if err != nil {
		return 0, "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		hdrs["Authorization"] = token
		env, status, err = c.request(http.MethodPost, endpoint, hdrs)
		if err != nil {
			return 0, "", err
		}
	}
	if status != http.StatusOK {
		return 0, "", fmt.Errorf("签到接口返回 HTTP %d", status)
	}
	msg = strings.TrimSpace(env.Message)
	if !env.Success {
		if strings.Contains(msg, "已签") || strings.Contains(strings.ToLower(msg), "already") {
			return 0, "今日已签到", nil
		}
		if msg == "" {
			msg = "站点未返回错误原因"
		}
		return 0, "", errors.New("签到失败: " + msg)
	}
	var d struct {
		QuotaAwarded int64 `json:"quota_awarded"`
	}
	_ = json.Unmarshal(env.Data, &d)
	return d.QuotaAwarded, msg, nil
}

// Sub2ApiCheckIn 执行 Sub2API 签到（POST /api/v1/redeem/checkin），返回站方消息。
func (c *apiClient) Sub2ApiCheckIn(baseURL, token string) (string, error) {
	env, status, err := c.sub2apiRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/redeem/checkin", token, "")
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", errors.New("JWT 已过期，请重新获取令牌")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("签到接口返回 HTTP %d", status)
	}
	msg := strings.TrimSpace(env.Message)
	if env.Code != 0 {
		if strings.Contains(msg, "已签") || strings.Contains(strings.ToLower(msg), "already") {
			return "今日已签到", nil
		}
		if msg == "" {
			msg = fmt.Sprintf("code %d", env.Code)
		}
		return "", errors.New("签到失败: " + msg)
	}
	if msg == "" {
		msg = "签到成功"
	}
	return msg, nil
}

// TransferAff 调用 /api/user/aff_transfer，把邀请奖励转入余额。
func (c *apiClient) TransferAff(baseURL, token string, userID int64) error {
	endpoint := baseURL + "/api/user/aff_transfer"
	hdrs := authHeaders(token, userID)
	env, status, err := c.request(http.MethodPost, endpoint, hdrs)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		hdrs["Authorization"] = token
		env, status, err = c.request(http.MethodPost, endpoint, hdrs)
		if err != nil {
			return err
		}
	}
	if status != http.StatusOK {
		return fmt.Errorf("接口返回 HTTP %d", status)
	}
	if !env.Success {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "站点未返回错误原因"
		}
		return errors.New("站点返回: " + msg)
	}
	return nil
}

// ---------- Sub2API（Wei-Shaw/sub2api）适配 ----------
// 与 new-api 系的差异：信封为 {code:0,data}；鉴权为登录换取的 Bearer JWT；
// 身份接口 GET /api/v1/auth/me；余额字段 balance（美元）；邀请码 GET /api/v1/user/aff。

type sub2apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// sub2apiRequest 发起带 Bearer JWT 的请求并解析 Sub2API 信封。
func (c *apiClient) sub2apiRequest(method, endpoint, token, body string) (*sub2apiEnvelope, int, error) {
	var rd io.Reader
	if method == http.MethodPost {
		if body == "" {
			body = "{}"
		}
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接站点: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败: %w", err)
	}
	env := &sub2apiEnvelope{}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("响应不是 Sub2API 格式的 JSON（HTTP %d）", resp.StatusCode)
	}
	return env, resp.StatusCode, nil
}

func sub2apiErr(env *sub2apiEnvelope, status int) error {
	msg := strings.TrimSpace(env.Message)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	return errors.New("站点返回: " + msg)
}

// Sub2ApiMe 是 /api/v1/auth/me 的关键字段（余额为美元）。
type Sub2ApiMe struct {
	ID       int64   `json:"id"`
	Username string  `json:"username"`
	Email    string  `json:"email"`
	Balance  float64 `json:"balance"`
}

// Sub2ApiMe 拉取当前用户身份与美元余额。
func (c *apiClient) Sub2ApiMe(baseURL, token string) (*Sub2ApiMe, error) {
	env, status, err := c.sub2apiRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/auth/me", token, "")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, errors.New("令牌无效或已过期（Sub2API 的 JWT 有有效期，请重新用账号密码获取）")
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("auth/me 接口返回 HTTP %d", status)
	}
	if env.Code != 0 {
		return nil, sub2apiErr(env, status)
	}
	var me Sub2ApiMe
	if err := json.Unmarshal(env.Data, &me); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %w", err)
	}
	if me.ID == 0 {
		return nil, errors.New("auth/me 未返回用户 ID")
	}
	return &me, nil
}

// Sub2ApiAffCode 拉取邀请码（尽力而为，失败不阻塞同步）。
func (c *apiClient) Sub2ApiAffCode(baseURL, token string) string {
	env, status, err := c.sub2apiRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/user/aff", token, "")
	if err != nil || status != http.StatusOK || env.Code != 0 {
		return ""
	}
	var d struct {
		AffCode string `json:"aff_code"`
	}
	if json.Unmarshal(env.Data, &d) != nil {
		return ""
	}
	return d.AffCode
}

// Sub2ApiLogin 用邮箱+密码登录，返回 JWT、用户 ID 与用户名。
func (c *apiClient) Sub2ApiLogin(baseURL, email, password string) (token string, userID int64, username string, err error) {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	env, status, err := c.sub2apiRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/auth/login", "", string(body))
	if err != nil {
		return "", 0, "", err
	}
	if status != http.StatusOK {
		return "", 0, "", fmt.Errorf("登录接口返回 HTTP %d", status)
	}
	if env.Code != 0 {
		return "", 0, "", sub2apiErr(env, status)
	}
	var d struct {
		AccessToken string          `json:"access_token"`
		Requires2FA bool            `json:"requires_2fa"`
		User        json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return "", 0, "", fmt.Errorf("解析登录响应失败: %w", err)
	}
	if d.Requires2FA {
		return "", 0, "", errors.New("该账号开启了两步验证，无法自动登录，请手动获取")
	}
	if strings.TrimSpace(d.AccessToken) == "" {
		return "", 0, "", errors.New("登录成功但站点未返回令牌")
	}
	var u struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	_ = json.Unmarshal(d.User, &u)
	if u.ID == 0 {
		return "", 0, "", errors.New("登录成功但未能解析用户 ID")
	}
	return d.AccessToken, u.ID, u.Username, nil
}
