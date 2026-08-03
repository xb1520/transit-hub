<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  AlertCircle,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Loader2,
  Plus,
  RefreshCw,
  Search,
  Wallet,
  X,
} from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  listSiteUsers,
  patchSiteBalanceSettings,
  searchSiteUserCandidates,
  type SiteUserItem,
} from '../../api/siteUsers'
import SiteUserTopupModal from './SiteUserTopupModal.vue'
import SiteUserUsageModal from './SiteUserUsageModal.vue'

const props = defineProps<{
  open: boolean
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'updated'): void
}>()

const { t } = useI18n()

const loading = ref(false)
const savingRate = ref(false)
const errorKey = ref('')
const messageKey = ref('')

const users = ref<SiteUserItem[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const pages = ref(1)
const pageJumpDraft = ref('')
const searchDraft = ref('')
const searchFilter = ref('')
const sortBy = ref('balance')
const sortOrder = ref<'asc' | 'desc'>('desc')

const siteRate = ref('1')
const excludeAdmin = ref(true)
/** 整户排除名单（测试号等） */
const excludeUserIds = ref<string[]>([])
const excludeDraft = ref('')
const excludeCandidates = ref<SiteUserItem[]>([])
const excludeCandidatesOpen = ref(false)
let excludeSearchTimer: ReturnType<typeof setTimeout> | null = null
const pageCostTotal = ref(0)
const pageRevenueTotal = ref(0)

const ledgerOpen = ref(false)
const ledgerUser = ref<SiteUserItem | null>(null)
const usageOpen = ref(false)
const usageUser = ref<SiteUserItem | null>(null)

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
}

const totalPages = computed(() => Math.max(1, pages.value || 1))

const ratePreview = computed(() => {
  const n = Number.parseFloat(siteRate.value)
  return Number.isFinite(n) && n > 0 ? n : null
})

