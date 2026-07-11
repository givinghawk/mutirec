let state = null;
let config = null;
let watchPlayer = null;
let recPlayer = null;
let recordings = [];
let lolEvents = [];
let selectedTimetableDay = null;
let editingSet = null;
let nowPlayingId = null;
let highlightSourceId = null;
let playerPreviousView = 'recordings';
let evCurrentFestivalId = null;
let evEditingOrgId = null;
let evEditingFestId = null;
const accents = { red: '#ef4444', cyan: '#06b6d4', lime: '#84cc16', amber: '#f59e0b', pink: '#ec4899' };

// Registers the PWA service worker so the app can be installed to a phone's
// home screen. The worker only caches the static app shell (see sw.js) -
// live state, recordings, and API calls always go straight to the network.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}

async function api(path, opts) {
  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    toast(`Network error: ${err.message}`, 'error');
    throw err;
  }
  if (res.status === 401) {
    window.location.href = `/login?next=${encodeURIComponent(window.location.pathname)}`;
    throw new Error('session expired');
  }
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    toast(text || `Request failed (${res.status})`, 'error');
    throw new Error(text || `Request failed (${res.status})`);
  }
  return res.json();
}

function $(id) { return document.getElementById(id); }

// Shows/hides the preview image and remove button for one image-upload-field
// based on the current value of its hidden input - called whenever an editor
// modal is (re)populated so re-opening it reflects what's actually saved.
function syncImageUploadPreview(fieldId) {
  const url = $(fieldId).value;
  const preview = $(`${fieldId}-preview`);
  const removeBtn = document.querySelector(`[data-remove-btn="${fieldId}"]`);
  if (url) {
    preview.src = url;
    preview.classList.remove('hidden');
    if (removeBtn) removeBtn.classList.remove('hidden');
  } else {
    preview.classList.add('hidden');
    preview.removeAttribute('src');
    if (removeBtn) removeBtn.classList.add('hidden');
  }
}

// Wires every "image URL -> file upload" field on the page (app logo,
// Organisation/Festival logo, Event cover): clicking Upload opens a file
// picker, the chosen file is POSTed to /api/uploads/image, and the returned
// content-addressed URL is stashed in the field's hidden input. Bound once at
// load - the fields themselves are static parts of the page, not re-rendered.
function setupImageUploadFields() {
  document.querySelectorAll('[data-image-field]').forEach(container => {
    const fieldId = container.dataset.imageField;
    const fileInput = container.querySelector(`[data-file-input="${fieldId}"]`);
    const uploadBtn = container.querySelector(`[data-upload-btn="${fieldId}"]`);
    const removeBtn = container.querySelector(`[data-remove-btn="${fieldId}"]`);
    uploadBtn.onclick = () => fileInput.click();
    removeBtn.onclick = () => {
      $(fieldId).value = '';
      syncImageUploadPreview(fieldId);
    };
    fileInput.onchange = async () => {
      const file = fileInput.files[0];
      fileInput.value = '';
      if (!file) return;
      const form = new FormData();
      form.append('image', file);
      let res;
      try {
        res = await api('/api/uploads/image', { method: 'POST', body: form });
      } catch {
        return;
      }
      $(fieldId).value = res.url;
      syncImageUploadPreview(fieldId);
    };
  });
}

function toast(message, level) {
  const el = document.createElement('div');
  el.className = `toast toast-${level || 'info'}`;
  el.textContent = message;
  $('toasts').appendChild(el);
  setTimeout(() => el.classList.add('toast-out'), 4000);
  setTimeout(() => el.remove(), 4500);
}

async function refresh() {
  try {
    state = await api('/api/state');
  } catch (err) {
    return;
  }
  config = state.config;
  if (!config) return;
  // A rendering bug in any one panel should never take down the rest of the
  // UI silently - log it and keep whatever else already rendered visible.
  try {
    applyTheme();
    applyRoleVisibility();
    renderVersion();
    renderDashboard();
    renderEditors();
    updateWatchIfActive();
    maybeStartOnboarding();
    maybeShowDiscordLinkResult();
  } catch (err) {
    console.error('Render failed:', err);
    toast(`UI failed to render: ${err.message}`, 'error');
  }
}

// Hides admin-only controls (source/settings management, the Users list,
// entire nav tabs that are all-mutation) for a viewer role. This is UX only
// - the real boundary is server-side (rbacAllowed in auth.go); a viewer who
// forced one of these open would just get a 403 from the API.
function applyRoleVisibility() {
  const isAdmin = state.role === 'admin';
  document.querySelectorAll('[data-admin-only]').forEach(el => el.classList.toggle('hidden', !isAdmin));
  ['sources', 'diagnostics', 'events-tab', 'explorer'].forEach(id => {
    const btn = document.querySelector(`.nav[data-view="${id}"]`);
    if (btn) btn.classList.toggle('hidden', !isAdmin);
  });
}

// The Discord OAuth callback redirects back here with ?discordLinked=1 or
// ?discordError=... (see handleDiscordCallback) - surface it once as a toast
// and strip the query param so a later manual refresh doesn't repeat it.
let discordLinkResultHandled = false;
function maybeShowDiscordLinkResult() {
  if (discordLinkResultHandled) return;
  discordLinkResultHandled = true;
  const params = new URLSearchParams(location.search);
  const linked = params.get('discordLinked');
  const err = params.get('discordError');
  if (!linked && !err) return;
  history.replaceState(null, '', location.pathname);
  if (linked) toast('Discord account linked', 'info');
  else toast(`Discord link failed (${err})`, 'error');
}

// The first-run setup wizard (setup.html) redirects here with ?onboarding=1
// right after account creation, so a brand new install lands straight in
// the Quick Add Source wizard instead of an empty dashboard. Runs at most
// once per page load, and strips the query param so a later manual refresh
// doesn't reopen it.
let onboardingHandled = false;
function maybeStartOnboarding() {
  if (onboardingHandled) return;
  onboardingHandled = true;
  if (new URLSearchParams(location.search).get('onboarding') !== '1') return;
  history.replaceState(null, '', location.pathname);
  switchToView('sources');
  openWizard();
}

function renderVersion() {
  const v = state.version || 'dev';
  $('version-footer').textContent = v;
  $('version-footer').title = `MutiRec ${v}`;
  const helpVersion = $('help-version');
  if (helpVersion) helpVersion.textContent = v;
}

function applyTheme() {
  document.body.className = `min-h-screen text-zinc-100 theme-${config.ui.theme || 'midnight'} bg-zinc-950`;
  $('app-name').textContent = config.ui.appName || 'MutiRec';
  $('custom-css').textContent = config.ui.customCss || '';
  if (config.ui.logoUrl) {
    $('logo').src = config.ui.logoUrl;
    $('logo').classList.remove('hidden');
  } else {
    $('logo').classList.add('hidden');
  }

  if (config.ui.themeColors) {
    applyThemeColors(config.ui.themeColors);
  } else {
    document.documentElement.style.setProperty('--accent', accents[config.ui.accent] || accents.red);
  }
}

function retryCountdown(nextRetryAt) {
  const ms = new Date(nextRetryAt).getTime() - Date.now();
  const secs = Math.max(0, Math.ceil(ms / 1000));
  return secs >= 60 ? `${Math.ceil(secs / 60)}m` : `${secs}s`;
}

function elapsed(startedAt) {
  const start = new Date(startedAt).getTime();
  if (!start) return '';
  let secs = Math.max(0, Math.floor((Date.now() - start) / 1000));
  const h = Math.floor(secs / 3600); secs -= h * 3600;
  const m = Math.floor(secs / 60); secs -= m * 60;
  return `${h ? h + 'h ' : ''}${m}m ${secs}s`;
}

// openGroupIds tracks which source-group accordions are expanded, keyed by
// festival id ("" for Ungrouped), so the layout survives periodic re-renders.
let openGroupIds = new Set(['']);

function sourceCardHtml(src) {
  return `
    <article class="source-card ${src.id === nowPlayingId ? 'now-playing' : ''} ${src.orphaned ? 'orphaned' : ''}" style="border-left-color:${src.color || 'var(--accent)'}">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3>${escapeHtml(src.name)}</h3>
          <p class="text-sm text-zinc-400">${escapeHtml(src.type)} · ${escapeHtml(src.quality || 'best')} · ${escapeHtml(src.container || 'mkv')}</p>
        </div>
        <span class="pill status-${escapeHtml(src.status)}"><span class="status-dot dot-${escapeHtml(src.status)}"></span>${escapeHtml(src.status)}</span>
      </div>
      ${src.orphaned ? '<div class="mt-2 rounded border border-amber-400/30 bg-amber-500/10 px-2 py-1 text-xs text-amber-100">This source was deleted while still recording — stop it to free it up.</div>' : ''}
      <div class="mt-3 text-sm text-zinc-300">
        ${src.id === nowPlayingId ? '<div class="now-playing-tag">&#9654; Now watching</div>' : ''}
        ${!src.orphaned ? `<div>Now: ${escapeHtml(src.currentSet || 'No current set')}</div><div>Next: ${escapeHtml(src.nextSet || 'No upcoming set')}</div>` : ''}
        <div>Size: ${formatBytes(src.size || 0)}${src.status === 'recording' ? ` · Recording for ${elapsed(src.startedAt)}` : ''}</div>
        ${src.status === 'reconnecting' ? `<div class="text-amber-300">Stream appears down - retrying in ${retryCountdown(src.nextRetryAt)} (attempt ${src.reconnectAttempts})</div>` : ''}
        ${src.lastError ? `<div class="text-rose-300">Error: ${escapeHtml(src.lastError)}</div>` : ''}
      </div>
      <div class="mt-3 flex flex-wrap gap-2">
        ${src.orphaned ? '' : `<button class="btn" ${src.status === 'recording' ? 'disabled' : ''} onclick="start('${src.id}')">Record</button>`}
        <button class="btn" ${src.status !== 'recording' ? 'disabled' : ''} onclick="stopRec('${src.id}', '${escapeAttr(src.name)}')">Stop</button>
        ${!src.orphaned && src.liveRewind ? `<button class="btn primary" onclick="playLive('${src.id}')">${src.liveRewindActive ? 'Watch Live (rewind)' : 'Watch Live'}</button>` : ''}
        ${src.mediaPath ? `<button class="btn" onclick="openSourceLatest('${src.id}')">Open latest</button>` : ''}
      </div>
    </article>`;
}

// openSourceLatest plays a source's most recent finished file through the
// same in-app Video.js recordings player as everything else, rather than
// dumping the raw file into a new browser tab (which used the browser's
// native player and left the app). Looks the path up from live state at
// click time so no file path has to be escaped into the markup.
function openSourceLatest(id) {
  const src = (state.sources || []).find(s => s.id === id);
  if (src && src.mediaPath) openRecordingPlayer(src.mediaPath, src.name);
}

function toggleSourceGroup(id) {
  if (openGroupIds.has(id)) openGroupIds.delete(id); else openGroupIds.add(id);
  renderDashboard();
}

function renderDashboard() {
  const warnings = state.warnings || [];
  const sources = state.sources || [];
  const events = state.events || [];
  $('active-count').textContent = `${state.activeCount} active`;
  $('warnings').innerHTML = warnings.map(w => `<div class="rounded border border-amber-400/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-100">${escapeHtml(w)}</div>`).join('');

  if (!sources.length) {
    $('source-grid').innerHTML = '<div class="empty-state md:col-span-2"><div class="empty-icon">📡</div><div>No sources yet</div><button class="btn primary" onclick="goToView(\'sources\')">Add your first source</button></div>';
  } else {
    const festivals = config.festivals || [];
    const groups = new Map(); // festivalId ("" = ungrouped) -> { name, color, sources }
    sources.forEach(src => {
      const fid = src.festivalId || '';
      if (!groups.has(fid)) {
        const f = festivals.find(f => f.id === fid);
        groups.set(fid, { name: f ? f.name : 'Ungrouped', color: f ? f.color : null, sources: [] });
      }
      groups.get(fid).sources.push(src);
    });
    const ordered = [...groups.entries()].sort((a, b) => {
      if (a[0] === '') return 1;
      if (b[0] === '') return -1;
      return a[1].name.localeCompare(b[1].name);
    });
    $('source-grid').innerHTML = ordered.map(([gid, group]) => {
      const open = openGroupIds.has(gid);
      const recording = group.sources.filter(s => s.status === 'recording').length;
      return `
      <div class="source-group md:col-span-2 ${open ? 'open' : ''}">
        <div class="source-group-head" style="border-left-color:${group.color || 'var(--accent)'}" onclick="toggleSourceGroup('${escapeAttr(gid)}')">
          <span class="source-group-chevron">&#9656;</span>
          <span class="font-semibold">${escapeHtml(group.name)}</span>
          <span class="pill">${group.sources.length} source${group.sources.length === 1 ? '' : 's'}</span>
          ${recording ? `<span class="pill status-recording">${recording} recording</span>` : ''}
        </div>
        <div class="source-group-body ${open ? '' : 'hidden'} grid gap-3 md:grid-cols-2">
          ${group.sources.map(sourceCardHtml).join('')}
        </div>
      </div>`;
    }).join('');
  }

  const free = state.disk.volumeFree || 0;
  const total = state.disk.volumeTotal || 0;
  $('storage').innerHTML = `<div>Free: ${formatBytes(free)}</div><div>Total: ${formatBytes(total)}</div><div>Recorded: ${formatBytes(state.disk.total || 0)}</div>`;
  $('storage-forecast').textContent = forecastText(state.storageForecast);
  $('events').innerHTML = [...events].reverse().slice(0, 80).map(e => `<div class="event-${e.level}"><span class="text-zinc-500">${new Date(e.time).toLocaleTimeString()}</span> ${escapeHtml(e.text)}</div>`).join('');
  renderFavoritesPanel();
  renderDashboardCharts(sources);
}

function renderDashboardCharts(sources) {
  // Status breakdown: a simple proportional stacked bar, no chart library needed.
  const counts = { recording: 0, idle: 0, disabled: 0 };
  sources.forEach(s => { counts[s.status] = (counts[s.status] || 0) + 1; });
  const total = sources.length || 1;
  const segments = [
    { key: 'recording', label: 'Recording', color: '#ef4444' },
    { key: 'idle', label: 'Idle', color: '#52525b' },
    { key: 'disabled', label: 'Disabled', color: '#3f3f46' },
  ].filter(s => counts[s.key] > 0);
  $('status-chart').innerHTML = `
    <div class="status-bar">${segments.map(s => `<div style="width:${(counts[s.key] / total) * 100}%;background:${s.color}" title="${s.label}: ${counts[s.key]}"></div>`).join('')}</div>
    <div class="mt-2 flex flex-wrap gap-3 text-xs text-zinc-400">
      ${segments.map(s => `<span class="flex items-center gap-1"><span class="inline-block h-2 w-2 rounded-full" style="background:${s.color}"></span>${escapeHtml(s.label)} (${counts[s.key]})</span>`).join('')}
    </div>`;

  // Storage by source: horizontal bars from the per-stage disk usage breakdown.
  const perStage = (state.disk && state.disk.perStage) || {};
  const entries = Object.entries(perStage).sort((a, b) => b[1] - a[1]).slice(0, 8);
  const maxSize = entries.length ? entries[0][1] : 1;
  $('storage-chart').innerHTML = entries.length
    ? entries.map(([name, size]) => `
      <div class="storage-bar-row">
        <span class="storage-bar-label" title="${escapeHtml(name)}">${escapeHtml(name)}</span>
        <div class="storage-bar-track"><div class="storage-bar-fill" style="width:${maxSize ? (size / maxSize) * 100 : 0}%"></div></div>
        <span class="storage-bar-value">${formatBytes(size)}</span>
      </div>`).join('')
    : '<p class="text-sm text-zinc-400">No recordings yet.</p>';
}

function renderFavoritesPanel() {
  const favIds = new Set(config.settings.favoriteSetIds || []);
  const now = new Date();
  const upcoming = [];
  (config.timetable || []).forEach(st => (st.sets || []).forEach(set => {
    if (!favIds.has(set.id)) return;
    const end = set.end ? new Date(set.end) : null;
    if (end && end < now) return;
    upcoming.push({ stage: st.stage, set });
  }));
  upcoming.sort((a, b) => new Date(a.set.start) - new Date(b.set.start));
  if (!upcoming.length) {
    $('favorites-panel').innerHTML = '<p class="text-zinc-400">No starred sets yet — star one in the Timetable tab to get reminders.</p>';
    return;
  }
  $('favorites-panel').innerHTML = upcoming.slice(0, 6).map(({ stage, set }) => `
    <div class="flex items-center justify-between gap-2">
      <div class="min-w-0">
        <div class="truncate font-medium">${escapeHtml(set.name)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(stage)} · ${new Date(set.start).toLocaleString()}</div>
      </div>
    </div>`).join('');
}

// Named callbacks a dropdown option can trigger instead of selecting a value
// (see the "action" handling in setupCustomDropdowns below), keyed by the
// option's data-action attribute.
const dropdownActions = {};

// Wires up every .custom-dropdown under `root` (defaults to the whole page)
// that hasn't already been bound - safe to call repeatedly (e.g. once after
// every drawSourceEditor() re-render, and again after setDropdownOptions()
// rebuilds a single dropdown's contents) since already-bound dropdowns are
// skipped rather than getting a second stacked set of listeners.
function setupCustomDropdowns(root) {
  const scope = root || document;
  const dropdowns = [...scope.querySelectorAll('.custom-dropdown')];
  if (scope.classList && scope.classList.contains('custom-dropdown')) dropdowns.unshift(scope);

  dropdowns.forEach(dropdown => {
    if (dropdown.dataset.bound === '1') return;
    // Dropdowns populated later via setDropdownOptions() (assign-event,
    // assign-set) start out as empty containers with nothing to bind yet -
    // skip them without marking bound, so they get wired up for real once
    // setDropdownOptions() gives them a toggle/menu to work with.
    const toggle = dropdown.querySelector('.dropdown-toggle');
    if (!toggle) return;
    dropdown.dataset.bound = '1';

    const label = toggle.querySelector('.dropdown-toggle-label');
    const menu = dropdown.querySelector('.dropdown-menu');
    const hiddenInput = dropdown.querySelector('input[type="hidden"]');
    const sourceRow = dropdown.closest('.source-row');

    toggle.addEventListener('click', (e) => {
      e.preventDefault();
      document.querySelectorAll('.custom-dropdown .dropdown-menu').forEach(m => {
        if (m !== menu) m.classList.add('hidden');
      });
      menu.classList.toggle('hidden');
    });

    dropdown.querySelectorAll('.dropdown-option').forEach(option => {
      option.addEventListener('click', () => {
        // A handful of dropdowns (e.g. "+ New Event…" in the source editor)
        // want a click to trigger an action - opening another modal - rather
        // than actually selecting a value. dropdownActions maps the action
        // name to a callback; the option's own value/label are never applied.
        if (option.dataset.action) {
          menu.classList.add('hidden');
          const handler = dropdownActions[option.dataset.action];
          if (handler) handler(dropdown);
          return;
        }
        hiddenInput.value = option.dataset.value;
        label.textContent = option.querySelector('.font-semibold').textContent;
        // Copy through any other data-* the option carries (e.g. a
        // timetable set's bare artist name alongside its display label) so
        // callers can read it straight off the hidden input afterward
        // instead of re-parsing the visible label text.
        for (const key in option.dataset) {
          if (key === 'value') continue;
          hiddenInput.dataset[key] = option.dataset[key];
        }
        menu.classList.add('hidden');
        if (sourceRow) markCardUnsaved(sourceRow);
        hiddenInput.dispatchEvent(new Event('change', { bubbles: true }));
      });
    });
  });

  if (!setupCustomDropdowns._docBound) {
    setupCustomDropdowns._docBound = true;
    document.addEventListener('click', (e) => {
      if (!e.target.closest('.custom-dropdown')) {
        document.querySelectorAll('.custom-dropdown .dropdown-menu').forEach(m => {
          m.classList.add('hidden');
        });
      }
    });
  }
}

// Builds/rebuilds the standard custom-dropdown markup (toggle button +
// option menu + backing hidden input) inside an existing empty
// `<div id="{id}-dropdown" class="custom-dropdown">` container, for
// dropdowns whose option list is fetched or changes at runtime rather than
// being fixed per row (assign-event, assign-set, tt-lol-event). The hidden
// input keeps id={id}, so every existing `$(id).value` read and `change`
// listener elsewhere in the code keeps working unchanged.
function setDropdownOptions(id, options, opts) {
  opts = opts || {};
  const container = $(id + '-dropdown');
  if (!container) return;
  const value = opts.value !== undefined ? opts.value : '';
  const current = options.find(o => o.value === value);
  const label = current ? current.label : (opts.placeholder || '');
  let lastGroup;
  const menuHtml = options.length
    ? options.map(o => {
        // Options can carry a `group` (e.g. an event/festival name) to
        // cluster related entries under a non-clickable header, like an
        // <optgroup> - assumes the caller has already sorted by group.
        const header = o.group !== undefined && o.group !== lastGroup
          ? (lastGroup = o.group, `<div class="dropdown-group-label">${escapeHtml(o.group || 'Ungrouped')}</div>`)
          : '';
        return `${header}
        <div class="dropdown-option" data-value="${escapeAttr(o.value)}"${o.name !== undefined ? ` data-name="${escapeAttr(o.name)}"` : ''}>
          <div class="font-semibold">${escapeHtml(o.label)}</div>
          ${o.description ? `<div class="text-xs text-zinc-400">${escapeHtml(o.description)}</div>` : ''}
        </div>`;
      }).join('')
    : `<div class="dropdown-option-empty">${escapeHtml(opts.placeholder || 'No options')}</div>`;
  container.innerHTML = `
    <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(label)}</span><span class="ml-auto">▼</span></button>
    <div class="dropdown-menu hidden">${menuHtml}</div>
    <input type="hidden" id="${escapeAttr(id)}" class="${escapeAttr(id)}" value="${escapeAttr(value)}">`;
  container.dataset.bound = '';
  setupCustomDropdowns(container);
}

// Updates a dropdown built by setDropdownOptions to a specific value
// programmatically (as opposed to the user clicking an option) - used when
// code picks a value on the user's behalf, e.g. "Find closest set".
function setDropdownValue(id, value) {
  const container = $(id + '-dropdown');
  const input = $(id);
  if (!container || !input) return;
  const option = container.querySelector(`.dropdown-option[data-value="${CSS.escape(String(value))}"]`);
  input.value = value;
  if (option) {
    const labelEl = container.querySelector('.dropdown-toggle-label');
    if (labelEl) labelEl.textContent = option.querySelector('.font-semibold').textContent;
    for (const key in option.dataset) {
      if (key === 'value') continue;
      input.dataset[key] = option.dataset[key];
    }
  }
  input.dispatchEvent(new Event('change', { bubbles: true }));
}

function renderEditors() {
  if (!$('source-editor').dataset.loaded) {
    $('source-editor').dataset.loaded = '1';
    drawSourceEditor();
    $('timetable-json').value = JSON.stringify(config.timetable, null, 2);
    fillSettings();
    loadAccount();
    loadUsers();
    loadShareConfig();
    renderVisualTimetable();
    renderLinkedBadge();
    renderSavedTimetables();
    loadLolEvents();
  }
}

// openSourceIds tracks which accordion rows are expanded, keyed by source
// id, so the layout survives the periodic re-renders triggered by refresh().
let openSourceIds = new Set();

function toggleSourceRow(id) {
  if (openSourceIds.has(id)) openSourceIds.delete(id); else openSourceIds.add(id);
  drawSourceEditor();
}

function drawSourceEditor() {
  config.sources = config.sources || [];
  $('source-count-pill').textContent = `${config.sources.length} source${config.sources.length === 1 ? '' : 's'}`;
  if (!config.sources.length) {
    $('source-editor').innerHTML = '<p class="text-sm text-zinc-400">No sources yet — click "+ Add Source" above.</p>';
    return;
  }
  $('source-editor').innerHTML = config.sources.map((s, i) => {
    const open = openSourceIds.has(s.id);
    return `
    <div class="source-row ${open ? 'open' : ''}" data-source="${i}" data-id="${escapeAttr(s.id || '')}">
      <div class="source-row-head" style="border-left-color:${s.color || 'var(--accent)'}" onclick="toggleSourceRow('${escapeAttr(s.id || '')}')">
        <div class="flex min-w-0 items-center gap-2">
          <span class="source-row-chevron">&#9656;</span>
          <span class="truncate font-semibold">${escapeHtml(s.name || 'Untitled source')}</span>
          <span class="hidden text-xs text-zinc-400 sm:inline">${escapeHtml(s.type)} · ${escapeHtml(s.quality || 'best')} · ${escapeHtml(s.container || 'mkv')}</span>
        </div>
        <div class="flex flex-shrink-0 items-center gap-2 text-xs">
          ${s.enabled ? '' : '<span class="pill status-disabled">disabled</span>'}
          ${s.record ? '<span class="pill status-recording">auto-record</span>' : ''}
          <span class="save-hint hidden">Unsaved</span>
        </div>
      </div>
      <div class="source-row-body ${open ? '' : 'hidden'}">
        <div class="grid gap-2 md:grid-cols-4">
          <label>Name<input class="input src-name" value="${escapeAttr(s.name)}"></label>
          <label>Type
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(s.type || 'youtube')}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="youtube"><div class="font-semibold">youtube</div></div>
                <div class="dropdown-option" data-value="twitch"><div class="font-semibold">twitch</div></div>
                <div class="dropdown-option" data-value="http"><div class="font-semibold">http</div></div>
              </div>
              <input type="hidden" class="src-type" value="${escapeAttr(s.type || 'youtube')}">
            </div>
          </label>
          <label>URL<input class="input src-url" value="${escapeAttr(s.url)}"></label>
          <label>Quality<input class="input src-quality" value="${escapeAttr(s.quality || 'best')}"></label>
          <label>Container
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(s.container || 'mkv')}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="mkv">
                  <div class="font-semibold">Matroska (MKV)</div>
                  <div class="text-xs text-zinc-400">Best quality, flexible</div>
                </div>
                <div class="dropdown-option" data-value="mp4">
                  <div class="font-semibold">MP4</div>
                  <div class="text-xs text-zinc-400">Streaming-friendly</div>
                </div>
                <div class="dropdown-option" data-value="ts">
                  <div class="font-semibold">MPEG-TS (.ts)</div>
                  <div class="text-xs text-zinc-400">Live playback, low overhead</div>
                </div>
                <div class="dropdown-option" data-value="m4a">
                  <div class="font-semibold">M4A</div>
                  <div class="text-xs text-zinc-400">Audio only, compact</div>
                </div>
              </div>
              <input type="hidden" class="src-container" value="${escapeAttr(s.container || 'mkv')}">
            </div>
          </label>
          <label>Transcode
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${s.transcode ? 'Yes (re-encode)' : 'No (copy codec)'}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="no">
                  <div class="font-semibold">No - Copy codec</div>
                  <div class="text-xs text-zinc-400">Fastest, lowest CPU</div>
                </div>
                <div class="dropdown-option" data-value="yes">
                  <div class="font-semibold">Yes - Re-encode</div>
                  <div class="text-xs text-zinc-400">H.264/AAC, more compatible</div>
                </div>
              </div>
              <input type="hidden" class="src-transcode" value="${s.transcode ? 'yes' : 'no'}">
            </div>
          </label>
          <label>Live Rewind
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${s.liveRewind ? 'HLS Buffer' : 'Disabled'}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="none">
                  <div class="font-semibold">Disabled</div>
                  <div class="text-xs text-zinc-400">No live playback</div>
                </div>
                <div class="dropdown-option" data-value="hls">
                  <div class="font-semibold">HLS Buffer</div>
                  <div class="text-xs text-zinc-400">Scrubbing, extra CPU</div>
                </div>
              </div>
              <input type="hidden" class="src-liverewind" value="${s.liveRewind ? 'hls' : 'none'}">
            </div>
          </label>
          <label>HW accel
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(s.hardwareAccel || 'none')}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="none"><div class="font-semibold">none</div></div>
                <div class="dropdown-option" data-value="cuda"><div class="font-semibold">cuda</div></div>
                <div class="dropdown-option" data-value="qsv"><div class="font-semibold">qsv</div></div>
                <div class="dropdown-option" data-value="vaapi"><div class="font-semibold">vaapi</div></div>
              </div>
              <input type="hidden" class="src-hw" value="${escapeAttr(s.hardwareAccel || 'none')}">
            </div>
          </label>
          <label>Color<input class="input src-color" value="${escapeAttr(s.color || '')}"></label>
          <label>NFO note<input class="input src-nfo" value="${escapeAttr(s.extraNfo || '')}"></label>
          <label title="Matches this source to a stage name in the Timetable tab for Now/Next lookup, if it doesn't match this source's own name.">Timetable stage<input class="input src-ttstage" list="timetable-stage-names" value="${escapeAttr(s.timetableStage || '')}" placeholder="defaults to source name"></label>
          <label title="Groups this source under a recurring event (e.g. a festival franchise) for the Watch tab's source picker and Dashboard groups.">Event
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(festivalName(s.festivalId) || 'None')}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value=""><div class="font-semibold">None</div></div>
                ${(config.festivals || []).map(f => `<div class="dropdown-option" data-value="${escapeAttr(f.id)}"><div class="font-semibold">${escapeHtml(f.name)}</div></div>`).join('')}
                <div class="dropdown-option" data-action="new-festival"><div class="font-semibold">+ New Event…</div></div>
                <div class="dropdown-option" data-action="manage-events"><div class="font-semibold">Manage Events &amp; Organisations…</div><div class="text-xs text-zinc-400">Opens the Events tab</div></div>
              </div>
              <input type="hidden" class="src-festival" value="${escapeAttr(s.festivalId || '')}">
            </div>
          </label>
          <label class="inline-flex items-center gap-2"><input class="src-enabled" type="checkbox" ${s.enabled ? 'checked' : ''}> Enabled</label>
          <label class="inline-flex items-center gap-2"><input class="src-record" type="checkbox" ${s.record ? 'checked' : ''}> Auto record</label>
          <label class="inline-flex items-center gap-2"><input class="src-audio" type="checkbox" ${s.audioOnly ? 'checked' : ''}> Audio only</label>
          <label class="inline-flex items-center gap-2" title="Normalizes recorded volume to a consistent loudness (EBU R128). Forces audio to be re-encoded even if video is stream-copied."><input class="src-loudnorm" type="checkbox" ${s.loudnessNormalize ? 'checked' : ''}> Loudness normalize</label>
          <label class="inline-flex items-center gap-2" title="Requires YouTube Auto-Upload to be configured in Settings."><input class="src-yt-upload" type="checkbox" ${s.youtubeUpload ? 'checked' : ''}> Auto-upload to YouTube</label>
          <label>YouTube privacy
            <div class="custom-dropdown">
              <button type="button" class="dropdown-toggle input"><span class="dropdown-toggle-label">${escapeHtml(youtubePrivacyLabel(s.youtubePrivacy))}</span><span class="ml-auto">▼</span></button>
              <div class="dropdown-menu hidden">
                <div class="dropdown-option" data-value="unlisted"><div class="font-semibold">Unlisted</div></div>
                <div class="dropdown-option" data-value="private"><div class="font-semibold">Private</div></div>
                <div class="dropdown-option" data-value="public"><div class="font-semibold">Public</div></div>
              </div>
              <input type="hidden" class="src-yt-privacy" value="${escapeAttr(s.youtubePrivacy || 'unlisted')}">
            </div>
          </label>
        </div>
        <label class="mt-2 block" title="Only relevant for 'http' type sources whose stream needs an auth token/cookie/custom header - sent with both the recording (ffmpeg) and the live-preview proxy. One 'Key: Value' per line.">HTTP headers <span class="text-xs text-zinc-500">(token-gated HTTP streams only, one "Key: Value" per line)</span>
          <textarea class="input src-httpheaders codebox h-20" placeholder="Authorization: Bearer your-token-here">${escapeHtml((s.httpHeaders || []).join('\n'))}</textarea>
        </label>
        <div class="flex flex-wrap items-center gap-2 pt-2">
          <button type="button" class="btn primary" onclick="saveSourceCard(${i})">Save</button>
          <button type="button" class="btn" onclick="testSource(${i})">Test Stream</button>
          <button type="button" class="btn" onclick="duplicateSource(${i})">Duplicate</button>
          <button type="button" class="btn" style="color:#fda4af" onclick="deleteSource(${i})">Delete</button>
          <span class="test-result text-sm text-zinc-400" id="test-result-${i}"></span>
        </div>
      </div>
    </div>`;
  }).join('');

  document.querySelectorAll('.source-row').forEach(el => {
    el.querySelectorAll('input, select').forEach(field => field.addEventListener('input', () => markCardUnsaved(el)));
  });

  setupCustomDropdowns();

  if (highlightSourceId) {
    openSourceIds.add(highlightSourceId);
    const el = [...document.querySelectorAll('.source-row')].find(c => c.dataset.id === highlightSourceId);
    if (el) {
      el.classList.add('open');
      el.querySelector('.source-row-body').classList.remove('hidden');
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
      el.classList.add('now-playing');
      setTimeout(() => el.classList.remove('now-playing'), 2000);
    }
    highlightSourceId = null;
  }
}

