# 高级配置与 API 兼容说明

日常部署见 [README](../README.md)。本页按需查阅，默认部署只需管理密码和加密主密钥。


## API

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/health` | 无认证健康检查 |
| GET | `/v1/status` | API 认证后的服务状态与并发 |
| GET | `/v1/models` | 当前账号真实模型目录，应用启用和别名配置 |
| POST | `/v1/chat/completions` | JSON / SSE，文本、图片、函数工具、多轮、reasoning |
| POST | `/v1/responses` | JSON / SSE，文本、图片、函数/自定义/namespace 工具、reasoning summary、previous_response_id |

支持 `Authorization: Bearer <API Key>` 和 `X-API-Key: <API Key>`。API Key 无法管理账号；管理 Cookie 也不能调用模型接口。浏览器同源使用，不开放任意来源 CORS。

```sh
curl http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer YOUR_API_KEY"

curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"从模型列表复制真实 id","messages":[{"role":"user","content":"你好"}],"stream":true}'

curl http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"从模型列表复制真实 id","input":"你好","stream":true}'
```

上面两个生成请求会调用真实上游并可能消耗额度，只在你准备好时运行。无账号时 `/v1/models` 和生成接口返回 `503 no_enabled_account`，不会模拟成功或提供虚构模型。

### 协议与兼容边界

- 上游为 `/api/agent/v3/llm_utils_chat`，使用 `Cloud-IDE-JWT` / IDE 请求头。模型从 `/api/ide/v1/get_detail_param` 的 `chat_v3` 和 `solo_agent` 目录合并，过滤隐藏、自定义和内部模型，正常每小时刷新，失败每五分钟重试；管理页或请求可即时触发拉取。
- `tools[].function.parameters` 从对象序列化为上游 JSON 字符串；工具历史改写为上游 `function_call`；按 index 拼接增量/快照参数，done 后输出完整调用。工具图片结果延迟补为标注来源的用户图片消息，保留连续工具结果的配对。
- 正文和 reasoning 均为增量，不会误删重复片段。只在真正收到 `done` / `[DONE]` 后成功；提前断流发出错误，不输出虚假的完成事件。HTTP 401 握手失败只刷新重试一次，已经发出的事件不重放。
- `reasoning_effort` / Responses `reasoning.effort` 仅取目录支持的值。管理页可设置模型别名、启用、默认思考档位和 Max；`is_max_mode`（数字或布尔）可按请求覆盖，使用目录的 `__max` 名称及最大上下文。
- `tool_choice: none` 移除工具；命名 choice 筛选声明；`required` 和命名 choice 最终仍为上游 `auto`，与参考实现相同，**不保证强制调用**。自定义工具包装为 `{input: string}` 函数工具，不执行 grammar 约束。
- `stop` 最多四条、每条 1024 字节，本地处理跨片段与 UTF-8 边界，仅截断正文，上游仍读到完成以获取用量，可能继续消耗额度。
- Responses 会话缓存按账号隔离，最近 64 条，单条历史上限 4 MiB，**只在内存保留**，重启或淘汰后返回 `previous_response_not_found`。`store:false` 不缓存。此版本没有原 Windows 可选的会话持久化，也没有 GET / DELETE / cancel Responses 接口。
- 用量只使用上游报告的 Token；个人缓存用量为 0 时，有限重试查询 `get_session_usage`，仅在 session 和输入/输出总量完全匹配时回填缓存。查询失败不影响生成结果。使用记录不保存消息正文。
- 不实现严格 JSON Schema、采样参数、seed、n、max_tokens / max_output_tokens 控制，以及 hosted web/file search、code interpreter、Files/PDF、音频、Embeddings、Images、Realtime、Batch。`background:true` 和不支持的内容/工具类型会被拒绝；其余部分未知参数会被忽略。需要严格 OpenAI 语义的客户端请先验证。
- 不支持国际版、企业 VPC / 私有部署。此版本支持手动个人积分查询和企业套餐额度查询，尚未移植自动签到、费率及实际消费金额统计；企业消费请在官方控制台查看。没有 Windows 自动发现、DPAPI、托盘和自启动。

## 可选环境变量

调整并发、Cookie 或 IDE 版本时，直接在 `.env` 中增加对应变量，再执行 `docker compose up -d --force-recreate`。Compose 内部监听固定为 `0.0.0.0:8317`，通过其 `environment` 明确设置；`RELAY_LISTEN`、`PORT` 和 `RELAY_DATA_DIR` 通常只在本地直接运行时调整。Docker 部署请保留容器端口 8317 和 `/data`，改变宿主端口只需编辑 `compose.yaml` 的 `ports`，例如 `"0.0.0.0:9000:8317"`。

HTTPS 反向代理部署时，添加 `RELAY_SECURE_COOKIE=true`，代理保留 Host / Origin，关闭 SSE 缓冲，读取超时至少 200 秒。直接通过内网 HTTP 访问时，保持默认 false。服务不自动信任 `X-Forwarded-*`。

| 环境变量 | 默认 / 要求 |
| --- | --- |
| `RELAY_MASTER_KEY` | 必填，32 随机字节的标准 Base64 |
| `RELAY_ADMIN_PASSWORD` | 必填，不能为空，建议随机 32 字节 |
| `RELAY_MAX_CONCURRENT` | 2，允许 1–8，每启用账号即时拒绝超限请求 |
| `RELAY_SECURE_COOKIE` | false，HTTPS 反向代理部署设 true |
| `RELAY_DATA_DIR` | 本地 `data`，镜像 `/data` |
| `RELAY_LISTEN` | `0.0.0.0:8317`，本地直接运行可设 `127.0.0.1:8317` |
| `PORT` | 8317，服务与镜像健康检查使用 |
| `TRAE_IDE_VERSION` | 3.4.0，来自参考登录实现默认值，可覆盖 |
| `TRAE_IDE_VERSION_CODE` | 20260919，协议参考值，可覆盖；应与实际可用 IDE 构建对应 |

IDE 版本头属于上游非公开兼容协议，本项目不自动探测或声称固定值永远有效。遇到模型目录异常时，先检查参考客户端当前版本与构建日期，再更新环境变量。

`state.enc` 包含账号凭据、API Key、模型设置和最近 500 条使用记录，整体采用 AES-256-GCM 加密；每次写入使用新随机 nonce，带版本 AAD，临时文件 fsync 后替换。主密钥来自部署环境，**不要与 volume 备份存放在同一位置**。备份和恢复必须同时保留原主密钥；密钥丢失无法恢复，加密文件损坏或密钥错误会拒绝启动。此版本不提供在线主密钥轮换。

管理页不回传 token / refresh token / 私钥；上游身份查询返回的 Password 不保存。默认运行日志只记录启动和本地存储错误，不记录请求体、回调、账号 token 或上游原始错误正文。生成失败的具体类别通过脱敏错误码及使用记录呈现。

管理会话 12 小时，HttpOnly + SameSite=Strict；重启和退出后需重新登录。所有管理写操作要求 JSON，同源 Origin 检查。管理登录限流，最多 64 个会话、8 个待授权流程。服务最多 64 个活跃 API/管理请求；请求体 16 MiB、头部 32 KiB、上游单 SSE 事件 2 MiB、累计事件 48 MiB、文本输出 32 MiB、128 个工具调用。单请求 180 秒，连接与响应头独立超时，下游写入超时 30 秒。客户端断开会取消上游；收到 SIGTERM / Ctrl+C 后取消在途请求并优雅退出。

账号变更会在有生成请求时返回 `409 requests_in_flight`。元数据请求与凭据刷新串行协调，避免删除账号后被刷新写回。单 volume 仅允许一个进程写入。

## 本地构建与验证

Go 1.25+，只依赖标准库：

```sh
go test ./...
go vet ./...
go build -trimpath -o relay .
./relay keygen  # 仅生成随机主密钥后退出
export RELAY_MASTER_KEY='上面的值'
export RELAY_ADMIN_PASSWORD='独立的随机管理密码'
export RELAY_LISTEN=0.0.0.0:8317
./relay
```

Windows 使用 `$env:RELAY_MASTER_KEY=...` 等环境变量和 `relay.exe`。Linux 交叉编译：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o relay-linux-amd64 .
```

