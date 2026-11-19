import { ApiError, setLogger } from './api.js';

// ── DOM helpers ───────────────────────────────────────────────────────────────

// appRoot is the page's #app element, looked up on each call (never cached
// at load) so a fresh document is always the one drawn into.
export const appRoot = () => document.getElementById('app');

export function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'disabled') {
      node.disabled = Boolean(v);
      if (v) node.setAttribute('disabled', '');
      else delete node.attributes['disabled'];
    }
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (k in node && typeof v !== 'string') node[k] = v;
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

export function card(title, ...body) {
  return el('div', { class: 'card bg-base-100 shadow' },
    el('div', { class: 'card-body gap-3' }, title && el('h2', { class: 'card-title', text: title }), ...body));
}

export function alertBox(kind, message) {
  return el('div', { role: 'alert', class: `alert alert-${kind} text-sm` }, el('span', { text: message }));
}

// form builds a form from field specs and calls onSubmit(values). Errors from
// the API are shown above the form, with per-field messages under each input.
// A 'radio' field is a group of options ([value, label]) whose value is the
// checked one; a 'note' field is a line of help text with no input. Fields
// sharing a `group` are shown together in a fieldset with that legend, and
// `show(values)` hides a field while it returns false.
export function form({ fields, submit, onSubmit, inline = false }) {
  const $error = el('div');
  const inputs = {};
  const hints = {};
  const radioGroups = {};
  const multi = new Set(fields.filter((f) => f.type === 'multiselect').map((f) => f.name));
  const fieldRows = fields.map((f) => {
    if (f.type === 'note') return { f, $row: el('p', { class: 'text-xs opacity-70', text: f.text }) };
    if (f.type === 'radio') {
      const radios = f.options.map(([value]) => el('input', {
        type: 'radio', name: f.name, value, class: 'radio radio-sm', checked: value === f.value,
        onclick: (e) => { for (const r of radios) r.checked = r === e.currentTarget; updateVisibility(); },
        onchange: () => updateVisibility(),
      }));
      radioGroups[f.name] = radios;
      return {
        f,
        $row: el('div', { role: 'radiogroup', class: 'flex flex-wrap gap-4' },
          f.options.map(([, label], i) => el('label', { class: 'label cursor-pointer justify-start gap-2' }, radios[i], el('span', { text: label })))),
      };
    }
    let input;
    if (f.type === 'select') {
      input = el('select', { name: f.name, class: 'select select-bordered w-full' },
        f.options.map(([value, label]) => el('option', { value, text: label, selected: value === (f.value ?? '') })));
    } else if (f.type === 'multiselect') {
      // value is the array of selected option values.
      const chosen = f.value || [];
      input = el('select', { name: f.name, multiple: true, class: 'select select-bordered w-full' },
        f.options.map(([value, label]) => el('option', { value, text: label, selected: chosen.includes(value) })));
    } else if (f.type === 'textarea') {
      input = el('textarea', { name: f.name, class: 'textarea textarea-bordered w-full', rows: 2, text: f.value ?? '' });
    } else if (f.type === 'checkbox') {
      input = el('input', {
        type: 'checkbox', name: f.name, class: 'checkbox', checked: !!f.value,
        onclick: () => {
          if (typeof window !== 'undefined' && typeof window.HTMLInputElement === 'undefined') {
            input.checked = !input.checked;
          }
          updateVisibility();
        },
        onchange: () => updateVisibility(),
      });
    } else {
      input = el('input', {
        type: f.type || 'text', name: f.name, class: 'input input-bordered w-full',
        required: f.required, value: f.value ?? '', placeholder: f.placeholder, autocomplete: f.autocomplete,
      });
    }
    inputs[f.name] = input;
    hints[f.name] = el('p', { class: 'text-error text-xs mt-1' });
    let $row;
    if (f.type === 'checkbox') {
      $row = el('label', { class: 'label cursor-pointer justify-start gap-2' }, input, el('span', { text: f.label }));
    } else {
      $row = el('label', { class: inline ? 'flex-1 min-w-32' : 'block' },
        el('span', { class: 'text-sm', text: f.label }), input, hints[f.name]);
    }
    return { f, $row };
  });

  const getValues = () => {
    const values = {};
    for (const [name, input] of Object.entries(inputs)) {
      if (input.type === 'checkbox') values[name] = input.checked;
      else if (multi.has(name)) values[name] = Array.from(input.children).filter((o) => o.selected).map((o) => o.value);
      else values[name] = input.value;
    }
    for (const [name, radios] of Object.entries(radioGroups)) values[name] = radios.find((r) => r.checked)?.value ?? '';
    return values;
  };

  // layout is the form's rows for these values: hidden fields left out, and
  // grouped fields gathered into their fieldset where the group first appears.
  const groups = {};
  const layout = (values) => {
    const top = [];
    for (const { f, $row } of fieldRows) {
      if (f.show && !f.show(values)) continue;
      if (!f.group) { top.push($row); continue; }
      const $set = (groups[f.group] ||= el('fieldset', { class: 'fieldset w-full border border-base-300 rounded p-2' }));
      if (!top.includes($set)) {
        $set.replaceChildren(el('legend', { class: 'fieldset-legend text-sm', text: f.group }));
        top.push($set);
      }
      $set.append($row);
    }
    return top;
  };

  const updateVisibility = () => {
    $form.replaceChildren($error, ...layout(getValues()), $submit);
  };

  const initialValues = {};
  for (const f of fields) {
    if (f.type === 'checkbox') initialValues[f.name] = !!f.value;
    else initialValues[f.name] = f.value ?? '';
  }
  const visibleRows = layout(initialValues);

  const $submit = el('button', { type: 'submit', class: 'btn btn-primary', text: submit });
  const $form = el('form', {
    class: inline ? 'flex flex-wrap items-end gap-2' : 'space-y-3',
    onsubmit: async (e) => {
      e.preventDefault();
      $error.replaceChildren();
      Object.values(hints).forEach((h) => { h.textContent = ''; });
      const values = getValues();
      $submit.disabled = true;
      try {
        await onSubmit(values, $form);
      } catch (err) {
        showError($error, err, hints);
      } finally {
        $submit.disabled = false;
      }
    },
  }, $error, ...visibleRows, $submit);
  return $form;
}