function youtubePrivacyLabel(p) {
  return p === 'private' ? 'Private' : p === 'public' ? 'Public' : 'Unlisted';
}

function markCardUnsaved(el) { el.querySelector('.save-hint').classList.remove('hidden'); }

function readSourceCard(el) {
  return {
    name: el.querySelector('.src-name').value,
    type: el.querySelector('.src-type').value,
    url: el.querySelector('.src-url').value,
    quality: el.querySelector('.src-quality').value,
    container: el.querySelector('.src-container').value,
    hardwareAccel: el.querySelector('.src-hw').value === 'none' ? '' : el.querySelector('.src-hw').value,
    color: el.querySelector('.src-color').value,
    extraNfo: el.querySelector('.src-nfo').value,
    enabled: el.querySelector('.src-enabled').checked,
    record: el.querySelector('.src-record').checked,
    audioOnly: el.querySelector('.src-audio').checked,
    loudnessNormalize: el.querySelector('.src-loudnorm').checked,
    transcode: el.querySelector('.src-transcode').value === 'yes',
    liveRewind: el.querySelector('.src-liverewind').value !== 'none',
    youtubeUpload: el.querySelector('.src-yt-upload').checked,
    youtubePrivacy: el.querySelector('.src-yt-privacy').value,
    timetableStage: el.querySelector('.src-ttstage').value,
    festivalId: el.querySelector('.src-festival').value,
    httpHeaders: el.querySelector('.src-httpheaders').value.split('\n').map(x => x.trim()).filter(Boolean)
  };
}

function festivalName(id) {
  const f = (config.festivals || []).find(f => f.id === id);
  return f ? f.name : '';
}

function sourceCardEl(i) { return document.querySelector(`.source-row[data-source="${i}"]`); }

// --- Quick-create modal for a Festival ("Event" grouping for live sources) ---

let festivalEditorTargetRowId = null; // stable source id, since refresh() replaces the row's DOM entirely

dropdownActions['new-festival'] = (dropdown) => openFestivalEditor(dropdown);
// Jump to the Events tab (full Festival/Organisation management) without
// making the user hunt for it while they're mid-way through setting up a
// source - the quick-create modal above only covers name+color.
dropdownActions['manage-events'] = () => goToView('events-tab');

function openFestivalEditor(dropdown) {
  // Captured now, before any refresh() can replace the row's DOM (which
  // would detach `dropdown` and break a later dropdown.closest() lookup).
  const sourceRow = dropdown.closest('.source-row');
  festivalEditorTargetRowId = sourceRow ? sourceRow.dataset.id : null;
  $('festival-name').value = '';
  $('festival-color').value = '#ef4444';
  $('festival-editor-error').classList.add('hidden');
  $('festival-editor-overlay').classList.remove('hidden');
}
function closeFestivalEditor() { $('festival-editor-overlay').classList.add('hidden'); festivalEditorTargetRowId = null; }
$('festival-editor-close').onclick = closeFestivalEditor;
$('festival-editor-overlay').addEventListener('click', (e) => { if (e.target.id === 'festival-editor-overlay') closeFestivalEditor(); });

$('festival-editor-save').onclick = async () => {
  const name = $('festival-name').value.trim();
  if (!name) {
    $('festival-editor-error').textContent = 'Name is required';
    $('festival-editor-error').classList.remove('hidden');
    return;
  }
  let created;
  try {
    created = await api('/api/festivals', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, color: $('festival-color').value }) });
  } catch {
    return;
  }
  toast(`Created "${name}"`, 'info');
  const rowId = festivalEditorTargetRowId;
  closeFestivalEditor();
  await refresh();
  if (rowId) {
    openSourceIds.add(rowId);
    drawSourceEditor();
    const sourceRow = document.querySelector(`.source-row[data-id="${CSS.escape(rowId)}"]`);
    const rebuilt = sourceRow && [...sourceRow.querySelectorAll('.custom-dropdown')].find(d => d.querySelector('.dropdown-option[data-action="new-festival"]'));
    const option = rebuilt && [...rebuilt.querySelectorAll('.dropdown-option')].find(o => o.dataset.value === created.id);
    if (option) option.click();
  }
};

async function saveSourceCard(i) {
  const el = sourceCardEl(i);
  const values = readSourceCard(el);
  if (!values.name.trim() || !values.url.trim()) { toast('Name and URL are required', 'error'); return; }
  const id = config.sources[i].id;
  try {
    await api(`/api/sources/${id}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...config.sources[i], ...values }) });
    toast(`Saved "${values.name}"`, 'info');
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch { /* toast already shown */ }
}

async function testSource(i) {
  const el = sourceCardEl(i);
  const values = readSourceCard(el);
  const label = $(`test-result-${i}`);
  label.textContent = 'Testing…';
  try {
    const result = await api('/api/sources/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: values.type, url: values.url, quality: values.quality, httpHeaders: values.httpHeaders })
    });
    label.textContent = result.ok ? `Resolved OK` : `Failed: ${result.error}`;
    label.className = `test-result text-sm ${result.ok ? 'text-emerald-300' : 'text-rose-300'}`;
    if (result.ok) toast(`${values.name || 'Source'}: stream resolved successfully`, 'info');
  } catch (err) {
    label.textContent = 'Test request failed';
  }
}

async function duplicateSource(i) {
  const el = sourceCardEl(i);
  const values = readSourceCard(el);
  const copy = { ...values, name: `${values.name} copy` };
  try {
    const created = await api('/api/sources', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(copy) });
    toast(`Duplicated as "${copy.name}"`, 'info');
    highlightSourceId = created.id;
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch { /* toast already shown */ }
}

async function deleteSource(i) {
  const src = config.sources[i];
  if (!confirm(`Delete source "${src.name}"? This does not delete existing recordings.`)) return;
  try {
    await api(`/api/sources/${src.id}`, { method: 'DELETE' });
    toast(`Deleted "${src.name}"`, 'info');
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch { /* toast already shown */ }
}

// GB<->bytes for the free-space threshold settings - stored as bytes in the
// config (unchanged, for backward compatibility with existing config.json
// files), shown/edited as GB since nobody thinks in raw byte counts.
const gbBytes = 1024 * 1024 * 1024;
function bytesToGb(b) { return Math.round((b / gbBytes) * 100) / 100; }
function gbToBytes(gb) { return Math.round(gb * gbBytes); }

function toggleBackupMethodFields() {
  const isWebdav = $('backup-method').value === 'webdav';
  $('backup-rclone-fields').classList.toggle('hidden', isWebdav);
  $('backup-webdav-fields').classList.toggle('hidden', !isWebdav);
}
$('backup-method').addEventListener('change', toggleBackupMethodFields);

function fillSettings() {
  const s = config.settings, ui = config.ui;
  ['finishedDir','tempDir','logDir','checkIntervalSeconds','liveRewindWindowSeconds','reminderLeadMinutes'].forEach(k => $(k).value = s[k]);
  $('minFreeGb').value = bytesToGb(s.minFreeBytes || 0);
  $('warnFreeGb').value = bytesToGb(s.warnFreeBytes || 0);
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => $(k).checked = !!s[k]);
  $('fileExplorerRoot').value = s.fileExplorerRoot || '';
  $('uiAppName').value = ui.appName || '';
  $('uiLogoUrl').value = ui.logoUrl || '';
  syncImageUploadPreview('uiLogoUrl');
  $('uiCustomCss').value = ui.customCss || '';
  $('discordWebhook').value = s.notifications.discordWebhook || '';
  $('smtpEnabled').checked = !!s.notifications.smtp.enabled;
  ['smtpHost','smtpUsername','smtpPassword','smtpFrom','smtpTo'].forEach(id => $(id).value = s.notifications.smtp[id.replace('smtp','').toLowerCase()] || '');
  $('smtpPort').value = s.notifications.smtp.port || 587;
  $('backupEnabled').checked = !!s.backup.enabled;
  $('backupAfterComplete').checked = !!s.backup.afterComplete;
  $('rcloneRemote').value = s.backup.rcloneRemote || '';
  $('rcloneArgs').value = (s.backup.rcloneArgs || []).join('\n');
  setDropdownValue('backup-method', s.backup.method || 'rclone');
  toggleBackupMethodFields();
  const wd = s.backup.webdav || {};
  $('backup-webdav-url').value = wd.url || '';
  $('backup-webdav-username').value = wd.username || '';
  $('backup-webdav-password').value = wd.password || '';
  $('backup-webdav-proxy').checked = !!wd.proxy;
  const d = s.discordOAuth || {};
  $('discordOAuthEnabled').checked = !!d.enabled;
  $('discordOAuthClientId').value = d.clientId || '';
  $('discordOAuthClientSecret').value = d.clientSecret || '';
  $('discordOAuthRedirectUrl').value = d.redirectUrl || '';
  const yt = s.youtube || {};
  $('youtubeEnabled').checked = !!yt.enabled;
  $('youtubeClientId').value = yt.clientId || '';
  $('youtubeClientSecret').value = yt.clientSecret || '';
  $('youtubeRefreshToken').value = yt.refreshToken || '';

  if (ui.themeColors) {
    updateColorInputs(ui.themeColors);
  }
  renderFestivalPresets();
}

function readSettings() {
  const s = config.settings;
  ['finishedDir','tempDir','logDir'].forEach(k => s[k] = $(k).value);
  ['checkIntervalSeconds','liveRewindWindowSeconds','reminderLeadMinutes'].forEach(k => s[k] = Number($(k).value));
  s.minFreeBytes = gbToBytes(Number($('minFreeGb').value) || 0);
  s.warnFreeBytes = gbToBytes(Number($('warnFreeGb').value) || 0);
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => s[k] = $(k).checked);
  s.fileExplorerRoot = $('fileExplorerRoot').value.trim();
  config.ui = { appName: $('uiAppName').value, logoUrl: $('uiLogoUrl').value, customCss: $('uiCustomCss').value, customTheme: config.ui.customTheme, themeColors: config.ui.themeColors };
  s.notifications.discordWebhook = $('discordWebhook').value;
  s.notifications.smtp = { enabled: $('smtpEnabled').checked, host: $('smtpHost').value, port: Number($('smtpPort').value), username: $('smtpUsername').value, password: $('smtpPassword').value, from: $('smtpFrom').value, to: $('smtpTo').value };
  s.backup = {
    enabled: $('backupEnabled').checked, afterComplete: $('backupAfterComplete').checked,
    method: $('backup-method').value || 'rclone',
    rcloneRemote: $('rcloneRemote').value, rcloneArgs: $('rcloneArgs').value.split('\n').map(x => x.trim()).filter(Boolean),
    webdav: { url: $('backup-webdav-url').value, username: $('backup-webdav-username').value, password: $('backup-webdav-password').value, proxy: $('backup-webdav-proxy').checked },
  };
  s.discordOAuth = { enabled: $('discordOAuthEnabled').checked, clientId: $('discordOAuthClientId').value, clientSecret: $('discordOAuthClientSecret').value, redirectUrl: $('discordOAuthRedirectUrl').value };
  s.youtube = { enabled: $('youtubeEnabled').checked, clientId: $('youtubeClientId').value, clientSecret: $('youtubeClientSecret').value, refreshToken: $('youtubeRefreshToken').value };
}

async function saveConfig() {
  await api('/api/config', { method: 'PUT', body: JSON.stringify(config), headers: { 'Content-Type': 'application/json' } });
  $('source-editor').dataset.loaded = '';
  toast('Saved', 'info');
  await refresh();
}

// Tests whatever is currently typed into the Notifications panel - not
// necessarily saved yet - the same "test before you save" convention the
// Sources tab's "Test Stream" button already established.
$('notif-test-btn').onclick = async () => {
  const resultEl = $('notif-test-result');
  const btn = $('notif-test-btn');
  btn.disabled = true;
  resultEl.classList.add('hidden');
  const body = {
    discordWebhook: $('discordWebhook').value,
    smtp: { enabled: $('smtpEnabled').checked, host: $('smtpHost').value, port: Number($('smtpPort').value), username: $('smtpUsername').value, password: $('smtpPassword').value, from: $('smtpFrom').value, to: $('smtpTo').value },
  };
  let result;
  try {
    result = await api('/api/notifications/test', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  } catch {
    btn.disabled = false;
    return;
  }
  btn.disabled = false;
  if (result.error) { toast(result.error, 'error'); return; }
  const lines = [];
  if (result.discord.tested) lines.push(result.discord.ok ? 'Discord: sent ✓' : `Discord failed: ${result.discord.error}`);
  if (result.smtp.tested) lines.push(result.smtp.ok ? 'Email: sent ✓' : `Email failed: ${result.smtp.error}`);
  resultEl.textContent = lines.join(' · ');
  const anyFailed = (result.discord.tested && !result.discord.ok) || (result.smtp.tested && !result.smtp.ok);
  resultEl.className = anyFailed ? 'mt-2 text-xs text-red-400' : 'mt-2 text-xs text-emerald-400';
  resultEl.classList.remove('hidden');
  lines.forEach(l => toast(l, l.includes('failed') ? 'error' : 'info'));
};

async function loadAccount() {
  let acct;
  try {
    acct = await api('/api/account');
  } catch {
    return;
  }
  $('acct-username').value = acct.username || '';
  if (acct.managedByEnv) {
    $('account-note').textContent = `Signed in as "${acct.username}" (${acct.role}). Credentials for this deployment are set via AUTH_USERNAME/AUTH_PASSWORD environment variables and can't be changed here.`;
    $('account-form').classList.add('hidden');
  } else {
    $('account-note').textContent = `Signed in as "${acct.username}" (${acct.role}).`;
    $('account-form').classList.remove('hidden');
  }

  let discordEnabled = false;
  try {
    discordEnabled = (await api('/api/auth/discord/status')).enabled;
  } catch { /* leave hidden */ }

  if (acct.discordLinked) {
    $('acct-discord-status').textContent = `Linked to Discord as "${acct.discordName}".`;
    $('acct-discord-link').classList.add('hidden');
    $('acct-discord-unlink').classList.toggle('hidden', acct.managedByEnv);
  } else {
    $('acct-discord-status').textContent = discordEnabled ? 'Not linked.' : 'Discord login is not configured on this instance.';
    $('acct-discord-unlink').classList.add('hidden');
    $('acct-discord-link').classList.toggle('hidden', acct.managedByEnv || !discordEnabled);
  }
}

$('acct-save').onclick = async () => {
  const currentPassword = $('acct-current').value;
  const username = $('acct-username').value.trim();
  const password = $('acct-password').value;
  if (!username || password.length < 8) { toast('A username and a password of at least 8 characters are required', 'error'); return; }
  try {
    await api('/api/account', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ currentPassword, username, password }) });
    toast('Credentials updated — use them next time you sign in', 'info');
    $('acct-current').value = '';
    $('acct-password').value = '';
  } catch { /* toast already shown */ }
};

$('acct-discord-link').onclick = () => { window.location.href = '/api/auth/discord/link/start'; };
$('acct-discord-unlink').onclick = async () => {
  try {
    await api('/api/auth/discord/unlink', { method: 'POST' });
    toast('Discord unlinked', 'info');
    await loadAccount();
  } catch { /* toast already shown */ }
};

// --- Users tab (admin only - loadUsers is a no-op for other roles) ---

let usersList = [];

async function loadUsers() {
  if (state.role !== 'admin') return;
  try {
    usersList = await api('/api/users');
  } catch {
    return;
  }
  renderUsersList();
}

function renderUsersList() {
  if (!usersList.length) {
    $('users-list').innerHTML = '<p class="text-sm text-zinc-400">No users yet.</p>';
    return;
  }
  $('users-list').innerHTML = usersList.map(u => `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div class="min-w-0">
        <div class="font-medium">${escapeHtml(u.username)} <span class="pill">${escapeHtml(u.role)}</span></div>
        ${u.discordName ? `<div class="text-xs text-zinc-500">Discord: ${escapeHtml(u.discordName)}</div>` : ''}
      </div>
      <div class="flex flex-shrink-0 items-center gap-2">
        <button type="button" class="btn" onclick="toggleUserRole('${escapeAttr(u.id)}', '${u.role === 'admin' ? 'viewer' : 'admin'}')">${u.role === 'admin' ? 'Make viewer' : 'Make admin'}</button>
        <button type="button" class="btn" style="color:#fda4af" onclick="deleteUserAccount('${escapeAttr(u.id)}', '${escapeAttr(u.username)}')">Delete</button>
      </div>
    </div>`).join('');
}

async function toggleUserRole(id, role) {
  try {
    await api(`/api/users/${id}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ role }) });
    toast('Role updated', 'info');
    await loadUsers();
  } catch { /* toast already shown */ }
}

async function deleteUserAccount(id, username) {
  if (!confirm(`Delete user "${username}"? This can't be undone.`)) return;
  try {
    await api(`/api/users/${id}`, { method: 'DELETE' });
    toast(`Deleted "${username}"`, 'info');
    await loadUsers();
  } catch { /* toast already shown */ }
}

$('user-add').onclick = async () => {
  const username = $('user-new-username').value.trim();
  const password = $('user-new-password').value;
  const role = $('user-new-admin').checked ? 'admin' : 'viewer';
  if (!username || password.length < 8) { toast('A username and a password of at least 8 characters are required', 'error'); return; }
  try {
    await api('/api/users', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username, password, role }) });
    toast(`Added user "${username}"`, 'info');
    $('user-new-username').value = '';
    $('user-new-password').value = '';
    $('user-new-admin').checked = false;
    await loadUsers();
  } catch { /* toast already shown */ }
};

// --- Peer sharing setup (Settings) ---

async function loadShareConfig() {
  if (state.role !== 'admin') return;
  let cfg;
  try { cfg = await api('/api/share/config'); } catch { return; }
  $('share-public-url').value = cfg.publicUrl || '';
  $('share-proxy-url').value = cfg.proxyUrl || '';
  const pill = $('share-status-pill');
  if (cfg.enabled) {
    pill.textContent = cfg.public ? 'on' : 'on (LAN?)';
    pill.className = 'pill status-recording';
    $('share-disable').classList.remove('hidden');
  } else {
    pill.textContent = 'off';
    pill.className = 'pill status-idle';
    $('share-disable').classList.add('hidden');
  }
  if (cfg.enabled && cfg.forced) {
    $('share-verify-status').textContent = 'Enabled WITHOUT verification (forced) - double-check this URL is actually reachable from outside.';
  } else if (cfg.enabled && !cfg.public) {
    $('share-verify-status').textContent = 'Verified, but the URL looks like a LAN/loopback address — other instances on the internet may not reach it.';
  } else if (cfg.verifiedAt) {
    $('share-verify-status').textContent = `Verified ${new Date(cfg.verifiedAt).toLocaleString()}.`;
  } else {
    $('share-verify-status').textContent = '';
  }
}

$('share-verify').onclick = async () => {
  const publicUrl = $('share-public-url').value.trim();
  const proxyUrl = $('share-proxy-url').value.trim();
  const force = $('share-force').checked;
  if (!publicUrl) { toast('Enter the public URL first', 'error'); return; }
  $('share-verify-status').textContent = force ? 'Enabling without verification…' : 'Checking reachability…';
  let result;
  try {
    result = await api('/api/share/verify', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ publicUrl, proxyUrl, force }) });
  } catch { $('share-verify-status').textContent = ''; return; }
  if (!result.ok) { $('share-verify-status').textContent = result.error || 'Verification failed.'; toast('Could not verify that URL', 'error'); return; }
  if (result.forced) toast('Sharing enabled without verification', 'info');
  else toast(result.public ? 'Public URL verified — sharing enabled' : 'Verified, but the URL looks LAN-only', 'info');
  await loadShareConfig();
};

