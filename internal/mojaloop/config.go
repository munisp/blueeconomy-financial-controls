package mojaloop

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Rail operating modes for MOJALOOP_MODE. The mode is mandatory so the
// adapter's posture is always explicit, never assumed.
const (
	// ModeReceiveOnly durably handles inbound signed transfer callbacks only;
	// the outbound quote/transfer leg is not implemented and quote callbacks
	// are authenticated, then answered with a truthful 501 problem document.
	ModeReceiveOnly = "receive-only"
	// ModeFull is reserved for the outbound quote -> transfer leg. It is
	// parsed but the adapter refuses to start with it until the outbound leg
	// is implemented (fail-closed, never a silent no-op).
	ModeFull = "full"
)

type Config struct {
	BaseURL                    *url.URL
	Source                     string
	Destination                string
	Mode                       string
	SigningKeyFile             string
	SigningKeyID               string
	VerificationKeyFile        string
	VerificationKeyID          string
	SignatureAlgorithm         string
	CallbackBaseURL            *url.URL
	CallbackTransferPathPrefix string
	CallbackQuotePathPrefix    string
	CABundleFile               string
	RequestTimeout             time.Duration
}

func LoadConfig() (Config, error) {
	return LoadConfigFrom(os.Getenv)
}

func LoadConfigFrom(getenv func(string) string) (Config, error) {
	baseURL, err := requiredHTTPSURL(getenv, "MOJALOOP_FSPIOP_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	callbackURL, err := requiredHTTPSURL(getenv, "MOJALOOP_CALLBACK_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	callbackTransferPathPrefix, err := callbackResourcePathPrefix(callbackURL, "transfers")
	if err != nil {
		return Config{}, err
	}
	callbackQuotePathPrefix, err := callbackResourcePathPrefix(callbackURL, "quotes")
	if err != nil {
		return Config{}, err
	}
	config := Config{
		BaseURL:                    baseURL,
		Source:                     getenv("MOJALOOP_FSPIOP_SOURCE"),
		Destination:                getenv("MOJALOOP_FSPIOP_DESTINATION"),
		Mode:                       getenv("MOJALOOP_MODE"),
		SigningKeyFile:             strings.TrimSpace(getenv("MOJALOOP_SIGNING_KEY_FILE")),
		SigningKeyID:               getenv("MOJALOOP_SIGNING_KID"),
		VerificationKeyFile:        strings.TrimSpace(getenv("MOJALOOP_VERIFICATION_KEY_FILE")),
		VerificationKeyID:          getenv("MOJALOOP_VERIFICATION_KID"),
		SignatureAlgorithm:         strings.TrimSpace(getenv("MOJALOOP_SIGNATURE_ALGORITHM")),
		CallbackBaseURL:            callbackURL,
		CallbackTransferPathPrefix: callbackTransferPathPrefix,
		CallbackQuotePathPrefix:    callbackQuotePathPrefix,
		CABundleFile:               strings.TrimSpace(getenv("MOJALOOP_CA_BUNDLE_FILE")),
	}
	if config.Mode != ModeReceiveOnly && config.Mode != ModeFull {
		return Config{}, fmt.Errorf("MOJALOOP_MODE is required and must be %q or %q", ModeReceiveOnly, ModeFull)
	}
	if config.Source == "" || config.Destination == "" || config.SigningKeyFile == "" || config.SigningKeyID == "" || config.CABundleFile == "" {
		return Config{}, errors.New("Mojaloop source, destination, signing key file, signing kid and CA bundle file are required")
	}
	for name, value := range map[string]string{"MOJALOOP_FSPIOP_SOURCE": config.Source, "MOJALOOP_FSPIOP_DESTINATION": config.Destination, "MOJALOOP_SIGNING_KID": config.SigningKeyID} {
		if err := canonicalReference(name, value); err != nil {
			return Config{}, err
		}
	}
	if config.VerificationKeyID != "" {
		if err := canonicalReference("MOJALOOP_VERIFICATION_KID", config.VerificationKeyID); err != nil {
			return Config{}, err
		}
	}
	if (config.VerificationKeyFile == "") != (config.VerificationKeyID == "") {
		return Config{}, errors.New("MOJALOOP_VERIFICATION_KEY_FILE and MOJALOOP_VERIFICATION_KID must be configured together")
	}
	if config.Source == config.Destination {
		return Config{}, errors.New("MOJALOOP_FSPIOP_SOURCE and MOJALOOP_FSPIOP_DESTINATION must identify distinct participants")
	}
	if config.SignatureAlgorithm != "RS256" && config.SignatureAlgorithm != "RS384" && config.SignatureAlgorithm != "RS512" {
		return Config{}, fmt.Errorf("unsupported Mojaloop signature algorithm %q", config.SignatureAlgorithm)
	}
	timeoutText := strings.TrimSpace(getenv("MOJALOOP_REQUEST_TIMEOUT"))
	if timeoutText == "" {
		config.RequestTimeout = 10 * time.Second
	} else {
		config.RequestTimeout, err = time.ParseDuration(timeoutText)
		if err != nil || config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute {
			return Config{}, errors.New("MOJALOOP_REQUEST_TIMEOUT must be between 1s and 2m")
		}
	}
	return config, nil
}

func canonicalReference(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
		return fmt.Errorf("%s must be canonical text of at most 256 bytes", name)
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return fmt.Errorf("%s must not contain whitespace or control characters", name)
		}
	}
	return nil
}

func requiredHTTPSURL(getenv func(string) string, name string) (*url.URL, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("%s must be an HTTPS URL without userinfo", name)
	}
	return parsed, nil
}

// callbackResourcePathPrefix derives the callback route prefix for one FSPIOP
// resource (transfers or quotes) from the approved callback base URL.
func callbackResourcePathPrefix(callbackURL *url.URL, resource string) (string, error) {
	if callbackURL.RawQuery != "" || callbackURL.Fragment != "" {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL must not contain query or fragment")
	}
	base := strings.TrimSuffix(callbackURL.EscapedPath(), "/")
	if base == "." || base == "/." || strings.Contains(base, "..") {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL must not contain traversal")
	}
	if base == "" || base == "/" {
		return "/" + resource + "/", nil
	}
	cleaned := path.Clean(base)
	if !strings.HasPrefix(cleaned, "/") || cleaned == "." || cleaned == "/" {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL path is invalid")
	}
	return cleaned + "/" + resource + "/", nil
}

func ValidateCABundle(pemBytes []byte) error {
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(pemBytes); !ok {
		return errors.New("CA bundle contains no valid certificates")
	}
	return nil
}

func ParseBool(getenv func(string) string, name string, defaultValue bool) (bool, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be boolean", name)
	}
	return parsed, nil
}
