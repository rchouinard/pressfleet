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
   * Writes series into the metrics store, one `name{labels} value` per
   * line. They count at once (they are stamped a minute ago), and stay the
   * series' latest value for minutes, until deleteSeries removes them.
   */
  async writeSeries(lines: string): Promise<void> {
    const res = await fetch(new URL('/metrics/series', this.url), { method: 'POST', body: lines })
    if (!res.ok) throw new Error(`harness: write series: status ${res.status}: ${await res.text()}`)
  }

  /** Deletes the series a selector (e.g. `{e2e="abc"}`) matches from the metrics store. */
  async deleteSeries(match: string): Promise<void> {
    const res = await fetch(new URL(`/metrics/delete?${new URLSearchParams({ match })}`, this.url), { method: 'POST' })
    if (!res.ok) throw new Error(`harness: delete series: status ${res.status}: ${await res.text()}`)
  }

  /** The webhook receiver's base URL, as the control plane reaches it; add any path. */
  async webhookURL(): Promise<string> {
    const res = await fetch(new URL('/webhook', this.url))
    if (!res.ok) throw new Error(`harness: webhook URL: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { url: string }).url
  }

  /** The requests the webhook receiver got on a path, oldest first. Header names are lower case. */
  async webhookRequests(path: string): Promise<WebhookRequest[]> {
    const res = await fetch(new URL(`/webhook/requests?${new URLSearchParams({ path })}`, this.url))
    if (!res.ok) throw new Error(`harness: webhook requests: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { requests: WebhookRequest[] }).requests
  }

  /** Requests a path of a site through Traefik, as a visitor would, and returns the status. */
  async visitSite(siteId: string, path: string): Promise<number> {
    const res = await fetch(new URL(`/sites/${encodeURIComponent(siteId)}/visit?${new URLSearchParams({ path })}`, this.url), { method: 'POST' })
    if (!res.ok) throw new Error(`harness: visit site: status ${res.status}: ${await res.text()}`)
    return ((await res.json()) as { status: number }).status
  }
}

/** A request the harness's webhook receiver got. */
export interface WebhookRequest {
  method: string
  headers: Record<string, string>
  body: string
}
