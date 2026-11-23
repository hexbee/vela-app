import clsx from 'clsx'
import { memo, useEffect, useRef, useState } from 'react'
import { useTranslate } from '../../i18n/index.js'
import { ToolService } from '../../mygo.js'
import { useChatStore } from '../../services/chat/store.js'
import type { ClientToolStep } from '../../services/chat/view.js'
import { markJobKilledLocally, useJobStore } from '../../services/jobs/store.js'
import { IconChevronDownOutline14, IconChevronRightOutline14 } from '../base/icons.js'
import { showToast } from '../base/toastState.js'
import { ApprovalCard } from './ApprovalCard.js'
import { DiffView } from './DiffView.js'
import { ProducedFilesBar } from './ProducedFilesBar.js'
import styles from './ToolCallCard.module.css'

const summarizeToolCall = (name: string, input: Record<string, unknown>): string => {
  const str = (key: string) => (typeof input[key] === 'string' ? input[key] : '')
  switch (name) {
    case 'read':
    case 'write':
    case 'edit':
    case 'delete_file':
      return str('file_path')
    case 'glob':
      return str('path') ? `${str('pattern')} (${str('path')})` : str('pattern')
    case 'grep':
      return str('path') ? `${str('pattern')} in ${str('path')}` : str('pattern')
    case 'bash':
      return str('description') || str('command')
    case 'job_output':
    case 'job_kill':
      return str('job_id')
    case 'job_list':
      return ''
    case 'skill':
      return str('name')
    default:
      if (name.startsWith('mcp__') && name.endsWith('__load_tools')) {
        const tools = Array.isArray(input.tools)
          ? input.tools.filter((v): v is string => typeof v === 'string')
          : []
        return tools.join(', ')
      }
      return ''
  }
}

const formatLiveOutput = (stdout: string, stderr: string): string => {
  const out = stdout.trimEnd()
  const err = stderr.trimEnd()
  if (out && err) {
    return `${out}\n[stderr]\n${err}`
  }
  if (err) {
    return `[stderr]\n${err}`
  }
  return out
}

