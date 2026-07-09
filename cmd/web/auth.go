package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Role is a user's permission level. Admins can do anything; viewers can
// watch live, browse recordings, and manage their own account (including
// linking Discord), but can't touch sources, settings, users, or anything
// else that changes how/what this instance records.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// envUserID identifies the virtual admin account backed by
// AUTH_USERNAME/AUTH_PASSWORD - it never appears in users.json and can't be
// edited or deleted via the Users tab, only via those environment variables,
// same as the single-user version of this app worked before.
const envUserID = "env"

// User is one login. PasswordHash is bcrypt and is never serialized to the
// client directly - see Public().
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash,omitempty"`
	Role         Role      `json:"role"`
	DiscordID    string    `json:"discordId,omitempty"`
	DiscordName  string    `json:"discordName,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// PublicUser is what the Users tab actually sees - no password hash, ever.
type PublicUser struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Role        Role      `json:"role"`
	DiscordName string    `json:"discordName,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (u User) Public() PublicUser {
	return PublicUser{ID: u.ID, Username: u.Username, Role: u.Role, DiscordName: u.DiscordName, CreatedAt: u.CreatedAt}
}

// userStore is the on-disk shape of users.json.
type userStore struct {
	Users []User `json:"users"`
}

// sessionInfo is what a session cookie's token maps to: which user it
// belongs to (envUserID for the env-pinned virtual admin) and when it
// expires.
type sessionInfo struct {
	UserID string
	Expiry time.Time
}

// pendingOAuth tracks one in-flight Discord OAuth round trip, keyed by a
// random CSRF state token so the callback can be trusted: which flow it's
// for ("login" or "link"), and - for a "link" flow - which already-logged-in
// user initiated it (a "login" flow has no user yet, that's the point).
type pendingOAuth struct {
	intent string
	userID string
	expiry time.Time
}

const sessionCookieName = "dqr_session"
const sessionTTL = 30 * 24 * time.Hour
const oauthStateTTL = 5 * time.Minute

// storedCredentials is the legacy (pre-multi-user) on-disk shape of
// auth.json - kept only so setupAuth can migrate an existing single-user
// install into the first admin user in users.json.
type storedCredentials struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// setupAuth decides how this instance authenticates, in priority order:
//  1. AUTH_USERNAME/AUTH_PASSWORD from the environment, for Docker-style
//     deployments that prefer to manage one pinned admin login externally -
//     this is on top of, not instead of, whatever's in users.json.
//  2. Users previously saved to users.json by the setup wizard or the Users
//     tab.
//  3. A legacy single-user auth.json from before multi-user support -
//     migrated once into the first admin user in users.json.
//  4. None of the above: needsSetup is set, and the web UI serves a
//     one-time setup wizard instead of ever generating or printing a
//     throwaway password.
func (a *App) setupAuth() {
	a.usersFile = filepath.Join(filepath.Dir(a.config), "users.json")
	a.authUser = os.Getenv("AUTH_USERNAME")
	a.authPass = os.Getenv("AUTH_PASSWORD")
	if a.authPass != "" {
		if a.authUser == "" {
			a.authUser = "admin"
		}
		log.Printf("Using AUTH_USERNAME/AUTH_PASSWORD from the environment as a pinned admin login.")
	} else {
		a.authUser = ""
	}

	if store, err := loadUserStore(a.usersFile); err == nil {
		a.usersMu.Lock()
		a.users = store.Users
		a.usersMu.Unlock()
		log.Printf("Loaded %d user(s) from %s.", len(store.Users), a.usersFile)
		return
	}

	legacyPath := filepath.Join(filepath.Dir(a.config), "auth.json")
	if creds, err := loadStoredCredentials(legacyPath); err == nil {
		migrated := User{ID: newID(), Username: creds.Username, PasswordHash: creds.PasswordHash, Role: RoleAdmin, CreatedAt: time.Now()}
		a.usersMu.Lock()
		a.users = []User{migrated}
		err := a.saveUsersLocked()
		a.usersMu.Unlock()
		if err != nil {
			log.Printf("Failed to migrate %s to %s: %s", legacyPath, a.usersFile, err)
			return
		}
		log.Printf("Migrated existing credentials from %s into %s as an admin user (%q).", legacyPath, a.usersFile, creds.Username)
		return
	}

	if a.authPass != "" {
		return // env-pinned admin is enough to sign in; users.json can stay empty
	}
	a.usersMu.Lock()
	a.needsSetup = true
	a.usersMu.Unlock()
	log.Printf("No credentials configured yet - open the web UI to finish setup and choose a username/password.")
}

