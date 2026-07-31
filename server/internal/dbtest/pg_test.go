package dbtest

import (
	"strings"
	"testing"
)

// guardOperatorDSN is the only thing standing between a stale `export
// GOCDNEXT_TEST_DSN=...` and TRUNCATE … CASCADE over every table in a real
// database. It is pure (parse + two env reads), so it gets a real unit test
// rather than the manual check it was born with — a data-loss control that is
// only ever verified by hand is one refactor away from silently not working.
func TestGuardOperatorDSN(t *testing.T) {
	const (
		prodDSN    = "postgres://u:p@localhost:5432/gocdnext?sslmode=disable"
		stagingDSN = "postgres://u:p@db.internal:5432/gocdnext_staging?sslmode=disable"
		testDSN    = "postgres://u:p@localhost:5432/gocdnext_test?sslmode=disable"
	)

	tests := []struct {
		name    string
		dsn     string
		allow   string // value of GOCDNEXT_TEST_ALLOW_TRUNCATE; "" = unset
		wantErr string // substring the refusal must explain; "" = must be allowed
	}{
		{
			// The headline case: a production DSN, with the operator having
			// already opted in to truncation for some other database.
			name: "production database refused even with the opt-in set",
			dsn:  prodDSN, allow: "1",
			wantErr: "must end in `_test`",
		},
		{
			// `_staging` is the near-miss that a suffix check must still catch.
			name: "staging database refused",
			dsn:  stagingDSN, allow: "1",
			wantErr: "must end in `_test`",
		},
		{
			// Right database, but nobody has confirmed it is disposable.
			name: "test database refused without the opt-in",
			dsn:  testDSN, allow: "",
			wantErr: "GOCDNEXT_TEST_ALLOW_TRUNCATE",
		},
		{
			// A truthy-looking value is NOT the opt-in: the check is exact so a
			// half-remembered `=true` fails closed rather than wiping a database.
			name: "test database refused when the opt-in is not exactly 1",
			dsn:  testDSN, allow: "true",
			wantErr: "GOCDNEXT_TEST_ALLOW_TRUNCATE",
		},
		{
			name: "both signals present is allowed",
			dsn:  testDSN, allow: "1",
		},
		{
			name: "malformed DSN is refused before anything connects",
			dsn:  "not-a-dsn://", allow: "1",
			wantErr: "not a valid connection string",
		},
		{
			// No database in the URL: libpq would fall back to the OS user's
			// database, which is emphatically not disposable.
			name: "DSN with no database name is refused",
			dsn:  "postgres://u:p@localhost:5432/?sslmode=disable", allow: "1",
			wantErr: "must end in `_test`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv restores the previous value on cleanup, so these cases
			// cannot leak an opt-in into each other (or into the rest of the
			// package, where it would defeat the guard entirely).
			t.Setenv(truncateAllowEnv, tt.allow)

			err := guardOperatorDSN(tt.dsn)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("guardOperatorDSN(%q) = %v, want allowed", tt.dsn, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("guardOperatorDSN(%q) allowed a database it must refuse", tt.dsn)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q so the operator knows what to fix", err, tt.wantErr)
			}
		})
	}
}

// The refusal must NAME the database it refused. An operator who mistypes a DSN
// under load needs to see which database was about to be wiped, not just that
// something was rejected.
func TestGuardOperatorDSN_NamesTheDatabase(t *testing.T) {
	t.Setenv(truncateAllowEnv, "1")
	err := guardOperatorDSN("postgres://u:p@localhost:5432/payments_live?sslmode=disable")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "payments_live") {
		t.Fatalf("error = %q, want it to name the refused database", err)
	}
}
