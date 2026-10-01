import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'

describe('ProblemAlert', () => {
  it('shows the problem, field errors, and request ID in an alert region', () => {
    const error = new ApiError({
      type: 'about:blank',
      title: 'Unprocessable Content',
      status: 422,
      detail: 'The request failed validation.',
      request_id: 'req-9',
      errors: [{ field: '/name', detail: 'is required' }],
    })

    const wrapper = mount(ProblemAlert, { props: { error } })

    expect(wrapper.attributes('role')).toBe('alert')
    expect(wrapper.text()).toContain('Unprocessable Content')
    expect(wrapper.text()).toContain('The request failed validation.')
    expect(wrapper.text()).toContain('/name')
    expect(wrapper.text()).toContain('is required')
    expect(wrapper.text()).toContain('Request ID: req-9')
  })

  it('omits optional parts that are absent', () => {
    const wrapper = mount(ProblemAlert, {
      props: { error: new ApiError({ type: 'about:blank', title: 'Not Found', status: 404 }) },
    })

    expect(wrapper.find('ul').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Request ID')
  })
})
