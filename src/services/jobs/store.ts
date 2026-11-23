import { create } from 'zustand'
import {
  events,
  ToolService,
  type BackgroundJobInfo,
  type JobOutputDelta,
  type JobSettledNotice,
} from '../../mygo.js'

const MAX_LIVE_STREAM_CHARS = 64 * 1024

export type LiveJobStatus = 'running' | 'completed' | 'killed'

export type LiveJobState = {
  jobId: string
  sessionId: string
  toolUseId?: string
  command?: string
  description?: string
  /** 仅真正转入后台（run_in_background / 超时 / 手动转后台）的任务为 true，供输入框任务托盘展示 */
  isBackground?: boolean
  stdout: string
  stderr: string
  outputTail?: string
  status: LiveJobStatus
  exitCode?: number
  producedFiles?: string[]
  startedAt?: number
  updatedAt: number
  /** 用户在托盘中手动关闭已结束任务时置为 true */
  dismissed?: boolean
}

type JobStoreState = {
  byJobId: Record<string, LiveJobState>
  byToolUseId: Record<string, string>
  settledNoticesBySession: Record<string, JobSettledNotice[]>
}

const normalizeJobStatus = (status: string): LiveJobStatus => {
  if (status === 'completed' || status === 'killed') {
    return status
  }
  return 'running'
}

const appendBoundedTail = (prev: string, delta: string | undefined): string => {
  if (!delta) {
    return prev
  }
  const combined = prev + delta
  if (combined.length <= MAX_LIVE_STREAM_CHARS) {
    return combined
  }
  return combined.slice(combined.length - MAX_LIVE_STREAM_CHARS)
}

export const useJobStore = create<JobStoreState>(() => ({
  byJobId: {},
  byToolUseId: {},
  settledNoticesBySession: {},
}))

const MAX_CLEANED_FOREGROUND_IDS = 200

let initialized = false
const settledListeners = new Set<(notice: JobSettledNotice) => void>()
/** 记录已清理的普通前台 bash toolUseId，忽略其后续迟到的异步流事件 */
const cleanedForegroundToolUseIds = new Set<string>()

const rememberCleanedForegroundToolUseId = (toolUseId: string) => {
  cleanedForegroundToolUseIds.add(toolUseId)
  if (cleanedForegroundToolUseIds.size > MAX_CLEANED_FOREGROUND_IDS) {
    const oldest = cleanedForegroundToolUseIds.values().next().value
    if (oldest !== undefined) {
      cleanedForegroundToolUseIds.delete(oldest)
    }
  }
}

/** 订阅后台任务完成事件（用于全局 Toast 或系统通知）。 */
export const onJobSettled = (listener: (notice: JobSettledNotice) => void) => {
  settledListeners.add(listener)
  return () => {
    settledListeners.delete(listener)
  }
}

const handleJobOutput = (delta: JobOutputDelta) => {
  if (!delta.jobId) {
    return
  }
  if (delta.toolUseId && cleanedForegroundToolUseIds.has(delta.toolUseId)) {
    return
  }
  useJobStore.setState((state) => {
    const existing = Object.hasOwn(state.byJobId, delta.jobId)
      ? state.byJobId[delta.jobId]
      : undefined
    const status = normalizeJobStatus(delta.status)
    // 忽略普通前台命令结束后晚于 cleanupForegroundJob 到达的终态流事件，防止僵尸缓存回写；
    // 真正转入后台的任务已有 registerBackgroundJobMeta 与 job:settled 兜底。
    if (!existing && status !== 'running') {
      return state
    }
    const nextJob: LiveJobState = {
      jobId: delta.jobId,
      sessionId: delta.sessionId || existing?.sessionId || '',
      toolUseId: delta.toolUseId || existing?.toolUseId,
      command: existing?.command,
      description: existing?.description,
      isBackground: existing?.isBackground,
      stdout: appendBoundedTail(existing?.stdout ?? '', delta.stdout),
      stderr: appendBoundedTail(existing?.stderr ?? '', delta.stderr),
      outputTail: existing?.outputTail,
      status,
      exitCode: existing?.exitCode,
      producedFiles: existing?.producedFiles,
      startedAt: existing?.startedAt ?? Date.now(),
      updatedAt: Date.now(),
      dismissed: existing?.dismissed,
    }
    const nextByToolUseId =
      nextJob.toolUseId && state.byToolUseId[nextJob.toolUseId] !== delta.jobId
        ? { ...state.byToolUseId, [nextJob.toolUseId]: delta.jobId }
        : state.byToolUseId
    return {
      byJobId: { ...state.byJobId, [delta.jobId]: nextJob },
      byToolUseId: nextByToolUseId,
    }
  })
}

