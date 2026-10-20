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
 return {command,calls,elapsed:()=>now};
}

test('acknowledged clicks settle in the worker without replay and within expiry',async()=>{
 for(const expires of [100,10000]) {
  const d=driver([ready,{success:true,data:{effect:'click_dispatched'}}]);
  assert.equal((await d.command(1,{args:['click','@e1'],expires})).effect,'click_dispatched');
  assert.equal(d.elapsed(),Math.min(200,expires));
  assert.deepEqual(d.calls,['ready','click']);
 }
});

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

function screenshotDriver(capture, active=true) {
 const timers=[];
 const execute=runInNewContext(source+'\nexecute',{
  chrome:{
   storage:{session:{get:async()=>({tabId:1,session:'run',epoch:'epoch'}),set:async()=>{}}},
   tabs:{get:async()=>({id:1,windowId:2,active,title:'Fixture'}),captureVisibleTab:capture},
  },
  setTimeout(callback,ms){const timer={callback,ms,cleared:false};timers.push(timer);return timer;},
  clearTimeout(timer){if(timer)timer.cleared=true;},
 });
 const command={session:'run',epoch:'epoch',args:['screenshot','png'],expires:Date.now()+2000};
 return {execute,command,timers};
}

test('screenshot captures only the owned active tab and clears its timer',async()=>{
 const driver=screenshotDriver(async(windowId,options)=>{
  assert.equal(windowId,2);
  assert.equal(options.format,'png');
  return 'data:image/png;base64,fixture';
 });
 assert.equal((await driver.execute(driver.command)).image,'data:image/png;base64,fixture');
 assert.equal(driver.timers[0].cleared,true);
 const inactive=screenshotDriver(()=>assert.fail('must not capture another tab'),false);
 await assert.rejects(inactive.execute(inactive.command),/Activate the bot/);
 assert.equal(inactive.timers.length,0);
});

test('stalled screenshot returns a bounded error and subsequent commands work',async()=>{
 const driver=screenshotDriver(()=>new Promise(()=>{}));
 const result=driver.execute(driver.command);
 const failure=assert.rejects(result,/Screenshot capture timed out/);
 await new Promise(setImmediate);
 assert.ok(driver.timers[0].ms>0 && driver.timers[0].ms<=2000);
 driver.timers[0].callback();
 await failure;
 assert.equal(driver.timers[0].cleared,true);
 const next={...driver.command,args:['get','title']};
 assert.equal((await driver.execute(next)).title,'Fixture');
});

test('page senders cannot issue extension commands; private expired commands fail',async()=>{
 let listener;
 const content=readFileSync(path.join(__dirname,'browser_extension/content.js'),'utf8');
 runInNewContext(content,{chrome:{runtime:{id:'private-extension',onMessage:{addListener(value){listener=value;}}}},document:{}});
 for(const sender of [{id:'foreign-extension'},{id:'private-extension',tab:{id:1}},{tab:{id:1}}]) {
  assert.equal(listener({agentRuntime:true,args:['reset']},sender,()=>assert.fail('untrusted sender reached command handler')),undefined);
 }
 const reply=await new Promise(resolve=>{
  assert.equal(listener({agentRuntime:true,args:['snapshot'],expires:0},{id:'private-extension'},resolve),true);
 });
 assert.equal(reply.success,false);
 assert.match(reply.error,/Expired browser command/);
});

function reconnectDriver(postFails=false, failures=1) {
 let now=0, boots=0, commands=0, posts=0, actions=0;
 const timers=[];
 const api=runInNewContext(source+'\n({poll, get polling(){return polling;}})',{
  chrome:{runtime:{getURL:()=> 'chrome-extension://fixture/bridge.json'},storage:{session:{get:async()=>({tabId:1,session:'run',epoch:'epoch'}),set:async()=>{}}},tabs:{get:async()=>({id:1}),sendMessage:async(id,message)=>{if(message.args[0]==='ready')return ready;actions++;return {success:true,data:{}};}}},AbortController,
  Date:{now:()=>now},
  setTimeout(callback,ms){const timer={callback,ms,cleared:false};timers.push(timer);return timer;},
  clearTimeout(timer){if(timer)timer.cleared=true;},
  fetch:async(url,options)=>{
   if(url.startsWith('chrome-extension:')) {
    if(++boots<=failures && !postFails)throw new Error('transient startup failure');
    return {json:async()=>({url:'http://localhost/bridge',token:'private'})};
   }
   if(options.method==='POST') {posts++;throw new Error('lost acknowledgement');}
   if(postFails && commands++===0)return {ok:true,status:200,json:async()=>({id:'one',session:'run',epoch:'epoch',args:['click','@e1'],expires:10000})};
   now+=240001;
   return {ok:true,status:204};
  },
 });
 return {api,timers,stats:()=>({boots,commands,posts,actions})};
}

