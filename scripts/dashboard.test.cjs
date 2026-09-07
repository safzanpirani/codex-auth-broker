const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { test } = require('node:test');
const vm = require('node:vm');

const html = readFileSync(join(__dirname, '..', 'dashboard.html'), 'utf8');
const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map(match => match[1]);

function dashboard() {
  const nodes = new Map();
  const requests = [];
  const intervals = [];
  const confirmations = [];
  const session = new Map([['codex_auth_broker_key', 'synthetic-session-key']]);
  function element(id) {
    if (!nodes.has(id)) {
      const handlers = {};
      const node = {
        value: '', textContent: '', innerHTML: '', title: '', style: {}, handlers,
        lastElementChild: { textContent: '' },
        classList: { add() {}, remove() {}, toggle() {} },
        setAttribute() {}, addEventListener(name, fn) { handlers[name] = fn; },
        parentElement: { classList: { add() {}, remove() {}, toggle() {} } },
      };
      nodes.set(id, node);
    }
    return nodes.get(id);
  }
  const storage = map => ({
    getItem: key => map.get(key) || null,
    setItem: (key, value) => map.set(key, value),
    removeItem: key => map.delete(key),
  });
  const context = vm.createContext({
    AbortController,
    document: { getElementById: element, querySelectorAll: () => [], addEventListener() {} },
    window: {},
    sessionStorage: storage(session), localStorage: storage(new Map()),
    confirm: message => { confirmations.push(message); return true; },
    setInterval: (fn, ms) => intervals.push({ fn, ms }),
    fetch: (url, options) => new Promise(resolve => {
      requests.push({ url, options, respond(body, status = 200) {
        resolve({ ok: status < 400, status, text: async () => JSON.stringify(body) });
      } });
    }),
  });
  vm.runInContext(scripts.find(script => script.includes('const state =')), context);
  return { nodes, requests, intervals, confirmations, session, element };
}

const settled = () => new Promise(resolve => setImmediate(resolve));
const costBody = amount => ({ windows: { '24h': { priced: 1, cost_usd: amount } } });

test('every embedded dashboard script parses', () => {
  for (const script of scripts) new vm.Script(script);
});

test('a stale pricing response cannot replace the selected mode', async () => {
  const ui = dashboard();
  const original = ui.requests.find(request => request.url.includes('/costs'));
  ui.element('pricingMode').handlers.change({ target: { value: 'api' } });
  const latest = ui.requests.filter(request => request.url.includes('/costs')).at(-1);
  assert.match(latest.url, /pricing=api$/);
  assert.equal(original.options.signal.aborted, true);
  latest.respond(costBody(20));
  await settled();
  const selected = ui.element('costGrid').innerHTML;
  assert.match(selected, /\$20/);
  original.respond(costBody(1));
  await settled();
  assert.equal(ui.element('costGrid').innerHTML, selected);
});

test('periodic refresh does not overlap an outstanding request', async () => {
  const ui = dashboard();
  const refresh = ui.intervals.find(interval => interval.ms === 3000).fn;
  const count = () => ui.requests.filter(request => request.url.includes('/requests')).length;
  refresh(); refresh();
  assert.equal(count(), 1);
  ui.requests.find(request => request.url.includes('/requests')).respond({ requests: [] });
  await settled();
  refresh();
  assert.equal(count(), 2);
});

test('clear history discloses disk deletion and reloads requests and costs', async () => {
  const ui = dashboard();
  const result = ui.element('clearHistory').handlers.click();
  assert.match(ui.confirmations[0], /Permanently delete.*on-disk/);
  const deletion = ui.requests.find(request => request.options.method === 'DELETE');
  assert.ok(deletion);
  const before = ui.requests.length;
  deletion.respond({ status: 'cleared' });
  await settled();
  const refreshes = ui.requests.slice(before);
  assert.equal(refreshes.length, 2);
  assert.ok(refreshes.some(request => request.url.includes('/requests')));
  assert.ok(refreshes.some(request => request.url.includes('/costs')));
  for (const request of refreshes) request.respond({ requests: [], windows: {} });
  await result;
});

test('sign out clears server cookie login and the saved bearer key', async () => {
  const ui = dashboard();
  const result = ui.element('clearKey').handlers.click();
  const logout = ui.requests.find(request => request.url === '/dashboard/api/logout');
  assert.equal(logout.options.method, 'POST');
  logout.respond({ status: 'logged_out' });
  await settled();
  assert.equal(ui.session.has('codex_auth_broker_key'), false);
  assert.equal(ui.element('apiKey').value, '');
  for (const request of ui.requests.slice(-4)) {
    assert.equal(request.options.headers.Authorization, undefined);
    request.respond({ error: { message: 'unauthorized' } }, 401);
  }
  await result;
});

test('sign out clears displayed metadata before refresh and ignores older authenticated loads', async () => {
  const ui = dashboard();
  const history = {
    requests: [{ id: 1, status: 200, input_tokens: 9876, output_tokens: 123, cost_usd: 12 }],
    retained: 1, total_seen: 7, limit: 250, generated_at: '2026-01-01T00:00:00Z',
  };
  for (const request of ui.requests) {
    if (request.url.includes('/requests')) request.respond(history);
    else if (request.url.includes('/costs')) request.respond({ ...costBody(12), source: 'file', persist_path: '/synthetic/history.jsonl', persist_bytes: 123 });
    else if (request.url.includes('/usage')) request.respond({ plan_type: 'synthetic-plan', _broker: { account_id_present: true } });
    else request.respond({});
  }
  await settled();
  assert.equal(ui.element('kpiTokens').textContent, '10.0k');
  assert.equal(ui.element('kpiCost').textContent, '$12.00');
  assert.equal(ui.element('requestCount').textContent, '7');
  assert.equal(ui.element('persistInfo').title, '/synthetic/history.jsonl');

  ui.element('refreshNow').handlers.click();
  const stale = ui.requests.slice(-4);
  const result = ui.element('clearKey').handlers.click();
  ui.requests.find(request => request.url === '/dashboard/api/logout').respond({ status: 'logged_out' });
  await settled();

  const assertCleared = () => {
    assert.equal(ui.element('kpiTokens').textContent, '0');
    assert.equal(ui.element('kpiCost').textContent, '—');
    assert.equal(ui.element('kpiVisibleSub').textContent, 'of 0 retained');
    assert.equal(ui.element('requestCount').textContent, '0');
    assert.equal(ui.element('retainedCount').textContent, '0 / 0');
    assert.equal(ui.element('persistInfo').textContent, '—');
    assert.equal(ui.element('persistInfo').title, '');
    assert.equal(ui.element('snapshotTime').textContent, '—');
  };
  assertCleared();
  assert.equal(ui.element('costGrid').innerHTML, '');
  assert.equal(ui.element('primaryBody').innerHTML, '');
  for (const request of stale) {
    assert.equal(request.options.signal.aborted, true);
    request.respond(request.url.includes('/requests') ? history : costBody(99));
  }
  await settled();
  assertCleared();
  for (const request of ui.requests.slice(-4)) request.respond({ error: { message: 'unauthorized' } }, 401);
  await result;
  assertCleared();
});
