import type {
  SiteSettings,
  SyncStreamEvent,
  UpstreamSiteForm,
  UpstreamSiteResponse,
} from '../types/upstream'
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

type AdminErrorPayload = {
  message?: string
}

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
  } catch (error) {
    throw new Error('admin.upstream.errors.network')
  }

  const text = await response.text()
  const payload = text ? JSON.parse(text) as T & AdminErrorPayload : ({} as T & AdminErrorPayload)

  if (!response.ok) {
    if (isUnauthorizedApiResponse(response.status, payload)) {
      handleAuthExpired()
      throw new Error(authUnauthorizedErrorKey)
    }

    throw new Error('admin.upstream.errors.request')
  }

  return payload
}

export const listUpstreamSites = async (): Promise<UpstreamSiteResponse[]> => requestJson<UpstreamSiteResponse[]>('/upstream-sites')

export const createUpstreamSite = async (form: UpstreamSiteForm): Promise<UpstreamSiteResponse> => (
  requestJson<UpstreamSiteResponse>('/upstream-sites', {
    method: 'POST',
    body: JSON.stringify(form),
  })
)

export const updateUpstreamSite = async (id: string, form: UpstreamSiteForm): Promise<UpstreamSiteResponse> => (
  requestJson<UpstreamSiteResponse>(`/upstream-sites/${id}`, {
    method: 'PUT',
    body: JSON.stringify(form),
  })
)

export const syncUpstreamSite = async (id: string): Promise<UpstreamSiteResponse> => (
  requestJson<UpstreamSiteResponse>(`/upstream-sites/${id}/sync`, { method: 'POST' })
)

export const syncAllUpstreamSites = async (): Promise<UpstreamSiteResponse[]> => (
  requestJson<UpstreamSiteResponse[]>('/upstream-sites/sync-all', { method: 'POST' })
)

export const removeUpstreamSite = async (id: string): Promise<void> => {
  await requestJson<{ success: boolean }>(`/upstream-sites/${id}`, { method: 'DELETE' })
}

export const updateSiteSettings = async (id: string, settings: SiteSettings): Promise<UpstreamSiteResponse> => (
  requestJson<UpstreamSiteResponse>(`/upstream-sites/${id}/settings`, {
    method: 'PATCH',
    body: JSON.stringify(settings),
  })
)

export const listSiteSettlements = async (siteId: string): Promise<{ items: import('../types/upstream').SettlementRecord[] }> => (
  requestJson(`/upstream-sites/${siteId}/settlements`)
)

export const createSiteSettlement = async (
  siteId: string,
  body: { amount: number; note?: string; settledAt?: string },
): Promise<import('../types/upstream').SettlementRecord> => (
  requestJson(`/upstream-sites/${siteId}/settlements`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
)

export const voidSiteSettlement = async (siteId: string, recordId: string): Promise<import('../types/upstream').SettlementRecord> => (
  requestJson(`/upstream-sites/${siteId}/settlements/${recordId}/void`, { method: 'POST' })
)

export const deleteSiteSettlement = async (siteId: string, recordId: string): Promise<void> => {
  await requestJson<{ success: boolean }>(`/upstream-sites/${siteId}/settlements/${recordId}`, { method: 'DELETE' })
}

export const getSiteSettlementSummary = async (siteId: string): Promise<import('../types/upstream').SettlementSummary> => (
  requestJson(`/upstream-sites/${siteId}/settlement-summary`)
)

export const createSubscriptionTopup = async (
  siteId: string,
  body: {
    subscriptionId: string
    groupName?: string
    amountCost: number
    amountPlatform?: number
    businessDate: string
    note?: string
  },
): Promise<import('../types/upstream').LedgerRecord> => (
  requestJson(`/upstream-sites/${siteId}/ledger/subscription-topup`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
)

export interface RechargeCandidate {
  platformId: string
  platformType?: string
  amountPlatform: number
  amountCost: number
  note?: string
  createdAt?: string | null
  suggestedTag?: string
  tag?: string
  ledgerId?: string
  countsAsInbound: boolean
  businessDate?: string
}

export interface RechargeCandidatesResponse {
  items: RechargeCandidate[]
  available: boolean
  rechargeRate: number
  messageKey?: string
  platform?: string
  page: number
  pageSize: number
  total: number
  /** 卡片「历史充值」同源，成本口径，含赠送/返利累计 */
  platformLifetimeCost?: number | null
  /** 本次拉到的平台明细合计（成本口径） */
  detailListCost?: number
  /** 已标记为充值的合计（成本口径，计进货） */
  markedRechargeCost?: number
  markedGiftCost?: number
  markedRebateCost?: number
  /** 平台累计 − 已标充值 */
  gapLifetimeVsMarked?: number | null
}

export const listSiteRechargeCandidates = async (
  siteId: string,
  params?: { page?: number; pageSize?: number },
): Promise<RechargeCandidatesResponse> => {
  const page = params?.page && params.page > 0 ? params.page : 1
  const pageSize = params?.pageSize && params.pageSize > 0 ? params.pageSize : 20
  return requestJson(`/upstream-sites/${siteId}/recharge-candidates?page=${page}&page_size=${pageSize}`)
}

export const markSiteRecharge = async (
  siteId: string,
  body: {
    platformRecordId: string
    tag: 'recharge' | 'gift' | 'rebate' | string
    amountPlatform: number
    note?: string
    createdAt?: string
  },
): Promise<import('../types/upstream').LedgerRecord> => (
  requestJson(`/upstream-sites/${siteId}/ledger/mark`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
)

/** 以 SSE 流方式逐站同步，每个站点的进度通过 onEvent 回调实时推送。 */
export const streamSyncAllUpstreamSites = async (
  onEvent: (event: SyncStreamEvent) => void,
  signal?: AbortSignal,
): Promise<void> => {
  let response: Response
  try {
    response = await fetch(endpoint('/upstream-sites/sync-stream'), {
      headers: { Accept: 'text/event-stream', ...authHeaders() },
      signal,
    })
  } catch {
    throw new Error('admin.upstream.errors.network')
  }

  if (!response.ok) {
    if (isUnauthorizedApiResponse(response.status, {})) {
      handleAuthExpired()
      throw new Error(authUnauthorizedErrorKey)
    }
    throw new Error('admin.upstream.errors.request')
  }

  const reader = response.body!.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  // eslint-disable-next-line no-constant-condition
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const parts = buffer.split('\n\n')
    buffer = parts.pop()!
    for (const part of parts) {
      const dataLine = part.split('\n').find(l => l.startsWith('data: '))
      if (dataLine) {
        try {
          const event = JSON.parse(dataLine.slice(6)) as SyncStreamEvent
          onEvent(event)
        } catch { /* skip malformed lines */ }
      }
    }
  }
}
