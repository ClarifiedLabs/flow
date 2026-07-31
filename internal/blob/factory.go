package blob

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	BackendLocal = "local"
	BackendS3    = "s3"
)

// FactoryConfig contains only non-secret storage settings. S3 credentials are
// always resolved by the AWS SDK default credential chain and are never copied
// into Flow configuration or coordinator databases.
type FactoryConfig struct {
	Backend       string
	LocalPath     string
	MaxRangeBytes int64
	S3            FactoryS3Config
}

type FactoryS3Config struct {
	Region, Bucket, Prefix, EndpointURL string
	PathStyle, AllowHTTP, BucketKey     bool
	Encryption                          S3Encryption
	KMSKeyID                            string
}

// Open constructs a blob store. Stores returned by the current local and S3
// backends own no background goroutines or open long-lived handles and therefore
// need no Close call.
func Open(ctx context.Context, config FactoryConfig) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(config.Backend)) {
	case BackendLocal:
		if strings.TrimSpace(config.LocalPath) == "" {
			return nil, fmt.Errorf("%w: local path is required", ErrInvalidConfig)
		}
		if factoryS3Configured(config.S3) {
			return nil, fmt.Errorf("%w: S3 settings cannot be used by the local backend", ErrInvalidConfig)
		}
		return NewLocal(config.LocalPath, LocalOptions{MaxRangeBytes: config.MaxRangeBytes})
	case BackendS3:
		return openS3(ctx, config)
	default:
		return nil, fmt.Errorf("%w: backend must be local or s3", ErrInvalidConfig)
	}
}

func openS3(ctx context.Context, config FactoryConfig) (Store, error) {
	s3Config := config.S3
	if strings.TrimSpace(config.LocalPath) != "" || strings.TrimSpace(s3Config.Region) == "" || strings.TrimSpace(s3Config.Bucket) == "" {
		return nil, fmt.Errorf("%w: S3 region and bucket are required and local path must be empty", ErrInvalidConfig)
	}
	if err := validateFactoryEndpoint(s3Config.EndpointURL, s3Config.AllowHTTP); err != nil {
		return nil, err
	}
	if s3Config.Encryption == S3EncryptionKMS && strings.TrimSpace(s3Config.KMSKeyID) == "" {
		return nil, fmt.Errorf("%w: KMS key is required for aws:kms encryption", ErrInvalidConfig)
	}
	if s3Config.Encryption != S3EncryptionKMS && (strings.TrimSpace(s3Config.KMSKeyID) != "" || s3Config.BucketKey) {
		return nil, fmt.Errorf("%w: KMS key and bucket key require aws:kms encryption", ErrInvalidConfig)
	}

	// LoadDefaultConfig deliberately receives no static credential provider.
	// Environment, shared config, workload identity, and instance/task roles are
	// resolved by the standard SDK chain when a request is signed.
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(strings.TrimSpace(s3Config.Region)))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for history blobs: %w", err)
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.UsePathStyle = s3Config.PathStyle
		if strings.TrimSpace(s3Config.EndpointURL) != "" {
			options.BaseEndpoint = &s3Config.EndpointURL
		}
	})
	return NewS3(S3Options{
		Client: client, Bucket: strings.TrimSpace(s3Config.Bucket), Prefix: s3Config.Prefix,
		EndpointURL: s3Config.EndpointURL, AllowHTTP: s3Config.AllowHTTP,
		Encryption: s3Config.Encryption, KMSKeyID: strings.TrimSpace(s3Config.KMSKeyID),
		BucketKey: s3Config.BucketKey, MaxRangeBytes: config.MaxRangeBytes,
	})
}

func validateFactoryEndpoint(value string, allowHTTP bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if allowHTTP {
			return fmt.Errorf("%w: allow-http requires an explicit endpoint", ErrInsecureEndpoint)
		}
		return nil
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return fmt.Errorf("%w: custom S3 endpoint must be an origin URL", ErrInvalidConfig)
	}
	if endpoint.Scheme != "https" && !(allowHTTP && endpoint.Scheme == "http") {
		return fmt.Errorf("%w: custom S3 endpoints require HTTPS", ErrInsecureEndpoint)
	}
	return nil
}

func factoryS3Configured(config FactoryS3Config) bool {
	return config.Region != "" || config.Bucket != "" || config.Prefix != "" || config.EndpointURL != "" || config.PathStyle || config.AllowHTTP || config.BucketKey || config.Encryption != "" || config.KMSKeyID != ""
}