export function showError($target, err, hints = {}) {
  if (!(err instanceof ApiError)) {
    console.error(err);
    $target.replaceChildren(alertBox('error', String(err)));
    return;
  }
  for (const [field, msg] of Object.entries(err.fields)) {
    if (hints[field]) hints[field].textContent = msg;
  }
  const unplaced = Object.entries(err.fields).filter(([f]) => !hints[f]).map(([f, m]) => `${f} ${m}`);
  $target.replaceChildren(alertBox('error', [err.message, ...unplaced].join(' ')));
}

// action wraps a click handler so API errors surface instead of vanishing.
export function action(fn, $errorTarget) {
  return async (e) => {
    e.preventDefault();
    try {
      await fn(e);
    } catch (err) {
      if ($errorTarget) showError($errorTarget, err);
      else toast(err.message || String(err), 'error');
    }
  };
}

export function toast(message, kind = 'info') {
  const t = el('div', { class: 'toast toast-top toast-center z-50' }, alertBox(kind, message));
  document.body.append(t);
  setTimeout(() => t.remove(), 4000);
  return t;
}

export const fmtDate = (iso) => new Date(iso).toLocaleString();

// ── API log panel ─────────────────────────────────────────────────────────────

const SECRET_KEYS = /password|token|^code$/i;
const redact = (v) => JSON.stringify(v, (k, val) => (k && SECRET_KEYS.test(k) && typeof val === 'string' ? '•••' : val), 2);

// installApiLog shows every API call in the log panel (secrets redacted) and
// wires its Clear button. app.js calls it once per load.
export function installApiLog() {
  const $log = document.getElementById('log');
  setLogger(({ method, path, status, ms, request, response }) => {
    const ok = status >= 200 && status < 300;
    const entry = el('li', { class: 'border-b border-base-300 pb-2' },
      el('details', {},
        el('summary', { class: 'cursor-pointer font-mono' },
          el('span', { class: `badge badge-sm mr-1 ${ok ? 'badge-success' : 'badge-error'}`, text: status || 'ERR' }),
          `${method} ${path} `, el('span', { class: 'opacity-50', text: `${ms}ms` })),
        request !== undefined && el('pre', { class: 'bg-base-200 p-2 mt-1 overflow-x-auto', text: '→ ' + redact(request) }),
        response !== null && response !== undefined && el('pre', { class: 'bg-base-200 p-2 mt-1 overflow-x-auto', text: '← ' + redact(response) })));
    $log.prepend(entry);
    while ($log.children.length > 50) $log.lastChild.remove();
  });
  document.getElementById('log-clear').addEventListener('click', () => $log.replaceChildren());
}
