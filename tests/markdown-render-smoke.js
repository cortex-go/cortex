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
    if(n._text!=='')parts.push(n._text);
    for(const c of n.children||[])walk(c);
  };
  walk(node);
  return parts.join('|');
}
const src=fs.readFileSync('content/assets/js/script.js','utf8');

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

// 3. Quoted-string highlighting (escaped-aware).
{
  const cases=[
    ['"this should be highlighted"','"this should be highlighted"'],
    ["'as should this'","'as should this'"],
    ["'that\\'s counted too'","'that\\'s counted too'"],
    ['"say \\"hello\\""','"say \\"hello\\""'],       // one double-quoted string with escaped quotes
    ["'C:\\Users\\Nick'","'C:\\Users\\Nick'"],
    ['"single quote \' inside double quotes"',"\"single quote ' inside double quotes\""],
    ["'double quote \" inside single quotes'","'double quote \" inside single quotes'"],
  ];
  for(const [text] of cases){
    const row=new Node('div');
    ctx.renderMarkdown(row,text);
    const quotes=byClass(row,'md-quote');
    if(quotes.length!==1||quotes[0].textContent!==text)throw new Error('quote mismatch for '+JSON.stringify(text)+' got '+quotes.length+' spans');
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
  const ctx2=load(src,'function eventKind','function renderFeed');
  if(ctx2.eventKind({type:'error',data:{message:'x'}})!=='error')throw new Error('error classification');
  if(ctx2.eventKind({type:'done',data:{}})!=='done')throw new Error('done classification');
  if(ctx2.eventKind({type:'opencode',data:{type:'tool',part:{type:'tool',tool:'bash'}}})!=='tool')throw new Error('tool classification');
  if(ctx2.eventKind({type:'opencode',data:{type:'text',part:{type:'text',text:'hi'}}})!=='assistant')throw new Error('text classification');
}

// ---- summarize/toolText produce an un-prefixed ↳ header ----
{
  const ctx3=load(src,'function clipped','async function runAgent');
  const ev={type:'opencode',data:{type:'tool',part:{type:'tool',tool:'bash',state:{status:'failed',input:{command:'gh auth status'},output:'x'}}}};
  const text=ctx3.summarize(ev);
  if(!text.startsWith('↳ bash · failed'))throw new Error('summarize tool text wrong: '+JSON.stringify(text));
  if(text.includes('Provider-reported tool event'))throw new Error('legacy prefix leaked into tool text');
  const toolText=ctx3.toolText(ev.data);
  if(!toolText.startsWith('↳ bash · failed'))throw new Error('toolText header wrong: '+JSON.stringify(toolText));
}

console.log('cortex markdown/tool render smoke: ok');