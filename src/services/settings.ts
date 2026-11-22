import { create } from 'zustand'
import type { TtsVoice } from './api/speech.js'
import type { ChatModel, ReasoningEffort } from './api/types.js'
import { normalizeLocation, type UserLocation } from './location.js'
import type { PermissionMode } from './workspace/types.js'

export type Locale = 'zh-CN' | 'en-US'

/** 主题偏好；默认跟随系统 */
export type ThemePreference = 'light' | 'dark' | 'system'

export type ClientOS = 'windows' | 'darwin' | 'linux'

export const detectClientOS = (): ClientOS => {
  if (typeof navigator === 'undefined') {
    return 'linux'
  }
  const probe = `${navigator.platform ?? ''} ${navigator.userAgent ?? ''}`.toLowerCase()
  if (probe.includes('win')) {
    return 'windows'
  }
  if (probe.includes('mac')) {
    return 'darwin'
  }
  return 'linux'
}

const COMMON_APPROVAL_BLACKLIST: readonly string[] = [
  'rm',
  'rmdir',
  'shutdown',
  'kill',
  'format',
  'git reset',
  'git clean',
  'git checkout',
  'git restore',
  'git rebase',
  'git merge',
  'git push',
  'curl',
  'wget',
  'ssh',
  'scp',
  'rsync',
  'nc',
  'netcat',
  'telnet',
  'ftp',
  'sftp',
  'gh',
  'aws',
  'gcloud',
  'az',
  'twine',
  'npm publish',
  'pnpm publish',
  'yarn publish',
  'cargo publish',
]

const UNIX_APPROVAL_BLACKLIST: readonly string[] = [
  'sudo',
  'su',
  'mkfs',
  'dd',
  'chmod',
  'chown',
  'reboot',
  'halt',
  'killall',
  'pkill',
]

const WINDOWS_APPROVAL_BLACKLIST: readonly string[] = [
  'remove-item',
  'ri',
  'del',
  'erase',
  'rd',
  'stop-process',
  'spps',
  'taskkill',
  'start-process',
  'saps',
  'invoke-expression',
  'iex',
  'icacls',
  'takeown',
  'reg',
  'sc',
  'net',
  'netsh',
  'diskpart',
  'bcdedit',
  'invoke-webrequest',
  'iwr',
  'invoke-restmethod',
  'irm',
  'bitsadmin',
  'certutil',
]

const buildPlatformApprovalBlacklist = (): readonly string[] => {
  const os = detectClientOS()
  if (os === 'windows') {
    return [...COMMON_APPROVAL_BLACKLIST, ...WINDOWS_APPROVAL_BLACKLIST]
  }
  if (os === 'darwin') {
    return [...COMMON_APPROVAL_BLACKLIST, ...UNIX_APPROVAL_BLACKLIST, 'diskutil']
  }
  return [...COMMON_APPROVAL_BLACKLIST, ...UNIX_APPROVAL_BLACKLIST]
}

/** 沙箱生效时（工作区内修改模式）默认需要人工审批的命令黑名单（按当前操作系统裁剪） */
export const DEFAULT_APPROVAL_BLACKLIST: readonly string[] = buildPlatformApprovalBlacklist()

export const DEFAULT_SKILL_PROJECT_DIRS: readonly string[] = ['.agents/skills', '.vela/skills']
export const DEFAULT_SKILL_USER_DIRS: readonly string[] = ['~/.agents/skills', '~/.vela/skills']

export type SkillSettings = {
  projectDirs: string[]
  userDirs: string[]
  disabledSkills: string[]
}

/** 本机设置，存 localStorage：API Key、语言、主题、位置、朗读音色、模型、推理等级、权限默认值、审批黑名单、Skills 配置、深度思考与智能搜索开关、侧边栏收起状态 */
type Settings = {
  apiKey: string | null
  locale: Locale
  themePreference: ThemePreference
  location: UserLocation | null
  ttsVoice: TtsVoice
  model: ChatModel
  reasoningEffort: ReasoningEffort
  defaultPermissionMode: PermissionMode
  approvalBlacklist: string[]
  skillConfig: SkillSettings
  thinking: boolean
  search: boolean
  siderCollapsed: boolean
}

const KEYS = {
  apiKey: 'chat-demo:api-key',
  locale: 'chat-demo:locale',
  location: 'chat-demo:location',
  themePreference: 'chat-demo:theme-preference',
  ttsVoice: 'chat-demo:tts-voice',
  model: 'chat-demo:model',
  reasoningEffort: 'chat-demo:reasoning-effort',
  permissionMode: 'chat-demo:permission-mode',
  approvalBlacklist: 'chat-demo:approval-blacklist',
  skillConfig: 'chat-demo:skill-config',
  thinking: 'chat-demo:thinking',
  search: 'chat-demo:search',
  siderCollapsed: 'chat-demo:sider-collapsed',
} as const

