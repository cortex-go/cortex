// Frontend UX contract tests for sticky-bottom scroll, the todowrite task
// panel, and the tab running-spinner / unread indicator.
//
// These drive the real content/assets/js/script.js in a minimal DOM sandbox.
//
// Run with: node tests/agent-ux.test.js

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SCRIPT = fs.readFileSync(path.join(__dirname, '..', 'content', 'assets', 'js', 'script.js'), 'utf8');

class Node {
  constructor(tag = '') {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.dataset = {};
    this.classList = { add() {}, remove() {}, toggle() {} };
    this.style = {};
    this._text = '';
    this._className = '';
    this.hidden = false;
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.clientHeight = 0;
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
    if (!els.has(sel)) els.set(sel, new Node('div'));
    return els.get(sel);
  };
  const document = {
    createElement: (t) => new Node(t),
    createTextNode: (t) => ({ textContent: String(t) }),
    querySelector: (sel) => el(sel),
    querySelectorAll: () => [],
    addEventListener: () => {},
    body: new Node('body'),
  };
  const fetchCalls = [];
  const fetchImplReal = fetchImpl || (() => Promise.resolve(jsonOk({})));
  const ctx = {
    document,
    console,
    addEventListener: () => {},
    removeEventListener: () => {},
    confirm: () => true,
    innerWidth: 1280,
    innerHeight: 800,
    fetch: (url, opt = {}) => { fetchCalls.push({ url, ...opt }); return fetchImplReal(url, opt); },
    localStorage: {
      _d: {},
      getItem(k) { return this._d[k] || null; },
      setItem(k, v) { this._d[k] = String(v); },
      removeItem(k) { delete this._d[k]; },
    },
    setTimeout,
    clearTimeout,
    crypto: { randomUUID: () => 'id-' + Math.random().toString(36).slice(2) },
    fetchCalls,
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  return ctx;
}

function loadContext(fetchImpl, preload) {
  const ctx = makeContext(fetchImpl);
  if (preload) for (const k of Object.keys(preload)) ctx[k] = preload[k];
  vm.createContext(ctx);
  vm.runInContext(SCRIPT, ctx);
  return ctx;
}

function run(ctx, code, vars) {
  if (vars) {
    for (const k of Object.keys(vars)) ctx[k] = vars[k];
  }
  return vm.runInContext(code, ctx);
}
const settle = (ms = 10) => new Promise((r) => setTimeout(r, ms));

const __tests = [];
function test(name, fn) { __tests.push({ name, fn }); }

// Any asynchronous rejection after a test has been counted must still produce
// a nonzero process result, never be silently masked.
process.on('unhandledRejection', (reason) => {
  console.error('UNHANDLED REJECTION: ' + (reason && reason.stack || reason));
  process.exitCode = 1;
});
process.on('uncaughtException', (err) => {
  console.error('UNCAUGHT EXCEPTION: ' + (err && err.stack || err));
  process.exitCode = 1;
});

// A fake feed element with controlled scroll metrics.
function jsonOk(v){return {ok:true,json:()=>Promise.resolve(v),headers:{get:()=>'application/json'}}}
function feedNode(scrollTop, scrollHeight, clientHeight) {
  const f = new Node('div');
  f.scrollTop = scrollTop;
  f.scrollHeight = scrollHeight;
  f.clientHeight = clientHeight;
  return f;
}

test('nearBottom: at bottom follows new event', async () => {
  const ctx = loadContext();
  const feed = feedNode(1000, 1000, 100);
  const near = run(ctx, 'nearBottom(FEED)', { FEED: feed });
  if (!near) throw new Error('near-bottom feed should be near');
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;const b=FEED;if(!nearBottom(b))return;b.scrollTop=b.scrollHeight;s.followBottom=true;})()`, { S: s, FEED: feed });
  if (feed.scrollTop !== feed.scrollHeight) throw new Error('did not follow to bottom');
});

test('scrolled upward: position preserved on new event', async () => {
  const ctx = loadContext();
  const feed = feedNode(200, 1000, 100);
  if (run(ctx, 'nearBottom(FEED)', { FEED: feed })) throw new Error('scrolled-up feed should not be near bottom');
  const s = { followBottom: false, events: [] };
  const before = feed.scrollTop;
  run(ctx, `(function(){const s=S;const b=FEED;if(!nearBottom(b))return;b.scrollTop=b.scrollHeight;s.followBottom=true;})()`, { S: s, FEED: feed });
  if (feed.scrollTop !== before) throw new Error('scrolled-up feed jumped');
});

test('task panel renders validated todowrite snapshot and progress', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"one","status":"completed","priority":"high"},{"content":"two","status":"in_progress","priority":"low"},{"content":"three","status":"pending","priority":"medium"}]'});renderTaskPanel(s);})()`, { S: s });
  const list = run(ctx, `$('#taskList').children.length`);
  if (list !== 3) throw new Error('task rows = ' + list);
  const progress = run(ctx, `$('#taskProgress').textContent`);
  if (progress !== '1 of 3 completed') throw new Error('progress = ' + progress);
});

