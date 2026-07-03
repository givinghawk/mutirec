let state = null;
let config = null;
let wave = null;
const accents = { red: '#ef4444', cyan: '#06b6d4', lime: '#84cc16', amber: '#f59e0b', pink: '#ec4899' };

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

function $(id) { return document.getElementById(id); }

async function refresh() {
  state = await api('/api/state');
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
        <span class="pill">${escapeHtml(src.status)}</span>
      </div>
      <div class="mt-3 text-sm text-zinc-300">
        <div>Now: ${escapeHtml(src.currentSet || 'No current set')}</div>
        <div>Next: ${escapeHtml(src.nextSet || 'No upcoming set')}</div>
        <div>Size: ${formatBytes(src.size || 0)}</div>
      </div>
      <div class="mt-3 flex flex-wrap gap-2">
        <button class="btn" onclick="start('${src.id}')">Record</button>
        <button class="btn" onclick="stopRec('${src.id}')">Stop</button>
        <button class="btn" onclick="playLive('${src.id}', ${src.audioOnly ? 'true' : 'false'})">Live</button>
        ${src.outputPath ? `<a class="btn" href="/media/${mediaRel(src.outputPath)}">Open</a>` : ''}
      </div>
    </article>`).join('');
  const free = state.disk.volumeFree || state.disk.VolumeFree || 0;
  const total = state.disk.volumeTotal || state.disk.VolumeTotal || 0;
  $('storage').innerHTML = `<div>Free: ${formatBytes(free)}</div><div>Total: ${formatBytes(total)}</div><div>Recorded: ${formatBytes(state.disk.total || state.disk.Total || 0)}</div>`;
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
    transcode: el.querySelector('.src-transcode').checked
  }));
}

function fillSettings() {
  const s = config.settings, ui = config.ui;
  ['finishedDir','tempDir','logDir','checkIntervalSeconds','minFreeBytes','warnFreeBytes'].forEach(k => $(k).value = s[k]);
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
  ['checkIntervalSeconds','minFreeBytes','warnFreeBytes'].forEach(k => s[k] = Number($(k).value));
  ['enableNfo','enableWaveform','allowLiveProxy'].forEach(k => s[k] = $(k).checked);
  config.ui = { appName: $('uiAppName').value, logoUrl: $('uiLogoUrl').value, theme: $('uiTheme').value, accent: $('uiAccent').value, customCss: $('uiCustomCss').value };
  s.notifications.discordWebhook = $('discordWebhook').value;
  s.notifications.smtp = { enabled: $('smtpEnabled').checked, host: $('smtpHost').value, port: Number($('smtpPort').value), username: $('smtpUsername').value, password: $('smtpPassword').value, from: $('smtpFrom').value, to: $('smtpTo').value };
  s.backup = { enabled: $('backupEnabled').checked, afterComplete: $('backupAfterComplete').checked, rcloneRemote: $('rcloneRemote').value, rcloneArgs: $('rcloneArgs').value.split('\n').map(x => x.trim()).filter(Boolean) };
}

async function saveConfig() {
  await api('/api/config', { method: 'PUT', body: JSON.stringify(config), headers: { 'Content-Type': 'application/json' } });
  $('source-editor').dataset.loaded = '';
  await refresh();
}

async function start(id) { await api(`/api/record/${id}`, { method: 'POST' }); await refresh(); }
async function stopRec(id) { await api(`/api/record/${id}`, { method: 'DELETE' }); await refresh(); }
function playLive(id, audioOnly) {
  const url = `/api/live/${id}`;
  const el = audioOnly ? $('audio') : $('video');
  $('video').classList.toggle('hidden', audioOnly);
  el.src = url;
  el.play();
  if ($('wave-toggle').checked && window.WaveSurfer) {
    $('wave').classList.remove('hidden');
    if (wave) wave.destroy();
    wave = WaveSurfer.create({ container: '#wave', waveColor: '#52525b', progressColor: accents[config.ui.accent] || accents.red, height: 80 });
    wave.load(url);
  }
}

document.querySelectorAll('.nav').forEach(b => b.onclick = () => {
  document.querySelectorAll('.nav').forEach(x => x.classList.remove('active'));
  document.querySelectorAll('.view').forEach(x => x.classList.add('hidden'));
  b.classList.add('active');
  $(b.dataset.view).classList.remove('hidden');
});
$('add-source').onclick = () => { config.sources.push({ id: crypto.randomUUID(), name: 'New Source', type: 'youtube', url: '', enabled: true, record: false, quality: 'best', container: 'mkv' }); drawSourceEditor(); };
$('save-sources').onclick = async () => { config.sources = readSources(); await saveConfig(); };
$('save-timetable').onclick = async () => { config.timetable = JSON.parse($('timetable-json').value); await saveConfig(); };
$('save-settings').onclick = async () => { readSettings(); await saveConfig(); };

function sel(v, x) { return (v || '') === x ? 'selected' : ''; }
function mediaRel(p) { const parts = p.split(/[\\/](recordings)[\\/]/); return parts.length > 1 ? encodeURI(parts[2].replaceAll('\\','/')) : encodeURI(p.split(/[\\/]/).slice(-2).join('/')); }
function formatBytes(v) { const u = ['B','KB','MB','GB','TB']; let i = 0; while (v > 1024 && i < u.length - 1) { v /= 1024; i++; } return `${v.toFixed(i ? 1 : 0)} ${u[i]}`; }
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function escapeAttr(s) { return escapeHtml(s).replace(/"/g, '&quot;'); }

refresh();
setInterval(refresh, 5000);
