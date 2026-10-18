// Package org holds payer organisations: members and their roles, the
// approval threshold, and the funding addresses and balance that batch runs
// are paid from (docs/design/teams.md, ADR-043).
package org

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Roles a member can hold.
const (
	RoleAdmin    = "admin"
	RoleMaker    = "maker"
	RoleApprover = "approver"
)

// Errors from Service. Handlers map them to 403, 404, 409 and 400.
var (
	ErrForbidden = errors.New("org: the member does not have the role this needs")
	ErrNotFound  = errors.New("org: no such member in this organisation")
	ErrLastAdmin = errors.New("org: the organisation's last admin cannot be removed or lose the admin role")
	ErrBadRoles  = errors.New("org: roles must be a non-empty set of admin, maker and approver")
)

// Service manages organisations.
type Service struct {
	Pool        *pgxpool.Pool
	Deployments map[safety.Network]deployments.Deployment
}

// Funding is an organisation's funding addresses and USDC balances.
type Funding struct {
	Addresses []FundingAddress
	Pending   money.Amount
	Available money.Amount
}

// FundingAddress is where to send test USDC on one network.
type FundingAddress struct {
	Network safety.Network
	Address string // EIP-55
	Token   string // EIP-55
}

// HasRole reports whether a member holds role.
func HasRole(m store.OrgMember, role string) bool {
	return m.RemovedAt == nil && slices.Contains(m.Roles, role)
}

// Caller loads the authenticated member, who must still be in the
// organisation.
func Caller(ctx context.Context, db store.DBTX, p auth.Principal) (store.OrgMember, error) {
	m, err := store.New(db).GetMember(ctx, store.GetMemberParams{ID: p.MemberID, OrgID: p.OrgID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.RemovedAt != nil) {
		return store.OrgMember{}, ErrForbidden
	}
	return m, err
}

func (s *Service) requireAdmin(ctx context.Context, db store.DBTX, p auth.Principal) error {
	m, err := Caller(ctx, db, p)
	if err != nil {
		return err
	}
	if !HasRole(m, RoleAdmin) {
		return ErrForbidden
	}
	return nil
}

// SetThreshold changes the approval threshold. Admins only.
func (s *Service) SetThreshold(ctx context.Context, p auth.Principal, threshold money.Amount) (store.PayerOrg, error) {
	if threshold.Sign() < 0 {
		return store.PayerOrg{}, fmt.Errorf("org: negative threshold")
	}
	if err := s.requireAdmin(ctx, s.Pool, p); err != nil {
		return store.PayerOrg{}, err
	}
	return store.New(s.Pool).SetOrgThreshold(ctx, store.SetOrgThresholdParams{ID: p.OrgID, ApprovalThreshold: threshold})
}

// NormalizeRoles returns roles sorted and without duplicates, or
// ErrBadRoles.
func NormalizeRoles(roles []string) ([]string, error) {
	out := slices.Clone(roles)
	sort.Strings(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil, ErrBadRoles
	}
	for _, r := range out {
		if r != RoleAdmin && r != RoleMaker && r != RoleApprover {
			return nil, ErrBadRoles
		}
	}
	return out, nil
}

// AddMember adds a member and returns them with their first API key, which
// is never shown again. Admins only.
func (s *Service) AddMember(ctx context.Context, p auth.Principal, name string, roles []string) (store.OrgMember, string, error) {
	roles, err := NormalizeRoles(roles)
	if err != nil {
		return store.OrgMember{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return store.OrgMember{}, "", fmt.Errorf("org: a member needs a name of 1 to 200 characters")
	}
	key, err := auth.GenerateKey()
	if err != nil {
		return store.OrgMember{}, "", err
	}
	var m store.OrgMember
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireAdmin(ctx, tx, p); err != nil {
			return err
		}
		q := store.New(tx)
		if m, err = q.InsertMember(ctx, store.InsertMemberParams{OrgID: p.OrgID, Name: name, Roles: roles}); err != nil {
			return err
		}
		return q.InsertMemberAPIKey(ctx, store.InsertMemberAPIKeyParams{
			MemberID: m.ID, Prefix: key.Prefix, SecretSha256: key.Hash[:], Name: "first key",
		})
	})
	if err != nil {
		return store.OrgMember{}, "", err
	}
	return m, key.Full, nil
}

