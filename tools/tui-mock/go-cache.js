import {mkdirSync,readdirSync,existsSync,symlinkSync} from 'node:fs';
import path from 'node:path';
// Reuse installed module sources while keeping download/stat metadata writable.
export function localModuleCache(source,target){
 mkdirSync(target,{recursive:true});
 for(const entry of readdirSync(source,{withFileTypes:true})){
  if(entry.name==='cache')continue;const link=path.join(target,entry.name);if(!existsSync(link))symlinkSync(path.join(source,entry.name),link);
 }
 function seed(src,dst){mkdirSync(dst,{recursive:true});for(const entry of readdirSync(src,{withFileTypes:true})){const a=path.join(src,entry.name),b=path.join(dst,entry.name);if(entry.isDirectory())seed(a,b);else if(!existsSync(b))symlinkSync(a,b);}}
 const download=path.join(source,'cache/download');if(existsSync(download))seed(download,path.join(target,'cache/download'));
 return target;
}
