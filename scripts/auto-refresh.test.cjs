const {test} = require('node:test');
const assert = require('node:assert/strict');
const {ConsolePoller, balanceCheckedAt} = require('../web/auto-refresh.js');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const until = async predicate => {
  for (let i = 0; i < 100; i++) { if (predicate()) return; await delay(5); }
  throw new Error('poller did not reach expected state');
};

test('cached balance timestamps do not postpone the next upstream refresh', () => {
  const now=Date.UTC(2026,10,6,12),old=now-59000;
  const report={accounts:[{checked_at:new Date(old).toISOString(),cached:true},{checked_at:new Date(now-1000).toISOString(),cached:true}]};
  assert.equal(balanceCheckedAt(report,now),old);
  assert.equal(balanceCheckedAt({accounts:[{checked_at:'invalid'},{checked_at:'0001-01-01T00:00:00Z'}]},now),now);
});

test('disabled pages skip network; waking a visible session refreshes', async t => {
  let enabled = false, calls = 0;
  const poller = new ConsolePoller({enabled: () => enabled, run: async () => calls++, interval: 10});
  t.after(() => poller.stop());poller.start();await delay(30);assert.equal(calls,0);
  enabled = true;poller.wake();await until(() => calls > 0);
  poller.stop();const stopped = calls;await delay(30);assert.equal(calls,stopped);
});
test('slow polls and repeated wake calls never overlap', async t => {
  let active = 0, maxActive = 0, calls = 0, release;
  const poller = new ConsolePoller({enabled: () => true, interval: 10, run: async () => {
    calls++;active++;maxActive = Math.max(maxActive,active);
    await new Promise(resolve => { release = resolve; });active--;
  }});
  t.after(() => {poller.stop();release?.();});poller.start();await until(() => calls === 1);
  for(let i=0;i<5;i++)poller.wake();await delay(35);assert.equal(calls,1);assert.equal(maxActive,1);
  release();await until(() => calls === 2);assert.equal(maxActive,1);
});
test('logout/hidden stop cancels an in-flight request without failure notice', async t => {
  let signal, errors = 0;
  const poller = new ConsolePoller({enabled: () => true, interval: 10,onError: () => errors++,run: async s => {
    signal=s;await new Promise((resolve,reject) => s.addEventListener('abort',()=>reject(new Error('cancelled')),{once:true}));
  }});
  t.after(() => poller.stop());poller.start();await until(() => !!signal);poller.stop();await delay(25);
  assert.equal(signal.aborted,true);assert.equal(errors,0);assert.equal(poller.running,false);assert.equal(poller.timer,null);
});
test('network failures retry instead of stopping the loop', async t => {
  let calls=0,errors=0;
  const poller = new ConsolePoller({enabled:()=>true,interval:10,onError:()=>errors++,run:async()=>{calls++;if(calls===1)throw new Error('offline');}});
  t.after(()=>poller.stop());poller.start();await until(()=>calls>=2);assert.equal(errors,1);
});
test('a stalled request times out and reports an update failure', async t => {
  let message='';
  const poller=new ConsolePoller({enabled:()=>true,timeout:15,interval:1000,onError:e=>{message=e.message;},run:async signal=>new Promise((resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('aborted')),{once:true}))});
  t.after(()=>poller.stop());poller.start();await until(()=>!!message);assert.equal(message,'自动更新超时');
});