$('share-disable').onclick = async () => {
  try { await api('/api/share/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: false }) }); } catch { return; }
  toast('Sharing disabled', 'info');
  await loadShareConfig();
};

$('share-proxy-save').onclick = async () => {
  const proxyUrl = $('share-proxy-url').value.trim();
  $('share-proxy-status').textContent = 'Saving…';
  try {
    await api('/api/share/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: $('share-status-pill').classList.contains('status-recording'), proxyUrl }) });
  } catch { $('share-proxy-status').textContent = ''; return; }
  $('share-proxy-status').textContent = proxyUrl ? 'Proxy saved.' : 'Proxy cleared - downloading/sharing directly again.';
  toast('Proxy settings saved', 'info');
};

async function start(id) { try { await api(`/api/record/${id}`, { method: 'POST' }); } catch { return; } await refresh(); }
async function stopRec(id, name) {
  if (!confirm(`Stop recording "${name || 'this source'}"? The file recorded so far will be kept.`)) return;
  try { await api(`/api/record/${id}`, { method: 'DELETE' }); } catch { return; }
  await refresh();
}

// The tech element Video.js actually plays through is either the element
// you handed it (when it can use your tag directly) or nested inside the
// wrapper it builds around it - checking both covers either case without
// caring which one this particular version/config produces.
function techVideoEl(player) {
  const el = player.el();
  if (el.tagName === 'VIDEO' || el.tagName === 'AUDIO') return el;
  return el.querySelector('video') || el.querySelector('audio') || el;
}

// playLive() is called from a source card's "Watch Live" button - it just
// hands off to the Watch tab (which owns the actual player) instead of
// playing inline on the Dashboard, so there's only ever one live player
// instance in the whole app.
function playLive(id) {
  document.querySelector('.nav[data-view="watch"]').click();
  watchSelectSource(id);
}

// --- Watch tab (Live View) ---

let watchPlayerBound = false;
let watchSourceId = null;

function initWatch() {
  populateWatchSourceDropdown();
  if (watchSourceId) watchSelectSource(watchSourceId);
  else renderLivecutPanel();
  livecutRefreshJoined();
  startLivecutPoll();
}

function populateWatchSourceDropdown() {
  // Only sources with live rewind enabled actually stream something worth
  // watching here - without it, "watch live" just resolves the raw
  // streamlink URL, which usually isn't playable directly in a <video> tag.
  const sources = ((state && state.sources) || []).filter(s => s.liveRewind);
  const groupFor = (s) => {
    const f = (config.festivals || []).find(f => f.id === s.festivalId);
    return f ? f.name : 'Ungrouped';
  };
  const sorted = [...sources].sort((a, b) => {
    const ga = groupFor(a), gb = groupFor(b);
    // "Ungrouped" always sorts last, other groups alphabetically.
    if (ga !== gb) {
      if (ga === 'Ungrouped') return 1;
      if (gb === 'Ungrouped') return -1;
      return ga.localeCompare(gb);
    }
    return a.name.localeCompare(b.name);
  });
  const options = sorted.map(s => ({
    value: s.id,
    label: s.name + (s.status === 'recording' ? ' ●' : ''),
    group: groupFor(s),
  }));
  setDropdownOptions('watch-source', options, { value: watchSourceId || '', placeholder: 'Choose a source' });
}

$('watch-source-dropdown').addEventListener('change', (e) => {
  if (e.target.id === 'watch-source') watchSelectSource(e.target.value);
});

function ensureWatchPlayer() {
  if (watchPlayer) return watchPlayer;
  watchPlayer = videojs('watch-video', { controls: false, autoplay: false, preload: 'auto', bigPlayButton: false, fluid: false, liveui: true });
  setupWatchPlayerControls(watchPlayer);
  return watchPlayer;
}

function watchSelectSource(id) {
  watchSourceId = id;
  nowPlayingId = id || null;
  if (!id) {
    $('watch-player').classList.add('hidden');
    $('watch-empty').classList.remove('hidden');
    $('watch-stats').classList.add('hidden');
    renderDashboard();
    renderLivecutPanel();
    return;
  }
  const src = (state.sources || []).find(s => s.id === id);
  if (!src) return;
  livecutRefreshHostSessions();

  const player = ensureWatchPlayer();
  $('watch-empty').classList.add('hidden');
  $('watch-player').classList.remove('hidden');
  $('watch-stats').classList.remove('hidden');
  populateWatchSourceDropdown();
  renderDashboard();

  const url = src.liveRewindActive ? `/api/live/${id}/hls/index.m3u8` : `/api/live/${id}`;
  const looksLikeHls = src.liveRewindActive || src.type !== 'http' || /\.m3u8(\?|$)/i.test(src.url || '');
  player.src(looksLikeHls ? { src: url, type: 'application/x-mpegURL' } : { src: url });
  player.play().catch(err => toast(`Could not start playback: ${err.message}`, 'error'));

  const statusEl = $('watch-player-status');
  if (src.liveRewindActive) {
    statusEl.textContent = 'Live rewind buffer connecting — drag the seek bar back to scrub within this recording.';
    statusEl.classList.remove('hidden');
  } else {
    statusEl.classList.add('hidden');
  }
  $('watch-seek-wrap').classList.toggle('watch-seek-disabled', !src.liveRewindActive);

  renderWatchStats(src);
}

// Called on every periodic refresh() tick regardless of which tab is
// active, so the stats/source list stay current if the user leaves the
// Watch tab open in the background - but never touches the player itself
// unless a source is actually selected.
function updateWatchIfActive() {
  if (!watchSourceId) return;
  const src = (state.sources || []).find(s => s.id === watchSourceId);
  if (!src) {
    watchSelectSource('');
    return;
  }
  renderWatchStats(src);
}

function renderWatchStats(src) {
  const rows = [
    { label: 'Status', value: src.status },
    { label: 'Elapsed', value: src.status === 'recording' ? elapsed(src.startedAt) : '—' },
    { label: 'Now playing', value: src.currentSet || 'No current set' },
    { label: 'Next set', value: src.nextSet || 'No upcoming set' },
    { label: 'Size', value: formatBytes(src.size || 0) },
    { label: 'Quality / container', value: `${src.quality || 'best'} · ${src.container || 'mkv'}` },
    { label: 'Live rewind', value: src.liveRewindActive ? 'Active' : (src.liveRewind ? 'Enabled (not active)' : 'Disabled') },
    { label: 'Type', value: src.type },
  ];
  $('watch-stats').innerHTML = rows.map(r => `
    <div class="rounded border border-white/10 px-3 py-2">
      <div class="text-xs text-zinc-400">${escapeHtml(r.label)}</div>
      <div class="font-medium truncate">${escapeHtml(String(r.value))}</div>
    </div>`).join('');
}

// Bound once: ensureWatchPlayer() only creates the Player instance the
// first time it's needed, so controls only ever get wired up once too.
function setupWatchPlayerControls(player) {
  const playPauseBtn = $('watch-playpause');
  const centerPlay = $('watch-center-play');
  const back10 = $('watch-back10');
  const fwd10 = $('watch-fwd10');
  const timeEl = $('watch-time');
  const seek = $('watch-seek');
  const seekWrap = $('watch-seek-wrap');
  const seekProgress = $('watch-seek-progress');
  const seekBuffer = $('watch-seek-buffer');
  const muteBtn = $('watch-mute');
  const volume = $('watch-volume');
  const vizToggle = $('watch-visualizer-toggle');
  let scrubbing = false;

  const setPlayIcon = (playing) => { playPauseBtn.innerHTML = playing ? '&#10074;&#10074;' : '&#9658;'; };
  const togglePlay = () => { if (player.paused()) player.play().catch(() => {}); else player.pause(); };

  playPauseBtn.onclick = togglePlay;
  centerPlay.onclick = togglePlay;
  player.el().addEventListener('click', togglePlay);
  player.on('play', () => { setPlayIcon(true); centerPlay.classList.add('hidden'); });
  player.on('pause', () => { setPlayIcon(false); centerPlay.classList.remove('hidden'); });
  player.on('waiting', () => $('watch-player').classList.add('cp-buffering'));
  player.on('playing', () => $('watch-player').classList.remove('cp-buffering'));
  player.on('error', () => {
    const err = player.error();
    toast(`Live stream error: ${err ? err.message : 'playback failed'}`, 'error');
  });

  back10.onclick = () => { if (isFinite(player.duration())) player.currentTime(Math.max(0, player.currentTime() - 10)); };
  fwd10.onclick = () => { if (isFinite(player.duration())) player.currentTime(Math.min(player.duration(), player.currentTime() + 10)); };

  player.on('timeupdate', () => {
    const duration = player.duration();
    if (scrubbing) return;
    if (!isFinite(duration) || !duration) {
      timeEl.textContent = 'LIVE';
      return;
    }
    const pct = (player.currentTime() / duration) * 1000;
    seek.value = pct;
    seekProgress.style.width = `${pct / 10}%`;
    timeEl.textContent = `${formatTime(player.currentTime())} / ${formatTime(duration)}`;
  });
  player.on('progress', () => {
    const duration = player.duration();
    const bufferedEnd = player.bufferedEnd();
    if (isFinite(duration) && duration && bufferedEnd) seekBuffer.style.width = `${(bufferedEnd / duration) * 100}%`;
  });

  seek.addEventListener('input', () => {
    scrubbing = true;
    const duration = player.duration();
    if (!isFinite(duration) || !duration) return;
    const pct = seek.value / 1000;
    seekProgress.style.width = `${pct * 100}%`;
    timeEl.textContent = `${formatTime(pct * duration)} / ${formatTime(duration)}`;
  });
  seek.addEventListener('change', () => {
    const duration = player.duration();
    if (isFinite(duration) && duration) player.currentTime((seek.value / 1000) * duration);
    scrubbing = false;
  });
  seekWrap.addEventListener('mousedown', () => { scrubbing = true; });

  volume.addEventListener('input', () => {
    player.volume(parseFloat(volume.value));
    player.muted(player.volume() === 0);
    muteBtn.innerHTML = player.muted() ? '&#128263;' : '&#128266;';
  });
  muteBtn.onclick = () => {
    player.muted(!player.muted());
    muteBtn.innerHTML = player.muted() ? '&#128263;' : '&#128266;';
    if (!player.muted() && player.volume() === 0) { player.volume(1); volume.value = 1; }
  };

  setupPiP($('watch-pip'), () => techVideoEl(player));

  $('watch-fullscreen').onclick = () => {
    if (player.isFullscreen()) player.exitFullscreen();
    else player.requestFullscreen();
  };

  const updateVisualizerVisibility = () => {
    const isAudioOnly = player.videoWidth() === 0 && player.videoHeight() === 0;
    $('watch-player').classList.toggle('audio-mode', isAudioOnly);
    vizToggle.disabled = isAudioOnly;
    if (isAudioOnly) vizToggle.checked = true;
    const showViz = isAudioOnly || vizToggle.checked;
    $('watch-audio-stage').classList.toggle('hidden', !showViz);
    if (showViz) startVisualizer(techVideoEl(player), 'watch-visualizer');
    else stopVisualizer('watch-visualizer');
  };
  player.on('loadedmetadata', updateVisualizerVisibility);
  vizToggle.addEventListener('change', () => { if (!vizToggle.disabled) updateVisualizerVisibility(); });
  populateVisualizerPresetSelect('watch-visualizer');
  $('watch-visualizer-next').addEventListener('click', () => nextVisualizerPreset('watch-visualizer', true));
}

// --- Live Cut Sessions: crowdsourced live transition marking ---
//
// A session hosted by this instance is tied to the currently-selected Watch
// tab source (recomputed each poll tick from the full session list, so
// switching sources just changes what livecutHostSession resolves to -
// there's no separate "select a session" step for the host). Joined
// sessions (hosted elsewhere) are independent of the selected source, since
// a guest instance may have no matching local source at all - just a plain
// list with its own mark button and feed per session.

let livecutHostSession = null; // this instance's own session for the selected source, if any
let livecutJoined = [];        // sessions hosted elsewhere that this instance has joined
let livecutFeeds = {};         // token -> { events: [...], since: <seq>, closed: bool }
let livecutPollTimer = null;

function livecutFeedState(token) {
  if (!livecutFeeds[token]) livecutFeeds[token] = { events: [], since: 0, closed: false };
  return livecutFeeds[token];
}

async function livecutRefreshHostSessions() {
  let sessions;
  try { sessions = await api('/api/livecut/sessions'); } catch { return; }
  livecutHostSession = sessions.find(s => s.sourceId === watchSourceId && !s.closed) || null;
  renderLivecutPanel();
}

async function livecutRefreshJoined() {
  try { livecutJoined = await api('/api/livecut/joined'); } catch { return; }
  renderLivecutJoinedList();
}

async function livecutPollFeed(kind, token) {
  const st = livecutFeedState(token);
  if (st.closed) return;
  const base = kind === 'host' ? `/api/livecut/sessions/${encodeURIComponent(token)}/feed` : `/api/livecut/joined/${encodeURIComponent(token)}/feed`;
  let result;
  try { result = await api(`${base}?since=${st.since}`); } catch { return; }
  if (result.events && result.events.length) {
    st.events = st.events.concat(result.events);
    st.since = result.events[result.events.length - 1].seq;
  }
  st.closed = !!result.closed;
  renderLivecutFeed(kind, token);
}

function livecutPollTick() {
  livecutRefreshHostSessions();
  if (livecutHostSession) livecutPollFeed('host', livecutHostSession.token);
  livecutJoined.forEach(j => livecutPollFeed('joined', j.token));
  livecutPruneFeeds();
}

// Drops accumulated feed state for any session this instance is no longer
// hosting or joined to - otherwise switching sources or closing sessions
// leaves their (potentially large) event lists in memory for the life of the
// Watch tab. A session still in the joined list keeps its feed even after its
// host closes it, so the final marks stay visible until the user leaves.
function livecutPruneFeeds() {
  const active = new Set();
  if (livecutHostSession) active.add(livecutHostSession.token);
  livecutJoined.forEach(j => active.add(j.token));
  Object.keys(livecutFeeds).forEach(token => { if (!active.has(token)) delete livecutFeeds[token]; });
}

function startLivecutPoll() {
  stopLivecutPoll();
  livecutPollTimer = setInterval(livecutPollTick, 1500);
  livecutPollTick();
}
function stopLivecutPoll() {
  if (livecutPollTimer) { clearInterval(livecutPollTimer); livecutPollTimer = null; }
}

function livecutFeedItemHtml(ev) {
  const who = ev.instanceName || ev.instanceId;
  const label = ev.username ? `${escapeHtml(who)} <span class="text-zinc-500">(${escapeHtml(ev.username)})</span>` : escapeHtml(who);
  return `<div class="flex items-center justify-between gap-2 rounded border border-white/10 px-2 py-1">
    <span>${label}</span><span class="text-xs text-zinc-500">${new Date(ev.ts).toLocaleTimeString()}</span>
  </div>`;
}

function livecutFeedHtml(token) {
  const st = livecutFeedState(token);
  return st.events.slice(-30).reverse().map(livecutFeedItemHtml).join('') || '<div class="text-xs text-zinc-500">No marks yet.</div>';
}

function renderLivecutFeed(kind, token) {
  if (kind === 'host') {
    $('livecut-host-feed').innerHTML = livecutFeedHtml(token);
    return;
  }
  const el = document.querySelector(`.livecut-joined-feed[data-token="${token}"]`);
  if (el) el.innerHTML = livecutFeedHtml(token);
}

function renderLivecutJoinedList() {
  const box = $('livecut-joined-list');
  if (!livecutJoined.length) { box.innerHTML = ''; return; }
  box.innerHTML = livecutJoined.map(j => `
    <div class="space-y-2 rounded border border-white/10 p-2">
      <div class="flex items-center justify-between gap-2">
        <div class="min-w-0 truncate font-medium">${escapeHtml(j.name || '(unnamed session)')}</div>
        <button type="button" class="btn livecut-leave" data-token="${escapeAttr(j.token)}">Leave</button>
      </div>
      <button type="button" class="btn primary w-full livecut-joined-mark" data-token="${escapeAttr(j.token)}">Mark Transition</button>
      <div class="livecut-joined-feed max-h-40 space-y-1 overflow-auto text-sm" data-token="${escapeAttr(j.token)}"></div>
    </div>`).join('');
  box.querySelectorAll('.livecut-leave').forEach(btn => btn.addEventListener('click', () => livecutLeave(btn.dataset.token)));
  box.querySelectorAll('.livecut-joined-mark').forEach(btn => btn.addEventListener('click', () => livecutMark('joined', btn.dataset.token)));
  livecutJoined.forEach(j => renderLivecutFeed('joined', j.token));
}

function renderLivecutPanel() {
  const src = (state.sources || []).find(s => s.id === watchSourceId);
  $('livecut-pick-source').classList.toggle('hidden', !!watchSourceId);
  $('livecut-start').classList.add('hidden');
  $('livecut-host').classList.add('hidden');
  if (watchSourceId && src) {
    if (livecutHostSession) {
      $('livecut-host').classList.remove('hidden');
      // Only rewrite the code field when it actually changes - this render
      // runs on every ~1.5s poll tick, and blindly reassigning .value would
      // clobber a manual text-selection the user is making to copy the code.
      if ($('livecut-host-code').value !== livecutHostSession.code) $('livecut-host-code').value = livecutHostSession.code;
      renderLivecutFeed('host', livecutHostSession.token);
    } else {
      $('livecut-start').classList.remove('hidden');
      const canStart = src.status === 'recording';
      $('livecut-start-btn').disabled = !canStart;
      $('livecut-start-btn').title = canStart ? '' : 'This source needs to be actively recording to start a session';
    }
  }
}

async function livecutStart() {
  if (!watchSourceId) return;
  let result;
  try {
    result = await api('/api/livecut/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sourceId: watchSourceId }) });
  } catch { return; }
  toast('Live Cut Session started', 'info');
  livecutHostSession = result;
  renderLivecutPanel();
}

async function livecutCloseHost() {
  if (!livecutHostSession) return;
  if (!confirm('Close this Live Cut Session? Anyone still connected will stop being able to mark.')) return;
  try { await api(`/api/livecut/sessions/${encodeURIComponent(livecutHostSession.token)}`, { method: 'DELETE' }); } catch { return; }
  delete livecutFeeds[livecutHostSession.token];
  livecutHostSession = null;
  renderLivecutPanel();
}

async function livecutImportHost() {
  if (!livecutHostSession) return;
  let result;
  try { result = await api(`/api/livecut/sessions/${encodeURIComponent(livecutHostSession.token)}/import`, { method: 'POST' }); } catch { return; }
  toast(`Added ${result.added} marker(s) to ${result.path}`, 'info');
}

async function livecutMark(kind, token) {
  const path = kind === 'host' ? `/api/livecut/sessions/${encodeURIComponent(token)}/mark` : `/api/livecut/joined/${encodeURIComponent(token)}/mark`;
  try { await api(path, { method: 'POST' }); } catch { return; }
  livecutPollFeed(kind, token);
}

async function livecutJoin() {
  const code = $('livecut-join-code').value.trim();
  if (!code) { toast('Paste a code first', 'error'); return; }
  let result;
  try {
    result = await api('/api/livecut/join', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code }) });
  } catch { return; }
  if (!result.ok) { toast(result.error || 'Could not join that session', 'error'); return; }
  $('livecut-join-code').value = '';
  toast(`Joined "${result.session.name}"`, 'info');
  await livecutRefreshJoined();
}

async function livecutLeave(token) {
  try { await api(`/api/livecut/joined/${encodeURIComponent(token)}`, { method: 'DELETE' }); } catch { return; }
  delete livecutFeeds[token];
  await livecutRefreshJoined();
}

$('livecut-start-btn').onclick = livecutStart;
$('livecut-host-close').onclick = livecutCloseHost;
$('livecut-host-import').onclick = livecutImportHost;
$('livecut-host-mark').onclick = () => livecutHostSession && livecutMark('host', livecutHostSession.token);
$('livecut-host-copy').onclick = () => copyShareCode($('livecut-host-code').value);
$('livecut-join-btn').onclick = livecutJoin;

function switchToView(viewId) {
  document.querySelectorAll('.nav').forEach(x => x.classList.remove('active'));
  document.querySelectorAll('.view').forEach(x => x.classList.add('hidden'));
  const navBtn = document.querySelector(`.nav[data-view="${viewId}"]`);
  if (navBtn) navBtn.classList.add('active');
  const section = $(viewId);
  if (section) section.classList.remove('hidden');
}

// goToView clicks a nav tab by name (runs its full open/refresh handler),
// used by in-content shortcuts like empty-state call-to-action buttons.
function goToView(viewId) {
  const btn = document.querySelector(`.nav[data-view="${viewId}"]`);
  if (btn) btn.click();
}

document.querySelectorAll('.nav').forEach(b => b.onclick = async () => {
  if (b.dataset.view !== 'watch') stopLivecutPoll();
  switchToView(b.dataset.view);
  // Each tab must pull fresh data on every open instead of showing stale state.
  $('source-editor').dataset.loaded = '';
  await refresh();
  if (b.dataset.view === 'recordings') initLibrary();
  if (b.dataset.view === 'diagnostics') runDiagnostics();
  if (b.dataset.view === 'watch') initWatch();
  if (b.dataset.view === 'events-tab') renderEventsTab();
  if (b.dataset.view === 'explorer') loadExplorer();
});

// --- Diagnostics (system check) ---

function renderDiagnostics(container, report) {
  const icon = s => s === 'pass' ? '&#10003;' : s === 'warn' ? '!' : '&#10007;';
  const banner = report.overallOk
    ? '<div class="syscheck-banner ok">Everything required is in place.</div>'
    : '<div class="syscheck-banner issues">Some required checks are failing — recording may not work correctly until these are fixed.</div>';
  const items = report.checks.map(c => `
    <div class="syscheck-item syscheck-${escapeAttr(c.status)}">
      <span class="syscheck-icon">${icon(c.status)}</span>
      <div class="min-w-0">
        <div class="font-medium">${escapeHtml(c.label)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(c.detail)}</div>
      </div>
    </div>`).join('');
  const rows = report.requirements.map(req => `
    <tr class="req-${escapeAttr(req.status)}">
      <td>${escapeHtml(req.label)}</td><td>${escapeHtml(req.min)}</td><td>${escapeHtml(req.recommended)}</td><td>${escapeHtml(req.value)}</td>
    </tr>`).join('');
  container.innerHTML = `
    ${banner}
    <div class="syscheck-list">${items}</div>
    <table class="syscheck-req-table">
      <thead><tr><th>Hardware</th><th>Minimum</th><th>Recommended</th><th>Your system</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

async function runDiagnostics() {
  const body = $('diag-body');
  body.innerHTML = '<p class="text-sm text-zinc-400">Checking…</p>';
  try {
    const report = await api('/api/system-check');
    renderDiagnostics(body, report);
  } catch {
    body.innerHTML = '<p class="text-sm text-rose-300">Could not run the system check — see the notification for details.</p>';
  }
}
$('diag-rerun').onclick = runDiagnostics;

$('logout-btn').onclick = async () => {
  try { await api('/api/logout', { method: 'POST' }); } catch { /* ignore */ }
  window.location.href = '/login';
};

// --- Add Source wizard ---

const wizardStepIds = ['wizard-step-type', 'wizard-step-details', 'wizard-step-review'];
let wizardStep = 0;
let wizardType = null;

function openWizard() {
  wizardStep = 0;
  wizardType = null;
  $('wiz-name').value = '';
  $('wiz-url').value = '';
  $('wiz-quality').value = 'best';
  $('wiz-test-result').textContent = '';
  document.querySelectorAll('.wizard-type-card').forEach(c => c.classList.remove('selected'));
  $('wizard-error').classList.add('hidden');
  renderWizardStep();
  $('wizard-overlay').classList.remove('hidden');
}

function closeWizard() {
  $('wizard-overlay').classList.add('hidden');
}

function wizardShowError(msg) {
  $('wizard-error').textContent = msg;
  $('wizard-error').classList.remove('hidden');
}

function renderWizardStep() {
  wizardStepIds.forEach((id, i) => $(id).classList.toggle('hidden', i !== wizardStep));
  $('wizard-steps').innerHTML = wizardStepIds.map((_, i) =>
    `<span class="wizard-dot ${i === wizardStep ? 'active' : i < wizardStep ? 'done' : ''}"></span>`
  ).join('');
  $('wizard-back').classList.toggle('hidden', wizardStep === 0);
  $('wizard-next').textContent = wizardStep === wizardStepIds.length - 1 ? 'Create Source' : 'Next';
  if (wizardStep === 2) renderWizardReview();
}

function renderWizardReview() {
  const typeLabel = { youtube: 'YouTube', twitch: 'Twitch', http: 'Raw HTTP/HLS' }[wizardType] || wizardType;
  $('wizard-review').innerHTML = `
    <div><span class="text-zinc-400">Type:</span> ${escapeHtml(typeLabel)}</div>
    <div><span class="text-zinc-400">Name:</span> ${escapeHtml($('wiz-name').value.trim())}</div>
    <div class="break-all"><span class="text-zinc-400">URL:</span> ${escapeHtml($('wiz-url').value.trim())}</div>
    <div><span class="text-zinc-400">Quality:</span> ${escapeHtml($('wiz-quality').value.trim() || 'best')}</div>`;
}

$('open-wizard').onclick = openWizard;
$('wizard-close').onclick = closeWizard;
$('wizard-overlay').addEventListener('click', (e) => { if (e.target.id === 'wizard-overlay') closeWizard(); });

document.querySelectorAll('.wizard-type-card').forEach(card => card.onclick = () => {
  wizardType = card.dataset.type;
  document.querySelectorAll('.wizard-type-card').forEach(c => c.classList.remove('selected'));
  card.classList.add('selected');
});

$('wizard-back').onclick = () => {
  if (wizardStep === 0) return;
  wizardStep--;
  $('wizard-error').classList.add('hidden');
  renderWizardStep();
};

$('wizard-next').onclick = async () => {
  $('wizard-error').classList.add('hidden');
  if (wizardStep === 0) {
    if (!wizardType) { wizardShowError('Choose a source type to continue'); return; }
    wizardStep++;
    renderWizardStep();
    return;
  }
  if (wizardStep === 1) {
    if (!$('wiz-name').value.trim() || !$('wiz-url').value.trim()) { wizardShowError('Give the source a name and a URL'); return; }
    wizardStep++;
    renderWizardStep();
    return;
  }
  const payload = {
    name: $('wiz-name').value.trim(),
    type: wizardType,
    url: $('wiz-url').value.trim(),
    enabled: true,
    record: false,
    quality: $('wiz-quality').value.trim() || 'best',
    container: 'mkv'
  };
  try {
    const created = await api('/api/sources', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
    toast(`Added "${payload.name}" — fine-tune it below if needed`, 'info');
    highlightSourceId = created.id;
    closeWizard();
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch {
    wizardShowError('Could not create the source — see the notification for details');
  }
};

$('wiz-test').onclick = async () => {
  $('wizard-error').classList.add('hidden');
  if (!wizardType) { wizardShowError('Choose a source type first'); return; }
  const url = $('wiz-url').value.trim();
  if (!url) { wizardShowError('Enter a URL first'); return; }
  const label = $('wiz-test-result');
  label.textContent = 'Testing…';
  label.className = 'text-sm text-zinc-400';
  try {
    const result = await api('/api/sources/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: wizardType, url, quality: $('wiz-quality').value.trim() || 'best' })
    });
    label.textContent = result.ok ? 'Resolved OK' : `Failed: ${result.error}`;
    label.className = `text-sm ${result.ok ? 'text-emerald-300' : 'text-rose-300'}`;
  } catch {
    label.textContent = 'Test request failed';
  }
};

// --- Preset Packs: bundled, ready-to-add DJs/streamers/events ---

let presetPacks = [];

$('open-presets').onclick = openPresets;
$('presets-close').onclick = () => $('presets-overlay').classList.add('hidden');

async function openPresets() {
  $('presets-overlay').classList.remove('hidden');
  $('presets-list').innerHTML = '<p class="text-sm text-zinc-400">Loading…</p>';
  try {
    presetPacks = await api('/api/presets');
  } catch {
    presetPacks = [];
  }
  renderPresetsList();
}

function presetIsFullyAdded(preset) {
  const existingUrls = new Set((config.sources || []).map(s => s.url.toLowerCase()));
  return preset.sources.every(s => existingUrls.has(s.url.toLowerCase()));
}

function renderPresetsList() {
  if (!presetPacks.length) {
    $('presets-list').innerHTML = '<p class="text-sm text-zinc-400">No presets available.</p>';
    return;
  }
  $('presets-list').innerHTML = presetPacks.map(p => {
    const added = presetIsFullyAdded(p);
    return `
    <div class="flex flex-wrap items-center justify-between gap-3 rounded border border-white/10 px-3 py-2">
      <div class="flex min-w-0 items-center gap-3">
        ${p.logoUrl ? `<img src="${escapeAttr(p.logoUrl)}" class="h-10 w-10 flex-shrink-0 rounded object-cover" alt="">` : ''}
        <div class="min-w-0">
          <div class="font-medium">${escapeHtml(p.name)} ${p.category ? `<span class="pill">${escapeHtml(p.category)}</span>` : ''}</div>
          ${p.description ? `<div class="text-xs text-zinc-400">${escapeHtml(p.description)}</div>` : ''}
          <div class="truncate text-xs text-zinc-500">${p.sources.map(s => escapeHtml(s.url)).join(', ')}</div>
        </div>
      </div>
      <button type="button" class="btn flex-shrink-0 ${added ? '' : 'primary'}" ${added ? 'disabled' : ''} onclick="applyPreset('${escapeAttr(p.id)}')">${added ? 'Added' : 'Add'}</button>
    </div>`;
  }).join('');
}

async function applyPreset(id) {
  const preset = presetPacks.find(p => p.id === id);
  if (!preset) return;
  const existingUrls = new Set((config.sources || []).map(s => s.url.toLowerCase()));
  const toAdd = preset.sources.filter(s => !existingUrls.has(s.url.toLowerCase()));
  if (!toAdd.length) {
    toast(`${preset.name} is already added`, 'info');
    return;
  }
  let added = 0;
  for (const src of toAdd) {
    try {
      await api('/api/sources', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...src, enabled: true, record: false })
      });
      added++;
    } catch { /* toast already shown by api() */ }
  }
  if (added) {
    toast(`Added ${added} source${added === 1 ? '' : 's'} from "${preset.name}"`, 'info');
    $('source-editor').dataset.loaded = '';
    await refresh();
    renderPresetsList();
  }
}

$('save-timetable').onclick = async () => {
  try {
    config.timetable = JSON.parse($('timetable-json').value);
  } catch (err) {
    toast(`Timetable JSON is invalid: ${err.message}`, 'error');
    return;
  }
  await saveConfig();
};
$('save-settings').onclick = async () => { readSettings(); await saveConfig(); };
$('tt-toggle-json').onclick = () => {
  const wrap = $('timetable-json-wrap');
  const showing = !wrap.classList.contains('hidden');
  if (!showing) $('timetable-json').value = JSON.stringify(config.timetable, null, 2);
  wrap.classList.toggle('hidden');
  $('tt-toggle-json').textContent = showing ? 'Show raw JSON' : 'Hide raw JSON';
};

// --- Visual timetable ---

function parseIso(iso) {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}):(\d{2})/.exec(iso || '');
  if (!m) return null;
  return { date: m[1], minutes: parseInt(m[2], 10) * 60 + parseInt(m[3], 10) };
}

// festivalDayRolloverMin mirrors the backend's festivalDayRolloverHour
// (main.go): festival timetables group a program that runs from the
// afternoon/evening into the small hours of the following morning all under
// one day label, so a set that starts at, say, 01:00 is really the tail end
// of the *previous* festival day's night, not the start of a new one - the
// same set the backend's combineDateTime already dates as the next calendar
// day when importing. Grouping sets by their literal ISO date (instead of
// this festival-day convention) is what used to put a 1am afterparty set on
// the wrong day's tab entirely, as an odd early-morning blip disconnected
// from the day it was actually part of.
const festivalDayRolloverMin = 8 * 60;

// festivalDayOf returns which festival "day" a parsed timestamp belongs to
// for display/grouping purposes: its own calendar date, unless it's before
// the rollover hour, in which case it belongs to the previous calendar day.
function festivalDayOf(p) {
  if (!p) return null;
  if (p.minutes >= festivalDayRolloverMin) return p.date;
  const d = new Date(p.date + 'T00:00:00Z');
  d.setUTCDate(d.getUTCDate() - 1);
  return d.toISOString().slice(0, 10);
}

// festivalMinutes returns how many minutes after midnight of `day` a parsed
// timestamp falls - possibly past 24:00 (or negative) when its own calendar
// date differs from `day`, so a set that runs into the small hours of the
// next calendar date still positions/sorts correctly as part of `day`'s
// timeline instead of snapping back to the start of it.
function festivalMinutes(p, day) {
  const dayDiff = Math.round((Date.parse(p.date + 'T00:00:00Z') - Date.parse(day + 'T00:00:00Z')) / 86400000);
  return p.minutes + dayDiff * 24 * 60;
}

function timetableDays() {
  const days = new Set();
  (config.timetable || []).forEach(st => (st.sets || []).forEach(s => { const p = parseIso(s.start); if (p) days.add(festivalDayOf(p)); }));
  return [...days].sort();
}

function stagePalette(name) {
  const palette = ['#ef4444', '#3b82f6', '#22c55e', '#eab308', '#a855f7', '#f97316', '#ec4899', '#06b6d4'];
  let hash = 0;
  for (const c of name || '') hash = (hash * 31 + c.charCodeAt(0)) >>> 0;
  return palette[hash % palette.length];
}

function festivalOffset() {
  for (const st of config.timetable || []) {
    for (const s of st.sets || []) {
      const m = /([+-]\d{2}:\d{2}|Z)$/.exec(s.start || '');
      if (m) return m[1] === 'Z' ? '+00:00' : m[1];
    }
  }
  return '+00:00';
}

function syncTimetableJsonFromConfig() {
  $('timetable-json').value = JSON.stringify(config.timetable, null, 2);
}

function updateStageDatalist() {
  const stages = [...new Set((config.timetable || []).map(st => st.stage))];
  $('timetable-stage-names').innerHTML = stages.map(s => `<option value="${escapeAttr(s)}">`).join('');
}

// maxVerticalStages: at or below this many *visible* stages for the selected
// day, the timetable renders as side-by-side columns (sets listed
// top-to-bottom, chronologically, like a printed festival day-schedule)
// instead of the horizontal per-stage timeline used above it - a handful of
// stages compressed into thin horizontal tracks reads poorly and wastes most
// of the panel as empty space, while a few columns of readable set cards fit
// comfortably.
const maxVerticalStages = 4;

// showEmptyTtStages toggles whether stages with no sets on the selected day
// are included - see the reveal button built in renderVisualTimetable.
let showEmptyTtStages = false;
function toggleEmptyTtStages() { showEmptyTtStages = !showEmptyTtStages; renderVisualTimetable(); }

// formatTtMinutes renders minutes-of-day back to a plain "HH:MM" wall-clock
// label - festivalMinutes may have pushed the value past 24*60 for an
// after-midnight set, so it's normalized back into 0..23:59 for display
// (nobody wants to read "26:30" on a set card).
function formatTtMinutes(minutes) {
  const h = Math.floor(minutes / 60) % 24;
  const m = minutes % 60;
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
}

function renderVisualTimetable() {
  updateStageDatalist();
  const days = timetableDays();
  if (!selectedTimetableDay || !days.includes(selectedTimetableDay)) selectedTimetableDay = days[0] || null;

  $('timetable-day-tabs').innerHTML = days.length
    ? days.map(d => `<button type="button" class="btn tt-day-tab ${d === selectedTimetableDay ? 'active' : ''}" onclick="selectTimetableDay('${d}')">${d}</button>`).join('')
    : '<span class="text-sm text-zinc-400">No dated sets yet. Import from timetable.lol or add one below.</span>';

  if (!config.timetable || !config.timetable.length) {
    $('timetable-visual').innerHTML = '<p class="text-sm text-zinc-400">No stages yet — add sources first, or import a timetable above.</p>';
    return;
  }

  const favIds = new Set(config.settings.favoriteSetIds || []);
  const day = selectedTimetableDay;

  // Each stage's sets that fall on the selected festival day (see
  // festivalDayOf), sorted chronologically - a set is grouped by which
  // festival day it belongs to, not its literal calendar date, so a 1am
  // afterparty stays attached to the night before it, not the next day's tab.
  const stageEntries = config.timetable.map((st, si) => {
    const sets = (st.sets || [])
      .map((set, seti) => ({ set, seti, sp: parseIso(set.start), ep: parseIso(set.end) }))
      .filter(x => x.sp && festivalDayOf(x.sp) === day)
      .sort((a, b) => festivalMinutes(a.sp, day) - festivalMinutes(b.sp, day));
    return { st, si, sets };
  });

  const withSets = stageEntries.filter(e => e.sets.length > 0);
  const emptyCount = stageEntries.length - withSets.length;
  let visible, toggleHtml = '';
  if (withSets.length === 0) {
    // Nothing has a set today yet on any stage - show every stage anyway, or
    // there'd be no way left to add the day's first set.
    visible = stageEntries;
  } else if (showEmptyTtStages) {
    visible = stageEntries;
    toggleHtml = `<button type="button" class="btn text-xs mb-2" onclick="toggleEmptyTtStages()">Hide ${emptyCount} empty stage${emptyCount === 1 ? '' : 's'}</button>`;
  } else {
    visible = withSets;
    if (emptyCount > 0) {
      toggleHtml = `<button type="button" class="btn text-xs mb-2" onclick="toggleEmptyTtStages()">+ ${emptyCount} stage${emptyCount === 1 ? '' : 's'} with no sets today</button>`;
    }
  }

  const useColumns = visible.length > 0 && visible.length <= maxVerticalStages;

  if (useColumns) {
    const colHtml = (e) => {
      const color = e.st.color || stagePalette(e.st.stage);
      const items = e.sets.map(x => {
        const spMin = festivalMinutes(x.sp, day);
        const starred = x.set.id && favIds.has(x.set.id);
        const timeLabel = x.ep ? `${formatTtMinutes(spMin)}–${formatTtMinutes(festivalMinutes(x.ep, day))}` : formatTtMinutes(spMin);
        return `<div class="tt-col-item" style="background:${color}">
          <button type="button" class="tt-star-btn ${starred ? 'active' : ''}" onclick="event.stopPropagation();toggleFavorite('${x.set.id || ''}')">&#9733;</button>
          <div onclick="editTimetableSet(${e.si},${x.seti})">
            <div class="tt-col-time">${timeLabel}</div>
            <div class="tt-col-name">${escapeHtml(x.set.name)}</div>
          </div>
        </div>`;
      }).join('') || '<p class="text-xs text-zinc-500">No sets yet.</p>';
      return `<div class="tt-col">
        <div class="tt-col-header" style="color:${color};border-color:${color}" title="${escapeAttr(e.st.stage)}">${escapeHtml(e.st.stage)}</div>
        ${items}
        <button type="button" class="btn w-full mt-1" onclick="addTimetableSet(${e.si})">+ Set</button>
      </div>`;
    };
    $('timetable-visual').innerHTML = toggleHtml + `<div class="tt-cols">${visible.map(colHtml).join('')}</div>`;
    return;
  }

  let minMin = 24 * 60, maxMin = 0, any = false;
  visible.forEach(e => e.sets.forEach(x => {
    any = true;
    const spMin = festivalMinutes(x.sp, day);
    const epMin = x.ep ? festivalMinutes(x.ep, day) : spMin + 60;
    minMin = Math.min(minMin, spMin);
    maxMin = Math.max(maxMin, epMin);
  }));
  if (!any) { minMin = 12 * 60; maxMin = 24 * 60; }
  const span = Math.max(60, maxMin - minMin);

  const rowHtml = (e) => {
    const color = e.st.color || stagePalette(e.st.stage);
    const blocks = e.sets.map(x => {
      const spMin = festivalMinutes(x.sp, day);
      const epMin = x.ep ? festivalMinutes(x.ep, day) : spMin + 60;
      const left = ((spMin - minMin) / span) * 100;
      const width = Math.max(3, ((epMin - spMin) / span) * 100);
      const starred = x.set.id && favIds.has(x.set.id);
      return `<div class="tt-block" style="left:${left}%;width:${width}%;background:${color}" title="${escapeAttr(x.set.name)}">
        <button type="button" class="tt-star-btn ${starred ? 'active' : ''}" onclick="event.stopPropagation();toggleFavorite('${x.set.id || ''}')">&#9733;</button>
        <span onclick="editTimetableSet(${e.si},${x.seti})">${escapeHtml(x.set.name)}</span>
      </div>`;
    }).join('');
    return `<div class="tt-row">
      <div class="tt-row-label" style="color:${color}" title="${escapeAttr(e.st.stage)}">${escapeHtml(e.st.stage)}</div>
      <div class="tt-row-track">${blocks}</div>
      <button type="button" class="btn" onclick="addTimetableSet(${e.si})">+ Set</button>
    </div>`;
  };
  $('timetable-visual').innerHTML = toggleHtml + visible.map(rowHtml).join('');
}

function selectTimetableDay(day) { selectedTimetableDay = day; renderVisualTimetable(); }

function editTimetableSet(si, seti) {
  const set = config.timetable[si].sets[seti];
  editingSet = { si, seti };
  $('tt-edit-name').value = set.name || '';
  $('tt-edit-start').value = (set.start || '').slice(0, 16);
  $('tt-edit-end').value = (set.end || '').slice(0, 16);
  $('tt-edit-delete').classList.remove('hidden');
  $('timetable-edit-form').classList.remove('hidden');
}

function addTimetableSet(si) {
  const day = selectedTimetableDay || new Date().toISOString().slice(0, 10);
  editingSet = { si, seti: -1 };
  $('tt-edit-name').value = '';
  $('tt-edit-start').value = `${day}T20:00`;
  $('tt-edit-end').value = `${day}T21:00`;
  $('tt-edit-delete').classList.add('hidden');
  $('timetable-edit-form').classList.remove('hidden');
}

function closeTimetableEdit() {
  editingSet = null;
  $('timetable-edit-form').classList.add('hidden');
}

function applyTimetableSetEdit() {
  if (!editingSet) return;
  const name = $('tt-edit-name').value.trim();
  const start = $('tt-edit-start').value;
  const end = $('tt-edit-end').value;
  if (!name || !start) { toast('Name and start time are required', 'error'); return; }
  const offset = festivalOffset();
  const stage = config.timetable[editingSet.si];
  stage.sets = stage.sets || [];
  const existing = editingSet.seti >= 0 ? stage.sets[editingSet.seti] : null;
  const set = { id: existing ? existing.id : undefined, name, start: `${start}:00${offset}`, end: end ? `${end}:00${offset}` : '' };
  if (editingSet.seti === -1) stage.sets.push(set);
  else stage.sets[editingSet.seti] = set;
  closeTimetableEdit();
  syncTimetableJsonFromConfig();
  renderVisualTimetable();
}

function deleteTimetableSet() {
  if (!editingSet || editingSet.seti === -1) return;
  if (!confirm('Delete this set?')) return;
  config.timetable[editingSet.si].sets.splice(editingSet.seti, 1);
  closeTimetableEdit();
  syncTimetableJsonFromConfig();
  renderVisualTimetable();
}

async function toggleFavorite(id) {
  if (!id) { toast('Save the timetable first so this set has a stable ID', 'error'); return; }
  const ids = new Set(config.settings.favoriteSetIds || []);
  if (ids.has(id)) ids.delete(id); else ids.add(id);
  config.settings.favoriteSetIds = [...ids];
  renderVisualTimetable();
  renderFavoritesPanel();
  try {
    await api('/api/timetable/favorites', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(config.settings.favoriteSetIds) });
  } catch {
    // toast already shown by api()
  }
}

// --- timetable.lol integration (optional) ---

// lolEventDateLabel prefers a real date (normalized server-side into
// displayStartDate/displayEndDate, whatever field name timetable.lol
// actually used) over just the year, so events from the same festival
// franchise in different years - or on different days within one edition -
// are distinguishable in the picker.
function lolEventDateLabel(e) {
  if (e.displayStartDate) {
    return e.displayEndDate && e.displayEndDate !== e.displayStartDate ? `${e.displayStartDate} – ${e.displayEndDate}` : e.displayStartDate;
  }
  return e.year ? String(e.year) : '';
}

async function loadLolEvents() {
  try {
    const result = await api('/api/timetable/lol-events');
    lolEvents = result.events || [];
    const options = lolEvents.map(e => ({ value: e.slug, label: `${e.title || e.slug}${lolEventDateLabel(e) ? ' (' + lolEventDateLabel(e) + ')' : ''}` }));
    setDropdownOptions('tt-lol-event', options, { value: options.length ? options[0].value : '', placeholder: 'No events found' });
    $('tt-lol-status').textContent = `${lolEvents.length} events available from timetable.lol.`;
  } catch {
    setDropdownOptions('tt-lol-event', [], { placeholder: 'Could not reach timetable.lol' });
    $('tt-lol-status').textContent = 'Could not reach timetable.lol — you can still build a timetable by hand below.';
  }
}
$('tt-lol-refresh').onclick = loadLolEvents;

async function importFromLol(slug) {
  if (!slug) { toast('Choose an event first', 'error'); return; }
  if (config.timetable && config.timetable.length && !confirm('This replaces your current timetable with the one imported from timetable.lol. Continue?')) return;
  $('tt-lol-status').textContent = 'Importing…';
  try {
    const result = await api('/api/timetable/lol-import', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ eventSlug: slug }) });
    toast(`Imported ${result.timetable.length} stages from timetable.lol${result.warnings && result.warnings.length ? ` (${result.warnings.length} warnings)` : ''}`, 'info');
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch {
    $('tt-lol-status').textContent = 'Import failed.';
  }
}
// Import a timetable from a local JSON file (app export format or the compact
// community format - the server accepts either). Paired with the timetables
// attached to each GitHub release.
$('tt-file-import').onclick = () => $('tt-file-input').click();
$('tt-file-input').addEventListener('change', async () => {
  const file = $('tt-file-input').files[0];
  $('tt-file-input').value = '';
  if (!file) return;
  if (config.timetable && config.timetable.length && !confirm(`Replace your current timetable with "${file.name}"?`)) return;
  $('tt-file-status').textContent = `Importing "${file.name}"…`;
  let text;
  try { text = await file.text(); } catch { $('tt-file-status').textContent = 'Could not read that file.'; return; }
  let result;
  try {
    result = await api(`/api/timetable/import?name=${encodeURIComponent(file.name)}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: text });
  } catch {
    $('tt-file-status').textContent = 'Import failed — is it a valid timetable JSON file?';
    return;
  }
  $('tt-file-status').textContent = `Imported ${result.stages} stage(s), ${result.sets} set(s).`;
  toast(`Imported ${result.stages} stage(s), ${result.sets} set(s) from "${file.name}"`, 'info');
  $('source-editor').dataset.loaded = '';
  await refresh();
});
$('tt-lol-import').onclick = () => importFromLol($('tt-lol-event').value);
$('tt-lol-resync').onclick = () => { if (config.timetableSource) importFromLol(config.timetableSource.eventSlug); };
$('tt-lol-unlink').onclick = async () => {
  if (!confirm('Forget the timetable.lol link? This keeps your current timetable data, just stops treating it as imported.')) return;
  try {
    await api('/api/timetable/lol-unlink', { method: 'POST' });
    $('source-editor').dataset.loaded = '';
    await refresh();
  } catch { /* toast already shown */ }
};

