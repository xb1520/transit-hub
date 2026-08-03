<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, PackagePlus, RefreshCw, X } from 'lucide-vue-next'
import { getTodayInboundBreakdown, type TodayInboundBreakdownItem } from '../../api/dashboardAdmin'
import { formatCny } from '../../utils/dashboard'

const props = defineProps<{
  open: boolean
}>()

const emit = defineEmits<{
  (event: 'close'): void
}>()

const { t } = useI18n()
const loading = ref(false)
const error = ref<string | null>(null)
const sites = ref<TodayInboundBreakdownItem[]>([])
const total = ref(0)
const date = ref('')

const loadData = async () => {
  loading.value = true
  error.value = null
  try {
    const response = await getTodayInboundBreakdown()
    sites.value = response.sites ?? []
    total.value = response.total ?? 0
    date.value = response.date ?? ''
  } catch {
    error.value = 'admin.dashboard.todayInbound.loadError'
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (isOpen) => {
  if (isOpen) void loadData()
})
</script>

<template>
  <Teleport defer to="body">
    <div v-if="open" class="fixed inset-0 z-[100] flex items-center justify-center p-4">
      <div class="absolute inset-0 bg-background/80 backdrop-blur-sm" @click="emit('close')" />
      <div
        role="dialog"
        aria-modal="true"
        class="relative w-full max-w-2xl overflow-hidden rounded-[2rem] border border-border/60 bg-card text-card-foreground shadow-2xl"
      >
        <div class="flex items-start justify-between gap-4 px-6 pt-6">
          <div class="flex items-center gap-3">
            <div class="flex h-11 w-11 items-center justify-center rounded-full bg-primary/10 text-primary">
              <PackagePlus class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-lg font-semibold text-foreground">{{ t('admin.dashboard.todayInbound.title') }}</h2>
              <p class="text-sm text-muted-foreground">
                {{ t('admin.dashboard.todayInbound.subtitle', { total: formatCny(total), date }) }}
              </p>
            </div>
          </div>
          <div class="flex items-center gap-2">
            <button
              type="button"
              class="rounded-md p-1.5 text-muted-foreground hover:bg-surface-elevated"
              :disabled="loading"
              @click="loadData"
            >
              <RefreshCw class="h-4 w-4" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button type="button" class="rounded-md p-1.5 text-muted-foreground hover:bg-surface-elevated" @click="emit('close')">
              <X class="h-5 w-5" />
            </button>
          </div>
        </div>

        <div class="max-h-[60vh] overflow-y-auto px-6 py-5">
          <div v-if="loading" class="flex justify-center py-12 text-muted-foreground">
            <Loader2 class="h-6 w-6 animate-spin" />
          </div>
          <p v-else-if="error" class="py-8 text-center text-sm text-destructive">{{ t(error) }}</p>
          <div v-else-if="!sites.length" class="py-8 text-center text-sm text-muted-foreground">
            {{ t('admin.dashboard.todayInbound.empty') }}
          </div>
          <table v-else class="w-full text-left text-sm">
            <thead class="text-xs text-muted-foreground">
              <tr class="border-b border-border/40">
                <th class="py-2 font-medium">{{ t('admin.dashboard.todayInbound.colSite') }}</th>
                <th class="py-2 font-medium">{{ t('admin.dashboard.todayInbound.colPlatform') }}</th>
                <th class="py-2 font-medium text-right">{{ t('admin.dashboard.todayInbound.colEntries') }}</th>
                <th class="py-2 font-medium text-right">{{ t('admin.dashboard.todayInbound.colAmount') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="site in sites" :key="site.siteId" class="border-b border-border/30">
                <td class="py-2.5 font-medium">{{ site.siteName }}</td>
                <td class="py-2.5 text-muted-foreground">{{ site.platform || '—' }}</td>
                <td class="py-2.5 text-right tabular-nums">{{ site.entryCount }}</td>
                <td class="py-2.5 text-right font-semibold tabular-nums">{{ formatCny(site.amountCost) }}</td>
              </tr>
            </tbody>
            <tfoot>
              <tr>
                <td colspan="3" class="pt-3 text-right text-muted-foreground">{{ t('admin.dashboard.todayInbound.total') }}</td>
                <td class="pt-3 text-right font-semibold tabular-nums">{{ formatCny(total) }}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </div>
    </div>
  </Teleport>
</template>
