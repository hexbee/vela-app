import clsx from 'clsx'
import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import {
  IconGlobeLanguageOutline14,
  IconPaperclipOutline16,
  IconSendOutline16,
  IconStopFill16,
} from '../base/icons.js'
import { supportsServerExtensions } from '../../services/api/types.js'
import { setSearch, useSettings } from '../../services/settings.js'
import { useIsMobile } from '../../app/viewport.js'
import { sendMessage, stopReply, useChatStore } from '../../services/chat/store.js'
import {
  addDraftFiles,
  consumePendingQuote,
  EMPTY_DRAFT,
  getDraft,
  prepareDraftMessage,
  removeDraftAttachment,
  retryDraftAttachment,
  setDraftSegments,
  setDraftText,
  useDrafts,
  type DraftSegment,
} from '../../services/chat/drafts.js'
import { createId } from '../../utils/id.js'
import { stopReadAloud } from '../../services/speech/readAloud.js'
import { IconButton } from '../base/IconButton.js'
import { Tooltip } from '../base/Tooltip.js'
import { ensureApiKey } from '../settings/settingsDialogState.js'
import { DragMask, useFileDrop } from './DragMask.js'
import { showToast } from '../base/toastState.js'
import { ACCEPT, useUploads } from '../../services/attachments.js'
import { DraftAttachments } from './DraftAttachments.js'
import { useVoiceInput } from './useVoiceInput.js'
import { VoiceButton } from './VoiceButton.js'
import { Spectrum } from './Spectrum.js'
import { refreshSkills, useSkillStore } from '../../services/skills/store.js'
import { useWorkspaceStore } from '../../services/workspace/store.js'
import { WorkspaceService, type WorkspaceFileCandidate } from '../../mygo.js'
import { AtFileMenu, type AtBreadcrumbItem } from './AtFileMenu.js'
import { ModelPicker } from './ModelPicker.js'
import { PermissionPicker } from './PermissionPicker.js'
import { SlashSkillMenu, type SlashCommandItem, type SlashMenuItem } from './SlashSkillMenu.js'
import { WorkspacePicker } from './WorkspacePicker.js'
import {
  getAtMatchAtCaret,
  getSlashMatchAtCaret,
  handleAtomicQuoteDeletion,
  handleHorizontalTokenNavigation,
  insertPlainTextAtRange,
  insertQuoteAtRange,
  isCaretAtEnd,
  isCaretAtStart,
  moveCaretToEnd,
  parseEditorSegments,
  placeCaretBeforeLeadingTokenIfClicked,
  removeQuoteToken,
  renderEditorSegments,
  replaceAtQueryText,
  replaceAtWithFileToken,
  replaceSlashWithSkillToken,
  type AtCaretMatch,
  type SlashCaretMatch,
} from './quoteEditor.js'
import { useTranslate } from '../../i18n/index.js'
import styles from './ChatInput.module.css'

const QUOTE_CLASSES = {
  token: styles.quoteToken,
  icon: styles.quoteIcon,
  quoteGlyph: styles.quoteGlyph,
  removeGlyph: styles.removeGlyph,
  label: styles.quoteLabel,
  skillToken: styles.skillToken,
  skillIcon: styles.skillIcon,
  skillGlyph: styles.skillGlyph,
  skillLabel: styles.skillLabel,
  fileToken: styles.fileToken,
  fileIcon: styles.fileIcon,
  fileGlyph: styles.fileGlyph,
  fileLabel: styles.fileLabel,
}

/** 输入框：自适应高度的富文本输入框（支持内联多引用） + 文件 + 深度思考与智能搜索开关 + 语音输入 + 发送 / 停止 */
export const ChatInput = memo(function ChatInput({ sessionId }: { sessionId: string | null }) {
  // 切换目标时释放录音、转写和 DOM 状态；文字与附件由草稿服务保留。
  return <Input key={sessionId === null ? 'new' : `session:${sessionId}`} sessionId={sessionId} />
})

