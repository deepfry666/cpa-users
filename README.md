# cpa-users：CLIProxyAPI 用户分发插件

给 CLIProxyAPI（CPA）加一层很薄的“用户”概念：每个人一个或多个 Key，在 CPA 管理中心里直接管理他们**能看到 / 能调用的模型**、**额度**和**用量**。

- 管理页面直接嵌在 **Oh My CPA（OMC）** 和 CPA 管理中心（CPAMC）的插件页里，跟随面板的主题和登录状态。
- 给用户一个自助查询链接，他们输入自己的 Key 就能看到可用模型、剩余额度和最近用量。
- 原生 `api-keys` 完全不受影响，适合留给管理员自己用。

## 能做什么

| 功能 | 说明 |
|---|---|
| 用户 + 多 Key | Key 形如 `sk-cu-` + 40 位随机字符，只存 SHA-256，签发时显示一次 |
| 模型分组 | 一组可复用的模型白名单；用户还能单独追加模型。支持通配符，`*` 表示全部 |
| 模型列表裁剪 | `/v1/models`、`/v1beta/models`、Claude 格式的模型列表都只返回该用户可用的模型 |
| 调用拦截 | 无权限的模型直接返回 403（按 OpenAI / Claude / Gemini 各自的错误格式），不会到达上游 |
| 额度 | 每日 / 每月 Token、每日 / 每月请求次数、最大并发、到期日；超出返回 429 + `Retry-After` |
| 用量统计 | 按用户 / Key / 模型 / 天统计，原生 Key 的流量也会记录为“其他 / 原生 Key” |
| 请求记录 | 每次请求的模型、状态、Token、耗时、首字时间、失败原因 |

## 依赖

- CLIProxyAPI **v8.0.37 及以上**（在 v8.0.57 上完整测试过），并且是**支持插件**的构建（不能用 `no-plugin` 版本）。
- 构建插件需要 Go 1.24+ 和 C 编译器（插件是 cgo 动态库，内置 SQLite）。

## 安装（推荐：从插件商店）

插件源地址：

```
https://raw.githubusercontent.com/deepfry666/cpa-users/main/registry.json
```

**Oh My CPA**：设置 → 插件商店 → 第三方插件源，添加上面的地址；然后到插件商店搜索「用户分发」安装（需要输入插件 id `cpa-users` 确认）。

**CPAMC**：在 `config.yaml` 的 `plugins.store-sources` 里加上这个地址，再到插件商店安装。

安装后在插件设置里至少填两项：

| 配置 | 示例 | 为什么 |
|---|---|---|
| `public_base_url` | `https://cpa.example.com` | 用户访问 CPA 的地址。OMC 里的页面域名是控制台，不是 CPA，自助查询链接和接入说明靠它生成 |
| `timezone` | `Asia/Shanghai` | 按哪个时区切日 / 切月 |

Docker 部署时 `db_path` 默认写到 `plugins/cpa-users.db`，也就是挂载出来的插件目录，容器重建后数据还在。

## 从源码构建

```bash
make build      # 产物：dist/plugins/<os>/<arch>/cpa-users.{dylib,so,dll}
make test       # 单元测试 + 完整流程测试
```

服务器是 Linux 时，在 Linux 上编译，或者用 Docker：

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 make build
```

## 手动安装

1. 把产物复制到 **CPA 工作目录**下的 `plugins/<os>/<arch>/`，文件名必须是 `cpa-users.<扩展名>`，例如 `plugins/linux/amd64/cpa-users.so`。
2. 在 CPA 的 `config.yaml` 里启用：

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       cpa-users:
         enabled: true
         priority: 10
         timezone: "Asia/Shanghai"      # 按哪个时区切日 / 切月
         db_path: "plugins/cpa-users.db" # 建议改成绝对路径
   ```

3. 重启 CPA，打开管理中心，左侧栏会出现「用户分发」。

## 使用流程

1. **模型分组**：比如建「基础」（`gpt-5*`、`deepseek-v4-flash`）和「全部」（`*`）。模型列表来自 CPA 凭证，可以点「从 CPA 刷新」。
2. **用户**：新建用户，选分组，按需追加模型、设置额度。保存后会直接弹出签发 Key 的窗口。
3. **发给用户**：点「复制接入说明」，里面有 Base URL、Key 和自助查询链接。
   - OpenAI 兼容客户端：Base URL 填 `https://你的地址/v1`。
   - Claude Code：`ANTHROPIC_BASE_URL=https://你的地址`，`ANTHROPIC_AUTH_TOKEN=<Key>`。
