import { describe, expect, it } from 'vitest'
import { createSseParser, type SseEvent } from '../app/utils/sse'

function parse(chunks: string[], maxLineLength?: number) {
  const events: SseEvent[] = []
  let activity = 0
  const p = createSseParser({ onEvent: e => events.push(e), onActivity: () => activity++, maxLineLength })
  for (const c of chunks) p.feed(c)
  return { events, activity: () => activity, parser: p }
}

describe('sse parser', () => {
  it('parses a log event with its id', () => {
    const { events } = parse(['id: 1790856000123456789\nevent: log\ndata: {"a":1}\n\n'])
    expect(events).toEqual([{ event: 'log', data: '{"a":1}', id: '1790856000123456789' }])
  })

  it('handles events split at arbitrary points, including inside a field name', () => {
    const text = 'id: 7\nevent: log\ndata: hello\n\nid: 8\nevent: log\ndata: world\n\n'
    for (let cut = 1; cut < text.length; cut++) {
      const { events } = parse([text.slice(0, cut), text.slice(cut)])
      expect(events.map(e => e.data), `cut at ${cut}`).toEqual(['hello', 'world'])
      expect(events.map(e => e.id)).toEqual(['7', '8'])
    }
  })

  it('accepts CRLF and bare CR terminators, even when CRLF is split across chunks', () => {
    expect(parse(['event: log\r\ndata: a\r\n\r\n']).events).toHaveLength(1)
    expect(parse(['event: log\rdata: a\r\r']).events).toHaveLength(1)
    const split = parse(['event: log\r', '\ndata: a\r', '\n\r', '\n'])
    expect(split.events).toEqual([{ event: 'log', data: 'a', id: '' }])
  })

  it('ignores comments but counts them as activity', () => {
    const { events, activity } = parse([': keepalive\n\n'])
    expect(events).toEqual([])
    expect(activity()).toBe(2)
  })

  it('joins multi-line data with newlines and strips one leading space only', () => {
    const { events } = parse(['data:  two spaces\ndata: second\ndata\n\n'])
    expect(events[0]!.data).toBe(' two spaces\nsecond\n')
  })

  it('defaults the event type to message and does not dispatch events without data', () => {
    const { events } = parse(['data: x\n\nevent: log\n\n'])
    expect(events).toEqual([{ event: 'message', data: 'x', id: '' }])
  })

  it('keeps the last id across events and ignores ids with NUL', () => {
    const { events } = parse(['id: 1\ndata: a\n\ndata: b\n\nid: 2\0\ndata: c\n\n'])
    expect(events.map(e => e.id)).toEqual(['1', '1', '1'])
  })

  it('strips a byte order mark and ignores unknown fields and retry', () => {
    const { events } = parse(['﻿retry: 10\nfoo: bar\nevent: log\ndata: a\n\n'])
    expect(events).toEqual([{ event: 'log', data: 'a', id: '' }])
  })

  it('does not deliver an event that has no terminating blank line yet', () => {
    expect(parse(['event: log\ndata: a\n']).events).toEqual([])
  })

  it('discards a line longer than the limit and carries on with the next event', () => {
    const { events, parser } = parse(['data: ', 'x'.repeat(50), 'y'.repeat(50), '\n\n', 'event: log\ndata: ok\n\n'], 64)
    expect(parser.oversized).toBe(1)
    expect(events.map(e => e.data)).toEqual(['ok'])
  })
})
