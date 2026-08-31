package cvffapi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Config is the complete environment-driven configuration for cvff-api.
// Every value is required; a missing value is a startup error.
type Config struct {
	ListenAddr  string
	DatabaseURL string
	Keycloak    KeycloakConfig
	Limits      Limits
	AVScanURL   string
	Temporal    TemporalConfig
	// PolicyDir is the directory of .rego policy files the embedded OPA
	// evaluator loads at startup; an unreadable or empty directory refuses
	// the boot.
	PolicyDir string
}

// TemporalConfig carries the fail-closed coordinates cvff-api uses to start
// CVFFDisbursementWorkflow instances for newly recorded applications.
type TemporalConfig struct {
	HostPort  string
	Namespace string
	TaskQueue string
}

// Environment variable names for the service-level coordinates.
const (
	EnvListenAddr        = "CVFF_API_LISTEN_ADDR"
	EnvDatabaseURL       = "DATABASE_URL"
	EnvKeycloakIssuer    = "CVFF_API_KEYCLOAK_ISSUER"
	EnvKeycloakJWKS      = "CVFF_API_KEYCLOAK_JWKS_URL"
	EnvJWTAudience       = "CVFF_API_JWT_AUDIENCE"
	EnvAVScanURL         = "CVFF_API_AVSCAN_URL"
	EnvMaxDocBytes       = "CVFF_API_MAX_DOCUMENT_BYTES"
	EnvMaxDocsPerApp     = "CVFF_API_MAX_DOCUMENTS_PER_APPLICATION"
	EnvDocContentTypes   = "CVFF_API_DOCUMENT_CONTENT_TYPES"
	EnvTemporalHostPort  = "TEMPORAL_HOST_PORT"
	EnvTemporalNamespace = "TEMPORAL_NAMESPACE"
	EnvTemporalTaskQueue = "TEMPORAL_TASK_QUEUE"
	EnvPolicyDir         = "CVFF_API_POLICY_DIR"
)

// ConfigFromEnv resolves and validates the service configuration, failing
// closed on the first gap. lookup is os.Getenv in production and a map
// accessor in tests.
func ConfigFromEnv(lookup func(string) string) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	var config Config
	var err error
	require := func(name string) string {
		if err != nil {
			return ""
		}
		value := lookup(name)
		if value == "" || strings.TrimSpace(value) != value {
			err = fmt.Errorf("%s is required and must be canonical", name)
			return ""
		}
		return value
	}
	config.ListenAddr = require(EnvListenAddr)
	config.DatabaseURL = require(EnvDatabaseURL)
	config.Keycloak.Issuer = require(EnvKeycloakIssuer)
	config.Keycloak.JWKSURL = require(EnvKeycloakJWKS)
	config.Keycloak.Audience = require(EnvJWTAudience)
	config.AVScanURL = require(EnvAVScanURL)
	config.Temporal.HostPort = require(EnvTemporalHostPort)
	config.Temporal.Namespace = require(EnvTemporalNamespace)
	config.Temporal.TaskQueue = require(EnvTemporalTaskQueue)
	config.PolicyDir = require(EnvPolicyDir)
	if err != nil {
		return Config{}, err
	}
	if err := config.Keycloak.validate(); err != nil {
		return Config{}, err
	}
	maxBytes, parseErr := strconv.ParseInt(require(EnvMaxDocBytes), 10, 64)
	if parseErr != nil {
		return Config{}, fmt.Errorf("%s is not an integer", EnvMaxDocBytes)
	}
	maxDocuments, parseErr := strconv.Atoi(require(EnvMaxDocsPerApp))
	if parseErr != nil {
		return Config{}, fmt.Errorf("%s is not an integer", EnvMaxDocsPerApp)
	}
	rawTypes := require(EnvDocContentTypes)
	if err != nil {
		return Config{}, err
	}
	contentTypes := []string{}
	for _, item := range strings.Split(rawTypes, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return Config{}, fmt.Errorf("%s contains an empty content type", EnvDocContentTypes)
		}
		contentTypes = append(contentTypes, item)
	}
	config.Limits = Limits{
		MaxDocumentBytes:           maxBytes,
		MaxDocumentsPerApplication: maxDocuments,
		DocumentContentTypes:       contentTypes,
	}
	if err := config.Limits.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}