function renderLinkedBadge() {
  const link = config.timetableSource;
  const badge = $('tt-linked-badge');
  if (!link) {
    badge.classList.add('hidden');
    $('tt-lol-resync').classList.add('hidden');
    $('tt-lol-unlink').classList.add('hidden');
    return;
  }
  badge.classList.remove('hidden');
  badge.innerHTML = `Linked to <a href="${escapeAttr(link.sourceUrl)}" target="_blank" rel="noopener">${escapeHtml(link.eventTitle || link.eventSlug)}</a> · imported ${new Date(link.importedAt).toLocaleString()}`;
  $('tt-lol-resync').classList.remove('hidden');
  $('tt-lol-unlink').classList.remove('hidden');
}

// --- Saved timetables (snapshots taken on every import, so a previous one
// can be switched back to instantly, and what the Set Cutter falls back to
// for recordings from a source attached to an event) ---

function renderSavedTimetables() {
  const list = $('tt-saved-list');
  const saved = config.savedTimetables || [];
  if (!saved.length) {
    list.innerHTML = '<p class="text-sm text-zinc-400">No saved timetables yet - every timetable.lol or file import is snapshotted here automatically.</p>';
    return;
  }
  list.innerHTML = [...saved].reverse().map(s => `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div>
        <div class="font-medium">${escapeHtml(s.name)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(s.source || '')} · ${s.stages} stage(s), ${s.sets} set(s) · imported ${new Date(s.importedAt).toLocaleString()}</div>
      </div>
      <div class="flex gap-2">
        <button type="button" class="btn primary tt-saved-activate" data-id="${escapeAttr(s.id)}">Switch to this</button>
        <button type="button" class="btn tt-saved-delete" style="color:#fda4af" data-id="${escapeAttr(s.id)}">Delete</button>
      </div>
    </div>`).join('');

  list.querySelectorAll('.tt-saved-activate').forEach(el => el.addEventListener('click', () => activateSavedTimetable(el.dataset.id)));
  list.querySelectorAll('.tt-saved-delete').forEach(el => el.addEventListener('click', () => deleteSavedTimetable(el.dataset.id)));
}

async function activateSavedTimetable(id) {
  const snap = (config.savedTimetables || []).find(s => s.id === id);
  if (snap && config.timetable && config.timetable.length && !confirm(`Replace your current timetable with the saved "${snap.name}" snapshot?`)) return;
  try {
    await api(`/api/timetable/saved/${encodeURIComponent(id)}/activate`, { method: 'POST' });
  } catch {
    return;
  }
  toast(`Switched to saved timetable "${snap ? snap.name : id}"`, 'info');
  $('source-editor').dataset.loaded = '';
  await refresh();
}

async function deleteSavedTimetable(id) {
  if (!confirm('Delete this saved timetable snapshot? This does not affect the currently active timetable.')) return;
  try {
    await api(`/api/timetable/saved/${encodeURIComponent(id)}`, { method: 'DELETE' });
  } catch {
    return;
  }
  config.savedTimetables = (config.savedTimetables || []).filter(s => s.id !== id);
  renderSavedTimetables();
}

// --- Recordings Library (Events -> Channels -> Sets) ---
//
// Recordings are plain files scanned from FinishedDir (via /api/recordings),
// enriched server-side with whatever RecordingMeta has been assigned to them
// (eventId/channel/setId/artist/start/end). LibraryEvents themselves live in
// config.libraryEvents, so no separate fetch is needed for those - only the
// per-file metadata and each event's *archived* timetable (fetched lazily)
// require their own requests.

const UNSORTED_ID = '__unsorted__';
let libView = 'home'; // 'home' | 'event'
let libCurrentEventId = null;
let libEditingEventId = null;
let libTimetableEventId = null;
let libAssignTarget = null;
let libEventTimetableCache = {};

async function initLibrary() {
  try {
    recordings = await api('/api/recordings');
  } catch {
    return;
  }
  renderLibrary();
}

// --- Smart Match: filename/channel -> timetable set suggestions ---

let matchSuggestions = [];

$('lib-match-open').onclick = openMatchView;
$('lib-match-close').onclick = closeMatchView;

$('lib-folder-help-open').onclick = () => $('lib-folder-help-overlay').classList.remove('hidden');
$('lib-folder-help-close').onclick = () => $('lib-folder-help-overlay').classList.add('hidden');
$('lib-folder-help-close-2').onclick = () => $('lib-folder-help-overlay').classList.add('hidden');
$('lib-folder-help-overlay').addEventListener('click', (e) => { if (e.target.id === 'lib-folder-help-overlay') $('lib-folder-help-overlay').classList.add('hidden'); });
$('explorer-folder-help-open').onclick = () => $('lib-folder-help-overlay').classList.remove('hidden');

async function openMatchView() {
  $('lib-home').classList.add('hidden');
  $('lib-event-view').classList.add('hidden');
  $('lib-search-results').classList.add('hidden');
  $('lib-share-view').classList.add('hidden');
  $('lib-receive-view').classList.add('hidden');
  $('lib-transcode-view').classList.add('hidden');
  $('lib-back').classList.add('hidden');
  $('lib-match-view').classList.remove('hidden');
  $('lib-title').textContent = 'Recordings Library';
  $('lib-match-list').innerHTML = '<p class="text-zinc-400">Scanning…</p>';
  try {
    matchSuggestions = await api('/api/recordings/match-suggestions');
  } catch {
    matchSuggestions = [];
  }
  renderMatchList();
}

async function closeMatchView() {
  $('lib-match-view').classList.add('hidden');
  await reloadLibraryData();
}

// --- Hash-based match file: exact-match sibling to Smart Match, for sharing
// organized-recording metadata between different people's copies of the
// same files (matched by content hash, not path). ---

$('lib-matchfile-export').onclick = async () => {
  let entries;
  try {
    entries = await api('/api/recordings/matchfile/export');
  } catch {
    return;
  }
  if (!entries.length) {
    toast('Nothing organized yet to export', 'info');
    return;
  }
  const blob = new Blob([JSON.stringify(entries, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'recordings-matchfile.json';
  a.click();
  URL.revokeObjectURL(url);
  toast(`Exported ${entries.length} recording${entries.length === 1 ? '' : 's'}`, 'info');
};

// Import is a two-step flow: a dry run first (so the user can review exactly
// which local recordings would be organized, and as what), then the real
// apply only after confirmation. `matchfilePendingEntries` holds the parsed
// file between the two requests.
let matchfilePendingEntries = null;

$('lib-matchfile-import').onclick = () => $('lib-matchfile-input').click();
$('lib-matchfile-input').addEventListener('change', async () => {
  const file = $('lib-matchfile-input').files[0];
  $('lib-matchfile-input').value = '';
  if (!file) return;
  let entries;
  try {
    entries = JSON.parse(await file.text());
  } catch {
    toast('That file is not valid JSON', 'error');
    return;
  }
  if (!Array.isArray(entries)) {
    toast('Expected a match file (JSON array)', 'error');
    return;
  }
  let result;
  try {
    result = await api('/api/recordings/matchfile/import?dryRun=1', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(entries) });
  } catch {
    return;
  }
  if (!result.matched) {
    toast('No local recordings matched this file', 'warn');
    return;
  }
  matchfilePendingEntries = entries;
  $('matchfile-review-summary').textContent = `${result.matched} local recording${result.matched === 1 ? '' : 's'} matched this file and will be organized as follows:`;
  const dupes = $('matchfile-review-dupes');
  if (result.duplicates) {
    dupes.textContent = `${result.duplicates} entr${result.duplicates === 1 ? 'y' : 'ies'} in the file repeat${result.duplicates === 1 ? 's' : ''} an already-listed recording with different metadata - the first entry wins.`;
    dupes.classList.remove('hidden');
  } else {
    dupes.classList.add('hidden');
  }
  $('matchfile-review-list').innerHTML = (result.matches || []).map(m => {
    const target = [m.eventName, m.stageName, m.artist].filter(Boolean).join(' · ') || 'metadata only';
    return `<div class="matchfile-review-row"><div class="matchfile-review-path">${escapeHtml(m.path)}</div><div class="matchfile-review-target">→ ${escapeHtml(target)}</div></div>`;
  }).join('');
  $('matchfile-review-overlay').classList.remove('hidden');
});

function closeMatchfileReview() {
  $('matchfile-review-overlay').classList.add('hidden');
  matchfilePendingEntries = null;
}
$('matchfile-review-close').onclick = closeMatchfileReview;
$('matchfile-review-cancel').onclick = closeMatchfileReview;
$('matchfile-review-overlay').addEventListener('click', (e) => { if (e.target.id === 'matchfile-review-overlay') closeMatchfileReview(); });

$('matchfile-review-apply').onclick = async () => {
  const entries = matchfilePendingEntries;
  if (!entries) return;
  let result;
  try {
    result = await api('/api/recordings/matchfile/import', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(entries) });
  } catch {
    return;
  }
  closeMatchfileReview();
  toast(result.matched ? `Matched and organized ${result.matched} recording${result.matched === 1 ? '' : 's'}` : 'No local recordings matched this file', result.matched ? 'info' : 'warn');
  await reloadLibraryData();
};

// --- Peer-to-peer sharing (sender side): pick sets, generate a share code ---

let shareSelected = new Set(); // recording paths chosen to share

function openShareView() {
  ['lib-home', 'lib-event-view', 'lib-search-results', 'lib-back', 'lib-match-view', 'lib-receive-view', 'lib-transcode-view']
    .forEach(id => $(id) && $(id).classList.add('hidden'));
  $('lib-share-view').classList.remove('hidden');
  $('lib-title').textContent = 'Recordings Library';
  shareSelected = new Set();
  $('lib-share-code-box').classList.add('hidden');
  $('lib-share-name').value = '';
  $('lib-share-filter').value = '';
  const s = (config.settings && config.settings.sharing) || {};
  const warn = $('lib-share-setup-warn');
  if (!s.enabled || !s.publicUrl) {
    warn.textContent = 'Set up and verify a public URL in Settings → Peer Sharing before a share code will work.';
    warn.classList.remove('hidden');
  } else {
    warn.classList.add('hidden');
  }
  renderShareList();
  renderExistingShares();
}
function closeShareView() { $('lib-share-view').classList.add('hidden'); reloadLibraryData(); }

function updateShareSelCount() { $('lib-share-selcount').textContent = `${shareSelected.size} selected`; }

function shareFilteredRecordings() {
  const q = $('lib-share-filter').value.trim().toLowerCase();
  let list = recordings || [];
  if (q) list = list.filter(r => `${r.name} ${r.channel || ''} ${r.artist || ''}`.toLowerCase().includes(q));
  return list;
}

function renderShareList() {
  const list = shareFilteredRecordings();
  if (!list.length) {
    $('lib-share-list').innerHTML = '<p class="text-zinc-400">No recordings to share yet.</p>';
    updateShareSelCount();
    return;
  }
  // Group by channel for "select whole stage".
  const groups = new Map();
  list.forEach(r => { const ch = r.channel || 'Unsorted'; if (!groups.has(ch)) groups.set(ch, []); groups.get(ch).push(r); });
  $('lib-share-list').innerHTML = [...groups.entries()].map(([ch, items]) => `
    <div class="source-group open">
      <div class="source-group-head">
        <label class="flex items-center gap-2 font-semibold"><input type="checkbox" class="share-group-check" data-ch="${escapeAttr(ch)}"> ${escapeHtml(ch)}</label>
        <span class="pill">${items.length}</span>
      </div>
      <div class="source-group-body space-y-1">
        ${items.map(r => `
          <label class="flex items-center justify-between gap-2 rounded border border-white/10 px-2 py-1">
            <span class="flex min-w-0 items-center gap-2">
              <input type="checkbox" class="share-item-check" data-path="${escapeAttr(r.path)}" ${shareSelected.has(r.path) ? 'checked' : ''}>
              <span class="truncate">${escapeHtml(libDisplayTitle(r))}</span>
            </span>
            <span class="flex-shrink-0 text-xs text-zinc-500">${formatBytes(r.size)}</span>
          </label>`).join('')}
      </div>
    </div>`).join('');
  $('lib-share-list').querySelectorAll('.share-item-check').forEach(cb => cb.addEventListener('change', () => {
    if (cb.checked) shareSelected.add(cb.dataset.path); else shareSelected.delete(cb.dataset.path);
    updateShareSelCount();
  }));
  $('lib-share-list').querySelectorAll('.share-group-check').forEach(cb => cb.addEventListener('change', () => {
    const ch = cb.dataset.ch;
    (groups.get(ch) || []).forEach(r => { if (cb.checked) shareSelected.add(r.path); else shareSelected.delete(r.path); });
    renderShareList(); updateShareSelCount();
  }));
  updateShareSelCount();
}

async function renderExistingShares() {
  let shares;
  try { shares = await api('/api/shares'); } catch { return; }
  const box = $('lib-share-existing');
  if (!shares.length) { box.innerHTML = ''; return; }
  box.innerHTML = `<div class="mb-2 text-sm font-medium text-zinc-300">Active shares</div>` + shares.map(s => `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div class="min-w-0">
        <div class="font-medium">${escapeHtml(s.name || '(unnamed)')} <span class="pill">${s.count} file${s.count === 1 ? '' : 's'}</span></div>
        <div class="truncate text-xs text-zinc-500">${escapeHtml(s.code)}</div>
      </div>
      <div class="flex flex-shrink-0 gap-2">
        <button class="btn" onclick="copyShareCode('${escapeAttr(s.code)}')">Copy code</button>
        <button class="btn" style="color:#fda4af" onclick="revokeShare('${escapeAttr(s.token)}')">Revoke</button>
      </div>
    </div>`).join('');
}

function copyShareCode(code) { navigator.clipboard.writeText(code).then(() => toast('Share code copied', 'info')).catch(() => {}); }

async function revokeShare(token) {
  if (!confirm('Revoke this share? Its code will stop working immediately.')) return;
  try { await api(`/api/shares/${encodeURIComponent(token)}`, { method: 'DELETE' }); toast('Share revoked', 'info'); } catch { return; }
  renderExistingShares();
}

$('lib-share-open').onclick = openShareView;
$('lib-share-close').onclick = closeShareView;
$('lib-share-filter').addEventListener('input', renderShareList);
$('lib-share-selectall').onclick = () => { shareFilteredRecordings().forEach(r => shareSelected.add(r.path)); renderShareList(); };
$('lib-share-clear').onclick = () => { shareSelected = new Set(); renderShareList(); };
$('lib-share-copy').onclick = () => copyShareCode($('lib-share-code').value);
$('lib-share-create').onclick = async () => {
  if (!shareSelected.size) { toast('Select at least one recording to share', 'error'); return; }
  let result;
  try {
    result = await api('/api/shares', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: $('lib-share-name').value.trim(), paths: [...shareSelected] }) });
  } catch { return; }
  $('lib-share-code').value = result.code;
  $('lib-share-code-box').classList.remove('hidden');
  toast(`Share created with ${result.count} recording${result.count === 1 ? '' : 's'}`, 'info');
  renderExistingShares();
};