测试通过本机临时 HTTP 服务替代传输层，生产主机地址仍固定为官方域名。测试覆盖加密防篡改与恢复、权限隔离、CSRF、密钥轮换、原生聊天协议、SSE/断流、工具/图片/思考、Responses 生命周期与多轮、PKCE/P-256、个人授权/旧刷新/企业刷新与身份验证、可信主机限制、用量归属、取消及并发。它们验证移植逻辑，不能替代官方真实兼容性测试。

完整验证记录见 [VALIDATION.md](../VALIDATION.md)。

## 管理接口补充

这些接口需要管理 Cookie，同样要求 JSON 请求和同源 Origin；API Key 不具备管理权限。

| POST 路径 | 请求示例 | 说明 |
| --- | --- | --- |
| `/admin/account/check` | `{"id":"账号 ID"}` | 身份 + 实时目录检查，不发送生成请求 |
| `/admin/account/balance` | `{"id":"账号 ID"}` | 查询个人积分 / 企业套餐额度 |
| `/admin/key` | `{"action":"create","name":"工作站"}` | 创建独立 Key，响应仅此时带完整密钥 |
| `/admin/key` | `{"action":"disable","id":"密钥 ID"}` | 另支持 enable、delete；rename 还需 name |

诊断接口以 `ok` 字段标识查询是否成功，并附检查时间。HTTP 200 本身不表示账号验证成功。余额与诊断结果保存在加密状态中。个人余额移植自原项目 `ide_user_ent_usage`：优先 usage_summary，其次积分事件，最后有效权益包；企业只显示官方 plan_attribute 套餐额度，不推算剩余金额。未使用真实账号实测。
