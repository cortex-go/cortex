const fs=require('fs'),vm=require('vm');
class Node {
  constructor(tag=''){this.tagName=tag.toUpperCase();this.children=[];this.dataset={};this.classes=[];this.classList={add:(...x)=>{this.classes.push(...x)}};this._text='';this._className='';}
  append(...x){this.children.push(...x)}
  appendChild(x){this.children.push(x);return x}
  set textContent(v){this._text=String(v)}
  get textContent(){return this._text}
  set className(v){this._className=String(v)}
  get className(){return this._className}
}
function makeContext(){
  const document={createElement:t=>new Node(t),createTextNode:t=>({textContent:String(t)}),querySelector:()=>new Node(),querySelectorAll:()=>[],addEventListener:()=>{},body:new Node('body')};
  return {document,console};
}
function load(src,from,to){
  const ctx=makeContext();
  const start=src.indexOf(from),end=src.indexOf(to,start);
  if(start<0||end<0)throw new Error('could not slice '+from+'..'+to);
  vm.createContext(ctx);
  vm.runInContext(src.slice(start,end),ctx);
  return ctx;
}
function tagNames(node,out=[]){
  if(node.tagName)out.push(node.tagName);
  for(const c of node.children||[])tagNames(c,out);
  return out;
}
function byClass(node,cls,out=[]){
  if(String(node.className||'').split(/\s+/).includes(cls))out.push(node);
  for(const c of node.children||[])byClass(c,cls,out);
  return out;
}
function flatText(node){
  const parts=[];
  const walk=n=>{
    const t=n.textContent;
    if(t!=='')parts.push(t);
    for(const c of n.children||[])walk(c);
  };
  walk(node);
  return parts.join('');
}
const src=fs.readFileSync('public/assets/js/script.js','utf8');
const page=fs.readFileSync('content/index.html','utf8');
if(!page.includes('<textarea id="prompt"></textarea>'))throw new Error('prompt textarea must be empty');
if(page.includes('Implement, investigate')||page.includes('drop or paste images'))throw new Error('prompt placeholder returned');

// ---- Inline + block markdown rendering (also covers tool events) ----
const ctx=load(src,'function isToolEventText','function renderFeed');

// 1. Markdown smoke (existing behaviour preserved).
{
  const row=new Node('div');
  ctx.renderMarkdown(row,'## Heading\n\n**bold** and `code`\n\n- one\n- two');
  const flat=JSON.stringify(row);
  for(const want of ['md-heading','## Heading','md-gap','"STRONG"','**bold**','"CODE"','`code`','"UL"'])if(!flat.includes(want))throw new Error('markdown missing '+want);
}

// 2. Underscore-delimited text must stay literal (no emphasis).
{
  const row=new Node('div');
  ctx.renderMarkdown(row,'Use foo_bar and my_var and __init__ and C:/Users/Nick');
  const flat=JSON.stringify(row);
  if(flat.includes('"EM"')||flat.includes('"STRONG"'))throw new Error('underscore text was emphasised');
  if(!flat.includes('foo_bar')||!flat.includes('my_var')||!flat.includes('__init__'))throw new Error('underscore identifiers not preserved');
}

// 3. Quoted-string highlighting (escaped-aware; single quotes need a preceding space).
{
  const cases=[
    ['"this should be highlighted"','"this should be highlighted"'],
    ["Value: 'as should this'","'as should this'"],
    ["Value: 'that\\'s counted too'","'that\\'s counted too'"],
    ['"say \\"hello\\""','"say \\"hello\\""'],       // one double-quoted string with escaped quotes
    ["Path: 'C:\\Users\\Nick'","'C:\\Users\\Nick'"],
    ['"single quote \' inside double quotes"','"single quote \' inside double quotes"'],
    ["Value: 'double quote \" inside single quotes'","'double quote \" inside single quotes'"],
  ];
  for(const [text,want] of cases){
    const row=new Node('div');
    ctx.renderMarkdown(row,text);
    const quotes=byClass(row,'md-quote');
    if(quotes.length!==1||quotes[0].textContent!==want)throw new Error('quote mismatch for '+JSON.stringify(text)+' got '+quotes.length+' spans');
  }
  // Unterminated strings stay unhighlighted.
  {
    const row=new Node('div');
    ctx.renderMarkdown(row,'an unterminated "string and \'other');
    if(byClass(row,'md-quote').length)throw new Error('unterminated string was highlighted');
  }
  // Adjacent quoted strings are each highlighted.
  {
    const row=new Node('div');
    ctx.renderMarkdown(row,'adjacent "one" "two" and \'a\' \'b\'');
    if(byClass(row,'md-quote').length!==4)throw new Error('adjacent quote count wrong');
  }
  // Contractions, possessives and ordinary apostrophes must never be highlighted.
  {
    const prose=['this isn\'t a sentence we want highlighted just cause it\'s got single quotes',"Nick's project","can't match through another apostrophe later","rock'n'roll","foo'bar'"];
    for(const text of prose){
      const row=new Node('div');
      ctx.renderMarkdown(row,text);
      if(byClass(row,'md-quote').length)throw new Error('apostrophe prose was highlighted: '+text);
    }
  }
  // A space-prefixed single-quoted block is highlighted and its leading space
  // stays outside the highlighted element.
  {
    const row=new Node('div');
    ctx.renderMarkdown(row,"Use 'this quoted block' here");
    const quotes=byClass(row,'md-quote');
    if(quotes.length!==1)throw new Error('space-prefixed quote not highlighted');
    if(quotes[0].textContent!=="'this quoted block'")throw new Error('leading space leaked into the highlighted element: '+JSON.stringify(quotes[0].textContent));
    if(!flatText(row).includes("Use 'this quoted block' here"))throw new Error('space was not preserved as plain text');
  }
  // A single quote at the start of a line is not highlighted.
  {
    const row=new Node('div');
    ctx.renderMarkdown(row,"'as should this'");
    if(byClass(row,'md-quote').length)throw new Error('line-start single quote was highlighted');
  }
}