test('transient bootstrap reconnects promptly with one active poll',async()=>{
 const driver=reconnectDriver();
 const first=driver.api.poll();
 await driver.api.poll();
 await first;
 assert.equal(driver.stats().boots,1);
 assert.equal(driver.api.polling,false);
 const retry=driver.timers.find(timer=>!timer.cleared);
 assert.equal(retry.ms,1000);
 retry.callback();
 await new Promise(setImmediate);
 assert.equal(driver.stats().boots,2);
 assert.equal(driver.api.polling,false);
 assert.ok(driver.timers.every(timer=>timer.cleared));
});

test('lost command acknowledgement reconnects without replaying the command',async()=>{
 const driver=reconnectDriver(true);
 const pending=driver.api.poll();
 await new Promise(setImmediate);
 const settle=driver.timers.find(timer=>!timer.cleared && timer.ms===200);
 assert.ok(settle);
 settle.cleared=true;
 settle.callback();
 await pending;
 const retry=driver.timers.find(timer=>!timer.cleared);
 assert.equal(retry.ms,1000);
 retry.callback();
 await new Promise(setImmediate);
 assert.equal(driver.stats().posts,1);
 assert.equal(driver.stats().actions,1);
 assert.equal(driver.stats().boots,2);
 assert.equal(driver.api.polling,false);
});


test('repeated bootstrap errors use bounded reconnect backoff',async()=>{
 const driver=reconnectDriver(false,10);
 await driver.api.poll();
 for(const delay of [1000,2000,4000,5000,5000]) {
  const retry=driver.timers.find(timer=>!timer.cleared);
  assert.equal(retry.ms,delay);
  retry.callback();
  await new Promise(setImmediate);
  assert.equal(driver.api.polling,false);
 }
});

function windowDriver(available, delay=0, page='') {
 let now=0, attempts=0, creations=0, createdURL, reads=0;
 const ownedTab=runInNewContext(source+'\nbridge='+JSON.stringify({page})+';ownedTab',{
  chrome:{
   storage:{session:{get:async()=>({}),set:async()=>{}}},
   windows:{getAll:async()=>{now+=delay;return ++attempts>=available?[{id:7,focused:true}]:[];}},
   tabs:{create:async(options)=>{creations++;createdURL=options.url;assert.equal(options.windowId,7);return {id:8};},get:async()=>{reads++;return {status:'complete'};},sendMessage:async()=>({})},
  },
  Date:{now:()=>now},setTimeout(callback,ms){now+=ms;callback();},
 });
 return {ownedTab,stats:()=>({attempts,creations,createdURL,reads})};
}

test('startup waits for an owned browser window and creates the tab exactly once',async()=>{
 const driver=windowDriver(3);
 assert.equal((await driver.ownedTab('run','epoch',1000)).id,8);
 assert.equal(driver.stats().creations,1);
 assert.equal(driver.stats().attempts,3);
 const missing=windowDriver(Infinity);
 await assert.rejects(missing.ownedTab('run','epoch',200),/no tab was opened/);
 assert.equal(missing.stats().creations,0);
 const expired=windowDriver(1,1000);
 await assert.rejects(expired.ownedTab('run','epoch',200),/Expired browser command/);
 assert.equal(expired.stats().creations,0);
});

test('explicit navigation skips restoring the previous page first',async()=>{
 const open=windowDriver(1,0,'http://localhost/previous');
 await open.ownedTab('run','epoch',1000,false);
 assert.equal(open.stats().createdURL,'about:blank');
 assert.equal(open.stats().reads,0);
 const restore=windowDriver(1,0,'http://localhost/previous');
 await restore.ownedTab('run','epoch',1000);
 assert.equal(restore.stats().createdURL,'http://localhost/previous');
 assert.equal(restore.stats().reads,1);
});
