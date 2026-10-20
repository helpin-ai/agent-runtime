// DOM keyboard defaults are deliberately limited; these events are not trusted input.
globalThis.runtimeExtensionPress = (element, combination, expires, validate) => {
  const view = element.ownerDocument.defaultView;
  const document = element.ownerDocument;
  const parts = combination.split('+');
  let key = parts.pop();
  const modifiers = parts.map(part => part.toLowerCase());
  if (new Set(modifiers).size !== modifiers.length || modifiers.some(part => !['shift', 'control', 'ctrl', 'meta', 'cmd'].includes(part))) throw new Error('Unsupported keyboard shortcut; use human takeover');
  const shiftKey = modifiers.includes('shift');
  const ctrlKey = modifiers.includes('control') || modifiers.includes('ctrl');
  const metaKey = modifiers.includes('meta') || modifiers.includes('cmd');
  const selectAll = (ctrlKey !== metaKey) && !shiftKey && key.toLowerCase() === 'a';
  if ((ctrlKey || metaKey) && !selectAll) throw new Error('Unsupported keyboard shortcut; use human takeover');
  if (key === 'Space') key = ' ';
  const editing = element instanceof view.HTMLTextAreaElement || (element instanceof view.HTMLInputElement && ['text', 'search', 'url', 'tel', 'password'].includes(element.type));
  const movement = ['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(key);
  const deletion = ['Backspace', 'Delete'].includes(key);
  const insertion = Array.from(key).length === 1 && !ctrlKey && !metaKey;
  if (!selectAll && !movement && !deletion && !insertion && !['Enter', 'Escape', 'Tab', 'ArrowUp', 'ArrowDown'].includes(key)) throw new Error('Unsupported key; use human takeover');
  if (shiftKey && !movement && key !== 'Tab') throw new Error('Unsupported Shift combination; use human takeover');
  const button = element instanceof view.HTMLButtonElement || (element instanceof view.HTMLInputElement && ['button', 'submit', 'reset', 'checkbox', 'radio'].includes(element.type));
  const activates = (key === 'Enter' || key === ' ') && button;
  const entersForm = key === 'Enter' && element instanceof view.HTMLInputElement && !!element.form && !button;
  if ((selectAll || movement || deletion || (insertion && !activates)) && !editing) throw new Error('Text keyboard editing requires a supported input or textarea; use human takeover');
  if ((deletion || (insertion && !activates) || (key === 'Enter' && editing)) && element.readOnly) throw new Error('Element is read-only');
  if (key === 'Enter' && !editing && !activates && !entersForm) throw new Error('Enter default is unsupported for this control; use human takeover');
  if (key === 'Enter' && editing && !(element instanceof view.HTMLTextAreaElement) && !entersForm) throw new Error('Input has no form to submit; use human takeover');

  let nextFocus;
  if (key === 'Tab') {
    const visible = Array.from(document.querySelectorAll('a[href],button,input,textarea,select,iframe,[tabindex],[contenteditable=true]'))
      .filter(node => node.tabIndex >= 0 && !node.matches(':disabled,input[type=hidden]') && !node.closest('[inert]') && node.getClientRects().length && node.checkVisibility({ opacityProperty: true, visibilityProperty: true }));
    const candidates = visible.filter(node => {
      if (!(node instanceof view.HTMLInputElement) || node.type !== 'radio' || !node.name) return true;
      const group = visible.filter(other => other instanceof view.HTMLInputElement && other.type === 'radio' && other.name === node.name && other.form === node.form);
      return node === (group.find(other => other.checked) || group[0]);
    })
      .sort((a, b) => (a.tabIndex || Infinity) - (b.tabIndex || Infinity));
    nextFocus = candidates[candidates.indexOf(element) + (shiftKey ? -1 : 1)];
    if (!candidates.includes(element) || !nextFocus || nextFocus.matches('iframe')) throw new Error('Tab crosses an unsupported focus boundary; use a fresh target reference or human takeover');
  }
  let submitter;
  if (entersForm) {
    submitter = Array.from(element.form.elements).find(node => (node instanceof view.HTMLButtonElement && node.type === 'submit') || (node instanceof view.HTMLInputElement && ['submit', 'image'].includes(node.type)));
    if (submitter?.matches(':disabled') || submitter?.type === 'image') throw new Error('Default form submitter is unsupported or disabled; inspect the form');
    if (!submitter && Array.from(element.form.elements).filter(node => node instanceof view.HTMLInputElement && ['text', 'search', 'url', 'tel', 'email', 'password', 'number', 'date', 'time', 'month', 'week', 'datetime-local'].includes(node.type)).length > 1) throw new Error('Form has no unambiguous implicit submit action; use human takeover');
  }
  const code = /^[a-z]$/i.test(key) ? `Key${key.toUpperCase()}` : key === ' ' ? 'Space' : key;
  const keyCode = ({Backspace:8,Tab:9,Enter:13,Escape:27,' ':32,End:35,Home:36,ArrowLeft:37,ArrowUp:38,ArrowRight:39,ArrowDown:40,Delete:46})[key] || (/^[a-z]$/i.test(key) ? key.toUpperCase().charCodeAt(0) : 0);
  const event = type => new view.KeyboardEvent(type, { key, code, keyCode, which:keyCode, shiftKey, ctrlKey, metaKey, bubbles: true, cancelable: true, composed: true });
  element.focus();
  if (document.activeElement !== element) throw new Error('Keyboard target could not receive focus');
  if (Date.now() >= expires) throw new Error('Expired browser command');
  const permitted = element.dispatchEvent(event('keydown'));
  let effect = permitted ? 'events_dispatched' : 'cancelled';
  try {
    if (!permitted || !element.isConnected || Date.now() >= expires) return { effect };
    validate();
    if (key === 'Tab') {
      nextFocus.focus();
      if (document.activeElement !== nextFocus) throw new Error('Tab target could not receive focus');
      effect = 'focus_changed';
    } else if (activates) {
      element.click();
      effect = 'activation_dispatched';
    } else if (entersForm) {
      view.HTMLFormElement.prototype.requestSubmit.call(element.form, submitter);
      effect = 'activation_dispatched';
    } else if (editing) {
      const value = element.value;
      let start = element.selectionStart, end = element.selectionEnd;
      const direction = element.selectionDirection;
      const boundaries = [0, ...Array.from(new view.Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(value), item => item.index + item.segment.length)];
      const previous = position => boundaries.filter(boundary => boundary < position).at(-1) ?? 0;
      const next = position => boundaries.find(boundary => boundary > position) ?? value.length;
      if (selectAll) {
        element.setSelectionRange(0, value.length);
        effect = 'selection_changed';
      } else if (movement) {
        const focus = direction === 'backward' ? start : end;
        let position;
        if (key === 'Home') position = value.slice(0, focus).lastIndexOf('\n') + 1;
        if (key === 'End') { position = value.indexOf('\n', focus); if (position < 0) position = value.length; }
        if (key === 'ArrowLeft') position = !shiftKey && start !== end ? start : previous(focus);
        if (key === 'ArrowRight') position = !shiftKey && start !== end ? end : next(focus);
        const anchor = direction === 'backward' ? end : start;
        element.setSelectionRange(shiftKey ? Math.min(anchor, position) : position, shiftKey ? Math.max(anchor, position) : position, shiftKey && position < anchor ? 'backward' : 'forward');
        effect = 'selection_changed';
      } else if (deletion || insertion || key === 'Enter') {
        if (element.readOnly) throw new Error('Element is read-only');
        if (deletion && start === end) { if (key === 'Backspace') start = previous(start); else end = next(end); }
        const data = deletion ? '' : key === 'Enter' ? '\n' : key;
        const inputType = deletion ? (key === 'Backspace' ? 'deleteContentBackward' : 'deleteContentForward') : key === 'Enter' ? 'insertLineBreak' : 'insertText';
        if (!element.dispatchEvent(new view.InputEvent('beforeinput', { bubbles: true, composed: true, cancelable: true, inputType, data: deletion ? null : data }))) return { effect: 'cancelled' };
        if (!element.isConnected || Date.now() >= expires) return { effect: 'events_dispatched' };
        validate();
        if (element.readOnly || element.value !== value) throw new Error('Input changed during the keyboard event; inspect the field before retrying');
        const replacement = value.slice(0, start) + data + value.slice(end);
        if (element.maxLength >= 0 && replacement.length > element.maxLength && replacement.length > value.length) throw new Error('Keyboard edit would exceed the field maxlength');
        if (replacement === value) return { effect: 'events_dispatched' };
        const prototype = element instanceof view.HTMLInputElement ? view.HTMLInputElement.prototype : view.HTMLTextAreaElement.prototype;
        Object.getOwnPropertyDescriptor(prototype, 'value').set.call(element, replacement);
        element.setSelectionRange(start + data.length, start + data.length);
        element.dispatchEvent(new view.InputEvent('input', { bubbles: true, composed: true, inputType, data: deletion ? null : data }));
        effect = 'value_changed';
      }
    }
    return { effect };
  } finally { if (element.isConnected && Date.now() < expires) element.dispatchEvent(event('keyup')); }
};
