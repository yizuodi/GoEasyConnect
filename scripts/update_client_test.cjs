const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const test = require('node:test');

async function monitor(replies, previousUpdateAt = 0) {
  const alerts=[];
  let reloads=0;
  let requests=0;
  const context=vm.createContext({
    Date, AbortSignal, setTimeout:callback=>callback(), alert:message=>alerts.push(message),
    location:{reload:()=>reloads++}
  });
  vm.runInContext(fs.readFileSync(require.resolve('../web/update-client.js'),'utf8'),context);
  const request=async()=>{
    requests++;
    assert.ok(requests<=replies.length,'monitor did not terminate');
    const value=replies[requests-1];
    if(value instanceof Error)throw value;
    return {ok:true,status:200,json:async()=>value};
  };
  await context.monitorEasyConnectUpdate(request,'v0.5.2',{textContent:''},previousUpdateAt);
  return {alerts,reloads,requests};
}

test('update monitor reports failure instead of success',async()=>{
  const result=await monitor([{state:'failed',phase:'download',updatedAt:1000}]);
  assert.match(result.alerts[0],/更新失败/);
  assert.equal(result.reloads,0);
});
test('update monitor tolerates restart disconnect and verifies the running version',async()=>{
  const result=await monitor([new Error('network'),{state:'succeeded',currentVersion:'v0.5.1',updatedAt:1000},
    {state:'succeeded',currentVersion:'v0.5.2',updatedAt:1000}]);
  assert.equal(result.reloads,1);
  assert.match(result.alerts[0],/已成功更新至 v0.5.2/);
});
test('update monitor ignores a previous attempt result',async()=>{
  const result=await monitor([{state:'failed',updatedAt:1},{state:'succeeded',currentVersion:'v0.5.2',updatedAt:2}],1);
  assert.equal(result.requests,2);
  assert.equal(result.reloads,1);
});
