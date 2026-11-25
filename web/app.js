'use strict';

// ---------- tiny DOM helpers (never use innerHTML: node names come from subscriptions) ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === false || value === null || value === undefined) continue;
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else if (key in el && key !== 'list' && key !== 'type') el[key] = value;
    else el.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

const $ = (selector) => document.querySelector(selector);

function toast(message, isError) {
  const el = h('div', { class: 'toast' + (isError ? ' error' : ''), text: message });
  $('#toasts').append(el);
  setTimeout(() => el.remove(), isError ? 7000 : 2600);
}

function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(() => toast('已复制'), () => fallbackCopy(text));
  } else {
    fallbackCopy(text);
  }
}

function fallbackCopy(text) {
  const area = h('textarea', { value: text, readOnly: true });
  area.className = 'hidden-copy';
  document.body.append(area);
  area.select();
  try {
    document.execCommand('copy');
    toast('已复制');
  } catch {
    toast('复制失败，请手动选择复制', true);
  }
  area.remove();
}

function timeAgo(iso) {
  if (!iso) return '从未';
  const seconds = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (seconds < 60) return '刚刚';
  if (seconds < 3600) return Math.floor(seconds / 60) + ' 分钟前';
  if (seconds < 86400) return Math.floor(seconds / 3600) + ' 小时前';
  return Math.floor(seconds / 86400) + ' 天前';
}

function maskURL(raw) {
  try {
    const url = new URL(raw);
    const parts = url.pathname.split('/').filter(Boolean);
    const last = parts.length ? parts[parts.length - 1] : '';
    const path = parts.length > 1 ? '/…/' + last : url.pathname;
    return url.protocol + '//' + url.host + path + (url.search ? '?…' : '');
  } catch {
    return raw;
  }
}

// ---------- API ----------

class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  const options = { method, headers: { 'X-SBPanel': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch('/api' + path, options);
  } catch {
    throw new ApiError('连不上面板，请检查网络或 sbpanel 是否在运行', 0);
  }
  let data = null;
  try {
    data = await response.json();
  } catch {
    data = null;
  }
  if (!response.ok) throw new ApiError((data && data.error) || response.statusText, response.status);
  return data;
}

// Runs an action with a spinner on the button, then refreshes the state. Polling does not
// re-render while an action is pending, so the spinner is not wiped out by a fresh button.
async function act(button, action, successMessage) {
  ui.pending++;
  if (button) {
    button.disabled = true;
    button.classList.add('busy');
  }
  try {
    await action();
    if (successMessage) toast(successMessage);
    return true;
  } catch (error) {
    toast(error.message, true);
    return false;
  } finally {
    ui.pending--;
    if (button) {
      button.disabled = false;
      button.classList.remove('busy');
    }
    await refresh(true);
  }
}

// ---------- state ----------

const ui = {
  view: null,
  tab: readStored('sbpanel-tab', 'nodes'),
  showAdd: false,
  editing: null,
  logs: [],
  followLogs: true,
  pending: 0,
  kernel: null,
  kernelPoll: null,
  panel: null,
  panelPoll: null,
  restarting: false,
};

function readStored(key, fallback) {
  try {
    return localStorage.getItem(key) || fallback;
  } catch {
    return fallback;
  }
}

function store(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* private mode: ignore */
  }
}

async function refresh(force) {
  if (ui.pending > 0 && !force) return;
  try {
    const view = await api('GET', '/state');
    view.subscriptions = (view.subscriptions || []).map((sub) => ({ ...sub, nodes: sub.nodes || [] }));
    ui.view = view;
    if (ui.pending > 0 && !force) return;
    renderDynamic();
  } catch (error) {
    if (!ui.restarting) setOffline(error.message);
  }
}

async function refreshLogs() {
  if (ui.tab !== 'logs') return;
  try {
    const data = await api('GET', '/logs');
    ui.logs = data.lines || [];
    renderLogs();
  } catch {
    /* shown by the state poll */
  }
}

function setOffline(message) {
  const banner = $('#offline');
  if (banner) {
    banner.textContent = message;
    banner.classList.remove('hidden');
  }
}

// ---------- helpers on the view ----------

function allNodes(view) {
  return view.subscriptions.flatMap((sub) => sub.nodes.map((node) => ({ ...node, subName: sub.name })));
}

function findNode(view, ref) {
  if (!ref) return null;
  return allNodes(view).find((node) => node.subId === ref.subId && node.tag === ref.tag) || null;
}

function sameRef(a, b) {
  return !!a && !!b && a.subId === b.subId && a.tag === b.tag;
}

function delayBadge(node) {
  if (node.delay === undefined || node.delay === null) return null;
  if (node.delay <= 0) return h('span', { class: 'badge bad', text: '超时' });
  const level = node.delay < 300 ? 'good' : node.delay < 800 ? 'warn' : 'bad';
  return h('span', { class: 'badge ' + level, text: node.delay + ' ms' });
}

// ---------- rendering ----------

