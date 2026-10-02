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
export function sftpDetails(slug: string, host: string, port: number): SftpDetails {
  return {
    host,
    port,
    username: slug,
    command: `sftp -P ${port} ${slug}@${host}`,
    uri: `sftp://${slug}@${host}:${port}/`,
  }
}
