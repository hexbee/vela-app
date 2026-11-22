# Windows ACL 受限令牌沙盒设计与进展

> 文档状态：代码实现已完成（Phase 1、Phase 3、Phase 4 已完成，Phase 2 仅剩 Windows 10/11 真机端到端验证）  
> 最后更新：2026-11-21  
> 参考实现：`deepseek-harness` (`@deepseek-ai/dsh-sandbox-windows-acl`)

---

## 1. 背景与目标

### 1.1 现状问题

此前 `vela-app` 仅在 macOS（`sandbox-exec` / Seatbelt）和 Linux（`bwrap` / bubblewrap）实现了 OS 级沙盒；Windows 编译命中 `tools_sandbox_other.go`，`isOSSandboxAvailable()` 恒返回 `false`。

这带来两个直接问题：

1. **缺少内核级写隔离**：Windows 下 `workspace-write` 模式的命令直接以宿主进程完整权限运行。
2. **审批弹窗极频繁**：`tools_policy.go` 中 `workspace-write` 模式只有在 `osSandboxAvailableFn() == true` 时才启用「默认放行 + 仅黑名单弹窗」快路径；Windows 下被迫退化为极窄的 Unix 只读白名单，几乎所有命令（含常见构建/测试命令及复合管道命令）都触发人工审批。

### 1.2 设计目标

- 在 Windows 端实现零外部依赖（纯 Go 标准库 `syscall` + `unsafe`）的 OS 级写入隔离沙盒。
- 实现 **Fail-closed**：准备或令牌创建失败时直接报错，绝不静默降级为无沙盒裸跑。
- 让 Windows 端 `isOSSandboxAvailable()` 返回 `true`，自动对齐 macOS / Linux 的「默认放行 + 仅 `approvalBlacklist` 弹窗」审批体验。
- 明确记录已知安全边界，并按阶段推进黑名单补齐、真机验证、按需升权与进程树托管。

---

## 2. 核心架构与技术方案

### 2.1 为什么选择 Win32 ACL 受限令牌（而非 AppContainer / 容器）

- **轻量、零安装依赖**：直接调用 `advapi32.dll` / `kernel32.dll`，无需 Docker、WSL 或管理员提权。
- **兼容开发工具链**：`pwsh` / `powershell.exe`、`go`、`node`、`pnpm`、`git` 等工具在 Low 完整性 + `WRITE_RESTRICTED` 下可正常启动并读取系统工具链，同时写/删权限被内核强制限制在工作区与私有临时目录内。

### 2.2 双层写边界机制

