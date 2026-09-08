import {Terminal} from '@xterm/xterm';
import {WebglAddon} from '@xterm/addon-webgl';
import {installControls} from './preview-control.js';
import {FitAddon} from '@xterm/addon-fit';
import './style.css';

const $=id=>document.getElementById(id);
const host=$('terminal');
const instance=crypto.randomUUID();
const terminal=new Terminal({fontFamily:'Menlo, Consolas, monospace',fontSize:12,lineHeight:1.1,theme:{background:'#202026',foreground:'#d1ceda'},cursorBlink:false,scrollback:0,screenReaderMode:true,convertEol:true});
const fit=new FitAddon();terminal.loadAddon(fit);terminal.open(host);
let captureError=null;try{const webgl=new WebglAddon(true);terminal.loadAddon(webgl);webgl.onContextLoss(()=>{captureError="WebGL context lost; reload the preview to restore PNG capture";webgl.dispose();});}catch(error){captureError=error.message;}
const defaults={importId:'',messageNumber:0,bottom:false,data:{},focusItem:'',modal:'none',popover:'none',menuRow:0,example:'all',resetRevision:0,model:'dummy-coder',scenario:'working',compact:false,planExpanded:true,toolsExpanded:false,toolsCompact:false,input:'',scroll:0,submitted:[]};
let state={...defaults},revision=0,busy=false,pending=false,connected=false,frame=null;
const clicks=[];
const renderEvents=new EventTarget();let paintedRevision=0,lastCatalog=null;
let buildNotice=null;
function status(text,error=false){if(buildNotice){text=buildNotice.text;error=buildNotice.error;}$('bridge-status').textContent=text;$('bridge-status').classList.toggle('error',error);}
function requestFrame(){revision++;pending=true;void flush();}
async function flush(){
 if(busy||!pending)return;
 if(terminal.cols<45||terminal.rows<15){status('Enlarge the terminal to at least 45 columns × 15 rows.',true);return;}
 busy=true;pending=false;const version=revision;
 const click=clicks.shift();
 try{
  const response=await fetch('/api/preview',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({...state,instance,cols:terminal.cols,rows:terminal.rows,...(click?{click}:{})})});
  const result=await response.json();if(!response.ok)throw new Error(result.error||`Go renderer returned ${response.status}`);
  if(result.renderer!=='crux/internal/ui/model.UI.View')throw new Error('Unexpected renderer identity');
  connected=true;
  if(version===revision){frame=result;$('example-note').textContent=result.exampleNote;await new Promise(resolve=>terminal.write('\x1b[?25l\x1b[?7l\x1b[0m\x1b[2J\x1b[H'+result.content.replace(/\r?\n/g,'\r\n')+'\x1b[0m',resolve));await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));paintedRevision=version;renderEvents.dispatchEvent(new Event('painted'));$('jump-message').max=result.storedMessages?.length||result.messageCount||1;$('dimensions').textContent=`${result.cols} × ${result.rows} · sidebar ${result.sidebarColumns} cols · ${result.messageCount} items`;status(`Connected · actual Go UI.View · ${result.compact?'compact layout':'full layout'}`);}
 }catch(error){connected=false;status(`Go preview unavailable: ${error.message}`,true);renderEvents.dispatchEvent(new CustomEvent('failed',{detail:error}));}
 finally{busy=false;if(clicks.length)pending=true;if(pending)void flush();}
}
let lastResize='';
function resize(){const controlHeight=document.querySelector('.preview-controls').getBoundingClientRect().height;const key=[host.clientWidth,host.clientHeight,controlHeight,terminal.options.fontSize,terminal.options.lineHeight].join(':');if(key===lastResize)return;lastResize=key;document.documentElement.style.setProperty('--controls-height',document.querySelector('.preview-controls').getBoundingClientRect().height+'px');fit.fit();requestFrame();}
new ResizeObserver(()=>requestAnimationFrame(resize)).observe(host);new ResizeObserver(()=>requestAnimationFrame(resize)).observe(document.querySelector('.preview-controls'));
terminal.onResize(requestFrame);
for(const [id,key] of [['model','model'],['scenario','scenario'],['example','example'],['modal','modal'],['popover','popover']])$(id).addEventListener('change',e=>{state[key]=e.target.value;if(key==='example'){state.focusItem='';state.messageNumber=0;state.bottom=false;state.importId='';state.data={};state.model=defaults.model;void catalog();}if(key==='modal'||key==='popover'){state.menuRow=0;$('menu-row').value=0;const other=key==='modal'?'popover':'modal';state[other]='none';$(other).value='none';}state.scroll=key==='scenario'&&state[key]!=='working'?100000:0;requestFrame();});
for(const [id,key] of [['compact','compact'],['plan','planExpanded'],['expanded','toolsExpanded'],['compact-tools','toolsCompact']])$(id).addEventListener('change',e=>{state[key]=e.target.checked;requestFrame();});
$('menu-row').addEventListener('input',e=>{state.menuRow=Number(e.target.value);requestFrame();});
$('font-size').addEventListener('input',e=>{terminal.options.fontSize=Number(e.target.value);$('font-value').textContent=e.target.value+'px';resize();});
$('line-height').addEventListener('change',e=>{terminal.options.lineHeight=Number(e.target.value);resize();});
$('reset').addEventListener('click',()=>{state={...defaults,submitted:[],resetRevision:state.resetRevision+1};clicks.length=0;syncControls();terminal.options.fontSize=12;terminal.options.lineHeight=1.1;$('font-size').value=12;$('font-value').textContent='12px';$('line-height').value='1.1';void catalog();requestFrame();resize();});
function syncControls(){$('menu-row').value=state.menuRow;for(const key of ['model','scenario','example','modal','popover'])$(key).value=state[key];for(const [id,key] of [['compact','compact'],['plan','planExpanded'],['expanded','toolsExpanded'],['compact-tools','toolsCompact']])$(id).checked=state[key];}
function scrollBy(lines){state.scroll=Math.max(0,(frame?.scrollOffset??state.scroll)+lines);state.focusItem='';state.messageNumber=0;state.bottom=false;}
terminal.onData(data=>{
 if(state.modal!=='none'||state.popover!=='none'){if(data==='\x1b'||data==='\x03'){state.modal='none';state.popover='none';syncControls();}else if(data==='\x1b[A'||data==='\x1b[B'){state.menuRow=Math.max(0,Math.min(100,state.menuRow+(data.endsWith('A')?-1:1)));$('menu-row').value=state.menuRow;}requestFrame();return;}

 if(data==='\x14'||data==='\x1b[Z'){state.planExpanded=!state.planExpanded;$('plan').checked=state.planExpanded;}
 else if(data==='\x1b[A'||data==='\x1b[B')scrollBy(data.endsWith('A')?-1:1);
 else if(data==='\x1b[5~'||data==='\x1b[6~')scrollBy(data==='\x1b[5~'?-20:20);
 else if(data==='\r'){if(state.input.trim()){state.submitted=[...state.submitted,state.input.trim()];state.scenario='idle';$('scenario').value='idle';state.scroll=100000;}state.input='';}
 else if(data==='\x7f')state.input=Array.from(state.input).slice(0,-1).join('');
 else if(data==='\x03'||data==='\x1b')state.input='';
 else if(!data.startsWith('\x1b'))state.input+=data.replace(/[\x00-\x1f\x7f]/g,'');
 requestFrame();
});
host.addEventListener('wheel',event=>{event.preventDefault();scrollBy(Math.sign(event.deltaY)*3);requestFrame();},{passive:false});
let pointer=null;host.addEventListener('pointerdown',e=>{pointer={x:e.clientX,y:e.clientY};});
host.addEventListener('click',e=>{if(!frame||!pointer||Math.hypot(e.clientX-pointer.x,e.clientY-pointer.y)>5)return;const rect=host.querySelector('.xterm-screen').getBoundingClientRect();clicks.push({x:Math.floor((e.clientX-rect.left)/rect.width*terminal.cols),y:Math.floor((e.clientY-rect.top)/rect.height*terminal.rows)});terminal.focus();requestFrame();});
async function catalog(){try{const response=await fetch('/api/catalog'+(state.importId?'?importId='+encodeURIComponent(state.importId):''));if(!response.ok)throw new Error(`HTTP ${response.status}`);const data=await response.json();lastCatalog=data;$('provider').textContent=data.provider;const picker=$('model');picker.replaceChildren(...data.models.map(model=>{const option=document.createElement('option');option.value=model.id;option.textContent=`${model.name} · ${Math.round(model.context_window/1000)}K`;return option;}));picker.value=state.model;for(const id of ['modal','popover']){const menu=$(id);menu.replaceChildren(...data[id+'s'].map(entry=>{const option=document.createElement('option');option.value=entry.id;option.textContent=entry.label;return option;}));menu.value=state[id];}const examples=$('example');examples.replaceChildren();for(const [value,label] of [['all','All types']]){const option=document.createElement('option');option.value=value;option.textContent=label;examples.append(option);}const groups=new Map();for(const entry of data.examples){let group=groups.get(entry.group);if(!group){group=document.createElement('optgroup');group.label=entry.group;groups.set(entry.group,group);examples.append(group);}const option=document.createElement('option');option.value=entry.id;option.textContent=entry.label;group.append(option);}examples.value=state.example;requestFrame();}catch(error){status(`Waiting for Go renderer: ${error.message}`,true);}}
if(import.meta.hot){import.meta.hot.on('go-renderer',event=>{if(event.state==='ready'){buildNotice=null;connected=false;void catalog();}else if(event.state==='building'){buildNotice={text:'Compiling updated Go renderer…',error:false};status('');}else{buildNotice={text:`Go build failed; showing previous renderer: ${event.error}`,error:true};status('');}});}
const reconnect=setInterval(()=>{if(!connected)void catalog();},5000);
if(import.meta.hot)import.meta.hot.dispose(()=>{clearInterval(reconnect);terminal.dispose();});
installControls({terminal,host,defaults,getState:()=>state,setState:value=>{state=value;syncControls();},requestFrame,resize,renderEvents,getRevision:()=>revision,getPaintedRevision:()=>paintedRevision,getFrame:()=>frame,getCatalog:()=>lastCatalog,reloadCatalog:catalog,getCaptureError:()=>captureError,clicks});
resize();void catalog();
