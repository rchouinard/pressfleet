import type { components } from '../api/schema.gen'

/** The running control plane's build, from `GET /api/v1/platform`. */
export type PlatformBuild = components['schemas']['PlatformBuild']

/**
 * The commit shortened to 12 characters, as `pressfleet version` prints it,
 * with `-dirty` for a build from a modified tree. Empty when unknown.
 */
export function shortCommit(build: PlatformBuild): string {
  if (!build.commit) return ''
  return build.commit.slice(0, 12) + (build.modified ? '-dirty' : '')
}

/**
 * The build on one line, in the format of `pressfleet version`, for pasting
 * into a bug report.
 */
export function buildSummary(build: PlatformBuild): string {
  const commit = shortCommit(build)
  return `pressfleet ${build.version} (${commit ? `commit ${commit}, ` : ''}${build.go_version}, ${build.os}/${build.arch})`
}
