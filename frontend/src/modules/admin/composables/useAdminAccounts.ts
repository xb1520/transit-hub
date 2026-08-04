import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import type { AdminAccount } from '../types/adminAccounts'
import {
  listAdminAccounts,
  getCurrentAdminAccount,
  switchAdminAccount,
  updateAdminAccount,
  deleteAdminAccount,
} from '../api/adminAccounts'
import { markWorkspaceActive, resetWorkspaceCheck } from '@/lib/workspaceGuard'
import {
  clearSelectedWorkspaceId,
  getSelectedWorkspaceId,
  setSelectedWorkspaceId,
} from '@/lib/workspaceSelection'
import type { DeleteAdminAccountResponse, WorkspaceDeleteConfirmation } from '../types/adminAccounts'

const accounts = ref<AdminAccount[]>([])
const currentAccount = ref<AdminAccount | null>(null)
const isLoading = ref(false)
const isSwitching = ref(false)
const isDeleting = ref(false)
const errorKey = ref('')
const noticeKey = ref('')

const adminAccountsErrorPrefix = 'admin.adminAccounts.errors.'

const toAdminAccountsErrorKey = (err: unknown, fallback: string): string => {
  if (!(err instanceof Error)) return fallback
  if (err.message.startsWith(adminAccountsErrorPrefix) || err.message === 'auth.errors.unauthorized') {
    return err.message
  }
  return fallback
}

