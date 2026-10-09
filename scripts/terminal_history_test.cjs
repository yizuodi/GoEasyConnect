const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const test = require('node:test');
const context=vm.createContext({AbortController});
vm.runInContext(fs.readFileSync(require.resolve('../web/terminal-history.js'),'utf8'),context);
const Loader=vm.runInContext('TerminalHistoryLoader',context);
const response=page=>({ok:true,json:async()=>page});

test('terminal preview requests a bounded recent page and strips stale ANSI commands',async()=>{
  const calls=[],writes=[];
  const loader=new Loader(async path=>{
    calls.push(path);return response({chunks:[{role:'assistant',content:'\x1b[31mrecent\x1b[0m\x1b]0;title\x07'}],has_older:true});
  });
  await loader.load('session',{write:text=>writes.push(text)},()=>true);
  assert.equal(calls[0],'/api/sessions/session/terminal/history');
  assert.equal(writes[0],'recent');
  assert.match(writes[1],/终端历史/);
});

test('terminal preview aborts old requests and ignores late responses after switching sessions',async()=>{
  let finish;
  const writes=[];
  let signal;
  const waiting=new Promise(resolve=>{finish=resolve;});
  const loader=new Loader(async(path,options)=>{signal=options.signal;return waiting;});
  const pending=loader.load('old',{write:text=>writes.push(text)},()=>true);
  loader.close();assert.equal(signal.aborted,true);
  finish(response({chunks:[{role:'assistant',content:'old output'}]}));
  await pending;
  assert.equal(writes.length,0);
});

test('terminal preview ignores stale terminal instances even when fetch succeeds',async()=>{
  const writes=[];
  const loader=new Loader(async()=>response({chunks:[{role:'assistant',content:'wrong session'}]}));
  await loader.load('old',{write:text=>writes.push(text)},()=>false);
  assert.equal(writes.length,0);
});

test('terminal history drops incomplete trailing escapes and leaves HTML as plain text',()=>{
  assert.equal(context.terminalHistoryText('safe\x1b[31'),'safe');
  assert.equal(context.terminalHistoryText('safe\x1bPignore\x1b\\end'),'safeend');
  assert.equal(context.terminalHistoryText('<script>not markup</script>'),'<script>not markup</script>');
});

test('desktop and mobile do not load the full messages endpoint for terminal history',()=>{
  const desktop=fs.readFileSync(require.resolve('../web/app.js'),'utf8');
  const mobile=fs.readFileSync(require.resolve('../web/mobile.html'),'utf8');
  assert.ok(!desktop.includes('api(`/api/sessions/${currentSessionId}/messages`)'));
  assert.ok(!mobile.includes("api('/api/sessions/'+id+'/messages')"));
  assert.match(desktop,/if \(!s\.terminal_running\)/);
  assert.match(mobile,/if\(!s\.terminal_running\)await loadHistory/);
});
