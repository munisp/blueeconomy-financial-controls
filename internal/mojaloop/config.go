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

type Config struct {
	BaseURL                    *url.URL
	Source                     string
	Destination                string
	SigningKeyFile             string
	SigningKeyID               string
	VerificationKeyFile        string
	VerificationKeyID          string
	SignatureAlgorithm         string
	CallbackBaseURL            *url.URL
	CallbackTransferPathPrefix string
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
	callbackTransferPathPrefix, err := callbackTransferPathPrefix(callbackURL)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		BaseURL:                    baseURL,
		Source:                     getenv("MOJALOOP_FSPIOP_SOURCE"),
		Destination:                getenv("MOJALOOP_FSPIOP_DESTINATION"),
		SigningKeyFile:             strings.TrimSpace(getenv("MOJALOOP_SIGNING_KEY_FILE")),
		SigningKeyID:               getenv("MOJALOOP_SIGNING_KID"),
		VerificationKeyFile:        strings.TrimSpace(getenv("MOJALOOP_VERIFICATION_KEY_FILE")),
		VerificationKeyID:          getenv("MOJALOOP_VERIFICATION_KID"),
		SignatureAlgorithm:         strings.TrimSpace(getenv("MOJALOOP_SIGNATURE_ALGORITHM")),
		CallbackBaseURL:            callbackURL,
		CallbackTransferPathPrefix: callbackTransferPathPrefix,
		CABundleFile:               strings.TrimSpace(getenv("MOJALOOP_CA_BUNDLE_FILE")),
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

func callbackTransferPathPrefix(callbackURL *url.URL) (string, error) {
	if callbackURL.RawQuery != "" || callbackURL.Fragment != "" {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL must not contain query or fragment")
	}
	base := strings.TrimSuffix(callbackURL.EscapedPath(), "/")
	if base == "." || base == "/." || strings.Contains(base, "..") {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL must not contain traversal")
	}
	if base == "" || base == "/" {
		return "/transfers/", nil
	}
	cleaned := path.Clean(base)
	if !strings.HasPrefix(cleaned, "/") || cleaned == "." || cleaned == "/" {
		return "", errors.New("MOJALOOP_CALLBACK_BASE_URL path is invalid")
	}
	return cleaned + "/transfers/", nil
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
