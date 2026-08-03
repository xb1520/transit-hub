<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronLeft, ChevronRight, Loader2, PackagePlus, RefreshCw, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  listSiteRechargeCandidates,
  markSiteRecharge,
  type RechargeCandidate,
} from '../../api/upstream'
import type { UpstreamSite } from '../../types/upstream'

const props = defineProps<{
  open: boolean
  site: UpstreamSite | null
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'updated'): void
}>()

const { t } = useI18n()
const loading = ref(false)
const markingId = ref('')
const errorKey = ref('')
const messageKey = ref('')
const available = ref(false)
const items = ref<RechargeCandidate[]>([])
const rate = ref(0)
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const platform = ref('')
const platformLifetimeCost = ref<number | null>(null)
const detailListCost = ref(0)
const markedRechargeCost = ref(0)
const markedGiftCost = ref(0)
const markedRebateCost = ref(0)
const gapLifetimeVsMarked = ref<number | null>(null)

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
}

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value) || 1))

const unmarkedOnPage = computed(() => items.value.filter((r) => !r.tag).length)

const load = async () => {
  if (!props.site) return
  loading.value = true
  errorKey.value = ''
  messageKey.value = ''
  try {
    const resp = await listSiteRechargeCandidates(props.site.id, {
      page: page.value,
      pageSize: pageSize.value,
    })
    items.value = resp.items || []
    available.value = !!resp.available
    rate.value = resp.rechargeRate || props.site.rechargeRate || 0
    total.value = resp.total ?? 0
    page.value = resp.page || page.value
    pageSize.value = resp.pageSize || pageSize.value
    platform.value = resp.platform || props.site.platform || ''
    platformLifetimeCost.value = resp.platformLifetimeCost ?? null
    detailListCost.value = resp.detailListCost ?? 0
    markedRechargeCost.value = resp.markedRechargeCost ?? 0
    markedGiftCost.value = resp.markedGiftCost ?? 0
    markedRebateCost.value = resp.markedRebateCost ?? 0
    gapLifetimeVsMarked.value = resp.gapLifetimeVsMarked ?? null
    if (resp.messageKey) messageKey.value = resp.messageKey
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (open) => {
  if (open) {
    page.value = 1
    void load()
  }
})

const goPage = (next: number) => {
  if (next < 1 || next > totalPages.value || next === page.value) return
  page.value = next
  void load()
}

const tagLabel = (tag: string) => {
  const key = `admin.upstream.ledger.tags.${tag}`
  const translated = t(key)
  return translated === key ? tag : translated
}

const applyTag = async (item: RechargeCandidate, tag: 'recharge' | 'gift' | 'rebate') => {
  if (!props.site || markingId.value) return
  markingId.value = item.platformId
  errorKey.value = ''
  try {
    await markSiteRecharge(props.site.id, {
      platformRecordId: item.platformId,
      tag,
      amountPlatform: item.amountPlatform,
      note: item.note,
      createdAt: item.createdAt || undefined,
    })
    await load()
    emit('updated')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    markingId.value = ''
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
    <div v-if="open && site" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')" />
      <div class="relative w-full max-w-xl max-h-[90vh] overflow-y-auto rounded-2xl border border-border/60 bg-card shadow-2xl">
        <div class="flex items-center justify-between border-b border-border/40 px-6 py-4">
          <div class="flex items-center gap-3">
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <PackagePlus class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-base font-semibold">{{ t('admin.upstream.ledger.title') }}</h2>
              <p class="text-xs text-muted-foreground">
                {{ site.name }} · {{ t('admin.upstream.ledger.costUnit') }}
                <span v-if="rate > 0"> · ×{{ rate }}</span>
                <span v-if="platform"> · {{ platform }}</span>
              </p>
            </div>
          </div>
          <div class="flex items-center gap-1">
            <button
              type="button"
              class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated"
              :disabled="loading"
              :title="t('admin.upstream.action.sync')"
              @click="load"
            >
              <RefreshCw class="h-4 w-4" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
              <X class="h-5 w-5" />
            </button>
          </div>
        </div>

        <div class="space-y-5 px-6 py-5">
          <div v-if="loading" class="flex justify-center py-8 text-muted-foreground">
            <Loader2 class="h-6 w-6 animate-spin" />
          </div>
          <template v-else>
            <!-- 口径对照：历史充值 vs 明细 vs 已标充值 -->
            <div class="rounded-xl border border-border/50 bg-surface-elevated/30 p-3 space-y-2">
              <div class="text-sm font-medium text-foreground">{{ t('admin.upstream.ledger.compareTitle') }}</div>
              <p class="text-[11px] leading-4 text-muted-foreground">{{ t('admin.upstream.ledger.compareHint') }}</p>
              <dl class="grid gap-1.5 text-xs sm:grid-cols-2">
                <div class="rounded-lg border border-border/40 bg-card/60 px-2.5 py-2">
                  <dt class="text-muted-foreground">{{ t('admin.upstream.ledger.platformLifetime') }}</dt>
                  <dd class="mt-0.5 text-sm font-semibold tabular-nums">{{ formatMoney(platformLifetimeCost) }}</dd>
                </div>
                <div class="rounded-lg border border-border/40 bg-card/60 px-2.5 py-2">
                  <dt class="text-muted-foreground">{{ t('admin.upstream.ledger.detailListSum') }}</dt>
                  <dd class="mt-0.5 text-sm font-semibold tabular-nums">{{ formatMoney(detailListCost) }}</dd>
                </div>
                <div class="rounded-lg border border-primary/20 bg-primary/5 px-2.5 py-2">
                  <dt class="text-muted-foreground">{{ t('admin.upstream.ledger.markedRechargeSum') }}</dt>
                  <dd class="mt-0.5 text-sm font-semibold tabular-nums text-primary">{{ formatMoney(markedRechargeCost) }}</dd>
                </div>
                <div class="rounded-lg border border-border/40 bg-card/60 px-2.5 py-2">
                  <dt class="text-muted-foreground">{{ t('admin.upstream.ledger.markedOtherSum') }}</dt>
                  <dd class="mt-0.5 text-sm font-semibold tabular-nums text-muted-foreground">
                    {{ formatMoney(markedGiftCost + markedRebateCost) }}
                  </dd>
                  <dd class="mt-0.5 text-[11px] leading-4 text-muted-foreground/90">
                    {{ t('admin.upstream.ledger.tags.gift') }} {{ formatMoney(markedGiftCost) }}
                    · {{ t('admin.upstream.ledger.tags.rebate') }} {{ formatMoney(markedRebateCost) }}
                  </dd>
                </div>
              </dl>
              <p
                v-if="gapLifetimeVsMarked != null"
                class="text-[11px] tabular-nums"
                :class="Math.abs(gapLifetimeVsMarked) < 0.01 ? 'text-muted-foreground' : 'text-warning'"
              >
                {{ t('admin.upstream.ledger.gapLifetimeVsMarked', { value: formatMoney(gapLifetimeVsMarked) }) }}
              </p>
              <p class="text-[11px] text-muted-foreground">
                {{ t('admin.upstream.ledger.totalRecords') }}: {{ total }}
                <span v-if="unmarkedOnPage"> · {{ t('admin.upstream.ledger.unmarkedPage', { count: unmarkedOnPage }) }}</span>
              </p>
            </div>
            <p class="text-xs text-muted-foreground">{{ t('admin.upstream.ledger.help') }}</p>
            <p v-if="messageKey" class="text-xs text-warning">{{ t(messageKey) }}</p>

            <div>
              <div class="mb-2 flex items-center justify-between gap-2">
                <div class="text-sm font-medium">{{ t('admin.upstream.ledger.platformRecords') }}</div>
                <div v-if="total > 0" class="flex items-center gap-1 text-xs text-muted-foreground">
                  <button
                    type="button"
                    class="rounded p-1 hover:bg-surface-elevated disabled:opacity-40"
                    :disabled="page <= 1 || loading"
                    @click="goPage(page - 1)"
                  >
                    <ChevronLeft class="h-4 w-4" />
                  </button>
                  <span class="tabular-nums px-1">{{ page }} / {{ totalPages }}</span>
                  <button
                    type="button"
                    class="rounded p-1 hover:bg-surface-elevated disabled:opacity-40"
                    :disabled="page >= totalPages || loading"
                    @click="goPage(page + 1)"
                  >
                    <ChevronRight class="h-4 w-4" />
                  </button>
                </div>
              </div>
              <div v-if="!items.length" class="text-sm text-muted-foreground">
                {{ available ? t('admin.upstream.ledger.empty') : t('admin.upstream.ledger.historyUnavailable') }}
              </div>
              <ul v-else class="divide-y divide-border/40 rounded-xl border border-border/50">
                <li v-for="item in items" :key="item.platformId" class="space-y-2 px-3 py-3 text-sm">
                  <div class="flex items-start justify-between gap-3">
                    <div class="min-w-0">
                      <div class="font-medium">
                        {{ formatMoney(item.amountCost) }}
                        <span class="ml-1 text-xs font-normal text-muted-foreground">
                          ({{ formatMoney(item.amountPlatform) }} × {{ rate || '—' }})
                        </span>
                      </div>
                      <div class="mt-0.5 text-xs text-muted-foreground">
                        {{ formatTime(item.createdAt) }}
                        <template v-if="item.note"> · {{ item.note }}</template>
                        <template v-if="item.platformType"> · {{ item.platformType }}</template>
                      </div>
                    </div>
                    <span
                      v-if="item.tag"
                      class="shrink-0 rounded-md border px-2 py-0.5 text-[11px] font-medium"
                      :class="item.countsAsInbound
                        ? 'border-primary/30 bg-primary/10 text-primary'
                        : 'border-border/60 bg-surface text-muted-foreground'"
                    >
                      {{ tagLabel(item.tag) }}
                    </span>
                    <span
                      v-else-if="item.suggestedTag"
                      class="shrink-0 rounded-md border border-warning/30 bg-warning/10 px-2 py-0.5 text-[11px] text-warning"
                    >
                      {{ t('admin.upstream.ledger.suggested', { tag: tagLabel(item.suggestedTag) }) }}
                    </span>
                  </div>
                  <div class="flex flex-wrap gap-1.5">
                    <Button
                      size="sm"
                      variant="secondary"
                      class="h-7 text-xs"
                      :disabled="markingId === item.platformId"
                      :class="item.tag === 'recharge' ? 'border-primary bg-primary/10' : ''"
                      @click="applyTag(item, 'recharge')"
                    >
                      {{ tagLabel('recharge') }}
                    </Button>
                    <Button
                      size="sm"
                      variant="secondary"
                      class="h-7 text-xs"
                      :disabled="markingId === item.platformId"
                      :class="item.tag === 'gift' ? 'border-primary bg-primary/10' : ''"
                      @click="applyTag(item, 'gift')"
                    >
                      {{ tagLabel('gift') }}
                    </Button>
                    <Button
                      size="sm"
                      variant="secondary"
                      class="h-7 text-xs"
                      :disabled="markingId === item.platformId"
                      :class="item.tag === 'rebate' ? 'border-primary bg-primary/10' : ''"
                      @click="applyTag(item, 'rebate')"
                    >
                      {{ tagLabel('rebate') }}
                    </Button>
                  </div>
                </li>
              </ul>
            </div>
          </template>
          <p v-if="errorKey" class="text-sm text-destructive">{{ t(errorKey) }}</p>
        </div>
      </div>
    </div>
  </Teleport>
</template>
