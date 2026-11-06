// The bridge credential stays in extension context, never in a content script.
let bridge;
let polling = false;
let reconnect;
let reconnectDelay = 1000;
const sessionQueues = new Map();
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

// Tabs: the bot keeps one main tab, which a run takes over from the last
// one so pages and sign-ins carry on. Runs that work at the same time (job
// workers) each get their own background tab in the same window. When a
// run ends its tab closes, together with any tabs its pages opened; the
// main tab stays. State lives in session storage and changes one at a time.
let tabLock = Promise.resolve();
function withTabs(fn) {
  const run = tabLock.then(fn);
  tabLock = run.catch(() => {});
  return run;
}

async function tabState() {
  const state = await chrome.storage.session.get(['main', 'claims', 'marks', 'openers', 'tidied']);
  return { main: state.main, claims: state.claims || {}, marks: state.marks || {}, openers: state.openers || {}, tidied: !!state.tidied };
}

async function liveTab(id) {
  if (id === undefined) return undefined;
  try { return await chrome.tabs.get(id); } catch { return undefined; }
}

async function botWindow(expires) {
  // An MV3 worker can connect before Chrome has a current browser window.
  const deadline = Math.min(expires, Date.now() + 5000);
  while (Date.now() < deadline) {
    const windows = await chrome.windows.getAll({ windowTypes: ['normal'] });
    const window = windows.find(window => window.focused) || windows[0];
    if (window) return window;
    await sleep(100);
  }
  throw new Error('The bot browser window is still starting; no tab was opened');
}

// tidy runs once per browser start: blank start pages and tabs left from
// earlier runs close, leaving one tab to become the main tab.
async function tidy(state) {
  const tabs = await chrome.tabs.query({ windowType: 'normal' });
  const keep = tabs.find(tab => bridge.page && tab.url === bridge.page) || tabs.find(tab => !/^(about:blank|chrome:\/\/newtab)/.test(tab.url || '')) || tabs[0];
  for (const tab of tabs) if (tab.id !== keep?.id) await chrome.tabs.remove(tab.id).catch(() => {});
  state.main = keep?.id;
  state.tidied = true;
}

// claimTab returns the session's tab, opening one when needed. Sessions ''
// and 'control' (the runtime itself) use the main tab without claiming it.
async function claimTab(session, epoch, expires, restore = true) {
  const tab = await withTabs(async () => {
    const state = await tabState();
    if (!state.tidied) await tidy(state);
    const shared = session === '' || session === 'control';
    let tab = shared ? undefined : await liveTab(state.claims[session]);
    let main = await liveTab(state.main);
    if (!tab) {
      const mainTaken = main && Object.entries(state.claims).some(([owner, id]) => owner !== session && id === main.id);
      if (main && (shared || !mainTaken)) {
        tab = main;
      } else {
        const windowId = main ? main.windowId : (await botWindow(expires)).id;
        if (Date.now() >= expires) throw new Error('Expired browser command; no tab was opened');
        const url = !main && restore && bridge.page ? bridge.page : 'about:blank';
        tab = await chrome.tabs.create({ windowId, url, active: !main });
        if (!main) { main = tab; state.main = tab.id; }
      }
      if (!shared) state.claims[session] = tab.id;
    }
    const mark = `${session}\n${epoch}`;
    const reset = state.marks[tab.id] !== mark;
    state.marks[tab.id] = mark;
    await chrome.storage.session.set(state);
    return Object.assign(tab, { reset });
  });
  if (tab.reset) {
    // A new run can retain the page but must get fresh element references.
    try { await chrome.tabs.sendMessage(tab.id, { agentRuntime: true, args: ['reset'] }, { frameId: 0 }); } catch { /* no content script yet */ }
  }
  if (tab.status !== 'complete' && tab.url && tab.url !== 'about:blank') await loaded(tab.id, expires).catch(() => {});
  return liveTab(tab.id);
}

