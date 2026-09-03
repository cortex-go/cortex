// Frontend conversation-persistence contract tests.
//
// These drive the real content/assets/js/script.js in a minimal DOM/fetch
// sandbox and assert the exact conversation PUT behaviour:
//
//   - mutating one conversation issues one PUT for that id and no fan-out;
//   - switching tabs issues zero PUTs;
//   - boot synchronization issues zero PUTs;
//   - an archived unavailable conversation never interferes with an active
//     valid conversation;
//   - an unavailable workspace disables Run, and selecting a replacement
//     re-enables it.
//
// Run with: node tests/conversation-save.test.js

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

const SCRIPT = fs.readFileSync(path.join(__dirname, '..', 'content', 'assets', 'js', 'script.js'), 'utf8');

class El {
  constructor(tag = '') {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.dataset = {};
    this.classList = { add() {}, remove() {}, toggle() {} };
    this.style = {};
    this._text = '';
    this._className = '';
    this._innerHTML = '';
    this.hidden = false;
    this.disabled = false;
    this.value = '';
    this.checked = false;
    this.placeholder = '';
    this.href = '';
    this.target = '';
    this.rel = '';
    this.scrollTop = 0;
    this.scrollHeight = 0;
  }
  append(...x) { for (const k of x) if (k != null) this.children.push(k); }
  appendChild(x) { this.children.push(x); return x; }
  remove() {}
  addEventListener() {}
  focus() {}
  closest() { return null; }
  getBoundingClientRect() { return { height: 100 }; }
  requestSubmit() {}
  stopPropagation() {}
  setAttribute(name, value) { this[name] = String(value); }
  removeAttribute(name) { this[name] = ''; }
  getAttribute(name) { return this[name] || null; }
  querySelector() { return null; }
  querySelectorAll() { return []; }
  set textContent(v) { this._text = String(v); }
  get textContent() { return this._text; }
  set className(v) { this._className = String(v); }
  get className() { return this._className; }
  set innerHTML(v) { this._innerHTML = String(v); this.children = []; }
  get innerHTML() { return this._innerHTML; }
}

function makeContext(fetchImpl) {
  const els = new Map();
  const el = (sel) => {
    if (!els.has(sel)) els.set(sel, new El('div'));
    return els.get(sel);
  };
  const document = {
    createElement: (t) => new El(t),
    createTextNode: (t) => ({ textContent: String(t) }),
    querySelector: (sel) => el(sel),
    querySelectorAll: () => [],
    addEventListener: () => {},
    body: new El('body'),
  };
  const fetchCalls = [];
  const fetch = async (url, opt = {}) => {
    fetchCalls.push({ url: String(url), method: (opt.method || 'GET').toUpperCase(), body: opt.body || '' });
    return fetchImpl(url, opt);
  };
  const localStorage = {
    store: {},
    getItem(k) { return k in this.store ? this.store[k] : null; },
    setItem(k, v) { this.store[k] = String(v); },
    removeItem(k) { delete this.store[k]; },
    clear() { this.store = {}; },
  };
  const ctx = {
    document,
    window: { addEventListener() {} },
    location: { origin: 'http://localhost', reload() {} },
    localStorage,
    crypto: { randomUUID: (() => { let n = 0; return () => 'id-' + (++n); })() },
    fetch,
    console,
    requestAnimationFrame: () => 0,
    confirm: () => true,
    prompt: () => '',
    FileReader: class {},
    Image: class {},
    TextDecoder: require('util').TextDecoder,
    TextEncoder: require('util').TextEncoder,
    AbortController,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    URL,
    Blob: class {},
  };
  ctx.fetchCalls = fetchCalls;
  ctx.els = els;
  vm.createContext(ctx);
  return ctx;
}

