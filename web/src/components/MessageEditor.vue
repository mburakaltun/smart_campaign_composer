<script setup>
import { computed, reactive, watch } from 'vue'
import { SMS_MAX, SUBJECT_MAX, PLACEHOLDERS, counterState, unknownPlaceholders } from '../limits.js'

const props = defineProps({
  content: { type: Object, required: true }, // {sms_text, email_subject, email_body}
  streaming: Boolean,
  readonly: Boolean,
  saving: Boolean,
  dirty: Boolean,
  problems: { type: Array, default: () => [] },
  samples: { type: Array, default: () => [] },
})
const emit = defineEmits(['update', 'save'])

const form = reactive({ sms_text: '', email_subject: '', email_body: '' })
watch(
  () => props.content,
  (c) => Object.assign(form, { sms_text: c.sms_text || '', email_subject: c.email_subject || '', email_body: c.email_body || '' }),
  { immediate: true, deep: true },
)

const sms = computed(() => counterState(form.sms_text, SMS_MAX))
const subject = computed(() => counterState(form.email_subject, SUBJECT_MAX))
const unknown = computed(() => [
  ...new Set([...unknownPlaceholders(form.sms_text), ...unknownPlaceholders(form.email_subject), ...unknownPlaceholders(form.email_body)]),
])
const invalid = computed(() => sms.value.over || subject.value.over || unknown.value.length > 0 || !form.sms_text || !form.email_subject || !form.email_body)

const ph = (name) => '{{' + name + '}}'

function change() {
  emit('update', { ...form })
}
function insert(field, name) {
  form[field] += ph(name)
  change()
}
</script>

<template>
  <section class="card">
    <h2>3. Mesajlar</h2>
    <p v-if="streaming" class="muted live">Claude yazıyor…</p>
    <div class="cols">
      <div class="col">
        <label for="sms">SMS</label>
        <textarea id="sms" v-model="form.sms_text" rows="4" :readonly="readonly || streaming" data-test="sms" @input="change" />
        <div class="row">
          <span class="chips" v-if="!readonly && !streaming">
            <button v-for="p in PLACEHOLDERS" :key="p" class="chip" type="button" @click="insert('sms_text', p)">{{ ph(p) }}</button>
          </span>
          <span :class="['counter', { over: sms.over, near: sms.near }]" data-test="sms-counter">{{ sms.n }}/{{ SMS_MAX }}</span>
        </div>
      </div>
      <div class="col">
        <label for="subject">Email konusu</label>
        <input id="subject" v-model="form.email_subject" :readonly="readonly || streaming" data-test="subject" @input="change" />
        <div class="row">
          <span class="chips" v-if="!readonly && !streaming">
            <button v-for="p in PLACEHOLDERS" :key="p" class="chip" type="button" @click="insert('email_subject', p)">{{ ph(p) }}</button>
          </span>
          <span :class="['counter', { over: subject.over, near: subject.near }]" data-test="subject-counter">{{ subject.n }}/{{ SUBJECT_MAX }}</span>
        </div>
        <label for="body">Email gövdesi</label>
        <textarea id="body" v-model="form.email_body" rows="8" :readonly="readonly || streaming" data-test="body" @input="change" />
      </div>
    </div>

    <p v-if="unknown.length" class="error">Tanımsız değişken: {{ unknown.map(ph).join(', ') }}. İzinli: {{ PLACEHOLDERS.join(', ') }}.</p>
    <ul v-if="problems.length" class="error problems">
      <li v-for="p in problems" :key="p">{{ p }}</li>
    </ul>

    <div v-if="!readonly && !streaming" class="row">
      <button class="primary" :disabled="invalid || saving || !dirty" data-test="save" @click="emit('save')">
        {{ saving ? 'Kaydediliyor…' : dirty ? 'Düzenlemeyi kaydet' : 'Kaydedildi' }}
      </button>
      <span class="muted small">Kişiselleştirme sonrası uzunluk, kitledeki en uzun isimle sunucuda doğrulanır.</span>
    </div>

    <div v-if="samples.length && !streaming" class="preview">
      <h3>Önizleme (gerçek alıcılarla)</h3>
      <div v-for="(s, i) in samples" :key="i" class="sample">
        <div class="sms-bubble">{{ s.sms }}</div>
        <div class="email-preview">
          <b>{{ s.subject }}</b>
          <pre>{{ s.body }}</pre>
        </div>
      </div>
    </div>
  </section>
</template>
