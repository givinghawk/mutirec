# Roadmap

- First-run setup wizard now covers credentials (no env vars required); still missing guided source presets, test-record buttons, and storage checks.
- Add per-user accounts and role-based access (current auth is a single shared login).
- Consider server-side HLS restreaming for sources whose CDN blocks cross-origin playback (non-recording live playback still uses a direct redirect + hls.js).
- Add backup queue history with retry controls.
- Add Prometheus metrics and healthcheck endpoint.
