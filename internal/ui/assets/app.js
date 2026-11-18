'use strict';

const app = document.getElementById('app');
const keyInput = document.getElementById('api-key');
const state = { key: sessionStorage.getItem('janus-key') || '', timer: null };
keyInput.value = state.key;

const STATUS_CLASS = {
  open: 'st-open',
  running: 'st-running',
  review: 'st-review',
  approved: 'st-approved',
  integrating: 'st-integrating',
  done: 'st-done',
  blocked: 'st-blocked',
  cancelled: 'st-cancelled',
  planning: 'st-planning',
  active: 'st-running',
  failed: 'st-blocked',
};

function esc(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function badge(status) {
  const label = esc(status || 'unknown');
  return `<span class="badge ${STATUS_CLASS[status] || ''}">${label}</span>`;
}

function pct(value) {
  return Math.max(0, Math.min(100, Number(value) || 0));
}

async function api(path, options) {
  if (!state.key) throw new Error('请先在右上角保存 API key');
  const opts = Object.assign({}, options);
  opts.headers = Object.assign(
    { 'Content-Type': 'application/json', 'X-API-Key': state.key },
    (options && options.headers) || {}
  );
  const response = await fetch(path, opts);
  if (response.status === 204) return null;
  const text = await response.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { /* keep text */ }
  if (!response.ok) {
    const code = (data && data.error && data.error.code) || String(response.status);
    const message = (data && data.error && data.error.message) || text || 'request failed';
    throw new Error(code + ': ' + message);
  }
  return data;
}

function toast(message, bad) {
  let node = document.getElementById('toast');
  if (!node) {
    node = document.createElement('div');
    node.id = 'toast';
    document.body.appendChild(node);
  }
  node.textContent = message;
  node.className = bad ? 'toast bad' : 'toast';
  clearTimeout(node._timer);
  node._timer = setTimeout(() => { node.remove(); }, 3000);
}

document.getElementById('save-key').addEventListener('click', () => {
  state.key = keyInput.value.trim();
  sessionStorage.setItem('janus-key', state.key);
  toast(state.key ? 'API key 已保存到本标签页' : 'API key 已清除');
  route();
});

document.getElementById('auto-refresh').addEventListener('change', (event) => {
  if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
  if (event.target.checked) {
    state.timer = setInterval(() => {
      const tag = document.activeElement && document.activeElement.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      route();
    }, 8000);
  }
});

window.addEventListener('hashchange', route);
window.addEventListener('DOMContentLoaded', route);

async function route() {
  const hash = location.hash || '#/goals';
  document.querySelectorAll('.tabs a').forEach((link) => {
    link.classList.toggle('active', hash.startsWith(link.getAttribute('href')));
  });
  app.innerHTML = '<p class="muted">加载中…</p>';
  try {
    if (hash.startsWith('#/goals/')) {
      return await renderGoal(decodeURIComponent(hash.split('/')[2]));
    }
    if (hash.startsWith('#/goals')) return await renderGoals();
    if (hash.startsWith('#/items/')) {
      return await renderItem(decodeURIComponent(hash.split('/')[2]));
    }
    if (hash.startsWith('#/items')) return await renderItems();
    app.innerHTML = '<div class="error">未知页面</div>';
  } catch (error) {
    app.innerHTML = `<div class="error">${esc(error.message)}</div>`;
  }
}

async function renderGoals() {
  const data = await api('/api/v1/goals?limit=100');
  const goals = data.goals || [];

  app.innerHTML = `
    <section class="panel">
      <h2>新建目标</h2>
      <form id="goal-form" class="grid">
        <label>标题<input name="title" required placeholder="fix claim race"></label>
        <label>工作区<input name="workspace_id" required value="ws"></label>
        <label class="wide">意图<textarea name="brief" rows="3" placeholder="一段话说明要达成什么"></textarea></label>
        <label>风险<select name="risk">
          <option value="low">low</option>
          <option value="medium" selected>medium</option>
          <option value="high">high</option>
          <option value="critical">critical</option>
        </select></label>
        <label>预算 token<input name="budget_tokens" type="number" min="0" placeholder="可选"></label>
        <div class="actions"><button type="submit">提交（202 异步拆项）</button></div>
      </form>
    </section>
    <section class="panel">
      <h2>目标 <span class="muted small">${goals.length}</span></h2>
      ${goals.length ? `
        <table class="table">
          <thead><tr><th>目标</th><th>状态</th><th>工作项</th><th>创建</th></tr></thead>
          <tbody>${goals.map((goal) => `
            <tr>
              <td>
                <a href="#/goals/${encodeURIComponent(goal.id)}">${esc(goal.title)}</a>
                <div class="muted small">${esc(goal.id)}</div>
              </td>
              <td>${badge(goal.status)}</td>
              <td>${(goal.item_ids || []).length}</td>
              <td class="muted small">${new Date((goal.created_at || 0) * 1000).toLocaleString()}</td>
            </tr>`).join('')}
          </tbody>
        </table>` : '<p class="muted">还没有目标，上面建一个。</p>'}
    </section>`;

  document.getElementById('goal-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = new FormData(event.target);
    const payload = {
      title: form.get('title'),
      brief: form.get('brief'),
      workspace_id: form.get('workspace_id'),
      risk: form.get('risk'),
    };
    const budget = Number(form.get('budget_tokens'));
    if (budget > 0) payload.budget_tokens = budget;
    try {
      const created = await api('/api/v1/goals', { method: 'POST', body: JSON.stringify(payload) });
      toast('已提交：' + created.goal_id);
      location.hash = '#/goals/' + encodeURIComponent(created.goal_id);
    } catch (error) {
      toast(error.message, true);
    }
  });
}

