import { streamSSE } from './sse.js'

async function request(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const err = new Error(data.error || `HTTP ${res.status}`)
    err.status = res.status
    err.problems = data.problems || []
    throw err
  }
  return data
}

export const api = {
  createCampaign: (brief) => request('POST', '/api/campaigns', { brief }),
  getCampaign: (id) => request('GET', `/api/campaigns/${id}`),
  saveContent: (id, content) => request('PUT', `/api/campaigns/${id}/content`, content),
  approve: (id) => request('POST', `/api/campaigns/${id}/approve`),
  compose: (id, onEvent, signal) => streamSSE(`/api/campaigns/${id}/compose`, { method: 'POST', signal }, onEvent),
  watch: (id, onEvent, signal) => streamSSE(`/api/campaigns/${id}/events`, { signal }, onEvent),
}
