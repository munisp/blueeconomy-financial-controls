package riskscore

import (
	"context"
	"errors"
	"strings"

	riskscorev1 "github.com/munisp/blueeconomy-contracts/gen/go/blueeconomy/riskscore/v1"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// grpcService serves blueeconomy.riskscore.v1.RiskScoreService over the same
// deterministic scoring core as the HTTP handler: identical rules, identical
// verdicts.
type grpcService struct {
	riskscorev1.UnimplementedRiskScoreServiceServer
	rules Rules
}

// ScoreDeclaration maps the wire contract onto ScoreRequest, enforces the
// field contract (INVALID_ARGUMENT on any violation) and returns the
// deterministic verdict. Authentication has already happened in the unary
// interceptor; there is no unauthenticated path here.
func (service *grpcService) ScoreDeclaration(_ context.Context, request *riskscorev1.ScoreDeclarationRequest) (*riskscorev1.ScoreDeclarationResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "scoring request is required")
	}
	scoreRequest := ScoreRequest{
		DeclarationRef:       request.GetDeclarationRef(),
		DeclarationType:      request.GetDeclarationType(),
		HSCode:               request.GetHsCode(),
		GoodsDescription:     request.GetGoodsDescription(),
		CountryOfOrigin:      request.GetCountryOfOrigin(),
		CountryOfDestination: request.GetCountryOfDestination(),
		PortOfEntry:          request.GetPortOfEntry(),
		GrossWeightKg:        request.GetGrossWeightKg(),
		NumberOfPackages:     int(request.GetNumberOfPackages()),
		InvoiceAmountMinor:   request.GetInvoiceAmountMinor(),
		InvoiceCurrency:      request.GetInvoiceCurrency(),
		ConsigneeID:          request.GetConsigneeId(),
		OperatorID:           request.GetOperatorId(),
		TraderID:             request.GetTraderId(),
		IsAEO:                request.GetIsAeo(),
	}
	if err := scoreRequest.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	verdict := service.rules.Score(scoreRequest)
	return &riskscorev1.ScoreDeclarationResponse{
		Score:        int32(verdict.Score),
		ModelVersion: verdict.ModelVersion,
		RuleBased:    verdict.RuleBased,
		Sanctioned:   verdict.Sanctioned,
		Reasons:      verdict.Reasons,
	}, nil
}

// publicGRPCMethods are reachable without a bearer token: the standard
// health-check and server-reflection endpoints only. Every other method —
// above all ScoreDeclaration — requires a verified Keycloak RS256 token.
func publicGRPCMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

// unaryAuthInterceptor enforces the same Keycloak RS256 authentication as
// the HTTP handler by reusing the Authenticator against the authorization
// RPC metadata. Any gap — absent metadata, malformed header, unverifiable
// token — is UNAUTHENTICATED; the interceptor fails closed.
func unaryAuthInterceptor(auth Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if publicGRPCMethod(info.FullMethod) {
			return handler(ctx, request)
		}
		authorization := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			values := md.Get("authorization")
			if len(values) > 0 {
				authorization = values[0]
			}
		}
		if err := auth.Authenticate(ctx, authorization); err != nil {
			return nil, status.Error(codes.Unauthenticated, "bearer token is absent or unverifiable")
		}
		return handler(ctx, request)
	}
}

// NewGRPCServer builds the gRPC server for RiskScoreService with the health
// and reflection endpoints registered (public per the contract). It fails
// closed exactly like NewHandler: invalid rules or a missing authenticator
// are construction errors — there is no unauthenticated scoring path. The
// pipeline may be nil (telemetry disabled): every RPC then still gets a
// (non-recording) span over a noop provider. When a pipeline is supplied, the
// otelgrpc stats handler joins every scoring RPC span to the caller trace via
// the gRPC metadata carrier, and the baggage interceptor stamps tenant.id and
// agency onto the server span before authentication runs.
func NewGRPCServer(rules Rules, auth Authenticator, pipeline *telemetry.Telemetry) (*grpc.Server, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("riskscore: an authenticator is required")
	}
	if pipeline == nil {
		pipeline = telemetry.Default()
	}
	server := grpc.NewServer(
		pipeline.GRPCServerOption(),
		grpc.ChainUnaryInterceptor(telemetry.UnaryBaggageAttributesInterceptor(), unaryAuthInterceptor(auth)),
	)
	riskscorev1.RegisterRiskScoreServiceServer(server, &grpcService{rules: rules})
	healthServer := health.NewServer()
	healthServer.SetServingStatus(riskscorev1.RiskScoreService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)
	reflection.Register(server)
	return server, nil
}
