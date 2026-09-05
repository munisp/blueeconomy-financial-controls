package cvffapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// s3Store PUTs objects to an S3-compatible endpoint (AWS S3, MinIO, Ceph or
// the GCS S3-interoperability endpoint) signed with AWS Signature Version 4.
// Coordinates come from the environment; credentials come from the standard
// AWS_* chain and are never embedded in URIs.
type s3Store struct {
	bucket     string
	region     string
	endpoint   string // empty for AWS S3 (regional endpoint derived)
	secure     bool
	accessKey  string
	secretKey  string
	httpClient *http.Client
	now        func() time.Time
}

func newS3Store(lookup func(string) string) (BlobStore, error) {
	bucket, err := requireEnv(lookup, envS3Bucket)
	if err != nil {
		return nil, err
	}
	if err := validateS3BucketName(bucket); err != nil {
		return nil, err
	}
	region, err := requireEnv(lookup, envS3Region)
	if err != nil {
		return nil, err
	}
	if !s3RegionPattern.MatchString(region) {
		return nil, storageConfigError("%s must be a valid region identifier (for example us-east-1)", envS3Region)
	}
	secureRaw, err := requireEnv(lookup, envS3Secure)
	if err != nil {
		return nil, err
	}
	if secureRaw != "true" && secureRaw != "false" {
		return nil, storageConfigError("%s must be exactly 'true' or 'false'", envS3Secure)
	}
	secure := secureRaw == "true"
	endpoint := ""
	if raw := lookup(envS3EndpointURL); raw != "" {
		if strings.TrimSpace(raw) != raw {
			return nil, storageConfigError("%s must be canonical", envS3EndpointURL)
		}
		parsed, parseErr := url.Parse(raw)
		expectedScheme := "https"
		if !secure {
			expectedScheme = "http"
		}
		if parseErr != nil || parsed.Scheme != expectedScheme {
			return nil, storageConfigError("%s must use the %s:// scheme when %s=%s", envS3EndpointURL, expectedScheme, envS3Secure, secureRaw)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, storageConfigError("%s must not contain credentials, query parameters or fragments", envS3EndpointURL)
		}
		if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
			return nil, storageConfigError("%s must not contain a path", envS3EndpointURL)
		}
		if parsed.Hostname() == "" {
			return nil, storageConfigError("%s must name a valid host", envS3EndpointURL)
		}
		endpoint = strings.TrimSuffix(raw, "/")
	} else if !secure {
		return nil, storageConfigError("%s=false is only permitted against an explicit %s (for example a MinIO deployment); AWS S3 transport is always TLS", envS3Secure, envS3EndpointURL)
	}
	accessKey, err := requireEnv(lookup, envAWSAccessKey)
	if err != nil {
		return nil, err
	}
	secretKey, err := requireEnv(lookup, envAWSSecretKey)
	if err != nil {
		return nil, err
	}
	return &s3Store{
		bucket:    bucket,
		region:    region,
		endpoint:  endpoint,
		secure:    secure,
		accessKey: accessKey,
		secretKey: secretKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		now: time.Now,
	}, nil
}

func (store *s3Store) Backend() string { return backendS3 }

// objectURL resolves the virtual-host style object URL. For custom endpoints
// the bucket is addressed in the path (MinIO/Ceph convention).
func (store *s3Store) objectURL(key string) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	escaped := s3EscapeKey(key)
	if store.endpoint != "" {
		return store.endpoint + "/" + store.bucket + "/" + escaped, nil
	}
	return "https://" + store.bucket + ".s3." + store.region + ".amazonaws.com/" + escaped, nil
}

// s3EscapeKey percent-encodes each key segment per RFC 3986, as SigV4
// requires the same encoding in the canonical URI and the request URI.
func s3EscapeKey(key string) string {
	segments := strings.Split(key, "/")
	for index, segment := range segments {
		segments[index] = s3Escape(segment)
	}
	return strings.Join(segments, "/")
}

func s3Escape(value string) string {
	var builder strings.Builder
	const upperHex = "0123456789ABCDEF"
	for index := 0; index < len(value); index++ {
		octet := value[index]
		if octet >= 'A' && octet <= 'Z' || octet >= 'a' && octet <= 'z' || octet >= '0' && octet <= '9' ||
			octet == '-' || octet == '_' || octet == '.' || octet == '~' {
			builder.WriteByte(octet)
		} else {
			builder.WriteByte('%')
			builder.WriteByte(upperHex[octet>>4])
			builder.WriteByte(upperHex[octet&0x0F])
		}
	}
	return builder.String()
}

func (store *s3Store) Put(ctx context.Context, key string, contentType string, content io.Reader, sizeBytes int64) error {
	if content == nil {
		return errors.New("content is required")
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("read content for S3 put: %w", err)
	}
	if sizeBytes >= 0 && int64(len(body)) != sizeBytes {
		return fmt.Errorf("content length %d does not match declared size %d", len(body), sizeBytes)
	}
	objectURL, err := store.objectURL(key)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, objectURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("build S3 request: %w", err)
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", contentType)
	store.signV4(request, body)
	response, err := store.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("put S3 object: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("S3 put returned HTTP %d", response.StatusCode)
	}
	return nil
}

// Get streams one object. Absent keys fail with ErrObjectNotFound; the
// caller closes the returned body.
func (store *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	objectURL, err := store.objectURL(key)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build S3 read request: %w", err)
	}
	store.signV4(request, nil)
	response, err := store.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get S3 object: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("S3 object %q: %w", key, ErrObjectNotFound)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("S3 get returned HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

// signV4 applies AWS Signature Version 4 with the payload hash in the
// canonical headers; keys in this service are already content-addressed.
func (store *s3Store) signV4(request *http.Request, payload []byte) {
	now := store.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256.Sum256(payload)
	payloadHashHex := hex.EncodeToString(payloadHash[:])
	request.Header.Set("X-Amz-Date", amzDate)
	request.Header.Set("X-Amz-Content-Sha256", payloadHashHex)

	signedHeaderNames := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}
	canonicalHeaders := "content-type:" + strings.TrimSpace(request.Header.Get("Content-Type")) + "\n" +
		"host:" + request.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHashHex + "\n" +
		"x-amz-date:" + amzDate + "\n"
	canonicalRequest := strings.Join([]string{
		request.Method,
		request.URL.EscapedPath(),
		"",
		canonicalHeaders,
		strings.Join(signedHeaderNames, ";"),
		payloadHashHex,
	}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	credentialScope := dateStamp + "/" + store.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hex.EncodeToString(canonicalHash[:])

	sign := func(key []byte, value string) []byte {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(value))
		return mac.Sum(nil)
	}
	derived := sign([]byte("AWS4"+store.secretKey), dateStamp)
	derived = sign(derived, store.region)
	derived = sign(derived, "s3")
	derived = sign(derived, "aws4_request")
	signature := hex.EncodeToString(sign(derived, stringToSign))

	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+store.accessKey+"/"+credentialScope+
		", SignedHeaders="+strings.Join(signedHeaderNames, ";")+", Signature="+signature)
}