export const ToolCallCard = memo(function ToolCallCard({
  step,
  isLast,
  onExpand,
}: {
  step: ClientToolStep
  isLast: boolean
  onExpand?: () => void
}) {
  const t = useTranslate()
  const pendingApproval = useChatStore((state) =>
    Object.hasOwn(state.pendingApprovals, step.use.id)
      ? state.pendingApprovals[step.use.id]
      : undefined
  )
  const uiMeta = step.result?.uiMeta
  const isBashTool = step.use.name === 'bash'
  const liveJob = useJobStore((state) => {
    if (!isBashTool) {
      return undefined
    }
    const mappedId = Object.hasOwn(state.byToolUseId, step.use.id)
      ? state.byToolUseId[step.use.id]
      : undefined
    const targetId = uiMeta?.backgroundJobId ?? mappedId
    if (!targetId || !Object.hasOwn(state.byJobId, targetId)) {
      return undefined
    }
    return state.byJobId[targetId]
  })

  const hasDiffs = Boolean(uiMeta?.diffs?.length)
  const [expanded, setExpanded] = useState(hasDiffs)
  const [killingJob, setKillingJob] = useState(false)
  const [promotingJob, setPromotingJob] = useState(false)
  const [killedLocally, setKilledLocally] = useState(false)
  const livePreviewRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    if (hasDiffs) {
      setExpanded(true)
    }
  }, [hasDiffs])

  const summary = summarizeToolCall(step.use.name, step.use.input)
  const diffs = uiMeta?.diffs ?? []
  const producedFiles = uiMeta?.producedFiles ?? liveJob?.producedFiles ?? []
  // 仅当结果元数据中携带 backgroundJobId 时，才表示该命令真正进入过后台；
  // 普通前台命令执行期间也会通过 liveJob.jobId 推送实时流，但结束后不应显示为「后台已完成」。
  const backgroundJobId = isBashTool ? uiMeta?.backgroundJobId : undefined
  const liveJobId = liveJob?.jobId
  const isBackgroundJob = isBashTool && Boolean(backgroundJobId)
  const effectiveJobStatus = killedLocally
    ? 'killed'
    : isBackgroundJob
      ? (liveJob?.status ?? uiMeta?.jobStatus)
      : step.status === 'wip'
        ? liveJob?.status
        : undefined
  const effectiveExitCode = isBackgroundJob
    ? (liveJob?.exitCode ?? uiMeta?.exitCode)
    : uiMeta?.exitCode
  const sandboxed = Boolean(uiMeta?.sandboxed)
  const skillName = uiMeta?.skillName

  const isLiveForegroundBash =
    step.use.name === 'bash' &&
    step.status === 'wip' &&
    !pendingApproval &&
    Boolean(liveJobId) &&
    liveJob?.status === 'running'

  const isRunningBackgroundJob =
    step.use.name === 'bash' &&
    step.status !== 'wip' &&
    isBackgroundJob &&
    effectiveJobStatus === 'running'

  const liveOutput = liveJob ? formatLiveOutput(liveJob.stdout, liveJob.stderr) : ''

  useEffect(() => {
    if (livePreviewRef.current && liveOutput) {
      livePreviewRef.current.scrollTop = livePreviewRef.current.scrollHeight
    }
  }, [liveOutput])

  const statusLabel = pendingApproval
    ? t('toolApprovalStatusPending')
    : step.status === 'wip'
      ? isLiveForegroundBash && liveJobId
        ? t('toolJobLiveRunning', { id: liveJobId })
        : t('toolStatusRunning')
      : step.status === 'failed'
        ? uiMeta?.approvalStatus === 'rejected'
          ? t('toolApprovalStatusRejected')
          : t('toolStatusFailed')
        : backgroundJobId && effectiveJobStatus === 'running'
          ? t('toolJobRunning', { id: backgroundJobId })
          : backgroundJobId && effectiveJobStatus === 'killed'
            ? t('toolJobKilled', { id: backgroundJobId })
            : backgroundJobId && effectiveJobStatus === 'completed'
              ? typeof effectiveExitCode === 'number' && effectiveExitCode !== 0
                ? t('toolExitCode', { code: effectiveExitCode })
                : t('toolJobCompleted', { id: backgroundJobId })
              : typeof effectiveExitCode === 'number' && effectiveExitCode !== 0
                ? t('toolExitCode', { code: effectiveExitCode })
                : t('toolStatusDone')

  const inputPreview = step.use.parseError
    ? step.use.partialJson || step.use.parseError
    : JSON.stringify(step.use.input, null, 2)

  const handleKillJob = async () => {
    if (!backgroundJobId || killingJob) {
      return
    }
    setKillingJob(true)
    try {
      await ToolService.killBackgroundJob(backgroundJobId)
      setKilledLocally(true)
      markJobKilledLocally(backgroundJobId)
      showToast(t('toolJobStoppedToast', { id: backgroundJobId }))
    } catch {
      // 任务可能已在点击瞬间结束，忽略竞态错误
    } finally {
      setKillingJob(false)
    }
  }

  const handlePromoteJob = async () => {
    const target = liveJobId || step.use.id
    if (!target || promotingJob) {
      return
    }
    setPromotingJob(true)
    try {
      const info = await ToolService.promoteBackgroundJob(target)
      if (info.status === 'running') {
        showToast(t('toolJobMovedToBackgroundToast', { id: info.id || target }))
      }
    } catch {
      // 命令可能已在点击瞬间结束，忽略竞态错误
    } finally {
      setPromotingJob(false)
    }
  }

  return (
    <div className={clsx(styles.wrapper, !isLast && styles.withLine)}>
      <span className={styles.statusDot}>
        <span
          className={clsx(
            styles.dot,
            (step.status === 'wip' || pendingApproval || effectiveJobStatus === 'running') &&
              styles.dotRunning,
            !pendingApproval &&
              effectiveJobStatus !== 'running' &&
              step.status === 'done' &&
              !(typeof effectiveExitCode === 'number' && effectiveExitCode !== 0) &&
              styles.dotDone,
            !pendingApproval &&
              (step.status === 'failed' ||
                (effectiveJobStatus !== 'running' &&
                  typeof effectiveExitCode === 'number' &&
                  effectiveExitCode !== 0)) &&
              styles.dotError
          )}
        />
      </span>
      <div className={styles.card}>
        <button
          type="button"
          className={styles.header}
          onClick={() => {
            if (!expanded) {
              onExpand?.()
            }
            setExpanded((value) => !value)
          }}
        >
          <span className={styles.toolName}>{step.use.name}</span>
          {sandboxed ? <span className={styles.metaBadge}>{t('toolSandboxedBadge')}</span> : null}
          {skillName ? <span className={styles.metaBadge}>{t('skillLoadedBadge')}</span> : null}
          {uiMeta?.mcpServer || step.use.name.startsWith('mcp__') ? (
            <span className={styles.metaBadge}>
              {uiMeta?.mcpServer ? `MCP · ${uiMeta.mcpServer}` : 'MCP'}
            </span>
          ) : null}
          {summary ? (
            <span className={styles.summary} title={summary}>
              {summary}
            </span>
          ) : null}
          <span
            className={clsx(
              styles.statusText,
              (step.status === 'failed' ||
                (effectiveJobStatus !== 'running' &&
                  typeof effectiveExitCode === 'number' &&
                  effectiveExitCode !== 0)) &&
                styles.statusError
            )}
          >
            {statusLabel}
          </span>
          <span className={styles.chevron}>
            {expanded ? <IconChevronDownOutline14 /> : <IconChevronRightOutline14 />}
          </span>
        </button>

        {isLiveForegroundBash && liveJobId ? (
          <div className={styles.jobBar}>
            <span className={styles.jobBarText}>{t('toolJobLiveRunning', { id: liveJobId })}</span>
            <div className={styles.jobActions}>
              <button
                type="button"
                className={styles.jobPromoteBtn}
                disabled={promotingJob}
                onClick={() => {
                  void handlePromoteJob()
                }}
              >
                {t('toolJobMoveToBackground')}
              </button>
            </div>
          </div>
        ) : null}

        {isRunningBackgroundJob && backgroundJobId ? (
          <div className={styles.jobBar}>
            <span className={styles.jobBarText}>
              {t('toolJobRunning', { id: backgroundJobId })}
            </span>
            <div className={styles.jobActions}>
              <button
                type="button"
                className={styles.jobStopBtn}
                disabled={killingJob}
                onClick={() => {
                  void handleKillJob()
                }}
              >
                {t('toolJobStop')}
              </button>
            </div>
          </div>
        ) : null}

        {!expanded && isLiveForegroundBash && liveOutput ? (
          <div className={styles.livePreview}>
            <pre ref={livePreviewRef} className={styles.livePre}>
              {liveOutput}
            </pre>
          </div>
        ) : null}

        {expanded ? (
          <div className={styles.body}>
            <div className={styles.sectionLabel}>{t('toolInputLabel')}</div>
            <pre className={styles.pre}>{inputPreview}</pre>

            {diffs.map((diff, index) => (
              <DiffView key={`${diff.path}-${index}`} diff={diff} />
            ))}

            <ProducedFilesBar files={producedFiles} compact />

            {liveOutput && (step.status === 'wip' || isBackgroundJob) ? (
              <>
                <div className={styles.sectionLabel}>{t('toolJobLiveOutputLabel')}</div>
                <pre ref={livePreviewRef} className={styles.pre}>
                  {liveOutput}
                </pre>
              </>
            ) : null}

            {step.result ? (
              <>
                <div className={styles.sectionLabel}>{t('toolOutputLabel')}</div>
                <pre className={clsx(styles.pre, step.result.isError && styles.preError)}>
                  {step.result.content}
                </pre>
              </>
            ) : null}
          </div>
        ) : null}
      </div>
      {pendingApproval ? <ApprovalCard approval={pendingApproval} /> : null}
    </div>
  )
})