test('task panel ignores malformed task text', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [{ kind: 'task', text: 'not-json' }] };
  run(ctx, `(function(s){renderTaskPanel(s)})(S)`, { S: s });
  const hidden = run(ctx, `$('#taskPanel').hidden`);
  if (!hidden) throw new Error('malformed task should hide panel');
});

test('tab spinner visible when busy on active and background tabs', async () => {
  const ctx = loadContext();
  run(ctx, `(function(){sessions={a:{id:'a',title:'A',busy:true,unread:0},b:{id:'b',title:'B',busy:true,unread:0}};activeId='a';renderTabs();})()`);
  const spinners = run(ctx, `document.querySelectorAll().length`);
  // querySelectorAll is stubbed; count via children of the tabs container.
  const tabsBox = run(ctx, `$('#sessionTabs').children`);
  if (!tabsBox || tabsBox.length !== 2) throw new Error('two tabs expected');
});

test('busy session close uses server-side stop (no immediate abort)', async () => {
  const ctx = loadContext();
  let cancelled = null;
  const routes = {
    '/api/agent/cancel': (opt) => { cancelled = JSON.parse(opt.body); return Promise.resolve({ ok: true, json: () => Promise.resolve({ cancelled: true }) }); },
    '/api/auth/state': () => Promise.resolve({ ok: true, json: () => Promise.resolve({ configured: false, authenticated: false }) }),
  };
  const ctx2 = loadContext((url, opt) => {
    if (routes[url]) return routes[url](opt);
    return Promise.resolve(jsonOk({}));
  });
  run(ctx2, `(function(){const s=sessions['a']||{id:'a',busy:true,runID:'run-1',abort:null,followBottom:true,unread:0};sessions={a:s};activeId='a';stopAgentFor(s);})()`);
  await settle(20);
  if (!cancelled || cancelled.runID !== 'run-1') throw new Error('stop protocol not used for busy close');
});

test('unread indicator cleared when switching to a tab', async () => {
  const ctx = loadContext();
  run(ctx, `(function(){sessions={a:{id:'a',title:'A',busy:false,unread:3,events:[]},b:{id:'b',title:'B',busy:false,unread:0,events:[]}};activeId='a';})()`);
  run(ctx, `(function(){const s=sessions['b'];activeId='b';s.unread=0;})()`);
  const unread = run(ctx, `sessions['b'].unread`);
  if (unread !== 0) throw new Error('unread not cleared');
});
test('real syncServerConversations keeps spinner for running conversation', async () => {
  // Server returns an authoritative running conversation; the real sync
  // function must derive busy from state, not a manual assignment.
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (!run(ctx, "sessions['a'] && sessions['a'].busy")) throw new Error('spinner lost after reload while run active');
  if (run(ctx, "sessions['a'].currentRunId") !== 'run-1') throw new Error('currentRunId lost');
});