// release closes a finished run's tab and the tabs it opened. The main tab
// stays open for the next run.
async function release(session) {
  return withTabs(async () => {
    const state = await tabState();
    const id = state.claims[session];
    delete state.claims[session];
    if (id !== undefined) {
      const closing = new Set(id === state.main ? [] : [id]);
      // Tabs opened from the run's tab (new-tab links, pop-ups), transitively.
      let grew = true;
      while (grew) {
        grew = false;
        for (const [child, opener] of Object.entries(state.openers)) {
          const childId = Number(child);
          if (!closing.has(childId) && (opener === id || closing.has(opener))) { closing.add(childId); grew = true; }
        }
      }
      for (const tabId of closing) {
        await chrome.tabs.remove(tabId).catch(() => {});
        delete state.openers[tabId];
        delete state.marks[tabId];
      }
    }
    await chrome.storage.session.set(state);
    return {};
  });
}

// Screenshots capture the visible tab, so a background tab is brought to
// the front for the capture; one capture at a time.
let captureLock = Promise.resolve();
function oneCapture(fn) {
  const run = captureLock.then(fn);
  captureLock = run.catch(() => {});
  return run;
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
    // Settle after acknowledgement: waiting in the page can lose it on navigation.
    if (command.args[0] === 'click') await sleep(Math.min(200, Math.max(0, command.expires - Date.now())));
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
  if (action === 'release') return release(command.session);
  if (action === 'tabs') {
    // Runtime-only: the bot window's tabs, for status and tests.
    const tabs = await chrome.tabs.query({ windowType: 'normal' });
    return { count: tabs.length, urls: tabs.map(tab => tab.url || '') };
  }
  const tab = await claimTab(command.session, command.epoch, command.expires, action !== 'open');
  if (!tab) throw new Error('The bot\'s tab was closed; call browser_open again');
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
    return oneCapture(async () => {
      // captureVisibleTab captures the active tab: bring this one forward and
      // check it is still the active tab before capturing.
      if (!tab.active) await chrome.tabs.update(tab.id, { active: true });
      const current = await chrome.tabs.get(tab.id);
      if (!current.active) throw new Error('The bot\'s tab could not be shown for a screenshot');
      let timeout;
      try {
        // Chrome capture can stall; do not block the command queue until the bridge expires.
        const image = await Promise.race([
          chrome.tabs.captureVisibleTab(current.windowId, { format: value }),
          new Promise((_, reject) => {
            timeout = setTimeout(() => reject(new Error('Screenshot capture timed out; check that the bot browser window is visible before requesting another screenshot')),
              Math.max(0, Math.min(5000, command.expires - Date.now())));
          }),
        ]);
        return { image };
      } finally { clearTimeout(timeout); }
    });
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
      // Runs work in parallel, each in its own tab; one run's commands keep
      // their order. Keep polling while a command runs.
      const key = command.session || '';
      const previous = sessionQueues.get(key) || Promise.resolve();
      const next = previous.then(async () => {
        let result;
        try { result = { success: true, data: await execute(command) }; }
        catch (error) { result = { success: false, error: error.message }; }
        try { await request('POST', { id: command.id, result }); } catch { /* the runtime times the command out */ }
      });
      sessionQueues.set(key, next);
      void next.finally(() => { if (sessionQueues.get(key) === next) sessionQueues.delete(key); });
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
// Record which tab opened a new one, so a run's pop-ups close with it.
// Links with target=_blank have no opener tab; webNavigation still names
// the source tab.
function recordOpener(tabId, openerId) {
  if (openerId === undefined) return;
  void withTabs(async () => {
    const state = await tabState();
    const known = new Set([state.main, ...Object.values(state.claims), ...Object.keys(state.openers).map(Number)]);
    if (!known.has(openerId)) return;
    state.openers[tabId] = openerId;
    await chrome.storage.session.set(state);
  });
}
chrome.tabs.onCreated.addListener(tab => recordOpener(tab.id, tab.openerTabId));
chrome.webNavigation.onCreatedNavigationTarget.addListener(details => recordOpener(details.tabId, details.sourceTabId));
chrome.tabs.onRemoved.addListener(tabId => {
  void withTabs(async () => {
    const state = await tabState();
    delete state.openers[tabId];
    delete state.marks[tabId];
    if (state.main === tabId) delete state.main;
    for (const [session, id] of Object.entries(state.claims)) if (id === tabId) delete state.claims[session];
    await chrome.storage.session.set(state);
  });
});
