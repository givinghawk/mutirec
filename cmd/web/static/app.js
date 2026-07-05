let state = null;
let config = null;
let wave = null;
let hlsPlayer = null;
let recordings = [];
let lolEvents = [];
let selectedTimetableDay = null;
let editingSet = null;
let nowPlayingId = null;
let highlightSourceId = null;
const accents = { red: '#ef4444', cyan: '#06b6d4', lime: '#84cc16', amber: '#f59e0b', pink: '#ec4899' };

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
    renderVersion();
    renderDashboard();
    renderEditors();
  } catch (err) {
    console.error('Render failed:', err);
    toast(`UI failed to render: ${err.message}`, 'error');
  }
}

function renderVersion() {
  const v = state.version || 'dev';
  $('version-footer').textContent = v;
  $('version-footer').title = `Defqon Stream Recorder ${v}`;
  const helpVersion = $('help-version');
  if (helpVersion) helpVersion.textContent = v;
}

function applyTheme() {
  document.body.className = `min-h-screen text-zinc-100 theme-${config.ui.theme || 'midnight'} bg-zinc-950`;
  $('app-name').textContent = config.ui.appName || 'Defqon Stream Recorder';
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

function elapsed(startedAt) {
  const start = new Date(startedAt).getTime();
  if (!start) return '';
  let secs = Math.max(0, Math.floor((Date.now() - start) / 1000));
  const h = Math.floor(secs / 3600); secs -= h * 3600;
  const m = Math.floor(secs / 60); secs -= m * 60;
  return `${h ? h + 'h ' : ''}${m}m ${secs}s`;
}

function renderDashboard() {
  const warnings = state.warnings || [];
  const sources = state.sources || [];
  const events = state.events || [];
  $('active-count').textContent = `${state.activeCount} active`;
  $('warnings').innerHTML = warnings.map(w => `<div class="rounded border border-amber-400/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-100">${escapeHtml(w)}</div>`).join('');
  $('source-grid').innerHTML = sources.length ? sources.map(src => `
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
        ${src.lastError ? `<div class="text-rose-300">Error: ${escapeHtml(src.lastError)}</div>` : ''}
      </div>
      <div class="mt-3 flex flex-wrap gap-2">
        ${src.orphaned ? '' : `<button class="btn" ${src.status === 'recording' ? 'disabled' : ''} onclick="start('${src.id}')">Record</button>`}
        <button class="btn" ${src.status !== 'recording' ? 'disabled' : ''} onclick="stopRec('${src.id}', '${escapeAttr(src.name)}')">Stop</button>
        ${src.orphaned ? '' : `<button class="btn primary" onclick="playLive('${src.id}', ${src.audioOnly ? 'true' : 'false'})">${src.liveRewindActive ? 'Watch Live (rewind)' : 'Watch Live'}</button>`}
        ${src.mediaPath ? `<a class="btn" href="/media/${encodeMediaPath(src.mediaPath)}" target="_blank" rel="noopener">Open</a>` : ''}
      </div>
    </article>`).join('') : '<p class="text-sm text-zinc-400 md:col-span-2">No sources yet — add one from the Sources tab.</p>';
  const free = state.disk.volumeFree || 0;
  const total = state.disk.volumeTotal || 0;
  $('storage').innerHTML = `<div>Free: ${formatBytes(free)}</div><div>Total: ${formatBytes(total)}</div><div>Recorded: ${formatBytes(state.disk.total || 0)}</div>`;
  $('events').innerHTML = [...events].reverse().slice(0, 80).map(e => `<div class="event-${e.level}"><span class="text-zinc-500">${new Date(e.time).toLocaleTimeString()}</span> ${escapeHtml(e.text)}</div>`).join('');
  renderFavoritesPanel();
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

function setupCustomDropdowns() {
  document.querySelectorAll('.custom-dropdown').forEach(dropdown => {
    const toggle = dropdown.querySelector('.dropdown-toggle');
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
        const value = option.dataset.value;
        const label = option.querySelector('.font-semibold').textContent;
        hiddenInput.value = value;
        toggle.textContent = label + '▼';
        menu.classList.add('hidden');
        if (sourceRow) markCardUnsaved(sourceRow);
      });
    });
  });

  document.addEventListener('click', (e) => {
    if (!e.target.closest('.custom-dropdown')) {
      document.querySelectorAll('.custom-dropdown .dropdown-menu').forEach(m => {
        m.classList.add('hidden');
      });
    }
  });
}