4. **看用量**：「概览」页有每日用量图、每个人的额度进度和按模型统计；「请求记录」页可以看失败原因。

管理页不用单独登录：

- 在 **OMC** 里，页面请求管理接口时不带密钥，由 OMC 在服务器端注入它保存的管理密钥。
- 在 **CPAMC** 里，读取登录时“记住密码”保存的管理密钥；没勾选的话第一次会让你输入一次（只存在本标签页）。

## 配置项

| 配置 | 默认值 | 说明 |
|---|---|---|
| `db_path` | `plugins/cpa-users.db` | SQLite 文件，相对路径以 CPA 工作目录为基准 |
| `timezone` | `Local` | 额度和统计按哪个时区切日 / 切月 |
| `filter_model_list` | `true` | 是否裁剪模型列表。打开时所有非流式响应都会经过插件一次，关掉可以省一点开销，但调用拦截照常生效 |
| `key_prefix` | `sk-cu-` | 签发 Key 的前缀，插件只处理带这个前缀的 Key |
| `log_retention_days` | `30` | 请求明细保留天数；按小时汇总的用量永久保留 |
| `public_base_url` | 空 | 用户访问 CPA 的地址，用于自助查询链接和接入说明；在 OMC 中必须填写 |

## 工作原理（用到的 CPA 插件钩子）

| 钩子 | 用途 |
|---|---|
| `frontend_auth`（非独占） | 识别插件签发的 Key，身份写成 `cpa-users/<用户ID>/<KeyID>`；其他 Key 交回 CPA 原生鉴权 |
| `request.intercept_before` | 用 CPA 传来的 caller scope（身份的 SHA-256）找到用户，检查模型权限、额度、并发 |
| `request.complete` | 释放并发槽位 |
| `response.intercept_after` | 只处理模型列表响应，按请求头里的 Key 裁剪 |
| `usage.handle` | 记录 Token 用量，更新额度计数 |
| `management.*` | 管理 API（`/v0/management/cpa-users/...`，需要管理密钥）和页面（`/v0/resource/plugins/cpa-users/ui`、`/me`） |
| `host.auth.models` | 从 CPA 凭证读取可选模型 |

## 已知限制

- **额度是软限制**：Token 上限在请求开始前检查，单个请求可能略微超出，下一次请求才会被拒；几个请求同时进来时也可能一起放行。
- **不按金额计费**：只按 Token 和次数计量，没有模型单价和充值。需要计费可以看 cpa-key-billing，或者在前面套 new-api / sub2api。
- **不绑定上游账号**：所有用户共用 CPA 的凭证池和调度策略。
- 用 `?key=` 查询参数请求模型列表时，列表不会被裁剪（CPA 只把请求头传给插件），但调用仍会被拦截。
- 数据存在单机 SQLite，多个 CPA 节点之间不共享。
- Key 丢了不能找回，只能重新签发。
- 插件依赖 CPA 计算 caller scope 的方式（`sha256("cli-proxy-api:caller-scope:v1\0" + 身份)`）。CPA 如果改了这个算法，需要同步更新 `state.go`。
- 在 CPAMC 里直接访问管理 API 受 CPA 的 `remote-management.allow-remote` 控制；在 OMC 里由 OMC 代理，不受影响。
- 预编译版本提供 linux（amd64 / arm64，glibc 2.31 及以上）和 macOS（arm64 / amd64）。Alpine、OpenWrt 等 musl 系统和 Windows 需要自己编译。

## 发布

推送 `v*` 标签后，GitHub Actions 会跑测试、编译各平台版本，并按插件商店要求的格式发布 Release：`cpa-users_<版本>_<系统>_<架构>.zip`（压缩包根目录是 `cpa-users.<扩展名>`）和 `checksums.txt`。发布前同步修改 `registry.json` 里的 `version`。

## 端到端测试

`test/` 里有一套本地端到端测试：一个假的 OpenAI 兼容上游、一份测试用 CPA 配置和 28 项检查。

```bash
python3 test/mock_upstream.py 18400 &
# 在一个放了 cli-proxy-api、plugins/<os>/<arch>/cpa-users.* 和 test/config.e2e.yaml 的目录里：
./cli-proxy-api -config config.e2e.yaml -local-model &
python3 test/e2e.py
```

测试脚本假设数据库是空的；重跑前删掉 `plugins/cpa-users.db*`。
