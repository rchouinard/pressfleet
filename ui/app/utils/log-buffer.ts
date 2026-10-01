/**
 * A bounded, oldest-first list of log lines. A busy site can write
 * thousands of lines a minute and a viewer left open overnight must not grow
 * without limit, so appending past `capacity` drops the oldest lines.
 *
 * `items` is replaced (never mutated) on every change, so it can sit in a
 * `shallowRef` and stay cheap to render.
 */
export class LogBuffer<T> {
  private list: readonly T[] = []
  /** Lines dropped from the old end by `append` since the last `clear`. */
  private droppedCount = 0

  constructor(readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity < 1) throw new RangeError('LogBuffer capacity must be a positive integer')
  }

  get items(): readonly T[] {
    return this.list
  }

  get length(): number {
    return this.list.length
  }

  get dropped(): number {
    return this.droppedCount
  }

  /** Room left for older lines. */
  get free(): number {
    return this.capacity - this.list.length
  }

  /** Adds newer lines at the end, dropping the oldest ones to stay within capacity. Returns how many were dropped. */
  append(lines: readonly T[]): number {
    if (lines.length === 0) return 0
    const merged = this.list.concat(lines)
    const over = Math.max(0, merged.length - this.capacity)
    this.list = over > 0 ? merged.slice(over) : merged
    this.droppedCount += over
    return over
  }

  /**
   * Adds older lines at the start, as many as fit (the newest lines are what
   * the user is looking at, so history never pushes them out). Returns how
   * many were added; they are the newest of `lines`, since `lines` is oldest
   * first and sits directly before the current first line.
   */
  prepend(lines: readonly T[]): number {
    const room = Math.max(0, this.free)
    const take = lines.slice(Math.max(0, lines.length - room))
    if (take.length > 0) this.list = take.concat(this.list)
    return take.length
  }

  clear(): void {
    this.list = []
    this.droppedCount = 0
  }
}
