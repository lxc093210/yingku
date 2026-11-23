package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h1, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h1, "pbkdf2$100000$") {
		t.Fatalf("hash format = %q", h1)
	}
	if !VerifyPassword("correct horse", h1) {
		t.Error("correct password does not verify")
	}
	if VerifyPassword("wrong", h1) {
		t.Error("wrong password verifies")
	}
	// Salting: the same password hashes differently each time.
	h2, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("two hashes of one password are identical (no salt?)")
	}
	// Malformed digests never verify.
	for _, bad := range []string{"", "plain", "pbkdf2$abc$!!$??", "pbkdf2$100000$%%%$%%%"} {
		if VerifyPassword("correct horse", bad) {
			t.Errorf("malformed digest %q verifies", bad)
		}
	}
}

func TestUserCRUDAndTokens(t *testing.T) {
	st := openTestStore(t)

	alice, err := st.CreateUser("alice", "pw-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.ID) != 32 {
		t.Errorf("user id = %q, want 32 hex chars", alice.ID)
	}
	bob, err := st.CreateUser("bob", "pw-b", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("alice", "pw-x", false); err == nil {
		t.Error("duplicate user name accepted")
	}
	if n, _ := st.CountUsers(); n != 2 {
		t.Errorf("CountUsers = %d, want 2", n)
	}
	if n, _ := st.CountAdmins(); n != 1 {
		t.Errorf("CountAdmins = %d, want 1", n)
	}
	if got, _ := st.GetUserByName("bob"); got == nil || got.ID != bob.ID || got.IsAdmin {
		t.Errorf("GetUserByName(bob) = %+v", got)
	}
	if got, _ := st.GetUserByID(alice.ID); got == nil || got.Name != "alice" || !got.IsAdmin {
		t.Errorf("GetUserByID(alice) = %+v", got)
	}
	if got, _ := st.GetUserByName("ghost"); got != nil {
		t.Errorf("GetUserByName(ghost) = %+v, want nil", got)
	}
	if admin, _ := st.FirstAdmin(); admin == nil || admin.ID != alice.ID {
		t.Errorf("FirstAdmin = %+v, want alice", admin)
	}
	users, err := st.ListUsers()
	if err != nil || len(users) != 2 || users[0].Name != "alice" || users[1].Name != "bob" {
		t.Errorf("ListUsers = %+v, err %v", users, err)
	}

	// Tokens: issue → resolve → revoke.
	tok, err := st.IssueToken(bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok, err := st.ResolveToken(tok); err != nil || !ok || uid != bob.ID {
		t.Errorf("ResolveToken = %q, %v, %v", uid, ok, err)
	}
	if _, ok, _ := st.ResolveToken("deadbeef"); ok {
		t.Error("unknown token resolves")
	}
	// The stored form is a hash, never the plaintext.
	var stored string
	if err := st.DB().QueryRow(`SELECT token_hash FROM access_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok || len(stored) != 64 {
		t.Errorf("stored token_hash = %q, want 64-char hash", stored)
	}
	if err := st.RevokeToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ResolveToken(tok); ok {
		t.Error("revoked token still resolves")
	}

	// A password change revokes outstanding tokens.
	tok2, _ := st.IssueToken(bob.ID)
	if err := st.SetUserPassword(bob.ID, "pw-b2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ResolveToken(tok2); ok {
		t.Error("token survives password change")
	}
	if got, _ := st.GetUserByName("bob"); !VerifyPassword("pw-b2", got.PasswordHash) {
		t.Error("new password does not verify after SetUserPassword")
	}

	// Deleting a user removes the account and its tokens.
	tok3, _ := st.IssueToken(bob.ID)
	if err := st.DeleteUser(bob.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetUserByID(bob.ID); got != nil {
		t.Errorf("deleted user still present: %+v", got)
	}
	if _, ok, _ := st.ResolveToken(tok3); ok {
		t.Error("deleted user's token still resolves")
	}
}

func TestLegacyUserDataAdoption(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.DB().Exec(`INSERT INTO libraries(id, name, kind, path) VALUES (1, 'L', 'movie', '/lib')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO items(id, library_id, type, title, path) VALUES (7, 1, 'movie', 'M', '/lib/m.strm')`); err != nil {
		t.Fatal(err)
	}
	// Pre-account state under the legacy id, plus another user's row.
	if err := st.UpsertProgress(7, LegacyUserID, 4200, false, 99); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFavorite(7, LegacyUserID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProgress(7, "someone-else", 111, false, 100); err != nil {
		t.Fatal(err)
	}

	admin, err := st.CreateUser("boss", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateLegacyUserData(admin.ID); err != nil {
		t.Fatal(err)
	}
	ud, err := st.UserDataOf(7, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ud.ProgressTicks != 4200 || !ud.Favorite {
		t.Errorf("adopted user data = %+v, want ticks 4200 + favorite", ud)
	}
	if ud, _ := st.UserDataOf(7, LegacyUserID); ud.ProgressTicks != 0 || ud.Favorite {
		t.Errorf("legacy rows remain: %+v", ud)
	}
	if ud, _ := st.UserDataOf(7, "someone-else"); ud.ProgressTicks != 111 {
		t.Errorf("other user's rows touched: %+v", ud)
	}
	// Idempotent: a second run changes nothing.
	if err := st.MigrateLegacyUserData(admin.ID); err != nil {
		t.Fatal(err)
	}
}

// TestUserIDColumnMigration rebuilds a pre-batch-12 database (where
// user_data.user_id was INTEGER) and checks the column comes out TEXT
// with the legacy value preserved as '1'.
func TestUserIDColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(schema,
		"user_id        TEXT NOT NULL,", "user_id        INTEGER NOT NULL,", 1)
	if oldSchema == schema {
		t.Fatal("schema substitution failed: user_data line not found")
	}
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO libraries(id, name, kind, path) VALUES (1, 'L', 'movie', '/lib')`,
		`INSERT INTO items(id, library_id, type, title, path) VALUES (7, 1, 'movie', 'M', '/lib/m.strm')`,
		`INSERT INTO user_data(item_id, user_id, progress_ticks, played, favorite, last_played_at) VALUES (7, 1, 42, 0, 1, 5)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	typ, err := columnType(st.db, "user_data", "user_id")
	if err != nil {
		t.Fatal(err)
	}
	if typ != "TEXT" {
		t.Errorf("user_id column type = %q, want TEXT", typ)
	}
	var uid, typeof string
	if err := st.db.QueryRow(`SELECT user_id, typeof(user_id) FROM user_data WHERE item_id = 7`).Scan(&uid, &typeof); err != nil {
		t.Fatal(err)
	}
	if uid != "1" || typeof != "text" {
		t.Errorf("migrated row = (%q, %q), want ('1', 'text')", uid, typeof)
	}
	if ud, err := st.UserDataOf(7, LegacyUserID); err != nil || ud.ProgressTicks != 42 || !ud.Favorite {
		t.Errorf("UserDataOf after migration = %+v, %v", ud, err)
	}
}
