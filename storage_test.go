package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPresignUsesPublicEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	cfg := &Config{S3Bucket: "photos", S3Region: "us-east-1", S3PathStyle: true,
		S3Endpoint: "http://minio:9000", S3PublicEndpoint: "https://s3.example.com"}
	st, err := newS3Store(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.PresignGet(context.Background(), "photos/abc.jpg", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://s3.example.com/photos/photos/abc.jpg?") {
		t.Errorf("presigned URL = %s", u)
	}

	cfg.S3PublicEndpoint = ""
	st, _ = newS3Store(context.Background(), cfg)
	u, _ = st.PresignGet(context.Background(), "photos/abc.jpg", time.Hour)
	if !strings.HasPrefix(u, "http://minio:9000/photos/") {
		t.Errorf("without public endpoint, presigned URL = %s", u)
	}
}
