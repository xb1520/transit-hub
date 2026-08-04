import { getAccessToken } from '@/modules/auth/api/auth'
import {
  getSelectedWorkspaceId,
  workspaceAccountIdHeader,
} from '@/lib/workspaceSelection'

// Shared auth + browser-local workspace headers for admin API requests.
export const buildAuthHeaders = (): Record<string, string> => {
  const headers: Record<string, string> = {}
  const token = getAccessToken()
  if (token) {
    headers.Authorization = `Bearer ${token}`
  }
  const workspaceId = getSelectedWorkspaceId()
  if (workspaceId) {
    headers[workspaceAccountIdHeader] = workspaceId
  }
  return headers
}
