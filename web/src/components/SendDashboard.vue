<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api.js'

const props = defineProps({ campaignId: { type: String, required: true } })
const status = ref('sending')
const stats = ref({ sms: {}, email: {} })
const lost = ref(false)
let abort = null
let stopped = false

const KEYS = [
  ['sent', 'Gönderildi', 'ok'],
  ['sending', 'Gönderiliyor', 'busy'],
  ['pending', 'Kuyrukta / yeniden denenecek', 'wait'],
  ['failed', 'Başarısız', 'bad'],
  ['skipped', 'Atlandı', 'skip'],
]
const channels = computed(() =>
  ['sms', 'email'].map((ch) => {
    const s = stats.value[ch] || {}
    const total = Object.values(s).reduce((a, b) => a + b, 0)
    return { ch, label: ch === 'sms' ? 'SMS' : 'Email', total, s }
  }),
)
const pct = (n, total) => (total ? Math.round((n / total) * 100) : 0)
const done = computed(() => status.value === 'completed')

async function watch() {
  while (!stopped && status.value !== 'completed') {
    abort = new AbortController()
    try {
      lost.value = false
      await api.watch(
        props.campaignId,
        (name, data) => {
          if (name === 'stats') {
            status.value = data.status
            stats.value = data.stats
          }
        },
        abort.signal,
      )
    } catch {
      if (stopped) return
      lost.value = true
    }
    if (!stopped && status.value !== 'completed') await new Promise((r) => setTimeout(r, 1500)) // reconnect
  }
}
onMounted(watch)
onBeforeUnmount(() => {
  stopped = true
  abort?.abort()
})
</script>

<template>
  <section class="card">
    <h2>5. Canlı gönderim durumu</h2>
    <p>
      <span :class="['badge', done ? 'done' : 'live']" data-test="status">{{ done ? 'Tamamlandı' : 'Gönderiliyor' }}</span>
      <span v-if="lost" class="muted small"> Bağlantı koptu, yeniden bağlanılıyor…</span>
    </p>
    <div v-for="c in channels" :key="c.ch" class="channel">
      <div class="row">
        <b>{{ c.label }}</b>
        <span class="muted small">{{ c.total }} mesaj</span>
      </div>
      <div class="bar" role="progressbar" :aria-valuenow="pct(c.s.sent || 0, c.total)" aria-valuemin="0" aria-valuemax="100">
        <div v-for="[k, , cls] in KEYS" :key="k" :class="['seg', cls]" :style="{ width: pct(c.s[k] || 0, c.total) + '%' }" />
      </div>
      <ul class="legend">
        <li v-for="[k, label, cls] in KEYS" :key="k">
          <i :class="['dot', cls]" />{{ label }}: <b :data-test="`${c.ch}-${k}`">{{ c.s[k] || 0 }}</b>
        </li>
      </ul>
    </div>
    <p class="muted small">Provider zaman zaman 5xx / 429 / zaman aşımı döndürür; mesajlar aynı idempotency key ile otomatik yeniden denenir.</p>
  </section>
</template>
