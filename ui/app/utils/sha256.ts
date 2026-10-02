// An incremental SHA-256. Web Crypto only hashes a whole buffer at once, and
// an import archive can be many gigabytes, so the upload hashes the file one
// slice at a time with this instead (the API wants the checksum of the whole
// file). It is a straight FIPS 180-4 implementation, checked against Node's
// crypto in tests/sha256.test.ts.

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])

export class Sha256 {
  private h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ])

  private buf = new Uint8Array(64)
  private bufLen = 0
  private total = 0
  private w = new Uint32Array(64)

  /** Feeds more bytes; may be called any number of times. */
  update(data: Uint8Array): this {
    this.total += data.length
    let i = 0
    if (this.bufLen > 0) {
      const take = Math.min(64 - this.bufLen, data.length)
      this.buf.set(data.subarray(0, take), this.bufLen)
      this.bufLen += take
      i = take
      if (this.bufLen < 64) return this
      this.block(this.buf, 0)
      this.bufLen = 0
    }
    for (; i + 64 <= data.length; i += 64) this.block(data, i)
    if (i < data.length) {
      this.buf.set(data.subarray(i), 0)
      this.bufLen = data.length - i
    }
    return this
  }

  /** The digest as lowercase hex. The hasher can't be used afterwards. */
  digest(): string {
    const bits = this.total * 8
    const pad = new Uint8Array((this.bufLen < 56 ? 56 : 120) - this.bufLen + 8)
    pad[0] = 0x80
    const view = new DataView(pad.buffer)
    // total is below 2^53, so the high word is exact.
    view.setUint32(pad.length - 8, Math.floor(bits / 0x100000000))
    view.setUint32(pad.length - 4, bits >>> 0)
    this.update(pad)
    return Array.from(this.h, v => v.toString(16).padStart(8, '0')).join('')
  }

  private block(data: Uint8Array, offset: number) {
    const w = this.w
    const H = this.h
    for (let t = 0; t < 16; t++) {
      const j = offset + t * 4
      w[t] = ((data[j]! << 24) | (data[j + 1]! << 16) | (data[j + 2]! << 8) | data[j + 3]!) >>> 0
    }
    for (let t = 16; t < 64; t++) {
      const x = w[t - 15]!
      const y = w[t - 2]!
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3)
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10)
      w[t] = (w[t - 16]! + s0 + w[t - 7]! + s1) >>> 0
    }
    let a = H[0]!
    let b = H[1]!
    let c = H[2]!
    let d = H[3]!
    let e = H[4]!
    let f = H[5]!
    let g = H[6]!
    let h = H[7]!
    for (let t = 0; t < 64; t++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7))
      const ch = (e & f) ^ (~e & g)
      const t1 = (h + S1 + ch + K[t]! + w[t]!) >>> 0
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10))
      const maj = (a & b) ^ (a & c) ^ (b & c)
      const t2 = (S0 + maj) >>> 0
      h = g
      g = f
      f = e
      e = (d + t1) >>> 0
      d = c
      c = b
      b = a
      a = (t1 + t2) >>> 0
    }
    H[0] = (H[0]! + a) >>> 0
    H[1] = (H[1]! + b) >>> 0
    H[2] = (H[2]! + c) >>> 0
    H[3] = (H[3]! + d) >>> 0
    H[4] = (H[4]! + e) >>> 0
    H[5] = (H[5]! + f) >>> 0
    H[6] = (H[6]! + g) >>> 0
    H[7] = (H[7]! + h) >>> 0
  }
}

/** Anything with `size` and `slice(...).arrayBuffer()`, i.e. a File or Blob. */
export interface Sliceable {
  readonly size: number
  slice(start: number, end: number): { arrayBuffer(): Promise<ArrayBuffer> }
}

/**
 * SHA-256 of a file in lowercase hex, read in 4 MiB slices so memory stays
 * flat. `onProgress` gets the bytes hashed so far; an aborted signal rejects.
 */
export async function sha256File(
  file: Sliceable,
  onProgress?: (done: number, total: number) => void,
  signal?: AbortSignal,
  sliceSize = 4 * 1024 * 1024,
): Promise<string> {
  const hasher = new Sha256()
  for (let pos = 0; pos < file.size; pos += sliceSize) {
    if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')
    const end = Math.min(pos + sliceSize, file.size)
    const buf = await file.slice(pos, end).arrayBuffer()
    hasher.update(new Uint8Array(buf))
    onProgress?.(end, file.size)
  }
  return hasher.digest()
}
