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


// fakeTabs models the bot window's tabs and session storage.
function fakeTabs(initial=[{id:1,windowId:2,url:'about:blank',active:true,status:'complete',title:'Fixture'}]) {
 const tabs=new Map(initial.map(tab=>[tab.id,{...tab}]));
 let next=100, store={};
 const removed=[], created=[];
 const chrome={
  storage:{session:{get:async keys=>Object.fromEntries(keys.filter(key=>key in store).map(key=>[key,structuredClone(store[key])])),set:async values=>{store={...store,...structuredClone(values)};}}},
  windows:{getAll:async()=>[{id:2,focused:true}]},
  tabs:{
   get:async id=>{if(!tabs.has(id))throw new Error('No tab with id');return {...tabs.get(id)};},
   query:async()=>[...tabs.values()].map(tab=>({...tab})),
   create:async options=>{const tab={id:next++,windowId:options.windowId,url:options.url,active:!!options.active,status:'complete',openerTabId:options.openerTabId};tabs.set(tab.id,tab);created.push(tab.id);return {...tab};},
   remove:async id=>{if(!tabs.delete(id))throw new Error('No tab with id');removed.push(id);},
   update:async(id,props)=>{const tab=tabs.get(id);if(props.active)for(const other of tabs.values())if(other.windowId===tab.windowId)other.active=false;Object.assign(tab,props);return {...tab};},
   sendMessage:async()=>({}),
   captureVisibleTab:async()=> 'data:image/png;base64,fixture',
  },
 };
 return {chrome,tabs,removed,created,store:()=>store};
}

function screenshotDriver(capture, active=true) {
 const timers=[];
 const fake=fakeTabs([{id:1,windowId:2,url:'http://localhost/a',active,status:'complete',title:'Fixture'},{id:3,windowId:2,url:'http://localhost/b',active:!active,status:'complete'}]);
 fake.chrome.tabs.captureVisibleTab=capture;
 const execute=runInNewContext(source+'\nbridge={page:""};execute',{
  chrome:fake.chrome,structuredClone,
  setTimeout(callback,ms){const timer={callback,ms,cleared:false};timers.push(timer);return timer;},
  clearTimeout(timer){if(timer)timer.cleared=true;},
 });
 const command={session:'run',epoch:'epoch',args:['screenshot','png'],expires:Date.now()+2000};
 return {execute,command,timers,fake};
}

