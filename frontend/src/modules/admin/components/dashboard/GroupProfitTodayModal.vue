<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  ArrowDownWideNarrow,
  ArrowUpWideNarrow,
  ChevronDown,
  ChevronRight,
  Gauge,
  Loader2,
  PiggyBank,
  RefreshCw,
  X,
} from 'lucide-vue-next'
import {
  getGroupProfitToday,
  type GroupProfitTodayItem,
  type UpstreamGroupProfitTodayItem,
} from '../../api/dashboardAdmin'
import { useCurrencyDisplay } from '../../composables/useCurrencyDisplay'

const props = defineProps<{
  open: boolean
  /** 打开来源：净利润卡 / 利润率卡，仅影响标题文案。 */
  focus?: 'profit' | 'margin'
  /**
   * 仪表盘已加载的管理站充值倍率（兜底）。
   * 接口返回的 siteRechargeRate 优先；两者都无效时按 1。
   */
  siteRechargeRate?: number
}>()

const emit = defineEmits<{
  (event: 'close'): void
}>()

const { t, locale } = useI18n()
const { displayMode, formatMoneyParts } = useCurrencyDisplay()

const loading = ref(false)
const error = ref<string | null>(null)
const groups = ref<GroupProfitTodayItem[]>([])
const unmappedUpstreams = ref<UpstreamGroupProfitTodayItem[]>([])
const upstreamPartial = ref(false)
const totalProfit = ref(0)
const totalMargin = ref(0)
const totalRevenue = ref(0)
/** 管理站充值倍率：后端已把平台营收 × rate 成成本/CNY；双币种时 USD = CNY / rate。 */
const siteRate = ref(1)
// 默认按利润从高到低；toggle 后按利润从低到高，金额相同时用分组名排序。
const sortAsc = ref(false)
/** 展开的自有分组名集合。 */
const expanded = ref<Set<string>>(new Set())

const percentFormatter = computed(() => new Intl.NumberFormat(locale.value, {
  style: 'percent',
  maximumFractionDigits: 1,
  minimumFractionDigits: 1,
}))

const formatMargin = (value: number): string => percentFormatter.value.format(Number.isFinite(value) ? value : 0)

/** 成本分摊时主显预算利润率（各上游不同）；账号真实时主显实际利润率。 */
const displayMargin = (up: UpstreamGroupProfitTodayItem): number => {
  if (up.revenueSource === 'allocated' && up.budgetMargin != null && Number.isFinite(up.budgetMargin)) {
    return up.budgetMargin
  }
  return Number.isFinite(up.profitMargin) ? up.profitMargin : 0
}

/**
 * 分组利润接口金额均为成本/CNY 口径。
 * - 只传 cny：USD = cny / siteRate
 * - 同时传 usd（如 revenuePlatform）：避免浮点二次误差
 */
const money = (cny: number | null | undefined, usd?: number | null) => {
  void displayMode.value
  const rate = siteRate.value > 0 ? siteRate.value : 1
  return formatMoneyParts({
    cny,
    usd: usd != null && Number.isFinite(usd) ? usd : null,
    rate,
  })
}

const moneyText = (cny: number | null | undefined, usd?: number | null): string => {
  const parts = money(cny, usd)
  return parts.secondary ? `${parts.primary} / ${parts.secondary}` : parts.primary
}

const sortedGroups = computed(() => {
  return [...groups.value].sort((a, b) => {
    const primary = sortAsc.value ? a.profit - b.profit : b.profit - a.profit
    if (primary !== 0) return primary
    const secondary = sortAsc.value ? a.profitMargin - b.profitMargin : b.profitMargin - a.profitMargin
    if (secondary !== 0) return secondary
    return a.groupName.localeCompare(b.groupName)
  })
})

const sortedUnmapped = computed(() => {
  return [...unmappedUpstreams.value].sort((a, b) => {
    if (a.cost !== b.cost) return sortAsc.value ? a.cost - b.cost : b.cost - a.cost
    return a.groupName.localeCompare(b.groupName) || a.siteName.localeCompare(b.siteName)
  })
})

const nestedUpstreamCount = computed(() =>
  groups.value.reduce((n, g) => n + (g.upstreams?.length ?? 0), 0) + unmappedUpstreams.value.length,
)

const hasAnyRows = computed(() => sortedGroups.value.length > 0 || sortedUnmapped.value.length > 0)

const toggleSort = () => {
  sortAsc.value = !sortAsc.value
}

const isExpanded = (name: string) => expanded.value.has(name)

const toggleExpand = (name: string) => {
  const next = new Set(expanded.value)
  if (next.has(name)) next.delete(name)
  else next.add(name)
  expanded.value = next
}

