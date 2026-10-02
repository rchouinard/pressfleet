/**
 * The port the SFTP gateway publishes unless `sftp.port` was changed
 * (docs/configuration.md). The API doesn't report the configured port, so the
 * dashboard shows the default and says where to look if it was changed.
 */
export const DEFAULT_SFTP_PORT = 2222

export interface SftpDetails {
  host: string
  port: number
  /** The SSH username names the site; the key names the user (design doc §5.8). */
  username: string
  command: string
  /** A URI for clients that take one (Cyberduck, FileZilla, ...). */
  uri: string
}

/**
 * A site's SFTP connection details. The gateway lives on the dashboard's
 * host, there are no passwords, and the client sees `wp-content` as `/`.
 */
export function sftpDetails(slug: string, host: string, port: number = DEFAULT_SFTP_PORT): SftpDetails {
  return {
    host,
    port,
    username: slug,
    command: `sftp -P ${port} ${slug}@${host}`,
    uri: `sftp://${slug}@${host}:${port}/`,
  }
}
