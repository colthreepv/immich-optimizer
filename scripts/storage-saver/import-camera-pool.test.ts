import {test,expect} from 'bun:test';
import {mkdtemp,mkdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join,parse} from 'node:path';
import {ConverterPool,poolRoots,type Docker,type DockerResult} from './import-camera-pool';

const ok=(stdout=''):DockerResult=>({code:0,stdout,stderr:''});
class FakeDocker {
  containers=new Map<string,any>();calls:string[][]=[];maxLive=0;creates=0;removeFails=false;keepAfterRemove=false;startFailsAt=0;
  exec:(name:string)=>Promise<DockerResult>=async()=>ok();
  run:Docker=async(args)=>{
    this.calls.push(args);
    const [command]=args;
    if(command==='ps')return ok([...this.containers.keys()].join('\n'));
    if(command==='container')return this.containers.has(args[2])?ok(JSON.stringify([this.containers.get(args[2])])):{code:1,stdout:'',stderr:'No such container'};
    if(command==='create'){
      const name=args[args.indexOf('--name')+1];if(this.containers.has(name))return {code:1,stdout:'',stderr:'conflict'};
      const labels:Record<string,string>={};for(let i=0;i<args.length;i++)if(args[i]==='--label'){const value=args[++i],split=value.indexOf('=');labels[value.slice(0,split)]=value.slice(split+1);}
      this.creates++;
      const image=args[args.indexOf('sleep')-1];
      this.containers.set(name,{Id:name+'-id',Name:'/'+name,Config:{Labels:labels,Image:image},State:{Running:false},HostConfig:{Memory:Number(args[args.indexOf('--memory')+1].slice(0,-1))*1024*1024,MemorySwap:Number(args[args.indexOf('--memory-swap')+1].slice(0,-1))*1024*1024,PidsLimit:64,NanoCpus:1000000000,NetworkMode:'none'},Mounts:[{Type:'bind',Destination:'/input',RW:false},{Type:'bind',Destination:'/output',RW:true},...(args.some(x=>x.includes('target=/converter.sh,'))?[{Type:'bind',Destination:'/converter.sh',RW:false}]:[])]});
      this.maxLive=Math.max(this.maxLive,this.containers.size);return ok(name+'-id');
    }
    if(command==='start'){if(this.creates===this.startFailsAt)return {code:1,stdout:'',stderr:'start failed'};this.containers.get(args[1]).State.Running=true;return ok();}
    if(command==='rm'){if(this.removeFails)return {code:1,stdout:'',stderr:'daemon unavailable'};if(!this.keepAfterRemove)for(const [name,c] of this.containers)if(c.Id===args[2])this.containers.delete(name);return ok();}
    if(command==='exec')return this.exec(args[1]);
    throw Error('Unexpected fake Docker command '+command);
  };
}
async function fixture(workers=2) {
  const root=await mkdtemp(join(tmpdir(),'camera-pool-')),source=join(root,'source'),stage=join(root,'stage');
  await mkdir(source);await mkdir(stage);await mkdir(join(stage,'item'));
  const file=join(source,'camera.jpg');await Bun.write(file,'jpeg fixture');
  const roots=await poolRoots([file],stage,null),fake=new FakeDocker();
  const config={state:roots.stage,...roots,image:'test-image',workers,jobTimeoutMs:10,memoryMB:1024};
  return {root,file,dir:join(stage,'item'),fake,config,pool:new ConverterPool(config,fake.run)};
}
test('N fixed slots, exact limits and repeated jobs do not proliferate containers',async()=>{
  const f=await fixture();try{
    await f.pool.start();
    for(let i=0;i<5;i++)await Promise.all([f.pool.convert(0,f.file,f.dir),f.pool.convert(1,f.file,f.dir)]);
    expect(f.fake.maxLive).toBe(2);expect(f.fake.creates).toBe(2);
    const create=f.fake.calls.find(x=>x[0]==='create')!;
    expect(create).toContain('type=bind,source='+f.config.source+',target=/input,readonly');
    for(const flag of ['--memory','--memory-swap','--pids-limit','--cpus','--network','--restart'])expect(create).toContain(flag);
    await f.pool.close();expect(f.fake.containers.size).toBe(0);
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
test('stable slot names reconcile stale workers before allocating a smaller pool',async()=>{
  const f=await fixture(3);try{
    await f.pool.start();const oldNames=[...f.fake.containers.keys()];
    const next=new ConverterPool({...f.config,workers:1},f.fake.run);expect(next.slots[0].name).toBe(oldNames[0]);
    await next.start();expect(f.fake.containers.size).toBe(1);expect(f.fake.creates).toBe(4);
    expect(f.fake.calls.filter(x=>x[0]==='rm').length).toBe(3);await next.close();
  }finally{await rm(f.root,{recursive:true,force:true});}
});
test('timeouts and dead workers are removed before replacement at the same slot',async()=>{
  for(const mode of ['timeout','crash']){
    const f=await fixture(1);try{
      await f.pool.start();let count=0;
      f.fake.exec=async(name)=>{if(count++===0){if(mode==='crash')f.fake.containers.get(name).State.Running=false;return {code:137,stdout:'',stderr:'lost',timedOut:mode==='timeout'};}return ok();};
      await f.pool.convert(0,f.file,f.dir);expect(f.fake.creates).toBe(2);expect(f.fake.maxLive).toBe(1);
      const actions=f.fake.calls.map(x=>x[0]);expect(actions.lastIndexOf('rm')).toBeLessThan(actions.lastIndexOf('create'));
    }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
  }
});
test('unconfirmed removal blocks replacement and all later jobs',async()=>{
  for(const kind of ['error','still-present']){
    const f=await fixture(1);try{
      await f.pool.start();f.fake.exec=async()=>({code:1,stdout:'',stderr:'timeout',timedOut:true});
      f.fake.removeFails=kind==='error';f.fake.keepAfterRemove=kind==='still-present';
      await expect(f.pool.convert(0,f.file,f.dir)).rejects.toThrow();expect(f.fake.creates).toBe(1);
      await expect(f.pool.convert(0,f.file,f.dir)).rejects.toThrow();expect(f.fake.creates).toBe(1);
      f.fake.removeFails=false;f.fake.keepAfterRemove=false;
    }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
  }
});
test('replacement lifetime budget is bounded across jobs',async()=>{
  const f=await fixture(1);try{
    await f.pool.start();f.fake.exec=async()=>({code:1,stdout:'',stderr:'timeout',timedOut:true});
    await expect(f.pool.convert(0,f.file,f.dir)).rejects.toThrow('budget');
    expect(f.fake.creates).toBe(3);expect(f.fake.maxLive).toBe(1);expect(f.fake.containers.size).toBe(0);
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
test('partial startup failure cleans already created owned workers',async()=>{
  const f=await fixture();try{
    f.fake.startFailsAt=2;await expect(f.pool.start()).rejects.toThrow();expect(f.fake.containers.size).toBe(0);
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
test('ownership mismatch never removes a foreign container',async()=>{
  const f=await fixture(1);try{
    await f.pool.start();const foreign=f.fake.containers.values().next().value!;foreign.Config.Labels['io.immich-import.run']='foreign';
    await expect(f.pool.close()).rejects.toThrow('cleanup incomplete');expect(f.fake.calls.filter(x=>x[0]==='rm')).toHaveLength(0);
  }finally{await rm(f.root,{recursive:true,force:true});}
});
test('closing interrupts an in-flight job and is idempotent',async()=>{
  const f=await fixture(1);try{
    await f.pool.start();let release!:()=>void,entered!:()=>void;
    const ready=new Promise<void>(r=>entered=r);f.fake.exec=async()=>{entered();await new Promise<void>(r=>release=r);return ok();};
    const job=f.pool.convert(0,f.file,f.dir);await ready;
    await f.pool.close();release();await expect(job).rejects.toThrow('closed');await f.pool.close();expect(f.fake.containers.size).toBe(0);
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
test('explicit converter script is read-only and busy slots refuse a second job',async()=>{
  const f=await fixture(1);try{
    const pool=new ConverterPool({...f.config,script:join(f.root,'converter.sh')},f.fake.run);await pool.start();
    let release!:()=>void,entered!:()=>void;const ready=new Promise<void>(r=>entered=r);
    f.fake.exec=async()=>{entered();await new Promise<void>(r=>release=r);return ok();};
    const job=pool.convert(0,f.file,f.dir);await ready;
    await expect(pool.convert(0,f.file,f.dir)).rejects.toThrow('busy');release();await job;
    expect(f.fake.calls.find(x=>x[0]==='create')).toContain('type=bind,source='+join(f.root,'converter.sh')+',target=/converter.sh,readonly');
    await pool.close();
  }finally{await rm(f.root,{recursive:true,force:true});}
});
test('explicit unsupported-feature failure preserves identity fallback without replacement',async()=>{
  const f=await fixture(1);try{
    await f.pool.start();f.fake.exec=async()=>({code:1,stdout:'',stderr:'import-camera-convert: unsupported motion or HDR JPEG'});
    expect((await f.pool.convert(0,f.file,f.dir)).code).toBe(1);expect(f.fake.creates).toBe(1);
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
test('roots reject overlapping mounts and filesystem-root common ancestors',async()=>{
  const f=await fixture(1);try{
    await expect(poolRoots([f.file],f.config.source,null)).rejects.toThrow('overlap');
    await expect(poolRoots([f.file],parse(f.config.stage).root,null)).rejects.toThrow('filesystem-root');
    await expect(f.pool.convert(0,join(f.root,'outside.jpg'),f.dir)).rejects.toThrow();
  }finally{await f.pool.close();await rm(f.root,{recursive:true,force:true});}
});
