import { getRegionName, provinces, type UserLocation } from '../location.js'
import type { Locale } from '../settings.js'
import { formatDate } from '../../utils/format.js'
import { translate } from '../../i18n/index.js'

const locationName = (location: UserLocation | null, locale: Locale): string => {
  if (!location) {
    return ''
  }
  if (location.region === 'CN' && locale === 'zh-CN') {
    const province = provinces.find((item) => item.code === location.province)
    const city = province?.cities.find((item) => item.code === location.city)
    return city?.name ?? province?.name ?? getRegionName('CN', locale)
  }
  return getRegionName(location.region, locale)
}

/** 校验并兜底历史快照的 platform 字段，确保满足服务端 role: 'latest_reminder' 仅接受 App/Web 的约束。 */
export const sanitizeLatestReminder = (reminder: string): string => {
  const parts = reminder.split(',')
  if (parts.length !== 5 || parts[3] === 'App' || parts[3] === 'Web') {
    return reminder
  }
  parts[3] = 'App'
  return parts.join(',')
}

/** 模型约定的五项信息；日期与星期使用浏览器本地时区，避免 UTC 日期跨日。 */
export const createLatestReminder = (
  { locale, location }: { locale: Locale; location: UserLocation | null },
  now = new Date()
): string => {
  return [
    formatDate(now),
    new Intl.DateTimeFormat(locale, { weekday: 'long' }).format(now),
    locationName(location, locale),
    'App',
    translate('latestReminderLanguage'),
  ].join(',')
}

/** 为不支持 role: 'latest_reminder' 的模型将快照格式化为标准 <system-reminder> 文本块。 */
export const formatLatestReminderTag = (reminder: string): string => {
  const [date, weekday, location, platform, language] = reminder.split(',')
  const fields: string[] = []
  if (date) {
    fields.push(`Date: ${date}${weekday ? ` (${weekday})` : ''}`)
  }
  if (location) {
    fields.push(`Location: ${location}`)
  }
  if (platform) {
    fields.push(`Client: ${platform}`)
  }
  if (language) {
    fields.push(`Language: ${language}`)
  }
  return `<system-reminder>\n${fields.join(', ') || reminder}\n</system-reminder>`
}
