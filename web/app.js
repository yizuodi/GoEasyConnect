// ============ State ============
let authToken = localStorage.getItem('ec_token') || '';
let currentTab = 'sessions';
let currentSessionId = null;
let sessions = [];
let profiles = [];
let ws = null;
let wsConnectGeneration = 0;
let term = null;
let fitAddon = null;
let appConfig = {};  // from /api/config
let appDefaults = {}; // from /api/defaults
let sidebarCollapsed = localStorage.getItem('ec_sidebar_collapsed') === 'true';
let terminalFitTimer = null;

// Polling state
let usePolling = localStorage.getItem('ec_polling') === 'true';
let pollSeq = 0;
let pollTimer = null;
let POLL_INTERVAL = 300; // will be overridden by server config

// ============ Init ============
// Login handled by inline script; app.js loads async
(function initApp() {
  if (window.__ecLoggedIn || (authToken && document.getElementById('loginOverlay').classList.contains('hidden'))) {
    showApp();
  }
})();

// ============ Auth ============
async function checkAuth() {
  try {
    const r = await api('/api/sessions');
    if (r.ok) showApp();
    else { localStorage.removeItem('ec_token'); authToken = ''; }
  } catch { localStorage.removeItem('ec_token'); authToken = ''; }
}

async function doLogin() {
  const pw = document.getElementById('loginPassword').value;
  if (!pw) return;
  authToken = pw;
  localStorage.setItem('ec_token', pw);
  try {
    const r = await api('/api/sessions');
    if (r.ok) {
      document.getElementById('loginError').style.display = 'none';
      showApp();
    } else {
      document.getElementById('loginError').style.display = 'block';
      localStorage.removeItem('ec_token'); authToken = '';
    }
  } catch {
    document.getElementById('loginError').style.display = 'block';
    localStorage.removeItem('ec_token'); authToken = '';
  }
}

function showApp() {
  document.getElementById('loginOverlay').classList.add('hidden');
  document.getElementById('appLayout').classList.remove('hidden');
  applySidebarState();
  updateFullscreenControl();
  // Load config and defaults from server
  api('/api/app-config').then(r => r.json()).then(c => {
    appConfig = c;
    POLL_INTERVAL = c.pollIntervalMs || 300;
    if (c.branding) {
      const b = c.branding;
      document.title = b.documentTitle || 'EasyConnect';
    }
  });
  api('/api/defaults').then(r => r.json()).then(d => {
    appDefaults = d;
  });
  updatePollToggle();
  loadSessions();
  loadProfiles();
  notifyAppReady();
}

// ============ View Controls ============
function fitTerminal() {
  try {
    if (fitAddon) fitAddon.fit();
  } catch {}
}

function scheduleTerminalFit(delay = 0) {
  if (terminalFitTimer) clearTimeout(terminalFitTimer);
  terminalFitTimer = setTimeout(() => {
    terminalFitTimer = null;
    fitTerminal();
  }, delay);
}

function applySidebarState() {
  const layout = document.getElementById('appLayout');
  const sidebar = document.getElementById('sidebar');
  const button = document.getElementById('sidebarToggle');
  if (!layout || !sidebar || !button) return;

  layout.classList.toggle('sidebar-collapsed', sidebarCollapsed);
  sidebar.setAttribute('aria-hidden', String(sidebarCollapsed));
  sidebar.inert = sidebarCollapsed;
  button.setAttribute('aria-expanded', String(!sidebarCollapsed));
  button.setAttribute('aria-label', sidebarCollapsed ? '展开侧边栏' : '收起侧边栏');
  button.title = sidebarCollapsed ? '展开侧边栏' : '收起侧边栏';
}

function toggleSidebar() {
  sidebarCollapsed = !sidebarCollapsed;
  localStorage.setItem('ec_sidebar_collapsed', String(sidebarCollapsed));
  applySidebarState();
  fitTerminal();
  scheduleTerminalFit(220);
}

function fullscreenElement() {
  return document.fullscreenElement || document.webkitFullscreenElement || null;
}

