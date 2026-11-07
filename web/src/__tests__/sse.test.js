import { describe, expect, it } from 'vitest'
import { createSSEParser } from '../sse.js'

function collect(chunks) {
  const events = []
  const p = createSSEParser((name, data) => events.push([name, data]))
  chunks.forEach((c) => p.push(c))
  return events
}

describe('SSE parser', () => {
  it('parses named JSON events', () => {
    expect(collect(['event: partial\ndata: {"a":1}\n\n'])).toEqual([['partial', { a: 1 }]])
  })

  it('handles events split across arbitrary chunk boundaries', () => {
    const text = 'event: partial\ndata: {"fields":{"sms_text":"Merhaba"}}\n\nevent: final\ndata: {"ok":true}\n\n'
    for (let cut = 1; cut < text.length; cut += 7) {
      const evs = collect([text.slice(0, cut), text.slice(cut)])
      expect(evs.map((e) => e[0])).toEqual(['partial', 'final'])
      expect(evs[0][1].fields.sms_text).toBe('Merhaba')
    }
  })

  it('handles CRLF and keeps unicode intact', () => {
    expect(collect(['event: x\r\ndata: {"t":"İstanbul ş"}\r\n\r\n'])).toEqual([['x', { t: 'İstanbul ş' }]])
  })

  it('does not emit an incomplete trailing event', () => {
    expect(collect(['event: a\ndata: {"x":1}\n\nevent: b\ndata: {"y"'])).toHaveLength(1)
  })

  it('falls back to raw text for non-JSON data', () => {
    expect(collect(['data: hello\n\n'])).toEqual([['message', 'hello']])
  })
})
