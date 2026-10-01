import { h } from 'vue'

// Minimal stand-ins for the Nuxt UI components the form and table components
// use, so they mount under Vitest without the Nuxt runtime. They keep the
// behaviour the tests rely on (v-model, click, slots) and nothing else.
export const uiStubs = {
  UButton: {
    inheritAttrs: true,
    props: ['icon', 'loading', 'disabled', 'type'],
    setup(props: { disabled?: boolean, type?: string }, { slots }: { slots: { default?: () => unknown } }) {
      return () => h('button', { type: props.type ?? 'button', disabled: props.disabled }, slots.default?.() as never)
    },
  },
  UBadge: { template: '<span><slot /></span>' },
  UIcon: { template: '<i />' },
  UFormField: {
    props: ['label', 'error', 'description'],
    template: '<div><label>{{ label }}</label><slot /><p v-if="error" role="alert">{{ error }}</p></div>',
  },
  UInput: {
    props: ['modelValue', 'type'],
    emits: ['update:modelValue'],
    template: '<div><input :type="type" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" /><slot name="trailing" /></div>',
  },
  USelect: {
    props: ['modelValue', 'items', 'disabled'],
    emits: ['update:modelValue'],
    template: '<select :disabled="disabled" :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="i in items" :key="i.value" :value="i.value">{{ i.label }}</option></select>',
  },
  URadioGroup: {
    props: ['modelValue', 'items'],
    emits: ['update:modelValue'],
    template: '<div><label v-for="i in items" :key="i.value"><input type="radio" :value="i.value" :checked="modelValue === i.value" @change="$emit(\'update:modelValue\', i.value)" />{{ i.label }}</label></div>',
  },
  UCheckbox: {
    props: ['modelValue', 'label', 'disabled'],
    emits: ['update:modelValue'],
    template: '<label><input type="checkbox" :checked="modelValue" :disabled="disabled" @change="$emit(\'update:modelValue\', $event.target.checked)" />{{ label }}</label>',
  },
}
