let state = null;
let config = null;
let wave = null;
let hlsPlayer = null;
let recordings = [];
const accents = { red: '#ef4444', cyan: '#06b6d4', lime: '#84cc16', amber: '#f59e0b', pink: '#ec4899' };

async function api(path, opts) {
  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    toast(`Network error: ${err.message}`, 'error');
    throw err;
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
  applyTheme();
  renderDashboard();
  renderEditors();
}

function applyTheme() {
  document.body.className = `min-h-screen text-zinc-100 theme-${config.ui.theme || 'midnight'} bg-zinc-950`;
  document.documentElement.style.setProperty('--accent', accents[config.ui.accent] || accents.red);
  $('app-name').textContent = config.ui.appName || 'Defqon Stream Recorder';
  $('custom-css').textContent = config.ui.customCss || '';
  if (config.ui.logoUrl) {
    $('logo').src = config.ui.logoUrl;
    $('logo').classList.remove('hidden');
  } else {
    $('logo').classList.add('hidden');
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
  $('active-count').textContent = `${state.activeCount} active`;
  $('warnings').innerHTML = state.warnings.map(w => `<div class="rounded border border-amber-400/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-100">${escapeHtml(w)}</div>`).join('');
  $('source-grid').innerHTML = state.sources.map(src => `
    <article class="source-card" style="border-left-color:${src.color || 'var(--accent)'}">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3>${escapeHtml(src.name)}</h3>
          <p class="text-sm text-zinc-400">${escapeHtml(src.type)} · ${escapeHtml(src.quality || 'best')} · ${escapeHtml(src.container || 'mkv')}</p>
        </div>
        <span class="pill status-${escapeHtml(src.status)}">${escapeHtml(src.status)}</span>
      </div>
      <div class="mt-3 text-sm text-zinc-300">
        <div>Now: ${escapeHtml(src.currentSet || 'No current set')}</div>
        <div>Next: ${escapeHtml(src.nextSet || 'No upcoming set')}</div>
        <div>Size: ${formatBytes(src.size || 0)}${src.status === 'recording' ? ` · Recording for ${elapsed(src.startedAt)}` : ''}</div>
        ${src.lastError ? `<div class="text-rose-300">Error: ${escapeHtml(src.lastError)}</div>` : ''}
      </div>
      <div class="mt-3 flex flex-wrap gap-2">
        <button class="btn" ${src.status === 'recording' ? 'disabled' : ''} onclick="start('${src.id}')">Record</button>
        <button class="btn" ${src.status !== 'recording' ? 'disabled' : ''} onclick="stopRec('${src.id}', '${escapeAttr(src.name)}')">Stop</button>
        <button class="btn" onclick="playLive('${src.id}', ${src.audioOnly ? 'true' : 'false'})">${src.liveRewindActive ? 'Live (rewind)' : 'Live'}</button>
        ${src.mediaPath ? `<a class="btn" href="/media/${encodeMediaPath(src.mediaPath)}" target="_blank" rel="noopener">Open</a>` : ''}
      </div>
    </article>`).join('');
  const free = state.disk.volumeFree || 0;
  const total = state.disk.volumeTotal || 0;
  $('storage').innerHTML = `<div>Free: ${formatBytes(free)}</div><div>Total: ${formatBytes(total)}</div><div>Recorded: ${formatBytes(state.disk.total || 0)}</div>`;
  $('events').innerHTML = [...state.events].reverse().slice(0, 80).map(e => `<div class="event-${e.level}"><span class="text-zinc-500">${new Date(e.time).toLocaleTimeString()}</span> ${escapeHtml(e.text)}</div>`).join('');
}

function renderEditors() {
  if (!$('source-editor').dataset.loaded) {
    $('source-editor').dataset.loaded = '1';
    drawSourceEditor();
    $('timetable-json').value = JSON.stringify(config.timetable, null, 2);
    fillSettings();
  }
}

function drawSourceEditor() {
  $('source-editor').innerHTML = config.sources.map((s, i) => `
    <div class="grid gap-2 rounded border border-white/10 p-3 md:grid-cols-4" data-source="${i}">
      <label>Name<input class="input src-name" value="${escapeAttr(s.name)}"></label>
      <label>Type<select class="input src-type"><option ${sel(s.type,'youtube')}>youtube</option><option ${sel(s.type,'twitch')}>twitch</option><option ${sel(s.type,'http')}>http</option></select></label>
      <label>URL<input class="input src-url" value="${escapeAttr(s.url)}"></label>
      <label>Quality<input class="input src-quality" value="${escapeAttr(s.quality || 'best')}"></label>
      <label>Container<input class="input src-container" value="${escapeAttr(s.container || 'mkv')}"></label>
      <label>HW accel<select class="input src-hw"><option ${sel(s.hardwareAccel,'')}>none</option><option ${sel(s.hardwareAccel,'cuda')}>cuda</option><option ${sel(s.hardwareAccel,'qsv')}>qsv</option><option ${sel(s.hardwareAccel,'vaapi')}>vaapi</option></select></label>
      <label>Color<input class="input src-color" value="${escapeAttr(s.color || '')}"></label>
      <label>NFO note<input class="input src-nfo" value="${escapeAttr(s.extraNfo || '')}"></label>
      <label class="inline-flex items-center gap-2"><input class="src-enabled" type="checkbox" ${s.enabled ? 'checked' : ''}> Enabled</label>
      <label class="inline-flex items-center gap-2"><input class="src-record" type="checkbox" ${s.record ? 'checked' : ''}> Auto record</label>
      <label class="inline-flex items-center gap-2"><input class="src-audio" type="checkbox" ${s.audioOnly ? 'checked' : ''}> Audio only</label>
      <label class="inline-flex items-center gap-2"><input class="src-transcode" type="checkbox" ${s.transcode ? 'checked' : ''}> Transcode</label>
      <label class="inline-flex items-center gap-2" title="Lets viewers scrub backward while this source is recording live, using a rolling HLS buffer. Uses extra CPU for the transcode."><input class="src-liverewind" type="checkbox" ${s.liveRewind ? 'checked' : ''}> Live rewind</label>
      <div class="col-span-full flex flex-wrap items-center gap-2 pt-1">
        <button type="button" class="btn" onclick="testSource(${i})">Test Stream</button>
        <button type="button" class="btn" onclick="duplicateSource(${i})">Duplicate</button>
        <button type="button" class="btn" style="color:#fda4af" onclick="deleteSource(${i})">Delete</button>
        <span class="test-result text-sm text-zinc-400" id="test-result-${i}"></span>
      </div>
    </div>`).join('');
}

function readSources() {
  return [...document.querySelectorAll('[data-source]')].map((el, i) => ({
    ...config.sources[i],
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
    transcode: el.querySelector('.src-transcode').checked,
    liveRewind: el.querySelector('.src-liverewind').checked
  }));
}

async function testSource(i) {
  config.sources = readSources();
  const s = config.sources[i];
  const label = $(`test-result-${i}`);
  label.textContent = 'Testing…';
  try {
    const result = await api('/api/sources/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: s.type, url: s.url, quality: s.quality })
    });
    label.textContent = result.ok ? `Resolved OK` : `Failed: ${result.error}`;
    label.className = `test-result text-sm ${result.ok ? 'text-emerald-300' : 'text-rose-300'}`;
    if (result.ok) toast(`${s.name || 'Source'}: stream resolved successfully`, 'info');
  } catch (err) {
    label.textContent = 'Test request failed';
  }
}