const load = async (opts?: { silent?: boolean }) => {
  // 已有列表时静默刷新，避免整表替换成 spinner 白屏闪烁
  const silent = !!opts?.silent && users.value.length > 0
  if (!silent) loading.value = true
  errorKey.value = ''
  messageKey.value = ''
  try {
    const resp = await listSiteUsers({
      page: page.value,
      pageSize: pageSize.value,
      search: searchFilter.value || undefined,
      sortBy: sortBy.value,
      sortOrder: sortOrder.value,
      hideExcluded: true,
    })
    users.value = resp.items || []
    total.value = resp.total ?? 0
    page.value = resp.page || page.value
    pages.value = resp.pages || 1
    if (resp.siteRechargeRate != null && resp.siteRechargeRate > 0) {
      siteRate.value = String(resp.siteRechargeRate)
    }
    if (typeof resp.excludeAdmin === 'boolean') {
      excludeAdmin.value = resp.excludeAdmin
    }
    if (Array.isArray(resp.excludeUserIds)) {
      excludeUserIds.value = [...resp.excludeUserIds]
    }
    pageCostTotal.value = resp.pageCostTotal ?? 0
    pageRevenueTotal.value = resp.pageRevenueTotal ?? 0
    pageJumpDraft.value = String(page.value)
    if (resp.messageKey) messageKey.value = resp.messageKey
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (open) => {
  if (open) {
    page.value = 1
    ledgerOpen.value = false
    ledgerUser.value = null
    void load()
  }
})

const applySearch = () => {
  searchFilter.value = searchDraft.value.trim()
  page.value = 1
  void load()
}

const toggleSort = (key: string) => {
  if (sortBy.value === key) {
    sortOrder.value = sortOrder.value === 'desc' ? 'asc' : 'desc'
  } else {
    sortBy.value = key
    sortOrder.value = key === 'email' || key === 'username' ? 'asc' : 'desc'
  }
  page.value = 1
  void load()
}

const sortIcon = (key: string) => {
  if (sortBy.value !== key) return ArrowUpDown
  return sortOrder.value === 'asc' ? ArrowUp : ArrowDown
}

const goPage = (next: number) => {
  if (next < 1 || next > totalPages.value || next === page.value) return
  page.value = next
  pageJumpDraft.value = String(next)
  void load()
}

const jumpToPage = () => {
  const n = Number.parseInt(pageJumpDraft.value.trim(), 10)
  if (!Number.isFinite(n)) {
    pageJumpDraft.value = String(page.value)
    return
  }
  goPage(Math.min(totalPages.value, Math.max(1, n)))
}

const saveSettings = async () => {
  const rate = Number.parseFloat(siteRate.value)
  if (!Number.isFinite(rate) || rate <= 0) {
    errorKey.value = 'admin.siteUsers.errors.invalidRate'
    return
  }
  savingRate.value = true
  errorKey.value = ''
  try {
    await patchSiteBalanceSettings({
      siteRechargeRate: rate,
      excludeAdmin: excludeAdmin.value,
      excludeUserIds: [...excludeUserIds.value],
    })
    emit('updated')
    page.value = 1
    await load()
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    savingRate.value = false
  }
}

const onExcludeDraftInput = () => {
  const q = excludeDraft.value.trim()
  if (excludeSearchTimer) clearTimeout(excludeSearchTimer)
  if (q.length < 1) {
    excludeCandidates.value = []
    excludeCandidatesOpen.value = false
    return
  }
  excludeSearchTimer = setTimeout(async () => {
    try {
      const resp = await searchSiteUserCandidates(q, 8)
      excludeCandidates.value = resp.items || []
      excludeCandidatesOpen.value = true
    } catch {
      excludeCandidates.value = []
    }
  }, 280)
}

const addExcludeId = (id: string) => {
  const trimmed = id.trim()
  if (!trimmed) return
  if (!excludeUserIds.value.includes(trimmed)) {
    excludeUserIds.value = [...excludeUserIds.value, trimmed]
  }
  excludeDraft.value = ''
  excludeCandidates.value = []
  excludeCandidatesOpen.value = false
}

const removeExcludeId = (id: string) => {
  excludeUserIds.value = excludeUserIds.value.filter((x) => x !== id)
}

const openUserLedger = (user: SiteUserItem) => {
  ledgerUser.value = user
  ledgerOpen.value = true
}

const closeUserLedger = () => {
  ledgerOpen.value = false
  ledgerUser.value = null
}

const openUserUsage = (user: SiteUserItem) => {
  usageUser.value = user
  usageOpen.value = true
}

const closeUserUsage = () => {
  usageOpen.value = false
  usageUser.value = null
}

const onLedgerUpdated = async () => {
  emit('updated')
  await load({ silent: true })
}

const userDisplay = (u: SiteUserItem) => u.email || u.username || u.id

const formatLastUsed = (iso?: string | null) => {
  if (!iso) return t('admin.siteUsers.neverUsed')
  try {
    const d = new Date(iso)
    if (Number.isNaN(d.getTime())) return iso
    return d.toLocaleString()
  } catch {
    return iso
  }
}
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')" />
      <div
        role="dialog"
        aria-modal="true"
        class="relative flex max-h-[90vh] w-full max-w-4xl flex-col overflow-hidden rounded-[2rem] border border-border/60 bg-card shadow-2xl"
      >
        <div class="absolute left-0 right-0 top-0 h-1 bg-gradient-to-r from-accent via-primary to-accent" />

        <div class="flex items-start justify-between gap-4 border-b border-border/40 px-6 pt-6 pb-4">
          <div class="flex items-center gap-3">
            <div class="flex h-11 w-11 items-center justify-center rounded-full bg-accent/10 text-accent">
              <Wallet class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-lg font-semibold text-foreground">{{ t('admin.siteBalance.title') }}</h2>
              <p class="text-sm text-muted-foreground">{{ t('admin.siteBalance.subtitle') }}</p>
            </div>
          </div>
          <div class="flex items-center gap-1">
            <button
              type="button"
              class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated"
              :disabled="loading"
              @click="() => load()"
            >
              <RefreshCw class="h-4 w-4" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
              <X class="h-5 w-5" />
            </button>
          </div>
        </div>

        <div class="space-y-4 overflow-y-auto px-6 py-4">
          <!-- 设置：倍率 + 排除规则 -->
          <div class="space-y-3 rounded-xl border border-border/50 bg-surface/40 px-3 py-3">
            <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
              <div class="flex items-center gap-2">
                <span class="shrink-0 text-xs font-medium text-muted-foreground">{{ t('admin.siteBalance.rechargeRate') }}</span>
                <div class="flex items-center gap-1.5">
                  <span class="text-xs text-muted-foreground tabular-nums">1 USD</span>
                  <span class="text-xs text-muted-foreground">→</span>
                  <Input
                    v-model="siteRate"
                    type="number"
                    min="0.0001"
                    step="0.01"
                    class="h-8 w-[5.5rem] text-center font-mono text-sm tabular-nums"
                  />
                  <span class="text-xs text-muted-foreground">CNY</span>
                </div>
                <span
                  v-if="ratePreview != null"
                  class="hidden rounded-md bg-primary/10 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-primary sm:inline"
                >
                  ×{{ ratePreview }}
                </span>
              </div>

              <div class="hidden h-4 w-px bg-border/60 sm:block" />

              <button
                type="button"
                role="switch"
                :aria-checked="excludeAdmin"
                class="inline-flex items-center gap-2 text-xs text-muted-foreground transition-colors hover:text-foreground"
                @click="excludeAdmin = !excludeAdmin"
              >
                <span
                  class="relative h-5 w-9 shrink-0 rounded-full transition-colors"
                  :class="excludeAdmin ? 'bg-primary' : 'bg-muted'"
                >
                  <span
                    class="absolute top-0.5 left-0.5 h-4 w-4 rounded-full bg-white shadow transition-transform"
                    :class="excludeAdmin ? 'translate-x-4' : 'translate-x-0'"
                  />
                </span>
                {{ t('admin.siteBalance.excludeAdmin') }}
              </button>

              <div class="ml-auto">
                <Button size="sm" class="h-8" :disabled="savingRate" @click="saveSettings">
                  <Loader2 v-if="savingRate" class="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  {{ t('admin.siteBalance.saveSettings') }}
                </Button>
              </div>
            </div>

            <!-- 整户排除指定用户 -->
            <div class="border-t border-border/40 pt-2.5">
              <div class="mb-1.5 text-xs font-medium text-muted-foreground">{{ t('admin.siteBalance.excludeUsers') }}</div>
              <p class="mb-2 text-[11px] leading-4 text-muted-foreground">{{ t('admin.siteBalance.excludeUsersHelp') }}</p>
              <div v-if="excludeUserIds.length" class="mb-2 flex flex-wrap gap-1.5">
                <span
                  v-for="id in excludeUserIds"
                  :key="id"
                  class="inline-flex items-center gap-1 rounded-lg border border-border/60 bg-card px-2 py-0.5 text-xs font-mono"
                >
                  {{ id }}
                  <button type="button" class="text-muted-foreground hover:text-destructive" @click="removeExcludeId(id)">
                    <X class="h-3 w-3" />
                  </button>
                </span>
              </div>
              <div class="relative flex gap-2">
                <Input
                  v-model="excludeDraft"
                  class="h-8 flex-1 text-sm"
                  :placeholder="t('admin.siteBalance.excludeUsersPlaceholder')"
                  @input="onExcludeDraftInput"
                  @keydown.enter.prevent="addExcludeId(excludeDraft)"
                />
                <Button size="sm" variant="secondary" class="h-8 shrink-0" @click="addExcludeId(excludeDraft)">
                  <Plus class="h-3.5 w-3.5" />
                </Button>
                <div
                  v-if="excludeCandidatesOpen && excludeCandidates.length"
                  class="absolute left-0 right-12 top-full z-20 mt-1 max-h-48 overflow-y-auto rounded-xl border border-border/60 bg-card shadow-xl"
                >
                  <button
                    v-for="c in excludeCandidates"
                    :key="c.id"
                    type="button"
                    class="flex w-full items-center justify-between gap-2 border-b border-border/30 px-3 py-2 text-left text-xs last:border-0 hover:bg-surface-elevated"
                    @click="addExcludeId(c.id)"
                  >
                    <span class="min-w-0 truncate">
                      <span class="font-medium">{{ c.email || c.username || c.id }}</span>
                      <span v-if="c.notes" class="ml-1 text-muted-foreground">· {{ c.notes }}</span>
                    </span>
                    <span class="shrink-0 font-mono text-muted-foreground">{{ c.id }}</span>
                  </button>
                </div>
              </div>
            </div>
          </div>
          <p class="px-0.5 text-[11px] leading-4 text-muted-foreground">
            {{ t('admin.siteBalance.rechargeRateHelp') }}
          </p>

          <div class="grid grid-cols-2 gap-2 text-xs sm:grid-cols-2">
            <div class="rounded-lg border border-border/40 px-3 py-2">
              <div class="text-muted-foreground">{{ t('admin.siteBalance.pageCost') }}</div>
              <div class="mt-0.5 text-base font-semibold tabular-nums">¥{{ formatMoney(pageCostTotal) }}</div>
              <div v-if="ratePreview" class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">
                ${{ formatMoney(pageCostTotal / ratePreview) }}
              </div>
            </div>
            <div class="rounded-lg border border-border/40 px-3 py-2">
              <div class="text-muted-foreground">{{ t('admin.siteBalance.pageRecharge') }}</div>
              <div class="mt-0.5 text-base font-semibold tabular-nums">¥{{ formatMoney(pageRevenueTotal) }}</div>
              <div v-if="ratePreview" class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">
                ${{ formatMoney(pageRevenueTotal / ratePreview) }}
              </div>
            </div>
          </div>

          <div class="flex gap-2">
            <div class="relative flex-1">
              <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                v-model="searchDraft"
                class="pl-9"
                :placeholder="t('admin.siteUsers.searchPlaceholder')"
                @keydown.enter.prevent="applySearch"
              />
            </div>
            <Button variant="secondary" @click="applySearch">{{ t('admin.siteUsers.search') }}</Button>
          </div>

          <p v-if="messageKey" class="text-sm text-muted-foreground">{{ t(messageKey) }}</p>
          <p v-if="errorKey" class="flex items-center gap-2 text-sm text-destructive">
            <AlertCircle class="h-4 w-4" />{{ t(errorKey) }}
          </p>

          <div class="relative overflow-x-auto rounded-xl border border-border/50">
            <div
              v-if="loading && !users.length"
              class="flex justify-center py-12 text-muted-foreground"
            >
              <Loader2 class="h-6 w-6 animate-spin" />
            </div>
            <table v-else class="w-full min-w-[640px] text-left text-sm" :class="loading ? 'opacity-80' : ''">
              <thead class="border-b border-border/40 bg-surface/50 text-xs text-muted-foreground">
                <tr>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('id')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteUsers.sort.id') }}
                      <component :is="sortIcon('id')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('email')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteBalance.colUser') }}
                      <component :is="sortIcon('email')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('balance_cny')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteBalance.colBalanceCny') }}
                      <component :is="sortIcon('balance_cny')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('recharge')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteBalance.colRechargeTotal') }}
                      <component :is="sortIcon('recharge')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('gift')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteBalance.colGift') }}
                      <component :is="sortIcon('gift')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('rebate')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteBalance.colRebate') }}
                      <component :is="sortIcon('rebate')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="cursor-pointer select-none px-3 py-2.5 font-medium" @click="toggleSort('last_used_at')">
                    <span class="inline-flex items-center gap-1">
                      {{ t('admin.siteUsers.lastUsed') }}
                      <component :is="sortIcon('last_used_at')" class="h-3.5 w-3.5" />
                    </span>
                  </th>
                  <th class="px-3 py-2.5 font-medium">{{ t('admin.siteBalance.colAction') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-border/30">
                <tr v-if="!users.length">
                  <td colspan="8" class="px-3 py-10 text-center text-muted-foreground">
                    {{ t('admin.siteUsers.empty') }}
                  </td>
                </tr>
                <tr
                  v-for="u in users"
                  :key="u.id"
                  class="hover:bg-surface-elevated/40"
                >
                  <td class="px-3 py-2.5 font-mono text-xs tabular-nums text-muted-foreground">
                    {{ u.id }}
                  </td>
                  <td class="px-3 py-2.5">
                    <div class="font-medium text-foreground">{{ userDisplay(u) }}</div>
                    <div class="text-[11px] text-muted-foreground">
                      {{ u.role || '—' }} · {{ u.status || '—' }}
                    </div>
                    <div
                      v-if="u.notes"
                      class="mt-0.5 max-w-[14rem] truncate text-[11px] text-foreground/80"
                      :title="u.notes"
                    >
                      {{ t('admin.siteUsers.notes') }}：{{ u.notes }}
                    </div>
                  </td>
                  <td class="px-3 py-2.5 font-mono tabular-nums font-semibold">
                    <div>¥{{ formatMoney(u.balanceCny) }}</div>
                    <div v-if="u.balance != null" class="text-[10px] font-normal text-muted-foreground">${{ formatMoney(u.balance) }}</div>
                  </td>
                  <td class="px-3 py-2.5 font-mono tabular-nums text-sky-700 dark:text-sky-300">
                    <div>¥{{ formatMoney((u.markedRecharge || 0) * (ratePreview || 1)) }}</div>
                    <div class="text-[10px] opacity-70">${{ formatMoney(u.markedRecharge) }}</div>
                  </td>
                  <td class="px-3 py-2.5 font-mono tabular-nums text-emerald-700 dark:text-emerald-300">
                    <div>¥{{ formatMoney((u.markedGift || 0) * (ratePreview || 1)) }}</div>
                    <div class="text-[10px] opacity-70">${{ formatMoney(u.markedGift) }}</div>
                  </td>
                  <td class="px-3 py-2.5 font-mono tabular-nums text-amber-700 dark:text-amber-300">
                    <div>¥{{ formatMoney((u.markedRebate || 0) * (ratePreview || 1)) }}</div>
                    <div class="text-[10px] opacity-70">${{ formatMoney(u.markedRebate) }}</div>
                  </td>
                  <td class="px-3 py-2.5 text-[11px] text-muted-foreground">
                    {{ formatLastUsed(u.lastUsedAt) }}
                  </td>
                  <td class="px-3 py-2.5">
                    <div class="flex flex-wrap gap-1">
                      <Button size="sm" variant="secondary" class="h-7 text-xs" @click="openUserUsage(u)">
                        {{ t('admin.siteUsers.usage.detailBtn') }}
                      </Button>
                      <Button size="sm" variant="secondary" class="h-7 text-xs" @click="openUserLedger(u)">
                        {{ t('admin.siteBalance.markLedger') }}
                      </Button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="flex flex-wrap items-center justify-between gap-2 pb-2">
            <span class="text-xs text-muted-foreground">{{ t('admin.siteUsers.total', { count: total }) }}</span>
            <div class="flex items-center gap-1.5">
              <Button variant="ghost" size="sm" :disabled="page <= 1 || loading" @click="goPage(page - 1)">
                ‹
              </Button>
              <Input
                v-model="pageJumpDraft"
                type="number"
                min="1"
                :max="totalPages"
                class="h-7 w-14 px-1.5 text-center text-xs tabular-nums"
                :disabled="loading"
                @keydown.enter.prevent="jumpToPage"
              />
              <span class="text-xs text-muted-foreground">/ {{ totalPages }}</span>
              <Button variant="secondary" size="sm" class="h-7 px-2 text-xs" :disabled="loading" @click="jumpToPage">
                {{ t('admin.siteUsers.jumpPage') }}
              </Button>
              <Button variant="ghost" size="sm" :disabled="page >= totalPages || loading" @click="goPage(page + 1)">
                ›
              </Button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <SiteUserTopupModal
      :open="ledgerOpen"
      :user="ledgerUser"
      :site-recharge-rate="ratePreview"
      @close="closeUserLedger"
      @updated="onLedgerUpdated"
    />
    <SiteUserUsageModal
      :open="usageOpen"
      :user="usageUser"
      :site-recharge-rate="ratePreview"
      @close="closeUserUsage"
    />
  </Teleport>
</template>
