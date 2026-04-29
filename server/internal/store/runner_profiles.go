package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrRunnerProfileNotFound is the canonical 404 signal — handlers
// surface as 404, the apply-time resolver surfaces as a YAML
// validation error ("unknown profile X").
var ErrRunnerProfileNotFound = errors.New("store: runner profile not found")

// RunnerProfile is the store-facing shape. Strings carry k8s
// quantity format ("100m", "256Mi"); empty means "not set" so the
// caller falls back to either user input or zero policy.
//
// Env carries plain key/value pairs the agent injects into every
// plugin container that runs on this profile (bucket name, region,
// non-secret config). SecretKeys is the list of secret keys
// configured — values stay in the encrypted column and are NEVER
// returned via this struct. Use ResolveProfileEnvByName to get the
// decrypted secret values during dispatch.
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
	Env               map[string]string
	SecretKeys        []string // names only, sorted; values never decrypted on this path
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RunnerProfileInput is the write shape for Insert + Update. ID is
// allocated by the DB on insert, ignored on update (passed via
// Update's first arg).
//
// Secrets carries plaintext values on the way IN — Insert/Update
// encrypt each value with the cipher before persisting. Reads never
// return Secrets in this shape; only SecretKeys on the read path.
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
	Env               map[string]string
	Secrets           map[string]string
}

// ListRunnerProfiles returns every profile, sorted by name.
func (s *Store) ListRunnerProfiles(ctx context.Context) ([]RunnerProfile, error) {
	rows, err := s.q.ListRunnerProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list runner profiles: %w", err)
	}
	out := make([]RunnerProfile, 0, len(rows))
	for _, r := range rows {
		p, err := runnerProfileFromRow(r)
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
// shape (id + timestamps populated by the DB). When in.Secrets is
// non-empty, cipher must be non-nil — each value is sealed before
// hitting the column. Plaintext values never reach the DB.
func (s *Store) InsertRunnerProfile(ctx context.Context, cipher *crypto.Cipher, in RunnerProfileInput) (RunnerProfile, error) {
	cfg, err := encodeProfileConfig(in.Config)
	if err != nil {
		return RunnerProfile{}, err
	}
	envBytes, err := encodeProfileEnv(in.Env)
	if err != nil {
		return RunnerProfile{}, err
	}
	secretsBytes, err := encodeProfileSecrets(cipher, in.Secrets)
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
		Env:               envBytes,
		Secrets:           secretsBytes,
	})
	if err != nil {
		return RunnerProfile{}, fmt.Errorf("store: insert runner profile %q: %w", in.Name, err)
	}
	return runnerProfileFromRow(row)
}

