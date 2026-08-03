<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, BarChart3, Loader2, X } from 'lucide-vue-next'
import {
  getSiteUserUsage,
  type SiteUserItem,
  type SiteUserUsagePeriod,
} from '../../api/siteUsers'

const props = defineProps<{
  open: boolean
  user: SiteUserItem | null
  siteRechargeRate?: number | null
}>()

const emit = defineEmits<{
  (event: 'close'): void
}>()

const { t } = useI18n()

const loading = ref(false)
const errorKey = ref('')
const messageKey = ref('')
const rate = ref(1)
const detailUser = ref<SiteUserItem | null>(null)
const today = ref<SiteUserUsagePeriod | null>(null)
const history = ref<SiteUserUsagePeriod | null>(null)

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
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

const toCny = (platform: number | null | undefined) => {
  if (platform == null || Number.isNaN(platform)) return null
  return platform * (rate.value > 0 ? rate.value : 1)
}

const userDisplay = (u: SiteUserItem | null | undefined) => {
  if (!u) return ''
  return u.email || u.username || u.id
}

let usageReqSeq = 0

const load = async () => {
  if (!props.user) return
  const uid = props.user.id
  const seq = ++usageReqSeq
  loading.value = true
  errorKey.value = ''
  messageKey.value = ''
  try {
    const resp = await getSiteUserUsage(uid)
    if (seq !== usageReqSeq || props.user?.id !== uid) return
    today.value = resp.today
    history.value = resp.history
    if (resp.siteRechargeRate != null && resp.siteRechargeRate > 0) {
      rate.value = resp.siteRechargeRate
    } else if (props.siteRechargeRate != null && props.siteRechargeRate > 0) {
      rate.value = props.siteRechargeRate
    }
    if (resp.user) detailUser.value = resp.user
    else detailUser.value = props.user
    if (resp.messageKey) messageKey.value = resp.messageKey
  } catch (err) {
    if (seq !== usageReqSeq || props.user?.id !== uid) return
    errorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    if (seq === usageReqSeq) loading.value = false
  }
}

watch(
  () => [props.open, props.user?.id] as const,
  ([open]) => {
    if (open && props.user) {
      detailUser.value = props.user
      today.value = null
      history.value = null
      if (props.siteRechargeRate != null && props.siteRechargeRate > 0) {
        rate.value = props.siteRechargeRate
      }
      void load()
    }
  },
)

const formatPercent = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return `${n.toFixed(1)}%`
}
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open && user" class="fixed inset-0 z-[110] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/70 backdrop-blur-[2px]" @click="emit('close')" />
      <div class="relative flex max-h-[88vh] w-full max-w-lg flex-col overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl">
        <div class="flex items-start justify-between gap-3 border-b border-border/40 px-5 py-4">
          <div class="flex min-w-0 items-center gap-3">
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <BarChart3 class="h-5 w-5" />
            </div>
            <div class="min-w-0">
              <h2 class="truncate text-base font-semibold">{{ t('admin.siteUsers.usage.title') }}</h2>
              <p class="mt-0.5 truncate text-xs text-muted-foreground">
                {{ userDisplay(detailUser || user) }} · ID {{ user.id }}
              </p>
            </div>
          </div>
          <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="min-h-0 flex-1 space-y-4 overflow-y-auto px-5 py-4">
          <p class="text-[11px] leading-4 text-muted-foreground">{{ t('admin.siteUsers.usage.help') }}</p>

          <p v-if="messageKey" class="text-xs text-muted-foreground">{{ t(messageKey) }}</p>
          <p v-if="errorKey" class="flex items-center gap-1.5 text-xs text-destructive">
            <AlertCircle class="h-3.5 w-3.5 shrink-0" />{{ t(errorKey) }}
          </p>

          <div v-if="loading" class="flex justify-center py-12 text-muted-foreground">
            <Loader2 class="h-6 w-6 animate-spin" />
          </div>

          <template v-else>
            <section
              v-for="period in [
                { key: 'today', data: today, title: t('admin.siteUsers.usage.today') },
                { key: 'history', data: history, title: t('admin.siteUsers.usage.history') },
              ]"
              :key="period.key"
              class="space-y-2"
            >
              <div class="flex items-end justify-between gap-2">
                <div>
                  <h3 class="text-sm font-semibold text-foreground">{{ period.title }}</h3>
                  <p v-if="period.data" class="text-[11px] text-muted-foreground">
                    {{ period.data.startDate }}
                    <template v-if="period.data.endDate !== period.data.startDate">
                      → {{ period.data.endDate }}
                    </template>
                  </p>
                </div>
                <div class="text-right">
                  <div class="text-[11px] text-muted-foreground">{{ t('admin.siteUsers.usage.total') }}</div>
                  <div class="font-mono text-sm font-semibold tabular-nums">
                    ¥{{ formatMoney(toCny(period.data?.totalActualCost ?? 0)) }}
                  </div>
                  <div class="text-[10px] tabular-nums text-muted-foreground">
                    ${{ formatMoney(period.data?.totalActualCost ?? 0) }}
                  </div>
                </div>
              </div>

              <div
                v-if="!period.data?.groups?.length"
                class="rounded-xl border border-dashed border-border/50 px-3 py-6 text-center text-xs text-muted-foreground"
              >
                {{ t('admin.siteUsers.usage.empty') }}
              </div>
              <ul v-else class="divide-y divide-border/30 overflow-hidden rounded-xl border border-border/50">
                <li
                  v-for="g in period.data.groups"
                  :key="`${period.key}-${g.groupId || g.groupName}`"
                  class="px-3 py-2.5"
                >
                  <div class="flex items-start justify-between gap-3">
                    <div class="min-w-0">
                      <div class="truncate text-sm font-medium text-foreground">{{ g.groupName }}</div>
                      <div class="mt-0.5 text-[11px] text-muted-foreground">
                        <template v-if="g.requests">{{ t('admin.siteUsers.usage.requests', { n: g.requests }) }}</template>
                        <template v-if="g.requests && g.totalTokens"> · </template>
                        <template v-if="g.totalTokens">{{ t('admin.siteUsers.usage.tokens', { n: formatTokens(g.totalTokens) }) }}</template>
                      </div>
                    </div>
                    <div class="shrink-0 text-right">
                      <div class="font-mono text-sm font-semibold tabular-nums">
                        ¥{{ formatMoney(toCny(g.actualCost)) }}
                      </div>
                      <div class="text-[10px] tabular-nums text-muted-foreground">
                        ${{ formatMoney(g.actualCost) }}
                      </div>
                      <div class="mt-0.5 text-xs font-semibold tabular-nums text-primary">
                        {{ formatPercent(g.percent) }}
                      </div>
                    </div>
                  </div>
                  <div class="mt-2 h-1.5 overflow-hidden rounded-full bg-muted">
                    <div
                      class="h-full rounded-full bg-primary/80 transition-all"
                      :style="{ width: `${Math.min(100, Math.max(0, g.percent || 0))}%` }"
                    />
                  </div>
                </li>
              </ul>
            </section>
          </template>
        </div>
      </div>
    </div>
  </Teleport>
</template>