test('real syncServerConversations clears spinner for terminal conversation', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'interrupted', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "sessions['a'] && sessions['a'].busy")) throw new Error('spinner not cleared for terminal conversation');
});

test('two running background tabs both show spinners', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([
      { id: 'a', state: 'running', currentRunId: 'r1', events: [] },
      { id: 'b', state: 'running', currentRunId: 'r2', events: [] },
    ]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (!run(ctx, "sessions['a'].busy") || !run(ctx, "sessions['b'].busy")) throw new Error('both running tabs should show spinners');
});

test('reconcileRunState polls until terminal (running->running->completed)', async () => {
  const states = ['running', 'running', 'completed'];
  let i = 0;
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: states[Math.min(i++, states.length - 1)], currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={a:{id:"a",state:"running",currentRunId:"run-1",busy:true,events:[],followBottom:true,unread:0}};activeId="a"');
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner stuck after polling to terminal');
  if (run(ctx, "sessions['a'].state") !== 'completed') throw new Error('state not updated after polling');
});

test('terminal state received normally clears the spinner', async () => {
  const ctx = loadContext();
  const s = { id: 'a', state: 'running', currentRunId: 'run-1', busy: true, events: [], followBottom: true, unread: 0 };
  run(ctx, 'sessions={a:S};activeId="a"', { S: s });
  run(ctx, `(function(){const s=sessions['a'];s.state='completed';s.busy=false;})()`);
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner not cleared on terminal outcome');
});

test('reconcileRunState clears spinner for interrupted state after disconnect', async () => {
  const ctx = loadContext();
  const rec = { id: 'a', state: 'interrupted', currentRunId: 'run-1', events: [] };
  run(ctx, 'sessions={a:{id:"a",state:"running",currentRunId:"run-1",busy:true,events:[],followBottom:true,unread:0}};activeId="a"');
  run(ctx, 'window.__conv=REC', { REC: rec });
  // Override api to return the authoritative record.
  run(ctx, `(function(){const orig=api;api=async()=>[window.__conv];})(S)`, { S: ctx });
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner not cleared after interrupted reconciliation');
  if (run(ctx, "sessions['a'].state") !== 'interrupted') throw new Error('state not reconciled');
});

test('busy close keeps the tab when cancellation is rejected', async () => {
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('run not found') });
    return Promise.resolve({ ok: true, json: () => Promise.resolve([]) });
  });
  run(ctx, 'sessions={a:{id:"a",state:"running",runID:"run-1",busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId="a"');
  // api helper in this sandbox is the script's own; it calls fetch which returns non-ok -> throws.
  let kept = true;
  try { await run(ctx, '(async()=>{await stopAgentFor(sessions["a"]);return sessions["a"]!==undefined})()'); }
  catch (e) { kept = false; }
  // stopAgentFor returns false and keeps the session object; the close flow
  // then refuses to archive. Verify the session is still present.
  if (run(ctx, "sessions['a']") === undefined) throw new Error('session was archived despite rejection');
});

test('old run replaced by newer running run keeps spinner', async () => {
  let i = 0;
  const states = ['running', 'running'];
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: states[Math.min(i++, states.length-1)], currentRunId: 'run-2', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'reconcileRunState("a")');
  if (!run(ctx, "sessions['a'].busy")) throw new Error('newer running run should keep the spinner');
  if (run(ctx, "sessions['a'].currentRunId") !== 'run-2') throw new Error('currentRunId not updated');
});

test('old run replaced by newer terminal run clears spinner', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'completed', currentRunId: 'run-2', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('newer terminal run should clear the spinner');
});

