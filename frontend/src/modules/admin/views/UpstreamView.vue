<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Search, Plus, CheckCircle2, XCircle, X, Loader2, AlertCircle, Trash2, Edit2, LayoutGrid, List, RefreshCw, Settings2, Receipt, PackagePlus } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Tooltip } from '@/components/ui/tooltip'
import { getStrategySettings } from '../api/settings'
import { useUpstreamSites } from '../composables/useUpstreamSites'
import SiteSettingsModal from '../components/upstream/SiteSettingsModal.vue'
import SettlementModal from '../components/upstream/SettlementModal.vue'
import LedgerModal from '../components/upstream/LedgerModal.vue'
import SubscriptionTopupModal from '../components/upstream/SubscriptionTopupModal.vue'
import type { UpstreamGroupInfo, UpstreamMetricValue, UpstreamSite, UpstreamSiteForm, UpstreamStatus, UpstreamSubscriptionInfo } from '../types/upstream'

const { t, locale } = useI18n()

const searchQuery = ref('')
const isAddModalOpen = ref(false)
const { sites: upstreamSites, isAdding, isRefreshing, addErrorKey, connectedCount, siteSyncStates, syncingSiteIds, addSite, updateSite, deleteSite, streamRefreshSites, refreshSingleSite } = useUpstreamSites()
const deletingSiteId = ref<string | null>(null)
const deleteErrorKey = ref<string | null>(null)
const editingSiteId = ref<string | null>(null)
const refreshIntervalSeconds = ref<number | null>(null)
const remainingSeconds = ref(0)
let countdownTimer: ReturnType<typeof window.setInterval> | null = null
const nextRefreshAtStorageKey = 'transit-hub:upstream-next-refresh-at'
const viewModeStorageKey = 'transit-hub:upstream-view-mode'

type ViewMode = 'card' | 'list'
const readStoredViewMode = (): ViewMode => {
  try {
    const raw = window.localStorage.getItem(viewModeStorageKey)
    return raw === 'list' ? 'list' : 'card'
  } catch {
    return 'card'
  }
}
const viewMode = ref<ViewMode>(readStoredViewMode())
const setViewMode = (mode: ViewMode) => {
  viewMode.value = mode
  try {
    window.localStorage.setItem(viewModeStorageKey, mode)
  } catch {
    // 隐私模式等写失败时忽略
  }
}

const countdownDisplay = computed(() => {
  if (!refreshIntervalSeconds.value) return t('admin.upstream.refresh.disabled')
  return t('admin.upstream.refresh.countdown', { seconds: remainingSeconds.value })
})

const readNextRefreshAt = (): number | null => {
  const value = Number.parseInt(window.localStorage.getItem(nextRefreshAtStorageKey) ?? '', 10)
  if (!Number.isFinite(value) || value <= Date.now()) return null
  return value
}

const writeNextRefreshAt = (timestamp: number) => {
  window.localStorage.setItem(nextRefreshAtStorageKey, String(timestamp))
}

const updateRemainingSeconds = () => {
  const nextRefreshAt = readNextRefreshAt()
  remainingSeconds.value = nextRefreshAt ? Math.max(Math.ceil((nextRefreshAt - Date.now()) / 1000), 0) : 0
}

const scheduleNextRefresh = () => {
  if (!refreshIntervalSeconds.value) return
  writeNextRefreshAt(Date.now() + refreshIntervalSeconds.value * 1000)
  updateRemainingSeconds()
}

const runRefresh = async () => {
  if (isRefreshing.value) return
  await streamRefreshSites()
  scheduleNextRefresh()
}

const startCountdown = (seconds: number) => {
  refreshIntervalSeconds.value = seconds
  const nextRefreshAt = readNextRefreshAt()
  if (!nextRefreshAt || nextRefreshAt > Date.now() + seconds * 1000) scheduleNextRefresh()
  updateRemainingSeconds()
  countdownTimer = window.setInterval(() => {
    if (!refreshIntervalSeconds.value || isRefreshing.value) return
    updateRemainingSeconds()
    if (remainingSeconds.value <= 0) void runRefresh()
  }, 1000)
}

const stopCountdown = () => {
  if (countdownTimer) window.clearInterval(countdownTimer)
  countdownTimer = null
}

const loadRefreshSettings = async () => {
  try {
    const settings = await getStrategySettings()
    if (!settings.enableRefreshInterval) return
    startCountdown(Math.max(settings.refreshInterval, 60))
  } catch (error) {
    refreshIntervalSeconds.value = null
  }
}

const createEmptyForm = (): UpstreamSiteForm => ({
  name: '',
  siteUrl: '',
  platform: 'auto',
  authMode: 'password',
  account: '',
  password: '',
  accessToken: '',
  refreshToken: '',
  tokenType: 'Bearer',
  userId: '',
  rechargeRate: 1,
  remark: '',
})

const newSiteForm = ref<UpstreamSiteForm>(createEmptyForm())

watch(
  () => newSiteForm.value.platform,
  (platform) => {
    if (platform === 'newapi' && newSiteForm.value.authMode === 'token') {
      newSiteForm.value.authMode = 'password'
    } else if (platform !== 'newapi' && newSiteForm.value.authMode === 'user_key') {
      newSiteForm.value.authMode = 'password'
    }
  },
)

const handleAddSite = async () => {
  const success = editingSiteId.value
    ? await updateSite(editingSiteId.value, newSiteForm.value)
    : await addSite(newSiteForm.value)
  if (!success) return
  isAddModalOpen.value = false
  newSiteForm.value = createEmptyForm()
  editingSiteId.value = null
}

const handleEditSite = (site: UpstreamSite) => {
  editingSiteId.value = site.id
  newSiteForm.value = {
    name: site.name,
    siteUrl: site.baseUrl,
    platform: site.platform,
    authMode: 'password',
    account: site.account,
    password: '',
    accessToken: '',
    refreshToken: '',
    tokenType: 'Bearer',
    userId: '',
    rechargeRate: site.rechargeRate > 0 ? site.rechargeRate : 1,
    remark: site.remark,
  }
  isAddModalOpen.value = true
}

const closeSiteModal = () => {
  isAddModalOpen.value = false
  editingSiteId.value = null
  newSiteForm.value = createEmptyForm()
}

const requestDeleteSite = (id: string) => {
  deletingSiteId.value = id
  deleteErrorKey.value = null
}

const cancelDeleteSite = () => {
  deletingSiteId.value = null
  deleteErrorKey.value = null
}

const confirmDeleteSite = async () => {
  if (!deletingSiteId.value) return
  try {
    await deleteSite(deletingSiteId.value)
    cancelDeleteSite()
  } catch (error) {
    deleteErrorKey.value = error instanceof Error ? error.message : 'admin.upstream.errors.unknown'
  }
}

type SortKey = 'default' | 'balance' | 'todayConsume' | 'historyRecharge'
type SortDir = 'asc' | 'desc'

const sortStorageKey = 'transit-hub:upstream-sort'
const validSortKeys: SortKey[] = ['default', 'balance', 'todayConsume', 'historyRecharge']

const readStoredSort = (): { key: SortKey; dir: SortDir } => {
  try {
    const raw = window.localStorage.getItem(sortStorageKey)
    if (!raw) return { key: 'default', dir: 'desc' }
    const parsed = JSON.parse(raw) as { key?: string; dir?: string }
    const key = validSortKeys.includes(parsed.key as SortKey) ? (parsed.key as SortKey) : 'default'
    const dir: SortDir = parsed.dir === 'asc' ? 'asc' : 'desc'
    return { key, dir }
  } catch {
    return { key: 'default', dir: 'desc' }
  }
}

const storedSort = readStoredSort()
const sortKey = ref<SortKey>(storedSort.key)
const sortDir = ref<SortDir>(storedSort.dir)

const persistSort = () => {
  try {
    window.localStorage.setItem(sortStorageKey, JSON.stringify({ key: sortKey.value, dir: sortDir.value }))
  } catch {
    // 隐私模式等写失败时忽略，不影响当前会话排序。
  }
}