// 4. Tool-status events (live and restored shapes).
{
  const toolCases=['↳ bash · running','↳ bash · completed','↳ bash · failed','↳ read · completed','↳ gh · failed','↳ bash · queued','↳ bash · cancelled','↳ bash · interrupted'];
  for(const line of toolCases){
    const row=new Node('div');
    ctx.renderTool(row,line+'\n$ some command\noutput');
    if(!row.children.length||row.children[0].className!=='tool-head')throw new Error('tool not structured: '+line);
    const name=byClass(row,'tool-name')[0],status=byClass(row,'tool-status')[0];
    if(!name||name.textContent!==line.split(' · ')[0].replace('↳ ',''))throw new Error('tool name wrong: '+line);
    if(!status||status.textContent!==line.split(' · ')[1])throw new Error('tool status wrong: '+line);
    if(!status.classes.includes(line.split(' · ')[1]))throw new Error('tool status class missing: '+line);
  }
  // Regression-era stored text (provider wrapper + kind assistant) renders as a tool event.
  {
    const row=ctx.eventNode({kind:'assistant',text:'Provider-reported tool event\n↳ bash · failed\n$ gh auth status\noutput'});
    if(!row.children.length||row.children[0].className!=='tool-head')throw new Error('regression-era tool text not restored');
  }
  // Ordinary assistant prose containing an arrow must NOT be reinterpreted.
  {
    const row=ctx.eventNode({kind:'assistant',text:'I used the ↳ symbol in prose, not a tool event.'});
    if(row.children.length&&row.children[0].className==='tool-head')throw new Error('prose arrow was misinterpreted as a tool event');
    if(!row.children.length)throw new Error('prose did not render as markdown');
  }
  // Live tool events classified as kind tool render structured.
  {
    const row=ctx.eventNode({kind:'tool',text:'↳ bash · completed\n$ true'});
    if(!row.children.length||row.children[0].className!=='tool-head')throw new Error('live tool event not structured');
  }
}

// ---- Event classification used by the streaming run loop ----
{
  const ctx2=load(src,'function eventData','function renderFeed');
  if(ctx2.eventKind({type:'error',data:{message:'x'}})!=='error')throw new Error('error classification');
  if(ctx2.eventKind({type:'done',data:{}})!=='done')throw new Error('done classification');
  if(ctx2.eventKind({type:'opencode',data:{type:'tool',part:{type:'tool',tool:'bash'}}})!=='tool')throw new Error('tool classification');
  if(ctx2.eventKind({type:'opencode',data:{type:'text',part:{type:'text',text:'hi'}}})!=='assistant')throw new Error('text classification');
}

// ---- summarize/toolText produce an un-prefixed ↳ header ----
{
  const ctx3=load(src,'function eventData','async function runAgent');
  const ev={type:'opencode',data:{type:'tool',part:{type:'tool',tool:'bash',state:{status:'failed',input:{command:'gh auth status'},output:'x'}}}};
  const text=ctx3.summarize(ev);
  if(!text.startsWith('↳ bash · failed'))throw new Error('summarize tool text wrong: '+JSON.stringify(text));
  if(text.includes('Provider-reported tool event'))throw new Error('legacy prefix leaked into tool text');
  const toolText=ctx3.toolText(ev.data);
  if(!toolText.startsWith('↳ bash · failed'))throw new Error('toolText header wrong: '+JSON.stringify(toolText));
}

console.log('cortex markdown/tool render smoke: ok');
