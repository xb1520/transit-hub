<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, Receipt, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  createSiteSettlement,
  deleteSiteSettlement,
  getSiteSettlementSummary,
  listSiteSettlements,
  voidSiteSettlement,
} from '../../api/upstream'
import type { SettlementRecord, SettlementSummary, UpstreamSite } from '../../types/upstream'

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
const saving = ref(false)
const actingId = ref('')
const errorKey = ref('')
const summary = ref<SettlementSummary | null>(null)
const records = ref<SettlementRecord[]>([])
const amount = ref('')
const note = ref('')

const formatMoney = (n: number | null | undefined) => {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(2)
}

const load = async () => {
  if (!props.site) return
  loading.value = true
  errorKey.value = ''
  try {
    const [sum, list] = await Promise.all([
      getSiteSettlementSummary(props.site.id),
      listSiteSettlements(props.site.id),
    ])
    summary.value = sum
    records.value = list.items || []
    if (sum.outstanding > 0) {
      amount.value = String(Number(sum.outstanding.toFixed(4)))
    } else {
      amount.value = ''
    }
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (open) => {
  if (open) void load()
})

const currency = computed(() => summary.value?.settlementCurrency || props.site?.settings.settlementCurrency || 'CNY')

const submit = async () => {
  if (!props.site || saving.value) return
  const n = Number.parseFloat(amount.value)
  if (!Number.isFinite(n) || n <= 0) {
    errorKey.value = 'admin.upstream.settlement.invalidAmount'
    return
  }
  saving.value = true
  errorKey.value = ''
  try {
    await createSiteSettlement(props.site.id, { amount: n, note: note.value.trim() })
    note.value = ''
    await load()
    emit('updated')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    saving.value = false
  }
}

const voidRecord = async (record: SettlementRecord) => {
  if (!props.site || record.status !== 'active' || actingId.value) return
  if (!window.confirm(t('admin.upstream.settlement.voidConfirm'))) return
  actingId.value = record.id
  errorKey.value = ''
  try {
    await voidSiteSettlement(props.site.id, record.id)
    await load()
    emit('updated')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    actingId.value = ''
  }
}

const deleteRecord = async (record: SettlementRecord) => {
  if (!props.site || actingId.value) return
  if (!window.confirm(t('admin.upstream.settlement.deleteConfirm'))) return
  actingId.value = record.id
  errorKey.value = ''
  try {
    await deleteSiteSettlement(props.site.id, record.id)
    await load()
    emit('updated')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    actingId.value = ''
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
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-warning/10 text-warning">
              <Receipt class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-base font-semibold">{{ t('admin.upstream.settlement.title') }}</h2>
              <p class="text-xs text-muted-foreground">{{ site.name }} · {{ t('admin.upstream.settlement.costUnit') }} · {{ currency }}</p>
            </div>
          </div>
          <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="space-y-5 px-6 py-5">
          <div v-if="loading" class="flex justify-center py-8 text-muted-foreground">
            <Loader2 class="h-6 w-6 animate-spin" />
          </div>
          <template v-else>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <div class="rounded-xl border border-border/50 bg-surface-elevated/40 p-3">
                <div class="text-[11px] text-muted-foreground">{{ t('admin.upstream.settlement.outstanding') }}</div>
                <div class="mt-1 text-lg font-semibold text-warning">{{ formatMoney(summary?.outstanding) }}</div>
              </div>
              <div class="rounded-xl border border-border/50 bg-surface-elevated/40 p-3">
                <div class="text-[11px] text-muted-foreground">{{ t('admin.upstream.settlement.settled') }}</div>
                <div class="mt-1 text-lg font-semibold">{{ formatMoney(summary?.settledCost) }}</div>
              </div>
              <div class="rounded-xl border border-border/50 bg-surface-elevated/40 p-3">
                <div class="text-[11px] text-muted-foreground">{{ t('admin.upstream.settlement.consumed') }}</div>
                <div class="mt-1 text-lg font-semibold">{{ formatMoney(summary?.consumedCost) }}</div>
              </div>
              <div class="rounded-xl border border-border/50 bg-surface-elevated/40 p-3">
                <div class="text-[11px] text-muted-foreground">{{ t('admin.upstream.settlement.creditRemaining') }}</div>
                <div class="mt-1 text-lg font-semibold">{{ formatMoney(summary?.creditRemaining) }}</div>
              </div>
            </div>
            <p class="text-xs text-muted-foreground">
              {{ t('admin.upstream.settlement.platformBalanceHint', { value: formatMoney(summary?.platformBalance) }) }}
            </p>

            <div class="space-y-2 rounded-xl border border-border/50 p-4">
              <div class="text-sm font-medium">{{ t('admin.upstream.settlement.register') }}</div>
              <div class="flex flex-col gap-2 sm:flex-row">
                <Input v-model="amount" type="number" min="0" step="0.01" class="sm:w-40" :placeholder="t('admin.upstream.settlement.amountPlaceholder')" />
                <Input v-model="note" class="flex-1" :placeholder="t('admin.upstream.settlement.notePlaceholder')" />
                <Button :disabled="saving" @click="submit">
                  <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
                  {{ t('admin.upstream.settlement.submit') }}
                </Button>
              </div>
              <p class="text-xs text-muted-foreground">{{ t('admin.upstream.settlement.manualHint') }}</p>
            </div>

            <div>
              <div class="mb-2 text-sm font-medium">{{ t('admin.upstream.settlement.history') }}</div>
              <div v-if="!records.length" class="text-sm text-muted-foreground">{{ t('admin.upstream.settlement.empty') }}</div>
              <ul v-else class="divide-y divide-border/40 rounded-xl border border-border/50">
                <li v-for="item in records" :key="item.id" class="flex items-center justify-between gap-3 px-3 py-2 text-sm">
                  <div class="min-w-0">
                    <div class="font-medium" :class="item.status === 'voided' ? 'line-through text-muted-foreground' : ''">
                      {{ formatMoney(item.amount) }}
                      <span class="ml-2 text-xs text-muted-foreground">{{ new Date(item.settledAt).toLocaleString() }}</span>
                    </div>
                    <div class="text-xs text-muted-foreground">{{ item.note || '—' }} · {{ item.status }}</div>
                  </div>
                  <div class="flex shrink-0 items-center gap-1">
                    <Button
                      v-if="item.status === 'active'"
                      size="sm"
                      variant="ghost"
                      :disabled="actingId === item.id"
                      @click="voidRecord(item)"
                    >
                      {{ t('admin.upstream.settlement.void') }}
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      class="text-destructive hover:text-destructive"
                      :disabled="actingId === item.id"
                      @click="deleteRecord(item)"
                    >
                      {{ t('admin.upstream.settlement.delete') }}
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
