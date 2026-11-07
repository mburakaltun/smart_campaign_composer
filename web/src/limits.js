export const SMS_MAX = 160
export const SUBJECT_MAX = 80
export const PLACEHOLDERS = ['first_name', 'last_name', 'city']

// Count characters (code points), not UTF-16 units, matching the server.
export const charCount = (s) => [...(s || '')].length

const placeholderRe = /\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g

export function unknownPlaceholders(s) {
  const bad = new Set()
  for (const m of (s || '').matchAll(placeholderRe)) {
    if (!PLACEHOLDERS.includes(m[1])) bad.add(m[1])
  }
  return [...bad]
}

export function counterState(s, max) {
  const n = charCount(s)
  return { n, max, over: n > max, near: n > max * 0.9 && n <= max }
}

const FIELD_LABELS = {
  city: 'Şehir',
  language: 'Dil',
  purchase_count: 'Sipariş sayısı',
  total_spent: 'Toplam harcama (TL)',
  cart_items: 'Sepetteki ürün',
  days_since_last_cart_add: 'Son sepete ekleme (gün önce)',
  days_since_last_purchase: 'Son alışveriş (gün önce)',
}
const OP_LABELS = { eq: '=', neq: '≠', gt: '>', gte: '≥', lt: '<', lte: '≤', in: 'şunlardan biri', not_in: 'şunlardan hiçbiri' }

// Turn a validated rule tree into readable lines for the audience card.
export function ruleToLines(node, depth = 0) {
  const pad = '  '.repeat(depth)
  if (node.op) {
    const head = node.op === 'and' ? 'Hepsi sağlanmalı:' : 'Herhangi biri yeterli:'
    return [pad + head, ...(node.children || []).flatMap((c) => ruleToLines(c, depth + 1))]
  }
  const label = FIELD_LABELS[node.field] || node.field
  if (node.operator === 'is_null') return [`${pad}${label} boş (hiç olmamış)`]
  if (node.operator === 'is_not_null') return [`${pad}${label} dolu`]
  const v = Array.isArray(node.value) ? node.value.join(', ') : node.value
  return [`${pad}${label} ${OP_LABELS[node.operator] || node.operator} ${v}`]
}