function updateFullscreenControl() {
  const button = document.getElementById('fullscreenToggle');
  if (!button) return;

  const active = Boolean(fullscreenElement());
  const supported = Boolean(
    document.documentElement.requestFullscreen ||
    document.documentElement.webkitRequestFullscreen
  );
  button.disabled = !supported;
  button.textContent = active ? '🗗' : '⛶';
  button.setAttribute('aria-pressed', String(active));
  button.setAttribute('aria-label', active ? '退出全屏' : '进入全屏');
  button.title = supported ? (active ? '退出全屏' : '进入全屏') : '当前浏览器不支持页面全屏';
}

async function toggleFullscreen() {
  try {
    if (fullscreenElement()) {
      const exit = document.exitFullscreen || document.webkitExitFullscreen;
      if (exit) await exit.call(document);
    } else {
      const root = document.documentElement;
      const request = root.requestFullscreen || root.webkitRequestFullscreen;
      if (request) await request.call(root);
    }
  } catch (error) {
    console.warn('切换全屏失败:', error);
  } finally {
    updateFullscreenControl();
    scheduleTerminalFit(100);
  }
}

window.addEventListener('resize', () => scheduleTerminalFit());
document.addEventListener('fullscreenchange', () => {
  updateFullscreenControl();
  scheduleTerminalFit(100);
});
document.addEventListener('webkitfullscreenchange', () => {
  updateFullscreenControl();
  scheduleTerminalFit(100);
});

function notifyAppReady() {
  if (typeof window.__easyconnectReady === 'function') window.__easyconnectReady();
  // Keep the legacy integration hook working after an in-place upgrade.
  if (typeof window.__easyclaudeReady === 'function') window.__easyclaudeReady();
}

// ============ API ============
async function api(path, options = {}) {
  const headers = { 'Authorization': `Bearer ${authToken}` };
  if (options.body) headers['Content-Type'] = 'application/json';
  return fetch(path, { ...options, headers });
}

// ============ Polling Toggle ============
function updatePollToggle() {
  const btn = document.getElementById('pollToggle');
  if (btn) {
    btn.textContent = usePolling ? '📡 轮询模式' : '🔌 WebSocket';
    btn.title = usePolling ? '当前: HTTP轮询 (点击切换WebSocket)' : '当前: WebSocket (点击切换HTTP轮询)';
  }
}

function togglePolling() {
  usePolling = !usePolling;
  localStorage.setItem('ec_polling', usePolling);
  updatePollToggle();

  // Reconnect current session with new mode
  if (currentSessionId) {
    const s = sessions.find(x => x.id === currentSessionId);
    if (s && s.isRunning) {
      disconnectAll();
      if (usePolling) startPolling(currentSessionId);
      else connectWS(currentSessionId);
    }
  }
}

function sendInput(data) {
  if (!currentSessionId) return;
  if (usePolling) {
    api(`/api/sessions/${currentSessionId}/input`, {
      method: 'POST',
      body: JSON.stringify({ data })
    });
  } else if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'input', sessionId: currentSessionId, data }));
  }
}

function sendResize(cols, rows) {
  if (!currentSessionId) return;
  if (usePolling) {
    api(`/api/sessions/${currentSessionId}/resize`, {
      method: 'POST',
      body: JSON.stringify({ cols, rows })
    });
  } else if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'resize', sessionId: currentSessionId, cols, rows }));
  }
}

// ============ Tabs ============
function switchTab(tab) {
  currentTab = tab;
  document.getElementById('tabSessions').classList.toggle('active', tab === 'sessions');
  document.getElementById('tabProfiles').classList.toggle('active', tab === 'profiles');
  const btn = document.getElementById('sidebarActionBtn');
  if (tab === 'sessions') { btn.textContent = '+ 新建会话'; renderSessions(); }
  else { btn.textContent = '+ 新建配置'; renderProfiles(); }
}

function onSidebarAction() {
  if (currentTab === 'sessions') showNewSessionModal();
  else showNewProfileModal();
}

