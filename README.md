# qoder2api

轻量级 [Qoder](https://qoder.ai) → OpenAI / Claude / Codex 兼容 API 网关。

仅包含 **Web 控制台 + Bridge**，无 macOS 桌面 GUI、无 Wails、无系统托盘，适合 Linux 服务器与本地轻量部署。

本项目基于 [wangtufly/QCCG](https://github.com/wangtufly/QCCG) **二次开发**，抽取 Bridge / 账号 / 签名核心逻辑，重做成可服务器部署的精简版本。

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-见原项目-blue)](https://github.com/wangtufly/QCCG/blob/main/LICENSE)

> ## 🆕 2026-10-23 · 本轮新增模型：Sonus / Cantus / Fast
>
> 通过**新版推理协议**（`cosy-clienttype=10` / `cosy-version=1.1.69` + **真实设备指纹**），
> 国际版账号模型清单由 13 → **15** 个，新增三档：
>
> | Key | 模型 | 定位 | 上下文 | 最大输出 | 倍率 | 推理 |
> |-----|------|------|--------|----------|------|:----:|
> | `smodel` | **Sonus** | 顶级旗舰 · 最贵 · 热门时段常排队 | 200,000 | 32,768 | **8** | ✅ |
> | `cmodel` | **Cantus** | 高能力主力档 | 200,000 | 32,768 | **4** | ✅ |
> | `fast` | **Fast** | 低延迟快速档 | 200,000 | 16,384 | 0.8 | — |
>
> - 三档均需账号在 `accounts/*.json` 配置 `checkin_fingerprint`（真实设备指纹）后走新版协议才会出现并可推理；未配置的账号仍是旧版 13 模型清单，行为不变。
> - **Sonus 倍率 8 为全表最高**，用的人多，上游高频返回 `10605 isQueued`（排队中）——属容量问题，稍后重试或换模型即可。
> - 详见下方「更新记录 · 新版推理协议解锁 Sonus / Cantus / Fast」。

---

## 📖 [2026-10-20 更新] Qoder 注册链路逆向研究 · 阶段性成果

> 独立研究文档 → **[docs/registration-research/README.md](docs/registration-research/README.md)**（点击查看完整报告）

本轮对 Qoder **Web 端注册流程**做了完整协议还原，主要发现：

| # | 发现 | 说明 |
|---|---|---|
| 1 | **真注册接口** | `POST /api/v1/users`（不是 `/users/sign-up`，后者只是前端路由）|
| 2 | **两轮人机验证** | ① AliyunCaptcha 2.0 滑块（scene `1r7eif79x`）② 阿里 baxia 风控 + 3×3 网格点选（`RGV587` 触发）|
| 3 | **baxia 可完整解析** | iframe 同源可访问，WASM 解密后经 `putImageData` 写 canvas，**`getImageData` 直接读到明文图片**（样本图片见 `artifacts/baxia/`）|
| 4 | **★ 设备↔账号绑定** | App 登录会打开 `/device/selectAccounts?nonce=` → 「继续使用此账户吗？」→ 服务端按设备维护账号关联表 |
| 5 | **★ 权益消失机制** | 网页注册可拿到 **300 积分 + Pro Trial**；同设备登录 App 后消失 → 与 `machineid = f(IOPlatformSerialNumber)` 的设备识别一致（见 `../resetfree/`）|
| 6 | **剩余卡点** | 题目文字为 anti-OCR 像素图（tesseract 全败），需多模态视觉识别；`bx-ua` 指纹算法未解 |

**欢迎社区接力**：多模态识别方案 / baxia `bx-ua` 算法 / 设备绑定接口细节。

### 🧰 配套工具包

本轮沉淀的可复用工具已整理到 **[tools/qoder-register-kit/](tools/qoder-register-kit/README.md)**：

| 工具 | 用途 |
|---|---|
| `cdp.js` | **CDP 驱动**：拟人鼠标轨迹（贝塞尔曲线+抖动）、逐键输入、随机延迟。所有脚本的地基 |
| `incog.js` | **无痕上下文管理**：规避登录态重定向；每次失败后重建，避免 React/baxia 状态污染 |
| `mailbox.py` | **临时邮箱客户端**：建地址 → 收信 → 提取 6 位验证码（`x-admin-auth` + Bearer JWT）|
| `detect_gap.py` | **滑块缺口检测**：拼图块不透明像素做模板匹配，输出缺口 x 坐标 |
| `flows/page-submit.js` | **注册第 1 步**：填姓名/邮箱 → 提交。含 antd Form 实例绕过（`__reactFiber$` + `setFieldsValue`）|
| `flows/step2-submit.js` | **注册第 2 步**：填密码 → 提交 → 触发滑块 |
| `flows/solve-slider6.js` | **滑块自动求解**：缺口检测 + 三段式闭环拖拽（实测拼图/手柄比例 ≈0.91）|
| `flows/type-code.js` | **填邮箱验证码**：6 位逐键输入 + 调 `onChangeCallback` 触发提交 |
| `flows/watch-baxia2.js` | **等待 baxia 弹窗**：轮询 punish iframe 是否展示 |
| `flows/analyze-baxia.js` | **baxia 只读分析**：导出题目图 + 9 张网格图（iframe 同源，无需逆向 WASM）|
| `flows/capture-success.js` | **全量网络存档**：请求/响应/`bx-ua` 指纹 → JSONL，用于抓人工成功样本 |
| `flows/net-observe.js` | **单次请求定位**：不确定按钮调什么接口时，一次看清 URL 与 body |

### ⚠️ 协助者注意事项

1. **合规**：本工具包仅供**协议研究与学习**；请勿用于批量注册、绕过风控或商业化滥用，
   并遵守目标站点服务条款与当地法律法规。
2. **不要提交凭证**：admin passkey、账号密码、`bx-ua` 指纹等**禁止入库**；
   抓包存档提交前必须脱敏。
3. **保持人类节奏**：`cdp.js` 内置随机延迟与拟人轨迹，请勿移除——高频请求会触发风控，
   也会影响其他人的研究。
4. **可回滚**：所有实验仅在浏览器无痕上下文内进行，不改动系统配置与硬件信息。
5. **提交前自检**：`git diff` 确认无凭证、无 `node_modules`、无个人数据。

> ⚠️ 研究性质声明：仅做协议与机制分析，不提供绕过验证的成品工具。

---

## 🙏 特别致谢 · 贡献者

本项目的海外版签到、多账号池、图片输入等核心特性由社区贡献者 **[LeonardW-sl](https://github.com/LeonardW-sl)** 主导实现，特此致以诚挚谢意。

| 贡献 | PR | 说明 |
|------|-----|------|
| **国际版（Global）签到支持** | [`b84c41b`](https://github.com/Zhengyuuuui/qoder2api/commit/b84c41b) | 新增 `Endpoints.OpenAPIHost`，签到域名按账号 `region` 自动路由（`cn` → `openapi.qoder.com.cn`，`global` → `openapi.qoder.sh`），修复国际账号 token 打到国内端点导致 `401 TOKEN_EXPIRE` 的问题；并补充 `TestCheckinHostForRegion` 单元测试 |
| **每日自动签到时间可配置** | [`fcde08e`](https://github.com/Zhengyuuuui/qoder2api/commit/fcde08e) | 新增 `auto_checkin_times`，支持多时段签到（`HH:MM` 列表，留空默认 `10:00`） |
| **多账号池** | [`c5fa3fa`](https://github.com/Zhengyuuuui/qoder2api/commit/c5fa3fa) ~ [`afe1d11`](https://github.com/Zhengyuuuui/qoder2api/commit/afe1d11) | 账号轮换、冷却、故障转移、状态持久化、池状态接口与控制台可视化 |
| **图片输入（多模态）** | [`63cef2a`](https://github.com/Zhengyuuuui/qoder2api/commit/63cef2a) | 支持 `image_url` 类型的多模态请求 |
| OAuth 会话 TTL 可配置 | [`624ccf6`](https://github.com/Zhengyuuuui/qoder2api/commit/624ccf6) | 新增 `QODER2API_OAUTH_TIMEOUT` 环境变量 |

感谢 [@LeonardW-sl](https://github.com/LeonardW-sl) 的持续贡献与代码质量（含单元测试覆盖）。欢迎更多社区朋友参与共建。

### 其他贡献者

| 贡献者 | 贡献 | 说明 |
|------|-----|------|
| [@SDCOM-0415](https://github.com/SDCOM-0415) | **定位 `--instance=global` 端口被覆盖 Bug** | 发现 `ensureConsolePassword()` 首次启动写盘默认 `port:8963` 覆盖 `--instance=global` 预设的 `8964`，并给出修复方向（见[更新记录](#-修复-instanceglobal-的-bridge-端口被-settings-默认值冲成-8963)） |

---

## ✨ 新功能：每日签到领 100 Credits

控制台「账号列表 / 额度」区域内置一键签到，可将 Qoder 官方 **每日 100 Credits** 活动直接打进控制台：

- **🎁 一键签到**：顶部按钮批量对所有账号签到（CN 与 Global 账号可混在同一实例）
- **🎁 单账号签到**：账号表格「操作」列独立签到按钮
- **⏰ 每日自动签到**：可选开关（**默认关闭**），开启后每天 `10:00 (UTC+8)` 自动为全部账号签到；
  签到时间点可配置（支持多时段，如 `09:15,17:00`）
- **🔥 连续签到统计**：自动记录签到历史，跨天累计「连续 N 天 / 累计 N 天 / 共 N 积分」
- **幂等安全**：已领取自动跳过，重复点击不会重复领取

### 双区域支持

签到接口按账号 `region` 自动路由到对应站点，**同一控制台可同时管理国内版与国际版账号**：

| Region | `Endpoints.OpenAPIHost` | 签到接口 | `daily-check-in/status` |
|--------|-------------------------|---------|--------------------------|
| `cn` | `openapi.qoder.com.cn` | ✅ 可用 | `200`（legacy 已 `DISABLED`，统计恒 0） |
| `global` | `openapi.qoder.sh` | ✅ 可用 | `404`（该端点从未部署） |

实现由 [@LeonardW-sl](https://github.com/LeonardW-sl) 在 [`b84c41b`](https://github.com/Zhengyuuuui/qoder2api/commit/b84c41b) 贡献：`account.Endpoints` 新增 `OpenAPIHost` 字段，
`checkinAccount` 取 `checkinHostFor(acct.Region)` 后透传给 `campaignsCheckin` / `readDailyCheckinStats` / `doCheckinRequest`。

> 签到链路基于抓包还原，两区路径完全一致：
> `GET /sash/api/v1/me/campaigns` → `POST /sash/api/v1/me/campaigns/{id}/claim`，
> 仅需 device token + `cosy-clienttype: 10`，**无需 COSY 签名**。详见 `scripts/auto_checkin.py`。

### 实现说明

- **权威签到走 `campaigns`**：这是唯一真实发放积分的接口，返回 `grantId` 可核验到账
- **不依赖 `daily-check-in`**：该 legacy 端点在国内已 `DISABLED` 且对未领取日恒返回 `409`（会误判为已领取而漏领），海外则直接 `404`

#### ⚠️ 设备指纹政策（重要）

国际版（`qoder.com`）的签到活动**按真实设备身份下发**。缺指纹时服务端不下发
`CLAIM_BEFEFIT` 活动，表现为「无可用签到活动」（`no_campaign`）。
该结论由社区 A/B 实验验证（[issue #4](https://github.com/Zhengyuuuui/qoder2api/issues/4)，
对照 [yetone/magpie#1268](https://github.com/yetone/magpie/issues/1268)）：
补入 `cosy-machinetoken` + `cosy-machinetype` 后活动立即出现，移除任一字段活动消失。

**自造指纹无效**：`cosy-machinetoken` 由官方客户端本地生成，客户端无法自造（实测任意自造值均被拒）。
必须使用官方客户端的真实指纹。

| 区域 | 指纹要求 |
|------|---------|
| `cn`（`openapi.qoder.com.cn`） | 通常**不需要** |
| `global`（`openapi.qoder.sh`） | **首次领取前必须携带**真实指纹 |

**懒下发机制**：Global 账号在官方客户端**首次领取一次后**，服务端转为「懒下发」——
之后即使不带指纹，`campaigns` 也能查到该活动。所以新账号需先用官方客户端领一次，
或在 qoder2api 中配置指纹。

**配置方式**：在账号 JSON 中添加 `checkin_fingerprint` 字段：

```json
{
  "id": "acct_xxx",
  "name": "your-global-account",
  "region": "global",
  "checkin_fingerprint": {
    "machine_id": "6ba86f67-4909-4a9a-9567-c289d999f0c1",
    "machine_token": "<cosy-machinetoken>",
    "machine_type": "<cosy-machinetype>",
    "machine_code": "<可选，cosy-machinecode>",
    "hostname": "localhost",
    "machine_os": "aarch64_darwin",
    "version": "0.4.4"
  }
}
```

**获取指纹**：

```bash
# 自动读取 machine_id（明文文件），并给出 token/type 的获取指引
./scripts/extract_fingerprint.sh global
```

`machine_id` 存放在客户端明文中：
`~/Library/Application Support/com.qoder.app.stable/auth.machine-id`（macOS）

`machine_token` / `machine_type` 存于**加密**文件 `auth.v1.dat`（非标准密钥保护，无法离线解密），
需通过抓包获取：

```bash
# 1. 启动抓包
mitmdump -q -s scripts/capture_addon.py -p 8080
# 2. 开启代理
./scripts/proxy_toggle.sh on
# 3. 打开官方客户端 →「用量面板 → 礼物图标」触发活动加载
# 4. 关闭代理
./scripts/proxy_toggle.sh off
# 5. 从 scripts/capture/ 中取 URL 含 /sash/api/v1/me/campaigns 的请求头，
#    复制 cosy-machinetoken / cosy-machinetype / cosy-version
```

#### 其他行为

- **领取窗口**：`10:00 → 次日 09:59 (UTC+8)`，窗口未开启时点击会明确提示下次开放时间
- **连续天数本地统计**：上游 legacy 统计恒为 0，故由 qoder2api 记录 `checkin_history.json` 并按**活动窗口 `startAt` 日期**（而非自然日）计算，避免 10:00 前误记
- **调度容错**：签到时刻若活动尚未下发，不标记当日完成，每分钟重试至 12:00，避免漏签
- **多时段支持**：`auto_checkin_times` 可配置多个 `HH:MM` 时间点（贡献者 `fcde08e`）
- **排查辅助**：`no_campaign` 时日志会打印该账号是否带指纹、活动类型列表与原始响应，
  便于定位是「缺指纹」还是「服务端未下发」

**手动签到 API**：

```bash
# 所有账号一键签到
curl -X POST http://127.0.0.1:3588/api/checkin

# 指定单账号
curl -X POST http://127.0.0.1:3588/api/checkin -d '{"account_id":"acct_xxx"}'
```

**开启每日自动签到**（默认关闭）：控制台勾选「每日 10:00 自动签到」即可，配置存于 `settings.json` 的 `auto_checkin` 字段。
签到时间点可在控制台「自动签到时间」输入框配置（逗号分隔 `HH:MM`，留空为 `10:00`），
或直接写入 `settings.json` 的 `auto_checkin_times` 字段，例如：

```json
{ "auto_checkin": true, "auto_checkin_times": ["09:15", "17:00"] }
```

⚠️ 自动签到依赖服务常驻运行（电脑开机 + qoder2api 启动）。

---

## ✨ 新功能：Token 自动续期（15 天）

账号的 access token 与 refresh token 均有有效期。此前 bridge **只保存 refresh_token 却从不使用它**，
一旦两个 token 双双过期，账号会静默失效（控制台额度显示为空、请求 `401 TOKEN_EXPIRE`），只能重新走 OAuth。

本功能利用 Qoder jobToken 端点的 `needRefresh` 模式（传入 `drt-` 前缀的 refresh_token，服务端签发新
access token 并回传新的 refresh_token）在过期前**主动续期**。

- **⏰ 定时续期**：默认**关闭**，开启后每 6 小时检查一次；token 年龄 ≥ 12 天自动续期（间隔 15 天，留 3 天余量）
- **🖱️ 手动续期**：控制台「续期 Token」按钮一键续期；「强制续期」忽略年龄窗口
- **📊 状态可见**：账号表格新增「Token 续期」列，显示 token 年龄、状态徽章、失败原因、下次续期日期
- **🔀 多模式兼容**：
  - OAuth 账号：优先用 `refresh_token`（同时拿到新 at + rt），无 rt 时回退 `device_token`
  - PAT 账号：跳过（每次请求实时交换，无需续期）
  - 无 secret 账号：跳过
- **🔁 续期后需重启 Bridge**：续期写入新的 secret 文件，重启后生效（界面会提示）

### 使用方式

控制台「账号列表 / 额度」区域新增一行控制条：

```
[续期 Token]  [☑ 自动续期(15天)]  [强制续期]   每 15 天自动续期（token 满 12 天触发）
```

对应接口：

| 接口 | 说明 |
|------|------|
| `GET /api/refresh` | 续期状态（开关、间隔、各账号 token 年龄、上次结果） |
| `POST /api/refresh/run` | 手动续期（跳过未满 12 天的账号） |
| `POST /api/refresh/run?force=1` | 强制续期全部账号 |

### 续期失败的常见原因

| 报错 | 原因 | 处理 |
|------|------|------|
| `personal token is invalid` | refresh_token 本身已过期 | 只能重新 OAuth / PAT 授权 |
| `TOKEN_EXPIRE` | device_token 已过期但 rt 仍有效 | 续期会自动处理 |
| `secret 中既无 refresh_token 也无 device_token` | 早期版本只存了 PAT | 重新添加账号 |

> ⚠️ 本功能是**预防性**的：若 token 已彻底死亡（at 和 rt 同时失效），续期无法救回，
> 必须重新授权。建议开启自动续期避免再次发生。

## 更新记录

### 🧠 修正模型「思考」能力显示（2026-10-28）

模型表此前的「推理」列只读上游 `is_reasoning`，导致 `dfmodel`（DeepSeek-Flash）、
`kmodel_latest`（Kimi-K3.1-Pro）显示没有勾——但抓取原始 `model/list` 发现：

| 模型 | `is_reasoning` | `thinking_config` | 实际能力 |
|------|----------------|-------------------|----------|
| `qmodel_38max` | true | 有（low/medium/xhigh）| 思考默认开 |
| `dfmodel` | **false** | **有**（low/high/max，默认开）| 思考可开关 |
| `kmodel_latest` | **false** | **有**（low/high/max，默认开）| 思考可开关 |
| `mmodel` | false | 无 | 不支持 |

即 `is_reasoning` 与 `thinking_config` 并不一致，前者不能代表"是否支持思考"。

改动：

- `QoderModel` 新增 `thinking_support` / `thinking_default` / `thinking_efforts`
  （从 `thinking_config` 解析，efforts 按 low→max 排序）；
- 控制台模型表「推理」列改为「思考」，显示 `默认开`/`可开关`/`常开`/`不支持`
  以及可选档位；
- 说明：bridge 请求模板默认 `model_config.is_reasoning=false`；Claude 客户端
  发送 `thinking.type=enabled` 时 bridge 会置 true，OpenAI 兼容客户端没有标准开关。
  上游即使未显式开启，也可能返回 `reasoning_tokens`（实测常见）。

### 🔁 排队等待升级：「首字前一直等」开关 + 最近等待记录（2026-10-27）

**2026-10-27 CST**

| 改动 | 内容 |
|------|------|
| 🆕 **「首字前一直等」开关**（`queue_wait_first_token`，**默认开**） | 开 = 排队时**忽略重试次数上限**，持续在同账号等待重试，直到出首字或客户端断开才停；关 = 按「最多重试次数」封顶。控制台「模型排队重试」卡片可改 |
| 🆕 **控制台「最近排队等待」面板** | 记录最近 50 轮排队片段（时间 / 模型 / 账号 / 区域 / 重试次数 / 累计等待 / 结果），由 `GET /api/queue/log` 提供，控制台每 20s 刷新 |

**实测**（CN `qfmodel`，上游拥塞）：开盾后排队可重试到第 3+ 次（不被 2 次封顶），累计等待与结果正确落入等待记录；客户端断开即记 `cancelled` 并中止。

> 等待记录仅存内存（环形缓冲，不落盘），重启清空。

### 🔄 上游模型清单更新（2026-10-26）

**2026-10-26 CST** · 模型列表运行时动态拉取（`/algo/api/v2/model/list`），无需改代码；以下为变更留档。

| 变化 | 详情 |
|------|------|
| 🆕 **`kmodel_latest` 升级为 Kimi-K3.1-Pro** | 原 Kimi-K3；key 未变，显示名与版本更新，倍率 1.4 → **1**（降价）。已有调用/映射无需调整 |
| ➖ **CN 移除 `qmodel`（Qwen3.7-Plus）** | 国内版 11 → **10** 个 |
| 📈 **Global `gfmodel` 最大输出 16,384 → 32,768** | 与国内版对齐 |
| 💰 **CN `qmodel_38max` 倍率 0.2 → 0.5** | 涨价 |

> Sonus / Cantus / Fast 仍在国际版新版推理协议账号的清单内（15 个不变）；本次无全新 key 新增。

### 🎚️ 新增：Context 多档位支持（200K / 400K / 1M）

**2026-10-23**

上游 `model/list` 的 `context_config` 按模型提供多档 context，但 bridge 此前只取
`is_default` 那一档（通常 200K），导致 400K / 1M 档位在 `/v1/models` 中不可见，
App 端能选到的大 context 在 bridge 侧完全丢失。

**改动**：

| 文件 | 内容 |
|------|------|
| `internal/bridge/bridge.go` | `QoderModel` 新增 `ContextTiers`；`parseQoderModels` 保留全部档位；新增 `resolveContextTokens` / `contextTierTokens` / `modelCatalog`（5 分钟缓存）|
| `internal/bridge/context_tier_test.go` (新增) | 档位解析、排序、默认档回退、大小写不敏感等用例 |
| `account/model.go` | `Settings` 新增 `context_tier_default` 与 `context_tier_overrides` |
| `service.go` | `ListQoderModels` 透出 `context_tiers` |
| `main.go` | settings 保存支持档位字段 |
| `webconsole/index.html` | 模型表格新增「可用档位」列；模型区新增档位下拉与保存按钮 |

**使用**：控制台「模型」区域顶部选择档位（如 `400K`），保存后对该实例所有模型生效；
留空 = 用上游默认档。请求时把所选 token 数写入 `model_config.max_input_tokens`。

> ⚠️ **未证实**：上游 `context_config` 的结构已从抓包确认，但「App 端用哪个请求字段
> 传递所选档位」尚未抓到真实模型请求验证。当前按最可能的 `model_config.max_input_tokens`
> 实现（该字段已存在于请求模板）。若实测不生效，需抓一次 App 的
> `agent_chat_generation` 请求确认字段名。

**实测档位分布**（`scripts/capture/` 抓包，15 个模型中 12 个有三档）：

| 模型 | 档位 |
|------|------|
| `qmodel_38max` / `qfmodel` / `dmodel` / `dfmodel` / `gfmodel` 等 | 200K / 400K / 1M |
| `ultimate` / `fast` / `smodel` / `cmodel` 等 | 200K / 400K / 1M |
| `auto` / `efficient` | 仅 200K |


### 🔁 改进：模型排队改为「同账号等待重试 + 友好提示」（2026-10-23）

**2026-10-23 CST**

热门模型（如 Sonus，倍率 8）被大量占用时，上游返回 `10605 isQueued`（带 `queueCount` /
`retryAfterSeconds`）。此前桥接把它误判为「无权限」（RESOURCE_LIMIT），于是**换号 + 冷却全部账号**，
最终回 `no_available_account` 直接报错断开——而换号对同模型同队列毫无帮助。

**改动**：

| 文件 | 内容 |
|------|------|
| `internal/bridge/errors.go` | `isQueued`/`retryAfterSeconds` 一律判为**可重试拥塞**（`upstream_queued`）；消息含「前方排队人数 + 预计等待秒数」；新增 `isUpstreamQueue` / `queueRetryAfter` |
| `internal/bridge/pool.go` | `ShouldFailover` 对队列错误返回 **false**（不换号、不冷却） |
| `internal/bridge/bridge.go` | `streamWithSlot` 外层新增**同账号排队重试**：等待 `retryAfterSeconds`（夹在 1s~30s）后重试，最多 2 次 |

**新行为**：

- 命中排队 → **在原账号上等待 ~30s 重试**（最多 2 次，共 ~60s），撑到队列疏通常能成功；
  期间不切换、不冷却账号，其他模型不受影响。
- 仍未疏解 → 返回友好错误（HTTP 503）：`模型 X 当前使用人数较多，正在排队（前方约 N 人），
  预计还需等待约 M 秒…`，客户端稍后重试或改用其他模型即可。

> 排队重试参数**控制台可配、热更新生效**（`NewAPI / 中转站接入` 卡片 → 「模型排队重试」）：
> **最多重试次数**（默认 2，范围 1~10）、**单次等待上限**（默认 30s，范围 1~300）、
> **默认等待**（上游未给 retryAfter 时，默认 15s）。实际单次等待 = `min(上游 retryAfterSeconds, 等待上限)`。
> 保存后即时生效，无需重启。

### 🆕 新增：新版推理协议解锁 Sonus / Cantus / Fast（2026-10-23）

**2026-10-23 CST**

上游按 `(cosy-clienttype, cosy-version)` 决定下发哪套模型清单。桥接此前固定用旧协议
（`clienttype=5` / `version=1.0.10` + 派生指纹），只能拿到 13 个模型，官方桌面端首选的
**Sonus / Cantus / Fast** 三档不可见、无法转发。

**改动**：为账号引入「真实设备指纹 → 新版推理协议」开关。

| 文件 | 内容 |
|------|------|
| `account/model.go` | `CheckinFingerprint` 扩展 `client_ip` / `client_type` / `version` / `scene` / `product` / `data_policy` 字段 |
| `internal/bridge/client.go` | `BearerClient` 新增 `fp`；`buildHeaders` 按是否配置真实指纹切换新旧协议头 |
| `internal/bridge/bridge.go` | 新增 `NewBridgeWithFP`，指纹非空时启用新版协议 |
| `internal/bridge/bridge_pool.go` | 新增 `NewSlotFromSecretWithFP` |
| `service.go` | `buildRegionPool` 读取账号 JSON 的 `checkin_fingerprint` 并下传 |

**机制**：账号 `accounts/*.json` 配置 `checkin_fingerprint`（`machine_id` = `machine_token` = `client_ip`，取自官方客户端抓包）后，该账号请求头切换为：

- `cosy-clienttype: 10` / `cosy-version: 1.1.69` / `user-agent: undici`
- `cosy-business-product: app` / `cosy-scene: app` / `cosy-data-policy: agree`
- 真实 `cosy-machineid` / `cosy-machinetoken` / `cosy-clientip`

`model/list` 随即从 13 → **15** 个，新增下列三档；**未配置指纹的账号行为完全不变**（旧协议、旧清单、零回归）。

| Key | 模型 | 上下文 | 最大输出 | 倍率 | 推理 |
|-----|------|--------|----------|------|:----:|
| `smodel` | **Sonus** | 200,000 | 32,768 | **8** | ✅ |
| `cmodel` | **Cantus** | 200,000 | 32,768 | **4** | ✅ |
| `fast` | **Fast** | 200,000 | 16,384 | 0.8 | — |

> ⚠️ **Sonus 倍率 8 为全表最高，热门时段上游返回 `10605 isQueued`（排队中）**——这是容量/占用
> 问题，非鉴权或协议问题；稍后重试或改用其他模型即可。Cantus / Fast 为同批下发的通用档。
>
> ✅ **回归验证**：新版协议不影响普通模型（`qfmodel` 等）推理；以纯 HEAD 基线对照确认无回归。

### 🔐 新增：Token 自动续期（15 天）

**2026-10-21**

修复「账号 token 过期后静默失效」问题：bridge 早前只保存 `refresh_token` 却从未使用，
双 token 同时过期后只能重新走 OAuth。

**改动**：

| 文件 | 内容 |
|------|------|
| `refresh.go` (新增) | 续期核心逻辑 + 调度器 + HTTP handlers |
| `account/model.go` | `Settings` 新增 `auto_refresh_token` 字段 |
| `account/secret.go` | 导出 `SecretPathFor()`（供 token 年龄判断） |
| `main.go` | 注册 `/api/refresh` 路由 + 启动调度器 + settings 支持 |
| `webconsole/index.html` | 账号表格新增「Token 续期」列 + 续期控制条 |

**机制**：调用已有的 `cosy.ExchangeJobToken` 并传入 `drt-` 前缀 token 触发 `needRefresh` 模式，
服务端返回新的 access token 与 refresh token。token 年龄 ≥ 12 天触发（间隔 15 天）。

**调度**：默认关闭；开启后每 6 小时检查一次，启动时延迟 45s 先跑一次让控制台有状态。

**测试覆盖**：死号（401 明确报错）/ PAT 模式（跳过）/ 无 secret（跳过）/ 新 token（跳过）/
开关持久化（重启后保持）。

### 🌐 国际版签到的账号差异（灰度投放）

**2026-10-20 CST**

国际版（`qoder.com`）的活动是**按账号灰度投放**的，不同账号拿到的活动**不重叠**。实测两个账号：

| | `yusheng zheng`（老号） | `tmpreg gep`（新号） |
|---|---|---|
| 注册时间 / 方式 | 2026-07-29 · `sso.google` | 2026-10-11 · `dashboard.email_pwd` |
| **每日签到**（每天 100 Credits） | ✅ 可正常自动签到 | ❌ 服务端未下发 |
| **SOTA 专属积分**（2000，限 Sonus 模型） | ❌ 未下发 | ✅ 已领取 |

两点结论：

1. **老号签到正常，但没有 SOTA 专属积分** —— 自动签到只覆盖「每日签到」这一类活动
2. **新号无签到，但有 SOTA 积分** —— 用新号的 SOTA 包 ID 给老号代领会返回
   `CAMPAIGN_NOT_FOUND`（服务端校验活动归属），说明这是运营灰度，不是接口或指纹问题

> 即：**自动签到能领的活动范围，取决于服务端给该账号下发了什么**。
> qoder2api 只负责把已下发的 `CLAIM_BENEFIT` 活动自动领掉，无法代领未下发的活动。

#### 官方 App 中的 SOTA 专属积分

![国际版 App 的 SOTA 专属积分面板](docs/img/sota-credits-app.jpg)

#### qoder2api 控制台展示效果

![qoder2api 控制台账号额度列表](docs/img/console-account-packages.jpg)

### 📋 模型总表（2026-10-26 实拉）

模型清单由上游 `GET /algo/api/v2/model/list` 在运行时动态拉取，下方为当时快照，仅供对照；
实际以 `/v1/models` 返回为准。`price_factor` 为相对倍率，`0` 表示限免。

> ⚠️ 同一 key 在**两区的推理支持与最大输出可能不同**（上游按区配置），故分表列出。
> 国际版下表为**新版推理协议账号**（配置真实指纹）快照；旧协议账号仍为较少的模型清单。

#### 国内版 · 10 个（Bridge `8963`）

| Key | 显示名 | 上下文 | 最大输出 | 倍率 | 推理 |
|-----|--------|--------|----------|------|:----:|
| `auto` | Auto | 200,000 | 32,768 | 0.5 | ✅ |
| `qmodel_38max` | Qwen3.8-Max | 200,000 | 32,768 | 0.5 | ✅ |
| `qfmodel` | Qwen3.8-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `dmodel` | DeepSeek-V4-Pro | 200,000 | 32,768 | 0.5 | ✅ |
| `dfmodel` | DeepSeek-Flash | 200,000 | 16,384 | **0** 🆓 | — |
| `gmodel` | GLM-5.3 | 200,000 | 32,768 | 0.8 | ✅ |
| `gfmodel` | GLM-5.3-Flash | 200,000 | 32,768 | 0.1 | ✅ |
| `kmodel_latest` | **Kimi-K3.1-Pro** | 200,000 | 16,384 | 1 | — |
| `kmodel` | Kimi-K2.8-Preview | 200,000 | 32,768 | 0.8 | ✅ |
| `mmodel` | MiniMax-M3.1-Flash | 200,000 | 16,384 | 0.1 | — |

#### 国际版 · 15 个（Bridge `8964` · 新版推理协议账号）

| Key | 显示名 | 上下文 | 最大输出 | 倍率 | 推理 |
|-----|--------|--------|----------|------|:----:|
| `auto` | Auto | 200,000 | 16,384 | 0.5 | — |
| `ultimate` | Ultimate | 200,000 | 32,768 | 2 | ✅ |
| `efficient` | Efficient | 200,000 | 16,384 | 0.3 | — |
| `qmodel_38max` | Qwen3.8-Max | 200,000 | 32,768 | 0.5 | ✅ |
| `qfmodel` | Qwen3.8-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `dmodel` | DeepSeek-V4-Pro | 200,000 | 32,768 | 0.5 | ✅ |
| `dfmodel` | DeepSeek-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `gmodel` | GLM-5.3 | 200,000 | 32,768 | 0.8 | ✅ |
| `gfmodel` | GLM-5.3-Flash | 200,000 | 32,768 | 0.1 | ✅ |
| `kmodel_latest` | **Kimi-K3.1-Pro** | 200,000 | 16,384 | 1 | — |
| `kmodel` | Kimi-K2.8-Preview | 200,000 | 16,384 | 0.8 | — |
| `mmodel` | MiniMax-M3.1-Flash | 200,000 | 16,384 | 0.1 | — |
| `smodel` | **Sonus** 🆕 | 200,000 | 32,768 | **8** | ✅ |
| `cmodel` | **Cantus** 🆕 | 200,000 | 32,768 | **4** | ✅ |
| `fast` | **Fast** 🆕 | 200,000 | 16,384 | 0.8 | — |

> 🆕 = 2026-10-23 新增（Sonus / Cantus / Fast）；**Kimi-K3.1-Pro** 为 2026-10-26 由 Kimi-K3 升级而来（key 未变、倍率 1.4→1）。倍率为该快照账号实测，可能随账号档位/活动浮动。

**区域差异速览**

| 差异 | 详情 |
|------|------|
| 国际独占 | `ultimate` / `efficient`；新版协议账号额外独占 `smodel`(Sonus) / `cmodel`(Cantus) / `fast` |
| 推理标记不同 | `auto` / `kmodel` 国内支持、国际不支持；`dfmodel` 反之 |
| 最大输出不同 | 多数模型国内 32,768、国际 16,384；`dfmodel` 相反 |

> 🆓 = 限免（倍率 0）；🆕 = 本轮（2026-10-23）新增。倍率可能随账号档位/活动浮动。

#### ✅ SOTA「Sonus」模型已可通过新版推理协议访问（2026-10-23 更正）

> 本节此前的结论「Sonus 不通过 `model/list` 下发、API 侧不可见」**已过时**，特此更正。

持有 **SOTA 专属积分**的账号，官方 App 的模型选择器里会多出一个 **Sonus** 模型
（积分说明：选择 Sonus 时优先抵扣该积分）。此前实测它不走默认协议的下发，是因为桥接用的是
旧协议（`clienttype=5` / `version=1.0.10`）。

**2026-10-23 复测**：同一账号（`tmpreg gep`）改用**新版推理协议**
（`clienttype=10` / `version=1.1.69` + 真实设备指纹）后，`model/list` 直接下发
**Sonus(`smodel`)**，并连同 **Cantus(`cmodel`)** / **Fast(`fast`)** 一起进入清单（13 → 15 个）。
配置方法见上方[更新记录 · 新版推理协议](#-新增新版推理协议解锁-sonus--cantus--fast2026-10-23)。

> ⚠️ Sonus 倍率 8 为全表最高、占用高，热门时段上游返回 `10605 isQueued`（排队中）——
> 属容量问题，稍后重试或改用其他模型即可，并非鉴权或协议失败。

### 🔄 上游模型清单更新

**2026-10-20 CST** · 模型列表为运行时动态拉取（`/algo/api/v2/model/list`），无需改代码；以下仅作变更留档。

**国内版（14 个）**

| Key | 显示名 | 变化 |
|-----|--------|------|
| `mmodel` | **MiniMax-M3.1-Flash** | 🔄 更名（原 MiniMax-M2.7） |
| `qmodel_38max` | Qwen3.8-Max | 💰 price 0.5 → **0.2**（降价） |
| `dmodel` | DeepSeek-V4-Pro | 💰 price 0.8 → **0.5**（降价） |
| `gmodel` | GLM-5.3 | 💰 price 0.6 → **0.8**（涨价） |
| `kmodel` | Kimi-K2.8-Preview | 💰 price 0.3 → **0.8**（涨价） |
| `kmodel_latest` | Kimi-K3 | 💰 price 0.8 → **1.4**（涨价） |
| `dfmodel` | DeepSeek-Flash | 🆓 price=**0**（免费） |
| `qfmodel` | Qwen3.8-Flash | 🆓 price=**0**（限免活动延续） |

**国际版（15 个）**：比国内多 `ultimate` / `performance`（ctx **272K**，最大）/ `efficient` 三个独占档位；
国内独有 `q37fmodel`（Qwen3.7-Flash）与 `gm51model`（GLM-5.2）。

> ⚠️ `mmodel` 只是**显示名**变化，key 未变，已有调用与映射无需调整。
> 若在控制台手工配过 `ModelMappings` 且使用了旧显示名，请改为 key。

### 🐛 修复：`--instance=global` 的 Bridge 端口被 settings 默认值冲成 8963

**2026-10-16 14:25 CST** · 由社区贡献者 [@SDCOM-0415](https://github.com/SDCOM-0415) 定位

**问题现象**

按 README 使用 `./qoder2api --instance global` 启动，文档承诺 Bridge 端口为 `8964`，
实际却监听 `8963`；`ss -tlnp | grep 8964` 无结果，按 8964 接入的客户端全部连不上。

**根因**

`ensureConsolePassword()` 在**首次启动**时会 `LoadSettings()`（无 `settings.json` 时返回默认
`Port=8963`），把 `{port:8963, console_password:...}` **写盘**，并同步
`svc.bridgePort = cur.Port`。这一步发生在 `--instance` 预设端口应用**之后**，
于是 `--instance=global` 预设的 `8964` 被静默覆盖回 `8963`，且无任何端口相关日志。

> 注：并非 `LoadRuntimeSettings()` 的问题——`main.go` 在其后已用 `*bridgePort` 修正过，
> 真正冲掉的是随后执行的 `ensureConsolePassword()`。

**修复**

- `auth.go`：`ensureConsolePassword()` 只同步 `BridgeToken`，**不再覆盖** `svc.bridgePort`
- `main.go`：把端口最终裁决移到 `ensureConsolePassword()` **之后**，并明确优先级：
  `显式 --instance 预设 > 显式端口 flag > settings.json > 内置默认`

**验证（全新数据目录）**

| 启动参数 | 修复前 | 修复后 | 说明 |
|---|---|---|---|
| `--instance=global` | ❌ `8963` | ✅ `8964` | 本 Bug 主场景 |
| `--instance=cn` | `8963` | ✅ `8963` | 回归 |
| `--bridge-port=9999` | `9999` | ✅ `9999` | 显式 flag 优先（`9999` 仅为测试用任意端口） |
| 无端口参数（默认 cn） | `8963` | ✅ `8963` | 回归 |

> 表中 `9999` 是验证"显式 `--bridge-port` 是否优先生效"所用的**任意空闲测试端口**，
> 并非项目默认或推荐端口。生产请使用默认 `8963`（CN）/ `8964`（Global）。

---

### 📌 签到设备指纹政策（Global 需真实官方指纹）

**2026-10-16 CST** · 关联 [issue #4](https://github.com/Zhengyuuuui/qoder2api/issues/4) · 对照 [yetone/magpie#1268](https://github.com/yetone/magpie/issues/1268)

国际版账号签到恒为 `no_campaign` 的根因与处理方式：

- **根因**：`campaigns` 接口需要**真实**的 `cosy-machinetoken` + `cosy-machinetype`
  （官方客户端本地持有），缺失时服务端不下发 `CLAIM_BEFEFIT` 活动。
  A/B 实验：补入后活动出现，移除任一字段活动消失
- **自造指纹无效**：`cosy-machinetoken` 由客户端本地生成且受服务端校验，
  任意自造值（含正确长度/格式）都会被拒 → 此前 `7702e28` 移除自造指纹方向正确但不完整
- **指纹不是登录下发的**：实测 `deviceToken/poll`、`deviceToken/refresh` 响应中
  均无这些字段，需从客户端本地或抓包获取
- **懒下发**：Global 账号在官方客户端首次领取后，之后不带指纹也能查到活动——
  解释了「同一控制台内有的 Global 号正常、有的恒 no_campaign」
- **实现**：新增账号级 `checkin_fingerprint` 配置（`account/model.go`），
  `checkinHeaders` 有指纹才附带、无指纹只发必要头（CN 行为）
- **工具**：新增 `scripts/extract_fingerprint.sh`（读取明文 `machine_id` 并给出抓包指引）；
  `no_campaign` 时日志打印指纹状态、活动类型与原始响应，便于排查

### 🏗️ 架构：单控制台 + 双桥端口（CN 8963 / Global 8964）

**2026-10-10 21:20 CST** · [`6199925`](https://github.com/Zhengyuuuui/qoder2api/commit/6199925)

Global Web 控制台（3589）闲置但其 Bridge 链路仍需保留，故将两个 Bridge 合并到同一控制台实例：

- 新增 `--global-port`（默认 `8964`，传 `0` 关闭）
- `--instance=cn` 单进程启动 `console 3588 + bridge 8963 + bridge 8964`
- `8964` 使用 Global 专用账号池；`8963` 保留全量池并按模型 region 路由（见下方跨区修复）
- `--instance=global` 保留为独立兜底模式（`3589 + 8964`）

---

### 🐛 修复：请求跨区域路由导致误扣 Global 账号额度

**2026-10-12 21:31 CST** · [`f4b22a0`](https://github.com/Zhengyuuuui/qoder2api/commit/f4b22a0)

**问题现象**

单控制台 + 双桥改造后，CN 桥（8963）把请求派给了 **Global 账号**并扣除其 Credits。
实测 `yusheng zheng`（国际版）在客户端未启动的情况下被消耗 549 Credits。

**根因**

8963 使用 `buildAccountPool()` 构建账号池，**不区分 region**，CN 与 Global 账号混在一起。
选号逻辑 `pickSlotForModel` 只检查模型的**目录归属**，从不检查账号的 **region**：

```go
// ❌ 修复前
slot, ok := b.pool.Pick(exclude, func(s *Slot) bool {
    return b.slotSupportsModel(s, model)   // 只看模型目录，不看 region
})
if ok { return slot, true }
return b.pool.Pick(exclude)               // 兜底完全不看 region
```

由于 `qmodel_38max` / `qfmodel` 等模型在**两个区域的目录中都存在**，
CN 请求因此会被匹配到 Global 账号并扣费。

**修复内容**

| 文件 | 修改 |
|------|------|
| `service.go` | 8963 改用 `buildRegionPool(acct.Region)`，池内只含同区域账号；8964 保持 Global 专用池；删除已无调用者的 `buildAccountPool()` |
| `internal/bridge/bridge_pool.go` | 新增 `NewPoolBridgeFor(region, pool, tmpl)` 将区域绑定到 Bridge；选号的**目录匹配**与**兜底**均增加 region 过滤，杜绝跨区选号 |
| `internal/bridge/bridge.go` | 日志新增账号名 / ID / region / 模型 |

**端口与账号的隔离关系**（修复后）：

```
console 3588    管理全部账号
bridge  8963    region=cn      仅使用 region=cn 的账号
bridge  8964    region=global  仅使用 region=global 的账号
```

**排查用日志**

每次调用上游都会记录实际使用的账号，便于核对扣费归属：

```bash
# 查看某次请求实际用了哪个账号
grep "using account" ~/.qoder2api-cn/logs/qccg-*.log

# 排查是否有跨区误用（应只出现在 8964 侧）
grep "using account" ~/.qoder2api-cn/logs/qccg-*.log | grep region=global
```

输出示例：

```
callQoder region=cn model=gmodel prompt=hi
callQoder: using account=nick8034851628 id=acct1789... region=cn model=gmodel
callQoder: account=yusheng zheng id=zysgm... region=global failed, failing over to next account: ...
```

### 签到：支持 Global 国际版 + 修复「显示签到但没到账」

**2026-10-10 ~ 2026-10-11** · [`b84c41b`](https://github.com/Zhengyuuuui/qoder2api/commit/b84c41b) 双区域 · [`fcde08e`](https://github.com/Zhengyuuuui/qoder2api/commit/fcde08e) 可配置时间 · [`2cd2ccd`](https://github.com/Zhengyuuuui/qoder2api/commit/2cd2ccd) 设备指纹

> 本节特性由社区贡献者 [@LeonardW-sl](https://github.com/LeonardW-sl) 主导实现，详见文首[致谢](#🙏-特别致谢--贡献者)。

- **双区域签到**（`b84c41b`）：`account.Endpoints` 新增 `OpenAPIHost`，签到域名按账号 `region` 自动路由
  （`cn` → `openapi.qoder.com.cn`，`global` → `openapi.qoder.sh`），
  同一控制台实例可同时管理国内版与国际版账号，无需分实例
- **修复国际版签到不可用（重要）**：抓包对比官方客户端发现，国际版签到活动**按设备指纹下发**。
  此前签到请求只带 `Authorization` + `cosy-clienttype`，缺少
  `cosy-version` / `cosy-machineid` / `cosy-machinetoken` / `cosy-machinetype` /
  `cosy-machinecode` / `cosy-machinehostname` / `cosy-machineos` 共 7 个头，
  服务端因此不下发 `CLAIM_BENEFIT` 活动，表现为「无可用签到活动」。
  现已复用 Bridge 的 `cosy.FingerprintSeed` 派生规则补齐全部指纹头，
  国际版账号签到恢复正常（实测由 `no_campaign` 变为可领取并成功到账）
- **修复空签到（重要）**：`daily-check-in/claim` 在国内已 `DISABLED`，却对**未领取日也恒返回 409**
  （被误读为「已领取」而跳过真实领取）；海外该端点直接 404。
  现已**彻底移除**对它的领取调用，`campaigns` 成为唯一权威签到路径（返回 `grantId` 可核验到账）
- **窗口日期记账**：本地签到历史改按活动窗口 `startAt` 记录，而非自然日。
  此前在 `10:00` 前点击（仍属前一日窗口）会把当天误记为已签，连续天数虚高
- **提示更明确**：未到新窗口时明确输出「已领取 YYYY-MM-DD 窗口额度（新窗口 MM-DD 10:00 开放）」
- **调度防漏签**：10:00 整点若活动尚未下发，不再标记当日完成，改为每分钟重试至 12:00
- **连续签到统计**：本地 `checkin_history.json` 按账号分别记录，
  跨天自动累计「连续 N 天 / 累计 N 天 / 共 N 积分」，支持断签识别
- **签到时间可配置**（`fcde08e`）：`auto_checkin_times` 支持多时段（HH:MM 列表），留空默认 `10:00`

### 流稳定性 / 错误分类 / 安全加固

**2026-10-07 20:56 CST** · [`be38b8f`](https://github.com/Zhengyuuuui/qoder2api/commit/be38b8f)（合并 `LeonardW-sl` 多账号池 / 图片输入 / 区域路由）

**上游流处理（对齐 hub 行为）**

- **错误帧及时中止**：上游 HTTP200 建流后在信封里投递错误帧（418/5xx 等）且不关流时，
  网关立即停止读取并按瞬时故障自动重开上游（有界重试，对调用方无感），请求不再挂起
- **空流显式报错**：上游建流成功但零有效帧即关流时返回 `empty upstream stream`；
  三协议流式路径输出 error 事件帧（chat 为 data 错误帧，claude/codex 为 `event: error`），
  非流式路径返回 500 + 错误 JSON（此前为空内容正常 finish，属与 hub 对齐的有意变更）
- **错误四分类**：上游错误按「内容审核 / 瞬时可重试 / 客户端参数 / 普通上游」分类，
  对客户端输出友好中文消息与正确 HTTP 状态（内容审核 400、瞬时耗尽 502 等）；
  同账号瞬时故障（418/5xx/传输抖动）连接层 1s/2s 退避自动重试
- **usage 同帧合并**：上游同帧返回 usage + choices 时，token 用量与内容一并返回，不再二选一

**安全**

- **Bridge 端点鉴权**：`/v1/*` 必须携带 API Key（默认 `qccg`，控制台可修改）——
  支持 `Authorization: Bearer <token>` 或 `x-api-key: <token>`，常量时间比较；
  OPTIONS 预检直接 204 放行；校验失败返回 401 + OpenAI 风格错误体。
  同网络无凭证客户端不再能消耗账号配额
- **短 token 防护**：误粘贴短 token 不再导致启动 panic，日志只输出前缀
- **PII 降级**：userinfo 原始响应日志降为 Debug 级并截断 500 字节，默认日志级别不再落盘
- **未跟踪文件隔离**：`.gitignore` 新增 `.ydevsphere/`、`webconsole/*.bak*`、`scripts/capture/`
  （抓包流量目录，可能含凭证，仅存本地、不入库，请自行处理）

**设备指纹与可观测性**

- **本机指纹盐**：`settings.json` 的 `machine_salt` 首次启动自动生成，使每个部署的
  指纹派生空间独立（uid + 本机盐派生机器码，重启不变）；**勿手动修改，变更即全部账号指纹漂移**
- **盐降级明示**：盐加载失败时日志明确写出「运行于无盐模式（hub 兼容指纹），下次启动
  恢复加盐将导致全部账号指纹漂移」；成功输出 `machine salt loaded (len=N)`；
  `cmd/checkin` 调试工具失败时向 stderr 告警（不阻断运行）

**测试与工具**

- `internal/bridge`、`internal/cosy` 新增单元/回归测试（信封重试闸门、错误帧停读信号、
  空流报错、usage 合并、指纹派生等），离线可跑、无外部依赖
- 新增 `cmd/checkin` 调试工具（只读查询活动/额度接口）与 `scripts/` 签到辅助脚本
  （`checkin.py` 等，凭证运行时从本地数据目录读取，无硬编码）

---

## 功能

- 🎁 **每日签到**：一键/单账号/定时自动领取每日 100 Credits（见上）
- 将 Qoder 账号转为本地兼容 API，供 **NewAPI**、**OpenCode**、**Claude Code**、**Codex** 等客户端使用
- Bridge 端点鉴权：`/v1/*` 需携带 API Key（默认 `qccg`，控制台可改）
- 多账号管理：支持 **OAuth** 与 **PAT**
- 账号额度展示（套餐额度 + 个人拓展包分开显示）
- 模型列表展示上下文窗口 / 最大输出 / 推理支持
- 一键复制 NewAPI 渠道配置（Base URL + API Key）
- 数据与密钥落盘，适合服务器常驻与保活

## 端口

**默认（`--instance=cn`，推荐）**：单进程同时提供控制台与两个 Bridge

| 服务 | 端口 | 说明 |
|------|------|------|
| Web 控制台 | `3588` | **唯一控制台**，管理全部账号（CN + Global） |
| Bridge（CN） | `8963` | 仅使用 `region=cn` 账号，按模型 region 路由 |
| Bridge（Global） | `8964` | 仅使用 `region=global` 账号，可通过 `--global-port=0` 关闭 |

数据目录：`~/.qoder2api-cn`（账号集中存放，按 `region` 字段区分归属）

客户端接入：

- CN 用户 → `http://<host>:8963/v1`
- Global 用户 → `http://<host>:8964/v1`

<details>
<summary>兼容模式：独立 Global 实例（通常不需要）</summary>

| 实例 | Web 控制台 | Bridge API | 数据目录 |
|------|------------|------------|----------|
| `--instance=cn` | `3588` | `8963` + `8964` | `~/.qoder2api-cn` |
| `--instance=global` | `3589` | `8964` | `~/.qoder2api-global` |

</details>

双开请用 `--instance`：

```bash
# 国内
./qoder2api --instance=cn

# 国际
./qoder2api --instance=global
```

## Docker 部署

### 本地构建运行

需要安装 Docker Engine 与 Docker Compose：

```bash
docker compose up -d --build
```

控制台位于 `http://localhost:3588`，Bridge 端口为 `8963`（CN）和 `8964`（Global）。账号、密钥与设置保存在 Compose 命名卷 `qoder2api-data` 中；可通过环境变量 `QODER2API_CONSOLE_PASSWORD` 指定控制台密码，留空则首次启动时自动生成并保存。

查看运行日志或停止服务：

```bash
docker compose logs -f
docker compose down
```

### 点击按钮构建并推送到 GHCR

在 GitHub 仓库打开 **Actions → Build and publish Docker image → Run workflow**，输入要发布的镜像标签并运行。工作流会构建 `linux/amd64` 和 `linux/arm64`，推送输入标签以及对应的 `sha-<commit>` 标签到：

```text
ghcr.io/<仓库所有者>/qoder2api:<tag>
```

镜像命名空间由工作流的 `GITHUB_REPOSITORY` 自动推导（例如本仓库发布到 `ghcr.io/zhengyuuuui/qoder2api`），fork 后无需改动即可发布到自己账号下。工作流使用仓库的 `GITHUB_TOKEN`，无需额外配置推送密钥。首次发布后，可在仓库 **Packages** 中调整镜像可见性；拉取公开镜像示例：

```bash
docker pull ghcr.io/zhengyuuuui/qoder2api:latest
```

## 快速开始

### 1. 编译

```bash
git clone <你的仓库地址>/qoder2api.git
cd qoder2api
go build -ldflags="-s -w" -o qoder2api .
```

Linux 交叉编译示例：

```bash
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o qoder2api .
```

### 2. 启动（控制台 + 桥一体）

一条命令同时启动 **网页控制台** 与 **Bridge 服务框架**：

```bash
./qoder2api --web-port=3588 --bridge-port=8963
```

可选参数：

| 参数 | 默认 | 说明 |
|------|------|------|
| `--web-port` | `3588` | Web 控制台端口 |
| `--bridge-port` | `8963` | Bridge API 端口 |
| `--bind` | `0.0.0.0` | 控制台监听地址 |
| `--data-dir` | `~/.qoder2api` | 数据根目录（账号/密钥隔离） |
| `--instance` | 空 | `cn`→3588 + 8963 + 8964；`global`→3589 + 8964（独立模式） |
| `--global-port` | `8964` | Global 专用桥端口；`0` = 不启动 |
| 环境变量 `QODER2API_HOME` | — | 等同 `--data-dir` |
| 环境变量 `QODER2API_CONSOLE_PASSWORD` | — | 控制台登录密码 |

启动成功后日志类似：

```text
qoder2api console: http://0.0.0.0:3588
bridge target port: 8963
```

### 3. 打开网页控制台（需登录）

浏览器访问：

```text
http://127.0.0.1:3588
```

会跳转到 `/login.html`。控制台密码来源（优先级从高到低）：

1. 环境变量 `QODER2API_CONSOLE_PASSWORD`
2. `~/.qoder2api/settings.json` 里的 `console_password`
3. 若都没有，**首次启动自动生成**并写入 settings，日志会打印一次

```bash
export QODER2API_CONSOLE_PASSWORD='你的强密码'
./qoder2api --web-port=3588 --bridge-port=8963
```

服务器部署时替换为：

```text
http://<服务器IP>:3588
```

登录后在控制台中：

1. 选择 Region（`global` / `cn`）
2. 点击 **OAuth 登录**（或填写 PAT 添加）
3. 授权成功后账号会写入本地，并自动 **激活**
4. 激活后 **Bridge 自动监听 8963**

> 说明：控制台密码只保护 **管理页**；Bridge 仍用独立 API Key（默认 `qccg`），与 NewAPI 对接不受影响。  
> CN / Global 双开时，会话 cookie 按端口隔离（`qoder2api_session_3588` / `qoder2api_session_3589`），同一浏览器可同时登录两端。

### 4. 启动 / 确认 Bridge

- **自动**：激活账号后会自动 `startBridge`
- **手动**：控制台可调用启动逻辑；也可用 API：

```bash
# 查看状态
curl http://127.0.0.1:3588/api/status

# 确认 Bridge 模型列表（需携带 API Key，默认 qccg）
curl -H "x-api-key: qccg" http://127.0.0.1:8963/v1/models
```

若返回模型 JSON，说明桥已正常；返回 401 说明 API Key 缺失或不匹配。

---

## 使用方法

### Bridge 端点

| 端点 | 兼容格式 |
|------|----------|
| `POST /v1/chat/completions` | OpenAI Chat |
| `POST /v1/messages` | Anthropic Claude |
| `GET  /v1/models` | 模型列表 |
| `POST /v1/responses` | OpenAI Responses (Codex) |

### 默认接入信息

| 字段 | 值 |
|------|-----|
| Base URL (OpenAI) | `http://127.0.0.1:8963/v1` |
| API Key | `qccg`（可在控制台修改） |
| 推荐模型 | `auto` |

控制台 **「NewAPI / 中转站接入」** 区域可复制完整配置。

### 接入 NewAPI

1. 打开 qoder2api 控制台 → 复制 Base URL 与 API Key  
2. NewAPI 新建渠道：
   - 类型：`OpenAI`
   - Base URL：`http://<host>:8963/v1`
   - Key：控制台中的密钥  
3. 模型填 Qoder 侧 ID，例如：`auto`、`qmodel_38max`、`qfmodel`、`gmodel`、`dmodel`、`kmodel_latest` 等  

### 接入 OpenCode

在 `~/.config/opencode/opencode.json` 中增加 provider：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "qoder2api": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Qoder2API",
      "options": {
        "baseURL": "http://127.0.0.1:8963/v1",
        "apiKey": "qccg"
      },
      "models": {
        "auto": { "name": "Qoder Auto" },
        "ultimate": { "name": "Ultimate" },
        "qmodel_latest": { "name": "Qwen Max" }
      }
    }
  },
  "model": "qoder2api/auto"
}
```

### 接入 Claude Code

```bash
# 示例：环境变量方式（也可写 ~/.claude/settings.json）
export ANTHROPIC_BASE_URL="http://127.0.0.1:8963"
export ANTHROPIC_AUTH_TOKEN="qccg"
export ANTHROPIC_MODEL="auto"
claude
```

### 命令行快速自测

```bash
curl http://127.0.0.1:8963/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer qccg" \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}]}'
```

---

## 数据目录

```text
~/.qoder2api/
├── accounts/          # 账号元数据 JSON
├── secrets/           # OAuth/PAT 凭证（权限 0600）
├── settings.json      # 端口、bridge_token 等
└── logs/              # 运行日志
```

服务器上请保证进程用户对 `$HOME/.qoder2api` 可写；用 Docker 时挂载数据卷到 `/data`（`HOME=/data`）。

---

## Docker 部署

```bash
docker build -t qoder2api .
docker run -d --name qoder2api \
  -p 3588:3588 -p 8963:8963 \
  -v qoder2api-data:/data \
  -e HOME=/data \
  qoder2api
```

然后访问：`http://<服务器IP>:3588` 完成 OAuth 登录。

## systemd 部署

```bash
sudo mkdir -p /opt/qoder2api
sudo cp qoder2api /opt/qoder2api/
sudo cp qoder2api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now qoder2api
```

---

## 项目结构

```text
qoder2api/
├── main.go              # 入口：Web + API
├── service.go           # 账号 / Bridge 业务
├── checkin.go           # 每日签到（手动 API + 10:00 自动调度 + 连续天数统计 + CN/Global 双区域）
├── account/             # 账号、OAuth、设置、密钥文件存储
├── cmd/checkin/         # 签到/额度只读调试工具
├── internal/
│   ├── bridge/          # OpenAI / Claude / Codex 兼容层 + 错误分类
│   └── cosy/            # Qoder 签名、会话与设备指纹
├── logger/
├── scripts/             # 签到辅助脚本与抓包工具
├── webconsole/          # 轻量 HTML 控制台
├── baseprompt.json
├── Dockerfile
└── qoder2api.service
```

---

## 致谢

- **[QCCG](https://github.com/wangtufly/QCCG)**（[wangtufly](https://github.com/wangtufly)）  
  本项目在 QCCG 之上进行二次开发：复用 / 精简了 Bridge、账号体系、OAuth 与 Qoder 协议相关实现，并改为无 GUI 的服务器友好形态。  
  **感谢原作者的开源工作。** 若你需要完整的 macOS 桌面端体验，请优先使用官方 QCCG。

- 上游思路与生态亦受益于 Qoder 社区及相关逆向/兼容项目（见 QCCG 仓库 README 中的鸣谢）。

---

## 免责声明

本项目仅供学习与自用。请遵守 [Qoder](https://qoder.ai) 服务条款与当地法律法规；账号与 API 使用风险自负。

---

## License

二次开发基于 [QCCG](https://github.com/wangtufly/QCCG) 的开源协议；请同时遵守原项目 [LICENSE](https://github.com/wangtufly/QCCG/blob/main/LICENSE) 的要求。
