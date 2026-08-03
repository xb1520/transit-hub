<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  AlertCircle,
  ArrowDown,
  ArrowUp,
  BarChart3,
  ChevronLeft,
  ChevronRight,
  Loader2,
  RefreshCw,
  Search,
  Users,
  X,
} from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  listSiteUsers,
  listSiteUserTopups,
  markSiteUserTopup,
  searchSiteUserCandidates,
  type SiteUserItem,
  type SiteUserTopupCandidate,
} from '../api/siteUsers'
import { getDashboardAdminStatus } from '../api/dashboardAdmin'
import SiteUserUsageModal from '../components/dashboard/SiteUserUsageModal.vue'

const { t } = useI18n()
const route = useRoute()

const adminReady = ref(false)
const checkingAdmin = ref(true)

const users = ref<SiteUserItem[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const pages = ref(1)
const pageJumpDraft = ref('')
const searchDraft = ref('')
const searchFilter = ref('')
const loadingUsers = ref(false)
const listMessageKey = ref('')
const listErrorKey = ref('')

const SORT_KEY = 'transit-hub:site-users-sort'
const sortOptions = [
  { value: 'balance_cny', labelKey: 'admin.siteUsers.sort.balanceCny' },
  { value: 'recharge', labelKey: 'admin.siteUsers.sort.recharge' },
  { value: 'gift', labelKey: 'admin.siteUsers.sort.gift' },
  { value: 'rebate', labelKey: 'admin.siteUsers.sort.rebate' },
  { value: 'today_tokens', labelKey: 'admin.siteUsers.sort.todayTokens' },
  { value: 'today_cost', labelKey: 'admin.siteUsers.sort.todayCost' },
  { value: 'total_tokens', labelKey: 'admin.siteUsers.sort.totalTokens' },
  { value: 'total_cost', labelKey: 'admin.siteUsers.sort.totalCost' },
  { value: 'last_used_at', labelKey: 'admin.siteUsers.sort.lastUsed' },
  { value: 'id', labelKey: 'admin.siteUsers.sort.id' },
] as const
type SortByValue = (typeof sortOptions)[number]['value']

const usageSortKeys = new Set(['today_tokens', 'today_cost', 'total_tokens', 'total_cost'])

const readSortPrefs = (): { by: SortByValue; order: 'asc' | 'desc' } => {
  try {
    const raw = window.localStorage.getItem(SORT_KEY)
    if (raw) {
      const parsed = JSON.parse(raw) as { by?: string; order?: string }
      const by = sortOptions.some((o) => o.value === parsed.by)
        ? (parsed.by as SortByValue)
        : 'balance_cny'
      const order = parsed.order === 'asc' ? 'asc' : 'desc'
      return { by, order }
    }
  } catch { /* ignore */ }
  return { by: 'balance_cny', order: 'desc' }
}
const sortPrefs = readSortPrefs()
const sortBy = ref<SortByValue>(sortPrefs.by)
const sortOrder = ref<'asc' | 'desc'>(sortPrefs.order)
const persistSort = () => {
  try {
    window.localStorage.setItem(SORT_KEY, JSON.stringify({ by: sortBy.value, order: sortOrder.value }))
  } catch { /* ignore */ }
}

/** 用量详情弹窗 */
const usageOpen = ref(false)
const usageUser = ref<SiteUserItem | null>(null)

/** 隐藏站点用户余额里配置的排除用户（含排除 admin 时的管理员） */
const HIDE_EXCLUDED_KEY = 'transit-hub:site-users-hide-excluded'
const readHideExcluded = (): boolean => {
  try {
    const raw = window.localStorage.getItem(HIDE_EXCLUDED_KEY)
    if (raw === '0' || raw === 'false') return false
    if (raw === '1' || raw === 'true') return true
  } catch { /* ignore */ }
  return false // 默认显示全部，便于标记流水
}
const hideExcludedUsers = ref(readHideExcluded())
const toggleHideExcluded = () => {
  hideExcludedUsers.value = !hideExcludedUsers.value
  try {
    window.localStorage.setItem(HIDE_EXCLUDED_KEY, hideExcludedUsers.value ? '1' : '0')
  } catch { /* ignore */ }
  page.value = 1
  clearSelection()
  void loadUsers()
}

// typeahead
const candidates = ref<SiteUserItem[]>([])
const candidatesOpen = ref(false)
const candidatesLoading = ref(false)
let searchTimer: ReturnType<typeof setTimeout> | null = null

// detail panel
const selectedUser = ref<SiteUserItem | null>(null)
const topups = ref<SiteUserTopupCandidate[]>([])
const topupPage = ref(1)
const topupPageSize = ref(20)
const topupTotal = ref(0)
const topupLoading = ref(false)
const topupErrorKey = ref('')
const topupMessageKey = ref('')
/** 按流水 id 锁定，不阻塞其它行连续点击 */
const markingIds = ref<Record<string, true>>({})
const platformLifetime = ref<number | null>(null)
let topupReloadTimer: ReturnType<typeof setTimeout> | null = null
/** 切换用户时递增，丢弃过期的 topups 响应，避免串数据 */
let topupRequestSeq = 0
const detailListSum = ref(0)
const markedRecharge = ref(0)
const markedGift = ref(0)
const markedRebate = ref(0)

const resetTopupPanel = () => {
  topups.value = []
  topupPage.value = 1
  topupTotal.value = 0
  platformLifetime.value = null
  detailListSum.value = 0
  markedRecharge.value = 0
  markedGift.value = 0
  markedRebate.value = 0
  topupErrorKey.value = ''
  topupMessageKey.value = ''
  markingIds.value = {}
}

const clearSelection = () => {
  topupRequestSeq += 1
  selectedUser.value = null
  resetTopupPanel()
  topupLoading.value = false
}

const siteRate = ref(1)

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
}

