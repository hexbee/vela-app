# Bug Fix 0002：增量更新资产（`*.delta`）从未上传到 Release

- **状态**：已修复（待下一个 tag 发布验收）
- **日期**：2026-11-22
- **发现场景**：v0.3.5 发布后核对 Release 资产
- **影响版本**：至少 v0.3.3 / v0.3.4 / v0.3.5（三版均已实测确认，`.delta` 资产数为 0）
- **涉及文件**：
  - `.github/workflows/release.yml`
  - `updater.go`
  - `updater_test.go`
  - `docs/release-sop.md`

## 1. 问题描述

从 v0.3.2 / v0.3.3 / v0.3.4 升级到 v0.3.5 时，客户端本应只下载约 380–515 KB 的增量包，实际每次都下载约 7.7 MB 的整包，且在此之前还会发出一次必然失败的请求。

`update-*.json` 的 `deltas` 数组声明了增量更新条目，但条目指向的 `.delta` 文件并不存在于 Release 资产中。

复现与确认：

```bash
# 清单声明的 delta 条目数
curl -sSL https://github.com/hexbee/vela-app/releases/latest/download/update-darwin-arm64.json \
  | grep -c '"from"'
# → 3

# Release 上实际存在的 delta 资产数
gh release view v0.3.5 --json assets \
  --jq '[.assets[].name | select(endswith(".delta"))] | length'
# → 0
```

| 版本   | 清单中 `deltas` 条目数 | Release 上 `.delta` 资产数 |
| ------ | ---------------------- | -------------------------- |
| v0.3.3 | 3                      | 0                          |
| v0.3.4 | 3                      | 0                          |
| v0.3.5 | 3                      | 0                          |

## 2. 根因分析

1. **构建阶段确实生成了 delta 文件。** `mygo build`（`cmd/mygo/build.go:421`、`:489`）调用 `writeArchive`（`cmd/mygo/updates.go:199`），后者在 `Updates.Deltas > 0` 时调用 `writeDeltas`（`updates.go:286`）与 `writeDelta`（`updates.go:345`）。`Updates.Deltas` 未配置时默认为 3，因此每个目标平台会为最近 3 个历史版本各生成一个 delta，产物写入 `build/` 暂存目录。`writeSigned` 对 delta 文件取 SHA-256 摘要并用 ed25519 私钥签名，`size` 与 `signature` 一并写入清单——这两个字段只有在文件确实写成功后才会被填充，可作为「文件已生成」的判据。
2. **上传阶段按扩展名白名单挑文件，遗漏 `*.delta`。** `.github/workflows/release.yml:56` 使用 `find` 过滤后逐个 `gh release upload`，白名单为 `*.dmg`、`*.exe`、`*.deb`、`*.rpm`、`*.tar.gz`、`*.json`，不含 `*.delta`。delta 文件因此留在 runner 的工作目录，随 runner 销毁，从未进入 Release。
3. **客户端的静默回退掩盖了缺陷。** `mygo@v0.9.1` 的 `updater.go:299` 在 `applyDelta` 失败后仅打印一行日志 `mygo: downloading the whole update, as its delta update failed`，随即继续整包下载。界面无任何异常提示，缺陷因此在多个版本中未被发现。

## 3. 影响评估

### 3.1 功能

不破坏。更新仍能完成，只是退化为全量下载。

### 3.2 代价

以 v0.3.5 的 `darwin-arm64` 为例：

| 路径                  | 文件                                     | 体积        |
| --------------------- | ---------------------------------------- | ----------- |
| 增量（0.3.4 → 0.3.5） | `vela-0.3.4-to-0.3.5-darwin-arm64.delta` | 388,034 B   |
| 增量（0.3.3 → 0.3.5） | `vela-0.3.3-to-0.3.5-darwin-arm64.delta` | 423,492 B   |
| 增量（0.3.2 → 0.3.5） | `vela-0.3.2-to-0.3.5-darwin-arm64.delta` | 515,181 B   |
| 实际发生              | `vela-0.3.5-darwin-arm64.tar.gz`         | 8,126,481 B |

即每次更新多传约 7.6–7.7 MB（整包体积的 95% 被重复传输），外加一次 404 往返。跨版本升级的用户数量为 n 时，浪费流量约为 `n × 7.7 MB`；在移动网络或弱网环境下，更新时间与失败概率相应上升。

### 3.3 规模

6 个构建目标 × 3 个历史版本 = 每次发布生成 18 个 delta 文件，全部被丢弃。

## 4. 修复方案

