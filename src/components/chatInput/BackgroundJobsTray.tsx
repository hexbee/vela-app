import clsx from 'clsx'
import { memo, useEffect, useMemo, useState } from 'react'
import { useTranslate } from '../../i18n/index.js'
import { ToolService } from '../../mygo.js'
import { sendQuickMessage } from '../../services/chat/store.js'
import {
  dismissBackgroundJob,
  markJobKilledLocally,
  syncSessionBackgroundJobs,
  useJobStore,
  type LiveJobState,
} from '../../services/jobs/store.js'
import { IconChevronDownOutline14, IconChevronRightOutline14 } from '../base/icons.js'
import { showToast } from '../base/toastState.js'
import { ensureApiKey } from '../settings/settingsDialogState.js'
import styles from './BackgroundJobsTray.module.css'

const formatJobPreview = (job: LiveJobState): string => {
  const out = job.stdout.trimEnd()
  const err = job.stderr.trimEnd()
  if (out && err) {
    return `${out}\n[stderr]\n${err}`
  }
  if (err) {
    return `[stderr]\n${err}`
  }
  if (out) {
    return out
  }
  return job.outputTail?.trimEnd() ?? ''
}

export const BackgroundJobsTray = memo(function BackgroundJobsTray({
  sessionId,
  streaming,
}: {
  sessionId: string
  streaming: boolean
}) {
  const t = useTranslate()
  const byJobId = useJobStore((state) => state.byJobId)
  const [collapsed, setCollapsed] = useState(false)
  const [expandedJobId, setExpandedJobId] = useState<string | null>(null)
  const [killingIds, setKillingIds] = useState<Record<string, boolean>>({})

  useEffect(() => {
    void syncSessionBackgroundJobs(sessionId)
  }, [sessionId])

  const jobs = useMemo(() => {
    const matched: LiveJobState[] = []
    for (const job of Object.values(byJobId)) {
      if (job.sessionId === sessionId && job.isBackground && !job.dismissed) {
        matched.push(job)
      }
    }
    matched.sort((a, b) => {
      if (a.status === 'running' && b.status !== 'running') {
        return -1
      }
      if (a.status !== 'running' && b.status === 'running') {
        return 1
      }
      return b.updatedAt - a.updatedAt
    })
    return matched
  }, [byJobId, sessionId])

  if (jobs.length === 0) {
    return null
  }

  const runningJobs = jobs.filter((job) => job.status === 'running')
  const failedJobs = jobs.filter(
    (job) => job.status === 'completed' && typeof job.exitCode === 'number' && job.exitCode !== 0
  )
  const succeededJobs = jobs.filter(
    (job) => job.status === 'completed' && !(typeof job.exitCode === 'number' && job.exitCode !== 0)
  )
  const killedJobs = jobs.filter((job) => job.status === 'killed')
  const hasFailed = failedJobs.length > 0

  const summaryParts: string[] = []
  if (runningJobs.length > 0) {
    summaryParts.push(t('jobTrayPartRunning', { count: runningJobs.length }))
  }
  if (failedJobs.length > 0) {
    summaryParts.push(t('jobTrayPartFailed', { count: failedJobs.length }))
  }
  if (succeededJobs.length > 0) {
    summaryParts.push(t('jobTrayPartCompleted', { count: succeededJobs.length }))
  }
  if (killedJobs.length > 0) {
    summaryParts.push(t('jobTrayPartStopped', { count: killedJobs.length }))
  }

  const summaryText =
    summaryParts.length > 1
      ? summaryParts.join(' · ')
      : runningJobs.length > 0
        ? t('jobTraySummaryRunning', { count: runningJobs.length })
        : failedJobs.length > 0
          ? t('jobTraySummaryFailed', { count: failedJobs.length })
          : succeededJobs.length > 0
            ? t('jobTraySummaryCompleted', { count: succeededJobs.length })
            : t('jobTraySummaryStopped', { count: killedJobs.length })

  const handleKill = async (jobId: string) => {
    if (killingIds[jobId]) {
      return
    }
    setKillingIds((prev) => ({ ...prev, [jobId]: true }))
    try {
      await ToolService.killBackgroundJob(jobId)
      markJobKilledLocally(jobId)
      showToast(t('toolJobStoppedToast', { id: jobId }))
    } catch {
      void syncSessionBackgroundJobs(sessionId)
    } finally {
      setKillingIds((prev) =>
        Object.fromEntries(Object.entries(prev).filter(([id]) => id !== jobId))
      )
    }
  }

  const handleDiagnoseJob = (job: LiveJobState) => {
    if (streaming || !ensureApiKey()) {
      return
    }
    const prompt = t('jobTrayDiagnosePrompt', {
      id: job.jobId,
      label: job.description || job.command || job.jobId,
      code: job.exitCode ?? 1,
    })
    if (sendQuickMessage(sessionId, prompt)) {
      dismissBackgroundJob(job.jobId)
    }
  }

  return (
    <div className={styles.tray} role="region" aria-label={t('jobTrayAriaLabel')}>
      <div className={styles.header}>
        <button type="button" className={styles.summaryBtn} onClick={() => setCollapsed((v) => !v)}>
          <span
            className={clsx(
              styles.dot,
              runningJobs.length > 0
                ? styles.dotRunning
                : hasFailed
                  ? styles.dotError
                  : succeededJobs.length > 0
                    ? styles.dotDone
                    : styles.dotMuted
            )}
          />
          <span className={styles.summaryText}>{summaryText}</span>
          <span className={styles.chevron}>
            {collapsed ? <IconChevronRightOutline14 /> : <IconChevronDownOutline14 />}
          </span>
        </button>
      </div>

      {!collapsed ? (
        <div className={styles.list}>
          {jobs.map((job) => {
            const label = job.description?.trim() || job.command?.trim() || job.jobId
            const isFailed =
              job.status === 'completed' && typeof job.exitCode === 'number' && job.exitCode !== 0
            const isExpanded = expandedJobId === job.jobId
            const preview = isExpanded ? formatJobPreview(job) : ''
            const statusText =
              job.status === 'running'
                ? t('jobTrayStatusRunning')
                : job.status === 'killed'
                  ? t('jobTrayStatusKilled')
                  : isFailed
                    ? t('toolExitCode', { code: job.exitCode ?? 1 })
                    : t('jobTrayStatusCompleted')

            return (
              <div key={job.jobId} className={styles.rowWrap}>
                <div className={styles.row}>
                  <button
                    type="button"
                    className={styles.rowMain}
                    title={job.command || label}
                    onClick={() =>
                      setExpandedJobId((prev) => (prev === job.jobId ? null : job.jobId))
                    }
                  >
                    <span
                      className={clsx(
                        styles.dot,
                        job.status === 'running'
                          ? styles.dotRunning
                          : isFailed
                            ? styles.dotError
                            : job.status === 'completed'
                              ? styles.dotDone
                              : styles.dotMuted
                      )}
                    />
                    <span className={styles.jobId}>{job.jobId}</span>
                    <span className={styles.jobLabel}>{label}</span>
                    <span className={clsx(styles.jobStatus, isFailed && styles.jobStatusError)}>
                      {statusText}
                    </span>
                  </button>

                  <div className={styles.rowActions}>
                    {job.status === 'running' ? (
                      <button
                        type="button"
                        className={styles.stopBtn}
                        disabled={Boolean(killingIds[job.jobId])}
                        onClick={() => {
                          void handleKill(job.jobId)
                        }}
                      >
                        {t('jobTrayStopShort')}
                      </button>
                    ) : (
                      <>
                        {isFailed ? (
                          <button
                            type="button"
                            className={styles.diagnoseBtn}
                            disabled={streaming}
                            onClick={() => handleDiagnoseJob(job)}
                          >
                            {t('jobTrayDiagnose')}
                          </button>
                        ) : null}
                        <button
                          type="button"
                          className={styles.dismissBtn}
                          aria-label={t('close')}
                          title={t('close')}
                          onClick={() => dismissBackgroundJob(job.jobId)}
                        >
                          ×
                        </button>
                      </>
                    )}
                  </div>
                </div>

                {isExpanded ? (
                  <pre className={styles.preview}>{preview || t('jobTrayNoOutput')}</pre>
                ) : null}
              </div>
            )
          })}
        </div>
      ) : null}
    </div>
  )
})