function render() {
  const app = $('#app');
  app.replaceChildren();
  app.append(
    renderHeader(),
    h('div', { id: 'offline', class: 'notice hidden' }),
    h('div', { id: 'config-error', class: 'notice hidden' }),
    h('div', { id: 'status' }),
    renderTabs(),
    h('div', { id: 'tab-body' }),
  );
  renderTabBody();
  renderDynamic();
}

function renderHeader() {
  return h('div', { class: 'header' },
    h('div', { class: 'brand' }, h('div', { class: 'logo', text: 'S' }), h('h1', { text: 'sbpanel' })),
    h('div', { id: 'core-pill' }),
  );
}

const UPDATE_VIA_LABELS = {
  auto: '优先走代理，不通再直连',
  proxy: '只走代理',
  direct: '只直连',
};

const TABS = [
  ['nodes', '节点'],
  ['subs', '订阅'],
  ['logs', '日志'],
  ['settings', '设置'],
];

function renderTabs() {
  return h('div', { class: 'tabs', role: 'tablist' }, TABS.map(([id, label]) =>
    h('button', {
      class: 'tab' + (ui.tab === id ? ' active' : ''), role: 'tab', text: label,
      onclick: () => {
        ui.tab = id;
        store('sbpanel-tab', id);
        ui.showAdd = false;
        ui.editing = null;
        render();
        if (id === 'logs') refreshLogs();
      },
    })));
}

// Parts that change on every poll. Forms are rendered separately so typing is not interrupted.
function renderDynamic() {
  const view = ui.view;
  if (!view || !$('#status')) return;
  $('#offline').classList.add('hidden');

  const configError = $('#config-error');
  configError.textContent = view.configError ? '出错了：' + view.configError : '';
  configError.classList.toggle('hidden', !view.configError);

  $('#core-pill').replaceChildren(corePill(view));
  $('#status').replaceChildren(renderStatus(view));
  if (ui.tab === 'nodes') $('#tab-body').replaceChildren(renderNodes(view));
  if (ui.tab === 'subs') {
    const list = $('#sub-list');
    if (!list) renderTabBody();
    else if (!ui.editing) list.replaceChildren(...renderSubList(view));
  }
  if (ui.tab === 'settings' && !$('#settings-form')) renderTabBody();
}

function corePill(view) {
  if (view.core.running) return h('span', { class: 'pill good', text: 'sing-box 运行中' });
  if (view.coreEnabled && view.configError) return h('span', { class: 'pill bad', text: 'sing-box 启动失败' });
  if (view.coreEnabled && hasNodes(view)) return h('span', { class: 'pill warn', text: 'sing-box 正在重启' });
  return h('span', { class: 'pill', text: 'sing-box 已停止' });
}

function hasNodes(view) {
  return allNodes(view).some((node) => !node.helper);
}