// UpdateRunnerProfile rewrites an existing row in place. ID must
// match an existing row; returns ErrRunnerProfileNotFound when the
// row is gone (treated as zero rows affected). Secrets get sealed
// with the cipher before the write — same contract as Insert.
func (s *Store) UpdateRunnerProfile(ctx context.Context, cipher *crypto.Cipher, id uuid.UUID, in RunnerProfileInput) error {
	cfg, err := encodeProfileConfig(in.Config)
	if err != nil {
		return err
	}
	envBytes, err := encodeProfileEnv(in.Env)
	if err != nil {
		return err
	}
	secretsBytes, err := encodeProfileSecrets(cipher, in.Secrets)
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
            env = $14, secrets = $15,
            updated_at = NOW()
        WHERE id = $1
    `, toPgUUID(id),
		in.Name, in.Description, in.Engine,
		in.DefaultImage,
		in.DefaultCPURequest, in.DefaultCPULimit,
		in.DefaultMemRequest, in.DefaultMemLimit,
		in.MaxCPU, in.MaxMem,
		normalizeTags(in.Tags), cfg,
		envBytes, secretsBytes,
	)
	if err != nil {
		return fmt.Errorf("store: update runner profile %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRunnerProfileNotFound
	}
	return nil
}

// ResolveProfileEnvByName is the dispatch path: scheduler asks for a
// profile by name and wants the merged env (plaintext + decrypted
// secrets) ready to drop into a JobAssignment, plus the list of
// secret VALUES so the caller can append them to LogMasks.
//
// Cipher must be non-nil when the profile has any secrets; on a
// decrypt failure (wrong key, tampered ciphertext) we surface the
// error so the dispatch fails closed instead of silently shipping
// garbage env vars to the agent.
//
// Returns ErrRunnerProfileNotFound for unknown names — the
// scheduler turns that into "skip this dispatch, leave the run
// queued so an admin sees the misconfiguration".
func (s *Store) ResolveProfileEnvByName(ctx context.Context, cipher *crypto.Cipher, name string) (env map[string]string, secretValues []string, err error) {
	row, err := s.q.GetRunnerProfileByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrRunnerProfileNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: resolve profile %q: %w", name, err)
	}

	plain, err := decodeProfileEnv(row.Env)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := decodeProfileSecrets(cipher, row.Secrets)
	if err != nil {
		return nil, nil, fmt.Errorf("store: decrypt profile secrets %q: %w", name, err)
	}

	merged := make(map[string]string, len(plain)+len(secrets))
	for k, v := range plain {
		merged[k] = v
	}
	values := make([]string, 0, len(secrets))
	for k, v := range secrets {
		merged[k] = v
		if v != "" {
			values = append(values, v)
		}
	}
	return merged, values, nil
}

// RunnerProfileUsage counts the live dependents of a profile that
// the delete-guard cares about: pipelines whose definition still
// names the profile, and queued/running runs against any such
// pipeline. Either > 0 means a delete is unsafe.
type RunnerProfileUsage struct {
	Pipelines  int
	ActiveRuns int
}

// CountRunnerProfileUsage returns the live dependent counts in one
// round-trip. Callers (today: the admin delete handler) use it to
// surface a unified error explaining what blocks the delete — N
// pipelines still reference the profile, M runs are still in flight.
//
// Pipelines is queried directly via jsonb_path_exists. ActiveRuns
// joins runs to those pipelines and filters status IN ('queued',
// 'running'), capturing both rewire-then-delete races and
// in-flight dispatches.
func (s *Store) CountRunnerProfileUsage(ctx context.Context, name string) (RunnerProfileUsage, error) {
	var u RunnerProfileUsage
	err := s.pool.QueryRow(ctx, `
        WITH refs AS (
            SELECT id FROM pipelines
            WHERE jsonb_path_exists(
                definition,
                '$.Jobs[*] ? (@.Profile == $name)',
                jsonb_build_object('name', $1::text)
            )
        )
        SELECT
            (SELECT COUNT(*) FROM refs)::INT AS pipelines,
            (SELECT COUNT(*) FROM runs r
                WHERE r.status IN ('queued', 'running')
                  AND r.pipeline_id IN (SELECT id FROM refs))::INT AS active_runs
    `, name).Scan(&u.Pipelines, &u.ActiveRuns)
	if err != nil {
		return RunnerProfileUsage{}, fmt.Errorf("store: count profile usage %q: %w", name, err)
	}
	return u, nil
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
	env, err := decodeProfileEnv(r.Env)
	if err != nil {
		return RunnerProfile{}, err
	}
	keys, err := decodeProfileSecretKeys(r.Secrets)
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
		Env:               env,
		SecretKeys:        keys,
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

// encodeProfileEnv marshals the plain env map. nil/empty becomes
// "{}" so the JSONB column stays a valid object.
func encodeProfileEnv(env map[string]string) ([]byte, error) {
	if len(env) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("store: marshal profile env: %w", err)
	}
	return b, nil
}

func decodeProfileEnv(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("store: unmarshal profile env: %w", err)
	}
	return out, nil
}

// encodeProfileSecrets seals each value with the cipher and stores
// the result hex-encoded inside a JSONB object so reads can pull
// individual keys without unmarshaling the whole bag. Empty/nil
// input → "{}", no cipher needed (same fast path as project secrets).
func encodeProfileSecrets(cipher *crypto.Cipher, in map[string]string) ([]byte, error) {
	if len(in) == 0 {
		return []byte("{}"), nil
	}
	if cipher == nil {
		return nil, errors.New("store: profile secrets: cipher not configured")
	}
	enc := make(map[string]string, len(in))
	for k, v := range in {
		ct, err := cipher.Encrypt([]byte(v))
		if err != nil {
			return nil, fmt.Errorf("store: encrypt profile secret %q: %w", k, err)
		}
		enc[k] = hex.EncodeToString(ct)
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return nil, fmt.Errorf("store: marshal profile secrets: %w", err)
	}
	return b, nil
}

// decodeProfileSecrets reverses encodeProfileSecrets, decrypting
// every value. Used on the dispatch path where the scheduler needs
// the actual secret strings to inject into the assignment env.
func decodeProfileSecrets(cipher *crypto.Cipher, raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	enc := map[string]string{}
	if err := json.Unmarshal(raw, &enc); err != nil {
		return nil, fmt.Errorf("store: unmarshal profile secrets: %w", err)
	}
	if len(enc) == 0 {
		return map[string]string{}, nil
	}
	if cipher == nil {
		return nil, errors.New("store: profile secrets: cipher not configured")
	}
	out := make(map[string]string, len(enc))
	for k, v := range enc {
		ct, err := hex.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("store: hex-decode profile secret %q: %w", k, err)
		}
		plain, err := cipher.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt profile secret %q: %w", k, err)
		}
		out[k] = string(plain)
	}
	return out, nil
}

// decodeProfileSecretKeys returns just the configured secret names,
// sorted, without touching the cipher. Used on the read/list path
// for the admin UI — the values stay encrypted at rest and never
// leave the server's process memory through this surface.
func decodeProfileSecretKeys(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	enc := map[string]string{}
	if err := json.Unmarshal(raw, &enc); err != nil {
		return nil, fmt.Errorf("store: unmarshal profile secret keys: %w", err)
	}
	keys := make([]string, 0, len(enc))
	for k := range enc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
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
