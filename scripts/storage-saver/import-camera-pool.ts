import {createHash,randomUUID} from 'node:crypto';
import {realpath} from 'node:fs/promises';
import {dirname,isAbsolute,parse,relative,resolve,sep} from 'node:path';

export type DockerResult={code:number;stdout:string;stderr:string;timedOut?:boolean};
export type Docker=(args:string[],timeoutMs:number)=>Promise<DockerResult>;
export const docker:Docker=async(args,timeoutMs)=>{
  const child=Bun.spawn(['docker',...args],{stdout:'pipe',stderr:'pipe'});
  let timedOut=false;
  const timer=setTimeout(()=>{timedOut=true;child.kill('SIGKILL');},timeoutMs);
  const drain=async(stream:ReadableStream<Uint8Array>)=>{let text='',size=0;const decoder=new TextDecoder();for await(const part of stream){if(size<65536)text+=decoder.decode(part.subarray(0,65536-size),{stream:true});size+=part.length;}return text;};
  try {const [code,stdout,stderr]=await Promise.all([child.exited,drain(child.stdout),drain(child.stderr)]);return {code,stdout,stderr,timedOut};}
  finally {clearTimeout(timer);}
};
const KIND='immich-camera-converter-v1',LABEL='io.immich-import';
export function inside(root:string,path:string):string {
  const part=relative(root,path);
  if(!part||part==='..'||part.startsWith('..'+sep)||isAbsolute(part))throw Error('Pool path must be strictly inside its mount');
  return part.split(sep).join('/');
}
export async function poolRoots(sources:string[],stage:string,script:string|null) {
  if(!sources.length)throw Error('Pool requires source files');
  const files=await Promise.all(sources.map(s=>realpath(s)));
  let source=dirname(files[0]);
  for(const file of files)while(file!==source&&!file.startsWith(source+sep)){const parent=dirname(source);if(parent===source)throw Error('Pool sources have no safe common root');source=parent;}
  const target=await realpath(stage),converter=script?await realpath(script):null;
  if(source===parse(source).root||target===parse(target).root)throw Error('Pool refuses filesystem-root mounts');
  const overlaps=(a:string,b:string)=>a===b||(!isAbsolute(relative(a,b))&&relative(a,b)!=='..'&&!relative(a,b).startsWith('..'+sep));
  if(overlaps(source,target)||overlaps(target,source))throw Error('Pool source/stage mounts overlap');
  if(converter&&overlaps(target,converter))throw Error('Pool converter must be outside writable stage');
  return {source,stage:target,script:converter};
}
type Config={state:string;source:string;stage:string;script:string|null;image:string;workers:number;jobTimeoutMs:number;memoryMB:number};
type Slot={name:string;replacements:number;busy:boolean};
export class ConverterPool {
  // Caller must hold the canonical state directory's import.lock for this
  // object's entire lifetime, including stale reconciliation and close().
  readonly stateHash:string; readonly prefix:string; readonly run=randomUUID();readonly slots:Slot[];
  private closed=false;private fault:Error|null=null;
  private closing:Promise<void>|null=null;
  get failed():boolean {return this.fault!==null;}
  private lifecycle:Promise<unknown>=Promise.resolve();
  private serial<T>(fn:()=>Promise<T>):Promise<T> {const next=this.lifecycle.then(fn);this.lifecycle=next.catch(()=>{});return next;}
  constructor(readonly config:Config,private invoke:Docker=docker) {
    if(!Number.isInteger(config.workers)||config.workers<1||config.workers>16||!Number.isInteger(config.jobTimeoutMs)||config.jobTimeoutMs<1||!Number.isInteger(config.memoryMB)||config.memoryMB<128||config.memoryMB>4096)throw Error('Invalid pool limits');
    for(const path of [config.source,config.stage,config.script].filter(Boolean))if(path!.includes(','))throw Error('Docker mount paths cannot contain commas');
    this.stateHash=createHash('sha256').update(resolve(config.state).toLowerCase()).digest('hex');
    this.prefix='immich-import-'+this.stateHash.slice(0,16)+'-';
    this.slots=Array.from({length:config.workers},(_,i)=>({name:this.prefix+String(i+1).padStart(2,'0'),replacements:0,busy:false}));
  }
  private async command(args:string[],timeout=30000) {const r=await this.invoke(args,timeout);if(r.timedOut||r.code!==0)throw Error(`Pool Docker ${args[0]} failed${r.timedOut?' (timeout)':''}: ${r.stderr.slice(0,256)}`);return r;}
  private async inspect(name:string):Promise<any|null> {
    const r=await this.invoke(['container','inspect',name],30000);
    if(!r.timedOut&&r.code!==0&&/No such (?:container|object)/i.test(r.stderr))return null;
    if(r.timedOut||r.code!==0)throw Error('Cannot confirm pool container state');
    const data=JSON.parse(r.stdout);if(!Array.isArray(data)||data.length!==1||typeof data[0]?.Id!=='string')throw Error('Invalid container inspection');return data[0];
  }
  private owned(data:any,name:string,current:boolean) {
    const labels=data.Config?.Labels,slot=name.slice(this.prefix.length);
    if(!/^(?:0[1-9]|1[0-6])$/.test(slot)||data.Name!=='/'+name||labels?.[LABEL+'.kind']!==KIND||labels?.[LABEL+'.state']!==this.stateHash||labels?.[LABEL+'.slot']!==slot||!/^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/.test(labels?.[LABEL+'.run']??'')||(current&&labels[LABEL+'.run']!==this.run))throw Error('Refusing unrelated pool container');
  }
  private async remove(name:string,current:boolean) {
    const data=await this.inspect(name);if(!data)return;
    this.owned(data,name,current);
    // Remove by inspected ID: never delete an unrelated container that takes its name.
    await this.command(['rm','--force',data.Id]);
    if(await this.inspect(name))throw Error('Pool removal did not confirm absence');
  }
  async start() {
    try {
      await this.serial(async()=>{
      const found=await this.command(['ps','--all','--filter',`label=${LABEL}.kind=${KIND}`,'--filter',`label=${LABEL}.state=${this.stateHash}`,'--format','{{.Names}}']);
      for(const name of found.stdout.trim().split(/\r?\n/).filter(Boolean)) {
        if(!name.startsWith(this.prefix))throw Error('Unexpected labelled pool container name');
        await this.remove(name,false);
      }
      for(const slot of this.slots){if(this.closed)throw Error('Pool closed');await this.create(slot);}
      });
    } catch(e) {this.fault=e as Error;try{await this.close();}catch(cleanup){throw new AggregateError([e,cleanup],'Pool startup and cleanup failed');}throw e;}
  }
  private async create(slot:Slot) {
    if(this.closed)throw Error('Pool closed');
    if(await this.inspect(slot.name))throw Error('Pool slot name already occupied');
    const c=this.config;
    const args=['create','--name',slot.name,'--network','none','--restart','no','--cpus','1','--memory',`${c.memoryMB}m`,'--memory-swap',`${c.memoryMB}m`,'--pids-limit','64'];
    for(const [key,value] of Object.entries({kind:KIND,state:this.stateHash,slot:slot.name.slice(this.prefix.length),run:this.run}))args.push('--label',`${LABEL}.${key}=${value}`);
    args.push('-e','QUIET_EXEC_SENSOR_ENABLED=0','-e','AVIF_JOBS=1','--mount',`type=bind,source=${c.source},target=/input,readonly`,'--mount',`type=bind,source=${c.stage},target=/output`);
    if(c.script)args.push('--mount',`type=bind,source=${c.script},target=/converter.sh,readonly`);
    args.push(c.image,'sleep','infinity');
    await this.command(args);
    await this.command(['start',slot.name]);
    const data=await this.inspect(slot.name);if(!data)throw Error('Pool worker disappeared');this.owned(data,slot.name,true);
    if(data.State?.Running!==true||data.Config?.Image!==c.image||data.HostConfig?.Memory!==c.memoryMB*1024*1024||data.HostConfig?.MemorySwap!==c.memoryMB*1024*1024||data.HostConfig?.PidsLimit!==64||data.HostConfig?.NanoCpus!==1000000000||data.HostConfig?.NetworkMode!=='none')throw Error('Pool worker state/resource validation failed');
    for(const [destination,writable] of [['/input',false],['/output',true],...(c.script?[['/converter.sh',false]]:[])] as [string,boolean][]) {
      const mounts=data.Mounts?.filter((m:any)=>m.Destination===destination);
      if(mounts?.length!==1||mounts[0].Type!=='bind'||mounts[0].RW!==writable)throw Error('Pool worker mount validation failed');
    }
  }
  async convert(index:number,source:string,dir:string):Promise<DockerResult> {
    if(this.closed||this.fault)throw this.fault??Error('Pool closed');
    const slot=this.slots[index];if(!slot||slot.busy)throw Error('Invalid or busy pool slot');
    slot.busy=true;
    try {
      const input=inside(this.config.source,await realpath(source));const output=inside(this.config.stage,await realpath(dir))+'/output.avif';
      for(;;) {
        if(this.closed||this.fault)throw this.fault??Error('Pool closed');
        const data=await this.inspect(slot.name);
        if(data)this.owned(data,slot.name,true);
        let result:DockerResult={code:125,stdout:'',stderr:'Pool worker not running'};
        if(data?.State?.Running===true)result=await this.invoke(['exec',slot.name,...(this.config.script?['bash','/converter.sh']:['jpeg-to-avif.sh']),'/input/'+input,'/output/'+output],this.config.jobTimeoutMs);
        if(this.closed)throw Error('Pool closed');
        if(!result.timedOut&&result.code===0)return result;
        // Only explicit feature rejection is a reusable failure. Other failed
        // exec clients may leave server-side processes, so retire their worker.
        const after=await this.inspect(slot.name);if(after)this.owned(after,slot.name,true);
        if(!result.timedOut&&result.code===1&&/unsupported (?:multi-picture JPEG \(MPF\)|motion or HDR JPEG)/.test(result.stderr)&&after?.State?.Running===true)return result;
        await this.serial(async()=>{
          await this.remove(slot.name,true);
          if(this.closed)throw Error('Pool closed');
          if(slot.replacements>=2)throw Error('Pool slot replacement budget exhausted');
          slot.replacements++;
          await this.create(slot);
        });
      }
    } catch(e) {
      // A failed/uncertain lifecycle transition blocks every subsequent job.
      this.fault=e as Error;throw e;
    } finally {slot.busy=false;}
  }
  close():Promise<void> {
    this.closed=true;
    return this.closing??=this.serial(async()=>{const errors=[];for(const slot of this.slots){try{await this.remove(slot.name,true);}catch(e){errors.push(e);}}if(errors.length)throw new AggregateError(errors,'Pool cleanup incomplete; do not allocate another pool');});
  }
}
