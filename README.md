# Trae Relay Web

独立的 Go Web 服务，把 Trae 国内个人 / 公共 SaaS 企业账号转换成 OpenAI 兼容 API。单个进程内嵌中文管理页面，可部署到 Linux Docker。根据 Trae Relay **0.21.0** 的 C++ 实现移植真实上游协议；原 Windows 项目未改动。

**当前交付状态：可编译、离线协议测试通过，Windows 与 WSL Ubuntu 的 Linux 无凭据启动、SIGTERM 退出及重启恢复已验证。未使用真实账号登录、刷新或生成；Windows 和 WSL Ubuntu 均未发现 Docker，镜像构建与容器运行尚未实测。** 请先在自己的测试账号上验证官方登录和模型兼容性，再用于生产。

## Docker 部署

需要 Docker Engine + Compose v2。以下命令在本目录执行：

```sh
cp .env.example .env
openssl rand -base64 32   # 填入 RELAY_MASTER_KEY
openssl rand -base64 32   # 另生成一个随机值，填入 RELAY_ADMIN_PASSWORD
# 编辑 .env 后启动
docker compose up -d --build
docker compose logs -f --tail=50
curl http://127.0.0.1:8317/health
```

PowerShell 生成随机值的替代命令（执行两次，分别填写）：

```powershell
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
[Convert]::ToBase64String($bytes)
$rng.Dispose()
```

管理页：[http://127.0.0.1:8317](http://127.0.0.1:8317)。使用 `.env` 的管理密码登录，添加账号并启用，刷新模型目录。在“API 接入”生成并复制 API Key；新密钥只显示一次。启动时已生成的初始 Key 可直接轮换，不会返回到概览接口。

Compose 默认只绑定服务器 `127.0.0.1`，数据保存在 `relay-data` volume。远程访问推荐先建立 SSH 隧道：

```sh
ssh -N -L 8317:127.0.0.1:8317 user@your-server
```

然后在自己电脑打开 `http://127.0.0.1:8317`。需要公网服务时，通过你自己的 HTTPS 反向代理连接本机 8317，设置 `RELAY_SECURE_COOKIE=true`，保留 Host / Origin，并关闭代理 SSE 缓冲、设置至少 200 秒读取超时。服务不会信任任意 `X-Forwarded-*` 来决定安全 Cookie。也可在 Compose 网络中给反向代理接入 `relay:8317`。

镜像采用非 root 用户、只读根文件系统、最小权限；volume 为可写目录。不要同时启动多个副本共享同一数据目录，本项目采用单进程加密快照存储。

## 远程部署如何登录

没有假定官方接受远端回调，也没有要求容器读取宿主 Windows 凭据。流程如下：

1. 在管理页点击“添加账号”。服务生成 48 字节 PKCE verifier、S256 challenge、P-256 设备密钥及 30 分钟有效的授权会话。
2. 点击官方登录链接，在自己的浏览器完成个人或企业登录。授权参数仍使用原项目的 `http://127.0.0.1:<随机端口>/authorize` 回调格式。
3. 浏览器通常会显示本机回调地址无法访问，因为本机没有监听该随机端口。**从地址栏复制完整的最终 URL**，返回管理页粘贴并提交。不要把地址发给别人，也不要复制到日志、截图或命令行。
4. 服务验证回调 scheme、host、port、path、重复参数、会话时效；个人账号用该会话的 PKCE verifier 交换授权码并查询用户身份。企业账号还验证 `loginTraceID`，只向固定公共 SaaS 主机刷新并验证企业身份。
5. 凭据加密保存，账号默认不启用。点击“启用”，再刷新模型目录。

这是一个已实现、可操作的粘贴流程，但**官方网页是否在无本机监听器时仍将完整授权参数留在地址栏，尚未用真实账号实测**。如官方页面未进入带参数的回调 URL，请停止并记录脱敏后的页面现象；不能用猜测参数或修改远端回调来绕过。每次提交均消费该登录会话，失败后重新创建；个人路径由 PKCE 绑定授权码，企业路径由 trace ID 绑定会话。

设备公钥按 SPKI PEM 导出，私钥按跨平台 SEC1 DER 保存；刷新签名是 ECDSA P-256 / SHA-256 ASN.1 DER，与原 CNG 实现的线上签名格式一致。设备元数据保留原客户端的 Windows 协议字段以兼容上游，它不表示容器运行于 Windows。

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
- 不支持国际版、企业 VPC / 私有部署。此版本未移植个人积分查询、自动签到、费率与金额统计；企业消费请在官方控制台查看。没有 Windows 自动发现、DPAPI、托盘和自启动。

## 配置与数据

| 环境变量 | 默认 / 要求 |
| --- | --- |
| `RELAY_MASTER_KEY` | 必填，32 随机字节的标准 Base64 |
| `RELAY_ADMIN_PASSWORD` | 必填，至少 16 字符，建议随机 32 字节 |
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
export RELAY_LISTEN=127.0.0.1:8317
./relay
```

Windows 使用 `$env:RELAY_MASTER_KEY=...` 等环境变量和 `relay.exe`。Linux 交叉编译：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o relay-linux-amd64 .
```

测试通过本机临时 HTTP 服务替代传输层，生产主机地址仍固定为官方域名。测试覆盖加密防篡改与恢复、权限隔离、CSRF、密钥轮换、原生聊天协议、SSE/断流、工具/图片/思考、Responses 生命周期与多轮、PKCE/P-256、个人授权/旧刷新/企业刷新与身份验证、可信主机限制、用量归属、取消及并发。它们验证移植逻辑，不能替代官方真实兼容性测试。

完整验证记录见 [VALIDATION.md](VALIDATION.md)。

## 实现来源

协议参考只读原项目的 `src/upstream/UpstreamSolo.cpp`、`Upstream.cpp`、`ModelCatalog.cpp`，`src/accounts/WebLogin.cpp`、`TokenRefresh.cpp`、`Saas.cpp` 和 `src/api/Facade.cpp`、`ResponsesApi.cpp`。新项目没有依赖原 Windows 可执行文件、用户目录或 DPAPI。原项目目录未发现独立 LICENSE 文件；本交付不擅自为原代码或衍生协议移植授予开源许可，公开分发前请确认原作者授权。
