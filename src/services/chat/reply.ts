import { formatLatestReminderTag } from './latestReminder.js'
import type {
  AssistantBlockParam,
  FileBlockParam,
  MessageParam,
  MessageStreamEvent,
  OutputBlockStart,
  TextBlockParam,
  ToolResultBlockParam,
} from '../api/types.js'
import type {
  AssistantMessage,
  ClientToolResultPart,
  ClientToolUsePart,
  FileAttachment,
  Message,
  ReplyBlock,
  ReplyStatus,
  UserMessage,
} from './types.js'

const isPlainObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

/**
 * 文件按 Files API 的 file_id（附件的 serverId）引用：图片用 image 块，其余用 document 块。
 * 发送前 streamReply 会先补齐 serverId；仍为 null 的是本地没有原件的附件，不进入请求。
 */
const toFileBlocks = (files: FileAttachment[]): FileBlockParam[] =>
  files.flatMap(({ serverId, isImage }) =>
    serverId
      ? [
          {
            type: isImage ? 'image' : 'document',
            source: { type: 'file', file_id: serverId },
          },
        ]
      : []
  )

const toReplyBlock = (block: OutputBlockStart, now: number): ReplyBlock | null => {
  switch (block.type) {
    case 'text':
      return { type: 'text', text: block.text, citations: block.citations ?? [] }
    case 'thinking':
      return {
        type: 'thinking',
        thinking: block.thinking,
        signature: block.signature,
        startedAt: now,
      }
    case 'server_tool_use':
    case 'web_search_tool_result':
    case 'web_fetch_tool_result':
      return block
    case 'tool_use':
      return {
        type: 'client_tool_use',
        id: block.id,
        name: block.name,
        partialJson: '',
        input: isPlainObject(block.input) ? block.input : {},
      }
    default:
      return null
  }
}

const finalizeToolUseInput = (block: ClientToolUsePart): ClientToolUsePart => {
  const raw = block.partialJson?.trim() ?? ''
  if (!raw) {
    return { ...block, parseError: undefined }
  }
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!isPlainObject(parsed)) {
      return { ...block, input: {}, parseError: 'Tool input must be a JSON object' }
    }
    return { ...block, input: parsed, parseError: undefined }
  } catch (error) {
    return {
      ...block,
      input: {},
      parseError: error instanceof Error ? error.message : 'Invalid JSON',
    }
  }
}

const updateBlock = (
  reply: AssistantMessage,
  index: number,
  update: (block: ReplyBlock) => ReplyBlock
): AssistantMessage => {
  const block = reply.blocks[index]
  if (!block) {
    return reply
  }
  const blocks = [...reply.blocks]
  blocks[index] = update(block)
  return { ...reply, blocks }
}

/**
 * 把一个流式事件合并进回复：只追加可识别的内容块，用本次请求的临时索引映射处理增量。
 * 跳过的块不占位；续写直接追加到已有内容之后，message_delta 更新结束原因与累计用量。
 */
export const applyEvent = (
  reply: AssistantMessage,
  event: MessageStreamEvent,
  now: number,
  blockIndexes: Map<number, number>
): AssistantMessage => {
  const next: AssistantMessage =
    reply.status === 'streaming' ? reply : { ...reply, status: 'streaming' }
  switch (event.type) {
    case 'message_start':
      return { ...next, usage: event.message.usage ?? next.usage }
    case 'content_block_start': {
      blockIndexes.delete(event.index)
      const block = toReplyBlock(event.content_block, now)
      if (!block) {
        return next
      }
      blockIndexes.set(event.index, next.blocks.length)
      return {
        ...next,
        // 含客户端工具调用的回复不允许走 Prefill 续写，避免协议冲突。
        ...(block.type === 'client_tool_use' ? { prefix: false } : {}),
        blocks: [...next.blocks, block],
      }
    }
    case 'content_block_delta': {
      const index = blockIndexes.get(event.index)
      if (index === undefined) {
        return next
      }
      const { delta } = event
      return updateBlock(next, index, (block) => {
        if (block.type === 'text' && delta.type === 'text_delta') {
          return { ...block, text: block.text + delta.text }
        }
        if (block.type === 'thinking' && delta.type === 'thinking_delta') {
          return { ...block, thinking: block.thinking + delta.thinking }
        }
        if (block.type === 'thinking' && delta.type === 'signature_delta') {
          return { ...block, signature: (block.signature ?? '') + delta.signature }
        }
        if (block.type === 'text' && delta.type === 'citations_delta') {
          return { ...block, citations: [...(block.citations ?? []), delta.citation] }
        }
        if (block.type === 'client_tool_use' && delta.type === 'input_json_delta') {
          return { ...block, partialJson: (block.partialJson ?? '') + delta.partial_json }
        }
        return block
      })
    }
    case 'content_block_stop': {
      const index = blockIndexes.get(event.index)
      if (index === undefined) {
        return next
      }
      blockIndexes.delete(event.index)
      return updateBlock(next, index, (block) => {
        if (
          block.type === 'thinking' &&
          block.startedAt !== undefined &&
          block.endedAt === undefined
        ) {
          return { ...block, endedAt: now }
        }
        if (block.type === 'client_tool_use') {
          return finalizeToolUseInput(block)
        }
        return block
      })
    }
    case 'message_delta':
      return {
        ...next,
        stopReason: event.delta.stop_reason,
        stopDetails: event.delta.stop_details,
        usage: event.usage ?? next.usage,
      }
    default:
      return next
  }
}