// ============ Terminal (xterm.js) ============
function initTerminal() {
  if (term) { term.dispose(); term = null; }

  const container = document.getElementById('terminalContainer');
  container.innerHTML = '';

  term = new Terminal({
    theme: {
      background: '#0a0a14',
      foreground: '#c0c0c0',
      cursor: '#4fc3f7',
      selectionBackground: '#264f78',
      black: '#000000',
      red: '#ef5350',
      green: '#66bb6a',
      yellow: '#ffa726',
      blue: '#42a5f5',
      magenta: '#ab47bc',
      cyan: '#4fc3f7',
      white: '#e0e0e0',
      brightBlack: '#666666',
      brightRed: '#ef5350',
      brightGreen: '#66bb6a',
      brightYellow: '#ffa726',
      brightBlue: '#64b5f6',
      brightMagenta: '#ce93d8',
      brightCyan: '#80deea',
      brightWhite: '#ffffff'
    },
    fontFamily: 'Menlo, Monaco, "Courier New", monospace',
    fontSize: 13,
    lineHeight: 1.4,
    cursorBlink: true,
    scrollback: 10000,
    convertEol: false,
  });

  fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);
  term.open(container);

  fitTerminal();

  term.onData(data => sendInput(data));
  term.onResize(({ cols, rows }) => sendResize(cols, rows));

}

// ============ Sessions ============
async function loadSessions() {
  const r = await api('/api/sessions');
  if (r.ok) sessions = await r.json();
  if (currentTab === 'sessions') renderSessions();
}

function renderSessions() {
  const el = document.getElementById('sidebarContent');
  if (!sessions.length) {
    el.innerHTML = '<div style="padding:20px;color:var(--text-muted);text-align:center;">暂无会话</div>';
    return;
  }
  el.innerHTML = sessions.map(s => `
    <div class="session-item ${s.id === currentSessionId ? 'active' : ''}" data-session-id="${escAttr(s.id)}">
      <span class="session-name"><span class="agent-badge ${s.agent === 'codex' ? 'codex' : 'claude'}">${s.agent === 'codex' ? 'Codex' : 'Claude'}</span><span class="session-label">${esc(s.name)}</span></span>
      <div class="session-status ${s.isRunning ? 'running' : 'stopped'}" title="${s.isRunning ? '运行中' : '已停止'}"></div>
    </div>
  `).join('');
  el.querySelectorAll('.session-item').forEach(item => {
    item.addEventListener('click', () => selectSession(item.dataset.sessionId));
  });
}

async function selectSession(id) {
  currentSessionId = id;
  const s = sessions.find(x => x.id === id);
  if (!s) return;

  renderSessions();

  document.getElementById('emptyState').classList.add('hidden');
  document.getElementById('sessionView').classList.remove('hidden');
  document.getElementById('sessionView').style.display = 'flex';
  updateSessionTitle(s);

  updateSessionControls(s);

  initTerminal();

  // Load history messages into terminal
  const r = await api(`/api/sessions/${id}/messages`);
  let msgs = [];
  if (r.ok) msgs = await r.json();

  msgs.forEach(m => {
    if (m.role === 'user') {
      term.write('\x1b[1;36m> ' + m.content + '\x1b[0m\r\n');
    } else if (m.content) {
      term.write(m.content);
    }
  });

  // Connect with current mode
  disconnectAll();
  if (s.isRunning) {
    if (usePolling) startPolling(id);
    else connectWS(id);
  }

  setTimeout(() => { try { fitAddon.fit(); } catch {} }, 100);
}

function updateSessionControls(s) {
  const running = s.isRunning;
  document.getElementById('btnStart').classList.toggle('hidden', running);
  document.getElementById('btnStop').classList.toggle('hidden', !running);
  document.getElementById('sessionStatus').textContent = running ? '● 运行中' : '○ 已停止';
  document.getElementById('sessionStatus').style.color = running ? 'var(--success)' : 'var(--text-muted)';
  // Skip permissions toggle
  const label = document.getElementById('skipPermLabel');
  const check = document.getElementById('skipPermCheck');
  if (s) {
    check.checked = !!s.skip_permissions;
    label.classList.toggle('disabled', running);
    label.title = s.agent === 'codex'
      ? '启用 Codex 完全访问模式（仅停止时可切换）'
      : '跳过 Claude Code 权限确认（仅停止时可切换）';
  }
  // Profile selector (stopped only)
  const sel = document.getElementById('profileSelect');
  if (sel) {
    populateProfileSelect(sel, s.agent);
    sel.classList.toggle('hidden', running);
    sel.value = s.profile_id || '';
    updateSessionTitle(s);
  }
}