const read = (key: string) => {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

const write = (key: string, value: string | null) => {
  try {
    if (value === null) {
      localStorage.removeItem(key)
    } else {
      localStorage.setItem(key, value)
    }
  } catch {
    // 存储不可用时只在内存中生效
  }
}

/** 位置选择模式跟随浏览器语言，与应用内选择的界面语言无关。 */
export const isChineseBrowser = (): boolean =>
  (typeof navigator === 'undefined' ? 'zh' : navigator.language).toLowerCase().startsWith('zh')

const initialLocale = (): Locale => {
  const saved = read(KEYS.locale)
  if (saved === 'zh-CN' || saved === 'en-US') {
    return saved
  }
  return isChineseBrowser() ? 'zh-CN' : 'en-US'
}

const initialLocation = (): UserLocation | null => {
  try {
    const location = normalizeLocation(JSON.parse(read(KEYS.location) ?? 'null'))
    if (!location) {
      return null
    }
    return isChineseBrowser()
      ? location.region === 'CN' && location.province
        ? location
        : null
      : { region: location.region }
  } catch {
    return null
  }
}

/** 未保存过时跟随系统 */
const initialThemePreference = (): ThemePreference => {
  const saved = read(KEYS.themePreference)
  return saved === 'light' || saved === 'dark' ? saved : 'system'
}

/** 未保存过时使用服务端默认音色 stella（海星） */
const initialTtsVoice = (): TtsVoice => (read(KEYS.ttsVoice) === 'tide' ? 'tide' : 'stella')

/** 未保存过时使用默认模型 deepseek-flash */
const initialModel = (): ChatModel =>
  read(KEYS.model) === 'deepseek-v4-pro' ? 'deepseek-v4-pro' : 'deepseek-flash'

/** 未保存过时兼容旧版 thinking 开关：开启对应 high，关闭对应 off */
const initialReasoningEffort = (): ReasoningEffort => {
  const saved = read(KEYS.reasoningEffort)
  if (saved === 'off' || saved === 'low' || saved === 'high' || saved === 'max') {
    return saved
  }
  return read(KEYS.thinking) === 'true' ? 'high' : 'off'
}

/** 未保存过时默认权限档位为工作区内修改 */
const initialPermissionMode = (): PermissionMode => {
  const saved = read(KEYS.permissionMode)
  if (saved === 'read-only' || saved === 'workspace-write' || saved === 'full-access') {
    return saved
  }
  return 'workspace-write'
}

const normalizeBlacklist = (items: readonly string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of items) {
    const cleaned = item.trim().replace(/\s+/g, ' ')
    if (!cleaned) {
      continue
    }
    const key = cleaned.toLowerCase()
    if (!seen.has(key)) {
      seen.add(key)
      out.push(cleaned)
    }
  }
  return out
}

const initialApprovalBlacklist = (): string[] => {
  const raw = read(KEYS.approvalBlacklist)
  if (raw === null) {
    return [...DEFAULT_APPROVAL_BLACKLIST]
  }
  try {
    const parsed: unknown = JSON.parse(raw)
    if (Array.isArray(parsed)) {
      return normalizeBlacklist(parsed.filter((item): item is string => typeof item === 'string'))
    }
  } catch {
    // fall back to defaults
  }
  return [...DEFAULT_APPROVAL_BLACKLIST]
}

const normalizeDirList = (items: readonly string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of items) {
    const trimmed = item.trim()
    if (!trimmed || seen.has(trimmed)) {
      continue
    }
    seen.add(trimmed)
    out.push(trimmed)
  }
  return out
}

const normalizeSkillNameList = (items: readonly string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of items) {
    const trimmed = item.trim()
    if (!trimmed) {
      continue
    }
    const key = trimmed.toLowerCase()
    if (!seen.has(key)) {
      seen.add(key)
      out.push(trimmed)
    }
  }
  return out
}

const defaultSkillSettings = (): SkillSettings => ({
  projectDirs: [...DEFAULT_SKILL_PROJECT_DIRS],
  userDirs: [...DEFAULT_SKILL_USER_DIRS],
  disabledSkills: [],
})

const initialSkillConfig = (): SkillSettings => {
  const raw = read(KEYS.skillConfig)
  if (!raw) {
    return defaultSkillSettings()
  }
  try {
    const parsed: unknown = JSON.parse(raw)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const obj = parsed as Record<string, unknown>
      return {
        projectDirs: Array.isArray(obj.projectDirs)
          ? normalizeDirList(obj.projectDirs.filter((v): v is string => typeof v === 'string'))
          : [...DEFAULT_SKILL_PROJECT_DIRS],
        userDirs: Array.isArray(obj.userDirs)
          ? normalizeDirList(obj.userDirs.filter((v): v is string => typeof v === 'string'))
          : [...DEFAULT_SKILL_USER_DIRS],
        disabledSkills: Array.isArray(obj.disabledSkills)
          ? normalizeSkillNameList(
              obj.disabledSkills.filter((v): v is string => typeof v === 'string')
            )
          : [],
      }
    }
  } catch {
    // fall back to defaults
  }
  return defaultSkillSettings()
}