// --- Peer-to-peer sharing (receiver side): paste a code, preview, import ---

let receiveManifest = null;
let receiveSelected = new Set();

function openReceiveView() {
  ['lib-home', 'lib-event-view', 'lib-search-results', 'lib-back', 'lib-match-view', 'lib-share-view', 'lib-transcode-view']
    .forEach(id => $(id) && $(id).classList.add('hidden'));
  $('lib-receive-view').classList.remove('hidden');
  $('lib-title').textContent = 'Recordings Library';
  $('lib-receive-code').value = '';
  $('lib-receive-status').textContent = '';
  $('lib-receive-preview-box').classList.add('hidden');
  $('lib-receive-job-box').classList.add('hidden');
  stopReceiveJobPoll();
  $('lib-receive-import').disabled = false;
  receiveManifest = null;
  receiveSelected = new Set();
}
function closeReceiveView() {
  $('lib-receive-view').classList.add('hidden');
  stopReceiveJobPoll();
  $('lib-receive-job-box').classList.add('hidden');
  reloadLibraryData();
}

function updateReceiveSelCount() { $('lib-receive-selcount').textContent = `${receiveSelected.size} selected`; }

function renderReceiveList() {
  if (!receiveManifest) return;
  const items = receiveManifest.items || [];
  $('lib-receive-list').innerHTML = items.map(it => `
    <label class="flex items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <span class="flex min-w-0 items-center gap-2">
        <input type="checkbox" class="receive-item-check" data-index="${it.index}" ${receiveSelected.has(it.index) ? 'checked' : ''}>
        <span class="min-w-0">
          <span class="block truncate font-medium">${escapeHtml(it.artist || it.name)}</span>
          <span class="block truncate text-xs text-zinc-500">${escapeHtml(it.channel || '')}${it.eventName ? ' · ' + escapeHtml(it.eventName) : ''} · ${formatBytes(it.size)}${it.hasNfo ? ' · +nfo' : ''}</span>
        </span>
      </span>
    </label>`).join('');
  $('lib-receive-list').querySelectorAll('.receive-item-check').forEach(cb => cb.addEventListener('change', () => {
    const i = Number(cb.dataset.index);
    if (cb.checked) receiveSelected.add(i); else receiveSelected.delete(i);
    updateReceiveSelCount();
  }));
  updateReceiveSelCount();
}

$('lib-receive-open').onclick = openReceiveView;
$('lib-receive-close').onclick = closeReceiveView;
$('lib-receive-selectall').onclick = () => { (receiveManifest.items || []).forEach(it => receiveSelected.add(it.index)); renderReceiveList(); };
$('lib-receive-preview').onclick = async () => {
  const code = $('lib-receive-code').value.trim();
  if (!code) { toast('Paste a share code first', 'error'); return; }
  $('lib-receive-status').textContent = 'Fetching…';
  let result;
  try {
    result = await api('/api/share/preview', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code }) });
  } catch { $('lib-receive-status').textContent = ''; return; }
  if (!result.ok) { $('lib-receive-status').textContent = result.error || 'Could not read that share.'; $('lib-receive-preview-box').classList.add('hidden'); return; }
  receiveManifest = result.manifest;
  receiveSelected = new Set((receiveManifest.items || []).map(it => it.index));
  $('lib-receive-status').textContent = `"${receiveManifest.name || 'Share'}" — ${receiveManifest.items.length} recording(s):`;
  $('lib-receive-preview-box').classList.remove('hidden');
  renderReceiveList();
};
let receiveJobPollTimer = null;

// The import itself runs as a background job on the server (see
// handleShareImport) so a large transfer doesn't need this tab left open -
// this just starts it and polls for progress until it's done, but closing
// the tab mid-import loses nothing since the job keeps running server-side.
$('lib-receive-import').onclick = async () => {
  if (!receiveManifest) return;
  if (!receiveSelected.size) { toast('Select at least one recording to import', 'error'); return; }
  const code = $('lib-receive-code').value.trim();
  $('lib-receive-import').disabled = true;
  $('lib-receive-status').textContent = 'Starting import…';
  let result;
  try {
    result = await api('/api/share/import', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code, indices: [...receiveSelected] }) });
  } catch { $('lib-receive-status').textContent = 'Import failed to start.'; $('lib-receive-import').disabled = false; return; }
  if (!result.ok) { $('lib-receive-status').textContent = result.error || 'Import failed to start.'; $('lib-receive-import').disabled = false; return; }
  $('lib-receive-status').textContent = 'Import running in the background — you can navigate away, it will keep going.';
  $('lib-receive-job-box').classList.remove('hidden');
  pollReceiveJob(result.jobId);
};

function stopReceiveJobPoll() {
  if (receiveJobPollTimer) { clearTimeout(receiveJobPollTimer); receiveJobPollTimer = null; }
}

async function pollReceiveJob(jobId) {
  stopReceiveJobPoll();
  let job;
  try {
    job = await api(`/api/share/jobs/${encodeURIComponent(jobId)}`);
  } catch {
    receiveJobPollTimer = setTimeout(() => pollReceiveJob(jobId), 2000);
    return;
  }
  renderReceiveJob(job);
  if (job.status === 'running') {
    receiveJobPollTimer = setTimeout(() => pollReceiveJob(jobId), 1000);
  } else {
    $('lib-receive-import').disabled = false;
    toast(`Import ${job.status === 'error' ? 'failed' : 'finished'}: ${job.doneFiles} imported, ${job.skippedFiles} skipped, ${job.failedFiles} failed`, job.status === 'error' ? 'error' : 'info');
  }
}

function renderReceiveJob(job) {
  const pct = job.totalBytes > 0 ? Math.min(100, Math.round(job.transferredBytes / job.totalBytes * 100)) : 0;
  $('lib-receive-job-title').textContent = job.status === 'running' ? 'Importing…' : (job.status === 'error' ? 'Import failed' : 'Import finished');
  $('lib-receive-job-stats').textContent = `${formatBytes(job.transferredBytes)} / ${formatBytes(job.totalBytes)} · ${formatBytes(job.speedBps)}/s · ${job.doneFiles + job.skippedFiles + job.failedFiles}/${job.totalFiles} files`;
  $('lib-receive-job-bar').style.width = `${pct}%`;
  $('lib-receive-job-current').textContent = job.currentFile
    ? `${job.currentFile} (${formatBytes(job.currentFileBytes)} / ${formatBytes(job.currentFileTotal)})`
    : (job.error || '');
  $('lib-receive-job-log').textContent = (job.log || []).map(l => `[${l.time}] ${l.text}`).join('\n');
  $('lib-receive-job-log').scrollTop = $('lib-receive-job-log').scrollHeight;
}

function renderMatchList() {
  $('lib-match-count').textContent = `${matchSuggestions.length} unsorted`;
  if (!matchSuggestions.length) {
    $('lib-match-list').innerHTML = '<p class="text-zinc-400">Nothing to match — every recording is already organized (or there are none yet).</p>';
    return;
  }
  const badge = { high: 'text-emerald-300', medium: 'text-amber-300', low: 'text-rose-300', none: 'text-zinc-500' };
  $('lib-match-list').innerHTML = matchSuggestions.map((s, i) => `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div class="min-w-0">
        <div class="truncate font-medium">${escapeHtml(s.name)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(s.channel)}${s.eventName ? ' · ' + escapeHtml(s.eventName) : (s.newEventName ? ' · New event: ' + escapeHtml(s.newEventName) + (s.newEventYear ? ' ' + s.newEventYear : '') : '')}${s.artist ? ' · ' + escapeHtml(s.artist) : ''}</div>
        ${s.guessedArtist ? `<div class="text-xs text-zinc-500">Filename suggests: "${escapeHtml(s.guessedArtist)}"</div>` : ''}
        <div class="text-xs ${badge[s.confidence] || 'text-zinc-500'}">${escapeHtml(s.reason)}</div>
      </div>
      <div class="flex flex-shrink-0 items-center gap-2">
        ${(s.eventId || s.newEventName) ? `<button type="button" class="btn primary lib-match-approve" data-index="${i}">${s.newEventName ? 'Create Event & Approve' : 'Approve'}</button>` : ''}
        <button type="button" class="btn lib-match-edit" data-index="${i}">${s.eventId ? 'Edit' : 'Assign manually'}</button>
        <button type="button" class="btn lib-match-skip" data-index="${i}" style="color:#fda4af">Skip</button>
      </div>
    </div>`).join('');

  document.querySelectorAll('.lib-match-approve').forEach(btn => btn.addEventListener('click', () => approveSuggestion(parseInt(btn.dataset.index, 10))));
  document.querySelectorAll('.lib-match-edit').forEach(btn => btn.addEventListener('click', () => editSuggestion(parseInt(btn.dataset.index, 10))));
  document.querySelectorAll('.lib-match-skip').forEach(btn => btn.addEventListener('click', () => skipSuggestion(parseInt(btn.dataset.index, 10))));
}

async function approveSuggestion(i) {
  const s = matchSuggestions[i];
  try {
    let eventId = s.eventId;
    if (!eventId && s.newEventName) {
      const created = await api('/api/events', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: s.newEventName, year: s.newEventYear || undefined }) });
      eventId = created.id;
      await refresh(); // pick up the new event for the rest of this Smart Match session
    }
    await api('/api/recordings/meta', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: s.path, eventId, setId: s.setId, artist: s.artist, start: s.guessedTime }) });
    toast(`Organized "${s.name}"`, 'info');
  } catch {
    return;
  }
  matchSuggestions.splice(i, 1);
  renderMatchList();
}

function skipSuggestion(i) {
  matchSuggestions.splice(i, 1);
  renderMatchList();
}

async function editSuggestion(i) {
  const s = matchSuggestions[i];
  await refresh(); // make sure config.libraryEvents/festivals are current first
  openAssignModal({ path: s.path, name: s.name, channel: s.channel, source: s.channel, artist: s.artist, start: s.guessedTime, eventId: s.eventId });
  if (s.eventId && s.setId) {
    await populateAssignSetOptions();
    setDropdownValue('assign-set', s.setId);
  }
  // Taken off the pending list once the modal opens - assign-save persists
  // it from there, and re-running Smart Match picks it back up if canceled.
  matchSuggestions.splice(i, 1);
}

async function reloadLibraryData() {
  await refresh();
  try {
    recordings = await api('/api/recordings');
  } catch {
    return;
  }
  renderLibrary();
}

function libraryEvents() { return config.libraryEvents || []; }

function libEventById(id) {
  if (id === UNSORTED_ID) return { id: UNSORTED_ID, name: 'Unsorted', color: '#52525b', description: 'Recordings not yet assigned to an event.' };
  return libraryEvents().find(e => e.id === id) || null;
}

function libDisplayTitle(r) { return r.artist || r.name; }

function libEventDates(e) {
  if (e.startDate) return `${e.startDate}${e.endDate && e.endDate !== e.startDate ? ' – ' + e.endDate : ''}`;
  return e.year ? String(e.year) : '';
}

function renderLibrary() {
  // The Smart Match / Share / Receive overlays sit alongside the library views;
  // rendering the normal library always dismisses them.
  ['lib-match-view', 'lib-share-view', 'lib-receive-view', 'lib-transcode-view'].forEach(id => $(id) && $(id).classList.add('hidden'));
  const term = ($('lib-search').value || '').trim().toLowerCase();
  if (term) {
    renderLibrarySearch(term);
    return;
  }
  $('lib-search-results').classList.add('hidden');
  if (libView === 'event' && libCurrentEventId) {
    $('lib-home').classList.add('hidden');
    $('lib-event-view').classList.remove('hidden');
    $('lib-back').classList.remove('hidden');
    renderLibraryEventView(libCurrentEventId);
  } else {
    $('lib-event-view').classList.add('hidden');
    $('lib-home').classList.remove('hidden');
    $('lib-back').classList.add('hidden');
    $('lib-title').textContent = 'Recordings Library';
    renderLibraryHome();
  }
}
$('lib-search').addEventListener('input', renderLibrary);

$('lib-back').onclick = () => {
  libView = 'home';
  libCurrentEventId = null;
  $('lib-search').value = '';
  renderLibrary();
};

function renderLibraryHome() {
  const evs = [...libraryEvents()].sort((a, b) => (b.year || 0) - (a.year || 0) || a.name.localeCompare(b.name));
  const unsortedCount = recordings.filter(r => !r.eventId).length;
  const cards = [libEventCardHtml(libEventById(UNSORTED_ID), unsortedCount)];
  evs.forEach(e => cards.push(libEventCardHtml(e, recordings.filter(r => r.eventId === e.id).length)));
  $('lib-events-grid').innerHTML = cards.join('');
  document.querySelectorAll('.lib-event-card').forEach(card => {
    card.addEventListener('click', () => openLibraryEvent(card.dataset.id));
  });
}

function libEventCardHtml(e, count) {
  const bg = e.coverUrl
    ? `background-image:url('${escapeAttr(e.coverUrl)}');background-size:cover;background-position:center;`
    : `background:linear-gradient(160deg, ${e.color || 'var(--accent)'}66, rgb(0 0 0 / .45) 75%);`;
  return `
    <div class="lib-event-card" data-id="${escapeAttr(e.id)}" style="${bg}">
      <div class="lib-event-card-overlay">
        <div class="truncate font-semibold">${escapeHtml(e.name)}</div>
        <div class="text-xs text-zinc-300">${escapeHtml(libEventDates(e))}</div>
        <div class="text-xs text-zinc-400">${count} recording${count === 1 ? '' : 's'}</div>
      </div>
    </div>`;
}

function openLibraryEvent(id) {
  libView = 'event';
  libCurrentEventId = id;
  $('lib-search').value = '';
  renderLibrary();
}

function renderLibraryEventView(id) {
  const ev = libEventById(id);
  if (!ev) {
    libView = 'home';
    libCurrentEventId = null;
    renderLibrary();
    return;
  }
  $('lib-title').textContent = ev.name;
  $('lib-event-name').textContent = ev.name;
  $('lib-event-dates').textContent = libEventDates(ev);
  $('lib-event-desc').textContent = ev.description || '';
  $('lib-event-banner').style.borderLeftColor = ev.color || 'var(--accent)';
  const isReal = id !== UNSORTED_ID;
  $('lib-event-edit').classList.toggle('hidden', !isReal);
  $('lib-event-timetable').classList.toggle('hidden', !isReal);
  $('lib-event-delete').classList.toggle('hidden', !isReal);
  if (isReal) {
    $('lib-event-edit').onclick = () => openEventEditor(ev);
    $('lib-event-timetable').onclick = () => openEventTimetableImport(ev.id);
    $('lib-event-delete').onclick = () => deleteLibraryEvent(ev.id);
  }

  const rows = recordings.filter(r => (isReal ? r.eventId === id : !r.eventId));
  const byChannel = new Map();
  rows.forEach(r => {
    const ch = r.channel || r.source || 'Unknown';
    if (!byChannel.has(ch)) byChannel.set(ch, []);
    byChannel.get(ch).push(r);
  });
  const channels = [...byChannel.keys()].sort();
  if (!channels.length) {
    $('lib-channel-rows').innerHTML = `<p class="text-sm text-zinc-400">No recordings here yet${isReal ? ' — organize one from Unsorted to bring it into this event.' : '.'}</p>`;
    return;
  }
  $('lib-channel-rows').innerHTML = channels.map(ch => {
    const items = byChannel.get(ch).sort((a, b) => (a.start || a.modTime || '').localeCompare(b.start || b.modTime || ''));
    // Group by day (YYYY-MM-DD extracted from r.start, or fallback to mod date)
    const byDay = new Map();
    items.forEach(r => {
      let day = '';
      if (r.start) {
        day = r.start.slice(0, 10);
      } else {
        const d = new Date(r.modTime);
        day = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
      }
      if (!byDay.has(day)) byDay.set(day, []);
      byDay.get(day).push(r);
    });
    const days = [...byDay.keys()].sort();
    const dayHtml = days.length > 1
      ? days.map(day => {
          const label = new Date(day + 'T12:00:00').toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' });
          return `<div class="mb-3">
            <div class="mb-1 text-xs text-zinc-500 font-medium">${escapeHtml(label)}</div>
            <div class="lib-row-scroll">${byDay.get(day).map(r => libSetCardHtml(r)).join('')}</div>
          </div>`;
        }).join('')
      : `<div class="lib-row-scroll">${items.map(r => libSetCardHtml(r)).join('')}</div>`;
    return `
      <div class="lib-channel-row">
        <h3 class="mb-2 font-semibold">${escapeHtml(ch)}</h3>
        ${dayHtml}
      </div>`;
  }).join('');

  document.querySelectorAll('.lib-set-card').forEach(card => {
    const path = card.dataset.path;
    const r = rows.find(x => x.path === path);
    if (!r) return;
    card.querySelector('.lib-set-play').addEventListener('click', () => openRecordingPlayer(r.path, libDisplayTitle(r)));
    card.querySelector('.lib-set-organize').addEventListener('click', (e) => { e.stopPropagation(); openAssignModal(r); });
    card.querySelector('.lib-set-cut').addEventListener('click', (e) => { e.stopPropagation(); openCutterModal(r); });
  });
  observeThumbnails($('lib-channel-rows'));
}

function libSetCardHtml(r) {
  const title = escapeHtml(libDisplayTitle(r));
  const when = r.start
    ? new Date(r.start).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : new Date(r.modTime).toLocaleDateString();
  return `
    <div class="lib-set-card" data-path="${escapeAttr(r.path)}">
      <div class="lib-set-thumb lib-set-play">
        <img class="lib-set-thumb-img" data-thumb="${thumbnailUrl(r.path)}" alt="" onerror="this.style.display='none'">
        <span class="lib-set-play-icon">&#9658;</span>
        <button type="button" class="lib-set-cut" title="Set Cutter" aria-label="Set Cutter">&#9986;</button>
        <button type="button" class="lib-set-organize" title="Organize" aria-label="Organize">&#8942;</button>
      </div>
      <div class="lib-set-info">
        <div class="truncate text-sm font-medium">${title}</div>
        <div class="truncate text-xs text-zinc-400">${escapeHtml(when)} · ${formatBytes(r.size)}</div>
      </div>
    </div>`;
}

function renderLibrarySearch(term) {
  $('lib-home').classList.add('hidden');
  $('lib-event-view').classList.add('hidden');
  $('lib-back').classList.add('hidden');
  $('lib-title').textContent = 'Recordings Library';
  $('lib-search-results').classList.remove('hidden');
  const matches = recordings.filter(r => `${r.artist || ''} ${r.channel || r.source || ''} ${r.name}`.toLowerCase().includes(term)).slice(0, 200);
  if (!matches.length) {
    $('lib-search-list').innerHTML = `<p class="text-zinc-400">No recordings match "${escapeHtml(term)}".</p>`;
    return;
  }
  $('lib-search-list').innerHTML = matches.map(r => {
    const ev = r.eventId ? libEventById(r.eventId) : null;
    return `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div class="min-w-0">
        <div class="truncate font-medium">${escapeHtml(libDisplayTitle(r))}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(r.channel || r.source || '')} · ${ev ? escapeHtml(ev.name) : 'Unsorted'} · ${formatBytes(r.size)}</div>
      </div>
      <div class="flex flex-shrink-0 items-center gap-2">
        <button type="button" class="btn primary lib-search-play" data-path="${escapeAttr(r.path)}">&#9658; Play</button>
        <button type="button" class="btn lib-search-cut" data-path="${escapeAttr(r.path)}">Cut</button>
        <button type="button" class="btn lib-search-organize" data-path="${escapeAttr(r.path)}">Organize</button>
        <a class="btn" href="/media/${encodeMediaPath(r.path)}" download title="Download">&#8681;</a>
      </div>
    </div>`;
  }).join('');
  document.querySelectorAll('.lib-search-play').forEach(btn => btn.addEventListener('click', () => {
    const r = recordings.find(x => x.path === btn.dataset.path);
    if (r) openRecordingPlayer(r.path, libDisplayTitle(r));
  }));
  document.querySelectorAll('.lib-search-organize').forEach(btn => btn.addEventListener('click', () => {
    const r = recordings.find(x => x.path === btn.dataset.path);
    if (r) openAssignModal(r);
  }));
  document.querySelectorAll('.lib-search-cut').forEach(btn => btn.addEventListener('click', () => {
    const r = recordings.find(x => x.path === btn.dataset.path);
    if (r) openCutterModal(r);
  }));
}

// --- Event create/edit modal ---

function openEventEditor(ev) {
  libEditingEventId = ev ? ev.id : null;
  $('event-editor-title').textContent = ev ? 'Edit Event' : 'New Event';
  $('ev-name').value = ev ? ev.name : '';
  $('ev-year').value = ev && ev.year ? ev.year : '';
  $('ev-start').value = ev ? (ev.startDate || '') : '';
  $('ev-end').value = ev ? (ev.endDate || '') : '';
  $('ev-color').value = (ev && ev.color) || '#ef4444';
  $('ev-cover').value = ev ? (ev.coverUrl || '') : '';
  syncImageUploadPreview('ev-cover');
  $('ev-desc').value = ev ? (ev.description || '') : '';
  // Populate festival (franchise) dropdown; setDropdownOptions creates the hidden input inside the container
  const festOpts = [{ value: '', label: 'None' }, ...(config.festivals || []).map(f => ({ value: f.id, label: f.name }))];
  setDropdownOptions('ev-festival', festOpts, { value: ev ? (ev.festivalId || '') : '' });
  setDropdownValue('ev-festival', ev ? (ev.festivalId || '') : '');
  $('event-editor-error').classList.add('hidden');
  $('event-editor-overlay').classList.remove('hidden');
}
function closeEventEditor() { $('event-editor-overlay').classList.add('hidden'); }
$('event-editor-close').onclick = closeEventEditor;
$('event-editor-overlay').addEventListener('click', (e) => { if (e.target.id === 'event-editor-overlay') closeEventEditor(); });
$('lib-new-event').onclick = () => openEventEditor(null);

