package grpcsrv_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const heartbeatSecs = 30

func TestRegister_UnknownAgent(t *testing.T) {
	_, client := bootServer(t)

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "ghost",
		Token:   "whatever",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", code)
	}
}

func TestRegister_WrongToken(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-01", store.HashToken("right"))

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-01",
		Token:   "wrong",
	})
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", code)
	}
}

func TestRegister_Succeeds(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-01", store.HashToken("tok"))

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId:  "runner-01",
		Token:    "tok",
		Version:  "0.1.0",
		Os:       "linux",
		Arch:     "amd64",
		Tags:     []string{"docker"},
		Capacity: 4,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session_id")
	}
	if resp.HeartbeatSeconds != heartbeatSecs {
		t.Fatalf("heartbeat = %d, want %d", resp.HeartbeatSeconds, heartbeatSecs)
	}

	s := store.New(pool)
	a, err := s.FindAgentByName(context.Background(), "runner-01")
	if err != nil {
		t.Fatalf("lookup agent: %v", err)
	}
	if a.Status != "online" {
		t.Fatalf("status = %s, want online", a.Status)
	}
	if a.Version != "0.1.0" || a.OS != "linux" || a.Arch != "amd64" || a.Capacity != 4 {
		t.Fatalf("metadata not persisted: %+v", a)
	}
	if time.Since(a.LastSeenAt) > 5*time.Second {
		t.Fatalf("last_seen_at not refreshed: %v", a.LastSeenAt)
	}
}

func TestRegister_AutoRegister_CreatesRowOnFirstHit(t *testing.T) {
	pool, client := bootServerWithAutoRegister(t, "shared-token")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId:  "agent-0",
		Token:    "shared-token",
		Tags:     []string{"linux"},
		Capacity: 2,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session_id")
	}

	// Row must exist post-Register, with the token hashed in place
	// so subsequent registers validate against it instead of the
	// shared registration token.
	s := store.New(pool)
	a, err := s.FindAgentByName(context.Background(), "agent-0")
	if err != nil {
		t.Fatalf("auto-registered row missing: %v", err)
	}
	if !store.VerifyToken("shared-token", a.TokenHash) {
		t.Fatalf("token hash not stored or wrong")
	}
	if a.Status != "online" {
		t.Fatalf("status = %s, want online", a.Status)
	}
}

func TestRegister_AutoRegister_RefusesWrongToken(t *testing.T) {
	_, client := bootServerWithAutoRegister(t, "shared-token")

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-0",
		Token:   "different-token",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound (auto-register must not accept wrong token)", code)
	}
}

func TestRegister_AutoRegister_OffByDefault(t *testing.T) {
	// bootServer (no auto-register token) — same as before, unknown
	// agent must be rejected with NotFound regardless of token.
	_, client := bootServer(t)
	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-0",
		Token:   "anything",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", code)
	}
}

func TestRegister_InvalidArgs(t *testing.T) {
	_, client := bootServer(t)

	tests := []struct {
		name string
		req  *gocdnextv1.RegisterRequest
	}{
		{"missing agent_id", &gocdnextv1.RegisterRequest{Token: "t"}},
		{"missing token", &gocdnextv1.RegisterRequest{AgentId: "a"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.Register(context.Background(), tt.req)
			if code := status.Code(err); code != codes.InvalidArgument {
				t.Fatalf("code = %s, want InvalidArgument", code)
			}
		})
	}
}

// --- test harness ---

func bootServer(t *testing.T) (*pgxpool.Pool, gocdnextv1.AgentServiceClient) {
	return bootServerWithAutoRegister(t, "")
}

func bootServerWithAutoRegister(t *testing.T, autoRegToken string) (*pgxpool.Pool, gocdnextv1.AgentServiceClient) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	svc := grpcsrv.NewAgentService(s, grpcsrv.NewSessionStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeatSecs).
		WithAutoRegisterToken(autoRegToken)

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcSrv, svc)
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
		_ = lis.Close()
	})
	return pool, gocdnextv1.NewAgentServiceClient(conn)
}

func seedAgentViaSQL(t *testing.T, pool *pgxpool.Pool, name, tokenHash string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2)`,
		name, tokenHash,
	)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}