function renderEditors() {
  if (!$('source-editor').dataset.loaded) {
    $('source-editor').dataset.loaded = '1';
    drawSourceEditor();
    $('timetable-json').value = JSON.stringify(config.timetable, null, 2);
    fillSettings();
    loadAccount();
    renderVisualTimetable();
    renderLinkedBadge();
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
          <label>Type<select class="input src-type"><option ${sel(s.type,'youtube')}>youtube</option><option ${sel(s.type,'twitch')}>twitch</option><option ${sel(s.type,'http')}>http</option></select></label>
          <label>URL<input class="input src-url" value="${escapeAttr(s.url)}"></label>
          <label>Quality<input class="input src-quality" value="${escapeAttr(s.quality || 'best')}"></label>
          <label>Container
            <div class="custom-dropdown" data-field="src-container" data-value="${escapeAttr(s.container || 'mkv')}">
              <button type="button" class="dropdown-toggle input">${s.container || 'mkv'}<span class="ml-auto">▼</span></button>
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
            <div class="custom-dropdown" data-field="src-transcode" data-value="${s.transcode ? 'yes' : 'no'}">
              <button type="button" class="dropdown-toggle input">${s.transcode ? 'Yes (re-encode)' : 'No (copy codec)'}<span class="ml-auto">▼</span></button>
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
            <div class="custom-dropdown" data-field="src-liverewind" data-value="${s.liveRewind ? 'hls' : 'none'}">
              <button type="button" class="dropdown-toggle input">${s.liveRewind ? 'HLS Buffer' : 'Disabled'}<span class="ml-auto">▼</span></button>
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
          <label>HW accel<select class="input src-hw"><option ${sel(s.hardwareAccel,'')}>none</option><option ${sel(s.hardwareAccel,'cuda')}>cuda</option><option ${sel(s.hardwareAccel,'qsv')}>qsv</option><option ${sel(s.hardwareAccel,'vaapi')}>vaapi</option></select></label>
          <label>Color<input class="input src-color" value="${escapeAttr(s.color || '')}"></label>
          <label>NFO note<input class="input src-nfo" value="${escapeAttr(s.extraNfo || '')}"></label>
          <label title="Matches this source to a stage name in the Timetable tab for Now/Next lookup, if it doesn't match this source's own name.">Timetable stage<input class="input src-ttstage" list="timetable-stage-names" value="${escapeAttr(s.timetableStage || '')}" placeholder="defaults to source name"></label>
          <label class="inline-flex items-center gap-2"><input class="src-enabled" type="checkbox" ${s.enabled ? 'checked' : ''}> Enabled</label>
          <label class="inline-flex items-center gap-2"><input class="src-record" type="checkbox" ${s.record ? 'checked' : ''}> Auto record</label>
          <label class="inline-flex items-center gap-2"><input class="src-audio" type="checkbox" ${s.audioOnly ? 'checked' : ''}> Audio only</label>
        </div>
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
    transcode: el.querySelector('.src-transcode').value === 'yes',
    liveRewind: el.querySelector('.src-liverewind').value !== 'none',
    timetableStage: el.querySelector('.src-ttstage').value
  };
}

function sourceCardEl(i) { return document.querySelector(`.source-row[data-source="${i}"]`); }

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
      body: JSON.stringify({ type: values.type, url: values.url, quality: values.quality })
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

function fillSettings() {
  const s = config.settings, ui = config.ui;
  ['finishedDir','tempDir','logDir','checkIntervalSeconds','minFreeBytes','warnFreeBytes','liveRewindWindowSeconds','reminderLeadMinutes'].forEach(k => $(k).value = s[k]);
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => $(k).checked = !!s[k]);
  $('uiAppName').value = ui.appName || '';
  $('uiLogoUrl').value = ui.logoUrl || '';
  $('uiCustomCss').value = ui.customCss || '';
  $('discordWebhook').value = s.notifications.discordWebhook || '';
  $('smtpEnabled').checked = !!s.notifications.smtp.enabled;
  ['smtpHost','smtpUsername','smtpPassword','smtpFrom','smtpTo'].forEach(id => $(id).value = s.notifications.smtp[id.replace('smtp','').toLowerCase()] || '');
  $('smtpPort').value = s.notifications.smtp.port || 587;
  $('backupEnabled').checked = !!s.backup.enabled;
  $('backupAfterComplete').checked = !!s.backup.afterComplete;
  $('rcloneRemote').value = s.backup.rcloneRemote || '';
  $('rcloneArgs').value = (s.backup.rcloneArgs || []).join('\n');
  $('wave-toggle').checked = !!s.enableWaveform;

  if (ui.themeColors) {
    updateColorInputs(ui.themeColors);
  }
  renderFestivalPresets();
}

