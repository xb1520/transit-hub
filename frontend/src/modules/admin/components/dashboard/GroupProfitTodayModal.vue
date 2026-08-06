<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowDownWideNarrow, ArrowUpWideNarrow, Gauge, Loader2, PiggyBank, RefreshCw, X } from 'lucide-vue-next'
import {
  getGroupProfitToday,
  type GroupProfitTodayItem,
  type UpstreamGroupProfitTodayItem,
} from '../../api/dashboardAdmin'
import { formatCny } from '../../utils/dashboard'

const props = defineProps<{
  open: boolean
  /** 打开来源：净利润卡 / 利润率卡，仅影响标题文案。 */
  focus?: 'profit' | 'margin'
}>()

const emit = defineEmits<{
  (event: 'close'): void
}>()

const { t, locale } = useI18n()

const loading = ref(false)
const error = ref<string | null>(null)
const groups = ref<GroupProfitTodayItem[]>([])
const upstreamGroups = ref<UpstreamGroupProfitTodayItem[]>([])
const upstreamPartial = ref(false)
const totalProfit = ref(0)
const totalMargin = ref(0)
const totalRevenue = ref(0)
// 默认按利润从高到低；toggle 后按利润从低到高，金额相同时用分组名排序。
const sortAsc = ref(false)

const percentFormatter = computed(() => new Intl.NumberFormat(locale.value, {
  style: 'percent',
  maximumFractionDigits: 1,
  minimumFractionDigits: 1,
}))

const formatMargin = (value: number): string => percentFormatter.value.format(Number.isFinite(value) ? value : 0)

const sortedGroups = computed(() => {
  return [...groups.value].sort((a, b) => {
    const primary = sortAsc.value ? a.profit - b.profit : b.profit - a.profit
    if (primary !== 0) return primary
    const secondary = sortAsc.value ? a.profitMargin - b.profitMargin : b.profitMargin - a.profitMargin
    if (secondary !== 0) return secondary
    return a.groupName.localeCompare(b.groupName)
  })
})

const sortedUpstreamGroups = computed(() => {
  return [...upstreamGroups.value].sort((a, b) => {
    const primary = sortAsc.value ? a.profit - b.profit : b.profit - a.profit
    if (primary !== 0) return primary
    const secondary = sortAsc.value ? a.cost - b.cost : b.cost - a.cost
    if (secondary !== 0) return secondary
    return a.groupName.localeCompare(b.groupName) || a.siteName.localeCompare(b.siteName)
  })
})

const hasAnyRows = computed(() => sortedGroups.value.length > 0 || sortedUpstreamGroups.value.length > 0)

