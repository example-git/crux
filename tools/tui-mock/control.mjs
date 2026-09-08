#!/usr/bin/env node
import {readFile,writeFile} from 'node:fs/promises';
// HTTP diagnostic client: no browser automation or second renderer.
const args=process.argv.slice(2),options={url:'http://127.0.0.1:8767'};let command;
try{
 for(let i=0;i<args.length;i++){if(args[i].startsWith('--')){if(!['--url','--png','--json','--file','--client'].includes(args[i]))throw Error(`Unknown option ${args[i]}`);options[args[i].slice(2)]=args[++i];}else if(!command)command=args[i];else throw Error('Pass one JSON command');}
 const body=JSON.parse(options.file?await readFile(options.file,'utf8'):command||'{"action":"inspect"}');
 if(options.client)body.clientId=options.client;
 const response=await fetch(options.url+'/api/control',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body),signal:AbortSignal.timeout(25000)});
 const receipt=await response.json();if(!response.ok)throw Error(JSON.stringify(receipt));
 if(options.png){if(!receipt.screenshot?.url)throw Error('Command must request screenshot:true or action:screenshot when using --png');const png=await fetch(new URL(receipt.screenshot.url,options.url));if(!png.ok)throw Error('PNG retrieval failed');await writeFile(options.png,Buffer.from(await png.arrayBuffer()));receipt.savedPNG=options.png;}
 if(options.json)await writeFile(options.json,JSON.stringify(receipt,null,2)+'\n');
 console.log(JSON.stringify(options.json?{revision:receipt.revision,renderer:receipt.renderer,savedPNG:receipt.savedPNG,savedJSON:options.json}:receipt,null,2));
}catch(error){console.error(error.message);process.exitCode=1;}
