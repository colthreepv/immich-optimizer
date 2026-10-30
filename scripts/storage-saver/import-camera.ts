import { createHash } from 'node:crypto';
import { mkdir, open, readFile, rename, stat, unlink } from 'node:fs/promises';
import { basename, dirname, extname, join, resolve } from 'node:path';
import {ConverterPool,poolRoots} from './import-camera-pool';

export type Source = {source_path:string;relative_path:string;size_bytes:number;last_write_utc:string;source_sha1:string;source_sha256:string};
type Prepared = {source:Source;path:string;sha1:string;sha256:string;size:number;filename:string;preset:string;reason:string};
const KEY = 'storage-saver-source';
const PRESET = 'avif-q50-s6-420-v1';
export function object(v:unknown):Record<string,any> { if (!v || typeof v !== 'object' || Array.isArray(v)) throw Error('Expected JSON object'); return v as Record<string,any>; }
export function parseSource(v:unknown):Source {
  const s=object(v);
  for (const k of ['source_path','relative_path','last_write_utc','source_sha1','source_sha256']) if(typeof s[k] !== 'string') throw Error(`Invalid manifest ${k}`);
  if(!/^[a-f0-9]{40}$/.test(s.source_sha1)||!/^[a-f0-9]{64}$/.test(s.source_sha256)||!Number.isSafeInteger(s.size_bytes)||s.size_bytes<1||!Number.isFinite(Date.parse(s.last_write_utc))) throw Error('Invalid manifest hashes/size/date');
  if(!/\.jpe?g$/i.test(s.source_path)) throw Error('Only JPEG sources are allowed');
  return s as Source;
}
export async function digest(path:string) {
  const a=createHash('sha1'), b=createHash('sha256'); let size=0;
  for await(const chunk of Bun.file(path).stream()) {a.update(chunk);b.update(chunk);size+=chunk.length;}
  return {sha1:a.digest('hex'),sha256:b.digest('hex'),size};
}
export async function checkSource(s:Source) {
  const h=await digest(s.source_path);
  if(h.sha1!==s.source_sha1||h.sha256!==s.source_sha256||h.size!==s.size_bytes) throw Error('Source differs from immutable manifest');
}
export function fallbackReason(stderr:string):string|null {
  return stderr.match(/unsupported (?:multi-picture JPEG \(MPF\)|motion or HDR JPEG)/)?.[0] ?? null;
}
export function converterCommand(image:string, source:string, dir:string, script:string|null):string[] {
  const command=['docker','run','--rm','--network','none','--cpus','1','-e','QUIET_EXEC_SENSOR_ENABLED=0','-e','AVIF_JOBS=1','--mount',`type=bind,source=${dirname(source)},target=/input,readonly`,'--mount',`type=bind,source=${dir},target=/output`];
  if(script)command.push('--mount',`type=bind,source=${script},target=/converter.sh,readonly`);
  command.push(image,...(script?['bash','/converter.sh']:['jpeg-to-avif.sh']),`/input/${basename(source)}`,'/output/output.avif');
  return command;
}
export function parseDuplicate(value:unknown, hash:string):string|null {
  const root=object(value); if(!Array.isArray(root.results)||root.results.length!==1) throw Error('Invalid bulk check results');
  const r=object(root.results[0]); if(r.id!==hash) throw Error('Unrelated bulk check result');
  if(r.action==='accept') return null;
  if(r.action!=='reject'||r.reason!=='duplicate'||typeof r.assetId!=='string'||!r.assetId||r.isTrashed!==false) throw Error('Duplicate rejected, trashed, or unknown trash state');
  return r.assetId;
}
async function atomic(path:string, value:unknown) {
  const tmp=path+'.tmp'; const f=await open(tmp,'w');
  try {await f.writeFile(JSON.stringify(value,null,2)+'\n');await f.sync();} finally {await f.close();}
  await rename(tmp,path);
}
export async function main(args=process.argv.slice(2)) {
  const options=new Map<string,string>();
  for(let i=0;i<args.length;i++) {if(!args[i].startsWith('--')) throw Error('Expected named option'); const k=args[i].slice(2);if(options.has(k))throw Error(`Duplicate option ${k}`); options.set(k,args[i+1]?.startsWith('--')||!args[i+1]?'true':args[++i]);}
  const allowed=['manifest','state','workers','limit','upload','bulk-reviewed','download-verify','image','recheck','converter-script','persistent-workers','worker-timeout-seconds','pool-memory-mb'];
  for(const k of options.keys()) if(!allowed.includes(k))throw Error(`Unknown option ${k}`);
  const required=(k:string)=>{const v=options.get(k);if(!v||v==='true')throw Error(`--${k} is required`);return v;};
  const manifest=resolve(required('manifest')), state=resolve(required('state'));
  const workers=Number(options.get('workers')??'4'),limit=Number(options.get('limit')??'3');
  if(!Number.isInteger(workers)||workers<1||workers>16||!Number.isInteger(limit)||limit<1)throw Error('Invalid workers/limit');
  const upload=options.has('upload'), bulk=options.has('bulk-reviewed');
  if(upload&&limit>10&&!bulk)throw Error('More than 10 uploads requires --bulk-reviewed after parent pilot approval');
  const verifyDownload=options.has('download-verify')||!bulk;
  const converterScript=options.has('converter-script')?resolve(required('converter-script')):null;
  const persistent=options.has('persistent-workers');
  if(persistent&&options.get('persistent-workers')!=='true')throw Error('--persistent-workers is a boolean flag');
  const workerTimeout=Number(options.get('worker-timeout-seconds')??'300'),poolMemory=Number(options.get('pool-memory-mb')??'1024');
  if(!Number.isInteger(workerTimeout)||workerTimeout<1||workerTimeout>3600||!Number.isInteger(poolMemory)||poolMemory<128||poolMemory>4096)throw Error('Invalid pool timeout/memory');
  if(converterScript&&!(await stat(converterScript)).isFile())throw Error('--converter-script must name a file');
  const base=(process.env.IUO_IMMICH_URL??'').replace(/\/$/,''); const key=process.env.IUO_IMMICH_API_KEY??'';
  if(upload&&(!base||!key))throw Error('Set IUO_IMMICH_URL (direct backend) and IUO_IMMICH_API_KEY');
  await mkdir(state,{recursive:true});
  const lock=await open(join(state,'import.lock'),'wx'); await lock.writeFile(String(process.pid));
  let pool:ConverterPool|null=null,stopping=false;
  const stop=()=>{stopping=true;process.exitCode=130;void pool?.close().catch(e=>console.error(String(e)));};
  try {
    const request=async(path:string,init:RequestInit={})=>{
      const r=await fetch(base+path,{...init,headers:{'x-api-key':key,...init.headers},signal:AbortSignal.timeout(180000)});
      if(!r.ok){await r.body?.cancel();throw Error(`Immich ${init.method??'GET'} ${path} HTTP ${r.status}`);} return r;
    };
    const json=async(path:string,init:RequestInit={})=>{const text=await (await request(path,init)).text();return text?JSON.parse(text):null;};
    const duplicate=async(hash:string)=>parseDuplicate(await json('/api/assets/bulk-upload-check',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({assets:[{id:hash,checksum:hash}]})}),hash);
    const owner=upload?object(await json('/api/users/me')).id:'prepare-only'; if(typeof owner!=='string'||!owner)throw Error('Invalid owner');
    const rows=(await readFile(manifest,'utf8')).split(/\r?\n/).filter(Boolean).map(x=>parseSource(JSON.parse(x))).slice(0,limit);
    if(new Set(rows.map(s=>s.source_sha1)).size!==rows.length)throw Error('Repeated source SHA1 in manifest');
    if(persistent){
      const roots=await poolRoots(rows.map(s=>s.source_path),state,converterScript);
      pool=new ConverterPool({state:roots.stage,...roots,image:options.get('image')??'colthreepv/immich-optimizer:storage-saver-ccc6b3e',workers,jobTimeoutMs:workerTimeout*1000,memoryMB:poolMemory});
      process.on('SIGINT',stop);process.on('SIGTERM',stop);
      await pool.start();
    }
    let cursor=0,failed=0,completed=0;const successful=new Set<string>();
    const started=Date.now();
    const run=async(s:Source,slot:number)=>{
      if(stopping||pool?.failed)throw Error('Import stopping');
      const dir=join(state,s.source_sha1); await mkdir(dir,{recursive:true});
      await checkSource(s);
      if(upload&&!options.has('recheck')) {
        let prior:any=null;try{prior=JSON.parse(await readFile(join(dir,'result.json'),'utf8'));}catch(e:any){if(e.code!=='ENOENT')throw e;}
        if(prior) {
          const r=object(prior.record);
          if(r.owner_id!==owner||r.source_sha1!==s.source_sha1||r.source_size!==s.size_bytes||prior.source_sha256!==s.source_sha256||typeof r.asset_id!=='string'||!r.asset_id||!/^[a-f0-9]{40}$/.test(r.output_sha1)||!/^[a-f0-9]{64}$/.test(prior.output_sha256)||!Number.isSafeInteger(r.output_size)||r.output_size<1||!['uploaded','verified'].includes(r.state)||!['download-sha256','server-checksum-size','server-checksum-pending-size'].includes(prior.verification))throw Error('Completed checkpoint invalid or belongs to another owner/source');
          if(r.state==='verified'&&prior.verification!=='download-sha256')throw Error('Invalid verified checkpoint evidence');
          console.log(JSON.stringify({source:s.relative_path,assetId:r.asset_id,phase:'checkpoint',verification:prior.verification}));return;
        }
      }
      const checkpoint=join(dir,'prepared.json'); let p:Prepared;
      let existing:string|null=upload?await duplicate(s.source_sha1):null;
      if(existing) p={source:s,path:s.source_path,...await digest(s.source_path),filename:basename(s.source_path),preset:'identity',reason:'existing-original'};
      else {
        let cached:any=null; try{cached=JSON.parse(await readFile(checkpoint,'utf8'));}catch(e:any){if(e.code!=='ENOENT')throw e;}
        if(cached) {
          p=cached; const h=await digest(p.path);
          if(JSON.stringify(p.source)!==JSON.stringify(s)||h.sha1!==p.sha1||h.sha256!==p.sha256||h.size!==p.size)throw Error('Prepared checkpoint mismatch');
        } else {
          const out=join(dir,'output.avif'); const image=options.get('image')??'colthreepv/immich-optimizer:storage-saver-ccc6b3e';
          let stderr:string,code:number;
          if(pool){const result=await pool.convert(slot,s.source_path,dir);stderr=result.stderr;code=result.code;}
          else {const child=Bun.spawn(converterCommand(image,s.source_path,dir,converterScript),{stdout:'ignore',stderr:'pipe'});stderr=await new Response(child.stderr).text();code=await child.exited;}
          await Bun.write(join(dir,'converter.log'),stderr);
          let reason='converted',path=out;
          if(code!==0){const unsupported=fallbackReason(stderr);if(!unsupported)throw Error(`Converter failed (${code}); inspect converter.log`);reason=unsupported;path=s.source_path;}
          else if((await stat(out)).size>=s.size_bytes){reason='AVIF not smaller';path=s.source_path;}
          await checkSource(s);
          p={source:s,path,...await digest(path),filename:path===s.source_path?basename(s.source_path):basename(s.source_path,extname(s.source_path))+'.avif',preset:path===s.source_path?'identity':PRESET,reason};
          await atomic(checkpoint,p);
        }
      }
      if(!upload){console.log(JSON.stringify({source:s.relative_path,phase:'prepared',bytes:p.size,reason:p.reason}));return;}
      existing=existing??await duplicate(p.sha1);
      const provenance={schemaVersion:1,ownerId:owner,sourceSHA1:s.source_sha1,outputSHA1:p.sha1,sourceFilename:basename(s.source_path),outputFilename:p.filename,preset:p.preset,sourceSize:s.size_bytes,outputSize:p.size};
      let id=existing;
      if(!id) {
        if(stopping)throw Error('Import stopping');
        await checkSource(s); const h=await digest(p.path); if(h.sha1!==p.sha1||h.sha256!==p.sha256)throw Error('Output changed before upload');
        const form=new FormData();
        const fields={deviceAssetId:s.source_sha1,deviceId:'bangkokian-camera-import-v1',fileCreatedAt:s.last_write_utc,fileModifiedAt:s.last_write_utc,isFavorite:'false',filename:p.filename,metadata:JSON.stringify([{key:KEY,value:provenance}])};
        for(const [k,v] of Object.entries(fields))form.set(k,v);
        form.set('assetData',Bun.file(p.path),p.filename);
        const result=object(await json('/api/assets',{method:'POST',body:form}));
        if(typeof result.id!=='string'||!result.id)throw Error('Missing upload asset ID');id=result.id;
      }
      const encoded=encodeURIComponent(id!); const asset=object(await json(`/api/assets/${encoded}`));
      const checksum=Buffer.from(p.sha1,'hex').toString('base64');
      const serverSize=asset.exifInfo?.fileSizeInByte;
      if(asset.ownerId!==owner||asset.isTrashed!==false||asset.checksum!==checksum||(serverSize!=null&&serverSize!==p.size))throw Error('Server owner/trash/checksum/size verification failed');
      const metadata=await json(`/api/assets/${encoded}/metadata`); if(!Array.isArray(metadata))throw Error('Invalid metadata response');
      const prev=metadata.filter((m:any)=>m.key===KEY);
      if(prev.length>1)throw Error('Duplicate provenance metadata');
      if(prev.length===1) {
        const value=object(prev[0].value);
        for(const [k,v] of Object.entries(provenance))if(value[k]!==v)throw Error(`Existing provenance conflict: ${k}`);
      } else {
        await json(`/api/assets/${encoded}/metadata`,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({items:[{key:KEY,value:provenance}]})});
        const saved=await json(`/api/assets/${encoded}/metadata`);
        if(!Array.isArray(saved)||saved.filter((m:any)=>m.key===KEY).length!==1)throw Error('Provenance readback missing');
        const value=object(saved.find((m:any)=>m.key===KEY).value);
        for(const [k,v] of Object.entries(provenance))if(value[k]!==v)throw Error(`Provenance readback mismatch: ${k}`);
      }
      let verification=serverSize==null?'server-checksum-pending-size':'server-checksum-size';
      if(verifyDownload) {
        const r=await request(`/api/assets/${encoded}/original`);const a=createHash('sha1'),b=createHash('sha256');let size=0;
        for await(const chunk of r.body!){a.update(chunk);b.update(chunk);size+=chunk.length;}
        if(a.digest('hex')!==p.sha1||b.digest('hex')!==p.sha256||size!==p.size)throw Error('Downloaded original mismatch'); verification='download-sha256';
      }
      const record={owner_id:owner,asset_id:id,source_sha1:s.source_sha1,output_sha1:p.sha1,source_filename:basename(s.source_path),output_filename:p.filename,preset:p.preset,state:verifyDownload?'verified':'uploaded',source_size:s.size_bytes,output_size:p.size,spool_path:'',updated_at:new Date().toISOString()};
      await atomic(join(dir,'result.json'),{record,verification,source_sha256:s.source_sha256,output_sha256:p.sha256,reason:p.reason,original_path:asset.originalPath??null,server_size:serverSize??null,exif:asset.exifInfo??null});
      console.log(JSON.stringify({source:s.relative_path,assetId:id,bytes:p.size,reason:p.reason,verification}));
    };
    await Promise.all(Array.from({length:workers},async(_,slot)=>{
      while(!stopping&&cursor<rows.length){const s=rows[cursor++];let success=false;for(let attempt=1;attempt<=3&&!stopping;attempt++){try{await run(s,slot);success=true;break;}catch(e){console.error(JSON.stringify({source:s.relative_path,attempt,error:String(e)}));if(pool?.failed){stopping=true;process.exitCode=1;}if(attempt<3&&!stopping)await Bun.sleep(1000*2**attempt);}}
        if(success){completed++;successful.add(s.source_sha1);}else failed++;
      }
    }));
    const results=[];for(const s of rows){if(!successful.has(s.source_sha1))continue;try{results.push(JSON.parse(await readFile(join(state,s.source_sha1,'result.json'),'utf8')));}catch(e:any){if(e.code!=='ENOENT')throw e;}}
    await atomic(join(state,'results.json'),results);
    await atomic(join(state,'ledger-fragment.json'),{version:1,records:results.map(r=>r.record)});
    console.log(JSON.stringify({completed,failed,seconds:(Date.now()-started)/1000,ledger:join(state,'ledger-fragment.json')}));
    if(failed)process.exitCode=1;
  } finally {
    process.off('SIGINT',stop);process.off('SIGTERM',stop);
    // An uncertain cleanup keeps the existing lock as an explicit recovery fence.
    try {await pool?.close();} finally {await lock.close();}
    await unlink(join(state,'import.lock'));
  }
}
if(import.meta.main)main().catch(e=>{console.error(String(e));process.exitCode=1;});
