# Vela 发版标准操作流程（Release SOP）

> 适用仓库：`vela-app`（Go + MyGo + Rspack + GitHub Releases 自动更新）  
> 版本真源：`package.json` 中的 `version` 字段（`mygo.config.ts` 与 `main.go` 均自动读取该值）

---

## 1. 发版前检查（Preflight）

在准备发版的功能分支或 `main` 分支上，按顺序执行以下检查：

### Step 1：同步前端 RPC 绑定

```bash
pnpm run generate
```

- 每次新增、修改或删除 `mygo.Bind(...)` 暴露的 Go 导出方法后，**必须**执行 `pnpm run generate`（`mygo generate`），将变更同步到 `src/mygo.ts`。
- 仅供 Go 后端内部调用的方法请使用小写非导出命名，避免向 `src/mygo.ts` 暴露无用 RPC。

### Step 2：前端类型、规范与产物校验

```bash
pnpm run type
pnpm run lint
pnpm run format
pnpm run build:web
```

- 确认 TypeScript 0 错误、`oxlint` 0 警告 0 错误、`oxfmt` 格式一致，且 `dist/` 可正常产出。

### Step 3：后端本机测试与跨平台校验

```bash
go vet ./...
go test ./...
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=windows GOARCH=arm64 go vet ./...
GOOS=linux GOARCH=amd64 go vet ./...
```

- 若涉及 Windows 沙盒或进程树（`*_windows.go`）改动，额外校验 Windows 测试二进制可正常编译：
  ```bash
  GOOS=windows GOARCH=amd64 go test -c -o /tmp/vela_win_test.exe . && rm -f /tmp/vela_win_test.exe
  ```

### 一键 Preflight 命令

```bash
pnpm run generate && pnpm run type && pnpm run lint && pnpm run build:web \
  && go vet ./... && go test ./... \
  && GOOS=windows GOARCH=amd64 go vet ./... \
  && GOOS=linux GOARCH=amd64 go vet ./...
```

---

## 2. 提交功能改动

1. 运行 `git status` 与 `git diff --stat`，确认：
   - 所有功能改动及重新生成的 `src/mygo.ts` 均已纳入提交。
   - **禁止**提交签名私钥（如 `mygo-update.key`）、临时构建目录或调试日志。
2. 按 Conventional Commits 规范提交功能改动（如 `feat: ...` / `fix: ...`）。

---

## 3. 升版本号与打标签

遵循仓库既有惯例（如 `v0.3.2` / `v0.3.3` / `v0.3.4`），版本号变更单独成一个 commit：

1. 将 `package.json` 中的 `"version"` 更新为目标版本 `X.Y.Z`（例如 `0.3.5`）。
2. 提交版本升级并打同名标签：
   ```bash
   git add package.json
   git commit -m "chore: bump version to vX.Y.Z"
   git tag vX.Y.Z
   ```
3. 核对最近提交与标签指向无误：
   ```bash
   git log -n 2 --format="%h %an <%ae> %s"
   git show-ref --tags vX.Y.Z
   ```

---

## 4. 推送与发布

```bash
git push origin main && git push origin vX.Y.Z
```

等待构建与 GitHub Release（`hexbee/vela-app`）发布完成后，核对：

- macOS / Windows / Linux 各目标架构安装包资产齐全。
- MyGo 自动更新元数据与签名清单已正确生成（公钥配置见 `mygo.config.ts` 的 `updates.publicKey`）。
- 增量更新资产（`*.delta`）数量符合预期（`目标平台数 × min(3, 已发布历史版本数)`，当前配置为 6 × 3 = 18），且 `publish-release` 中的清单引用完整性校验已通过：
  ```bash
  gh release view vX.Y.Z --json assets \
    --jq '[.assets[].name | select(endswith(".delta"))] | length'
  ```

---

## 5. 发布后冒烟清单

- **常规启动与更新检查**：启动新版本，确认版本号显示为 `X.Y.Z`，点击「检查更新」无报错。
- **沙箱与审批策略**：在 `workspace-write` 模式下执行常规工作区命令免审放行、高危/外联黑名单命令触发审批弹窗；跨目录写入被沙箱拦截并提示按需升权。
- **后台任务与进程树清理**：启动长跑服务（如 `python -m http.server`）转后台后，执行 `job_kill` 确认外层 Shell 与 Python 子进程均已退出，且直接退出 Vela 应用后无残留进程。
- **Windows 真机专项（涉及 Windows 改动时）**：在 NTFS 工作区执行：
  ```powershell
  go test -v -run "TestWindows" .
  ```
