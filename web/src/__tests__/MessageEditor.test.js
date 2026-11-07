import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import MessageEditor from '../components/MessageEditor.vue'

const base = { sms_text: 'Merhaba {{first_name}}', email_subject: 'Konu', email_body: 'Gövde' }

describe('MessageEditor', () => {
  it('shows live character counters', () => {
    const w = mount(MessageEditor, { props: { content: base, dirty: true } })
    expect(w.get('[data-test="sms-counter"]').text()).toBe('22/160')
    expect(w.get('[data-test="subject-counter"]').text()).toBe('4/80')
  })

  it('disables saving and warns when the SMS exceeds 160 characters', async () => {
    const w = mount(MessageEditor, { props: { content: base, dirty: true } })
    await w.get('[data-test="sms"]').setValue('a'.repeat(161))
    expect(w.get('[data-test="sms-counter"]').classes()).toContain('over')
    expect(w.get('[data-test="save"]').attributes('disabled')).toBeDefined()
  })

  it('blocks unknown placeholders', async () => {
    const w = mount(MessageEditor, { props: { content: base, dirty: true } })
    await w.get('[data-test="sms"]').setValue('Selam {{phone}}')
    expect(w.text()).toContain('Tanımsız değişken')
    expect(w.get('[data-test="save"]').attributes('disabled')).toBeDefined()
  })

  it('emits the edited draft and allows saving valid copy', async () => {
    const w = mount(MessageEditor, { props: { content: base, dirty: true } })
    await w.get('[data-test="subject"]').setValue('Yeni konu')
    expect(w.emitted('update').at(-1)[0].email_subject).toBe('Yeni konu')
    expect(w.get('[data-test="save"]').attributes('disabled')).toBeUndefined()
    await w.get('[data-test="save"]').trigger('click')
    expect(w.emitted('save')).toHaveLength(1)
  })

  it('is read-only while streaming and after approval', () => {
    const w = mount(MessageEditor, { props: { content: base, streaming: true } })
    expect(w.get('[data-test="sms"]').attributes('readonly')).toBeDefined()
    expect(w.find('[data-test="save"]').exists()).toBe(false)
  })
})
