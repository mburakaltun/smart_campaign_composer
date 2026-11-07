<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from './api.js'
import BriefForm from './components/BriefForm.vue'
import AudienceCard from './components/AudienceCard.vue'
import MessageEditor from './components/MessageEditor.vue'
import SendDashboard from './components/SendDashboard.vue'

const campaign = ref(null)
const loading = ref(false)
const error = ref('')
const composing = ref(false)
const live = ref({ sms_text: '', email_subject: '', email_body: '' })
const retryNote = ref([])
const draft = ref({ sms_text: '', email_subject: '', email_body: '' })
const saving = ref(false)
const problems = ref([])
const confirming = ref(false)
const approving = ref(false)
let abort = null

const status = computed(() => campaign.value?.status)
const editable = computed(() => status.value === 'segment_ready' || status.value === 'content_ready')
const hasContent = computed(() => !!campaign.value?.content?.sms_text)
const shownContent = computed(() => (composing.value ? live.value : editable.value ? draft.value : campaign.value?.content || draft.value))
const dirty = computed(() => {
  const c = campaign.value?.content
  return !!c && (c.sms_text !== draft.value.sms_text || c.email_subject !== draft.value.email_subject || c.email_body !== draft.value.email_body)
})
const sending = computed(() => status.value === 'sending' || status.value === 'completed')

function setCampaign(c) {
  campaign.value = c
  draft.value = { ...c.content }
  location.hash = `#/c/${c.id}`
}

async function load(id) {
  loading.value = true
  try {
    setCampaign(await api.getCampaign(id))
  } catch {
    location.hash = ''
    campaign.value = null
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  const m = location.hash.match(/^#\/c\/([0-9a-f-]{36})$/i)
  if (m) load(m[1])
})
onBeforeUnmount(() => abort?.abort())

async function createCampaign(brief) {
  loading.value = true
  error.value = ''
  try {
    setCampaign(await api.createCampaign(brief))
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function compose() {
  composing.value = true
  error.value = ''
  problems.value = []
  retryNote.value = []
  live.value = { sms_text: '', email_subject: '', email_body: '' }
  abort = new AbortController()
  try {
    await api.compose(
      campaign.value.id,
      (name, data) => {
        if (name === 'partial') live.value = { ...live.value, ...data.fields }
        else if (name === 'retry') {
          retryNote.value = data.problems
          live.value = { sms_text: '', email_subject: '', email_body: '' }
        } else if (name === 'final') {
          setCampaign(data.campaign)
          retryNote.value = data.attempts > 1 ? ['İlk taslak kurallara uymadığı için yapay zeka metni yeniden yazdı.'] : []
        } else if (name === 'error') error.value = data.message
      },
      abort.signal,
    )
  } catch (e) {
    if (e.name !== 'AbortError') error.value = e.message
  } finally {
    composing.value = false
  }
}

async function save() {
  saving.value = true
  problems.value = []
  error.value = ''
  try {
    setCampaign(await api.saveContent(campaign.value.id, draft.value))
  } catch (e) {
    error.value = e.message
    problems.value = e.problems || []
  } finally {
    saving.value = false
  }
}

async function approve() {
  approving.value = true
  error.value = ''
  try {
    await api.approve(campaign.value.id)
    setCampaign(await api.getCampaign(campaign.value.id))
    confirming.value = false
  } catch (e) {
    error.value = e.message
    problems.value = e.problems || []
  } finally {
    approving.value = false
  }
}

function reset() {
  abort?.abort()
  campaign.value = null
  location.hash = ''
  error.value = ''
  problems.value = []
  confirming.value = false
}
const fmt = (n) => new Intl.NumberFormat('tr-TR').format(n)
</script>

<template>
  <main>
    <header>
      <h1>Smart Campaign Composer</h1>
      <button v-if="campaign" class="link" @click="reset">+ Yeni kampanya</button>
    </header>

    <BriefForm v-if="!campaign" :loading="loading" :error="error" @submit="createCampaign" />

    <template v-else>
      <section class="card brief">
        <span class="muted small">Brief</span>
        <p>{{ campaign.brief }}</p>
      </section>

      <AudienceCard :campaign="campaign" />

      <section v-if="status === 'segment_ready' && !composing" class="card">
        <h2>3. Mesajlar</h2>
        <p class="muted">Claude, SMS ve email metnini yazarken kanal limitlerine ve kitleye uyacak.</p>
        <button class="primary" :disabled="campaign.audience.total === 0" data-test="compose" @click="compose">Mesajları üret</button>
        <p v-if="error" class="error" role="alert">{{ error }}</p>
      </section>

      <template v-if="composing || hasContent">
        <p v-if="retryNote.length && composing" class="notice" role="status">
          Taslak kurallara uymadı, yeniden yazılıyor:
          <span v-for="p in retryNote" :key="p"><br />• {{ p }}</span>
        </p>
        <p v-else-if="retryNote.length" class="notice" role="status">{{ retryNote[0] }}</p>
        <MessageEditor
          :content="shownContent"
          :streaming="composing"
          :readonly="!editable"
          :saving="saving"
          :dirty="dirty"
          :problems="problems"
          :samples="campaign.samples || []"
          @update="draft = $event"
          @save="save"
        />
        <p v-if="error && (composing || hasContent)" class="error" role="alert">{{ error }}</p>
      </template>

      <section v-if="status === 'content_ready' && !composing" class="card">
        <h2>4. Onay</h2>
        <p v-if="dirty" class="muted">Önce düzenlemeni kaydet, sonra onaylayabilirsin.</p>
        <template v-else-if="!confirming">
          <p>
            Onaylarsan <b>{{ fmt(campaign.audience.sms) }}</b> SMS ve <b>{{ fmt(campaign.audience.email) }}</b> email kuyruğa alınır. Onaydan sonra metin değiştirilemez.
          </p>
          <div class="row">
            <button class="primary" data-test="approve" @click="confirming = true">Onayla ve gönder</button>
            <button class="secondary" :disabled="composing" @click="compose">Yeniden üret</button>
          </div>
        </template>
        <template v-else>
          <p><b>Emin misin?</b> Bu işlem geri alınamaz.</p>
          <div class="row">
            <button class="primary danger" :disabled="approving" data-test="confirm" @click="approve">{{ approving ? 'Kuyruğa alınıyor…' : 'Evet, gönder' }}</button>
            <button class="secondary" :disabled="approving" @click="confirming = false">Vazgeç</button>
          </div>
        </template>
      </section>

      <SendDashboard v-if="sending" :key="campaign.id" :campaign-id="campaign.id" />
    </template>
  </main>
</template>