export function useAdminAccounts() {
  const router = useRouter()

  const hasAccounts = computed(() => accounts.value.length > 0)
  const hasCurrentAccount = computed(() => currentAccount.value !== null)

  const applyLocalCurrent = (accountId: string | null) => {
    if (!accountId) {
      accounts.value = accounts.value.map(a => ({ ...a, current: false }))
      currentAccount.value = null
      return
    }
    accounts.value = accounts.value.map(a => ({
      ...a,
      current: a.id === accountId,
    }))
    currentAccount.value = accounts.value.find(a => a.id === accountId) ?? currentAccount.value
  }

  const loadAccounts = async () => {
    isLoading.value = true
    errorKey.value = ''
    try {
      const listed = await listAdminAccounts()
      accounts.value = listed
      // Backend already marks current from the browser-local header when present.
      // If the local selection is stale/missing, sync it from the list response.
      const listedCurrent = listed.find(a => a.current)
      const localId = getSelectedWorkspaceId()
      if (listedCurrent) {
        setSelectedWorkspaceId(listedCurrent.id)
        currentAccount.value = listedCurrent
      } else if (localId && !listed.some(a => a.id === localId)) {
        clearSelectedWorkspaceId()
        currentAccount.value = null
      }
    } catch (err) {
      errorKey.value = err instanceof Error ? err.message : 'admin.adminAccounts.errors.request'
    } finally {
      isLoading.value = false
    }
  }

  const loadCurrentAccount = async (): Promise<boolean> => {
    try {
      currentAccount.value = await getCurrentAdminAccount()
      if (currentAccount.value) {
        // Stick this browser to the resolved workspace so later DB default
        // changes from other browsers do not hijack this frontend.
        setSelectedWorkspaceId(currentAccount.value.id)
      }
      return true
    } catch {
      currentAccount.value = null
      return false
    }
  }

  const switchAccount = async (id: string) => {
    isSwitching.value = true
    errorKey.value = ''
    try {
      // Already the active workspace in this browser: skip the shared-server
      // switch write and just enter the admin console.
      if (currentAccount.value?.id === id || accounts.value.some(a => a.id === id && a.current)) {
        setSelectedWorkspaceId(id)
        applyLocalCurrent(id)
        markWorkspaceActive()
        await router.push('/admin')
        return
      }

      currentAccount.value = await switchAdminAccount(id)
      setSelectedWorkspaceId(id)
      applyLocalCurrent(id)
      markWorkspaceActive()
      await router.push('/admin')
    } catch (err) {
      // 切换失败时重置 workspace 缓存，下次导航会重新验证后端状态，
      // 防止前端缓存一个实际已失效的 workspace。
      resetWorkspaceCheck()
      errorKey.value = err instanceof Error ? err.message : 'admin.adminAccounts.errors.request'
    } finally {
      isSwitching.value = false
    }
  }

  const renameAccount = async (id: string, displayName: string): Promise<AdminAccount> => {
    errorKey.value = ''
    const name = displayName.trim()
    if (!name) {
      const nextErrorKey = 'admin.adminAccounts.errors.nameRequired'
      errorKey.value = nextErrorKey
      throw new Error(nextErrorKey)
    }
    try {
      const wasCurrent = accounts.value.some(a => a.id === id && a.current)
        || currentAccount.value?.id === id
      const updated = {
        ...await updateAdminAccount(id, name),
        current: wasCurrent,
      }
      accounts.value = accounts.value.map(a => (a.id === id ? updated : a))
      if (currentAccount.value?.id === id) {
        currentAccount.value = updated
      }
      return updated
    } catch (err) {
      const nextErrorKey = toAdminAccountsErrorKey(err, 'admin.adminAccounts.errors.request')
      errorKey.value = nextErrorKey
      throw new Error(nextErrorKey)
    }
  }

  const deleteAccount = async (
    id: string,
    confirmation: WorkspaceDeleteConfirmation,
  ): Promise<DeleteAdminAccountResponse> => {
    isDeleting.value = true
    errorKey.value = ''
    noticeKey.value = ''
    const deletedCurrent = currentAccount.value?.id === id || accounts.value.some(account => account.id === id && account.current)

    try {
      const response = await deleteAdminAccount(id, confirmation)
      const remainingAccounts = accounts.value.filter(account => account.id !== response.deletedId)

      if (deletedCurrent) {
        // This browser lost its active workspace: adopt the server fallback
        // (or clear selection when no workspaces remain).
        if (response.hasCurrent && response.currentAdminAccountId) {
          accounts.value = remainingAccounts.map(account => ({
            ...account,
            current: account.id === response.currentAdminAccountId,
          }))
          currentAccount.value = accounts.value.find(account => account.id === response.currentAdminAccountId) ?? null
          setSelectedWorkspaceId(response.currentAdminAccountId)
          resetWorkspaceCheck()
          markWorkspaceActive()
          await router.push('/admin')
        } else {
          accounts.value = remainingAccounts.map(account => ({
            ...account,
            current: false,
          }))
          currentAccount.value = null
          clearSelectedWorkspaceId()
          resetWorkspaceCheck()
          if (router.currentRoute.value.name !== 'AdminAccounts') {
            await router.push('/admin/accounts')
          }
        }
      } else {
        // Deleted a non-active workspace for this browser: keep local selection.
        const localId = getSelectedWorkspaceId()
        accounts.value = remainingAccounts.map(account => ({
          ...account,
          current: localId != null && account.id === localId,
        }))
        if (localId) {
          currentAccount.value = accounts.value.find(account => account.id === localId) ?? currentAccount.value
        }
      }

      if (response.cleanupPending) {
        noticeKey.value = 'admin.adminAccounts.delete.cleanupPending'
      }

      return response
    } catch (err) {
      const nextErrorKey = toAdminAccountsErrorKey(err, 'admin.adminAccounts.errors.deleteFailed')
      errorKey.value = nextErrorKey
      throw new Error(nextErrorKey)
    } finally {
      isDeleting.value = false
    }
  }

  return {
    accounts,
    currentAccount,
    isLoading,
    isSwitching,
    isDeleting,
    errorKey,
    noticeKey,
    hasAccounts,
    hasCurrentAccount,
    loadAccounts,
    loadCurrentAccount,
    switchAccount,
    renameAccount,
    deleteAccount,
  }
}