function renderStatus(view) {
  if (view.subscriptions.length === 0) {
    return h('div', { class: 'card empty' },
      h('h2', { text: '还没有订阅' }),
      h('p', { text: '把订阅地址加进来，sbpanel 会下载节点、生成 sing-box 配置并启动代理。' }),
      h('button', { class: 'primary', text: '添加订阅', onclick: () => openAddSubscription() }),
    );
  }

  const running = view.core.running;
  let title;
  let subtitle;
  if (!view.selected) {
    const picked = findNode(view, view.autoPick);
    title = '自动选择';
    subtitle = running ? (picked ? '当前用的是：' + picked.tag : '正在测速挑选…') : '启动后自动挑延迟最低的节点';
  } else {
    const node = findNode(view, view.selected);
    title = node ? node.tag : view.selected.tag;
    subtitle = node ? node.subName + ' · ' + node.type : '这个节点已经不在订阅里了';
  }

  const actions = running
    ? [
        h('button', { text: '重启', onclick: (event) => act(event.currentTarget, () => api('POST', '/core/restart'), '已重启') }),
        h('button', { class: 'danger', text: '停止', onclick: (event) => act(event.currentTarget, () => api('POST', '/core/stop'), '已停止') }),
      ]
    : [h('button', { class: 'primary', text: '启动代理', disabled: !hasNodes(view),
        onclick: (event) => act(event.currentTarget, () => api('POST', '/core/start'), '已启动') })];

  const proxy = view.proxyUrl;
  const localHostPort = proxy.replace(/^http:\/\//, '');
  // When the proxy listens on every interface, other machines reach it by the address
  // they already use for this panel.
  const lan = view.settings.listenAddr === '0.0.0.0' && location.hostname !== '127.0.0.1' && location.hostname !== 'localhost';
  const hostPort = lan ? location.hostname + ':' + view.settings.listenPort : localHostPort;
  const exportLine = 'export http_proxy=' + proxy + ' https_proxy=' + proxy + ' all_proxy=socks5h://' + hostPort;

  return h('div', { class: 'card status-card' },
    h('div', { class: 'status-main' },
      h('span', { class: 'muted small', text: running ? '当前节点' : '已选节点（未运行）' }),
      h('span', { class: 'status-node', text: title }),
      h('span', { class: 'muted small break', text: subtitle }),
    ),
    h('div', { class: 'status-actions' }, actions),
    h('div', { class: 'proxy-line' },
      h('span', { class: 'muted small', text: lan ? '代理地址（局域网设备用这个；本机也可以用 ' + localHostPort + '）' : '代理地址（HTTP 和 SOCKS5 都行）' }),
      h('span', { class: 'mono grow proxy-address', text: hostPort }),
      h('button', { class: 'small', text: '复制地址', onclick: () => copyText(hostPort) }),
      h('button', { class: 'small', text: '复制终端命令', title: exportLine, onclick: () => copyText(exportLine) }),
    ),
  );
}

function renderTabBody() {
  const body = $('#tab-body');
  if (ui.tab === 'nodes') return; // filled by renderDynamic
  if (ui.tab === 'subs') body.replaceChildren(renderSubscriptions());
  if (ui.tab === 'logs') body.replaceChildren(renderLogsTab());
  if (ui.tab === 'settings') body.replaceChildren(renderSettings());
}

// ---------- nodes tab ----------

function renderNodes(view) {
  if (view.subscriptions.length === 0) return h('div');
  const running = view.core.running;
  const testButton = h('button', { text: '全部测速', disabled: !running, title: running ? '' : '先启动代理才能测速',
    onclick: (event) => act(event.currentTarget, () => api('POST', '/delay-test'), '测速完成') });

  const autoNode = h('button', {
    class: 'node' + (!view.selected ? ' selected' : ''),
    onclick: (event) => select(event.currentTarget, null),
  },
  h('span', { class: 'radio' }),
  h('span', { class: 'grow' },
    h('div', { class: 'node-name', text: '自动选择' }),
    h('div', { class: 'node-meta', text: '每 10 分钟测速一次，自动用延迟最低的节点' })),
  );

  const groups = view.subscriptions.map((sub) => {
    const selectable = sub.nodes.filter((node) => !node.helper);
    const disabledCount = selectable.filter((node) => node.disabled).length;
    return h('div', { class: 'card' },
      h('div', { class: 'group-head' },
        h('h2', { text: sub.name }),
        h('span', { class: 'muted small', text: selectable.length + ' 个节点' + (disabledCount ? '，' + disabledCount + ' 个不可用' : '') })),
      selectable.length === 0
        ? h('p', { class: 'muted small', text: sub.lastError ? '更新失败，到“订阅”页查看原因' : '这个订阅还没有节点' })
        : h('div', { class: 'node-list' }, selectable.map((node) => renderNode(view, node))),
    );
  });

  return h('div', {},
    h('div', { class: 'card' },
      h('div', { class: 'group-head' }, h('h2', { text: '选择节点' }), testButton),
      h('div', { class: 'node-list' }, autoNode),
    ),
    groups,
  );
}

function renderNode(view, node) {
  const selected = sameRef(view.selected, node);
  const inUse = view.core.running && (sameRef(view.current, node) || (!view.selected && sameRef(view.autoPick, node)));
  const meta = [node.type.toUpperCase(), node.server ? node.server + (node.port ? ':' + node.port : '') : null]
    .filter(Boolean).join(' · ');
  return h('button', {
    class: 'node' + (selected ? ' selected' : ''),
    disabled: !!node.disabled,
    title: node.disabled || '',
    onclick: (event) => select(event.currentTarget, { subId: node.subId, tag: node.tag }),
  },
  h('span', { class: 'radio' }),
  h('span', { class: 'grow' },
    h('div', { class: 'node-name', text: node.tag }),
    h('div', { class: 'node-meta', text: node.disabled ? '不可用：' + node.disabled : meta })),
  inUse && !selected ? h('span', { class: 'badge accent', text: '自动选中' }) : null,
  delayBadge(node),
  );
}

function select(button, ref) {
  act(button, () => api('POST', '/select', ref || {}), null);
}

// ---------- subscriptions tab ----------

function openAddSubscription() {
  ui.tab = 'subs';
  store('sbpanel-tab', 'subs');
  ui.showAdd = true;
  ui.editing = null;
  render();
  const input = $('#sub-url');
  if (input) input.focus();
}

function renderSubscriptions() {
  const view = ui.view;
  const header = h('div', { class: 'spread card-title' },
    h('h2', { text: '订阅' }),
    h('div', { class: 'row' },
      view && view.subscriptions.length > 1
        ? h('button', { text: '全部更新', onclick: (event) => act(event.currentTarget, () => api('POST', '/subscriptions/update-all'), '全部更新完成') })
        : null,
      h('button', { class: 'primary', text: '添加订阅', onclick: () => {
        ui.showAdd = true;
        ui.editing = null;
        renderTabBody();
        $('#sub-url').focus();
      } }),
    ),
  );
  return h('div', {},
    ui.showAdd ? subscriptionForm(null) : null,
    h('div', { class: 'card' }, header, h('div', { id: 'sub-list' }, view ? renderSubList(view) : [])),
  );
}

function renderSubList(view) {
  if (view.subscriptions.length === 0) {
    return [h('p', { class: 'muted', text: '还没有订阅。点右上角“添加订阅”。' })];
  }
  return view.subscriptions.map((sub) => {
    if (ui.editing === sub.id) return subscriptionForm(sub);
    const selectable = sub.nodes.filter((node) => !node.helper).length;
    const interval = sub.intervalHours > 0 ? '每约 ' + sub.intervalHours + ' 小时自动更新' : '手动更新';
    const via = sub.updatedVia ? '（上次通过' + sub.updatedVia + '）' : '';
    return h('div', { class: 'sub-item' },
      h('div', { class: 'spread' },
        h('div', { class: 'grow' },
          h('h3', { text: sub.name }),
          h('div', { class: 'mono muted ellipsis', title: '点“编辑”可以看到完整地址', text: maskURL(sub.url) }),
          h('div', { class: 'muted small' },
            selectable + ' 个节点 · 更新于 ' + timeAgo(sub.updatedAt) + via + ' · ' + interval + ' · ' + UPDATE_VIA_LABELS[sub.updateVia || 'auto']),
        ),
        h('div', { class: 'row' },
          h('button', { class: 'small' + (sub.updating ? ' busy' : ''), text: '更新', disabled: sub.updating,
            onclick: (event) => act(event.currentTarget, () => api('POST', '/subscriptions/' + sub.id + '/update'), '已更新 ' + sub.name) }),
          h('button', { class: 'small', text: '编辑', onclick: () => {
            ui.editing = sub.id;
            ui.showAdd = false;
            renderTabBody();
          } }),
          h('button', { class: 'small danger', text: '删除', onclick: (event) => {
            if (!confirm('删除订阅“' + sub.name + '”？它的节点也会一起删除。')) return;
            act(event.currentTarget, () => api('DELETE', '/subscriptions/' + sub.id), '已删除');
          } }),
        ),
      ),
      sub.lastError ? h('div', { class: 'sub-error', text: '上次更新失败：' + sub.lastError }) : null,
    );
  });
}

function subscriptionForm(sub) {
  const editing = !!sub;
  const name = h('input', { type: 'text', placeholder: '不填就用域名', value: editing ? sub.name : '' });
  const url = h('input', { type: 'url', id: editing ? null : 'sub-url', placeholder: 'https://…', value: editing ? sub.url : '', required: true });
  const userAgent = h('input', { type: 'text', placeholder: '默认：sbpanel (sing-box; SFA compatible)', value: editing ? sub.userAgent : '' });
  const interval = h('input', { type: 'number', min: 0, max: 720, value: editing ? sub.intervalHours : 0 });
  const currentVia = editing ? sub.updateVia || 'auto' : 'auto';
  const updateVia = h('select', {}, Object.entries(UPDATE_VIA_LABELS).map(([value, label]) =>
    h('option', { value, text: label, selected: value === currentVia })));
  const submit = h('button', { class: 'primary', type: 'submit', text: editing ? '保存' : '添加并更新' });

  const close = () => {
    ui.showAdd = false;
    ui.editing = null;
    renderTabBody();
  };

  const form = h('form', { class: 'stack', onsubmit: async (event) => {
    event.preventDefault();
    const body = {
      name: name.value,
      url: url.value,
      userAgent: userAgent.value,
      intervalHours: Number(interval.value || 0),
      updateVia: updateVia.value,
    };
    const ok = await act(submit, () => editing
      ? api('PUT', '/subscriptions/' + sub.id, body)
      : api('POST', '/subscriptions', body), editing ? '已保存' : '订阅已添加');
    if (ok || !editing) close();
  } },
  h('div', { class: 'form-grid' },
    h('label', { class: 'field' }, '订阅地址', url,
      h('span', { class: 'hint', text: 'fscarmen 脚本的订阅：用 http://服务器:端口/UUID/sing-box 这个地址' })),
    h('label', { class: 'field' }, '名称', name),
  ),
  h('details', { class: 'advanced', open: editing && (sub.userAgent || sub.intervalHours > 0 || currentVia !== 'auto') },
    h('summary', { text: '高级选项' }),
    h('div', { class: 'form-grid' },
      h('label', { class: 'field' }, 'User-Agent', userAgent,
        h('span', { class: 'hint', text: '订阅服务器按它决定返回哪种格式，一般不用改' })),
      h('label', { class: 'field' }, '更新方式', updateVia,
        h('span', { class: 'hint', text: '订阅地址里有你的账号密钥，走代理更新可以加密这次请求' })),
      h('label', { class: 'field' }, '自动更新间隔（小时，0 = 只手动更新）', interval,
        h('span', { class: 'hint', text: '默认只手动更新：固定时间拉订阅容易被识别。开了也会随机推迟一点' })),
    ),
  ),
  h('div', { class: 'form-actions' },
    h('button', { type: 'button', text: '取消', onclick: close }),
    submit,
  ));

  return h('div', { class: editing ? 'sub-item' : 'card' },
    h('h2', { class: 'card-title', text: editing ? '编辑订阅' : '添加订阅' }),
    form,
  );
}

// ---------- logs tab ----------

function renderLogsTab() {
  const pre = h('pre', { class: 'log', id: 'log-view' });
  pre.addEventListener('scroll', () => {
    ui.followLogs = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 8;
  });
  const result = h('div', { class: 'card' },
    h('div', { class: 'spread card-title' },
      h('h2', { text: 'sing-box 日志' }),
      h('span', { class: 'muted small', text: '最近 500 行，每 2 秒刷新' })),
    pre,
  );
  queueMicrotask(renderLogs);
  return result;
}

function renderLogs() {
  const pre = $('#log-view');
  if (!pre) return;
  if (ui.logs.length === 0) {
    pre.textContent = '（还没有日志）';
    return;
  }
  pre.replaceChildren(...ui.logs.map((line) => {
    let cls = '';
    if (line.startsWith('[sbpanel]')) cls = 'log-panel';
    else if (/\b(ERROR|FATAL|PANIC)\b/.test(line)) cls = 'log-error';
    else if (/\bWARN\b/.test(line)) cls = 'log-warn';
    return h('div', { class: cls, text: line });
  }));
  if (ui.followLogs) pre.scrollTop = pre.scrollHeight;
}

// ---------- settings tab ----------

function renderSettings() {
  const view = ui.view;
  if (!view) return h('div');
  const settings = view.settings;

  const listenOptions = [['127.0.0.1', '仅本机（127.0.0.1）'], ['0.0.0.0', '局域网也能用（0.0.0.0）']];
  if (!listenOptions.some(([value]) => value === settings.listenAddr)) listenOptions.push([settings.listenAddr, settings.listenAddr]);
  const listenAddr = h('select', {}, listenOptions.map(([value, label]) =>
    h('option', { value, text: label, selected: value === settings.listenAddr })));
  const listenPort = h('input', { type: 'number', min: 1, max: 65535, value: settings.listenPort });
  const proxyUser = h('input', { type: 'text', value: settings.proxyUser, autocomplete: 'off' });
  const proxyPass = h('input', { type: 'text', value: settings.proxyPass, autocomplete: 'off' });
  const dnsServer = h('input', { type: 'text', value: settings.dnsServer });
  const strategies = [['', '默认'], ['prefer_ipv4', '优先 IPv4'], ['prefer_ipv6', '优先 IPv6'], ['ipv4_only', '只用 IPv4'], ['ipv6_only', '只用 IPv6']];
  const dnsStrategy = h('select', {}, strategies.map(([value, label]) =>
    h('option', { value, text: label, selected: value === settings.dnsStrategy })));
  const testUrl = h('input', { type: 'url', value: settings.testUrl });
  const levels = ['trace', 'debug', 'info', 'warn', 'error'];
  const logLevel = h('select', {}, levels.map((level) =>
    h('option', { value: level, text: level, selected: level === settings.logLevel })));
  const bypassChina = h('input', { type: 'checkbox', checked: !!settings.bypassChina });
  const save = h('button', { class: 'primary', type: 'submit', text: '保存并应用' });

  const settingsForm = h('form', { class: 'card stack', id: 'settings-form', onsubmit: async (event) => {
    event.preventDefault();
    const ok = await act(save, () => api('PUT', '/settings', {
      listenAddr: listenAddr.value,
      listenPort: Number(listenPort.value),
      proxyUser: proxyUser.value,
      proxyPass: proxyPass.value,
      dnsServer: dnsServer.value,
      dnsStrategy: dnsStrategy.value,
      testUrl: testUrl.value,
      logLevel: logLevel.value,
      bypassChina: bypassChina.checked,
    }), '设置已保存');
    if (ok) renderTabBody();
  } },
  h('h2', { text: '分流' }),
  h('label', { class: 'check' }, bypassChina, '绕过大陆：国内网站和 IP 直连，其他走代理'),
  h('span', { class: 'muted small', text: '和 v2rayN 的“绕过大陆”一样，用 sing-box 官方的 geosite-cn、geoip-cn 规则。关掉时只有内网地址直连，其余全部走代理。' }),
  renderRuleSetStatus(view),
  h('h2', { text: '代理设置' }),
  h('div', { class: 'form-grid' },
    h('label', { class: 'field' }, '谁能用这个代理', listenAddr),
    h('label', { class: 'field' }, '代理端口', listenPort),
    h('label', { class: 'field' }, '代理用户名（可选）', proxyUser,
      h('span', { class: 'hint', text: '选“局域网也能用”时建议设置' })),
    h('label', { class: 'field' }, '代理密码（可选）', proxyPass),
  ),
  h('h2', { text: 'DNS 与测速' }),
  h('div', { class: 'form-grid' },
    h('label', { class: 'field' }, 'DNS 服务器', dnsServer,
      h('span', { class: 'hint', text: 'local = 用系统 DNS；也可以填 223.5.5.5、tls://223.5.5.5、https://dns.alidns.com/dns-query' })),
    h('label', { class: 'field' }, 'DNS 策略', dnsStrategy),
    h('label', { class: 'field' }, '测速地址', testUrl),
    h('label', { class: 'field' }, '日志级别', logLevel),
  ),
  h('div', { class: 'form-actions' }, save));

  return h('div', {}, settingsForm, renderPanelCard(), renderKernelCard(), renderConfigCard());
}

function renderRuleSetStatus(view) {
  const rules = view.ruleSets || {};
  let text = '还没有下载规则文件。打开开关并保存时会自动下载（约 100 KB，优先走代理）。';
  if (rules.present) {
    text = (rules.updatedAt ? '规则文件更新于 ' + timeAgo(rules.updatedAt) + '。' : '规则文件已就绪。')
      + '规则文件不会自动更新，隔几周手动点一次就行。';
  }
  return h('div', { class: 'row small muted' },
    h('span', { class: 'grow', text }),
    h('button', { type: 'button', class: 'small', text: rules.present ? '更新规则文件' : '下载规则文件',
      onclick: (event) => act(event.currentTarget, () => api('POST', '/rulesets/update'), '规则文件已更新') }));
}

// ---------- sing-box kernel ----------

function renderKernelCard() {
  const card = h('div', { class: 'card stack', id: 'kernel-card' }, h('h2', { text: 'sing-box 内核' }), h('p', { class: 'muted small', text: '读取中…' }));
  loadKernel();
  return card;
}

async function loadKernel() {
  try {
    ui.kernel = await api('GET', '/kernel');
  } catch {
    return;
  }
  renderKernelBody();
  if (ui.kernel.job && ui.kernel.job.running) scheduleKernelPoll();
}

function scheduleKernelPoll() {
  clearTimeout(ui.kernelPoll);
  ui.kernelPoll = setTimeout(async () => {
    if (ui.tab !== 'settings') return;
    const wasRunning = ui.kernel && ui.kernel.job && ui.kernel.job.running;
    await loadKernel();
    const job = ui.kernel && ui.kernel.job;
    if (wasRunning && job && !job.running) {
      if (job.error) toast('升级失败：' + job.error, true);
      else toast(job.message || '升级完成');
      refresh();
    }
  }, 1000);
}

function compareVersions(a, b) {
  const parse = (v) => {
    const [core, pre] = String(v).replace(/^v/, '').split('-', 2);
    return { nums: core.split('.').map(Number), pre: pre || '' };
  };
  const x = parse(a);
  const y = parse(b);
  for (let i = 0; i < 3; i++) {
    if ((x.nums[i] || 0) !== (y.nums[i] || 0)) return (x.nums[i] || 0) - (y.nums[i] || 0);
  }
  if (x.pre === y.pre) return 0;
  if (!x.pre) return 1;
  if (!y.pre) return -1;
  return x.pre.localeCompare(y.pre, 'en', { numeric: true });
}

function renderKernelBody() {
  const card = $('#kernel-card');
  const kernel = ui.kernel;
  if (!card || !kernel) return;
  const job = kernel.job || {};
  const active = kernel.active || {};
  const installed = !!active.path;
  const sourceLabel = kernel.source === 'managed' ? '面板安装的版本' : '系统自带（' + active.path + '）';

  const rows = [
    h('div', { class: 'spread' },
      h('div', {},
        h('div', { class: 'status-node', text: !installed ? '还没有安装 sing-box' : active.version ? 'sing-box ' + active.version : '版本未知' }),
        h('div', { class: 'muted small', text: installed ? '正在使用：' + sourceLabel : '点“检查更新”，然后安装稳定版' })),
      h('button', { text: '检查更新', disabled: job.running || !kernel.supported,
        title: '只在你点的时候访问 GitHub，不会自动检查',
        onclick: async (event) => {
          await act(event.currentTarget, async () => { ui.kernel = await api('POST', '/kernel/check'); }, null);
          renderKernelBody(); // after act, so its finally block does not touch a replaced button
        } })),
  ];

  if (!kernel.supported) rows.push(h('p', { class: 'muted small', text: '这台机器的系统或架构暂不支持在面板里升级。' }));

  if (job.running) {
    const percent = job.total ? Math.floor((job.done / job.total) * 100) : 0;
    rows.push(h('div', { class: 'notice', text: '正在安装 ' + job.tag + '：' + job.stage + (job.stage === '下载中' && job.total ? '（' + percent + '%）' : '') }));
  }

  const releases = kernel.releases;
  if (releases) {
    const offers = [];
    for (const [release, label] of [[releases.stable, '稳定版'], [releases.prerelease, '测试版']]) {
      if (!release) continue;
      const cmp = active.version ? compareVersions(release.version, active.version) : 1;
      offers.push(h('div', { class: 'spread sub-item' },
        h('div', {},
          h('div', {}, label + ' ' + release.version + (cmp === 0 ? '（正在使用）' : cmp < 0 ? '（比现在的旧）' : '')),
          h('div', { class: 'muted small', text: '发布于 ' + timeAgo(release.publishedAt) })),
        h('button', { class: cmp > 0 ? 'primary small' : 'small', disabled: job.running || cmp === 0,
          text: cmp > 0 ? '升级到 ' + release.version : '安装这个版本',
          onclick: (event) => {
            const tip = label === '测试版' ? '测试版可能有问题，' : '';
            if (!confirm(tip + '安装 sing-box ' + release.version + '？\n会先用新版本检查当前配置，通过后才切换；启动失败会自动换回原来的版本。')) return;
            act(event.currentTarget, async () => {
              await api('POST', '/kernel/install', { tag: release.tag });
              await loadKernel();
              scheduleKernelPoll();
            }, '开始下载，完成后会提示');
          } })));
    }
    rows.push(h('div', { class: 'muted small', text: '检查于 ' + timeAgo(releases.checkedAt) + '（通过' + releases.via + '）' }));
    rows.push(...offers);
  }

  const extra = [];
  if (kernel.previous) {
    extra.push(h('button', { class: 'small', disabled: job.running, text: '回退到 ' + (kernel.previous.version || '上一个版本'),
      onclick: (event) => {
        if (!confirm('换回 sing-box ' + (kernel.previous.version || '上一个版本') + '？')) return;
        act(event.currentTarget, async () => { await api('POST', '/kernel/rollback'); await loadKernel(); }, '已回退');
      } }));
  }
  if (kernel.source === 'managed' && kernel.system) {
    extra.push(h('button', { class: 'small', disabled: job.running, text: '改用系统自带的 ' + (kernel.system.version || ''),
      onclick: (event) => act(event.currentTarget, async () => { await api('POST', '/kernel/use-system'); await loadKernel(); }, '已改用系统自带的 sing-box') }));
  }
  if (extra.length) rows.push(h('div', { class: 'row' }, extra));
  if (job.error && !job.running) rows.push(h('div', { class: 'sub-error', text: '上次升级失败：' + job.error }));

  card.replaceChildren(h('h2', { text: 'sing-box 内核' }), ...rows);
}

// ---------- sbpanel itself ----------

function renderPanelCard() {
  const card = h('div', { class: 'card stack', id: 'panel-card' }, h('h2', { text: 'sbpanel 版本' }), h('p', { class: 'muted small', text: '读取中…' }));
  loadPanel();
  return card;
}

async function loadPanel() {
  try {
    ui.panel = await api('GET', '/panel');
  } catch {
    return;
  }
  renderPanelBody();
  if (ui.panel.job && ui.panel.job.running) schedulePanelPoll();
}

// Keeps polling on every tab: when the download is done the panel restarts, and the
// page has to reload to get the new version's web files.
function schedulePanelPoll() {
  clearTimeout(ui.panelPoll);
  ui.panelPoll = setTimeout(async () => {
    const before = (ui.panel && ui.panel.job) || {};
    let panel;
    try {
      panel = await api('GET', '/panel');
    } catch (error) {
      if (error.status === 0) waitForRestart();
      else schedulePanelPoll();
      return;
    }
    const job = panel.job || {};
    if (before.running && !job.running && !job.error) {
      waitForRestart(); // a fresh process answered: the restart already happened
      return;
    }
    ui.panel = panel;
    renderPanelBody();
    if (job.running) schedulePanelPoll();
    else if (before.running && job.error) toast('升级失败：' + job.error, true);
  }, 1000);
}

async function waitForRestart() {
  if (ui.restarting) return;
  ui.restarting = true;
  clearTimeout(ui.panelPoll);
  setOffline('sbpanel 正在重启，稍等几秒…');
  renderPanelBody();
  const started = Date.now();
  while (Date.now() - started < 180000) {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    try {
      const response = await fetch('/api/health', { cache: 'no-store' });
      if (response.ok) {
        location.reload();
        return;
      }
    } catch {
      /* still restarting */
    }
  }
  ui.restarting = false;
  setOffline('sbpanel 没有重新启动起来，请在服务器上运行 sbpanel status 和 sbpanel logs 查看');
}

function rememberSwitch(target) {
  try {
    sessionStorage.setItem('sbpanel-switch', target);
  } catch {
    /* private mode: no message after the reload */
  }
}

// After the reload, says whether the switch to the new version worked.
async function reportSwitch() {
  let target = null;
  try {
    target = sessionStorage.getItem('sbpanel-switch');
    sessionStorage.removeItem('sbpanel-switch');
  } catch {
    return;
  }
  if (!target) return;
  try {
    const panel = await api('GET', '/panel');
    if (compareVersions(panel.version, target) === 0) toast('sbpanel 已切换到 ' + panel.version);
    else toast(panel.lastError || '没有切换成功，现在还是 ' + panel.version, true);
  } catch {
    /* the state poll shows connection problems */
  }
}

function renderPanelBody() {
  const card = $('#panel-card');
  const panel = ui.panel;
  if (!card || !panel) return;
  const job = panel.job || {};
  const busy = job.running || ui.restarting;

  const rows = [
    h('div', { class: 'spread' },
      h('div', {},
        h('div', { class: 'status-node', text: 'sbpanel ' + panel.version }),
        h('div', { class: 'muted small', text: panel.managed ? '在网页里升级的版本（安装时是 ' + panel.baseVersion + '）' : '安装时的版本' })),
      h('button', { text: '检查更新', disabled: busy || !panel.supported,
        title: '只在你点的时候访问 GitHub，优先走已启动的代理',
        onclick: async (event) => {
          await act(event.currentTarget, async () => { ui.panel = await api('POST', '/panel/check'); }, null);
          renderPanelBody();
        } })),
  ];

  if (!panel.supported) rows.push(h('p', { class: 'muted small', text: panel.unsupported }));

  if (ui.restarting) {
    rows.push(h('div', { class: 'notice', text: '正在重启，面板和代理会中断几秒钟…' }));
  } else if (job.running) {
    const percent = job.total ? Math.floor((job.done / job.total) * 100) : 0;
    rows.push(h('div', { class: 'notice', text: (job.tag ? '正在升级到 ' + job.tag + '：' : '') + job.stage
      + (job.stage === '下载中' && job.total ? '（' + percent + '%）' : '') }));
  }

  const release = panel.release;
  if (release) {
    const cmp = compareVersions(release.version, panel.version);
    rows.push(h('div', { class: 'muted small', text: '检查于 ' + timeAgo(release.checkedAt) + '（通过' + release.via + '）' }));
    rows.push(h('div', { class: 'spread sub-item' },
      h('div', {},
        h('div', {}, '最新版 ' + release.tag + (cmp === 0 ? '（正在使用）' : cmp < 0 ? '（比现在的旧）' : '')),
        h('div', { class: 'muted small' }, '发布于 ' + timeAgo(release.publishedAt) + ' · ',
          h('a', { href: release.url, target: '_blank', rel: 'noopener noreferrer', text: '更新说明' }))),
      cmp > 0 ? h('button', { class: 'primary small', disabled: busy || !panel.supported, text: '升级到 ' + release.tag,
        onclick: (event) => {
          if (!confirm('升级 sbpanel 到 ' + release.tag + '？\n\n下载优先走已启动的代理，按 SHA-256 校验。'
            + '下载完面板会自动重启，代理会中断几秒钟；新版本启动失败会自动换回现在的版本。')) return;
          act(event.currentTarget, async () => {
            await api('POST', '/panel/install', { tag: release.tag });
            rememberSwitch(release.tag);
            await loadPanel();
            schedulePanelPoll();
          }, '开始下载，完成后面板会自动重启');
        } }) : null));
  }

  if (panel.managed && panel.previous) {
    rows.push(h('div', { class: 'row' },
      h('button', { class: 'small', disabled: busy, text: '回退到 ' + panel.previous,
        onclick: (event) => {
          if (!confirm('换回 sbpanel ' + panel.previous + '？面板会重启，代理会中断几秒钟。')) return;
          act(event.currentTarget, async () => {
            await api('POST', '/panel/rollback');
            rememberSwitch(panel.previous);
            ui.panel.job = { running: true, stage: '重启中' };
            renderPanelBody();
            schedulePanelPoll();
          }, null);
        } })));
  }
  if (panel.lastError) rows.push(h('div', { class: 'sub-error', text: panel.lastError }));
  if (job.error && !job.running) rows.push(h('div', { class: 'sub-error', text: '上次升级失败：' + job.error }));

  card.replaceChildren(h('h2', { text: 'sbpanel 版本' }), ...rows);
}

function renderConfigCard() {
  const pre = h('pre', { class: 'code hidden' });
  const button = h('button', { text: '查看', onclick: async () => {
    if (!pre.classList.contains('hidden')) {
      pre.classList.add('hidden');
      button.textContent = '查看';
      return;
    }
    try {
      const response = await fetch('/api/config', { credentials: 'same-origin' });
      pre.textContent = response.ok ? await response.text() : '还没有生成配置（先添加订阅）';
    } catch {
      pre.textContent = '读取失败';
    }
    pre.classList.remove('hidden');
    button.textContent = '收起';
  } });
  return h('div', { class: 'card stack' },
    h('div', { class: 'spread' },
      h('div', {},
        h('h2', { text: '生成的 sing-box 配置' }),
        h('div', { class: 'muted small', text: '只读，排查问题用。里面有节点密码，不要截图发给别人。' })),
      button),
    pre,
  );
}

// ---------- start ----------

async function boot() {
  render();
  await refresh();
  reportSwitch();
  if (ui.tab === 'logs') refreshLogs();
  setInterval(() => {
    if (!document.hidden) refresh();
  }, 3000);
  setInterval(() => {
    if (!document.hidden) refreshLogs();
  }, 2000);
}

boot();
