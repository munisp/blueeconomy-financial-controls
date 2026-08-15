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
)

type Client struct {
	BaseURL            *url.URL
	HTTPClient         *http.Client
	Source             string
	Destination        string
	SigningKey         *rsa.PrivateKey
	SignatureAlgorithm string
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
	signature, err := SignRequest(method, request.URL.RequestURI(), client.Source, client.Destination, body, client.SigningKey, client.SignatureAlgorithm)
	if err != nil {
		return nil, err
	}
	AddSignatureHeaders(request, signature, client.Source, client.Destination)
	return request, nil
}

func (client Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	request, err := client.NewSignedRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return httpClient.Do(request)
}

func (client Client) PostQuotes(ctx context.Context, quoteID string, body []byte) (*http.Response, error) {
	return client.Do(ctx, http.MethodPost, "/quotes/"+quoteID, body)
}

func (client Client) PostTransfers(ctx context.Context, body []byte) (*http.Response, error) {
	return client.Do(ctx, http.MethodPost, "/transfers", body)
}

func (client Client) GetTransfer(ctx context.Context, transferID string) (*http.Response, error) {
	return client.Do(ctx, http.MethodGet, "/transfers/"+transferID, nil)
}
