// Persistent offline writes. Only a successful response may remove an entry.
export function createOutbox(indexedDB) {
  return {
    db: null,
    async open() {
      if (this.db) return this.db;
      this.db = await new Promise((resolve, reject) => {
        const req = indexedDB.open('lifetask', 1);
        req.onupgradeneeded = () => req.result.createObjectStore('outbox', { keyPath: 'key' });
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error);
      });
      return this.db;
    },
    async tx(mode, fn) {
      const db = await this.open();
      return new Promise((resolve, reject) => {
        const t = db.transaction('outbox', mode);
        t.onabort = t.onerror = () => reject(t.error || new Error('Не удалось сохранить очередь'));
        const r = fn(t.objectStore('outbox'));
        t.oncomplete = () => resolve(r && r.result);
      });
    },
    add(entry) { return this.tx('readwrite', (st) => st.put(entry)); },
    remove(key) { return this.tx('readwrite', (st) => st.delete(key)); },
    async all() { return ((await this.tx('readonly', (st) => st.getAll())) || []).sort((a, b) => a.ts - b.ts); },
  };
}

function retryDelay(attempts, response, now) {
  const backoff = Math.min(300000, 2000 * 2 ** Math.min(attempts - 1, 8));
  const value = response?.headers?.get('Retry-After');
  if (!value) return backoff;
  const seconds = /^\d+$/.test(value.trim()) ? Number(value) * 1000 : Date.parse(value) - now;
  return Number.isFinite(seconds) ? Math.max(backoff, seconds) : backoff;
}

async function responseError(response) {
  const data = await response.json().catch(() => null);
  return (data && data.error) || response.statusText || `HTTP ${response.status}`;
}

// A transient failure pauses the batch so later changes cannot overtake it.
// Failed validation stays visible, but does not prevent unrelated writes.
export async function replayOutbox({ store, send, now = Date.now, onlyKey }) {
  const result = { sent: 0, failed: 0, changed: false, authRequired: false, nextRetryAt: null };
  for (const entry of await store.all()) {
    if (onlyKey !== undefined && entry.key !== onlyKey) continue;
    if (entry.state === 'failed') continue;
    if (entry.retryAt > now()) break;
    let response;
    try {
      response = await send(entry.method, entry.path, entry.body, entry.key);
    } catch {
      const attempts = (entry.attempts || 0) + 1;
      await store.add({ ...entry, state: 'retry', attempts, retryAt: now() + retryDelay(attempts, null, now()), error: 'Нет связи с сервером' });
      result.changed = true;
      break;
    }
    if (response.ok) {
      await store.remove(entry.key);
      result.sent++;
      result.changed = true;
      continue;
    }
    const error = await responseError(response);
    result.changed = true;
    if (response.status === 401) {
      await store.add({ ...entry, state: 'auth', retryAt: null, error });
      result.authRequired = true;
      break;
    }
    if (response.status === 429 || response.status === 408 || response.status >= 500) {
      const attempts = (entry.attempts || 0) + 1;
      await store.add({ ...entry, state: 'retry', attempts, retryAt: now() + retryDelay(attempts, response, now()), error });
      break;
    }
    await store.add({ ...entry, state: 'failed', retryAt: null, error });
    result.failed++;
  }
  // Use the first pending write's deadline; auth waits for an explicit wakeup.
  const pending = (await store.all()).find((entry) => entry.state !== 'failed');
  if (!result.authRequired && pending?.state === 'retry') result.nextRetryAt = pending.retryAt;
  return result;
}
