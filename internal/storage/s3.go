package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config configures an S3 compatible object store.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	PathStyle       bool
	HTTPClient      *http.Client
	Now             func() time.Time
}

// S3 is an S3 compatible object store client implemented with the Go
// standard library only. It signs requests with AWS Signature Version 4.
type S3 struct {
	endpoint        *url.URL
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	pathStyle       bool
	httpClient      *http.Client
	now             func() time.Time
}

// NewS3 validates the configuration and returns a client.
func NewS3(config S3Config) (*S3, error) {
	if config.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return nil, fmt.Errorf("S3 access key id and secret access key are required")
	}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.Endpoint == "" {
		config.Endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", config.Region)
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse S3 endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("S3 endpoint must use http or https")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &S3{
		endpoint:        endpoint,
		region:          config.Region,
		bucket:          config.Bucket,
		accessKeyID:     config.AccessKeyID,
		secretAccessKey: config.SecretAccessKey,
		sessionToken:    config.SessionToken,
		pathStyle:       config.PathStyle,
		httpClient:      config.HTTPClient,
		now:             config.Now,
	}, nil
}

// Backend implements ObjectStore.
func (s *S3) Backend() string { return "s3" }

// Put implements ObjectStore.
func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	sum := sha256.Sum256(data)
	return s.send(ctx, http.MethodPut, s.objectURL(key), map[string]string{
		"content-type": contentType,
	}, hex.EncodeToString(sum[:]), bytes.NewReader(data), int64(len(data)))
}

// PutFile implements ObjectStore.
func (s *S3) PutFile(ctx context.Context, key, path, contentType string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return s.send(ctx, http.MethodPut, s.objectURL(key), map[string]string{
		"content-type": contentType,
	}, hex.EncodeToString(hasher.Sum(nil)), file, info.Size())
}

// Get implements ObjectStore.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	var buffer bytes.Buffer
	if err := s.GetTo(ctx, key, &buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// GetTo implements ObjectStore.
func (s *S3) GetTo(ctx context.Context, key string, w io.Writer) error {
	response, err := s.do(ctx, http.MethodGet, s.objectURL(key), nil, emptyPayloadHash(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return s.statusError(response)
	}
	_, err = io.Copy(w, response.Body)
	return err
}

// Delete implements ObjectStore.
func (s *S3) Delete(ctx context.Context, key string) error {
	response, err := s.do(ctx, http.MethodDelete, s.objectURL(key), nil, emptyPayloadHash(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return s.statusError(response)
	}
	return nil
}

// Exists implements ObjectStore.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	response, err := s.do(ctx, http.MethodHead, s.objectURL(key), nil, emptyPayloadHash(), nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return false, s.statusError(response)
	}
	return true, nil
}

// List implements ObjectStore.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	continuation := ""
	for {
		target := s.bucketURL()
		query := url.Values{}
		query.Set("list-type", "2")
		query.Set("prefix", prefix)
		if continuation != "" {
			query.Set("continuation-token", continuation)
		}
		target.RawQuery = canonicalQuery(query)

		response, err := s.do(ctx, http.MethodGet, target, nil, emptyPayloadHash(), nil, 0)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode > 299 {
			statusErr := s.statusError(response)
			response.Body.Close()
			return nil, statusErr
		}
		var result listBucketResult
		decodeErr := xml.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode list response: %w", decodeErr)
		}
		for _, item := range result.Contents {
			keys = append(keys, item.Key)
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		continuation = result.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

// Presign implements ObjectStore by generating a SigV4 query signed URL.
func (s *S3) Presign(ctx context.Context, method, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("presign ttl must be positive")
	}
	if ttl > 7*24*time.Hour {
		return "", fmt.Errorf("presign ttl must not exceed 7 days")
	}
	target := s.objectURL(key)
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := strings.Join([]string{dateStamp, s.region, "s3", "aws4_request"}, "/")

	query := url.Values{}
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", s.accessKeyID+"/"+scope)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")
	if s.sessionToken != "" {
		query.Set("X-Amz-Security-Token", s.sessionToken)
	}
	target.RawQuery = canonicalQuery(query)

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(target),
		canonicalQuery(target.Query()),
		"host:" + canonicalHost(target) + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(dateStamp), stringToSign))
	target.RawQuery = canonicalQuery(query) + "&X-Amz-Signature=" + signature
	return target.String(), nil
}

