package cvffapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BlobStore is the platform object-storage boundary for beneficiary
// documents. Implementations are selected exclusively from environment
// configuration; there is no default backend.
type BlobStore interface {
	// Put durably stores content under key, replacing nothing: keys are
	// content-addressed so an identical put is an idempotent no-op.
	Put(ctx context.Context, key string, contentType string, content io.Reader, sizeBytes int64) error
	// Get streams one stored object. Absent keys fail with
	// ErrObjectNotFound; the caller closes the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Backend returns the canonical backend name recorded in metadata rows.
	Backend() string
}

// ErrObjectNotFound marks an absent object key; callers map it to 404
// without leaking whether the key belongs to another tenant.
var ErrObjectNotFound = errors.New("object not found")

// Environment variable names mirror blueeconomy-data-platform storage.py.
const (
	envStorageBackend   = "BLUEECONOMY_STORAGE_BACKEND"
	envAzureCloud       = "BLUEECONOMY_AZURE_CLOUD"
	envStorageAccount   = "BLUEECONOMY_STORAGE_ACCOUNT"
	envStorageKey       = "AZURE_STORAGE_ACCESS_KEY"
	envStorageContainer = "BLUEECONOMY_STORAGE_FILESYSTEM"
	envS3Bucket         = "BLUEECONOMY_S3_BUCKET"
	envS3Region         = "BLUEECONOMY_S3_REGION"
	envS3EndpointURL    = "BLUEECONOMY_S3_ENDPOINT_URL"
	envS3Secure         = "BLUEECONOMY_S3_SECURE"
	envAWSAccessKey     = "AWS_ACCESS_KEY_ID"
	envAWSSecretKey     = "AWS_SECRET_ACCESS_KEY"
	envAllowLocal       = "BLUEECONOMY_ALLOW_LOCAL_STORAGE"
	envLocalRoot        = "BLUEECONOMY_LOCAL_STORAGE_ROOT"
)

const (
	backendADLS      = "adls"
	backendS3        = "s3"
	backendLocal     = "local-gated"
	backendADLSAlias = "adls-gen2"
	backendLocalAka  = "local"
)

// StorageConfigurationError marks absent or invalid storage configuration;
// access fails closed.
type StorageConfigurationError struct{ message string }

func (err *StorageConfigurationError) Error() string { return err.message }

func storageConfigError(format string, args ...any) *StorageConfigurationError {
	return &StorageConfigurationError{message: fmt.Sprintf(format, args...)}
}

func requireEnv(lookup func(string) string, name string) (string, error) {
	value := lookup(name)
	if value == "" || strings.TrimSpace(value) != value {
		return "", storageConfigError("environment variable %s must be set and canonical", name)
	}
	return value, nil
}

