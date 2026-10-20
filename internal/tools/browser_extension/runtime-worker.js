// The bridge credential stays in extension context, never in a content script.
let bridge;
let polling = false;
let reconnect;
let reconnectDelay = 1000;
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function request(method, body) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 25000);
  try {
    const response = await fetch(bridge.url, {
      method, headers: { Authorization: `Bearer ${bridge.token}`, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal,
      cache: 'no-store', redirect: 'error',
    });
    if (!response.ok) throw new Error('Bridge unavailable');
    return response.status === 204 ? null : await response.json();
  } finally { clearTimeout(timeout); }
}

async function ownedTab(session, epoch, expires, restore = true) {
  const state = await chrome.storage.session.get(['tabId', 'session', 'epoch']);
  let tab;
  if (state.tabId !== undefined) {
    try { tab = await chrome.tabs.get(state.tabId); } catch { /* closed by user */ }
  }
  if (!tab) {
    // An MV3 worker can connect before Chrome has a current browser window.
    const deadline = Math.min(expires, Date.now() + 5000);
    let window;
    while (Date.now() < deadline) {
      const windows = await chrome.windows.getAll({ windowTypes: ['normal'] });
      window = windows.find(window => window.focused) || windows[0];
      if (window) break;
      await sleep(100);
    }
    if (!window) throw new Error('The bot browser window is still starting; no tab was opened');
    if (Date.now() >= expires) throw new Error('Expired browser command; no tab was opened');
    tab = await chrome.tabs.create({ windowId: window.id, url: restore && bridge.page ? bridge.page : 'about:blank', active: true });
    await chrome.storage.session.set({ tabId: tab.id });
    if (restore && bridge.page) await loaded(tab.id, expires);
  }
  if (state.session !== session || state.epoch !== epoch) {
    // A new run can retain the page but must get fresh element references.
    try { await chrome.tabs.sendMessage(tab.id, { agentRuntime: true, args: ['reset'] }, { frameId: 0 }); } catch { /* no content script yet */ }
  }
  await chrome.storage.session.set({ tabId: tab.id, session, epoch });
  return tab;
}

async function loaded(tabId, expires) {
  while (Date.now() < expires) {
    const tab = await chrome.tabs.get(tabId);
    if (tab.status === 'complete') return;
    await sleep(100);
  }
  throw new Error('Navigation timed out; inspect the page before retrying');
}

function navigationError(error) {
  return /Receiving end does not exist|message port closed|message channel closed/i.test(error.message || '');
}

async function pageCommand(tabId, command) {
  const deadline = Math.min(command.expires, Date.now() + 5000);
  const retryable = command.args[0] === 'snapshot' || (command.args[0] === 'get' && command.args[1] === 'text');
  while (Date.now() < deadline) {
    try {
      const ready = await chrome.tabs.sendMessage(tabId, { agentRuntime: true, args: ['ready'], expires: command.expires }, { frameId: 0 });
      if (!ready?.success || !ready.data?.ready) { await sleep(100); continue; }
    } catch (error) {
      if (!navigationError(error)) throw error;
      await sleep(100);
      continue;
    }
    if (Date.now() >= command.expires) throw new Error('Expired browser command');
    let result;
    try {
      result = await chrome.tabs.sendMessage(tabId, { agentRuntime: true, args: command.args, expires: command.expires }, { frameId: 0 });
    } catch (error) {
      if (!navigationError(error)) throw error;
      if (!retryable) throw new Error('Page changed before the action could be confirmed; do not repeat it automatically. Call browser_snapshot to inspect the current page.');
      await sleep(100);
      continue;
    }
    if (!result?.success) throw new Error(result?.error || 'Page driver unavailable');
    return result.data;
  }
  throw new Error('Page is still navigating; call browser_snapshot again. The extension is connected.');
}

async function execute(command) {
  if (!command || !Array.isArray(command.args) || Date.now() >= command.expires) throw new Error('Expired browser command');
  const [action, value] = command.args;
  if (action === 'close') {
    const windows = await chrome.windows.getAll();
    setTimeout(() => { for (const window of windows) void chrome.windows.remove(window.id); }, 300);
    return {};
  }
  const tab = await ownedTab(command.session, command.epoch, command.expires, action !== 'open');
  if (Date.now() >= command.expires) throw new Error('Expired browser command');
  if (action === 'open') {
    const url = new URL(value);
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) throw new Error('Only HTTP and HTTPS pages are supported');
    await chrome.tabs.update(tab.id, { url: url.href });
    await loaded(tab.id, command.expires);
    const page = await chrome.tabs.get(tab.id);
    return { url: page.url, title: page.title };
  }
  if (action === 'get' && ['url', 'title'].includes(value)) return { [value]: tab[value] || '' };
  if (action === 'screenshot') {
    if (!['png', 'jpeg'].includes(value)) throw new Error('Unsupported screenshot format');
    // captureVisibleTab captures the active tab: never capture a different page.
    if (!tab.active) throw new Error('Activate the bot\'s tab before taking a screenshot');
    let timeout;
    try {
      // Chrome capture can stall; do not block the command queue until the bridge expires.
      const image = await Promise.race([
        chrome.tabs.captureVisibleTab(tab.windowId, { format: value }),
        new Promise((_, reject) => {
          timeout = setTimeout(() => reject(new Error('Screenshot capture timed out; check that the bot browser window is visible before requesting another screenshot')),
            Math.max(0, Math.min(5000, command.expires - Date.now())));
        }),
      ]);
      return { image };
    } finally { clearTimeout(timeout); }
  }
  if (action === 'wait' && /^\d+$/.test(value)) {
    const ms = Number(value);
    if (ms > 10000) throw new Error('Extension waits are limited to 10 seconds');
    await sleep(ms);
    return {};
  }
  if (Date.now() >= command.expires) throw new Error('Expired browser command');
  return pageCommand(tab.id, command);
}

async function poll() {
  if (polling) return;
  clearTimeout(reconnect);
  polling = true;
  try {
    // Read data afresh: Chrome can cache imported scripts across browser restarts.
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 5000);
    try { bridge = await (await fetch(chrome.runtime.getURL('bridge.json'), { cache: 'no-store', signal: controller.signal })).json(); }
    finally { clearTimeout(timeout); }
    // A bounded cycle lets Manifest V3 retire this worker; alarms reconnect it.
    const until = Date.now() + 4 * 60 * 1000;
    while (Date.now() < until) {
      const command = await request('GET');
      reconnectDelay = 1000;
      if (!command) continue;
      let result;
      try { result = { success: true, data: await execute(command) }; }
      catch (error) { result = { success: false, error: error.message }; }
      await request('POST', { id: command.id, result });
    }
  } catch {
    // Retry the connection, never a dequeued command; alarms remain the MV3 fallback.
    reconnect = setTimeout(() => void poll(), reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 5000);
  }
  finally { polling = false; }
}
chrome.alarms.create('runtime-bridge', { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(alarm => { if (alarm.name === 'runtime-bridge') void poll(); });
chrome.runtime.onStartup.addListener(() => void poll());
chrome.runtime.onInstalled.addListener(() => void poll());
void poll();
