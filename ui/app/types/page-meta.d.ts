import type { Action } from '../utils/permissions'

declare module '#app' {
  interface PageMeta {
    /** Opts a page out of the sign-in requirement (the login page). */
    public?: boolean
    /** The action a user needs to open the page; roles below it get a 403 page. */
    requires?: Action
  }
}

export {}