const Input = ({ sessionId }: { sessionId: string | null }) => {
  const t = useTranslate()
  const draft = useDrafts((state) => state.drafts.get(sessionId) ?? EMPTY_DRAFT)
  const pendingQuote = useDrafts((state) => state.pendingQuote)
  const { segments, attachments } = draft
  const isEmpty = segments.length === 0
  const draftReady = useUploads((state) => prepareDraftMessage(draft, state.tasks) !== null)
  const streaming = useChatStore(
    (state) => sessionId !== null && Object.hasOwn(state.streaming, sessionId)
  )
  const importingHistory = useChatStore((state) => state.importingHistory)
  const isMobile = useIsMobile()
  const model = useSettings((state) => state.model)
  const search = useSettings((state) => state.search)
  const searchDisabled = !supportsServerExtensions(model)
  const searchActive = !searchDisabled && search
  const fileInputRef = useRef<HTMLInputElement>(null)
  const editorRef = useRef<HTMLDivElement>(null)
  const togglesRef = useRef<HTMLDivElement>(null)
  const historyIndexRef = useRef<number>(-1)
  const savedDraftRef = useRef<DraftSegment[]>([])
  const savedRangeRef = useRef<Range | null>(null)
  const composingRef = useRef(false)
  const [composing, setComposing] = useState(false)
  const lastSyncedSegmentsRef = useRef<DraftSegment[] | null>(null)
  const [slashMatch, setSlashMatch] = useState<SlashCaretMatch | null>(null)
  const [slashIndex, setSlashIndex] = useState(0)
  const [slashDismissed, setSlashDismissed] = useState(false)
  const [atMatch, setAtMatch] = useState<AtCaretMatch | null>(null)
  const [atIndex, setAtIndex] = useState(0)
  const [atDismissed, setAtDismissed] = useState(false)
  const [atDrilled, setAtDrilled] = useState(false)
  const [atItems, setAtItems] = useState<WorkspaceFileCandidate[]>([])
  const atReqIdRef = useRef(0)
  const skills = useSkillStore((state) => state.skills)
  const workspaces = useWorkspaceStore((state) => state.workspaces)
  const draftWorkspaceId = useWorkspaceStore((state) => state.draftWorkspaceId)
  const activeSession = useChatStore((state) =>
    sessionId
      ? (state.sessionCache[sessionId] ?? state.sessions.find((s) => s.id === sessionId))
      : undefined
  )
  const activeWorkspaceId = sessionId ? (activeSession?.workspaceId ?? null) : draftWorkspaceId
  const activeWorkdir =
    workspaces.find((w) => w.id === activeWorkspaceId)?.path ?? activeSession?.scratchDir ?? ''

  useEffect(() => {
    void refreshSkills(activeWorkdir)
  }, [activeWorkdir])

  const slashQuery = slashMatch ? slashMatch.query : null

  const slashCommands = useMemo<SlashCommandItem[]>(
    () => [{ name: 'compact', description: t('slashCommandCompactDesc') }],
    [t]
  )

  const matchingSkills = useMemo(() => {
    if (slashQuery === null || slashDismissed) {
      return []
    }
    if (!slashQuery) {
      return skills
    }
    return skills.filter(
      (skill) =>
        skill.name.toLowerCase().includes(slashQuery) ||
        skill.description.toLowerCase().includes(slashQuery)
    )
  }, [skills, slashQuery, slashDismissed])

  const matchingCommands = useMemo(() => {
    if (slashQuery === null || slashDismissed) {
      return []
    }
    if (!slashQuery) {
      return slashCommands
    }
    return slashCommands.filter(
      (cmd) =>
        cmd.name.toLowerCase().includes(slashQuery) ||
        cmd.description.toLowerCase().includes(slashQuery)
    )
  }, [slashCommands, slashQuery, slashDismissed])

  const slashItems = useMemo<SlashMenuItem[]>(
    () => [
      ...matchingSkills.map((skill): SlashMenuItem => ({ kind: 'skill', skill })),
      ...matchingCommands.map((command): SlashMenuItem => ({ kind: 'command', command })),
    ],
    [matchingSkills, matchingCommands]
  )

  useEffect(() => {
    setSlashDismissed(false)
    setSlashIndex(0)
  }, [slashQuery])

  const atQuery = atMatch ? atMatch.query : null
  const atOpen = atQuery !== null && !atDismissed && slashQuery === null

  useEffect(() => {
    setAtDismissed(false)
    setAtIndex(0)
    if (atQuery === null || !atQuery.includes('/')) {
      setAtDrilled(false)
    }
  }, [atQuery])

  useEffect(() => {
    if (!atOpen || !activeWorkdir || atQuery === null) {
      setAtItems([])
      return
    }
    const reqId = ++atReqIdRef.current
    void WorkspaceService.searchWorkspaceFiles(activeWorkdir, atQuery, 30)
      .then((items) => {
        if (atReqIdRef.current === reqId) {
          setAtItems(items ?? [])
        }
      })
      .catch(() => {
        if (atReqIdRef.current === reqId) {
          setAtItems([])
        }
      })
  }, [atOpen, activeWorkdir, atQuery])

  const atCrumbs = useMemo<AtBreadcrumbItem[]>(() => {
    if (!atOpen || !atDrilled || !atQuery) {
      return []
    }
    const slash = atQuery.lastIndexOf('/')
    if (slash < 0) {
      return []
    }
    const parts = atQuery.slice(0, slash).split('/').filter(Boolean)
    if (parts.length === 0) {
      return []
    }
    const crumbs: AtBreadcrumbItem[] = [{ label: t('atFileMenuCrumbRoot'), path: '' }]
    for (let i = 0; i < parts.length; i++) {
      crumbs.push({
        label: parts[i],
        path: `${parts.slice(0, i + 1).join('/')}/`,
        current: i === parts.length - 1,
      })
    }
    return crumbs
  }, [atOpen, atDrilled, atQuery, t])

  const syncFromDom = useCallback(() => {
    const editor = editorRef.current
    if (!editor) {
      return
    }
    const nextSegments = parseEditorSegments(editor)
    lastSyncedSegmentsRef.current = nextSegments
    setDraftSegments(sessionId, nextSegments)
    setSlashMatch(getSlashMatchAtCaret(editor))
    setAtMatch(getAtMatchAtCaret(editor))
  }, [sessionId])

  const selectSlashItem = useCallback(
    (item: SlashMenuItem) => {
      const editor = editorRef.current
      if (!editor) {
        return
      }
      const name = item.kind === 'skill' ? item.skill.name : item.command.name
      const kind = item.kind
      const liveMatch = slashMatch ?? getSlashMatchAtCaret(editor)
      setSlashDismissed(true)
      setSlashMatch(null)
      if (liveMatch) {
        savedRangeRef.current = replaceSlashWithSkillToken(
          editor,
          liveMatch,
          createId(),
          name,
          QUOTE_CLASSES,
          kind
        )
        syncFromDom()
      } else {
        setDraftSegments(sessionId, [
          { type: 'skill', id: createId(), name, kind },
          { type: 'text', text: ' ' },
        ])
      }
    },
    [sessionId, slashMatch, syncFromDom]
  )

  const selectAtItem = useCallback(
    (item: WorkspaceFileCandidate) => {
      const editor = editorRef.current
      if (!editor) {
        return
      }
      const slash = item.path.lastIndexOf('/')
      const name = slash < 0 ? item.path : item.path.slice(slash + 1)
      const kind = item.kind === 'directory' ? 'directory' : 'file'
      const liveMatch = atMatch ?? getAtMatchAtCaret(editor)
      setAtDismissed(true)
      setAtMatch(null)
      setAtDrilled(false)
      if (liveMatch) {
        savedRangeRef.current = replaceAtWithFileToken(
          editor,
          liveMatch,
          createId(),
          item.path,
          name,
          kind,
          QUOTE_CLASSES
        )
        syncFromDom()
      } else {
        setDraftSegments(sessionId, [
          { type: 'file', id: createId(), path: item.path, name, kind },
          { type: 'text', text: ' ' },
        ])
      }
    },
    [atMatch, sessionId, syncFromDom]
  )

  const drillAtDirectory = useCallback(
    (item: WorkspaceFileCandidate) => {
      const editor = editorRef.current
      if (!editor || item.kind !== 'directory') {
        return
      }
      const liveMatch = atMatch ?? getAtMatchAtCaret(editor)
      if (!liveMatch) {
        return
      }
      const nextQuery = item.path.endsWith('/') ? item.path : `${item.path}/`
      setAtDrilled(true)
      setAtIndex(0)
      savedRangeRef.current = replaceAtQueryText(editor, liveMatch, nextQuery)
      syncFromDom()
    },
    [atMatch, syncFromDom]
  )

  const navigateAtCrumb = useCallback(
    (dirPath: string) => {
      const editor = editorRef.current
      if (!editor) {
        return
      }
      const liveMatch = atMatch ?? getAtMatchAtCaret(editor)
      if (!liveMatch) {
        return
      }
      setAtDrilled(dirPath.length > 0)
      setAtIndex(0)
      savedRangeRef.current = replaceAtQueryText(editor, liveMatch, dirPath)
      syncFromDom()
    },
    [atMatch, syncFromDom]
  )

  const getUserMessagesHistory = useCallback((): DraftSegment[][] => {
    if (!sessionId) {
      return []
    }
    const session = useChatStore.getState().sessionCache[sessionId]
    if (!session || !session.messages) {
      return []
    }
    const userMsgs: DraftSegment[][] = []
    for (let i = session.messages.length - 1; i >= 0; i--) {
      const m = session.messages[i]
      if (m.role === 'user' && (m.content?.trim() || m.segments?.length)) {
        if (m.segments && m.segments.length > 0) {
          userMsgs.push(
            m.segments.map((segment) =>
              segment.type === 'quote'
                ? { type: 'quote', id: createId(), text: segment.text }
                : segment.type === 'skill'
                  ? { type: 'skill', id: createId(), name: segment.name, kind: segment.kind }
                  : segment.type === 'file'
                    ? {
                        type: 'file',
                        id: createId(),
                        path: segment.path,
                        name: segment.name,
                        kind: segment.kind,
                      }
                    : { type: 'text', text: segment.text }
            )
          )
        } else {
          userMsgs.push([{ type: 'text', text: m.content }])
        }
      }
    }
    return userMsgs
  }, [sessionId])

  // 外部草稿变化（初始化、清空、切换历史等）同步到 contentEditable DOM；本地输入已同步时跳过，避免打断光标与 IME。
  useLayoutEffect(() => {
    const editor = editorRef.current
    if (!editor || lastSyncedSegmentsRef.current === segments) {
      return
    }
    renderEditorSegments(editor, segments, QUOTE_CLASSES)
    lastSyncedSegmentsRef.current = segments
  }, [segments])

  // 桌面端挂载后自动聚焦输入框
  useEffect(() => {
    const editor = editorRef.current
    if (!isMobile && editor && !importingHistory) {
      editor.focus({ preventScroll: true })
      moveCaretToEnd(editor)
    }
  }, [isMobile, importingHistory])

  // 记录输入框内最近一次光标位置，便于选中消息文本后点击「引用文本」时在原光标处插入
  useEffect(() => {
    const onSelectionChange = () => {
      const editor = editorRef.current
      const selection = window.getSelection()
      if (!editor || !selection || selection.rangeCount === 0) {
        return
      }
      const range = selection.getRangeAt(0)
      if (editor.contains(range.startContainer) && editor.contains(range.endContainer)) {
        savedRangeRef.current = range.cloneRange()
        setSlashMatch(getSlashMatchAtCaret(editor))
        setAtMatch(getAtMatchAtCaret(editor))
      }
    }
    document.addEventListener('selectionchange', onSelectionChange)
    return () => document.removeEventListener('selectionchange', onSelectionChange)
  }, [])

  // 响应消息列表发来的引用文本插入请求，支持连续多引用
  useEffect(() => {
    if (!pendingQuote || pendingQuote.sessionId !== sessionId) {
      return
    }
    const editor = editorRef.current
    if (!editor) {
      return
    }
    consumePendingQuote(pendingQuote.seq)
    historyIndexRef.current = -1
    savedRangeRef.current = insertQuoteAtRange(
      editor,
      savedRangeRef.current,
      createId(),
      pendingQuote.text,
      QUOTE_CLASSES
    )
    syncFromDom()
  }, [pendingQuote, sessionId, syncFromDom])

  useLayoutEffect(() => {
    const toggles = togglesRef.current
    if (!toggles) {
      return
    }

    const updateLabels = () => {
      toggles.classList.remove(styles.collapsedToggles)
      toggles.classList.toggle(styles.collapsedToggles, toggles.scrollWidth > toggles.clientWidth)
    }

    updateLabels()
    const observer = new ResizeObserver(updateLabels)
    observer.observe(toggles)
    return () => observer.disconnect()
  }, [t])

  const acceptFiles = (files: File[]) => {
    if (files.length === 0 || useChatStore.getState().importingHistory || !ensureApiKey()) {
      return
    }
    const error = addDraftFiles(sessionId, files, useSettings.getState().apiKey ?? '')
    if (error) {
      showToast(error)
    }
  }
  const retryFile = useCallback(
    (id: string) => {
      if (!useChatStore.getState().importingHistory && ensureApiKey()) {
        retryDraftAttachment(sessionId, id, useSettings.getState().apiKey ?? '')
      }
    },
    [sessionId]
  )
  const removeFile = useCallback(
    (id: string) => {
      if (!useChatStore.getState().importingHistory) {
        removeDraftAttachment(sessionId, id)
      }
    },
    [sessionId]
  )
  const dragging = useFileDrop(acceptFiles)

  /** 转写结果插入到光标处，并把光标移到插入内容之后 */
  const voice = useVoiceInput((transcript) => {
    const editor = editorRef.current
    if (!editor) {
      const current = getDraft(sessionId).text
      setDraftText(sessionId, current + transcript)
      return
    }
    historyIndexRef.current = -1
    savedRangeRef.current = insertPlainTextAtRange(editor, savedRangeRef.current, transcript)
    syncFromDom()
  })

  // 欢迎页导入后会话 ID 不变，不能依赖卸载取消语音；订阅开始事件也覆盖快速完成的导入。
  const cancelVoice = voice.cancel
  useEffect(
    () =>
      useChatStore.subscribe((state, previous) => {
        if (state.importingHistory && !previous.importingHistory) {
          cancelVoice()
        }
      }),
    [cancelVoice]
  )

  // 移动端录音时用频谱替代输入框内容
  const recording = voice.status === 'recording'

  // 排队、上传中或上传失败的附件都留在输入框，全部上传成功后才能发送
  const canSend = !importingHistory && !streaming && voice.status === 'idle' && draftReady

  const send = () => {
    if (
      useChatStore.getState().importingHistory ||
      voice.status !== 'idle' ||
      !prepareDraftMessage(getDraft(sessionId), useUploads.getState().tasks)
    ) {
      return
    }
    // 文本与上传完成态由提交动作读取最新快照，不使用上一次渲染的草稿。
    if (!ensureApiKey() || !sendMessage(sessionId)) {
      return
    }
    savedRangeRef.current = null
    stopReadAloud()
    if (isMobile) {
      editorRef.current?.blur()
    } else {
      editorRef.current?.focus({ preventScroll: true })
    }
  }

  return (
    <div
      className={clsx(styles.wrapper, sessionId === null ? styles.home : styles.session)}
      onClick={(event) => {
        if (event.target === event.currentTarget && editorRef.current) {
          editorRef.current.focus({ preventScroll: true })
          moveCaretToEnd(editorRef.current)
        }
      }}
    >
      <SlashSkillMenu
        skills={matchingSkills}
        commands={matchingCommands}
        query={slashQuery ?? ''}
        selectedIndex={Math.min(slashIndex, Math.max(0, slashItems.length - 1))}
        onSelect={selectSlashItem}
        onHover={setSlashIndex}
      />
      <AtFileMenu
        open={atOpen}
        items={atItems}
        crumbs={atCrumbs}
        selectedIndex={Math.min(atIndex, Math.max(0, atItems.length - 1))}
        hasWorkspace={Boolean(activeWorkdir)}
        onSelect={selectAtItem}
        onDrill={drillAtDirectory}
        onCrumb={navigateAtCrumb}
        onHover={setAtIndex}
      />
      <WorkspacePicker sessionId={sessionId} disabled={importingHistory} />
      <DraftAttachments items={attachments} onRetry={retryFile} onRemove={removeFile} />
      <div className={styles.textareaWrapper}>
        <div
          ref={editorRef}
          role="textbox"
          aria-multiline="true"
          aria-label={t('chatInputPlaceholderChat')}
          aria-disabled={importingHistory || undefined}
          contentEditable={!importingHistory}
          suppressContentEditableWarning
          data-placeholder={t('chatInputPlaceholderChat')}
          data-empty={isEmpty && !composing ? 'true' : undefined}
          className={clsx(styles.editor, recording && isMobile && styles.textareaHidden)}
          onCompositionStart={() => {
            composingRef.current = true
            setComposing(true)
          }}
          onCompositionEnd={() => {
            composingRef.current = false
            setComposing(false)
            historyIndexRef.current = -1
            syncFromDom()
          }}
          onInput={() => {
            if (composingRef.current) {
              return
            }
            historyIndexRef.current = -1
            syncFromDom()
          }}
          onMouseDown={(event) => {
            const removeQuoteBtn = (event.target as Element | null)?.closest(
              '[data-quote-remove="true"]'
            )
            if (removeQuoteBtn && editorRef.current) {
              const token = removeQuoteBtn.closest<HTMLElement>('[data-quote-id]')
              if (token) {
                event.preventDefault()
                historyIndexRef.current = -1
                savedRangeRef.current = removeQuoteToken(editorRef.current, token)
                syncFromDom()
                return
              }
            }
            const removeSkillBtn = (event.target as Element | null)?.closest(
              '[data-skill-remove="true"]'
            )
            if (removeSkillBtn && editorRef.current) {
              const token = removeSkillBtn.closest<HTMLElement>('[data-skill-id]')
              if (token) {
                event.preventDefault()
                historyIndexRef.current = -1
                savedRangeRef.current = removeQuoteToken(editorRef.current, token)
                syncFromDom()
                return
              }
            }
            const removeFileBtn = (event.target as Element | null)?.closest(
              '[data-file-remove="true"]'
            )
            if (removeFileBtn && editorRef.current) {
              const token = removeFileBtn.closest<HTMLElement>('[data-file-id]')
              if (token) {
                event.preventDefault()
                historyIndexRef.current = -1
                savedRangeRef.current = removeQuoteToken(editorRef.current, token)
                syncFromDom()
                return
              }
            }
            if (
              event.button === 0 &&
              editorRef.current &&
              placeCaretBeforeLeadingTokenIfClicked(editorRef.current, event.clientX, event.clientY)
            ) {
              event.preventDefault()
              return
            }
            // 首次点击时手动聚焦，避免 Safari 把整个页面滚到输入框位置。
            if (isMobile && event.button === 0 && document.activeElement !== event.currentTarget) {
              event.preventDefault()
              event.currentTarget.focus({ preventScroll: true })
            }
          }}
          onKeyDown={(event) => {
            if (event.nativeEvent.isComposing || composingRef.current) {
              return
            }

            if (
              (event.key === 'ArrowLeft' || event.key === 'ArrowRight') &&
              !event.shiftKey &&
              !event.altKey &&
              !event.metaKey &&
              !event.ctrlKey &&
              editorRef.current
            ) {
              if (
                handleHorizontalTokenNavigation(
                  editorRef.current,
                  event.key === 'ArrowLeft' ? 'left' : 'right'
                )
              ) {
                event.preventDefault()
                return
              }
            }

            if (event.key === 'Backspace' && editorRef.current) {
              if (handleAtomicQuoteDeletion(editorRef.current, 'backward')) {
                event.preventDefault()
                historyIndexRef.current = -1
                syncFromDom()
                return
              }
            }

            if (event.key === 'Delete' && editorRef.current) {
              if (handleAtomicQuoteDeletion(editorRef.current, 'forward')) {
                event.preventDefault()
                historyIndexRef.current = -1
                syncFromDom()
                return
              }
            }

            if (slashItems.length > 0) {
              if (event.key === 'ArrowDown') {
                event.preventDefault()
                setSlashIndex((prev) => (prev + 1) % slashItems.length)
                return
              }
              if (event.key === 'ArrowUp') {
                event.preventDefault()
                setSlashIndex((prev) => (prev - 1 + slashItems.length) % slashItems.length)
                return
              }
              if (event.key === 'Escape') {
                event.preventDefault()
                setSlashDismissed(true)
                return
              }
              if ((event.key === 'Enter' && !event.shiftKey) || event.key === 'Tab') {
                event.preventDefault()
                const chosen = slashItems[Math.min(slashIndex, slashItems.length - 1)]
                if (chosen) {
                  selectSlashItem(chosen)
                }
                return
              }
            }

            if (atOpen && atItems.length > 0) {
              if (event.key === 'ArrowDown') {
                event.preventDefault()
                setAtIndex((prev) => (prev + 1) % atItems.length)
                return
              }
              if (event.key === 'ArrowUp') {
                event.preventDefault()
                setAtIndex((prev) => (prev - 1 + atItems.length) % atItems.length)
                return
              }
              if (event.key === 'Escape') {
                event.preventDefault()
                setAtDismissed(true)
                return
              }
              if (event.key === 'Tab') {
                event.preventDefault()
                const chosen = atItems[Math.min(atIndex, atItems.length - 1)]
                if (chosen) {
                  if (chosen.kind === 'directory') {
                    drillAtDirectory(chosen)
                  } else {
                    selectAtItem(chosen)
                  }
                }
                return
              }
              if (event.key === 'Enter' && !event.shiftKey) {
                event.preventDefault()
                const chosen = atItems[Math.min(atIndex, atItems.length - 1)]
                if (chosen) {
                  selectAtItem(chosen)
                }
                return
              }
            }

            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              historyIndexRef.current = -1
              send()
              return
            }

            // 按方向键 ↑ 快速调出上一条发送的消息进行重新编辑或重发
            if (
              event.key === 'ArrowUp' &&
              !event.shiftKey &&
              !event.altKey &&
              !event.metaKey &&
              !event.ctrlKey &&
              editorRef.current
            ) {
              if (isEmpty || isCaretAtStart(editorRef.current)) {
                const history = getUserMessagesHistory()
                if (history.length > 0) {
                  const nextIndex = historyIndexRef.current + 1
                  if (nextIndex < history.length) {
                    event.preventDefault()
                    if (historyIndexRef.current === -1) {
                      savedDraftRef.current = segments
                    }
                    historyIndexRef.current = nextIndex
                    setDraftSegments(sessionId, history[nextIndex])
                    requestAnimationFrame(() => {
                      if (editorRef.current) {
                        moveCaretToEnd(editorRef.current)
                      }
                    })
                  }
                }
              }
              return
            }

            // 按方向键 ↓ 切换回更新的消息或之前未发送的草稿
            if (
              event.key === 'ArrowDown' &&
              !event.shiftKey &&
              !event.altKey &&
              !event.metaKey &&
              !event.ctrlKey &&
              editorRef.current
            ) {
              if ((isEmpty || isCaretAtEnd(editorRef.current)) && historyIndexRef.current >= 0) {
                event.preventDefault()
                const history = getUserMessagesHistory()
                const nextIndex = historyIndexRef.current - 1
                historyIndexRef.current = nextIndex
                setDraftSegments(
                  sessionId,
                  nextIndex === -1 ? savedDraftRef.current : history[nextIndex]
                )
                requestAnimationFrame(() => {
                  if (editorRef.current) {
                    moveCaretToEnd(editorRef.current)
                  }
                })
              }
              return
            }
          }}
          onPaste={(event) => {
            const files = Array.from(event.clipboardData.files)
            if (files.length > 0) {
              event.preventDefault()
              acceptFiles(files)
              return
            }
            const plainText = event.clipboardData.getData('text/plain')
            event.preventDefault()
            if (plainText && editorRef.current) {
              historyIndexRef.current = -1
              savedRangeRef.current = insertPlainTextAtRange(
                editorRef.current,
                savedRangeRef.current,
                plainText
              )
              syncFromDom()
            }
          }}
        />
        {recording && isMobile ? <Spectrum getAnalyser={voice.getAnalyser} /> : null}
      </div>
      <div className={styles.functionRow} onClick={(event) => event.stopPropagation()}>
        {/* 附件按钮 + 三档权限下拉（对齐 DSH）+ 智能搜索开关 */}
        <div ref={togglesRef} className={styles.toggles}>
          <IconButton
            size="m"
            tone="primary"
            icon={<IconPaperclipOutline16 />}
            tooltip={t('uploadFile')}
            disabled={importingHistory}
            onClick={() => ensureApiKey() && fileInputRef.current?.click()}
          />
          <input
            ref={fileInputRef}
            type="file"
            multiple
            hidden
            accept={ACCEPT}
            onChange={(event) => {
              acceptFiles(Array.from(event.target.files ?? []))
              event.target.value = ''
            }}
          />
          <PermissionPicker sessionId={sessionId} disabled={importingHistory} />
          <Tooltip
            content={
              searchDisabled ? t('searchUnavailableForModelTooltip') : t('deactivatedSearchTooltip')
            }
          >
            <button
              type="button"
              className={clsx(
                styles.toggle,
                searchActive && styles.selected,
                searchDisabled && styles.disabled
              )}
              aria-label={t('search')}
              aria-pressed={searchActive}
              aria-disabled={searchDisabled}
              onClick={() => {
                if (!searchDisabled) {
                  setSearch(!search)
                }
              }}
            >
              <IconGlobeLanguageOutline14 />
              <span className={styles.toggleLabel}>{t('search')}</span>
            </button>
          </Tooltip>
        </div>
        <div className={styles.right}>
          <ModelPicker disabled={importingHistory} />
          <VoiceButton
            status={voice.status}
            disabled={!voice.available}
            seconds={voice.seconds}
            onClick={() => {
              if (
                voice.status !== 'idle' ||
                (!useChatStore.getState().importingHistory && ensureApiKey())
              ) {
                void voice.toggle()
              }
            }}
          />
          {streaming && sessionId ? (
            <button
              type="button"
              className={styles.sendButton}
              aria-label={t('chatInputStopButtonTooltip')}
              onClick={() => {
                stopReadAloud()
                stopReply(sessionId)
              }}
            >
              <IconStopFill16 />
            </button>
          ) : (
            <button
              type="button"
              className={clsx(styles.sendButton, !canSend && styles.disabled)}
              aria-label={t('send')}
              aria-disabled={!canSend}
              onClick={send}
            >
              <IconSendOutline16 />
            </button>
          )}
        </div>
      </div>
      {dragging ? <DragMask /> : null}
    </div>
  )
}