const toCny = (platform: number | null | undefined) => {
  if (platform == null || Number.isNaN(platform)) return null
  return platform * (siteRate.value > 0 ? siteRate.value : 1)
}

const totalPages = computed(() => Math.max(1, pages.value || Math.ceil(total.value / pageSize.value) || 1))
const topupTotalPages = computed(() => Math.max(1, Math.ceil(topupTotal.value / topupPageSize.value) || 1))

const checkAdmin = async () => {
  checkingAdmin.value = true
  try {
    const status = await getDashboardAdminStatus()
    adminReady.value = !!status.authenticated
  } catch {
    adminReady.value = false
  } finally {
    checkingAdmin.value = false
  }
}

const loadUsers = async (opts?: { silent?: boolean }) => {
  const silent = !!opts?.silent && users.value.length > 0
  if (!silent) loadingUsers.value = true
  listErrorKey.value = ''
  listMessageKey.value = ''
  try {
    const resp = await listSiteUsers({
      page: page.value,
      pageSize: pageSize.value,
      search: searchFilter.value || undefined,
      sortBy: sortBy.value,
      sortOrder: sortOrder.value,
      hideExcluded: hideExcludedUsers.value,
    })
    users.value = resp.items || []
    total.value = resp.total ?? 0
    page.value = resp.page || page.value
    pageSize.value = resp.pageSize || pageSize.value
    pages.value = resp.pages || 1
    pageJumpDraft.value = String(page.value)
    if (resp.siteRechargeRate != null && resp.siteRechargeRate > 0) {
      siteRate.value = resp.siteRechargeRate
    }
    if (resp.messageKey) listMessageKey.value = resp.messageKey

    // 从余额弹窗带入 ?user=id 时自动打开流水
    const qUser = typeof route.query.user === 'string' ? route.query.user.trim() : ''
    if (qUser && !selectedUser.value) {
      const found = users.value.find((u) => u.id === qUser)
      if (found) {
        await openUser(found)
      } else {
        await openUser({
          id: qUser,
          email: '',
          username: '',
          role: '',
          status: '',
          markedGift: 0,
          markedRebate: 0,
          markedRecharge: 0,
          nonRevenue: 0,
        })
      }
    }
  } catch (err) {
    listErrorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    loadingUsers.value = false
  }
}

const applySearch = () => {
  searchFilter.value = searchDraft.value.trim()
  page.value = 1
  candidatesOpen.value = false
  void loadUsers()
}

const onSearchInput = () => {
  const q = searchDraft.value.trim()
  if (searchTimer) clearTimeout(searchTimer)
  if (q.length < 1) {
    candidates.value = []
    candidatesOpen.value = false
    return
  }
  searchTimer = setTimeout(async () => {
    candidatesLoading.value = true
    try {
      const resp = await searchSiteUserCandidates(q, 10)
      candidates.value = resp.items || []
      candidatesOpen.value = true
    } catch {
      candidates.value = []
    } finally {
      candidatesLoading.value = false
    }
  }, 280)
}

const pickCandidate = (user: SiteUserItem) => {
  searchDraft.value = user.email || user.id
  candidatesOpen.value = false
  void openUser(user)
}

const goPage = (next: number) => {
  if (next < 1 || next > totalPages.value || next === page.value) return
  page.value = next
  pageJumpDraft.value = String(next)
  void loadUsers()
}

const jumpToPage = () => {
  const n = Number.parseInt(pageJumpDraft.value.trim(), 10)
  if (!Number.isFinite(n)) {
    pageJumpDraft.value = String(page.value)
    return
  }
  const clamped = Math.min(totalPages.value, Math.max(1, n))
  goPage(clamped)
}

const toggleSortOrder = () => {
  sortOrder.value = sortOrder.value === 'desc' ? 'asc' : 'desc'
  persistSort()
  page.value = 1
  void loadUsers()
}

const setSortBy = (by: SortByValue) => {
  if (sortBy.value === by) {
    toggleSortOrder()
    return
  }
  sortBy.value = by
  // 金额类默认降序，ID 默认升序
  sortOrder.value = by === 'id' ? 'asc' : 'desc'
  persistSort()
  page.value = 1
  void loadUsers()
}

