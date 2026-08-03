<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, ChevronLeft, ChevronRight, Loader2, Receipt, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  listSiteUserTopups,
  markSiteUserTopup,
  type SiteUserItem,
  type SiteUserTopupCandidate,
} from '../../api/siteUsers'

const props = defineProps<{
  open: boolean
  user: SiteUserItem | null
  /** 可选：父级已有倍率时传入，避免二次请求 */
  siteRechargeRate?: number | null
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'updated'): void
}>()

const { t } = useI18n()

const topups = ref<SiteUserTopupCandidate[]>([])
const topupPage = ref(1)
const topupPageSize = ref(20)
const topupTotal = ref(0)
const topupLoading = ref(false)
const topupRefreshing = ref(false)
const topupErrorKey = ref('')
const topupMessageKey = ref('')
/** 正在提交标记的流水 id 集合（按行锁定，不阻塞其它行） */
const markingIds = ref<Record<string, true>>({})
const platformLifetime = ref<number | null>(null)
let reloadTimer: ReturnType<typeof setTimeout> | null = null
const detailListSum = ref(0)
const markedRecharge = ref(0)
const markedGift = ref(0)
const markedRebate = ref(0)
const detailUser = ref<SiteUserItem | null>(null)
const rate = ref(1)

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
}

const toCny = (platform: number | null | undefined) => {
  if (platform == null || Number.isNaN(platform)) return null
  return platform * (rate.value > 0 ? rate.value : 1)
}

/** 主金额 CNY + 副金额 USD */
const moneyPair = (platform: number | null | undefined) => {
  const cny = toCny(platform)
  return {
    cny: formatMoney(cny),
    usd: formatMoney(platform),
  }
}

const topupTotalPages = computed(() => Math.max(1, Math.ceil(topupTotal.value / topupPageSize.value) || 1))

const userDisplay = computed(() => {
  const u = detailUser.value || props.user
  if (!u) return ''
  return u.email || u.username || u.id
})

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

const loadTopups = async (opts?: { silent?: boolean }) => {
  if (!props.user) return
  const silent = !!opts?.silent && topups.value.length > 0
  if (silent) topupRefreshing.value = true
  else topupLoading.value = true
  topupErrorKey.value = ''
  topupMessageKey.value = ''
  try {
    const resp = await listSiteUserTopups(props.user.id, {
      page: topupPage.value,
      pageSize: topupPageSize.value,
    })
    topups.value = resp.items || []
    topupTotal.value = resp.total ?? 0
    topupPage.value = resp.page || topupPage.value
    platformLifetime.value = resp.platformLifetime ?? null
    detailListSum.value = resp.detailListSum ?? 0
    markedRecharge.value = resp.markedRecharge ?? 0
    markedGift.value = resp.markedGift ?? 0
    markedRebate.value = resp.markedRebate ?? 0
    if (resp.siteRechargeRate != null && resp.siteRechargeRate > 0) {
      rate.value = resp.siteRechargeRate
    } else if (props.siteRechargeRate != null && props.siteRechargeRate > 0) {
      rate.value = props.siteRechargeRate
    }
    // 自动打标后以 DB 汇总为准回填头部用户
    if (resp.user) {
      detailUser.value = {
        ...props.user,
        ...resp.user,
        markedRecharge: resp.markedRecharge ?? resp.user.markedRecharge ?? 0,
        markedGift: resp.markedGift ?? resp.user.markedGift ?? 0,
        markedRebate: resp.markedRebate ?? resp.user.markedRebate ?? 0,
      }
    } else {
      detailUser.value = props.user
        ? {
            ...props.user,
            markedRecharge: resp.markedRecharge ?? 0,
            markedGift: resp.markedGift ?? 0,
            markedRebate: resp.markedRebate ?? 0,
          }
        : null
    }
    if (resp.messageKey) topupMessageKey.value = resp.messageKey
  } catch (err) {
    topupErrorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
  } finally {
    topupLoading.value = false
    topupRefreshing.value = false
  }
}

