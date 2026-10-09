class TerminalHistoryLoader {
  constructor(request) { this.request = request; this.controller = null; }
  close() { if (this.controller) this.controller.abort(); this.controller = null; }
  async load(id, terminal, isCurrent) {
    this.close();
    const controller = new AbortController();
    this.controller = controller;
    try {
      const response = await this.request(`/api/sessions/${encodeURIComponent(id)}/terminal/history`, {signal:controller.signal});
      if (!response.ok) throw new Error('加载近期终端历史失败');
      const page = await response.json();
      if (controller.signal.aborted || !isCurrent()) return;
      let output = '';
      for (const chunk of page.chunks || []) {
        if (chunk.role === 'user') output += '\r\n> ' + chunk.content + '\r\n';
        else output += chunk.content;
      }
      // Persisted ANSI streams are not full screen snapshots. Render a safe,
      // bounded text preview rather than replaying stale TUI cursor movements.
      terminal.write(terminalHistoryText(output));
      if (page.has_older) terminal.write('\r\n\x1b[33m[仅显示近期终端输出；更早记录请点击“终端历史”]\x1b[0m\r\n');
    } catch (error) {
      if (!controller.signal.aborted && isCurrent()) terminal.write('\r\n[近期历史加载失败，请点击“终端历史”重试]\r\n');
    } finally { if (this.controller === controller) this.controller = null; }
  }
}

function terminalHistoryText(text) {
  // Remove CSI, OSC and other ANSI strings before placing history in textContent.
  return text.replace(/\x1b(?:\[[0-?]*[ -/]*[@-~]|\][\s\S]*?(?:\x07|\x1b\\)|[P^_][\s\S]*?\x1b\\|(?![\[\]P^_])[@-_])/g, '')
    .replace(/\x1b[\s\S]*$/, '')
    .replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, '');
}

function openTerminalHistory(request, id) {
  const overlay = document.createElement('div');
  overlay.className = 'terminal-history-overlay';
  const panel = document.createElement('section');
  panel.className = 'terminal-history-panel';
  panel.setAttribute('role','dialog'); panel.setAttribute('aria-modal','true'); panel.setAttribute('aria-label','终端历史');
  const heading = document.createElement('h3'); heading.textContent = '终端历史（输出流，非原生对话记录）';
  const close = document.createElement('button'); close.textContent = '关闭';
  const older = document.createElement('button'); older.textContent = '加载更早输出'; older.disabled = true;
  const latest = document.createElement('button'); latest.textContent = '回到最新';
  const info = document.createElement('div'); info.textContent = '每页最多 128 KiB；查看不会影响实时终端。';
  const content = document.createElement('pre'); content.tabIndex = 0;
  panel.append(heading, close, older, latest, info, content); overlay.append(panel); document.body.append(overlay);
  const previousFocus = document.activeElement;
  const controller = new AbortController();
  let cursor = null;
  let loading = false;
  let active = true;
  const dismiss = () => { active = false; controller.abort(); overlay.remove(); document.removeEventListener('keydown',keyHandler); previousFocus?.focus(); };
  const keyHandler = event => { if(event.key==='Escape') dismiss(); };
  close.onclick = dismiss;
  overlay.onclick = event => { if(event.target===overlay) dismiss(); };
  document.addEventListener('keydown',keyHandler);
  async function load(earlier) {
    if (loading || !active) return;
    loading = true; older.disabled = true; latest.disabled = true;
    try {
      const query = earlier && cursor ? `?before=${cursor.before}&end=${cursor.end}` : '';
      const response = await request(`/api/sessions/${encodeURIComponent(id)}/terminal/history${query}`,{signal:controller.signal});
      if(!response.ok) throw new Error('读取历史失败');
      const page = await response.json();
      if(!active) return;
      cursor=page;
      content.textContent=terminalHistoryText((page.chunks||[]).map(chunk=>(chunk.role==='user'?'\n> ':'')+chunk.content).join('')) || '此页没有可显示的文本输出。';
      content.scrollTop=earlier?0:content.scrollHeight;
      older.disabled=!page.has_older;
      info.textContent=page.has_older?'仅显示当前历史页；可继续读取更早输出。':'已到达最早记录。';
    } catch(error) { if(active) {info.textContent=error.message+'，请重试。';older.disabled=!cursor?.has_older;} }
    finally {loading=false;latest.disabled=false;}
  }
  older.onclick=()=>load(true);latest.onclick=()=>load(false);
  close.focus();load(false);
}
