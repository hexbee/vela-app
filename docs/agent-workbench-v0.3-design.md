# 船帆座 · Vela v0.3 轻量 Agent 工作台架构与功能设计

> **状态**：设计终稿（已合入 `docs/agent-workbench-v0.3-review.md` 全部评审修订）  
> **代码基准**：`vela-app` `v0.2.3`  
> **目标**：在不破坏 Vela「4~8 MB 单文件、零 Node/Python 运行时依赖、极简安静」核心基因的前提下，将其从纯 DeepSeek 聊天客户端升级为**轻量原生 Agent 工作台（Lite Harness）**。

---

## 1. 产品定位与核心原则

1. **聚焦 DeepSeek 单点打透**：优先把 DeepSeek 生态做到极致（开放 `deepseek-flash` / `deepseek-v4-pro` 模型切换与深度思考开关），多模型与自定义 Base URL 延后至后续版本。
2. **UI 向 DSH 对齐，底层做 Go 轻量化重构**：界面交互与视觉语言尽量向 [deepseek-harness](https://github.com/deepseek-ai/deepseek-harness)（DSH）对齐以降低学习成本；底层彻底剥离 Cordis 全插件内核与 Node/Python 运行时，用纯 Go + MyGo 原生实现核心执行层。
3. **架构统一（一套引擎走到底）**：普通闲聊会话与挂载工作区的项目会话，**共用同一套 Agent Loop、工具注册表与三档权限体系**，不维护「纯聊天」和「Agent 模式」双套割裂代码。

---

## 2. 与 `deepseek-harness`（DSH）的对照关系总览

本表明确每个模块参考 DSH 的具体源码位置，以及采用策略（**UI 1:1 对齐** / **部分复刻** / **架构参考** / **Vela 独立设计**）：

| Vela 模块                      | DSH 参考路径（`deepseek-harness/`）                                                           | 参考策略                           | 具体取舍说明                                                                                                                                                                                                                                                                                                                                                                                                   |
| :----------------------------- | :-------------------------------------------------------------------------------------------- | :--------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **侧边栏工作区分组**           | `packages/client/ui-workspace`<br>`packages/workspace/workspace`                              | **UI 对齐 + 结构减法**             | 对齐「工作区可折叠分组 + 未分组」视觉与交互；**去掉** DSH 侧栏顶部的“插件 / 自动化任务 / 更多”重菜单，未分组区沿用 Vela 现有按时间分组                                                                                                                                                                                                                                                                         |
| **输入框工作区下拉**           | `packages/client/ui-workspace`<br>`packages/client/ui-directory-picker-native`                | **UI 1:1 对齐（精简）**            | 复刻输入框左上角 `📁 工作区 v` 下拉（已添加工作区列表 + 分隔线 + `+ 添加工作区...`）；**去掉**旁边的“标准模式”下拉                                                                                                                                                                                                                                                                                             |
| **三档权限下拉菜单**           | `packages/client/ui-permission-presets`<br>`packages/interaction/permission-presets`          | **UI 1:1 对齐 + 底层渐进**         | 1:1 对齐输入框左下角三档文案与盾牌图标（`仅可查看` / `工作区内修改` / `完全权限`）；一期先由工具层 + 分级审批承接，二期补 OS 内核沙箱                                                                                                                                                                                                                                                                          |
| **模型与深度思考菜单**         | `packages/client/ui-model-selection`                                                          | **UI 对齐 + 协议收口**             | 对齐输入框右下角弹出卡片样式（`模型 >`、`深度思考`开关）；严格匹配当前 DeepSeek 协议，**不做**多档推理假 UI，并**去掉**“智能体团队协作”开关                                                                                                                                                                                                                                                                    |
| **工具调用 / Diff / 审批卡片** | `packages/client/ui-tool`<br>`packages/client/ui-trajectory`<br>`packages/client/ui-approval` | **UI 对齐**                        | 复刻紧凑折叠的工具轨迹条、红绿 Diff 预览卡片与一次性人工审批交互；模型返回文本与 UI 展示元数据彻底分离                                                                                                                                                                                                                                                                                                         |
| **产出文件操作与预览**         | `packages/client/ui-file-actions`<br>`packages/client/ui-deliverables`                        | **参考交互（分期交付）**           | Phase 1（M3）提供基础「打开文件 / 访达定位」；Phase 2 补齐「另存为」与增强预览卡片                                                                                                                                                                                                                                                                                                                             |
| **临时会话懒创建目录**         | `packages/workspace/workspace`（对比项）                                                      | **Vela 独立设计**                  | 不采用 DSH 首次启动强制建默认目录的做法；采用 **Go 后端透明懒创建 + `bash` 事后空目录自动清理**，对模型完全透明                                                                                                                                                                                                                                                                                                |
| **Agent Loop 状态机**          | `packages/core/agent-loop`                                                                    | **架构参考（轻量复刻）**           | 借鉴「并发只读 + 独占写屏障」「取消/异常自动补齐残缺 `tool_result`」「KV Cache 仅追加友好」；去掉 Cordis 事件总线与多处持久化投影抽象                                                                                                                                                                                                                                                                          |
| **内置文件工具集**             | `packages/fs/tool-fs`<br>`packages/fs/tool-fs-search`<br>`packages/fs/fs-observation-policy`  | **部分复刻 + Go 原生实现**         | 复刻 `read`/`write`/`edit` 参数与有界截断信封；`glob`/`grep` 用 Go 原生遍历+正则实现（不打包 `rg` 二进制）；**新增** Vela 独有的回收站式 `delete_file`；暂缓 `apply_patch` 与 `spill`                                                                                                                                                                                                                          |
| **Bash / CLI 命令执行**        | `packages/shell/tool-bash`<br>`packages/subprocess`<br>`packages/sandbox/sandbox-local`       | **部分复刻 + 安全增强**            | 复刻环境变量清洗、非零退出码作正常结果返回、二期超时转后台 Job 与 macOS `sandbox-exec`；**新增**面向普通用户的「严格只读白名单自动放行 + 写/删命令一次性审批」分级策略                                                                                                                                                                                                                                         |
| **Skills 渐进式加载**          | `packages/skill/skill-filesystem`<br>`packages/skill/tool-skill`                              | **部分复刻**                       | 复刻 `SKILL.md` YAML frontmatter、摘要目录 `<system-reminder>` 注入、`skill` 工具按需读全文与 `/name` 用户显式触发；只扫 `<workspace>/.agents/skills` 与 `~/.vela/skills`                                                                                                                                                                                                                                      |
| **MCP 客户端**                 | `packages/mcp/mcp-client`<br>`packages/mcp/mcp-manager`                                       | **部分复刻 + 架构参考 + 另行实现** | **复刻** `mcp__<serverName>__<rawName>` 命名、`on-demand` 按需加载、`stdio` 环境清洗、无副作用 `probe` 与指数退避重连；**参考**双层连接/编队管理但去 Cordis 化；**另行实现**基于官方 `modelcontextprotocol/go-sdk` 的 Go 桥接、`streamable-http` 优先策略（含静态 `headers` 鉴权）与 `stdio` 登录 Shell `PATH` 探测 / `npx`·`uvx` 缺失明确提示；暂缓 OAuth、独立 `mcp-resources` 与插件动态注册（详见 6.3 节） |

---

## 3. UI 与交互设计（向 DSH 对齐）

### 3.1 侧边栏（Workspace + 常规会话共存）

- **顶部区域**：保持 Vela 现有极简结构（双帆徽标 + 搜索 + 收起 + 新建对话），不增加插件市场、自动化任务等重导航。
- **工作区区块**：位于会话列表上方，展示已添加的本地工作区（文件夹图标 + 目录名，可折叠/展开），右侧提供添加工作区入口；每个工作区下按时间倒序列出归属该目录的会话，点击该工作区下的「新会话」自动绑定该目录。
- **普通对话区块**：位于工作区下方，沿用 Vela 现有按时间分组（今天 / 昨天 / 7 天内等），承接未挂载目录的普通闲聊与临时会话。

### 3.2 输入框（Composer）外围控件

1. **顶部左侧 — 工作区下拉（对齐 DSH）**：
   - 显示当前绑定目录名（如 `📁 vela-app v`）；未挂载目录时显示 `📁 未选择工作区 v`（触发临时文件生成后显示 `📁 临时目录`，点击可打开目录）。
   - 展开后列出已添加工作区（带选中勾选 `✓`）、分隔线以及 `+ 添加工作区...`（调用原生目录选择器）。
2. **底部左侧 — 三档权限下拉（1:1 对齐 DSH）**：
   - 位于 `+` 附件按钮右侧，采用三态盾牌图标与中文文案：
     - **仅可查看**：只开放只读文件工具与严格白名单只读命令。
     - **工作区内修改（默认）**：允许在当前工作区（或会话临时目录）内修改文件，非只读命令走一次性审批。
     - **完全权限**：允许跨目录操作与命令执行（高危/越界操作仍走统一预检与一次性确认）。
   - 权限按会话独立记忆，会话中途可随时切换。
3. **底部右侧 — 模型与深度思考控制（对齐 DSH 并收口至现有协议）**：
   - 显示当前模型与思考状态（如 `DeepSeek-Flash ^` / `DeepSeek-V4-Pro · 思考 ^`），点击弹出紧凑卡片：
     - **模型**：`DeepSeek-Flash`（`deepseek-flash`）/ `DeepSeek-V4-Pro`（`deepseek-v4-pro`）
     - **深度思考**：二元开关（对齐现有 `thinking: { type: 'enabled' | 'disabled' }`，后续协议支持多档位时再扩展为推理等级）

### 3.3 工具执行、审批与产物展示

- **工具调用轨迹卡片**：默认紧凑折叠显示工具名、简短 `description` 与状态图标（运行中 / 完成 / 拒绝 / 错误）；展开可查看参数、输出或红绿 `Diff`。
- **一次性审批卡片**：当需要执行非只读命令或越界文件操作时，在对话流内嵌审批卡片，高亮展示**工具名、完整命令/目标路径、工作目录与原因说明**，提供「允许这一次」与「拒绝」（不做“永久允许”）。
- **产出文件芯片**：工具创建或修改了文件（如 HTML 小游戏、脚本、报表）后，展示文件操作条；**Phase 1（M3）**支持「打开文件（默认应用/浏览器）」「在访达/资源管理器中显示」，**Phase 2** 补充「另存为」与增强预览。

---

## 4. 临时闲聊与透明懒创建隔离目录（Vela 独立设计）

为解决「统一 Agent 底座下，普通闲聊既不误建垃圾目录、又能顺畅生成 HTML 小游戏或运行脚本」的问题，采用 **Go 后端透明懒创建 + 事后空目录回收**，**不暴露为模型显式工具**：

1. **目录路径与归属**：
   - 临时目录固定位于 `~/.vela/scratch/<sessionId>/`（按会话完全隔离）。
   - 自动分配的临时目录**不加入侧边栏的工作区列表**，避免污染用户的项目工作区。
2. **分工具触发规则与边界保护**：
   - **`write` / `edit`**：必然产生文件 → 若会话尚未绑定目录，Go 后端原地 `os.MkdirAll` 创建 `~/.vela/scratch/<sessionId>/`，在结果中通过 `assignedScratch` 返回并由前端写回 `session.scratchDir`、点亮 `📁 临时目录`。
   - **`bash` / `run_command`**：未必产生文件（如 `go version`、`python3 -c`、`curl`）→ 执行前先对目标目录做文件快照；若本次调用前 `Workdir` 为空，Go 后端先临时建好 `~/.vela/scratch/<sessionId>/` 作为本次命令的安全 `cwd`。**命令进程组完全退出（`Wait()` 返回）后**：
     - **仅当本次调用前 `Workdir` 为空（本次新懒创建）**且目录仍为空 → 直接 `os.Remove` 删除空目录，**不绑定会话、UI 不显示 `📁 临时目录`**，磁盘零残留（绝不误删已绑定的用户工作区或此前已绑定的临时目录）。
     - 目录非空 → 若为本次新懒创建，正式回传 `assignedScratch` 绑定会话；同时通过**执行前后目录快照 diff** 计算本次真正新增/修改的文件列表写入 `uiMeta.producedFiles`，避免后续 `bash` 重复上报旧文件。
3. **防闲聊误触的系统上下文**：
   - 请求组装时注入当前工作区状态：未挂载用户目录时明确提示模型「当前为无项目目录的临时会话；常规问答直接回答，无需探测磁盘；仅当用户需要生成文件、可运行 Demo 或执行脚本验证时再调用工具」。
4. **生命周期清理**：
   - 删除关联了临时目录的普通会话时，确认弹窗提供「同时删除该对话生成的临时文件」选项（默认勾选），防止长期堆积。

---

## 5. 安全防线与权限模型

| 权限档位                 | 文件工具权限                                                                                                                | 命令执行（`bash` / `pwsh`）权限                                                                                                      |
| :----------------------- | :-------------------------------------------------------------------------------------------------------------------------- | :----------------------------------------------------------------------------------------------------------------------------------- |
| **仅可查看**             | 仅允许 `read`、`glob`、`grep`（工作区内）                                                                                   | 仅允许严格白名单只读命令，其余直接 `deny`                                                                                            |
| **工作区内修改（默认）** | 允许 `read`/`write`/`edit`/`delete_file`/`glob`/`grep`，**严格锁死在工作区或会话临时目录内**；越界访问走 `ask`（或 `deny`） | 严格只读白名单自动放行；**其余所有写操作、复合命令、未知 flag 与删除类命令一律弹一次性审批卡片（`allowed-once`）**；二期叠加 OS 沙箱 |
| **完全权限**             | 允许访问工作区外路径（高危删除/敏感路径仍走 `ask` 确认）                                                                    | 命令可执行（高危删除/越界写仍可按规则走 `ask` 确认）                                                                                 |

核心安全不变量：

- **规范化路径强校验（兼容新建文件与防软链逃逸）**：
  1. 授权根目录本身先做 `filepath.EvalSymlinks`（规避 macOS `/var` → `/private/var`、`/tmp` → `/private/tmp` 等软链差异）。
  2. 对目标路径逐级向上解析到**最近已存在的祖先目录**并做 `EvalSymlinks` 校验其归属授权根目录，同时校验剩余未创建相对路径段不含 `..`（避免新建文件或多级子目录时直接 `EvalSymlinks` 报 `os.ErrNotExist`）。
  3. `delete_file` 必须使用 `os.Lstat` 检查目标本身，**绝不跟随软链接删除工作区外文件**。
- **严格只读命令白名单（防参数绕过与跨平台差异）**：
  - 改为**严格子命令 + 允许参数白名单**（例如 `git diff` 显式禁止 `--output`/`--ext-diff` 及全局 `-c`；禁止 `find -exec`/`-delete`）。
  - 凡包含任何未明确放行的 flag、或含 POSIX/PowerShell 多语句/管道/重定向/命令替换符（`|`、`>`、`<`、`;`、`&`、`&&`、`||`、`$(...)`、反引号、`Out-File`、`Set-Content` 等），一律降级为 `ask`（`read-only` 模式下直接 `deny`）。
- **回收站式安全删除**：内置 `delete_file` 默认将文件移入系统回收站或工作区 `.vela-trash/`，引导模型删文件优先用 `delete_file` 而非 `rm`。
- **敏感环境变量清洗**：`bash` 与 MCP `stdio` 子进程启动前剥离父进程中匹配 `KEY|PASSWORD|SECRET|TOKEN` 的敏感变量。
- **后端强制复核与失败关闭（Fail-Closed）**：仅提供一次性授权（`allowed-once`）；`ExecuteTool` 内部必须再次执行策略复核——`deny` 即使前端传入 `approved: true` 也强制拒绝，`ask` 且 `!approved` 直接返回错误；`RemoveScratchDir(sessionID)` 强校验 `sessionID` 仅匹配 `^[A-Za-z0-9_-]+$`。

---

## 6. 核心技术架构

### 6.1 前端 Agent Loop（基于现有 `src/services/api/` 与 `src/services/chat/`）

Vela 对接的 DeepSeek `/anthropic/v1/messages` 协议已预留 `ToolUseBlock`、`input_json_delta` 与 `stop_reason: 'tool_use'`，但需补齐解析、多步循环与展开规则：

- **流式解析补齐（`src/services/api/parseEvent.ts`）**：
  - `readBlock()` 对 `case 'tool_use'` 增加 `id`、`name`、可选初始 `input` 校验；
  - `readDelta()` 补齐 `case 'input_json_delta'` 解析（当前会落入 `default: return null` 被丢弃）；
  - `content_block_stop` 解析累积的 `partialJson` 时：空串按 `{}` 处理；非法 JSON 不中断流，而在工具执行阶段生成 `TOOL_ERROR` 自愈结果回传模型。
- **多步循环与单轮展开规则（`src/services/chat/reply.ts` & `store.ts`）**：
  - **同轮多步（未 `end_turn` 前）**：开启深度思考时，同轮中间步骤的 `assistant(thinking + tool_use)` 原样保留 `thinking` 与 `signature` 发给下一步请求；
  - **跨轮历史上下文**：下一轮用户提问时，将历史 `AssistantMessage.blocks` 中的 `client_tool_use` 与 `client_tool_result` 成对展开回传；同时放宽 `isUsableReply()`，允许“仅有工具调用结果、无最终正文”的已完成轮次进入上下文；
  - **禁止续写冲突**：`canContinueReply()` 显式拦截 `blocks` 中含 `client_tool_use` 或 `client_tool_result` 的回复，防止走 Prefill 续写产生协议冲突。
- **并发、自愈与 KV Cache**：
  - 只读工具（`read`/`glob`/`grep`）支持有界并发执行；写入/修改/命令类工具串行执行（独占屏障）；所有 `tool_result` 严格按模型声明顺序回填。
  - 用户中止或工具异常时，自动为未结算的 `tool_use` 补写错误 `tool_result`（`ABORTED_BEFORE_DISPATCH` / `TOOL_ERROR`），确保消息历史永远成对合法。
  - 工具结果、权限变更与运行时上下文采用仅追加策略，不改写前置消息历史。

### 6.2 Go 后端工具执行层（`WorkspaceService` / `ToolService`）

通过 `mygo.Bind` 暴露给前端，零外部运行时依赖：

- **6 个内置文件工具**：
  - `read(file_path, offset?, limit?)`：带行号 UTF-8 分页读，默认上限 2000 行 / 单行 2000 字符 / 50 KB 字节，大文件流式读。
  - `write(file_path, content)`：原子写入（临时文件 + rename）。
  - `edit(file_path, old_string, new_string, replace_all?)`：字面量精准替换，默认要求唯一匹配，返回结构化 `Diff` 供前端渲染。
  - `delete_file(file_path)`：不跟随软链接，安全移入回收站或 `.vela-trash/`。
  - `glob(pattern, path?)` & `grep(pattern, path?, include?)`：Go 原生 `filepath.WalkDir` + 正则，跳过 `.git`/`node_modules`，按修改时间或文件分组返回有界结果。
- **命令执行工具 `bash`（Windows 对应 `pwsh`/PowerShell）**：
  - 参数：`description`（5~10 词给 UI 展示）、`command`、`workdir?`、`timeoutMs?`。
  - 进程组管理（POSIX `Setpgid` / Windows Job Object），超时或取消时彻底清理子进程树。
  - **非零退出码作正常结果返回**：格式化输出 `stdout`、`[stderr]`、尾部截断提示与 `[exit code: N]`，由模型自行排查。
  - 二期补齐 **超时自动转后台 Job**（`goroutine` + 环形输出缓冲 + `job_output` / `job_kill`）。
- **返回值双轨设计**：工具返回 `{ modelContent, isError, assignedScratch?, uiMeta? }`，`modelContent` 保持极简信封省 Token，`uiMeta` 携带 `diffs`、新增文件列表、退出码供 UI 渲染与持久化回放。

### 6.3 扩展能力（Skills 与 MCP）

- **Skills（Phase 2）**：
  - 扫描 `<workspace>/.agents/skills` 与 `~/.vela/skills` 一层目录（`<name>/SKILL.md` 或 `<name>.md`），解析 YAML frontmatter（`name`、`description`）。
  - 摘要目录按 digest 增量注入 `<system-reminder>`；模型调用 `skill(name)` 按需加载全文；输入框支持 `/name` 显式注入。
- **MCP Client（Phase 3）**：
  - **传输优先级与本地运行时策略（不破零依赖红线）**：
    - **优先支持 `streamable-http`**：面向普通用户零本地环境门槛，支持自定义静态 `headers`（如 `Authorization: Bearer ...`、`X-API-Key`），覆盖主流托管 MCP 服务。
    - **同时支持 `stdio`（不捆绑 Node/Python）**：面向本机已有环境的开发者，直接兼容本机已安装的 `npx`、`uvx` 或 Go/Rust 单二进制 MCP Server。启动与 `probe` 前先补齐用户登录 Shell 的 `PATH`（规避 macOS/Linux GUI 应用 `PATH` 缺失，同时允许填写命令绝对路径），并通过 `LookPath` 做前置预检——**未检测到 `npx` / `uvx` 等可执行文件时，明确区分于服务启动报错并向用户弹出清晰指引（提示安装 Node.js / uv、填写绝对路径或改用远程 `streamable-http`）**；本机已安装时自然直接支持。
  - **针对 `deepseek-harness`（`packages/mcp/*`）的拆解边界**：
    - **直接复刻（协议行为与契约语义对齐）**：
      1. **稳定工具命名与冲突规则**（`mcp-client/src/tools.ts`）：公开工具名固定为 `mcp__<serverName>__<rawName>`，`serverName` 校验 `[A-Za-z0-9_-]{1,32}`；名称规范化有损时追加 12 位十六进制 SHA-256 哈希防碰撞；`tools/call` 始终传递原始 `rawName`；同作用域重名直接拒绝。
      2. **`on-demand` 按需加载机制**（`mcp-client/src/tool-loading.ts`）：按需服务器默认不把全部 schema 注入工具列表，仅在系统提示词追加目录摘要（单条摘要限长如 `160` 字符），由模型调用 `mcp__<serverName>__load_tools` 按需把工具定义加入当前会话以省 Token。作用域对齐 DSH：**纯内存、按会话隔离**（前端 `store.ts` 维护 `Map<sessionId, Set<string>>`，新会话与重启/刷新恢复均从空集合开始，不落盘）；服务器重连同步新工具列表时自动与当前已加载集合求交集（保留仍提供的、移除已下线的）；若上游服务器自身提供名为 `load_tools` 的工具则跳过不列入目录并记录警告。
      3. **`stdio` 敏感环境变量清洗**（`mcp-client/src/transport.ts`）：启动子进程前剥离父进程中匹配 `KEY|PASSWORD|SECRET|TOKEN` 的变量，再叠加上层显式配置的 `env`。
      4. **连接容错与探测契约**（`mcp-client/src/connection.ts`、`probe.ts`）：一次性无副作用 `probe`（连接 → 读身份与工具列表 → 关闭，不注册工具）；指数退避重连（`500ms` 起翻倍至 `30s`、默认上限 `10` 次，持续正常连接满 `30s` 后重置计数以防闪退死循环；**但对 `stdio` 前置 `LookPath` 缺运行时、以及 `streamable-http` 明确的配置/鉴权致命错误如非法 URL、HTTP `401`/`403` 或首次建连握手返回 HTTP `404` 直接停在错误态、不消耗重试预算；运行中因远端重启导致旧 `Mcp-Session-Id` 失效返回的 `404` 则走会话重建/重连**）；工具同步在单一 `MCPService` 内加锁原子替换该服务器内存快照（拉取或校验失败保留旧快照）；默认单次调用超时 `60s`，且 MCP `isError` 如实作为工具错误返回模型。
    - **架构参考（去 Cordis 化，纯 Go 轻量重构）**：
      1. **双层管理职责**（参考 `mcp-client` 单实例监督 + `mcp-manager` 编队对账）：剥离 Cordis Fiber、`McpClientHost`、volatile Config 与 `dsh-api-remotes`，改用单一 Go `MCPService` + 串行操作队列管理实例增删改启停/重启/探测，状态变更经 `mygo.NewEvent` 推送前端。
      2. **配置条目结构与服务器指令注入**（参考 `mcp-manager` 扁平 `servers.<name>` 与 `mcp-client/src/server-context.ts`）：沿用简洁扁平的逐服务器配置语义（`transport`、`url`/`headers`、`command`/`args`/`env`/`cwd`、`enabled`、`toolLoading`、`reconnect`），但落地为 Vela 轻量本地 JSON 配置而非 `cordis.yml`；非空白 `instructions` 按字面文本注入系统提示词并加字节上限（如 `32 KB`）。
    - **Vela 另行实现**：
      1. **基于官方 Go SDK 的协议与传输层**：不引入 `@modelcontextprotocol/sdk`（Node），直接接入官方 [`github.com/modelcontextprotocol/go-sdk/mcp`](https://github.com/modelcontextprotocol/go-sdk)（`v1.8.0+`，已完整支持至 `2026-07-28` 协议并与当前 `go 1.27.4` 兼容），无需手写 JSON-RPC 与传输帧解析；`stdio` 经 `mcp.CommandTransport` 注入清洗后的 `cmd.Env` 与解析后的 `PATH`，`streamable-http` 通过自定义 `http.Client` 注入静态 `headers`，编译进单一二进制且零外部运行时依赖。
      2. **`streamable-http` 优先与 `stdio` 本机运行时预检**：按上述策略补齐 GUI 登录 Shell `PATH` 探测与 `npx`/`uvx` 缺失的友好提示（DSH 面向自带 Node 的部署环境，无此桌面端缺运行时引导需求）。
      3. **三档权限与一次性审批接入**（DSH 未在 `mcp-client` 内置审批，交由外围插件钩子扩展；Vela 统一收口至 `ToolService.ClassifyToolCall`）：
         - `mcp__<serverName>__load_tools`：本地只读元工具，三档权限下均自动放行。
         - 外部 MCP 工具调用：**`仅可查看`** 仅放行工具定义中明确声明 `annotations.readOnlyHint === true` 的工具，其余 `deny`（未声明按非只读处理）；**`工作区内修改（默认）`** 对 `readOnlyHint === true` 自动放行，其余走一次性审批卡片（`ask`），同时在服务器高级选项中提供「信任此服务器（免审批）」开关（默认关闭）；**`完全权限`** 直接放行（`allow`）。注：`readOnlyHint` 为 MCP 服务端自声明提示（非协议强约束），自动放行隐含“仅接入可信 MCP 服务”的前提，添加服务器表单中需附简短风险提示。
      4. **轻量凭据脱敏**：暂不引入 DSH 的 `credentials` 插件与 `${credential:REF}` 模板引擎，统一由本地 `~/.vela/mcp.json`（权限 `0600`）保存静态 Header Token，并在失败诊断与 `isError` 文本中对敏感凭据做 `[redacted]` 脱敏。
    - **明确暂缓（不进 Phase 3 首发）**：`mcp-account` 与 OAuth 动态客户端注册/本地回调 WebServer、系统钥匙串凭据存储、独立 `mcp-resources` 资源工具包、第三方插件运行时注册（`mcpManager.register`）、PTC 图片附件投影与 MCP Prompts 模板。
  - **设置页配置交互与持久化（`SettingsDialog` 新增 `MCP` 分组）**：
    - **入口与布局**：在现有 `SettingsDialog`（`通用设置 / 安全与审批 / Skills / 数据管理`）左侧导航的 `Skills` 与 `数据管理` 之间新增 **`MCP`** 分组，延续 `SkillsSetting` 的顶部操作栏 + 卡片列表 + 启停开关视觉规范；顶部提供 **`+ 添加服务器`** 与 **`导入 JSON`** 两个入口。
    - **双模式配置（表单默认 + JSON 进阶）**：
      1. **可视化表单（默认）**：传输方式默认选中 `Streamable HTTP`（填写 `名称`、`URL` 与可选 `Headers` 键值对，敏感值遮罩显示）；切到 `Stdio` 时填写 `Command`、`Args`、可选 `Env` 与 `Cwd`，输入 `npx`/`uvx` 时实时走 Go 侧预检，未检测到运行时直接在表单内高亮提示（指引安装 Node.js / uv、填绝对路径或改用 HTTP MCP）；折叠「高级选项」提供加载模式（默认 `on-demand`、可切 `eager`）、调用超时、自动重连与「信任此服务器（工作区内修改模式免审批）」开关（默认关闭）。
      2. **JSON 快速导入 / 编辑**：支持粘贴主流工具通用的 `{ "mcpServers": { ... } }` 片段，保存前统一经 Go 侧做 schema 与字段级校验并定位报错字段。
    - **服务器卡片与状态操作**：每张卡片展示 `serverName`、类型徽章（`HTTP` / `Stdio`）、实时状态（已连接与工具数 / 连接中 / 已停用 / 缺运行时或错误详情）及启停开关；内置「测试连接（Probe）」「查看工具列表」「重启连接」「编辑 / 删除」操作。
    - **配置存储与密钥隔离**：配置文件由 Go 侧统一落盘管理（如 `~/.vela/mcp.json`，权限 `0600`），**不存 `localStorage`、不随聊天历史 ZIP 导出**，防止静态 Header Token 随备份外泄。

---

## 7. 分期实施路线

| 阶段                         | 核心交付                                                                                                                                                                                                                                                                                                                                                                                        | 验收标志                                                                                                                                                                                       |
| :--------------------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Phase 1：最小 Agent 闭环** | 1. 开放 `deepseek-flash` / `deepseek-v4-pro` 模型与深度思考开关 UI<br>2. 侧边栏工作区分组 + 输入框工作区与三档权限下拉（对齐 DSH）<br>3. 前端 `tool_use` Agent Loop（含 `parseEvent.ts` 补齐与协议自愈）<br>4. Go 透明懒创建临时目录（含 `bash` 空目录回收与新增文件 diff）<br>5. 6 个文件工具 + 严格分级审批 `bash`<br>6. 工具调用卡片、Diff 预览、一次性审批卡片与基础「打开文件 / 访达定位」 | 挂载项目目录可自主读代码、改文件、跑测试；临时闲聊说「写个 HTML 小游戏」可自动落盘临时目录并一键打开试玩；跑 `go version` 零目录残留                                                           |
| **Phase 2：体验与安全跃迁**  | 1. `bash` 超时自动转后台 Job（`job_output` / `job_kill`）<br>2. 产出文件「另存为」与增强预览卡片<br>3. macOS `sandbox-exec`（及 Linux `bwrap`）轻量 OS 沙箱<br>4. Skills 发现、按需加载工具与 `/` 快捷唤起                                                                                                                                                                                      | 长耗时构建不中断；内核级兜底工作区外写入；支持项目与用户级 `SKILL.md`                                                                                                                          |
| **Phase 3：生态扩展**        | 1. 基于官方 `go-sdk` 的 MCP Client（`streamable-http` 优先含静态 `headers` 鉴权 + `stdio` 兼容本机 `npx`/`uvx`/二进制与缺失明确提示 + `on-demand` 按需加载）及设置页 `MCP` 分组（表单 + JSON 导入 + Probe 探测）<br>2. 输入框 `@` 快速引用工作区文件<br>3. 视需求扩展多模型与自定义 Base URL                                                                                                    | 普通用户在设置页填远程 HTTP MCP URL 零环境门槛即可测试并启用；本机有 `npx`/`uvx` 的开发者可表单或粘贴 `mcpServers` JSON 无缝用 `stdio`，缺失运行时有明确引导提示；全程保持 Vela 单文件极轻体积 |

---

## 8. 明确暂不做（Non-Goals）

- **不做** Cordis 全插件内核、插件市场与 Creator Mode。
- **不捆绑** Node.js、Python 或 `ripgrep` 等重型外部运行时。
- **不做** 多子 Agent 团队协作编排与后台定时自动化任务。
- **不做** “永久允许/自动放行一切命令”白名单。
- **暂缓** 全局呼出热键、系统托盘常驻与独立 Artifacts 分栏画布。

---

## 9. Phase 1 工程实施蓝图（新会话开箱即用规格）

> **面向后续实施会话的直接施工说明**：本节已对照 `vela-app` 现有源码（`v0.2.3`）补齐精确的数据模型、协议扩展、Go ↔ TS 接口契约、统一预检与审批机制、开发红线与 **M1 → M4 独立可验收切片**。新会话每次只需领一个 Milestone 推进即可。

### 9.1 现有代码锚点与工程红线（必读）

1. **保持 IndexedDB 库名与向后兼容**：`src/services/chat/db.ts` 当前库名为 `'chat-demo'`、`DB_VERSION = 11`。新增 `workspaces` 表时递增至 `DB_VERSION = 12`，**切勿改库名**，且 `Session` / `SessionSummary` 新增字段全部设为可选，保证老会话与历史 ZIP 备份零迁移无损读取。
2. **ZIP 历史备份边界**：`src/services/chat/history/toArchive.ts` 当前仅导出 `text`、`thinking`、`server_tool_use`。**Phase 1 保持该行为不变**（备份保留对话正文，暂不序列化 `client_tool_use` / `client_tool_result` 与 `workspaceId`）。
3. **保持单轮分支树结构不变**：`src/services/chat/branches.ts` 与 `getContextRounds()` 假设历史是 `UserMessage → AssistantMessage` 交替。**不要**为工具往返拆出多条消息；把单轮多步的工具调用与结果都作为 `ReplyBlock` 存入同一条 `AssistantMessage.blocks` 中，只在 `toRequestMessages()` 组包发往 DeepSeek API 时展开为标准的 `assistant(tool_use)` + `user(tool_result)` 序列。
4. **MyGo 绑定与前置核对红线**：
   - `src/mygo.ts` 由 `pnpm run generate`（`mygo generate`）自动生成；修改 Go 导出的 Service 或 Event 后同步更新绑定。
   - **禁止**在 Go 回调中同步调用 `win.Page().Eval(...)`（Windows WebView2 会死锁），后端推送统一使用 `mygo.NewEvent[T](...)`。
   - **M1 开工前核对**：先查看当前固定依赖的 `mygo`（`v0.9.1`）源码中 `Dialog` 目录选择 API 参数及 macOS / Windows 表现。
   - **M3 开工前冒烟验证**：实测确认当前扩展模式（含联网搜索与阅读链接工具）与自定义 `ClientToolDefinition` 共存时接口正常响应、不返回 400。
5. **验证标准**：每个 Milestone 完成后必须通过 `tsc -p tsconfig.json`、`oxlint`（改动文件 0 报错）、`rspack` 生产构建，以及 Go 侧 `gofmt -l` / `go vet ./...` / `go test ./...`（本地有 Go 缓存时）。

### 9.2 精确数据模型扩展（TypeScript）

#### A. 工作区与权限类型（建议新建 `src/services/workspace/types.ts`）

```ts
export type PermissionMode = 'read-only' | 'workspace-write' | 'full-access'

export type Workspace = {
  id: string
  /** 规范化绝对路径（唯一键语义） */
  path: string
  /** 默认取路径最后一段，可重命名 */
  name: string
  createdAt: number
  updatedAt: number
}
```

#### B. 扩展 `Session`、`SessionSummary` 与 `ReplyBlock`（`src/services/chat/types.ts`）

```ts
// 1. 扩展客户端工具调用与结果块
export type ToolApprovalStatus = 'pending' | 'approved' | 'rejected'

export type ToolFileDiff = {
  path: string
  oldText: string
  newText: string
}

export type ToolUIMeta = {
  workdir?: string
  diffs?: ToolFileDiff[]
  producedFiles?: string[]
  exitCode?: number | null
  truncated?: boolean
  approvalStatus?: ToolApprovalStatus
  approvalReason?: string
}

export type ClientToolUsePart = {
  type: 'client_tool_use'
  id: string
  name: string
  /** 流式 input_json_delta 累积的原始 JSON 文本 */
  partialJson?: string
  /** content_block_stop 后解析得到的对象（空串按 {}，非法 JSON 标记 parseError） */
  input: Record<string, unknown>
  parseError?: string
}

export type ClientToolResultPart = {
  type: 'client_tool_result'
  tool_use_id: string
  toolName: string
  /** 喂给模型的精简文本信封 */
  content: string
  isError?: boolean
  /** 仅用于前端展示（Diff / 退出码 / 产出文件 / 审批态），不发给模型 */
  uiMeta?: ToolUIMeta
}

// ReplyBlock 追加上述两项：
export type ReplyBlock =
  | ThinkingPart
  | TextPart
  | ServerToolBlock
  | ClientToolUsePart
  | ClientToolResultPart

// 2. Session 新增可选字段（SessionSummary 与 toSessionSummary / sameSummary 同步透传）
export type Session = {
  id: string
  title: string
  createdAt: number
  updatedAt: number
  messages: Message[]
  rootMessageId?: string | null
  /** 绑定的用户工作区 ID；null/undefined 表示普通/临时会话 */
  workspaceId?: string | null
  /** 临时会话透明懒创建的隔离目录绝对路径（不进入侧边栏工作区列表） */
  scratchDir?: string | null
  /** 会话级权限档位，缺省视为 'workspace-write' */
  permissionMode?: PermissionMode
}
```

#### C. 扩展设置项与移除模型硬编码（`src/services/settings.ts` & `store.ts`）

- `settings.ts` 新增 `model: ChatModel`（`localStorage` 键 `'chat-demo:model'`，默认 `'deepseek-flash'`，可选 `'deepseek-v4-pro'`）与 `defaultPermissionMode: PermissionMode`（键 `'chat-demo:permission-mode'`，默认 `'workspace-write'`）；深度思考继续复用现有 `thinking: boolean`。
- `store.ts` 移除顶部硬编码的 `const MODEL: ChatModel = 'deepseek-flash'`，统一从 `useSettings.getState().model` 读取，并核对 `createReply`、`continueReply`、`regenerateReply` 与 `streamReply` 中的模型取值。

#### D. 扩展 DeepSeek API 协议与解析（`src/services/api/types.ts` & `parseEvent.ts`）

```ts
export type ClientToolDefinition = {
  name: string
  description: string
  input_schema: Record<string, unknown>
}

export type ToolResultBlockParam = {
  type: 'tool_result'
  tool_use_id: string
  content: string
  is_error?: boolean
}

// CreateMessageRequest.tools 扩展为：
tools: (DeepseekWebSearchTool | DeepseekReadUrlTool | ClientToolDefinition)[]

// MessageParam 的 user content 允许 ToolResultBlockParam；
// AssistantBlockParam 允许 ToolUseBlock。
```

### 9.3 Go 后端接口契约（`workspace.go` / `tools.go`）

新增两个通过 `mygo.Bind` 暴露的 Go Service；其中 `ToolService` 为**带锁的有状态指针实例**（参照 `main.go` 中 `globalUpdateService` 的注册方式）：

```go
// WorkspaceService：原生目录选择、临时目录管理与打开/定位
type WorkspaceService struct{}

func (WorkspaceService) PickDirectory() (string, error)
func (WorkspaceService) RemoveScratchDir(sessionID string) error // 必须校验合法字符集，禁止路径穿越
func (WorkspaceService) OpenPath(absPath string) error           // 系统默认应用/浏览器打开文件
func (WorkspaceService) RevealInFinder(absPath string) error     // 在访达/资源管理器中定位

// ToolService：有状态实例（按 sessionID + callID 维护活跃工具取消映射，避免并发覆盖）
type ToolService struct {
    mu      sync.Mutex
    nextID  uint64
    cancels map[string]map[uint64]context.CancelFunc
}

var globalToolService = &ToolService{
    cancels: make(map[string]map[uint64]context.CancelFunc),
}

type ToolClassifyResult struct {
    Decision string `json:"decision"` // "allow" | "ask" | "deny"
    Reason   string `json:"reason"`
}

type ToolExecuteRequest struct {
    SessionID      string         `json:"sessionId"`
    ToolName       string         `json:"toolName"`       // read | write | edit | delete_file | glob | grep | bash
    Input          map[string]any `json:"input"`
    Workdir        string         `json:"workdir"`        // 用户工作区路径或已有 scratchDir；为空时按规则懒创建
    PermissionMode string         `json:"permissionMode"` // read-only | workspace-write | full-access
    Approved       bool           `json:"approved"`       // 仅一次性审批通过时为 true；后端仍强制复核
}

type ToolExecuteResult struct {
    ModelContent    string         `json:"modelContent"`
    IsError         bool           `json:"isError"`
    AssignedScratch string         `json:"assignedScratch,omitempty"` // 仅本次新懒创建且确认非空后回传
    UIMeta          map[string]any `json:"uiMeta,omitempty"`          // diffs / producedFiles / exitCode / truncated / workdir
}

// 统一预检（覆盖 bash 严格白名单与文件越界/高危删除）
func (s *ToolService) ClassifyToolCall(req ToolExecuteRequest) ToolClassifyResult
// 执行前内部强制再调 ClassifyToolCall 复核：deny 一律拒绝；ask && !req.Approved 一律拒绝
func (s *ToolService) ExecuteTool(req ToolExecuteRequest) (ToolExecuteResult, error)
// 取消当前会话正在运行的 bash 进程组
func (s *ToolService) CancelSessionTools(sessionID string) error
```

### 9.4 审批通信闭环设计（前端编排 + 后端强制复核，零死锁）

采用**前端编排审批（Frontend-Orchestrated Approval）**避免 Go 协程阻塞等待前端回调，并由 Go 后端兜底强制复核：

1. Agent Loop 收到任意工具调用时，前端先调用 `ToolService.ClassifyToolCall(req)`（纯快检，同时覆盖 `bash` 与文件越界/删除）。
2. 返回 `deny` → 直接生成拒绝 `ClientToolResultPart`，不调 `ExecuteTool`。
3. 返回 `ask` → 在 `store.ts` 中挂起当前循环（`await waitForApproval(sessionId, toolUseId, signal)`），将工具块状态置为 `approvalStatus: 'pending'`，渲染内嵌审批卡片。
4. 用户点击「允许这一次」→ 调用 `ToolService.ExecuteTool({ ..., approved: true })`（Go 内部再次校验策略，`deny` 不可绕过）；点击「拒绝」或停止生成 → 立即 resolve 为拒绝结果并继续/终止循环。

### 9.5 Phase 1 分步施工切片（M1 → M4，每片可独立验收）

| Milestone                                             | 核心目标                                                                                                                                                                                                                                                                                     | 涉及改动/新增文件                                                                                                                                                                                                                 | 独立验收标准                                                                                                                                                   |
| :---------------------------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **M1：工作区数据层 + DSH 对齐 UI + 模型切换**         | 核对 `mygo@v0.9.1` 目录选择 API；跑通工作区 CRUD、侧边栏分组、输入框工作区/三档权限/模型与深度思考开关 UI，移除 `store.ts` 中 `MODEL` 硬编码（暂不接工具执行）                                                                                                                               | `workspace.go`、`main.go`、`src/mygo.ts`<br>`src/services/workspace/`、`src/services/chat/db.ts`（v12）、`types.ts`、`settings.ts`、`store.ts`<br>`src/components/sider/`、`src/components/chatInput/`、`src/i18n/zh.ts`、`en.ts` | 可原生选目录添加工作区、侧边栏按工作区折叠分组且保留原有时间分组、可切三档权限与 `deepseek-flash`/`deepseek-v4-pro` + 深度思考开关并正常对话                   |
| **M2：Go 侧 6 个文件工具 + 透明懒创建临时目录**       | 实现 `read`/`write`/`edit`/`delete_file`/`glob`/`grep`、祖先路径规范化防逃逸校验、`os.Lstat` 回收站删除、`RemoveScratchDir` 强校验、双轨返回（含 `Diff`）与懒创建目录；**先内置基础硬拦截（`read-only` 拒写、`workspace-write` 拒越界）避免 M3 裸跑，M4 再将越界/高危分支升级为 `ask` 审批** | `tools_fs.go`、`tools_path.go`、`tools_trash.go`、`tools_test.go`、`main.go`、`src/mygo.ts`                                                                                                                                       | `go test ./...` 覆盖新建多级目录校验、软链逃逸拦截、权限硬拦截、唯一匹配替换、Diff 生成、安全删除与临时目录懒创建                                              |
| **M3：前端 Agent Loop + 文件工具闭环 + 卡片 UI**      | 先冒烟验证扩展模式与自定义工具共存；补齐 `parseEvent.ts` 的 `tool_use` / `input_json_delta` 解析；升级 `reply.ts`/`store.ts` 多步循环（同轮保留 thinking、跨轮展开、放宽 `isUsableReply`、禁止工具回复续写、协议自愈），接入 M2 文件工具，落地工具卡片、Diff 与「打开文件 / 访达定位」       | `src/services/api/types.ts`、`src/services/api/parseEvent.ts`、`src/services/chat/reply.ts`、`store.ts`、`agentTools.ts`<br>`src/components/messages/ToolCallCard.tsx`、`DiffView.tsx`、`AssistantMessage.tsx`                    | 挂载项目目录可自主读代码、改文件并看红绿 Diff；临时闲聊说「写个 HTML 小游戏」自动落盘 `~/.vela/scratch/<sessionId>/` 并可一键打开试玩或访达定位                |
| **M4：Go `bash` 执行 + 统一预检与分级一次性审批闭环** | 实现 `ClassifyToolCall`（严格子命令+参数白名单与文件越界预检）、`ExecuteTool` 后端强制复核、`bash`/`pwsh` 进程组执行与环境清洗、事后空临时目录安全回收与新增文件快照 diff、`CancelSessionTools` 与前端审批卡片                                                                               | `tools_policy.go`、`tools_shell.go`、`tools_shell_unix.go`、`tools_shell_windows.go`、`tools_shell_test.go`<br>`src/components/messages/ApprovalCard.tsx`、`src/services/chat/store.ts`                                           | 严格白名单只读命令自动放行；未知 flag/写/删/越界操作弹一次性审批卡片；停止生成可杀掉子进程树；临时闲聊跑 `go version` 零目录残留且多次 `bash` 不重复上报旧文件 |

### 9.6 新会话开工 Prompt 示例

开新会话时，直接发下面这句即可无缝接力 M1（做完 M1 再开新会话领 M2，依此类推）：

> 请阅读 `/Users/jiamingfeng/workspace/vela-app/docs/agent-workbench-v0.3-design.md`（重点看第 3 节 UI 对齐与第 9 节工程实施蓝图），帮我在 `/Users/jiamingfeng/workspace/vela-app` 中完成 **Phase 1 - M1（工作区数据层 + DSH 对齐 UI + 模型切换）**，完成后跑完类型检查与构建。
