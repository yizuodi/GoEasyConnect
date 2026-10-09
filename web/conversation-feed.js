class ConversationFeed {
  constructor(request, onUpdate, onState) {
    this.request = request;
    this.onUpdate = onUpdate;
    this.onState = onState;
    this.messages = new Map();
    this.events = new Map();
    this.messageCursor = 0;
    this.eventCursor = 0;
    this.before = 0;
    this.hasOlder = false;
    this.initialized = false;
    this.pending = null;
    this.closed = false;
    this.loadingOlder = false;
    this.toolPages = [];
    this.historyMode = false;
    this.toolHistoryMode = false;
    this.unseen = 0;
    this.unseenIDs = new Set();
    this.running = false;
    this.failures = 0;
    this.controller = new AbortController();
  }

  close() { this.closed = true; this.controller.abort(); }

  async exclusive(operation) {
    while (this.pending) await this.pending;
    if (this.closed) return;
    const task = operation();
    this.pending = task;
    try { return await task; }
    finally { if (this.pending === task) this.pending = null; }
  }

  async poll() {
    if (this.closed) return;
    if (this.pending) return this.pending;
    try {
      const result = await this.exclusive(() => this.fetchPage(false));
      this.failures = 0;
      return result;
    } catch (error) { this.failures++; throw error; }
  }

  async older() {
    if (!this.hasOlder || this.closed || this.loadingOlder) return;
    this.loadingOlder = true;
    try {
      return await this.exclusive(() => this.fetchPage(true));
    } finally { this.loadingOlder = false; }
  }

  async olderTools() {
    if (this.closed || this.loadingOlder || !this.toolPages.length) return;
    this.loadingOlder = true;
    try {
      return await this.exclusive(() => this.fetchPage(true, this.toolPages[0]));
    } finally { this.loadingOlder = false; }
  }

  async latest() {
    return this.exclusive(async () => {
      await this.fetchPage(false, null, true);
    });
  }

  delay(hasMore, hidden) {
    if (hidden) return 30000;
    if (this.failures) return Math.min(30000, 1000 * 2 ** this.failures);
    return hasMore ? 100 : this.running ? 1000 : 5000;
  }

  async fetchPage(older, toolPage = null, latest = false) {
    if (this.closed) return;
    const pageBefore = toolPage ? toolPage.before : older ? this.before : 0;
    const query = latest ? '' : toolPage ? `?before=${pageBefore}&event_before=${toolPage.eventBefore}` : older ? `?before=${this.before}` : this.initialized
      ? `?message_after=${this.messageCursor}&event_after=${this.eventCursor}` : '';
    const response = await this.request(`/conversation/feed${query}`, { signal: this.controller.signal });
    if (!response.ok) throw new Error('加载对话失败');
    const data = await response.json();
    if (this.closed) return;
    if (latest) {
      this.messages.clear();
      this.events.clear();
      this.toolPages = [];
      this.initialized = false;
      this.historyMode = false;
      this.toolHistoryMode = false;
      this.unseen = 0;
      this.unseenIDs.clear();
    }
    if (older && !toolPage) this.historyMode = true;
    if (toolPage) this.toolHistoryMode = true;
    if (toolPage) this.toolPages.shift();
    for (const id of data.deleted || []) this.messages.delete(id);
    let changed = (data.deleted || []).length > 0;
    for (const message of data.messages || []) {
      const previous = this.messages.get(message.id);
      if (!older && !latest && this.historyMode && !previous) {
        if (!this.unseenIDs.has(message.id)) {
          if (this.unseenIDs.size < 1000) this.unseenIDs.add(message.id);
          this.unseen = Math.min(1000, this.unseen + 1);
        }
        changed = true;
        continue;
      }
      if (previous && previous.revision > message.revision) continue;
      if (!previous || previous.revision !== message.revision || previous.content !== message.content || previous.truncated !== message.truncated) changed = true;
      this.messages.set(message.id, message);
    }
    for (const event of data.events || []) {
      if (!(event.type.startsWith('tool.') || event.type === 'file.change' || event.type === 'turn.failed')) continue;
      const key = event.item_id ? `${event.turn_id}:${event.item_id}` : event.id;
      if (this.historyMode && !this.messages.has(event.payload?.assistant_message_id)) continue;
      const previous = this.events.get(key);
      if (this.toolHistoryMode && !older && !latest && !previous) continue;
      if (!previous || previous.seq < event.seq) { this.events.set(key, event); changed = true; }
    }
    if (!toolPage && (older || !this.initialized)) {
      this.before = data.before;
      this.hasOlder = data.has_older;
    }
    if (data.has_older_tools) {
      const messages = data.messages || [];
      const windowBefore = messages.length ? messages[messages.length - 1].position + 1 : pageBefore;
      this.toolPages.push({ before: windowBefore, first: messages[0]?.position || 0, eventBefore: data.tool_before });
    }
    if (!older) {
      this.messageCursor = data.message_cursor;
      this.eventCursor = data.event_cursor;
      this.initialized = true;
      this.running = Boolean(data.running);
      this.onState(data.running, data.session_running);
    }
    const ordered = [...this.messages.values()].sort((left, right) => left.position - right.position);
    if (ordered.length > 200) {
      const removed = this.historyMode ? ordered.slice(200) : ordered.slice(0, ordered.length - 200);
      for (const message of removed) this.messages.delete(message.id);
      const retained = [...this.messages.values()].sort((left, right) => left.position - right.position);
      this.before = retained[0].position;
      if (!this.historyMode) this.hasOlder = true;
      changed = true;
    }
    if (this.messages.size) {
      const retained = [...this.messages.values()].sort((left, right) => left.position - right.position);
      this.toolPages = this.toolPages.filter(page => page.before > retained[0].position && (!page.first || page.first <= retained[retained.length - 1].position));
    }
    if (older || latest || ordered.length > 200) {
      for (const [key, event] of this.events) {
        if (!this.messages.has(event.payload?.assistant_message_id)) this.events.delete(key);
      }
    }
    if (this.events.size > 400) {
      const orderedEvents = [...this.events.entries()].sort((left, right) => left[1].seq - right[1].seq);
      const removed = this.historyMode || this.toolHistoryMode ? orderedEvents.slice(400) : orderedEvents.slice(0, orderedEvents.length - 400);
      if (!this.historyMode && !this.toolHistoryMode) {
        const messages = [...this.messages.values()].sort((left, right) => left.position - right.position);
        // Each recovery window must match the server's 50-message page size.
        for (let index = 0; index < messages.length; index += 50) {
          const window = messages.slice(index, index + 50);
          const ids = new Set(window.map(message => message.id));
          const evicted = removed.filter(([, event]) => ids.has(event.payload?.assistant_message_id));
          if (!evicted.length) continue;
          const before = window[window.length - 1].position + 1;
          const eventBefore = Math.max(...evicted.map(([, event]) => event.seq)) + 1;
          const page = this.toolPages.find(page => page.before === before);
          if (page) page.eventBefore = Math.max(page.eventBefore, eventBefore);
          else this.toolPages.push({ before, first: window[0].position, eventBefore });
        }
      }
      for (const [key] of removed) this.events.delete(key);
    }
    if (changed || older || latest || !this.rendered) {
      this.onUpdate([...this.messages.values()].sort((left, right) => left.position - right.position),
        [...this.events.values()].sort((left, right) => left.seq - right.seq), older, latest);
      this.rendered = true;
    }
    return data.has_more;
  }

  async detail(kind, id) {
    const response = await this.request(`/conversation/${kind}/${encodeURIComponent(id)}`, { signal: this.controller.signal });
    if (!response.ok) throw new Error('加载详情失败');
    return response.json();
  }
}