const bootReasoningEffort = initialReasoningEffort()

export const useSettings = create<Settings>(() => ({
  apiKey: read(KEYS.apiKey),
  locale: initialLocale(),
  location: initialLocation(),
  themePreference: initialThemePreference(),
  ttsVoice: initialTtsVoice(),
  model: initialModel(),
  reasoningEffort: bootReasoningEffort,
  defaultPermissionMode: initialPermissionMode(),
  approvalBlacklist: initialApprovalBlacklist(),
  skillConfig: initialSkillConfig(),
  thinking: bootReasoningEffort !== 'off',
  search: read(KEYS.search) === 'true',
  siderCollapsed: read(KEYS.siderCollapsed) === 'true',
}))

export const setLocale = (locale: Locale) => {
  write(KEYS.locale, locale)
  useSettings.setState({ locale })
}

export const setLocation = (value: UserLocation | null) => {
  const location = normalizeLocation(value)
  write(KEYS.location, location ? JSON.stringify(location) : null)
  useSettings.setState({ location })
}

export const setThemePreference = (themePreference: ThemePreference) => {
  write(KEYS.themePreference, themePreference)
  useSettings.setState({ themePreference })
}

export const setTtsVoice = (ttsVoice: TtsVoice) => {
  write(KEYS.ttsVoice, ttsVoice)
  useSettings.setState({ ttsVoice })
}

export const setApiKey = (apiKey: string | null) => {
  write(KEYS.apiKey, apiKey)
  useSettings.setState({ apiKey })
}

export const setModel = (model: ChatModel) => {
  write(KEYS.model, model)
  useSettings.setState({ model })
}

export const setReasoningEffort = (reasoningEffort: ReasoningEffort) => {
  const thinking = reasoningEffort !== 'off'
  write(KEYS.reasoningEffort, reasoningEffort)
  write(KEYS.thinking, String(thinking))
  useSettings.setState({ reasoningEffort, thinking })
}

export const setDefaultPermissionMode = (defaultPermissionMode: PermissionMode) => {
  write(KEYS.permissionMode, defaultPermissionMode)
  useSettings.setState({ defaultPermissionMode })
}

export const setApprovalBlacklist = (rules: readonly string[]) => {
  const normalized = normalizeBlacklist(rules)
  write(KEYS.approvalBlacklist, JSON.stringify(normalized))
  useSettings.setState({ approvalBlacklist: normalized })
}

export const resetApprovalBlacklist = () => {
  write(KEYS.approvalBlacklist, null)
  useSettings.setState({ approvalBlacklist: [...DEFAULT_APPROVAL_BLACKLIST] })
}

const saveSkillConfig = (skillConfig: SkillSettings) => {
  write(KEYS.skillConfig, JSON.stringify(skillConfig))
  useSettings.setState({ skillConfig })
}

export const setSkillProjectDirs = (dirs: readonly string[]) => {
  const current = useSettings.getState().skillConfig
  saveSkillConfig({ ...current, projectDirs: normalizeDirList(dirs) })
}

export const setSkillUserDirs = (dirs: readonly string[]) => {
  const current = useSettings.getState().skillConfig
  saveSkillConfig({ ...current, userDirs: normalizeDirList(dirs) })
}

export const toggleSkillEnabled = (skillName: string, enabled: boolean) => {
  const current = useSettings.getState().skillConfig
  const key = skillName.trim().toLowerCase()
  if (!key) {
    return
  }
  const filtered = current.disabledSkills.filter((item) => item.toLowerCase() !== key)
  const nextDisabled = enabled ? filtered : normalizeSkillNameList([...filtered, skillName.trim()])
  saveSkillConfig({ ...current, disabledSkills: nextDisabled })
}

export const resetSkillConfig = () => {
  write(KEYS.skillConfig, null)
  useSettings.setState({ skillConfig: defaultSkillSettings() })
}

export const setThinking = (thinking: boolean) => {
  setReasoningEffort(thinking ? 'high' : 'off')
}

export const setSearch = (search: boolean) => {
  write(KEYS.search, String(search))
  useSettings.setState({ search })
}

export const setSiderCollapsed = (siderCollapsed: boolean) => {
  write(KEYS.siderCollapsed, String(siderCollapsed))
  useSettings.setState({ siderCollapsed })
}