/** 找出尚未结算（缺少对应 client_tool_result）的客户端工具调用 */
export const getUnsettledToolUses = (reply: AssistantMessage): ClientToolUsePart[] => {
  const settledIds = new Set<string>()
  for (const block of reply.blocks) {
    if (block.type === 'client_tool_result') {
      settledIds.add(block.tool_use_id)
    }
  }
  return reply.blocks.filter(
    (block): block is ClientToolUsePart =>
      block.type === 'client_tool_use' && !settledIds.has(block.id)
  )
}

/**
 * 协议自愈：为所有尚未产生结果的 client_tool_use 补写错误 tool_result，
 * 确保任意中止或异常后消息历史依然成对合法，不会在下一轮请求触发 400。
 */
export const healUnsettledToolCalls = (
  reply: AssistantMessage,
  reason: 'aborted' | 'error'
): AssistantMessage => {
  const unsettled = getUnsettledToolUses(reply)
  if (unsettled.length === 0) {
    return reply
  }
  const syntheticResults: ClientToolResultPart[] = unsettled.map((call) => ({
    type: 'client_tool_result',
    tool_use_id: call.id,
    toolName: call.name,
    content:
      reason === 'aborted'
        ? 'ABORTED_BEFORE_DISPATCH: User stopped generation before this tool executed.'
        : 'TOOL_ERROR: Tool execution did not complete.',
    isError: true,
  }))
  return {
    ...reply,
    blocks: [...reply.blocks, ...syntheticResults],
  }
}

/**
 * 回复结束：未结束的思考块以当前时间收尾，并对未结算的工具调用执行协议自愈。
 * 安全拒绝（stop_reason: refusal）时已下发的思考与正文不能撤回，按协议要求由客户端丢弃，只保留工具块。
 */
export const settleReply = (
  reply: AssistantMessage,
  status: Extract<ReplyStatus, 'done' | 'stopped' | 'error'>,
  now: number,
  error?: AssistantMessage['error']
): AssistantMessage => {
  const settled: AssistantMessage = {
    ...reply,
    status,
    error,
    blocks: reply.blocks.flatMap((block): ReplyBlock[] => {
      if (reply.stopReason === 'refusal' && (block.type === 'thinking' || block.type === 'text')) {
        return []
      }
      if (
        block.type === 'thinking' &&
        block.startedAt !== undefined &&
        block.endedAt === undefined
      ) {
        return [{ ...block, endedAt: now }]
      }
      if (
        block.type === 'client_tool_use' &&
        block.partialJson &&
        Object.keys(block.input).length === 0 &&
        !block.parseError
      ) {
        return [finalizeToolUseInput(block)]
      }
      return [block]
    }),
  }
  return healUnsettledToolCalls(settled, status === 'stopped' ? 'aborted' : 'error')
}

/** 回复里所有文本块拼起来，用于复制与判断是否有回答 */
export const getReplyText = (reply: AssistantMessage) =>
  reply.blocks.map((block) => (block.type === 'text' ? block.text : '')).join('')

/**
 * 只有正常达到输出上限或被停止的回复可续写，报错回复不能续写。
 * 末条限制由调用方判断，拒绝和工具调用（含服务端与客户端工具）不能续写。
 * 服务端模板提示不能作为续写前缀。
 */
export const canContinueReply = (reply: AssistantMessage): boolean => {
  if (
    !reply.prefix ||
    reply.stopReason === 'refusal' ||
    reply.stopReason === 'tool_use' ||
    reply.blocks.some(
      (block) =>
        block.type === 'server_tool_use' ||
        block.type === 'web_search_tool_result' ||
        block.type === 'web_fetch_tool_result' ||
        block.type === 'client_tool_use' ||
        block.type === 'client_tool_result' ||
        (block.type === 'text' && block.isTemplate)
    )
  ) {
    return false
  }
  return (
    reply.status === 'stopped' || (reply.status === 'done' && reply.stopReason === 'max_tokens')
  )
}

