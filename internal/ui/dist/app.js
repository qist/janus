// app.js —— 控制台入口：登录引导 + hash 路由 + 侧边栏布局。

import { html } from './html.js';
import { render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import { api, getKey, setKey, clearKey } from './api.js';
import { Toasts, ConfirmHost, toast, toastErr, Field, ErrorBox } from './components.js';

import Overview from './pages/overview.js';
import Providers from './pages/providers.js';
import Credentials from './pages/credentials.js';
import Models from './pages/models.js';
import Routes from './pages/routes.js';
import Keys from './pages/keys.js';
import Agents from './pages/agents.js';
import Settings from './pages/settings.js';
import Requests from './pages/requests.js';
import Sessions from './pages/sessions.js';

// ---------- 路由表 ----------
const NAV = [
  { group: '总览' },
  { path: '/overview', label: '概览', ico: '◧', comp: Overview },
  { path: '/requests', label: '请求日志', ico: '☰', comp: Requests },
  { path: '/sessions', label: '会话', ico: '❏', comp: Sessions },
  { group: '接入' },
  { path: '/providers', label: 'Providers', ico: '⬡', comp: Providers },
  { path: '/credentials', label: '凭据', ico: '⚿', comp: Credentials },
  { path: '/models', label: '模型目录', ico: '▤', comp: Models },
  { path: '/routes', label: '路由规则', ico: '⇉', comp: Routes },
  { group: '客户端' },
  { path: '/keys', label: 'API Keys', ico: '⚿', comp: Keys },
  { path: '/agents', label: 'Agents', ico: '◎', comp: Agents },
  { group: '系统' },
  { path: '/settings', label: '设置', ico: '⚙', comp: Settings },
];

const ROUTES = Object.fromEntries(NAV.filter((n) => n.comp).map((n) => [n.path, n]));

function parseHash() {
  const raw = (location.hash || '').replace(/^#/, '');
  const path = raw.split('?')[0] || '/overview';
  return ROUTES[path] ? path : '/overview';
}

// ---------- 登录页 ----------
function Login({ onDone }) {
  const [mode, setMode] = useState('password'); // password | key
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [manual, setManual] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const submitPassword = async (e) => {
    e.preventDefault();
    if (!username.trim() || !password) { setError(new Error('请输入用户名和密码')); return; }
    setBusy(true); setError(null);
    try {
      const res = await api.login(username.trim(), password);
      setKey(res.key);
      toast('登录成功', 'ok');
      onDone();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  const submitKey = async (e) => {
    e.preventDefault();
    const k = manual.trim();
    if (!k) { setError(new Error('请粘贴 admin key')); return; }
    setBusy(true); setError(null);
    setKey(k);
    try {
      // 用 /settings 验证 key 是否可用（admin 专用端点）。
      await api.settings();
      toast('已连接', 'ok');
      onDone();
    } catch (err) {
      clearKey();
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return html`<div class="login">
    <div class="loginbox">
      <div class="brand">
        <div class="logo">J</div>
        <div class="name">Janus 控制台</div>
      </div>
      <div class="tabs">
        <button class=${mode === 'password' ? 'on' : ''} onClick=${() => { setMode('password'); setError(null); }}>账号登录</button>
        <button class=${mode === 'key' ? 'on' : ''} onClick=${() => { setMode('key'); setError(null); }}>Admin Key</button>
      </div>

      ${error ? html`<${ErrorBox} error=${error} />` : null}

      ${mode === 'password' ? html`<form onSubmit=${submitPassword}>
        <${Field} label="用户名">
          <input value=${username} onInput=${(e) => setUsername(e.target.value)} autocomplete="username" placeholder="admin" autofocus />
        <//>
        <${Field} label="密码">
          <input type="password" value=${password} onInput=${(e) => setPassword(e.target.value)} autocomplete="current-password" placeholder="••••••••" />
        <//>
        <button class="primary" style="width:100%;margin-top:6px" disabled=${busy}>
          ${busy ? '登录中…' : '登录'}
        </button>
        <div class="small muted" style="margin-top:12px;line-height:1.7">
          首次启动时终端会打印管理员账号与密码，也可用
          <code>data/bootstrap.key</code> 里的 admin key 直接登录。
        </div>
      </form>` : html`<form onSubmit=${submitKey}>
        <${Field} label="Admin Key" hint="形如 sk-janus-a-…；保存在浏览器 localStorage，不会上传。">
          <input type="password" value=${manual} onInput=${(e) => setManual(e.target.value)} placeholder="sk-janus-a-..." autocomplete="off" spellcheck="false" autofocus />
        <//>
        <button class="primary" style="width:100%;margin-top:6px" disabled=${busy}>
          ${busy ? '验证中…' : '连接'}
        </button>
      </form>`}
    </div>
  </div>`;
}

// ---------- 布局 ----------
function Shell({ path, children, onLogout }) {
  const active = ROUTES[path];
  useEffect(() => { document.title = (active ? active.label + ' · ' : '') + 'Janus 控制台'; }, [path]);
  return html`<div class="shell">
    <aside class="side">
      <div class="brand">
        <div class="logo">J</div>
        <div class="name">Janus</div>
        <div class="ver">console</div>
      </div>
      <nav class="nav">
        ${NAV.map((n) => n.group
          ? html`<div class="group">${n.group}</div>`
          : html`<a href=${'#' + n.path} class=${path === n.path ? 'on' : ''}>
              <span class="ico">${n.ico}</span>${n.label}
            </a>`)}
      </nav>
      <div class="foot">
        <button class="sm ghost" onClick=${onLogout} style="width:100%">退出登录</button>
      </div>
    </aside>
    <main class="main"><div class="wrap">${children}</div></main>
  </div>`;
}

// ---------- 根组件 ----------
function App() {
  const [authed, setAuthed] = useState(!!getKey());
  const [path, setPath] = useState(parseHash());

  useEffect(() => {
    const onHash = () => setPath(parseHash());
    const onUnauth = () => { setAuthed(false); };
    window.addEventListener('hashchange', onHash);
    window.addEventListener('janus:unauthorized', onUnauth);
    return () => {
      window.removeEventListener('hashchange', onHash);
      window.removeEventListener('janus:unauthorized', onUnauth);
    };
  }, []);

  const logout = () => { clearKey(); setAuthed(false); };

  if (!authed) {
    return html`<${Login} onDone=${() => { setAuthed(true); if (!location.hash) location.hash = '#/overview'; }} />
      <${Toasts} />`;
  }

  const active = ROUTES[path] || ROUTES['/overview'];
  const Page = active.comp;
  return html`<${Shell} path=${path} onLogout=${logout}>
      <${Page} key=${active.path} />
    <//>
    <${Toasts} />
    <${ConfirmHost} />`;
}

render(html`<${App} />`, document.getElementById('app'));
