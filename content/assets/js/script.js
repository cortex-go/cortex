const $=s=>document.querySelector(s);
let root='',providers=[],browserPath='',activeId='',sessions={},closedSessions=[],settings={},serverReady=false;
const serverSaveTimers=new Map();
const STORE='cortex.sessions.v1',COMPOSER_STORE='cortex.composer.height.v1';
const MAX_IMAGES=6,MAX_IMAGE_BYTES=10*1024*1024;
let attachments=[],modelCatalog=[];
function toast(m){const t=$('#toast');t.textContent=m;t.classList.add('show');setTimeout(()=>t.classList.remove('show'),1800)}
async function api(url,opt={}){opt={...opt,headers:{...(opt.headers||{})}};const method=(opt.method||'GET').toUpperCase();if(!['GET','HEAD','OPTIONS'].includes(method)&&authState?.csrf)opt.headers['X-Cortex-CSRF']=authState.csrf;const r=await fetch(url,opt);if(!r.ok)throw Error((await r.text()).trim()||r.statusText);return r.headers.get('content-type')?.includes('json')?r.json():r.text()}
function sid(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+Math.random().toString(36).slice(2)}
function sessionTitle(s){if(s.title)return s.title;if(s.workspace)return s.workspace.split('/').filter(Boolean).pop()||s.workspace;return 'New session'}
function sessionSafe(s){return{id:s.id,workspace:s.workspace||'',workspaceStatus:s.workspaceStatus||'',title:s.title||'',provider:s.provider||'',model:s.model||'',openCodeSession:s.openCodeSession||'',state:s.busy?'running':'idle',createdAt:s.createdAt||Date.now(),updatedAt:Date.now(),archivedAt:s.archivedAt||s.closedAt||0,events:s.events||[]}}
function scheduleServerSave(s){
  if(!serverReady||!s?.id)return;
  clearTimeout(serverSaveTimers.get(s.id));
  serverSaveTimers.set(s.id,setTimeout(async()=>{serverSaveTimers.delete(s.id);try{await api('/api/conversation',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(sessionSafe(s))})}catch(e){toast('Conversation save failed · '+e.message)}},120))
}
// saveSessionToServer issues a PUT for exactly one conversation, coalescing
// rapid changes for the same id without fan-out to unrelated sessions.
function saveSessionToServer(id){const s=sessions[id]||closedSessions.find(x=>x.id===id);if(s)scheduleServerSave(s)}
// persistLocalState writes only the browser's local UI state (active tab,
// open and archived sessions). It never issues a conversation PUT.
function persistLocalState(){
  const safe={};for(const [id,s] of Object.entries(sessions))safe[id]=sessionSafe(s);
  localStorage.setItem(STORE,JSON.stringify({activeId,sessions:safe,closedSessions:closedSessions.slice(0,20)}));
}
function saveSessions(){
  persistLocalState();
  if(!Object.keys(sessions).length)newSession('',false);
  renderAll();
}
function workspaceUnavailable(s){return s&&s.workspaceStatus&&s.workspaceStatus!=='available'}
function loadSessions(){
  try{const x=JSON.parse(localStorage.getItem(STORE)||'{}');sessions=x.sessions||{};closedSessions=Array.isArray(x.closedSessions)?x.closedSessions:[];activeId=x.activeId||''}catch{}
  const retained=Object.values(sessions).sort((a,b)=>(b.updatedAt||b.createdAt||0)-(a.updatedAt||a.createdAt||0)).slice(0,32);sessions=Object.fromEntries(retained.map(s=>[s.id,s]));closedSessions=closedSessions.slice(0,20);
  for(const s of Object.values(sessions)){s.busy=false;s.abort=null;s.createdAt=s.createdAt||Date.now();s.events=(s.events||[]).slice(-500)}
  if(!Object.keys(sessions).length)newSession('',false);
  if(!sessions[activeId])activeId=Object.keys(sessions)[0]
}
function newSession(workspace='',render=true){
	if(Object.keys(sessions).length>=32){toast('Close a session before opening another.');return null}
  const id=sid();sessions[id]={id,workspace,title:'',openCodeSession:'',events:[],createdAt:Date.now(),busy:false,abort:null};activeId=id;saveSessions();saveSessionToServer(id);
  if(render)renderAll();
  return sessions[id]
}
function newSessionSameWorkspace(){const workspace=active()?.workspace||'';newSession(workspace);hideSessionMenu();if(!workspace)setTimeout(openWorkspacePicker,0)}
function newWorkspaceSession(){newSession('');hideSessionMenu();setTimeout(openWorkspacePicker,0)}
function closeSession(id){
  const s=sessions[id];if(!s)return;
  if(s.busy&&!confirm('This agent is still working. Stop and close it?'))return;
  s.abort?.abort();
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
function renderTabs(){const box=$('#sessionTabs');box.innerHTML='';for(const s of Object.values(sessions)){const b=document.createElement('button');b.className='session-tab'+(s.id===activeId?' active':'');const title=document.createElement('span');title.className='tab-title';title.textContent=sessionTitle(s);const x=document.createElement('span');x.className='tab-close';x.textContent='×';x.onclick=e=>{e.stopPropagation();closeSession(s.id)};b.append(title,x);b.onclick=()=>{activeId=s.id;persistLocalState();renderAll()};box.append(b)}}
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
function eventKind(ev){const raw=ev?.data?.data||ev?.data||{},t=String(raw.type||'');if(ev.type==='error')return'error';if(ev.type==='done')return'done';return t.includes('tool')?'tool':'assistant'}
function eventNode(ev){const row=document.createElement('div');row.className='event '+ev.kind;row.dataset.kind=ev.kind;if(ev.kind==='tool'||(ev.kind==='assistant'&&isToolEventText(ev.text)))renderTool(row,ev.text);else if(ev.kind==='assistant')renderMarkdown(row,ev.text);else if(ev.kind==='image'){const fig=document.createElement('figure');fig.className='image-attach';const img=document.createElement('img');img.src=ev.text;img.alt=ev.name||'attached image';img.title=ev.name||'';fig.append(img);if(ev.name){const cap=document.createElement('figcaption');cap.textContent=ev.name;fig.append(cap)}row.append(fig)}else row.textContent=ev.text;return row}
function renderFeed(){const s=active(),box=$('#feed');box.innerHTML='';if(!s.events.length){box.innerHTML='<div class="empty"><span class="orb">C</span><strong>What do you want to build?</strong><span>Choose a workspace, then give Cortex a development task.</span></div>';return}for(const ev of s.events)box.append(eventNode(ev));box.scrollTop=box.scrollHeight}
function renderSession(){const s=active();$('#workspaceLabel').textContent=s.workspace||'Choose workspace';$('#run').disabled=s.busy||!s.workspace||workspaceUnavailable(s);$('#prompt').disabled=s.busy;$('#stop').hidden=!s.busy;$('#activity').hidden=!s.busy;$('#wsUnavailable').hidden=!workspaceUnavailable(s);$('#wsUnavailable').textContent=workspaceUnavailable(s)?('Workspace unavailable ('+s.workspaceStatus+') — choose a replacement to run.'):'';renderFeed()}
function renderAll(){renderTabs();renderSession()}
function addEvent(id,kind,text,name=''){const clean=String(text??'').trim();if(!clean)return;const s=sessions[id];if(!s)return;s.events.push({kind,text:clean,name});if(s.events.length>500)s.events=s.events.slice(-500);persistLocalState();saveSessionToServer(id);if(activeId===id){$('#feed .empty')?.remove();$('#feed').append(eventNode({kind,text:clean,name}));$('#feed').scrollTop=$('#feed').scrollHeight}}
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
function summarize(ev){const raw=ev?.data?.data||ev?.data||{},type=String(raw.type||'');if(ev.type==='error')return raw.message||'Agent failed';if(ev.type==='truncated')return raw.message||'Provider output was truncated.';if(ev.type==='recovered')return raw.text||'';if(ev.type==='done'){const i=raw.inputTokens||0,o=raw.outputTokens||0;return i||o?`Done · ${i} input · ${o} output tokens`:'Done'};if(ev.type==='output')return raw.text||'';if(type.includes('tool'))return toolText(raw);return raw.part?.text||raw.text||''}
async function runAgent(prompt){const s=active();if(!s||s.busy||!s.workspace||workspaceUnavailable(s))return;const id=s.id,p=providers.find(x=>x.id===settings.activeProvider);s.provider=settings.activeProvider||'';s.model=p?.model||p?.defaultModel||'';s.busy=true;s.abort=new AbortController();if(!s.title)s.title=prompt.split(/\s+/).slice(0,5).join(' ');const images=attachments.map(a=>({name:a.name,data:a.dataUrl}));for(const a of attachments)addEvent(id,'image',a.thumb,a.name);addEvent(id,'user',prompt);attachments=[];renderAttachments();renderAll();try{const r=await fetch('/api/agent/run',{method:'POST',headers:{'Content-Type':'application/json','X-Cortex-CSRF':authState.csrf||''},body:JSON.stringify({workspace:s.workspace,prompt,session:s.openCodeSession||'',clientSession:id,images}),signal:s.abort.signal});if(!r.ok)throw Error((await r.text()).trim()||r.statusText);const rd=r.body.getReader(),dec=new TextDecoder();let buf='';const seenImages=new Set();for(;;){const {value,done}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf('\n'))>=0){const line=buf.slice(0,i).trim();buf=buf.slice(i+1);if(!line)continue;const ev=JSON.parse(line),text=summarize(ev),raw=ev?.data?.data||ev?.data||{};if(ev.type==='done'&&raw.sessionID)s.openCodeSession=raw.sessionID;if(text)addEvent(id,eventKind(ev),text);if(ev.type==='recovered-images'){for(const im of (raw.images||[]))addGeneratedImage(id,im)}for(const im of extractImages(raw)){if(seenImages.has(im.url))continue;seenImages.add(im.url);addGeneratedImage(id,im)}}}}catch(e){addEvent(id,'error',e.name==='AbortError'?'Agent stopped.':e.message)}finally{if(sessions[id]){sessions[id].busy=false;sessions[id].abort=null;persistLocalState();saveSessionToServer(id)}if(activeId===id)renderAll();agentStatus()}}
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
function chooseWorkspace(){const s=active();s.workspace=browserPath;s.openCodeSession='';s.title='';s.events=[];s.workspaceStatus='available';persistLocalState();saveSessionToServer(activeId);$('#workspaceModal').hidden=true;renderAll();toast('Workspace selected')}
async function copySession(){const s=active(),labels={user:'You',assistant:'Agent',tool:'Tool',error:'Error',done:'Status'},text=s.events.filter(x=>x.kind!=='image').map(x=>`${labels[x.kind]||'Agent'}:\n${x.text}`).join('\n\n');if(!text)return toast('Nothing to copy');await navigator.clipboard.writeText(text);toast('Session copied')}
function applyComposerHeight(value){
  const card=document.querySelector('.agent-card');if(!card)return;
  const max=Math.max(170,Math.floor(card.getBoundingClientRect().height*.55));
  const height=Math.max(112,Math.min(max,Number(value)||150));
  card.style.setProperty('--composer-height',height+'px');
  return height
}
function restoreComposerHeight(){applyComposerHeight(localStorage.getItem(COMPOSER_STORE)||150)}
function startComposerResize(e){
  if(e.button!==0)return;e.preventDefault();
  const form=$('#agentForm'),startY=e.clientY,startHeight=form.getBoundingClientRect().height;
  document.body.classList.add('resizing-composer');
  const move=ev=>applyComposerHeight(startHeight+(startY-ev.clientY));
  const up=ev=>{
    document.removeEventListener('pointermove',move);document.removeEventListener('pointerup',up);
    document.body.classList.remove('resizing-composer');
    localStorage.setItem(COMPOSER_STORE,String(Math.round($('#agentForm').getBoundingClientRect().height)))
  };
  document.addEventListener('pointermove',move);document.addEventListener('pointerup',up)
}
// migrateLegacy imports the browser's local conversations that are not already
// on the server and consumes the deterministic imported/rejected result.
// Successfully imported records are dropped from local storage (the server now
// holds them); rejected records are preserved locally with their transcripts
// and marked recoverable so they can be corrected and retried. Returns the
// number of rejected records.
async function migrateLegacy(legacy){
  if(!legacy.length)return 0;
  const res=await api('/api/conversations',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({conversations:legacy})});
  const imported=new Set(res.imported||[]);
  const rejectedById=new Map((res.rejected||[]).map(r=>[r.id,r.reason]));
  for(const [id,s] of Object.entries(sessions)){
    if(rejectedById.has(id)){s.migrationRejected=true;s.migrationReason=rejectedById.get(id)}
    else if(imported.has(id)){delete sessions[id]}
  }
  closedSessions=closedSessions.filter(s=>{
    if(rejectedById.has(s.id)){s.migrationRejected=true;s.migrationReason=rejectedById.get(s.id);return true}
    return !imported.has(s.id);
  });
  localStorage.setItem('cortex.sessions.migrated.sqlite.v1','1');
  renderMigrationNotice();
  return rejectedById.size;
}
function renderMigrationNotice(){
  const el=$('#migrationNotice'),btn=$('#retryMigration');
  const rejected=[...Object.values(sessions),...closedSessions].filter(s=>s.migrationRejected);
  if(!rejected.length){el.hidden=true;return}
  el.hidden=false;
  el.textContent='Some conversations could not be imported ('+rejected.length+'). They remain in this browser with their transcripts. ';
  btn.hidden=false
}
// retryMigration re-attempts the import of every local conversation that is not
// already on the server, so a corrected or newly-valid record can be migrated
// even when the database already contains other conversations.
async function retryMigration(){
  const legacy=[...Object.values(sessions),...closedSessions].map(sessionSafe);
  const stored=await api('/api/conversations');
  const toMigrate=legacy.filter(l=>!stored.some(s=>s.id===l.id));
  const rejected=await migrateLegacy(toMigrate);
  persistLocalState();renderAll();
  toast(rejected?rejected+' conversations could not be imported.':'All remaining conversations imported.')
}
async function syncServerConversations(){
  const legacy=[...Object.values(sessions),...closedSessions].map(sessionSafe);
  let stored=await api('/api/conversations');
  // Attempt migration of every legacy record the server does not already hold.
  // This runs even when the server has other conversations, so a previously
  // rejected record stays recoverable and is retried on a later boot. The
  // idempotent upsert plus the not-on-server filter prevent duplicates.
  const toMigrate=legacy.filter(l=>!stored.some(s=>s.id===l.id));
  if(toMigrate.length){
    await migrateLegacy(toMigrate);
    stored=await api('/api/conversations');
  }
  const authoritative=stored.length?stored:await api('/api/conversations');
  for(const s of authoritative){s.busy=false;s.abort=null;if(s.archivedAt){s.closedAt=s.archivedAt;closedSessions.push(s)}else sessions[s.id]=s}
  if(!Object.keys(sessions).length)newSession('',false);
  if(!sessions[activeId])activeId=Object.keys(sessions)[0];
  // Loading authoritative server conversations must not PUT any of them back:
  // only the local UI state is persisted here.
  renderMigrationNotice();
  serverReady=true;persistLocalState();renderAll()
}
async function boot(){const st=await api('/api/status');root=st.root;loadSessions();renderAll();restoreComposerHeight();await Promise.all([loadSettings(),agentStatus(),syncServerConversations()]);if(!active()?.workspace)setTimeout(openWorkspacePicker,0)}
$('#newSession').onclick=e=>{e.stopPropagation();toggleSessionMenu()};$('#newSameWorkspace').onclick=newSessionSameWorkspace;$('#newWorkspace').onclick=newWorkspaceSession;$('#workspaceBtn').onclick=openWorkspacePicker;$('#closeWorkspace').onclick=$('#cancelWorkspace').onclick=()=>$('#workspaceModal').hidden=true;$('#browserUp').onclick=()=>browse(parentPath(browserPath));$('#chooseWorkspace').onclick=chooseWorkspace;$('#retryMigration').onclick=retryMigration;
$('#composerResize').onpointerdown=startComposerResize;$('#composerResize').ondblclick=()=>{applyComposerHeight(150);localStorage.setItem(COMPOSER_STORE,'150')};document.addEventListener('click',e=>{if(!e.target.closest('.new-session-wrap'))hideSessionMenu()});window.addEventListener('resize',()=>applyComposerHeight($('#agentForm').getBoundingClientRect().height));
$('#agentForm').onsubmit=e=>{e.preventDefault();const p=$('#prompt').value.trim();if(!p)return;$('#prompt').value='';runAgent(p)};$('#prompt').onkeydown=e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();$('#agentForm').requestSubmit()}};$('#stop').onclick=()=>active()?.abort?.abort();$('#copy').onclick=copySession;
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
