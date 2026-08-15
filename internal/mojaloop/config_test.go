package mojaloop

import (
	"strings"
	"testing"
)

func validMojaloopEnvironment() map[string]string {
	return map[string]string{
		"MOJALOOP_FSPIOP_BASE_URL":     "https://switch.example.invalid/fspiop",
		"MOJALOOP_CALLBACK_BASE_URL":   "https://fmmbe.example.invalid/callbacks",
		"MOJALOOP_FSPIOP_SOURCE":       "FMMBE",
		"MOJALOOP_FSPIOP_DESTINATION":  "SWITCH",
		"MOJALOOP_SIGNING_KEY_FILE":    "/run/secrets/mojaloop-signing-key.pem",
		"MOJALOOP_SIGNING_KID":         "fmmbe-2026-01",
		"MOJALOOP_SIGNATURE_ALGORITHM": "RS256",
		"MOJALOOP_CA_BUNDLE_FILE":      "/run/secrets/mojaloop-ca.pem",
		"MOJALOOP_REQUEST_TIMEOUT":     "15s",
	}
}

func TestLoadConfigRequiresApprovedTrustInputs(t *testing.T) {
	environment := validMojaloopEnvironment()
	config, err := LoadConfigFrom(func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseURL.String() != "https://switch.example.invalid/fspiop" || config.RequestTimeout.String() != "15s" {
		t.Fatalf("unexpected config: %+v", config)
	}
	for name, value := range map[string]string{"MOJALOOP_FSPIOP_BASE_URL": "http://switch.invalid", "MOJALOOP_SIGNATURE_ALGORITHM": "none", "MOJALOOP_REQUEST_TIMEOUT": "300ms"} {
		invalid := validMojaloopEnvironment()
		invalid[name] = value
		if _, err := LoadConfigFrom(func(key string) string { return invalid[key] }); err == nil {
			t.Fatalf("%s=%q accepted", name, value)
		}
	}
}

func TestValidateCABundleRequiresCertificate(t *testing.T) {
	if err := ValidateCABundle([]byte("not-a-certificate")); err == nil {
		t.Fatal("invalid CA bundle accepted")
	}
	if err := ValidateCABundle([]byte(strings.TrimSpace(`-----BEGIN CERTIFICATE-----
invalid
-----END CERTIFICATE-----`))); err == nil {
		t.Fatal("malformed CA certificate accepted")
	}
}

func TestLoadConfigRejectsNonCanonicalParticipantOrKID(t *testing.T) {
	for name, value := range map[string]string{
		"MOJALOOP_FSPIOP_SOURCE":      "FMMBE OPS",
		"MOJALOOP_FSPIOP_DESTINATION": "SWITCH\n",
		"MOJALOOP_SIGNING_KID":        "kid with spaces",
	} {
		environment := validMojaloopEnvironment()
		environment[name] = value
		if _, err := LoadConfigFrom(func(key string) string { return environment[key] }); err == nil {
			t.Fatalf("%s=%q accepted", name, value)
		}
	}
	environment := validMojaloopEnvironment()
	environment["MOJALOOP_FSPIOP_DESTINATION"] = environment["MOJALOOP_FSPIOP_SOURCE"]
	if _, err := LoadConfigFrom(func(key string) string { return environment[key] }); err == nil {
		t.Fatal("identical source and destination accepted")
	}
}

func TestLoadConfigBindsCallbackBasePathToTransferProfile(t *testing.T) {
	environment := validMojaloopEnvironment()
	config, err := LoadConfigFrom(func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.CallbackTransferPathPrefix != "/callbacks/transfers/" {
		t.Fatalf("unexpected callback transfer path prefix %q", config.CallbackTransferPathPrefix)
	}
	for _, callbackURL := range []string{
		"https://fmmbe.example.invalid/callbacks?environment=sandbox",
		"https://fmmbe.example.invalid/callbacks#fragment",
		"https://fmmbe.example.invalid/callbacks/../other",
	} {
		invalid := validMojaloopEnvironment()
		invalid["MOJALOOP_CALLBACK_BASE_URL"] = callbackURL
		if _, err := LoadConfigFrom(func(name string) string { return invalid[name] }); err == nil {
			t.Fatalf("callback base URL %q accepted", callbackURL)
		}
	}
}
