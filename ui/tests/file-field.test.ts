import { describe, expect, it } from 'vitest'
import { computed, defineComponent, h, provide, ref } from 'vue'
import { mount } from '@vue/test-utils'
import { formFieldInjectionKey, inputIdInjectionKey } from '@nuxt/ui/composables/useFormField'
import FileField from '../app/components/FileField.vue'

// UFormField as far as labelling goes: it provides its input ID and field
// context, and points its <label> at that ID, as the real one does.
const UFormField = defineComponent({
  props: { label: String, description: String, error: String },
  setup(props, { slots }) {
    const id = ref('field-1')
    provide(inputIdInjectionKey, id)
    provide(formFieldInjectionKey, computed(() => ({ description: props.description, error: props.error, ariaId: 'field-1' })))
    return () => h('div', [h('label', { for: id.value }, props.label), slots.default?.()])
  },
})

function mountField(props: Record<string, unknown> = {}) {
  return mount(FileField, {
    props: { label: 'Package', ...props },
    global: { stubs: { UFormField } },
  })
}

describe('FileField', () => {
  it('is the input its label points at', () => {
    const w = mountField()
    const input = w.get('input[type="file"]')
    expect(input.attributes('id')).toBe(w.get('label').attributes('for'))
    expect(input.attributes('id')).toBeTruthy()
  })

  it('is described by its hint and error', () => {
    const input = mountField({ hint: 'A .zip', error: 'Choose a file' }).get('input[type="file"]')
    expect(input.attributes('aria-describedby')).toBe('field-1-error field-1-description')
    expect(input.attributes('aria-invalid')).toBe('true')
  })

  it('emits the chosen file, and clears it', async () => {
    const w = mountField({ accept: '.zip' })
    const input = w.get('input[type="file"]')
    expect(input.attributes('accept')).toBe('.zip')
    const file = new File(['x'], 'theme.zip')
    Object.defineProperty(input.element, 'files', { value: [file] })
    await input.trigger('change')
    expect(w.emitted('update:file')).toEqual([[file]])
    ;(w.vm as unknown as { clear: () => void }).clear()
    expect(w.emitted('update:file')?.[1]).toEqual([undefined])
  })
})