// objectURL builds the URL for an object key.
func (s *S3) objectURL(key string) *url.URL {
	target := *s.endpoint
	if s.pathStyle {
		target.Path = strings.TrimRight(target.Path, "/") + "/" + s.bucket + "/" + escapePathSegments(key)
		return &target
	}
	target.Host = s.bucket + "." + target.Host
	target.Path = strings.TrimRight(target.Path, "/") + "/" + escapePathSegments(key)
	return &target
}

// bucketURL builds the URL used for listing the bucket.
func (s *S3) bucketURL() *url.URL {
	target := *s.endpoint
	if s.pathStyle {
		target.Path = strings.TrimRight(target.Path, "/") + "/" + s.bucket
		return &target
	}
	target.Host = s.bucket + "." + target.Host
	return &target
}

// send signs and performs a request, returning the response.
func (s *S3) do(ctx context.Context, method string, target *url.URL, headers map[string]string, payloadHash string, body io.Reader, contentLength int64) (*http.Response, error) {
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := strings.Join([]string{dateStamp, s.region, "s3", "aws4_request"}, "/")

	signed := map[string]string{}
	for name, value := range headers {
		signed[strings.ToLower(name)] = strings.TrimSpace(value)
	}
	signed["host"] = canonicalHost(target)
	signed["x-amz-content-sha256"] = payloadHash
	signed["x-amz-date"] = amzDate
	if s.sessionToken != "" {
		signed["x-amz-security-token"] = s.sessionToken
	}

	names := make([]string, 0, len(signed))
	for name := range signed {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(signed[name])
		canonicalHeaders.WriteString("\n")
	}

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(target),
		canonicalQuery(target.Query()),
		canonicalHeaders.String(),
		strings.Join(names, ";"),
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(dateStamp), stringToSign))

	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for name, value := range signed {
		if name == "host" {
			continue
		}
		request.Header.Set(name, value)
	}
	request.ContentLength = contentLength
	request.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKeyID, scope, strings.Join(names, ";"), signature))

	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

// send performs a request and discards the body.
func (s *S3) send(ctx context.Context, method string, target *url.URL, headers map[string]string, payloadHash string, body io.Reader, contentLength int64) error {
	response, err := s.do(ctx, method, target, headers, payloadHash, body, contentLength)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return s.statusError(response)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

// statusError converts a non 2xx response into an error.
func (s *S3) statusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return fmt.Errorf("s3 %s %s: %s", response.Request.Method, response.Request.URL, strings.TrimSpace(string(body)))
}

// signingKey derives the SigV4 signing key for a date.
func (s *S3) signingKey(dateStamp string) []byte {
	key := hmacSHA256([]byte("AWS4"+s.secretAccessKey), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, "s3")
	return hmacSHA256(key, "aws4_request")
}

// listBucketResult models a ListObjectsV2 response.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// canonicalHost returns the normalized host header value.
func canonicalHost(target *url.URL) string {
	host := target.Host
	host = strings.ToLower(host)
	if target.Scheme == "https" && strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	if target.Scheme == "http" && strings.HasSuffix(host, ":80") {
		host = strings.TrimSuffix(host, ":80")
	}
	return host
}

// canonicalURI returns the canonical path component of a request.
func canonicalURI(target *url.URL) string {
	path := target.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery encodes query parameters per the SigV4 rules.
func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		list := append([]string(nil), values[key]...)
		sort.Strings(list)
		for _, value := range list {
			parts = append(parts, uriEncode(key, true)+"="+uriEncode(value, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent encodes a string per RFC 3986.
func uriEncode(value string, encodeSlash bool) string {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteByte(char)
		case char == '-', char == '_', char == '.', char == '~':
			builder.WriteByte(char)
		case char == '/' && !encodeSlash:
			builder.WriteByte('/')
		default:
			builder.WriteString(fmt.Sprintf("%%%02X", char))
		}
	}
	return builder.String()
}

// escapePathSegments escapes each path segment of an object key.
func escapePathSegments(key string) string {
	parts := strings.Split(key, "/")
	for index, part := range parts {
		parts[index] = uriEncode(part, true)
	}
	return strings.Join(parts, "/")
}

func emptyPayloadHash() string {
	return hex.EncodeToString(sha256Sum(nil))
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// ensure the errors package stays referenced for future use in this file.
var _ = errors.Is
