(() => {
  let references = new Map();
  let generation = 0;
  const documentID = Array.from(crypto.getRandomValues(new Uint32Array(2)), n => n.toString(16)).join('');
  const text = value => String(value || '').replace(/\s+/g, ' ').trim();
  const visible = element => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden';
  const selector = 'a[href],button,input:not([type=hidden]),textarea,select,[role=button],[role=link],[role=checkbox],[role=combobox],[role=menuitem],[role=option],[role=radio],[role=switch],[contenteditable=true],[tabindex]:not([tabindex="-1"])';

  // shortcut: main-frame DOM snapshots, add frame routing after compatibility trials.
  function snapshot(args) {
    references = new Map();
    generation++;
    const rootSelector = args.includes('-s') ? args[args.indexOf('-s') + 1] : 'body';
    const root = document.querySelector(rootSelector);
    if (!root) throw new Error('Snapshot selector has no match');
    const lines = [];
    for (const element of root.querySelectorAll(selector)) {
      if (!visible(element)) continue;
      const ref = `e${documentID}_${generation}_${references.size + 1}`;
      references.set(`@${ref}`, element);
      const labelledBy = (element.getAttribute('aria-labelledby') || '').split(/\s+/).map(id => document.getElementById(id)?.textContent || '').join(' ');
      const label = text(labelledBy || element.getAttribute('aria-label') || (element.labels && Array.from(element.labels).map(label => label.innerText).join(' ')) || element.innerText || element.getAttribute('placeholder') || element.getAttribute('title') || element.querySelector('img[alt]')?.getAttribute('alt')).slice(0, 180);
      const role = element.getAttribute('role') || element.tagName.toLowerCase();
      lines.push(`- ${role} ${JSON.stringify(label)} [ref=${ref}]${element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true' ? ' [disabled]' : ''}`);
      if (lines.length >= 300) break;
    }
    const frames = Array.from(document.querySelectorAll('iframe')).filter(visible);
    if (frames.length) lines.push(`[${frames.length} embedded frame(s): frame interactions require human takeover in this version]`);
    return { snapshot: lines.join('\n') || '[No visible interactive elements]' };
  }

  function target(ref) {
    const element = references.get(ref);
    if (!element?.isConnected || !visible(element)) throw new Error('Stale element reference; call browser_snapshot again');
    if (element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true' || element.closest('[inert]')) throw new Error('Element is disabled');
    return element;
  }

  function requireHuman(element) {
    if (element.matches('input[type=file]')) throw new Error('File uploads require human takeover');
    const area = element.closest('[id*="captcha" i],[class*="captcha" i],[id*="turnstile" i],[class*="turnstile" i]');
    if (area) throw new Error('Visible verification requires human takeover');
  }

  function edit(element, value) {
    if (element.readOnly) throw new Error('Element is read-only');
    if (element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement) {
      const prototype = element instanceof HTMLInputElement ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
      Object.getOwnPropertyDescriptor(prototype, 'value').set.call(element, value);
    } else if (element.isContentEditable) element.textContent = value;
    else throw new Error('Element cannot accept text');
    element.dispatchEvent(new Event('input', { bubbles: true }));
    element.dispatchEvent(new Event('change', { bubbles: true }));
  }

  async function execute(args, expires) {
    const [action, ref, value] = args;
    if (action === 'reset') { references.clear(); generation++; return {}; }
    if (Date.now() >= expires) throw new Error('Expired browser command');
    if (action === 'ready') return { ready: document.readyState !== 'loading' };
    if (action === 'snapshot') return snapshot(args);
    if (action === 'get' && ref === 'text') {
      const element = document.querySelector(value || 'body');
      if (!element) throw new Error('Text selector has no match');
      return { text: element.innerText.slice(0, 400000) };
    }
    if (action === 'scroll') {
      const amount = Math.min(Number(value) || 600, 10000);
      const directions = { up: [0, -amount], down: [0, amount], left: [-amount, 0], right: [amount, 0] };
      if (!directions[ref]) throw new Error('Invalid scroll direction');
      window.scrollBy(...directions[ref]);
      return {};
    }
    if (action === 'wait') {
      while (Date.now() < expires) {
        if (document.querySelector(ref)) return {};
        await new Promise(resolve => setTimeout(resolve, 100));
      }
      throw new Error('Wait timed out');
    }
    if (!['click', 'fill', 'type', 'select', 'check', 'uncheck'].includes(action)) throw new Error('This action requires human takeover in Extension mode');
    const element = target(ref);
    requireHuman(element);
    element.scrollIntoView({ block: 'center', inline: 'nearest' });
    element.focus();
    if (Date.now() >= expires) throw new Error('Expired browser command');
    if (action === 'click') element.click();
    if (action === 'fill' || action === 'type') edit(element, action === 'type' ? (element.value || element.textContent || '') + value : value);
    if (action === 'select') {
      if (!(element instanceof HTMLSelectElement)) throw new Error('Element is not a select control');
      if (!Array.from(element.options).some(option => option.value === value)) throw new Error('Select option does not exist');
      element.value = value;
      element.dispatchEvent(new Event('input', { bubbles: true }));
      element.dispatchEvent(new Event('change', { bubbles: true }));
    }
    if (action === 'check' || action === 'uncheck') {
      if (!(element instanceof HTMLInputElement) || !['checkbox', 'radio'].includes(element.type)) throw new Error('Control needs human takeover');
      if (element.checked !== (action === 'check')) element.click();
    }
    return {};
  }

  chrome.runtime.onMessage.addListener((message, sender, reply) => {
    // Only our extension worker can issue commands. No window.postMessage bridge.
    if (sender.id !== chrome.runtime.id || sender.tab || !message?.agentRuntime) return;
    execute(message.args, message.expires).then(data => reply({ success: true, data }), error => reply({ success: false, error: error.message }));
    return true;
  });
})();