/**
 * 已结束或停止且有回答文本或客户端工具结果的回复才放进上下文；被安全拒绝的轮次不回传。
 * 服务端模板提示不进入上下文。
 */
const isUsableReply = (message: Message): message is AssistantMessage =>
  message.role === 'assistant' &&
  (message.status === 'done' || message.status === 'stopped') &&
  message.stopReason !== 'refusal' &&
  !message.blocks.some((block) => block.type === 'text' && block.isTemplate) &&
  (getReplyText(message) !== '' ||
    message.blocks.some((block) => block.type === 'client_tool_result'))

/** 有文件或内联 reminder 时用内容块数组：先 reminder、再文件、后正文，文件按 Files API 的 file_id 引用 */
const toUserParam = ({ content, files }: UserMessage, inlineReminder?: string): MessageParam => {
  const blocks = toFileBlocks(files)
  const reminderBlocks: TextBlockParam[] = inlineReminder
    ? [{ type: 'text', text: formatLatestReminderTag(inlineReminder) }]
    : []
  if (blocks.length === 0 && reminderBlocks.length === 0) {
    return { role: 'user', content }
  }
  return {
    role: 'user',
    content: [
      ...reminderBlocks,
      ...blocks,
      ...(content ? [{ type: 'text' as const, text: content }] : []),
    ],
  }
}

/**
 * 将一条 AssistantMessage 展开为符合 Anthropic/DeepSeek 协议的 MessageParam 序列：
 * - 单轮无客户端工具时返回 1 条 assistant 消息；
 * - 含客户端工具时，按 client_tool_result 边界拆分为交替的 assistant(tool_use) + user(tool_result) 序列；
 * - preserveThinking=true（同轮多步循环或续写）保留 thinking 与 signature；
 *   preserveThinking=false（跨轮历史）省略历史 thinking；
 * - ensureTrailingAssistant=true（跨轮历史）确保序列不以 user(tool_result) 结尾，避免与下一轮 user 连续冲突。
 */
const expandAssistantToMessages = (
  { blocks }: AssistantMessage,
  preserveThinking = false,
  ensureTrailingAssistant = false,
  includeServerExtensions = true
): MessageParam[] => {
  const resultIds = new Set<string>()
  for (const block of blocks) {
    if (block.type === 'client_tool_result') {
      resultIds.add(block.tool_use_id)
    }
  }

  const unsignedReadLinks = new Set(
    blocks.flatMap((block) =>
      block.type === 'web_fetch_tool_result' &&
      block.kind === 'read_link' &&
      block.content.type === 'web_fetch_result' &&
      !block.signature?.trim()
        ? [block.tool_use_id]
        : []
    )
  )

  const messages: MessageParam[] = []
  let assistantContent: AssistantBlockParam[] = []
  let text: TextBlockParam | null = null
  const emittedToolUseIds = new Set<string>()

  const flushAssistant = () => {
    const cleaned = assistantContent.filter((block) => block.type !== 'text' || block.text !== '')
    if (cleaned.length > 0) {
      messages.push({ role: 'assistant', content: cleaned })
    }
    assistantContent = []
    text = null
  }

  let i = 0
  while (i < blocks.length) {
    const block = blocks[i]
    if (block.type === 'client_tool_result') {
      flushAssistant()
      const toolResults: ToolResultBlockParam[] = []
      while (i < blocks.length && blocks[i].type === 'client_tool_result') {
        const res = blocks[i] as ClientToolResultPart
        if (emittedToolUseIds.has(res.tool_use_id)) {
          toolResults.push({
            type: 'tool_result',
            tool_use_id: res.tool_use_id,
            content: res.content,
            ...(res.isError ? { is_error: true } : {}),
          })
        }
        i += 1
      }
      if (toolResults.length > 0) {
        messages.push({ role: 'user', content: toolResults })
      }
      continue
    }

    if (preserveThinking && block.type === 'thinking' && !block.thinking.trim()) {
      i += 1
      continue
    }

    if (block.type === 'text') {
      if (block.isTemplate) {
        i += 1
        continue
      }
      if (text) {
        text.text += block.text
      } else {
        const previous = assistantContent.at(-1)
        text = {
          type: 'text',
          text: `${previous?.type === 'text' ? '\n\n' : ''}${block.text}`,
        }
        assistantContent.push(text)
      }
      i += 1
      continue
    }

    text = null
    if (preserveThinking && block.type === 'thinking') {
      assistantContent.push({
        type: 'thinking',
        thinking: block.thinking,
        signature: block.signature ?? '',
      })
    } else if (block.type === 'client_tool_use') {
      // 仅回传有对应 tool_result 的调用，防止异常历史触发协议 400。
      if (resultIds.has(block.id)) {
        emittedToolUseIds.add(block.id)
        assistantContent.push({
          type: 'tool_use',
          id: block.id,
          name: block.name,
          input: block.input,
        })
      }
    } else if (
      includeServerExtensions &&
      block.type === 'server_tool_use' &&
      block.kind === 'read_link' &&
      !unsignedReadLinks.has(block.id)
    ) {
      const { type, id, name, input, kind } = block
      assistantContent.push({ type, id, name, input, kind })
    } else if (
      includeServerExtensions &&
      block.type === 'web_fetch_tool_result' &&
      block.kind === 'read_link' &&
      !unsignedReadLinks.has(block.tool_use_id)
    ) {
      assistantContent.push(block)
    }
    i += 1
  }

  flushAssistant()

  if (ensureTrailingAssistant && messages.at(-1)?.role === 'user') {
    messages.push({
      role: 'assistant',
      content: [{ type: 'text', text: '[Tool execution completed]' }],
    })
  }

  return messages
}

