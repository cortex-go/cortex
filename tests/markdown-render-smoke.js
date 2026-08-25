const fs=require('fs'),vm=require('vm');
class Node {
  constructor(tag=''){this.tagName=tag.toUpperCase();this.children=[];this.dataset={};this.classList={add:(...x)=>{this.classes=(this.classes||[]).concat(x)}};this._text='';}
  append(...x){this.children.push(...x)}
  appendChild(x){this.children.push(x);return x}
  set textContent(v){this._text=String(v)}
  get textContent(){return this._text}
  set className(v){this._className=v}
  get className(){return this._className||''}
}
const document={createElement:t=>new Node(t),createTextNode:t=>({textContent:String(t)}),querySelector:()=>new Node(),querySelectorAll:()=>[],addEventListener:()=>{},body:new Node('body')};
const src=fs.readFileSync('content/assets/js/script.js','utf8');
const start=src.indexOf('function appendMarkdownInline'),end=src.indexOf('function renderFeed',start);
const ctx={document,console};vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
const row=new Node('div');
ctx.renderMarkdown(row,'## Heading\n\n**bold** and `code`\n\n- one\n- two');
const flat=JSON.stringify(row);
if(!flat.includes('md-heading')||!flat.includes('md-gap')||!flat.includes('"STRONG"')||!flat.includes('**bold**')||!flat.includes('"CODE"')||!flat.includes('"UL"'))throw new Error('Markdown smoke render incomplete');
console.log('markdown render smoke: ok');
