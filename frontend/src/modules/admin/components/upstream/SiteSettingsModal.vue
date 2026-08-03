<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Settings2, X, Save, Loader2, CheckCircle2 } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { updateSiteSettings } from '../../api/upstream'
import type { SiteSettings, UpstreamSite } from '../../types/upstream'

const props = defineProps<{
  open: boolean
  site: UpstreamSite | null
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'saved', siteId: string, settings: SiteSettings): void
}>()

const { t } = useI18n()

const useCustomThreshold = ref(false)
const balanceThreshold = ref('')
const settlementMode = ref<'prepaid_wallet' | 'credit_line'>('prepaid_wallet')
const creditLimit = ref('')
const settlementCurrency = ref('CNY')
const isSaving = ref(false)
const showSuccess = ref(false)
const errorMsg = ref<string | null>(null)

watch(() => props.open, (isOpen) => {
  if (!isOpen || !props.site) return
  const s = props.site.settings
  if (s.balanceThreshold != null) {
    useCustomThreshold.value = true
    balanceThreshold.value = String(s.balanceThreshold)
  } else {
    useCustomThreshold.value = false
    balanceThreshold.value = ''
  }
  settlementMode.value = (s.settlementMode === 'credit_line' ? 'credit_line' : 'prepaid_wallet')
  creditLimit.value = s.creditLimit != null ? String(s.creditLimit) : ''
  settlementCurrency.value = s.settlementCurrency || 'CNY'
  errorMsg.value = null
  showSuccess.value = false
})

const asText = (value: unknown): string => {
  if (value == null) return ''
  return String(value)
}

const parseOptionalNumber = (raw: unknown): number | null => {
  const trimmed = asText(raw).trim()
  if (!trimmed) return null
  const n = Number.parseFloat(trimmed)
  return Number.isFinite(n) ? n : null
}

