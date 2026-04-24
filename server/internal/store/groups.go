package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrGroupNotFound signals no group row matches the query.
// Handlers surface this as 404.
var ErrGroupNotFound = errors.New("store: group not found")

// Group is the store-facing shape of one approver group. Name is
// the stable identifier that approval gates reference; id is the
// URL handle the admin UI uses.
type Group struct {
	ID          uuid.UUID
	Name        string
	Description string
	MemberCount int
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GroupMember is one row in the join table joined against the
// users table — enough for the admin UI to render the member
// list without a second lookup.
type GroupMember struct {
	UserID  uuid.UUID
	Email   string
	Name    string
	Role    string
	AddedAt time.Time
}

// GroupInput is the write shape for Insert + Update.
type GroupInput struct {
	Name        string
	Description string
	CreatedBy   *uuid.UUID
}

// ListGroups returns every group in the system + its member count.
// Admin-only at the API layer.
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.q.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list groups: %w", err)
	}
	out := make([]Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, Group{
			ID:          fromPgUUID(r.ID),
			Name:        r.Name,
			Description: r.Description,
			MemberCount: int(r.MemberCount),
			CreatedBy:   pgUUIDPtr(r.CreatedBy),
			CreatedAt:   r.CreatedAt.Time,
			UpdatedAt:   r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (s *Store) GetGroup(ctx context.Context, id uuid.UUID) (Group, error) {
	row, err := s.q.GetGroup(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Group{}, ErrGroupNotFound
		}
		return Group{}, fmt.Errorf("store: get group: %w", err)
	}
	return Group{
		ID:          fromPgUUID(row.ID),
		Name:        row.Name,
		Description: row.Description,
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

func (s *Store) InsertGroup(ctx context.Context, in GroupInput) (Group, error) {
	row, err := s.q.InsertGroup(ctx, db.InsertGroupParams{
		Name:        in.Name,
		Description: in.Description,
		CreatedBy:   pgUUIDFromPtr(in.CreatedBy),
	})
	if err != nil {
		return Group{}, fmt.Errorf("store: insert group: %w", err)
	}
	return Group{
		ID:          fromPgUUID(row.ID),
		Name:        row.Name,
		Description: row.Description,
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

func (s *Store) UpdateGroup(ctx context.Context, id uuid.UUID, in GroupInput) error {
	if err := s.q.UpdateGroup(ctx, db.UpdateGroupParams{
		ID:          pgUUID(id),
		Name:        in.Name,
		Description: in.Description,
	}); err != nil {
		return fmt.Errorf("store: update group: %w", err)
	}
	return nil
}

func (s *Store) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteGroup(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete group: %w", err)
	}
	return nil
}

// ListGroupMembers returns the members of a group joined with
// their user details.
func (s *Store) ListGroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error) {
	rows, err := s.q.ListGroupMembers(ctx, pgUUID(groupID))
	if err != nil {
		return nil, fmt.Errorf("store: list group members: %w", err)
	}
	out := make([]GroupMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, GroupMember{
			UserID:  fromPgUUID(r.ID),
			Email:   r.Email,
			Name:    r.Name,
			Role:    r.Role,
			AddedAt: r.AddedAt.Time,
		})
	}
	return out, nil
}

// AddGroupMember is idempotent — re-adding a member is a no-op.
func (s *Store) AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, addedBy *uuid.UUID) error {
	if err := s.q.AddGroupMember(ctx, db.AddGroupMemberParams{
		GroupID: pgUUID(groupID),
		UserID:  pgUUID(userID),
		AddedBy: pgUUIDFromPtr(addedBy),
	}); err != nil {
		return fmt.Errorf("store: add group member: %w", err)
	}
	return nil
}

func (s *Store) RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error {
	if err := s.q.RemoveGroupMember(ctx, db.RemoveGroupMemberParams{
		GroupID: pgUUID(groupID),
		UserID:  pgUUID(userID),
	}); err != nil {
		return fmt.Errorf("store: remove group member: %w", err)
	}
	return nil
}

// ListUserGroupNames returns the group names this user belongs to.
// Hot path: called on every approve/reject to check whether the
// user's groups intersect the gate's approver_groups.
func (s *Store) ListUserGroupNames(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := s.q.ListUserGroupNames(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("store: list user groups: %w", err)
	}
	return rows, nil
}
