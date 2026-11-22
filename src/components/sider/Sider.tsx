import clsx from 'clsx'
import { useEffect, useMemo, useState } from 'react'
import { revealInFileManagerKey, useTranslate } from '../../i18n/index.js'
import {
  IconChevronRightOutline14,
  IconEditOutline16,
  IconEllipsisOutline16,
  IconFolderOpenOutline14,
  IconFolderOutline14,
  IconPanelLeftOutline16,
  IconPlusOutline14,
  IconSearchOutline16,
  IconSettingsOutline16,
  IconTrashOutline16,
  NewChatIcon,
  VelaBrandMark,
} from '../base/icons.js'
import { openChatSearch } from '../chatSearch/chatSearchState.js'
import { setSiderCollapsed } from '../../services/settings.js'
import { groupSessions } from '../../services/chat/sessionGroups.js'
import {
  newChat,
  newChatInWorkspace,
  removeSession,
  removeWorkspace,
  useChatStore,
} from '../../services/chat/store.js'
import type { SessionSummary } from '../../services/chat/types.js'
import {
  pickAndAddWorkspace,
  renameWorkspace,
  revealWorkspaceInFinder,
  toggleWorkspaceCollapsed,
  useWorkspaceStore,
} from '../../services/workspace/store.js'
import type { Workspace } from '../../services/workspace/types.js'
import { Button } from '../base/Button.js'
import { IconButton } from '../base/IconButton.js'
import { Menu } from '../base/Menu.js'
import { Modal } from '../base/Modal.js'
import { showToast } from '../base/toastState.js'
import { openSettings } from '../settings/settingsDialogState.js'
import { DesktopUpdateIndicator } from '../updater/DesktopUpdateIndicator.js'
import { useUpdateStore } from '../../services/updater/updateStore.js'
import { modKey } from '../../app/shortcuts.js'
import { closeMobileSider, useMobileSider } from './mobileSiderState.js'
import { SessionItem } from './SessionItem.js'
import styles from './Sider.module.css'

