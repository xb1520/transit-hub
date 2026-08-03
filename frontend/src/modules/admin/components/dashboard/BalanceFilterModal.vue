<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { AlertCircle, Filter, Loader2, Plus, Trash2, Users, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { getBalanceFilter, saveBalanceFilter, type BalanceFilterConfig } from '../../api/dashboardAdmin'
import { searchSiteUserCandidates, type SiteUserItem } from '../../api/siteUsers'

const props = defineProps<{
  open: boolean
}>()

const emit = defineEmits<{
  (event: 'close'): void
  (event: 'saved'): void
}>()

const { t } = useI18n()
const router = useRouter()

const loading = ref(false)
const saving = ref(false)
const errorKey = ref<string | null>(null)

const excludeAdmin = ref(true)
const excludeBalances = ref<number[]>([])
const excludeUserIds = ref<string[]>([])
const giftPairs = ref<Array<{ userId: string; amount: string }>>([])
const newBalanceInput = ref('')
const newUserIdInput = ref('')
const newGiftUserId = ref('')
const newGiftAmount = ref('')

// 用户搜索候选（整户排除 / 赠送手调共用）
const candidateTarget = ref<'exclude' | 'gift' | null>(null)
const candidates = ref<SiteUserItem[]>([])
const candidatesLoading = ref(false)
let candidateTimer: ReturnType<typeof setTimeout> | null = null

const runCandidateSearch = (raw: string, target: 'exclude' | 'gift') => {
  const q = raw.trim()
  candidateTarget.value = target
  if (candidateTimer) clearTimeout(candidateTimer)
  if (q.length < 1) {
    candidates.value = []
    return
  }
  candidateTimer = setTimeout(async () => {
    candidatesLoading.value = true
    try {
      const resp = await searchSiteUserCandidates(q, 8)
      candidates.value = resp.items || []
    } catch {
      candidates.value = []
    } finally {
      candidatesLoading.value = false
    }
  }, 280)
}

const pickCandidate = (user: SiteUserItem) => {
  if (candidateTarget.value === 'exclude') {
    newUserIdInput.value = user.id
    addUserId()
  } else if (candidateTarget.value === 'gift') {
    newGiftUserId.value = user.id
  }
  candidates.value = []
  candidateTarget.value = null
}

const goSiteUsers = () => {
  emit('close')
  void router.push('/admin/site-users')
}

const loadConfig = async () => {
  loading.value = true
  errorKey.value = null
  try {
    const config = await getBalanceFilter()
    excludeAdmin.value = config.excludeAdmin
    excludeBalances.value = Array.isArray(config.excludeBalances) ? [...config.excludeBalances] : []
    excludeUserIds.value = Array.isArray(config.excludeUserIds) ? [...config.excludeUserIds] : []
    giftPairs.value = Object.entries(config.userGiftAmounts || {}).map(([userId, amount]) => ({
      userId,
      amount: String(amount),
    }))
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.dashboard.balanceFilter.loadError'
  } finally {
    loading.value = false
  }
}

const addBalance = () => {
  const raw = String(newBalanceInput.value).trim()
  if (!raw) return
  const num = parseFloat(raw)
  if (isNaN(num)) return
  if (!excludeBalances.value.includes(num)) {
    excludeBalances.value.push(num)
  }
  newBalanceInput.value = ''
}

const removeBalance = (index: number) => {
  excludeBalances.value.splice(index, 1)
}

const addUserId = () => {
  const id = newUserIdInput.value.trim()
  if (!id) return
  if (!excludeUserIds.value.includes(id)) excludeUserIds.value.push(id)
  newUserIdInput.value = ''
}

const removeUserId = (index: number) => {
  excludeUserIds.value.splice(index, 1)
}

const addGiftPair = () => {
  const userId = newGiftUserId.value.trim()
  const amount = Number.parseFloat(newGiftAmount.value)
  if (!userId || !Number.isFinite(amount) || amount < 0) return
  const existing = giftPairs.value.findIndex((p) => p.userId === userId)
  if (existing >= 0) giftPairs.value[existing].amount = String(amount)
  else giftPairs.value.push({ userId, amount: String(amount) })
  newGiftUserId.value = ''
  newGiftAmount.value = ''
}

const removeGiftPair = (index: number) => {
  giftPairs.value.splice(index, 1)
}

const handleSave = async () => {
  saving.value = true
  errorKey.value = null
  try {
    const userGiftAmounts: Record<string, number> = {}
    for (const pair of giftPairs.value) {
      const n = Number.parseFloat(pair.amount)
      if (pair.userId && Number.isFinite(n) && n >= 0) userGiftAmounts[pair.userId] = n
    }
    const config: BalanceFilterConfig = {
      excludeAdmin: excludeAdmin.value,
      excludeBalances: excludeBalances.value,
      excludeUserIds: excludeUserIds.value,
      userGiftAmounts: userGiftAmounts,
    }
    await saveBalanceFilter(config)
    emit('saved')
    emit('close')
  } catch (err) {
    errorKey.value = err instanceof Error ? err.message : 'admin.dashboard.balanceFilter.saveError'
  } finally {
    saving.value = false
  }
}

watch(() => props.open, (isOpen) => {
  if (isOpen) {
    void loadConfig()
  }
})

onMounted(() => {
  if (props.open) {
    void loadConfig()
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
        class="relative w-full max-w-lg overflow-hidden rounded-[2rem] border border-border/60 bg-card text-card-foreground shadow-2xl shadow-primary/10 animate-in fade-in zoom-in-95 duration-200"
      >
        <div class="absolute left-0 right-0 top-0 h-1 bg-gradient-to-r from-accent via-primary to-accent" />

        <!-- Header -->
        <div class="flex items-start justify-between gap-4 px-6 pt-6">
          <div class="flex items-center gap-3">
            <div class="flex h-11 w-11 items-center justify-center rounded-full bg-accent/10 text-accent">
              <Filter class="h-5 w-5" />
            </div>
            <div>
              <h2 class="text-lg font-semibold text-foreground">{{ t('admin.dashboard.balanceFilter.title') }}</h2>
              <p class="text-sm text-muted-foreground">{{ t('admin.dashboard.balanceFilter.subtitle') }}</p>
            </div>
          </div>
          <button
            type="button"
            class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-surface-elevated hover:text-foreground"
            :title="t('admin.dashboard.balanceFilter.close')"
            @click="emit('close')"
          >
            <X class="h-5 w-5" />
          </button>
        </div>

        <div class="space-y-5 px-6 py-6">
          <!-- Loading -->
          <div v-if="loading" class="flex items-center justify-center py-8">
            <Loader2 class="h-6 w-6 animate-spin text-primary/60" />
          </div>

          <template v-else>
            <!-- Exclude admin toggle -->
            <div class="flex items-center justify-between gap-4 rounded-xl border border-border/60 p-4">
              <div>
                <p class="text-sm font-medium text-foreground">{{ t('admin.dashboard.balanceFilter.excludeAdmin') }}</p>
                <p class="mt-0.5 text-xs text-muted-foreground">{{ t('admin.dashboard.balanceFilter.excludeAdminHelp') }}</p>
              </div>
              <button
                type="button"
                role="switch"
                :aria-checked="excludeAdmin"
                class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/50"
                :class="excludeAdmin ? 'bg-primary' : 'bg-muted'"
                @click="excludeAdmin = !excludeAdmin"
              >
                <span
                  class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-lg ring-0 transition duration-200"
                  :class="excludeAdmin ? 'translate-x-5' : 'translate-x-0'"
                />
              </button>
            </div>

            <!-- Exclude balance values -->
            <div class="space-y-3">
              <div>
                <p class="text-sm font-medium text-foreground">{{ t('admin.dashboard.balanceFilter.excludeBalances') }}</p>
                <p class="mt-0.5 text-xs text-muted-foreground">{{ t('admin.dashboard.balanceFilter.excludeBalancesHelp') }}</p>
              </div>
              <div v-if="excludeBalances.length > 0" class="flex flex-wrap gap-2">
                <span
                  v-for="(val, idx) in excludeBalances"
                  :key="idx"
                  class="inline-flex items-center gap-1 rounded-lg border border-border/60 bg-surface/60 px-2.5 py-1 text-sm text-foreground"
                >
                  <span class="font-mono">= {{ val }}</span>
                  <button type="button" class="rounded p-0.5 text-muted-foreground hover:text-red-500" @click="removeBalance(idx)">
                    <Trash2 class="h-3.5 w-3.5" />
                  </button>
                </span>
              </div>
              <div class="flex items-center gap-2">
                <Input v-model="newBalanceInput" type="number" step="any" :placeholder="t('admin.dashboard.balanceFilter.addPlaceholder')" class="flex-1" @keydown.enter.prevent="addBalance" />
                <Button type="button" variant="secondary" size="sm" class="shrink-0" :disabled="!String(newBalanceInput).trim()" @click="addBalance">
                  <Plus class="h-4 w-4" />
                  {{ t('admin.dashboard.balanceFilter.add') }}
                </Button>
              </div>
            </div>

            <!-- Exclude whole users -->
            <div class="space-y-3">
              <div>
                <p class="text-sm font-medium text-foreground">{{ t('admin.dashboard.balanceFilter.excludeUserIds') }}</p>
                <p class="mt-0.5 text-xs text-muted-foreground">{{ t('admin.dashboard.balanceFilter.excludeUserIdsHelp') }}</p>
              </div>
              <div v-if="excludeUserIds.length" class="flex flex-wrap gap-2">
                <span v-for="(id, idx) in excludeUserIds" :key="id" class="inline-flex items-center gap-1 rounded-lg border border-border/60 px-2.5 py-1 text-sm">
                  {{ id }}
                  <button type="button" class="text-muted-foreground hover:text-red-500" @click="removeUserId(idx)"><Trash2 class="h-3.5 w-3.5" /></button>
                </span>
              </div>
              <div class="relative flex gap-2">
                <Input
                  v-model="newUserIdInput"
                  :placeholder="t('admin.dashboard.balanceFilter.userIdPlaceholder')"
                  class="flex-1"
                  @input="runCandidateSearch(newUserIdInput, 'exclude')"
                  @keydown.enter.prevent="addUserId"
                />
                <Button type="button" variant="secondary" size="sm" @click="addUserId"><Plus class="h-4 w-4" /></Button>
                <div
                  v-if="candidateTarget === 'exclude' && (candidates.length || candidatesLoading)"
                  class="absolute left-0 right-10 top-full z-30 mt-1 max-h-48 overflow-y-auto rounded-xl border border-border/60 bg-card shadow-xl"
                >
                  <div v-if="candidatesLoading" class="flex justify-center py-3"><Loader2 class="h-4 w-4 animate-spin text-muted-foreground" /></div>
                  <button
                    v-for="c in candidates"
                    :key="c.id"
                    type="button"
                    class="flex w-full items-center justify-between gap-2 border-b border-border/30 px-3 py-2 text-left text-xs last:border-0 hover:bg-surface-elevated"
                    @click="pickCandidate(c)"
                  >
                    <span class="truncate">{{ c.email || c.username || c.id }}</span>
                    <span class="shrink-0 font-mono text-muted-foreground">{{ c.id }}</span>
                  </button>
                </div>
              </div>
            </div>

            <!-- Gift amounts (revenue only) -->
            <div class="space-y-3">
              <div>
                <p class="text-sm font-medium text-foreground">{{ t('admin.dashboard.balanceFilter.userGifts') }}</p>
                <p class="mt-0.5 text-xs text-muted-foreground">{{ t('admin.dashboard.balanceFilter.userGiftsHelp') }}</p>
                <button
                  type="button"
                  class="mt-1 inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
                  @click="goSiteUsers"
                >
                  <Users class="h-3.5 w-3.5" />
                  {{ t('admin.dashboard.balanceFilter.siteUsersLink') }}
                </button>
              </div>
              <div v-if="giftPairs.length" class="space-y-1">
                <div v-for="(pair, idx) in giftPairs" :key="pair.userId" class="flex items-center justify-between rounded-lg border border-border/50 px-3 py-1.5 text-sm">
                  <span class="font-mono">{{ pair.userId }} → {{ pair.amount }}</span>
                  <button type="button" class="text-muted-foreground hover:text-red-500" @click="removeGiftPair(idx)"><Trash2 class="h-3.5 w-3.5" /></button>
                </div>
              </div>
              <div class="relative flex flex-col gap-2 sm:flex-row">
                <Input
                  v-model="newGiftUserId"
                  :placeholder="t('admin.dashboard.balanceFilter.userIdPlaceholder')"
                  class="flex-1"
                  @input="runCandidateSearch(newGiftUserId, 'gift')"
                />
                <Input v-model="newGiftAmount" type="number" min="0" step="any" :placeholder="t('admin.dashboard.balanceFilter.giftAmountPlaceholder')" class="sm:w-32" />
                <Button type="button" variant="secondary" size="sm" @click="addGiftPair"><Plus class="h-4 w-4" /></Button>
                <div
                  v-if="candidateTarget === 'gift' && (candidates.length || candidatesLoading)"
                  class="absolute left-0 right-0 top-full z-30 mt-1 max-h-48 overflow-y-auto rounded-xl border border-border/60 bg-card shadow-xl sm:right-36"
                >
                  <div v-if="candidatesLoading" class="flex justify-center py-3"><Loader2 class="h-4 w-4 animate-spin text-muted-foreground" /></div>
                  <button
                    v-for="c in candidates"
                    :key="c.id"
                    type="button"
                    class="flex w-full items-center justify-between gap-2 border-b border-border/30 px-3 py-2 text-left text-xs last:border-0 hover:bg-surface-elevated"
                    @click="pickCandidate(c)"
                  >
                    <span class="truncate">{{ c.email || c.username || c.id }}</span>
                    <span class="shrink-0 font-mono text-muted-foreground">{{ c.id }}</span>
                  </button>
                </div>
              </div>
            </div>

            <!-- Error -->
            <div v-if="errorKey" class="flex items-start gap-2 rounded-xl border border-warning/20 bg-warning/10 p-3 text-sm text-warning">
              <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
              <span>{{ t(errorKey) }}</span>
            </div>

            <!-- Actions -->
            <div class="flex items-center justify-end gap-3 pt-1">
              <Button type="button" variant="secondary" @click="emit('close')">
                {{ t('admin.dashboard.balanceFilter.cancel') }}
              </Button>
              <Button type="button" :disabled="saving" @click="handleSave">
                <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
                {{ saving ? t('admin.dashboard.balanceFilter.saving') : t('admin.dashboard.balanceFilter.save') }}
              </Button>
            </div>
          </template>
        </div>
      </div>
    </div>
  </Teleport>
</template>
