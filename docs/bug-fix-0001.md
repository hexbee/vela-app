# Bug Fix 0001：侧边栏未激活会话的弹出菜单漂移至左上角

- **状态**：已修复并验收
- **日期**：2026-11-18
- **涉及文件**：
  - `src/components/sider/SessionItem.tsx`
  - `src/components/sider/SessionItem.module.css`
  - `src/components/base/anchored.ts`

## 1. 问题描述

- **复现步骤**：在侧边栏存在未选中激活的会话时，鼠标悬停该会话行显示 `...` 按钮并点击，此时「重命名 / 删除」弹出菜单正常显示在按钮下方；但只要将鼠标移动到弹出菜单上，原会话行的悬停背景和 `...` 按钮会立即消失，同时弹出菜单瞬间跳到窗口左上角（覆盖窗口左上角控制按钮区域）。
- **对照现象**：当前处于选中激活态（`.active`）的会话不会复现该问题。

## 2. 根因分析

1. **触发器受 `:hover` 控制且使用 `display: none` 隐藏**：在 `src/components/sider/SessionItem.tsx` 中，`<Menu>` 及其 `...` 触发按钮包裹在 `.action` 容器内；在 `src/components/sider/SessionItem.module.css` 中，`.action` 默认 `display: none`，桌面端（`@media (hover: hover)`）仅在 `.item:hover` 或 `.item.active` 时切换为 `display: flex`。
2. **弹出层脱离原 DOM 树**：在 `src/components/base/Menu.tsx` 中，下拉菜单通过 `createPortal(..., document.body)` 挂载到 `document.body`，并通过 `useAnchoredPosition` 追踪 `.action` 内部的 `<span ref={anchorRef}>` 来计算定位。
3. **锚点盒模型丢失导致坐标归零**：当会话未激活时，鼠标从列表项移入挂载于 `body` 的弹出菜单，列表项失去 `:hover` 状态，`.action` 恢复 `display: none`，`anchorRef` 脱离布局流导致几何尺寸归零。重绘或重算定位时读取到 `(0, 0)` 坐标，弹出菜单随即漂移到窗口左上角。

## 3. 修复方案与核心改动

采用「主修复 + 底层防御加固」两层改动，遵循单一数据源、无冗余状态、标准无障碍语义与最小改动原则。

### 3.1 主修复：菜单展开期间锁定该行高亮与 `.action` 显示

- **透出菜单展开态（`src/components/sider/SessionItem.tsx`）**：直接复用 `<Menu>` 已向 `children` 提供的 `{ open, toggle }`，不在 `SessionItem` 中维护重复状态；在 `...` 对应的 `IconButton` 上补充 `aria-expanded={open}` 与 `aria-haspopup="menu"`（`IconButton` 原生透传 `ButtonHTMLAttributes`）。

```tsx
{
  ;({ open, toggle }) => (
    <IconButton
      icon={<IconEllipsisOutline16 />}
      aria-label={t('more')}
      aria-expanded={open}
      aria-haspopup="menu"
      onClick={toggle}
    />
  )
}
```

- **更新桌面端悬停样式（`src/components/sider/SessionItem.module.css`）**：在 `@media (hover: hover)` 中将 `[aria-expanded='true']` 并入未激活行的悬停底色及 `.action` 的显示条件。

```css
@media (hover: hover) {
  &:not(.active):hover,
  &:not(.active):has([aria-expanded='true']) {
    background-color: var(--dsw-specific-sidebar-nav-item-hover);
  }

  &:hover,
  &.active,
  &:has([aria-expanded='true']) {
    .action {
      display: flex;
    }
  }
}
```

### 3.2 底层定位防御加固

- **跳过零宽高锚点更新（`src/components/base/anchored.ts`）**：在 `useAnchoredPosition` 的 `update` 回调中读取 `anchor.getBoundingClientRect()` 后增加防御判断；当 `rect.width === 0 && rect.height === 0` 时直接返回，保留上一次有效坐标，防止项目内其他浮层因锚点短暂隐藏而跳至 `(0, 0)`。

```ts
const rect = anchor.getBoundingClientRect()
if (rect.width === 0 && rect.height === 0) {
  return
}
```

## 4. 设计约束

- 不通过将 `.action` 的 `display: none` 改为 `opacity: 0` 绕过问题，避免未悬停时 `.action` 持续占据右侧宽度导致长标题提前截断，同时保证菜单展开时维持目标行高亮。
- 保留移动端/无悬停设备（`@media (hover: none)`）下 `.action` 常显的既有行为。
- 保留 `.action` 容器上 `onClick={(event) => event.stopPropagation()}`，防止点击 `...` 误触发切换会话。
- 保持已激活会话（`.active`）的现有视觉样式不受影响。

## 5. 验证与验收结果

### 5.1 构建与静态检查

- **TypeScript 类型检查（`tsc -p tsconfig.json`）**：通过（0 错误）。
- **代码检查（`oxlint`）**：本次改动文件（`SessionItem.tsx`、`anchored.ts`）0 警告、0 错误。
- **前端生产构建（`NODE_ENV=production rspack build -c rspack.config.ts`）**：编译通过。

### 5.2 交互验收清单

- [x] **核心回归场景**：未激活会话悬停 → 点击 `...` → 鼠标移入菜单并在「重命名」「删除」间移动，会话行保持悬停高亮、`...` 按钮保持可见、弹出菜单固定在原位不漂移。
- [x] **关闭恢复**：菜单打开时点击空白处或按 `Esc` 关闭，行高亮与 `...` 按钮恢复隐藏。
- [x] **菜单动作**：通过未激活会话的菜单执行「重命名」与「删除」，原有功能正常且不误激活会话。
- [x] **排版检查**：未悬停且未激活的长标题会话右侧无多余空白，标题占满行宽后正常省略号截断。
- [x] **切换检查**：菜单打开时直接点击另一条会话的 `...` 或切换选中会话，状态切换干净无残留。