1. **受限令牌（`CreateRestrictedToken`）**
   - 标志位：`DISABLE_MAX_PRIVILEGE | WRITE_RESTRICTED`。
   - Windows 内核对写操作执行两次访问检查（正常 SID + Restricting SIDs 交集），仅两次都通过才允许写入。
   - **Restricting SIDs 列表**：
     - `read-only`：`[Logon SID, Everyone]`（不携带任何写入 SID，即使工作区保留了历史常驻 ACE，在 `read-only` 下也完全失效）。
     - `workspace-write`：`[Logon SID, Everyone, workspaceWriteSid..., tempWriteSid]`。
   - 刻意不包含 `Authenticated Users`、`INTERACTIVE`、`LOCAL`，封堵 `C:\` 根目录与 `Users\Public` 逃逸。
2. **强制完整性控制（MIC / Low Integrity Level）**
   - 通过 `SetTokenInformation(TokenIntegrityLevel)` 将受限令牌降级为 Low 完整性级别（`S-1-16-4096`）。
   - 未标记 Low `NO_WRITE_UP` 标签的目录即使对 `Everyone` 可写，也会被内核拦截。
3. **防止父目录 `FILE_DELETE_CHILD` 越权删除**
   - 每次对目录授权时，在同一次 `SetNamedSecurityInfoW` 调用中同时写入：
     - `DENY_ACCESS`：`Everyone` 的 `FILE_DELETE_CHILD`（仅 `CONTAINER_INHERIT_ACE`，不继承到文件，避免破坏 `GENERIC_ALL` 打开）。
     - `GRANT_ACCESS`：能力 SID 的 `winGrantMask`（`FILE_GENERIC_WRITE | DELETE | FILE_DELETE_CHILD`，排除 `WRITE_DAC` / `WRITE_OWNER`，继承到子容器和对象）。
     - `SACL`：Low 强制标签（`SYSTEM_MANDATORY_LABEL_NO_WRITE_UP`，继承到子容器和对象）。
4. **扩展默认 DACL（保障管道与孙进程可用）**
   - 受限令牌默认继承用户的 Default DACL（不含 restricting SID），会导致子进程创建匿名管道时在写检查 pass-2 失败（`ERROR_ACCESS_DENIED` / `EPERM`）。
   - 通过 `SetTokenInformation(TokenDefaultDacl)` 将 `tempWriteSid`（或 `workspaceWriteSid` / `Everyone`）的 `FILE_ALL_ACCESS` 合并进令牌默认 DACL，确保 PowerShell 管道与孙进程 stdio 正常。

### 2.3 工作区常驻授权 vs 私有临时目录隔离

| 目录类型         | SID 派生规则                                                     | 生命周期                                         | 并发与性能优化                                                                                           |
| ---------------- | ---------------------------------------------------------------- | ------------------------------------------------ | -------------------------------------------------------------------------------------------------------- |
| **工作区根目录** | `S-1-4-<x>-<y>`（SHA-256 规范化小写路径）                        | **常驻**（不撤销，作为跨会话复用缓存）           | 每路径排他 `LockFileEx` 锁；检测精确 Allow ACE + Deny ACE + Low 标签已存在时 O(1) 跳过，避免重复全树传播 |
| **私有临时目录** | `S-1-4-<x>-<y>-1`（SHA-256 `"temp\x00"+path`，三段子权限域隔离） | **可回收**（命令/任务退出后撤销 ACL 并删除目录） | 绝不隐式授权全局 `%TEMP%`；强校验与工作区不相交；重写子进程 `TMP`/`TEMP` 指向私有目录                    |

### 2.4 进程启动与生命周期管理

- **跨平台统一接口**：引入 `prepareSandboxedShellCommand(command, spec, baseEnv) (sandboxedShellCommand, error)`，返回 `{ cmd, sandboxed, cleanup }`。
- **避开 `CREATE_NO_WINDOW` 崩溃**：Windows 上受限 Low-IL 进程若带 `CREATE_NO_WINDOW` 或 `CREATE_NEW_CONSOLE` 标志，会在 DLL 初始化阶段以 `STATUS_DLL_INIT_FAILED (0xC0000142)` 崩溃。因此 `configureProcessGroup` 仅在 `SysProcAttr.Token == 0` 时附加 `CREATE_NO_WINDOW`，受限进程仅保留 `HideWindow: true`。
- **完整清理链路**：
  - `cmd.Start()` 失败：立即调用 `cleanup()`。
  - 前台执行完成或用户取消：等待 `cmd.Wait()` 返回后调用 `cleanup()`。
  - 超时转为后台任务（`backgroundJob`）：将 `cleanup` 托管到 `backgroundJob.sandboxCleanup`，在 `job.finish()` 中等进程真正退出后再执行，绝不在子进程存活期间提前撤销 ACL。

### 2.5 已知安全边界

- **仅限制写与删**：`WRITE_RESTRICTED` 与 Low 标签不限制文件读取、网络访问与进程枚举；外部网络与高危删除命令需由 `approvalBlacklist` 审批兜底。
- **文件系统要求**：工作区需位于当前用户拥有完全控制（含 `WRITE_OWNER`）的 NTFS 分区；FAT32/exFAT 或无权目录会 fail-closed 报错。
- **硬链接别名**：NTFS 硬链接共享同一底层文件对象的安全描述符，工作区内的硬链接外部别名可能同步获得写权限。
- **AppContainer 冲突**：若某目录树被其他基于 AppContainer 的工具写入过包 SID（`S-1-15-2-...`）ACE，Low-IL 令牌可能无法读取该目录树。
- **常驻工作区标签**：工作区上的 Low 标签在会话结束后保留，同一用户下其他 Low-IL 进程也可写入该工作区。

---

## 3. 代码结构与文件清单

| 文件                                | 状态 | 职责说明                                                                                                                                                                   |
| ----------------------------------- | ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tools_sandbox_windows.go`          | 新增 | Windows ACL 受限令牌、Low 完整性、Default DACL、`LockFileEx`、幂等授权/撤销、可用性探测与 `prepareSandboxedShellCommand`                                                   |
| `tools_sandbox_windows_sid.go`      | 新增 | 跨平台工作区/临时目录 SID 派生、锁文件名哈希、大小写不敏感环境变量覆盖、目录重叠检测                                                                                       |
| `tools_sandbox_windows_sid_test.go` | 新增 | 跨平台单元测试（SID 确定性与域隔离、环境变量覆盖、目录重叠、Windows 拒绝签名）                                                                                             |
| `tools_sandbox_windows_win_test.go` | 新增 | Windows 专用测试（受限令牌不带 `CREATE_NO_WINDOW`、工作区内写成功 + 工作区外写拦截）                                                                                       |
| `tools_sandbox.go`                  | 修改 | `sandboxSpec` 新增纯工作区列表 `workspaceRoots`；新增 `sandboxedShellCommand` 与 `collectSandboxWorkspaceRoots`；`looksLikeSandboxDenial` 补充 Windows/PowerShell 拒绝签名 |
| `tools_sandbox_darwin.go`           | 修改 | 适配 `prepareSandboxedShellCommand`                                                                                                                                        |
| `tools_sandbox_linux.go`            | 修改 | 适配 `prepareSandboxedShellCommand`                                                                                                                                        |
| `tools_sandbox_other.go`            | 修改 | 构建标签改为 `!darwin && !linux && !windows`，适配 `prepareSandboxedShellCommand`                                                                                          |
| `tools_shell_windows.go`            | 修改 | `configureProcessGroup` 在 `Token != 0` 时跳过 `CREATE_NO_WINDOW`                                                                                                          |
| `tools_shell.go`                    | 修改 | 接入 `prepareSandboxedShellCommand`（fail-closed）与前台/取消/启动失败清理                                                                                                 |
| `tools_jobs.go`                     | 修改 | `backgroundJob` 持有 `sandboxCleanup`，在 `finish()` 中执行退出后回收                                                                                                      |