function readSettings() {
  const s = config.settings;
  ['finishedDir','tempDir','logDir'].forEach(k => s[k] = $(k).value);
  ['checkIntervalSeconds','minFreeBytes','warnFreeBytes','liveRewindWindowSeconds','reminderLeadMinutes'].forEach(k => s[k] = Number($(k).value));
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => s[k] = $(k).checked);
  config.ui = { appName: $('uiAppName').value, logoUrl: $('uiLogoUrl').value, customCss: $('uiCustomCss').value, customTheme: config.ui.customTheme, themeColors: config.ui.themeColors };
  s.notifications.discordWebhook = $('discordWebhook').value;
  s.notifications.smtp = { enabled: $('smtpEnabled').checked, host: $('smtpHost').value, port: Number($('smtpPort').value), username: $('smtpUsername').value, password: $('smtpPassword').value, from: $('smtpFrom').value, to: $('smtpTo').value };
  s.backup = { enabled: $('backupEnabled').checked, afterComplete: $('backupAfterComplete').checked, rcloneRemote: $('rcloneRemote').value, rcloneArgs: $('rcloneArgs').value.split('\n').map(x => x.trim()).filter(Boolean) };
}

async function saveConfig() {
  await api('/api/config', { method: 'PUT', body: JSON.stringify(config), headers: { 'Content-Type': 'application/json' } });
  $('source-editor').dataset.loaded = '';
  toast('Saved', 'info');
  await refresh();
}