/** 成本口径 CNY：平台原始 value × rechargeRate；无效时返回 null 排后。 */
const costCnyValue = (site: UpstreamSite, metric: UpstreamMetricValue): number | null => {
  if (metric.value === null || !Number.isFinite(metric.value)) return null
  if (site.rechargeRate <= 0 || !Number.isFinite(site.rechargeRate)) return null
  return metric.value * site.rechargeRate
}

const toggleSort = (key: Exclude<SortKey, 'default'>) => {
  if (sortKey.value === key) {
    sortDir.value = sortDir.value === 'desc' ? 'asc' : 'desc'
  } else {
    sortKey.value = key
    sortDir.value = 'desc'
  }
  persistSort()
}

const filteredSites = computed(() => {
  const q = searchQuery.value.trim().toLowerCase()
  let list = upstreamSites.value
  if (q) {
    list = list.filter(site =>
      site.name.toLowerCase().includes(q) || site.baseUrl.toLowerCase().includes(q),
    )
  }
  if (sortKey.value === 'default') return list

  const metricOf = (site: UpstreamSite): UpstreamMetricValue => {
    if (sortKey.value === 'todayConsume') return site.metrics.todayConsume
    if (sortKey.value === 'historyRecharge') return site.metrics.historyRecharge
    return site.metrics.balance
  }
  const dir = sortDir.value === 'asc' ? 1 : -1
  return [...list].sort((a, b) => {
    const av = costCnyValue(a, metricOf(a))
    const bv = costCnyValue(b, metricOf(b))
    if (av == null && bv == null) return 0
    if (av == null) return 1
    if (bv == null) return -1
    if (av === bv) return 0
    return av > bv ? dir : -dir
  })
})

const subscriptionSummary = (site: UpstreamSite): string => {
  const subs = site.metrics.subscriptions ?? []
  if (!subs.length) return '—'
  const active = subs.filter(s => s.status === 'active')
  const first = active[0] ?? subs[0]
  const max = first.todayMaxConsumableUsd
  if (max != null) {
    return t('admin.upstream.subscriptions.listSummaryMax', {
      count: subs.length,
      max: max.toFixed(1),
    })
  }
  return t('admin.upstream.subscriptions.listSummary', { count: subs.length })
}

const isCreditLine = (site: UpstreamSite) => site.settings.settlementMode === 'credit_line'

const statusClasses: Record<UpstreamStatus, string> = {
  connecting: 'bg-primary/10 text-primary border-primary/20',
  syncing: 'bg-warning/10 text-warning border-warning/20',
  connected: 'bg-signal/10 text-signal border-signal/20',
  error: 'bg-warning/10 text-warning border-warning/20',
}

const statusLabel = (status: UpstreamStatus): string => t(`admin.upstream.status.${status}`)

const deletingSite = computed(() => upstreamSites.value.find((site) => site.id === deletingSiteId.value) ?? null)

// Groups Modal Logic
const isGroupsModalOpen = ref(false)
const selectedSiteForGroups = ref<UpstreamSite | null>(null)

const openGroupsModal = (site: UpstreamSite) => {
  selectedSiteForGroups.value = site
  isGroupsModalOpen.value = true
}

const closeGroupsModal = () => {
  isGroupsModalOpen.value = false
  selectedSiteForGroups.value = null
}

const isSiteSettingsOpen = ref(false)
const selectedSiteForSettings = ref<UpstreamSite | null>(null)

const openSiteSettings = (site: UpstreamSite) => {
  selectedSiteForSettings.value = site
  isSiteSettingsOpen.value = true
}

const closeSiteSettings = () => {
  isSiteSettingsOpen.value = false
  selectedSiteForSettings.value = null
}

const onSiteSettingsSaved = (siteId: string, settings: UpstreamSite['settings']) => {
  const site = upstreamSites.value.find(s => s.id === siteId)
  if (site) {
    site.settings = { ...site.settings, ...settings }
  }
}

const isSettlementOpen = ref(false)
const selectedSiteForSettlement = ref<UpstreamSite | null>(null)
const openSettlement = (site: UpstreamSite) => {
  selectedSiteForSettlement.value = site
  isSettlementOpen.value = true
}
const closeSettlement = () => {
  isSettlementOpen.value = false
  selectedSiteForSettlement.value = null
}
const onSettlementUpdated = async () => {
  await streamRefreshSites()
}

const isLedgerOpen = ref(false)
const selectedSiteForLedger = ref<UpstreamSite | null>(null)
const openLedger = (site: UpstreamSite) => {
  selectedSiteForLedger.value = site
  isLedgerOpen.value = true
}
const closeLedger = () => {
  isLedgerOpen.value = false
  selectedSiteForLedger.value = null
}
const onLedgerUpdated = async () => {
  // 进货流水不强制全量同步，关闭后由仪表盘下次刷新体现
}

const isSubscriptionTopupOpen = ref(false)
const selectedSiteForSubscriptionTopup = ref<UpstreamSite | null>(null)
const selectedSubscriptionForTopup = ref<UpstreamSubscriptionInfo | null>(null)
const openSubscriptionTopup = (site: UpstreamSite, sub: UpstreamSubscriptionInfo) => {
  selectedSiteForSubscriptionTopup.value = site
  selectedSubscriptionForTopup.value = sub
  isSubscriptionTopupOpen.value = true
}
const closeSubscriptionTopup = () => {
  isSubscriptionTopupOpen.value = false
  selectedSiteForSubscriptionTopup.value = null
  selectedSubscriptionForTopup.value = null
}
const onSubscriptionTopupUpdated = () => {
  // 进货记入开通/续费日；仪表盘下次刷新即可看到对应日进货
}

const groupedGroups = computed<Record<string, UpstreamGroupInfo[]>>(() => {
  if (!selectedSiteForGroups.value) return {}
  const groups = selectedSiteForGroups.value.metrics.groups
  return groups.reduce<Record<string, UpstreamGroupInfo[]>>((acc, group) => {
    const platform = group.platform ?? t('admin.upstream.fields.unknownPlatform')
    if (!acc[platform]) acc[platform] = []
    acc[platform].push(group)
    return acc
  }, {})
})

const cnyMetricDisplay = (site: UpstreamSite, metric: UpstreamMetricValue): string | null => {
  if (metric.value === null || !Number.isFinite(metric.value) || site.rechargeRate <= 0 || !Number.isFinite(site.rechargeRate)) return null
  return t('admin.upstream.currency.cnyValue', { amount: (metric.value * site.rechargeRate).toFixed(2) })
}

const usdMetricDisplay = (metric: UpstreamMetricValue): string => {
  if (metric.display.toUpperCase().includes('USD')) return metric.display
  return t('admin.upstream.currency.usdValue', { amount: metric.display })
}

const lastUpdatedDisplay = (site: UpstreamSite): string => {
  if (!site.lastSyncedAt) return t('admin.upstream.fields.notSynced')
  const value = new Date(site.lastSyncedAt)
  if (Number.isNaN(value.getTime())) return t('admin.upstream.fields.notSynced')
  return new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'short' }).format(value)
}

onMounted(() => {
  void loadRefreshSettings()
})

onBeforeUnmount(() => {
  stopCountdown()
})
</script>

