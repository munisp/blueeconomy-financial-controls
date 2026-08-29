package mojaloop

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Money-path retry policy defaults. Transient 5xx responses and network
// errors are retried with exponential backoff; 4xx responses are never
// retried.
const (
	defaultMaxAttempts  = 3
	defaultRetryBackoff = 200 * time.Millisecond
	maxRetryBackoff     = 2 * time.Second
)

type Client struct {
	BaseURL            *url.URL
	HTTPClient         *http.Client
	Source             string
	Destination        string
	SigningKey         *rsa.PrivateKey
	SigningKeyID       string
	SignatureAlgorithm string
	// MaxAttempts bounds the total number of attempts per call (initial try
	// plus retries on transient failures); values below 1 use the default.
	MaxAttempts int
	// RetryBackoff is the base delay between attempts; non-positive values
	// use the default. The delay doubles per attempt up to maxRetryBackoff.
	RetryBackoff time.Duration
}

// NewClient builds the fail-closed FSPIOP money-path client from the loaded
// configuration: an explicit *http.Client with the configured request
// timeout (MOJALOOP_REQUEST_TIMEOUT, default 10s), redirects disabled and
// the retry policy above. http.DefaultClient is never used.
func NewClient(config Config, signingKey *rsa.PrivateKey) (Client, error) {
	if config.BaseURL == nil {
		return Client{}, errors.New("Mojaloop base URL is required")
	}
	if config.RequestTimeout <= 0 {
		return Client{}, errors.New("Mojaloop request timeout must be positive")
	}
	if signingKey == nil {
		return Client{}, errors.New("Mojaloop signing key is required")
	}
	return Client{
		BaseURL: config.BaseURL,
		HTTPClient: &http.Client{
			Timeout: config.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Source:             config.Source,
		Destination:        config.Destination,
		SigningKey:         signingKey,
		SigningKeyID:       config.SigningKeyID,
		SignatureAlgorithm: config.SignatureAlgorithm,
		MaxAttempts:        defaultMaxAttempts,
		RetryBackoff:       defaultRetryBackoff,
	}, nil
}

func (client Client) NewSignedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	if client.BaseURL == nil || client.BaseURL.Scheme != "https" {
		return nil, errors.New("Mojaloop base URL must be an explicit HTTPS URL")
	}
	if client.SigningKey == nil {
		return nil, errors.New("Mojaloop signing key is required")
	}
	if client.Source == "" || client.Destination == "" {
		return nil, errors.New("Mojaloop source and destination are required")
	}
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return nil, errors.New("Mojaloop request path must be an absolute relative path without traversal")
	}
	relative, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse Mojaloop path: %w", err)
	}
	target := client.BaseURL.ResolveReference(relative)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), io.NopCloser(strings.NewReader(string(body))))
	if err != nil {
		return nil, fmt.Errorf("create Mojaloop request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	signature, err := SignRequestWithKeyID(method, request.URL.RequestURI(), client.Source, client.Destination, body, client.SigningKey, client.SignatureAlgorithm, client.SigningKeyID)
	if err != nil {
		return nil, err
	}
	AddSignatureHeaders(request, signature, client.Source, client.Destination)
	return request, nil
}

// Do executes one signed FSPIOP call. The HTTP client must be explicit and
// timeout-bound — a nil HTTPClient is an error, never a fall back to
// http.DefaultClient. Transient failures (network errors and 5xx responses)
// are retried with exponential backoff; 4xx responses are returned without
// retry. The caller's context bounds every attempt and every backoff wait.
func (client Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	if client.HTTPClient == nil {
		return nil, errors.New("Mojaloop HTTP client is required; http.DefaultClient is never used on the money path")
	}
	attempts := client.MaxAttempts
	if attempts < 1 {
		attempts = defaultMaxAttempts
	}
	backoff := client.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
			if backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}
		}
		request, err := client.NewSignedRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		response, err := client.HTTPClient.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode >= http.StatusInternalServerError && attempt+1 < attempts {
			// Transient upstream failure: drain and close before retrying.
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			lastErr = fmt.Errorf("Mojaloop answered transient status %d", response.StatusCode)
			continue
		}
		return response, nil
	}
	return nil, fmt.Errorf("Mojaloop request failed after %d attempts: %w", attempts, lastErr)
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (client Client) PostQuotes(ctx context.Context, quoteID string, body []byte) (*http.Response, error) {
	return client.clientSpan(ctx, "mojaloop.quote", "fspiop.quote_id", quoteID, func(ctx context.Context) (*http.Response, error) {
		return client.Do(ctx, http.MethodPost, "/quotes/"+quoteID, body)
	})
}

func (client Client) PostTransfers(ctx context.Context, body []byte) (*http.Response, error) {
	return client.clientSpan(ctx, "mojaloop.transfer", "", "", func(ctx context.Context) (*http.Response, error) {
		return client.Do(ctx, http.MethodPost, "/transfers", body)
	})
}

func (client Client) GetTransfer(ctx context.Context, transferID string) (*http.Response, error) {
	return client.clientSpan(ctx, "mojaloop.transfer.lookup", "fspiop.transfer_id", transferID, func(ctx context.Context) (*http.Response, error) {
		return client.Do(ctx, http.MethodGet, "/transfers/"+transferID, nil)
	})
}
