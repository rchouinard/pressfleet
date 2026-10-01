export interface Crumb {
  label: string
  /** Omit on the current page. */
  to?: string
}

/**
 * Sets the breadcrumb trail the app shell shows above the page. Call it in a
 * page's setup; the trail resets on every navigation, so a page without a
 * call has none. Give the current page last, without `to`.
 *
 *     useBreadcrumbs([{ label: 'Jobs', to: '/jobs' }, { label: id }])
 */
export function useBreadcrumbs(crumbs: MaybeRefOrGetter<Crumb[]>) {
  const state = useState<Crumb[]>('breadcrumbs', () => [])
  watchEffect(() => {
    state.value = toValue(crumbs)
  })
}