/** 展开的侧边栏：名称与收起按钮、开启新对话、工作区分组 + 按时间分组的普通会话列表、底部设置；移动端放在抽屉里，收起按钮关闭抽屉 */
export const Sider = ({ mobile = false }: { mobile?: boolean }) => {
  const t = useTranslate()
  const sessions = useChatStore((state) => state.sessions)
  const activeId = useChatStore((state) => state.activeId)
  const workspaces = useWorkspaceStore((state) => state.workspaces)
  const collapsedIds = useWorkspaceStore((state) => state.collapsedIds)
  const [now, setNow] = useState(Date.now)
  const [deleting, setDeleting] = useState<SessionSummary | null>(null)
  const [removeScratchOnDelete, setRemoveScratchOnDelete] = useState(true)
  const [deletePending, setDeletePending] = useState(false)
  const [removingWorkspace, setRemovingWorkspace] = useState<Workspace | null>(null)
  const [removeWorkspacePending, setRemoveWorkspacePending] = useState(false)
  const [renamingWorkspaceId, setRenamingWorkspaceId] = useState<string | null>(null)
  const [pickingWorkspace, setPickingWorkspace] = useState(false)

  const workspaceSessionsMap = useMemo(() => {
    const map = new Map<string, SessionSummary[]>()
    for (const workspace of workspaces) {
      map.set(workspace.id, [])
    }
    for (const session of sessions) {
      if (session.workspaceId && map.has(session.workspaceId)) {
        map.get(session.workspaceId)?.push(session)
      }
    }
    return map
  }, [sessions, workspaces])

  const generalSessions = useMemo(() => {
    const workspaceIds = new Set(workspaces.map((w) => w.id))
    return sessions.filter(
      (session) => !session.workspaceId || !workspaceIds.has(session.workspaceId)
    )
  }, [sessions, workspaces])

  const groups = useMemo(() => groupSessions(generalSessions, now, t), [generalSessions, now, t])

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>
    const refresh = () => {
      clearTimeout(timer)
      const time = Date.now()
      setNow(time)
      const midnight = new Date(time).setHours(24, 0, 0, 0)
      timer = setTimeout(refresh, midnight - time)
    }
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') {
        refresh()
      }
    }
    refresh()
    document.addEventListener('visibilitychange', onVisibilityChange)
    window.addEventListener('focus', refresh)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibilityChange)
      window.removeEventListener('focus', refresh)
    }
  }, [])

  useEffect(() => {
    // 启动时后台静默检查更新
    useUpdateStore
      .getState()
      .checkUpdate()
      .catch(() => {})
  }, [])

  const handleAddWorkspace = async () => {
    if (pickingWorkspace) {
      return
    }
    setPickingWorkspace(true)
    try {
      await pickAndAddWorkspace()
    } catch {
      showToast({ key: 'workspacePickFailed', type: 'error' })
    } finally {
      setPickingWorkspace(false)
    }
  }

  return (
    <div className={clsx(styles.sider, mobile && styles.mobile)}>
      <div className={styles.top}>
        <div className={styles.brand}>
          <span className={styles.brandBadge}>
            <VelaBrandMark size={18} className={styles.brandIcon} />
          </span>
          <span className={styles.appName}>{t('appName')}</span>
        </div>
        <div className={styles.topActions}>
          <IconButton
            size="m"
            icon={<IconSearchOutline16 />}
            tooltip={`${t('searchChats')} (${modKey}K)`}
            tooltipPlacement="bottom"
            onClick={() => {
              closeMobileSider()
              openChatSearch()
            }}
          />
          <IconButton
            size="m"
            icon={<IconPanelLeftOutline16 />}
            tooltip={`${t('collapseSiderbarButton')} (${modKey}B)`}
            tooltipPlacement="bottom"
            onClick={() => (mobile ? closeMobileSider() : setSiderCollapsed(true))}
          />
        </div>
      </div>
      <button
        type="button"
        className={styles.newChat}
        title={`${t('newChat')} (${modKey}N)`}
        onClick={() => {
          newChat()
          closeMobileSider()
        }}
      >
        <span className={styles.newChatIcon}>
          <NewChatIcon />
        </span>
        {t('newChat')}
      </button>
      <div className={styles.listWrap}>
        <div className={styles.list}>
          <div className={styles.workspacesBlock}>
            <div className={styles.workspaceSectionHeader}>
              <span>{t('workspacesSectionTitle')}</span>
              <IconButton
                icon={<IconPlusOutline14 />}
                tooltip={t('workspaceAdd')}
                disabled={pickingWorkspace}
                onClick={() => void handleAddWorkspace()}
              />
            </div>
            {workspaces.map((workspace) => {
              const collapsed = collapsedIds[workspace.id] === true
              const items = workspaceSessionsMap.get(workspace.id) ?? []
              const renaming = renamingWorkspaceId === workspace.id
              return (
                <div key={workspace.id} className={styles.workspaceGroup}>
                  {renaming ? (
                    <input
                      className={styles.workspaceRenameInput}
                      defaultValue={workspace.name}
                      autoFocus
                      onFocus={(event) => event.target.select()}
                      onBlur={(event) => {
                        setRenamingWorkspaceId(null)
                        void renameWorkspace(workspace.id, event.target.value)
                      }}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter' && !event.nativeEvent.isComposing) {
                          setRenamingWorkspaceId(null)
                          void renameWorkspace(workspace.id, event.currentTarget.value)
                        } else if (event.key === 'Escape') {
                          setRenamingWorkspaceId(null)
                        }
                      }}
                    />
                  ) : (
                    <div
                      className={styles.workspaceRow}
                      title={workspace.path}
                      onClick={() => toggleWorkspaceCollapsed(workspace.id)}
                    >
                      <span
                        className={clsx(
                          styles.workspaceChevron,
                          !collapsed && styles.workspaceChevronOpen
                        )}
                      >
                        <IconChevronRightOutline14 />
                      </span>
                      <span className={styles.workspaceFolderIcon}>
                        {collapsed ? <IconFolderOutline14 /> : <IconFolderOpenOutline14 />}
                      </span>
                      <span className={styles.workspaceName}>{workspace.name}</span>
                      <div
                        className={styles.workspaceActions}
                        onClick={(event) => event.stopPropagation()}
                      >
                        <IconButton
                          icon={<IconPlusOutline14 />}
                          tooltip={t('workspaceNewChat')}
                          onClick={() => {
                            newChatInWorkspace(workspace.id)
                            closeMobileSider()
                          }}
                        />
                        <Menu
                          options={[
                            {
                              key: 'reveal',
                              label: t(revealInFileManagerKey),
                              icon: <IconFolderOpenOutline14 />,
                              onSelect: () => void revealWorkspaceInFinder(workspace.path),
                            },
                            {
                              key: 'rename',
                              label: t('sessionItemMenuRenameOption'),
                              icon: <IconEditOutline16 />,
                              onSelect: () => setRenamingWorkspaceId(workspace.id),
                            },
                            {
                              key: 'remove',
                              label: t('workspaceRemove'),
                              icon: <IconTrashOutline16 />,
                              danger: true,
                              onSelect: () => setRemovingWorkspace(workspace),
                            },
                          ]}
                        >
                          {({ open, toggle }) => (
                            <IconButton
                              icon={<IconEllipsisOutline16 />}
                              aria-label={t('more')}
                              aria-expanded={open}
                              aria-haspopup="menu"
                              onClick={toggle}
                            />
                          )}
                        </Menu>
                      </div>
                    </div>
                  )}
                  {!collapsed ? (
                    <div className={styles.workspaceSessions}>
                      {items.length > 0 ? (
                        items.map((session) => (
                          <SessionItem
                            key={session.id}
                            session={session}
                            active={session.id === activeId}
                            onDelete={() => {
                              setRemoveScratchOnDelete(true)
                              setDeleting(session)
                            }}
                          />
                        ))
                      ) : (
                        <div className={styles.workspaceEmpty}>{t('workspaceEmptyChats')}</div>
                      )}
                    </div>
                  ) : null}
                </div>
              )
            })}
          </div>
          {groups.map((group) => (
            <div key={group.key} className={styles.section}>
              <div className={styles.sectionHeader}>{group.label}</div>
              {group.sessions.map((session) => (
                <SessionItem
                  key={session.id}
                  session={session}
                  active={session.id === activeId}
                  onDelete={() => {
                    setRemoveScratchOnDelete(true)
                    setDeleting(session)
                  }}
                />
              ))}
            </div>
          ))}
        </div>
      </div>
      <div className={styles.bottom}>
        <button
          type="button"
          className={styles.settings}
          title={`${t('settings')} (${modKey},)`}
          onClick={() => openSettings('general')}
        >
          <span className={styles.settingsIcon}>
            <IconSettingsOutline16 />
          </span>
          <span className={styles.settingsLabel}>{t('settings')}</span>
        </button>
        <DesktopUpdateIndicator />
      </div>
      {deleting ? (
        <Modal
          title={t('deleteSessionModalContentTitle')}
          onClose={() => {
            if (!deletePending) {
              setDeleting(null)
            }
          }}
          footer={
            <>
              <Button variant="outlined" disabled={deletePending} onClick={() => setDeleting(null)}>
                {t('cancel')}
              </Button>
              <Button
                variant="danger"
                disabled={deletePending}
                onClick={async () => {
                  setDeletePending(true)
                  try {
                    if (
                      await removeSession(deleting.id, {
                        removeScratchDir: Boolean(deleting.scratchDir && removeScratchOnDelete),
                      })
                    ) {
                      setDeleting(null)
                    } else {
                      showToast({ key: 'sessionDeleteFailed', type: 'error' })
                    }
                  } finally {
                    setDeletePending(false)
                  }
                }}
              >
                {t('delete')}
              </Button>
            </>
          }
        >
          <div>{t('deleteSessionWithFilesDescription')}</div>
          {deleting.scratchDir ? (
            <label className={styles.deleteScratchOption}>
              <input
                type="checkbox"
                checked={removeScratchOnDelete}
                onChange={(event) => setRemoveScratchOnDelete(event.target.checked)}
              />
              <span>{t('deleteSessionRemoveScratch')}</span>
            </label>
          ) : null}
        </Modal>
      ) : null}
      {removingWorkspace ? (
        <Modal
          title={t('workspaceRemoveConfirmTitle')}
          onClose={() => {
            if (!removeWorkspacePending) {
              setRemovingWorkspace(null)
            }
          }}
          footer={
            <>
              <Button
                variant="outlined"
                disabled={removeWorkspacePending}
                onClick={() => setRemovingWorkspace(null)}
              >
                {t('cancel')}
              </Button>
              <Button
                variant="danger"
                disabled={removeWorkspacePending}
                onClick={async () => {
                  setRemoveWorkspacePending(true)
                  try {
                    await removeWorkspace(removingWorkspace.id)
                    setRemovingWorkspace(null)
                  } finally {
                    setRemoveWorkspacePending(false)
                  }
                }}
              >
                {t('delete')}
              </Button>
            </>
          }
        >
          {t('workspaceRemoveConfirmDescription')}
        </Modal>
      ) : null}
    </div>
  )
}

/** 移动端抽屉：浮在页面上方，点空白区域关闭。 */
export const MobileSider = () => {
  const open = useMobileSider((state) => state.open)
  if (!open) {
    return null
  }
  return (
    <>
      <div className={styles.drawerMask} onClick={closeMobileSider} />
      <div className={styles.drawer}>
        <Sider mobile />
      </div>
    </>
  )
}