function agentProfiles(agent) {
  return profiles.filter(profile => (profile.agent || 'claude') === agent);
}

function populateProfileSelect(select, agent) {
  const label = agent === 'codex' ? 'Codex 默认配置' : 'Claude 默认配置';
  select.innerHTML = `<option value="">${label}</option>` + agentProfiles(agent).map(profile =>
    `<option value="${escAttr(profile.id)}">${esc(profile.name)}</option>`
  ).join('');
}

function updateSessionTitle(s) {
  const agentName = s.agent === 'codex' ? 'Codex' : 'Claude Code';
  document.getElementById('sessionTitle').textContent = `${s.name} · ${agentName}${s.profile_name ? ` (${s.profile_name})` : ''}`;
}

async function changeProfile() {
  if (!currentSessionId) return;
  const s = sessions.find(x => x.id === currentSessionId);
  if (!s || s.isRunning) return;
  const newProfileId = document.getElementById('profileSelect').value || null;
  const r = await api(`/api/sessions/${currentSessionId}/profile`, {
    method: 'PATCH',
    body: JSON.stringify({ profile_id: newProfileId })
  });
  if (r.ok) {
    const updated = await r.json();
    Object.assign(s, updated);
    updateSessionControls(s);
  }
}

async function toggleSkipPerm() {
  if (!currentSessionId) return;
  const s = sessions.find(x => x.id === currentSessionId);
  if (!s || s.isRunning) return;
  const enabled = document.getElementById('skipPermCheck').checked;
  await api(`/api/sessions/${currentSessionId}/skip-permissions`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled })
  });
  s.skip_permissions = enabled ? 1 : 0;
}

async function startSession() {
  if (!currentSessionId) return;
  const response = await api(`/api/sessions/${currentSessionId}/start`, { method: 'POST' });
  if (!response.ok) {
    const error = await response.json();
    return alert(error.error || '启动失败');
  }
  const s = sessions.find(x => x.id === currentSessionId);
  if (s) { s.isRunning = true; updateSessionControls(s); }
  initTerminal();
  if (usePolling) startPolling(currentSessionId);
  else connectWS(currentSessionId);
  renderSessions();
}

async function stopSession() {
  if (!currentSessionId) return;
  await api(`/api/sessions/${currentSessionId}/stop`, { method: 'POST' });
  disconnectAll();
  const s = sessions.find(x => x.id === currentSessionId);
  if (s) { s.isRunning = false; updateSessionControls(s); }
  renderSessions();
}

async function deleteSession() {
  if (!currentSessionId) return;
  if (!confirm('确定删除此会话？')) return;
  disconnectAll();
  await api(`/api/sessions/${currentSessionId}`, { method: 'DELETE' });
  currentSessionId = null;
  if (term) { term.dispose(); term = null; }
  document.getElementById('emptyState').classList.remove('hidden');
  document.getElementById('sessionView').classList.add('hidden');
  loadSessions();
}

function showNewSessionModal() {
  const defaultDir = appDefaults.workingDir || appConfig.defaultWorkingDir || '/home/user/code';
  showModal(`
    <h3>新建会话</h3>
    <div class="form-group">
      <label>会话名称</label>
      <input id="mName" placeholder="如：my-project">
    </div>
    <div class="form-group">
      <label>智能体</label>
      <select id="mAgent" onchange="updateNewSessionAgent()">
        <option value="claude">Claude Code</option>
        <option value="codex">Codex</option>
      </select>
    </div>
    <div class="form-group" id="mProfileGroup">
      <label>配置文件</label>
      <select id="mProfile"></select>
    </div>
    <div class="form-group">
      <label>工作目录</label>
      <input id="mWorkDir" placeholder="/home/user/code" value="${escAttr(defaultDir)}">
    </div>
    <div class="modal-actions">
      <button class="btn btn-ghost" onclick="closeModal()">取消</button>
      <button class="btn btn-primary" onclick="createSession()" style="width:auto">创建并启动</button>
    </div>
  `);
  document.getElementById('mAgent').value = appConfig.defaultAgent || 'claude';
  updateNewSessionAgent();
}

