package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newAuthTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{
		usersFile:  filepath.Join(dir, "users.json"),
		sessions:   map[string]sessionInfo{},
		oauthState: map[string]pendingOAuth{},
		startedAt:  time.Now(),
	}
}

func TestRBACAllowed(t *testing.T) {
	cases := []struct {
		method, path string
		role         Role
		want         bool
	}{
		{http.MethodGet, "/api/state", RoleViewer, true},
		{http.MethodGet, "/api/config", RoleViewer, true},
		{http.MethodGet, "/api/users", RoleViewer, false},
		{http.MethodGet, "/api/users/abc123", RoleViewer, false},
		{http.MethodGet, "/api/users", RoleAdmin, true},
		{http.MethodPost, "/api/sources", RoleViewer, false},
		{http.MethodPut, "/api/config", RoleViewer, false},
		{http.MethodDelete, "/api/sources/abc123", RoleViewer, false},
		{http.MethodPost, "/api/account", RoleViewer, true},
		{http.MethodPost, "/api/logout", RoleViewer, true},
		{http.MethodPost, "/api/auth/discord/unlink", RoleViewer, true},
		{http.MethodGet, "/api/auth/discord/link/start", RoleViewer, true},
		{http.MethodPost, "/api/sources", RoleAdmin, true},
		{http.MethodPut, "/api/config", RoleAdmin, true},
	}
	for _, c := range cases {
		got := rbacAllowed(c.method, c.path, c.role)
		if got != c.want {
			t.Errorf("rbacAllowed(%s, %q, %s) = %v, want %v", c.method, c.path, c.role, got, c.want)
		}
	}
}

func TestUserCRUD(t *testing.T) {
	a := newAuthTestApp(t)

	if err := a.addUser(User{ID: "u1", Username: "Alice", Role: RoleAdmin}); err != nil {
		t.Fatalf("addUser: %v", err)
	}
	if err := a.addUser(User{ID: "u2", Username: "alice", Role: RoleViewer}); err == nil {
		t.Fatal("expected a case-insensitive duplicate username to be rejected")
	}
	if err := a.addUser(User{ID: "u2", Username: "Bob", Role: RoleViewer}); err != nil {
		t.Fatalf("addUser: %v", err)
	}

	if u, ok := a.findUserByUsername("ALICE"); !ok || u.ID != "u1" {
		t.Fatalf("expected case-insensitive username lookup to find u1, got %+v, %v", u, ok)
	}
	if _, ok := a.findUserByUsername("nobody"); ok {
		t.Fatal("expected lookup of a nonexistent username to fail")
	}

	if n := a.countAdmins(); n != 1 {
		t.Fatalf("expected 1 admin, got %d", n)
	}

	updated, err := a.updateUser("u2", func(u *User) error { u.Role = RoleAdmin; return nil })
	if err != nil || updated.Role != RoleAdmin {
		t.Fatalf("updateUser: %v, %+v", err, updated)
	}
	if n := a.countAdmins(); n != 2 {
		t.Fatalf("expected 2 admins after promotion, got %d", n)
	}

	if err := a.deleteUser("u1"); err != nil {
		t.Fatalf("deleteUser: %v", err)
	}
	if _, ok := a.findUserByID("u1"); ok {
		t.Fatal("expected u1 to be gone after deleteUser")
	}
	if n := a.countAdmins(); n != 1 {
		t.Fatalf("expected 1 admin remaining, got %d", n)
	}

	// Persistence: a fresh App reading the same usersFile should see the
	// current state (this is what setupAuth relies on across restarts).
	store, err := loadUserStore(a.usersFile)
	if err != nil {
		t.Fatalf("loadUserStore: %v", err)
	}
	if len(store.Users) != 1 || store.Users[0].ID != "u2" {
		t.Fatalf("expected persisted store to contain only u2, got %+v", store.Users)
	}
}

func TestCheckCredentialsEnvPinnedAdmin(t *testing.T) {
	a := newAuthTestApp(t)
	a.authUser = "admin"
	a.authPass = "correct-horse"

	if _, ok := a.checkCredentials("admin", "wrong"); ok {
		t.Fatal("expected wrong password to fail")
	}
	u, ok := a.checkCredentials("admin", "correct-horse")
	if !ok {
		t.Fatal("expected env-pinned credentials to succeed")
	}
	if u.ID != envUserID || u.Role != RoleAdmin {
		t.Fatalf("expected the env-pinned virtual admin user, got %+v", u)
	}

	// A username that doesn't match the pinned admin falls through to
	// users.json, not an automatic failure.
	hash, _ := hashPasswordForTest("viewer-pass")
	_ = a.addUser(User{ID: "u1", Username: "viewer", PasswordHash: hash, Role: RoleViewer})
	u2, ok := a.checkCredentials("viewer", "viewer-pass")
	if !ok || u2.Role != RoleViewer {
		t.Fatalf("expected the real user to authenticate, got %+v, %v", u2, ok)
	}
}

