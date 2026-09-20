export type SSEFrame = {
  event?: string;
  data: string;
};

function abortError(): DOMException {
  return new DOMException('The operation was aborted.', 'AbortError');
}

/** Incremental parser for server-sent events. It tolerates arbitrary UTF-8
 * chunk boundaries and dispatches only complete blank-line-delimited frames. */
export class SSEParser {
  private buffer = '';
  private eventName = '';
  private dataLines: string[] = [];

  feed(text: string): SSEFrame[] {
    this.buffer += text;
    const frames: SSEFrame[] = [];
    while (true) {
      const lineFeed = this.buffer.indexOf('\n');
      const carriageReturn = this.buffer.indexOf('\r');
      let index = -1;
      if (lineFeed >= 0 && carriageReturn >= 0) index = Math.min(lineFeed, carriageReturn);
      else index = Math.max(lineFeed, carriageReturn);
      if (index < 0) break;
      const delimiter = this.buffer[index];
      // Keep a terminal CR until the next chunk so a split CRLF is treated as
      // one line ending rather than an extra blank line.
      if (delimiter === '\r' && index === this.buffer.length - 1) break;
      const delimiterLength = delimiter === '\r' && this.buffer[index + 1] === '\n' ? 2 : 1;
      const line = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + delimiterLength);
      const frame = this.consumeLine(line);
      if (frame) frames.push(frame);
    }
    return frames;
  }

  finish(): SSEFrame[] {
    const frames: SSEFrame[] = [];
    // A final CR is a line delimiter even without a following LF. Any other
    // tail, and any event without its blank-line delimiter, is incomplete.
    if (this.buffer.endsWith('\r')) {
      const frame = this.consumeLine(this.buffer.slice(0, -1));
      if (frame) frames.push(frame);
    }
    this.buffer = '';
    this.eventName = '';
    this.dataLines = [];
    return frames;
  }

  private consumeLine(line: string): SSEFrame | undefined {
    if (line === '') return this.dispatch();
    if (line.startsWith(':')) return undefined;
    const separator = line.indexOf(':');
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? '' : line.slice(separator + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') this.eventName = value;
    else if (field === 'data') this.dataLines.push(value);
    return undefined;
  }

  private dispatch(): SSEFrame | undefined {
    if (this.dataLines.length === 0) {
      this.eventName = '';
      return undefined;
    }
    const frame: SSEFrame = {
      ...(this.eventName ? { event: this.eventName } : {}),
      data: this.dataLines.join('\n'),
    };
    this.eventName = '';
    this.dataLines = [];
    return frame;
  }
}

export async function* streamSSE(response: Response, signal?: AbortSignal): AsyncGenerator<SSEFrame> {
  const body = response.body;
  if (!body) throw new Error('SSE response body is unavailable.');
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const parser = new SSEParser();
  let aborted = Boolean(signal?.aborted);
  const onAbort = () => {
    aborted = true;
    void reader.cancel();
  };
  signal?.addEventListener('abort', onAbort, { once: true });
  try {
    while (true) {
      if (aborted) throw abortError();
      const { done, value } = await reader.read();
      if (done) break;
      if (aborted) throw abortError();
      for (const frame of parser.feed(decoder.decode(value, { stream: true }))) yield frame;
    }
    if (aborted) throw abortError();
    for (const frame of parser.feed(decoder.decode())) yield frame;
    for (const frame of parser.finish()) yield frame;
  } finally {
    signal?.removeEventListener('abort', onAbort);
    reader.releaseLock();
  }
}