const toggleSort = () => {
  sortAsc.value = !sortAsc.value
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
    upstreamGroups.value = response.upstreamGroups ?? []
    upstreamPartial.value = Boolean(response.upstreamPartial)
    totalProfit.value = response.totalProfit ?? 0
    totalMargin.value = response.totalMargin ?? 0
    totalRevenue.value = response.totalRevenue ?? 0
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
                  upstreamCount: upstreamGroups.length,
                  profit: formatCny(totalProfit),
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

          <div v-else class="max-h-[65vh] space-y-6 overflow-y-auto pr-1">
            <p class="text-xs text-muted-foreground">
              {{ t('admin.dashboard.groupProfit.hint', { revenue: formatCny(totalRevenue) }) }}
            </p>
            <p
              v-if="upstreamPartial"
              class="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-400"
            >
              {{ t('admin.dashboard.groupProfit.upstreamPartial') }}
            </p>

            <!-- 自有分组 -->
            <section v-if="sortedGroups.length > 0" class="space-y-2">
              <div class="flex items-baseline justify-between gap-3">
                <h3 class="text-sm font-semibold text-foreground">
                  {{ t('admin.dashboard.groupProfit.sections.own') }}
                </h3>
                <span class="text-xs text-muted-foreground">
                  {{ t('admin.dashboard.groupProfit.sections.ownCount', { count: sortedGroups.length }) }}
                </span>
              </div>
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
                    <tr
                      v-for="group in sortedGroups"
                      :key="`own-${group.groupName}`"
                      class="border-b border-border/40 last:border-b-0"
                    >
                      <td class="px-4 py-3 align-middle font-medium text-foreground">{{ group.groupName }}</td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-foreground">{{ formatCny(group.revenue) }}</td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-muted-foreground">{{ formatCny(group.cost) }}</td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profit >= 0 ? 'text-signal' : 'text-destructive'"
                      >
                        {{ formatCny(group.profit) }}
                      </td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profitMargin >= 0 ? 'text-foreground' : 'text-destructive'"
                      >
                        {{ formatMargin(group.profitMargin) }}
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </section>

            <!-- 上游分组 -->
            <section v-if="sortedUpstreamGroups.length > 0" class="space-y-2">
              <div class="flex items-baseline justify-between gap-3">
                <h3 class="text-sm font-semibold text-foreground">
                  {{ t('admin.dashboard.groupProfit.sections.upstream') }}
                </h3>
                <span class="text-xs text-muted-foreground">
                  {{ t('admin.dashboard.groupProfit.sections.upstreamCount', { count: sortedUpstreamGroups.length }) }}
                </span>
              </div>
              <p class="text-xs text-muted-foreground">
                {{ t('admin.dashboard.groupProfit.upstreamHint') }}
              </p>
              <div class="overflow-hidden rounded-xl border border-border/60">
                <table class="w-full min-w-[40rem] text-sm">
                  <thead class="bg-surface/90">
                    <tr class="border-b border-border/60 text-left text-xs font-medium text-muted-foreground">
                      <th class="px-4 py-3">{{ t('admin.dashboard.groupProfit.columns.upstreamGroup') }}</th>
                      <th class="px-4 py-3">{{ t('admin.dashboard.groupProfit.columns.site') }}</th>
                      <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.cost') }}</th>
                      <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.estRevenue') }}</th>
                      <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.profit') }}</th>
                      <th class="px-4 py-3 text-right">{{ t('admin.dashboard.groupProfit.columns.margin') }}</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr
                      v-for="group in sortedUpstreamGroups"
                      :key="`up-${group.siteId}-${group.groupName}`"
                      class="border-b border-border/40 last:border-b-0"
                    >
                      <td class="px-4 py-3 align-middle">
                        <div class="font-medium text-foreground">{{ group.groupName }}</div>
                        <div
                          v-if="group.mappedOwnGroups?.length"
                          class="mt-0.5 text-[11px] text-muted-foreground"
                        >
                          {{ t('admin.dashboard.groupProfit.mappedOwn', { groups: group.mappedOwnGroups.join('、') }) }}
                        </div>
                      </td>
                      <td class="px-4 py-3 align-middle">
                        <div class="text-foreground">{{ group.siteName || group.siteId }}</div>
                        <div v-if="group.platform" class="mt-0.5 text-[11px] text-muted-foreground">
                          {{ platformLabel(group.platform) }}
                        </div>
                      </td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-muted-foreground">{{ formatCny(group.cost) }}</td>
                      <td class="px-4 py-3 align-middle text-right tabular-nums text-foreground">{{ formatCny(group.revenue) }}</td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profit >= 0 ? 'text-signal' : 'text-destructive'"
                      >
                        {{ formatCny(group.profit) }}
                      </td>
                      <td
                        class="px-4 py-3 align-middle text-right tabular-nums font-medium"
                        :class="group.profitMargin >= 0 ? 'text-foreground' : 'text-destructive'"
                      >
                        {{ formatMargin(group.profitMargin) }}
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </section>

            <p
              v-else-if="sortedGroups.length > 0"
              class="rounded-xl border border-dashed border-border/60 px-4 py-6 text-center text-sm text-muted-foreground"
            >
              {{ t('admin.dashboard.groupProfit.upstreamEmpty') }}
            </p>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
