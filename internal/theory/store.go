package theory

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// LockLease is how long a compile lock lasts without renewal (LOCK-3).
const LockLease = 5 * time.Minute

// TokenPrefix starts every token, so secret scanners can spot one (AUTH-5).
const TokenPrefix = "theory_"

// ErrLockGone means the lock id is not the vault's live lock: it expired,
// was broken or released, or never existed (LOCK-6).
var ErrLockGone = errors.New("Your compile lock expired. The marker didn't move.")

// Member is a person on the server (AUTH-1).
type Member struct {
	ID    int64
	Name  string
	Email string
}

// Lock is a vault's compile lock (LOCK-1).
type Lock struct {
	ID        string
	Vault     string
	Name      string
	Email     string
	Since     time.Time
	ExpiresAt time.Time
}

// Report is a stored compile report (SHR-1).
type Report struct {
	ID        int64
	Name      string
	Email     string
	CreatedAt time.Time
	JSON      []byte
}

// Store is the server's SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS members (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	email TEXT NOT NULL UNIQUE COLLATE NOCASE
);
CREATE TABLE IF NOT EXISTS tokens (
	hash TEXT PRIMARY KEY,
	member_id INTEGER NOT NULL REFERENCES members(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS locks (
	vault TEXT PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	email TEXT NOT NULL,
	since INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS reports (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	vault TEXT NOT NULL,
	name TEXT NOT NULL,
	email TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	report BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS reports_by_vault ON reports(vault, id);
`

// OpenStore opens or creates the database at path.
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create the database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// AddMember adds a member. Emails are unique, ignoring case (AUTH-3).
func (s *Store) AddMember(name, email string) error {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	if name == "" {
		return errors.New("A member needs a name.")
	}
	if !strings.Contains(email, "@") {
		return fmt.Errorf("%q isn't an email address.", email)
	}
	if _, err := s.MemberByEmail(email); err == nil {
		return fmt.Errorf("A member with the email %s already exists.", email)
	}
	_, err := s.db.Exec(`INSERT INTO members (name, email) VALUES (?, ?)`, name, email)
	return err
}

// MemberByEmail finds a member, ignoring case.
func (s *Store) MemberByEmail(email string) (*Member, error) {
	var m Member
	err := s.db.QueryRow(`SELECT id, name, email FROM members WHERE email = ?`, strings.TrimSpace(email)).Scan(&m.ID, &m.Name, &m.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("There is no member with the email %s.", email)
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RemoveMember removes a member and revokes all their tokens (AUTH-4).
func (s *Store) RemoveMember(email string) error {
	m, err := s.MemberByEmail(email)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM members WHERE id = ?`, m.ID)
	return err
}

// CreateToken makes a token for a member. Only its hash is stored (AUTH-5, AUTH-6).
func (s *Store) CreateToken(email string) (string, error) {
	m, err := s.MemberByEmail(email)
	if err != nil {
		return "", err
	}
	token := TokenPrefix + randomHex(32)
	if _, err := s.db.Exec(`INSERT INTO tokens (hash, member_id) VALUES (?, ?)`, hashToken(token), m.ID); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeTokens revokes every token of a member (AUTH-7).
func (s *Store) RevokeTokens(email string) (int64, error) {
	m, err := s.MemberByEmail(email)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`DELETE FROM tokens WHERE member_id = ?`, m.ID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MemberByToken returns the token's member, or nil when the token isn't valid.
func (s *Store) MemberByToken(token string) (*Member, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, nil
	}
	var m Member
	err := s.db.QueryRow(`SELECT m.id, m.name, m.email FROM tokens t JOIN members m ON m.id = t.member_id WHERE t.hash = ?`,
		hashToken(token)).Scan(&m.ID, &m.Name, &m.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func scanLock(row interface{ Scan(...any) error }) (*Lock, error) {
	var l Lock
	var since, expires int64
	err := row.Scan(&l.ID, &l.Vault, &l.Name, &l.Email, &since, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.Since, l.ExpiresAt = time.UnixMilli(since).UTC(), time.UnixMilli(expires).UTC()
	return &l, nil
}

const lockColumns = `id, vault, name, email, since, expires_at`

// CurrentLock returns the vault's live lock, or nil. An expired lock is no lock (LOCK-3).
func (s *Store) CurrentLock(vault string, now time.Time) (*Lock, error) {
	return scanLock(s.db.QueryRow(`SELECT `+lockColumns+` FROM locks WHERE vault = ? AND expires_at > ?`, vault, now.UnixMilli()))
}

// TakeLock takes the vault's lock for m. When someone holds it, it returns
// their lock as held and no new lock (LOCK-1, LOCK-2).
func (s *Store) TakeLock(vault string, m *Member, now time.Time) (lock, held *Lock, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	held, err = scanLock(tx.QueryRow(`SELECT `+lockColumns+` FROM locks WHERE vault = ? AND expires_at > ?`, vault, now.UnixMilli()))
	if err != nil || held != nil {
		return nil, held, err
	}
	if _, err := tx.Exec(`DELETE FROM locks WHERE vault = ?`, vault); err != nil {
		return nil, nil, err
	}
	l := &Lock{ID: randomHex(16), Vault: vault, Name: m.Name, Email: m.Email,
		Since: now.UTC().Truncate(time.Millisecond), ExpiresAt: now.Add(LockLease).UTC().Truncate(time.Millisecond)}
	if _, err := tx.Exec(`INSERT INTO locks (`+lockColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		l.ID, l.Vault, l.Name, l.Email, l.Since.UnixMilli(), l.ExpiresAt.UnixMilli()); err != nil {
		return nil, nil, err
	}
	return l, nil, tx.Commit()
}

// RenewLock extends a live lock by a full lease (LOCK-3). Anything else is ErrLockGone (LOCK-6).
func (s *Store) RenewLock(vault, id string, now time.Time) (time.Time, error) {
	expires := now.Add(LockLease).UTC().Truncate(time.Millisecond)
	res, err := s.db.Exec(`UPDATE locks SET expires_at = ? WHERE vault = ? AND id = ? AND expires_at > ?`,
		expires.UnixMilli(), vault, id, now.UnixMilli())
	if err != nil {
		return time.Time{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return time.Time{}, ErrLockGone
	}
	return expires, nil
}

// ReleaseLock releases a live lock and stores the report, if any. While the
// release is open, it calls moveMarker; when that fails, nothing changes and
// the lock stays held. When saving fails after the marker moved, it calls the
// undo moveMarker returned. So the marker and the lock change together or not
// at all (LOCK-8).
func (s *Store) ReleaseLock(vault, id string, now time.Time, report []byte, moveMarker func() (undo func(), err error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	l, err := scanLock(tx.QueryRow(`SELECT `+lockColumns+` FROM locks WHERE vault = ? AND id = ? AND expires_at > ?`, vault, id, now.UnixMilli()))
	if err != nil {
		return err
	}
	if l == nil {
		return ErrLockGone
	}
	if _, err := tx.Exec(`DELETE FROM locks WHERE vault = ?`, vault); err != nil {
		return err
	}
	if report != nil {
		if _, err := tx.Exec(`INSERT INTO reports (vault, name, email, created_at, report) VALUES (?, ?, ?, ?, ?)`,
			vault, l.Name, l.Email, now.UnixMilli(), report); err != nil {
			return err
		}
	}
	undo, err := moveMarker()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if undo != nil {
			undo()
		}
		return err
	}
	return nil
}

// BreakLock removes the vault's lock, live or not (LOCK-5).
func (s *Store) BreakLock(vault string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM locks WHERE vault = ?`, vault)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReportPage is how many reports one request lists (SHR-2).
const ReportPage = 20

// Reports lists a vault's reports newest first, before the given id when it isn't 0 (SHR-2).
func (s *Store) Reports(vault string, before int64) ([]Report, error) {
	q := `SELECT id, name, email, created_at, report FROM reports WHERE vault = ?`
	args := []any{vault}
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, ReportPage)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []Report{}
	for rows.Next() {
		var r Report
		var created int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Email, &created, &r.JSON); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		reports = append(reports, r)
	}
	return reports, rows.Err()
}