const openUser = async (user: SiteUserItem) => {
  // 已选中同一用户则不重复拉；否则立刻切选中并清空右侧，避免旧用户闪现
  if (selectedUser.value?.id === user.id && topups.value.length > 0 && !topupLoading.value) {
    return
  }
  topupRequestSeq += 1
  const seq = topupRequestSeq
  selectedUser.value = { ...user }
  resetTopupPanel()
  topupLoading.value = true
  await loadTopups({ seq, forUserId: user.id })
}

const openUsage = (user: SiteUserItem, ev?: Event) => {
  ev?.stopPropagation()
  usageUser.value = user
  usageOpen.value = true
}

const closeUsage = () => {
  usageOpen.value = false
  usageUser.value = null
}

const formatRelativeTime = (iso?: string | null) => {
  if (!iso) return t('admin.siteUsers.neverUsed')
  try {
    const d = new Date(iso)
    if (Number.isNaN(d.getTime())) return iso
    return d.toLocaleString()
  } catch {
    return iso
  }
}

/** tokens 缩写：1.2K / 3.4M / 1.1B */
const formatTokens = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n) || n < 0) return '—'
  const abs = Math.abs(n)
  const trim = (v: number) => {
    const s = v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)
    return s.replace(/\.?0+$/, '')
  }
  if (abs >= 1e9) return `${trim(n / 1e9)}B`
  if (abs >= 1e6) return `${trim(n / 1e6)}M`
  if (abs >= 1e3) return `${trim(n / 1e3)}K`
  return String(Math.round(n))
}

const usageMetricLine = (u: SiteUserItem) => {
  switch (sortBy.value) {
    case 'today_tokens':
      return `${t('admin.siteUsers.sort.todayTokens')} ${formatTokens(u.todayTokens || 0)}`
    case 'today_cost':
      return `${t('admin.siteUsers.sort.todayCost')} ¥${formatMoney(toCny(u.todayCost || 0))} / $${formatMoney(u.todayCost || 0)}`
    case 'total_tokens':
      return `${t('admin.siteUsers.sort.totalTokens')} ${formatTokens(u.totalTokens || 0)}`
    case 'total_cost':
      return `${t('admin.siteUsers.sort.totalCost')} ¥${formatMoney(toCny(u.totalCost || 0))} / $${formatMoney(u.totalCost || 0)}`
    default:
      return ''
  }
}

const closeDetail = () => {
  clearSelection()
}

const loadTopups = async (opts?: { silent?: boolean; seq?: number; forUserId?: string }) => {
  const uid = opts?.forUserId || selectedUser.value?.id
  if (!uid) return
  const seq = opts?.seq ?? ++topupRequestSeq
  const silent = !!opts?.silent && topups.value.length > 0 && selectedUser.value?.id === uid
  if (!silent) topupLoading.value = true
  topupErrorKey.value = ''
  topupMessageKey.value = ''
  try {
    const resp = await listSiteUserTopups(uid, {
      page: topupPage.value,
      pageSize: topupPageSize.value,
    })
    // 用户已切换：丢弃过期响应，防止覆盖成上一个用户
    if (seq !== topupRequestSeq || selectedUser.value?.id !== uid) return
    topups.value = resp.items || []
    topupTotal.value = resp.total ?? 0
    topupPage.value = resp.page || topupPage.value
    platformLifetime.value = resp.platformLifetime ?? null
    detailListSum.value = resp.detailListSum ?? 0
    markedRecharge.value = resp.markedRecharge ?? 0
    markedGift.value = resp.markedGift ?? 0
    markedRebate.value = resp.markedRebate ?? 0
    if (resp.siteRechargeRate != null && resp.siteRechargeRate > 0) {
      siteRate.value = resp.siteRechargeRate
    }
    const patch = {
      markedRecharge: resp.markedRecharge ?? 0,
      markedGift: resp.markedGift ?? 0,
      markedRebate: resp.markedRebate ?? 0,
      nonRevenue: (resp.markedGift ?? 0) + (resp.markedRebate ?? 0),
    }
    if (selectedUser.value?.id === uid) {
      if (resp.user) {
        selectedUser.value = { ...selectedUser.value, ...resp.user, ...patch }
      } else {
        selectedUser.value = { ...selectedUser.value, ...patch }
      }
    }
    const idx = users.value.findIndex((u) => u.id === uid)
    if (idx >= 0) {
      users.value[idx] = { ...users.value[idx], ...(resp.user || {}), ...patch }
    }
    if (resp.messageKey) topupMessageKey.value = resp.messageKey
  } catch (err) {
    if (seq !== topupRequestSeq || selectedUser.value?.id !== uid) return
    topupErrorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    if (seq === topupRequestSeq) topupLoading.value = false
  }
}