function updateNewSessionAgent() {
  const agent = document.getElementById('mAgent')?.value || 'claude';
  const select = document.getElementById('mProfile');
  if (select) populateProfileSelect(select, agent);
}

async function createSession() {
  const name = document.getElementById('mName').value.trim();
  if (!name) return alert('请输入会话名称');
  const profile_id = document.getElementById('mProfile').value || null;
  const agent = document.getElementById('mAgent').value;
  const working_dir = document.getElementById('mWorkDir').value.trim() || appDefaults.workingDir || '/home/user/code';

  const r = await api('/api/sessions', {
    method: 'POST',
    body: JSON.stringify({ name, agent, profile_id, working_dir })
  });

  if (r.ok) {
    closeModal();
    await loadSessions();
    const data = await r.json();
    currentSessionId = data.id;
    // Auto-start (skip_permissions from DB)
    const startResponse = await api(`/api/sessions/${data.id}/start`, { method: 'POST' });
    if (!startResponse.ok) {
      const error = await startResponse.json();
      alert(error.error || '会话已创建，但启动失败');
    }
    await loadSessions();
    const s = sessions.find(x => x.id === data.id);
    if (s) {
      document.getElementById('emptyState').classList.add('hidden');
      document.getElementById('sessionView').classList.remove('hidden');
      document.getElementById('sessionView').style.display = 'flex';
      updateSessionTitle(s);
      updateSessionControls(s);
      initTerminal();
      if (startResponse.ok) {
        if (usePolling) startPolling(data.id);
        else connectWS(data.id);
      }
    }
  } else {
    const err = await r.json();
    alert(err.error || '创建失败');
  }
}

// ============ Profiles ============
async function loadProfiles() {
  const r = await api('/api/profiles');
  if (r.ok) profiles = await r.json();
  if (currentTab === 'profiles') renderProfiles();
  // Update profile selector in topbar
  const sel = document.getElementById('profileSelect');
  const session = sessions.find(item => item.id === currentSessionId);
  if (sel && session) populateProfileSelect(sel, session.agent);
}

function renderProfiles() {
  const el = document.getElementById('sidebarContent');
  if (!profiles.length) {
    el.innerHTML = '<div style="padding:20px;color:var(--text-muted);text-align:center;">暂无配置</div>';
    return;
  }
  el.innerHTML = profiles.map(p => `
    <div class="profile-item">
      <div class="profile-name"><span class="agent-badge ${p.agent === 'codex' ? 'codex' : 'claude'}">${p.agent === 'codex' ? 'Codex' : 'Claude'}</span> ${esc(p.name)}</div>
      <div class="profile-desc">${esc(p.description || '无描述')}</div>
      <div class="profile-actions">
        <button class="btn btn-sm btn-ghost js-edit-profile" data-profile-id="${escAttr(p.id)}">编辑</button>
        <button class="btn btn-sm btn-danger js-delete-profile" data-profile-id="${escAttr(p.id)}">删除</button>
      </div>
    </div>
  `).join('');
  el.querySelectorAll('.js-edit-profile').forEach(button => {
    button.addEventListener('click', () => editProfile(button.dataset.profileId));
  });
  el.querySelectorAll('.js-delete-profile').forEach(button => {
    button.addEventListener('click', () => deleteProfile(button.dataset.profileId));
  });
}