// UpdateMember changes a member's roles, or removes them. Admins only. The
// organisation always keeps at least one admin.
func (s *Service) UpdateMember(ctx context.Context, p auth.Principal, id uuid.UUID, roles []string, remove bool) (store.OrgMember, error) {
	if roles != nil {
		var err error
		if roles, err = NormalizeRoles(roles); err != nil {
			return store.OrgMember{}, err
		}
	}
	var m store.OrgMember
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		all, err := q.LockOrgMembers(ctx, p.OrgID)
		if err != nil {
			return err
		}
		if err := s.requireAdmin(ctx, tx, p); err != nil {
			return err
		}
		admins := 0
		var target *store.LockOrgMembersRow
		for i := range all {
			if all[i].RemovedAt == nil && slices.Contains(all[i].Roles, RoleAdmin) {
				admins++
			}
			if all[i].ID == id {
				target = &all[i]
			}
		}
		if target == nil {
			return ErrNotFound
		}
		wasAdmin := target.RemovedAt == nil && slices.Contains(target.Roles, RoleAdmin)
		losesAdmin := remove || (roles != nil && !slices.Contains(roles, RoleAdmin))
		if wasAdmin && losesAdmin && admins <= 1 {
			return ErrLastAdmin
		}
		if roles != nil {
			if m, err = q.SetMemberRoles(ctx, store.SetMemberRolesParams{ID: id, OrgID: p.OrgID, Roles: roles}); errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound // removed members keep their last roles
			} else if err != nil {
				return err
			}
		}
		if remove {
			if m, err = q.RemoveMember(ctx, store.RemoveMemberParams{ID: id, OrgID: p.OrgID}); err != nil {
				return err
			}
		}
		if roles == nil && !remove {
			m, err = q.GetMember(ctx, store.GetMemberParams{ID: id, OrgID: p.OrgID})
		}
		return err
	})
	return m, err
}

// Funding returns the organisation's funding addresses, creating them on
// first use, and its USDC balances.
func (s *Service) Funding(ctx context.Context, orgID uuid.UUID) (Funding, error) {
	q := store.New(s.Pool)
	salt := evm.SaltForOrg(orgID)
	networks := make([]safety.Network, 0, len(s.Deployments))
	for n := range s.Deployments {
		networks = append(networks, n)
	}
	slices.Sort(networks)
	var f Funding
	for _, n := range networks {
		d := s.Deployments[n]
		addr := d.PredictDepositAddress(salt)
		if err := q.InsertOrgDepositAddress(ctx, store.InsertOrgDepositAddressParams{
			Network: string(n), Address: evm.NormalizeAddress(addr), OrgID: orgID, Salt: salt[:],
		}); err != nil {
			return Funding{}, fmt.Errorf("org: funding address on %s: %w", n, err)
		}
		f.Addresses = append(f.Addresses, FundingAddress{Network: n, Address: addr.Hex(), Token: d.Token.Hex()})
	}
	var err error
	if f.Pending, err = s.balance(ctx, orgID, "pending"); err != nil {
		return Funding{}, err
	}
	if f.Available, err = s.balance(ctx, orgID, "available"); err != nil {
		return Funding{}, err
	}
	return f, nil
}

// balance reads a liability balance (stored as a credit, so non-positive)
// as the positive amount owed to the organisation.
func (s *Service) balance(ctx context.Context, orgID uuid.UUID, which string) (money.Amount, error) {
	code := fmt.Sprintf("liability:org:%s:%s:%s", orgID, money.USDC.Code, which)
	b, err := store.New(s.Pool).GetBalanceByCode(ctx, code)
	if err != nil {
		return money.Amount{}, err
	}
	return b.Neg(), nil
}