/** 历史中会进入请求上下文的轮次：提问后紧跟可用回答的才保留 */
export const getContextRounds = (history: Message[]): [UserMessage, AssistantMessage][] => {
  const rounds: [UserMessage, AssistantMessage][] = []
  for (let i = 0; i + 1 < history.length; i++) {
    const [user, reply] = [history[i], history[i + 1]]
    if (user.role === 'user' && isUsableReply(reply)) {
      rounds.push([user, reply])
      i++
    }
  }
  return rounds
}

/**
 * 把会话转成请求的 messages：只保留「提问 + 有回答的回复」成对的轮次，最后接上本轮提问。
 * - inTurnReply 用于同轮多步 Agent Loop（保留本轮思考与签名，并以 user(tool_result) 结尾继续生成）；
 * - prefix 用于普通文本回复的 Prefill 续写。
 */
export const toRequestMessages = (
  history: Message[],
  question: UserMessage,
  prefix?: AssistantMessage,
  thinking = prefix?.thinking ?? false,
  latestReminder?: string,
  inTurnReply?: AssistantMessage,
  includeServerExtensions = true
): MessageParam[] => {
  const messages: MessageParam[] = []
  let previousReminder: string | undefined
  const consumeChangedReminder = (reminder: string | undefined): string | undefined => {
    // 仅在当前分支首次出现或内容变化时插入快照，保留历史前缀的 Prompt Cache 命中率。
    if (reminder && reminder !== previousReminder) {
      previousReminder = reminder
      return reminder
    }
    return undefined
  }
  const pushUserWithReminder = (user: UserMessage, reminder: string | undefined) => {
    const changed = consumeChangedReminder(reminder)
    if (includeServerExtensions) {
      if (changed) {
        messages.push({ role: 'latest_reminder', content: changed })
      }
      messages.push(toUserParam(user))
    } else {
      messages.push(toUserParam(user, changed))
    }
  }
  for (const [user, reply] of getContextRounds(history)) {
    // 历史提问没有文字、附件也都没有 serverId 时，提问为空，连同回答一起省略。
    if (!user.content && toFileBlocks(user.files).length === 0) {
      continue
    }
    const expanded = expandAssistantToMessages(reply, false, true, includeServerExtensions)
    if (expanded.length === 0) {
      continue
    }
    pushUserWithReminder(user, reply.latestReminder)
    messages.push(...expanded)
  }
  pushUserWithReminder(question, latestReminder)

  if (inTurnReply) {
    messages.push(
      ...expandAssistantToMessages(inTurnReply, thinking, false, includeServerExtensions)
    )
    return messages
  }

  if (prefix?.prefix) {
    const [assistant] = expandAssistantToMessages(prefix, thinking, false, includeServerExtensions)
    // 没有非空白正文或有效思考时直接重发原问题，避免服务端拒绝空 Prefill。
    if (
      assistant?.role === 'assistant' &&
      assistant.content.some(
        (block) =>
          (block.type === 'text' && block.text.trim()) ||
          (block.type === 'thinking' && block.thinking.trim())
      )
    ) {
      messages.push(assistant)
    }
  }
  return messages
}

/** 思考用时（秒），至少 1 秒 */
export const getThinkingSeconds = (startedAt: number, endedAt: number) =>
  Math.max(1, Math.round((endedAt - startedAt) / 1000))
