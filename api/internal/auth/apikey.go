// Package auth authenticates contractors and organisation members by API
// key.
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

// Principal is who a key belongs to: a contractor, or a member of a payer
// organisation. Exactly one of ContractorID and MemberID is set.
type Principal struct {
	ContractorID uuid.UUID
	MemberID     uuid.UUID
	OrgID        uuid.UUID // the member's organisation
}

// IsMember reports whether the key belongs to an organisation member.
func (p Principal) IsMember() bool { return p.MemberID != uuid.Nil }

// Authenticate resolves an "Authorization: Bearer <key>" header value to the
// key's owner.
func Authenticate(ctx context.Context, db store.DBTX, authorization string) (Principal, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	k, err := ParseKey(strings.TrimSpace(token))
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	row, err := store.New(db).GetActiveAPIKey(ctx, k.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err // a database failure is not "wrong key"
	}
	if subtle.ConstantTimeCompare(row.SecretSha256, k.Hash[:]) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	switch {
	case row.ContractorID != nil:
		p.ContractorID = *row.ContractorID
	case row.MemberID != nil && row.OrgID != nil:
		p.MemberID, p.OrgID = *row.MemberID, *row.OrgID
	default:
		return Principal{}, ErrUnauthenticated
	}
	// Best effort: a failure to record use must not refuse a valid key.
	_ = store.New(db).TouchAPIKey(ctx, k.Prefix)
	return p, nil
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying the authenticated key owner.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// WithContractor returns ctx carrying an authenticated contractor.
func WithContractor(ctx context.Context, id uuid.UUID) context.Context {
	return WithPrincipal(ctx, Principal{ContractorID: id})
}

// ContractorFrom returns the authenticated contractor id, if the key
// belongs to a contractor.
func ContractorFrom(ctx context.Context) (uuid.UUID, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok || p.ContractorID == uuid.Nil {
		return uuid.Nil, false
	}
	return p.ContractorID, true
}

// MemberFrom returns the authenticated organisation member, if the key
// belongs to one.
func MemberFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok || !p.IsMember() {
		return Principal{}, false
	}
	return p, true
}
