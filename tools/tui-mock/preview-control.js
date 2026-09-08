export function installControls(ctx){
 const {terminal,host,defaults,renderEvents}=ctx;
 const clone=value=>structuredClone(value);
 const fixtureURL=()=>'/api/fixture'+(ctx.getState().importId?'?importId='+encodeURIComponent(ctx.getState().importId):'');
 const allowed=new Set(Object.keys(defaults));
 let chain=Promise.resolve(),clientId=null;
 function inspect(){
  const rect=host.querySelector('.xterm-screen').getBoundingClientRect();
  const lines=Array.from({length:terminal.rows},(_,y)=>({y,text:terminal.buffer.active.getLine(terminal.buffer.active.viewportY+y)?.translateToString(true)||''}));
  const state=clone(ctx.getState()),dataModified=Object.keys(state.data).length>0;delete state.data;
  return {revision:ctx.getPaintedRevision(),renderer:ctx.getFrame()?.renderer,state,dataModified,cols:terminal.cols,rows:terminal.rows,schemaNote:ctx.getFrame()?.schemaNote||null,scrollOffset:ctx.getFrame()?.scrollOffset||0,storedMessages:ctx.getFrame()?.storedMessages||[],imported:ctx.getFrame()?.imported||false,items:ctx.getFrame()?.items||[],regions:ctx.getFrame()?.regions||{},lines,geometry:{x:rect.x,y:rect.y,width:rect.width,height:rect.height,cellWidth:rect.width/terminal.cols,cellHeight:rect.height/terminal.rows},status:document.getElementById('bridge-status').textContent};
 }
 async function paint(){
  const target=ctx.getRevision()+1;
  await new Promise((resolve,reject)=>{
   const done=()=>{if(ctx.getPaintedRevision()>=target){cleanup();resolve();}};
   const failed=e=>{cleanup();reject(e.detail);};
   const timer=setTimeout(()=>{cleanup();reject(Error('No painted frame acknowledged within 15 seconds'));},15000);
   function cleanup(){clearTimeout(timer);renderEvents.removeEventListener('painted',done);renderEvents.removeEventListener('failed',failed);}
   renderEvents.addEventListener('painted',done);renderEvents.addEventListener('failed',failed);ctx.requestFrame();
  });
 }
 function capture(){
  if(ctx.getCaptureError())throw Error(`PNG capture unavailable: ${ctx.getCaptureError()}`);
  const canvases=[...host.querySelectorAll('.xterm-screen canvas')];
  if(!canvases.length)throw Error('xterm has no rendered canvas; PNG capture requires WebGL');
  const rect=host.querySelector('.xterm-screen').getBoundingClientRect(),scale=devicePixelRatio;
  const png=document.createElement('canvas');png.width=Math.round(rect.width*scale);png.height=Math.round(rect.height*scale);
  const draw=png.getContext('2d');draw.fillStyle=terminal.options.theme.background;draw.fillRect(0,0,png.width,png.height);
  for(const canvas of canvases){const r=canvas.getBoundingClientRect();draw.drawImage(canvas,(r.x-rect.x)*scale,(r.y-rect.y)*scale,r.width*scale,r.height*scale);}
  return {mimeType:'image/png',width:png.width,height:png.height,revision:ctx.getPaintedRevision(),dataURL:png.toDataURL('image/png')};
 }
 function checkedState(patch){
  if(!patch||typeof patch!=='object'||Array.isArray(patch))throw Error('state must be an object');
  const next=clone(ctx.getState());
  for(const [key,value]of Object.entries(patch)){
   if(!allowed.has(key))throw Error(`Unknown state field: ${key}`);
   if(typeof value==='number'&&(!Number.isFinite(value)||!Number.isInteger(value)))throw Error(`state.${key} must be an integer`);
   if(typeof value!==typeof defaults[key]||value===null)throw Error(`Invalid type for state.${key}`);
   if(key==='submitted'&&(!Array.isArray(value)||value.some(x=>typeof x!=='string')))throw Error('submitted must be an array of strings');
   if(key==='data'&&(Array.isArray(value)))throw Error('data must be an object');
   next[key]=clone(value);
  }
  if(patch.example!==undefined||patch.focusItem!==undefined)next.scroll=patch.scroll??0;
  if(patch.example!==undefined){next.focusItem=patch.focusItem??'';next.messageNumber=patch.messageNumber??0;next.bottom=patch.bottom??false;}
  if(patch.modal&&patch.modal!=='none'){if(patch.popover&&patch.popover!=='none')throw Error('Choose one modal or popover');next.popover='none';next.menuRow=patch.menuRow??0;}
  if(patch.popover&&patch.popover!=='none'){next.modal='none';next.menuRow=patch.menuRow??0;}
  return next;
 }
 async function execute(command){
  if(!command||typeof command!=='object')throw Error('Expected a command object');
  const fields={sessions:[],loadSession:['sessionId','screenshot'],inspect:[],catalog:[],data:['path'],patch:['changes','screenshot'],set:['state','fontSize','lineHeight','screenshot'],click:['cell','screenshot'],find:['text'],screenshot:[],reset:['screenshot']};
  if(!Object.hasOwn(fields,command.action))throw Error(`Unknown action: ${command.action}`);
  for(const key of Object.keys(command))if(key!=='action'&&!fields[command.action].includes(key))throw Error(`Unknown command field: ${key}`);
  if(command.action==='sessions'){const r=await fetch('/api/sessions');const value=await r.json();if(!r.ok)throw Error(value.error);return value;}
  if(command.action==='catalog')return {catalog:ctx.getCatalog()};
  if(command.action==='data'){const r=await fetch(fixtureURL());if(!r.ok)throw Error('Fixture data unavailable');const defaults=await r.json(),overrides=clone(ctx.getState().data);if(command.path!==undefined){if(typeof command.path!=='string'||!command.path.startsWith('/'))throw Error('data.path must be a JSON Pointer');let value=merge(defaults,overrides);for(const key of command.path.slice(1).split('/').map(x=>x.replaceAll('~1','/').replaceAll('~0','~'))){if(!value||!Object.hasOwn(value,key))throw Error('Unknown data path: '+command.path);value=value[key];}return {path:command.path,value};}return {defaults,overrides};}
  const previous=clone(ctx.getState());
  try{
   if(command.action==='loadSession'){if(typeof command.sessionId!=='string'||!command.sessionId)throw Error('sessionId is required');const r=await fetch('/api/sessions/load',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({sessionId:command.sessionId})});const value=await r.json();if(!r.ok)throw Error(value.error);ctx.setState({...clone(defaults),importId:value.importId,model:value.model,scenario:'idle',resetRevision:previous.resetRevision+1});document.getElementById('saved-session').value=value.sessionId;await ctx.reloadCatalog();}
   if(command.action==='set'){
    const next=checkedState(command.state||{});
    for(const [key,min,max]of [['fontSize',9,18],['lineHeight',1,1.5]])if(command[key]!==undefined){if(typeof command[key]!=='number'||command[key]<min||command[key]>max)throw Error(`${key} must be ${min}–${max}`);terminal.options[key]=command[key];document.getElementById(key==='fontSize'?'font-size':'line-height').value=command[key];if(key==='fontSize')document.getElementById('font-value').textContent=command[key]+'px';}
    ctx.setState(next);ctx.resize();
   }
   if(command.action==='patch'){
    if(!Array.isArray(command.changes)||!command.changes.length)throw Error('changes must be a nonempty array of {path,value}');
    const response=await fetch(fixtureURL());if(!response.ok)throw Error('Fixture data unavailable');
    const data=merge(await response.json(),previous.data);
    for(const change of command.changes){if(typeof change.path!=='string'||!change.path.startsWith('/'))throw Error('Use JSON pointer paths such as /session/Title');const parts=change.path.slice(1).split('/').map(x=>x.replaceAll('~1','/').replaceAll('~0','~'));let parent=data;for(const key of parts.slice(0,-1)){if(!parent||!Object.hasOwn(parent,key)||['__proto__','constructor','prototype'].includes(key))throw Error(`Unknown data path: ${change.path}`);parent=parent[key];}const key=parts.at(-1);if(!parent||!Object.hasOwn(parent,key)||['__proto__','constructor','prototype'].includes(key)||!Object.hasOwn(change,'value'))throw Error(`Unknown data path or missing value: ${change.path}`);parent[key]=clone(change.value);}
    ctx.setState({...previous,data,resetRevision:previous.resetRevision+1});
   }
   if(command.action==='reset'){ctx.setState(clone({...defaults,resetRevision:previous.resetRevision+1}));ctx.clicks.length=0;terminal.options.fontSize=12;terminal.options.lineHeight=1.1;document.getElementById('font-size').value=12;document.getElementById('font-value').textContent='12px';document.getElementById('line-height').value='1.1';await ctx.reloadCatalog();ctx.resize();}
   if(command.action==='click'){
    const {x,y}=command.cell||{};if(!Number.isInteger(x)||!Number.isInteger(y)||x<0||y<0||x>=terminal.cols||y>=terminal.rows)throw Error('cell must contain zero-based integer x/y within the terminal');
    if(previous.modal!=='none'||previous.popover!=='none')throw Error('Menu selection uses state.menuRow; menu actions are not executed by this fixture');
    ctx.clicks.push({x,y});
   }
   await paint();const result=inspect();
   if(command.action==='find'){if(typeof command.text!=='string'||!command.text.length)throw Error('find requires nonempty text');result.matches=[];for(let y=0;y<terminal.rows;y++){const line=terminal.buffer.active.getLine(terminal.buffer.active.viewportY+y);let text='',cells=[];for(let x=0;x<terminal.cols;x++){const cell=line?.getCell(x);if(cell?.getWidth()===0)continue;const chars=cell?.getChars()||' ';for(const c of chars){text+=c;for(let n=0;n<c.length;n++)cells.push(x);}}let start=0,index;while((index=text.indexOf(command.text,start))!==-1){const x=cells[index];result.matches.push({x,y,point:{x:result.geometry.x+(x+.5)*result.geometry.cellWidth,y:result.geometry.y+(y+.5)*result.geometry.cellHeight}});start=index+command.text.length;}}}
   if(command.action==='screenshot'||command.screenshot)result.screenshot=capture();
   return result;
  }catch(error){ctx.setState(previous);throw error;}
 }
 function merge(base,patch){for(const [key,value]of Object.entries(patch)){if(['__proto__','constructor','prototype'].includes(key))throw Error('Invalid data key');base[key]=value&&typeof value==='object'&&!Array.isArray(value)&&base[key]&&typeof base[key]==='object'?merge(base[key],value):clone(value);}return base;}
 const control=command=>{const job=chain.then(()=>execute(command));chain=job.catch(()=>{});return job;};
 window.cruxPreview={control,inspect};
 const report=job=>job.catch(error=>{document.getElementById('control-result').textContent='Error: '+error.message;document.getElementById('control-api').open=true;});
 document.getElementById('jump-top').onclick=()=>report(control({action:'set',state:{focusItem:'',messageNumber:0,bottom:false,scroll:0}}));
 document.getElementById('jump-bottom').onclick=()=>report(control({action:'set',state:{focusItem:'',messageNumber:0,bottom:true,scroll:0}}));
 document.getElementById('jump-go').onclick=()=>report(control({action:'set',state:{focusItem:'',messageNumber:Number(document.getElementById('jump-message').value),bottom:false,scroll:0}}));
 document.getElementById('session-load').onclick=()=>report(control({action:'loadSession',sessionId:document.getElementById('saved-session').value}));
 let sourceLoaded=false;
 async function sessionCatalog(){try{const r=await fetch('/api/sessions');if(!r.ok)throw Error('Session source unavailable');const source=await r.json();const picker=document.getElementById('saved-session');picker.replaceChildren(...source.sessions.map(row=>{const option=document.createElement('option');option.value=row.id;option.textContent=`${row.title||row.id} · ${row.shortId||row.id} · ${row.messageCount} messages${row.parentId?' · child':''}`;return option;}));picker.disabled=!source.enabled;document.getElementById('session-load').disabled=!source.enabled||!source.sessions.length;document.getElementById('session-source').textContent=source.enabled?'Read-only source: '+source.project:'Dummy data · use --project to enable saved sessions';if(!sourceLoaded&&source.initialSessionId){sourceLoaded=true;await control({action:'loadSession',sessionId:source.initialSessionId});}else sourceLoaded=true;}catch(error){document.getElementById('session-source').textContent=error.message;}}
 void sessionCatalog();

 document.getElementById('control-apply').addEventListener('click',async()=>{const output=document.getElementById('control-result');try{output.textContent='Waiting for painted frame…';const result=await control(JSON.parse(document.getElementById('control-command').value));if(result.screenshot){let image=document.getElementById('captured-frame');if(!image){image=document.createElement('img');image.id='captured-frame';image.alt='Captured xterm frame';image.style.maxWidth='100%';output.after(image);}image.src=result.screenshot.dataURL;}output.textContent=JSON.stringify(result,(key,value)=>key==='dataURL'?'[PNG shown below]':value,2);}catch(error){output.textContent='Error: '+error.message;}});
 let events,retry,disposed=false;function connect(){if(disposed)return;events=new EventSource('/api/control/events');events.onerror=()=>{events.close();document.getElementById('control-connection').textContent='Control API reconnecting…';retry=setTimeout(connect,1500);};events.onmessage=async event=>{const data=JSON.parse(event.data);if(data.clientId){clientId=data.clientId;if(!sourceLoaded)void sessionCatalog();document.getElementById('control-connection').textContent='Control API connected';return;}let response;try{response={result:await control(data.command)};}catch(error){response={error:error.message};}await fetch('/api/control/result',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:data.id,clientId,...response})});};}connect();
 if(import.meta.hot)import.meta.hot.dispose(()=>{disposed=true;clearTimeout(retry);events.close();delete window.cruxPreview;});
}
