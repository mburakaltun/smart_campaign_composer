<script setup>
import { ref } from 'vue'

defineProps({ loading: Boolean, error: String })
const emit = defineEmits(['submit'])

const example =
  "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."
const brief = ref('')
const MAX = 2000
</script>

<template>
  <section class="card">
    <h2>1. Kampanya brief'i</h2>
    <p class="muted">Kimlere, ne göndermek istediğini kendi cümlelerinle yaz. Kitleyi ve mesajları Claude önerecek, son karar senin.</p>
    <textarea
      v-model="brief"
      rows="5"
      :maxlength="MAX"
      placeholder="Örn. Son 30 günde sepetine ürün ekleyip alışveriş yapmayan..."
      :disabled="loading"
      data-test="brief"
    />
    <div class="row">
      <button class="link" type="button" :disabled="loading" @click="brief = example">Örnek brief'i kullan</button>
      <span class="muted small">{{ brief.length }}/{{ MAX }}</span>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <button class="primary" :disabled="loading || !brief.trim()" data-test="submit" @click="emit('submit', brief.trim())">
      {{ loading ? 'Kitle çıkarılıyor…' : 'Kitleyi çıkar' }}
    </button>
  </section>
</template>