async function renderGoal(id) {
  const data = await api('/api/v1/goals/' + encodeURIComponent(id));
  const goal = data.goal;
  const progress = data.progress || {};
  const counts = Object.entries(progress.counts || {})
    .map(([status, count]) => `${badge(status)} ${count}`)
    .join(' ');

  app.innerHTML = `
    <section class="panel">
      <div class="row between">
        <div>
          <h2>${esc(goal.title)}</h2>
          <p class="muted">${esc(goal.brief || '')}</p>
        </div>
        <div class="actions">
          <a class="button ghost" href="#/goals">返回</a>
          <button id="cancel-goal" class="danger" ${goal.status === 'done' || goal.status === 'cancelled' ? 'disabled' : ''}>取消目标</button>
        </div>
      </div>
      <div class="meter"><div style="width:${pct(progress.percent)}%"></div></div>
      <p class="muted small">${badge(progress.status)} 完成 ${progress.done || 0}/${progress.total || 0} · ${pct(progress.percent)}%</p>
      <p class="counts">${counts}</p>
      ${goal.error ? `<div class="error">${esc(goal.error)}</div>` : ''}
    </section>
    <section class="panel">
      <h3>工作项</h3>
      ${(goal.item_ids || []).length
        ? `<ul class="list">${goal.item_ids.map((itemID) => `
            <li><a href="#/items/${encodeURIComponent(itemID)}">${esc(itemID)}</a></li>`).join('')}
          </ul>`
        : '<p class="muted">规划中，或没有产出工作项。</p>'}
    </section>`;

  document.getElementById('cancel-goal').addEventListener('click', async () => {
    try {
      await api('/api/v1/goals/' + encodeURIComponent(goal.id) + '/cancel', { method: 'POST' });
      toast('目标已取消');
      route();
    } catch (error) {
      toast(error.message, true);
    }
  });
}

async function renderItems() {
  const query = new URLSearchParams((location.hash.split('?')[1]) || '');
  const status = query.get('status') || '';
  const data = await api('/api/v1/work-items' + (status ? '?status=' + encodeURIComponent(status) : ''));
  const items = data.items || [];
  const statuses = ['', 'open', 'running', 'review', 'approved', 'integrating', 'done', 'blocked', 'cancelled'];

  app.innerHTML = `
    <section class="panel">
      <div class="row between">
        <h2>工作项 <span class="muted small">${items.length}</span></h2>
        <label class="muted small">状态
          <select id="status-filter">
            ${statuses.map((value) => `<option value="${value}" ${value === status ? 'selected' : ''}>${value || '全部'}</option>`).join('')}
          </select>
        </label>
      </div>
      ${items.length ? `
        <table class="table">
          <thead><tr><th>ID</th><th>标题</th><th>状态</th><th>风险</th><th>优先级</th></tr></thead>
          <tbody>${items.map((item) => `
            <tr>
              <td><a href="#/items/${encodeURIComponent(item.id)}">${esc(item.id)}</a></td>
              <td>${esc(item.title)}</td>
              <td>${badge(item.status)}</td>
              <td>${esc((item.contract && item.contract.risk) || '')}</td>
              <td>${item.priority || 0}</td>
            </tr>`).join('')}
          </tbody>
        </table>` : '<p class="muted">该过滤条件下没有工作项。</p>'}
    </section>
    <section class="panel">
      <h3>新建工作项</h3>
      <form id="item-form" class="grid">
        <label>ID<input name="id" required placeholder="w1"></label>
        <label>工作区<input name="workspace_id" required value="ws"></label>
        <label>标题<input name="title" required></label>
        <label>路径（逗号分隔）<input name="paths" placeholder="internal/work"></label>
        <label>风险<select name="risk">
          <option value="low">low</option>
          <option value="medium" selected>medium</option>
          <option value="high">high</option>
          <option value="critical">critical</option>
        </select></label>
        <label>优先级<input name="priority" type="number" value="0"></label>
        <label class="wide">目标<textarea name="goal" rows="2"></textarea></label>
        <div class="actions"><button type="submit">创建工作项</button></div>
      </form>
    </section>`;

  document.getElementById('status-filter').addEventListener('change', (event) => {
    const value = event.target.value;
    location.hash = '#/items' + (value ? '?status=' + encodeURIComponent(value) : '');
  });

  document.getElementById('item-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = new FormData(event.target);
    const paths = String(form.get('paths') || '')
      .split(',')
      .map((part) => part.trim())
      .filter(Boolean);
    const payload = {
      item: {
        id: form.get('id'),
        workspace_id: form.get('workspace_id'),
        title: form.get('title'),
        priority: Number(form.get('priority') || 0),
        contract: {
          goal: form.get('goal') || '',
          risk: form.get('risk'),
          paths: paths,
        },
      },
    };
    try {
      await api('/api/v1/work-items', { method: 'POST', body: JSON.stringify(payload) });
      toast('工作项已创建');
      route();
    } catch (error) {
      toast(error.message, true);
    }
  });
}