---

## 4. 实施进展与待办清单

### Phase 1：Windows ACL 沙盒核心实现（已完成）

- [x] 实现 Win32 `CreateRestrictedToken(WRITE_RESTRICTED)` + Low 完整性级别 + Default DACL 扩展
- [x] 实现每路径 `LockFileEx` 锁、常驻工作区 ACL 幂等跳过、私有临时目录可回收授权与 `TMP`/`TEMP` 重写
- [x] 统一跨平台 `prepareSandboxedShellCommand` 与前/后台任务清理生命周期
- [x] 修复受限令牌与 `CREATE_NO_WINDOW` 冲突问题
- [x] 补充 Windows 拒绝 stderr 识别签名
- [x] 通过 `GOOS=windows GOARCH=amd64/arm64`、`GOOS=linux`、`GOOS=freebsd` 交叉编译与 `go vet` 检查
- [x] 通过 macOS 本机跨平台单元测试

### Phase 2：上线前必补（进行中 - 高优先级）

- [x] **补全 Windows / PowerShell 危险命令黑名单**（`tools_policy.go` + `src/services/settings.ts`）：
  - 已补充 `remove-item` / `ri` / `del` / `erase` / `rd`、`invoke-webrequest` / `iwr`、`invoke-restmethod` / `irm`、`invoke-expression` / `iex`、`start-process` / `saps`、`stop-process` / `spps`、`taskkill`、`icacls`、`takeown`、`reg`、`sc`、`net`、`netsh`、`diskpart`、`bcdedit`、`bitsadmin`、`certutil`，并新增跨平台命令名归一化（兼容 Windows 路径分隔符与 `.exe`/`.cmd`/`.bat`/`.com` 后缀）及单元测试。
