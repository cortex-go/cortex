const fs = require('fs');
const assert = require('assert');

const js = fs.readFileSync('public/assets/js/script.js', 'utf8');
const html = fs.readFileSync('content/index.html', 'utf8');

assert(js.includes("function startFreshSession(){sessions={};closedSessions=[];activeId='';serverReady=false;newSession('',false);serverReady=true"), 'boot must create one blank local session before enabling persistence');
const boot = js.split('\n').find(line => line.startsWith('async function boot()')) || '';
assert(boot.includes('startFreshSession()'), 'boot must use the fresh-session path');
assert(!boot.includes('syncServerConversations()'), 'boot must not restore server conversations');
assert(js.includes("Object.values(sessions).some(s=>s.busy)"), 'reload guard must inspect every agent tab');
assert(html.includes('id="sessionsPage"') && html.includes('id="sessionsHistory"'), 'recent sessions SPA page is missing');
assert(html.includes('id="sessionsPrev"') && html.includes('id="sessionsNext"'), 'history pagination controls are missing');

console.log('cortex fresh-session and history navigation contract: ok');