func TestSessionLifecycle(t *testing.T) {
	a := newAuthTestApp(t)
	hash, _ := hashPasswordForTest("password123")
	_ = a.addUser(User{ID: "u1", Username: "alice", PasswordHash: hash, Role: RoleAdmin})

	rec := httptest.NewRecorder()
	a.createSession(rec, "u1")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one cookie set, got %d", len(cookies))
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	u, ok := a.validSession(req)
	if !ok || u.ID != "u1" {
		t.Fatalf("expected a valid session resolving to u1, got %+v, %v", u, ok)
	}

	// Deleting the underlying user must invalidate any of their sessions.
	_ = a.deleteUser("u1")
	if _, ok := a.validSession(req); ok {
		t.Fatal("expected the session to be invalid once its user is deleted")
	}
}

func TestRedactSecrets(t *testing.T) {
	cfg := AppConfig{}
	cfg.Settings.Notifications.SMTP.Password = "smtp-secret"
	cfg.Settings.Notifications.DiscordWebhook = "https://discord.com/webhook/x"
	cfg.Settings.DiscordOAuth.ClientSecret = "oauth-secret"
	cfg.Settings.Backup.RcloneArgs = []string{"--sftp-pass", "hunter2"}

	redactSecrets(&cfg)

	if cfg.Settings.Notifications.SMTP.Password != "" {
		t.Error("expected SMTP password to be redacted")
	}
	if cfg.Settings.Notifications.DiscordWebhook != "" {
		t.Error("expected Discord webhook to be redacted")
	}
	if cfg.Settings.DiscordOAuth.ClientSecret != "" {
		t.Error("expected Discord OAuth client secret to be redacted")
	}
	if cfg.Settings.Backup.RcloneArgs != nil {
		t.Error("expected rclone args to be redacted")
	}
}

func TestDiscordConfigured(t *testing.T) {
	a := newAuthTestApp(t)
	cfg := AppConfig{}
	if a.discordConfigured(cfg) {
		t.Error("expected an empty config to be unconfigured")
	}
	cfg.Settings.DiscordOAuth = DiscordOAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "secret", RedirectURL: "https://example.com/api/auth/discord/callback"}
	if !a.discordConfigured(cfg) {
		t.Error("expected a fully filled-in, enabled config to be configured")
	}
	cfg.Settings.DiscordOAuth.Enabled = false
	if a.discordConfigured(cfg) {
		t.Error("expected Enabled=false to count as unconfigured even with credentials present")
	}
}

func TestDiscordAuthorizeURLIncludesState(t *testing.T) {
	cfg := DiscordOAuthConfig{ClientID: "abc", RedirectURL: "https://example.com/api/auth/discord/callback"}
	got := discordAuthorizeURL(cfg, "the-state-token")
	if !strings.Contains(got, "client_id=abc") || !strings.Contains(got, "state=the-state-token") || !strings.Contains(got, "response_type=code") {
		t.Errorf("discordAuthorizeURL missing expected params: %s", got)
	}
}

func TestPendingOAuthStoreConsumeOnce(t *testing.T) {
	a := newAuthTestApp(t)
	a.storePendingOAuth("state1", pendingOAuth{intent: "login", expiry: time.Now().Add(time.Minute)})

	p, ok := a.consumePendingOAuth("state1")
	if !ok || p.intent != "login" {
		t.Fatalf("expected to consume the pending state, got %+v, %v", p, ok)
	}
	if _, ok := a.consumePendingOAuth("state1"); ok {
		t.Fatal("expected a state token to be usable only once")
	}

	a.storePendingOAuth("state2", pendingOAuth{intent: "link", userID: "u1", expiry: time.Now().Add(-time.Minute)})
	if _, ok := a.consumePendingOAuth("state2"); ok {
		t.Fatal("expected an already-expired state token to be rejected")
	}
}

// hashPasswordForTest is a small helper for setting up a PasswordHash
// fixture without repeating bcrypt's generate call in every test.
func hashPasswordForTest(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}