### 4.1 核心改动

1. **补齐增量包上传并保证清单后置（`.github/workflows/release.yml`）**：
   - 在 `build` job 的上传白名单中补入 `-o -name "*.delta"`，使用 `set -euo pipefail` 与 `-print0` 安全枚举文件，并确保所有安装包/整包/增量包先于 `*.json` 清单上传（即使手动对已发布 Release 重跑 `--clobber`，也不会出现清单先于资产生效的窗口）。
2. **发布前增加自动化引用完整性门禁（`.github/workflows/release.yml`）**：
   - 在 `publish-release` 执行 `--draft=false` 之前，自动下载草稿 Release 中的全部 `update-*.json`，校验每条 `.url` 与 `.deltas[].url` 对应的资产文件名均存在于 Release 资产列表中，且字节数与清单声明的 `size` 一致；任一缺失或大小不符即直接失败并中止发布，防止漏传、上传截断或新清单配旧包静默上线。
3. **加固降级更新器的资产筛选（`updater.go`）**：
   - 开发/降级路径的 `checkGitHubRelease` 改为调用 `selectGitHubReleaseAsset`，改用按平台划分的扩展名白名单（区分平台专属后缀如 `.dmg`/`.exe`/`.msi`/`.deb`/`.rpm`/`.appimage` 与通用归档后缀 `.tar.gz`/`.zip`），优先匹配明确包含 OS 与架构的资产、次优先匹配同平台专属后缀 + 架构资产，无匹配时返回空字符串走明确错误分支，杜绝误下 `.delta`/`.json` 或跨平台错包。
4. **同步发布检查项（`docs/release-sop.md`）**：
   - 在第 4 节补充 `.delta` 资产数量核对与发布前门禁确认。

### 4.2 验收方法

发布完成后执行以下两项核对：

1. **数量核对**：`.delta` 资产数应等于 `目标平台数 × min(3, 已发布历史版本数)`，当前配置下为 6 × 3 = 18。

   ```bash
   gh release view vX.Y.Z --json assets \
     --jq '[.assets[].name | select(endswith(".delta"))] | length'
   ```

2. **引用完整性核对**：CI `Verify release manifest references` 步骤已自动校验所有 `update-*.json` 中的 `.url` 与 `.deltas[].url` 均存在于 Release 资产中；发布后可抽检下载并核对 `signature` 与文件 SHA-256 摘要匹配（ed25519，公钥取自 `mygo.config.ts` 的 `updates.publicKey`）。

### 4.3 已发布版本的处理

- v0.3.5 的 delta 文件只存在于当时的 runner 工作目录，已不可恢复。**不建议手工补造**：`signature` 是对该文件字节的 SHA-256 摘要签名，重新构建的产物即使内容等价，字节也不同，签名无法匹配，清单必须同步重签。
- 因此 v0.3.2–v0.3.5 的用户升级到下一版时仍需整包下载；修复从下一个 tag 起生效。

## 5. 设计约束

- **不要**把 `find` 简化成「上传 `build/` 下全部文件」：`Vela.exe`（未压缩裸二进制，约 18 MB）已作为副产物被上传，放宽过滤会进一步放大资产冗余。仅补单个扩展名分支。
- 避免「清单已发布、文件未到」中间态的核心机制是工作流的 **draft-then-publish** 三段式流程（`draft-release` 建草稿 → `build` 全部上传 → `publish-release` 校验通过后执行 `--draft=false`）；此外上传步骤显式将 `*.json` 放在最后，兼顾对已发布 Release 手动重跑的场景。
- `build` job 在 `macos-latest`（默认 bash 3.2）与 `ubuntu-latest` 上同时运行，上传脚本避免使用 `mapfile` 等 bash 4+ 特性，并在 `set -u` 下对数组展开做非空保护。

## 6. 验证记录

| 时间       | 操作                             | 结果                                                   |
| ---------- | -------------------------------- | ------------------------------------------------------ |
| 2026-11-22 | v0.3.5 Release 资产枚举          | 21 项资产，无 `.delta`                                 |
| 2026-11-22 | `update-*.json` 解析（6 个目标） | 均为 `version=0.3.5`，各含 3 条 `deltas`，签名 64 字节 |
| 2026-11-22 | 6 平台整包 ed25519 验签          | 6/6 通过（签名机制本身正常，缺陷仅在上传环节）         |
| 2026-11-22 | v0.3.3 / v0.3.4 历史核对         | `.delta` 资产数均为 0，确认非 v0.3.5 引入              |
