package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- users & access tokens (batch 12) ----
//
// Real accounts replace the single implicit user. Watch state
// (user_data) is keyed by the TEXT users.id, so every user's progress,
// favorites and played flags are separate. Passwords are stored as
// PBKDF2-HMAC-SHA256 digests; access tokens are stored only as their
// SHA-256 hex.

// LegacyUserID is the user_data.user_id value written by every release
// before accounts existed (the old defaultUserID = 1, as text). Rows
// still carrying it are adopted by the first administrator when one is
// created (see MigrateLegacyUserData).
const LegacyUserID = "1"

// User is one login-able account.
type User struct {
	ID           string
	Name         string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    int64
}

// newRandomHex returns n random bytes hex-encoded (2n chars), the same
// shape as the server id / API key generators in the api package.
func newRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---- password hashing: PBKDF2-HMAC-SHA256, stdlib only ----

const (
	pbkdf2Iterations = 100_000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
)

// pbkdf2SHA256 is PBKDF2 (RFC 2898) with HMAC-SHA256 as the PRF,
// written on crypto/hmac + crypto/sha256 only.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var dk []byte
	var blockBuf [4]byte
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blockBuf[:], uint32(block))
		prf.Write(blockBuf[:])
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// HashPassword digests a plaintext password for storage, in the form
// pbkdf2$<iterations>$<salt base64>$<hash base64>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyBytes)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword reports whether password matches a stored digest
// produced by HashPassword. Malformed digests never match.
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- user CRUD ----

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var isAdmin int
	err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin != 0
	return &u, nil
}

const userCols = `id, name, password_hash, is_admin, created_at`

// CreateUser inserts a new account with a freshly generated id and a
// hashed password. A duplicate name fails on the UNIQUE constraint.
func (s *Store) CreateUser(name, password string, isAdmin bool) (*User, error) {
	id, err := newRandomHex(16)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &User{ID: id, Name: name, PasswordHash: hash, IsAdmin: isAdmin, CreatedAt: time.Now().Unix()}
	if _, err := s.db.Exec(`INSERT INTO users(id, name, password_hash, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?)`, u.ID, u.Name, u.PasswordHash, boolInt(isAdmin), u.CreatedAt); err != nil {
		return nil, err
	}
	return u, nil
}

// GetUserByName returns the named user, or nil when absent.
func (s *Store) GetUserByName(name string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE name = ?`, name))
}

// GetUserByID returns the user with the given id, or nil when absent.
func (s *Store) GetUserByID(id string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// ListUsers returns all accounts, oldest first (rowid order is the
// true insertion order — created_at has only second resolution and
// same-second creations are the norm in tests and bulk imports).
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns how many accounts exist (0 = bootstrap needed).
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns how many administrator accounts exist.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin <> 0`).Scan(&n)
	return n, err
}

// FirstAdmin returns the earliest-created administrator, or nil when
// there is none. Server-key (Nan) traffic attributes its watch state
// to this user.
func (s *Store) FirstAdmin() (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT ` + userCols + ` FROM users
		WHERE is_admin <> 0 ORDER BY rowid ASC LIMIT 1`))
}

// DeleteUser removes an account and its access tokens. The user's
// user_data rows are kept (they become unreachable orphans; the items
// they reference may still exist for everyone else).
func (s *Store) DeleteUser(id string) error {
	if _, err := s.db.Exec(`DELETE FROM access_tokens WHERE user_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// SetUserPassword replaces a user's password digest and revokes the
// user's outstanding tokens, so a reset actually locks old sessions
// out (the caller re-issues its own token when it is the same user).
func (s *Store) SetUserPassword(id, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM access_tokens WHERE user_id = ?`, id)
	return err
}

// ---- access tokens ----

// tokenHash is the stored form of a token: its SHA-256 hex.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueToken creates a new access token for the user and returns the
// plaintext (the only time it exists outside the client's hands).
func (s *Store) IssueToken(userID string) (string, error) {
	token, err := newRandomHex(32)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO access_tokens(token_hash, user_id, created_at, last_used_at)
		VALUES (?, ?, ?, ?)`, tokenHash(token), userID, now, now); err != nil {
		return "", err
	}
	return token, nil
}

// ResolveToken maps a presented token to its user id. last_used_at is
// refreshed at most once a minute so steady playback reporting does
// not turn into a write per request.
func (s *Store) ResolveToken(token string) (string, bool, error) {
	if token == "" {
		return "", false, nil
	}
	var userID string
	var lastUsed int64
	err := s.db.QueryRow(`SELECT user_id, last_used_at FROM access_tokens WHERE token_hash = ?`,
		tokenHash(token)).Scan(&userID, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if now := time.Now().Unix(); now-lastUsed >= 60 {
		if _, err := s.db.Exec(`UPDATE access_tokens SET last_used_at = ? WHERE token_hash = ?`,
			now, tokenHash(token)); err != nil {
			return "", false, err
		}
	}
	return userID, true, nil
}

// RevokeToken drops one token (logout). Unknown tokens are a no-op.
func (s *Store) RevokeToken(token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM access_tokens WHERE token_hash = ?`, tokenHash(token))
	return err
}

// MigrateLegacyUserData adopts the pre-account watch-state rows
// (user_id = LegacyUserID) into the given (first) administrator's
// account. It runs once, when that administrator is created; after the
// update no legacy rows remain, so later calls are no-ops.
func (s *Store) MigrateLegacyUserData(adminID string) error {
	if adminID == "" || adminID == LegacyUserID {
		return nil
	}
	_, err := s.db.Exec(`UPDATE user_data SET user_id = ? WHERE user_id = ?`, adminID, LegacyUserID)
	return err
}