function showNewProfileModal() {
  const claudeTemplate = appDefaults.profileContent || {
    "hasCompletedOnboarding": true,
    "env": {
      "ANTHROPIC_AUTH_TOKEN": "",
      "ANTHROPIC_BASE_URL": "",
      "ANTHROPIC_DEFAULT_OPUS_MODEL": "",
      "ANTHROPIC_DEFAULT_SONNET_MODEL": "",
      "ANTHROPIC_DEFAULT_HAIKU_MODEL": ""
    }
  };
  showModal(`
    <h3>新建配置文件</h3>
    <div class="form-group">
      <label>智能体</label>
      <select id="mPAgent" onchange="updateNewProfileAgent()">
        <option value="claude">Claude Code (JSON)</option>
        <option value="codex">Codex (TOML)</option>
      </select>
    </div>
    <div class="form-group">
      <label>名称</label>
      <input id="mPName" placeholder="如：glm-opus">
    </div>
    <div class="form-group">
      <label>描述</label>
      <input id="mPDesc" placeholder="可选">
    </div>
    <div class="form-group">
      <label id="mPContentLabel">配置内容 (JSON)</label>
      <textarea id="mPContent"></textarea>
    </div>
    <div class="modal-actions">
      <button class="btn btn-ghost" onclick="closeModal()">取消</button>
      <button class="btn btn-primary" onclick="createProfile()" style="width:auto">创建</button>
    </div>
  `);
  document.getElementById('mPAgent').dataset.claudeTemplate = JSON.stringify(claudeTemplate, null, 2);
  document.getElementById('mPAgent').dataset.codexTemplate = appDefaults.codexProfileContent || '# Custom provider; credentials are injected only into this session\napi_key = ""\nbase_url = ""\nmodel = ""\nwire_api = "responses"\n';
  updateNewProfileAgent();
}

function updateNewProfileAgent() {
  const select = document.getElementById('mPAgent');
  const agent = select.value;
  document.getElementById('mPContentLabel').textContent = `配置内容 (${agent === 'codex' ? 'TOML' : 'JSON'})`;
  document.getElementById('mPContent').value = agent === 'codex' ? select.dataset.codexTemplate : select.dataset.claudeTemplate;
}

async function createProfile() {
  const name = document.getElementById('mPName').value.trim();
  if (!name) return alert('请输入名称');
  const description = document.getElementById('mPDesc').value.trim();
  const content = document.getElementById('mPContent').value;
  const agent = document.getElementById('mPAgent').value;
  if (agent === 'claude') {
    try { JSON.parse(content); } catch { return alert('JSON 格式错误'); }
  }

  const r = await api('/api/profiles', {
    method: 'POST',
    body: JSON.stringify({ name, agent, description, content })
  });
  if (r.ok) { closeModal(); loadProfiles(); }
  else { const err = await r.json(); alert(err.error || '创建失败'); }
}

async function editProfile(id) {
  const p = profiles.find(x => x.id === id);
  if (!p) return;
  showModal(`
    <h3>编辑 ${p.agent === 'codex' ? 'Codex' : 'Claude'} 配置: ${esc(p.name)}</h3>
    <div class="form-group">
      <label>名称</label>
      <input id="mPName" value="${escAttr(p.name)}">
    </div>
    <div class="form-group">
      <label>描述</label>
      <input id="mPDesc" value="${escAttr(p.description || '')}">
    </div>
    <div class="form-group">
      <label>配置内容 (${p.agent === 'codex' ? 'TOML' : 'JSON'})</label>
      <textarea id="mPContent">${esc(p.content)}</textarea>
    </div>
    <div class="modal-actions">
      <button class="btn btn-ghost" onclick="closeModal()">取消</button>
      <button class="btn btn-primary" id="mPSave" style="width:auto">保存</button>
    </div>
  `);
  document.getElementById('mPSave').addEventListener('click', () => updateProfile(id));
}

async function updateProfile(id) {
  const name = document.getElementById('mPName').value.trim();
  const description = document.getElementById('mPDesc').value.trim();
  const content = document.getElementById('mPContent').value;
  const profile = profiles.find(item => item.id === id);
  if ((profile?.agent || 'claude') === 'claude') {
    try { JSON.parse(content); } catch { return alert('JSON 格式错误'); }
  }

  const response = await api(`/api/profiles/${id}`, {
    method: 'PUT',
    body: JSON.stringify({ name, description, content })
  });
  if (!response.ok) {
    const error = await response.json();
    return alert(error.error || '保存失败');
  }
  closeModal();
  loadProfiles();
  notifyAppReady();
}

