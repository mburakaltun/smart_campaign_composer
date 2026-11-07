// Minimal Server-Sent Events client over fetch (EventSource cannot POST).

export function createSSEParser(onEvent) {
  let buf = ''
  return {
    push(chunk) {
      buf = (buf + chunk).replace(/\r\n/g, '\n')
      let idx
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const block = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        let name = 'message'
        const data = []
        for (const line of block.split('\n')) {
          if (line.startsWith('event:')) name = line.slice(6).trim()
          else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
        }
        if (!data.length) continue
        const raw = data.join('\n')
        let parsed = raw
        try {
          parsed = JSON.parse(raw)
        } catch {
          /* keep raw text */
        }
        onEvent(name, parsed)
      }
    },
  }
}

export async function streamSSE(url, { method = 'GET', signal } = {}, onEvent) {
  const res = await fetch(url, { method, signal, headers: { Accept: 'text/event-stream' } })
  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      msg = (await res.json()).error || msg
    } catch {
      /* not JSON */
    }
    const err = new Error(msg)
    err.status = res.status
    throw err
  }
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  const parser = createSSEParser(onEvent)
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    parser.push(decoder.decode(value, { stream: true }))
  }
}
