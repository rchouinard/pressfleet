import type { components } from '../api/schema.gen'

export type SiteImport = components['schemas']['SiteImport']

export const IMPORT_NAME_MAX = 100
const PHP_RE = /^[0-9]+\.[0-9]+$/

export interface ImportFormValues {
  client_group_id: string
  name: string
  php_version: string
}

/** Client-side checks for the form's own fields; the files are checked in utils/upload.ts. */
export function validateImport(v: ImportFormValues): Partial<Record<keyof ImportFormValues, string>> {
  const errors: Partial<Record<keyof ImportFormValues, string>> = {}
  if (!v.client_group_id) errors.client_group_id = 'Choose a client group.'
  if (v.name.trim().length > IMPORT_NAME_MAX) errors.name = `Use at most ${IMPORT_NAME_MAX} characters.`
  const php = v.php_version.trim()
  if (php !== '' && !PHP_RE.test(php)) errors.php_version = 'Use a version like 8.4.'
  return errors
}

/** The import body; optional fields left blank are omitted so the server's defaults apply. */
export function importPayload(v: ImportFormValues, archiveUploadId: string, dumpUploadId: string): SiteImport {
  const body: SiteImport = {
    client_group_id: v.client_group_id,
    archive_upload_id: archiveUploadId,
    dump_upload_id: dumpUploadId,
  }
  if (v.name.trim() !== '') body.name = v.name.trim()
  if (v.php_version.trim() !== '') body.php_version = v.php_version.trim()
  return body
}

/** The imported site's ID from a finished import job's result, if it has one. */
export function importedSiteId(result: Readonly<Record<string, unknown>> | null | undefined): string | undefined {
  return typeof result?.site_id === 'string' ? result.site_id : undefined
}
