const $=s=>document.querySelector(s);
let root='',providers=[],browserPath='',activeId='',sessions={},settings={};
const STORE='cortex.sessions.v1';
function toast(m){const t=$('#toast');t.textContent=m;t.classList.add('show');setTimeout(()=>t.classList.remove('show'),1800)}
async function api(url,opt={}){const r=await fetch(url,opt);if(!r.ok)throw Error((await r.text()).trim()||r.statusText);return r.headers.get('content-type')?.includes('json')?r.json():r.text()}
function sid(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+Math.random().toString(36).slice(2)}
function sessionTitle(s){if(s.title)return s.title;if(s.workspace)return s.workspace.split('/').filter(Boolean).pop()||s.workspace;return 'New session'}
function saveSessions(){const safe={};for(const [id,s] of Object.entries(sessions))safe[id]={id:s.id,workspace:s.workspace,title:s.title,openCodeSession:s.openCodeSession||'',events:s.events||[]};localStorage.setItem(STORE,JSON.stringify({activeId,sessions:safe}))}
function loadSessions(){try{const x=JSON.parse(localStorage.getItem(STORE)||'{}');sessions=x.sessions||{};activeId=x.activeId||''}catch{}if(!Object.keys(sessions).length)newSession();if(!sessions[activeId])activeId=Object.keys(sessions)[0]}
function newSession(workspace=''){const id=sid();sessions[id]={id,workspace,title:'',openCodeSession:'',events:[],busy:false,abort:null};activeId=id;saveSessions();renderAll()}
function closeSession(id){const s=sessions[id];if(s?.busy&&!confirm('This agent is still working. Stop and close it?'))return;s?.abort?.abort();delete sessions[id];if(!Object.keys(sessions).length){newSession();return}if(activeId===id)activeId=Object.keys(sessions)[0];saveSessions();renderAll()}
function active(){return sessions[activeId]}
function renderTabs(){const box=$('#sessionTabs');box.innerHTML='';for(const s of Object.values(sessions)){const b=document.createElement('button');b.className='session-tab'+(s.id===activeId?' active':'');const title=document.createElement('span');title.className='tab-title';title.textContent=sessionTitle(s);const x=document.createElement('span');x.className='tab-close';x.textContent='×';x.onclick=e=>{e.stopPropagation();closeSession(s.id)};b.append(title,x);b.onclick=()=>{activeId=s.id;saveSessions();renderAll()};box.append(b)}}
function renderTool(row,text){const lines=text.split('\n'),m=lines[0].match(/^↳ ([\w.:-]+) · (completed|error|running|pending)$/);if(!m){row.textContent=text;return}const h=document.createElement('div');h.className='tool-head';h.innerHTML='<span class="tool-arrow">↳ </span><span class="tool-name"></span><span> · </span><span class="tool-status"></span>';h.querySelector('.tool-name').textContent=m[1];const st=h.querySelector('.tool-status');st.textContent=m[2];st.classList.add(m[2]);row.append(h);if(lines.length>1){const body=document.createElement('div');body.className='tool-body';body.textContent=lines.slice(1).join('\n');row.append(body)}}
function eventNode(ev){const row=document.createElement('div');row.className='event '+ev.kind;row.dataset.kind=ev.kind;if(ev.kind==='tool')renderTool(row,ev.text);else row.textContent=ev.text;return row}
function renderFeed(){const s=active(),box=$('#feed');box.innerHTML='';if(!s.events.length){box.innerHTML='<div class="empty"><span class="orb">C</span><strong>What do you want to build?</strong><span>Choose a workspace, then give Cortex a development task.</span></div>';return}for(const ev of s.events)box.append(eventNode(ev));box.scrollTop=box.scrollHeight}
function renderSession(){const s=active();$('#workspaceLabel').textContent=s.workspace||'Choose workspace';$('#run').disabled=s.busy||!s.workspace;$('#prompt').disabled=s.busy;$('#stop').hidden=!s.busy;$('#activity').hidden=!s.busy;renderFeed()}
function renderAll(){renderTabs();renderSession()}
function addEvent(id,kind,text){const clean=String(text??'').trim();if(!clean)return;const s=sessions[id];if(!s)return;s.events.push({kind,text:clean});if(s.events.length>500)s.events=s.events.slice(-500);saveSessions();if(activeId===id){$('#feed .empty')?.remove();$('#feed').append(eventNode({kind,text:clean}));$('#feed').scrollTop=$('#feed').scrollHeight}}
async function loadSettings(){settings=await api('/api/settings');providers=settings.providers||[];const sel=$('#provider');sel.innerHTML='';for(const p of providers){const o=document.createElement('option');o.value=p.id;o.textContent=p.label+(p.configured?' · configured':'');sel.append(o)}sel.value=settings.activeProvider||providers[0]?.id||'';updateProviderUI()}
function updateProviderUI(){
  const p=providers.find(x=>x.id===$('#provider').value);if(!p)return;
  $('#model').value=p.model||p.defaultModel||'';
  $('#apiKey').value='';
  const auth=p.authMode==='opencode-auth';
  $('#keyLabel').hidden=auth;$('#deleteKey').hidden=auth;
  $('#apiKey').placeholder=p.configured?'Key stored · enter a new key to replace':'Paste API key';
  $('#providerNote').textContent=auth
    ? (p.configured
        ? `Connected through OpenCode on this host. Cortex copies that ${p.label} OAuth credential into the isolated session.`
        : `Not connected yet. Run: opencode auth login --provider ${p.openCodeID} on the Cortex host, then reopen Settings.`)
    : `Cortex passes this key only to the isolated OpenCode process. Model IDs use OpenCode's ${p.openCodeID}/… catalog.`;
}
async function saveKey(remove=false){
  const provider=$('#provider').value,p=providers.find(x=>x.id===provider),key=$('#apiKey').value.trim(),model=$('#model').value.trim();
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
function toolText(raw){const p=raw.part||raw,st=p.state||{},tool=p.tool||raw.tool||raw.name||'tool',status=st.status||'';const input=st.input||p.input||{},lines=[`↳ ${tool}${status?' · '+status:''}`];if(tool==='bash'&&input.command)lines.push('$ '+input.command);else if(input.filePath||input.path)lines.push(input.filePath||input.path);if(st.error)lines.push('ERROR: '+clipped(typeof st.error==='string'?st.error:JSON.stringify(st.error)));else if(st.output)lines.push(clipped(st.output));return lines.join('\n')}
function summarize(ev){const raw=ev?.data?.data||ev?.data||{},type=String(raw.type||'');if(ev.type==='error')return raw.message||'Agent failed';if(ev.type==='recovered')return raw.text||'';if(ev.type==='done'){const i=raw.inputTokens||0,o=raw.outputTokens||0;return i||o?`Done · ${i} input · ${o} output tokens`:'Done'};if(ev.type==='output')return raw.text||'';if(type.includes('tool'))return toolText(raw);return raw.part?.text||raw.text||''}
async function runAgent(prompt){const s=active();if(!s||s.busy||!s.workspace)return;const id=s.id;s.busy=true;s.abort=new AbortController();if(!s.title)s.title=prompt.split(/\s+/).slice(0,5).join(' ');addEvent(id,'user',prompt);renderAll();try{const r=await fetch('/api/agent/run',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({workspace:s.workspace,prompt,session:s.openCodeSession||'',clientSession:id}),signal:s.abort.signal});if(!r.ok)throw Error((await r.text()).trim()||r.statusText);const rd=r.body.getReader(),dec=new TextDecoder();let buf='';for(;;){const {value,done}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf('\n'))>=0){const line=buf.slice(0,i).trim();buf=buf.slice(i+1);if(!line)continue;const ev=JSON.parse(line),text=summarize(ev),raw=ev?.data?.data||ev?.data||{},t=String(raw.type||'');if(ev.type==='done'&&raw.sessionID)s.openCodeSession=raw.sessionID;if(text)addEvent(id,ev.type==='error'?'error':ev.type==='done'?'done':t.includes('tool')?'tool':'assistant',text)}}}catch(e){addEvent(id,'error',e.name==='AbortError'?'Agent stopped.':e.message)}finally{if(sessions[id]){sessions[id].busy=false;sessions[id].abort=null;saveSessions()}if(activeId===id)renderAll();agentStatus()}}
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
function chooseWorkspace(){const s=active();s.workspace=browserPath;s.openCodeSession='';s.title='';s.events=[];saveSessions();$('#workspaceModal').hidden=true;renderAll();toast('Workspace selected')}
async function copySession(){const s=active(),labels={user:'You',assistant:'Agent',tool:'Tool',error:'Error',done:'Status'},text=s.events.map(x=>`${labels[x.kind]||'Agent'}:\n${x.text}`).join('\n\n');if(!text)return toast('Nothing to copy');await navigator.clipboard.writeText(text);toast('Session copied')}
async function boot(){const st=await api('/api/status');root=st.root;loadSessions();renderAll();await Promise.all([loadSettings(),agentStatus()]);if(!active()?.workspace)setTimeout(openWorkspacePicker,0)}
$('#newSession').onclick=()=>newSession();$('#workspaceBtn').onclick=openWorkspacePicker;$('#closeWorkspace').onclick=$('#cancelWorkspace').onclick=()=>$('#workspaceModal').hidden=true;$('#browserUp').onclick=()=>browse(parentPath(browserPath));$('#chooseWorkspace').onclick=chooseWorkspace;
$('#agentForm').onsubmit=e=>{e.preventDefault();const p=$('#prompt').value.trim();if(!p)return;$('#prompt').value='';runAgent(p)};$('#prompt').onkeydown=e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();$('#agentForm').requestSubmit()}};$('#stop').onclick=()=>active()?.abort?.abort();$('#copy').onclick=copySession;
$('#settingsBtn').onclick=()=>{$('#settingsModal').hidden=false;loadSettings()};$('#closeSettings').onclick=()=>$('#settingsModal').hidden=true;$('#settingsModal').onclick=e=>{if(e.target===$('#settingsModal'))$('#settingsModal').hidden=true};$('#workspaceModal').onclick=e=>{if(e.target===$('#workspaceModal'))$('#workspaceModal').hidden=true};$('#provider').onchange=updateProviderUI;$('#saveKey').onclick=()=>saveKey(false);$('#deleteKey').onclick=()=>saveKey(true);
boot().catch(e=>toast(e.message));