function duplicateSource(i) {
  config.sources = readSources();
  const copy = { ...config.sources[i], id: undefined, name: `${config.sources[i].name} copy` };
  config.sources.splice(i + 1, 0, copy);
  drawSourceEditor();
}

function deleteSource(i) {
  config.sources = readSources();
  const src = config.sources[i];
  if (!confirm(`Delete source "${src.name}"? This does not delete existing recordings.`)) return;
  config.sources.splice(i, 1);
  drawSourceEditor();
}

function fillSettings() {
  const s = config.settings, ui = config.ui;
  ['finishedDir','tempDir','logDir','checkIntervalSeconds','minFreeBytes','warnFreeBytes','liveRewindWindowSeconds'].forEach(k => $(k).value = s[k]);
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => $(k).checked = !!s[k]);
  $('uiAppName').value = ui.appName || '';
  $('uiLogoUrl').value = ui.logoUrl || '';
  $('uiTheme').value = ui.theme || 'midnight';
  $('uiAccent').value = ui.accent || 'red';
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
}

function readSettings() {
  const s = config.settings;
  ['finishedDir','tempDir','logDir'].forEach(k => s[k] = $(k).value);
  ['checkIntervalSeconds','minFreeBytes','warnFreeBytes','liveRewindWindowSeconds'].forEach(k => s[k] = Number($(k).value));
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => s[k] = $(k).checked);
  config.ui = { appName: $('uiAppName').value, logoUrl: $('uiLogoUrl').value, theme: $('uiTheme').value, accent: $('uiAccent').value, customCss: $('uiCustomCss').value };
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

