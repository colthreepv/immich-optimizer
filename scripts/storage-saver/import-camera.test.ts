import {test,expect} from 'bun:test';
import {mkdtemp,readFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {checkSource,digest,fallbackReason,main,parseDuplicate,parseSource} from './import-camera';

test('bulk duplicate checks reject ambiguous and trashed responses',()=>{
  const r={id:'a',action:'reject',reason:'duplicate',assetId:'id',isTrashed:false};
  expect(parseDuplicate({results:[r]},'a')).toBe('id');
  for(const invalid of [{...r,isTrashed:true},{...r,isTrashed:undefined},{...r,id:'other'},{...r,reason:'other'}])expect(()=>parseDuplicate({results:[invalid]},'a')).toThrow();
  expect(parseDuplicate({results:[{id:'a',action:'accept'}]},'a')).toBeNull();
});
test('fallback only permits explicit feature exclusions',()=>{
  expect(fallbackReason('jpeg-to-avif.sh: unsupported motion or HDR JPEG')).toBe('unsupported motion or HDR JPEG');
  expect(fallbackReason('unsupported multi-picture JPEG (MPF)')).toBe('unsupported multi-picture JPEG (MPF)');
  expect(fallbackReason('metadata validation failed for GPSLatitude')).toBeNull();
});
test('bulk uploads require explicit parent review before touching state',async()=>{
  await expect(main(['--manifest','unused','--state','unused','--limit','8180','--upload'])).rejects.toThrow('--bulk-reviewed');
});
test('lost upload acknowledgement resumes by output checksum, verifies download and exports provenance',async()=>{
  const dir=await mkdtemp(join(tmpdir(),'camera-import-'));const oldUrl=process.env.IUO_IMMICH_URL,oldKey=process.env.IUO_IMMICH_API_KEY;
  let server:ReturnType<typeof Bun.serve>|undefined;
  try {
    const source=join(dir,'camera.jpg');await Bun.write(source,'source jpeg fixture');const a=await digest(source);
    const s=parseSource({source_path:source,relative_path:'camera.jpg',size_bytes:a.size,last_write_utc:'2023-04-08T11:36:35Z',source_sha1:a.sha1,source_sha256:a.sha256});
    await checkSource(s);expect(()=>parseSource({...s,source_sha256:'bad'})).toThrow();
    const state=join(dir,'state'),item=join(state,a.sha1);await Bun.write(join(item,'output.avif'),'avif');const b=await digest(join(item,'output.avif'));
    await Bun.write(join(item,'prepared.json'),JSON.stringify({source:s,path:join(item,'output.avif'),...b,filename:'camera.avif',preset:'avif-q50-s6-420-v1',reason:'converted'}));
    const manifest=join(dir,'manifest.jsonl');await Bun.write(manifest,JSON.stringify(s)+'\n');
    let uploads=0,metadata:any[]=[],present=false;
    server=Bun.serve({port:0,async fetch(req){
      const path=new URL(req.url).pathname;
      if(req.headers.get('x-api-key')!=='test-key')return new Response(null,{status:401});
      if(path==='/api/users/me')return Response.json({id:'owner'});
      if(path.endsWith('bulk-upload-check')){const body:any=await req.json();const hash=body.assets[0].checksum;return Response.json({results:[present&&hash===b.sha1?{id:hash,action:'reject',reason:'duplicate',assetId:'asset',isTrashed:false}:{id:hash,action:'accept'}]});}
      if(path==='/api/assets'&&req.method==='POST'){uploads++;const f=await req.formData();expect(await (f.get('assetData') as File).text()).toBe('avif');metadata=JSON.parse(String(f.get('metadata')));present=true;return new Response(null,{status:503});}
      if(path.endsWith('/metadata'))return Response.json(metadata);
      if(path.endsWith('/original'))return new Response('avif');
      if(path==='/api/assets/asset')return Response.json({ownerId:'owner',isTrashed:false,checksum:Buffer.from(b.sha1,'hex').toString('base64'),exifInfo:{fileSizeInByte:b.size},originalPath:'/upload/asset.avif'});
      return new Response(null,{status:404});
    }});
    process.env.IUO_IMMICH_URL=server.url.toString().replace(/\/$/,'');process.env.IUO_IMMICH_API_KEY='test-key';
    await main(['--manifest',manifest,'--state',state,'--limit','1','--workers','1','--upload']);
    expect(uploads).toBe(1);
    const ledger=JSON.parse(await readFile(join(state,'ledger-fragment.json'),'utf8'));
    expect(ledger.records[0]).toMatchObject({state:'verified',source_sha1:a.sha1,output_sha1:b.sha1,asset_id:'asset'});
    await Bun.write(source,'changed');await expect(checkSource(s)).rejects.toThrow('immutable manifest');
  } finally {server?.stop(true);if(oldUrl===undefined)delete process.env.IUO_IMMICH_URL;else process.env.IUO_IMMICH_URL=oldUrl;if(oldKey===undefined)delete process.env.IUO_IMMICH_API_KEY;else process.env.IUO_IMMICH_API_KEY=oldKey;await rm(dir,{recursive:true,force:true});}
});
