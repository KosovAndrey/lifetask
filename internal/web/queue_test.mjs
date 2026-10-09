import test from 'node:test';
import assert from 'node:assert/strict';
import { createOutbox, replayOutbox } from './static/queue.mjs';

const entry = (key = 'original-key', ts = 1) => ({ key, ts, method: 'POST', path: 'items', body: { title: 'Offline task' }, label: 'new task' });

function memoryStore(entries) {
  const data = new Map(entries.map((e) => [e.key, structuredClone(e)]));
  return {
    async all() { return [...data.values()].map((e) => structuredClone(e)).sort((a, b) => a.ts - b.ts); },
    async add(e) { data.set(e.key, structuredClone(e)); },
    async remove(key) { data.delete(key); },
  };
}

function response(status, error = 'server error', retryAfter) {
  return new Response(status === 204 ? null : JSON.stringify({ error }), {
    status,
    headers: retryAfter === undefined ? {} : { 'Retry-After': retryAfter },
  });
}

test('legacy entries are removed only after 2xx, including 204, preserving payload and key', async () => {
  const original = entry();
  const store = memoryStore([original, entry('second', 2)]);
  const calls = [];
  const result = await replayOutbox({ store, send: async (...args) => { calls.push(args); return response(calls.length === 1 ? 200 : 204); } });
  assert.deepEqual(calls[0], [original.method, original.path, original.body, original.key]);
  assert.equal(result.sent, 2);
  assert.deepEqual(await store.all(), []);
});

test('5xx persists the write, pauses later writes, and retries the same key after restart with exponential backoff', async () => {
  const store = memoryStore([entry(), entry('second', 2)]);
  let clock = 10000;
  const now = () => clock;
  const calls = [];
  const send = async (...args) => { calls.push(args); return response(503); };
  let result = await replayOutbox({ store, send, now });
  assert.equal(result.sent, 0);
  assert.equal(result.nextRetryAt, 12000);
  assert.equal(calls.length, 1);
  assert.equal((await store.all()).length, 2);
  assert.equal((await store.all())[0].attempts, 1);

  // A fresh invocation represents page reload; no in-memory timer is needed.
  result = await replayOutbox({ store, send, now });
  assert.equal(calls.length, 1);
  assert.equal(result.nextRetryAt, 12000);
  clock = 12000;
  result = await replayOutbox({ store, send, now });
  assert.equal(result.nextRetryAt, 16000);
  assert.equal((await store.all())[0].attempts, 2);
  assert.equal(calls[1][3], 'original-key');
  clock = 16000;
  const delivered = await replayOutbox({ store, now, send: async () => response(200) });
  assert.equal(delivered.sent, 2);
  assert.deepEqual(await store.all(), []);
});

test('429 honors Retry-After seconds and HTTP dates', async (t) => {
  for (const retryAfter of ['60', 'Thu, 01 Jan 1970 00:01:00 GMT']) {
    await t.test(retryAfter, async () => {
      const store = memoryStore([entry()]);
      const result = await replayOutbox({ store, now: () => 10000, send: async () => response(429, 'rate limited', retryAfter) });
      assert.equal(result.sent, 0);
      assert.equal(result.nextRetryAt, retryAfter === '60' ? 70000 : 60000);
      assert.equal((await store.all())[0].error, 'rate limited');
    });
  }
});

test('invalid Retry-After falls back to backoff, and repeated failures cap at five minutes', async () => {
  const store = memoryStore([{ ...entry(), attempts: 999 }]);
  const result = await replayOutbox({ store, now: () => 10000, send: async () => response(503, 'unavailable', 'invalid') });
  assert.equal(result.nextRetryAt, 310000);
});

test('a lost response keeps the original idempotency key and retries without losing later writes', async () => {
  const original = entry();
  const store = memoryStore([original, entry('second', 2)]);
  const result = await replayOutbox({ store, now: () => 0, send: async () => { throw new TypeError('Failed to fetch'); } });
  assert.equal(result.sent, 0);
  assert.equal(result.nextRetryAt, 2000);
  assert.deepEqual((await store.all())[0].body, original.body);
  assert.equal((await store.all())[0].key, original.key);
  const keys = [];
  await replayOutbox({ store, now: () => 2000, send: async (_method, _path, _body, key) => { keys.push(key); return response(200); } });
  assert.deepEqual(keys, ['original-key', 'second']);
});

test('401 preserves the write and pauses timers until authentication wakeup', async () => {
  const store = memoryStore([entry(), entry('second', 2)]);
  let calls = 0;
  const result = await replayOutbox({ store, send: async () => { calls++; return response(401, 'login required'); } });
  assert.equal(calls, 1);
  assert.equal(result.authRequired, true);
  assert.equal(result.nextRetryAt, null);
  assert.equal(result.sent, 0);
  assert.equal((await store.all())[0].state, 'auth');
  assert.equal((await store.all()).length, 2);
  const retried = await replayOutbox({ store, send: async () => response(200) });
  assert.equal(retried.sent, 2);
});

test('permanent errors remain visible, skip automatic retry, and permit explicit retry or discard', async () => {
  const store = memoryStore([entry(), entry('second', 2)]);
  const result = await replayOutbox({ store, send: async (_m, _p, _b, key) => response(key === 'original-key' ? 422 : 200, 'invalid title') });
  assert.equal(result.sent, 1);
  assert.equal(result.failed, 1);
  let [failed] = await store.all();
  assert.equal(failed.state, 'failed');
  assert.equal(failed.error, 'invalid title');
  await replayOutbox({ store, send: async () => assert.fail('permanent errors must not be automatically retried') });

  await store.add({ ...failed, state: 'pending', retryAt: null });
  const retried = await replayOutbox({ store, onlyKey: failed.key, send: async (_m, _p, _b, key) => { assert.equal(key, failed.key); return response(200); } });
  assert.equal(retried.sent, 1);
  assert.deepEqual(await store.all(), []);
  await store.add(failed);
  await store.remove(failed.key);
  assert.deepEqual(await store.all(), []);
});

test('non-JSON failures remain queued with a readable fallback', async () => {
  const store = memoryStore([entry()]);
  await replayOutbox({ store, send: async () => new Response('<html>bad gateway</html>', { status: 502, statusText: 'Bad Gateway' }) });
  const [pending] = await store.all();
  assert.equal(pending.error, 'Bad Gateway');
  assert.equal(pending.state, 'retry');
});

test('persistence failure does not discard the write', async () => {
  const store = memoryStore([entry()]);
  store.add = async () => { throw new Error('disk full'); };
  await assert.rejects(replayOutbox({ store, send: async () => response(503) }), /disk full/);
  assert.equal((await store.all()).length, 1);
});

test('IndexedDB transaction abort rejects instead of leaving a save pending', async () => {
  const store = createOutbox(null);
  const failure = new Error('quota exceeded');
  store.db = {
    transaction() {
      const transaction = { error: failure, objectStore: () => ({ put: () => ({}) }) };
      queueMicrotask(() => transaction.onabort());
      return transaction;
    },
  };
  await assert.rejects(store.add(entry()), failure);
});
