// Package auth authenticates contractors by API key.
//
// A key looks like wb_test_<prefix>_<secret>. The prefix (8 characters) is a
// public lookup id; the database stores only SHA-256 of the whole key, and
// comparison is constant-time. "test" in the key marks it as test-mode, in
// line with the test-money rule: there is no live key format.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/store"
)

// KeyPrefix starts every Waybill API key.
const KeyPrefix = "wb_test_"

var (
	// ErrUnauthenticated covers every failure: missing, malformed, unknown,
	// revoked or wrong key. Callers must not reveal which.
	ErrUnauthenticated = errors.New("auth: unauthenticated")

	keyPattern = regexp.MustCompile(`^wb_test_([0-9a-z]{8})_([a-z2-7]{52})$`)
	b32        = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// Key is a newly generated API key. Full is shown to its owner once.
type Key struct {
	Full   string
	Prefix string
	Hash   [32]byte
}

// GenerateKey returns a new random key.
func GenerateKey() (Key, error) {
	var p [5]byte // 40 bits → 8 base32 characters
	var s [32]byte
	if _, err := rand.Read(p[:]); err != nil {
		return Key{}, err
	}
	if _, err := rand.Read(s[:]); err != nil {
		return Key{}, err
	}
	prefix := strings.ToLower(b32.EncodeToString(p[:]))
	secret := strings.ToLower(b32.EncodeToString(s[:]))
	return ParseKey(KeyPrefix + prefix + "_" + secret)
}

// ParseKey validates the format of an existing key and computes its hash.
func ParseKey(full string) (Key, error) {
	m := keyPattern.FindStringSubmatch(full)
	if m == nil {
		return Key{}, ErrUnauthenticated
	}
	return Key{Full: full, Prefix: m[1], Hash: sha256.Sum256([]byte(full))}, nil
}

// Authenticate resolves an "Authorization: Bearer <key>" header value to a
// contractor id.
func Authenticate(ctx context.Context, db store.DBTX, authorization string) (uuid.UUID, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return uuid.Nil, ErrUnauthenticated
	}
	k, err := ParseKey(strings.TrimSpace(token))
	if err != nil {
		return uuid.Nil, ErrUnauthenticated
	}
	row, err := store.New(db).GetActiveAPIKey(ctx, k.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrUnauthenticated
	}
	if err != nil {
		return uuid.Nil, err // a database failure is not "wrong key"
	}
	if subtle.ConstantTimeCompare(row.SecretSha256, k.Hash[:]) != 1 {
		return uuid.Nil, ErrUnauthenticated
	}
	return row.ContractorID, nil
}

type ctxKey struct{}

// WithContractor returns ctx carrying the authenticated contractor id.
func WithContractor(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// ContractorFrom returns the authenticated contractor id, if any.
func ContractorFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKey{}).(uuid.UUID)
	return id, ok
}
