import {
  authUnauthorizedErrorKey,
  getAccessToken,
  handleAuthExpired,
  isUnauthorizedApiResponse,
} from '@/modules/auth/api/auth'

const apiBaseUrl = import.meta.env.VITE_API_BASE_URL ?? '/api'

const endpoint = (path: string): string => `${apiBaseUrl.replace(/\/$/, '')}${path}`

const authHeaders = (): HeadersInit => {
  const token = getAccessToken()
  if (!token) return {}
  return { Authorization: `Bearer ${token}` }
}

type AdminErrorPayload = { message?: string }

const requestJson = async <T>(path: string, options: RequestInit = {}): Promise<T> => {
  let response: Response
  try {
    response = await fetch(endpoint(path), {
      ...options,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        ...authHeaders(),
        ...(options.headers ?? {}),
      },
    })
  } catch {
    throw new Error('admin.siteUsers.errors.network')
  }

  const text = await response.text()
  const payload = text ? JSON.parse(text) as T & AdminErrorPayload : ({} as T & AdminErrorPayload)

  if (!response.ok) {
    if (isUnauthorizedApiResponse(response.status, payload)) {
      handleAuthExpired()
      throw new Error(authUnauthorizedErrorKey)
    }
    throw new Error(payload.message || 'admin.siteUsers.errors.request')
  }

  return payload
}

export interface SiteUserItem {
  id: string
  email: string
  username: string
  role: string
  status: string
  /** 上游后台用户备注 */
  notes?: string
  balance?: number | null
  frozenBalance?: number | null
  /** RFC3339 最后使用时间 */
  lastUsedAt?: string | null
  /** RFC3339 创建时间 */
  createdAt?: string | null
  markedGift: number
  markedRebate: number
  markedRecharge: number
  nonRevenue: number
  balanceCny?: number | null
  nonRevenueCny?: number
  revenueCny?: number | null
  /** 今日 token / 实际消费（平台）；按用量排序时返回 */
  todayTokens?: number
  todayCost?: number
  totalTokens?: number
  totalCost?: number
}

export interface SiteUserUsageGroup {
  groupId?: string
  groupName: string
  actualCost: number
  percent: number
  requests?: number
  totalTokens?: number
}

export interface SiteUserUsagePeriod {
  label: string
  startDate: string
  endDate: string
  totalActualCost: number
  groups: SiteUserUsageGroup[]
}

export interface SiteUserUsageResponse {
  user?: SiteUserItem | null
  today: SiteUserUsagePeriod
  history: SiteUserUsagePeriod
  siteRechargeRate?: number
  messageKey?: string
}

export interface SiteUsersResponse {
  items: SiteUserItem[]
  total: number
  page: number
  pageSize: number
  pages: number
  platform?: string
  messageKey?: string
  siteRechargeRate?: number
  excludeAdmin?: boolean
  excludeUserIds?: string[]
  pageCostTotal?: number
  pageRevenueTotal?: number
  sortBy?: string
  sortOrder?: string
}

export interface SiteBalanceSettings {
  excludeAdmin: boolean
  siteRechargeRate: number
  excludeBalances?: number[]
  excludeUserIds?: string[]
  userGiftAmounts?: Record<string, number>
}

export interface SiteUserTopupCandidate {
  platformId: string
  platformType?: string
  amountPlatform: number
  note?: string
  createdAt?: string | null
  suggestedTag?: string
  tag?: string
  markId?: string
  businessDate?: string
  countsAsPaid: boolean
}

export interface SiteUserTopupsResponse {
  items: SiteUserTopupCandidate[]
  available: boolean
  page: number
  pageSize: number
  total: number
  user?: SiteUserItem | null
  platformLifetime?: number | null
  detailListSum: number
  markedRecharge: number
  markedGift: number
  markedRebate: number
  messageKey?: string
  siteRechargeRate?: number
}

export const listSiteUsers = async (params?: {
  page?: number
  pageSize?: number
  search?: string
  sortBy?: string
  sortOrder?: 'asc' | 'desc' | string
  /** 隐藏已整户排除的用户（及排除 admin 时隐藏 admin） */
  hideExcluded?: boolean
}): Promise<SiteUsersResponse> => {
  const q = new URLSearchParams()
  if (params?.page) q.set('page', String(params.page))
  if (params?.pageSize) q.set('page_size', String(params.pageSize))
  if (params?.search) q.set('search', params.search)
  if (params?.sortBy) q.set('sort_by', params.sortBy)
  if (params?.sortOrder) q.set('sort_order', params.sortOrder)
  if (params?.hideExcluded) q.set('hide_excluded', '1')
  const qs = q.toString()
  return requestJson(`/dashboard/site-users${qs ? `?${qs}` : ''}`)
}

export const patchSiteBalanceSettings = async (body: {
  siteRechargeRate?: number
  excludeAdmin?: boolean
  excludeUserIds?: string[]
}): Promise<SiteBalanceSettings> =>
  requestJson('/dashboard/site-balance-settings', {
    method: 'PATCH',
    body: JSON.stringify(body),
  })

/** Typeahead 候选：邮箱 / ID / 用户名搜索 */
export const searchSiteUserCandidates = async (
  search: string,
  limit = 10,
): Promise<SiteUsersResponse> => {
  const q = new URLSearchParams({
    candidates: '1',
    search: search.trim(),
    page_size: String(limit),
  })
  return requestJson(`/dashboard/site-users?${q.toString()}`)
}

export const listSiteUserTopups = async (
  platformUserId: string,
  params?: { page?: number; pageSize?: number },
): Promise<SiteUserTopupsResponse> => {
  const q = new URLSearchParams()
  if (params?.page) q.set('page', String(params.page))
  if (params?.pageSize) q.set('page_size', String(params.pageSize))
  const qs = q.toString()
  return requestJson(`/dashboard/site-users/${encodeURIComponent(platformUserId)}/topups${qs ? `?${qs}` : ''}`)
}

export const markSiteUserTopup = async (
  platformUserId: string,
  body: {
    platformRecordId: string
    tag: 'recharge' | 'gift' | 'rebate' | string
    amountPlatform: number
    note?: string
    createdAt?: string
  },
): Promise<unknown> => (
  requestJson(`/dashboard/site-users/${encodeURIComponent(platformUserId)}/topups/mark`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
)

/** 用户今日 / 历史用量（按分组拆分 + 占比） */
export const getSiteUserUsage = async (
  platformUserId: string,
): Promise<SiteUserUsageResponse> =>
  requestJson(`/dashboard/site-users/${encodeURIComponent(platformUserId)}/usage`)
