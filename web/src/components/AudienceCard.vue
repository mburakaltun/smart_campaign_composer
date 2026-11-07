<script setup>
import { computed } from 'vue'
import { ruleToLines } from '../limits.js'

const props = defineProps({ campaign: { type: Object, required: true } })
const lines = computed(() => ruleToLines(props.campaign.segment_rule))
const fmt = (n) => new Intl.NumberFormat('tr-TR').format(n)
</script>

<template>
  <section class="card">
    <h2>2. Hedef kitle</h2>
    <p v-if="campaign.segment_explanation" class="lead">{{ campaign.segment_explanation }}</p>
    <div class="stats">
      <div class="stat">
        <b data-test="aud-total">{{ fmt(campaign.audience.total) }}</b>
        <span>kişi eşleşti</span>
      </div>
      <div class="stat">
        <b data-test="aud-sms">{{ fmt(campaign.audience.sms) }}</b>
        <span>SMS izni olan</span>
      </div>
      <div class="stat">
        <b data-test="aud-email">{{ fmt(campaign.audience.email) }}</b>
        <span>email izni olan</span>
      </div>
    </div>
    <p class="muted small">İzni olmayan kişiye o kanaldan mesaj gönderilmez.</p>
    <details>
      <summary>Segment kuralı (Claude'un ürettiği, backend'in doğruladığı)</summary>
      <pre class="rule">{{ lines.join('\n') }}</pre>
    </details>
    <p v-if="campaign.audience.total === 0" class="error">Bu kural kimseyle eşleşmedi. Brief'i değiştirip yeniden dene.</p>
  </section>
</template>