async function loadAccount() {
  let acct;
  try {
    acct = await api('/api/account');
  } catch {
    return;
  }
  $('acct-username').value = acct.username || '';
  if (acct.managedByEnv) {
    $('account-note').textContent = `Signed in as "${acct.username}". Credentials for this deployment are set via AUTH_USERNAME/AUTH_PASSWORD environment variables and can't be changed here.`;
    $('account-form').classList.add('hidden');
  } else {
    $('account-note').textContent = `Signed in as "${acct.username}".`;
    $('account-form').classList.remove('hidden');
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

async function start(id) { try { await api(`/api/record/${id}`, { method: 'POST' }); } catch { return; } await refresh(); }
async function stopRec(id, name) {
  if (!confirm(`Stop recording "${name || 'this source'}"? The file recorded so far will be kept.`)) return;
  try { await api(`/api/record/${id}`, { method: 'DELETE' }); } catch { return; }
  await refresh();
}

function playLive(id, audioOnly) {
  const src = state.sources.find(s => s.id === id);
  if (!src) return;
  // Twitch/YouTube (resolved via streamlink) and most raw HTTP sources are
  // served as HLS (.m3u8), which only Safari can play natively - every other
  // browser needs hls.js or "Watch Live" silently does nothing. The live
  // rewind buffer is always HLS; otherwise guess from the source itself.
  const looksLikeHls = src.liveRewindActive || src.type !== 'http' || /\.m3u8(\?|$)/i.test(src.url || '');
  const useHls = looksLikeHls && window.Hls && Hls.isSupported();
  const url = src.liveRewindActive ? `/api/live/${id}/hls/index.m3u8` : `/api/live/${id}`;
  const audioEl = $('audio'), videoEl = $('video');
  audioEl.classList.toggle('hidden', !audioOnly);
  videoEl.classList.toggle('hidden', audioOnly);
  const el = audioOnly ? audioEl : videoEl;
  (audioOnly ? videoEl : audioEl).pause();

  nowPlayingId = id;
  $('player-empty').classList.add('hidden');
  $('player-now').textContent = `— ${src.name}${audioOnly ? ' (audio)' : ''}`;
  renderDashboard();

  const panel = $('player-panel');
  panel.scrollIntoView({ behavior: 'smooth', block: 'center' });
  panel.classList.remove('flash');
  void panel.offsetWidth;
  panel.classList.add('flash');

  if (hlsPlayer) { hlsPlayer.destroy(); hlsPlayer = null; }

  const statusEl = $('player-status');
  if (src.liveRewindActive) {
    statusEl.textContent = 'Live rewind buffer connecting — drag the seek bar back to scrub within this recording.';
    statusEl.classList.remove('hidden');
  } else {
    statusEl.classList.add('hidden');
  }

  if (useHls) {
    hlsPlayer = new Hls({ liveSyncDurationCount: 3 });
    hlsPlayer.on(Hls.Events.ERROR, (_evt, data) => {
      if (data.fatal) toast(`Live stream error: ${data.details}`, 'error');
    });
    hlsPlayer.loadSource(url);
    hlsPlayer.attachMedia(el);
    hlsPlayer.on(Hls.Events.MANIFEST_PARSED, () => el.play().catch(err => toast(`Could not start playback: ${err.message}`, 'error')));
  } else {
    // Safari (and anything without hls.js) can play HLS natively via <video src>.
    el.src = url;
    el.play().catch(err => toast(`Could not start playback: ${err.message}`, 'error'));
  }

  if (!useHls && $('wave-toggle').checked && window.WaveSurfer) {
    $('wave').classList.remove('hidden');
    if (wave) wave.destroy();
    wave = WaveSurfer.create({ container: '#wave', waveColor: '#52525b', progressColor: accents[config.ui.accent] || accents.red, height: 80 });
    wave.load(url);
  } else {
    if (wave) { wave.destroy(); wave = null; }
    $('wave').classList.add('hidden');
  }
}

document.querySelectorAll('.nav').forEach(b => b.onclick = async () => {
  document.querySelectorAll('.nav').forEach(x => x.classList.remove('active'));
  document.querySelectorAll('.view').forEach(x => x.classList.add('hidden'));
  b.classList.add('active');
  $(b.dataset.view).classList.remove('hidden');
  // Each tab keeps its own view element rather than navigating to a real
  // separate page, but it must still behave like one: pull fresh data from
  // the server and redraw from scratch every time it's opened, instead of
  // showing whatever was rendered (possibly stale) the last time it was open.
  $('source-editor').dataset.loaded = '';
  await refresh();
  if (b.dataset.view === 'recordings') initLibrary();
});

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

function timetableDays() {
  const days = new Set();
  (config.timetable || []).forEach(st => (st.sets || []).forEach(s => { const p = parseIso(s.start); if (p) days.add(p.date); }));
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
  let minMin = 24 * 60, maxMin = 0, any = false;
  config.timetable.forEach(st => (st.sets || []).forEach(s => {
    const sp = parseIso(s.start), ep = parseIso(s.end);
    if (sp && sp.date === selectedTimetableDay) {
      any = true;
      minMin = Math.min(minMin, sp.minutes);
      maxMin = Math.max(maxMin, (ep && ep.date === selectedTimetableDay ? ep.minutes : sp.minutes + 60));
    }
  }));
  if (!any) { minMin = 12 * 60; maxMin = 24 * 60; }
  const span = Math.max(60, maxMin - minMin);

  $('timetable-visual').innerHTML = config.timetable.map((st, si) => {
    const color = st.color || stagePalette(st.stage);
    const blocks = (st.sets || []).map((set, seti) => {
      const sp = parseIso(set.start), ep = parseIso(set.end);
      if (!sp || sp.date !== selectedTimetableDay) return '';
      const left = ((sp.minutes - minMin) / span) * 100;
      const width = Math.max(3, (((ep ? ep.minutes : sp.minutes + 60) - sp.minutes) / span) * 100);
      const starred = set.id && favIds.has(set.id);
      return `<div class="tt-block" style="left:${left}%;width:${width}%;background:${color}" title="${escapeAttr(set.name)}">
        <button type="button" class="tt-star-btn ${starred ? 'active' : ''}" onclick="event.stopPropagation();toggleFavorite('${set.id || ''}')">&#9733;</button>
        <span onclick="editTimetableSet(${si},${seti})">${escapeHtml(set.name)}</span>
      </div>`;
    }).join('');
    return `<div class="tt-row">
      <div class="tt-row-label" style="color:${color}" title="${escapeAttr(st.stage)}">${escapeHtml(st.stage)}</div>
      <div class="tt-row-track">${blocks}</div>
      <button type="button" class="btn" onclick="addTimetableSet(${si})">+ Set</button>
    </div>`;
  }).join('');
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

async function loadLolEvents() {
  const select = $('tt-lol-event');
  try {
    const result = await api('/api/timetable/lol-events');
    lolEvents = result.events || [];
    select.innerHTML = lolEvents.map(e => `<option value="${escapeAttr(e.slug)}">${escapeHtml(e.title || e.slug)}${e.year ? ' (' + escapeHtml(e.year) + ')' : ''}</option>`).join('') || '<option value="">No events found</option>';
    $('tt-lol-status').textContent = `${lolEvents.length} events available from timetable.lol.`;
  } catch {
    select.innerHTML = '<option value="">Could not reach timetable.lol</option>';
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
    const items = byChannel.get(ch).sort((a, b) => (b.start || b.modTime || '').localeCompare(a.start || a.modTime || ''));
    return `
      <div class="lib-channel-row">
        <h3 class="mb-2 font-semibold">${escapeHtml(ch)}</h3>
        <div class="lib-row-scroll">${items.map(r => libSetCardHtml(r)).join('')}</div>
      </div>`;
  }).join('');

  document.querySelectorAll('.lib-set-card').forEach(card => {
    const path = card.dataset.path;
    const r = rows.find(x => x.path === path);
    if (!r) return;
    card.querySelector('.lib-set-play').addEventListener('click', () => openRecordingPlayer(r.path, libDisplayTitle(r)));
    card.querySelector('.lib-set-organize').addEventListener('click', (e) => { e.stopPropagation(); openAssignModal(r); });
  });
}

function libSetCardHtml(r) {
  const title = escapeHtml(libDisplayTitle(r));
  const when = r.start
    ? new Date(r.start).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : new Date(r.modTime).toLocaleDateString();
  return `
    <div class="lib-set-card" data-path="${escapeAttr(r.path)}">
      <div class="lib-set-thumb lib-set-play">
        <span class="lib-set-play-icon">&#9658;</span>
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
  $('ev-desc').value = ev ? (ev.description || '') : '';
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
  $('assign-find-result').textContent = '';
  $('assign-error').classList.add('hidden');

  $('assign-event').innerHTML = `<option value="">Unsorted</option>` + libraryEvents().map(e => `<option value="${escapeAttr(e.id)}">${escapeHtml(e.name)}</option>`).join('');
  $('assign-event').value = r.eventId || '';

  setAssignMode('artist');
  populateAssignSetOptions();
  $('assign-overlay').classList.remove('hidden');
}
function closeAssignModal() { $('assign-overlay').classList.add('hidden'); libAssignTarget = null; }
$('assign-close').onclick = closeAssignModal;
$('assign-overlay').addEventListener('click', (e) => { if (e.target.id === 'assign-overlay') closeAssignModal(); });

function setAssignMode(mode) {
  document.querySelectorAll('.assign-mode-btn').forEach(b => b.classList.toggle('active', b.dataset.mode === mode));
  $('assign-mode-artist').classList.toggle('hidden', mode !== 'artist');
  $('assign-mode-time').classList.toggle('hidden', mode !== 'time');
}
document.querySelectorAll('.assign-mode-btn').forEach(b => b.addEventListener('click', () => setAssignMode(b.dataset.mode)));
$('assign-event').addEventListener('change', () => populateAssignSetOptions());

async function populateAssignSetOptions() {
  const eventId = $('assign-event').value;
  const setSel = $('assign-set');
  if (!eventId) {
    setSel.innerHTML = `<option value="">Choose an event first</option>`;
    return;
  }
  setSel.innerHTML = `<option value="">Loading…</option>`;
  const tt = await fetchEventTimetable(eventId);
  if (!tt.length) {
    setSel.innerHTML = `<option value="">No timetable imported for this event yet</option>`;
    return;
  }
  const opts = ['<option value="">— choose a set —</option>'];
  tt.forEach(stage => (stage.sets || []).forEach(set => {
    const when = set.start ? new Date(set.start).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : '';
    opts.push(`<option value="${escapeAttr(set.id)}">${escapeHtml(stage.stage)} · ${escapeHtml(when)} · ${escapeHtml(set.name)}</option>`);
  }));
  setSel.innerHTML = opts.join('');
}

$('assign-set').addEventListener('change', () => {
  const opt = $('assign-set').selectedOptions[0];
  if (!opt || !opt.value) return;
  const parts = opt.textContent.split(' · ');
  $('assign-artist').value = parts.slice(2).join(' · ') || opt.textContent;
});

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
  $('assign-set').value = best.id;
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

let customPlayerBound = false;

function openRecordingPlayer(path, name) {
  const video = $('rec-video');
  $('rec-player-title').textContent = name || 'Recording';
  $('recording-player-overlay').classList.remove('hidden');
  setupCustomPlayerControls(video);
  video.src = `/media/${encodeMediaPath(path)}`;
  video.currentTime = 0;
  video.play().catch(() => { /* autoplay may be blocked; controls still work */ });
}

function closeRecordingPlayer() {
  const video = $('rec-video');
  video.pause();
  video.removeAttribute('src');
  video.load();
  $('recording-player-overlay').classList.add('hidden');
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
}

$('rec-player-close').onclick = closeRecordingPlayer;
$('recording-player-overlay').addEventListener('click', (e) => {
  if (e.target.id === 'recording-player-overlay') closeRecordingPlayer();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('recording-player-overlay').classList.contains('hidden')) closeRecordingPlayer();
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

// Bound once: the modal reuses a single <video> element, so controls only
// need to be wired up the first time the player is opened.
function setupCustomPlayerControls(video) {
  if (customPlayerBound) return;
  customPlayerBound = true;

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
    const icon = playing ? '&#10074;&#10074;' : '&#9658;';
    playPauseBtn.innerHTML = icon;
  };

  const togglePlay = () => { if (video.paused || video.ended) video.play().catch(() => {}); else video.pause(); };

  playPauseBtn.onclick = togglePlay;
  centerPlay.onclick = togglePlay;
  video.addEventListener('click', togglePlay);
  video.addEventListener('play', () => { setPlayIcon(true); centerPlay.classList.add('hidden'); });
  video.addEventListener('pause', () => { setPlayIcon(false); centerPlay.classList.remove('hidden'); });
  video.addEventListener('ended', () => { setPlayIcon(false); centerPlay.classList.remove('hidden'); });
  video.addEventListener('waiting', () => $('custom-player').classList.add('cp-buffering'));
  video.addEventListener('playing', () => $('custom-player').classList.remove('cp-buffering'));
  video.addEventListener('error', () => toast('Could not play this recording — the file may be missing or unsupported', 'error'));

  back10.onclick = () => { video.currentTime = Math.max(0, video.currentTime - 10); };
  fwd10.onclick = () => { video.currentTime = Math.min(video.duration || video.currentTime + 10, video.currentTime + 10); };

  video.addEventListener('timeupdate', () => {
    if (scrubbing || !isFinite(video.duration) || !video.duration) return;
    const pct = (video.currentTime / video.duration) * 1000;
    seek.value = pct;
    seekProgress.style.width = `${pct / 10}%`;
    timeEl.textContent = `${formatTime(video.currentTime)} / ${formatTime(video.duration)}`;
  });
  video.addEventListener('progress', () => {
    if (video.buffered.length && isFinite(video.duration) && video.duration) {
      const end = video.buffered.end(video.buffered.length - 1);
      seekBuffer.style.width = `${(end / video.duration) * 100}%`;
    }
  });
  video.addEventListener('loadedmetadata', () => {
    timeEl.textContent = `${formatTime(0)} / ${formatTime(video.duration)}`;
  });

  seek.addEventListener('input', () => {
    scrubbing = true;
    const pct = seek.value / 1000;
    seekProgress.style.width = `${pct * 100}%`;
    if (isFinite(video.duration) && video.duration) timeEl.textContent = `${formatTime(pct * video.duration)} / ${formatTime(video.duration)}`;
  });
  seek.addEventListener('change', () => {
    if (isFinite(video.duration) && video.duration) video.currentTime = (seek.value / 1000) * video.duration;
    scrubbing = false;
  });
  seekWrap.addEventListener('mousedown', () => { scrubbing = true; });

  volume.addEventListener('input', () => {
    video.volume = parseFloat(volume.value);
    video.muted = video.volume === 0;
    muteBtn.innerHTML = video.muted ? '&#128263;' : '&#128266;';
  });
  muteBtn.onclick = () => {
    video.muted = !video.muted;
    muteBtn.innerHTML = video.muted ? '&#128263;' : '&#128266;';
    if (!video.muted && video.volume === 0) { video.volume = 1; volume.value = 1; }
  };

  speed.addEventListener('change', () => { video.playbackRate = parseFloat(speed.value); });

  fullscreenBtn.onclick = () => {
    const el = $('custom-player');
    if (document.fullscreenElement) document.exitFullscreen();
    else el.requestFullscreen?.().catch(() => {});
  };
}

function sel(v, x) { return (v || '') === x ? 'selected' : ''; }
function encodeMediaPath(p) { return p.split('/').map(encodeURIComponent).join('/'); }
function formatBytes(v) { const u = ['B','KB','MB','GB','TB']; let i = 0; while (v > 1024 && i < u.length - 1) { v /= 1024; i++; } return `${v.toFixed(i ? 1 : 0)} ${u[i]}`; }
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function escapeAttr(s) { return escapeHtml(s).replace(/"/g, '&quot;'); }

// --- Theme Switcher ---

const festivalThemes = {
  'defqon': { name: 'Defqon.1', colors: { primary: '#ef4444', secondary: '#dc2626', bg: '#09090b', accent: '#ef4444', text: '#f4f4f5', textMuted: '#a1a1aa' } },
  'qlimax': { name: 'Qlimax', colors: { primary: '#7c3aed', secondary: '#6d28d9', bg: '#0a0a0a', accent: '#ec4899', text: '#f4f4f5', textMuted: '#a1a1aa' } },
  'defqon-pink': { name: 'Defqon Pink', colors: { primary: '#ec4899', secondary: '#be185d', bg: '#0f0a0a', accent: '#ec4899', text: '#fce7f3', textMuted: '#be185d' } },
  'defqon-cyan': { name: 'Defqon Cyan', colors: { primary: '#06b6d4', secondary: '#0891b2', bg: '#000d0f', accent: '#06b6d4', text: '#cffafe', textMuted: '#0a7ea4' } },
  'defqon-gold': { name: 'Defqon Gold', colors: { primary: '#f59e0b', secondary: '#d97706', bg: '#0b0803', accent: '#fbbf24', text: '#f9f5f0', textMuted: '#92400e' } },
  'xtreme': { name: 'Xtreme (Lime)', colors: { primary: '#84cc16', secondary: '#65a30d', bg: '#0a0a0a', accent: '#84cc16', text: '#f4f4f5', textMuted: '#4b5320' } },
  'mysteryland': { name: 'Mysteryland', colors: { primary: '#a855f7', secondary: '#9333ea', bg: '#0d0010', accent: '#d946ef', text: '#f4f4f5', textMuted: '#8b5cf6' } },
  'tomorrowland': { name: 'Tomorrowland', colors: { primary: '#3b82f6', secondary: '#1d4ed8', bg: '#000812', accent: '#0ea5e9', text: '#f4f4f5', textMuted: '#60a5fa' } },
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
  const css = `
    :root {
      --color-primary: ${colors.primary};
      --color-secondary: ${colors.secondary};
      --color-bg: ${colors.bg};
      --color-accent: ${colors.accent};
      --color-text: ${colors.text};
      --color-text-muted: ${colors.textMuted};
    }
    body { background: var(--color-bg); color: var(--color-text); }
    .panel { background: rgb(${hexToRgb(colors.bg).join(' ')} / .72); }
    .accent-color { color: var(--color-accent); }
    .btn.primary, .nav.active { background: var(--color-accent); }
    .source-card { border-left-color: var(--color-accent); }
    .nav { color: var(--color-text-muted); }
    label { color: var(--color-text-muted); }
  `;
  const styleEl = document.getElementById('custom-css');
  if (styleEl) {
    styleEl.textContent = config.ui.customCss + '\n' + css;
  }
  document.documentElement.style.setProperty('--accent', colors.accent);
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

document.addEventListener('DOMContentLoaded', () => {
  ['colorPrimary', 'colorSecondary', 'colorBg', 'colorAccent', 'colorText', 'colorTextMuted'].forEach(id => {
    const el = $(id);
    if (el) el.addEventListener('change', () => {
      const colors = readCustomTheme();
      applyThemeColors(colors);
    });
  });
});

refresh();
setInterval(refresh, 5000);