<template>
  <div class="mx-auto w-full max-w-[1600px] space-y-6">
    <!-- Top Action Bar -->
    <div class="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
      <div class="flex flex-col gap-3 w-full sm:w-auto">
        <div class="relative w-full sm:w-80">
          <Search class="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-muted-foreground" />
          <input
            v-model="searchQuery"
            name="upstreamSearch"
            type="text"
            :placeholder="t('admin.upstream.searchPlaceholder')"
            :aria-label="t('admin.upstream.searchPlaceholder')"
            autocomplete="off"
            spellcheck="false"
            class="h-10 w-full rounded-lg border border-border/50 bg-surface pl-10 pr-4 text-sm text-foreground outline-none transition-[color,background-color,border-color,box-shadow] placeholder:text-muted-foreground focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/30"
          />
        </div>
        <p class="text-xs text-muted-foreground">
          {{ t('admin.upstream.summary', { connected: connectedCount, total: upstreamSites.length }) }}
        </p>
      </div>

      <div class="flex w-full flex-wrap items-center gap-2 sm:w-auto sm:justify-end">
        <div class="flex shrink-0 items-center rounded-lg border border-border/50 bg-surface p-1" role="group" :aria-label="t('admin.upstream.viewMode.list')">
          <button
            type="button"
            @click="setViewMode('list')"
            :class="{'bg-card shadow-sm text-foreground': viewMode === 'list', 'text-muted-foreground hover:text-foreground': viewMode !== 'list'}"
            class="rounded-md p-1.5 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
            :title="t('admin.upstream.viewMode.list')"
            :aria-label="t('admin.upstream.viewMode.list')"
            :aria-pressed="viewMode === 'list'"
          >
            <List class="w-4 h-4" />
          </button>
          <button
            type="button"
            @click="setViewMode('card')"
            :class="{'bg-card shadow-sm text-foreground': viewMode === 'card', 'text-muted-foreground hover:text-foreground': viewMode !== 'card'}"
            class="rounded-md p-1.5 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
            :title="t('admin.upstream.viewMode.card')"
            :aria-label="t('admin.upstream.viewMode.card')"
            :aria-pressed="viewMode === 'card'"
          >
            <LayoutGrid class="w-4 h-4" />
          </button>
        </div>
        <div class="flex h-10 items-center gap-1 rounded-xl border border-border/50 bg-surface px-2 text-xs">
          <span class="px-1 text-muted-foreground whitespace-nowrap">{{ t('admin.upstream.sort.label') }}</span>
          <button
            v-for="key in (['balance', 'todayConsume', 'historyRecharge'] as const)"
            :key="key"
            type="button"
            class="rounded-md px-2 py-1 transition-colors whitespace-nowrap"
            :class="sortKey === key ? 'bg-card text-foreground shadow-sm font-medium' : 'text-muted-foreground hover:text-foreground'"
            @click="toggleSort(key)"
          >
            {{ t(`admin.upstream.sort.${key}`) }}
            <span v-if="sortKey === key" class="ml-0.5">{{ sortDir === 'desc' ? '↓' : '↑' }}</span>
          </button>
        </div>
        <div class="hidden md:flex h-10 items-center rounded-xl border border-border/50 bg-surface px-3 text-xs text-muted-foreground whitespace-nowrap">
          {{ countdownDisplay }}
        </div>
        <Button :disabled="isRefreshing" @click="runRefresh" variant="secondary" class="h-10 flex-1 gap-2 px-4 sm:flex-none">
          <Loader2 v-if="isRefreshing" class="w-4 h-4 animate-spin" />
          <RefreshCw v-else class="w-4 h-4" />
          {{ isRefreshing ? t('admin.upstream.refresh.refreshing') : t('admin.upstream.refresh.action') }}
        </Button>
        <Button @click="isAddModalOpen = true" class="h-10 flex-1 gap-2 px-4 shadow-sm sm:flex-none">
          <Plus class="w-4 h-4" />
          {{ t('admin.upstream.addSite') }}
        </Button>
      </div>
    </div>

    <!-- Cards Grid -->
    <div v-if="viewMode === 'card'" class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4 gap-6">
      <div
        v-for="site in filteredSites"
        :key="site.id"
        class="group relative bg-card border border-border/60 rounded-2xl p-5 hover:border-primary/50 transition-colors shadow-sm hover:shadow-md"
      >
        <!-- Sync Progress Overlay -->
        <div
          v-if="siteSyncStates.get(site.id)?.phase && siteSyncStates.get(site.id)?.phase !== 'idle'"
          class="absolute inset-0 z-10 flex flex-col items-center justify-center rounded-2xl backdrop-blur-sm transition-all"
          :class="{
            'bg-background/60': siteSyncStates.get(site.id)?.phase === 'syncing',
            'bg-signal/10 dark:bg-signal/5': siteSyncStates.get(site.id)?.phase === 'done',
            'bg-destructive/10 dark:bg-destructive/5': siteSyncStates.get(site.id)?.phase === 'error',
          }"
        >
          <template v-if="siteSyncStates.get(site.id)?.phase === 'syncing'">
            <Loader2 class="h-6 w-6 animate-spin text-primary" />
            <span class="mt-2 text-sm font-medium text-foreground">{{ t('admin.upstream.syncStream.syncing') }}</span>
          </template>
          <template v-else-if="siteSyncStates.get(site.id)?.phase === 'done'">
            <CheckCircle2 class="h-6 w-6 text-signal" />
            <span class="mt-2 text-sm font-medium text-signal">{{ t('admin.upstream.syncStream.done') }}</span>
          </template>
          <template v-else-if="siteSyncStates.get(site.id)?.phase === 'error'">
            <XCircle class="h-6 w-6 text-destructive" />
            <span class="mt-2 text-sm font-medium text-destructive">{{ t('admin.upstream.syncStream.error') }}</span>
          </template>
        </div>

        <!-- Card Header -->
        <div class="flex flex-col gap-4 mb-5 border-b border-border/40 pb-4">
          <div class="flex items-start justify-between gap-2">
            <div class="flex items-center gap-3 min-w-0">
              <div :class="['w-10 h-10 rounded-xl flex items-center justify-center font-bold text-lg shrink-0', site.logoBg]">
                {{ site.logo }}
              </div>
              <div class="flex flex-col min-w-0">
                <a :href="site.baseUrl" target="_blank" rel="noopener noreferrer" class="font-semibold text-lg text-foreground hover:text-primary transition-colors cursor-pointer truncate" :title="site.name">
                  {{ site.name }}
                </a>
                <div class="mt-1 flex flex-wrap items-center gap-1">
                  <span class="px-2 py-0.5 rounded-md bg-primary/10 text-primary border border-primary/20 text-[10px] font-bold uppercase tracking-wider w-fit">
                    {{ t(`admin.upstream.modal.form.platforms.${site.platform}`) }}
                  </span>
                  <span
                    v-if="site.settings.settlementMode === 'credit_line'"
                    class="px-2 py-0.5 rounded-md bg-warning/10 text-warning border border-warning/20 text-[10px] font-bold tracking-wider w-fit"
                  >
                    {{ t('admin.upstream.badges.creditLine') }}
                  </span>
                </div>
              </div>
            </div>

            <div
              class="flex items-center gap-1.5 px-2 py-1 rounded-md text-[11px] font-medium border shrink-0"
              :class="statusClasses[site.status]"
            >
              <Loader2 v-if="site.status === 'connecting' || site.status === 'syncing'" class="w-3 h-3 animate-spin" />
              <CheckCircle2 v-else-if="site.status === 'connected'" class="w-3 h-3" />
              <XCircle v-else class="w-3 h-3" />
              {{ statusLabel(site.status) }}
            </div>
          </div>
        </div>

        <!-- Card Body (Stats) -->
        <div class="space-y-4">
          <!--
            指标区用固定高度 + nowrap：大额（如 9818 CNY）换行会把格子撑高，
            导致同行「查看可用分组」Y 轴错位。
          -->
          <div class="grid h-[6.25rem] grid-cols-3 gap-3">
            <div class="flex h-full flex-col items-center justify-center overflow-hidden rounded-xl border border-border/40 bg-surface/50 px-1.5 py-2">
              <span class="mb-0.5 max-w-full truncate text-[11px] text-muted-foreground">
                {{ isCreditLine(site) ? t('admin.upstream.fields.platformBalance') : t('admin.upstream.fields.balance') }}
              </span>
              <span
                v-if="cnyMetricDisplay(site, site.metrics.balance)"
                class="max-w-full truncate text-center text-xs font-bold leading-tight text-primary sm:text-sm"
              >
                {{ cnyMetricDisplay(site, site.metrics.balance) }}
              </span>
              <span
                class="mt-0.5 max-w-full truncate text-center text-[10px] font-medium leading-tight"
                :class="cnyMetricDisplay(site, site.metrics.balance) ? 'text-primary/70' : 'text-sm font-bold text-primary'"
              >
                {{ usdMetricDisplay(site.metrics.balance) }}
              </span>
              <span class="mt-0.5 h-3 max-w-full truncate text-center text-[10px] leading-3 text-muted-foreground">
                {{ isCreditLine(site) ? t('admin.upstream.fields.platformBalanceHint') : '\u00a0' }}
              </span>
            </div>
            <div class="flex h-full flex-col items-center justify-center overflow-hidden rounded-xl border border-border/40 bg-surface/50 px-1.5 py-2">
              <span class="mb-0.5 max-w-full truncate text-[11px] text-muted-foreground">{{ t('admin.upstream.fields.todayConsume') }}</span>
              <span
                v-if="cnyMetricDisplay(site, site.metrics.todayConsume)"
                class="max-w-full truncate text-center text-xs font-bold leading-tight sm:text-sm"
                :class="site.metrics.todayConsume.value && site.metrics.todayConsume.value > 0 ? 'text-orange-500' : 'text-foreground'"
              >
                {{ cnyMetricDisplay(site, site.metrics.todayConsume) }}
              </span>
              <span
                class="mt-0.5 max-w-full truncate text-center text-[10px] font-medium leading-tight"
                :class="site.metrics.todayConsume.value && site.metrics.todayConsume.value > 0 ? 'text-orange-500/70' : 'text-muted-foreground'"
              >
                {{ usdMetricDisplay(site.metrics.todayConsume) }}
              </span>
              <span class="mt-0.5 h-3 text-[10px] leading-3">&nbsp;</span>
            </div>
            <div class="flex h-full flex-col items-center justify-center overflow-hidden rounded-xl border border-border/40 bg-surface/50 px-1.5 py-2">
              <span class="mb-0.5 max-w-full truncate text-[11px] text-muted-foreground">{{ t('admin.upstream.fields.historyRecharge') }}</span>
              <span
                v-if="cnyMetricDisplay(site, site.metrics.historyRecharge)"
                class="max-w-full truncate text-center text-xs font-bold leading-tight text-foreground sm:text-sm"
              >
                {{ cnyMetricDisplay(site, site.metrics.historyRecharge) }}
              </span>
              <span class="mt-0.5 max-w-full truncate text-center text-[10px] font-medium leading-tight text-muted-foreground">
                {{ usdMetricDisplay(site.metrics.historyRecharge) }}
              </span>
              <span class="mt-0.5 h-3 text-[10px] leading-3">&nbsp;</span>
            </div>
          </div>

          <!-- 固定高度槽，保证同行卡片按钮同一 Y -->
          <div class="h-9 shrink-0">
            <Button
              v-if="site.metrics.groups.length > 0"
              variant="secondary"
              class="h-9 w-full border border-border/50 bg-surface text-xs font-medium hover:bg-surface-elevated"
              @click="openGroupsModal(site)"
            >
              {{ t('admin.upstream.fields.viewAvailableGroups') }}
            </Button>
          </div>

          <!-- Card Actions -->
          <div class="flex items-center justify-between gap-3 pt-4 mt-2 border-t border-border/40">
            <div class="min-w-0 text-left text-[11px] leading-5 text-muted-foreground">
              <span class="block truncate">{{ t('admin.upstream.fields.lastUpdated') }}</span>
              <span class="block truncate font-medium text-foreground/80">{{ lastUpdatedDisplay(site) }}</span>
            </div>
            <div class="flex shrink-0 items-center justify-end gap-2">
              <Tooltip :text="syncingSiteIds.has(site.id) ? t('admin.upstream.action.syncing') : t('admin.upstream.action.sync')">
                <button
                  type="button"
                  class="inline-flex h-8 w-8 items-center justify-center rounded-lg border border-border/60 text-muted-foreground transition-colors hover:border-primary/60 hover:bg-primary/10 hover:text-primary"
                  :disabled="syncingSiteIds.has(site.id)"
                  @click="refreshSingleSite(site.id)"
                >
                  <Loader2 v-if="syncingSiteIds.has(site.id)" class="h-4 w-4 animate-spin" />
                  <RefreshCw v-else class="h-4 w-4" />
                </button>
              </Tooltip>
              <Tooltip :text="t('admin.upstream.action.settings')">
                <button
                  type="button"
                  class="inline-flex h-8 w-8 items-center justify-center rounded-lg border border-border/60 text-muted-foreground transition-colors hover:border-primary/60 hover:bg-primary/10 hover:text-primary"
                  @click="openSiteSettings(site)"
                >
                  <Settings2 class="h-4 w-4" />
                </button>
              </Tooltip>
              <Tooltip :text="t('admin.upstream.ledger.open')">
                <button
                  type="button"
                  class="inline-flex h-8 w-8 items-center justify-center rounded-lg border border-border/60 text-muted-foreground transition-colors hover:border-primary/60 hover:bg-primary/10 hover:text-primary"
                  @click="openLedger(site)"
                >
                  <PackagePlus class="h-4 w-4" />
                </button>
              </Tooltip>
              <Tooltip :text="t('admin.upstream.action.edit')">
                <button
                  type="button"
                  class="inline-flex h-8 w-8 items-center justify-center rounded-lg border border-border/60 text-muted-foreground transition-colors hover:border-primary/60 hover:bg-primary/10 hover:text-primary"
                  @click="handleEditSite(site)"
                >
                  <Edit2 class="h-4 w-4" />
                </button>
              </Tooltip>
              <Tooltip :text="t('admin.upstream.delete.action')">
                <button
                  type="button"
                  class="inline-flex h-8 w-8 items-center justify-center rounded-lg border border-border/60 text-muted-foreground transition-colors hover:border-red-400/60 hover:bg-red-500/10 hover:text-red-400"
                  @click="requestDeleteSite(site.id)"
                >
                  <Trash2 class="h-4 w-4" />
                </button>
              </Tooltip>
            </div>
          </div>

          <!-- 扩展槽：固定在操作栏下方，有内容才显示，结构统一 -->
          <div
            v-if="isCreditLine(site) || (site.metrics.subscriptions?.length ?? 0) > 0"
            class="mt-3 space-y-2 border-t border-border/30 pt-3"
          >
            <div
              v-if="isCreditLine(site)"
              class="rounded-xl border border-warning/30 bg-warning/5 px-3 py-2 text-xs"
            >
              <template v-if="site.settlement">
                <div class="flex items-center justify-between gap-2">
                  <span class="font-medium text-warning">{{ t('admin.upstream.settlement.outstanding') }}</span>
                  <span class="font-semibold text-foreground">{{ site.settlement.outstanding.toFixed(2) }}</span>
                </div>
                <div class="mt-1 flex justify-between text-muted-foreground">
                  <span>{{ t('admin.upstream.settlement.settled') }} {{ site.settlement.settledCost.toFixed(2) }}</span>
                  <span>{{ t('admin.upstream.settlement.consumed') }} {{ site.settlement.consumedCost.toFixed(2) }}</span>
                </div>
              </template>
              <p v-else class="text-muted-foreground">{{ t('admin.upstream.settlement.loadingSummary') }}</p>
              <Button variant="secondary" class="mt-2 h-8 w-full text-xs" @click="openSettlement(site)">
                <Receipt class="mr-1 h-3.5 w-3.5" />
                {{ t('admin.upstream.settlement.open') }}
              </Button>
            </div>

            <div
              v-if="site.metrics.subscriptions?.length"
              class="rounded-xl border border-border/50 bg-surface/40 px-3 py-2 text-xs space-y-2"
            >
              <div class="font-medium text-foreground">{{ t('admin.upstream.subscriptions.title') }}</div>
              <div v-for="sub in site.metrics.subscriptions" :key="sub.id" class="border-t border-border/30 pt-2 first:border-0 first:pt-0">
                <div class="flex justify-between gap-2">
                  <span class="font-medium truncate">{{ sub.groupName }}</span>
                  <span class="shrink-0 text-muted-foreground">{{ sub.status }}</span>
                </div>
                <div
                  v-if="sub.todayMaxConsumableUsd != null"
                  class="mt-1.5 rounded-lg bg-primary/10 px-2 py-1 font-medium text-primary"
                >
                  {{ t('admin.upstream.subscriptions.todayMax') }}:
                  {{ sub.todayMaxConsumableUsd.toFixed(2) }} USD
                  <span class="ml-1 font-normal opacity-80">（{{ t('admin.upstream.subscriptions.todayMaxHint') }}）</span>
                </div>
                <div class="mt-1 space-y-0.5 text-muted-foreground">
                  <div v-if="sub.dailyLimitUsd != null">
                    {{ t('admin.upstream.subscriptions.daily') }}:
                    {{ t('admin.upstream.subscriptions.usedOfLimit', { used: sub.dailyUsageUsd.toFixed(2), limit: sub.dailyLimitUsd }) }}
                    <template v-if="sub.dailyRemainingUsd != null">
                      · {{ t('admin.upstream.subscriptions.remaining') }} {{ sub.dailyRemainingUsd.toFixed(2) }}
                    </template>
                  </div>
                  <div v-if="sub.weeklyLimitUsd != null">
                    {{ t('admin.upstream.subscriptions.weekly') }}:
                    {{ t('admin.upstream.subscriptions.usedOfLimit', { used: sub.weeklyUsageUsd.toFixed(2), limit: sub.weeklyLimitUsd }) }}
                    <template v-if="sub.weeklyRemainingUsd != null">
                      · {{ t('admin.upstream.subscriptions.remaining') }} {{ sub.weeklyRemainingUsd.toFixed(2) }}
                    </template>
                  </div>
                  <div v-if="sub.monthlyLimitUsd != null">
                    {{ t('admin.upstream.subscriptions.monthly') }}:
                    {{ t('admin.upstream.subscriptions.usedOfLimit', { used: sub.monthlyUsageUsd.toFixed(2), limit: sub.monthlyLimitUsd }) }}
                    <template v-if="sub.monthlyRemainingUsd != null">
                      · {{ t('admin.upstream.subscriptions.remaining') }} {{ sub.monthlyRemainingUsd.toFixed(2) }}
                    </template>
                  </div>
                </div>
                <div v-if="sub.startsAt" class="mt-1 text-muted-foreground">
                  {{ t('admin.upstream.subscriptions.starts') }}: {{ new Date(sub.startsAt).toLocaleDateString() }}
                </div>
                <div v-if="sub.expiresAt" class="mt-1 text-muted-foreground">
                  {{ t('admin.upstream.subscriptions.expires') }}: {{ new Date(sub.expiresAt).toLocaleDateString() }}
                </div>
                <Button
                  variant="secondary"
                  class="mt-2 h-7 w-full text-[11px]"
                  @click="openSubscriptionTopup(site, sub)"
                >
                  <PackagePlus class="mr-1 h-3.5 w-3.5" />
                  {{ t('admin.upstream.subscriptions.topup') }}
                </Button>
              </div>
            </div>
          </div>
        </div>

        <div v-if="site.errorKey" class="mt-4 flex items-start gap-2 rounded-xl border border-warning/20 bg-warning/10 px-3 py-2 text-xs text-warning">
          <AlertCircle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>{{ t(site.errorKey) }}</span>
        </div>
      </div>
    </div>

    <!-- Table (List) View -->
    <div v-if="viewMode === 'list'" class="rounded-2xl border border-border/60 bg-card overflow-hidden shadow-sm">
      <div class="overflow-x-auto">
        <table class="w-full text-sm text-left">
          <thead class="bg-surface/50 text-muted-foreground border-b border-border/40">
            <tr>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.fields.siteName') }}</th>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.fields.platform') }}</th>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.status.connected') }}</th>
              <th class="px-4 py-4 font-medium cursor-pointer select-none hover:text-foreground" @click="toggleSort('balance')">
                {{ t('admin.upstream.fields.balance') }}
                <span v-if="sortKey === 'balance'">{{ sortDir === 'desc' ? '↓' : '↑' }}</span>
              </th>
              <th class="px-4 py-4 font-medium cursor-pointer select-none hover:text-foreground" @click="toggleSort('todayConsume')">
                {{ t('admin.upstream.fields.todayConsume') }}
                <span v-if="sortKey === 'todayConsume'">{{ sortDir === 'desc' ? '↓' : '↑' }}</span>
              </th>
              <th class="px-4 py-4 font-medium cursor-pointer select-none hover:text-foreground" @click="toggleSort('historyRecharge')">
                {{ t('admin.upstream.fields.historyRecharge') }}
                <span v-if="sortKey === 'historyRecharge'">{{ sortDir === 'desc' ? '↓' : '↑' }}</span>
              </th>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.fields.settlementModeCol') }}</th>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.settlement.outstanding') }}</th>
              <th class="px-4 py-4 font-medium">{{ t('admin.upstream.subscriptions.title') }}</th>
              <th class="px-4 py-4 font-medium text-right">{{ t('admin.upstream.action.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border/40">
            <tr v-for="site in filteredSites" :key="site.id" class="hover:bg-surface/30 transition-colors">
              <td class="px-4 py-4">
                <div class="flex items-center gap-3">
                  <div :class="['w-8 h-8 rounded-lg flex items-center justify-center font-bold text-sm shrink-0', site.logoBg]">
                    {{ site.logo }}
                  </div>
                  <a :href="site.baseUrl" target="_blank" rel="noopener noreferrer" class="font-medium text-foreground hover:text-primary transition-colors truncate max-w-[150px] inline-block">
                    {{ site.name }}
                  </a>
                </div>
              </td>
              <td class="px-4 py-4">
                <span class="px-2 py-1 rounded-md bg-primary/10 text-primary border border-primary/20 text-xs font-semibold uppercase tracking-wider">
                  {{ t(`admin.upstream.modal.form.platforms.${site.platform}`) }}
                </span>
              </td>
              <td class="px-6 py-4">
                <div
                  v-if="siteSyncStates.get(site.id)?.phase && siteSyncStates.get(site.id)?.phase !== 'idle'"
                  class="inline-flex items-center gap-1.5 text-xs font-medium"
                  :class="{
                    'text-primary': siteSyncStates.get(site.id)?.phase === 'syncing',
                    'text-signal': siteSyncStates.get(site.id)?.phase === 'done',
                    'text-destructive': siteSyncStates.get(site.id)?.phase === 'error',
                  }"
                >
                  <Loader2 v-if="siteSyncStates.get(site.id)?.phase === 'syncing'" class="w-3.5 h-3.5 animate-spin" />
                  <CheckCircle2 v-else-if="siteSyncStates.get(site.id)?.phase === 'done'" class="w-3.5 h-3.5" />
                  <XCircle v-else class="w-3.5 h-3.5" />
                  <template v-if="siteSyncStates.get(site.id)?.phase === 'syncing'">{{ t('admin.upstream.syncStream.syncing') }}</template>
                  <template v-else-if="siteSyncStates.get(site.id)?.phase === 'done'">{{ t('admin.upstream.syncStream.done') }}</template>
                  <template v-else>{{ t('admin.upstream.syncStream.error') }}</template>
                </div>
                <div
                  v-else
                  class="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-medium border"
                  :class="statusClasses[site.status]"
                >
                  <Loader2 v-if="site.status === 'connecting' || site.status === 'syncing'" class="w-3.5 h-3.5 animate-spin" />
                  <CheckCircle2 v-else-if="site.status === 'connected'" class="w-3.5 h-3.5" />
                  <XCircle v-else class="w-3.5 h-3.5" />
                  {{ statusLabel(site.status) }}
                </div>
              </td>
              <td class="px-6 py-4">
                <div class="flex flex-col gap-0.5">
                  <span v-if="cnyMetricDisplay(site, site.metrics.balance)" class="font-medium text-primary">
                    {{ cnyMetricDisplay(site, site.metrics.balance) }}
                  </span>
                  <span :class="[cnyMetricDisplay(site, site.metrics.balance) ? 'text-xs font-medium text-primary/70' : 'font-medium text-primary']">
                    {{ usdMetricDisplay(site.metrics.balance) }}
                  </span>
                </div>
              </td>
              <td class="px-6 py-4">
                <div class="flex flex-col gap-0.5">
                  <span v-if="cnyMetricDisplay(site, site.metrics.todayConsume)" :class="['font-medium', site.metrics.todayConsume.value && site.metrics.todayConsume.value > 0 ? 'text-orange-500' : 'text-muted-foreground']">
                    {{ cnyMetricDisplay(site, site.metrics.todayConsume) }}
                  </span>
                  <span :class="[cnyMetricDisplay(site, site.metrics.todayConsume) ? 'text-xs font-medium' : 'font-medium', site.metrics.todayConsume.value && site.metrics.todayConsume.value > 0 ? (cnyMetricDisplay(site, site.metrics.todayConsume) ? 'text-orange-500/70' : 'text-orange-500') : 'text-muted-foreground']">
                    {{ usdMetricDisplay(site.metrics.todayConsume) }}
                  </span>
                </div>
              </td>
              <td class="px-4 py-4">
                <div class="flex flex-col gap-0.5">
                  <span v-if="cnyMetricDisplay(site, site.metrics.historyRecharge)" class="font-medium text-muted-foreground">
                    {{ cnyMetricDisplay(site, site.metrics.historyRecharge) }}
                  </span>
                  <span :class="[cnyMetricDisplay(site, site.metrics.historyRecharge) ? 'text-xs font-medium text-muted-foreground' : 'text-muted-foreground']">
                    {{ usdMetricDisplay(site.metrics.historyRecharge) }}
                  </span>
                </div>
              </td>
              <td class="px-4 py-4">
                <span
                  class="inline-flex rounded-md border px-2 py-0.5 text-[11px] font-medium"
                  :class="isCreditLine(site) ? 'border-warning/30 bg-warning/10 text-warning' : 'border-border/50 text-muted-foreground'"
                >
                  {{ isCreditLine(site) ? t('admin.upstream.badges.creditLine') : t('admin.upstream.siteSettings.modePrepaid') }}
                </span>
              </td>
              <td class="px-4 py-4">
                <template v-if="isCreditLine(site)">
                  <button
                    type="button"
                    class="text-left hover:text-primary"
                    @click="openSettlement(site)"
                  >
                    <div class="font-medium text-warning">
                      {{ site.settlement ? site.settlement.outstanding.toFixed(2) : '—' }}
                    </div>
                    <div v-if="site.settlement" class="text-[11px] text-muted-foreground">
                      {{ t('admin.upstream.settlement.settled') }} {{ site.settlement.settledCost.toFixed(2) }}
                    </div>
                  </button>
                </template>
                <span v-else class="text-muted-foreground">—</span>
              </td>
              <td class="px-4 py-4 text-xs text-muted-foreground max-w-[160px]">
                {{ subscriptionSummary(site) }}
              </td>
              <td class="px-4 py-4 text-right">
                <div class="flex items-center justify-end gap-2">
                  <Button
                    v-if="site.metrics.groups.length > 0"
                    variant="ghost"
                    class="h-8 px-2 text-xs text-primary hover:text-primary hover:bg-primary/10"
                    @click="openGroupsModal(site)"
                  >
                    {{ t('admin.upstream.fields.availableGroups') }}
                  </Button>
                  <Tooltip :text="syncingSiteIds.has(site.id) ? t('admin.upstream.action.syncing') : t('admin.upstream.action.sync')">
                    <button
                      class="p-1.5 rounded-md text-muted-foreground hover:bg-primary/10 hover:text-primary transition-colors"
                      :disabled="syncingSiteIds.has(site.id)"
                      @click="refreshSingleSite(site.id)"
                    >
                      <Loader2 v-if="syncingSiteIds.has(site.id)" class="w-4 h-4 animate-spin" />
                      <RefreshCw v-else class="w-4 h-4" />
                    </button>
                  </Tooltip>
                  <Tooltip :text="t('admin.upstream.siteSettings.title')">
                    <button
                      class="p-1.5 rounded-md text-muted-foreground hover:bg-primary/10 hover:text-primary transition-colors"
                      @click="openSiteSettings(site)"
                    >
                      <Settings2 class="w-4 h-4" />
                    </button>
                  </Tooltip>
                  <Tooltip :text="t('admin.upstream.ledger.open')">
                    <button
                      class="p-1.5 rounded-md text-muted-foreground hover:bg-primary/10 hover:text-primary transition-colors"
                      @click="openLedger(site)"
                    >
                      <PackagePlus class="w-4 h-4" />
                    </button>
                  </Tooltip>
                  <Tooltip :text="t('admin.upstream.action.edit')">
                    <button
                      class="p-1.5 rounded-md text-muted-foreground hover:bg-primary/10 hover:text-primary transition-colors"
                      @click="handleEditSite(site)"
                    >
                      <Edit2 class="w-4 h-4" />
                    </button>
                  </Tooltip>
                  <Tooltip :text="t('admin.upstream.delete.action')">
                    <button
                      class="p-1.5 rounded-md text-muted-foreground hover:bg-red-500/10 hover:text-red-400 transition-colors"
                      @click="requestDeleteSite(site.id)"
                    >
                      <Trash2 class="w-4 h-4" />
                    </button>
                  </Tooltip>
                </div>
              </td>
            </tr>
            <tr v-if="filteredSites.length === 0">
              <td colspan="10" class="px-6 py-12 text-center text-muted-foreground">
                {{ t('admin.upstream.empty.description') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Empty State -->
    <div v-if="filteredSites.length === 0" class="flex flex-col items-center justify-center py-12 text-center border border-dashed border-border/60 rounded-2xl bg-surface/30">
      <div class="w-12 h-12 rounded-full bg-muted/50 flex items-center justify-center mb-4">
        <Search class="w-6 h-6 text-muted-foreground" />
      </div>
      <p class="text-foreground font-medium">{{ t('admin.upstream.empty.title') }}</p>
      <p class="text-sm text-muted-foreground mt-1">{{ t('admin.upstream.empty.description') }}</p>
    </div>

    <!-- Delete Confirm Modal -->
    <div v-if="deletingSite" class="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="cancelDeleteSite" />
      <div role="alertdialog" aria-modal="true" :aria-label="t('admin.upstream.delete.title')" class="relative w-full max-w-md overflow-hidden rounded-xl border border-border/70 border-t-2 border-t-destructive bg-card p-6 shadow-2xl">
        <div class="flex items-start gap-4">
          <div class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl border border-red-500/30 bg-red-500/10 text-red-400">
            <Trash2 class="h-5 w-5" />
          </div>
          <div class="min-w-0 flex-1">
            <h3 class="text-lg font-semibold text-foreground">{{ t('admin.upstream.delete.title') }}</h3>
            <p class="mt-2 text-sm leading-6 text-muted-foreground">
              {{ t('admin.upstream.delete.description', { name: deletingSite.name }) }}
            </p>
          </div>
        </div>

        <div v-if="deleteErrorKey" class="mt-5 flex items-start gap-2 rounded-xl border border-warning/30 bg-warning/10 px-3 py-2 text-sm text-warning">
          <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
          <span>{{ t(deleteErrorKey) }}</span>
        </div>

        <div class="mt-6 flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
          <Button type="button" variant="secondary" @click="cancelDeleteSite">
            {{ t('admin.upstream.delete.cancel') }}
          </Button>
          <Button type="button" class="bg-red-500 text-white hover:bg-red-400" @click="confirmDeleteSite">
            {{ t('admin.upstream.delete.confirm') }}
          </Button>
        </div>
      </div>
    </div>

    <!-- Groups Modal -->
    <Teleport defer to="body">
      <div v-if="isGroupsModalOpen" class="fixed inset-0 z-[100] flex items-center justify-center p-4 sm:p-0">
        <!-- Backdrop -->
        <div
          class="absolute inset-0 bg-background/80 backdrop-blur-sm"
          @click="closeGroupsModal"
        ></div>

        <!-- Modal Content -->
        <div role="dialog" aria-modal="true" :aria-label="t('admin.upstream.fields.availableGroups')" class="relative max-h-[calc(100dvh-2rem)] w-full max-w-2xl overflow-hidden rounded-xl border border-border/60 border-t-2 border-t-primary bg-card shadow-2xl animate-in fade-in zoom-in-95 duration-200">

          <div class="flex items-center justify-between px-6 py-5 border-b border-border/40">
            <h3 class="text-lg font-semibold text-foreground">
              {{ t('admin.upstream.fields.availableGroups') }}
              <span class="text-muted-foreground ml-2 text-sm font-medium">{{ selectedSiteForGroups?.name }}</span>
            </h3>
            <button type="button" @click="closeGroupsModal" class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary" :aria-label="t('admin.upstream.fields.closeGroupsModal')">
              <X class="w-5 h-5" />
            </button>
          </div>

          <div class="max-h-[60dvh] space-y-6 overflow-y-auto p-6 overscroll-contain">
            <div v-for="(groups, platform) in groupedGroups" :key="platform" class="space-y-3">
              <h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider flex items-center gap-2">
                <div class="w-1.5 h-1.5 rounded-full bg-primary"></div>
                {{ platform }}
              </h4>
              <div class="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 gap-3">
                <button
                  v-for="group in groups"
                  :key="group.name"
                  class="flex flex-col items-center justify-center p-3 rounded-xl border border-border/60 bg-surface/50 hover:bg-surface hover:border-primary/50 transition-colors text-center group"
                >
                  <span class="text-sm font-medium text-foreground truncate w-full group-hover:text-primary transition-colors">{{ group.name }}</span>
                  <span
                    v-if="group.multiplier !== null && selectedSiteForGroups && selectedSiteForGroups.rechargeRate > 0"
                    class="mt-2 text-xs font-semibold text-primary px-2 py-0.5 rounded-md bg-primary/10 border border-primary/20"
                  >
                    {{ (group.multiplier * selectedSiteForGroups.rechargeRate).toFixed(2) }}
                  </span>
                  <template v-if="group.hasDedicatedMultiplier">
                    <Tooltip :text="t('admin.upstream.fields.dedicatedMultiplierTooltip')" wide>
                      <span class="text-[10px] text-muted-foreground mt-1">
                        {{ group.defaultMultiplierDisplay }} -&gt; {{ group.dedicatedMultiplierDisplay }}
                      </span>
                    </Tooltip>
                    <span class="mt-1 text-[9px] font-semibold text-accent px-1.5 py-0.5 rounded bg-accent/10 border border-accent/20">
                      {{ t('admin.upstream.fields.dedicatedMultiplierBadge') }}
                    </span>
                  </template>
                  <span v-else class="text-[10px] text-muted-foreground mt-1">
                    {{ group.multiplierDisplay }}
                  </span>
                </button>
              </div>
            </div>
          </div>

          <div class="p-4 border-t border-border/40 flex justify-end">
             <Button variant="ghost" @click="closeGroupsModal">{{ t('admin.upstream.fields.closeGroupsModal') }}</Button>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- Add Site Modal -->
    <Teleport defer to="body">
      <div v-if="isAddModalOpen" class="fixed inset-0 z-[100] flex items-center justify-center p-4 sm:p-0">
        <!-- Backdrop -->
        <div
          class="absolute inset-0 bg-background/80 backdrop-blur-sm"
          @click="closeSiteModal"
        ></div>

        <!-- Modal Content -->
        <div role="dialog" aria-modal="true" :aria-label="t(editingSiteId ? 'admin.upstream.modal.editTitle' : 'admin.upstream.modal.title')" class="relative max-h-[calc(100dvh-2rem)] w-full max-w-2xl overflow-y-auto overscroll-contain rounded-xl border border-border/60 border-t-2 border-t-primary bg-card shadow-2xl animate-in fade-in zoom-in-95 duration-200">

          <div class="flex items-center justify-between px-6 py-5 border-b border-border/40">
            <h3 class="text-lg font-semibold text-foreground">
              {{ t(editingSiteId ? 'admin.upstream.modal.editTitle' : 'admin.upstream.modal.title') }}
            </h3>
            <button type="button" @click="closeSiteModal" class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary" :aria-label="t('admin.upstream.modal.cancel')">
              <X class="w-5 h-5" />
            </button>
          </div>

          <form @submit.prevent="handleAddSite" class="p-6">
            <div v-if="addErrorKey" class="mb-5 flex items-start gap-2 rounded-lg border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive" role="alert" aria-live="polite">
              <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
              <span>{{ t(addErrorKey) }}</span>
            </div>

            <div class="grid grid-cols-1 sm:grid-cols-2 gap-5">
              <!-- Site Name -->
              <div class="space-y-2">
                <label for="upstream-site-name" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.siteName') }}
                </label>
                <Input
                  id="upstream-site-name"
                  v-model="newSiteForm.name"
                  name="siteName"
                  :placeholder="t('admin.upstream.modal.form.siteNamePlaceholder')"
                  :disabled="isAdding"
                  required
                  class="bg-surface border-border/50 focus:border-primary h-10"
                />
              </div>

              <!-- Platform Select -->
              <div class="space-y-2">
                <label for="upstream-site-platform" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.platform') }}
                </label>
                <div class="relative">
                  <select
                    id="upstream-site-platform"
                    v-model="newSiteForm.platform"
                    name="platform"
                    :disabled="isAdding"
                    class="h-10 w-full appearance-none rounded-lg border border-border/50 bg-surface px-3 text-sm text-foreground outline-none transition-[color,background-color,border-color,box-shadow] focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/30"
                  >
                    <option value="auto">{{ t('admin.upstream.modal.form.platforms.auto') }}</option>
                    <option value="sub2api">{{ t('admin.upstream.modal.form.platforms.sub2api') }}</option>
                    <option value="newapi">{{ t('admin.upstream.modal.form.platforms.newapi') }}</option>
                  </select>
                  <!-- Custom arrow since we removed appearance -->
                  <div class="absolute right-3 top-1/2 -translate-y-1/2 pointer-events-none text-muted-foreground">
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
                  </div>
                </div>
              </div>

              <!-- Site URL -->
              <div class="space-y-2 sm:col-span-2">
                <label for="upstream-site-url" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.siteUrl') }}
                </label>
                <Input
                  id="upstream-site-url"
                  v-model="newSiteForm.siteUrl"
                  name="siteUrl"
                  type="url"
                  :placeholder="t('admin.upstream.modal.form.siteUrlPlaceholder')"
                  :disabled="isAdding"
                  required
                  class="bg-surface border-border/50 focus:border-primary h-10"
                />
              </div>

              <!-- Auth Mode -->
              <div class="space-y-2 sm:col-span-2">
                <span class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.authMode') }}
                </span>
                <div class="grid grid-cols-1 sm:grid-cols-2 gap-3" role="radiogroup" :aria-label="t('admin.upstream.modal.form.authMode')">
                  <label class="flex cursor-pointer items-start gap-3 rounded-xl border border-border/50 bg-surface p-3 text-sm transition-colors hover:border-primary/50">
                    <input v-model="newSiteForm.authMode" type="radio" value="password" :disabled="isAdding" class="mt-1" />
                    <span class="space-y-1">
                      <span class="block font-medium text-foreground">{{ t('admin.upstream.modal.form.authModes.password') }}</span>
                      <span class="block text-xs leading-5 text-muted-foreground">{{ t('admin.upstream.modal.form.authModes.passwordHelp') }}</span>
                    </span>
                  </label>
                  <label class="flex cursor-pointer items-start gap-3 rounded-xl border border-border/50 bg-surface p-3 text-sm transition-colors hover:border-primary/50">
                    <input v-model="newSiteForm.authMode" type="radio" :value="newSiteForm.platform === 'newapi' ? 'user_key' : 'token'" :disabled="isAdding" class="mt-1" />
                    <span class="space-y-1">
                      <span class="block font-medium text-foreground">{{ t(`admin.upstream.modal.form.authModes.${newSiteForm.platform === 'newapi' ? 'userKey' : 'token'}`) }}</span>
                      <span class="block text-xs leading-5 text-muted-foreground">{{ t(`admin.upstream.modal.form.authModes.${newSiteForm.platform === 'newapi' ? 'userKeyHelp' : 'tokenHelp'}`) }}</span>
                    </span>
                  </label>
                </div>
              </div>

              <!-- Account -->
              <div v-if="newSiteForm.authMode === 'password'" class="space-y-2">
                <label for="upstream-site-account" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.account') }}
                </label>
                <Input
                  id="upstream-site-account"
                  v-model="newSiteForm.account"
                  name="account"
                  :placeholder="t('admin.upstream.modal.form.accountPlaceholder')"
                  :disabled="isAdding"
                  required
                  class="bg-surface border-border/50 focus:border-primary h-10"
                />
              </div>

              <!-- Password -->
              <div v-if="newSiteForm.authMode === 'password'" class="space-y-2">
                <label for="upstream-site-password" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span v-if="!editingSiteId" class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.password') }}
                </label>
                <Input
                  id="upstream-site-password"
                  v-model="newSiteForm.password"
                  name="password"
                  type="password"
                  :placeholder="t(editingSiteId ? 'admin.upstream.modal.form.passwordEditPlaceholder' : 'admin.upstream.modal.form.passwordPlaceholder')"
                  :disabled="isAdding"
                  :required="!editingSiteId"
                  class="bg-surface border-border/50 focus:border-primary h-10"
                />
                <p v-if="editingSiteId" class="text-xs leading-5 text-muted-foreground">
                  {{ t('admin.upstream.modal.form.passwordEditHelp') }}
                </p>
              </div>

              <template v-else-if="newSiteForm.authMode === 'token'">
                <div class="space-y-2 sm:col-span-2">
                  <label for="upstream-site-access-token" class="text-sm font-medium text-foreground flex items-center gap-1">
                    {{ t('admin.upstream.modal.form.accessToken') }}
                  </label>
                  <Input
                    v-model="newSiteForm.accessToken"
                    :placeholder="t('admin.upstream.modal.form.accessTokenPlaceholder')"
                    id="upstream-site-access-token"
                    name="accessToken"
                    :disabled="isAdding"
                    class="bg-surface border-border/50 focus:border-primary h-10"
                  />
                </div>
                <div class="space-y-2">
                  <label for="upstream-site-refresh-token" class="text-sm font-medium text-foreground flex items-center gap-1">
                    {{ t('admin.upstream.modal.form.refreshToken') }}
                  </label>
                  <Input
                    id="upstream-site-refresh-token"
                    v-model="newSiteForm.refreshToken"
                    name="refreshToken"
                    :placeholder="t('admin.upstream.modal.form.refreshTokenPlaceholder')"
                    :disabled="isAdding"
                    class="bg-surface border-border/50 focus:border-primary h-10"
                  />
                </div>
                <div class="space-y-2">
                  <label for="upstream-site-token-type" class="text-sm font-medium text-foreground flex items-center gap-1">
                    {{ t('admin.upstream.modal.form.tokenType') }}
                  </label>
                  <Input
                    id="upstream-site-token-type"
                    v-model="newSiteForm.tokenType"
                    name="tokenType"
                    :placeholder="t('admin.upstream.modal.form.tokenTypePlaceholder')"
                    :disabled="isAdding"
                    class="bg-surface border-border/50 focus:border-primary h-10"
                  />
                  <p class="text-xs leading-5 text-muted-foreground">
                    {{ t('admin.upstream.modal.form.tokenHelp') }}
                  </p>
                </div>
              </template>

              <template v-else>
                <div class="space-y-2">
                  <label for="upstream-site-user-id" class="text-sm font-medium text-foreground flex items-center gap-1">
                    <span class="text-red-500">*</span>
                    {{ t('admin.upstream.modal.form.userId') }}
                  </label>
                  <Input
                    id="upstream-site-user-id"
                    v-model="newSiteForm.userId"
                    name="userId"
                    :placeholder="t('admin.upstream.modal.form.userIdPlaceholder')"
                    :disabled="isAdding"
                    inputmode="numeric"
                    autocomplete="off"
                    required
                    class="bg-surface border-border/50 focus:border-primary h-10"
                  />
                </div>
                <div class="space-y-2">
                  <label for="upstream-site-user-key" class="text-sm font-medium text-foreground flex items-center gap-1">
                    <span class="text-red-500">*</span>
                    {{ t('admin.upstream.modal.form.userKey') }}
                  </label>
                  <Input
                    id="upstream-site-user-key"
                    v-model="newSiteForm.accessToken"
                    name="userKey"
                    type="password"
                    :placeholder="t('admin.upstream.modal.form.userKeyPlaceholder')"
                    :disabled="isAdding"
                    autocomplete="off"
                    required
                    class="bg-surface border-border/50 focus:border-primary h-10"
                  />
                  <p class="text-xs leading-5 text-muted-foreground">
                    {{ t('admin.upstream.modal.form.userKeyHelp') }}
                  </p>
                </div>
              </template>

              <!-- Recharge Rate -->
              <div class="space-y-2">
                <label for="upstream-site-recharge-rate" class="text-sm font-medium text-foreground flex items-center gap-1">
                  <span class="text-red-500">*</span>
                  {{ t('admin.upstream.modal.form.rechargeRate') }}
                </label>
                <input
                  id="upstream-site-recharge-rate"
                  v-model.number="newSiteForm.rechargeRate"
                  name="rechargeRate"
                  type="number"
                  min="0.000001"
                  step="0.000001"
                  :placeholder="t('admin.upstream.modal.form.rechargeRatePlaceholder')"
                  :disabled="isAdding"
                  required
                  class="h-10 w-full rounded-lg border border-border/50 bg-surface px-3 text-sm text-foreground outline-none transition-[color,background-color,border-color,box-shadow] placeholder:text-muted-foreground focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/30 disabled:cursor-not-allowed disabled:opacity-50"
                />
                <p class="text-xs text-muted-foreground">
                  {{ t('admin.upstream.modal.form.rechargeRateHelp') }}
                </p>
              </div>

              <!-- Remark -->
              <div class="space-y-2">
                <label for="upstream-site-remark" class="ml-2.5 text-sm font-medium text-foreground">
                  {{ t('admin.upstream.modal.form.remark') }}
                </label>
                <Input
                  id="upstream-site-remark"
                  v-model="newSiteForm.remark"
                  name="remark"
                  :placeholder="t('admin.upstream.modal.form.remarkPlaceholder')"
                  :disabled="isAdding"
                  class="bg-surface border-border/50 focus:border-primary h-10"
                />
              </div>
            </div>

            <!-- Actions -->
            <div class="flex items-center justify-end gap-3 pt-4 border-t border-border/40 mt-6">
              <Button type="button" variant="ghost" :disabled="isAdding" @click="closeSiteModal" class="hover:bg-surface-line">
                {{ t('admin.upstream.modal.cancel') }}
              </Button>
              <Button type="submit" :disabled="isAdding" class="bg-primary text-primary-foreground hover:bg-primary/90">
                <Loader2 v-if="isAdding" class="h-4 w-4 animate-spin" />
              {{ isAdding ? t('admin.upstream.modal.submitting') : t(editingSiteId ? 'admin.upstream.modal.updateSubmit' : 'admin.upstream.modal.submit') }}
            </Button>
            </div>
          </form>
        </div>
      </div>
    </Teleport>

    <SiteSettingsModal
      :open="isSiteSettingsOpen"
      :site="selectedSiteForSettings"
      @close="closeSiteSettings"
      @saved="onSiteSettingsSaved"
    />
    <SettlementModal
      :open="isSettlementOpen"
      :site="selectedSiteForSettlement"
      @close="closeSettlement"
      @updated="onSettlementUpdated"
    />
    <LedgerModal
      :open="isLedgerOpen"
      :site="selectedSiteForLedger"
      @close="closeLedger"
      @updated="onLedgerUpdated"
    />
    <SubscriptionTopupModal
      :open="isSubscriptionTopupOpen"
      :site="selectedSiteForSubscriptionTopup"
      :subscription="selectedSubscriptionForTopup"
      @close="closeSubscriptionTopup"
      @updated="onSubscriptionTopupUpdated"
    />
  </div>
</template>
