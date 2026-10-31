# 各类型中转站的信息获取方式

调研自 [all-api-hub](https://github.com/qixing-jk/all-api-hub) 的适配层（35+ 站点家族），
结合本项目的实现现状。作为后续适配工作的参考底表。

## RelayHub 已支持

| 类型 | 身份/余额 | 签到 | 邀请码 | 备注 |
|---|---|---|---|---|
| new-api 系（new-api / one-api / one-hub / done-hub） | `GET /api/user/self`（Bearer 或裸 token 双重试 + `New-Api-User` 头） | `GET /api/user/checkin?month=` 查状态 → `POST /api/user/checkin`；站点开关在 `/api/status` 的 `checkin_enabled` | self 里的 `aff_code`，链接 `{base}/register?aff={码}` | 今日收益可走 `/api/log/self`（带时间戳范围），未接入 |
| Veloera | 同 new-api | 同上，但 `/api/status` 开关字段是 `check_in_enabled`（下划线位置不同，参考 Veloera controller/misc.go） | 同 new-api | 请求头额外带 `Veloera-User` |
| Sub2API | `GET /api/v1/auth/me`（登录 POST `/api/v1/auth/login` 换 Bearer JWT，余额为美元 `balance`） | `POST /api/v1/redeem/checkin`（幂等靠站方"已签"消息识别） | `GET /api/v1/user/aff` → `aff_code` | 信封 `{code:0,data}`；JWT 有有效期；`requires_2fa` 拦截自动登录 |
| 手动记录 | 不接 API | — | 手填 | 魔改站兜底 |

## all-api-hub 有、RelayHub 尚未做的

- **VoAPI v2**：dashboard JWT 用**裸 `Authorization`**（无 Bearer 前缀）+ 专用信封；邀请链接由官方接口直接返回 `data.url`（Pro 功能）；有 tokenResync（令牌自动重同步）机制。参考 `all-api-hub/src/services/apiService/voapiV2/`
- **今日收益/用量**：new-api 系 `GET /api/log/self` + 时间戳范围 → 每日收益统计（我们的进账提醒用快照差值已覆盖主场景）
- **更多家族**：agentrouter、aihubmix、openrouter、gptLoad 等 30+——多为海外聚合平台，与国内中转站场景重叠少，按需再补
- **签到端点按账号覆盖**：all-api-hub 允许 fork 站覆盖签到路径（`resolveCheckInEndpoint`）；我们目前固定标准路径，遇到魔改站再加配置
- **签到 Turnstile**：new-api 系签到可能带人机验证 replay，未处理（开启的站签到会失败并记日志）

## 适配新类型的检查清单

1. 信封形状（`{success,data}` vs `{code,data}` vs 其他）
2. 鉴权载体（Cookie 会话 / Bearer JWT / 裸 Authorization / 自定义头）
3. 身份与余额端点及字段（余额单位是 quota 还是美元）
4. 签到端点与站点开关字段
5. 邀请码端点与注册链接格式
6. JWT/令牌有效期与刷新机制