$('event-editor-save').onclick = async () => {
  const name = $('ev-name').value.trim();
  if (!name) {
    $('event-editor-error').textContent = 'Name is required';
    $('event-editor-error').classList.remove('hidden');
    return;
  }
  const payload = {
    name,
    year: parseInt($('ev-year').value, 10) || 0,
    startDate: $('ev-start').value,
    endDate: $('ev-end').value,
    color: $('ev-color').value,
    coverUrl: $('ev-cover').value.trim(),
    description: $('ev-desc').value.trim(),
    festivalId: ($('ev-festival') || {}).value || '',
  };
  try {
    if (libEditingEventId) {
      await api(`/api/events/${libEditingEventId}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Saved "${name}"`, 'info');
    } else {
      const created = await api('/api/events', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Created "${name}"`, 'info');
      libView = 'event';
      libCurrentEventId = created.id;
    }
  } catch {
    return;
  }
  closeEventEditor();
  await reloadLibraryData();
};

async function deleteLibraryEvent(id) {
  const ev = libEventById(id);
  if (!ev) return;
  if (!confirm(`Delete event "${ev.name}"? Recordings assigned to it become Unsorted, not deleted.`)) return;
  try {
    await api(`/api/events/${id}`, { method: 'DELETE' });
    toast(`Deleted "${ev.name}"`, 'info');
  } catch {
    return;
  }
  libView = 'home';
  libCurrentEventId = null;
  await reloadLibraryData();
}

// --- Archived timetable import modal ---

function openEventTimetableImport(eventId) {
  libTimetableEventId = eventId;
  $('event-timetable-json').value = '';
  $('event-timetable-error').classList.add('hidden');
  $('event-timetable-status').textContent = '';
  $('event-timetable-overlay').classList.remove('hidden');
}
function closeEventTimetableImport() { $('event-timetable-overlay').classList.add('hidden'); }
$('event-timetable-close').onclick = closeEventTimetableImport;
$('event-timetable-overlay').addEventListener('click', (e) => { if (e.target.id === 'event-timetable-overlay') closeEventTimetableImport(); });

$('event-timetable-import-btn').onclick = async () => {
  const raw = $('event-timetable-json').value.trim();
  $('event-timetable-error').classList.add('hidden');
  if (!raw) {
    $('event-timetable-error').textContent = 'Paste the timetable JSON first';
    $('event-timetable-error').classList.remove('hidden');
    return;
  }
  try {
    JSON.parse(raw);
  } catch (err) {
    $('event-timetable-error').textContent = `Invalid JSON: ${err.message}`;
    $('event-timetable-error').classList.remove('hidden');
    return;
  }
  let tt;
  try {
    tt = await api(`/api/events/${libTimetableEventId}/timetable`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: raw });
  } catch {
    return;
  }
  delete libEventTimetableCache[libTimetableEventId];
  const stageCount = tt.length;
  const setCount = tt.reduce((n, s) => n + (s.sets || []).length, 0);
  $('event-timetable-status').textContent = `Imported ${stageCount} stage(s), ${setCount} set(s).`;
  toast('Archived timetable imported', 'info');
};

// --- Organize/assign modal: link a recording to an Event + artist/set ---

async function fetchEventTimetable(eventId) {
  if (!eventId || eventId === UNSORTED_ID) return [];
  if (libEventTimetableCache[eventId]) return libEventTimetableCache[eventId];
  try {
    const tt = await api(`/api/events/${eventId}/timetable`);
    libEventTimetableCache[eventId] = tt || [];
  } catch {
    libEventTimetableCache[eventId] = [];
  }
  return libEventTimetableCache[eventId];
}

function toDatetimeLocal(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const pad = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function openAssignModal(r) {
  libAssignTarget = r;
  $('assign-title').textContent = `Organize: ${r.name}`;
  $('assign-channel').value = (r.channel && r.channel !== r.source) ? r.channel : '';
  $('assign-artist').value = r.artist || '';
  $('assign-time').value = r.start ? toDatetimeLocal(r.start) : '';
  $('assign-tracklist').value = r.tracklist || '';
  $('assign-find-result').textContent = '';
  $('assign-error').classList.add('hidden');
  refreshAssignThumbPreview();

  setDropdownOptions('assign-event',
    [{ value: '', label: 'Unsorted' }, ...libraryEvents().map(e => ({ value: e.id, label: e.name }))],
    { value: r.eventId || '' });

  setAssignMode('artist');
  populateAssignSetOptions();
  $('assign-overlay').classList.remove('hidden');
}
function closeAssignModal() { $('assign-overlay').classList.add('hidden'); libAssignTarget = null; }
$('assign-close').onclick = closeAssignModal;
$('assign-overlay').addEventListener('click', (e) => { if (e.target.id === 'assign-overlay') closeAssignModal(); });

// Reloads the Organize modal's thumbnail preview from scratch (bumping a
// cache-busting query param since the URL is derived from the recording's
// path, not its content, so the browser would otherwise keep showing a
// stale/removed image after a regenerate or upload).
function refreshAssignThumbPreview() {
  if (!libAssignTarget) return;
  const img = $('assign-thumb-preview');
  img.classList.add('hidden');
  img.onload = () => { img.classList.remove('hidden'); $('assign-thumb-remove-btn').classList.remove('hidden'); $('assign-thumb-regen-btn').classList.remove('hidden'); };
  img.onerror = () => { img.classList.add('hidden'); $('assign-thumb-remove-btn').classList.add('hidden'); $('assign-thumb-regen-btn').classList.add('hidden'); };
  img.src = `${thumbnailUrl(libAssignTarget.path)}&t=${Date.now()}`;
}

$('assign-thumb-upload-btn').onclick = () => $('assign-thumb-file').click();
$('assign-thumb-file').onchange = async () => {
  const file = $('assign-thumb-file').files[0];
  $('assign-thumb-file').value = '';
  if (!file || !libAssignTarget) return;
  const form = new FormData();
  form.append('image', file);
  try {
    await api(`/api/recordings/thumbnail?path=${encodeURIComponent(libAssignTarget.path)}`, { method: 'POST', body: form });
    toast('Thumbnail updated', 'info');
  } catch { return; }
  refreshAssignThumbPreview();
};
$('assign-thumb-regen-btn').onclick = async () => {
  if (!libAssignTarget) return;
  try {
    await api(`/api/recordings/thumbnail/regenerate?path=${encodeURIComponent(libAssignTarget.path)}`, { method: 'POST' });
    toast('Thumbnail regenerated', 'info');
  } catch { return; }
  refreshAssignThumbPreview();
};
$('assign-thumb-remove-btn').onclick = async () => {
  if (!libAssignTarget) return;
  try {
    await api(`/api/recordings/thumbnail?path=${encodeURIComponent(libAssignTarget.path)}`, { method: 'DELETE' });
    toast('Thumbnail removed', 'info');
  } catch { return; }
  refreshAssignThumbPreview();
};

function setAssignMode(mode) {
  document.querySelectorAll('.assign-mode-btn').forEach(b => b.classList.toggle('active', b.dataset.mode === mode));
  $('assign-mode-artist').classList.toggle('hidden', mode !== 'artist');
  $('assign-mode-time').classList.toggle('hidden', mode !== 'time');
}
document.querySelectorAll('.assign-mode-btn').forEach(b => b.addEventListener('click', () => setAssignMode(b.dataset.mode)));

// assign-event and assign-set are hidden inputs that don't exist in the DOM
// until setDropdownOptions() first builds them (when the modal opens), so
// their 'change' listeners are delegated from the always-present modal
// panel instead of attached directly at page-load time.
$('assign-overlay').addEventListener('change', (e) => {
  if (e.target.id === 'assign-event') populateAssignSetOptions();
  if (e.target.id === 'assign-set' && e.target.value) $('assign-artist').value = e.target.dataset.name || '';
});

async function populateAssignSetOptions() {
  const eventId = $('assign-event').value;
  if (!eventId) {
    setDropdownOptions('assign-set', [], { placeholder: 'Choose an event first' });
    return;
  }
  setDropdownOptions('assign-set', [], { placeholder: 'Loading…' });
  const tt = await fetchEventTimetable(eventId);
  if (!tt.length) {
    setDropdownOptions('assign-set', [], { placeholder: 'No timetable imported for this event yet' });
    return;
  }
  const options = [];
  tt.forEach(stage => (stage.sets || []).forEach(set => {
    const when = set.start ? new Date(set.start).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : '';
    options.push({ value: set.id, label: `${stage.stage} · ${when} · ${set.name}`, name: set.name });
  }));
  setDropdownOptions('assign-set', options, { placeholder: '— choose a set —' });
}

$('assign-find-btn').onclick = async () => {
  const eventId = $('assign-event').value;
  const timeVal = $('assign-time').value;
  if (!eventId) { $('assign-find-result').textContent = 'Choose an event first.'; return; }
  if (!timeVal) { $('assign-find-result').textContent = 'Enter a time first.'; return; }
  const tt = await fetchEventTimetable(eventId);
  if (!tt.length) { $('assign-find-result').textContent = "This event has no imported timetable yet."; return; }
  const target = new Date(timeVal).getTime();
  let best = null, bestDelta = Infinity, bestStage = '';
  tt.forEach(stage => (stage.sets || []).forEach(set => {
    const start = new Date(set.start).getTime();
    const end = set.end ? new Date(set.end).getTime() : start + 3600000;
    const delta = (target >= start && target < end) ? 0 : Math.min(Math.abs(target - start), Math.abs(target - end));
    if (delta < bestDelta) { bestDelta = delta; best = set; bestStage = stage.stage; }
  }));
  if (!best) { $('assign-find-result').textContent = 'No sets found in this timetable.'; return; }
  $('assign-artist').value = best.name;
  setDropdownValue('assign-set', best.id);
  const mins = Math.round(bestDelta / 60000);
  $('assign-find-result').textContent = mins === 0
    ? `Matched: ${bestStage} · ${best.name} (within this set's window)`
    : `Closest match: ${bestStage} · ${best.name} (${mins} min away)`;
};

$('assign-clear').onclick = async () => {
  if (!libAssignTarget) return;
  try {
    await api(`/api/recordings/meta?path=${encodeURIComponent(libAssignTarget.path)}`, { method: 'DELETE' });
    toast('Unassigned', 'info');
  } catch {
    return;
  }
  closeAssignModal();
  await reloadLibraryData();
};

$('assign-save').onclick = async () => {
  if (!libAssignTarget) return;
  const setId = $('assign-set').value;
  const payload = {
    path: libAssignTarget.path,
    eventId: $('assign-event').value,
    channel: $('assign-channel').value.trim(),
    setId,
    artist: $('assign-artist').value.trim(),
    tracklist: $('assign-tracklist').value.trim(),
  };
  if (!setId && $('assign-time').value) {
    payload.start = new Date($('assign-time').value).toISOString();
  }
  try {
    await api('/api/recordings/meta', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
    toast('Saved', 'info');
  } catch {
    return;
  }
  closeAssignModal();
  await reloadLibraryData();
};

// --- Custom video player (Recordings tab) ---
//
// Video.js is the playback engine (autoplay/HLS-capable tech, robust
// cross-browser fullscreen handling), but its own control bar is disabled
// (`controls: false`) in favor of this app's own dark/accent-colored
// control bar, wired directly to the Player API instead of a raw <video>.

let currentPlaybackUrl = '';

function ensureRecPlayer() {
  if (recPlayer) return recPlayer;
  recPlayer = videojs('rec-video', { controls: false, autoplay: false, preload: 'auto', bigPlayButton: false, fluid: false });
  setupCustomPlayerControls(recPlayer);
  return recPlayer;
}

function openRecordingPlayer(path, name) {
  // Track where we came from so the back button returns there. Don't overwrite it
  // when navigating between recommendations while already on the player view.
  const activeNav = document.querySelector('.nav.active');
  const playerEl = $('player');
  if (activeNav) playerPreviousView = activeNav.dataset.view;
  else if (playerEl && playerEl.classList.contains('hidden')) playerPreviousView = 'recordings';
  $('rec-player-title').textContent = name || 'Recording';
  switchToView('player');

  const player = ensureRecPlayer();
  currentPlaybackUrl = `/media/${encodeMediaPath(path)}`;
  player.src({ src: currentPlaybackUrl });
  player.currentTime(0);
  player.play().catch(() => {});

  // Download link
  $('rec-player-download').href = currentPlaybackUrl;

  // Details panel
  const r = recordings.find(x => x.path === path);
  renderRecordingDetails(r);

  // Recommendations sidebar
  renderRecordingRecommendations(r);
}

function renderRecordingDetails(r) {
  const dl = $('rec-details');
  if (!r) { dl.innerHTML = ''; return; }
  const ev = r.eventId ? (config.libraryEvents || []).find(e => e.id === r.eventId) : null;
  const rows = [
    r.artist && ['Artist', r.artist],
    r.channel && ['Channel', r.channel],
    ev && ['Event', ev.name],
    r.start && ['Start', new Date(r.start).toLocaleString()],
    r.end && ['End', new Date(r.end).toLocaleString()],
    ['File', r.name],
    ['Size', formatBytes(r.size)],
  ].filter(Boolean);
  dl.innerHTML = rows.map(([k, v]) => `<div class="flex gap-2"><dt class="w-16 flex-shrink-0 text-zinc-400">${escapeHtml(k)}</dt><dd class="min-w-0 truncate">${escapeHtml(String(v))}</dd></div>`).join('');
  const tracklist = r.tracklist;
  if (tracklist) {
    $('rec-tracklist').textContent = tracklist;
    $('rec-tracklist-panel').classList.remove('hidden');
  } else {
    $('rec-tracklist-panel').classList.add('hidden');
  }
}

function renderRecordingRecommendations(r) {
  const panel = $('rec-more-panel');
  const list = $('rec-more-list');
  if (!r) { panel.classList.add('hidden'); return; }
  // Find up to 10 recordings from the same channel/event
  const channel = r.channel || r.source;
  const related = recordings.filter(x => x.path !== r.path && (
    (channel && (x.channel || x.source) === channel) ||
    (r.eventId && x.eventId === r.eventId)
  )).slice(0, 10);
  if (!related.length) { panel.classList.add('hidden'); return; }
  $('rec-more-title').textContent = channel ? `More from ${channel}` : 'More from this event';
  list.innerHTML = related.map(x => libSetCardHtml(x)).join('');
  list.querySelectorAll('.lib-set-card').forEach(card => {
    const path = card.dataset.path;
    const rec = recordings.find(x => x.path === path);
    if (!rec) return;
    card.querySelector('.lib-set-play').addEventListener('click', () => openRecordingPlayer(rec.path, libDisplayTitle(rec)));
    card.querySelector('.lib-set-organize').addEventListener('click', (e) => { e.stopPropagation(); openAssignModal(rec); });
  });
  observeThumbnails(list);
  panel.classList.remove('hidden');
}

function closeRecordingPlayer() {
  if (recPlayer) recPlayer.pause();
  stopVisualizer();
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
  switchToView(playerPreviousView);
}

$('rec-player-close').onclick = closeRecordingPlayer;
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('player').classList.contains('hidden')) closeRecordingPlayer();
});

function formatTime(s) {
  if (!isFinite(s) || s < 0) return '0:00';
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = Math.floor(s % 60);
  const mm = h ? String(m).padStart(2, '0') : String(m);
  const ss = String(sec).padStart(2, '0');
  return h ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}

// Bound once: ensureRecPlayer() only creates the Player instance the first
// time it's needed, so controls only ever get wired up once too.
function setupCustomPlayerControls(player) {
  const playPauseBtn = $('cp-playpause');
  const centerPlay = $('cp-center-play');
  const back10 = $('cp-back10');
  const fwd10 = $('cp-fwd10');
  const timeEl = $('cp-time');
  const seek = $('cp-seek');
  const seekWrap = $('cp-seek-wrap');
  const seekProgress = $('cp-seek-progress');
  const seekBuffer = $('cp-seek-buffer');
  const muteBtn = $('cp-mute');
  const volume = $('cp-volume');
  const speed = $('cp-speed');
  const fullscreenBtn = $('cp-fullscreen');
  let scrubbing = false;

  const setPlayIcon = (playing) => {
    playPauseBtn.innerHTML = playing ? '&#10074;&#10074;' : '&#9658;';
  };

  const togglePlay = () => { if (player.paused() || player.ended()) player.play().catch(() => {}); else player.pause(); };

  playPauseBtn.onclick = togglePlay;
  centerPlay.onclick = togglePlay;
  player.el().addEventListener('click', togglePlay);
  player.on('play', () => { setPlayIcon(true); centerPlay.classList.add('hidden'); });
  player.on('pause', () => { setPlayIcon(false); centerPlay.classList.remove('hidden'); });
  player.on('ended', () => { setPlayIcon(false); centerPlay.classList.remove('hidden'); });
  player.on('waiting', () => $('custom-player').classList.add('cp-buffering'));
  player.on('playing', () => $('custom-player').classList.remove('cp-buffering'));
  player.on('error', () => toast('Could not play this recording — the file may be missing or unsupported', 'error'));

  back10.onclick = () => player.currentTime(Math.max(0, player.currentTime() - 10));
  fwd10.onclick = () => player.currentTime(Math.min(player.duration() || player.currentTime() + 10, player.currentTime() + 10));

  player.on('timeupdate', () => {
    const duration = player.duration();
    if (scrubbing || !isFinite(duration) || !duration) return;
    const pct = (player.currentTime() / duration) * 1000;
    seek.value = pct;
    seekProgress.style.width = `${pct / 10}%`;
    timeEl.textContent = `${formatTime(player.currentTime())} / ${formatTime(duration)}`;
  });
  player.on('progress', () => {
    const duration = player.duration();
    const bufferedEnd = player.bufferedEnd();
    if (isFinite(duration) && duration && bufferedEnd) {
      seekBuffer.style.width = `${(bufferedEnd / duration) * 100}%`;
    }
  });
  player.on('loadedmetadata', () => {
    const duration = player.duration();
    timeEl.textContent = `${formatTime(0)} / ${formatTime(duration)}`;
    // A file with no video track decodes to 0x0 dimensions regardless of
    // container/extension - checking this (rather than guessing from the
    // file name) is what reliably tells an audio-only recording apart from
    // a video one, so the waveform/visualizer only show up when there's
    // nothing to actually look at otherwise.
    const isAudioOnly = player.videoWidth() === 0 && player.videoHeight() === 0;
    $('custom-player').classList.toggle('audio-mode', isAudioOnly);
    $('cp-audio-stage').classList.toggle('hidden', !isAudioOnly);
    if (isAudioOnly) {
      setupWaveform(currentPlaybackUrl, techVideoEl(player));
      startVisualizer(techVideoEl(player));
    } else {
      stopVisualizer();
    }
  });

  seek.addEventListener('input', () => {
    scrubbing = true;
    const pct = seek.value / 1000;
    seekProgress.style.width = `${pct * 100}%`;
    const duration = player.duration();
    if (isFinite(duration) && duration) timeEl.textContent = `${formatTime(pct * duration)} / ${formatTime(duration)}`;
  });
  seek.addEventListener('change', () => {
    const duration = player.duration();
    if (isFinite(duration) && duration) player.currentTime((seek.value / 1000) * duration);
    scrubbing = false;
  });
  seekWrap.addEventListener('mousedown', () => { scrubbing = true; });

  volume.addEventListener('input', () => {
    player.volume(parseFloat(volume.value));
    player.muted(player.volume() === 0);
    muteBtn.innerHTML = player.muted() ? '&#128263;' : '&#128266;';
  });
  muteBtn.onclick = () => {
    player.muted(!player.muted());
    muteBtn.innerHTML = player.muted() ? '&#128263;' : '&#128266;';
    if (!player.muted() && player.volume() === 0) { player.volume(1); volume.value = 1; }
  };

  speed.addEventListener('change', () => { player.playbackRate(parseFloat(speed.value)); });

  populateVisualizerPresetSelect('cp-visualizer');
  $('cp-visualizer-next').addEventListener('click', () => nextVisualizerPreset('cp-visualizer', true));

  setupPiP($('cp-pip'), () => techVideoEl(player));

  fullscreenBtn.onclick = () => {
    if (player.isFullscreen()) player.exitFullscreen();
    else player.requestFullscreen();
  };
}

// --- Audio-only playback: interactive waveform + music visualizer ---
//
// The waveform is WaveSurfer bound directly to the same <video> element via
// its `media` option, so it's a real position/seek control (drag to scrub)
// rather than a decorative copy - clicking it seeks the actual element,
// no separate/duplicate audio path involved. The visualizer is a plain
// Web Audio API AnalyserNode reading the same element's output through a
// MediaElementSourceNode, drawn as a bar spectrum on a canvas.

let cpWave = null;

function setupWaveform(url, videoEl) {
  if (!window.WaveSurfer) return;
  if (cpWave) { cpWave.destroy(); cpWave = null; }
  // Passing `url` alongside `media` at construction time makes WaveSurfer
  // manage (and briefly reset) the element's own src while it loads, which
  // stomps on the playback we already started - creating first and loading
  // separately, exactly like the existing Dashboard waveform does, avoids
  // that without giving up the `media` binding (still the same element, so
  // no second/duplicate audio path and the waveform stays a real seek
  // control instead of a decorative copy).
  cpWave = WaveSurfer.create({
    container: '#cp-wave',
    height: 56,
    waveColor: '#52525b',
    progressColor: accents[config.ui.accent] || accents.red,
    cursorColor: '#f4f4f5',
    barWidth: 2,
    barGap: 1,
    media: videoEl,
  });
  cpWave.load(url);
}

// The audio graph (AudioContext + AnalyserNode + MediaElementSourceNode) can
// only ever be built once per <video> element - a second
// createMediaElementSource call on the same element throws - so each one is
// built lazily on first use and cached per canvas id (there are two
// independent visualizers in the app: the Recordings player's #cp-visualizer
// and the Watch tab's #watch-visualizer, each bound to its own video element).
const vizInstances = {};

// Butterchurn (a WebGL port of the Winamp MilkDrop plugin) renders the real
// thing; plain 2D canvas bars are only a fallback for browsers/contexts
// without WebGL2 (e.g. software-only remote desktops). Presets are loaded
// once, lazily, and shared read-only across every visualizer instance.
let vizPresetNames = null;
function vizPresets() {
  if (vizPresetNames) return vizPresetNames;
  // The UMD builds are webpack/babel output with esModuleInterop - depending
  // on version the real API can land directly on the global or nested under
  // `.default`, so probe both rather than assuming one shape.
  const mod = window.butterchurnPresets;
  const api = mod && (typeof mod.getPresets === 'function' ? mod : mod.default);
  if (!api) return (vizPresetNames = []);
  const presets = api.getPresets();
  vizPresetNames = Object.keys(presets).map(name => ({ name, preset: presets[name] }));
  return vizPresetNames;
}

function ensureVizGraph(videoEl, canvasId) {
  const existing = vizInstances[canvasId];
  if (existing && existing.sourceEl === videoEl) return existing;
  const AudioCtx = window.AudioContext || window.webkitAudioContext;
  if (!AudioCtx) return null;
  try {
    const audioCtx = new AudioCtx();
    const source = audioCtx.createMediaElementSource(videoEl);
    source.connect(audioCtx.destination);
    const inst = {
      audioCtx, source, raf: null, sourceEl: videoEl,
      butterchurn: null, presetIndex: -1, resizeObserver: null,
      analyser: null, data: null,
    };
    vizInstances[canvasId] = inst;
    return inst;
  } catch {
    return null;
  }
}

function resizeVizCanvas(canvas, butterchurnViz) {
  const w = canvas.clientWidth, h = canvas.clientHeight;
  if (canvas.width === w && canvas.height === h) return;
  canvas.width = w;
  canvas.height = h;
  if (butterchurnViz) butterchurnViz.setRendererSize(w, h);
}

// The bars fallback gets its own canvas (see index.html: `${canvasId}-bars`)
// rather than reusing the MilkDrop one - a canvas permanently locks to
// whichever context type ('webgl2' vs '2d') is first *successfully* created
// on it, so switching between MilkDrop and bars at runtime (not just once at
// startup as a failure fallback) requires two separate elements toggled by
// visibility rather than one canvas juggling two context types.
function barsCanvasId(canvasId) { return canvasId + '-bars'; }

// value is either '__bars__' or a MilkDrop preset name from vizPresets().
function setVisualizerPreset(canvasId, value) {
  canvasId = canvasId || 'cp-visualizer';
  const inst = vizInstances[canvasId];
  if (!inst) return;
  if (value === '__bars__') {
    inst.mode = 'bars';
    renderVisualizer(canvasId);
    return;
  }
  const presets = vizPresets();
  const idx = presets.findIndex(p => p.name === value);
  if (idx < 0) return;
  inst.presetIndex = idx;
  if (inst.mode === 'milkdrop' && inst.butterchurn) {
    inst.butterchurn.loadPreset(presets[idx].preset, 1.5);
    return;
  }
  inst.mode = 'milkdrop';
  renderVisualizer(canvasId);
}

function nextVisualizerPreset(canvasId, random) {
  canvasId = canvasId || 'cp-visualizer';
  const inst = vizInstances[canvasId];
  const presets = vizPresets();
  if (!inst || !presets.length) return;
  if (inst.mode !== 'milkdrop' || !inst.butterchurn) return;
  inst.presetIndex = random
    ? Math.floor(Math.random() * presets.length)
    : (inst.presetIndex + 1) % presets.length;
  inst.butterchurn.loadPreset(presets[inst.presetIndex].preset, 1.5);
  syncVisualizerPresetSelect(canvasId);
}

// Reflects an instance's current mode/preset (changed via the "Random
// preset" button, or on first auto-start) back into its <select> so the
// dropdown never silently goes stale relative to what's actually rendering.
function syncVisualizerPresetSelect(canvasId) {
  const inst = vizInstances[canvasId];
  if (!inst) return;
  const value = inst.mode === 'milkdrop' && inst.presetIndex >= 0
    ? (vizPresets()[inst.presetIndex] || {}).name
    : '__bars__';
  if (value !== undefined) setDropdownValue(canvasId + '-preset', value);
}

// Populates the "$canvasId-preset" custom-dropdown with a "Simple Bars"
// option plus every MilkDrop preset name, once per canvas. If MilkDrop
// isn't available at all (no WebGL2/butterchurn failed to load), only
// "Simple Bars" is offered.
function populateVisualizerPresetSelect(canvasId) {
  const id = canvasId + '-preset';
  if (!$(id + '-dropdown')) return;
  const presets = window.butterchurn ? vizPresets() : [];
  const options = [{ value: '__bars__', label: 'Simple Bars' }]
    .concat(presets.map(p => ({ value: p.name, label: p.name, group: 'MilkDrop' })));
  setDropdownOptions(id, options, { value: '__bars__', placeholder: 'Simple Bars' });
  $(id).addEventListener('change', () => setVisualizerPreset(canvasId, $(id).value));
}

function startVisualizer(videoEl, canvasId) {
  canvasId = canvasId || 'cp-visualizer';
  const inst = ensureVizGraph(videoEl, canvasId);
  if (!inst) return;
  if (inst.audioCtx.state === 'suspended') inst.audioCtx.resume().catch(() => {});
  if (!inst.mode) inst.mode = window.butterchurn ? 'milkdrop' : 'bars';
  renderVisualizer(canvasId);
}

function renderVisualizer(canvasId) {
  const inst = vizInstances[canvasId];
  if (!inst) return;
  if (inst.raf) cancelAnimationFrame(inst.raf);
  const mdCanvas = $(canvasId);
  const barsCanvas = $(barsCanvasId(canvasId)) || mdCanvas;

  if (inst.mode === 'milkdrop' && window.butterchurn) {
    if (barsCanvas !== mdCanvas) barsCanvas.classList.add('hidden');
    if (mdCanvas) mdCanvas.classList.remove('hidden');
    if (!inst.butterchurn) {
      try {
        mdCanvas.width = mdCanvas.clientWidth;
        mdCanvas.height = mdCanvas.clientHeight;
        const Butterchurn = typeof window.butterchurn.createVisualizer === 'function' ? window.butterchurn : window.butterchurn.default;
        inst.butterchurn = Butterchurn.createVisualizer(inst.audioCtx, mdCanvas, {
          width: mdCanvas.width, height: mdCanvas.height,
        });
        inst.butterchurn.connectAudio(inst.source);
        const presets = vizPresets();
        if (presets.length) {
          if (inst.presetIndex < 0) inst.presetIndex = Math.floor(Math.random() * presets.length);
          inst.butterchurn.loadPreset(presets[inst.presetIndex].preset, 0);
        }
        inst.resizeObserver = new ResizeObserver(() => resizeVizCanvas(mdCanvas, inst.butterchurn));
        inst.resizeObserver.observe(mdCanvas);
      } catch {
        inst.butterchurn = null;
        inst.mode = 'bars';
        renderVisualizer(canvasId);
        return;
      }
    }
    syncVisualizerPresetSelect(canvasId);
    const draw = () => {
      inst.raf = requestAnimationFrame(draw);
      inst.butterchurn.render();
    };
    draw();
    return;
  }

  // Bars mode: plain frequency-bar spectrum on a 2D canvas.
  inst.mode = 'bars';
  if (mdCanvas && mdCanvas !== barsCanvas) mdCanvas.classList.add('hidden');
  barsCanvas.classList.remove('hidden');
  if (!inst.analyser) {
    inst.analyser = inst.audioCtx.createAnalyser();
    inst.analyser.fftSize = 128;
    inst.data = new Uint8Array(inst.analyser.frequencyBinCount);
    inst.source.connect(inst.analyser);
  }
  syncVisualizerPresetSelect(canvasId);
  const ctx2d = barsCanvas.getContext('2d');
  const draw = () => {
    inst.raf = requestAnimationFrame(draw);
    const w = barsCanvas.clientWidth, h = barsCanvas.clientHeight;
    if (barsCanvas.width !== w) barsCanvas.width = w;
    if (barsCanvas.height !== h) barsCanvas.height = h;
    inst.analyser.getByteFrequencyData(inst.data);
    ctx2d.clearRect(0, 0, w, h);
    const accent = accents[config.ui.accent] || accents.red;
    const barCount = inst.data.length;
    const barWidth = w / barCount;
    for (let i = 0; i < barCount; i++) {
      const v = inst.data[i] / 255;
      const barHeight = Math.max(2, v * h);
      ctx2d.globalAlpha = 0.5 + v * 0.5;
      ctx2d.fillStyle = accent;
      ctx2d.fillRect(i * barWidth, h - barHeight, Math.max(1, barWidth - 1), barHeight);
    }
    ctx2d.globalAlpha = 1;
  };
  draw();
}

function stopVisualizer(canvasId) {
  canvasId = canvasId || 'cp-visualizer';
  const inst = vizInstances[canvasId];
  if (inst && inst.raf) cancelAnimationFrame(inst.raf);
  if (inst) inst.raf = null;
  if (inst && inst.resizeObserver) { inst.resizeObserver.disconnect(); inst.resizeObserver = null; }
  const barsCanvas = $(barsCanvasId(canvasId));
  if (barsCanvas && !(inst && inst.butterchurn)) {
    const ctx2d = barsCanvas.getContext('2d');
    if (ctx2d) ctx2d.clearRect(0, 0, barsCanvas.width, barsCanvas.height);
  }
}

// Toggles Picture-in-Picture for a player's underlying <video> element.
// getVideoEl is a function (not the element itself) since Video.js may not
// have finished attaching its tech element yet when setupPiP() runs.
function setupPiP(button, getVideoEl) {
  if (!document.pictureInPictureEnabled) {
    button.classList.add('hidden');
    return;
  }
  button.onclick = async () => {
    try {
      if (document.pictureInPictureElement) {
        await document.exitPictureInPicture();
      } else {
        await getVideoEl().requestPictureInPicture();
      }
    } catch (err) {
      toast(`Picture-in-picture failed: ${err.message}`, 'error');
    }
  };
}

function sel(v, x) { return (v || '') === x ? 'selected' : ''; }
function encodeMediaPath(p) { return p.split('/').map(encodeURIComponent).join('/'); }
function formatBytes(v) { const u = ['B','KB','MB','GB','TB']; let i = 0; while (v > 1024 && i < u.length - 1) { v /= 1024; i++; } return `${v.toFixed(i ? 1 : 0)} ${u[i]}`; }

// forecastText turns a StorageForecast into a one-line summary of how much
// recording time is left at the current combined write rate - blank when
// nothing is actively recording, since there's no rate to project from.
function forecastText(forecast) {
  if (!forecast || !forecast.applicable) return '';
  const rate = `${formatBytes(forecast.bytesPerSecond)}/s across ${forecast.activeRecordings} active recording${forecast.activeRecordings === 1 ? '' : 's'}`;
  const hours = forecast.hoursRemaining;
  let remaining;
  if (hours >= 48) remaining = `${(hours / 24).toFixed(1)} days`;
  else remaining = `${hours.toFixed(1)} hours`;
  return `~${rate} — about ${remaining} of storage left at this rate`;
}
function thumbnailUrl(path) { return `/api/recordings/thumbnail?path=${encodeURIComponent(path)}`; }

// Recording thumbnails load lazily via IntersectionObserver, not the native
// loading="lazy" attribute. A card image that starts hidden (or sits off to
// the side of a horizontal scroll row) never satisfies native lazy-loading's
// viewport check, so the old "start hidden, reveal on onload" approach could
// deadlock — the image never loads, so onload never fires, so it's never
// revealed. Here the real src is only assigned once the card actually scrolls
// into view; there's no hidden state to get stuck in, and onerror falls back
// to the card's gradient background without depending on the Tailwind
// `.hidden` utility (which isn't guaranteed to be present offline).
const thumbObserver = ('IntersectionObserver' in window)
  ? new IntersectionObserver((entries, obs) => {
      entries.forEach(entry => {
        if (!entry.isIntersecting) return;
        const img = entry.target;
        obs.unobserve(img);
        if (img.dataset.thumb) { img.src = img.dataset.thumb; delete img.dataset.thumb; }
      });
    }, { rootMargin: '300px' })
  : null;

// Attaches the lazy loader to any freshly-rendered thumbnail images under
// root (default: whole document). Call after setting innerHTML on a card
// list. Without IntersectionObserver support, loads them eagerly instead.
function observeThumbnails(root) {
  const imgs = (root || document).querySelectorAll('.lib-set-thumb-img[data-thumb]');
  imgs.forEach(img => {
    if (thumbObserver) thumbObserver.observe(img);
    else { img.src = img.dataset.thumb; delete img.dataset.thumb; }
  });
}
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function escapeAttr(s) { return escapeHtml(s).replace(/"/g, '&quot;'); }

// --- Theme Switcher ---

// Generic colour presets, not named after (or derived from) any real
// festival's branding - just a starting palette to tweak from.
const festivalThemes = {
  'crimson': { name: 'Crimson', colors: { primary: '#ef4444', secondary: '#dc2626', bg: '#09090b', accent: '#ef4444', text: '#f4f4f5', textMuted: '#a1a1aa' } },
  'violet': { name: 'Violet Pulse', colors: { primary: '#7c3aed', secondary: '#6d28d9', bg: '#0a0a0a', accent: '#ec4899', text: '#f4f4f5', textMuted: '#a1a1aa' } },
  'rose': { name: 'Rose', colors: { primary: '#ec4899', secondary: '#be185d', bg: '#0f0a0a', accent: '#ec4899', text: '#fce7f3', textMuted: '#be185d' } },
  'cyan': { name: 'Cyan Wave', colors: { primary: '#06b6d4', secondary: '#0891b2', bg: '#000d0f', accent: '#06b6d4', text: '#cffafe', textMuted: '#0a7ea4' } },
  'amber': { name: 'Amber', colors: { primary: '#f59e0b', secondary: '#d97706', bg: '#0b0803', accent: '#fbbf24', text: '#f9f5f0', textMuted: '#92400e' } },
  'lime': { name: 'Lime', colors: { primary: '#84cc16', secondary: '#65a30d', bg: '#0a0a0a', accent: '#84cc16', text: '#f4f4f5', textMuted: '#4b5320' } },
  'orchid': { name: 'Orchid', colors: { primary: '#a855f7', secondary: '#9333ea', bg: '#0d0010', accent: '#d946ef', text: '#f4f4f5', textMuted: '#8b5cf6' } },
  'ocean': { name: 'Ocean Blue', colors: { primary: '#3b82f6', secondary: '#1d4ed8', bg: '#000812', accent: '#0ea5e9', text: '#f4f4f5', textMuted: '#60a5fa' } },
};

function renderFestivalPresets() {
  const presetsContainer = $('festival-presets');
  if (!presetsContainer) return;
  const currentTheme = config.ui.customTheme || 'midnight';
  presetsContainer.innerHTML = Object.entries(festivalThemes).map(([key, theme]) => {
    const isActive = currentTheme === key;
    return `<button type="button" class="preset-theme-btn ${isActive ? 'active' : ''}" onclick="applyFestivalTheme('${key}')" title="${theme.name}">${theme.name}</button>`;
  }).join('');
}

function applyFestivalTheme(themeKey) {
  const theme = festivalThemes[themeKey];
  if (!theme) return;

  config.ui.customTheme = themeKey;
  config.ui.themeColors = theme.colors;

  updateColorInputs(theme.colors);
  applyThemeColors(theme.colors);
  renderFestivalPresets();
  toast(`Applied "${theme.name}" theme`, 'info');
}

function updateColorInputs(colors) {
  $('colorPrimary').value = rgbToHex(colors.primary) || colors.primary;
  $('colorSecondary').value = rgbToHex(colors.secondary) || colors.secondary;
  $('colorBg').value = rgbToHex(colors.bg) || colors.bg;
  $('colorAccent').value = rgbToHex(colors.accent) || colors.accent;
  $('colorText').value = rgbToHex(colors.text) || colors.text;
  $('colorTextMuted').value = rgbToHex(colors.textMuted) || colors.textMuted;
}

function readCustomTheme() {
  return {
    primary: $('colorPrimary').value,
    secondary: $('colorSecondary').value,
    bg: $('colorBg').value,
    accent: $('colorAccent').value,
    text: $('colorText').value,
    textMuted: $('colorTextMuted').value,
  };
}

function applyThemeColors(colors) {
  // Drive the design-token layer (app.css keys everything off these), so a
  // custom theme recolours the whole polished UI without per-component
  // overrides. A few legacy helper rules are kept for anything still
  // referencing the older --color-* names.
  const bgRgb = hexToRgb(colors.bg).join(' ');
  const css = `
    :root {
      --color-primary: ${colors.primary};
      --color-secondary: ${colors.secondary};
      --color-bg: ${colors.bg};
      --color-accent: ${colors.accent};
      --color-text: ${colors.text};
      --color-text-muted: ${colors.textMuted};
      --accent: ${colors.accent};
      --bg: ${colors.bg};
      --text: ${colors.text};
      --text-muted: ${colors.textMuted};
    }
    body { background: var(--color-bg); color: var(--color-text); }
    .panel { background: linear-gradient(180deg, rgb(255 255 255 / .035), rgb(255 255 255 / 0) 120px), rgb(${bgRgb} / .72); }
    .accent-color { color: var(--color-accent); }
  `;
  const styleEl = document.getElementById('custom-css');
  if (styleEl) {
    styleEl.textContent = config.ui.customCss + '\n' + css;
  }
  const root = document.documentElement.style;
  root.setProperty('--accent', colors.accent);
  root.setProperty('--bg', colors.bg);
  root.setProperty('--text', colors.text);
  root.setProperty('--text-muted', colors.textMuted);
}

function rgbToHex(rgb) {
  if (!rgb) return '';
  if (rgb.startsWith('#')) return rgb;
  const match = rgb.match(/rgb\((\d+),\s*(\d+),\s*(\d+)\)/);
  if (match) {
    return '#' + [parseInt(match[1]), parseInt(match[2]), parseInt(match[3])].map(x => {
      const h = x.toString(16);
      return h.length === 1 ? '0' + h : h;
    }).join('');
  }
  return '';
}

function hexToRgb(hex) {
  const h = hex.replace('#', '');
  return [parseInt(h.substr(0, 2), 16), parseInt(h.substr(2, 2), 16), parseInt(h.substr(4, 2), 16)];
}

$('applyCustomTheme').onclick = async () => {
  const colors = readCustomTheme();
  config.ui.customTheme = 'custom';
  config.ui.themeColors = colors;
  applyThemeColors(colors);
  renderFestivalPresets();
  toast('Custom theme applied', 'info');
};

if ($('backfill-timecodes-btn')) {
  $('backfill-timecodes-btn').onclick = async () => {
    const btn = $('backfill-timecodes-btn');
    btn.disabled = true;
    const originalText = btn.textContent;
    btn.textContent = 'Scanning...';
    try {
      const result = await api('/api/recordings/backfill-timecodes', { method: 'POST' });
      if (result) {
        toast(`Backfilled ${result.written} of ${result.scanned} recordings (${result.skipped} already had one, ${result.failed} failed)`, 'info');
      }
    } finally {
      btn.disabled = false;
      btn.textContent = originalText;
    }
  };
}

document.addEventListener('DOMContentLoaded', () => {
  ['colorPrimary', 'colorSecondary', 'colorBg', 'colorAccent', 'colorText', 'colorTextMuted'].forEach(id => {
    const el = $(id);
    if (el) el.addEventListener('change', () => {
      const colors = readCustomTheme();
      applyThemeColors(colors);
    });
  });
});

// ─── Events tab ──────────────────────────────────────────────────────────────

function renderEventsTab() {
  evCurrentFestivalId = null;
  $('ev-home').classList.remove('hidden');
  $('ev-detail').classList.add('hidden');
  $('ev-tab-back').classList.add('hidden');
  $('ev-tab-title').textContent = 'Events';
  renderEventsHome();
}

function renderEventsHome() {
  const festivals = config.festivals || [];
  const orgs = config.organisations || [];
  const libEvents = config.libraryEvents || [];
  const now = new Date();

  // Upcoming sets (next 12h from live timetable)
  const upcoming = [];
  (config.timetable || []).forEach(stage => {
    (stage.sets || []).forEach(set => {
      const start = new Date(set.start);
      const diff = (start - now) / 60000;
      if (diff > 0 && diff <= 720) upcoming.push({ ...set, stage: stage.stage });
    });
  });
  upcoming.sort((a, b) => new Date(a.start) - new Date(b.start));
  const upcomingEl = $('ev-upcoming');
  if (upcoming.length) {
    $('ev-upcoming-list').innerHTML = upcoming.slice(0, 8).map(s => {
      const t = new Date(s.start).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      return `<div class="flex items-center gap-3 py-1 border-b border-white/5 last:border-0">
        <span class="text-zinc-400 w-12 flex-shrink-0">${escapeHtml(t)}</span>
        <span class="font-medium">${escapeHtml(s.name)}</span>
        <span class="text-xs text-zinc-500 ml-auto">${escapeHtml(s.stage)}</span>
      </div>`;
    }).join('');
    upcomingEl.classList.remove('hidden');
  } else {
    upcomingEl.classList.add('hidden');
  }

  // Active editions (LibraryEvents where today is within startDate/endDate)
  const active = libEvents.filter(e => {
    if (!e.startDate || !e.endDate) return false;
    const start = new Date(e.startDate), end = new Date(e.endDate + 'T23:59:59');
    return now >= start && now <= end;
  });
  const activeEl = $('ev-active');
  if (active.length) {
    $('ev-active-list').innerHTML = active.map(e => {
      const bg = `background:linear-gradient(135deg, ${e.color || 'var(--accent)'}44, transparent);`;
      return `<div class="rounded-lg border border-white/10 p-3 cursor-pointer hover:border-white/20" style="${bg}" onclick="openEvFestivalDetail(null, '${escapeAttr(e.id)}')">
        <div class="font-semibold">${escapeHtml(e.name)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(e.startDate)} – ${escapeHtml(e.endDate)}</div>
      </div>`;
    }).join('');
    activeEl.classList.remove('hidden');
  } else {
    activeEl.classList.add('hidden');
  }

  // Festival/Org grid — group festivals under their org, standalone festivals ungrouped
  const grouped = new Map(); // orgId ("") -> { org, festivals }
  const orgMap = new Map(orgs.map(o => [o.id, o]));
  festivals.forEach(f => {
    const oid = f.organisationId || '';
    if (!grouped.has(oid)) grouped.set(oid, { org: orgMap.get(oid) || null, festivals: [] });
    grouped.get(oid).festivals.push(f);
  });
  // Orgs with no festivals still show up as group headers
  orgs.forEach(o => { if (!grouped.has(o.id)) grouped.set(o.id, { org: o, festivals: [] }); });

  let gridHtml = '';
  // Ungrouped festivals first
  const ungrouped = grouped.get('');
  if (ungrouped && ungrouped.festivals.length) {
    gridHtml += ungrouped.festivals.map(f => evFestivalCardHtml(f, libEvents, recordings)).join('');
  }
  // Then org groups
  [...grouped.entries()].filter(([k]) => k !== '').forEach(([, { org, festivals: fests }]) => {
    if (org) {
      gridHtml += `<div class="col-span-full flex items-center gap-2 mt-2 mb-1">
        <span class="font-semibold text-sm">${escapeHtml(org.name)}</span>
        <button class="btn text-xs" onclick="openOrgEditor(${JSON.stringify(org).replace(/&/g, '&amp;').replace(/"/g, '&quot;')})">Edit</button>
        <button class="btn danger text-xs" onclick="deleteOrg('${escapeAttr(org.id)}')">Delete</button>
      </div>`;
    }
    gridHtml += fests.map(f => evFestivalCardHtml(f, libEvents, recordings)).join('');
  });
  // Standalone orgs with no festivals
  orgs.forEach(o => {
    if (!grouped.has(o.id) || !grouped.get(o.id).festivals.length) {
      if (!grouped.has(o.id)) {
        gridHtml += `<div class="col-span-full flex items-center gap-2 mt-2 mb-1">
          <span class="font-semibold text-sm">${escapeHtml(o.name)}</span>
          <button class="btn text-xs" onclick="openOrgEditor(${JSON.stringify(o).replace(/&/g, '&amp;').replace(/"/g, '&quot;')})">Edit</button>
          <button class="btn danger text-xs" onclick="deleteOrg('${escapeAttr(o.id)}')">Delete</button>
        </div>`;
      }
    }
  });
  $('ev-grid').innerHTML = gridHtml || '<p class="col-span-full text-sm text-zinc-400">No events yet — create one with "+ New Event" above.</p>';
  // Wire up card clicks (can't use inline onclick with festival objects, so use data-id)
  $('ev-grid').querySelectorAll('.ev-festival-card').forEach(card => {
    card.addEventListener('click', () => openEvFestivalDetail(card.dataset.id, null));
  });
}

function evFestivalCardHtml(f, libEvents, recs) {
  const editionCount = libEvents.filter(e => e.festivalId === f.id).length;
  const recCount = recs.filter(r => {
    const ev = (config.libraryEvents || []).find(e => e.id === r.eventId);
    return ev && ev.festivalId === f.id;
  }).length;
  const bg = f.logoUrl
    ? `background-image:url('${escapeAttr(f.logoUrl)}');background-size:cover;background-position:center;`
    : `background:linear-gradient(160deg, ${f.color || 'var(--accent)'}66, rgb(0 0 0/.45) 75%);`;
  return `<div class="ev-festival-card lib-event-card" data-id="${escapeAttr(f.id)}" style="${bg}">
    <div class="lib-event-card-overlay">
      <div class="truncate font-semibold">${escapeHtml(f.name)}</div>
      <div class="text-xs text-zinc-300">${editionCount} edition${editionCount === 1 ? '' : 's'}</div>
      <div class="text-xs text-zinc-400">${recCount} recording${recCount === 1 ? '' : 's'}</div>
    </div>
  </div>`;
}

function openEvFestivalDetail(festivalId, libEventId) {
  evCurrentFestivalId = festivalId;
  $('ev-home').classList.add('hidden');
  $('ev-detail').classList.remove('hidden');
  $('ev-tab-back').classList.remove('hidden');
  renderEvFestivalDetail(festivalId, libEventId);
}

function renderEvFestivalDetail(festivalId, libEventId) {
  const festival = festivalId ? (config.festivals || []).find(f => f.id === festivalId) : null;
  const libEvent = libEventId ? (config.libraryEvents || []).find(e => e.id === libEventId) : null;
  const subject = festival || libEvent;
  if (!subject) return;

  $('ev-tab-title').textContent = subject.name;
  $('ev-detail-name').textContent = subject.name;
  $('ev-detail-desc').textContent = subject.description || '';
  $('ev-detail-banner').style.borderLeftColor = subject.color || 'var(--accent)';

  // Stats
  const editions = festival
    ? (config.libraryEvents || []).filter(e => e.festivalId === festival.id)
    : (libEvent ? [libEvent] : []);
  const eventIds = new Set(editions.map(e => e.id));
  const recCount = recordings.filter(r => eventIds.has(r.eventId)).length;
  const totalBytes = recordings.filter(r => eventIds.has(r.eventId)).reduce((s, r) => s + (r.size || 0), 0);
  $('ev-detail-stats').innerHTML = [
    festival && `<span>${editions.length} edition${editions.length === 1 ? '' : 's'}</span>`,
    `<span>${recCount} recording${recCount === 1 ? '' : 's'}</span>`,
    totalBytes && `<span>${formatBytes(totalBytes)} total</span>`,
  ].filter(Boolean).join('');

  if (festival) {
    $('ev-detail-edit').onclick = () => openEvFestivalEditor(festival);
    $('ev-detail-delete').onclick = () => deleteEvFestival(festival.id);
  } else {
    $('ev-detail-edit').classList.add('hidden');
    $('ev-detail-delete').classList.add('hidden');
  }

  // Sources linked to this festival
  const sources = festival
    ? (state.sources || []).filter(s => s.festivalId === festival.id)
    : [];
  $('ev-detail-sources').innerHTML = sources.length
    ? sources.map(s => `<div class="ev-detail-source flex items-center gap-2 py-1 cursor-pointer hover:text-white" data-id="${escapeAttr(s.id)}" title="Jump to this source">
        <span class="inline-block w-2 h-2 rounded-full flex-shrink-0" style="background:${escapeAttr(s.color || '#71717a')}"></span>
        <span>${escapeHtml(s.name)}</span>
        <span class="text-xs text-zinc-500 ml-auto">${escapeHtml(s.status || '')}</span>
      </div>`).join('')
    : '<p class="text-zinc-400">No live sources linked.</p>';
  document.querySelectorAll('.ev-detail-source').forEach(el => el.addEventListener('click', () => {
    highlightSourceId = el.dataset.id;
    document.querySelector('.nav[data-view="sources"]').click();
  }));

  // Editions (library events)
  $('ev-detail-editions').innerHTML = editions.length
    ? editions.map(e => {
        const eRecCount = recordings.filter(r => r.eventId === e.id).length;
        return `<div class="ev-detail-edition flex items-center justify-between rounded border border-white/10 px-3 py-2 cursor-pointer hover:border-white/20" data-id="${escapeAttr(e.id)}">
          <div>
            <div class="font-medium">${escapeHtml(e.name)}</div>
            <div class="text-xs text-zinc-400">${escapeHtml(e.startDate || String(e.year || ''))} · ${eRecCount} recording${eRecCount === 1 ? '' : 's'}</div>
          </div>
          <span class="text-zinc-400">&#8594;</span>
        </div>`;
      }).join('')
    : '<p class="text-zinc-400">No recording archives linked to this event.</p>';
  document.querySelectorAll('.ev-detail-edition').forEach(el => el.addEventListener('click', () => {
    openLibraryEvent(el.dataset.id);
    document.querySelector('.nav[data-view="recordings"]').click();
  }));

  // Live timetable stages
  const timetable = config.timetable || [];
  const relevantStages = festival
    ? (state.sources || []).filter(s => s.festivalId === festival.id && s.timetableStage).map(s => s.timetableStage)
    : [];
  const stageData = relevantStages.length
    ? timetable.filter(st => relevantStages.includes(st.stage))
    : timetable;
  $('ev-detail-timetable').innerHTML = stageData.length
    ? stageData.map(st => `<div class="mb-2">
        <div class="font-medium text-sm">${escapeHtml(st.stage)}</div>
        <div class="text-xs text-zinc-500">${(st.sets || []).length} sets</div>
      </div>`).join('')
    : '<p class="text-zinc-400">No timetable configured.</p>';
}

$('ev-tab-back').onclick = () => renderEventsTab();
$('ev-new-org').onclick = () => openOrgEditor(null);
$('ev-new-festival').onclick = () => openEvFestivalEditor(null);

// ─── Organisation CRUD ────────────────────────────────────────────────────────

function openOrgEditor(org) {
  evEditingOrgId = org ? org.id : null;
  $('org-editor-title').textContent = org ? 'Edit Organisation' : 'New Organisation';
  $('org-name').value = org ? org.name : '';
  $('org-desc').value = org ? (org.description || '') : '';
  $('org-color').value = (org && org.color) || '#ef4444';
  $('org-logo').value = org ? (org.logoUrl || '') : '';
  syncImageUploadPreview('org-logo');
  $('org-editor-error').classList.add('hidden');
  $('org-editor-overlay').classList.remove('hidden');
}
function closeOrgEditor() { $('org-editor-overlay').classList.add('hidden'); evEditingOrgId = null; }
$('org-editor-close').onclick = closeOrgEditor;
$('org-editor-overlay').addEventListener('click', e => { if (e.target.id === 'org-editor-overlay') closeOrgEditor(); });

$('org-editor-save').onclick = async () => {
  const name = $('org-name').value.trim();
  if (!name) { $('org-editor-error').textContent = 'Name is required'; $('org-editor-error').classList.remove('hidden'); return; }
  const payload = { name, description: $('org-desc').value.trim(), color: $('org-color').value, logoUrl: $('org-logo').value.trim() };
  try {
    if (evEditingOrgId) {
      await api(`/api/organisations/${evEditingOrgId}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Saved "${name}"`, 'info');
    } else {
      await api('/api/organisations', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Created "${name}"`, 'info');
    }
  } catch { return; }
  closeOrgEditor();
  await refresh();
  renderEventsHome();
};

async function deleteOrg(id) {
  const org = (config.organisations || []).find(o => o.id === id);
  if (!org) return;
  if (!confirm(`Delete organisation "${org.name}"? Festivals linked to it become standalone.`)) return;
  try { await api(`/api/organisations/${id}`, { method: 'DELETE' }); toast(`Deleted "${org.name}"`, 'info'); } catch { return; }
  await refresh();
  renderEventsHome();
}

// ─── Festival CRUD (from Events tab) ─────────────────────────────────────────

function openEvFestivalEditor(festival) {
  evEditingFestId = festival ? festival.id : null;
  $('ev-festival-editor-title').textContent = festival ? 'Edit Event' : 'New Event';
  $('ev-fest-name').value = festival ? festival.name : '';
  $('ev-fest-desc').value = festival ? (festival.description || '') : '';
  $('ev-fest-color').value = (festival && festival.color) || '#ef4444';
  $('ev-fest-logo').value = festival ? (festival.logoUrl || '') : '';
  syncImageUploadPreview('ev-fest-logo');
  // Populate organisation dropdown; setDropdownOptions creates the hidden input inside the container
  const orgOpts = [{ value: '', label: 'None' }, ...(config.organisations || []).map(o => ({ value: o.id, label: o.name }))];
  setDropdownOptions('ev-fest-org', orgOpts, { value: festival ? (festival.organisationId || '') : '' });
  setDropdownValue('ev-fest-org', festival ? (festival.organisationId || '') : '');
  $('ev-festival-editor-error').classList.add('hidden');
  $('ev-festival-editor-overlay').classList.remove('hidden');
}
function closeEvFestivalEditor() { $('ev-festival-editor-overlay').classList.add('hidden'); evEditingFestId = null; }
$('ev-festival-editor-close').onclick = closeEvFestivalEditor;
$('ev-festival-editor-overlay').addEventListener('click', e => { if (e.target.id === 'ev-festival-editor-overlay') closeEvFestivalEditor(); });

$('ev-festival-editor-save').onclick = async () => {
  const name = $('ev-fest-name').value.trim();
  if (!name) { $('ev-festival-editor-error').textContent = 'Name is required'; $('ev-festival-editor-error').classList.remove('hidden'); return; }
  const payload = { name, description: $('ev-fest-desc').value.trim(), color: $('ev-fest-color').value, logoUrl: $('ev-fest-logo').value.trim(), organisationId: ($('ev-fest-org') || {}).value || '' };
  try {
    if (evEditingFestId) {
      await api(`/api/festivals/${evEditingFestId}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Saved "${name}"`, 'info');
    } else {
      await api('/api/festivals', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      toast(`Created "${name}"`, 'info');
    }
  } catch { return; }
  closeEvFestivalEditor();
  await refresh();
  renderEventsHome();
};

async function deleteEvFestival(id) {
  const f = (config.festivals || []).find(x => x.id === id);
  if (!f) return;
  if (!confirm(`Delete event "${f.name}"? Sources linked to it become ungrouped.`)) return;
  try { await api(`/api/festivals/${id}`, { method: 'DELETE' }); toast(`Deleted "${f.name}"`, 'info'); } catch { return; }
  await refresh();
  renderEventsTab();
}

// ─── End of Events tab ────────────────────────────────────────────────────────

// ─── File Explorer ────────────────────────────────────────────────────────────

let explorerPath = '';
let explorerEntries = [];
let explorerSelected = new Set();
let explorerFetchJobPollTimer = null;

function explorerJoin(base, name) { return base ? `${base}/${name}` : name; }
function explorerParent(p) { const i = p.lastIndexOf('/'); return i === -1 ? '' : p.slice(0, i); }

async function loadExplorer() {
  explorerSelected = new Set();
  let data;
  try {
    data = await api(`/api/explorer/list?path=${encodeURIComponent(explorerPath)}`);
  } catch {
    // Path likely no longer exists (deleted/renamed elsewhere) - fall back to root.
    explorerPath = '';
    try { data = await api('/api/explorer/list?path='); } catch { return; }
  }
  explorerEntries = data.entries || [];
  renderExplorerBreadcrumb();
  renderExplorerRows();
}

function renderExplorerBreadcrumb() {
  const parts = explorerPath ? explorerPath.split('/') : [];
  let acc = '';
  const crumbs = ['<a href="#" data-path="">root</a>'];
  for (const part of parts) {
    acc = explorerJoin(acc, part);
    crumbs.push(`<a href="#" data-path="${escapeAttr(acc)}">${escapeHtml(part)}</a>`);
  }
  $('explorer-breadcrumb').innerHTML = crumbs.join(' / ');
  $('explorer-breadcrumb').querySelectorAll('a').forEach(a => a.addEventListener('click', (e) => {
    e.preventDefault();
    explorerPath = a.dataset.path;
    loadExplorer();
  }));
}

function renderExplorerRows() {
  const tbody = $('explorer-rows');
  $('explorer-empty').classList.toggle('hidden', explorerEntries.length > 0);
  tbody.innerHTML = explorerEntries.map(e => `
    <tr data-name="${escapeAttr(e.name)}">
      <td><input type="checkbox" class="explorer-row-check" data-name="${escapeAttr(e.name)}"></td>
      <td class="truncate">
        ${e.isDir
          ? `<a href="#" class="explorer-open-dir font-medium" data-name="${escapeAttr(e.name)}">&#128193; ${escapeHtml(e.name)}</a>`
          : `<span>&#128196; ${escapeHtml(e.name)}</span>`}
      </td>
      <td class="text-zinc-400">${e.isDir ? '' : formatBytes(e.size)}</td>
      <td class="text-zinc-400">${new Date(e.modTime).toLocaleString()}</td>
      <td class="whitespace-nowrap text-right">
        <button type="button" class="btn explorer-download" data-name="${escapeAttr(e.name)}">Download</button>
        ${!e.isDir && /\.zip$/i.test(e.name) ? `<button type="button" class="btn explorer-extract" data-name="${escapeAttr(e.name)}">Extract</button>` : ''}
        <button type="button" class="btn explorer-rename" data-name="${escapeAttr(e.name)}">Rename</button>
        <button type="button" class="btn explorer-delete" data-name="${escapeAttr(e.name)}" style="color:#fda4af">Delete</button>
      </td>
    </tr>`).join('');

  tbody.querySelectorAll('.explorer-open-dir').forEach(a => a.addEventListener('click', (e) => {
    e.preventDefault();
    explorerPath = explorerJoin(explorerPath, a.dataset.name);
    loadExplorer();
  }));
  tbody.querySelectorAll('.explorer-row-check').forEach(cb => cb.addEventListener('change', () => {
    if (cb.checked) explorerSelected.add(cb.dataset.name); else explorerSelected.delete(cb.dataset.name);
    updateExplorerSelectionButtons();
  }));
  tbody.querySelectorAll('.explorer-download').forEach(btn => btn.addEventListener('click', () => {
    window.open(`/api/explorer/download?path=${encodeURIComponent(explorerJoin(explorerPath, btn.dataset.name))}`, '_blank');
  }));
  tbody.querySelectorAll('.explorer-extract').forEach(btn => btn.addEventListener('click', async () => {
    try {
      await api('/api/explorer/unzip', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: explorerJoin(explorerPath, btn.dataset.name) }) });
      toast(`Extracted "${btn.dataset.name}"`, 'info');
    } catch { return; }
    loadExplorer();
  }));
  tbody.querySelectorAll('.explorer-rename').forEach(btn => btn.addEventListener('click', async () => {
    const newName = prompt(`Rename "${btn.dataset.name}" to:`, btn.dataset.name);
    if (!newName || newName === btn.dataset.name) return;
    try {
      await api('/api/explorer/rename', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: explorerJoin(explorerPath, btn.dataset.name), newName }) });
    } catch { return; }
    loadExplorer();
  }));
  tbody.querySelectorAll('.explorer-delete').forEach(btn => btn.addEventListener('click', async () => {
    if (!confirm(`Delete "${btn.dataset.name}"? This can't be undone.`)) return;
    try {
      await api('/api/explorer/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: explorerJoin(explorerPath, btn.dataset.name) }) });
    } catch { return; }
    loadExplorer();
  }));
  updateExplorerSelectionButtons();
}

function updateExplorerSelectionButtons() {
  const any = explorerSelected.size > 0;
  $('explorer-zip-selected').disabled = !any;
  $('explorer-download-selected').disabled = !any;
}

$('explorer-select-all').addEventListener('change', () => {
  const checked = $('explorer-select-all').checked;
  explorerSelected = new Set(checked ? explorerEntries.map(e => e.name) : []);
  document.querySelectorAll('.explorer-row-check').forEach(cb => cb.checked = checked);
  updateExplorerSelectionButtons();
});

$('explorer-refresh').onclick = () => loadExplorer();

$('explorer-mkdir').onclick = async () => {
  const name = prompt('New folder name:');
  if (!name) return;
  try {
    await api('/api/explorer/mkdir', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: explorerPath, name }) });
  } catch { return; }
  loadExplorer();
};

$('explorer-upload-btn').onclick = () => $('explorer-upload-input').click();
$('explorer-upload-input').onchange = async () => {
  const files = $('explorer-upload-input').files;
  if (!files.length) return;
  const form = new FormData();
  for (const f of files) form.append('file', f);
  $('explorer-upload-input').value = '';
  toast(`Uploading ${files.length} file(s)…`, 'info');
  try {
    const res = await api(`/api/explorer/upload?path=${encodeURIComponent(explorerPath)}`, { method: 'POST', body: form });
    toast(`Uploaded ${res.saved}/${res.total} file(s)`, 'info');
  } catch { return; }
  loadExplorer();
};

$('explorer-zip-selected').onclick = async () => {
  const zipName = prompt('Zip file name:', 'archive');
  if (!zipName) return;
  try {
    await api('/api/explorer/zip', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: explorerPath, names: [...explorerSelected], zipName }) });
    toast('Zip created', 'info');
  } catch { return; }
  loadExplorer();
};

