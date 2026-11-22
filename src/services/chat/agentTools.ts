import { ToolService, type ToolExecuteRequest } from '../../mygo.js'
import type { ClientToolDefinition } from '../api/types.js'
import { isMCPReadOnlyTool, markSessionMCPToolsLoaded } from '../mcp/store.js'
import { useSettings } from '../settings.js'
import type { PermissionMode } from '../workspace/types.js'
import type { ClientToolResultPart, ClientToolUsePart, ToolFileDiff, ToolUIMeta } from './types.js'

const READ_ONLY_TOOLS = new Set(['read', 'glob', 'grep', 'job_output', 'skill'])
const READ_CONCURRENCY_LIMIT = 4

const isReadOnlyToolName = (name: string): boolean =>
  READ_ONLY_TOOLS.has(name) || isMCPReadOnlyTool(name)

/** 暴露给 DeepSeek API 的内置文件、命令执行、后台任务与 Skills 工具定义 */
export const CLIENT_FILE_TOOLS: ClientToolDefinition[] = [
  {
    name: 'read',
    description:
      'Read a UTF-8 text file with line numbers. Supports pagination via offset (1-based line number) and limit (max 2000 lines).',
    input_schema: {
      type: 'object',
      properties: {
        file_path: {
          type: 'string',
          description: 'Path to the file (relative to workspace or absolute).',
        },
        offset: {
          type: 'integer',
          description: '1-based starting line number (default 1).',
        },
        limit: {
          type: 'integer',
          description: 'Maximum number of lines to read (default 2000, max 2000).',
        },
      },
      required: ['file_path'],
    },
  },
  {
    name: 'write',
    description:
      'Create or overwrite a file atomically. Automatically creates missing parent directories. In a temporary chat without a workspace, writing a relative path automatically creates an isolated scratch folder.',
    input_schema: {
      type: 'object',
      properties: {
        file_path: {
          type: 'string',
          description: 'Path to write (relative to workspace/scratch dir or absolute).',
        },
        content: {
          type: 'string',
          description: 'Complete UTF-8 content to write to the file.',
        },
      },
      required: ['file_path', 'content'],
    },
  },
  {
    name: 'edit',
    description:
      'Perform an exact literal string replacement in an existing file. Requires old_string to match uniquely unless replace_all is true.',
    input_schema: {
      type: 'object',
      properties: {
        file_path: {
          type: 'string',
          description: 'Path to the existing file to edit.',
        },
        old_string: {
          type: 'string',
          description: 'Exact literal text to find and replace.',
        },
        new_string: {
          type: 'string',
          description: 'Replacement text.',
        },
        replace_all: {
          type: 'boolean',
          description:
            'Replace every occurrence of old_string instead of requiring a unique match.',
        },
      },
      required: ['file_path', 'old_string', 'new_string'],
    },
  },
  {
    name: 'delete_file',
    description:
      'Safely move a file, directory, or symlink inside the workspace to trash (.vela-trash) without following symlinks. Always prefer delete_file over rm.',
    input_schema: {
      type: 'object',
      properties: {
        file_path: {
          type: 'string',
          description: 'Path to the file or directory to delete.',
        },
      },
      required: ['file_path'],
    },
  },
  {
    name: 'glob',
    description:
      'Find files matching a glob pattern (supports ** recursive wildcards), sorted by newest modification time first.',
    input_schema: {
      type: 'object',
      properties: {
        pattern: {
          type: 'string',
          description: 'Glob pattern such as "**/*.ts" or "src/**/*.go".',
        },
        path: {
          type: 'string',
          description: 'Optional directory to search within (defaults to workspace root).',
        },
      },
      required: ['pattern'],
    },
  },
  {
    name: 'grep',
    description:
      'Search file contents using a Go regular expression, grouped by file with line numbers.',
    input_schema: {
      type: 'object',
      properties: {
        pattern: {
          type: 'string',
          description: 'Regular expression pattern to search for.',
        },
        path: {
          type: 'string',
          description: 'Optional file or directory to search within (defaults to workspace root).',
        },
        include: {
          type: 'string',
          description: 'Optional glob filter for filenames (e.g. "*.ts", "**/*.go").',
        },
      },
      required: ['pattern'],
    },
  },
  {
    name: 'bash',
    description:
      'Execute a shell command (bash on macOS/Linux, PowerShell on Windows) in the active workspace or isolated temporary directory. Strict read-only commands run automatically; write/build/test commands run inside the OS sandbox (or request approval when needed). If the sandbox blocks an operation ([sandbox: ...]), you may retry that exact command once with sandbox_permissions (the narrowest wider mode that suffices) and a one-sentence justification to ask the user for one-time approval. Non-zero exit codes are returned as normal output with [exit code: N]. Commands running past timeoutMs are automatically moved to a background job so you can inspect them with job_output or stop them with job_kill. Prefer delete_file over rm.',
    input_schema: {
      type: 'object',
      properties: {
        description: {
          type: 'string',
          description: 'Short 5-10 word description of what this command does for the UI.',
        },
        command: {
          type: 'string',
          description: 'Shell command to execute.',
        },
        workdir: {
          type: 'string',
          description:
            'Optional working directory override (defaults to workspace or scratch dir).',
        },
        timeoutMs: {
          type: 'integer',
          description: 'Optional timeout in milliseconds (default 60000, max 300000).',
        },
        sandbox_permissions: {
          type: 'string',
          enum: ['workspace-write', 'full-access'],
          description:
            'Optional one-time sandbox escalation mode to retry after a sandbox denial (choose the narrowest wider mode that suffices). Must be paired with justification; always prompts the user for one-time approval.',
        },
        justification: {
          type: 'string',
          description:
            'One-sentence reason for requesting sandbox_permissions, shown directly in the user approval prompt (use the user language).',
        },
      },
      required: ['command'],
    },
  },
  {
    name: 'job_output',
    description:
      'Read new stdout/stderr output and current status (running/completed/killed) from a background bash job created when a command exceeded timeoutMs.',
    input_schema: {
      type: 'object',
      properties: {
        job_id: {
          type: 'string',
          description: 'Background job ID returned by bash (e.g. "job-1").',
        },
        wait_ms: {
          type: 'integer',
          description:
            'Optional milliseconds to wait for new output or completion (default 0, max 30000).',
        },
      },
      required: ['job_id'],
    },
  },
  {
    name: 'job_kill',
    description: 'Terminate a running background bash job and its process group.',
    input_schema: {
      type: 'object',
      properties: {
        job_id: {
          type: 'string',
          description: 'Background job ID to terminate (e.g. "job-1").',
        },
      },
      required: ['job_id'],
    },
  },
  {
    name: 'skill',
    description:
      'Load the full instructions of a local SKILL.md listed in the skills-catalog <system-reminder>. Call this on demand when a task matches an available skill.',
    input_schema: {
      type: 'object',
      properties: {
        name: {
          type: 'string',
          description: 'Exact name of the skill to load.',
        },
      },
      required: ['name'],
    },
  },
]

