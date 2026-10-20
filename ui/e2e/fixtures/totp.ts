// An authenticator app for the specs: RFC 6238 codes (HMAC-SHA1, 6 digits,
// 30-second steps, the control plane's parameters) computed from the
// enrollment secret, as the user's phone would.
//
// The control plane accepts a code once and never one of an earlier step
// (control-plane/internal/auth/totp.go), so the app remembers the last step
// it gave out and gives the next one, from the step before the current one
// to the step after (the skew the server allows). Only when a spec has used
// all three does code() wait for the clock to reach the next step.
import { createHmac } from 'node:crypto'

const PERIOD_SECONDS = 30
const DIGITS = 6
const SKEW_STEPS = 1

/** Decodes RFC 4648 base32, as the enrollment's secret is written. */
function base32Decode(text: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = 0
  let value = 0
  const out: number[] = []
  for (const ch of text.replace(/=+$/, '').toUpperCase()) {
    const i = alphabet.indexOf(ch)
    if (i < 0) throw new Error(`not base32: ${ch}`)
    value = (value << 5) | i
    bits += 5
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }
  return Buffer.from(out)
}

/** The HOTP code (RFC 4226) of a counter. */
function hotp(key: Buffer, counter: number): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(counter))
  const mac = createHmac('sha1', key).update(msg).digest()
  const offset = mac[mac.length - 1]! & 0x0f
  const n = (mac.readUInt32BE(offset) & 0x7fffffff) % 10 ** DIGITS
  return n.toString().padStart(DIGITS, '0')
}

function currentStep(): number {
  return Math.floor(Date.now() / 1000 / PERIOD_SECONDS)
}

/** An authenticator app holding one secret. */
export class TotpApp {
  private readonly key: Buffer
  private lastStep = 0

  constructor(readonly secret: string) {
    this.key = base32Decode(secret)
  }

  /** A code the server hasn't seen yet. */
  async code(): Promise<string> {
    let step = Math.max(this.lastStep + 1, currentStep())
    if (step > currentStep() + SKEW_STEPS) {
      // Every step the server would accept now is used: wait for the clock.
      const ready = (step - SKEW_STEPS) * PERIOD_SECONDS * 1000
      await new Promise(resolve => setTimeout(resolve, ready - Date.now() + 50))
      step = Math.max(this.lastStep + 1, currentStep())
    }
    this.lastStep = step
    return hotp(this.key, step)
  }

  /** Six digits that are no code the server accepts now. */
  wrongCode(): string {
    const valid = new Set<string>()
    for (let s = currentStep() - SKEW_STEPS - 1; s <= currentStep() + SKEW_STEPS + 1; s++) valid.add(hotp(this.key, s))
    for (let n = 0; ; n++) {
      const code = n.toString().padStart(DIGITS, '0')
      if (!valid.has(code)) return code
    }
  }
}
