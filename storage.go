package main

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Store is the subset of object storage the app needs; it is an interface so
// handlers can be tested without S3.
type Store interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
	Delete(ctx context.Context, keys ...string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

type S3Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// newS3Store uses the standard AWS credential chain (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY env vars, ~/.aws, instance roles, ...).
func newS3Store(ctx context.Context, c *Config) (*S3Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.S3Region))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if c.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(c.S3Endpoint)
		}
		o.UsePathStyle = c.S3PathStyle
	})
	return &S3Store{client: client, presign: s3.NewPresignClient(client), bucket: c.S3Bucket}, nil
}

func (s *S3Store) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   &contentType,
	})
	return err
}

func (s *S3Store) Delete(ctx context.Context, keys ...string) error {
	var errs []error
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: aws.String(k)}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// EnsureLifecycle installs a bucket rule that expires objects under prefix
// after 30 days. It is a safety net behind the in-app reaper; failure (e.g.
// missing permission or a provider without lifecycle support) is non-fatal.
func (s *S3Store) EnsureLifecycle(ctx context.Context, prefix string) error {
	days := int32(photoTTL/(24*time.Hour)) + 1
	_, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: &s.bucket,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{{
				ID:         aws.String("simple-photos-expire"),
				Status:     types.ExpirationStatusEnabled,
				Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(prefix)},
				Expiration: &types.LifecycleExpiration{Days: aws.Int32(days)},
			}},
		},
	})
	return err
}
