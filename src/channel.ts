/**
 * A queue that one consumer reads with `for await`.
 *
 * For turning callbacks - D-Bus signals, several streams at once - into the
 * async iterables a Source hands back. Items pushed before anyone is reading
 * are kept, and a failure is thrown to the reader after anything already
 * queued, so nothing that happened before the error is lost.
 */
export class Channel<T> implements AsyncIterable<T> {
  private readonly items: T[] = [];
  private waiter?: () => void;
  private finished = false;
  private error?: Error;

  push(item: T): void {
    if (this.finished) return;
    this.items.push(item);
    this.wake();
  }

  /** Finish cleanly. Anything pushed or failed afterwards is ignored. */
  end(): void {
    if (this.finished) return;
    this.finished = true;
    this.wake();
  }

  /** Finish with an error, unless already finished. */
  fail(error: Error): void {
    if (this.finished) return;
    this.finished = true;
    this.error = error;
    this.wake();
  }

  private wake(): void {
    const waiter = this.waiter;
    this.waiter = undefined;
    waiter?.();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    while (true) {
      const item = this.items.shift();
      if (item !== undefined) {
        yield item;
        continue;
      }
      if (this.finished) {
        if (this.error) throw this.error;
        return;
      }
      await new Promise<void>((resolve) => (this.waiter = resolve));
    }
  }
}
