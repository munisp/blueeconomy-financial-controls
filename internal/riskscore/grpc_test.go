package riskscore

import (
	"context"
	"net"
	"testing"
	"time"

	riskscorev1 "github.com/munisp/blueeconomy-contracts/gen/go/blueeconomy/riskscore/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// startGRPCFixture serves the RiskScoreService in-process over loopback TCP
// with the test JWKS authenticator and returns a client plus the token
// factory. Transport is plaintext inside the test process only; the
// production authentication path (Keycloak RS256) is fully exercised.
type grpcFixture struct {
	client riskscorev1.RiskScoreServiceClient
	health healthpb.HealthClient
	jwks   *jwksFixture
	rules  Rules
}

func startGRPCFixture(t *testing.T) *grpcFixture {
	t.Helper()
	jwks := newJWKSFixture(t)
	rules := testRules()
	server, err := NewGRPCServer(rules, jwks.authenticator(t), nil)
	if err != nil {
		t.Fatalf("build gRPC server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &grpcFixture{
		client: riskscorev1.NewRiskScoreServiceClient(conn),
		health: healthpb.NewHealthClient(conn),
		jwks:   jwks,
		rules:  rules,
	}
}

func (fixture *grpcFixture) ctx(t *testing.T, mutate func(*keycloakClaims)) context.Context {
	t.Helper()
	token := fixture.jwks.token(t, mutate)
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func grpcRequest() *riskscorev1.ScoreDeclarationRequest {
	return &riskscorev1.ScoreDeclarationRequest{
		DeclarationRef:     "NCS-2026-ABC123",
		DeclarationType:    "IMPORT",
		HsCode:             "870324",
		GoodsDescription:   "Used motor vehicles for transport",
		CountryOfOrigin:    "DE",
		PortOfEntry:        "APAPA",
		GrossWeightKg:      12000,
		NumberOfPackages:   4,
		InvoiceAmountMinor: 500000000,
		InvoiceCurrency:    "NGN",
		ConsigneeId:        "consignee-dangote-01",
		OperatorId:         "operator-apapa-01",
		TraderId:           "trader-01",
	}
}

func TestGRPCScoreDeclarationMatchesScoringCore(t *testing.T) {
	fixture := startGRPCFixture(t)
	response, err := fixture.client.ScoreDeclaration(fixture.ctx(t, nil), grpcRequest())
	if err != nil {
		t.Fatalf("score declaration: %v", err)
	}
	expected := fixture.rules.Score(ScoreRequest{
		DeclarationRef:     "NCS-2026-ABC123",
		DeclarationType:    "IMPORT",
		HSCode:             "870324",
		GoodsDescription:   "Used motor vehicles for transport",
		CountryOfOrigin:    "DE",
		PortOfEntry:        "APAPA",
		GrossWeightKg:      12000,
		NumberOfPackages:   4,
		InvoiceAmountMinor: 500000000,
		InvoiceCurrency:    "NGN",
		ConsigneeID:        "consignee-dangote-01",
		OperatorID:         "operator-apapa-01",
		TraderID:           "trader-01",
	})
	if int(response.GetScore()) != expected.Score {
		t.Fatalf("score %d, scoring core says %d", response.GetScore(), expected.Score)
	}
	if response.GetModelVersion() != expected.ModelVersion || !response.GetRuleBased() {
		t.Fatalf("dishonest verdict metadata: %+v", response)
	}
	if response.GetSanctioned() != expected.Sanctioned {
		t.Fatalf("sanctioned %v, scoring core says %v", response.GetSanctioned(), expected.Sanctioned)
	}
	if len(response.GetReasons()) != len(expected.Reasons) {
		t.Fatalf("reasons %v, scoring core says %v", response.GetReasons(), expected.Reasons)
	}
}

func TestGRPCScoreDeclarationUnauthenticated(t *testing.T) {
	fixture := startGRPCFixture(t)
	cases := map[string]context.Context{
		"no authorization metadata": context.Background(),
		"garbage token": metadata.AppendToOutgoingContext(context.Background(),
			"authorization", "Bearer not-a-jwt"),
		"wrong audience": fixture.ctx(t, func(claims *keycloakClaims) {
			claims.Audience = []string{"some-other-api"}
			claims.AuthorizedParty = ""
		}),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fixture.client.ScoreDeclaration(ctx, grpcRequest())
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("%s: code %v, want Unauthenticated (err=%v)", name, status.Code(err), err)
			}
		})
	}
}

func TestGRPCScoreDeclarationInvalidArgument(t *testing.T) {
	fixture := startGRPCFixture(t)
	request := grpcRequest()
	request.HsCode = "NOT-DIGITS"
	_, err := fixture.client.ScoreDeclaration(fixture.ctx(t, nil), request)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestGRPCHealthEndpointIsPublic(t *testing.T) {
	fixture := startGRPCFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := fixture.health.Check(ctx, &healthpb.HealthCheckRequest{
		Service: riskscorev1.RiskScoreService_ServiceDesc.ServiceName,
	})
	if err != nil {
		t.Fatalf("health check without a token must be public: %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health status %v, want SERVING", response.GetStatus())
	}
}

func TestNewGRPCServerFailsClosed(t *testing.T) {
	if _, err := NewGRPCServer(testRules(), nil, nil); err == nil {
		t.Fatal("gRPC server accepted a nil authenticator")
	}
	if _, err := NewGRPCServer(Rules{}, allowAll{}, nil); err == nil {
		t.Fatal("gRPC server accepted unloaded rules")
	}
}
