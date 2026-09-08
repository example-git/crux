import {defineConfig} from 'vite';
import {localModuleCache} from './go-cache.js';
import {spawn,execFileSync} from 'node:child_process';
import {mkdirSync,existsSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import path from 'node:path';

const root=fileURLToPath(new URL('.',import.meta.url));
const repo=path.resolve(root,'../..');
const goEnv={...process.env,GOCACHE:path.join(root,'.cache','go-build')};
const goRoot=execFileSync('go',['env','GOROOT'],{encoding:'utf8'}).trim();
const adjacentModules=path.resolve(goRoot,'../gopath/pkg/mod');
const sourceModules=goEnv.GOMODCACHE||adjacentModules;
if(existsSync(sourceModules)&&sourceModules!==path.join(root,'.cache/go-mod'))goEnv.GOMODCACHE=localModuleCache(sourceModules,path.join(root,'.cache/go-mod'));
function goRenderer(){
 let child,builder,timer,building=false,dirty=false,closing=false;
 return {name:'crux-production-ui-preview',apply:'serve',configureServer(server){
  const binary=path.join(root,'.cache','preview-server');mkdirSync(path.dirname(binary),{recursive:true});
  const broadcast=(state,error='')=>server.ws.send({type:'custom',event:'go-renderer',data:{state,error}});
  async function stopChild(){if(!child||child.exitCode!==null)return;await new Promise(resolve=>{child.once('exit',resolve);child.kill('SIGTERM');});}
  function build(){
   if(closing)return;if(building){dirty=true;return;}building=true;broadcast('building');
   console.log('[Go UI] Compiling real Crux renderer…');let errors='';
   builder=spawn('go',['build','-o',binary,'./tools/tui-mock/server'],{cwd:repo,env:goEnv,stdio:['ignore','pipe','pipe']});
   builder.stdout.on('data',b=>process.stdout.write(b));builder.stderr.on('data',b=>{errors+=b;process.stderr.write(b);});
   builder.once('error',err=>{building=false;broadcast('error',err.message)});
   builder.once('exit',async code=>{
    building=false;if(closing)return;
    if(code!==0){broadcast('error',errors||'Go build failed');}else{
     await stopChild();if(closing)return;
     child=spawn(binary,[],{cwd:repo,stdio:['ignore','pipe','pipe']});
     child.stderr.on('data',b=>{process.stderr.write(b);if(String(b).includes('listening'))broadcast('ready');});
     child.stdout.on('data',b=>process.stdout.write(b));
     child.once('error',err=>broadcast('error',err.message));
     console.log('[Go UI] Started updated renderer.');
    }
    if(dirty){dirty=false;build();}
   });
  }
  server.watcher.add([path.join(repo,'internal/ui'),path.join(repo,'internal/config/preview.go')]);
  server.watcher.on('all',(event,file)=>{if(file!==path.join(repo,'internal/config/preview.go')&&!file.startsWith(path.join(repo,'internal/ui'))&&!file.startsWith(path.join(root,'server')))return;if((!file.startsWith(path.join(repo,'internal/ui/demo/web'))&&!/\.(go|json)$/.test(file))||file.endsWith('_test.go'))return;clearTimeout(timer);timer=setTimeout(build,250);});
  server.httpServer?.once('close',()=>{closing=true;clearTimeout(timer);builder?.kill('SIGTERM');child?.kill('SIGTERM');});
  build();
 }};
}
export default defineConfig({plugins:[goRenderer()],server:{host:'127.0.0.1',port:8767,strictPort:true,watch:{ignored:['**/.cache/**']},proxy:{'/api':{target:'http://127.0.0.1:8768'},'/help':{target:'http://127.0.0.1:8768'},'^/(?:\\?.*)?$':{target:'http://127.0.0.1:8768',bypass(req){const accept=req.headers.accept||'';if(accept.includes('text/html')&&!accept.includes('text/markdown'))return req.url;}}}}});