async function renderItem(id) {
  const view = await api('/api/v1/work-items/' + encodeURIComponent(id));
  const item = view.item;
  const claim = view.claim;
  const reviews = view.reviews || [];
  const warnings = view.warnings || [];

  app.innerHTML = `
    <section class="panel">
      <div class="row between">
        <div>
          <h2>${esc(item.title)}</h2>
          <p class="muted small">${esc(item.id)} · ${esc(item.workspace_id)}</p>
        </div>
        <div class="actions">
          <a class="button ghost" href="#/items">返回</a>
          <button id="cancel-item" class="danger" ${item.status === 'done' || item.status === 'cancelled' ? 'disabled' : ''}>取消工作项</button>
        </div>
      </div>
      <p>${badge(item.status)}
        <span class="muted">风险 ${esc((item.contract && item.contract.risk) || '-')} · 优先级 ${item.priority || 0} · attempts ${item.attempts || 0}</span>
      </p>
      <p class="muted">${esc((item.contract && item.contract.goal) || item.brief || '')}</p>
      ${(item.contract && item.contract.paths || []).length
        ? `<p class="muted small">路径：${item.contract.paths.map(esc).join(', ')}</p>` : ''}
      ${warnings.length ? `<div class="warn">${warnings.map((w) => `<div>${esc(w)}</div>`).join('')}</div>` : ''}
    </section>
    <section class="panel">
      <h3>租约</h3>
      ${claim
        ? `<p>${esc(claim.agent_id)} · ${esc(claim.role)} · epoch ${claim.lease_epoch} · 到期 ${new Date(claim.lease_expires_at * 1000).toLocaleString()}${claim.forced ? ' · <strong>forced</strong>' : ''}</p>
           <p class="muted small">${esc(claim.plan || '')}</p>`
        : '<p class="muted">当前没有活跃租约。</p>'}
      <form id="assign-form" class="grid">
        <label>Agent<input name="agent_id" required value="agent-forced"></label>
        <label>角色<select name="role">
          <option value="worker">worker</option>
          <option value="verifier">verifier</option>
          <option value="integrator">integrator</option>
        </select></label>
        <label>模型<input name="model_id" placeholder="model-1"></label>
        <label class="wide">计划<textarea name="plan" rows="2" required>resolve the blocker</textarea></label>
        <div class="actions"><button type="submit" class="danger">强制派工（admin）</button></div>
      </form>
    </section>
    <section class="panel">
      <h3>评审 <span class="muted small">${reviews.length}</span></h3>
      ${reviews.length ? `<div class="reviews">${reviews.map((review) => `
        <div class="review">
          <p>${badge(review.verdict)} <span class="muted small">${esc(review.reviewer_id)}</span></p>
          <p>${esc(review.reasons || '')}</p>
          ${(review.checks || []).map((check) => `<div class="check">${esc(check.name)} exit=${check.exit_code}</div>`).join('')}
        </div>`).join('')}</div>` : '<p class="muted">还没有评审。</p>'}
    </section>`;

  document.getElementById('cancel-item').addEventListener('click', async () => {
    try {
      await api('/api/v1/work-items/' + encodeURIComponent(item.id) + '/cancel', { method: 'POST' });
      toast('工作项已取消');
      route();
    } catch (error) {
      toast(error.message, true);
    }
  });

  document.getElementById('assign-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = new FormData(event.target);
    const payload = {
      agent_id: form.get('agent_id'),
      role: form.get('role'),
      plan: form.get('plan'),
      model_id: form.get('model_id'),
    };
    try {
      await api('/api/v1/work-items/' + encodeURIComponent(item.id) + '/assign', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      toast('已强制派工');
      route();
    } catch (error) {
      toast(error.message, true);
    }
  });
}