const handleJobSettled = (notice: JobSettledNotice) => {
  if (!notice.jobId) {
    return
  }
  useJobStore.setState((state) => {
    const existing = Object.hasOwn(state.byJobId, notice.jobId)
      ? state.byJobId[notice.jobId]
      : undefined
    const nextJob: LiveJobState = {
      jobId: notice.jobId,
      sessionId: notice.sessionId || existing?.sessionId || '',
      toolUseId: notice.toolUseId || existing?.toolUseId,
      command: notice.command || existing?.command,
      description: notice.description || existing?.description,
      isBackground: true,
      stdout: existing?.stdout ?? '',
      stderr: existing?.stderr ?? '',
      outputTail: notice.outputTail || existing?.outputTail,
      status: normalizeJobStatus(notice.status),
      exitCode: notice.exitCode,
      producedFiles: notice.producedFiles ?? existing?.producedFiles,
      startedAt: existing?.startedAt,
      updatedAt: notice.endedAt || Date.now(),
      dismissed: false,
    }
    const nextByToolUseId =
      nextJob.toolUseId && state.byToolUseId[nextJob.toolUseId] !== notice.jobId
        ? { ...state.byToolUseId, [nextJob.toolUseId]: notice.jobId }
        : state.byToolUseId

    const sessionId = nextJob.sessionId
    const existingNotices =
      sessionId && Object.hasOwn(state.settledNoticesBySession, sessionId)
        ? state.settledNoticesBySession[sessionId]
        : []
    const nextSettled = sessionId
      ? {
          ...state.settledNoticesBySession,
          [sessionId]: [...existingNotices.filter((item) => item.jobId !== notice.jobId), notice],
        }
      : state.settledNoticesBySession

    return {
      byJobId: { ...state.byJobId, [notice.jobId]: nextJob },
      byToolUseId: nextByToolUseId,
      settledNoticesBySession: nextSettled,
    }
  })

  for (const listener of settledListeners) {
    listener(notice)
  }
}

/** 订阅 Go 后端推送的 bash 实时输出流与后台任务完成事件（幂等初始化）。 */
export const initJobEvents = () => {
  if (initialized) {
    return
  }
  initialized = true
  try {
    events.jobOutput.on(handleJobOutput)
    events.jobSettled.on(handleJobSettled)
  } catch {
    // 非 MyGo 运行时（例如纯浏览器预览）下忽略事件绑定错误
  }
}

/** 当 bash 工具返回 backgroundJobId 时登记后台任务元信息，便于输入框任务托盘实时展示。 */
export const registerBackgroundJobMeta = (params: {
  jobId: string
  sessionId: string
  toolUseId?: string
  command?: string
  description?: string
  status?: LiveJobStatus
  exitCode?: number
}) => {
  if (!params.jobId) {
    return
  }
  useJobStore.setState((state) => {
    const existing = Object.hasOwn(state.byJobId, params.jobId)
      ? state.byJobId[params.jobId]
      : undefined
    const status =
      existing && existing.status !== 'running'
        ? existing.status
        : (params.status ?? existing?.status ?? 'running')
    const nextJob: LiveJobState = {
      jobId: params.jobId,
      sessionId: params.sessionId || existing?.sessionId || '',
      toolUseId: params.toolUseId || existing?.toolUseId,
      command: params.command || existing?.command,
      description: params.description || existing?.description,
      isBackground: true,
      stdout: existing?.stdout ?? '',
      stderr: existing?.stderr ?? '',
      outputTail: existing?.outputTail,
      status,
      exitCode: existing?.exitCode ?? params.exitCode,
      producedFiles: existing?.producedFiles,
      startedAt: existing?.startedAt ?? Date.now(),
      updatedAt: Date.now(),
      dismissed: false,
    }
    const nextByToolUseId =
      nextJob.toolUseId && state.byToolUseId[nextJob.toolUseId] !== params.jobId
        ? { ...state.byToolUseId, [nextJob.toolUseId]: params.jobId }
        : state.byToolUseId
    return {
      byJobId: { ...state.byJobId, [params.jobId]: nextJob },
      byToolUseId: nextByToolUseId,
    }
  })
}

