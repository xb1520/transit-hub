<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Activity,
  AlertTriangle,
  ArrowDown,
  ArrowDownUp,
  ArrowUp,
  ArrowUpDown,
  ChevronDown,
  ChevronRight,
  Clock3,
  Eye,
  Gauge,
  Radar,
  Settings2,
  ShieldCheck,
  ShieldQuestion,
  Zap,
} from 'lucide-vue-next'
import { Tooltip } from '@/components/ui/tooltip'
import {
  connectionHealthMessageKey,
  connectionHealthStateBadgeClass,
  formatConnectionHealthTime,
} from '../../composables/useConnectionHealth'
import type {
  AdminGroupAccount,
  AdminGroupHealth,
  ConnectionHealthState,
} from '../../types/connectionHealth'

const props = defineProps<{
  group: AdminGroupHealth
}>()

const emit = defineEmits<{
  (event: 'setup', group: AdminGroupHealth): void
  (event: 'probe', account: AdminGroupAccount): void
  (event: 'view-events', account: AdminGroupAccount): void
}>()

const { t, te } = useI18n()
const prefix = 'admin.connectionHealth'
const detailPrefix = `${prefix}.groupDetail`
const expandedTargetId = ref('')

type AccountSortKey = 'priority' | 'upstreamMultiplier'
type SortDir = 'asc' | 'desc'

const sortKey = ref<AccountSortKey | null>(null)
const sortDir = ref<SortDir>('asc')

// 切换分组只收起展开行；升降序选择跨分组保留，避免每次点左侧列表都丢排序。
watch(() => props.group.id, () => {
  expandedTargetId.value = ''
})

const toggleSort = (key: AccountSortKey) => {
  if (sortKey.value === key) {
    sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc'
    return
  }
  sortKey.value = key
  // 优先级默认升序（数值小更优先的平台更直观）；倍率默认升序（成本更低在前）。
  sortDir.value = 'asc'
}

const sortIndicator = (key: AccountSortKey) => {
  if (sortKey.value !== key) return 'none' as const
  return sortDir.value
}

const monitoredCount = computed(() => props.group.monitoredAccountCount ?? props.group.accounts.filter((account) => account.hasAssignedPolicy).length)
const lastProbeAt = computed(() => props.group.healthSummary?.lastProbeAt ?? null)
const isNewAPI = computed(() => props.group.platform.toLowerCase().includes('new'))

const strictDegradedCount = computed(() => Math.max(
  0,
  (props.group.healthSummary.degradedModels ?? 0)
    - (props.group.healthSummary.observingModels ?? 0)
    - (props.group.healthSummary.recoveringModels ?? 0),
))

const stateBreakdown = computed(() => [
  { key: 'healthy', count: props.group.healthSummary.healthyModels ?? 0, tone: 'text-emerald-600 dark:text-emerald-400' },
  { key: 'degraded', count: strictDegradedCount.value, tone: 'text-amber-600 dark:text-amber-400' },
  { key: 'suspended', count: props.group.healthSummary.suspendedModels ?? 0, tone: 'text-red-600 dark:text-red-400' },
  { key: 'observing', count: props.group.healthSummary.observingModels ?? 0, tone: 'text-blue-600 dark:text-blue-400' },
  { key: 'recovering', count: props.group.healthSummary.recoveringModels ?? 0, tone: 'text-cyan-600 dark:text-cyan-400' },
  { key: 'disabled', count: props.group.healthSummary.disabledModels ?? 0, tone: 'text-muted-foreground' },
  { key: 'notProbed', count: props.group.healthSummary.unconfiguredModels ?? 0, tone: 'text-muted-foreground' },
  { key: 'unprobeable', count: props.group.healthSummary.unprobeableAccounts ?? 0, tone: 'text-amber-600 dark:text-amber-400' },
])

const sortedAccounts = computed(() => {
  const accounts = [...(props.group.accounts ?? [])]
  if (!sortKey.value) return accounts
  const dir = sortDir.value === 'asc' ? 1 : -1
  const key = sortKey.value
  return accounts.sort((left, right) => {
    if (key === 'priority') {
      const leftVal = left.priority
      const rightVal = right.priority
      if (leftVal == null && rightVal == null) return 0
      if (leftVal == null) return 1
      if (rightVal == null) return -1
      if (leftVal === rightVal) return 0
      return leftVal < rightVal ? -dir : dir
    }
    const leftVal = left.upstreamKeyGroupMultiplier
    const rightVal = right.upstreamKeyGroupMultiplier
    if (leftVal == null && rightVal == null) return 0
    // 未知倍率沉底，便于先比较已关联的真实成本。
    if (leftVal == null) return 1
    if (rightVal == null) return -1
    if (leftVal === rightVal) return 0
    return leftVal < rightVal ? -dir : dir
  })
})

