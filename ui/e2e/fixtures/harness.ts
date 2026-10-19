// The Go harness's view of platform state the API doesn't show
// (control-plane/internal/e2e/dashboard/harness_test.go). Specs use it to
// check outcomes on the platform itself, e.g. that a deleted site left
// nothing behind; they never run the docker CLI.
export class Harness {
  constructor(private readonly url: string) {}

  /**
   * What is left of a site on Docker, the data directory, MariaDB, and the
   * object cache, one entry per object. Empty once the site is gone.
   */
  async siteLeftovers(siteId: string): Promise<string[]> {
    const res = await fetch(new URL(`/sites/${encodeURIComponent(siteId)}/leftovers`, this.url))
    if (!res.ok) throw new Error(`harness: site leftovers: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { leftovers: string[] }).leftovers
  }

  /** Creates a client group's backup repository, as `pressfleet repository create` does. */
  async createBackupRepository(clientGroupId: string): Promise<void> {
    const res = await fetch(new URL(`/client-groups/${encodeURIComponent(clientGroupId)}/backup-repository`, this.url), { method: 'POST' })
    if (!res.ok) throw new Error(`harness: create backup repository: status ${res.status}: ${await res.text()}`)
  }

  /** Whether a path under the platform's data directory exists. */
  async exists(path: string): Promise<boolean> {
    const res = await fetch(new URL(`/exists?${new URLSearchParams({ path })}`, this.url))
    if (!res.ok) throw new Error(`harness: exists: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { exists: boolean }).exists
  }
}
