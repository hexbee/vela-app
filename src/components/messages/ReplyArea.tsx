import { useTranslate } from '../../i18n/index.js'
import clsx from 'clsx'
import {
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import {
  IconBrowseOutline16,
  IconBrowseWarningOutline16,
  IconChevronDownOutline14,
  IconChevronRightOutline14,
  IconCheckedOutline14,
  IconCloseFill14,
  IconLinkOutline16,
  IconRightUpOutline14,
  IconSearchOutline16,
  IconSearchWarningOutline16,
  IconThinkOutline16,
} from '../base/icons.js'
import type { AssistantMessage } from '../../services/chat/types.js'
import {
  getAreaTitle,
  isDeepSearchKind,
  isImageSearchKind,
  type FindStep,
  type OpenStep,
  type PageItem,
  type ReadLinkStep,
  type ReplyView,
  type SearchStep,
  type Source,
  type Step,
  type ThinkingStep,
} from '../../services/chat/view.js'
import { Tooltip } from '../base/Tooltip.js'
import { Markdown } from '../markdown/Markdown.js'
import { SiteThumbnails } from '../search/SiteLogo.js'
import { toggleSearchPanel, useSearchPanel } from '../search/searchPanelState.js'
import { ToolCallCard } from './ToolCallCard.js'
import styles from './ReplyArea.module.css'
import './thinking.markdown.css'

/** 进行中时每秒刷新，用于思考用时与搜索词轮播 */
const useTicker = (active: boolean, interval: number) => {
  const [tick, setTick] = useState(0)
  useEffect(() => {
    if (!active) {
      return
    }
    const timer = setInterval(() => setTick((value) => value + 1), interval)
    return () => clearInterval(timer)
  }, [active, interval])
  return tick
}

/** 取文本首行（用于已完成的单段思考折叠摘要） */
const firstLine = (text: string): string => {
  const trimmed = text.trimStart()
  const newline = trimmed.indexOf('\n')
  return (newline === -1 ? trimmed : trimmed.slice(0, newline)).trim()
}

/** 取最近完成段落的首行（流式思考预览，对齐 deepseek-harness ReasoningRow） */
const latestCompletedParagraphFirstLine = (text: string): string => {
  let summary = ''
  let paragraphStart = 0
  const separator = /\r?\n(?:[\t ]*\r?\n)+/g
  while (true) {
    const nextParagraph = separator.exec(text)
    const paragraphEnd =
      nextParagraph === null ? text.length : nextParagraph.index + nextParagraph[0].indexOf('\n')
    const newline = text.indexOf('\n', paragraphStart)
    if (newline !== -1 && newline <= paragraphEnd) {
      const candidate = text.slice(paragraphStart, newline).trim()
      if (candidate !== '') {
        summary = candidate
      }
    }
    if (nextParagraph === null) {
      return summary || firstLine(text)
    }
    paragraphStart = nextParagraph.index + nextParagraph[0].length
  }
}

type ScrollEdges = { canScrollUp: boolean; canScrollDown: boolean }
const AT_REST: ScrollEdges = { canScrollUp: false, canScrollDown: false }
const FOLLOW_THRESHOLD_PX = 24

/**
 * 思考与工具步骤的时间线（对齐 deepseek-harness 体验）：
 * - 进行中默认展开，并限制最大高度（min(400px, 50vh)）+ 内部滚动 + 自动贴底 + 上下渐隐；
 * - 进入正文或回复完成后自动收起（若用户手动切换过则尊重用户选择）；
 * - 多步骤任务中，每段思考默认折叠为单行动态预览，点击可独立展开且标题吸顶。
 */
export const ReplyArea = memo(function ReplyArea({
  message,
  view,
  streaming,
  hasResponse,
}: {
  message: AssistantMessage
  view: ReplyView
  streaming: boolean
  hasResponse: boolean
}) {
  const t = useTranslate()
  const [open, setOpen] = useState(() => streaming || !hasResponse)
  const userToggledRef = useRef(false)
  const bodyRef = useRef<HTMLDivElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const followingRef = useRef(true)
  const [edges, setEdges] = useState<ScrollEdges>(AT_REST)

  // 进入正文输出或整条回复完成后自动折叠，尊重用户手动展开/收起选择。
  useEffect(() => {
    if (userToggledRef.current) {
      return
    }
    if (hasResponse) {
      setOpen(false)
    } else if (streaming) {
      setOpen(true)
    }
  }, [hasResponse, streaming])

  const syncScrollEdges = useCallback(() => {
    const el = bodyRef.current
    if (!el) {
      setEdges(AT_REST)
      return
    }
    const maxScroll = el.scrollHeight - el.clientHeight
    const next: ScrollEdges =
      maxScroll > 2
        ? { canScrollUp: el.scrollTop > 1, canScrollDown: el.scrollTop < maxScroll - 1 }
        : AT_REST
    setEdges((prev) =>
      prev.canScrollUp === next.canScrollUp && prev.canScrollDown === next.canScrollDown
        ? prev
        : next
    )
  }, [])

  const handleScroll = useCallback(() => {
    const el = bodyRef.current
    if (!el) {
      return
    }
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    followingRef.current = distanceFromBottom <= FOLLOW_THRESHOLD_PX
    syncScrollEdges()
  }, [syncScrollEdges])

  // 滚轮/触摸离开底部阈值外时停止自动贴底。
  const interruptFollow = useCallback(() => {
    const el = bodyRef.current
    if (!el) {
      return
    }
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    if (distanceFromBottom > FOLLOW_THRESHOLD_PX) {
      followingRef.current = false
    }
  }, [])

  // 用户手动展开步骤时无条件暂停自动贴底，避免新展开内容被立刻顶出视口。
  const pauseFollow = useCallback(() => {
    followingRef.current = false
  }, [])

  // 监听内部高度变化：流式阶段若仍在跟随底部，则自动贴底并更新上下渐隐状态。
  useLayoutEffect(() => {
    const body = bodyRef.current
    const content = contentRef.current
    if (!open || !body) {
      return
    }
    if (streaming) {
      followingRef.current = true
    }
    const update = () => {
      if (streaming && followingRef.current) {
        body.scrollTop = body.scrollHeight
      }
      syncScrollEdges()
    }
    update()
    if (typeof ResizeObserver === 'undefined') {
      return
    }
    const observer = new ResizeObserver(update)
    observer.observe(body)
    if (content) {
      observer.observe(content)
    }
    return () => observer.disconnect()
  }, [open, streaming, syncScrollEdges])

  const titleActive = streaming && !hasResponse
  useTicker(titleActive, 1000)
  if (view.steps.length === 0 && !(message.thinking && streaming)) {
    return null
  }
  // 未记录请求开关时，按是否有思考内容显示标题；纯工具任务也提供可折叠过程栏。
  const thinking = message.thinking ?? view.steps.some((step) => step.type === 'thinking')
  const toolStepCount = view.steps.filter((step) => step.type === 'client_tool').length
  const hasCollapsibleProcess = thinking || toolStepCount > 0
  let title: string | null = null
  if (thinking) {
    title = getAreaTitle(view, streaming, Date.now(), hasResponse)
  } else if (toolStepCount > 0) {
    title =
      streaming && !hasResponse
        ? t('toolRunningTitle')
        : t(toolStepCount === 1 ? 'toolProcessDoneOne' : 'toolProcessDone', {
            count: toolStepCount,
          })
  }
  const stopped = !streaming && !hasResponse
  const singleThinkingOnly = view.steps.length === 1 && view.steps[0]?.type === 'thinking'

  return (
    <div className={styles.area}>
      {title ? (
        <div
          className={styles.title}
          onClick={() => {
            userToggledRef.current = true
            setOpen((value) => !value)
          }}
        >
          <span className={clsx(styles.titleIcon, stopped && styles.stoppedIcon)}>
            <IconThinkOutline16 />
          </span>
          <span className={clsx(styles.titleText, titleActive && styles.shimmer)}>{title}</span>
          {open ? <IconChevronDownOutline14 /> : <IconChevronRightOutline14 />}
        </div>
      ) : null}
      {open || !hasCollapsibleProcess ? (
        <div
          ref={bodyRef}
          className={clsx(
            hasCollapsibleProcess && styles.steps,
            hasCollapsibleProcess && edges.canScrollUp && styles.fadeTop,
            hasCollapsibleProcess && edges.canScrollDown && styles.fadeBottom
          )}
          onScroll={hasCollapsibleProcess ? handleScroll : undefined}
          onWheel={hasCollapsibleProcess ? interruptFollow : undefined}
          onTouchStart={hasCollapsibleProcess ? interruptFollow : undefined}
        >
          <div ref={contentRef} className={styles.stepsContent}>
            {view.steps.map((step, index) => (
              <StepItem
                key={stepKey(step, index)}
                step={step}
                message={message}
                sources={view.sources}
                streaming={streaming}
                isLast={index === view.steps.length - 1}
                defaultExpandThinking={singleThinkingOnly}
                onExpandStep={pauseFollow}
              />
            ))}
          </div>
        </div>
      ) : null}
    </div>
  )
})

const stepKey = (step: Step, index: number) =>
  step.type === 'thinking' ||
  step.type === 'text' ||
  step.type === 'search' ||
  step.type === 'client_tool'
    ? step.key
    : `${step.type}-${index}`

const StepItem = ({
  step,
  message,
  sources,
  streaming,
  isLast,
  defaultExpandThinking,
  onExpandStep,
}: {
  step: Step
  message: AssistantMessage
  sources: Source[]
  streaming: boolean
  isLast: boolean
  defaultExpandThinking: boolean
  onExpandStep: () => void
}) => {
  switch (step.type) {
    case 'thinking':
      return (
        <ThinkingRow
          step={step}
          messageId={message.id}
          streaming={streaming}
          isLast={isLast}
          defaultExpanded={defaultExpandThinking}
          onExpand={onExpandStep}
        />
      )
    case 'text':
      return (
        <ContentItem isLast={isLast} active={false}>
          <Markdown
            content={step.markdown}
            loading={streaming && isLast}
            cacheKey={`${message.id}-${step.key}`}
            disableLink={false}
            sources={sources}
          />
        </ContentItem>
      )
    case 'search':
      return <SearchRow step={step} message={message} streaming={streaming} isLast={isLast} />
    case 'open':
      return <OpenRow step={step} sources={sources} isLast={isLast} />
    case 'find':
      return <FindRow step={step} isLast={isLast} />
    case 'read_link':
      return <ReadLinkRow step={step} isLast={isLast} />
    case 'client_tool':
      return <ToolCallCard step={step} isLast={isLast} onExpand={onExpandStep} />
    default:
      return null
  }
}

/** 单段思考折叠行：默认折叠为单行动态预览（流式取最近完成段落首行），点击展开完整 Markdown 且标题吸顶 */
const ThinkingRow = memo(function ThinkingRow({
  step,
  messageId,
  streaming,
  isLast,
  defaultExpanded,
  onExpand,
}: {
  step: ThinkingStep
  messageId: string
  streaming: boolean
  isLast: boolean
  defaultExpanded: boolean
  onExpand: () => void
}) {
  const t = useTranslate()
  const [expanded, setExpanded] = useState(defaultExpanded)
  const lastBlock = step.blocks.at(-1)
  const active = streaming && lastBlock?.startedAt !== undefined && lastBlock.endedAt === undefined
  const rawText = useMemo(() => step.blocks.map((block) => block.thinking).join(''), [step.blocks])
  const summary = useMemo(() => {
    const line = active ? latestCompletedParagraphFirstLine(rawText) : firstLine(rawText)
    return line.replaceAll('**', '')
  }, [active, rawText])

  return (
    <div
      className={clsx(
        styles.thinkRow,
        expanded && styles.thinkRowExpanded,
        isLast && styles.lastItem
      )}
    >
      <span className={styles.dot}>
        <span className={clsx(styles.dotInner, !active && styles.inactive)} />
      </span>
      <button
        type="button"
        className={styles.thinkHeader}
        aria-expanded={expanded}
        onClick={() => {
          if (!expanded) {
            onExpand()
          }
          setExpanded((value) => !value)
        }}
      >
        <span className={clsx(styles.thinkLabel, active && styles.shimmer)}>
          {t(active ? 'toolThinkingStepRunning' : 'toolThinkingStepLabel')}
        </span>
        {!expanded && summary ? (
          <>
            <span className={styles.thinkSeparator} aria-hidden />
            <span className={clsx(styles.thinkSummary, active && styles.thinkSummaryStreaming)}>
              <span className={clsx(styles.thinkSummaryText, active && styles.shimmer)}>
                {summary}
              </span>
            </span>
          </>
        ) : null}
        <span className={styles.thinkChevron}>
          {expanded ? <IconChevronDownOutline14 /> : <IconChevronRightOutline14 />}
        </span>
      </button>
      {expanded ? (
        <div className={clsx(styles.thinkBody, 'ds-think-content')}>
          <Markdown
            content={rawText}
            loading={active}
            cacheKey={`${messageId}-${step.key}`}
            disableLink
          />
        </div>
      ) : null}
    </div>
  )
})

/** 过程性文字：左侧圆点 + 竖线，小号 markdown */
const ContentItem = ({
  children,
  active,
  isLast,
}: {
  children: ReactNode
  active: boolean
  isLast: boolean
}) => (
  <div className={clsx(styles.content, 'ds-think-content', isLast && styles.lastItem)}>
    <span className={styles.dot}>
      <span className={clsx(styles.dotInner, !active && styles.inactive)} />
    </span>
    {children}
  </div>
)

/** 搜索：普通搜索为「已阅读 N 个网页」，边想边搜为「搜索到 N 个网页」，点击打开搜索结果 */
const SearchRow = ({
  step,
  message,
  streaming,
  isLast,
}: {
  step: SearchStep
  message: AssistantMessage
  streaming: boolean
  isLast: boolean
}) => {
  const t = useTranslate()
  const entry = useSearchPanel((state) => state.entry)
  const wip = step.status === 'wip'
  const tick = useTicker(wip && step.queries.length > 1, 2000)
  const count = step.results.length
  const clickable = !wip && count > 0
  const panelEntry = `${message.id}-search-${step.key}`
  const failed = step.status === 'failed'
  const interrupted = failed && !streaming && message.status === 'stopped'

  let body: ReactNode
  if (wip) {
    body =
      step.queries.length > 0 ? (
        <>
          <span className={styles.prefix}>{t('toolSearchIntro')}</span>
          <span className={styles.query} key={tick}>
            {step.queries[tick % step.queries.length]}
          </span>
        </>
      ) : (
        <span>{t(isImageSearchKind(step.kind) ? 'imageSearching' : 'messageSearching')}</span>
      )
  } else if (interrupted) {
    body = <span>{t('searchStopped')}</span>
  } else if (failed) {
    body = <span>{t('toolSearchFail')}</span>
  } else if (count === 0) {
    body = (
      <span>{t(isDeepSearchKind(step.kind) ? 'toolSearchNoResult' : 'messageSearchNoResult')}</span>
    )
  } else {
    const text = t(
      isDeepSearchKind(step.kind)
        ? count === 1
          ? 'toolSearch1Result'
          : 'toolSearchResults'
        : count === 1
          ? 'messageSearchResult'
          : 'messageSearchResults',
      { count }
    )
    body = (
      <>
        <span className={styles.summary}>{text}</span>
        <SiteThumbnails sources={step.results} />
      </>
    )
  }
  return (
    <div
      className={clsx(
        styles.row,
        !isLast && styles.withLine,
        clickable && styles.clickable,
        entry === panelEntry && styles.active
      )}
      onClick={clickable ? () => toggleSearchPanel(panelEntry, step.results) : undefined}
    >
      <span className={styles.rowIcon}>
        {failed ? <IconSearchWarningOutline16 /> : <IconSearchOutline16 />}
      </span>
      {body}
    </div>
  )
}

/** 工具行外壳：左侧图标，右侧可换行的内容 */
const ToolRow = ({
  icon,
  children,
  isLast,
}: {
  icon: ReactNode
  children: ReactNode
  isLast: boolean
}) => (
  <div className={clsx(styles.tool, !isLast && styles.withLine)}>
    <span className={clsx(styles.rowIcon, styles.toolIcon)}>{icon}</span>
    <div className={styles.toolBody}>{children}</div>
  </div>
)

const PageLink = ({ page, failedTip }: { page: PageItem; failedTip?: string }) => (
  <span
    className={clsx(styles.page, page.status === 'failed' ? styles.pageFailed : styles.pageDone)}
  >
    {page.status === 'failed' ? (
      <Tooltip content={failedTip}>
        <span className={styles.pageText}>{page.title}</span>
      </Tooltip>
    ) : (
      <a className={styles.pageText} href={page.url} target="_blank" rel="noreferrer">
        {page.title}
      </a>
    )}
    <span className={styles.pageIcon}>
      <IconRightUpOutline14 />
    </span>
  </span>
)

/** 边想边搜打开的网页：「浏览 N 个页面」+ 最多 5 个页面 */
const OpenRow = ({
  step,
  sources,
  isLast,
}: {
  step: OpenStep
  sources: Source[]
  isLast: boolean
}) => {
  const t = useTranslate()
  const allFailed = step.pages.every((page) => page.status === 'failed')
  const count = step.pages.length
  const intro = t(count === 1 ? 'toolOpenOneIntro' : 'toolOpensIntro', { count })
  const panelEntry = `open-${step.pages[0]?.id}`
  return (
    <ToolRow
      icon={allFailed ? <IconBrowseWarningOutline16 /> : <IconBrowseOutline16 />}
      isLast={isLast}
    >
      {allFailed ? (
        t('toolOpenFail')
      ) : (
        <>
          <span className={styles.intro}>{intro}</span>
          {step.pages.slice(0, 5).map((page) => (
            <PageLink key={page.id} page={page} failedTip={t('toolOpenFailTooltip')} />
          ))}
          {count > 5 ? (
            <span
              className={styles.chip}
              onClick={() =>
                toggleSearchPanel(
                  panelEntry,
                  sources.filter((source) => step.pages.some((page) => page.url === source.url))
                )
              }
            >
              {t('toolOpenViewAll')}
            </span>
          ) : null}
        </>
      )}
    </ToolRow>
  )
}

/** 边想边搜的页内查找：「查找关键词」+ 关键词标签 */
const FindRow = ({ step, isLast }: { step: FindStep; isLast: boolean }) => {
  const t = useTranslate()
  const allFailed = step.patterns.every((item) => item.status === 'failed')
  return (
    <ToolRow
      icon={allFailed ? <IconBrowseWarningOutline16 /> : <IconBrowseOutline16 />}
      isLast={isLast}
    >
      {allFailed ? (
        t('toolFindFail')
      ) : (
        <>
          <span className={styles.intro}>{t('toolFindIntro')}</span>
          {step.patterns.map((item) => (
            <Tooltip
              key={item.id}
              content={item.status === 'failed' ? t('toolFindFailTooltip') : null}
            >
              <span className={clsx(styles.chip, item.status === 'failed' && styles.chipFailed)}>
                {item.status === 'wip' ? null : (
                  <span className={styles.chipIcon}>
                    {item.status === 'failed' ? <IconCloseFill14 /> : <IconCheckedOutline14 />}
                  </span>
                )}
                <span className={styles.chipText}>{item.pattern}</span>
              </span>
            </Tooltip>
          ))}
        </>
      )}
    </ToolRow>
  )
}

/** 阅读用户消息中的链接：视图只保留仍在读取的链接。 */
const ReadLinkRow = ({ step, isLast }: { step: ReadLinkStep; isLast: boolean }) => {
  const t = useTranslate()
  return (
    <ToolRow icon={<IconLinkOutline16 />} isLast={isLast}>
      <span className={clsx(styles.intro, styles.shimmer)}>{t('readingLink')}</span>
      {step.links.map((link) => (
        <PageLink key={link.id} page={link} />
      ))}
    </ToolRow>
  )
}