- [ ] **Windows 10/11 真机端到端验证**：
  - 验证 `TestWindowsACLSandboxEnforcement` 在真实 NTFS 分区通过。
  - 验证 GUI 启动下执行受限命令是否闪出控制台窗口。
  - 验证常见开发链（`go test`、`pnpm build`、PowerShell 管道）在沙盒内无 `EPERM` / `0xC0000142`。

### Phase 3：体验与健壮性增强（已完成 - 中优先级）

- [x] **沙盒拦截后按需一次性升权闭环**（`tools_policy.go` + `tools_shell.go` + `tools_jobs.go` + `src/services/chat/agentTools.ts`）：在 `bash` 工具 schema 暴露 `sandbox_permissions` + `justification`，后端实现严格更宽阶梯校验（`read-only` → `workspace-write`/`full-access`、`workspace-write` → `full-access`）、强制一次性人工审批（不进入会话记忆）与 fail-closed 二次校验，并在沙盒拒绝 stderr 后追加升权重试提示。
- [x] **Windows Job Object 进程树托管**（`tools_shell_windows.go` + `tools_shell.go`）：已引入 `CreateJobObjectW` + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` + `AssignProcessToJobObject` + `TerminateJobObject`，取消/结束/宿主异常退出时即时原子清理整棵进程树（不可分配 Job 时回退 `taskkill`）。
- [x] **会话级复用私有临时目录**（`tools_sandbox_windows.go` + `tools_sandbox_windows_sid.go` + `tools.go` + `main.go`）：已按 `(sessionID, workspaceRoots)` 池化复用私有临时目录并维护引用计数，避免短命令重复触发 `SetNamedSecurityInfoW`；会话释放（`ReleaseSessionSandbox`）或应用退出（`releaseAllSandboxTemps`）时等活跃任务结束后自动撤销 ACL 并删除目录。
- [x] **补全 PowerShell 只读命令白名单**（`tools_policy.go`）：已在 `isStrictReadOnlyCommand` 中补充 `get-location`/`gl`、`get-childitem`/`gci`/`dir`、`get-content`/`gc`/`type`（排除 `-wait`）、`select-string`/`sls`、`test-path`、`resolve-path`/`rvpa`、`get-item`/`gi`/`get-itemproperty`/`gp`、`where`/`where.exe`（拒绝脚本块）、`get-command`/`gcm`、`get-date`、`write-output` 及其大小写不敏感参数白名单与单元测试。

### Phase 4：运维与边界提示（已完成 - 低优先级）

- [x] **友好错误提示**（`tools_sandbox_windows.go` + `tools_sandbox_windows_sid.go`）：新增 `GetVolumeInformationW` 卷标志预检（拒绝 FAT32/exFAT 并指明文件系统名）与 `formatWindowsACLSandboxError`，将 Win32 权限不足（5/1314）、不支持 ACL/标签（1/50/53/67/87）与锁冲突（32/33）转换为可操作的友好提示。
- [x] **ACL 清理入口**（`tools_sandbox_windows.go` + `tools.go`）：暴露 `ToolService.CleanupWorkspaceSandboxACL(workspacePath)`，可按工作区路径幂等撤销常驻能力 ACE 并在无其他授权残留时清除 Low 标签。

---

## 5. Windows 真机验证清单

在 Windows 机器的 NTFS 工作区目录下执行：

```powershell
# 1. 运行 Windows 沙盒与进程属性测试
go test -v -run "TestWindows(ConfigureProcessGroup|ACLSandbox|ProcessHidesConsoleWindow|SandboxSidDerivation|DirOverlapAndDenialSignatures)" .

# 2. 运行全量工具链测试
go test -v ./...
```

手工核对项：

1. `workspace-write` 模式下，在工作区内执行 `go test` / `echo hi > a.txt` **无需弹窗**且成功执行，ToolCallCard 显示「沙箱隔离」徽章。
2. 尝试向工作区外（如桌面或用户目录）写入文件时，被内核拦截并输出 `[sandbox: kernel blocked ...]`。
3. GUI 启动 `Vela` 时执行多条命令，确认无黑色控制台窗口闪烁。
4. 超时转后台的任务结束后，确认 `%TEMP%\vela-sbx-*` 私有临时目录已被自动删除。
