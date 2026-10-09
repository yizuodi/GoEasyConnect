const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const test = require('node:test');

const context = vm.createContext({ Map, Set, AbortController });
vm.runInContext(fs.readFileSync(require.resolve('../web/conversation-feed.js'), 'utf8'), context);
const ConversationFeed = vm.runInContext('ConversationFeed', context);
const response = data => ({ ok: true, json: async () => data });
const snapshot = {
  messages: [{ id: 'latest', content: 'hello', position: 100, revision: 1 }],
  events: [], deleted: [], message_cursor: 10, event_cursor: 20, before: 100,
  has_older: true, running: false, session_running: true
};

test('first snapshot then delta only; pagination does not rewind live cursors', async () => {
  const paths = [];
  const renders = [];
  const responses = [snapshot,
    { ...snapshot, messages: [], before: 0, has_older: false },
    { ...snapshot, messages: [{ id: 'older', content: 'old', position: 50, revision: 0 }], before: 50, message_cursor: 99, event_cursor: 99 },
    { ...snapshot, messages: [{ ...snapshot.messages[0], content: 'updated', revision: 2 }], message_cursor: 11, event_cursor: 21 }
  ];
  const feed = new ConversationFeed(async path => { paths.push(path); return response(responses.shift()); },
    (messages, events, older) => renders.push({ messages, older }), () => {});
  await feed.poll();
  await feed.poll();
  assert.equal(renders.length, 1);
  assert.equal(feed.hasOlder, true);
  await feed.older();
  assert.equal(feed.messageCursor, 10);
  assert.equal(feed.eventCursor, 20);
  await feed.poll();
  assert.deepEqual(paths, ['/conversation/feed', '/conversation/feed?message_after=10&event_after=20',
    '/conversation/feed?before=100', '/conversation/feed?message_after=10&event_after=20']);
  assert.equal(renders.at(-1).messages.length, 2);
  assert.equal(renders.at(-1).messages[1].content, 'updated');
});

test('simultaneous polls share a request and closed feeds discard late responses', async () => {
  let resolve;
  let requests = 0;
  let renders = 0;
  let states = 0;
  const pending = new Promise(done => { resolve = done; });
  const feed = new ConversationFeed(async () => { requests++; return pending; }, () => { renders++; }, () => { states++; });
  const first = feed.poll();
  const second = feed.poll();
  assert.equal(requests, 1);
  feed.close();
  resolve(response(snapshot));
  await Promise.all([first, second]);
  assert.equal(renders, 0);
  assert.equal(states, 0);
});

test('lifecycle events merge by item and full details are fetched separately', async () => {
  let turn = 0;
  const requests = [];
  const feed = new ConversationFeed(async path => {
    requests.push(path);
    if (path.includes('/events/')) return response({ payload: { output: 'complete tool output' } });
    turn++;
    return response({ ...snapshot, messages: turn === 1 ? snapshot.messages : [],
      events: [{ id: 'event-'+turn, seq: turn, type: turn === 1 ? 'tool.started' : 'tool.completed', turn_id: 'turn', item_id: 'tool', payload: { assistant_message_id: 'latest' } }] });
  }, () => {}, () => {});
  await feed.poll();
  await feed.poll();
  assert.equal(feed.events.size, 1);
  assert.equal([...feed.events.values()][0].type, 'tool.completed');
  const data = await feed.detail('events', 2);
  assert.equal(data.payload.output, 'complete tool output');
  assert.equal(requests.at(-1), '/conversation/events/2');
});

test('tool pagination stays on its original message window without resetting cursors', async () => {
  const paths = [];
  let calls = 0;
  const feed = new ConversationFeed(async path => {
    paths.push(path);
    calls++;
    return response({ ...snapshot, has_older_tools: calls === 1, tool_before: 5 });
  }, () => {}, () => {});
  await feed.poll();
  assert.equal(feed.toolPages.length, 1);
  await feed.olderTools();
  assert.equal(paths[1], '/conversation/feed?before=101&event_before=5');
  assert.equal(feed.toolPages.length, 0);
  assert.equal(feed.messageCursor, 10);
  assert.equal(feed.eventCursor, 20);
});

test('live cache stays bounded and evicted messages remain accessible through history', async () => {
  let calls = 0;
  const feed = new ConversationFeed(async () => {
    calls++;
    const start = calls === 1 ? 1 : 51;
    const end = calls === 1 ? 50 : 351;
    return response({ ...snapshot, messages: Array.from({length: end-start+1}, (_, index) => ({
      id: 'message-'+(start+index), position: start+index, revision: 1, content: 'reply'
    })), has_older: false });
  }, () => {}, () => {});
  await feed.poll();
  await feed.poll();
  assert.equal(feed.messages.size, 200);
  assert.equal(feed.before, 152);
  assert.equal(feed.hasOlder, true);
});