$('explorer-download-selected').onclick = () => {
  const params = [...explorerSelected].map(n => `path=${encodeURIComponent(explorerJoin(explorerPath, n))}`).join('&');
  window.open(`/api/explorer/download?${params}`, '_blank');
};

// --- Fetch from URL (works with direct links and ownCloud/Nextcloud-style
// public share links, e.g. TransIP Stack) ---

function openExplorerFetchModal() {
  $('explorer-fetch-url').value = '';
  $('explorer-fetch-username').value = '';
  $('explorer-fetch-password').value = '';
  $('explorer-fetch-cookie').value = '';
  $('explorer-fetch-debug').checked = false;
  $('explorer-fetch-error').classList.add('hidden');
  $('explorer-fetch-job-box').classList.add('hidden');
  const proxyUrl = ((config.settings && config.settings.sharing) || {}).proxyUrl || '';
  $('explorer-fetch-use-proxy').checked = false;
  $('explorer-fetch-use-proxy').disabled = !proxyUrl;
  $('explorer-fetch-proxy-hint').textContent = proxyUrl ? `(${proxyUrl})` : '(none configured - set one in Settings → Peer Sharing)';
  stopExplorerFetchPoll();
  $('explorer-fetch-overlay').classList.remove('hidden');
}
function closeExplorerFetchModal() {
  $('explorer-fetch-overlay').classList.add('hidden');
  stopExplorerFetchPoll();
}
$('explorer-fetch-open').onclick = openExplorerFetchModal;
$('explorer-fetch-close').onclick = closeExplorerFetchModal;
$('explorer-fetch-cancel').onclick = closeExplorerFetchModal;
$('explorer-fetch-overlay').addEventListener('click', (e) => { if (e.target.id === 'explorer-fetch-overlay') closeExplorerFetchModal(); });