var (
	azureAccountPattern    = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	azureFilesystemPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$`)
	s3BucketPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3IPAddressPattern     = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
	s3RegionPattern        = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
)

var azureCloudSuffixes = map[string]string{
	"AzureCloud":        "dfs.core.windows.net",
	"AzureUSGovernment": "dfs.core.usgovcloudapi.net",
}

// NewBlobStore resolves the configured object-storage backend from the
// environment, failing closed on any gap. lookup is os.Getenv in production
// and a map accessor in tests.
func NewBlobStore(ctx context.Context, lookup func(string) string) (BlobStore, error) {
	if lookup == nil {
		return nil, storageConfigError("environment lookup is required")
	}
	raw := lookup(envStorageBackend)
	backend := raw
	switch raw {
	case backendADLSAlias:
		backend = backendADLS
	case backendLocalAka:
		backend = backendLocal
	}
	switch backend {
	case backendADLS:
		return newADLSStore(ctx, lookup)
	case backendS3:
		return newS3Store(lookup)
	case backendLocal:
		return newLocalStore(lookup)
	case "":
		return nil, storageConfigError("%s is not set; refusing to assume a storage backend", envStorageBackend)
	default:
		return nil, storageConfigError("%s=%q is not a supported backend (adls, s3, local-gated)", envStorageBackend, raw)
	}
}

// DocumentStorageKey builds the content-addressed object key for one upload.
// The digest places identical content at an identical key, so a retried
// upload never creates a duplicate object.
func DocumentStorageKey(applicationID string, digest [sha256.Size]byte) string {
	return "cvff-documents/" + applicationID + "/" + hex.EncodeToString(digest[:])
}

// validateObjectKey mirrors the S3 key rules from storage.py so keys stay
// portable across every backend.
func validateObjectKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") {
		return storageConfigError("object keys must be non-empty and must not start with '/'")
	}
	if len(key) > 1024 {
		return storageConfigError("object keys must not exceed 1024 bytes")
	}
	for _, character := range key {
		if character < 0x20 || character == 0x7F {
			return storageConfigError("object keys must not contain control characters")
		}
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return storageConfigError("object keys must not contain empty, '.' or '..' path segments")
		}
	}
	return nil
}

func validateS3BucketName(bucket string) error {
	if !s3BucketPattern.MatchString(bucket) {
		return storageConfigError("S3 bucket names must be 3-63 characters of lowercase letters, digits, dots and hyphens, starting and ending with a letter or digit")
	}
	if s3IPAddressPattern.MatchString(bucket) {
		return storageConfigError("S3 bucket names must not look like an IP address")
	}
	if strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
		return storageConfigError("S3 bucket names must not contain consecutive dots or dot-hyphen adjacency")
	}
	return nil
}

// localGatedStore is the explicitly gated local filesystem backend. It exists
// only behind BLUEECONOMY_ALLOW_LOCAL_STORAGE=true for approved development
// and conformance runs; production deployments use adls or s3.
type localGatedStore struct{ root string }

func newLocalStore(lookup func(string) string) (BlobStore, error) {
	if lookup(envAllowLocal) != "true" {
		return nil, storageConfigError("local storage requires %s=true as an explicit development opt-in", envAllowLocal)
	}
	root, err := requireEnv(lookup, envLocalRoot)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) {
		return nil, storageConfigError("%s must be an absolute path", envLocalRoot)
	}
	return &localGatedStore{root: filepath.Clean(root)}, nil
}

func (store *localGatedStore) Backend() string { return backendLocal }

func (store *localGatedStore) pathFor(key string) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	path := filepath.Join(store.root, filepath.FromSlash(key))
	rootWithSeparator := store.root + string(os.PathSeparator)
	if path != store.root && !strings.HasPrefix(path, rootWithSeparator) {
		return "", storageConfigError("object key escapes the gated local root")
	}
	return path, nil
}

func (store *localGatedStore) Put(ctx context.Context, key string, _ string, content io.Reader, _ int64) error {
	if content == nil {
		return errors.New("content is required")
	}
	path, err := store.pathFor(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create gated storage directory: %w", err)
	}
	// O_EXCL fails closed on a pre-existing object; content-addressed keys
	// make an identical retry a successful no-op below.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("verify existing gated object: %w", readErr)
			}
			incoming, copyErr := io.ReadAll(content)
			if copyErr != nil {
				return fmt.Errorf("read replacement content: %w", copyErr)
			}
			if sha256.Sum256(existing) == sha256.Sum256(incoming) {
				return nil
			}
			return fmt.Errorf("gated object %q already exists with different content", key)
		}
		return fmt.Errorf("create gated object: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(file, content); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write gated object: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("sync gated object: %w", err)
	}
	return nil
}

// Get opens one gated object for reading; absent keys fail with
// ErrObjectNotFound.
func (store *localGatedStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := store.pathFor(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gated object %q: %w", key, ErrObjectNotFound)
		}
		return nil, fmt.Errorf("open gated object: %w", err)
	}
	return file, nil
}