async function deleteProfile(id) {
  if (!confirm('确定删除此配置？')) return;
  await api(`/api/profiles/${id}`, { method: 'DELETE' });
  loadProfiles();
  notifyAppReady();
}

// ============ WebSocket ============
async function connectWS(sessionId) {
  stopPolling();
  const generation = ++wsConnectGeneration;
  let response;
  try {
    response = await api('/api/ws-ticket', { method: 'POST' });
  } catch (error) {
    console.warn('获取 WebSocket 凭据失败:', error);
    return;
  }
  if (!response.ok || generation !== wsConnectGeneration || currentSessionId !== sessionId) return;
  const { ticket } = await response.json();
  if (!ticket || generation !== wsConnectGeneration || currentSessionId !== sessionId) return;

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const socket = new WebSocket(`${proto}//${location.host}/ws?ticket=${encodeURIComponent(ticket)}`);
  ws = socket;

  socket.onopen = () => {
    if (generation !== wsConnectGeneration) return socket.close();
    socket.send(JSON.stringify({ type: 'subscribe', sessionId }));
  };

  socket.onmessage = (event) => {
    try {
      const msg = JSON.parse(event.data);
      if (msg.type === 'output' && term) {
        term.write(msg.data);
      } else if (msg.type === 'exit' && term) {
        handleExit(msg.exitCode);
      }
    } catch {}
  };

  socket.onclose = () => { if (ws === socket) ws = null; };
}

function disconnectWS() {
  wsConnectGeneration++;
  if (ws) { ws.close(); ws = null; }
}

// ============ HTTP Polling ============
function startPolling(sessionId) {
  disconnectWS();
  pollSeq = 0;
  doPoll(sessionId);
}

function doPoll(sessionId) {
  if (pollTimer) clearTimeout(pollTimer);
  if (!currentSessionId || currentSessionId !== sessionId || !usePolling) return;

  api(`/api/sessions/${sessionId}/output?seq=${pollSeq}`)
    .then(r => r.json())
    .then(data => {
      if (data.output && term) {
        term.write(data.output);
      }
      if (data.running) {
        pollSeq = data.seq;
        pollTimer = setTimeout(() => doPoll(sessionId), POLL_INTERVAL);
      } else {
        handleExit(0);
      }
    })
    .catch(() => {
      // On error, retry after longer interval
      pollTimer = setTimeout(() => doPoll(sessionId), 2000);
    });
}

function stopPolling() {
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
}

function disconnectAll() {
  disconnectWS();
  stopPolling();
}

function openFileBrowser() {
  if (!currentSessionId) return;
  const s = sessions.find(x => x.id === currentSessionId);
  if (!s) return;
  const fbUrl = appConfig.fileBrowserUrl || '';
  if (!fbUrl) return alert('未配置 FileBrowser 地址');
  const workDir = s.working_dir || '/root';
  const fbPath = workDir.startsWith('/') ? workDir.slice(1) : workDir;
  window.open(`${fbUrl}/files/${fbPath}`, '_blank');
}

function handleExit(exitCode) {
  if (term) {
    term.write(`\r\n\x1b[33m[进程退出, code=${exitCode || 0}]\x1b[0m\r\n`);
  }
  const s = sessions.find(x => x.id === currentSessionId);
  if (s) { s.isRunning = false; updateSessionControls(s); }
  loadSessions();
}

// ============ Modal ============
function showModal(html) {
  document.getElementById('modalContent').innerHTML = html;
  document.getElementById('modalOverlay').classList.remove('hidden');
}

function closeModal() {
  document.getElementById('modalOverlay').classList.add('hidden');
}

document.addEventListener('click', e => {
  if (e.target.id === 'modalOverlay') closeModal();
});

// ============ Helpers ============
function esc(s) {
  if (!s) return '';
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function escAttr(s) {
  return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// Auto-refresh sessions list every 10s
setInterval(() => {
  if (authToken) loadSessions();
}, 10000);