/** 普通前台 bash 结束后清理仅用于实时流的临时内存缓冲，避免长会话内存持续增长。 */
export const cleanupForegroundJob = (toolUseId: string) => {
  if (!toolUseId) {
    return
  }
  rememberCleanedForegroundToolUseId(toolUseId)
  useJobStore.setState((state) => {
    const jobId = Object.hasOwn(state.byToolUseId, toolUseId)
      ? state.byToolUseId[toolUseId]
      : undefined
    if (!jobId) {
      return state
    }
    const job = Object.hasOwn(state.byJobId, jobId) ? state.byJobId[jobId] : undefined
    if (job?.isBackground) {
      return state
    }
    return {
      byJobId: Object.fromEntries(Object.entries(state.byJobId).filter(([id]) => id !== jobId)),
      byToolUseId: Object.fromEntries(
        Object.entries(state.byToolUseId).filter(([id]) => id !== toolUseId)
      ),
    }
  })
}

/** 从 Go 后端同步当前会话的后台任务快照（用于切换会话、刷新或后端状态变更后对账）。 */
export const syncSessionBackgroundJobs = async (sessionId: string) => {
  if (!sessionId) {
    return
  }
  try {
    const list: BackgroundJobInfo[] = await ToolService.listBackgroundJobs(sessionId)
    if (!Array.isArray(list)) {
      return
    }
    const liveIds = new Set(list.map((info) => info.id))
    useJobStore.setState((state) => {
      const removedIds = new Set<string>()
      for (const [id, job] of Object.entries(state.byJobId)) {
        if (job.sessionId === sessionId && job.isBackground && !liveIds.has(id)) {
          removedIds.add(id)
        }
      }
      let changed = removedIds.size > 0
      const nextByJobId: Record<string, LiveJobState> = changed
        ? Object.fromEntries(Object.entries(state.byJobId).filter(([id]) => !removedIds.has(id)))
        : { ...state.byJobId }
      const nextByToolUseId: Record<string, string> = changed
        ? Object.fromEntries(
            Object.entries(state.byToolUseId).filter(([, jobId]) => !removedIds.has(jobId))
          )
        : { ...state.byToolUseId }
      for (const info of list) {
        const existing = Object.hasOwn(nextByJobId, info.id) ? nextByJobId[info.id] : undefined
        const status = normalizeJobStatus(info.status)
        if (
          existing &&
          existing.isBackground &&
          existing.status === status &&
          existing.command === info.command &&
          existing.description === info.description
        ) {
          continue
        }
        changed = true
        nextByJobId[info.id] = {
          jobId: info.id,
          sessionId: info.sessionId || sessionId,
          toolUseId: info.toolUseId || existing?.toolUseId,
          command: info.command || existing?.command,
          description: info.description || existing?.description,
          isBackground: true,
          stdout: existing?.stdout ?? '',
          stderr: existing?.stderr ?? '',
          outputTail: existing?.outputTail,
          status,
          exitCode: status === 'running' ? existing?.exitCode : info.exitCode,
          producedFiles: existing?.producedFiles,
          startedAt: info.startedAt || existing?.startedAt,
          updatedAt: info.endedAt || existing?.updatedAt || info.startedAt || Date.now(),
          dismissed: existing?.dismissed,
        }
        if (info.toolUseId) {
          nextByToolUseId[info.toolUseId] = info.id
        }
      }
      return changed ? { byJobId: nextByJobId, byToolUseId: nextByToolUseId } : state
    })
  } catch {
    // 非 MyGo 运行时或查询失败时静默忽略
  }
}