const goTopupPage = (next: number) => {
  if (next < 1 || next > topupTotalPages.value || next === topupPage.value) return
  const uid = selectedUser.value?.id
  if (!uid) return
  topupPage.value = next
  void loadTopups({ forUserId: uid })
}

const tagLabel = (tag: string, amount?: number) => {
  // 负向：同 tag 展示为退款/收回，避免与正向充值/赠送混淆
  if (amount != null && amount < 0) {
    if (tag === 'recharge') return t('admin.siteUsers.tags.rechargeRefund')
    if (tag === 'gift') return t('admin.siteUsers.tags.giftClawback')
    if (tag === 'rebate') return t('admin.siteUsers.tags.rebateClawback')
  }
  const key = `admin.siteUsers.tags.${tag}`
  const translated = t(key)
  return translated === key ? tag : translated
}

const tagActionLabel = (tag: 'recharge' | 'gift' | 'rebate', amount: number) => {
  if (amount < 0) {
    if (tag === 'recharge') return t('admin.siteUsers.tags.rechargeRefund')
    if (tag === 'gift') return t('admin.siteUsers.tags.giftClawback')
    return t('admin.siteUsers.tags.rebateClawback')
  }
  return tagLabel(tag)
}

const tagChipClass = (tag: string) => {
  switch (tag) {
    case 'recharge':
      return 'bg-sky-500/15 text-sky-700 ring-1 ring-inset ring-sky-500/30 dark:text-sky-300'
    case 'gift':
      return 'bg-emerald-500/15 text-emerald-700 ring-1 ring-inset ring-emerald-500/30 dark:text-emerald-300'
    case 'rebate':
      return 'bg-amber-500/15 text-amber-800 ring-1 ring-inset ring-amber-500/35 dark:text-amber-300'
    default:
      return 'bg-muted text-muted-foreground'
  }
}

const tagBtnClass = (tag: 'recharge' | 'gift' | 'rebate', current?: string) => {
  const selected = current === tag
  const base = 'h-7 min-w-[2.75rem] rounded-lg px-2 text-[11px] font-semibold transition-all border'
  if (tag === 'recharge') {
    return selected
      ? `${base} border-sky-500 bg-sky-500 text-white shadow-sm shadow-sky-500/25`
      : `${base} border-sky-500/25 bg-sky-500/5 text-sky-700 hover:bg-sky-500/15 dark:text-sky-300`
  }
  if (tag === 'gift') {
    return selected
      ? `${base} border-emerald-500 bg-emerald-500 text-white shadow-sm shadow-emerald-500/25`
      : `${base} border-emerald-500/25 bg-emerald-500/5 text-emerald-700 hover:bg-emerald-500/15 dark:text-emerald-300`
  }
  return selected
    ? `${base} border-amber-500 bg-amber-500 text-white shadow-sm shadow-amber-500/25`
    : `${base} border-amber-500/30 bg-amber-500/5 text-amber-800 hover:bg-amber-500/15 dark:text-amber-300`
}

const isMarking = (platformId: string) => !!markingIds.value[platformId]

const setMarking = (platformId: string, on: boolean) => {
  const next = { ...markingIds.value }
  if (on) next[platformId] = true
  else delete next[platformId]
  markingIds.value = next
}

const recomputeLocalTopupSums = () => {
  let recharge = 0
  let gift = 0
  let rebate = 0
  for (const row of topups.value) {
    const amt = row.amountPlatform || 0
    if (row.tag === 'recharge') recharge += amt
    else if (row.tag === 'gift') gift += amt
    else if (row.tag === 'rebate') rebate += amt
  }
  markedRecharge.value = recharge
  markedGift.value = gift
  markedRebate.value = rebate
}

const scheduleTopupReload = () => {
  if (topupReloadTimer) clearTimeout(topupReloadTimer)
  const uid = selectedUser.value?.id
  topupReloadTimer = setTimeout(() => {
    topupReloadTimer = null
    if (Object.keys(markingIds.value).length > 0) {
      scheduleTopupReload()
      return
    }
    if (uid && selectedUser.value?.id === uid) {
      void loadTopups({ silent: true, forUserId: uid })
    }
    void loadUsers({ silent: true })
  }, 280)
}

const applyTag = async (item: SiteUserTopupCandidate, tag: 'recharge' | 'gift' | 'rebate') => {
  if (!selectedUser.value) return
  const uid = selectedUser.value.id
  if (item.tag === tag) return
  if (isMarking(item.platformId)) return
  topupErrorKey.value = ''
  const prevTag = item.tag
  item.tag = tag
  recomputeLocalTopupSums()
  setMarking(item.platformId, true)
  try {
    await markSiteUserTopup(uid, {
      platformRecordId: item.platformId,
      tag,
      amountPlatform: item.amountPlatform,
      note: item.note,
      createdAt: item.createdAt || undefined,
    })
    if (selectedUser.value?.id === uid) scheduleTopupReload()
  } catch (err) {
    if (selectedUser.value?.id === uid) {
      item.tag = prevTag
      recomputeLocalTopupSums()
      topupErrorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
    }
  } finally {
    setMarking(item.platformId, false)
  }
}

