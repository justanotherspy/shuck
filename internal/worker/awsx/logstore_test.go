package awsx

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 scripts the narrowed S3 client and records the last put.
type fakeS3 struct {
	err  error
	last *s3.PutObjectInput
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.last = in
	if f.err != nil {
		return nil, f.err
	}
	return &s3.PutObjectOutput{}, nil
}

func TestS3LogStorePutRawLog(t *testing.T) {
	f := &fakeS3{}
	store := NewS3LogStore(f, "shuck-logs")

	url, err := store.PutRawLog(context.Background(), "o/r", 99, 7, "##[error]FAIL\n")
	if err != nil {
		t.Fatalf("PutRawLog: %v", err)
	}
	if url != "s3://shuck-logs/raw/o/r/99/7.log" {
		t.Errorf("url = %q", url)
	}
	if got := aws.ToString(f.last.Bucket); got != "shuck-logs" {
		t.Errorf("bucket = %q", got)
	}
	if got := aws.ToString(f.last.Key); got != "raw/o/r/99/7.log" {
		t.Errorf("key = %q", got)
	}
	body, err := io.ReadAll(f.last.Body)
	if err != nil || string(body) != "##[error]FAIL\n" {
		t.Errorf("body = %q, %v", body, err)
	}
}

func TestS3LogStorePutError(t *testing.T) {
	store := NewS3LogStore(&fakeS3{err: errors.New("denied")}, "b")
	if _, err := store.PutRawLog(context.Background(), "o/r", 1, 2, "x"); err == nil {
		t.Fatal("want error")
	}
}