test('historical window stays bounded, counts new messages once and returns to latest', async () => {
  let calls = 0;
  const feed = new ConversationFeed(async path => {
    calls++;
    if (path.includes('before=')) return response({ ...snapshot,
      messages: Array.from({length:250}, (_, index) => ({id:'old-'+index,position:index+1,revision:0,content:'old'})), has_older:false });
    if (path.includes('message_after=')) return response({ ...snapshot,
      messages:[{id:'new',position:300,revision:calls,content:'new reply'}] });
    return response(snapshot);
  }, () => {}, () => {});
  await feed.poll();
  await feed.older();
  assert.equal(feed.historyMode, true);
  assert.equal(feed.messages.size, 200);
  assert.equal(feed.hasOlder, false);
  await feed.poll();
  await feed.poll();
  assert.equal(feed.messages.has('new'), false);
  assert.equal(feed.unseen, 1);
  await feed.latest();
  assert.equal(feed.historyMode, false);
  assert.equal(feed.unseen, 0);
  assert.equal(feed.messages.size, 1);
});

test('history requests serialize with polling and close aborts fetch signals', async () => {
  let resolveOlder;
  let requests = 0;
  let signal;
  const waiting = new Promise(resolve => { resolveOlder = resolve; });
  const feed = new ConversationFeed(async (path, options) => {
    requests++;
    signal = options.signal;
    if (path.includes('before=')) return waiting;
    return response(snapshot);
  }, () => {}, () => {});
  await feed.poll();
  const older = feed.older();
  const poll = feed.poll();
  assert.equal(requests, 2);
  resolveOlder(response({ ...snapshot,messages:[] }));
  await Promise.all([older,poll]);
  assert.equal(requests, 2);
  assert.equal(signal.aborted, false);
  feed.close();
  assert.equal(signal.aborted, true);
});

test('idle, hidden and failed requests use slower polling', () => {
  const feed = new ConversationFeed(async () => response(snapshot), () => {}, () => {});
  assert.equal(feed.delay(false,false),5000);
  feed.running=true;
  assert.equal(feed.delay(false,false),1000);
  assert.equal(feed.delay(true,false),100);
  assert.equal(feed.delay(true,true),30000);
  feed.failures=10;
  assert.equal(feed.delay(false,false),30000);
});

test('tool history remains pageable beyond the cache limit and ignores unseen live tools', async () => {
  const tools = Array.from({length:950}, (_, seq) => ({
    id:'event-'+seq, seq:seq+1, type:'tool.completed', turn_id:'turn', item_id:'tool-'+seq,
    payload:{assistant_message_id:'latest'}
  }));
  const observed = new Set();
  let calls = 0;
  const feed = new ConversationFeed(async path => {
    calls++;
    const cursor = Number(new URLSearchParams(path.split('?')[1]).get('event_before'));
    if (cursor) {
      const matching = tools.filter(event => event.seq < cursor);
      return response({...snapshot,events:matching.slice(-100),has_older_tools:matching.length>100,
        tool_before:matching.slice(-100)[0]?.seq});
    }
    if (path.includes('message_after=')) {
      const start = Math.min(850, 550 + (calls - 2) * 100);
      return response({...snapshot,messages:[],events:tools.slice(start,start+100)});
    }
    return response({...snapshot,events:tools.slice(450,550),has_older_tools:true,tool_before:451});
  }, (messages,events) => events.forEach(event => observed.add(event.seq)), () => {});
  await feed.poll();
  for (let page=0;page<4;page++) await feed.poll();
  assert.equal(feed.events.size,400);
  assert.equal(feed.toolPages[0].eventBefore,551);
  await feed.olderTools();
  assert.equal(feed.toolHistoryMode,true);
  while (feed.toolPages.length) await feed.olderTools();
  assert.equal(observed.size,950);
  assert.equal(feed.events.size,400);
  assert.equal(Math.min(...[...feed.events.values()].map(event=>event.seq)),1);
  await feed.poll();
  assert.equal(feed.events.size,400);
  assert.equal(Math.min(...[...feed.events.values()].map(event=>event.seq)),1);
  await feed.latest();
  assert.equal(feed.toolHistoryMode,false);
  assert.equal(feed.events.size,100);
});

test('scroll anchor preserves the visible message when content above it changes', () => {
  const capture = vm.runInContext('captureFeedScroll',context);
  const restore = vm.runInContext('restoreFeedScroll',context);
  let messageTop = 90;
  const above = {getBoundingClientRect:()=>({top:40,bottom:90})};
  const visible = {getBoundingClientRect:()=>({top:messageTop,bottom:messageTop+80})};
  const container = {
    scrollTop:250,
    getBoundingClientRect:()=>({top:100}),
    querySelectorAll:()=>[above,visible],
    contains:node=>node===visible
  };
  const anchor = capture(container);
  assert.equal(anchor.node,visible);
  messageTop=190;
  restore(container,anchor);
  assert.equal(container.scrollTop,350);
  container.contains=()=>false;
  restore(container,anchor);
  assert.equal(container.scrollTop,250);
});
