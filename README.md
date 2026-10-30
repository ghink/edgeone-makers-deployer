# edgeone-makers-deployer

这是一个专用于 EdgeOne Makers 构建产物部署的 Go 命令行工具，运行时无需 Node、pnpm 或 npm。文件上传使用腾讯云官方 COS Go SDK，部署控制接口按官方 `edgeone@1.6.47` 和 Makers SDK 的协议实现。

## 命令组织

命令行使用 Cobra，推荐通过 `edgeone-makers-deployer deploy [directoryOrZip]` 执行部署。

```sh
./edgeone-makers-deployer --help
./edgeone-makers-deployer deploy --help
./edgeone-makers-deployer help deploy
./edgeone-makers-deployer completion zsh
```

帮助信息按项目认证、部署选项、上传调优、等待与输出分组。每个参数只展示标准长名称和短名称；`--n`、`--t`、`--e`、`--a` 仍可使用，但不会重复列出。

同时兼容 `makers deploy`、`pages deploy`，也可以省略 `deploy`，直接传入产物路径。参数可以放在路径前后，`--` 后的内容则按路径处理。

根命令不带参数时展示帮助；使用 `deploy` 但不传路径时，上传当前目录。

工具提供 bash、zsh、fish 和 PowerShell 补全脚本。`completion <shell>` 只生成脚本，不修改 Shell 配置。查看帮助和生成补全脚本均无需 Token，也不会显示环境变量或本地凭证中的 Token。

## 构建与使用

需要 Go 1.27.3 或更高版本。

```sh
go build -trimpath -o edgeone-makers-deployer .

export EDGEONE_PAGES_API_TOKEN='your-api-token'
./edgeone-makers-deployer deploy dist --name your-project-name --site china
```

兼容原命令结构、短参数和双横线短参数：

```sh
./edgeone-makers-deployer makers deploy dist \
  --n "$EDGEONE_MAKER_PROJECT_ID" \
  --t "$EDGEONE_MAKER_API_TOKEN" \
  --site china --json
```

`--name` / `--n` 接收的是**项目名称**。如果变量中存放的是项目 ID，应改用 `--project-id`。建议通过环境变量传递 Token，避免它出现在进程的命令行参数中。

```sh
# 部署 ZIP 到预览环境
./edgeone-makers-deployer deploy site.zip -n your-project -e preview --site china

# 按项目 ID 部署，使用国际站 Token
./edgeone-makers-deployer deploy dist --project-id project-id --site global

# 匿名部署，结果包含 claimUrl，可用官方 CLI 或控制台认领
./edgeone-makers-deployer deploy dist --anonymous --site china --json

# 上传并创建部署后立即返回，status 为 created，不代表发布成功
./edgeone-makers-deployer deploy dist -n your-project --no-wait --json
```

## 官方部署选项兼容表

| 参数 | 默认值 | 行为 |
| --- | --- | --- |
| `[directoryOrZip]` | `.` | 上传已构建目录或现有 ZIP |
| `-n`, `--n`, `--name` | 无 | 查找项目名称；不存在时创建直接上传项目 |
| `-t`, `--t`, `--token` | 环境/本地凭证 | Makers API Token |
| `-e`, `--e`, `--env` | `production` | `production` 或 `preview` |
| `-a`, `--a`, `--area` | `global` | 新项目区域：`global` 或 `overseas`，现有项目忽略 |
| `--json` | `false` | stdout 输出一行 JSON，阶段日志写 stderr |
| `--skip-ai-gateway-sync` | `false` | 跳过对产物 `edgeone.json` 中 Agent framework 的云凭证绑定 |
| `--anonymous` | `false` | 无 Token 时走匿名接口；已有 Token 时走认证部署 |
| `--site` | 自动探测 | `china` 或 `global`，表示 Token 所属站点，与 `--area` 不同 |
| `-h`, `--help` | 无 | 查看帮助 |

Token 按以下顺序读取：命令参数、`EDGEONE_PAGES_API_TOKEN`、`EDGEONE_MAKER_API_TOKEN`、当前目录下的 `.edgeone/auth.json`。

站点信息支持通过环境变量 `EDGEONE_PAGES_API_REGION` 指定，也支持从本地 `auth.json` 的同名字段读取。未启用匿名部署且没有可用凭证时，工具会立即退出，不启动浏览器登录。

未指定项目名称或 ID 时，工具会尝试读取当前目录下的 `.edgeone/project.json`。它不会修改该文件，也不会改写构建产物中的项目 ID。

预览部署要求项目已有正式部署。工具与官方 CLI 一样，通过最近 50 条部署记录中的 `UsedInProd` 字段检查这一条件。部署仅支持 Provider 为 `Upload` 的项目。

