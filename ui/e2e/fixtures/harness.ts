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

  /**
   * Runs a command in a site's php container, as the site's user, and
   * returns its output, trimmed.
   */
  async siteExec(siteId: string, command: string[]): Promise<string> {
    const res = await fetch(new URL(`/sites/${encodeURIComponent(siteId)}/exec`, this.url), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command }),
    })
    if (!res.ok) throw new Error(`harness: ${command.join(' ')}: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { output: string }).output.trim()
  }

  /** Runs WP-CLI on a site and returns its output, trimmed. */
  wp(siteId: string, ...args: string[]): Promise<string> {
    return this.siteExec(siteId, ['wp', ...args])
  }

  /**
   * Fetches a page of a site from its nginx, on the site's network, as a
   * visitor of its preview domain would over HTTPS, and returns the body.
   */
  async sitePage(siteId: string, previewDomain: string, path = '/'): Promise<string> {
    // A query string of its own, so no page cache answers.
    const url = `http://nginx:8080${path}${path.includes('?') ? '&' : '?'}pf-e2e=${Date.now()}`
    return this.siteExec(siteId, ['curl', '--silent', '--show-error', '--fail', '--max-time', '10',
      '--header', `Host: ${previewDomain}`, '--header', 'X-Forwarded-Proto: https', url])
  }

  /** Runs SQL on the platform MariaDB as root; the output has one line per row, tab-separated. */
  async mariadb(query: string): Promise<string> {
    const res = await fetch(new URL('/mariadb/query', this.url), { method: 'POST', body: query })
    if (!res.ok) throw new Error(`harness: MariaDB query: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { output: string }).output.trim()
  }
}
