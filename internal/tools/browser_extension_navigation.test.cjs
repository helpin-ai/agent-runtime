const {readFileSync}=require('node:fs');
const {runInNewContext}=require('node:vm');
const {test}=require('node:test');
const assert=require('node:assert/strict');
const path=require('node:path');

const source=readFileSync(path.join(__dirname,'browser_extension/runtime-worker.js'),'utf8').split("chrome.alarms.create('runtime-bridge'")[0];
const missing=()=>new Error('Could not establish connection. Receiving end does not exist.');
const ready={success:true,data:{ready:true}};
function driver(steps) {
 let now=0;
 const calls=[];
 const sendMessage=async (...args)=>{
  calls.push(args[1].args[0]);
  const value=steps.length ? steps.shift() : missing();
  if(value instanceof Error)throw value;
  return value;
 };
 const command=runInNewContext(source+'\npageCommand',{
  importScripts(){},self:{RUNTIME_BRIDGE:{}},chrome:{tabs:{sendMessage}},
  Date:{now:()=>now},setTimeout(callback,ms){now+=ms;callback();},
 });
 return {command,calls};
}

test('reads recover through navigation within the readiness bound',async()=>{
 const {command,calls}=driver([missing(),ready,missing(),ready,{success:true,data:{text:'Listing'}}]);
 const value=await command(1,{args:['get','text','body'],expires:10000});
 assert.equal(value.text,'Listing');
 assert.deepEqual(calls,['ready','ready','get','ready','get']);
});

test('snapshots retry but uncertain clicks never replay',async()=>{
 const snapshot=driver([ready,missing(),ready,{success:true,data:{snapshot:'Fresh refs'}}]);
 assert.equal((await snapshot.command(1,{args:['snapshot'],expires:10000})).snapshot,'Fresh refs');
 const click=driver([ready,missing()]);
 await assert.rejects(click.command(1,{args:['click','@e1'],expires:10000}),/do not repeat it automatically/);
 assert.deepEqual(click.calls,['ready','click']);
});

test('missing content script stops at the navigation deadline',async()=>{
 const {command,calls}=driver([]);
 await assert.rejects(command(1,{args:['snapshot'],expires:10000}),/still navigating/);
 assert.equal(calls.length,50);
});