/** 标记指定后台任务已被用户手动停止，立即同步 UI 状态。 */
export const markJobKilledLocally = (jobId: string) => {
  if (!jobId) {
    return
  }
  useJobStore.setState((state) => {
    const existing = Object.hasOwn(state.byJobId, jobId) ? state.byJobId[jobId] : undefined
    const nextJob: LiveJobState = existing
      ? { ...existing, status: 'killed', updatedAt: Date.now() }
      : {
          jobId,
          sessionId: '',
          isBackground: true,
          stdout: '',
          stderr: '',
          status: 'killed',
          updatedAt: Date.now(),
        }
    return {
      byJobId: { ...state.byJobId, [jobId]: nextJob },
    }
  })
}

/** 从输入框任务托盘中隐藏单条已结束的后台任务。 */
export const dismissBackgroundJob = (jobId: string) => {
  if (!jobId) {
    return
  }
  useJobStore.setState((state) => {
    const existing = Object.hasOwn(state.byJobId, jobId) ? state.byJobId[jobId] : undefined
    if (!existing) {
      return state
    }
    return {
      byJobId: {
        ...state.byJobId,
        [jobId]: { ...existing, dismissed: true },
      },
    }
  })
}

/** 删除会话时清空该会话在前端缓存的所有后台任务状态与待消费通知。 */
export const clearSessionJobs = (sessionId: string) => {
  if (!sessionId) {
    return
  }
  useJobStore.setState((state) => {
    const removedJobIds = new Set<string>()
    for (const [id, job] of Object.entries(state.byJobId)) {
      if (job.sessionId === sessionId) {
        removedJobIds.add(id)
      }
    }
    const hasNotices = Object.hasOwn(state.settledNoticesBySession, sessionId)
    if (removedJobIds.size === 0 && !hasNotices) {
      return state
    }
    return {
      byJobId: Object.fromEntries(
        Object.entries(state.byJobId).filter(([id]) => !removedJobIds.has(id))
      ),
      byToolUseId: Object.fromEntries(
        Object.entries(state.byToolUseId).filter(([, jobId]) => !removedJobIds.has(jobId))
      ),
      settledNoticesBySession: Object.fromEntries(
        Object.entries(state.settledNoticesBySession).filter(([id]) => id !== sessionId)
      ),
    }
  })
}

/**
 * 预览指定会话待注入的已完成后台任务摘要（不立即删除，保证整轮多步请求
 * 系统提示词稳定以复用 Prompt Cache，且网络失败重试时不丢失通知）。
 */
export const peekSettledJobNoticesPrompt = (
  sessionId: string
): { prompt: string; jobIds: string[] } | undefined => {
  if (!sessionId) {
    return undefined
  }
  const state = useJobStore.getState()
  const notices = Object.hasOwn(state.settledNoticesBySession, sessionId)
    ? state.settledNoticesBySession[sessionId]
    : undefined
  if (!notices || notices.length === 0) {
    return undefined
  }

  const lines = notices.map((notice) => {
    const label = notice.description?.trim() || notice.command
    const tail = notice.outputTail?.trim()
    const header = `- ${notice.jobId} (${label}): status=${notice.status}, exitCode=${notice.exitCode}`
    return tail ? `${header}\n  output tail:\n  ${tail.replaceAll('\n', '\n  ')}` : header
  })
  return {
    prompt: `<system-reminder>\n以下后台命令已结束：\n${lines.join('\n')}\n</system-reminder>`,
    jobIds: notices.map((notice) => notice.jobId),
  }
}

/** 仅清除已成功送达模型的那批已完成任务通知，保留期间新到达的通知。 */
export const acknowledgeSettledJobNotices = (sessionId: string, jobIds: string[]) => {
  if (!sessionId || jobIds.length === 0) {
    return
  }
  const acked = new Set(jobIds)
  useJobStore.setState((prev) => {
    const existing = Object.hasOwn(prev.settledNoticesBySession, sessionId)
      ? prev.settledNoticesBySession[sessionId]
      : undefined
    if (!existing || existing.length === 0) {
      return prev
    }
    const remaining = existing.filter((notice) => !acked.has(notice.jobId))
    if (remaining.length === existing.length) {
      return prev
    }
    if (remaining.length === 0) {
      return {
        settledNoticesBySession: Object.fromEntries(
          Object.entries(prev.settledNoticesBySession).filter(([id]) => id !== sessionId)
        ),
      }
    }
    return {
      settledNoticesBySession: {
        ...prev.settledNoticesBySession,
        [sessionId]: remaining,
      },
    }
  })
}