function renderFeedMessages(container, messages, feed, className) {
  const existing = new Map([...container.querySelectorAll('[data-message-id]')].map(node => [node.dataset.messageId, node]));
  const ids = new Set(messages.map(message => message.id));
  for (const [id, node] of existing) if (!ids.has(id)) node.remove();
  container.querySelectorAll('.conversation-empty,.conv-empty').forEach(node => node.remove());
  let previous = null;
  for (const message of messages) {
    let element = existing.get(message.id);
    if (!element) {
      element = document.createElement('div');
      element.className = `${className} ${message.role === 'user' ? 'user' : 'assistant'}`;
      element.dataset.messageId = message.id;
      const text = document.createElement('div');
      text.className = 'message-text';
      element.append(text);
    }
    const text = element.querySelector('.message-text');
    if (element.dataset.preview !== message.content || element.dataset.revision !== String(message.revision)) {
      text.textContent = !message.content && message.role === 'assistant' ? '正在思考…' : message.content;
      element.dataset.preview = message.content;
      element.dataset.revision = String(message.revision);
      delete element.dataset.full;
    }
    let button = element.querySelector('.message-expand');
    if (message.truncated && !button && !element.dataset.full) {
      button = document.createElement('button');
      button.className = 'message-expand';
      button.textContent = '展开完整消息';
      button.addEventListener('click', async () => {
        button.disabled = true;
        try {
          const revision = element.dataset.revision;
          const data = await feed.detail('messages', message.id);
          if (feed.closed) return;
          if (revision !== element.dataset.revision) { button.disabled = false; return; }
          text.textContent = data.content;
          element.dataset.full = 'true';
          button.remove();
        } catch (error) { button.disabled = false; button.textContent = error.message + '，重试'; }
      });
      element.append(button);
    } else if (!message.truncated && button) button.remove();
    if (!element.isConnected) {
      if (previous) previous.after(element);
      else container.prepend(element);
    }
    previous = element;
  }
  let older = container.querySelector('.history-older:not(.history-tools):not(.history-latest)');
  if (!older) {
    older = document.createElement('button');
    older.className = 'history-older';
    older.textContent = '加载更早消息';
    older.addEventListener('click', async () => {
      older.disabled = true;
      try { await feed.older(); }
      catch (error) { older.textContent = error.message + '，重试'; }
      finally { older.disabled = false; }
    });
  }
  older.hidden = !feed.hasOlder;
  container.prepend(older);
  let tools = container.querySelector('.history-tools');
  if (!tools) {
    tools = document.createElement('button');
    tools.className = 'history-older history-tools';
    tools.textContent = '加载更多工具记录';
    tools.addEventListener('click', async () => {
      tools.disabled = true;
      try { await feed.olderTools(); }
      catch (error) { tools.textContent = error.message + '，重试'; }
      finally { tools.disabled = false; }
    });
  }
  tools.hidden = !feed.toolPages.length;
  older.after(tools);
  let latest = container.querySelector('.history-latest');
  if (!latest) {
    latest = document.createElement('button');
    latest.className = 'history-older history-latest';
    latest.addEventListener('click', async () => {
      latest.disabled = true;
      try { await feed.latest(); container.scrollTop = container.scrollHeight; }
      catch (error) { latest.textContent = error.message + '，重试'; }
      finally { latest.disabled = false; }
    });
  }
  latest.hidden = !feed.historyMode && !feed.toolHistoryMode;
  latest.textContent = feed.unseen ? `回到最新（${feed.unseen} 条新消息）` : '回到最新';
  tools.after(latest);
}

