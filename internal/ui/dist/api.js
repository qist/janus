// api.js —— 控制面 API 封装。
//
// 所有数据端点都要 admin key（Authorization: Bearer）。key 存 localStorage，
// 页面本身是静态资源、免鉴权。任何请求 401 都清掉本地 key 并广播事件，
// 由 app.js 回到登录页（与 docs/REFACTOR.md §5.4 一致，无服务端 session）。

const KEY_STORAGE = 'janus.admin_key';

export function getKey() {
  try { return localStorage.getItem(KEY_STORAGE) || ''; } catch { return ''; }
}

export function setKey(k) {
  try { localStorage.setItem(KEY_STORAGE, k); } catch { /* 隐私模式下忽略 */ }
}

export function clearKey() {
  try { localStorage.removeItem(KEY_STORAGE); } catch { /* ignore */ }
}

export class APIError extends Error {
  constructor(status, code, message, details) {
    super(message || ('HTTP ' + status));
    this.status = status;
    this.code = code || '';
    this.details = details;
  }
}

async function request(method, path, body) {
  const headers = { 'Accept': 'application/json' };
  const key = getKey();
  if (key) headers['Authorization'] = 'Bearer ' + key;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let resp;
  try {
    resp = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    throw new APIError(0, 'network_error', '无法连接 janus：' + e.message);
  }

  if (resp.status === 401) {
    clearKey();
    window.dispatchEvent(new CustomEvent('janus:unauthorized'));
    throw new APIError(401, 'unauthorized', '鉴权失败，请重新登录');
  }
  if (resp.status === 204) return null;

  const text = await resp.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = { raw: text }; }
  }
  if (!resp.ok) {
    const err = (data && data.error) || {};
    throw new APIError(resp.status, err.code, err.message || text || resp.statusText, err.details);
  }
  return data;
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b === undefined ? {} : b),
  patch: (p, b) => request('PATCH', p, b),
  del: (p) => request('DELETE', p),

  // ---------- 资源 ----------
  login: (username, password) => request('POST', '/api/v1/auth/login', { username, password }),
  settings: () => request('GET', '/api/v1/settings'),
  patchSettings: (patch) => request('PATCH', '/api/v1/settings', patch),
  schema: () => request('GET', '/api/v1/settings/schema'),

  providers: () => request('GET', '/api/v1/providers'),
  createProvider: (b) => request('POST', '/api/v1/providers', b),
  patchProvider: (id, b) => request('PATCH', '/api/v1/providers/' + enc(id), b),
  deleteProvider: (id) => request('DELETE', '/api/v1/providers/' + enc(id)),
  testProvider: (id) => request('POST', '/api/v1/providers/' + enc(id) + '/test'),
  discoverProvider: (id) => request('POST', '/api/v1/providers/' + enc(id) + '/discover'),

  credentials: (providerId) => request('GET', '/api/v1/credentials' + (providerId ? '?provider_id=' + enc(providerId) : '')),
  createCredential: (b) => request('POST', '/api/v1/credentials', b),
  patchCredential: (id, b) => request('PATCH', '/api/v1/credentials/' + enc(id), b),
  deleteCredential: (id) => request('DELETE', '/api/v1/credentials/' + enc(id)),
  verifyCredential: (id) => request('POST', '/api/v1/credentials/' + enc(id) + '/verify'),

  models: (q) => request('GET', '/api/v1/models' + (q ? '?' + q : '')),
  createModel: (b) => request('POST', '/api/v1/models', b),
  patchModel: (id, b) => request('PATCH', '/api/v1/models/' + encPath(id), b),
  deleteModel: (id) => request('DELETE', '/api/v1/models/' + encPath(id)),

  routes: () => request('GET', '/api/v1/routes'),
  createRoute: (b) => request('POST', '/api/v1/routes', b),
  patchRoute: (id, b) => request('PATCH', '/api/v1/routes/' + enc(id), b),
  deleteRoute: (id) => request('DELETE', '/api/v1/routes/' + enc(id)),
  resolveRoute: (b) => request('POST', '/api/v1/routes/resolve', b),

  keys: (kind) => request('GET', '/api/v1/keys' + (kind ? '?kind=' + enc(kind) : '')),
  createKey: (b) => request('POST', '/api/v1/keys', b),
  revokeKey: (id) => request('DELETE', '/api/v1/keys/' + enc(id)),
  rotateKey: (id) => request('POST', '/api/v1/keys/' + enc(id) + '/rotate'),

  agents: () => request('GET', '/api/v1/agents'),
  createAgent: (b) => request('POST', '/api/v1/agents', b),
  patchAgent: (id, b) => request('PATCH', '/api/v1/agents/' + enc(id), b),
  deleteAgent: (id) => request('DELETE', '/api/v1/agents/' + enc(id)),
  provisionAgent: (id) => request('POST', '/api/v1/agents/' + enc(id) + '/provision'),

  sessions: () => request('GET', '/api/v1/sessions'),
  deleteSession: (key) => request('DELETE', '/api/v1/sessions/' + encPath(key)),

  usage: () => request('GET', '/api/v1/usage'),
  requests: (limit) => request('GET', '/api/v1/requests' + (limit ? '?limit=' + limit : '')),
};

function enc(s) { return encodeURIComponent(String(s)); }
// 模型 id 形如 provider/model：斜杠必须保留，其余字符转义。
function encPath(s) { return String(s).split('/').map(enc).join('/'); }
