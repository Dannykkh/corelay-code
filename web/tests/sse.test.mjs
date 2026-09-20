import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

const source = readFileSync(new URL('../src/lib/sse.ts', import.meta.url), 'utf8');
const compiled = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText;
const exported = {};
new Function('exports', compiled)(exported);
const { SSEParser, streamSSE } = exported;

test('EOF never synthesizes an unterminated done event', () => {
  for (const ending of ['', '\n', '\r', '\r\n']) {
    const parser = new SSEParser();
    assert.deepEqual(parser.feed('data: {"type":"done"}' + ending), []);
    assert.deepEqual(parser.finish(), []);
    assert.deepEqual(parser.finish(), []);
  }
});

test('complete frames survive split UTF-8 and all line endings', async () => {
  for (const delimiter of ['\n', '\r', '\r\n']) {
    const bytes = new TextEncoder().encode(`data: {"type":"text","data":"한글"}${delimiter}${delimiter}data: {"type":"done"}${delimiter}${delimiter}`);
    const body = new ReadableStream({ start(controller) {
      for (const byte of bytes) controller.enqueue(new Uint8Array([byte]));
      controller.close();
    } });
    const frames = [];
    for await (const frame of streamSSE(new Response(body))) frames.push(JSON.parse(frame.data));
    assert.deepEqual(frames, [{ type: 'text', data: '한글' }, { type: 'done' }]);
  }
});

test('empty EOF yields no completion and partial frames are discarded', async () => {
  for (const body of ['', 'data: {"type":"done"}', 'data: {"type":"text","data":"partial"}\n\ndata: {"type":"done"}']) {
    const frames = [];
    for await (const frame of streamSSE(new Response(body))) frames.push(JSON.parse(frame.data));
    assert.ok(frames.every(frame => frame.type !== 'done'));
    assert.equal(frames.length, body.includes('partial') ? 1 : 0);
  }
});

test('user cancellation interrupts a pending read without flushing pending done', async () => {
  const abort = new AbortController();
  let cancelled = false;
  const body = new ReadableStream({
    start(controller) { controller.enqueue(new TextEncoder().encode('data: {"type":"done"}')); },
    cancel() { cancelled = true; },
  });
  const stream = streamSSE(new Response(body), abort.signal);
  const pending = stream.next();
  abort.abort();
  await assert.rejects(pending, { name: 'AbortError' });
  assert.equal(cancelled, true);
});

test('transport failure after a complete done frame still rejects the stream', async () => {
  let controller;
  const body = new ReadableStream({ start(value) {
    controller = value;
    controller.enqueue(new TextEncoder().encode('data: {"type":"done"}\n\n'));
  } });
  const stream = streamSSE(new Response(body));
  const first = await stream.next();
  assert.equal(JSON.parse(first.value.data).type, 'done');
  const pending = stream.next();
  controller.error(new Error('connection reset before durable receipt'));
  await assert.rejects(pending, /connection reset/);
});
