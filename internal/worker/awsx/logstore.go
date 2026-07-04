package awsx

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3API is the subset of the S3 client the log store uses.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3LogStore implements worker.LogStore by writing each job's whole raw log
// to raw/{repo}/{run_id}/{job_id}.log in one bucket. Retention is the
// bucket's lifecycle rule (24h default, provisioned by JUS-92) — the worker
// only ever writes.
type S3LogStore struct {
	client S3API
	bucket string
}

// NewS3LogStore returns a store writing to bucket.
func NewS3LogStore(client S3API, bucket string) *S3LogStore {
	return &S3LogStore{client: client, bucket: bucket}
}

// PutRawLog stores one job's log and returns its s3:// URL.
func (s *S3LogStore) PutRawLog(ctx context.Context, repo string, runID, jobID int64, log string) (string, error) {
	key := fmt.Sprintf("raw/%s/%d/%d.log", repo, runID, jobID)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(log),
		ContentType: aws.String("text/plain; charset=utf-8"),
	})
	if err != nil {
		return "", fmt.Errorf("store raw log %s: %w", key, err)
	}
	return "s3://" + s.bucket + "/" + key, nil
}