func loadUserStore(path string) (userStore, error) {
	var store userStore
	data, err := os.ReadFile(path)
	if err != nil {
		return store, err
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return store, err
	}
	return store, nil
}

func loadStoredCredentials(path string) (storedCredentials, error) {
	var creds storedCredentials
	data, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, err
	}
	if creds.Username == "" || creds.PasswordHash == "" {
		return creds, errors.New("incomplete credentials file")
	}
	return creds, nil
}

// saveUsersLocked persists a.users to users.json. Caller must hold usersMu.
func (a *App) saveUsersLocked() error {
	data, err := json.MarshalIndent(userStore{Users: a.users}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.usersFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.usersFile, data, 0o600)
}

func (a *App) isSetupNeeded() bool {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	return a.needsSetup
}

func (a *App) findUserByUsername(username string) (User, bool) {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	for _, u := range a.users {
		if strings.EqualFold(u.Username, username) {
			return u, true
		}
	}
	return User{}, false
}

func (a *App) findUserByID(id string) (User, bool) {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	for _, u := range a.users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

func (a *App) findUserByDiscordID(discordID string) (User, bool) {
	if discordID == "" {
		return User{}, false
	}
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	for _, u := range a.users {
		if u.DiscordID == discordID {
			return u, true
		}
	}
	return User{}, false
}

func (a *App) countAdmins() int {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	n := 0
	for _, u := range a.users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

var errUsernameTaken = errors.New("that username is already taken")

// addUser appends a new user and persists it, rejecting a duplicate
// username (case-insensitive, same as login).
func (a *App) addUser(u User) error {
	a.usersMu.Lock()
	defer a.usersMu.Unlock()
	for _, existing := range a.users {
		if strings.EqualFold(existing.Username, u.Username) {
			return errUsernameTaken
		}
	}
	a.users = append(a.users, u)
	a.needsSetup = false
	return a.saveUsersLocked()
}

// updateUser applies mutate to the user with the given ID and persists the
// result.
func (a *App) updateUser(id string, mutate func(*User) error) (User, error) {
	a.usersMu.Lock()
	defer a.usersMu.Unlock()
	for i := range a.users {
		if a.users[i].ID == id {
			if err := mutate(&a.users[i]); err != nil {
				return User{}, err
			}
			if err := a.saveUsersLocked(); err != nil {
				return User{}, err
			}
			return a.users[i], nil
		}
	}
	return User{}, errors.New("user not found")
}

func (a *App) deleteUser(id string) error {
	a.usersMu.Lock()
	defer a.usersMu.Unlock()
	idx := -1
	for i, u := range a.users {
		if u.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return errors.New("user not found")
	}
	a.users = append(a.users[:idx], a.users[idx+1:]...)
	return a.saveUsersLocked()
}

// envAdminUser synthesizes the virtual admin backed by
// AUTH_USERNAME/AUTH_PASSWORD - it's never stored in users.json.
func (a *App) envAdminUser() User {
	return User{ID: envUserID, Username: a.authUser, Role: RoleAdmin, CreatedAt: a.startedAt}
}

// checkCredentials verifies a login attempt against the env-pinned admin
// first (if configured), then against users.json.
func (a *App) checkCredentials(username, password string) (User, bool) {
	if a.authPass != "" && subtle.ConstantTimeCompare([]byte(username), []byte(a.authUser)) == 1 {
		if subtle.ConstantTimeCompare([]byte(password), []byte(a.authPass)) == 1 {
			return a.envAdminUser(), true
		}
		return User{}, false
	}
	u, ok := a.findUserByUsername(username)
	if !ok || u.PasswordHash == "" {
		return User{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return User{}, false
	}
	return u, true
}

// isPublicPath lists the handful of routes reachable without a session, so
// the login/setup pages (and the requests that submit them) don't get
// redirected back to themselves. Discord's login flow is public too - it
// runs before there's a session to speak of, same as a password login.
func isPublicPath(p string) bool {
	switch p {
	case "/login", "/api/login", "/setup", "/api/setup", "/app.css", "/manifest.json", "/sw.js",
		"/api/auth/discord/status", "/api/auth/discord/login/start", "/api/auth/discord/callback",
		"/api/share/ping", "/api/livecut/host/mark", "/api/livecut/host/feed":
		return true
	}
	// Icons are pure branding assets (no user data) and need to load
	// unauthenticated for the browser's install/add-to-home-screen prompt
	// and for the login/setup pages themselves.
	if strings.HasPrefix(p, "/icons/") {
		return true
	}
	// Peer-to-peer share downloads are authenticated by an unguessable token
	// in the path, not a session - a receiving instance (or its user) has no
	// account here. The handler validates the token and only ever serves
	// files that token's share explicitly lists.
	return strings.HasPrefix(p, "/api/share/get/")
}

// rbacAllowed reports whether a given role may make a given request. Any
// authenticated role can read (GET/HEAD) almost everything - the frontend
// hides admin-only UI for viewers, but the real boundary is here. Mutating
// requests need an admin role, with a short allow-list of self-service
// actions (changing your own password, linking/unlinking your own Discord,
// logging out) any authenticated user can do regardless of role.
func rbacAllowed(method, path string, role Role) bool {
	if role == RoleAdmin {
		return true
	}
	if method == http.MethodGet || method == http.MethodHead {
		// The user list (even without password hashes) is admin-only info,
		// not something every viewer needs to see.
		return path != "/api/users" && !strings.HasPrefix(path, "/api/users/")
	}
	switch path {
	case "/api/account", "/api/logout", "/api/auth/discord/unlink":
		return true
	}
	if strings.HasPrefix(path, "/api/auth/discord/link/") {
		return true
	}
	// Pressing "Mark Transition" in a Live Cut Session is deliberately open to
	// any authenticated role, not just admins - crowdsourcing the button
	// press across everyone watching is the whole point of the feature.
	// Starting/closing/joining/importing a session stays admin-only (each
	// handler checks that itself).
	if strings.HasSuffix(path, "/mark") &&
		(strings.HasPrefix(path, "/api/livecut/sessions/") || strings.HasPrefix(path, "/api/livecut/joined/")) {
		return true
	}
	return false
}

// userContextKey is the request-context key requireAuth stashes the
// authenticated User under, so handlers can tell who's asking without
// re-parsing the session cookie themselves.
type userContextKey struct{}

func userFromContext(r *http.Request) (User, bool) {
	u, ok := r.Context().Value(userContextKey{}).(User)
	return u, ok
}

// roleFromRequest returns the authenticated caller's role, defaulting to the
// most restrictive (viewer) if somehow called outside requireAuth.
func roleFromRequest(r *http.Request) Role {
	if u, ok := userFromContext(r); ok {
		return u.Role
	}
	return RoleViewer
}

// requireAuth gates every request (UI and API) behind a session cookie set
// by a real login page (see handleLogin), redirecting page loads to /setup
// or /login as appropriate, returning error JSON for API calls when the
// session is missing/expired/insufficient, and stashing the resolved User on
// the request context for handlers and rbacAllowed's role check.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if a.isSetupNeeded() {
			// The setup wizard's system check needs to run - and be
			// re-run - before any credentials exist, so it can't wait
			// behind a session the user has no way to create yet.
			if r.URL.Path == "/api/system-check" {
				next.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "setup required", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		user, ok := a.validSession(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !rbacAllowed(r.Method, r.URL.Path, user.Role) {
			http.Error(w, "this action requires an admin account", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	})
}

// createSession mints a new random session token for the given user (or
// envUserID for the env-pinned admin), records its expiry, and sets it as an
// HttpOnly cookie on the response.
func (a *App) createSession(w http.ResponseWriter, userID string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	expiry := time.Now().Add(sessionTTL)
	a.sessMu.Lock()
	a.sessions[token] = sessionInfo{UserID: userID, Expiry: expiry}
	a.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// validSession resolves the request's session cookie to the User it belongs
// to. A session whose user has since been deleted (or a Discord flow that
// somehow leaves a dangling session) is treated as invalid rather than
// panicking on a stale reference.
func (a *App) validSession(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	a.sessMu.Lock()
	info, ok := a.sessions[c.Value]
	if ok && time.Now().After(info.Expiry) {
		delete(a.sessions, c.Value)
		ok = false
	}
	a.sessMu.Unlock()
	if !ok {
		return User{}, false
	}
	if info.UserID == envUserID {
		return a.envAdminUser(), true
	}
	u, found := a.findUserByID(info.UserID)
	if !found {
		a.sessMu.Lock()
		delete(a.sessions, c.Value)
		a.sessMu.Unlock()
		return User{}, false
	}
	return u, true
}

func (a *App) destroySession(r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return
	}
	a.sessMu.Lock()
	delete(a.sessions, c.Value)
	a.sessMu.Unlock()
}

// handleLoginPage serves the standalone login page, unauthenticated. Visitors
// who haven't chosen credentials yet are sent to the setup wizard instead.
func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.isSetupNeeded() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	serveStaticPage(w, r, "static/login.html")
}

// handleLogin verifies a login attempt (env-pinned admin or a users.json
// entry) and, on success, starts a session so the rest of the app is
// reachable without re-authenticating on every request.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.isSetupNeeded() {
		http.Error(w, "setup required", http.StatusConflict)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	user, ok := a.checkCredentials(req.Username, req.Password)
	if !ok {
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	a.createSession(w, user.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.destroySession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleSetupPage serves the first-run setup wizard. Once credentials exist
// it redirects to /login instead, so the wizard can't be replayed to hijack
// an already-configured instance.
func (a *App) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isSetupNeeded() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	serveStaticPage(w, r, "static/setup.html")
}

// handleSetup completes first-run setup by creating the first user - always
// an admin - then immediately starts a session. Only reachable once, before
// any users exist; afterwards use the Users tab or Account settings.
func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isSetupNeeded() {
		http.Error(w, "setup has already been completed", http.StatusConflict)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 8 {
		http.Error(w, "a username and a password of at least 8 characters are required", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := User{ID: newID(), Username: req.Username, PasswordHash: string(hash), Role: RoleAdmin, CreatedAt: time.Now()}
	if err := a.addUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.event("info", fmt.Sprintf("Initial setup completed for user %q", req.Username))
	a.createSession(w, user.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleAccount lets the signed-in user view or change their own
// username/password and see/manage their Discord link, from the Settings
// tab's Account section - available to every role, since it's entirely
// self-service. The env-pinned admin account is read-only here (change the
// environment variables and restart instead), same as before multi-user
// support existed.
func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	managedByEnv := user.ID == envUserID
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"username":      user.Username,
			"role":          user.Role,
			"managedByEnv":  managedByEnv,
			"discordLinked": user.DiscordID != "",
			"discordName":   user.DiscordName,
		})
	case http.MethodPost:
		if managedByEnv {
			http.Error(w, "credentials for this account are set via AUTH_USERNAME/AUTH_PASSWORD and can't be changed here", http.StatusConflict)
			return
		}
		var req struct {
			CurrentPassword string `json:"currentPassword"`
			Username        string `json:"username"`
			Password        string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || len(req.Password) < 8 {
			http.Error(w, "a username and a password of at least 8 characters are required", http.StatusBadRequest)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.CurrentPassword)) != nil {
			http.Error(w, "current password is incorrect", http.StatusUnauthorized)
			return
		}
		if !strings.EqualFold(req.Username, user.Username) {
			if _, taken := a.findUserByUsername(req.Username); taken {
				http.Error(w, errUsernameTaken.Error(), http.StatusConflict)
				return
			}
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := a.updateUser(user.ID, func(u *User) error {
			u.Username = req.Username
			u.PasswordHash = string(hash)
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.event("info", fmt.Sprintf("Credentials updated for user %q", req.Username))
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUsers lists all users or creates a new one - admin-only (enforced by
// rbacAllowed; GET is blocked for viewers there, POST for anyone non-admin).
func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.usersMu.RLock()
		list := make([]PublicUser, 0, len(a.users))
		for _, u := range a.users {
			list = append(list, u.Public())
		}
		a.usersMu.RUnlock()
		writeJSON(w, list)
	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     Role   `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || len(req.Password) < 8 {
			http.Error(w, "a username and a password of at least 8 characters are required", http.StatusBadRequest)
			return
		}
		if req.Role != RoleAdmin {
			req.Role = RoleViewer
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		user := User{ID: newID(), Username: req.Username, PasswordHash: string(hash), Role: req.Role, CreatedAt: time.Now()}
		if err := a.addUser(user); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errUsernameTaken) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		a.event("info", fmt.Sprintf("User %q created (role %s)", user.Username, user.Role))
		writeJSON(w, user.Public())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUserItem updates a user's role/password or deletes them - admin-only.
// Refuses to demote or delete the last remaining admin, so an admin can't
// accidentally lock everyone out of managing the instance.
func (a *App) handleUserItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/users/")
	if id == "" || id == envUserID {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		target, found := a.findUserByID(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Role     Role   `json:"role,omitempty"`
			Password string `json:"password,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Role != "" && req.Role != RoleAdmin && req.Role != RoleViewer {
			http.Error(w, "role must be admin or viewer", http.StatusBadRequest)
			return
		}
		if req.Role != "" && req.Role != RoleAdmin && target.Role == RoleAdmin && a.countAdmins() <= 1 {
			http.Error(w, "cannot demote the last admin account", http.StatusConflict)
			return
		}
		var newHash []byte
		if req.Password != "" {
			if len(req.Password) < 8 {
				http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
				return
			}
			h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			newHash = h
		}
		updated, err := a.updateUser(id, func(u *User) error {
			if req.Role != "" {
				u.Role = req.Role
			}
			if newHash != nil {
				u.PasswordHash = string(newHash)
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.event("info", fmt.Sprintf("User %q updated by an admin", updated.Username))
		writeJSON(w, updated.Public())
	case http.MethodDelete:
		target, found := a.findUserByID(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		if target.Role == RoleAdmin && a.countAdmins() <= 1 {
			http.Error(w, "cannot delete the last admin account", http.StatusConflict)
			return
		}
		if err := a.deleteUser(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.event("info", fmt.Sprintf("User %q deleted", target.Username))
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// redactSecrets blanks out fields a viewer has no business seeing when
// /api/state or /api/config (GET) is served to a non-admin role: SMTP
// credentials, the Discord notification webhook (lets whoever has it post as
// this instance), the Discord OAuth client secret, and any rclone args
// (which can carry embedded remote credentials).
func redactSecrets(cfg *AppConfig) {
	cfg.Settings.Notifications.SMTP.Password = ""
	cfg.Settings.Notifications.DiscordWebhook = ""
	cfg.Settings.DiscordOAuth.ClientSecret = ""
	cfg.Settings.Backup.RcloneArgs = nil
	cfg.Settings.YouTube.ClientSecret = ""
	cfg.Settings.YouTube.RefreshToken = ""
	// A configured proxy URL can carry embedded credentials (user:pass@host).
	cfg.Settings.Sharing.ProxyURL = ""
	// Share tokens are bearer credentials - never expose them (or the list of
	// what's being shared) to a non-admin.
	cfg.Shares = nil
}

// --- Discord OAuth ---
//
// Discord login is link-only: authorizing with Discord can never create a
// new account by itself. A user must already exist (created by an admin,
// via the Users tab or first-run setup) and, while logged in normally, link
// their Discord account from Settings -> Account. After that, the login
// page's "Log in with Discord" button works for that account too. This
// keeps access control anchored to "accounts an admin created," not "anyone
// with a Discord account."

func (a *App) discordConfigured(cfg AppConfig) bool {
	d := cfg.Settings.DiscordOAuth
	return d.Enabled && d.ClientID != "" && d.ClientSecret != "" && d.RedirectURL != ""
}

func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func discordAuthorizeURL(cfg DiscordOAuthConfig, state string) string {
	v := url.Values{}
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect_uri", cfg.RedirectURL)
	v.Set("response_type", "code")
	v.Set("scope", "identify")
	v.Set("state", state)
	v.Set("prompt", "consent")
	return "https://discord.com/api/oauth2/authorize?" + v.Encode()
}

// discordUser is the subset of Discord's "get current user" response
// (https://discord.com/api/users/@me) this app cares about.
type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

func (d discordUser) displayName() string {
	if d.GlobalName != "" {
		return d.GlobalName
	}
	return d.Username
}

// exchangeDiscordCode trades an OAuth2 authorization code for the Discord
// account that authorized it: a token exchange followed by a profile fetch,
// both plain REST calls against Discord's documented API.
func exchangeDiscordCode(ctx context.Context, cfg DiscordOAuthConfig, code string) (discordUser, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURL)

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://discord.com/api/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return discordUser{}, err
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		return discordUser{}, err
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return discordUser{}, fmt.Errorf("token exchange failed: %s: %s", tokenResp.Status, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return discordUser{}, err
	}

	profileReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://discord.com/api/users/@me", nil)
	if err != nil {
		return discordUser{}, err
	}
	profileReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	profileResp, err := http.DefaultClient.Do(profileReq)
	if err != nil {
		return discordUser{}, err
	}
	defer profileResp.Body.Close()
	if profileResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(profileResp.Body)
		return discordUser{}, fmt.Errorf("profile fetch failed: %s: %s", profileResp.Status, strings.TrimSpace(string(body)))
	}
	var du discordUser
	if err := json.NewDecoder(profileResp.Body).Decode(&du); err != nil {
		return discordUser{}, err
	}
	return du, nil
}

// storePendingOAuth records one in-flight OAuth round trip, sweeping expired
// entries opportunistically so the map doesn't grow unbounded if callbacks
// never come back.
func (a *App) storePendingOAuth(state string, p pendingOAuth) {
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	now := time.Now()
	for k, v := range a.oauthState {
		if now.After(v.expiry) {
			delete(a.oauthState, k)
		}
	}
	a.oauthState[state] = p
}

// consumePendingOAuth looks up and deletes a state token in one step, so a
// callback can never be replayed with the same state twice.
func (a *App) consumePendingOAuth(state string) (pendingOAuth, bool) {
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	p, ok := a.oauthState[state]
	delete(a.oauthState, state)
	if !ok || time.Now().After(p.expiry) {
		return pendingOAuth{}, false
	}
	return p, true
}

// handleDiscordStatus is a small public endpoint so the login page knows
// whether to show a "Log in with Discord" button at all.
func (a *App) handleDiscordStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]bool{"enabled": a.discordConfigured(a.snapshotConfig())})
}

func (a *App) handleDiscordLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := a.snapshotConfig()
	if !a.discordConfigured(cfg) {
		http.Error(w, "Discord login isn't configured", http.StatusNotFound)
		return
	}
	state := randomToken()
	a.storePendingOAuth(state, pendingOAuth{intent: "login", expiry: time.Now().Add(oauthStateTTL)})
	http.Redirect(w, r, discordAuthorizeURL(cfg.Settings.DiscordOAuth, state), http.StatusFound)
}

// handleDiscordLinkStart begins linking Discord to the *currently logged in*
// user - any role can link their own account.
func (a *App) handleDiscordLinkStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := userFromContext(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	cfg := a.snapshotConfig()
	if !a.discordConfigured(cfg) {
		http.Error(w, "Discord login isn't configured", http.StatusNotFound)
		return
	}
	state := randomToken()
	a.storePendingOAuth(state, pendingOAuth{intent: "link", userID: user.ID, expiry: time.Now().Add(oauthStateTTL)})
	http.Redirect(w, r, discordAuthorizeURL(cfg.Settings.DiscordOAuth, state), http.StatusFound)
}

// handleDiscordCallback is the single OAuth2 redirect target for *both* the
// login and link flows - Discord only allows redirecting to an exact,
// pre-registered URL, so both flows have to share one, and this dispatches
// on the pending state's intent (set by handleDiscordLoginStart/
// handleDiscordLinkStart) to decide which one actually happened. A login
// flow only ever signs in an *existing* user already linked to that Discord
// account (see the package doc comment above) - it never creates one.
func (a *App) handleDiscordCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := a.snapshotConfig()
	q := r.URL.Query()
	pending, ok := a.consumePendingOAuth(q.Get("state"))
	// Without a valid pending state we don't even know which flow this was,
	// so there's nowhere better to send the error than the login page.
	if !ok {
		http.Redirect(w, r, "/login?discordError=invalid_state", http.StatusFound)
		return
	}
	fail := func(code string) {
		if pending.intent == "link" {
			http.Redirect(w, r, "/?discordError="+code, http.StatusFound)
		} else {
			http.Redirect(w, r, "/login?discordError="+code, http.StatusFound)
		}
	}
	if !a.discordConfigured(cfg) {
		fail("not_configured")
		return
	}
	if q.Get("error") != "" {
		fail("denied")
		return
	}
	du, err := exchangeDiscordCode(r.Context(), cfg.Settings.DiscordOAuth, q.Get("code"))
	if err != nil {
		log.Printf("discord oauth exchange failed: %s", err)
		fail("exchange_failed")
		return
	}

	if pending.intent == "link" {
		if existing, found := a.findUserByDiscordID(du.ID); found && existing.ID != pending.userID {
			fail("already_linked")
			return
		}
		if _, err := a.updateUser(pending.userID, func(u *User) error {
			u.DiscordID = du.ID
			u.DiscordName = du.displayName()
			return nil
		}); err != nil {
			fail("link_failed")
			return
		}
		a.event("info", "A Discord account was linked to a user")
		http.Redirect(w, r, "/?discordLinked=1", http.StatusFound)
		return
	}

	user, found := a.findUserByDiscordID(du.ID)
	if !found {
		fail("not_linked")
		return
	}
	a.createSession(w, user.ID)
	a.event("info", fmt.Sprintf("User %q logged in via Discord", user.Username))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) handleDiscordUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := userFromContext(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if user.ID == envUserID {
		http.Error(w, "this account is managed by environment variables", http.StatusConflict)
		return
	}
	if _, err := a.updateUser(user.ID, func(u *User) error {
		u.DiscordID = ""
		u.DiscordName = ""
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