匿名 API 不支持指定项目名称、项目 ID、预览环境或部署区域。匿名部署时显式组合这些选项会报错。

未指定匿名部署站点时，工具通过官方 IP 接口探测；探测失败则使用国际站。成功结果中包含认领凭证，请妥善保存。此工具不实现 `claim` 子命令。

## 上传与等待选项

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `--project-id` | 无 | 直接选择已有项目，优先于名称 |
| `--compression` | `1` | ZIP 压缩等级 0–9，0 不压缩；现有 ZIP 不重打包 |
| `--concurrency` | `8` | COS 分片并发数，1–64 |
| `--part-size` | `8` | 分片大小，单位 MiB，1–5120 |
| `--accelerate` | `true` | COS 传输加速，`--accelerate=false` 使用区域域名 |
| `--timeout` | `15m` | 包括打包、上传、部署等待的总期限 |
| `--request-timeout` | `30s` | 单次控制 API 请求超时；COS 响应头等待期限 |
| `--poll-interval` | `5s` | 部署状态查询间隔 |
| `--no-wait` | `false` | 创建部署后返回，不查询发布状态和访问 URL |

退出码为：0 表示成功，1 表示执行失败，2 表示参数错误。使用 `--no-wait` 时，退出码 0 仅表示部署已创建，不代表发布完成。

收到 SIGINT 或 SIGTERM 后，工具会取消正在执行的请求。连续三次轮询请求失败，或超过部署总期限时，也会退出；已经创建的远端部署可能继续执行。控制 API 的写请求不会自动重放，以免重复创建资源。

运行时会分阶段输出耗时，包括 `package`、`resolve-site`、`resolve-project`、`credentials`、`upload`、`create-deployment`、`wait` 和 `resolve-url`。成功时的 JSON 结果包含 `timings` 和 `durationSeconds`，可用于区分本地打包、文件传输和服务端发布的耗时。

## 产物约定与边界

此工具用于上传已有构建产物，**不会运行前端 build、安装业务依赖、生成 Functions、同步 .env 或调用 Node 构建器**。官方 CLI 在不传路径时会自动构建，本工具则直接上传当前目录。请先在 CI 中完成构建，再传入 `dist`、`out`、`.edgeone` 或 ZIP。

普通目录沿用官方 CLI 的目录前缀打包方式，例如 `dist/index.html`。`.edgeone` 目录保留 `项目名/.edgeone/…` 布局，并加入同级的 `edgeone.json`。已有 ZIP 会原样上传，调用方需确保归档布局正确。

打包目录时，工具会忽略 `.git`、`.cache`、`.DS_Store`、`*.log` 和 `*.tmp`。普通目录还会忽略 `node_modules`、`.edgeone`、`.env` 和 `.env.*`；直接上传 `.edgeone` 时，则保留运行所需的 `node_modules`。

目录中的符号链接和特殊文件会被拒绝。对于已有 ZIP，工具会检查并拒绝路径穿越、绝对路径、重复路径和符号链接，但不会按目录排除规则重新过滤内容。

AI Gateway 绑定会从归档根目录或其下一级目录中的 `edgeone.json` 读取 `agents.framework`。支持的框架与官方一致，包括 `claude-agent-sdk`、`openai-agents-sdk`、`langgraph`、`crewai` 和 `deepagents`。

没有 Agent 配置时，工具不会发送绑定请求。构建产物所需的环境变量应在构建阶段配置。

工具采用全量 ZIP 上传，不提供服务端尚未公开支持的增量部署。默认使用较低的压缩等级，以减少 CPU 耗时；带宽较小时，可以适当提高压缩等级。

实际提速取决于构建产物、CI 网络和服务端发布时间。目前尚未使用真实 Token 进行线上部署或性能对比。

## 验证与接口依据

```sh
go test -race -cover ./...
go vet ./...
```

测试使用模拟的 Makers API 和 COS HTTP 响应，验证认证与匿名部署、预览前置检查、签名上传、ZIP 安全检查、访问 Token、错误脱敏，以及取消和超时行为。测试不会创建线上资源。

- [官方 CLI 文档](https://pages.edgeone.ai/document/edgeone-cli)
- [官方 CLI 1.6.47 元数据](https://registry.npmjs.org/edgeone/1.6.47)
- [官方 Makers SDK](https://pages.edgeone.ai/document/sdk-overview)
- [部署接口说明](https://pages.edgeone.ai/document/deployment)
- [腾讯云 COS Go SDK](https://github.com/tencentyun/cos-go-sdk-v5)

Makers 控制接口使用 `/v1` 下的 Action 协议，匿名接口使用 `/pages-public/*`。实现以官方发布的工具和 SDK 为依据，后续官方协议调整时，本工具可能需要相应更新。