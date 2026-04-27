package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrRunnerProfileNotFound is the canonical 404 signal — handlers
// surface as 404, the apply-time resolver surfaces as a YAML
// validation error ("unknown profile X").
var ErrRunnerProfileNotFound = errors.New("store: runner profile not found")

// RunnerProfile is the store-facing shape. Strings carry k8s
// quantity format ("100m", "256Mi"); empty means "not set" so the
// caller falls back to either user input or zero policy.
type RunnerProfile struct {
	ID                uuid.UUID
	Name              string
	Description       string
	Engine            string
	DefaultImage      string
	DefaultCPURequest string
	DefaultCPULimit   string
	DefaultMemRequest string
	DefaultMemLimit   string
	MaxCPU            string
	MaxMem            string
	Tags              []string
	Config            map[string]any
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RunnerProfileInput is the write shape for Insert + Update. ID is
// allocated by the DB on insert, ignored on update (passed via
// Update's first arg).
type RunnerProfileInput struct {
	Name              string
	Description       string
	Engine            string
	DefaultImage      string
	DefaultCPURequest string
	DefaultCPULimit   string
	DefaultMemRequest string
	DefaultMemLimit   string
	MaxCPU            string
	MaxMem            string
	Tags              []string
	Config            map[string]any
}

// ListRunnerProfiles returns every profile, sorted by name.
func (s *Store) ListRunnerProfiles(ctx context.Context) ([]RunnerProfile, error) {
	rows, err := s.q.ListRunnerProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list runner profiles: %w", err)
	}
	out := make([]RunnerProfile, 0, len(rows))
	for _, r := range rows {
		p, err := runnerProfileFromRow(db.RunnerProfile(r))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetRunnerProfile returns one profile by id; ErrRunnerProfileNotFound
// when no row matches.
func (s *Store) GetRunnerProfile(ctx context.Context, id uuid.UUID) (RunnerProfile, error) {
	row, err := s.q.GetRunnerProfile(ctx, toPgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunnerProfile{}, ErrRunnerProfileNotFound
	}
	if err != nil {
		return RunnerProfile{}, fmt.Errorf("store: get runner profile %s: %w", id, err)
	}
	return runnerProfileFromRow(row)
}

// GetRunnerProfileByName is the apply-time resolver path: pipelines
// reference profiles by name in YAML, so this is the lookup the
// validator uses to materialise the row.
func (s *Store) GetRunnerProfileByName(ctx context.Context, name string) (RunnerProfile, error) {
	row, err := s.q.GetRunnerProfileByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunnerProfile{}, ErrRunnerProfileNotFound
	}
	if err != nil {
		return RunnerProfile{}, fmt.Errorf("store: get runner profile %q: %w", name, err)
	}
	return runnerProfileFromRow(row)
}

// InsertRunnerProfile creates a new row. Returns the persisted
// shape (id + timestamps populated by the DB).
func (s *Store) InsertRunnerProfile(ctx context.Context, in RunnerProfileInput) (RunnerProfile, error) {
	cfg, err := encodeProfileConfig(in.Config)
	if err != nil {
		return RunnerProfile{}, err
	}
	row, err := s.q.InsertRunnerProfile(ctx, db.InsertRunnerProfileParams{
		Name:              in.Name,
		Description:       in.Description,
		Engine:            in.Engine,
		DefaultImage:      in.DefaultImage,
		DefaultCpuRequest: in.DefaultCPURequest,
		DefaultCpuLimit:   in.DefaultCPULimit,
		DefaultMemRequest: in.DefaultMemRequest,
		DefaultMemLimit:   in.DefaultMemLimit,
		MaxCpu:            in.MaxCPU,
		MaxMem:            in.MaxMem,
		Tags:              normalizeTags(in.Tags),
		Config:            cfg,
	})
	if err != nil {
		return RunnerProfile{}, fmt.Errorf("store: insert runner profile %q: %w", in.Name, err)
	}
	return runnerProfileFromRow(row)
}

// UpdateRunnerProfile rewrites an existing row in place. ID must
// match an existing row; returns ErrRunnerProfileNotFound when the
// row is gone (treated as zero rows affected).
func (s *Store) UpdateRunnerProfile(ctx context.Context, id uuid.UUID, in RunnerProfileInput) error {
	cfg, err := encodeProfileConfig(in.Config)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
        UPDATE runner_profiles
        SET name = $2, description = $3, engine = $4,
            default_image = $5,
            default_cpu_request = $6, default_cpu_limit = $7,
            default_mem_request = $8, default_mem_limit = $9,
            max_cpu = $10, max_mem = $11,
            tags = $12, config = $13,
            updated_at = NOW()
        WHERE id = $1
    `, toPgUUID(id),
		in.Name, in.Description, in.Engine,
		in.DefaultImage,
		in.DefaultCPURequest, in.DefaultCPULimit,
		in.DefaultMemRequest, in.DefaultMemLimit,
		in.MaxCPU, in.MaxMem,
		normalizeTags(in.Tags), cfg,
	)
	if err != nil {
		return fmt.Errorf("store: update runner profile %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRunnerProfileNotFound
	}
	return nil
}

// CountPipelinesUsingRunnerProfile returns the number of pipeline
// definitions whose JSONB has a job with `Profile == name`. Cheap
// gate the delete handler runs before issuing the destructive op
// — exposes the dependency without a deep scan in the handler.
func (s *Store) CountPipelinesUsingRunnerProfile(ctx context.Context, name string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
        SELECT COUNT(*) FROM pipelines
        WHERE jsonb_path_exists(
            definition,
            '$.Jobs[*] ? (@.Profile == $name)',
            jsonb_build_object('name', $1::text)
        )
    `, name).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pipelines using profile %q: %w", name, err)
	}
	return n, nil
}