const readableMessage = (rawKey: string): string => t(connectionHealthMessageKey(rawKey, te))

const monitoringEnabled = (account: AdminGroupAccount): boolean =>
  account.hasEnabledProbePolicy ?? account.hasEnabledPolicy ?? account.assignedPolicies?.some((policy) => policy.enabled) ?? Boolean(account.hasAssignedPolicy)

const STATE_PRIORITY: ConnectionHealthState[] = ['suspended', 'disabled', 'degraded', 'observing', 'recovering', 'healthy']

/** 已从上游白名单摘除的模型只保留在展开详情里监控，不参与行级健康聚合与 ×N 统计。 */
const isSchedulableModelHealth = (model: { modelLimitExcluded?: boolean; modelLimitStatus?: string }): boolean =>
  !model.modelLimitExcluded && model.modelLimitStatus !== 'excluded'

const schedulableModelHealth = (account: AdminGroupAccount) =>
  (account.modelHealth ?? []).filter(isSchedulableModelHealth)

const aggregateState = (account: AdminGroupAccount): ConnectionHealthState | '' => {
  const present = new Set(schedulableModelHealth(account).map((model) => model.state))
  return STATE_PRIORITY.find((state) => present.has(state)) ?? ''
}

/** 行首徽标的 ×N 必须统计「处于该代表性状态」的可调度模型数，不能用模型总数（否则 1 个暂停会显示成 ×8）。 */
const aggregateStateCount = (account: AdminGroupAccount): number => {
  const state = aggregateState(account)
  if (!state) return 0
  return schedulableModelHealth(account).filter((model) => model.state === state).length
}

/** 与后端 priority 延迟分档一致：可调度模型平均延迟 <3s / <10s / <30s / ≥30s。 */
type LatencyTier = 'fast' | 'medium' | 'slow' | 'verySlow'

const averageSchedulableLatencyMs = (account: AdminGroupAccount): number | null => {
  const samples = schedulableModelHealth(account)
    .map((model) => model.lastLatencyMs)
    .filter((value): value is number => value != null && Number.isFinite(value) && value >= 0)
  if (samples.length === 0) return null
  return Math.round(samples.reduce((sum, value) => sum + value, 0) / samples.length)
}

const latencyTier = (account: AdminGroupAccount): LatencyTier | null => {
  // 降级已并入延迟分档：行上 healthy/degraded 都展示平均延迟档。
  const state = aggregateState(account)
  if (state !== 'healthy' && state !== 'degraded') return null
  const avg = averageSchedulableLatencyMs(account)
  if (avg == null) return 'verySlow'
  if (avg < 3000) return 'fast'
  if (avg < 10000) return 'medium'
  if (avg < 30000) return 'slow'
  return 'verySlow'
}

const unprobedModels = (account: AdminGroupAccount) => account.unprobedModels ?? []

const assignmentLabel = (account: AdminGroupAccount): string => {
  const policies = account.assignedPolicies ?? []
  if (policies.length === 0) return t(`${detailPrefix}.unmonitored`)
  if (policies.length === 1) return policies[0].policyName
  return t(`${detailPrefix}.policyCount`, { name: policies[0].policyName, count: policies.length - 1 })
}

const assignmentSourceLabel = (account: AdminGroupAccount): string =>
  t(`${detailPrefix}.assignmentSources.${account.policyAssignmentSource ?? 'none'}`)

const toggleModels = (targetId: string) => {
  expandedTargetId.value = expandedTargetId.value === targetId ? '' : targetId
}

const formatNumber = (value: number | null | undefined): string => value == null ? '-' : String(value)

/** 倍率最多保留 3 位小数，去掉浮点噪声（如 0.18000000000000002）和多余尾零。 */
const formatMultiplier = (value: number | null | undefined): string => {
  if (value == null || !Number.isFinite(value)) return '-'
  return `${Number(value.toFixed(3))}x`
}