watch(
  () => [props.open, props.user?.id] as const,
  ([open]) => {
    if (open && props.user) {
      topupPage.value = 1
      detailUser.value = props.user
      topups.value = []
      if (props.siteRechargeRate != null && props.siteRechargeRate > 0) {
        rate.value = props.siteRechargeRate
      }
      void loadTopups()
    }
  },
)

const goTopupPage = (next: number) => {
  if (next < 1 || next > topupTotalPages.value || next === topupPage.value) return
  topupPage.value = next
  void loadTopups()
}

const tagLabel = (tag: string, amount?: number) => {
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

const recomputeLocalSums = () => {
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

const isMarking = (platformId: string) => !!markingIds.value[platformId]

const setMarking = (platformId: string, on: boolean) => {
  const next = { ...markingIds.value }
  if (on) next[platformId] = true
  else delete next[platformId]
  markingIds.value = next
}

/** 全部 in-flight 结束后再静默刷新一次，避免每点一条都卡全量 history */
const scheduleSilentReload = () => {
  if (reloadTimer) clearTimeout(reloadTimer)
  reloadTimer = setTimeout(() => {
    reloadTimer = null
    if (Object.keys(markingIds.value).length > 0) {
      scheduleSilentReload()
      return
    }
    void loadTopups({ silent: true }).then(() => emit('updated'))
  }, 280)
}

const applyTag = async (item: SiteUserTopupCandidate, tag: 'recharge' | 'gift' | 'rebate') => {
  if (!props.user) return
  if (item.tag === tag) return
  // 只锁当前行，其它行可继续点
  if (isMarking(item.platformId)) return
  topupErrorKey.value = ''
  const prevTag = item.tag
  item.tag = tag
  recomputeLocalSums()
  setMarking(item.platformId, true)
  try {
    await markSiteUserTopup(props.user.id, {
      platformRecordId: item.platformId,
      tag,
      amountPlatform: item.amountPlatform,
      note: item.note,
      createdAt: item.createdAt || undefined,
    })
    scheduleSilentReload()
  } catch (err) {
    item.tag = prevTag
    recomputeLocalSums()
    topupErrorKey.value = err instanceof Error ? err.message : 'admin.siteUsers.errors.request'
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
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open && user" class="fixed inset-0 z-[110] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/70 backdrop-blur-[2px]" @click="emit('close')" />
      <div class="relative flex max-h-[88vh] w-full max-w-xl flex-col overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl">
        <div class="flex items-start justify-between gap-3 border-b border-border/40 px-5 py-4">
          <div class="flex min-w-0 items-center gap-3">
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <Receipt class="h-5 w-5" />
            </div>
            <div class="min-w-0">
              <h2 class="truncate text-base font-semibold">{{ userDisplay }}</h2>
              <p class="mt-0.5 text-xs text-muted-foreground">
                ID {{ user.id }}
                <template v-if="detailUser?.balance != null">
                  · {{ t('admin.siteUsers.balance') }}
                  ¥{{ moneyPair(detailUser.balance).cny }}
                  <span class="text-muted-foreground/80"> / ${{ moneyPair(detailUser.balance).usd }}</span>
                </template>
                <span v-if="rate > 0 && rate !== 1" class="ml-1 text-muted-foreground/70">×{{ rate }}</span>
                <span v-if="detailUser?.notes || user.notes" class="mt-0.5 block truncate text-foreground/80" :title="detailUser?.notes || user.notes">
                  {{ t('admin.siteUsers.notes') }}：{{ detailUser?.notes || user.notes }}
                </span>
                <span v-if="topupRefreshing" class="ml-2 inline-flex items-center gap-1 text-muted-foreground">
                  <Loader2 class="h-3 w-3 animate-spin" />
                </span>
              </p>
            </div>
          </div>
          <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="min-h-0 flex-1 space-y-3 overflow-y-auto px-5 py-4">
          <p class="text-[11px] leading-4 text-muted-foreground">{{ t('admin.siteUsers.topupHelp') }}</p>

          <div class="grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
              <div class="text-muted-foreground">{{ t('admin.siteUsers.platformLifetime') }}</div>
              <div class="mt-0.5 font-semibold tabular-nums">¥{{ moneyPair(platformLifetime).cny }}</div>
              <div class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">${{ moneyPair(platformLifetime).usd }}</div>
            </div>
            <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
              <div class="text-muted-foreground">{{ t('admin.siteUsers.detailSum') }}</div>
              <div class="mt-0.5 font-semibold tabular-nums">¥{{ moneyPair(detailListSum).cny }}</div>
              <div class="mt-0.5 text-[10px] tabular-nums text-muted-foreground">${{ moneyPair(detailListSum).usd }}</div>
            </div>
            <div class="rounded-lg border border-sky-500/25 bg-sky-500/5 px-2.5 py-2">
              <div class="text-sky-700 dark:text-sky-300">{{ t('admin.siteUsers.tags.recharge') }}</div>
              <div class="mt-0.5 font-semibold tabular-nums text-sky-700 dark:text-sky-300">¥{{ moneyPair(markedRecharge).cny }}</div>
              <div class="mt-0.5 text-[10px] tabular-nums text-sky-700/70 dark:text-sky-300/70">${{ moneyPair(markedRecharge).usd }}</div>
            </div>
            <div class="rounded-lg border border-border/40 bg-surface/40 px-2.5 py-2">
              <div class="text-muted-foreground">{{ t('admin.siteUsers.giftRebate') }}</div>
              <div class="mt-0.5 font-semibold tabular-nums">¥{{ moneyPair(markedGift + markedRebate).cny }}</div>
              <div class="mt-0.5 text-[10px] leading-4">
                <span class="text-emerald-700 dark:text-emerald-300">{{ t('admin.siteUsers.tags.gift') }} ¥{{ moneyPair(markedGift).cny }}</span>
                <span class="text-muted-foreground"> · </span>
                <span class="text-amber-700 dark:text-amber-300">{{ t('admin.siteUsers.tags.rebate') }} ¥{{ moneyPair(markedRebate).cny }}</span>
              </div>
            </div>
          </div>

          <p v-if="topupMessageKey" class="text-xs text-muted-foreground">{{ t(topupMessageKey) }}</p>
          <p v-if="topupErrorKey" class="flex items-center gap-1.5 text-xs text-destructive">
            <AlertCircle class="h-3.5 w-3.5 shrink-0" />{{ t(topupErrorKey) }}
          </p>

          <div v-if="topupLoading && !topups.length" class="flex justify-center py-10 text-muted-foreground">
            <Loader2 class="h-5 w-5 animate-spin" />
          </div>
          <div v-else-if="!topups.length" class="py-10 text-center text-sm text-muted-foreground">
            {{ t('admin.siteUsers.topupEmpty') }}
          </div>
          <ul v-else class="divide-y divide-border/30 rounded-xl border border-border/50" :class="topupRefreshing ? 'opacity-90' : ''">
            <li v-for="item in topups" :key="item.platformId" class="px-3 py-2.5 text-sm">
              <div class="flex flex-wrap items-start justify-between gap-2">
                <div class="min-w-0">
                  <div
                    class="font-semibold tabular-nums"
                    :class="item.amountPlatform < 0 ? 'text-destructive' : ''"
                  >
                    {{ item.amountPlatform < 0 ? '' : '+' }}¥{{ moneyPair(item.amountPlatform).cny }}
                    <span
                      class="ml-1.5 text-xs font-normal"
                      :class="item.amountPlatform < 0 ? 'text-destructive/80' : 'text-muted-foreground'"
                    >${{ moneyPair(item.amountPlatform).usd }}</span>
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
            <Button variant="ghost" size="sm" :disabled="topupPage <= 1 || topupLoading || topupRefreshing" @click="goTopupPage(topupPage - 1)">
              <ChevronLeft class="h-4 w-4" />
            </Button>
            <span class="text-xs text-muted-foreground">{{ topupPage }} / {{ topupTotalPages }}</span>
            <Button variant="ghost" size="sm" :disabled="topupPage >= topupTotalPages || topupLoading || topupRefreshing" @click="goTopupPage(topupPage + 1)">
              <ChevronRight class="h-4 w-4" />
            </Button>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
