package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Account is a ForgeSync account: someone who signs in to the controllers
// themselves. They live in this database and nowhere else -- they aren't
// Forgejo users, nothing replicates them to a node, and a node never
// learns they exist. Every controller shares the database, so an account
// works on either one, which is the point: SceneID being unreachable, or
// one controller being the broken thing, is exactly when someone needs to
// sign in.
type Account struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	FullName   string     `json:"full_name,omitempty"`
	Role       string     `json:"role"`
	Disabled   bool       `json:"disabled"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastSignIn *time.Time `json:"last_sign_in,omitempty"`
}

// ErrUsernameTaken means another account already has that name.
var ErrUsernameTaken = errors.New("that username is taken")

// Accounts lists them, by name. Password hashes never leave this file.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, full_name, role, disabled, created_at, created_by, updated_at, last_sign_in
		FROM accounts ORDER BY lower(username)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		err := r.Scan(&a.ID, &a.Username, &a.FullName, &a.Role, &a.Disabled,
			&a.CreatedAt, &a.CreatedBy, &a.UpdatedAt, &a.LastSignIn)
		return a, err
	})
}

// Account returns one by id.
func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		SELECT id, username, full_name, role, disabled, created_at, created_by, updated_at, last_sign_in
		FROM accounts WHERE id = $1::uuid`, id).
		Scan(&a.ID, &a.Username, &a.FullName, &a.Role, &a.Disabled,
			&a.CreatedAt, &a.CreatedBy, &a.UpdatedAt, &a.LastSignIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// CreateAccount adds one. The password is hashed here; the plain one is
// never stored, logged or returned.
func (s *Store) CreateAccount(ctx context.Context, username, password, fullName, role, by string) (Account, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return Account{}, err
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO accounts (username, password_hash, full_name, role, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		username, hash, fullName, role, by).Scan(&id)
	if isUniqueViolation(err) {
		return Account{}, ErrUsernameTaken
	}
	if err != nil {
		return Account{}, err
	}
	return s.Account(ctx, id)
}

// UpdateAccount changes what an administrator may change about one: the
// name it shows, its role, and whether it can sign in at all.
func (s *Store) UpdateAccount(ctx context.Context, id, fullName, role string, disabled bool) (Account, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE accounts SET full_name = $2, role = $3, disabled = $4, updated_at = now()
		WHERE id = $1::uuid`, id, fullName, role, disabled)
	if err != nil {
		return Account{}, err
	}
	if tag.RowsAffected() == 0 {
		return Account{}, ErrNotFound
	}
	return s.Account(ctx, id)
}

// SetAccountPassword replaces one account's password.
func (s *Store) SetAccountPassword(ctx context.Context, id, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE accounts SET password_hash = $2, updated_at = now() WHERE id = $1::uuid`, id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAccount removes one.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAccounts says how many there are, so the first one can be made
// without one already existing to make it.
func (s *Store) CountAccounts(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n)
	return n, err
}

// CheckPassword returns the account when the name and password match and
// it isn't disabled. Everything else -- no such name, wrong password,
// disabled -- comes back the same way, so the answer says nothing about
// which it was. It costs the same either way, too: a name that doesn't
// exist is still compared against a hash.
func (s *Store) CheckPassword(ctx context.Context, username, password string) (Account, bool, error) {
	var id, hash string
	var disabled bool
	err := s.pool.QueryRow(ctx, `
		SELECT id, password_hash, disabled FROM accounts WHERE lower(username) = lower($1)`, username).
		Scan(&id, &hash, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		// Compare against a hash of nothing, so a name that doesn't exist
		// takes as long to refuse as one that does.
		_, _ = VerifyPassword(dummyHash, password)
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil || !ok || disabled {
		return Account{}, false, nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET last_sign_in = now() WHERE id = $1::uuid`, id); err != nil {
		return Account{}, false, err
	}
	a, err := s.Account(ctx, id)
	return a, err == nil, err
}

// isUniqueViolation reports a duplicate-key error from PostgreSQL.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// --------------------------------------------------------------- passwords

// pbkdf2Iterations is the cost of one check. It's stored in every hash,
// so raising it later leaves the accounts already here working.
const pbkdf2Iterations = 600_000

// dummyHash is a real hash of a value nobody has, for the constant-time
// path when the username doesn't exist.
var dummyHash, _ = HashPassword("forgesync has no such account")

// HashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<key>", the
// parts base64 without padding.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("the password is empty")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, enc(salt), enc(key)), nil
}

// VerifyPassword checks a password against a stored hash in constant time.
func VerifyPassword(stored, password string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, errors.New("unknown password hash")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, errors.New("bad iteration count in the password hash")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
