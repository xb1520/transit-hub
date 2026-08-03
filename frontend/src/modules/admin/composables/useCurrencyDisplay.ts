// 全局金额展示模式：CNY / USD / 双币种（localStorage 持久化）。
//
// 约定（与中转/仪表盘一致）：
// - 仪表盘营收、成本、进货、利润等原生单位是 **USD**（平台/中转余额口径）
// - 换算 CNY：CNY = USD × 站点充值倍率（中转倍率）
// - 若后端已给出 CNY（如站点用户余额已乘倍率），可同时传入 usd + cny 避免二次误差

import { computed, ref, watch } from 'vue'

export type CurrencyDisplayMode = 'cny' | 'usd' | 'dual'

const STORAGE_KEY = 'transit-hub:currency-display-mode'

const readStoredMode = (): CurrencyDisplayMode => {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    if (raw === 'cny' || raw === 'usd' || raw === 'dual') return raw
  } catch {
    // ignore
  }
  return 'dual'
}

const mode = ref<CurrencyDisplayMode>(typeof window !== 'undefined' ? readStoredMode() : 'dual')

watch(mode, (value) => {
  try {
    window.localStorage.setItem(STORAGE_KEY, value)
  } catch {
    // ignore
  }
})

const moneyFormatter = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
})

export function formatSymbolAmount(value: number | null | undefined, symbol: '¥' | '$'): string {
  if (value == null || !Number.isFinite(value)) return `${symbol}—`
  return `${symbol}${moneyFormatter.format(value)}`
}

/** USD → CNY：× 中转充值倍率 */
export function usdToCny(usd: number, rate: number): number {
  if (!Number.isFinite(usd)) return 0
  if (!Number.isFinite(rate) || rate <= 0) return usd
  return usd * rate
}

/** CNY → USD：÷ 中转充值倍率 */
export function cnyToUsd(cny: number, rate: number): number {
  if (!Number.isFinite(cny)) return 0
  if (!Number.isFinite(rate) || rate <= 0) return cny
  return cny / rate
}

export interface MoneyParts {
  primary: string
  secondary?: string
}

export interface MoneyInput {
  /** 平台/中转 USD（仪表盘原生单位） */
  usd?: number | null
  /** 已换算的 CNY（可选；站点余额后端已乘倍率时传入） */
  cny?: number | null
  /** 中转充值倍率：CNY = USD × rate */
  rate: number
}

function resolvePair(input: MoneyInput): { usd: number | null; cny: number | null } {
  const rate = input.rate > 0 ? input.rate : 1
  let usd = input.usd != null && Number.isFinite(input.usd) ? input.usd : null
  let cny = input.cny != null && Number.isFinite(input.cny) ? input.cny : null
  if (usd == null && cny != null) usd = cnyToUsd(cny, rate)
  if (cny == null && usd != null) cny = usdToCny(usd, rate)
  return { usd, cny }
}

/**
 * 按展示模式格式化金额。
 * 优先使用 USD 作为业务底量；CNY = USD × rate。
 */
export function formatMoneyParts(
  input: MoneyInput,
  displayMode: CurrencyDisplayMode,
): MoneyParts {
  const { usd, cny } = resolvePair(input)
  if (usd == null && cny == null) {
    if (displayMode === 'usd') return { primary: '$—' }
    if (displayMode === 'cny') return { primary: '¥—' }
    return { primary: '¥—', secondary: '$—' }
  }

  if (displayMode === 'cny') {
    return { primary: formatSymbolAmount(cny, '¥') }
  }
  if (displayMode === 'usd') {
    return { primary: formatSymbolAmount(usd, '$') }
  }
  // dual：主 CNY，副 USD
  return {
    primary: formatSymbolAmount(cny, '¥'),
    secondary: formatSymbolAmount(usd, '$'),
  }
}

/**
 * 紧凑版（图表轴）：输入为 **USD** 业务量。
 * CNY 模式显示 usd×rate，USD 模式显示 usd。
 */
export function formatMoneyCompact(
  usd: number,
  rate: number,
  displayMode: CurrencyDisplayMode,
  numberFmt: Intl.NumberFormat,
): string {
  const useCny = displayMode !== 'usd'
  const value = useCny ? usdToCny(usd, rate) : usd
  const symbol = useCny ? '¥' : '$'
  const absolute = Math.abs(value)
  if (absolute >= 1_000_000) return `${symbol}${numberFmt.format(value / 1_000_000)}M`
  if (absolute >= 1_000) return `${symbol}${numberFmt.format(value / 1_000)}K`
  return `${symbol}${numberFmt.format(value)}`
}

export function useCurrencyDisplay() {
  const displayMode = computed({
    get: () => mode.value,
    set: (v: CurrencyDisplayMode) => { mode.value = v },
  })

  const setDisplayMode = (v: CurrencyDisplayMode) => {
    mode.value = v
  }

  const cycleDisplayMode = () => {
    const order: CurrencyDisplayMode[] = ['dual', 'cny', 'usd']
    const idx = order.indexOf(mode.value)
    mode.value = order[(idx + 1) % order.length]
  }

  return {
    displayMode,
    setDisplayMode,
    cycleDisplayMode,
    /** 业务金额为 USD 时：money({ usd, rate })；已有 CNY 时：money({ usd, cny, rate }) */
    formatMoneyParts: (input: MoneyInput) => formatMoneyParts(input, mode.value),
    formatMoneyCompact: (usd: number, rate: number, numberFmt: Intl.NumberFormat) =>
      formatMoneyCompact(usd, rate, mode.value, numberFmt),
    usdToCny,
    cnyToUsd,
  }
}
