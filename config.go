package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	maxUploadBytes = 10 << 20 // 10 MiB per photo
	maxThumbBytes  = 1 << 20  // client-generated thumbnail
	photoTTL       = 30 * 24 * time.Hour
	sessionTTL     = 30 * 24 * time.Hour
)

type Config struct {
	ListenAddr  string
	BaseURL     string // e.g. https://sasoshi.party (no trailing slash)
	DatabaseURL string

	GoogleClientID     string
	GoogleClientSecret string
	AllowedEmails      map[string]bool // empty + empty domains = any Google account
	AllowedDomains     map[string]bool

	S3Bucket   string
	S3Region   string
	S3Endpoint string // optional, for R2 / B2 / MinIO etc.
	// S3PublicEndpoint is the endpoint browsers use to fetch photos via
	// presigned URLs, when it differs from the one the server uses (e.g.
	// MinIO reachable as http://minio:9000 inside Docker but
	// https://s3.sasoshi.party from outside). Defaults to S3Endpoint.
	S3PublicEndpoint string
	S3PathStyle      bool
	S3Prefix         string
	// S3ManageLifecycle makes the app install a 30-day expiry rule on the
	// bucket at startup. This REPLACES any existing lifecycle rules, so only
	// enable it on a bucket dedicated to this app.
	S3ManageLifecycle bool
}

func loadConfig() (*Config, error) {
	c := &Config{
		ListenAddr:         env("LISTEN_ADDR", ":8080"),
		BaseURL:            strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		AllowedEmails:      csvSet(os.Getenv("ALLOWED_EMAILS")),
		AllowedDomains:     csvSet(os.Getenv("ALLOWED_DOMAINS")),
		S3Bucket:           os.Getenv("S3_BUCKET"),
		S3Region:           env("S3_REGION", "us-east-1"),
		S3Endpoint:         os.Getenv("S3_ENDPOINT"),
		S3PublicEndpoint:   os.Getenv("S3_PUBLIC_ENDPOINT"),
		S3PathStyle:        os.Getenv("S3_PATH_STYLE") == "true",
		S3Prefix:           strings.Trim(os.Getenv("S3_PREFIX"), "/"),
		S3ManageLifecycle:  os.Getenv("S3_MANAGE_LIFECYCLE") == "true",
	}
	var missing []string
	for name, v := range map[string]string{
		"DATABASE_URL":         c.DatabaseURL,
		"GOOGLE_CLIENT_ID":     c.GoogleClientID,
		"GOOGLE_CLIENT_SECRET": c.GoogleClientSecret,
		"S3_BUCKET":            c.S3Bucket,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func (c *Config) secureCookies() bool { return strings.HasPrefix(c.BaseURL, "https://") }

func (c *Config) emailAllowed(email string) bool {
	email = strings.ToLower(email)
	if len(c.AllowedEmails) == 0 && len(c.AllowedDomains) == 0 {
		return true
	}
	if c.AllowedEmails[email] {
		return true
	}
	if at := strings.LastIndexByte(email, '@'); at >= 0 && c.AllowedDomains[email[at+1:]] {
		return true
	}
	return false
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func csvSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			m[part] = true
		}
	}
	return m
}