const formatTime = (iso?: string | null) => {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

const userDisplay = (u: SiteUserItem) => u.email || u.username || u.id

onMounted(async () => {
  await checkAdmin()
  if (adminReady.value) await loadUsers()
})

watch(adminReady, (ready) => {
  if (ready) void loadUsers()
})
</script>

<template>
  <div class="flex h-full min-h-0 flex-col gap-4">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1 class="text-xl font-semibold text-foreground">{{ t('admin.siteUsers.title') }}</h1>
        <p class="mt-1 text-sm text-muted-foreground">{{ t('admin.siteUsers.subtitle') }}</p>
      </div>
      <div v-if="adminReady" class="flex flex-wrap items-center gap-3">
        <button
          type="button"
          role="switch"
          :aria-checked="hideExcludedUsers"
          class="inline-flex items-center gap-2 rounded-lg border border-border/60 bg-card px-3 py-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground"
          @click="toggleHideExcluded"
        >
          <span
            class="relative h-5 w-9 shrink-0 rounded-full transition-colors"
            :class="hideExcludedUsers ? 'bg-primary' : 'bg-muted'"
          >
            <span
              class="absolute top-0.5 left-0.5 h-4 w-4 rounded-full bg-white shadow transition-transform"
              :class="hideExcludedUsers ? 'translate-x-4' : 'translate-x-0'"
            />
          </span>
          {{ hideExcludedUsers ? t('admin.siteUsers.hideExcludedOn') : t('admin.siteUsers.hideExcludedOff') }}
        </button>
        <Button
          variant="secondary"
          size="sm"
          :disabled="loadingUsers"
          @click="loadUsers"
        >
          <RefreshCw class="mr-1.5 h-4 w-4" :class="loadingUsers ? 'animate-spin' : ''" />
          {{ t('admin.siteUsers.refresh') }}
        </Button>
      </div>
    </div>

    <div v-if="checkingAdmin" class="flex flex-1 items-center justify-center text-muted-foreground">
      <Loader2 class="h-6 w-6 animate-spin" />
    </div>

    <div
      v-else-if="!adminReady"
      class="flex flex-1 flex-col items-center justify-center gap-3 rounded-2xl border border-border/60 bg-card p-10 text-center"
    >
      <Users class="h-10 w-10 text-muted-foreground/60" />
      <p class="text-sm font-medium text-foreground">{{ t('admin.siteUsers.needAdmin') }}</p>
      <p class="max-w-md text-xs text-muted-foreground">{{ t('admin.siteUsers.needAdminHelp') }}</p>
    </div>

    <template v-else>
      <!-- Search -->
      <div class="relative max-w-xl">
        <div class="flex gap-2">
          <div class="relative flex-1">
            <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              v-model="searchDraft"
              class="pl-9"
              :placeholder="t('admin.siteUsers.searchPlaceholder')"
              @input="onSearchInput"
              @keydown.enter.prevent="applySearch"
              @focus="candidates.length && (candidatesOpen = true)"
            />
            <!-- Candidates dropdown -->
            <div
              v-if="candidatesOpen && (candidates.length || candidatesLoading)"
              class="absolute left-0 right-0 top-full z-20 mt-1 max-h-64 overflow-y-auto rounded-xl border border-border/60 bg-card shadow-xl"
            >
              <div v-if="candidatesLoading" class="flex justify-center py-4 text-muted-foreground">
                <Loader2 class="h-4 w-4 animate-spin" />
              </div>
              <button
                v-for="c in candidates"
                :key="c.id"
                type="button"
                class="flex w-full items-center justify-between gap-3 border-b border-border/30 px-3 py-2.5 text-left text-sm last:border-0 hover:bg-surface-elevated"
                @click="pickCandidate(c)"
              >
                <div class="min-w-0">
                  <div class="truncate font-medium text-foreground">{{ userDisplay(c) }}</div>
                  <div class="truncate text-xs text-muted-foreground">
                    ID {{ c.id }}
                    <span v-if="c.username && c.email"> · {{ c.username }}</span>
                    · {{ c.role || '—' }} · {{ c.status || '—' }}
                  </div>
                  <div v-if="c.notes" class="mt-0.5 truncate text-xs text-foreground/80" :title="c.notes">
                    {{ t('admin.siteUsers.notes') }}：{{ c.notes }}
                  </div>
                </div>
                <div class="shrink-0 font-mono text-xs text-muted-foreground">
                  {{ formatMoney(c.balance) }}
                </div>
              </button>
            </div>
          </div>
          <Button @click="applySearch">{{ t('admin.siteUsers.search') }}</Button>
        </div>
      </div>

      <p v-if="listMessageKey" class="text-sm text-muted-foreground">{{ t(listMessageKey) }}</p>
      <p v-if="listErrorKey" class="flex items-center gap-2 text-sm text-destructive">
        <AlertCircle class="h-4 w-4" />{{ t(listErrorKey) }}
      </p>

      <div class="grid min-h-0 flex-1 gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
        <!-- User list -->
        <div class="flex min-h-0 flex-col overflow-hidden rounded-2xl border border-border/60 bg-card">
          <div class="space-y-2 border-b border-border/40 px-4 py-3">
            <div class="flex items-center justify-between gap-2">
              <div class="flex items-center gap-2">
                <span class="text-sm font-medium">{{ t('admin.siteUsers.listTitle') }}</span>
                <span class="text-xs text-muted-foreground">{{ t('admin.siteUsers.total', { count: total }) }}</span>
              </div>
              <button
                type="button"
                class="inline-flex h-7 items-center gap-1 rounded-md px-2 text-[11px] text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground"
                :title="sortOrder === 'asc' ? t('admin.siteUsers.sort.asc') : t('admin.siteUsers.sort.desc')"
                @click="toggleSortOrder"
              >
                <ArrowUp v-if="sortOrder === 'asc'" class="h-3.5 w-3.5" />
                <ArrowDown v-else class="h-3.5 w-3.5" />
                {{ sortOrder === 'asc' ? t('admin.siteUsers.sort.asc') : t('admin.siteUsers.sort.desc') }}
              </button>
            </div>
            <div class="flex flex-wrap gap-1">
              <button
                v-for="opt in sortOptions"
                :key="opt.value"
                type="button"
                class="rounded-full px-2.5 py-0.5 text-[11px] font-medium transition-colors"
                :class="sortBy === opt.value
                  ? 'bg-primary text-primary-foreground shadow-sm'
                  : 'bg-muted/60 text-muted-foreground hover:bg-muted hover:text-foreground'"
                @click="setSortBy(opt.value)"
              >
                {{ t(opt.labelKey) }}
              </button>
            </div>
          </div>
          <div class="min-h-0 flex-1 overflow-y-auto">
            <div v-if="loadingUsers && !users.length" class="flex justify-center py-12 text-muted-foreground">
              <Loader2 class="h-6 w-6 animate-spin" />
            </div>
            <div v-else-if="!users.length" class="px-4 py-10 text-center text-sm text-muted-foreground">
              {{ t('admin.siteUsers.empty') }}
            </div>
            <ul v-else class="divide-y divide-border/30" :class="loadingUsers ? 'opacity-80' : ''">
              <li
                v-for="u in users"
                :key="u.id"
                class="cursor-pointer px-4 py-2.5 transition-colors hover:bg-surface-elevated/60"
                :class="selectedUser?.id === u.id ? 'bg-primary/5' : ''"
                @click="openUser(u)"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-1.5">
                      <div class="truncate text-sm font-medium text-foreground">{{ userDisplay(u) }}</div>
                      <button
                        type="button"
                        class="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-muted-foreground/70 hover:bg-primary/10 hover:text-primary"
                        :title="t('admin.siteUsers.usage.detailBtn')"
                        @click="openUsage(u, $event)"
                      >
                        <BarChart3 class="h-3.5 w-3.5" />
                      </button>
                    </div>
                    <div class="mt-0.5 truncate text-xs text-muted-foreground">
                      ID {{ u.id }}
                      <span v-if="u.username && u.email"> · {{ u.username }}</span>
                      · {{ u.role || '—' }} · {{ u.status || '—' }}
                    </div>
                    <div class="mt-0.5 truncate text-[11px] text-muted-foreground">
                      {{ t('admin.siteUsers.lastUsed') }}：{{ formatRelativeTime(u.lastUsedAt) }}
                    </div>
                    <div
                      v-if="u.notes"
                      class="mt-0.5 truncate text-xs text-foreground/80"
                      :title="u.notes"
                    >
                      {{ t('admin.siteUsers.notes') }}：{{ u.notes }}
                    </div>
                  </div>
                  <div class="shrink-0 text-right">
                    <div class="font-mono text-sm font-semibold text-foreground">
                      ¥{{ formatMoney(u.balanceCny ?? toCny(u.balance)) }}
                    </div>
                    <div v-if="u.balance != null" class="text-[10px] tabular-nums text-muted-foreground">
                      ${{ formatMoney(u.balance) }}
                    </div>
                    <div class="mt-0.5 text-[10px] leading-4 text-sky-700 dark:text-sky-300">
                      {{ t('admin.siteUsers.listRechargeTotal') }}
                      ¥{{ formatMoney(toCny(u.markedRecharge || 0)) }}
                      <span class="opacity-70"> / ${{ formatMoney(u.markedRecharge || 0) }}</span>
                    </div>
                    <div v-if="(u.markedGift || 0) !== 0 || (u.markedRebate || 0) !== 0" class="mt-0.5 text-[10px] leading-4 text-warning">
                      <template v-if="(u.markedGift || 0) !== 0">
                        {{ t('admin.siteUsers.tags.gift') }} ¥{{ formatMoney(toCny(u.markedGift)) }}
                      </template>
                      <template v-if="(u.markedGift || 0) !== 0 && (u.markedRebate || 0) !== 0"> · </template>
                      <template v-if="(u.markedRebate || 0) !== 0">
                        {{ t('admin.siteUsers.tags.rebate') }} ¥{{ formatMoney(toCny(u.markedRebate)) }}
                      </template>
                    </div>
                    <div
                      v-if="usageSortKeys.has(sortBy) && usageMetricLine(u)"
                      class="mt-0.5 text-[10px] leading-4 text-violet-700 dark:text-violet-300"
                    >
                      {{ usageMetricLine(u) }}
                    </div>
                  </div>
                </div>
              </li>
            </ul>
          </div>
          <div class="flex items-center justify-center gap-3 border-t border-border/40 px-3 py-2">
            <button
              type="button"
              class="inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground disabled:opacity-30"
              :disabled="page <= 1 || loadingUsers"
              @click="goPage(page - 1)"
            >
              <ChevronLeft class="h-4 w-4" />
            </button>
            <div class="flex items-center gap-1 text-xs text-muted-foreground">
              <span>{{ t('admin.siteUsers.pageLabel') }}</span>
              <input
                v-model="pageJumpDraft"
                type="text"
                inputmode="numeric"
                class="h-6 w-9 border-0 border-b border-border/60 bg-transparent p-0 text-center text-xs font-medium tabular-nums text-foreground outline-none focus:border-primary"
                :disabled="loadingUsers"
                @keydown.enter.prevent="jumpToPage"
                @blur="jumpToPage"
              >
              <span>/ {{ totalPages }}</span>
            </div>
            <button
              type="button"
              class="inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground disabled:opacity-30"
              :disabled="page >= totalPages || loadingUsers"
              @click="goPage(page + 1)"
            >
              <ChevronRight class="h-4 w-4" />
            </button>
          </div>
        </div>

        <!-- Topup detail -->
        <div class="flex min-h-0 flex-col overflow-hidden rounded-2xl border border-border/60 bg-card">
          <template v-if="!selectedUser">
            <div class="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center text-muted-foreground">
              <Users class="h-8 w-8 opacity-40" />
              <p class="text-sm">{{ t('admin.siteUsers.selectHint') }}</p>
            </div>
          </template>
          <template v-else>
            <div class="flex items-start justify-between gap-3 border-b border-border/40 px-4 py-3">
              <div class="min-w-0">
                <div class="truncate text-sm font-semibold">{{ userDisplay(selectedUser) }}</div>
                <div class="mt-0.5 text-xs text-muted-foreground">
                  ID {{ selectedUser.id }} · {{ t('admin.siteUsers.balance') }}
                  ¥{{ formatMoney(selectedUser.balanceCny ?? toCny(selectedUser.balance)) }}
                  <span class="text-muted-foreground/80"> / ${{ formatMoney(selectedUser.balance) }}</span>
                </div>
                <div v-if="selectedUser.notes" class="mt-0.5 truncate text-xs text-foreground/80" :title="selectedUser.notes">
                  {{ t('admin.siteUsers.notes') }}：{{ selectedUser.notes }}
                </div>
              </div>
              <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="closeDetail">
                <X class="h-4 w-4" />
              </button>
            </div>

            <div v-if="topupLoading && !topups.length" class="flex flex-1 flex-col items-center justify-center gap-2 py-16 text-muted-foreground">
              <Loader2 class="h-6 w-6 animate-spin" />
              <p class="text-xs">{{ t('admin.siteUsers.loadingTopups') }}</p>
            </div>
            <div v-else class="space-y-3 overflow-y-auto px-4 py-3">
              <p class="text-[11px] leading-4 text-muted-foreground">{{ t('admin.siteUsers.topupHelp') }}</p>

              <div class="grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
                <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
                  <div class="text-muted-foreground">{{ t('admin.siteUsers.platformLifetime') }}</div>
                  <div class="mt-0.5 font-semibold tabular-nums">¥{{ formatMoney(toCny(platformLifetime)) }}</div>
                  <div class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">${{ formatMoney(platformLifetime) }}</div>
                </div>
                <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
                  <div class="text-muted-foreground">{{ t('admin.siteUsers.detailSum') }}</div>
                  <div class="mt-0.5 font-semibold tabular-nums">¥{{ formatMoney(toCny(detailListSum)) }}</div>
                  <div class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">${{ formatMoney(detailListSum) }}</div>
                </div>
                <div class="rounded-lg border border-sky-500/25 bg-sky-500/5 px-2.5 py-2">
                  <div class="text-sky-700 dark:text-sky-300">{{ t('admin.siteUsers.tags.recharge') }}</div>
                  <div class="mt-0.5 font-semibold tabular-nums text-sky-700 dark:text-sky-300">¥{{ formatMoney(toCny(markedRecharge)) }}</div>
                  <div class="mt-0.5 text-[10px] tabular-nums text-sky-700/70">${{ formatMoney(markedRecharge) }}</div>
                </div>
                <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
                  <div class="text-muted-foreground">{{ t('admin.siteUsers.giftRebate') }}</div>
                  <div class="mt-0.5 font-semibold tabular-nums">¥{{ formatMoney(toCny(markedGift + markedRebate)) }}</div>
                  <div class="mt-0.5 text-[10px] text-muted-foreground">
                    {{ t('admin.siteUsers.tags.gift') }} ¥{{ formatMoney(toCny(markedGift)) }}
                    · {{ t('admin.siteUsers.tags.rebate') }} ¥{{ formatMoney(toCny(markedRebate)) }}
                  </div>
                </div>
              </div>

              <p v-if="topupMessageKey" class="text-xs text-muted-foreground">{{ t(topupMessageKey) }}</p>
              <p v-if="topupErrorKey" class="text-xs text-destructive">{{ t(topupErrorKey) }}</p>

              <div v-if="!topups.length" class="py-8 text-center text-sm text-muted-foreground">
                {{ t('admin.siteUsers.topupEmpty') }}
              </div>
              <ul v-else class="divide-y divide-border/30 rounded-xl border border-border/50" :class="topupLoading ? 'opacity-90' : ''">
                <li v-for="item in topups" :key="item.platformId" class="px-3 py-2.5 text-sm">
                  <div class="flex flex-wrap items-start justify-between gap-2">
                    <div class="min-w-0">
                      <div
                        class="font-semibold tabular-nums"
                        :class="item.amountPlatform < 0 ? 'text-destructive' : ''"
                      >
                        {{ item.amountPlatform < 0 ? '' : '+' }}¥{{ formatMoney(toCny(item.amountPlatform)) }}
                        <span
                          class="ml-1.5 text-xs font-normal"
                          :class="item.amountPlatform < 0 ? 'text-destructive/80' : 'text-muted-foreground'"
                        >${{ formatMoney(item.amountPlatform) }}</span>
                      </div>
                      <div class="mt-0.5 text-xs text-muted-foreground">
                        {{ formatTime(item.createdAt) }}
                        <span v-if="item.platformType"> · {{ item.platformType }}</span>
                        <span v-if="item.businessDate"> · {{ item.businessDate }}</span>
                      </div>
                      <div v-if="item.note" class="mt-0.5 truncate text-xs text-muted-foreground">{{ item.note }}</div>
                      <div v-if="item.tag" class="mt-1.5 text-[11px]">
                        <span class="inline-flex rounded-md px-1.5 py-0.5 font-semibold" :class="tagChipClass(item.tag)">
                          {{ tagLabel(item.tag, item.amountPlatform) }}
                        </span>
                      </div>
                      <div v-else-if="item.suggestedTag" class="mt-1 text-[11px] text-muted-foreground">
                        {{ t('admin.siteUsers.suggested', { tag: tagLabel(item.suggestedTag, item.amountPlatform) }) }}
                      </div>
                    </div>
                    <div class="flex flex-wrap gap-1.5">
                      <button
                        type="button"
                        :class="tagBtnClass('recharge', item.tag)"
                        :disabled="isMarking(item.platformId)"
                        @click="applyTag(item, 'recharge')"
                      >
                        {{ tagActionLabel('recharge', item.amountPlatform) }}
                      </button>
                      <button
                        type="button"
                        :class="tagBtnClass('gift', item.tag)"
                        :disabled="isMarking(item.platformId)"
                        @click="applyTag(item, 'gift')"
                      >
                        {{ tagActionLabel('gift', item.amountPlatform) }}
                      </button>
                      <button
                        type="button"
                        :class="tagBtnClass('rebate', item.tag)"
                        :disabled="isMarking(item.platformId)"
                        @click="applyTag(item, 'rebate')"
                      >
                        {{ tagActionLabel('rebate', item.amountPlatform) }}
                      </button>
                    </div>
                  </div>
                </li>
              </ul>

              <div v-if="topupTotal > topupPageSize" class="flex items-center justify-between pt-1">
                <Button variant="ghost" size="sm" :disabled="topupPage <= 1 || topupLoading" @click="goTopupPage(topupPage - 1)">
                  <ChevronLeft class="h-4 w-4" />
                </Button>
                <span class="text-xs text-muted-foreground">{{ topupPage }} / {{ topupTotalPages }}</span>
                <Button variant="ghost" size="sm" :disabled="topupPage >= topupTotalPages || topupLoading" @click="goTopupPage(topupPage + 1)">
                  <ChevronRight class="h-4 w-4" />
                </Button>
              </div>
            </div>
          </template>
        </div>
      </div>
    </template>

    <SiteUserUsageModal
      :open="usageOpen"
      :user="usageUser"
      :site-recharge-rate="siteRate"
      @close="closeUsage"
    />
  </div>
</template>