$('explorer-fetch-start').onclick = async () => {
  const url = $('explorer-fetch-url').value.trim();
  const username = $('explorer-fetch-username').value.trim();
  const password = $('explorer-fetch-password').value;
  const cookie = $('explorer-fetch-cookie').value.trim();
  const debug = $('explorer-fetch-debug').checked;
  const useProxy = $('explorer-fetch-use-proxy').checked;
  if (!url) { $('explorer-fetch-error').textContent = 'Enter a URL first'; $('explorer-fetch-error').classList.remove('hidden'); return; }
  $('explorer-fetch-error').classList.add('hidden');
  $('explorer-fetch-start').disabled = true;
  let result;
  try {
    result = await api('/api/explorer/fetch', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url, username, password, cookie, path: explorerPath, debug, useProxy }) });
  } catch { $('explorer-fetch-start').disabled = false; return; }
  if (!result.ok) {
    $('explorer-fetch-error').textContent = result.error || 'Could not start that download.';
    $('explorer-fetch-error').classList.remove('hidden');
    $('explorer-fetch-start').disabled = false;
    return;
  }
  $('explorer-fetch-job-box').classList.remove('hidden');
  pollExplorerFetchJob(result.jobId);
};

function stopExplorerFetchPoll() {
  if (explorerFetchJobPollTimer) { clearTimeout(explorerFetchJobPollTimer); explorerFetchJobPollTimer = null; }
}

async function pollExplorerFetchJob(jobId) {
  stopExplorerFetchPoll();
  let job;
  try {
    job = await api(`/api/explorer/fetch/jobs/${encodeURIComponent(jobId)}`);
  } catch {
    explorerFetchJobPollTimer = setTimeout(() => pollExplorerFetchJob(jobId), 2000);
    return;
  }
  renderExplorerFetchJob(job);
  if (job.status === 'running') {
    explorerFetchJobPollTimer = setTimeout(() => pollExplorerFetchJob(jobId), 1000);
  } else {
    $('explorer-fetch-start').disabled = false;
    if (job.status === 'error') toast(`Download failed: ${job.error || 'unknown error'}`, 'error');
    else { toast(`Downloaded "${job.destName}"`, 'info'); loadExplorer(); }
  }
}

function renderExplorerFetchJob(job) {
  const pct = job.totalBytes > 0 ? Math.min(100, Math.round(job.transferredBytes / job.totalBytes * 100)) : 0;
  $('explorer-fetch-job-title').textContent = job.status === 'running' ? 'Downloading…' : (job.status === 'error' ? 'Failed' : 'Done');
  $('explorer-fetch-job-stats').textContent = job.totalBytes > 0
    ? `${formatBytes(job.transferredBytes)} / ${formatBytes(job.totalBytes)} · ${formatBytes(job.speedBps)}/s`
    : `${formatBytes(job.transferredBytes)} · ${formatBytes(job.speedBps)}/s`;
  $('explorer-fetch-job-bar').style.width = `${pct}%`;
  $('explorer-fetch-job-log').textContent = (job.log || []).map(l => `[${l.time}] ${l.text}`).join('\n');
  $('explorer-fetch-job-log').scrollTop = $('explorer-fetch-job-log').scrollHeight;
}

// Explorer only loads on demand (its tab click handler calls loadExplorer()
// below, alongside the other per-tab loaders), not eagerly at page load.

// Wires up the handful of custom dropdowns that are static parts of the page
// (not rebuilt by drawSourceEditor() or setDropdownOptions()) as soon as the
// script runs - the source editor's per-row dropdowns get the same wiring
// again after every render, safely, since already-bound elements are skipped.
// ─── Set Cutter ──────────────────────────────────────────────────────────────
// Splits a whole-day recording into individual set files. Video and
// audio-only recordings share this same UI: markers are always placed
// against the audio waveform, but a video recording also gets a <video>
// element playing alongside it instead of a plain <audio> one.

let cutterRecording = null;   // the RecordingFile being cut
let cutterMarkers = [];       // array of CutterMarker
let cutterSidecar = null;     // the .timecode.json sidecar, or null if none yet

const cutterAudioExts = ['.mp3', '.m4a', '.aac', '.opus', '.flac', '.ogg', '.wav'];
function cutterIsAudioOnly(path) {
  const ext = path.slice(path.lastIndexOf('.')).toLowerCase();
  return cutterAudioExts.includes(ext);
}

function cutterPlayerEl() {
  return cutterRecording && cutterIsAudioOnly(cutterRecording.path) ? $('cutter-audio') : $('cutter-video');
}

function formatClock(totalSeconds) {
  const s = Math.max(0, Math.round(totalSeconds));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return h > 0
    ? `${h}:${String(m).padStart(2, '0')}:${String(sec).padStart(2, '0')}`
    : `${m}:${String(sec).padStart(2, '0')}`;
}

async function openCutterModal(r) {
  cutterRecording = r;
  cutterMarkers = [];
  cutterSidecar = null;
  $('cutter-title').textContent = `Set Cutter: ${libDisplayTitle(r)}`;
  $('cutter-error').classList.add('hidden');
  $('cutter-precise').checked = false;
  cutterDetectProposals = [];
  $('cutter-proposals-box').classList.add('hidden');
  $('cutter-detect-status').classList.add('hidden');
  stopCutterDetectPoll();

  const audioEl = $('cutter-audio'), videoEl = $('cutter-video');
  const isAudio = cutterIsAudioOnly(r.path);
  audioEl.classList.toggle('hidden', !isAudio);
  videoEl.classList.toggle('hidden', isAudio);
  const player = isAudio ? audioEl : videoEl;
  player.src = `/media/${encodeMediaPath(r.path)}`;

  $('cutter-waveform-img').src = `/api/recordings/waveform?path=${encodeURIComponent(r.path)}`;
  $('cutter-waveform-status').textContent = 'Loading waveform...';
  $('cutter-waveform-img').onload = () => { $('cutter-waveform-status').textContent = ''; };
  $('cutter-waveform-img').onerror = () => { $('cutter-waveform-status').textContent = 'Waveform unavailable for this file.'; };

  try {
    const res = await fetch(`/api/recordings/timecode?path=${encodeURIComponent(r.path)}`);
    if (res.ok) cutterSidecar = await res.json();
  } catch (e) { /* no sidecar yet - fine, timetable seeding just won't be available */ }

  try {
    cutterMarkers = await api(`/api/cutter/markers?path=${encodeURIComponent(r.path)}`) || [];
  } catch (e) {
    cutterMarkers = [];
  }
  renderCutterMarkers();

  $('cutter-overlay').classList.remove('hidden');
}

function closeCutterModal() {
  $('cutter-overlay').classList.add('hidden');
  $('cutter-audio').pause(); $('cutter-audio').removeAttribute('src');
  $('cutter-video').pause(); $('cutter-video').removeAttribute('src');
  cutterRecording = null;
  stopCutterDetectPoll();
}
$('cutter-close').onclick = closeCutterModal;
$('cutter-overlay').addEventListener('click', (e) => { if (e.target.id === 'cutter-overlay') closeCutterModal(); });

function cutterDurationSeconds() {
  const player = cutterPlayerEl();
  if (player && player.duration && isFinite(player.duration)) return player.duration;
  if (cutterSidecar && cutterSidecar.durationSec) return cutterSidecar.durationSec;
  return 0;
}

function renderCutterMarkers() {
  cutterMarkers.sort((a, b) => a.offsetSec - b.offsetSec);

  // Ticks over the waveform.
  const dur = cutterDurationSeconds();
  $('cutter-timeline-markers').innerHTML = dur > 0 ? cutterMarkers.map(m => {
    const pct = Math.min(100, Math.max(0, (m.offsetSec / dur) * 100));
    return `<div class="cutter-marker-tick" style="left:${pct}%" data-offset="${m.offsetSec}"><span class="cutter-marker-tick-label">${escapeHtml(m.name || m.artist || '')}</span></div>`;
  }).join('') : '';

  // Editable marker rows.
  $('cutter-markers-list').innerHTML = cutterMarkers.map((m, i) => `
    <div class="cutter-marker-row" data-index="${i}">
      <div class="cutter-marker-time">${formatClock(m.offsetSec)}</div>
      <input class="input cutter-m-name" placeholder="Set / artist name" value="${escapeAttr(m.name || '')}">
      <input class="input cutter-m-channel" placeholder="Channel" value="${escapeAttr(m.channel || '')}">
      <input class="input cutter-m-tracklist" placeholder="Tracklist (optional)" value="${escapeAttr(m.tracklist || '')}">
      <div class="flex gap-1">
        <button type="button" class="btn cutter-m-seek" title="Jump playback here">&#9658;</button>
        <button type="button" class="btn cutter-m-remove" style="color:#fda4af" title="Remove marker">&times;</button>
      </div>
    </div>`).join('') || '<p class="text-sm text-zinc-400">No markers yet - play the recording and click "Add marker at current time", or use "Load from timetable".</p>';

  document.querySelectorAll('.cutter-marker-row').forEach(row => {
    const i = Number(row.dataset.index);
    row.querySelector('.cutter-m-name').addEventListener('change', (e) => { cutterMarkers[i].name = e.target.value; });
    row.querySelector('.cutter-m-channel').addEventListener('change', (e) => { cutterMarkers[i].channel = e.target.value; });
    row.querySelector('.cutter-m-tracklist').addEventListener('change', (e) => { cutterMarkers[i].tracklist = e.target.value; });
    row.querySelector('.cutter-m-seek').addEventListener('click', () => {
      const player = cutterPlayerEl();
      if (player) { player.currentTime = cutterMarkers[i].offsetSec; player.play(); }
    });
    row.querySelector('.cutter-m-remove').addEventListener('click', () => {
      cutterMarkers.splice(i, 1);
      renderCutterMarkers();
    });
  });
}

$('cutter-add-marker').onclick = () => {
  const player = cutterPlayerEl();
  if (!player) return;
  cutterMarkers.push({ id: `m${Date.now()}`, offsetSec: player.currentTime || 0, name: '', channel: cutterRecording ? (cutterRecording.channel || cutterRecording.source || '') : '' });
  renderCutterMarkers();
};

// Clicking the waveform seeks playback to that position - the timeline is a
// visual scrub bar, not a drag-to-place-marker surface (markers are added at
// the current playback position instead, so "does this sound right" is
// always one click away via the player's own controls).
$('cutter-timeline').addEventListener('click', (e) => {
  const img = $('cutter-waveform-img');
  const rect = img.getBoundingClientRect();
  const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
  const dur = cutterDurationSeconds();
  const player = cutterPlayerEl();
  if (player && dur > 0) player.currentTime = frac * dur;
});

// "Load from timetable": for the event this recording is assigned to, map
// every archived timetable set that falls within the recording's wall-clock
// span onto a file offset using the sidecar's startedAt, and seed one marker
// per set. Requires both a timecode sidecar and an assigned event+channel -
// without either there's nothing to map wall-clock time onto.
$('cutter-load-timetable').onclick = () => {
  if (!cutterRecording || !cutterSidecar || !cutterSidecar.startedAt) {
    toast('This recording has no timecode sidecar yet - use "Backfill timecodes" in Settings first.', 'error');
    return;
  }
  const ev = cutterRecording.eventId ? libEventById(cutterRecording.eventId) : null;
  if (!ev) {
    toast('Organize this recording (assign it to an event) before loading its timetable.', 'error');
    return;
  }
  const channel = cutterRecording.channel || cutterRecording.source || '';
  const stage = (ev.timetable || []).find(st => st.stage.toLowerCase() === channel.toLowerCase());
  if (!stage || !stage.sets.length) {
    toast(`No archived timetable found for channel "${channel}" on ${ev.name}.`, 'error');
    return;
  }
  const startedAt = new Date(cutterSidecar.startedAt).getTime();
  const dur = cutterDurationSeconds();
  let added = 0;
  stage.sets.forEach(s => {
    const setStart = new Date(s.start).getTime();
    const offsetSec = (setStart - startedAt) / 1000;
    if (offsetSec < 0 || (dur > 0 && offsetSec > dur)) return; // outside this file's span
    if (cutterMarkers.some(m => Math.abs(m.offsetSec - offsetSec) < 1)) return; // already have one here
    cutterMarkers.push({
      id: `tt-${s.id || added}`, offsetSec, name: s.name, channel,
      eventId: ev.id, setId: s.id || '', artist: s.name, start: s.start, end: s.end,
    });
    added++;
  });
  renderCutterMarkers();
  toast(added > 0 ? `Added ${added} marker(s) from the timetable` : 'No new sets fell within this recording\'s time span', 'info');
};

// Auto-detect cuts: proposes a refined cut point at every timetable set
// boundary using silence detection (always) and Whisper (optional, only if
// installed server-side). Proposals are reviewed here, never written
// straight into cutterMarkers - "Accept" or "Accept all" does that.

let cutterDetectProposals = [];
let cutterDetectPollTimer = null;

document.addEventListener('DOMContentLoaded', () => {
  const dbSlider = $('cutter-silence-db'), durSlider = $('cutter-silence-dur');
  if (dbSlider) dbSlider.addEventListener('input', () => { $('cutter-silence-db-val').textContent = dbSlider.value; });
  if (durSlider) durSlider.addEventListener('input', () => { $('cutter-silence-dur-val').textContent = parseFloat(durSlider.value).toFixed(1); });
});

$('cutter-auto-detect').onclick = async () => {
  if (!cutterRecording) return;
  const options = {
    silenceThresholdDb: parseFloat($('cutter-silence-db').value) || -50,
    silenceMinDurationSec: parseFloat($('cutter-silence-dur').value) || 2,
    useWhisper: $('cutter-use-whisper').checked,
    whisperLanguage: $('cutter-whisper-lang').value,
  };
  $('cutter-auto-detect').disabled = true;
  $('cutter-detect-status').classList.remove('hidden');
  $('cutter-detect-status').textContent = 'Analyzing timetable boundaries...';
  let job;
  try {
    job = await api('/api/cutter/detect', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: cutterRecording.path, options }),
    });
  } catch (e) { $('cutter-auto-detect').disabled = false; $('cutter-detect-status').classList.add('hidden'); return; }
  pollCutterDetectJob(job.jobId);
};

function stopCutterDetectPoll() {
  if (cutterDetectPollTimer) { clearTimeout(cutterDetectPollTimer); cutterDetectPollTimer = null; }
}

async function pollCutterDetectJob(jobId) {
  stopCutterDetectPoll();
  let job;
  try {
    job = await api(`/api/cutter/detect/jobs/${encodeURIComponent(jobId)}`);
  } catch (e) {
    cutterDetectPollTimer = setTimeout(() => pollCutterDetectJob(jobId), 2000);
    return;
  }
  if (job.status === 'running') {
    $('cutter-detect-status').textContent = `Analyzing... (${(job.log || []).length} boundaries checked so far)`;
    cutterDetectPollTimer = setTimeout(() => pollCutterDetectJob(jobId), 1500);
    return;
  }
  $('cutter-auto-detect').disabled = false;
  $('cutter-detect-status').classList.add('hidden');
  if (job.status === 'error') {
    toast(`Auto-detect failed: ${job.error}`, 'error');
    return;
  }
  cutterDetectProposals = job.proposals || [];
  renderCutterProposals();
  toast(`Auto-detect found ${cutterDetectProposals.length} candidate cut(s)`, 'info');
}

function renderCutterProposals() {
  if (!cutterDetectProposals.length) {
    $('cutter-proposals-box').classList.add('hidden');
    return;
  }
  $('cutter-proposals-box').classList.remove('hidden');
  const confColor = { high: '#4ade80', medium: '#fbbf24', low: '#94a3b8' };
  $('cutter-proposals-list').innerHTML = cutterDetectProposals.map((p, i) => `
    <div class="flex items-center justify-between gap-2 rounded border border-white/10 px-2 py-1 text-sm" data-index="${i}">
      <span class="flex min-w-0 items-center gap-2">
        <span class="cutter-marker-time">${formatClock(p.offsetSec)}</span>
        <span class="truncate">${escapeHtml(p.name)}</span>
        <span style="color:${confColor[p.confidence] || '#94a3b8'}" class="text-xs">${escapeHtml(p.confidence)} · ${escapeHtml(p.source)}</span>
      </span>
      <button type="button" class="btn cutter-proposal-accept flex-shrink-0" data-index="${i}">Accept</button>
    </div>`).join('');
  document.querySelectorAll('.cutter-proposal-accept').forEach(btn => btn.addEventListener('click', () => {
    acceptCutterProposal(Number(btn.dataset.index));
  }));
}

function acceptCutterProposal(i) {
  const p = cutterDetectProposals[i];
  if (!p) return;
  cutterMarkers.push({
    id: `detect-${Date.now()}-${i}`, offsetSec: p.offsetSec, name: p.name, artist: p.artist,
    channel: p.channel, eventId: p.eventId, setId: p.setId, start: p.start, end: p.end,
  });
  cutterDetectProposals.splice(i, 1);
  renderCutterMarkers();
  renderCutterProposals();
}

$('cutter-proposals-accept-all').onclick = () => {
  while (cutterDetectProposals.length) acceptCutterProposal(0);
};

$('cutter-save-markers').onclick = async () => {
  if (!cutterRecording) return;
  try {
    await api(`/api/cutter/markers?path=${encodeURIComponent(cutterRecording.path)}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(cutterMarkers),
    });
    toast('Markers saved', 'info');
  } catch (e) { /* api() already toasted the error */ }
};

let cutterExportPollTimer = null;

$('cutter-export').onclick = async () => {
  if (!cutterRecording) return;
  if (cutterMarkers.length === 0) {
    $('cutter-error').textContent = 'Add at least one marker before exporting.';
    $('cutter-error').classList.remove('hidden');
    return;
  }
  $('cutter-error').classList.add('hidden');
  const precise = $('cutter-precise').checked;
  let job;
  try {
    job = await api('/api/cutter/export', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: cutterRecording.path, markers: cutterMarkers, precise }),
    });
  } catch (e) { return; } // api() already toasted the error
  toast('Export started - splitting in the background...', 'info');
  if (cutterExportPollTimer) clearInterval(cutterExportPollTimer);
  cutterExportPollTimer = setInterval(async () => {
    let status;
    try { status = await api(`/api/cutter/jobs/${job.jobId}`); } catch (e) { clearInterval(cutterExportPollTimer); return; }
    if (status.status === 'done') {
      clearInterval(cutterExportPollTimer);
      toast(`Export complete: ${status.doneSegments} segment(s) saved to the library`, 'info');
      await refresh();
    } else if (status.status === 'error') {
      clearInterval(cutterExportPollTimer);
      toast(`Export failed: ${status.error}`, 'error');
    }
  }, 2000);
};

// ─── Mass Transcode ──────────────────────────────────────────────────────────
// Bulk re-encode a batch of recordings in one background job. Mirrors the
// Share Sets view's selection UI (group-by-channel checkboxes, filter,
// select-all/clear) since it's the same "pick some recordings" interaction.

let transcodeSelected = new Set();
let transcodeJobPollTimer = null;

function openTranscodeView() {
  ['lib-home', 'lib-event-view', 'lib-search-results', 'lib-back', 'lib-match-view', 'lib-share-view', 'lib-receive-view']
    .forEach(id => $(id) && $(id).classList.add('hidden'));
  $('lib-transcode-view').classList.remove('hidden');
  $('lib-title').textContent = 'Recordings Library';
  transcodeSelected = new Set();
  $('lib-transcode-filter').value = '';
  $('lib-transcode-job-box').classList.add('hidden');
  if (transcodeJobPollTimer) { clearTimeout(transcodeJobPollTimer); transcodeJobPollTimer = null; }
  renderTranscodeList();
}
function closeTranscodeView() { $('lib-transcode-view').classList.add('hidden'); reloadLibraryData(); }

function updateTranscodeSelCount() { $('lib-transcode-selcount').textContent = `${transcodeSelected.size} selected`; }

function transcodeFilteredRecordings() {
  const q = $('lib-transcode-filter').value.trim().toLowerCase();
  let list = recordings || [];
  if (q) list = list.filter(r => `${r.name} ${r.channel || ''} ${r.artist || ''}`.toLowerCase().includes(q));
  return list;
}

function renderTranscodeList() {
  const list = transcodeFilteredRecordings();
  if (!list.length) {
    $('lib-transcode-list').innerHTML = '<p class="text-zinc-400">No recordings to transcode yet.</p>';
    updateTranscodeSelCount();
    return;
  }
  const groups = new Map();
  list.forEach(r => { const ch = r.channel || 'Unsorted'; if (!groups.has(ch)) groups.set(ch, []); groups.get(ch).push(r); });
  $('lib-transcode-list').innerHTML = [...groups.entries()].map(([ch, items]) => `
    <div class="source-group open">
      <div class="source-group-head">
        <label class="flex items-center gap-2 font-semibold"><input type="checkbox" class="tc-group-check" data-ch="${escapeAttr(ch)}"> ${escapeHtml(ch)}</label>
        <span class="pill">${items.length}</span>
      </div>
      <div class="source-group-body space-y-1">
        ${items.map(r => `
          <label class="flex items-center justify-between gap-2 rounded border border-white/10 px-2 py-1">
            <span class="flex min-w-0 items-center gap-2">
              <input type="checkbox" class="tc-item-check" data-path="${escapeAttr(r.path)}" ${transcodeSelected.has(r.path) ? 'checked' : ''}>
              <span class="truncate">${escapeHtml(libDisplayTitle(r))}</span>
            </span>
            <span class="flex-shrink-0 text-xs text-zinc-500">${formatBytes(r.size)}</span>
          </label>`).join('')}
      </div>
    </div>`).join('');
  $('lib-transcode-list').querySelectorAll('.tc-item-check').forEach(cb => cb.addEventListener('change', () => {
    if (cb.checked) transcodeSelected.add(cb.dataset.path); else transcodeSelected.delete(cb.dataset.path);
    updateTranscodeSelCount();
  }));
  $('lib-transcode-list').querySelectorAll('.tc-group-check').forEach(cb => cb.addEventListener('change', () => {
    const ch = cb.dataset.ch;
    (groups.get(ch) || []).forEach(r => { if (cb.checked) transcodeSelected.add(r.path); else transcodeSelected.delete(r.path); });
    renderTranscodeList(); updateTranscodeSelCount();
  }));
  updateTranscodeSelCount();
}

$('lib-transcode-open').onclick = openTranscodeView;
$('lib-transcode-close').onclick = closeTranscodeView;
$('lib-transcode-filter').addEventListener('input', renderTranscodeList);
$('lib-transcode-selectall').onclick = () => { transcodeFilteredRecordings().forEach(r => transcodeSelected.add(r.path)); renderTranscodeList(); };
$('lib-transcode-clear').onclick = () => { transcodeSelected = new Set(); renderTranscodeList(); };

$('lib-transcode-start').onclick = async () => {
  if (!transcodeSelected.size) { toast('Select at least one recording to transcode', 'error'); return; }
  const options = {
    container: $('tc-container').value,
    videoCodec: $('tc-video-codec').value,
    audioCodec: $('tc-audio-codec').value,
    crf: parseInt($('tc-crf').value, 10) || 0,
    audioBitrateKbps: parseInt($('tc-bitrate').value, 10) || 0,
    hardwareAccel: $('tc-hwaccel').value,
    replace: $('tc-replace').checked,
  };
  $('lib-transcode-start').disabled = true;
  let result;
  try {
    result = await api('/api/transcode/start', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ paths: [...transcodeSelected], options }),
    });
  } catch (e) { $('lib-transcode-start').disabled = false; return; }
  $('lib-transcode-job-box').classList.remove('hidden');
  pollTranscodeJob(result.jobId);
};

function stopTranscodeJobPoll() {
  if (transcodeJobPollTimer) { clearTimeout(transcodeJobPollTimer); transcodeJobPollTimer = null; }
}

async function pollTranscodeJob(jobId) {
  stopTranscodeJobPoll();
  let job;
  try {
    job = await api(`/api/transcode/jobs/${encodeURIComponent(jobId)}`);
  } catch (e) {
    transcodeJobPollTimer = setTimeout(() => pollTranscodeJob(jobId), 2000);
    return;
  }
  renderTranscodeJob(job);
  if (job.status === 'running') {
    transcodeJobPollTimer = setTimeout(() => pollTranscodeJob(jobId), 1500);
  } else {
    $('lib-transcode-start').disabled = false;
    toast(`Transcode finished: ${job.done} done, ${job.failed} failed`, job.failed > 0 ? 'error' : 'info');
    await refresh();
  }
}

function renderTranscodeJob(job) {
  const pct = job.totalFiles > 0 ? Math.min(100, Math.round((job.done + job.failed) / job.totalFiles * 100)) : 0;
  $('lib-transcode-job-title').textContent = job.status === 'running' ? 'Transcoding...' : 'Transcode finished';
  $('lib-transcode-job-stats').textContent = `${job.done + job.failed}/${job.totalFiles} files · ${job.done} done · ${job.failed} failed`;
  $('lib-transcode-job-bar').style.width = `${pct}%`;
  $('lib-transcode-job-log').textContent = (job.log || []).map(l => `[${l.time}] ${l.text}`).join('\n');
  $('lib-transcode-job-log').scrollTop = $('lib-transcode-job-log').scrollHeight;
}

setupCustomDropdowns();
setupImageUploadFields();

refresh();
setInterval(refresh, 5000);