// DeleteRunnerProfile removes the row. Caller must have already
// checked that no pipeline references the profile name.
func (s *Store) DeleteRunnerProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM runner_profiles WHERE id = $1`, toPgUUID(id))
	if err != nil {
		return fmt.Errorf("store: delete runner profile %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRunnerProfileNotFound
	}
	return nil
}

func runnerProfileFromRow(r db.RunnerProfile) (RunnerProfile, error) {
	cfg, err := decodeProfileConfig(r.Config)
	if err != nil {
		return RunnerProfile{}, err
	}
	return RunnerProfile{
		ID:                fromPgUUID(r.ID),
		Name:              r.Name,
		Description:       r.Description,
		Engine:            r.Engine,
		DefaultImage:      r.DefaultImage,
		DefaultCPURequest: r.DefaultCpuRequest,
		DefaultCPULimit:   r.DefaultCpuLimit,
		DefaultMemRequest: r.DefaultMemRequest,
		DefaultMemLimit:   r.DefaultMemLimit,
		MaxCPU:            r.MaxCpu,
		MaxMem:            r.MaxMem,
		Tags:              append([]string(nil), r.Tags...),
		Config:            cfg,
		CreatedAt:         r.CreatedAt.Time,
		UpdatedAt:         r.UpdatedAt.Time,
	}, nil
}

func encodeProfileConfig(cfg map[string]any) ([]byte, error) {
	if len(cfg) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("store: marshal runner profile config: %w", err)
	}
	return b, nil
}

func decodeProfileConfig(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("store: unmarshal runner profile config: %w", err)
	}
	return out, nil
}

// toPgUUID is the inverse of fromPgUUID for the rare path that
// crafts a row directly (most code carries pgtype.UUID through).
func toPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// normalizeTags coerces a nil slice to an empty one so the NOT
// NULL `tags TEXT[]` column accepts the row. pgx maps nil → SQL
// NULL but []string{} → '{}'::text[] which is what we want.
func normalizeTags(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
