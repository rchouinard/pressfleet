// Small tar.gz archives: deploy packages built in memory, and the archives
// the platform hands out (site exports) read back for checking. Regular
// files only, which is all either needs; no dependency for it.
import { gunzipSync, gzipSync } from 'node:zlib'

const BLOCK = 512

function octal(value: number, width: number): string {
  return value.toString(8).padStart(width - 1, '0') + '\0'
}

function header(name: string, size: number): Buffer {
  if (Buffer.byteLength(name) > 100) throw new Error(`tar: name too long: ${name}`)
  const h = Buffer.alloc(BLOCK)
  h.write(name, 0)
  h.write(octal(0o644, 8), 100)
  h.write(octal(0, 8), 108) // uid
  h.write(octal(0, 8), 116) // gid
  h.write(octal(size, 12), 124)
  h.write(octal(Math.floor(Date.now() / 1000), 12), 136)
  h.write(' '.repeat(8), 148) // checksum, counted as spaces
  h.write('0', 156) // regular file
  h.write('ustar\0' + '00', 257)
  let sum = 0
  for (const b of h) sum += b
  h.write(sum.toString(8).padStart(6, '0') + '\0 ', 148)
  return h
}

/** A gzipped tar of the files, by path. */
export function tarGz(files: Record<string, string>): Buffer {
  const parts: Buffer[] = []
  for (const [name, content] of Object.entries(files)) {
    const data = Buffer.from(content)
    parts.push(header(name, data.length), data, Buffer.alloc((BLOCK - (data.length % BLOCK)) % BLOCK))
  }
  parts.push(Buffer.alloc(2 * BLOCK))
  return gzipSync(Buffer.concat(parts))
}

function field(h: Buffer, offset: number, length: number): string {
  const s = h.subarray(offset, offset + length).toString()
  const nul = s.indexOf('\0')
  return nul < 0 ? s : s.slice(0, nul)
}

/**
 * The regular files of a gzipped tar, by path, with their contents as
 * text. Reads ustar names with a prefix, PAX paths, and GNU long names,
 * whichever the writer used.
 */
export function untarGz(archive: Buffer): Map<string, string> {
  const tar = gunzipSync(archive)
  const files = new Map<string, string>()
  let longName: string | undefined
  for (let off = 0; off + BLOCK <= tar.length;) {
    const h = tar.subarray(off, off + BLOCK)
    if (h.every(b => b === 0)) break
    const size = parseInt(field(h, 124, 12).trim() || '0', 8)
    const type = field(h, 156, 1)
    const data = tar.subarray(off + BLOCK, off + BLOCK + size)
    off += BLOCK + Math.ceil(size / BLOCK) * BLOCK

    if (type === 'x') {
      longName = data.toString().match(/^\d+ path=(.*)$/m)?.[1]
      continue
    }
    if (type === 'L') {
      longName = field(data, 0, data.length)
      continue
    }
    const prefix = field(h, 345, 155)
    const name = longName ?? (prefix ? `${prefix}/${field(h, 0, 100)}` : field(h, 0, 100))
    longName = undefined
    if (type === '0' || type === '') files.set(name.replace(/^\.\//, ''), data.toString())
  }
  return files
}
