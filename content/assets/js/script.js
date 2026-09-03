const $=s=>document.querySelector(s);
let root='',providers=[],browserPath='',activeId='',sessions={},closedSessions=[],settings={},serverReady=false;
const serverSaveTimers=new Map();
const UI_STORE='cortex.ui.v1',COMPOSER_STORE='cortex.composer.height.v1';
const MAX_IMAGES=6,MAX_IMAGE_BYTES=10*1024*1024;
let attachments=[],modelCatalog=[],uiPrefs={activeId:'',collapsed:{}};
function toast(m){const t=$('#toast');t.textContent=m;t.classList.add('show');setTimeout(()=>t.classList.remove('show'),1800)}
async function api(url,opt={}){opt={...opt,headers:{...(opt.headers||{})}};const method=(opt.method||'GET').toUpperCase();if(!['GET','HEAD','OPTIONS'].includes(method)&&authState?.csrf)opt.headers['X-Cortex-CSRF']=authState.csrf;const r=await fetch(url,opt);if(!r.ok)throw Error((await r.text()).trim()||r.statusText);return r.headers.get('content-type')?.includes('json')?r.json():r.text()}
function sid(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+Math.random().toString(36).slice(2)}
function sessionTitle(s){if(s.title)return s.title;if(s.workspace)return s.workspace.split('/').filter(Boolean).pop()||s.workspace;return 'New session'}
function sessionSafe(s){return{id:s.id,workspace:s.workspace||'',workspaceStatus:s.workspaceStatus||'',title:s.title||'',provider:s.provider||'',model:s.model||'',openCodeSession:s.openCodeSession||'',currentRunId:s.currentRunId||'',state:s.busy?'running':(s.state||'idle'),createdAt:s.createdAt||Date.now(),updatedAt:Date.now(),archivedAt:s.archivedAt||s.closedAt||0,events:s.events||[],tasksCollapsed:!!s.tasksCollapsed}}
// sessionServerSafe is the browser-local UI view minus the task-panel collapsed
// preference, which is intentionally local-only and must not be sent to the
// server conversation endpoint (the Go decoder rejects unknown fields).
function sessionServerSafe(s){const o=sessionSafe(s);delete o.tasksCollapsed;return o}
function scheduleServerSave(s){
  if(!serverReady||!s?.id)return;
  clearTimeout(serverSaveTimers.get(s.id));
  serverSaveTimers.set(s.id,setTimeout(async()=>{serverSaveTimers.delete(s.id);try{await api('/api/conversation',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(sessionServerSafe(s))})}catch(e){toast('Conversation save failed · '+e.message)}},120))
}
// saveSessionToServer issues a PUT for exactly one conversation, coalescing
// rapid changes for the same id without fan-out to unrelated sessions.
function saveSessionToServer(id){const s=sessions[id]||closedSessions.find(x=>x.id===id);if(s)scheduleServerSave(s)}
// UI preferences are the only browser-local state Cortex keeps: the selected
// tab id and the per-session collapsed task-panel preference. Conversations,
// events and transcripts live in SQLite on the server and are never serialized
// to browser storage. Storage access and quota failures are caught so a full,
// disabled or throwing localStorage can never interrupt session operations.
function getPref(key){try{return localStorage.getItem(key)}catch(e){return null}}
function setPref(key,val){try{localStorage.setItem(key,val)}catch(e){}}
function loadUiPrefs(){
  uiPrefs={activeId:'',collapsed:{}};
  try{const x=JSON.parse(getPref(UI_STORE)||'{}');uiPrefs.activeId=typeof x.activeId==='string'?x.activeId:'';if(x.collapsed&&typeof x.collapsed==='object'&&!Array.isArray(x.collapsed))uiPrefs.collapsed=x.collapsed}catch{}
  // Remove the obsolete full-session payload (and its migration marker) so the
  // oversized blob no longer consumes the origin quota. It is never parsed or
  // imported into the authoritative server store.
  try{localStorage.removeItem('cortex.sessions.v1')}catch(e){}
  try{localStorage.removeItem('cortex.sessions.migrated.sqlite.v1')}catch(e){}
}
function saveUiPrefs(){setPref(UI_STORE,JSON.stringify({activeId,collapsed:uiPrefs.collapsed}))}
function saveSessions(){
  saveUiPrefs();
  if(!Object.keys(sessions).length)newSession('',false);
  renderAll();
}
function workspaceUnavailable(s){return s&&s.workspaceStatus&&s.workspaceStatus!=='available'}
function loadSessions(){
  loadUiPrefs();
  activeId=uiPrefs.activeId||'';
  // Sessions are loaded from the authoritative server store in
  // syncServerConversations; nothing here is read from browser storage.
  for(const s of Object.values(sessions)){if(uiPrefs.collapsed[s.id])s.tasksCollapsed=true}
}
function newSession(workspace='',render=true){
	if(Object.keys(sessions).length>=32){toast('Close a session before opening another.');return null}
  const id=sid();sessions[id]={id,workspace,title:'',openCodeSession:'',events:[],createdAt:Date.now(),busy:false,abort:null,followBottom:true,unread:0};activeId=id;saveSessions();saveSessionToServer(id);
  if(render)renderAll();
  return sessions[id]
}
function newSessionSameWorkspace(){const workspace=active()?.workspace||'';newSession(workspace);hideSessionMenu();if(!workspace)setTimeout(openWorkspacePicker,0)}
function newWorkspaceSession(){newSession('');hideSessionMenu();setTimeout(openWorkspacePicker,0)}
async function closeSession(id){
  const s=sessions[id];if(!s)return;
  if(s.busy&&!confirm('This agent is still working. Stop and close it?'))return;
  if(s.busy){
    // Submit the server-side stop, then wait for a durable terminal outcome via
    // bounded reconciliation. Rejection and unconfirmed results keep the tab;
    // an accepted-but-still-draining stop keeps the tab and reports pending.
    const res=await stopAgentFor(s);
    if(res&&res.rejected){toast('Could not stop the running agent; the session stays open.');return}
    if(res&&res.draining){toast('Stop accepted; the agent is still winding down.');return}
    if(res&&res.unconfirmed){toast('Could not confirm the stop; the agent is still running.');return}
    if(s.busy)await reconcileRunState(id);
  }else{s.abort?.abort()}
  const archived={...sessionSafe(s),archivedAt:Date.now(),closedAt:Date.now()};
  closedSessions=[archived,...closedSessions.filter(x=>x.id!==id)].slice(0,20);
  const fallbackWorkspace=s.workspace||'';
  delete sessions[id];
  if(!Object.keys(sessions).length){newSession(fallbackWorkspace,false)}
  if(activeId===id)activeId=Object.keys(sessions)[0];
  saveSessions();saveSessionToServer(id);renderAll()
}
function restoreSession(id){
  const i=closedSessions.findIndex(x=>x.id===id);if(i<0)return;
  const restored=closedSessions.splice(i,1)[0];
  sessions[id]={...restored,busy:false,abort:null,archivedAt:0};delete sessions[id].closedAt;
  activeId=id;saveSessions();saveSessionToServer(id);hideSessionMenu();renderAll()
}
function active(){return sessions[activeId]}
function renderTabs(){const box=$('#sessionTabs');box.innerHTML='';for(const s of Object.values(sessions)){const b=document.createElement('button');b.className='session-tab'+(s.id===activeId?' active':'');b.dataset.sessionId=s.id;const title=document.createElement('span');title.className='tab-title';title.textContent=sessionTitle(s);const spinner=document.createElement('span');spinner.className='tab-spinner'+(s.busy?'':' hidden');spinner.setAttribute('role','status');spinner.setAttribute('aria-label','Agent running');b.append(spinner,title);if(s.unread){const dot=document.createElement('span');dot.className='tab-unread';dot.textContent='●';dot.title=s.unread+' unseen events';b.append(dot)}const x=document.createElement('span');x.className='tab-close';x.textContent='×';x.onclick=e=>{e.stopPropagation();closeSession(s.id)};b.append(x);b.onclick=()=>{activeId=s.id;s.unread=0;saveUiPrefs();renderAll()};box.append(b)}}
function renderRestoreSessions(){
  const box=$('#restoreSessions');box.innerHTML='';
  $('#restoreDivider').hidden=!closedSessions.length;
  for(const s of closedSessions.slice(0,8)){
    const b=document.createElement('button');b.type='button';b.className='restore-session';
    const title=document.createElement('strong');title.textContent='Restore '+sessionTitle(s);
    const meta=document.createElement('span');meta.textContent=s.workspace||'No workspace';
    b.append(title,meta);b.onclick=()=>restoreSession(s.id);box.append(b)
  }
}
function toggleSessionMenu(){
  const menu=$('#newSessionMenu'),open=menu.hidden;
  if(open){renderRestoreSessions();menu.hidden=false}else menu.hidden=true
}
function hideSessionMenu(){$('#newSessionMenu').hidden=true}
function isToolEventText(t){const lines=String(t??'').split('\n');let h=lines[0]||'';if(/^Provider-reported tool event\s*$/.test(h))h=lines[1]||'';return /^↳ [A-Za-z0-9_.:-]+ · [A-Za-z0-9_.-]+$/.test(h)}
function renderTool(row,text){const lines=String(text??'').split('\n');let h=lines[0]||'',bodyLines=lines.slice(1);if(/^Provider-reported tool event\s*$/.test(h)){h=lines[1]||'';bodyLines=lines.slice(2)}const m=h.match(/^↳ ([A-Za-z0-9_.:-]+) · ([A-Za-z0-9_.-]+)$/);if(!m){row.textContent=text;return}const head=document.createElement('div');head.className='tool-head';const arrow=document.createElement('span');arrow.className='tool-arrow';arrow.textContent='↳ ';const name=document.createElement('span');name.className='tool-name';name.textContent=m[1];const sep=document.createElement('span');sep.className='tool-sep';sep.textContent=' · ';const st=document.createElement('span');st.className='tool-status';st.textContent=m[2];st.classList.add(m[2]);head.append(arrow,name,sep,st);row.append(head);if(bodyLines.length){const body=document.createElement('div');body.className='tool-body';body.textContent=bodyLines.join('\n');row.append(body)}}
function appendMarkdownInline(parent,text){
  // Deliberately small renderer: text is always emitted with textContent, so agent output
  // cannot inject HTML. Support only the Markdown that improves transcript scanning.
  // Underscore-delimited text is left literal because it matches ordinary identifiers.
  // Quoted strings support backslash escapes: the closing quote only terminates when
  // preceded by an even number of consecutive backslashes. A single-quoted block is only
  // recognized when its opening quote is immediately preceded by a space, so contractions
  // and possessives are never joined into a quote span; the leading space is emitted as
  // plain text and stays outside the highlighted element.
  const re=/(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|"(?:\\.|[^"\\])*"| '(?:\\.|[^'\\])*'|\[[^\]\n]+\]\(https?:\/\/[^)\s]+\))/g;
  let at=0,m;
  while((m=re.exec(text))){
    if(m.index>at)parent.append(document.createTextNode(text.slice(at,m.index)));
    const token=m[0];
    if(token.startsWith('`')){const el=document.createElement('code');el.textContent=token;parent.append(el)}
    else if(token.startsWith('**')){const el=document.createElement('strong');el.textContent=token;parent.append(el)}
    else if(token.startsWith('*')){const el=document.createElement('em');el.textContent=token;parent.append(el)}
    else if(token.startsWith('"')){const el=document.createElement('span');el.className='md-quote';el.textContent=token;parent.append(el)}
    else if(token.startsWith(" '")){parent.append(document.createTextNode(' '));const el=document.createElement('span');el.className='md-quote';el.textContent=token.slice(1);parent.append(el)}
    else{
      const lm=token.match(/^\[([^\]]+)\]\((https?:\/\/[^)]+)\)$/),a=document.createElement('a');
      a.textContent=lm[1];a.href=lm[2];a.target='_blank';a.rel='noopener noreferrer';parent.append(a)
    }
    at=m.index+token.length
  }
  if(at<text.length)parent.append(document.createTextNode(text.slice(at)))
}
function renderMarkdown(row,text){
  const lines=String(text).replace(/\r\n?/g,'\n').split('\n');
  let fence=null,list=null,quote=null;
  const flushList=()=>{list=null},flushQuote=()=>{quote=null};
  for(const line of lines){
    const fm=line.match(/^```([\w.+-]*)\s*$/);
    if(fm){
      flushList();flushQuote();
      if(fence){fence=null}
      else{const pre=document.createElement('pre'),code=document.createElement('code');if(fm[1])code.dataset.language=fm[1];pre.append(code);row.append(pre);fence=code}
      continue
    }
    if(fence){fence.textContent+=(fence.textContent?'\n':'')+line;continue}
    const hm=line.match(/^(#{1,4})\s+(.+)$/);
    if(hm){flushList();flushQuote();const h=document.createElement('div');h.className='md-heading md-h'+hm[1].length;appendMarkdownInline(h,hm[1]+' '+hm[2]);row.append(h);continue}
    const lm=line.match(/^\s*([-*+])\s+(.+)$/);
    if(lm){flushQuote();if(!list||list.tagName!=='UL'){list=document.createElement('ul');row.append(list)}const li=document.createElement('li');appendMarkdownInline(li,lm[2]);list.append(li);continue}
    const om=line.match(/^\s*(\d+)\.\s+(.+)$/);
    if(om){flushQuote();if(!list||list.tagName!=='OL'){list=document.createElement('ol');row.append(list)}const li=document.createElement('li');li.value=Number(om[1]);appendMarkdownInline(li,om[2]);list.append(li);continue}
    const qm=line.match(/^>\s?(.*)$/);
    if(qm){flushList();if(!quote){quote=document.createElement('blockquote');row.append(quote)}const p=document.createElement('div');appendMarkdownInline(p,qm[1]);quote.append(p);continue}
    flushList();flushQuote();
    if(!line.trim()){const gap=document.createElement('div');gap.className='md-gap';row.append(gap);continue}
    const p=document.createElement('div');p.className='md-line';appendMarkdownInline(p,line);row.append(p)
  }
}
function eventKind(ev){const raw=ev?.data?.data||ev?.data||{},t=String(raw.type||'');if(ev.type==='error')return'error';if(ev.type==='warning')return'warning';if(ev.type==='done')return'done';if(ev.type==='cancelled')return'error';return t.includes('tool')?'tool':'assistant'}
function eventNode(ev){const row=document.createElement('div');row.className='event '+ev.kind;row.dataset.kind=ev.kind;if(ev.kind==='tool'||(ev.kind==='assistant'&&isToolEventText(ev.text)))renderTool(row,ev.text);else if(ev.kind==='assistant')renderMarkdown(row,ev.text);else if(ev.kind==='image'){const fig=document.createElement('figure');fig.className='image-attach';const img=document.createElement('img');img.src=ev.text;img.alt=ev.name||'attached image';img.title=ev.name||'';fig.append(img);if(ev.name){const cap=document.createElement('figcaption');cap.textContent=ev.name;fig.append(cap)}row.append(fig)}else row.textContent=ev.text;const marker=decodeRunMarker(ev.name);if(marker&&(marker.outcome==='failed'||marker.outcome==='completed_with_process_error'||marker.outcome==='cancelled'||marker.outcome==='truncated'))row.append(technicalDetailsNode(marker.runID));return row}
function decodeRunMarker(name){const m=String(name||'').match(/^run:([^:]+):([a-z_]+)$/);if(!m)return null;return{runID:m[1],outcome:m[2]}}
function technicalDetailsNode(runID){const wrap=document.createElement('details');wrap.className='tech-details';const sum=document.createElement('summary');sum.textContent='Technical details';const body=document.createElement('div');body.className='tech-body';wrap.append(sum,body);wrap.addEventListener('toggle',async()=>{if(!wrap.open||body.dataset.loaded)return;body.dataset.loaded='1';try{const d=await api('/api/agent/run-diagnostics?runID='+encodeURIComponent(runID));const parts=[];if(d.category)parts.push('Category: '+d.category);if(d.exitCode)parts.push('Exit code: '+d.exitCode);if(d.signal)parts.push('Signal: '+d.signal);if(d.stderrTruncated)parts.push('stderr truncated');if(d.errors&&d.errors.length)parts.push('Errors:\n'+d.errors.join('\n'));if(d.warnings&&d.warnings.length)parts.push('Warnings:\n'+d.warnings.join('\n'));if(d.stderrTail)parts.push('stderr tail:\n'+d.stderrTail);body.textContent=parts.join('\n')||'No additional details.'}catch(e){body.textContent='Could not load details: '+e.message}});return wrap}
function nearBottom(box){if(!box)return true;return box.scrollHeight-box.scrollTop-box.clientHeight<40}
function followIfNeeded(s,box){if(!box)return;if(!s.followBottom&&!nearBottom(box)){return}box.scrollTop=box.scrollHeight;s.followBottom=true;$('#newActivity').hidden=true}
function updateNewActivity(s){const box=$('#feed');if(!box)return;if(nearBottom(box)){box.scrollTop=box.scrollHeight;s.followBottom=true;$('#newActivity').hidden=true}else if(!$('#newActivity')?.hidden&&!s.followBottom){/* already shown */}}
function renderFeed(){const s=active(),box=$('#feed');const wasNear=nearBottom(box);box.innerHTML='';if(!s.events.length){box.innerHTML='<div class="empty"><span class="orb">C</span><strong>What do you want to build?</strong><span>Choose a workspace, then give Cortex a development task.</span></div>';$('#newActivity').hidden=true;if($('#taskPanel')){$('#taskPanel').hidden=true}if($('#taskReopen')){$('#taskReopen').hidden=true}return}for(const ev of s.events){if(ev.kind==='task')continue;box.append(eventNode(ev))}if(wasNear){box.scrollTop=box.scrollHeight;s.followBottom=true;$('#newActivity').hidden=true}else if(s.followBottom){box.scrollTop=box.scrollHeight}renderTaskPanel(s);updateNewActivity(s)}// sessionSafe persists run state and current run id for the tab indicator.
function renderTaskPanel(s){const panel=$('#taskPanel');if(!panel)return;const list=$('#taskList');const reopen=$('#taskReopen');const latest=[...s.events].reverse().find(e=>e.kind==='task');if(!latest){list.innerHTML='';panel.hidden=true;if(reopen)reopen.hidden=true;return}let todos=[];try{todos=JSON.parse(latest.text||'[]')}catch{todos=null}if(!Array.isArray(todos)){panel.hidden=true;if(reopen)reopen.hidden=true;return}if(!todos.length){list.innerHTML='';$('#taskProgress').textContent='0 of 0 completed';panel.hidden=true;if(reopen)reopen.hidden=true;return}if(s.tasksCollapsed){panel.hidden=true;if(reopen)reopen.hidden=false;return}panel.hidden=false;if(reopen)reopen.hidden=true;list.innerHTML='';const done=todos.filter(t=>t.status==='completed').length;const order={in_progress:0,pending:1,completed:2};const sorted=[...todos].sort((a,b)=>(order[a.status]??3)-(order[b.status]??3));for(const t of sorted){const row=document.createElement('div');row.className='task-row '+t.status;const mark=document.createElement('span');mark.className='task-mark';mark.textContent=t.status==='completed'?'✓':t.status==='in_progress'?'●':'·';mark.setAttribute('aria-hidden','true');const label=document.createElement('span');label.className='task-label';label.textContent=t.content;row.append(mark,label);list.append(row)}$('#taskProgress').textContent=`${done} of ${todos.length} completed`}
function renderSession(){const s=active();$('#workspaceLabel').textContent=s.workspace||'Choose workspace';$('#run').disabled=s.busy||!s.workspace||workspaceUnavailable(s);$('#prompt').disabled=s.busy;$('#stop').hidden=!s.busy;$('#activity').hidden=!s.busy;$('#wsUnavailable').hidden=!workspaceUnavailable(s);$('#wsUnavailable').textContent=workspaceUnavailable(s)?('Workspace unavailable ('+s.workspaceStatus+') — choose a replacement to run.'):'';renderFeed()}
function renderAll(){renderTabs();renderSession()}
function addEvent(id,kind,text,name='',extra={}){const clean=String(text??'').trim();if(!clean)return;const s=sessions[id];if(!s)return;const ev={kind,text:clean,name,...extra};s.events.push(ev);if(s.events.length>500)s.events=s.events.slice(-500);saveSessionToServer(id);if(activeId===id){if(kind==='task'){renderTaskPanel(s);return}$('#feed .empty')?.remove();const wasNear=nearBottom($('#feed'));$('#feed').append(eventNode(ev));if(wasNear||s.followBottom){$('#feed').scrollTop=$('#feed').scrollHeight;s.followBottom=true;$('#newActivity').hidden=true}else{$('#newActivity').hidden=false}}else{if(kind==='task')return;s.unread=(s.unread||0)+1;renderTabs()}}
async function loadSettings(){settings=await api('/api/settings');providers=settings.providers||[];const sel=$('#provider');sel.innerHTML='';for(const p of providers){const o=document.createElement('option');o.value=p.id;o.textContent=p.label+(p.configured?' · configured':'');sel.append(o)}sel.value=settings.activeProvider||providers[0]?.id||'';updateProviderUI()}
function currentModel(){
  const sel=$('#model');
  return sel.value==='__custom__'?$('#modelCustom').value.trim():sel.value||''
}
function renderModelSelect(p,useDefault){
  const sel=$('#model');
  const base=useDefault?(p.model||p.defaultModel||''):currentModel();
  const opts=[...new Set([...(p.model?[p.model]:[]),...(p.defaultModel?[p.defaultModel]:[]),...modelCatalog])];
  sel.innerHTML='';
  for(const m of opts){const o=document.createElement('option');o.value=m;o.textContent=m;sel.append(o)}
  const custom=document.createElement('option');custom.value='__custom__';custom.textContent='Custom model…';sel.append(custom);
  const val=opts.includes(base)?base:'';
  sel.value=val;
  const isCustom=!val;
  $('#modelCustom').hidden=!isCustom;
  if(isCustom)$('#modelCustom').value=base
}
function updateProviderUI(){
  const p=providers.find(x=>x.id===$('#provider').value);if(!p)return;
  modelCatalog=[];
  renderModelSelect(p,true);
  $('#apiKey').value='';
  const auth=p.authMode==='opencode-auth';
  $('#keyLabel').hidden=auth;$('#deleteKey').hidden=auth;
  $('#apiKey').placeholder=p.configured?'Key stored · enter a new key to replace':'Paste API key';
  $('#providerNote').textContent=auth
    ? (p.configured
        ? `Connected through OpenCode on this host. Cortex copies that ${p.label} OAuth credential into the isolated session.`
        : `Not connected yet. Run: opencode auth login --provider ${p.openCodeID} on the Cortex host, then reopen Settings.`)
    : `Cortex passes this key only to the isolated OpenCode process. Model IDs use OpenCode's ${p.openCodeID}/… catalog.`;
  loadModels(p.id)
}
async function loadModels(providerID,quiet=true){
  try{
    const x=await api('/api/agent/models?provider='+encodeURIComponent(providerID));
    modelCatalog=x.models||[];
    const p=providers.find(q=>q.id===$('#provider').value);
    if(p&&p.id===providerID)renderModelSelect(p,false);
    if(!quiet)toast(modelCatalog.length?modelCatalog.length+' models loaded':'No models available')
  }catch(e){if(!quiet)toast('Could not load models · '+e.message)}
}
async function saveKey(remove=false){
  const provider=$('#provider').value,p=providers.find(x=>x.id===provider),key=$('#apiKey').value.trim(),model=currentModel();
  if(!model)return toast('Enter a model ID');
  await api('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider,key,model,removeKey:remove})});
  await loadSettings();await agentStatus();toast(remove?'Key removed':'Provider saved')
}
async function agentStatus(){try{
  const s=await api('/api/agent/status');$('#dot').classList.toggle('ready',s.available);
  $('#agentModel').textContent=`${s.providerLabel||s.provider} · ${s.model||''} · through OpenCode`;
  $('#agentState').textContent=!s.opencodeInstalled?'OpenCode not installed':!s.credentialAvailable?(s.authMode==='opencode-auth'?'OpenCode login required':'API key required'):'Ready'
}catch(e){$('#agentState').textContent=e.message}}
function clipped(v,n=2600){const s=String(v??'').trim();return s.length>n?s.slice(0,n)+'\n…':s}
function readDataURL(file){return new Promise((res,rej)=>{const fr=new FileReader();fr.onload=()=>res(fr.result);fr.onerror=()=>rej(fr.error);fr.readAsDataURL(file)})}
function thumbnail(dataUrl){return new Promise((res,rej)=>{const img=new Image();img.onload=()=>{const max=512;let w=img.width,h=img.height;if(w>max||h>max){const r=Math.min(max/w,max/h);w=Math.round(w*r);h=Math.round(h*r)}const c=document.createElement('canvas');c.width=w;c.height=h;const ctx=c.getContext('2d');ctx.fillStyle='#111312';ctx.fillRect(0,0,w,h);ctx.drawImage(img,0,0,w,h);res(c.toDataURL('image/jpeg',.8))};img.onerror=()=>rej(new Error('Could not read image'));img.src=dataUrl})}
function addImage(file){
  if(!file.type||!file.type.startsWith('image/')){toast('Only image files can be attached');return false}
  if(file.size>MAX_IMAGE_BYTES){toast('Image too large · max 10 MiB');return false}
  if(sessions[activeId]?.busy){toast('Agent is running');return false}
  if(attachments.length>=MAX_IMAGES){toast('Limit of '+MAX_IMAGES+' images per run');return false}
  readDataURL(file).then(dataUrl=>thumbnail(dataUrl).then(thumb=>{
    if(attachments.length>=MAX_IMAGES){toast('Limit of '+MAX_IMAGES+' images per run');return}
    attachments.push({id:sid(),name:file.name||'image',dataUrl,thumb,size:file.size});
    renderAttachments()
  })).catch(()=>toast('Could not read image'));
  return true
}
function fmtSize(n){if(n>1<<20)return (n/(1<<20)).toFixed(1)+' MiB';if(n>1024)return Math.round(n/1024)+' KiB';return n+' B'}
function renderAttachments(){
  const box=$('#attachments');box.innerHTML='';
  if(!attachments.length){box.hidden=true;return}
  for(const a of attachments){
    const chip=document.createElement('div');chip.className='attachment-chip';
    const img=document.createElement('img');img.src=a.thumb;img.alt=a.name;
    const meta=document.createElement('div');meta.className='attach-meta';
    const name=document.createElement('span');name.className='attach-name';name.textContent=a.name;name.title=a.name;
    const size=document.createElement('span');size.className='attach-size';size.textContent=fmtSize(a.size);
    const x=document.createElement('button');x.type='button';x.title='Remove image';x.textContent='×';
    x.onclick=()=>{attachments=attachments.filter(y=>y.id!==a.id);renderAttachments()};
    meta.append(name,size);chip.append(img,meta,x);box.append(chip)
  }
  box.hidden=false
}
function extractImages(raw){
  const out=[];const add=(u,n)=>{if(u&&!out.some(x=>x.url===u))out.push({url:u,name:n})};
  const part=p=>{
    if(!p||typeof p!=='object')return;
    if((p.type==='file'||p.type==='image')&&String(p.mediaType||'').startsWith('image/'))add(p.url,p.filename);
    const atts=p.state?.attachments||p.attachments;
    if(Array.isArray(atts))for(const a of atts){if(a&&String(a.mime||a.mediaType||'').startsWith('image/'))add(a.url,a.filename)}
  };
  part(raw?.part);
  if(Array.isArray(raw?.parts))raw.parts.forEach(part);
  if(Array.isArray(raw?.info?.parts))raw.info.parts.forEach(part);
  return out
}
async function addGeneratedImage(id,im){
  let url=im.url;
  try{
    if(url.startsWith('/api/')){const blob=await (await fetch(url)).blob();const data=await new Promise((res,rej)=>{const fr=new FileReader();fr.onload=()=>res(fr.result);fr.onerror=()=>rej(fr.error);fr.readAsDataURL(blob)});url=await thumbnail(data)}
    else if(url.startsWith('data:'))url=await thumbnail(url);
    else return;
  }catch{return}
  addEvent(id,'image',url,im.name||'Generated image')
}
function toolText(raw){const p=raw.part||raw,st=p.state||{},tool=p.tool||raw.tool||raw.name||'tool',status=st.status||'';const input=st.input||p.input||{},lines=[`↳ ${tool}${status?' · '+status:''}`];if(tool==='bash'&&input.command)lines.push('$ '+input.command);else if(input.filePath||input.path)lines.push(input.filePath||input.path);if(st.error)lines.push('ERROR: '+clipped(typeof st.error==='string'?st.error:JSON.stringify(st.error)));else if(st.output)lines.push(clipped(st.output));return lines.join('\n')}
function summarize(ev){const raw=ev?.data?.data||ev?.data||{},type=String(raw.type||'');if(ev.type==='error')return raw.message||'Agent failed';if(ev.type==='warning')return raw.message||'The agent completed, but the process reported a problem.';if(ev.type==='truncated')return raw.message||'Provider output was truncated.';if(ev.type==='cancelled')return raw.message||'Agent stopped.';if(ev.type==='recovered')return raw.text||'';if(ev.type==='done'){const i=raw.inputTokens||0,o=raw.outputTokens||0;return i||o?`Done · ${i} input · ${o} output tokens`:'Done'};if(ev.type==='output')return raw.text||'';if(type.includes('tool'))return toolText(raw);return raw.part?.text||raw.text||''}
// reconcileRunState polls the authoritative conversation state after a stream
// ends without a durable terminal event (disconnect/timeout) so the running
// spinner reflects the server, not the vanished browser request. It polls with
// bounded backoff until the run is terminal, the session is removed/archived, a
// newer run replaces it, or the documented timeout is reached. Concurrent
// polling for the same session is deduplicated.
const reconcilePolls=new Map();
// RECONCILE_MAX_WAIT_MS and RECONCILE_STOP_WAIT_MS are overridable through the
// window for tests (e.g. window.__RECONCILE_MAX_WAIT_MS) to keep suites fast.
const RECONCILE_MAX_WAIT_MS=(typeof window!=='undefined'&&window.__RECONCILE_MAX_WAIT_MS)||30000;
const RECONCILE_STOP_WAIT_MS=(typeof window!=='undefined'&&window.__RECONCILE_STOP_WAIT_MS)||5000;
async function reconcileRunState(id, maxWaitMs){
  maxWaitMs=maxWaitMs||RECONCILE_MAX_WAIT_MS;
  if(!sessions[id]||reconcilePolls.has(id))return;
  reconcilePolls.set(id,true);
  try{
    const start=Date.now();
    const delay=ms=>new Promise(r=>setTimeout(r,ms));
    // Capture the run being reconciled separately so the session's mutable
    // currentRunId is never mistaken for the reconciliation target.
    const targetRunId=sessions[id].currentRunId||sessions[id].runID||'';
    while(Date.now()-start<maxWaitMs){
      const s=sessions[id];
      if(!s)return; // session removed
      try{
        const all=await api('/api/conversations');
        const rec=all.find(c=>c.id===id);
        if(rec){
          const authoritativeRunId=rec.currentRunId||'';
          const authoritativeRunning=rec.state==='running';
          const authoritativeTerminal=rec.state!=='running'&&rec.state!=='idle'&&rec.state!=='';
          s.state=rec.state||s.state;
          // A newer run superseded the reconciled run. Apply the authoritative
          // state of the newer run: running keeps the spinner; terminal clears
          // it; idle/unknown follows the explicit contract below.
          if(authoritativeRunId&&authoritativeRunId!==targetRunId){
            s.currentRunId=authoritativeRunId;
            if(authoritativeRunning){s.busy=true;saveUiPrefs();saveSessionToServer(id);if(activeId===id)renderAll();return}
            if(authoritativeTerminal){s.busy=false;saveUiPrefs();saveSessionToServer(id);if(activeId===id)renderAll();return}
            // idle/unknown newer run: keep polling the authoritative state.
            await delay(1000);
            continue;
          }
          s.currentRunId=authoritativeRunId||s.currentRunId;
          if(authoritativeRunning){s.busy=true}
          else if(authoritativeTerminal){s.busy=false;saveUiPrefs();saveSessionToServer(id);if(activeId===id)renderAll();return}
        }
      }catch{}
      if(sessions[id]&&!sessions[id].busy)return; // became terminal meanwhile
      await delay(Math.min(1500,Date.now()-start+250));
    }
  }finally{reconcilePolls.delete(id)}
}
function consumeAgentEvent(id,s,ev,seenImages){const text=summarize(ev),raw=ev?.data?.data||ev?.data||{};if(ev.type==='run'&&raw.runID){s.runID=raw.runID;s.currentRunId=raw.runID}if(ev.type==='done'&&raw.sessionID)s.openCodeSession=raw.sessionID;if(ev.type==='task'&&typeof raw.snapshot==='string')addEvent(id,'task',raw.snapshot);const oc=raw.outcome||'';if(oc)s.state=oc;else if(ev.type==='done')s.state='completed';else if(ev.type==='warning')s.state='completed_with_process_error';else if(ev.type==='error')s.state='failed';else if(ev.type==='cancelled')s.state='cancelled';else if(ev.type==='truncated')s.state='truncated';if(ev.type==='done'||ev.type==='warning'||ev.type==='error'||ev.type==='cancelled'||ev.type==='truncated'){s.busy=false;s.abort=null};const termEvent={kind:eventKind(ev),text,name:'run:'+(raw.runID||s.runID)+':'+oc,outcome:oc,runID:raw.runID||s.runID};if(termEvent.text)addEvent(id,termEvent.kind,termEvent.text,termEvent.name,{outcome:oc,runID:termEvent.runID});if(ev.type==='recovered-images'){for(const im of (raw.images||[]))addGeneratedImage(id,im)}for(const im of extractImages(raw)){if(seenImages.has(im.url))continue;seenImages.add(im.url);addGeneratedImage(id,im)}}
async function runAgent(prompt){const s=active();if(!s||s.busy||!s.workspace||workspaceUnavailable(s))return;const id=s.id,p=providers.find(x=>x.id===settings.activeProvider);s.provider=settings.activeProvider||'';s.model=p?.model||p?.defaultModel||'';s.busy=true;s.abort=new AbortController();s.runID='';if(!s.title)s.title=prompt.split(/\s+/).slice(0,5).join(' ');const images=attachments.map(a=>({name:a.name,data:a.dataUrl}));for(const a of attachments)addEvent(id,'image',a.thumb,a.name);addEvent(id,'user',prompt);attachments=[];renderAttachments();renderAll();try{const r=await fetch('/api/agent/run',{method:'POST',headers:{'Content-Type':'application/json','X-Cortex-CSRF':authState.csrf||''},body:JSON.stringify({workspace:s.workspace,prompt,session:s.openCodeSession||'',clientSession:id,images}),signal:s.abort.signal});if(!r.ok)throw Error((await r.text()).trim()||r.statusText);const rd=r.body.getReader(),dec=new TextDecoder();let buf='';const seenImages=new Set();for(;;){const {value,done}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf('\n'))>=0){const line=buf.slice(0,i).trim();buf=buf.slice(i+1);if(!line)continue;const ev=JSON.parse(line);consumeAgentEvent(id,s,ev,seenImages);}}}catch(e){addEvent(id,'error',e.name==='AbortError'?'Agent stopped.':e.message)}finally{if(sessions[id]){sessions[id].abort=null;saveUiPrefs();saveSessionToServer(id);if(sessions[id]&&sessions[id].busy){reconcileRunState(id)}}if(activeId===id)renderAll();agentStatus()}}

// stopAgent performs a server-side, authenticated Stop for the active run and
// surfaces the distinct outcome: terminal stop, explicit rejection, accepted-
// but-draining, or an unconfirmed (timeout/transport) result.
async function stopAgent(){
  const s=active();if(!s)return;
  const res=await stopAgentFor(s);
  if(res&&res.rejected)toast('Could not stop the running agent.');
  else if(res&&res.draining)toast('Stop accepted; the agent is still winding down.');
  else if(res&&res.unconfirmed)toast('Could not confirm the stop; the agent is still running.');
  else toast('Agent stopped.');
}
// requestCancel sends the authenticated cancel request and reports whether it
// was acknowledged, explicitly rejected, or unconfirmed (timeout/transport).
const CANCEL_TIMEOUT_MS=(typeof window!=='undefined'&&window.__CANCEL_TIMEOUT_MS)||2000;
const STOP_SETTLE_MS=(typeof window!=='undefined'&&window.__STOP_SETTLE_MS)||1500;
async function requestCancel(runID){
  let res;
  try{
    res=await Promise.race([
      fetch('/api/agent/cancel',{method:'POST',headers:{'Content-Type':'application/json','X-Cortex-CSRF':authState?.csrf||''},body:JSON.stringify({runID})}),
      new Promise((_,rej)=>setTimeout(()=>rej(Error('cancel timeout')),CANCEL_TIMEOUT_MS))
    ]);
  }catch{return {unconfirmed:true}}
  if(res&&res.ok)return {acknowledged:true};
  return {rejected:true};
}
// stopAgentFor submits the server-side stop and waits for the run to reach a
// durable terminal state via bounded reconciliation. It resolves to
// {ok:true} when the run became terminal, {ok:false,rejected:true} when the
// server explicitly refused cancellation, {ok:false,draining:true} when the
// stop was acknowledged but the run is still draining, and
// {ok:false,unconfirmed:true} when the cancel request timed out or failed and
// the run is still running (never described as an explicit rejection).
async function stopAgentFor(s){
  if(!s?.busy)return {ok:true};
  const abortFallback=()=>s.abort?.abort();
  if(!s.runID){abortFallback();return {ok:false,rejected:true}}
  const cancelResult=await requestCancel(s.runID);
  if(cancelResult.rejected)return {ok:false,rejected:true};
  const acknowledged=cancelResult.acknowledged===true;
  // Wait briefly for the durable terminal event; if it did not arrive, poll
  // the server with a bounded budget.
  await new Promise(res=>setTimeout(res,STOP_SETTLE_MS));
  if(s.busy){
    await reconcileRunState(s.id||activeId, RECONCILE_STOP_WAIT_MS);
    if(s.busy){
      if(acknowledged)return {ok:false,draining:true};
      return {ok:false,unconfirmed:true};
    }
  }
  return {ok:true};
}

function joinPath(base,name){return (base.replace(/\/+$/,'')+'/'+name).replace(/\/+/g,'/')}
function parentPath(p){if(p===root)return root;const x=p.replace(/\/+$/,'');const i=x.lastIndexOf('/');const out=i<=0?'/':x.slice(0,i);return out.length<root.length?root:out}
async function browse(path){
  browserPath=path||root;
  $('#browserPath').textContent=browserPath;
  $('#browserUp').disabled=browserPath===root;
  const list=await api('/api/files?path='+encodeURIComponent(browserPath));
  const box=$('#browserDirs');box.innerHTML='';
  const dirs=list.filter(x=>x.dir);
  if(!dirs.length){box.innerHTML='<div class="browser-empty">No subdirectories</div>';return}
  for(const d of dirs){
    const b=document.createElement('button');b.className='browser-dir';b.type='button';
    b.innerHTML='<span class="browser-dir-chevron">›</span><span class="browser-dir-name"></span>';
    b.querySelector('.browser-dir-name').textContent=d.name;
    b.title='Open '+d.name;
    b.onclick=()=>browse(joinPath(browserPath,d.name));
    box.append(b)
  }
}
async function openWorkspacePicker(){const s=active();$('#workspaceModal').hidden=false;try{await browse(s.workspace||root)}catch(e){toast(e.message)}}
function chooseWorkspace(){const s=active();s.workspace=browserPath;s.openCodeSession='';s.title='';s.events=[];s.workspaceStatus='available';saveUiPrefs();saveSessionToServer(activeId);$('#workspaceModal').hidden=true;renderAll();toast('Workspace selected')}
async function copySession(){const s=active(),labels={user:'You',assistant:'Agent',tool:'Tool',error:'Error',done:'Status'},text=s.events.filter(x=>x.kind!=='image').map(x=>`${labels[x.kind]||'Agent'}:\n${x.text}`).join('\n\n');if(!text)return toast('Nothing to copy');await navigator.clipboard.writeText(text);toast('Session copied')}
function applyComposerHeight(value){
  const card=document.querySelector('.agent-card');if(!card)return;
  const max=Math.max(170,Math.floor(card.getBoundingClientRect().height*.55));
  const height=Math.max(112,Math.min(max,Number(value)||150));
  card.style.setProperty('--composer-height',height+'px');
  return height
}
function restoreComposerHeight(){applyComposerHeight(getPref(COMPOSER_STORE)||150)}
function startComposerResize(e){
  if(e.button!==0)return;e.preventDefault();
  const form=$('#agentForm'),startY=e.clientY,startHeight=form.getBoundingClientRect().height;
  document.body.classList.add('resizing-composer');
  const move=ev=>applyComposerHeight(startHeight+(startY-ev.clientY));
  const up=ev=>{
    document.removeEventListener('pointermove',move);document.removeEventListener('pointerup',up);
    document.body.classList.remove('resizing-composer');
    setPref(COMPOSER_STORE,String(Math.round($('#agentForm').getBoundingClientRect().height)))
  };
  document.addEventListener('pointermove',move);document.addEventListener('pointerup',up)
}
// syncServerConversations loads the authoritative conversation store from the
// server (SQLite) after authentication. It never reads, writes or imports
// browser-local transcripts: sessions, events, titles, workspace association,
// provider identifiers, run state and archived state all come from the server.
// Only the compact UI preferences (selected tab, collapsed task panels) are
// applied locally, and loading authoritative records never PUTs them back.
async function syncServerConversations(){
  const collapsed=uiPrefs.collapsed||{};
  const stored=await api('/api/conversations');
  sessions={};closedSessions=[];
  for(const s of stored){s.abort=null;s.currentRunId=s.currentRunId||'';if(s.archivedAt){s.closedAt=s.archivedAt;closedSessions.push(s)}else sessions[s.id]=s}
  // Derive authoritative running state from the server, not browser-local
  // busy: a conversation whose state is still 'running' has a live server run.
  for(const s of Object.values(sessions)){s.busy=(s.state==='running');if(s.followBottom===undefined)s.followBottom=true;if(collapsed[s.id])s.tasksCollapsed=true}
  serverReady=true;
  if(!Object.keys(sessions).length)newSession('',false);
  if(!sessions[activeId])activeId=Object.keys(sessions)[0];
  saveUiPrefs();renderAll()
}
async function boot(){const st=await api('/api/status');root=st.root;loadSessions();restoreComposerHeight();await Promise.all([loadSettings(),agentStatus(),syncServerConversations()]);if(!active()?.workspace)setTimeout(openWorkspacePicker,0)}
$('#newSession').onclick=e=>{e.stopPropagation();toggleSessionMenu()};$('#newSameWorkspace').onclick=newSessionSameWorkspace;$('#newWorkspace').onclick=newWorkspaceSession;$('#workspaceBtn').onclick=openWorkspacePicker;$('#closeWorkspace').onclick=$('#cancelWorkspace').onclick=()=>$('#workspaceModal').hidden=true;$('#browserUp').onclick=()=>browse(parentPath(browserPath));$('#chooseWorkspace').onclick=chooseWorkspace;
$('#composerResize').onpointerdown=startComposerResize;$('#composerResize').ondblclick=()=>{applyComposerHeight(150);setPref(COMPOSER_STORE,'150')};document.addEventListener('click',e=>{if(!e.target.closest('.new-session-wrap'))hideSessionMenu()});window.addEventListener('resize',()=>applyComposerHeight($('#agentForm').getBoundingClientRect().height));
$('#agentForm').onsubmit=e=>{e.preventDefault();const p=$('#prompt').value.trim();if(!p)return;$('#prompt').value='';runAgent(p)};$('#prompt').onkeydown=e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();$('#agentForm').requestSubmit()}};$('#stop').onclick=()=>stopAgent();$('#copy').onclick=copySession;
$('#newActivity').onclick=()=>{const s=active();if(!s)return;const box=$('#feed');if(box){box.scrollTop=box.scrollHeight}s.followBottom=true;$('#newActivity').hidden=true};
function setTasksCollapsed(c){const s=active();if(!s)return;s.tasksCollapsed=c;if(c)uiPrefs.collapsed[s.id]=true;else delete uiPrefs.collapsed[s.id];saveUiPrefs();renderTaskPanel(s)}$('#taskToggle').onclick=()=>setTasksCollapsed(true);$('#taskReopen').onclick=()=>setTasksCollapsed(false);
$('#feed').addEventListener('scroll',()=>{const s=active(),box=$('#feed');if(!box||!s)return;if(box.scrollTop+box.clientHeight>=box.scrollHeight-4){s.followBottom=true;$('#newActivity').hidden=true}else{s.followBottom=false}});
document.addEventListener('selectionchange',()=>{const s=active();if(!s)return;s.followBottom=false});
$('#prompt').addEventListener('paste',e=>{const files=[...(e.clipboardData?.items||[])].map(i=>i.getAsFile()).filter(Boolean).filter(f=>f.type&&f.type.startsWith('image/'));if(!files.length)return;let handled=false;for(const f of files)handled=addImage(f)||handled;if(handled)e.preventDefault()});
document.addEventListener('dragover',e=>{if([...(e.dataTransfer?.types||[])].includes('Files'))e.preventDefault()});
document.addEventListener('drop',e=>{const files=[...(e.dataTransfer?.files||[])].filter(f=>f.type&&f.type.startsWith('image/'));if(!files.length)return;e.preventDefault();for(const f of files)addImage(f)});
$('#settingsBtn').onclick=()=>{$('#settingsModal').hidden=false;loadSettings()};$('#closeSettings').onclick=()=>$('#settingsModal').hidden=true;$('#settingsModal').onclick=e=>{if(e.target===$('#settingsModal'))$('#settingsModal').hidden=true};$('#workspaceModal').onclick=e=>{if(e.target===$('#workspaceModal'))$('#workspaceModal').hidden=true};$('#provider').onchange=updateProviderUI;$('#model').onchange=()=>{if($('#model').value==='__custom__'){$('#modelCustom').hidden=false;$('#modelCustom').focus()}else $('#modelCustom').hidden=true};$('#saveKey').onclick=()=>saveKey(false);$('#deleteKey').onclick=()=>saveKey(true);$('#refreshModels').onclick=()=>{const p=providers.find(x=>x.id===$('#provider').value);if(p)loadModels(p.id,false)};
async function bootAuthenticated(){if(!await loadAuthState())return;await boot()}
// Authentication and self-hosted security settings.
let authState={};
async function loadAuthState(){
  authState=await api('/api/auth/state');
  const gate=$('#authGate');
  if(!authState.configured){
    gate.hidden=false;$('#setupForm').hidden=false;$('#loginForm').hidden=true;
    $('#loginTOTPLabel').hidden=true;$('#googleLogin').hidden=true;
    $('#authTitle').textContent='Secure Cortex';$('#authIntro').textContent='Set a password before using this Cortex instance.';requestAnimationFrame(()=>$('#setupPassword').focus());return false
  }
  if(!authState.authenticated){
    gate.hidden=false;$('#setupForm').hidden=true;$('#loginForm').hidden=false;
    $('#authTitle').textContent='Sign in to Cortex';$('#authIntro').textContent='Unlock this Cortex instance to continue.';
    $('#loginTOTPLabel').hidden=!authState.totpEnabled;
    $('#googleLogin').hidden=!(authState.googleEnabled&&authState.googleConfigured);requestAnimationFrame(()=>$('#loginPassword').focus());return false
  }
  gate.hidden=true;return true
}
function authError(e){$('#authError').textContent=e?.message||String(e)}
$('#setupForm').onsubmit=async e=>{e.preventDefault();$('#authError').textContent='';try{await api('/api/auth/setup',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:$('#setupPassword').value,confirm:$('#setupConfirm').value})});await bootAuthenticated()}catch(x){authError(x)}};
$('#loginForm').onsubmit=async e=>{e.preventDefault();$('#authError').textContent='';try{await api('/api/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:$('#loginPassword').value,TOTP:$('#loginTOTP').value})});await bootAuthenticated()}catch(x){authError(x)}};
function showSettingsTab(name){const security=name==='security';$('#providerSettings').hidden=security;$('#securitySettings').hidden=!security;$('#providerTab').classList.toggle('active',!security);$('#securityTab').classList.toggle('active',security);if(security)loadSecuritySettings()}
$('#providerTab').onclick=()=>showSettingsTab('providers');$('#securityTab').onclick=()=>showSettingsTab('security');
async function loadSecuritySettings(){authState=await api('/api/auth/state');$('#totpState').textContent=authState.totpEnabled?'Two-factor authentication is enabled.':'Two-factor authentication is off.';$('#startTOTP').hidden=authState.totpEnabled;$('#disableTOTP').hidden=!authState.totpEnabled;$('#totpSetup').hidden=true;$('#googleEnabled').checked=!!authState.googleEnabled;$('#googleClientID').value=authState.googleClientID||'';$('#googleEmail').value=authState.googleEmail||'';$('#googleRedirect').value=location.origin+'/api/auth/google/callback'}
$('#changePassword').onclick=async()=>{try{await api('/api/auth/password',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({Current:$('#currentPassword').value,Password:$('#newPassword').value,Confirm:$('#confirmPassword').value})});await loadAuthState();$('#currentPassword').value=$('#newPassword').value=$('#confirmPassword').value='';toast('Password changed')}catch(e){toast(e.message)}};
$('#startTOTP').onclick=async()=>{try{const x=await api('/api/auth/totp/begin',{method:'POST'});$('#totpSecret').value=x.secret;$('#totpSetup').hidden=false}catch(e){toast(e.message)}};
$('#enableTOTP').onclick=async()=>{try{await api('/api/auth/totp/enable',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({code:$('#totpCode').value})});$('#totpCode').value='';await loadSecuritySettings();toast('Two-factor authentication enabled')}catch(e){toast(e.message)}};
$('#disableTOTP').onclick=async()=>{const password=prompt('Enter your Cortex password to disable two-factor authentication');if(!password)return;try{await api('/api/auth/totp/disable',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password})});await loadSecuritySettings();toast('Two-factor authentication disabled')}catch(e){toast(e.message)}};
$('#saveGoogle').onclick=async()=>{try{await api('/api/auth/google',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:$('#googleEnabled').checked,ClientID:$('#googleClientID').value,ClientSecret:$('#googleClientSecret').value,Email:$('#googleEmail').value})});$('#googleClientSecret').value='';await loadSecuritySettings();toast('Google auth saved')}catch(e){toast(e.message)}};
$('#logout').onclick=async()=>{await api('/api/auth/logout',{method:'POST'});location.reload()};

loadAuthState().then(ok=>{if(ok)return boot()}).catch(e=>toast(e.message));