const formatMultiplierDisplay = (display: string | null | undefined, fallback?: number | null): string => {
  if (fallback != null && Number.isFinite(fallback)) return formatMultiplier(fallback)
  if (!display) return '-'
  const match = display.trim().match(/^(-?\d+(?:\.\d+)?)x?$/i)
  if (!match) return display
  const parsed = Number(match[1])
  return Number.isFinite(parsed) ? formatMultiplier(parsed) : display
}
</script>

<template>
  <section class="min-w-0" :aria-label="group.name">
    <header class="flex flex-col gap-4 border-b border-border/50 px-5 py-5 lg:flex-row lg:items-start lg:justify-between">
      <div class="min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <h2 class="truncate text-xl font-semibold text-foreground">{{ group.name }}</h2>
          <span class="rounded-md bg-surface px-2 py-0.5 text-xs text-muted-foreground">{{ group.platform || '-' }}</span>
          <span
            class="rounded-md px-2 py-0.5 text-xs font-medium"
            :class="group.status === 'active' || group.status === '1'
              ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400'
              : 'bg-muted text-muted-foreground'"
          >
            {{ t(`${prefix}.groupStatusLabels.${group.status === 'active' || group.status === '1' ? 'active' : 'inactive'}`) }}
          </span>
          <span
            v-if="group.multiplierDisplay || group.multiplier != null"
            class="rounded-md bg-surface px-2 py-0.5 text-xs font-medium tabular-nums text-foreground"
            :title="t(`${detailPrefix}.columns.strategyMultiplier`)"
          >
            {{ t(`${detailPrefix}.columns.strategyMultiplier`) }} {{ formatMultiplierDisplay(group.multiplierDisplay, group.multiplier) }}
          </span>
          <span v-if="group.priorityMode === 'multiplier'" class="inline-flex items-center gap-1 rounded-md bg-primary/10 px-2 py-0.5 text-xs font-medium text-primary">
            <ArrowDownUp class="h-3 w-3" />
            {{ t(`${detailPrefix}.multiplierPriority`) }}
          </span>
        </div>
        <p class="mt-1 text-sm text-muted-foreground">
          {{ t(`${detailPrefix}.subtitle`, { monitored: monitoredCount, total: group.accountCount }) }}
        </p>
      </div>
      <button
        type="button"
        class="inline-flex h-9 shrink-0 items-center justify-center gap-2 rounded-lg bg-primary px-3 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        @click="emit('setup', group)"
      >
        <Settings2 class="h-4 w-4" />
        {{ t(`${detailPrefix}.${group.hasAssignedPolicy ? 'manageMonitoring' : 'enableMonitoring'}`) }}
      </button>
    </header>

    <dl class="grid border-b border-border/50 sm:grid-cols-2 xl:grid-cols-4">
      <div class="border-b border-border/50 px-5 py-4 sm:border-r xl:border-b-0">
        <dt class="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Radar class="h-3.5 w-3.5" />{{ t(`${detailPrefix}.metrics.accounts`) }}</dt>
        <dd class="mt-1 text-xl font-semibold tabular-nums text-foreground">{{ group.accountCount }}</dd>
      </div>
      <div class="border-b border-border/50 px-5 py-4 xl:border-b-0 xl:border-r">
        <dt class="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><ShieldCheck class="h-3.5 w-3.5" />{{ t(`${detailPrefix}.metrics.monitored`) }}</dt>
        <dd class="mt-1 text-xl font-semibold tabular-nums text-foreground">{{ monitoredCount }}</dd>
      </div>
      <div class="border-b border-border/50 px-5 py-4 sm:border-b-0 sm:border-r">
        <dt class="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Gauge class="h-3.5 w-3.5" />{{ t(`${detailPrefix}.metrics.probeable`) }}</dt>
        <dd class="mt-1 text-xl font-semibold tabular-nums text-foreground">{{ group.healthSummary.probeableAccounts }}</dd>
      </div>
      <div class="px-5 py-4">
        <dt class="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Clock3 class="h-3.5 w-3.5" />{{ t(`${detailPrefix}.metrics.lastProbe`) }}</dt>
        <dd class="mt-1 text-sm font-medium text-foreground">{{ formatConnectionHealthTime(lastProbeAt) }}</dd>
      </div>
    </dl>

    <div v-if="(group.priorityConflictCount ?? 0) > 0" class="flex items-start gap-2 border-b border-amber-500/25 bg-amber-500/[0.07] px-5 py-3 text-sm text-amber-700 dark:text-amber-400">
      <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
      <span>{{ t(`${detailPrefix}.priorityConflict`, { count: group.priorityConflictCount }) }}</span>
    </div>

    <section class="border-b border-border/50 bg-surface/20 px-5 py-4" :aria-label="t(`${detailPrefix}.statusBreakdown.title`)">
      <div class="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between sm:gap-4">
        <h3 class="text-xs font-semibold text-foreground">{{ t(`${detailPrefix}.statusBreakdown.title`) }}</h3>
        <p class="text-xs leading-5 text-muted-foreground">{{ t(`${detailPrefix}.statusBreakdown.hint`) }}</p>
      </div>
      <dl class="mt-3 grid grid-cols-2 divide-x divide-y divide-border/50 overflow-hidden rounded-lg border border-border/50 bg-background sm:grid-cols-4 xl:grid-cols-8 xl:divide-y-0">
        <div v-for="item in stateBreakdown" :key="item.key" class="min-w-0 px-3 py-2.5">
          <dt class="truncate text-[11px] font-medium" :class="item.tone">{{ t(`${detailPrefix}.statusBreakdown.${item.key}`) }}</dt>
          <dd class="mt-0.5 text-lg font-semibold tabular-nums text-foreground">{{ item.count }}</dd>
        </div>
      </dl>
    </section>

    <div class="px-5 py-5">
      <div v-if="group.accountsError" class="rounded-lg bg-destructive/10 px-4 py-3 text-sm text-destructive">
        {{ readableMessage(group.accountsError) }}
      </div>
      <div v-else-if="group.accounts.length === 0" class="flex min-h-64 flex-col items-center justify-center text-center">
        <Activity class="h-8 w-8 text-muted-foreground/40" />
        <p class="mt-3 text-sm text-muted-foreground">{{ t(`${detailPrefix}.empty`) }}</p>
      </div>
      <div v-else class="overflow-x-auto rounded-lg border border-border/60">
        <table class="w-full min-w-[58rem] text-sm">
          <thead class="bg-surface/60 text-left text-xs text-muted-foreground">
            <tr>
              <th class="w-10 px-3 py-2.5 font-medium"><span class="sr-only">{{ t(`${detailPrefix}.columns.expand`) }}</span></th>
              <th class="px-3 py-2.5 font-medium">{{ t(`${detailPrefix}.columns.account`) }}</th>
              <th class="px-3 py-2.5 font-medium">{{ t(`${detailPrefix}.columns.health`) }}</th>
              <th class="px-3 py-2.5 font-medium">{{ t(`${detailPrefix}.columns.strategy`) }}</th>
              <th class="px-3 py-2.5 font-medium">
                <button
                  type="button"
                  class="inline-flex items-center gap-1 rounded-md px-1 py-0.5 transition-colors hover:bg-surface hover:text-foreground"
                  :class="sortKey === 'priority' ? 'text-foreground' : ''"
                  @click="toggleSort('priority')"
                >
                  <span>{{ t(`${detailPrefix}.columns.priority`) }}</span>
                  <ArrowUp v-if="sortIndicator('priority') === 'asc'" class="h-3.5 w-3.5" />
                  <ArrowDown v-else-if="sortIndicator('priority') === 'desc'" class="h-3.5 w-3.5" />
                  <ArrowUpDown v-else class="h-3.5 w-3.5 opacity-50" />
                </button>
              </th>
              <th class="px-3 py-2.5 font-medium">
                <button
                  type="button"
                  class="inline-flex items-center gap-1 rounded-md px-1 py-0.5 transition-colors hover:bg-surface hover:text-foreground"
                  :class="sortKey === 'upstreamMultiplier' ? 'text-foreground' : ''"
                  @click="toggleSort('upstreamMultiplier')"
                >
                  <span>{{ t(`${detailPrefix}.columns.upstreamMultiplier`) }}</span>
                  <ArrowUp v-if="sortIndicator('upstreamMultiplier') === 'asc'" class="h-3.5 w-3.5" />
                  <ArrowDown v-else-if="sortIndicator('upstreamMultiplier') === 'desc'" class="h-3.5 w-3.5" />
                  <ArrowUpDown v-else class="h-3.5 w-3.5 opacity-50" />
                </button>
              </th>
              <th class="px-3 py-2.5 text-right font-medium">{{ t(`${detailPrefix}.columns.actions`) }}</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="account in sortedAccounts" :key="account.targetId">
              <tr class="border-t border-border/40 transition-colors hover:bg-surface/35">
                <td class="px-3 py-3">
                  <button
                    type="button"
                    class="rounded p-1 text-muted-foreground hover:bg-surface hover:text-foreground"
                    :aria-label="t(`${detailPrefix}.columns.expand`)"
                    @click="toggleModels(account.targetId)"
                  >
                    <ChevronDown v-if="expandedTargetId === account.targetId" class="h-4 w-4" />
                    <ChevronRight v-else class="h-4 w-4" />
                  </button>
                </td>
                <td class="px-3 py-3">
                  <div class="max-w-56">
                    <p class="truncate font-medium text-foreground">{{ account.name || account.id }}</p>
                    <p class="mt-0.5 truncate text-xs text-muted-foreground">
                      {{ account.platform || account.type || '-' }} · {{ t(`${detailPrefix}.upstreamStatus`, { status: account.status || t(`${detailPrefix}.unknownUpstreamStatus`) }) }}
                    </p>
                  </div>
                </td>
                <td class="px-3 py-3">
                  <div class="flex flex-col items-start gap-1">
                    <span v-if="!account.probeAvailable" class="inline-flex items-center gap-1 rounded-md bg-amber-500/10 px-2 py-1 text-xs font-medium text-amber-600 dark:text-amber-400">
                      <AlertTriangle class="h-3 w-3" />{{ t(`${detailPrefix}.unprobeable`) }}
                    </span>
                    <span v-else-if="!aggregateState(account)" class="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground">
                      <ShieldQuestion class="h-3 w-3" />{{ t(`${prefix}.notProbed`) }}
                    </span>
                    <span v-else class="inline-flex items-center rounded-md px-2 py-1 text-xs font-medium" :class="connectionHealthStateBadgeClass(aggregateState(account))">
                      {{ t(`${prefix}.stateLabels.${aggregateState(account)}`) }}
                      <span v-if="aggregateStateCount(account) > 1" class="ml-1 opacity-70">×{{ aggregateStateCount(account) }}</span>
                    </span>
                    <span
                      v-if="latencyTier(account)"
                      class="text-[11px] text-muted-foreground"
                      :title="t(`${detailPrefix}.latencyTier.hint`)"
                    >
                      {{ t(`${detailPrefix}.latencyTier.${latencyTier(account)}`) }}
                      <template v-if="averageSchedulableLatencyMs(account) != null">
                        · {{ averageSchedulableLatencyMs(account) }} ms
                      </template>
                    </span>
                    <span v-if="unprobedModels(account).length > 0 && aggregateState(account)" class="text-[11px] text-muted-foreground">
                      {{ t(`${prefix}.notProbed`) }} ×{{ unprobedModels(account).length }}
                    </span>
                  </div>
                </td>
                <td class="px-3 py-3">
                  <Tooltip :text="assignmentLabel(account)" wide>
                    <div class="max-w-52">
                      <p class="truncate text-xs font-medium" :class="monitoringEnabled(account) ? 'text-foreground' : 'text-muted-foreground'">{{ assignmentLabel(account) }}</p>
                      <p class="mt-0.5 text-[11px] text-muted-foreground">{{ assignmentSourceLabel(account) }}</p>
                    </div>
                  </Tooltip>
                </td>
                <td class="px-3 py-3 tabular-nums text-foreground">
                  <div class="flex items-center gap-1.5">
                    <span>{{ formatNumber(account.priority) }}</span>
                    <span v-if="account.priorityConflict" class="h-2 w-2 rounded-full bg-amber-500" :title="t(`${detailPrefix}.priorityConflictShort`)" />
                    <ArrowDownUp v-else-if="account.priorityManaged" class="h-3.5 w-3.5 text-primary" />
                  </div>
                </td>
                <td class="px-3 py-3 tabular-nums text-foreground">
                  <span v-if="account.upstreamKeyGroupMultiplier == null" class="text-xs text-muted-foreground">
                    {{ t(`${detailPrefix}.upstreamMultiplierPending`) }}
                  </span>
                  <span v-else>{{ formatMultiplier(account.upstreamKeyGroupMultiplier) }}</span>
                  <span v-if="account.upstreamKeyGroupName" class="mt-0.5 block max-w-32 truncate text-[11px] text-muted-foreground">
                    {{ account.upstreamKeyGroupName }}
                  </span>
                </td>
                <td class="px-3 py-3">
                  <div class="flex items-center justify-end gap-1">
                    <Tooltip :text="t(`${prefix}.actions.probe`)">
                      <button
                        type="button"
                        class="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-surface hover:text-primary disabled:opacity-35"
                        :aria-label="t(`${prefix}.actions.probe`)"
                        :disabled="!account.probeAvailable"
                        @click="emit('probe', account)"
                      >
                        <Zap class="h-4 w-4" />
                      </button>
                    </Tooltip>
                    <Tooltip :text="t(`${prefix}.actions.viewEvents`)">
                      <button
                        type="button"
                        class="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-surface hover:text-foreground disabled:opacity-35"
                        :aria-label="t(`${prefix}.actions.viewEvents`)"
                        :disabled="!account.hasAssignedPolicy"
                        @click="emit('view-events', account)"
                      >
                        <Eye class="h-4 w-4" />
                      </button>
                    </Tooltip>
                  </div>
                </td>
              </tr>
              <tr v-if="expandedTargetId === account.targetId" class="border-t border-border/40 bg-surface/25">
                <td colspan="7" class="px-12 py-4">
                  <div v-if="account.modelHealth.length === 0 && unprobedModels(account).length === 0" class="text-xs text-muted-foreground">{{ t(`${detailPrefix}.models.empty`) }}</div>
                  <div v-else class="grid gap-2 lg:grid-cols-2">
                    <div v-for="model in account.modelHealth" :key="model.modelName" class="rounded-lg border border-border/50 bg-background px-3 py-2.5">
                      <div class="flex items-center justify-between gap-3">
                        <span class="truncate text-sm font-medium text-foreground">{{ model.modelName }}</span>
                        <div class="flex shrink-0 flex-wrap items-center justify-end gap-1">
                          <span
                            v-if="model.modelLimitStatus === 'excluded' || model.modelLimitExcluded"
                            class="rounded-md bg-violet-500/15 px-2 py-0.5 text-xs font-medium text-violet-700 dark:text-violet-300"
                            :title="t(`${detailPrefix}.models.limitExcludedHint`)"
                          >{{ t(`${detailPrefix}.models.limitExcluded`) }}</span>
                          <span
                            v-else-if="model.modelLimitStatus === 'pending'"
                            class="rounded-md bg-amber-500/15 px-2 py-0.5 text-xs font-medium text-amber-700 dark:text-amber-300"
                            :title="t(`${detailPrefix}.models.limitPendingHint`)"
                          >{{ t(`${detailPrefix}.models.limitPending`) }}</span>
                          <span
                            v-else-if="model.modelLimitStatus === 'failed'"
                            class="rounded-md bg-destructive/15 px-2 py-0.5 text-xs font-medium text-destructive"
                            :title="t(`${detailPrefix}.models.limitFailedHint`)"
                          >{{ t(`${detailPrefix}.models.limitFailed`) }}</span>
                          <span class="rounded-md px-2 py-0.5 text-xs font-medium" :class="connectionHealthStateBadgeClass(model.state)">{{ t(`${prefix}.stateLabels.${model.state}`) }}</span>
                        </div>
                      </div>
                      <div class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                        <span>{{ t(`${detailPrefix}.models.latency`, { value: model.lastLatencyMs ?? '-' }) }}</span>
                        <span>{{ t(`${detailPrefix}.models.lastProbe`, { value: formatConnectionHealthTime(model.lastProbeAt) }) }}</span>
                        <span>{{ t(`${detailPrefix}.models.weight`, { value: model.currentWeight }) }}</span>
                      </div>
                      <p v-if="model.lastErrorKey" class="mt-2 truncate text-xs text-destructive">{{ readableMessage(model.lastErrorKey) }}</p>
                    </div>
                    <div v-for="model in unprobedModels(account)" :key="`unprobed:${model.modelName}`" class="rounded-lg border border-border/50 bg-background px-3 py-2.5">
                      <div class="flex items-center justify-between gap-3">
                        <span class="truncate text-sm font-medium text-foreground">{{ model.modelName }}</span>
                        <span class="rounded-md bg-muted px-2 py-0.5 text-xs text-muted-foreground">{{ t(`${prefix}.notProbed`) }}</span>
                      </div>
                      <div class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                        <span>{{ t(`${detailPrefix}.models.latency`, { value: '-' }) }}</span>
                        <span>{{ t(`${detailPrefix}.models.lastProbe`, { value: formatConnectionHealthTime(null) }) }}</span>
                        <span>{{ t(`${detailPrefix}.models.weight`, { value: '-' }) }}</span>
                      </div>
                    </div>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </div>
  </section>
</template>
