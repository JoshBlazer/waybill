package httpapi

import (
	"context"
	"errors"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

func forbidden() Problem {
	return problem(403, "forbidden", "Your role in this organisation does not allow this.", "")
}

func usdcAmount(a money.Amount) Amount {
	return Amount{Asset: string(money.USDC.Code), Minor: a.String(), Scale: int(money.USDC.Scale)}
}

func orgJSON(o store.PayerOrg) Org {
	return Org{Id: o.ID, Name: o.Name, ApprovalThreshold: usdcAmount(o.ApprovalThreshold), CreatedAt: o.CreatedAt.UTC()}
}

func memberJSON(m store.OrgMember) Member {
	roles := make([]MemberRole, len(m.Roles))
	for i, r := range m.Roles {
		roles[i] = MemberRole(r)
	}
	out := Member{Id: m.ID, Name: m.Name, Roles: roles, CreatedAt: m.CreatedAt.UTC()}
	if m.RemovedAt != nil {
		t := m.RemovedAt.UTC()
		out.RemovedAt = &t
	}
	return out
}

func roleStrings(rs []MemberRole) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

func (s *Server) member(ctx context.Context) auth.Principal {
	p, _ := auth.MemberFrom(ctx) // the middleware guarantees a member
	return p
}

// GetOrg returns the caller's organisation.
func (s *Server) GetOrg(ctx context.Context, _ GetOrgRequestObject) (GetOrgResponseObject, error) {
	o, err := store.New(s.Pool).GetOrg(ctx, s.member(ctx).OrgID)
	if err != nil {
		return nil, err
	}
	return GetOrg200JSONResponse(orgJSON(o)), nil
}

// UpdateOrg changes the approval threshold.
func (s *Server) UpdateOrg(ctx context.Context, req UpdateOrgRequestObject) (UpdateOrgResponseObject, error) {
	threshold, err := money.ParseDecimal(req.Body.ApprovalThreshold, money.USDC.Scale)
	if err != nil {
		return UpdateOrg400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(problem(400, "bad-request",
			"The threshold must be a USDC amount with at most 6 decimals.", ""))}, nil
	}
	o, err := s.Org.SetThreshold(ctx, s.member(ctx), threshold)
	if errors.Is(err, org.ErrForbidden) {
		return UpdateOrg403ApplicationProblemPlusJSONResponse(forbidden()), nil
	}
	if err != nil {
		return nil, err
	}
	return UpdateOrg200JSONResponse(orgJSON(o)), nil
}

// ListMembers lists the organisation's members.
func (s *Server) ListMembers(ctx context.Context, _ ListMembersRequestObject) (ListMembersResponseObject, error) {
	ms, err := store.New(s.Pool).ListMembers(ctx, s.member(ctx).OrgID)
	if err != nil {
		return nil, err
	}
	out := MemberList{Data: make([]Member, len(ms))}
	for i, m := range ms {
		out.Data[i] = memberJSON(m)
	}
	return ListMembers200JSONResponse(out), nil
}

// AddMember adds a member with a first API key.
func (s *Server) AddMember(ctx context.Context, req AddMemberRequestObject) (AddMemberResponseObject, error) {
	m, key, err := s.Org.AddMember(ctx, s.member(ctx), req.Body.Name, roleStrings(req.Body.Roles))
	switch {
	case errors.Is(err, org.ErrForbidden):
		return AddMember403ApplicationProblemPlusJSONResponse(forbidden()), nil
	case errors.Is(err, org.ErrBadRoles):
		return AddMember400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(problem(400, "bad-request", "Unknown or missing roles.", ""))}, nil
	case err != nil:
		return nil, err
	}
	return AddMember201JSONResponse(AddedMember{Member: memberJSON(m), ApiKey: key}), nil
}

// UpdateMember changes a member's roles or removes them.
func (s *Server) UpdateMember(ctx context.Context, req UpdateMemberRequestObject) (UpdateMemberResponseObject, error) {
	var roles []string
	if req.Body.Roles != nil {
		roles = roleStrings(*req.Body.Roles)
	}
	remove := req.Body.Removed != nil && *req.Body.Removed
	if roles == nil && !remove {
		return UpdateMember400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(problem(400, "bad-request", "Nothing to change: give roles, or removed: true.", ""))}, nil
	}
	m, err := s.Org.UpdateMember(ctx, s.member(ctx), req.Id, roles, remove)
	switch {
	case errors.Is(err, org.ErrForbidden):
		return UpdateMember403ApplicationProblemPlusJSONResponse(forbidden()), nil
	case errors.Is(err, org.ErrNotFound):
		return UpdateMember404ApplicationProblemPlusJSONResponse(problem(404, "not-found", "No such member in your organisation.", "")), nil
	case errors.Is(err, org.ErrLastAdmin):
		return UpdateMember409ApplicationProblemPlusJSONResponse(problem(409, "last-admin",
			"The organisation needs at least one admin. Make someone else an admin first.", "")), nil
	case errors.Is(err, org.ErrBadRoles):
		return UpdateMember400ApplicationProblemPlusJSONResponse{ProblemApplicationProblemPlusJSONResponse(problem(400, "bad-request", "Unknown or missing roles.", ""))}, nil
	case err != nil:
		return nil, err
	}
	return UpdateMember200JSONResponse(memberJSON(m)), nil
}

// GetFunding returns the funding addresses and balances.
func (s *Server) GetFunding(ctx context.Context, _ GetFundingRequestObject) (GetFundingResponseObject, error) {
	f, err := s.Org.Funding(ctx, s.member(ctx).OrgID)
	if err != nil {
		return nil, err
	}
	out := Funding{DepositAddresses: make([]DepositAddress, len(f.Addresses)), Pending: usdcAmount(f.Pending), Available: usdcAmount(f.Available)}
	for i, a := range f.Addresses {
		tok := a.Token
		out.DepositAddresses[i] = DepositAddress{Network: string(a.Network), Address: a.Address, Token: &tok}
	}
	return GetFunding200JSONResponse(out), nil
}