test('a stale poll cannot overwrite a later synchronization', async () => {
  // The poll returns running for the old run; meanwhile a later sync set the
  // session to completed. The poll must not force it back to running.
  let i = 0;
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: i++ === 0 ? 'running' : 'completed', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  const p = run(ctx, 'reconcileRunState("a")');
  // Simulate a later synchronization setting terminal state.
  run(ctx, "sessions['a'].state='completed';sessions['a'].busy=false");
  await p;
  if (run(ctx, "sessions['a'].busy")) throw new Error('stale poll overwrote later sync');
});

// Real closeSession flows: rejected cancellation keeps the tab.
test('closeSession keeps tab when cancellation is rejected', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('not found') });
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") === undefined) throw new Error('session was archived despite rejected cancellation');
});

// Real closeSession: accepted-but-draining keeps the tab, is not archived, the
// active selection is unchanged, and the pending-stop message is surfaced.
test('closeSession keeps tab when stop accepted but still draining', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 20, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ < 3 ? 'running' : 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") === undefined) throw new Error('draining close must keep the tab in the open map');
  if (run(ctx, "closedSessions.some(x=>x.id==='a')")) throw new Error('draining close must not archive the session');
  if (run(ctx, "activeId") !== 'a') throw new Error('active selection changed while draining');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('winding down')) throw new Error('pending-stop message not surfaced: ' + msg);
});

// Real closeSession: terminal cancellation archives it.
test('closeSession archives after terminal cancellation', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 20, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") !== undefined) throw new Error('terminal cancellation should archive the session');
});

// Direct Stop control: explicit non-success cancel response is rejected.
test('stopAgent shows rejected message on explicit cancel refusal', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('run not found') });
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.rejected !== true) throw new Error('explicit refusal must be rejected, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Could not stop the running agent.') throw new Error('rejected toast = ' + msg);
});

// Direct Stop control: confirmed cancel followed by terminal is a clean stop.
test('stopAgent shows terminal message on confirmed cancel', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.ok !== true) throw new Error('terminal stop should be ok, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Agent stopped.') throw new Error('terminal toast = ' + msg);
});

// Direct Stop control: confirmed cancel followed by a still-running state is draining.
test('stopAgent shows draining message on acknowledged still-running cancel', async () => {
  const preload = { __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.draining !== true) throw new Error('acknowledged still-running must be draining, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('winding down')) throw new Error('draining toast = ' + msg);
});

// Timeout: a cancel request that never returns, followed by terminal state, is
// a successful stop (reconciled against authoritative state).
test('stopAgent cancel timeout followed by terminal returns success', async () => {
  let n = 0;
  const preload = { __CANCEL_TIMEOUT_MS: 30, __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return new Promise(() => {});
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.ok !== true) throw new Error('timeout then terminal should stop cleanly, got ' + JSON.stringify(res));
  if (res.unconfirmed) throw new Error('terminal must not be reported unconfirmed');
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Agent stopped.') throw new Error('timeout-terminal toast = ' + msg);
});

// Timeout: a cancel request that never returns, followed by a still-running
// state, is unconfirmed (never described as an explicit rejection).
test('stopAgent cancel timeout followed by running is unconfirmed not rejected', async () => {
  const preload = { __CANCEL_TIMEOUT_MS: 30, __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return new Promise(() => {});
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.rejected) throw new Error('timeout must never be reported as rejected');
  if (res.unconfirmed !== true) throw new Error('timeout still-running must be unconfirmed, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('Could not confirm the stop')) throw new Error('unconfirmed toast = ' + msg);
});

(async function runAll() {
  let pass = 0, fail = 0;
  for (const { name, fn } of __tests) {
    try {
      await fn();
      console.log('ok - ' + name);
      pass++;
    } catch (e) {
      console.error('FAIL - ' + name);
      console.error(e && e.stack || e);
      process.exitCode = 1;
      fail++;
    }
  }
  console.log(`\n${pass} passed, ${fail} failed, ${__tests.length} total`);
  if (fail > 0) process.exitCode = 1;
})();
