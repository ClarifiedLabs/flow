package blob

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestFactoryOpensPrivateLocalStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "history")
	store, err := Open(context.Background(), FactoryConfig{Backend: BackendLocal, LocalPath: root, MaxRangeBytes: 1024})
	if err != nil {
		t.Fatalf("Open local: %v", err)
	}
	if _, ok := store.(*Local); !ok {
		t.Fatalf("store type = %T, want *Local", store)
	}
	upload, err := store.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin local upload: %v", err)
	}
	if _, err := upload.Write([]byte("factory")); err != nil {
		t.Fatalf("write local upload: %v", err)
	}
	if _, err := upload.Complete(context.Background()); err != nil {
		t.Fatalf("complete local upload: %v", err)
	}
}

func TestFactoryRejectsUnsafeS3ConfigurationBeforeCredentialResolution(t *testing.T) {
	tests := []struct {
		name string
		cfg  FactoryConfig
		want error
	}{
		{name: "plaintext endpoint", cfg: FactoryConfig{Backend: BackendS3, S3: FactoryS3Config{Region: "us-east-1", Bucket: "private", EndpointURL: "http://minio:9000"}}, want: ErrInsecureEndpoint},
		{name: "endpoint credentials", cfg: FactoryConfig{Backend: BackendS3, S3: FactoryS3Config{Region: "us-east-1", Bucket: "private", EndpointURL: "https://user:pass@example.test"}}, want: ErrInvalidConfig},
		{name: "kms without key", cfg: FactoryConfig{Backend: BackendS3, S3: FactoryS3Config{Region: "us-east-1", Bucket: "private", Encryption: S3EncryptionKMS}}, want: ErrInvalidConfig},
		{name: "local with s3 settings", cfg: FactoryConfig{Backend: BackendLocal, LocalPath: t.TempDir(), S3: FactoryS3Config{Region: "us-east-1"}}, want: ErrInvalidConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Open(context.Background(), test.cfg)
			if !errors.Is(err, test.want) {
				t.Fatalf("Open error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestFactoryBuildsS3WithDefaultCredentialChain(t *testing.T) {
	store, err := Open(context.Background(), FactoryConfig{
		Backend: BackendS3,
		S3: FactoryS3Config{
			Region: "us-east-1", Bucket: "private", EndpointURL: "http://127.0.0.1:9000",
			AllowHTTP: true, PathStyle: true, Encryption: S3EncryptionAES256,
		},
	})
	if err != nil {
		t.Fatalf("Open S3: %v", err)
	}
	if _, ok := store.(*S3Store); !ok {
		t.Fatalf("store type = %T, want *S3Store", store)
	}
}
