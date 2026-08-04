import {
  authUnauthorizedErrorKey,
  handleAuthExpired,
  isUnauthorizedApiResponse,
} from '@/modules/auth/api/auth'
import { buildAuthHeaders } from '@/lib/apiHeaders'

const apiBaseUrl = import.meta.env.VITE_API_BASE_URL ?? '/api'

const endpoint = (path: string): string => `${apiBaseUrl.replace(/\/$/, '')}${path}`

const authHeaders = (): HeadersInit => buildAuthHeaders()

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
    throw new Error('admin.system.errors.network')
  }

  const text = await response.text()
  const payload = text ? JSON.parse(text) as T & AdminErrorPayload : ({} as T & AdminErrorPayload)

  if (!response.ok) {
    if (isUnauthorizedApiResponse(response.status, payload)) {
      handleAuthExpired()
      throw new Error(authUnauthorizedErrorKey)
    }
    throw new Error(payload.message ?? 'admin.system.errors.request')
  }

  return payload
}

export interface SystemVersionResponse {
  version: string
}

export const getSystemVersion = async (): Promise<SystemVersionResponse> => (
  requestJson<SystemVersionResponse>('/system/version')
)
