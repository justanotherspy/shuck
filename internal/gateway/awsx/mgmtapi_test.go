package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi/types"

	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// fakeMgmt is a scriptable ManagementAPI.
type fakeMgmt struct {
	postErr   error
	deleteErr error
	posts     []*apigatewaymanagementapi.PostToConnectionInput
	deletes   []*apigatewaymanagementapi.DeleteConnectionInput
}

func (f *fakeMgmt) PostToConnection(_ context.Context, in *apigatewaymanagementapi.PostToConnectionInput, _ ...func(*apigatewaymanagementapi.Options)) (*apigatewaymanagementapi.PostToConnectionOutput, error) {
	f.posts = append(f.posts, in)
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &apigatewaymanagementapi.PostToConnectionOutput{}, nil
}

func (f *fakeMgmt) DeleteConnection(_ context.Context, in *apigatewaymanagementapi.DeleteConnectionInput, _ ...func(*apigatewaymanagementapi.Options)) (*apigatewaymanagementapi.DeleteConnectionOutput, error) {
	f.deletes = append(f.deletes, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &apigatewaymanagementapi.DeleteConnectionOutput{}, nil
}

func TestConnAPIPost(t *testing.T) {
	fake := &fakeMgmt{}
	if err := NewAPIGWConnAPI(fake).Post(context.Background(), "conn-1", []byte("frame")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if len(fake.posts) != 1 || *fake.posts[0].ConnectionId != "conn-1" || string(fake.posts[0].Data) != "frame" {
		t.Fatalf("posts = %+v", fake.posts)
	}
}

func TestConnAPIPostGoneMapsToErrGone(t *testing.T) {
	fake := &fakeMgmt{postErr: &types.GoneException{}}
	err := NewAPIGWConnAPI(fake).Post(context.Background(), "conn-1", []byte("frame"))
	if !errors.Is(err, serverless.ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestConnAPIPostOtherError(t *testing.T) {
	fake := &fakeMgmt{postErr: errors.New("throttled")}
	err := NewAPIGWConnAPI(fake).Post(context.Background(), "conn-1", []byte("frame"))
	if err == nil || errors.Is(err, serverless.ErrGone) {
		t.Fatalf("err = %v, want a non-Gone failure", err)
	}
}

func TestConnAPIClose(t *testing.T) {
	fake := &fakeMgmt{}
	if err := NewAPIGWConnAPI(fake).Close(context.Background(), "conn-1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(fake.deletes) != 1 || *fake.deletes[0].ConnectionId != "conn-1" {
		t.Fatalf("deletes = %+v", fake.deletes)
	}
}

func TestConnAPICloseGoneIsSuccess(t *testing.T) {
	fake := &fakeMgmt{deleteErr: &types.GoneException{}}
	if err := NewAPIGWConnAPI(fake).Close(context.Background(), "conn-1"); err != nil {
		t.Fatalf("close of a gone connection = %v, want nil", err)
	}
}

func TestConnAPICloseOtherError(t *testing.T) {
	fake := &fakeMgmt{deleteErr: errors.New("throttled")}
	if err := NewAPIGWConnAPI(fake).Close(context.Background(), "conn-1"); err == nil {
		t.Fatal("close with failing delete returned nil error")
	}
}
