/**
 * An incremental parser for `text/event-stream` bodies (the WHATWG
 * "Server-sent events" format), for reading the log stream with `fetch`.
 * `EventSource` can't expose HTTP status codes or Problem Details, and a
 * fetch-based reader lets the viewer decide when to reconnect.
 *
 * Feed it decoded text in whatever chunks arrive; events are delivered as
 * soon as their terminating blank line is seen. Comment lines
 * (`: keepalive`) produce no event but are reported through `onActivity`,
 * so a caller can tell an idle connection from a dead one.
 */
export interface SseEvent {
  /** The event type; `message` when the stream doesn't name one. */
  event: string
  data: string
  /** The last event ID the stream set, which persists across events. */
  id: string
}

export interface SseParserOptions {
  onEvent: (event: SseEvent) => void
  /** Called for every complete line, comments included. */
  onActivity?: () => void
  /**
   * Longest line (in UTF-16 units) the parser will hold. A longer line is
   * discarded rather than buffered without limit; a server bug or a hostile
   * log line must not exhaust the browser's memory.
   */
  maxLineLength?: number
}

export const defaultMaxLineLength = 1 << 20

export interface SseParser {
  feed: (chunk: string) => void
  /** Number of lines dropped for being longer than the limit. */
  readonly oversized: number
}

export function createSseParser(options: SseParserOptions): SseParser {
  const max = options.maxLineLength ?? defaultMaxLineLength
  let pending = ''
  let skipping = false // inside an oversized line, discarding until its end
  let lastWasCR = false
  let first = true
  let oversized = 0

  let data: string[] = []
  let event = ''
  let id = ''

  function dispatch() {
    if (data.length > 0) {
      options.onEvent({ event: event || 'message', data: data.join('\n'), id })
    }
    data = []
    event = ''
  }

  function line(text: string) {
    options.onActivity?.()
    if (text === '') {
      dispatch()
      return
    }
    if (text.startsWith(':')) return
    const colon = text.indexOf(':')
    const field = colon === -1 ? text : text.slice(0, colon)
    let value = colon === -1 ? '' : text.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        event = value
        break
      case 'data':
        data.push(value)
        break
      case 'id':
        // A NUL in the ID makes the field invalid.
        if (!value.includes('\0')) id = value
        break
      default:
        // `retry` and unknown fields are ignored: reconnecting is the caller's policy.
        break
    }
  }

  return {
    feed(chunk) {
      if (first) {
        first = false
        if (chunk.startsWith('﻿')) chunk = chunk.slice(1)
      }
      let start = 0
      for (let i = 0; i < chunk.length; i++) {
        const c = chunk[i]
        if (c !== '\n' && c !== '\r') continue
        // A "\n" right after a "\r" (possibly in the previous chunk) is the second half of CRLF.
        if (c === '\n' && lastWasCR && i === start) {
          lastWasCR = false
          start = i + 1
          continue
        }
        lastWasCR = c === '\r'
        if (skipping) {
          skipping = false
        } else {
          line(pending + chunk.slice(start, i))
        }
        pending = ''
        start = i + 1
      }
      const rest = chunk.slice(start)
      if (rest !== '') lastWasCR = false
      if (!skipping) {
        pending += rest
        if (pending.length > max) {
          pending = ''
          skipping = true
          oversized++
        }
      }
    },
    get oversized() {
      return oversized
    },
  }
}