function loadContext(server) {
  const ok = (body) => ({
    ok: true,
    status: 200,
    headers: { get: (h) => (String(h).toLowerCase() === 'content-type' ? 'application/json' : '') },
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
  // Stateful conversation store shared across "browser restarts": GET returns
  // the stored records, POST imports records not in `rejectIds` and reports
  // imported/rejected deterministically.
  const store = server || { records: [], rejectIds: new Set() };
  const handleConversations = (opt) => {
    const method = (opt.method || 'GET').toUpperCase();
    if (method === 'GET') return ok([...store.records]);
    const batch = JSON.parse(opt.body || '{}');
    const imported = [];
    const rejected = [];
    for (const c of (batch.conversations || [])) {
      if (store.rejectIds.has(c.id)) {
        rejected.push({ id: c.id, reason: 'rejected by fixture' });
      } else if (!store.records.some((r) => r.id === c.id)) {
        store.records.push(c);
        imported.push(c.id);
      } else {
        imported.push(c.id); // idempotent upsert
      }
    }
    return ok({ imported, rejected });
  };
  const routes = {
    '/api/auth/state': () => ok({ configured: false, authenticated: false }),
    '/api/status': () => ok({ root: '/home/nick', settings: {} }),
    '/api/settings': () => ok({ providers: [], activeProvider: '' }),
    '/api/agent/status': () => ok({ available: false }),
    '/api/conversations': (opt) => handleConversations(opt),
  };
  const ctx = makeContext((url, opt) => {
    const h = routes[url];
    if (h) return h(opt);
    return ok({});
  });
  ctx.routes = routes;
  ctx.store = store;
  vm.runInContext(SCRIPT, ctx);
  return ctx;
}

function run(ctx, code) { return vm.runInContext(code, ctx); }
async function runAsync(ctx, code) { return vm.runInContext('(async()=>{return ' + code + '})()', ctx); }
const settle = (ms = 260) => new Promise((r) => setTimeout(r, ms));

function putCalls(ctx) {
  return ctx.fetchCalls.filter((c) => c.method === 'PUT' && c.url === '/api/conversation');
}

async function test(t) {
  const ctx = loadContext();
  await settle(10); // let the top-level loadAuthState() settle
  ctx.fetchCalls.length = 0;
  await t(ctx);
  console.log('ok - ' + t.name);
}

// bootSimulation seeds the obsolete browser payload (if provided) and runs a
// fresh boot load. It proves the obsolete payload is discarded without import,
// and that a boot never PUTs authoritative conversations back to the server.
async function bootSimulation(ctx, localStore) {
  if (localStore) {
    const payload = JSON.stringify({ activeId: 'a', sessions: localStore.sessions || {}, closedSessions: localStore.closedSessions || [] });
    run(ctx, `localStorage.setItem('cortex.sessions.v1', ${JSON.stringify(payload)});`);
  }
  run(ctx, `loadSessions()`);
  await runAsync(ctx, `syncServerConversations()`);
  await settle();
}

(async () => {
  await test(async function mutating_one_conversation_issues_one_put(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/w1',events:[],createdAt:1}; sessions['b']={id:'b',workspace:'/w2',events:[],createdAt:2}; activeId='a';`);
    run(ctx, `addEvent('a','user','hello')`);
    await settle();
    const puts = putCalls(ctx);
    assert.strictEqual(puts.length, 1, 'expected exactly one PUT, got ' + puts.length);
    const body = JSON.parse(puts[0].body);
    assert.strictEqual(body.id, 'a', 'PUT must target the mutated conversation');
    assert.ok(!putCalls(ctx).some((p) => JSON.parse(p.body).id === 'b'), 'unrelated conversation was written');
  });

  await test(async function switching_tabs_issues_zero_puts(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/w1',events:[],createdAt:1}; sessions['b']={id:'b',workspace:'/w2',events:[],createdAt:2}; activeId='a';`);
    run(ctx, `renderTabs()`);
    const tabs = ctx.els.get('#sessionTabs').children;
    assert.ok(tabs.length >= 2, 'expected at least two session tabs');
    tabs[1].onclick();
    await settle();
    assert.strictEqual(putCalls(ctx).length, 0, 'tab switching must not PUT any conversation');
  });

  await test(async function boot_synchronization_issues_zero_puts(ctx) {
    run(ctx, `serverReady=false; sessions={}; closedSessions=[];`);
    ctx.store.records.push(
      { id: 'valid', workspace: '/home/nick/repo', workspaceStatus: 'available', archivedAt: 0, events: [], createdAt: 1, updatedAt: 1 },
      { id: 'archived', workspace: '/home/nick/gone', workspaceStatus: 'missing', archivedAt: 123, events: [], createdAt: 1, updatedAt: 1 },
    );
    await runAsync(ctx, `syncServerConversations()`);
    await settle();
    assert.strictEqual(putCalls(ctx).length, 0, 'loading authoritative conversations must issue no PUT');
  });

  await test(async function archived_unavailable_conversation_does_not_interfere(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['active']={id:'active',workspace:'/w1',workspaceStatus:'available',events:[],createdAt:1}; closedSessions=[{id:'archived',workspace:'/gone',workspaceStatus:'missing',archivedAt:9,events:[],createdAt:1}]; activeId='active';`);
    run(ctx, `addEvent('active','user','work')`);
    await settle();
    const puts = putCalls(ctx);
    assert.strictEqual(puts.length, 1, 'expected exactly one PUT');
    assert.strictEqual(JSON.parse(puts[0].body).id, 'active', 'the archived unavailable conversation must not be written');
  });

  await test(async function unavailable_workspace_disables_run_and_replacement_reenables(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/gone',workspaceStatus:'missing',events:[],createdAt:1}; activeId='a';`);
    run(ctx, `renderAll()`);
    assert.strictEqual(ctx.els.get('#run').disabled, true, 'Run must be disabled for an unavailable workspace');
    assert.strictEqual(ctx.els.get('#wsUnavailable').hidden, false, 'unavailable banner must be visible');
    run(ctx, `browserPath='/home/nick/real'; chooseWorkspace();`);
    run(ctx, `renderAll()`);
    assert.strictEqual(ctx.els.get('#run').disabled, false, 'Run must be re-enabled after a valid replacement workspace');
    assert.strictEqual(ctx.els.get('#wsUnavailable').hidden, true, 'unavailable banner must clear after replacement');
  });

  await test(async function replacing_workspace_clears_old_opencode_session(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/old',workspaceStatus:'available',openCodeSession:'sess-old',events:[],createdAt:1}; activeId='a';`);
    run(ctx, `browserPath='/home/nick/real'; chooseWorkspace();`);
    assert.strictEqual(run(ctx, `sessions['a'].openCodeSession`), '', 'old OpenCode session must be cleared when the workspace changes');
    assert.strictEqual(run(ctx, `sessions['a'].workspace`), '/home/nick/real', 'replacement workspace must be recorded');
  });

  await test(async function task_collapsed_preference_is_local_only(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/w1',events:[],createdAt:1,tasksCollapsed:true}; activeId='a';`);
    run(ctx, `addEvent('a','user','hello')`);
    await settle();
    const puts = putCalls(ctx);
    assert.strictEqual(puts.length, 1, 'expected exactly one PUT');
    const body = JSON.parse(puts[0].body);
    assert.ok(!('tasksCollapsed' in body), 'tasksCollapsed must not be sent to the server conversation endpoint');
    assert.strictEqual(run(ctx, `sessions['a'].tasksCollapsed`), true, 'tasksCollapsed lost in local session state');
    // The collapsed preference is retained as compact UI state, and no session
    // transcript or metadata is ever written to browser storage.
    run(ctx, `setTasksCollapsed(true)`);
    const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
    assert.ok(ui && ui.includes('"a":true'), 'collapsed preference not persisted as UI state');
    assert.ok(!ui.includes('"events"'), 'session transcript leaked into browser storage');
    assert.strictEqual(run(ctx, `localStorage.getItem('cortex.sessions.v1')`), null, 'session payload must never be written');
  });

  // A new session is created through the server API and only compact UI state
  // reaches browser storage; the oversized session payload is never written.
  await test(async function new_session_persists_to_server_not_browser_storage(ctx) {
    run(ctx, `serverReady=true; sessions={}; closedSessions=[]; activeId='';`);
    run(ctx, `newSession('/w1',false)`);
    await settle();
    const puts = putCalls(ctx);
    assert.strictEqual(puts.length, 1, 'a new session must be persisted to the server');
    const newId = run(ctx, `Object.keys(sessions)[0]`);
    assert.strictEqual(JSON.parse(puts[0].body).id, newId, 'new session id must match the server PUT');
    const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
    assert.ok(!ui || !ui.includes('"events"'), 'session data must not be written to browser storage');
    assert.strictEqual(run(ctx, `localStorage.getItem('cortex.sessions.v1')`), null, 'legacy session payload must not be written');
  });

  // A full/over-quota localStorage must never prevent creating or using a
  // session, and must never surface an uncaught quota exception.
  await test(async function quota_exhaustion_does_not_break_session_operations(ctx) {
    run(ctx, `localStorage.setItem=(k,v)=>{throw new DOMException('Quota exceeded','QuotaExceededError')};`);
    run(ctx, `serverReady=true; sessions={}; activeId='';`);
    const created = run(ctx, `(function(){try{newSession('/w',false);return true}catch(e){return false}})()`);
    assert.strictEqual(created, true, 'newSession must survive a quota-exhausted localStorage');
    assert.ok(Object.keys(run(ctx, `sessions`)).length >= 1, 'session must be created despite the quota');
    run(ctx, `addEvent(Object.keys(sessions)[0],'user','still usable')`);
    await settle();
    assert.ok(run(ctx, `sessions[Object.keys(sessions)[0]].events.length`) >= 1, 'session must remain usable');
  });

  // A throwing localStorage (disabled/denied storage) must never interrupt
  // session operations.
  await test(async function localStorage_totally_throwing_still_usable(ctx) {
    run(ctx, `localStorage.getItem=()=>{throw new Error('storage disabled')};localStorage.setItem=()=>{throw new Error('storage disabled')};localStorage.removeItem=()=>{throw new Error('storage disabled')};`);
    run(ctx, `serverReady=true; sessions={}; activeId='';`);
    const created = run(ctx, `(function(){try{newSession('/w',false);return true}catch(e){return false}})()`);
    assert.strictEqual(created, true, 'session creation must survive a throwing localStorage');
    run(ctx, `addEvent(Object.keys(sessions)[0],'user','works')`);
    await settle();
    assert.strictEqual(putCalls(ctx).length, 1, 'server persistence must still work when browser storage throws');
  });

  // Conversations, transcripts and archived records are restored from SQLite
  // after a browser reload / server restart, never from browser copies.
  await test(async function reload_and_server_restart_restore_conversations_from_sqlite(ctx) {
    const server = { records: [
      { id: 'c1', title: 'First', workspace: '/home/nick/repo', state: 'completed', archivedAt: 0, events: [{ kind: 'user', text: 'hello', name: '' }, { kind: 'assistant', text: 'hi', name: '' }], createdAt: 1, updatedAt: 2 },
      { id: 'c2', title: 'Old', workspace: '/gone', state: 'idle', archivedAt: 9, events: [], createdAt: 1, updatedAt: 2 },
    ] };
    const ctx1 = loadContext(server);
    await settle(10);
    await runAsync(ctx1, `syncServerConversations()`);
    await settle();
    assert.strictEqual(run(ctx1, `sessions['c1'] && sessions['c1'].events.length`), 2, 'transcript must be restored from the server');
    assert.strictEqual(run(ctx1, `closedSessions.some(s=>s.id==='c2')`), true, 'archived conversation must be restored from the server');
    const ui1 = run(ctx1, `localStorage.getItem('cortex.ui.v1')`);
    assert.ok(!ui1 || !ui1.includes('"events"'), 'no transcript may be stored in the browser');
    assert.strictEqual(run(ctx1, `localStorage.getItem('cortex.sessions.v1')`), null, 'legacy payload must be absent');
    // A fresh browser context loading the same server state (a server restart)
    // restores the identical conversations from SQLite alone.
    const ctx2 = loadContext(server);
    await settle(10);
    await runAsync(ctx2, `syncServerConversations()`);
    await settle();
    assert.strictEqual(run(ctx2, `sessions['c1'] && sessions['c1'].events.length`), 2, 'reload must restore the transcript from SQLite');
    assert.strictEqual(putCalls(ctx2).length, 0, 'restoring from the server must not PUT conversations back');
  });

  // Clearing browser storage must never delete or lose server-side
  // conversations.
  await test(async function clearing_browser_storage_does_not_lose_conversations(ctx) {
    const server = { records: [
      { id: 'c1', title: 'Keep', workspace: '/home/nick/repo', state: 'idle', archivedAt: 0, events: [{ kind: 'user', text: 'persisted', name: '' }], createdAt: 1, updatedAt: 2 },
    ] };
    const ctx1 = loadContext(server);
    await settle(10);
    await runAsync(ctx1, `syncServerConversations()`);
    await settle();
    run(ctx1, `localStorage.clear();`);
    assert.strictEqual(run(ctx1, `localStorage.getItem('cortex.ui.v1')`), null, 'browser storage cleared');
    const ctx2 = loadContext(server);
    await settle(10);
    await runAsync(ctx2, `syncServerConversations()`);
    await settle();
    assert.strictEqual(run(ctx2, `sessions['c1'] && sessions['c1'].events[0].text`), 'persisted', 'clearing browser storage must not lose conversations');
  });

  // The obsolete cortex.sessions.v1 payload and migration marker are removed on
  // boot without being parsed, imported or written back anywhere.
  await test(async function obsolete_cortex_sessions_v1_removed_without_import(ctx) {
    const legacy = { activeId: 'legacy', sessions: { legacy: { id: 'legacy', workspace: '/w', events: [{ kind: 'user', text: 'old' }] } }, closedSessions: [] };
    run(ctx, `localStorage.setItem('cortex.sessions.v1', ${JSON.stringify(JSON.stringify(legacy))});`);
    run(ctx, `localStorage.setItem('cortex.sessions.migrated.sqlite.v1','1');`);
    run(ctx, `loadSessions()`);
    assert.strictEqual(run(ctx, `localStorage.getItem('cortex.sessions.v1')`), null, 'obsolete payload must be removed on boot');
    assert.strictEqual(run(ctx, `localStorage.getItem('cortex.sessions.migrated.sqlite.v1')`), null, 'migration marker must be removed');
    assert.strictEqual(run(ctx, `Object.keys(sessions).length`), 0, 'obsolete payload must never be imported into sessions');
    const posted = ctx.fetchCalls.filter((c) => c.method === 'POST' && c.url === '/api/conversations');
    assert.strictEqual(posted.length, 0, 'obsolete payload must not be migrated to the server');
    const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
    assert.ok(!ui || !ui.includes('legacy'), 'no legacy session data may be written to browser storage');
  });

  // Archiving and restoring flow through the server API: archive persists an
  // archivedAt via PUT, and a reload restores it as a closed session; restoring
  // clears archivedAt via PUT.
  await test(async function archive_and_restore_persist_through_server(ctx) {
    run(ctx, `serverReady=true; sessions={}; sessions['a']={id:'a',workspace:'/w1',events:[{kind:'user',text:'x',name:''}],createdAt:1}; closedSessions=[]; activeId='a';`);
    run(ctx, `sessions['a'].busy=false; closeSession('a')`);
    await settle();
    const archivedPut = putCalls(ctx).find((p) => JSON.parse(p.body).id === 'a');
    assert.ok(archivedPut, 'archive must be persisted to the server');
    assert.ok(JSON.parse(archivedPut.body).archivedAt > 0, 'archive PUT must carry archivedAt');
    assert.ok(run(ctx, `closedSessions.some(s=>s.id==='a')`), 'archived session must be in the closed list');
    // Restore clears the archive via the server.
    run(ctx, `restoreSession('a')`);
    await settle();
    const restorePut = putCalls(ctx).filter((p) => JSON.parse(p.body).id === 'a').pop();
    assert.ok(restorePut, 'restore must be persisted to the server');
    assert.ok(!JSON.parse(restorePut.body).archivedAt, 'restore PUT must clear archivedAt');
  });
})();