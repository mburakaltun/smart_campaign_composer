import { describe, expect, it } from 'vitest'
import { charCount, counterState, ruleToLines, unknownPlaceholders } from '../limits.js'

describe('limits', () => {
  it('counts characters, not bytes', () => {
    expect(charCount('şğüİ')).toBe(4)
    expect(charCount('👍a')).toBe(2)
  })

  it('flags over-limit and near-limit text', () => {
    expect(counterState('a'.repeat(160), 160)).toMatchObject({ n: 160, over: false })
    expect(counterState('a'.repeat(161), 160).over).toBe(true)
    expect(counterState('a'.repeat(150), 160).near).toBe(true)
  })

  it('finds unknown placeholders but accepts spaced known ones', () => {
    expect(unknownPlaceholders('Merhaba {{ first_name }} {{phone}} {{phone}}')).toEqual(['phone'])
    expect(unknownPlaceholders('{{first_name}} {{city}}')).toEqual([])
  })

  it('renders a rule tree as readable lines', () => {
    const lines = ruleToLines({
      op: 'and',
      children: [
        { field: 'city', operator: 'eq', value: 'İstanbul' },
        { op: 'or', children: [{ field: 'days_since_last_purchase', operator: 'is_null' }, { field: 'days_since_last_purchase', operator: 'gt', value: 30 }] },
      ],
    })
    expect(lines[0]).toBe('Hepsi sağlanmalı:')
    expect(lines).toContain('  Şehir = İstanbul')
    expect(lines.join('\n')).toContain('boş (hiç olmamış)')
    expect(lines.join('\n')).toContain('> 30')
  })
})
