# 船帆座 · Vela

> 调暗喧嚣，留一盏思考的微光。  
> _Dim the noise. Keep a quiet light for thinking._

**Vela（船帆座）** 是一款基于 [MyGo](https://github.com/egoist/mygo) 构建的轻量级跨平台原生 AI 桌面工作台，支持 **macOS**、**Windows** 和 **Linux**。

告别数百兆 Electron 运行时开销，Vela 直接依托操作系统级原生 Webview（macOS WKWebView、Windows WebView2、Linux WebKitGTK）并由纯 Go（零 CGO）后端驱动，专注提供安静、克制、本地隐私优先的桌面思考体验。当前已深度适配 DeepSeek 全系列能力，并持续演进为支持多模型与多 API 的统一桌面工作台。

- **超轻量体积：** macOS `.dmg` 安装包仅约 **4.2 MB**，运行包约 **13.5 MB**（远小于 Electron 的 150 MB+）。
- **极速冷启动：** 启动时间 < 300ms，日常内存占用约 60 ~ 80 MB，常驻桌面几乎零存在感。
- **完整富文本与推理渲染：** 内置流式 Markdown 增量渲染、KaTeX 数学公式、Mermaid 矢量图表与 Prism 代码高亮。
- **原生桌面体验：** 沉浸式无边框标题栏、macOS 交通灯避让、原生菜单与快捷键、系统级文件对话框与签名自动更新。

---

## 界面预览

### 对话交互与深度思考

| 深色模式（深度思考 + 联网搜索引用） |    浅色模式（排版渲染与来源链接）    |
| :---------------------------------: | :----------------------------------: |
| ![深色模式对话](shot/chat-dark.png) | ![浅色模式对话](shot/chat-light.png) |

### 欢迎主界面

|               深色模式主页               |               浅色模式主页                |
| :--------------------------------------: | :---------------------------------------: |
| ![深色模式欢迎页](shot/welcome-dark.png) | ![浅色模式欢迎页](shot/welcome-light.png) |

### 原生设置与数据管理

| 数据管理（深色模式 · 本地备份与隐私控制） |   数据管理（浅色模式 · 本地备份与隐私控制）   |
| :---------------------------------------: | :-------------------------------------------: |
|  ![数据管理深色](shot/settings-data.png)  | ![数据管理浅色](shot/settings-data-light.png) |

---

## 快速安装与运行

前往项目的 [Releases](https://github.com/hexbee/vela-app/releases) 页面下载适合您操作系统的安装包：

- **macOS：** 下载 `.dmg` 文件，双击挂载并拖拽到 `Applications` 目录即可（支持 Apple Silicon 与 Intel 双架构）。
- **Windows：** 下载 `Vela.exe` 直接运行或使用安装包。
- **Linux：** 提供 `.deb`、`.rpm`、`.tar.gz` 安装方式。

> **提示：** 首次使用时，请点击左下角 **设置** 填写您的 API Key 即可开始使用。

---

## 核心能力

- **高效对话与思考流：**
  - **划词引用与悬浮预览：** 选中回答中的任意文本片段即可一键引用追问，发送后支持悬浮预览完整引用原文。
  - **全局会话搜索：** 支持按关键词全文检索全部历史对话与消息内容，点击直接定位跳转（`Cmd/Ctrl + K`）。
  - **终端式历史回溯：** 在输入框中按 `↑` / `↓` 方向键即可快速召回与编辑上一条发送过的提问。
  - **多会话与多版本回答：** 支持自动生成标题、重命名、二次编辑、重新生成、多版本回答切换，以及生成中断与续写。
  - **深度思考与联网阅读：** 完整呈现深度思考（Reasoning 折叠块）、智能联网搜索、网页阅读过程与来源引用卡片。
- **富文本与专业排版：**
  - 基于 `micromark` / `mdast` 的流式逐字增量平滑渲染。
  - KaTeX 数学公式排版（行内 `$..$` 与块级 `$$..$$`）。
  - Mermaid 流程图、时序图、状态图矢量渲染与源码切换。
  - Prism.js 代码语法高亮、行号展示以及一键复制代码块。
- **多模态与语音交互：**
  - 支持图片、PDF、Office 文档、代码文件上传，以及剪贴板粘贴与全局拖拽上传。
  - 系统级麦克风录音与实时音频频谱动效，支持流式语音朗读（TTS）。
- **原生桌面能力：**
  - **原生菜单与窗口体验：** 系统级中英文自适应菜单栏、无边框沉浸式标题栏、macOS 交通灯避让、窗口尺寸位置记忆，以及侧边栏收起态悬浮工具栏。
  - **系统级文件对话框：** 历史归档导入与导出直接调用操作系统原生保存/打开对话框。
  - **安全自动更新：** 启动后台静默检查更新、内置公钥签名校验与更新日志弹窗，并具备**对话感知保护**——活跃输出期间不强制打断重启。
  - **双语与深浅色主题：** 支持简体中文 / English，浅色 / 深色 / 跟随系统自动切换。

---

## 效率与快捷键

| 快捷键 / 手势         | 功能说明                                       |
| :-------------------- | :--------------------------------------------- |
| `Cmd/Ctrl + N`        | 开启新对话                                     |
| `Cmd/Ctrl + K`        | 打开 / 关闭全局会话搜索                        |
| `Cmd/Ctrl + B`        | 展开 / 收起左侧边栏                            |
| `Cmd/Ctrl + ,`        | 打开 / 关闭偏好设置                            |
| `↑` / `↓`（输入框内） | 快速回溯并编辑上一条 / 下一条历史提问          |
| **划选消息文本**      | 唤起引用浮窗，一键带入输入框并支持悬浮预览原文 |

---

## 数据与隐私说明

- **本地私有存储：** API Key 保存在本地 `localStorage`，会话记录及多模态图片原件存放在本地 `IndexedDB`，除您配置的模型 API 接口外绝不上报给任何第三方服务器。
- **历史导入与导出：** 支持标准的 ZIP 格式数据备份（兼容 Vela 与 DeepSeek 导出的历史对话 ZIP）。导入会在校验格式和版本后原子替换本地记录；导出将打包所有会话及本地图片原件，不包含 API Key。
- **清空对话：** 设置中提供“删除所有对话”，确认后清空所有本地会话、附件原件和草稿，保留 API Key 与配置项。

---

## 本地开发与构建

### 1. 环境准备

- **Go：** 1.27 或更高版本 (`go version`)
- **Node.js / 包管理器：** Node.js 22+ 及 `pnpm` (或 `bun 1.4+`)
- **MyGo CLI：** 全局安装或通过项目依赖运行
  ```bash
  pnpm add -g mygo-cli
  ```
  执行 `mygo doctor` 验证环境完备性。

### 2. 开发命令

```bash
# 克隆仓库并安装依赖
git clone <repo-url>
cd vela-app
pnpm install

# 启动桌面端开发调试（自动启动 Rspack 服务并拉起桌面窗口，支持 HMR 与 Go 增量构建）
pnpm dev

# 仅调试前端 Web 页面（端口 8090）
pnpm dev:web

# 重新生成 Go 与 TypeScript 强类型 IPC 客户端
pnpm generate
```

### 3. 项目编译与多平台打包

由于 MyGo 采用纯 Go 编写且**零 CGO 依赖**，您可以在任何操作系统上一键交叉编译全平台二进制：

```bash
# 1. 编译并打包当前宿主机安装包（macOS 下生成 .app 与 .dmg）
pnpm build

# 2. 跨平台一次性交叉编译全平台产物（macOS + Windows + Linux）
mygo build -platform darwin/arm64,darwin/amd64,windows/amd64,linux/amd64
```

产物将输出在 `build/` 目录下：

- `build/darwin-arm64/Vela.app` 及 `.dmg`
- `build/windows-amd64/Vela.exe`
- `build/linux-amd64/`（包含 `.deb`、`.rpm`、`.tar.gz` 等）

---

## 项目结构

```text
vela-app/
├── .agents/skills/           # MyGo 智能维护与原生模块规范 (mygo-maintenance 等)
├── .github/workflows/        # CI/CD 自动发布打包流水线 (release.yml)
├── main.go                   # Go 桌面端主入口（窗口生命周期、系统服务与 IPC 注册）
├── mygo.config.ts            # MyGo 核心配置文件（跨平台权限、窗口、前端构建参数）
├── package.json              # 前端依赖与脚本
├── rspack.config.ts          # 高性能 Rspack 构建配置（支持 lightningcss、swc）
├── tsconfig.json             # TypeScript 编译配置
├── shot/                     # 桌面端界面实际运行截图
├── src/
│   ├── app/                  # 应用顶层布局、视口与深浅色主题
│   ├── components/           # 界面组件（消息列表、输入框、设置框、侧边栏等）
│   ├── i18n/                 # 国际化文案（中文 / 英文）
│   ├── mygo.ts               # mygo generate 自动生成的类型安全 IPC 客户端
│   ├── services/
│   │   ├── api/              # DeepSeek API、Anthropic 兼容协议、SSE 流解析
│   │   ├── chat/             # 会话状态机、分支切换、IndexedDB 存储与 ZIP 归档
│   │   ├── desktop/          # Go 桌面端原生服务适配（文件对话框等）
│   │   ├── markdown/         # KaTeX, Mermaid, Prism 高亮与 AST 渲染
│   │   └── speech/           # 麦克风录音与流式 TTS 朗读
│   ├── utils/                # 剪贴板、ID、日期格式化等基础工具
│   └── main.tsx              # 前端入口
└── build/                    # 桌面端安装包产物目录
```

---

## 质量与代码规范

| 命令          | 说明                                 |
| :------------ | :----------------------------------- |
| `pnpm type`   | TypeScript 全量严格类型检查          |
| `pnpm lint`   | 使用 Oxlint 执行代码检查             |
| `pnpm format` | 使用 Oxfmt 统一代码格式              |
| `mygo doctor` | 检查本地 Go、Webview、打包工具链状态 |

---

## 致谢

- 感谢 [deepseek-ai/chat-demo](https://github.com/deepseek-ai/chat-demo) 提供的优秀对话交互与渲染基础，Vela 的早期版本正是在其基础上演化而来。
- 感谢 [egoist/mygo](https://github.com/egoist/mygo) 提供零 CGO、超轻量的原生跨平台桌面框架。

---

## 许可证

本项目遵循 [MIT License](LICENSE)。构建产物中包含的第三方开源组件均遵循各自的许可协议。