function captureFeedScroll(container) {
  const top = container.getBoundingClientRect().top;
  const node = [...container.querySelectorAll('[data-message-id],[data-event-key]')]
    .find(element => element.getBoundingClientRect().bottom > top);
  return { node, offset: node ? node.getBoundingClientRect().top - top : 0, scrollTop: container.scrollTop };
}

function restoreFeedScroll(container, anchor) {
  if (anchor.node && container.contains(anchor.node)) {
    container.scrollTop += anchor.node.getBoundingClientRect().top - container.getBoundingClientRect().top - anchor.offset;
  } else {
    container.scrollTop = anchor.scrollTop;
  }
}

function bindFeedDetail(detail, event, feed, body) {
  detail.addEventListener('toggle', async () => {
    if (!detail.open || detail.dataset.loaded || !event.payload?.details_available) return;
    detail.dataset.loaded = 'pending';
    const output = detail.querySelector('pre');
    output.textContent = '加载详情…';
    try {
      const data = await feed.detail('events', event.seq);
      if (feed.closed) return;
      output.textContent = body(data.payload || {});
      detail.dataset.loaded = 'true';
    } catch (error) { delete detail.dataset.loaded; output.textContent = error.message + '；重新展开以重试'; }
  });
}

function renderFeedEvents(container, events, feed, className, body) {
  const messages = new Map([...container.querySelectorAll('[data-message-id]')].map(node => [node.dataset.messageId, node]));
  const existing = new Map([...container.querySelectorAll('[data-event-key]')].map(node => [node.dataset.eventKey, node]));
  const retained = new Set();
  for (const event of events) {
    const anchor = messages.get(event.payload?.assistant_message_id);
    if (!anchor) continue;
    const key = event.item_id ? `${event.turn_id}:${event.item_id}` : event.id;
    retained.add(key);
    let element = existing.get(key);
    if (!element || element.dataset.seq !== String(event.seq)) {
      if (element) element.remove();
      if (event.type === 'turn.failed') {
        element = document.createElement('div');
        element.className = className === 'conv-tool' ? 'conv-error' : 'conversation-error';
        element.textContent = event.payload?.message || '执行失败';
      } else {
        element = document.createElement('details');
        element.className = className;
        const summary = document.createElement('summary');
        summary.textContent = event.payload?.command || event.payload?.kind || '文件变更';
        const output = document.createElement('pre');
        output.textContent = body(event.payload || {});
        element.append(summary, output);
        bindFeedDetail(element, event, feed, body);
      }
      element.dataset.eventKey = key;
      element.dataset.seq = String(event.seq);
      container.insertBefore(element, anchor);
    }
    if (element.nextSibling !== anchor) container.insertBefore(element, anchor);
  }
  for (const [key, node] of existing) if (!retained.has(key)) node.remove();
}
