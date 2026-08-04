// Browser-local workspace selection.
// Stored per browser (localStorage) so different frontends can open independent
// workspaces for the same TransitHub user without clobbering each other.

export const workspaceAccountIdStorageKey = 'transithub.workspace.adminAccountId'

// Header name must match backend authctx.AdminAccountIDHeader.
export const workspaceAccountIdHeader = 'X-Admin-Account-Id'

export const getSelectedWorkspaceId = (): string | null => {
  if (typeof window === 'undefined') return null
  const value = localStorage.getItem(workspaceAccountIdStorageKey)
  const trimmed = value?.trim()
  return trimmed ? trimmed : null
}

export const setSelectedWorkspaceId = (adminAccountId: string): void => {
  const trimmed = adminAccountId.trim()
  if (!trimmed) {
    clearSelectedWorkspaceId()
    return
  }
  localStorage.setItem(workspaceAccountIdStorageKey, trimmed)
}

export const clearSelectedWorkspaceId = (): void => {
  localStorage.removeItem(workspaceAccountIdStorageKey)
}