export type AgentWorkspaceContext = {
  workspacePath: string | null
  scratchDir: string | null
  permissionMode: PermissionMode
  skillsReminder?: string
  invokedSkillReminder?: string
  mcpPromptSection?: string
}

/** 组装包含工作区状态、权限边界、Skills 渐进式摘要与 MCP 指令/按需目录的系统提示，避免普通闲聊误触磁盘探测 */
export const buildAgentSystemPrompt = (basePrompt: string, ctx: AgentWorkspaceContext): string => {
  const permissionNote =
    ctx.permissionMode === 'read-only'
      ? '当前权限档位：仅可查看（read-only），仅允许 read/glob/grep 与严格只读命令（OS 沙箱禁止写盘），禁止写入、修改、删除或执行写命令。'
      : ctx.permissionMode === 'full-access'
        ? '当前权限档位：完全权限（full-access），允许跨目录读写文件与执行常规命令（高危删除或提权命令仍需一次性确认）。'
        : '当前权限档位：工作区内修改（workspace-write），允许在当前工作区或会话临时目录内读写编辑文件以及执行严格只读命令；非只读命令或越界路径会请求用户一次性审批，且命令受轻量 OS 沙箱限制工作区外写入。'

  const parts: string[] = [basePrompt]

  if (ctx.workspacePath) {
    parts.push(
      `当前已挂载本地工作区：${ctx.workspacePath}。`,
      permissionNote,
      '你可以使用内置工具（read、write、edit、delete_file、glob、grep、bash、job_output、job_kill、skill）查看代码、修改文件与运行命令；优先使用相对路径，删除文件优先使用 delete_file 而非 rm。'
    )
  } else if (ctx.scratchDir) {
    parts.push(
      `当前为无项目目录的临时会话，已自动分配隔离临时目录：${ctx.scratchDir}。`,
      permissionNote,
      '常规问答请直接回答，无需探测磁盘；仅当用户需要生成文件、可运行 Demo 或执行脚本验证时再调用工具（优先使用相对路径，删除文件优先使用 delete_file）。'
    )
  } else {
    parts.push(
      '当前为无项目目录的临时会话；常规问答直接回答，无需探测磁盘。',
      permissionNote,
      '仅当用户需要生成文件、可运行 Demo（如 HTML 小游戏、脚本、文档）或执行命令验证时再调用工具；使用 write 写入相对路径（如 index.html）时，系统会自动创建会话专属临时目录。'
    )
  }

  if (ctx.skillsReminder) {
    parts.push(ctx.skillsReminder)
  }
  if (ctx.invokedSkillReminder) {
    parts.push(ctx.invokedSkillReminder)
  }
  if (ctx.mcpPromptSection) {
    parts.push(ctx.mcpPromptSection)
  }

  return parts.join('\n')
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

const parseToolUIMeta = (raw?: Record<string, unknown>): ToolUIMeta | undefined => {
  if (!raw) {
    return undefined
  }
  const meta: ToolUIMeta = {}
  if (typeof raw.workdir === 'string' && raw.workdir) {
    meta.workdir = raw.workdir
  }
  if (typeof raw.truncated === 'boolean') {
    meta.truncated = raw.truncated
  }
  if (typeof raw.exitCode === 'number' || raw.exitCode === null) {
    meta.exitCode = raw.exitCode
  }
  if (typeof raw.backgroundJobId === 'string' && raw.backgroundJobId) {
    meta.backgroundJobId = raw.backgroundJobId
  }
  if (raw.jobStatus === 'running' || raw.jobStatus === 'completed' || raw.jobStatus === 'killed') {
    meta.jobStatus = raw.jobStatus
  }
  if (typeof raw.sandboxed === 'boolean') {
    meta.sandboxed = raw.sandboxed
  }
  if (typeof raw.sandboxMode === 'string' && raw.sandboxMode) {
    meta.sandboxMode = raw.sandboxMode
  }
  if (typeof raw.skillName === 'string' && raw.skillName) {
    meta.skillName = raw.skillName
  }
  if (typeof raw.skillScope === 'string' && raw.skillScope) {
    meta.skillScope = raw.skillScope
  }
  if (typeof raw.skillPath === 'string' && raw.skillPath) {
    meta.skillPath = raw.skillPath
  }
  if (typeof raw.mcpServer === 'string' && raw.mcpServer) {
    meta.mcpServer = raw.mcpServer
  }
  if (typeof raw.mcpTool === 'string' && raw.mcpTool) {
    meta.mcpTool = raw.mcpTool
  }
  if (Array.isArray(raw.mcpLoadedTools)) {
    const loaded = raw.mcpLoadedTools.filter((item): item is string => typeof item === 'string')
    if (loaded.length > 0) {
      meta.mcpLoadedTools = loaded
    }
  }
  if (Array.isArray(raw.producedFiles)) {
    const files = raw.producedFiles.filter((item): item is string => typeof item === 'string')
    if (files.length > 0) {
      meta.producedFiles = files
    }
  }
  if (Array.isArray(raw.diffs)) {
    const diffs: ToolFileDiff[] = []
    for (const item of raw.diffs) {
      if (
        isRecord(item) &&
        typeof item.path === 'string' &&
        typeof item.oldText === 'string' &&
        typeof item.newText === 'string'
      ) {
        diffs.push({ path: item.path, oldText: item.oldText, newText: item.newText })
      }
    }
    if (diffs.length > 0) {
      meta.diffs = diffs
    }
  }
  return Object.keys(meta).length > 0 ? meta : undefined
}

export type ApprovalDecision = {
  approved: boolean
  allowSession?: boolean
}

export type ToolExecutionContext = {
  sessionId: string
  getWorkdir: () => string
  permissionMode: PermissionMode
  signal: AbortSignal
  onAssignedScratch: (scratchDir: string) => void
  requestApproval?: (
    call: ClientToolUsePart,
    reason: string,
    workdir: string,
    sessionAllowKey?: string
  ) => Promise<ApprovalDecision>
  onResult?: (result: ClientToolResultPart) => void
}

const runSingleTool = async (
  call: ClientToolUsePart,
  ctx: ToolExecutionContext
): Promise<ClientToolResultPart> => {
  if (ctx.signal.aborted) {
    return {
      type: 'client_tool_result',
      tool_use_id: call.id,
      toolName: call.name,
      content: 'ABORTED_BEFORE_DISPATCH: User stopped generation before this tool executed.',
      isError: true,
    }
  }

  if (call.parseError) {
    return {
      type: 'client_tool_result',
      tool_use_id: call.id,
      toolName: call.name,
      content: `TOOL_ERROR: Failed to parse tool input JSON (${call.parseError}). Please retry with valid JSON arguments.`,
      isError: true,
    }
  }

  const workdir = ctx.getWorkdir()
  const settings = useSettings.getState()
  const req: ToolExecuteRequest = {
    sessionId: ctx.sessionId,
    toolName: call.name,
    input: call.input,
    workdir,
    permissionMode: ctx.permissionMode,
    approved: false,
    approvalBlacklist: settings.approvalBlacklist,
    skillConfig: settings.skillConfig,
  }

  try {
    const classification = await ToolService.classifyToolCall(req)
    if (ctx.signal.aborted) {
      return {
        type: 'client_tool_result',
        tool_use_id: call.id,
        toolName: call.name,
        content: 'ABORTED_BEFORE_DISPATCH: User stopped generation before this tool executed.',
        isError: true,
      }
    }
    if (classification.decision === 'deny') {
      return {
        type: 'client_tool_result',
        tool_use_id: call.id,
        toolName: call.name,
        content: `Permission denied: ${classification.reason}`,
        isError: true,
        uiMeta: {
          approvalStatus: 'rejected',
          approvalReason: classification.reason,
        },
      }
    }

    let approvedOnce = false
    if (classification.decision === 'ask') {
      const decision = ctx.requestApproval
        ? await ctx.requestApproval(
            call,
            classification.reason,
            workdir,
            classification.sessionAllowKey
          )
        : { approved: false }
      if (ctx.signal.aborted) {
        return {
          type: 'client_tool_result',
          tool_use_id: call.id,
          toolName: call.name,
          content: 'ABORTED_BEFORE_DISPATCH: User stopped generation while awaiting approval.',
          isError: true,
          uiMeta: {
            approvalStatus: 'rejected',
            approvalReason: classification.reason,
          },
        }
      }
      if (!decision.approved) {
        return {
          type: 'client_tool_result',
          tool_use_id: call.id,
          toolName: call.name,
          content: `User rejected tool call (${call.name}): ${classification.reason}`,
          isError: true,
          uiMeta: {
            approvalStatus: 'rejected',
            approvalReason: classification.reason,
          },
        }
      }
      approvedOnce = true
      req.approved = true
      if (decision.allowSession) {
        req.allowSession = true
      }
    }

    const res = await ToolService.executeTool(req)
    if (res.assignedScratch) {
      ctx.onAssignedScratch(res.assignedScratch)
    }
    const uiMeta = parseToolUIMeta(res.uiMeta) ?? {}
    if (!res.isError && uiMeta.mcpServer && uiMeta.mcpLoadedTools?.length) {
      markSessionMCPToolsLoaded(ctx.sessionId, uiMeta.mcpServer, uiMeta.mcpLoadedTools)
    }
    if (approvedOnce) {
      uiMeta.approvalStatus = 'approved'
      uiMeta.approvalReason = classification.reason
    }
    return {
      type: 'client_tool_result',
      tool_use_id: call.id,
      toolName: call.name,
      content: res.modelContent,
      isError: res.isError,
      uiMeta: Object.keys(uiMeta).length > 0 ? uiMeta : undefined,
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    return {
      type: 'client_tool_result',
      tool_use_id: call.id,
      toolName: call.name,
      content: `TOOL_ERROR: ${message}`,
      isError: true,
    }
  }
}

/**
 * 并发只读 + 独占写屏障执行调度：
 * 连续的只读文件工具（read/glob/grep）以有界并发执行；写入/修改/删除及 bash 命令串行执行。
 * 最终结果严格按模型声明顺序返回，保证 tool_use / tool_result 顺序一致。
 */
export const executeClientToolCalls = async (
  calls: ClientToolUsePart[],
  ctx: ToolExecutionContext
): Promise<ClientToolResultPart[]> => {
  const results = new Array<ClientToolResultPart>(calls.length)
  let index = 0

  while (index < calls.length) {
    const call = calls[index]
    if (!isReadOnlyToolName(call.name)) {
      const result = await runSingleTool(call, ctx)
      results[index] = result
      ctx.onResult?.(result)
      index += 1
      continue
    }

    // 收集连续只读工具批次，有界并发执行
    const batchStart = index
    while (index < calls.length && isReadOnlyToolName(calls[index].name)) {
      index += 1
    }
    const batchEnd = index
    let cursor = batchStart

    const worker = async () => {
      while (cursor < batchEnd) {
        const current = cursor
        cursor += 1
        const result = await runSingleTool(calls[current], ctx)
        results[current] = result
        ctx.onResult?.(result)
      }
    }

    const concurrency = Math.min(READ_CONCURRENCY_LIMIT, batchEnd - batchStart)
    await Promise.all(Array.from({ length: concurrency }, () => worker()))
  }

  return results
}