const title = computed(() => props.focus === 'margin'
  ? t('admin.dashboard.groupProfit.titleMargin')
  : t('admin.dashboard.groupProfit.titleProfit'))

const platformLabel = (platform: string): string => {
  const key = `admin.upstream.modal.form.platforms.${platform}`
  const label = t(key)
  return label === key ? platform : label
}

const loadData = async () => {
  loading.value = true
  error.value = null
  try {
    const response = await getGroupProfitToday()
    groups.value = response.groups ?? []
    unmappedUpstreams.value = response.unmappedUpstreams ?? []
    upstreamPartial.value = Boolean(response.upstreamPartial)
    totalProfit.value = response.totalProfit ?? 0
    totalMargin.value = response.totalMargin ?? 0
    totalRevenue.value = response.totalRevenue ?? 0
    const fromApi = response.siteRechargeRate
    const fromProp = props.siteRechargeRate
    if (fromApi != null && fromApi > 0) {
      siteRate.value = fromApi
    } else if (fromProp != null && fromProp > 0) {
      siteRate.value = fromProp
    } else {
      siteRate.value = 1
    }
    // 默认展开所有有上游的自有分组，方便对照。
    const next = new Set<string>()
    for (const g of groups.value) {
      if ((g.upstreams?.length ?? 0) > 0) next.add(g.groupName)
    }
    expanded.value = next
  } catch (err) {
    error.value = err instanceof Error ? err.message : 'admin.dashboard.groupProfit.loadError'
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (isOpen) => {
  if (isOpen) {
    void loadData()
  }
})
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')"></div>

      <div
        role="dialog"
        aria-modal="true"
        class="relative w-full max-w-4xl overflow-hidden rounded-[2rem] border border-border/60 bg-card text-card-foreground shadow-2xl shadow-primary/10 animate-in fade-in zoom-in-95 duration-200"
      >
        <div class="absolute left-0 right-0 top-0 h-1 bg-gradient-to-r from-signal via-accent to-primary" />

        <div class="flex items-start justify-between gap-4 px-6 pt-6">
          <div class="flex items-center gap-3">
            <div
              class="flex h-11 w-11 items-center justify-center rounded-full"
              :class="focus === 'margin' ? 'bg-accent/10 text-accent' : 'bg-signal/10 text-signal'"
            >
              <Gauge v-if="focus === 'margin'" class="h-5 w-5" />
              <PiggyBank v-else class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-lg font-semibold text-foreground">{{ title }}</h2>
              <p class="text-sm text-muted-foreground">
                {{ t('admin.dashboard.groupProfit.subtitle', {
                  ownCount: groups.length,
                  profit: moneyText(totalProfit),
                  margin: formatMargin(totalMargin),
                }) }}
              </p>
            </div>
          </div>
          <div class="flex items-center gap-2">
            <button
              type="button"
              :disabled="loading || !!error || !hasAnyRows"
              class="inline-flex items-center gap-1.5 rounded-lg border border-border/60 px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:border-primary/40 hover:text-foreground disabled:opacity-50"
              @click="toggleSort"
            >
              <ArrowUpWideNarrow v-if="sortAsc" class="h-3.5 w-3.5" />
              <ArrowDownWideNarrow v-else class="h-3.5 w-3.5" />
              {{ sortAsc ? t('admin.dashboard.groupProfit.sort.asc') : t('admin.dashboard.groupProfit.sort.desc') }}
            </button>
            <button
              type="button"
              class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground"
              :title="t('admin.dashboard.groupProfit.close')"
              @click="emit('close')"
            >
              <X class="h-5 w-5" />
            </button>
          </div>
        </div>

        <div class="px-6 py-6">
          <div v-if="loading" class="flex items-center justify-center py-12">
            <Loader2 class="h-6 w-6 animate-spin text-primary/60" />
          </div>

          <div
            v-else-if="error"
            class="flex flex-col items-center justify-center gap-3 py-12 text-center"
          >
            <p class="text-sm text-muted-foreground">{{ t(error) }}</p>
            <button
              type="button"
              class="inline-flex items-center gap-1.5 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90"
              @click="loadData"
            >
              <RefreshCw class="h-4 w-4" />
              {{ t('admin.dashboard.groupProfit.retry') }}
            </button>
          </div>

          <div
            v-else-if="!hasAnyRows"
            class="flex flex-col items-center justify-center gap-2 py-12 text-center"
          >
            <PiggyBank class="h-8 w-8 text-muted-foreground/40" />
            <p class="text-sm text-muted-foreground">{{ t('admin.dashboard.groupProfit.empty') }}</p>
          </div>

          <div v-else class="max-h-[65vh] space-y-4 overflow-y-auto pr-1">
            <p class="text-xs text-muted-foreground">
              {{ t('admin.dashboard.groupProfit.hint', { revenue: moneyText(totalRevenue) }) }}
            </p>
            <p
              v-if="upstreamPartial"
              class="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-400"
            >
              {{ t('admin.dashboard.groupProfit.upstreamPartial') }}
            </p>

            <div class="overflow-hidden rounded-xl border border-border/60">
              <table class="w-full text-sm">
                <thead class="bg-surface/90">
                  <tr class="border-b border-border/60 text-left text-xs font-medium text-muted-foreground">
                    <th class="px-4 py-3">{{ t('admin.dashboard.groupProfit.columns.groupName') }}</th>
                    <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.revenue') }}</th>
                    <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.cost') }}</th>
                    <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.profit') }}</th>
                    <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.margin') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <template v-for="group in sortedGroups" :key="`own-${group.groupName}`">
                    <!-- 自有分组主行 -->
                    <tr class="border-b border-border/40 bg-card">
                      <td class="px-4 py-3 align-middle">
                        <button
                          v-if="(group.upstreams?.length ?? 0) > 0"
                          type="button"
                          class="inline-flex max-w-full items-center gap-1.5 text-left font-medium text-foreground hover:text-primary"
                          :aria-expanded="isExpanded(group.groupName)"
                          :title="isExpanded(group.groupName)
                            ? t('admin.dashboard.groupProfit.collapse')
                            : t('admin.dashboard.groupProfit.expand')"
                          @click="toggleExpand(group.groupName)"
                        >
                          <ChevronDown v-if="isExpanded(group.groupName)" class="h-4 w-4 shrink-0 text-muted-foreground" />
                          <ChevronRight v-else class="h-4 w-4 shrink-0 text-muted-foreground" />
                          <span class="truncate">{{ group.groupName }}</span>
                          <span class="shrink-0 text-[11px] font-normal text-muted-foreground">
                            {{ t('admin.dashboard.groupProfit.upstreamCount', { count: group.upstreams?.length ?? 0 }) }}
                          </span>
                        </button>
                        <span v-else class="font-medium text-foreground">{{ group.groupName }}</span>
                      </td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-foreground">
                        <div>{{ money(group.revenue, group.revenuePlatform).primary }}</div>
                        <div
                          v-if="money(group.revenue, group.revenuePlatform).secondary"
                          class="text-[11px] font-normal text-muted-foreground"
                        >
                          {{ money(group.revenue, group.revenuePlatform).secondary }}
                        </div>
                      </td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-muted-foreground">
                        <div>{{ money(group.cost).primary }}</div>
                        <div
                          v-if="money(group.cost).secondary"
                          class="text-[11px] font-normal text-muted-foreground/80"
                        >
                          {{ money(group.cost).secondary }}
                        </div>
                        <div
                          v-if="group.probeCost != null && group.probeCost > 0"
                          class="text-[11px] font-normal text-muted-foreground/80"
                        >
                          {{ t('admin.dashboard.groupProfit.probeCost', { cost: money(group.probeCost).primary }) }}
                        </div>
                      </td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profit >= 0 ? 'text-signal' : 'text-destructive'"
                      >
                        <div>{{ money(group.profit).primary }}</div>
                        <div
                          v-if="money(group.profit).secondary"
                          class="text-[11px] font-normal text-muted-foreground"
                        >
                          {{ money(group.profit).secondary }}
                        </div>
                      </td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profitMargin >= 0 ? 'text-foreground' : 'text-destructive'"
                      >
                        {{ formatMargin(group.profitMargin) }}
                      </td>
                    </tr>

                    <!-- 嵌套上游子行 -->
                    <template v-if="isExpanded(group.groupName)">
                      <tr
                        v-for="up in group.upstreams"
                        :key="`up-${group.groupName}-${up.siteId}-${up.groupName}`"
                        class="border-b border-border/30 bg-surface/40 last:border-b-0"
                      >
                        <td class="px-4 py-2.5 align-middle pl-10">
                          <div class="text-sm text-foreground">{{ up.groupName }}</div>
                          <div class="mt-0.5 text-[11px] text-muted-foreground">
                            {{ up.siteName || up.siteId }}
                            <span v-if="up.platform"> · {{ platformLabel(up.platform) }}</span>
                            <span v-if="up.revenueSource === 'account'">
                              · {{ t('admin.dashboard.groupProfit.revenueSourceAccount') }}
                            </span>
                            <span v-else-if="up.revenueSource === 'allocated'">
                              · {{ t('admin.dashboard.groupProfit.revenueSourceAllocated') }}
                            </span>
                          </div>
                        </td>
                        <td class="px-4 py-2.5 align-middle text-right tabular-nums text-foreground">
                          <div>{{ money(up.revenue).primary }}</div>
                          <div
                            v-if="money(up.revenue).secondary"
                            class="text-[11px] font-normal text-muted-foreground"
                          >
                            {{ money(up.revenue).secondary }}
                          </div>
                        </td>
                        <td class="px-4 py-2.5 align-middle text-right tabular-nums text-muted-foreground">
                          <div>{{ money(up.cost).primary }}</div>
                          <div
                            v-if="money(up.cost).secondary"
                            class="text-[11px] font-normal text-muted-foreground/80"
                          >
                            {{ money(up.cost).secondary }}
                          </div>
                          <div
                            v-if="up.probeCost != null && up.probeCost > 0"
                            class="text-[11px] font-normal text-muted-foreground/80"
                          >
                            {{ t('admin.dashboard.groupProfit.probeCost', { cost: money(up.probeCost).primary }) }}
                          </div>
                        </td>
                        <td
                          class="px-4 py-2.5 align-middle text-right tabular-nums font-medium"
                          :class="up.profit >= 0 ? 'text-signal' : 'text-destructive'"
                        >
                          <div>{{ money(up.profit).primary }}</div>
                          <div
                            v-if="money(up.profit).secondary"
                            class="text-[11px] font-normal text-muted-foreground"
                          >
                            {{ money(up.profit).secondary }}
                          </div>
                        </td>
                        <td
                          class="px-4 py-2.5 align-middle text-right tabular-nums font-medium"
                          :class="displayMargin(up) >= 0 ? 'text-foreground' : 'text-destructive'"
                        >
                          <!-- 成本分摊时实际利润率恒等于母行，主显预算更有区分度；账号真实时主显实际 -->
                          <div>{{ formatMargin(displayMargin(up)) }}</div>
                          <div
                            v-if="up.revenueSource === 'allocated' && up.budgetMargin != null && Number.isFinite(up.budgetMargin)"
                            class="text-[11px] font-normal text-muted-foreground"
                            :title="t('admin.dashboard.groupProfit.allocatedMarginHint')"
                          >
                            {{ t('admin.dashboard.groupProfit.actualMarginLabel', { margin: formatMargin(up.profitMargin) }) }}
                          </div>
                          <div
                            v-else-if="up.revenueSource === 'account' && up.budgetMargin != null && Number.isFinite(up.budgetMargin)"
                            class="text-[11px] font-normal text-muted-foreground"
                          >
                            {{ t('admin.dashboard.groupProfit.budgetMargin', { margin: formatMargin(up.budgetMargin) }) }}
                          </div>
                        </td>
                      </tr>
                    </template>
                  </template>
                </tbody>
              </table>
            </div>

            <!-- 未映射上游 -->
            <section v-if="sortedUnmapped.length > 0" class="space-y-2">
              <h3 class="text-sm font-semibold text-foreground">
                {{ t('admin.dashboard.groupProfit.unmappedTitle') }}
                <span class="ml-2 text-xs font-normal text-muted-foreground">
                  {{ t('admin.dashboard.groupProfit.upstreamCount', { count: sortedUnmapped.length }) }}
                </span>
              </h3>
              <p class="text-xs text-muted-foreground">
                {{ t('admin.dashboard.groupProfit.unmappedHint') }}
              </p>
              <div class="overflow-hidden rounded-xl border border-dashed border-border/60">
                <table class="w-full text-sm">
                  <thead class="bg-surface/90">
                    <tr class="border-b border-border/60 text-left text-xs font-medium text-muted-foreground">
                      <th class="px-4 py-3">{{ t('admin.dashboard.groupProfit.columns.upstreamGroup') }}</th>
                      <th class="px-4 py-3">{{ t('admin.dashboard.groupProfit.columns.site') }}</th>
                      <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.cost') }}</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr
                      v-for="up in sortedUnmapped"
                      :key="`unmap-${up.siteId}-${up.groupName}`"
                      class="border-b border-border/40 last:border-b-0"
                    >
                      <td class="px-4 py-3 align-middle font-medium text-foreground">{{ up.groupName }}</td>
                      <td class="px-4 py-3 align-middle">
                        <div class="text-foreground">{{ up.siteName || up.siteId }}</div>
                        <div v-if="up.platform" class="mt-0.5 text-[11px] text-muted-foreground">
                          {{ platformLabel(up.platform) }}
                        </div>
                      </td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-muted-foreground">
                        <div>{{ money(up.cost).primary }}</div>
                        <div
                          v-if="money(up.cost).secondary"
                          class="text-[11px] font-normal text-muted-foreground/80"
                        >
                          {{ money(up.cost).secondary }}
                        </div>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </section>

            <p v-if="nestedUpstreamCount === 0 && sortedGroups.length > 0" class="text-center text-xs text-muted-foreground">
              {{ t('admin.dashboard.groupProfit.unmappedHint') }}
            </p>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
