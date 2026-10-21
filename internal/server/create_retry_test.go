package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/traumventures/e2b-minimal/internal/pb/orchestrator"
)

type busyClient struct {
	orchestrator.SandboxServiceClient
	busy  int
	calls int
	err   error
}

func (c *busyClient) Create(context.Context, *orchestrator.SandboxCreateRequest, ...grpc.CallOption) (*orchestrator.SandboxCreateResponse, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	if c.calls <= c.busy {
		return nil, status.Error(codes.ResourceExhausted, "too many sandboxes starting on this node, please retry")
	}
	return &orchestrator.SandboxCreateResponse{ClientId: "node"}, nil
}

func TestCreateWaitsForAStartSlot(t *testing.T) {
	c := &busyClient{busy: 3}
	resp, err := createRetrying(context.Background(), c, &orchestrator.SandboxCreateRequest{}, 5*time.Second)
	if err != nil || resp.GetClientId() != "node" || c.calls != 4 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, c.calls)
	}
}

func TestCreateGivesUpAfterTheWait(t *testing.T) {
	c := &busyClient{busy: 1000}
	_, err := createRetrying(context.Background(), c, &orchestrator.SandboxCreateRequest{}, 300*time.Millisecond)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err=%v", err)
	}
}

func TestCreateReturnsOtherErrorsAtOnce(t *testing.T) {
	c := &busyClient{err: status.Error(codes.InvalidArgument, "bad")}
	_, err := createRetrying(context.Background(), c, &orchestrator.SandboxCreateRequest{}, 5*time.Second)
	if status.Code(err) != codes.InvalidArgument || c.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, c.calls)
	}
}