async function start(id) { try { await api(`/api/record/${id}`, { method: 'POST' }); } catch { return; } await refresh(); }
async function stopRec(id, name) {
  if (!confirm(`Stop recording "${name || 'this source'}"? The file recorded so far will be kept.`)) return;
  try { await api(`/api/record/${id}`, { method: 'DELETE' }); } catch { return; }
  await refresh();
}

function playLive(id, audioOnly) {
  const src = state.sources.find(s => s.id === id);
  const useHls = !!(src && src.liveRewindActive);
  const url = useHls ? `/api/live/${id}/hls/index.m3u8` : `/api/live/${id}`;
  const audioEl = $('audio'), videoEl = $('video');
  audioEl.classList.toggle('hidden', !audioOnly);
  videoEl.classList.toggle('hidden', audioOnly);
  const el = audioOnly ? audioEl : videoEl;
  (audioOnly ? videoEl : audioEl).pause();

  if (hlsPlayer) { hlsPlayer.destroy(); hlsPlayer = null; }

  const statusEl = $('player-status');
  if (useHls) {
    statusEl.textContent = 'Live rewind buffer connecting — drag the seek bar back to scrub within this recording.';
    statusEl.classList.remove('hidden');
  } else {
    statusEl.classList.add('hidden');
  }

  if (useHls && window.Hls && Hls.isSupported()) {
    hlsPlayer = new Hls({ liveSyncDurationCount: 3 });
    hlsPlayer.on(Hls.Events.ERROR, (_evt, data) => {
      if (data.fatal) toast(`Live rewind stream error: ${data.details}`, 'error');
    });
    hlsPlayer.loadSource(url);
    hlsPlayer.attachMedia(el);
    hlsPlayer.on(Hls.Events.MANIFEST_PARSED, () => el.play().catch(err => toast(`Could not start playback: ${err.message}`, 'error')));
  } else {
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

document.querySelectorAll('.nav').forEach(b => b.onclick = () => {
  document.querySelectorAll('.nav').forEach(x => x.classList.remove('active'));
  document.querySelectorAll('.view').forEach(x => x.classList.add('hidden'));
  b.classList.add('active');
  $(b.dataset.view).classList.remove('hidden');
  if (b.dataset.view === 'recordings') loadRecordings();
});

$('add-source').onclick = () => { config.sources.push({ id: undefined, name: 'New Source', type: 'youtube', url: '', enabled: true, record: false, quality: 'best', container: 'mkv' }); drawSourceEditor(); };
$('save-sources').onclick = async () => {
  config.sources = readSources();
  const missing = config.sources.find(s => !s.name.trim() || !s.url.trim());
  if (missing) { toast('Every source needs a name and URL', 'error'); return; }
  await saveConfig();
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

async function loadRecordings() {
  try {
    recordings = await api('/api/recordings');
  } catch {
    return;
  }
  renderRecordings();
}

function renderRecordings() {
  const filter = ($('recordings-filter').value || '').toLowerCase();
  const rows = recordings.filter(r => !filter || r.name.toLowerCase().includes(filter) || (r.source || '').toLowerCase().includes(filter));
  if (!rows.length) {
    $('recordings-list').innerHTML = `<p class="text-zinc-400">No recordings found${filter ? ' matching that filter' : ''}.</p>`;
    return;
  }
  $('recordings-list').innerHTML = rows.map(r => `
    <div class="flex flex-wrap items-center justify-between gap-2 rounded border border-white/10 px-3 py-2">
      <div class="min-w-0">
        <div class="truncate font-medium">${escapeHtml(r.name)}</div>
        <div class="text-xs text-zinc-400">${escapeHtml(r.source || '')} · ${formatBytes(r.size)} · ${new Date(r.modTime).toLocaleString()}</div>
      </div>
      <a class="btn" href="/media/${encodeMediaPath(r.path)}" target="_blank" rel="noopener">Open</a>
    </div>`).join('');
}
$('recordings-filter').addEventListener('input', renderRecordings);

function sel(v, x) { return (v || '') === x ? 'selected' : ''; }
function encodeMediaPath(p) { return p.split('/').map(encodeURIComponent).join('/'); }
function formatBytes(v) { const u = ['B','KB','MB','GB','TB']; let i = 0; while (v > 1024 && i < u.length - 1) { v /= 1024; i++; } return `${v.toFixed(i ? 1 : 0)} ${u[i]}`; }
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function escapeAttr(s) { return escapeHtml(s).replace(/"/g, '&quot;'); }

refresh();
setInterval(refresh, 5000);
