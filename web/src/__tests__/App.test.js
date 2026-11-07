import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('../api.js', () => ({
  api: { createCampaign: vi.fn(), compose: vi.fn(), saveContent: vi.fn(), approve: vi.fn(), getCampaign: vi.fn(), watch: vi.fn() },
}))
import { api } from '../api.js'
import App from '../App.vue'

const base = {
  id: '11111111-1111-1111-1111-111111111111',
  brief: 'Son 30 günde sepetine ürün ekleyen İstanbul kullanıcıları',
  status: 'segment_ready',
  segment_rule: { op: 'and', children: [{ field: 'city', operator: 'eq', value: 'İstanbul' }] },
  segment_explanation: 'İstanbul’daki terk edilmiş sepetler',
  audience: { total: 812, sms: 590, email: 700 },
  content: { sms_text: '', email_subject: '', email_body: '' },
}
const withContent = {
  ...base,
  status: 'content_ready',
  content: { sms_text: 'Merhaba {{first_name}}, %30 indirim!', email_subject: '{{first_name}}, sepetin seni bekliyor', email_body: 'Gövde' },
  samples: [{ first_name: 'Ayşe', sms: 'Merhaba Ayşe, %30 indirim!', subject: 'Ayşe, sepetin seni bekliyor', body: 'Gövde' }],
}

beforeEach(() => {
  vi.clearAllMocks()
  location.hash = ''
})

describe('App workflow', () => {
  it('brief → audience → streamed messages → approve → live dashboard', async () => {
    api.createCampaign.mockResolvedValue(base)
    api.compose.mockImplementation(async (_id, onEvent) => {
      onEvent('partial', { fields: { sms_text: 'Merha' } })
      onEvent('partial', { fields: { sms_text: 'Merhaba {{first_name}}, %30 indirim!' } })
      onEvent('final', { campaign: withContent, attempts: 1 })
    })
    api.approve.mockResolvedValue({ status: 'sending', queued: 1290 })
    api.getCampaign.mockResolvedValue({ ...withContent, status: 'sending' })
    api.watch.mockImplementation(async (_id, onEvent) => {
      onEvent('stats', { status: 'sending', stats: { sms: { sent: 40, pending: 550 }, email: { sent: 10, pending: 690 } } })
      await new Promise(() => {}) // keep the stream open
    })

    const w = mount(App)
    await w.get('[data-test="brief"]').setValue(base.brief)
    await w.get('[data-test="submit"]').trigger('click')
    await flushPromises()
    expect(api.createCampaign).toHaveBeenCalledWith(base.brief)
    expect(w.get('[data-test="aud-total"]').text()).toMatch(/812/)

    await w.get('[data-test="compose"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-test="sms"]').element.value).toContain('%30 indirim')
    expect(w.text()).toContain('Merhaba Ayşe, %30 indirim!') // rendered preview

    // approval needs an explicit second confirmation
    await w.get('[data-test="approve"]').trigger('click')
    expect(api.approve).not.toHaveBeenCalled()
    await w.get('[data-test="confirm"]').trigger('click')
    await flushPromises()
    expect(api.approve).toHaveBeenCalledTimes(1)
    expect(w.get('[data-test="sms-sent"]').text()).toBe('40')
    expect(w.get('[data-test="status"]').text()).toBe('Gönderiliyor')
  })

  it('shows server-side validation problems when saving an edit fails', async () => {
    api.getCampaign.mockResolvedValue(withContent)
    location.hash = `#/c/${base.id}`
    const w = mount(App)
    await flushPromises()
    await w.get('[data-test="sms"]').setValue('Yeni metin {{first_name}}')
    const err = Object.assign(new Error('content does not meet the channel rules'), { problems: ['sms_text is 171 characters after personalisation, limit is 160'] })
    api.saveContent.mockRejectedValue(err)
    await w.get('[data-test="save"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('171 characters after personalisation')
  })

  it('surfaces an AI failure from the stream and lets the user retry', async () => {
    api.createCampaign.mockResolvedValue(base)
    api.compose.mockImplementation(async (_id, onEvent) => onEvent('error', { message: 'The AI could not produce a valid result' }))
    const w = mount(App)
    await w.get('[data-test="brief"]').setValue('x')
    await w.get('[data-test="submit"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="compose"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('The AI could not produce a valid result')
    expect(w.find('[data-test="compose"]').exists()).toBe(true)
  })

  it('restores a campaign from the URL hash after a refresh', async () => {
    api.getCampaign.mockResolvedValue({ ...withContent, status: 'completed' })
    api.watch.mockImplementation(async (_id, onEvent) => onEvent('stats', { status: 'completed', stats: { sms: { sent: 5 }, email: {} } }))
    location.hash = `#/c/${base.id}`
    const w = mount(App)
    await flushPromises()
    expect(w.get('[data-test="status"]').text()).toBe('Tamamlandı')
    expect(w.find('[data-test="save"]').exists()).toBe(false) // read-only after approval
  })
})
