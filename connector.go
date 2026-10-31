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
