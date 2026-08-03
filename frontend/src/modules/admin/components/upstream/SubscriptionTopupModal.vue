<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, PackagePlus, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { createSubscriptionTopup } from '../../api/upstream'
import type { UpstreamSite, UpstreamSubscriptionInfo } from '../../types/upstream'

const props = defineProps<{
  open: boolean
  site: UpstreamSite | null
  subscription: UpstreamSubscriptionInfo | null
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'updated'): void
}>()

const { t } = useI18n()
const amountCost = ref('')
const businessDate = ref('')
const note = ref('')
const saving = ref(false)
const errorKey = ref('')

/** 从 startsAt（开通/续费日）解析业务日 YYYY-MM-DD，与后端业务时区 Asia/Shanghai 对齐。 */
const businessDateFromStartsAt = (raw?: string): string => {
  const s = (raw || '').trim()
  if (!s) return ''
  if (/^\d{4}-\d{2}-\d{2}$/.test(s)) return s
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) {
    const m = s.match(/^(\d{4}-\d{2}-\d{2})/)
    return m ? m[1] : ''
  }
  // en-CA → YYYY-MM-DD；固定上海时区，避免 UTC 把跨日时刻算错
  return new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(d)
}

const resetForm = () => {
  amountCost.value = ''
  note.value = ''
  businessDate.value = businessDateFromStartsAt(props.subscription?.startsAt)
  errorKey.value = ''
}

watch(
  () => [props.open, props.subscription?.id, props.subscription?.startsAt] as const,
  ([open]) => {
    if (open) resetForm()
  },
)

const title = computed(() => {
  const name = props.subscription?.groupName || props.subscription?.id || ''
  return t('admin.upstream.subscriptions.topupTitle', { name })
})

const submit = async () => {
  if (!props.site || !props.subscription || saving.value) return
  const cost = Number.parseFloat(amountCost.value)
  if (!Number.isFinite(cost) || cost <= 0) {
    errorKey.value = 'admin.upstream.subscriptions.topupInvalidAmount'
    return
  }
  const date = businessDate.value.trim()
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) {
    errorKey.value = 'admin.upstream.subscriptions.topupInvalidDate'
    return
  }
  saving.value = true
  errorKey.value = ''
  try {
    await createSubscriptionTopup(props.site.id, {
      subscriptionId: props.subscription.id,
      groupName: props.subscription.groupName,
      amountCost: cost,
      businessDate: date,
      note: note.value.trim() || undefined,
    })
    emit('updated')
    emit('close')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open && site && subscription" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')" />
      <div class="relative w-full max-w-md overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl">
        <div class="flex items-center justify-between border-b border-border/40 px-5 py-4">
          <div class="flex items-center gap-3 min-w-0">
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <PackagePlus class="h-5 w-5" />
            </div>
            <div class="min-w-0">
              <h2 class="truncate text-base font-semibold">{{ title }}</h2>
              <p class="text-xs text-muted-foreground">{{ site.name }} · {{ t('admin.upstream.ledger.costUnit') }}</p>
            </div>
          </div>
          <button type="button" class="rounded-md p-1 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="space-y-4 px-5 py-5">
          <p class="text-xs leading-5 text-muted-foreground">
            {{ t('admin.upstream.subscriptions.topupHint') }}
          </p>

          <div>
            <label class="text-xs font-medium text-muted-foreground">{{ t('admin.upstream.subscriptions.topupAmount') }}</label>
            <Input
              v-model="amountCost"
              type="number"
              min="0"
              step="0.01"
              class="mt-1"
              :placeholder="t('admin.upstream.subscriptions.topupAmountPlaceholder')"
            />
          </div>

          <div>
            <label class="text-xs font-medium text-muted-foreground">{{ t('admin.upstream.subscriptions.topupDate') }}</label>
            <Input v-model="businessDate" type="date" class="mt-1" />
            <p class="mt-1 text-[11px] text-muted-foreground">
              {{ t('admin.upstream.subscriptions.topupDateHelp') }}
              <template v-if="subscription.startsAt">
                · startsAt: {{ subscription.startsAt }}
              </template>
            </p>
          </div>

          <div>
            <label class="text-xs font-medium text-muted-foreground">{{ t('admin.upstream.settlement.notePlaceholder') }}</label>
            <Input v-model="note" class="mt-1" :placeholder="t('admin.upstream.subscriptions.topupNotePlaceholder')" />
          </div>

          <p v-if="errorKey" class="text-sm text-destructive">{{ t(errorKey) }}</p>

          <div class="flex justify-end gap-2 pt-1">
            <Button variant="ghost" @click="emit('close')">{{ t('admin.upstream.siteSettings.cancel') }}</Button>
            <Button :disabled="saving" @click="submit">
              <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
              {{ t('admin.upstream.subscriptions.topupSubmit') }}
            </Button>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
