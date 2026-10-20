(() => {
  let references = new Map();
  let generation = 0;
  let hovered;
  const documents = new WeakMap();
  const text = value => String(value || '').replace(/\s+/g, ' ').trim();
  const selector = 'a[href],button,input:not([type=hidden]),textarea,select,[role=button],[role=link],[role=checkbox],[role=combobox],[role=menuitem],[role=option],[role=radio],[role=switch],[contenteditable=true],[tabindex]:not([tabindex="-1"])';
  const isReference = value => /^@?e(?:[0-9a-f]+_\d+_\d+|\d+)$/.test(value || '');
  const visible = element => element.getClientRects().length > 0 && element.checkVisibility({ opacityProperty: true, visibilityProperty: true });

  function requireHuman(element) {
    if (element.matches('input[type=file]')) throw new Error('File uploads require human takeover');
    if (element.closest('[id*="captcha" i],[class*="captcha" i],[id*="turnstile" i],[class*="turnstile" i]')) throw new Error('Visible verification requires human takeover');
  }

  function validateTree(element, mutation = false) {
    if (!element?.isConnected || !visible(element)) throw new Error('Stale element reference; call browser_snapshot again');
    let current = element;
    while (current) {
      if (mutation) {
        requireHuman(current);
        if (current.matches(':disabled') || current.getAttribute('aria-disabled') === 'true' || current.closest('[inert]')) throw new Error('Element is disabled');
      }
      const owner = current.ownerDocument;
      if (owner === document) return;
      const frame = owner.defaultView?.frameElement;
      if (!frame || !frame.isConnected || frame.contentDocument !== owner || !visible(frame)) throw new Error('Stale or hidden frame; call browser_snapshot again');
      current = frame;
    }
    throw new Error('Stale frame; call browser_snapshot again');
  }

  function target(ref, mutation = false) {
    const entry = references.get(ref.startsWith('@') ? ref : `@${ref}`);
    if (!entry || entry.element.ownerDocument !== entry.document) throw new Error('Stale element reference; call browser_snapshot again');
    validateTree(entry.element, mutation);
    return entry.element;
  }

  function snapshot(args) {
    const scope = args.includes('-s') ? args[args.indexOf('-s') + 1] : 'body';
    const root = isReference(scope) ? target(scope) : document.querySelector(scope);
    if (!root) throw new Error('Snapshot selector has no match');
    references = new Map();
    generation++;
    const lines = [];
    let frames = 0;
    function reference(element) {
      const owner = element.ownerDocument;
      if (!documents.has(owner)) documents.set(owner, Array.from(crypto.getRandomValues(new Uint32Array(2)), n => n.toString(16)).join(''));
      const ref = `e${documents.get(owner)}_${generation}_${references.size + 1}`;
      references.set(`@${ref}`, { element, document: owner });
      return ref;
    }
    function collect(root, depth, framed) {
      if (framed) lines.push(`- frame ${JSON.stringify(text(root.ownerDocument.title))} origin=${new URL(root.ownerDocument.URL).origin} [ref=${reference(root)}]`);
      const elements = [...(root.matches(selector) ? [root] : []), ...root.querySelectorAll(selector)];
      for (const element of elements) {
        if (!visible(element)) continue;
        if (references.size >= 300) { lines.push('[Element limit reached; scope a snapshot to a frame reference]'); break; }
        const owner = element.ownerDocument;
        const labelledBy = (element.getAttribute('aria-labelledby') || '').split(/\s+/).map(id => owner.getElementById(id)?.textContent || '').join(' ');
        const label = text(labelledBy || element.getAttribute('aria-label') || (element.labels && Array.from(element.labels).map(label => label.innerText).join(' ')) || element.innerText || element.getAttribute('placeholder') || element.getAttribute('title') || element.querySelector('img[alt]')?.getAttribute('alt')).slice(0, 180);
        const role = element.getAttribute('role') || element.tagName.toLowerCase();
        lines.push(`- ${role} ${JSON.stringify(label)} [ref=${reference(element)}]${element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true' ? ' [disabled]' : ''}`);
      }
      for (const frame of root.querySelectorAll('iframe,frame')) {
        if (!visible(frame)) continue;
        if (++frames > 16 || depth >= 4 || references.size >= 300) { lines.push('[Frame limit reached; additional frames unavailable]'); break; }
        try {
          validateTree(frame, true);
          const child = frame.contentDocument;
          // shortcut: same-origin frames only; extend after verifying cross-origin parent ownership and visibility without debugger access.
          if (!child?.body || new URL(child.URL).origin !== new URL(document.URL).origin) { lines.push('[Cross-origin or inaccessible frame: human takeover required]'); continue; }
          collect(child.body, depth + 1, true);
        } catch { lines.push('[Hidden, disabled, or verification frame: human takeover required]'); }
      }
    }
    collect(root, 0, false);
    return { snapshot: lines.join('\n') || '[No visible interactive elements]' };
  }

  function edit(element, value) {
    const view = element.ownerDocument.defaultView;
    if (element.readOnly) throw new Error('Element is read-only');
    if (element instanceof view.HTMLInputElement || element instanceof view.HTMLTextAreaElement) {
      const prototype = element instanceof view.HTMLInputElement ? view.HTMLInputElement.prototype : view.HTMLTextAreaElement.prototype;
      Object.getOwnPropertyDescriptor(prototype, 'value').set.call(element, value);
    } else if (element.isContentEditable) element.textContent = value;
    else throw new Error('Element cannot accept text');
    element.dispatchEvent(new view.Event('input', { bubbles: true, composed: true }));
    element.dispatchEvent(new view.Event('change', { bubbles: true }));
  }

  function hover(element) {
    const chain = node => { const result = []; for (; node; node = node.parentElement) result.push(node); return result; };
    let old = hovered;
    try { validateTree(old, true); } catch { old = null; }
    const before = chain(old), after = chain(element);
    function send(node, type, relatedTarget, bubbles) {
      const view = node.ownerDocument.defaultView;
      const box = node.getBoundingClientRect();
      const Event = type.startsWith('pointer') ? view.PointerEvent : view.MouseEvent;
      node.dispatchEvent(new Event(type, { bubbles, composed: true, relatedTarget: relatedTarget?.ownerDocument === node.ownerDocument ? relatedTarget : null, clientX: box.x + box.width / 2, clientY: box.y + box.height / 2, pointerId: 1, pointerType: 'mouse', isPrimary: true }));
    }
    if (old !== element) {
      if (old) { send(old, 'pointerout', element, true); send(old, 'mouseout', element, true); }
      for (const node of before.filter(node => !after.includes(node))) { send(node, 'pointerleave', element, false); send(node, 'mouseleave', element, false); }
      send(element, 'pointerover', old, true); send(element, 'mouseover', old, true);
      for (const node of after.filter(node => !before.includes(node)).reverse()) { send(node, 'pointerenter', old, false); send(node, 'mouseenter', old, false); }
    }
    send(element, 'pointermove', old, true); send(element, 'mousemove', old, true);
    hovered = element;
    return { effect: 'events_dispatched', limitation: 'DOM hover events only; CSS hover and trusted pointer input require human takeover' };
  }

  async function execute(args, expires) {
    const [action, ref, value] = args;
    if (action === 'reset') { references.clear(); hovered = undefined; generation++; return {}; }
    if (Date.now() >= expires) throw new Error('Expired browser command');
    if (action === 'ready') return { ready: document.readyState !== 'loading' };
    if (action === 'snapshot') return snapshot(args);
    if (action === 'get' && ref === 'text') {
      const element = isReference(value) ? target(value) : document.querySelector(value || 'body');
      if (!element) throw new Error('Text selector has no match');
      return { text: element.innerText.slice(0, 400000) };
    }
    if (action === 'scroll') {
      const amount = Math.min(Number(value) || 600, 10000);
      const directions = { up: [0, -amount], down: [0, amount], left: [-amount, 0], right: [amount, 0] };
      if (!directions[ref]) throw new Error('Invalid scroll direction');
      const element = args[3] ? target(args[3], true) : document.body;
      const owner = element.ownerDocument;
      const scrollable = element === owner.body || element === owner.documentElement ? owner.defaultView : element;
      scrollable.scrollBy(...directions[ref]);
      return { effect: 'scroll_dispatched' };
    }
    if (action === 'wait') {
      while (Date.now() < expires) {
        if (document.querySelector(ref)) return {};
        await new Promise(resolve => setTimeout(resolve, 100));
      }
      throw new Error('Wait timed out');
    }
    if (!['click', 'fill', 'type', 'select', 'check', 'uncheck', 'press', 'hover'].includes(action)) throw new Error('This action requires human takeover in Extension mode');
    const element = target(action === 'press' ? value : ref, true);
    const view = element.ownerDocument.defaultView;
    if (element.matches('body,html') || element instanceof view.HTMLIFrameElement) throw new Error('Frame references support reading, snapshots, and scrolling; use a control reference for other actions');
    element.scrollIntoView({ block: 'center', inline: 'nearest' });
    if (action === 'hover') {
      const result = hover(element);
      await new Promise(resolve => setTimeout(resolve, Math.min(50, Math.max(0, expires - Date.now()))));
      return result;
    }
    if (action === 'press') return runtimeExtensionPress(element, ref, expires, () => validateTree(element, true));
    element.focus();
    validateTree(element, true);
    if (Date.now() >= expires) throw new Error('Expired browser command');
    if (action === 'click') element.click();
    if (action === 'fill' || action === 'type') edit(element, action === 'type' ? (element.value ?? element.textContent ?? '') + value : value);
    if (action === 'select') {
      if (!(element instanceof view.HTMLSelectElement)) throw new Error('Element is not a select control');
      if (!Array.from(element.options).some(option => option.value === value)) throw new Error('Select option does not exist');
      element.value = value;
      element.dispatchEvent(new view.Event('input', { bubbles: true }));
      element.dispatchEvent(new view.Event('change', { bubbles: true }));
    }
    if (action === 'check' || action === 'uncheck') {
      if (!(element instanceof view.HTMLInputElement) || !['checkbox', 'radio'].includes(element.type)) throw new Error('Control needs human takeover');
      const checked = action === 'check';
      if (element.checked !== checked) {
        if (element.type === 'radio' && !checked) throw new Error('Radio buttons cannot be unchecked directly; select another option in the group');
        element.click();
        if (element.checked !== checked) throw new Error('Control did not change to the requested state; inspect the page before retrying');
      }
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
