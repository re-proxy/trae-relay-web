# 下游余额查询 API

`GET /v1/balance` 使用现有 API Key，供下游 router 查询这台中继的**启用账号池剩余积分**。它不是某个 API Key 的独立额度，也不是 OpenAI 标准接口；router 需要支持自定义查询 URL 和响应字段。

```sh
curl http://192.168.1.10:8317/v1/balance \
  -H "Authorization: Bearer YOUR_API_KEY"
```

也支持 `X-API-Key: YOUR_API_KEY`。无需管理密码或管理 Cookie。已停用/撤销的 Key 不能查询。接口不会返回账号名称、用户身份、token、refresh token 或设备私钥。

## router 如何读取

| 配置 / 字段 | 含义 |
| --- | --- |
| 查询地址 | `http://服务器内网IP:8317/v1/balance` |
| 方法 | GET |
| 鉴权 | 与聊天请求使用同一个 Bearer / X-API-Key |
| 数值字段 | `balance`，JSONPath 通常写为 `$.balance` |
| 单位字段 | `unit`，固定为 `credits`（积分） |
| 完整性 | `complete`，只有全部启用账号都取得可合计的个人积分时为 true |
| 建议轮询周期 | 60 秒或更长 |

**积分不等于人民币或美元。** 如果 router 只接受美元余额或固定供应商的余额协议，需要在 router 侧配置解析/单位，不能直接伪装成货币。自动判断“额度耗尽”前，应确认 HTTP 200、`complete:true` 且 `balance` 为数值；`null` 不表示 0。

## 返回示例

以下是格式示例，不是真实账号额度：

```json
{
  "object": "balance",
  "balance": 125.5,
  "unit": "credits",
  "ok": true,
  "complete": true,
  "known_accounts": 1,
  "unknown_accounts": 0,
  "cache_ttl_seconds": 60,
  "accounts": [
    {
      "account_id": "账号的不透明 ID",
      "kind": "personal",
      "ok": true,
      "remaining": 125.5,
      "unit": "credits",
      "checked_at": "2026-11-06T08:00:00Z",
      "cached": false
    }
  ]
}
```

- `balance`：查询成功且数值有效的个人积分合计。没有任何已知个人余额时为 `null`，已知余额确实为 0 时返回数字 `0`。
- `known_accounts` / `unknown_accounts`：有可合计积分的账号数 / 其余账号数。部分失败时仍返回已知部分之和，但 `complete:false`，不可当作完整池余额。
- `accounts[].checked_at`：该账号上次实际查询完成的 UTC 时间；`cached:true` 表示本次复用了该结果。缓存命中不会重写该时间。
- `accounts[].ok`：该账号的查询是否成功；失败时 `remaining:null` 并附脱敏 `error` 码，不把过期的旧额度当成查询成功。
- `accounts[].kind`：`personal` 或 `enterprise`；查询开始前已删除/停用的账号可能显示 `unknown`。
- 企业账号的 `remaining` 始终为 `null`；可取得套餐额度时另返回 `plan_quota`、`unit:"CNY"` 和 `details_url`。套餐总额不会算入顶层积分合计，实际消费/剩余额度需到官方控制台查看。

## 缓存和错误

每个账号的结果缓存 60 秒，包括失败结果；管理页刚查询的余额也可复用。过期后自动调用现有上游余额接口并刷新加密持久化状态。并发轮询会合并等待，最多同时查询 4 个账号；每账号超时 12 秒，整个接口超时 40 秒。该查询不占聊天并发槽、不切换账号，也不发送模型生成请求。

| HTTP 状态 | 含义 |
| --- | --- |
| 200 | 至少一个账号查询成功；仍需检查 `complete` 和各账号状态 |
| 401 | API Key 缺失、无效、已停用或撤销 |
| 502 | 所有账号查询失败，返回余额对象和各账号错误，`balance:null` |
| 503 | 没有启用账号，或本机请求并发已满 |
| 504 | 整体查询/等待超时 |

余额会随上游消耗变化，缓存结果只代表 `checked_at` 时刻；它不是 router 的扣费账本。真实上游行为尚未使用真实账号实测，协议、缓存和鉴权已通过离线 HTTP 夹具验证。
