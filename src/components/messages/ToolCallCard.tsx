import clsx from 'clsx'
import { memo, useEffect, useState } from 'react'
import { useTranslate } from '../../i18n/index.js'
import { ToolService } from '../../mygo.js'
import { useChatStore } from '../../services/chat/store.js'
import type { ClientToolStep } from '../../services/chat/view.js'
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
  const hasDiffs = Boolean(step.result?.uiMeta?.diffs?.length)
  const [expanded, setExpanded] = useState(hasDiffs)
  const [killingJob, setKillingJob] = useState(false)
  const [killedLocally, setKilledLocally] = useState(false)

  useEffect(() => {
    if (hasDiffs) {
      setExpanded(true)
    }
  }, [hasDiffs])

  const summary = summarizeToolCall(step.use.name, step.use.input)
  const uiMeta = step.result?.uiMeta
  const diffs = uiMeta?.diffs ?? []
  const producedFiles = uiMeta?.producedFiles ?? []
  const exitCode = uiMeta?.exitCode
  const backgroundJobId = uiMeta?.backgroundJobId
  const effectiveJobStatus = killedLocally ? 'killed' : uiMeta?.jobStatus
  const sandboxed = Boolean(uiMeta?.sandboxed)
  const skillName = uiMeta?.skillName

  const statusLabel = pendingApproval
    ? t('toolApprovalStatusPending')
    : step.status === 'wip'
      ? t('toolStatusRunning')
      : step.status === 'failed'
        ? uiMeta?.approvalStatus === 'rejected'
          ? t('toolApprovalStatusRejected')
          : t('toolStatusFailed')
        : backgroundJobId && effectiveJobStatus === 'running'
          ? t('toolJobRunning', { id: backgroundJobId })
          : backgroundJobId && effectiveJobStatus === 'killed'
            ? t('toolJobKilled', { id: backgroundJobId })
            : backgroundJobId && effectiveJobStatus === 'completed'
              ? t('toolJobCompleted', { id: backgroundJobId })
              : typeof exitCode === 'number' && exitCode !== 0
                ? t('toolExitCode', { code: exitCode })
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
      showToast(t('toolJobStoppedToast', { id: backgroundJobId }))
    } finally {
      setKillingJob(false)
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
              styles.dotDone,
            !pendingApproval && step.status === 'failed' && styles.dotError
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
              (step.status === 'failed' || (typeof exitCode === 'number' && exitCode !== 0)) &&
                styles.statusError
            )}
          >
            {statusLabel}
          </span>
          <span className={styles.chevron}>
            {expanded ? <IconChevronDownOutline14 /> : <IconChevronRightOutline14 />}
          </span>
        </button>

        {backgroundJobId && effectiveJobStatus === 'running' ? (
          <div className={styles.jobBar}>
            <span className={styles.jobBarText}>
              {t('toolJobRunning', { id: backgroundJobId })}
            </span>
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
        ) : null}

        {expanded ? (
          <div className={styles.body}>
            <div className={styles.sectionLabel}>{t('toolInputLabel')}</div>
            <pre className={styles.pre}>{inputPreview}</pre>

            {diffs.map((diff, index) => (
              <DiffView key={`${diff.path}-${index}`} diff={diff} />
            ))}

            <ProducedFilesBar files={producedFiles} compact />

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
