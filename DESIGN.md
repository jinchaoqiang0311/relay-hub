# RelayHub DESIGN.md

## Discovery
- **Artifact type**: dashboard / data tool（自托管单用户管理面板，内嵌于 Go 单二进制）
- **Audience**: 站长本人（中文用户），关注"钱在哪个站、该不该提现"
- **Primary outcome**: 一眼看清各站余额与奖励，快速执行同步/转余额
- **Adjectives**: 克制、精确、可靠
- **Aesthetic essence**: 账本 · 琥珀墨水 · 暖夜

## Committed aesthetic
「Ledger 账本」：暖色近黑背景，琥珀金（墨水/金钱）作唯一强调色，数据全部使用等宽 tabular 数字，
关键数字下方压一条账本式基线，汇总行用双细线（会计合计线）。拒绝 indigo/purple 渐变的默认 AI 风。

**Signature move**: 会计账本线 —— 统计行上下双细线 + 每张卡片主数字下的基线规则线。

## Typography
- 离线约束（内嵌二进制、不引 CDN）：不使用外部字体。
- 正文/中文: `"Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans SC", sans-serif`
- 全部数字/金额: `ui-monospace, "Cascadia Mono", Consolas, monospace` + `font-variant-numeric: tabular-nums`
- 模块 scale（base 13px, 1.2–1.38 混合）: 12 / 13 / 15 / 18 / 26

## Color (OKLCH, 60-30-10)
| 角色 | 值 |
|---|---|
| bg | `oklch(0.16 0.008 80)` 暖近黑 |
| surface / surface-2 | `oklch(0.205 0.01 80)` / `oklch(0.245 0.012 80)` |
| border / border-strong | `oklch(0.31 0.012 80)` / `oklch(0.42 0.014 80)` |
| fg / muted / faint | `oklch(0.94 0.004 80)` / `oklch(0.68 0.008 80)` / `oklch(0.52 0.008 80)` |
| accent / accent-fg | `oklch(0.80 0.135 85)` 琥珀 / `oklch(0.19 0.03 85)` |
| ok / err | `oklch(0.74 0.14 155)` / `oklch(0.68 0.17 25)` |

深色为"设计出的深色"：暖色相、靠亮度做层级，不用纯黑/纯白。

## Tokens
- Spacing base 4px（4/8/12/16/24/32/48）；组内紧、组间松
- Radius: 8px（卡片/弹窗）、5px（按钮/输入框）；两档封顶
- Shadow: 仅弹窗 backdrop 用黑色遮罩；元素一律 defined edge（1px border），不叠 diffuse shadow
- Motion: `--t-fast: 130ms; --t-med: 200ms; ease-out`；只动 transform/opacity；`prefers-reduced-motion` 全部关闭

## Craft decisions
- 布局: 单列 max-width 1160；统计 4 栏（移动 2 栏）；站点卡 `repeat(auto-fill, minmax(320px, 1fr))`
- 按钮: 按重要性排序 —— primary（琥珀底深字）/ default（surface-2 描边）/ danger（ghost 红）；含 hover/active/focus-visible/disabled/loading
- 表单: 真实 label、错误就地显示且保留输入、probe 结果作为独立状态面板
- 状态: 站点状态 = 圆点 + 文字（不靠颜色单传）；错误信息完整展示在卡内
- A11y: `:focus-visible` 2px accent outline、native `<dialog>` 焦点圈定、目标 ≥28px、lang=zh-CN
- 数字右对齐语境：金额一律 mono + tabular-nums

## Slop audit
- 无 indigo/violet、无渐变文字、无 glassmorphism、无 border+shadow 叠加、无 blob 圆角
- 数字 tabular、无脉冲假"live"动效、空/错/载态齐备
- 系统字体为离线内嵌工具的主动决策（CJK 字体体积不可内嵌），辨识度由色彩/账本线/等宽数字承担
