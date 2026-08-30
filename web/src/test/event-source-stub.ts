/**
 * EventSourceStub は jsdom に無い EventSource を埋める。最後に作られたインスタンスを
 * 覚えておき、テストからサーバー側のイベントを発火できるようにする。
 *
 * `lib/events.test.tsx` と `components/connection-banner.test.tsx` の両方が
 * `useServerEvents`（`/api/events` を購読するフック）を経由してこれを使う。
 */
export class EventSourceStub {
  static last: EventSourceStub | null = null
  private listeners = new Map<string, Set<(event: Event) => void>>()
  closed = false
  url: string

  // 引数プロパティ（`constructor(public url)`）は erasableSyntaxOnly で使えない
  constructor(url: string) {
    this.url = url
    EventSourceStub.last = this
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    const set = this.listeners.get(type) ?? new Set()
    set.add(listener)
    this.listeners.set(type, set)
  }

  removeEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.get(type)?.delete(listener)
  }

  close(): void {
    this.closed = true
  }

  emit(type: string, data?: string): void {
    const event = data === undefined ? new Event(type) : new MessageEvent(type, { data })
    for (const listener of this.listeners.get(type) ?? []) listener(event)
  }
}
