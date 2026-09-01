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

function loadContext() {
  const ok = (body) => ({
    ok: true,
    status: 200,
    headers: { get: (h) => (String(h).toLowerCase() === 'content-type' ? 'application/json' : '') },
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
  const notOk = (msg) => ({
    ok: false,
    status: 400,
    headers: { get: () => '' },
    json: async () => ({ error: msg }),
    text: async () => msg,
  });
  // Default backend behaviour: auth unconfigured (no auto-boot), status ok,
  // conversations list configurable per-test.
  const routes = {
    '/api/auth/state': () => ok({ configured: false, authenticated: false }),
    '/api/status': () => ok({ root: '/home/nick', settings: {} }),
    '/api/settings': () => ok({ providers: [], activeProvider: '' }),
    '/api/agent/status': () => ok({ available: false }),
    '/api/conversations': () => ok([]),
  };
  let conversationList = () => ok([]);
  const ctx = makeContext((url, opt) => {
    if (url === '/api/conversations' && (!opt.method || opt.method.toUpperCase() === 'GET')) {
      return conversationList();
    }
    const h = routes[url];
    if (h) return h();
    return ok({});
  });
  ctx.routes = routes;
  ctx.setConversations = (fn) => { conversationList = fn; };
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
    run(ctx, `serverReady=false; sessions={}; closedSessions=[]; localStorage.removeItem('cortex.sessions.v1');`);
    ctx.setConversations(() => ({
      ok: true,
      status: 200,
      headers: { get: () => 'application/json' },
      json: async () => [
        { id: 'valid', workspace: '/home/nick/repo', workspaceStatus: 'available', archivedAt: 0, events: [], createdAt: 1, updatedAt: 1 },
        { id: 'archived', workspace: '/home/nick/gone', workspaceStatus: 'missing', archivedAt: 123, events: [], createdAt: 1, updatedAt: 1 },
      ],
      text: async () => '',
    }));
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
})();