import { ref, type Ref } from 'vue'

/** A once-a-second countdown, such as the wait a throttled form shows. */
export interface Countdown {
  /** Seconds left; 0 when stopped. */
  left: Ref<number>
  start(seconds: number): void
  stop(): void
}

/** Creates a countdown. Call stop() when the component unmounts. */
export function createCountdown(): Countdown {
  const left = ref(0)
  let timer: ReturnType<typeof setInterval> | undefined
  function stop() {
    if (timer) clearInterval(timer)
    timer = undefined
    left.value = 0
  }
  function start(seconds: number) {
    stop()
    left.value = seconds
    timer = setInterval(() => {
      left.value = Math.max(0, left.value - 1)
      if (left.value === 0) stop()
    }, 1000)
  }
  return { left, start, stop }
}

/** "3 seconds", "1 second". */
export function seconds(n: number): string {
  return `${n} ${n === 1 ? 'second' : 'seconds'}`
}