test('screenshot brings the run tab forward, captures it and clears its timer',async()=>{
 const driver=screenshotDriver(async(windowId,options)=>{
  assert.equal(windowId,2);
  assert.equal(options.format,'png');
  return 'data:image/png;base64,fixture';
 });
 assert.equal((await driver.execute(driver.command)).image,'data:image/png;base64,fixture');
 assert.equal(driver.timers[0].cleared,true);
 const background=screenshotDriver(async()=> 'data:image/png;base64,background',false);
 assert.equal((await background.execute(background.command)).image,'data:image/png;base64,background');
 const tab=[...background.fake.tabs.values()].find(tab=>tab.active);
 assert.equal(background.fake.store().claims.run,tab.id);
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

function reconnectChrome(onAction) {
 const fake=fakeTabs();
 fake.chrome.runtime={getURL:()=> 'chrome-extension://fixture/bridge.json'};
 fake.chrome.tabs.sendMessage=async(id,message)=>{if(message.args[0]==='ready')return ready;if(message.args[0]!=='reset')onAction();return {success:true,data:{}};};
 return fake.chrome;
}

function reconnectDriver(postFails=false, failures=1) {
 let now=0, boots=0, commands=0, posts=0, actions=0;
 const timers=[];
 const api=runInNewContext(source+'\n({poll, get polling(){return polling;}})',{
  chrome:reconnectChrome(()=>actions++),AbortController,structuredClone,
  Date:{now:()=>now},
  setTimeout(callback,ms){const timer={callback,ms,cleared:false};timers.push(timer);return timer;},
  clearTimeout(timer){if(timer)timer.cleared=true;},
  fetch:async(url,options)=>{
   if(url.startsWith('chrome-extension:')) {
    if(++boots<=failures && !postFails)throw new Error('transient startup failure');
    return {json:async()=>({url:'http://localhost/bridge',token:'private'})};
   }
   if(options.method==='POST') {posts++;throw new Error('lost acknowledgement');}
   if(postFails && commands++===0)return {ok:true,status:200,json:async()=>({id:'one',session:'run',epoch:'epoch',args:['click','@e1'],expires:1e12})};
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

test('lost command acknowledgement never replays the command',async()=>{
 const driver=reconnectDriver(true);
 const pending=driver.api.poll();
 for(let i=0;i<20 && !driver.timers.some(timer=>!timer.cleared && timer.ms===200);i++)await new Promise(setImmediate);
 const settle=driver.timers.find(timer=>!timer.cleared && timer.ms===200);
 assert.ok(settle);
 settle.cleared=true;
 settle.callback();
 await pending;
 for(let i=0;i<20;i++)await new Promise(setImmediate);
 assert.equal(driver.stats().posts,1);
 assert.equal(driver.stats().actions,1);
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

function claimDriver({windows=1, delay=0, page='', tabs}={}) {
 let now=0, attempts=0;
 const fake=fakeTabs(tabs ?? []);
 fake.chrome.windows.getAll=async()=>{now+=delay;return ++attempts>=windows?[{id:2,focused:true}]:[];};
 const api=runInNewContext(source+'\nbridge='+JSON.stringify({page})+';({claimTab,release})',{
  chrome:fake.chrome,structuredClone,Date:{now:()=>now},setTimeout(callback,ms){now+=ms;callback();},
 });
 return {...api,fake,attempts:()=>attempts};
}

test('startup waits for the bot window and opens one tab',async()=>{
 const driver=claimDriver({windows:3});
 assert.equal((await driver.claimTab('run','epoch',1000)).id,100);
 assert.equal(driver.fake.created.length,1);
 assert.equal(driver.attempts(),3);
 const missing=claimDriver({windows:Infinity});
 await assert.rejects(missing.claimTab('run','epoch',200),/no tab was opened/);
 assert.equal(missing.fake.created.length,0);
 const expired=claimDriver({delay:1000});
 await assert.rejects(expired.claimTab('run','epoch',200),/no tab was opened|Expired browser command/);
 assert.equal(expired.fake.created.length,0);
});

test('explicit navigation skips restoring the previous page first',async()=>{
 const open=claimDriver({page:'http://localhost/previous'});
 await open.claimTab('run','epoch',1000,false);
 assert.equal(open.fake.tabs.get(100).url,'about:blank');
 const restore=claimDriver({page:'http://localhost/previous'});
 await restore.claimTab('run','epoch',1000);
 assert.equal(restore.fake.tabs.get(100).url,'http://localhost/previous');
});

test('the first use after a browser start keeps one tab and closes the rest',async()=>{
 const driver=claimDriver({page:'http://localhost/kept',tabs:[
  {id:1,windowId:2,url:'about:blank',status:'complete'},
  {id:2,windowId:2,url:'http://localhost/kept',status:'complete'},
  {id:3,windowId:2,url:'http://localhost/stale',status:'complete'},
  {id:4,windowId:2,url:'chrome://newtab/',status:'complete'},
 ]});
 assert.equal((await driver.claimTab('run','epoch',1000)).id,2);
 assert.deepEqual([...driver.fake.tabs.keys()],[2]);
 // Later runs do not tidy again: a tab the user opened stays.
 driver.fake.tabs.set(9,{id:9,windowId:2,url:'http://localhost/user',status:'complete'});
 await driver.release('run');
 await driver.claimTab('next','epoch',1000);
 assert.ok(driver.fake.tabs.has(9));
});

test('runs at the same time get their own tabs; a later run reuses the main tab',async()=>{
 const driver=claimDriver({tabs:[{id:1,windowId:2,url:'http://localhost/main',status:'complete',active:true}]});
 const [a,b,c]=await Promise.all(['w1','w2','w3'].map(session=>driver.claimTab(session,'epoch',1000)));
 assert.equal(new Set([a.id,b.id,c.id]).size,3);
 assert.ok([a.id,b.id,c.id].includes(1));
 assert.equal((await driver.claimTab('w2','epoch',1000)).id,b.id);
 for(const tab of [a,b,c])if(tab.id!==1)assert.equal(driver.fake.tabs.get(tab.id).active,false);
 for(const session of ['w1','w2','w3'])await driver.release(session);
 assert.deepEqual([...driver.fake.tabs.keys()],[1]);
 assert.equal((await driver.claimTab('chat','epoch',1000)).id,1);
});

test('release closes the run tab and the tabs it opened, keeping the main tab',async()=>{
 const driver=claimDriver({tabs:[{id:1,windowId:2,url:'http://localhost/main',status:'complete'}]});
 const main=await driver.claimTab('chat','epoch',1000);
 const worker=await driver.claimTab('worker','epoch',1000);
 // Pages opened new tabs: from the worker (and one from that), and from the main tab.
 const state=driver.fake.store();
 for(const [id,opener] of [[50,worker.id],[51,50],[52,main.id]]) {
  driver.fake.tabs.set(id,{id,windowId:2,url:'http://localhost/popup',status:'complete'});
  state.openers[id]=opener;
 }
 await driver.fake.chrome.storage.session.set(state);
 await driver.release('worker');
 assert.deepEqual([...driver.fake.tabs.keys()].sort((x,y)=>x-y),[1,52]);
 await driver.release('chat');
 assert.deepEqual([...driver.fake.tabs.keys()],[1]);
});