const save = async () => {
  if (isSaving.value || !props.site) return
  isSaving.value = true
  errorMsg.value = null
  try {
    // Input type=number 在部分环境下会把 v-model 写成 number，禁止直接 .trim()。
    // 预警阈值允许负数：例如 -1 表示余额用完也不报警（站点弃用场景）。
    let threshold: number | null = null
    if (useCustomThreshold.value) {
      threshold = parseOptionalNumber(balanceThreshold.value)
    }
    const settings: SiteSettings = {
      balanceThreshold: threshold,
      settlementMode: settlementMode.value,
      creditLimit: settlementMode.value === 'credit_line' ? parseOptionalNumber(creditLimit.value) : null,
      settlementCurrency: asText(settlementCurrency.value).trim() || 'CNY',
    }
    if (settings.creditLimit != null && settings.creditLimit < 0) {
      errorMsg.value = 'admin.upstream.siteSettings.creditLimitNonNegative'
      return
    }
    await updateSiteSettings(props.site.id, settings)
    emit('saved', props.site.id, settings)
    showSuccess.value = true
    setTimeout(() => { showSuccess.value = false }, 2000)
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : 'admin.upstream.errors.unknown'
  } finally {
    isSaving.value = false
  }
}
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open && site" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')"></div>

      <div
        role="dialog"
        aria-modal="true"
        class="relative w-full max-w-lg max-h-[90vh] overflow-y-auto rounded-2xl border border-border/60 bg-card text-card-foreground shadow-2xl animate-in fade-in zoom-in-95 duration-200"
      >
        <div class="absolute left-0 right-0 top-0 h-1 bg-gradient-to-r from-primary via-accent to-primary" />

        <div class="flex items-center justify-between px-6 pt-6 pb-4 border-b border-border/40">
          <div class="flex items-center gap-3">
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <Settings2 class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-base font-semibold text-foreground">{{ t('admin.upstream.siteSettings.title') }}</h2>
              <p class="text-xs text-muted-foreground">{{ site.name }}</p>
            </div>
          </div>
          <button
            type="button"
            class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground"
            @click="emit('close')"
          >
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="px-6 py-5 space-y-6">
          <div class="space-y-3">
            <div class="flex items-center justify-between">
              <label class="text-sm font-medium text-foreground">{{ t('admin.upstream.siteSettings.balanceThreshold') }}</label>
              <label class="relative inline-flex items-center cursor-pointer">
                <input type="checkbox" v-model="useCustomThreshold" class="sr-only peer">
                <div class="w-9 h-5 bg-surface-elevated rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-border after:border after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:bg-primary"></div>
              </label>
            </div>
            <p class="text-xs text-muted-foreground">{{ t('admin.upstream.siteSettings.balanceThresholdHelp') }}</p>
            <div v-if="useCustomThreshold" class="animate-in slide-in-from-top-2 fade-in duration-200">
              <Input
                type="number"
                v-model="balanceThreshold"
                min="0"
                step="0.01"
                :placeholder="t('admin.upstream.siteSettings.balanceThresholdPlaceholder')"
                class="max-w-[200px]"
              />
            </div>
          </div>

          <div class="space-y-3 border-t border-border/40 pt-5">
            <label class="text-sm font-medium text-foreground">{{ t('admin.upstream.siteSettings.settlementMode') }}</label>
            <p class="text-xs text-muted-foreground">{{ t('admin.upstream.siteSettings.settlementModeHelp') }}</p>
            <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
              <button
                type="button"
                class="rounded-xl border px-3 py-3 text-left text-sm transition-colors"
                :class="settlementMode === 'prepaid_wallet' ? 'border-primary bg-primary/10 text-foreground' : 'border-border/60 text-muted-foreground hover:bg-surface-elevated'"
                @click="settlementMode = 'prepaid_wallet'"
              >
                <div class="font-medium">{{ t('admin.upstream.siteSettings.modePrepaid') }}</div>
                <div class="mt-1 text-xs opacity-80">{{ t('admin.upstream.siteSettings.modePrepaidHelp') }}</div>
              </button>
              <button
                type="button"
                class="rounded-xl border px-3 py-3 text-left text-sm transition-colors"
                :class="settlementMode === 'credit_line' ? 'border-primary bg-primary/10 text-foreground' : 'border-border/60 text-muted-foreground hover:bg-surface-elevated'"
                @click="settlementMode = 'credit_line'"
              >
                <div class="font-medium">{{ t('admin.upstream.siteSettings.modeCredit') }}</div>
                <div class="mt-1 text-xs opacity-80">{{ t('admin.upstream.siteSettings.modeCreditHelp') }}</div>
              </button>
            </div>

            <div v-if="settlementMode === 'credit_line'" class="space-y-3 rounded-xl border border-border/50 bg-surface-elevated/40 p-3">
              <div>
                <label class="text-xs font-medium text-muted-foreground">{{ t('admin.upstream.siteSettings.creditLimit') }}</label>
                <Input type="number" v-model="creditLimit" min="0" step="0.01" class="mt-1" :placeholder="t('admin.upstream.siteSettings.creditLimitPlaceholder')" />
              </div>
              <div>
                <label class="text-xs font-medium text-muted-foreground">{{ t('admin.upstream.siteSettings.settlementCurrency') }}</label>
                <Input v-model="settlementCurrency" class="mt-1" placeholder="CNY" />
              </div>
              <p class="text-xs text-muted-foreground">{{ t('admin.upstream.siteSettings.creditCostHint') }}</p>
            </div>
          </div>

          <p v-if="errorMsg" class="text-sm text-destructive rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2">
            {{ t(errorMsg) }}
          </p>
        </div>

        <div class="px-6 pb-6 flex justify-end gap-3">
          <Button variant="ghost" @click="emit('close')">
            {{ t('admin.upstream.siteSettings.cancel') }}
          </Button>
          <Button :disabled="isSaving" @click="save">
            <Loader2 v-if="isSaving" class="h-4 w-4 animate-spin mr-2" />
            <CheckCircle2 v-else-if="showSuccess" class="h-4 w-4 mr-2 text-green-400" />
            <Save v-else class="h-4 w-4 mr-2" />
            {{ showSuccess ? t('admin.upstream.siteSettings.saveSuccess') : (isSaving ? t('admin.upstream.siteSettings.saving') : t('admin.upstream.siteSettings.save')) }}
          </Button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
