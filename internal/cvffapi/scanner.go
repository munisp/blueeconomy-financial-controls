package cvffapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Scan verdicts.
var (
	// ErrDocumentInfected marks content the scanner rejected. It maps to a
	// 422 problem and the document is never persisted.
	ErrDocumentInfected = errors.New("the document was rejected by the malware scanner")
	// ErrScannerUnavailable marks a scanner that could not reach a verdict.
	// Uploads fail closed with 503; nothing is silently skipped.
	ErrScannerUnavailable = errors.New("the malware scanner did not return a verdict")
)

// Scanner is the malware-scan hook every upload passes before persistence.
// The hook is mandatory: a handler without a scanner refuses to start.
type Scanner interface {
	// Scan returns nil for clean content, ErrDocumentInfected for rejected
	// content and ErrScannerUnavailable when no verdict was reached.
	Scan(ctx context.Context, fileName string, content []byte) error
}

// httpScanner posts the candidate bytes to the deployment-provided scan
// endpoint (a clamd REST bridge or equivalent gateway-side ICAP facade).
// HTTP 200 is the only clean verdict; 4xx rejects the content, anything else
// is treated as unavailable.
type httpScanner struct {
	url        string
	httpClient *http.Client
}

// NewHTTPScanner fails closed without an explicit, canonical HTTPS (or gated
// cluster-local HTTP) endpoint.
func NewHTTPScanner(rawURL string) (Scanner, error) {
	if strings.TrimSpace(rawURL) != rawURL || rawURL == "" {
		return nil, errors.New("CVFF_API_AVSCAN_URL is required; uploads are rejected without a configured malware scanner")
	}
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, errors.New("CVFF_API_AVSCAN_URL must use the https (or cluster-local http) scheme")
	}
	return &httpScanner{
		url: rawURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}, nil
}

func (scanner *httpScanner) Scan(ctx context.Context, fileName string, content []byte) error {
	if len(content) == 0 {
		return fmt.Errorf("%w: empty content", ErrScannerUnavailable)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, scanner.url, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("%w: build scan request", ErrScannerUnavailable)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Content-Name", fileName)
	response, err := scanner.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrScannerUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	switch {
	case response.StatusCode == http.StatusOK:
		return nil
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return ErrDocumentInfected
	default:
		return fmt.Errorf("%w: scan endpoint returned HTTP %d", ErrScannerUnavailable, response.StatusCode)
	}
}
