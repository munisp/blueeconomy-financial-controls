package cvffapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// adlsStore PUTs blobs to Azure Data Lake Storage Gen2 through the DFS REST
// API (create + append + flush) signed with the account shared key. The
// account key comes from the environment only; it is never embedded in URIs.
type adlsStore struct {
	account    string
	filesystem string
	suffix     string
	sharedKey  string
	httpClient *http.Client
	now        func() time.Time
}

func newADLSStore(_ context.Context, lookup func(string) string) (BlobStore, error) {
	cloud, err := requireEnv(lookup, envAzureCloud)
	if err != nil {
		return nil, err
	}
	suffix, ok := azureCloudSuffixes[cloud]
	if !ok {
		return nil, storageConfigError("%s must be one of AzureCloud, AzureUSGovernment; use AzureUSGovernment for the CVFF segregated lakehouse", envAzureCloud)
	}
	account, err := requireEnv(lookup, envStorageAccount)
	if err != nil {
		return nil, err
	}
	if !azureAccountPattern.MatchString(account) {
		return nil, storageConfigError("%s must be a valid storage account name (3-24 lowercase alnum)", envStorageAccount)
	}
	filesystem, err := requireEnv(lookup, envStorageContainer)
	if err != nil {
		return nil, err
	}
	if !azureFilesystemPattern.MatchString(filesystem) {
		return nil, storageConfigError("%s must be a valid ADLS Gen2 filesystem name", envStorageContainer)
	}
	sharedKey, err := requireEnv(lookup, envStorageKey)
	if err != nil {
		return nil, err
	}
	if _, err := base64.StdEncoding.DecodeString(sharedKey); err != nil {
		return nil, storageConfigError("%s must be the base64 account key material", envStorageKey)
	}
	return &adlsStore{
		account:    account,
		filesystem: filesystem,
		suffix:     suffix,
		sharedKey:  sharedKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		now: time.Now,
	}, nil
}

func (store *adlsStore) Backend() string { return backendADLS }

func (store *adlsStore) blobURL(key string, query url.Values) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	segments := strings.Split(key, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	requestURL := "https://" + store.account + "." + store.suffix + "/" + store.filesystem + "/" + strings.Join(segments, "/")
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	return requestURL, nil
}

// Put stores content through the ADLS Gen2 create/append/flush sequence. A
// retried put recreates the same path with identical content, which the
// content-addressed key makes safe.
func (store *adlsStore) Put(ctx context.Context, key string, contentType string, content io.Reader, sizeBytes int64) error {
	if content == nil {
		return errors.New("content is required")
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("read content for ADLS put: %w", err)
	}
	if sizeBytes >= 0 && int64(len(body)) != sizeBytes {
		return fmt.Errorf("content length %d does not match declared size %d", len(body), sizeBytes)
	}
	createURL, err := store.blobURL(key, url.Values{"resource": {"file"}})
	if err != nil {
		return err
	}
	if err := store.do(ctx, http.MethodPut, createURL, nil, ""); err != nil {
		return fmt.Errorf("create ADLS file: %w", err)
	}
	if len(body) > 0 {
		appendURL, err := store.blobURL(key, url.Values{"action": {"append"}, "position": {"0"}})
		if err != nil {
			return err
		}
		if err := store.do(ctx, http.MethodPatch, appendURL, body, contentType); err != nil {
			return fmt.Errorf("append ADLS content: %w", err)
		}
	}
	flushURL, err := store.blobURL(key, url.Values{"action": {"flush"}, "position": {fmt.Sprintf("%d", len(body))}})
	if err != nil {
		return err
	}
	if err := store.do(ctx, http.MethodPatch, flushURL, nil, ""); err != nil {
		return fmt.Errorf("flush ADLS file: %w", err)
	}
	return nil
}

// Get streams one blob through the ADLS Gen2 read path. Absent blobs fail
// with ErrObjectNotFound; the caller closes the returned body.
func (store *adlsStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	blobURL, err := store.blobURL(key, nil)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build ADLS read request: %w", err)
	}
	request.Header.Set("x-ms-date", store.now().UTC().Format(http.TimeFormat))
	request.Header.Set("x-ms-version", "2020-10-02")
	if err := store.sign(request, 0); err != nil {
		return nil, err
	}
	response, err := store.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ADLS read request: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("ADLS object %q: %w", key, ErrObjectNotFound)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("ADLS read returned HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

func (store *adlsStore) do(ctx context.Context, method string, requestURL string, body []byte, contentType string) error {
	request, err := http.NewRequestWithContext(ctx, method, requestURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("build ADLS request: %w", err)
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("x-ms-date", store.now().UTC().Format(http.TimeFormat))
	request.Header.Set("x-ms-version", "2020-10-02")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := store.sign(request, int64(len(body))); err != nil {
		return err
	}
	response, err := store.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("ADLS request: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ADLS %s returned HTTP %d", method, response.StatusCode)
	}
	return nil
}

// sign applies the Azure Storage shared-key authorization scheme.
func (store *adlsStore) sign(request *http.Request, contentLength int64) error {
	keyBytes, err := base64.StdEncoding.DecodeString(store.sharedKey)
	if err != nil {
		return storageConfigError("%s must be the base64 account key material", envStorageKey)
	}
	lengthHeader := ""
	if request.Method == http.MethodPut || request.Method == http.MethodPatch {
		lengthHeader = fmt.Sprintf("%d", contentLength)
	}
	// x-ms-* headers participate sorted, lowercased and trimmed.
	extended := map[string]string{}
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-ms-") {
			extended[lower] = strings.Join(values, ",")
		}
	}
	names := make([]string, 0, len(extended))
	for name := range extended {
		names = append(names, name)
	}
	sort.Strings(names)
	var extendedLines strings.Builder
	for _, name := range names {
		extendedLines.WriteString(name + ":" + strings.TrimSpace(extended[name]) + "\n")
	}
	canonicalizedResource := "/" + store.account + request.URL.EscapedPath()
	query := request.URL.Query()
	queryNames := make([]string, 0, len(query))
	for name := range query {
		queryNames = append(queryNames, strings.ToLower(name))
	}
	sort.Strings(queryNames)
	seen := map[string]bool{}
	for _, name := range queryNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		values := query[name]
		sort.Strings(values)
		canonicalizedResource += "\n" + name + ":" + strings.Join(values, ",")
	}
	stringToSign := strings.Join([]string{
		request.Method,
		"", // Content-Encoding
		"", // Content-Language
		lengthHeader,
		"", // Content-MD5
		request.Header.Get("Content-Type"),
		"", // Date (x-ms-date is authoritative)
		"", // If-Modified-Since
		"", // If-Match
		"", // If-None-Match
		"", // If-Unmodified-Since
		"", // Range
	}, "\n") + "\n" + extendedLines.String() + canonicalizedResource
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(stringToSign))
	request.Header.Set("Authorization", "SharedKey "+store.account+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return nil
}
