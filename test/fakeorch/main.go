// Command fakeorch is a stand-in for the orchestrator's gRPC API, for
// checking the REST layer against the official SDKs without KVM. Builds
// finish after two seconds; sandboxes exist only as records, so anything
// that talks to envd (commands, files) does not work against it.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/traumventures/e2b-minimal/internal/pb/orchestrator"
	tm "github.com/traumventures/e2b-minimal/internal/pb/templatemanager"
)

type fake struct {
	orchestrator.UnimplementedSandboxServiceServer
	tm.UnimplementedTemplateServiceServer

	mu        sync.Mutex
	sandboxes map[string]*orchestrator.SandboxCreateRequest
	builds    map[string]time.Time
}

func (f *fake) Create(_ context.Context, r *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := r.GetSandbox()
	if c.GetBuildId() == "" || c.GetEnvdVersion() == "" || c.GetKernelVersion() == "" || c.GetEnvdAccessToken() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "incomplete config: %v", c)
	}
	if _, ok := f.sandboxes[c.GetSandboxId()]; ok {
		return nil, status.Error(codes.AlreadyExists, "sandbox exists")
	}
	f.sandboxes[c.GetSandboxId()] = r
	log.Printf("create %s template=%s build=%s snapshot=%v", c.GetSandboxId(), c.GetTemplateId(), c.GetBuildId(), c.GetSnapshot())

	return &orchestrator.SandboxCreateResponse{ClientId: "fake"}, nil
}

func (f *fake) List(context.Context, *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &orchestrator.SandboxListResponse{}
	for id, r := range f.sandboxes {
		out.Sandboxes = append(out.Sandboxes, &orchestrator.RunningSandbox{SandboxId: id, StartTime: r.GetStartTime(), EndTime: r.GetEndTime()})
	}

	return out, nil
}

func (f *fake) Delete(_ context.Context, r *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[r.GetSandboxId()]; !ok {
		return nil, status.Error(codes.NotFound, "no such sandbox")
	}
	delete(f.sandboxes, r.GetSandboxId())
	log.Printf("delete %s", r.GetSandboxId())

	return &emptypb.Empty{}, nil
}

func (f *fake) Update(_ context.Context, r *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sandboxes[r.GetSandboxId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such sandbox")
	}
	s.EndTime = r.GetEndTime()
	log.Printf("update %s end=%s", r.GetSandboxId(), r.GetEndTime().AsTime())

	return &emptypb.Empty{}, nil
}

func (f *fake) Pause(_ context.Context, r *orchestrator.SandboxPauseRequest) (*orchestrator.SandboxPauseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[r.GetSandboxId()]; !ok {
		return nil, status.Error(codes.NotFound, "no such sandbox")
	}
	delete(f.sandboxes, r.GetSandboxId())
	log.Printf("pause %s -> %s/%s fsOnly=%v", r.GetSandboxId(), r.GetTemplateId(), r.GetBuildId(), r.GetFilesystemOnly())

	return &orchestrator.SandboxPauseResponse{}, nil
}

func (f *fake) TemplateCreate(_ context.Context, r *tm.TemplateCreateRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := r.GetTemplate()
	f.builds[t.GetBuildID()] = time.Now()
	log.Printf("build %s/%s image=%q from=%v steps=%d mem=%d version=%s", t.GetTemplateID(), t.GetBuildID(), t.GetFromImage(), t.GetFromTemplate(), len(t.GetSteps()), t.GetMemoryMB(), r.GetVersion())

	return &emptypb.Empty{}, nil
}

func (f *fake) TemplateBuildStatus(_ context.Context, r *tm.TemplateStatusRequest) (*tm.TemplateBuildStatusResponse, error) {
	f.mu.Lock()
	started, ok := f.builds[r.GetBuildID()]
	f.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "build expired")
	}
	all := []*tm.TemplateBuildLogEntry{
		{Timestamp: timestamppb.New(started), Message: "pulling image", Level: tm.LogLevel_Info},
		{Timestamp: timestamppb.New(started.Add(time.Second)), Message: "building rootfs", Level: tm.LogLevel_Info},
	}
	var logs []*tm.TemplateBuildLogEntry
	for i, e := range all {
		if int32(i) >= r.GetOffset() && time.Now().After(e.GetTimestamp().AsTime()) && (r.Limit == nil || uint32(len(logs)) < r.GetLimit()) {
			logs = append(logs, e)
		}
	}
	resp := &tm.TemplateBuildStatusResponse{Status: tm.TemplateBuildState_Building, LogEntries: logs}
	if time.Since(started) > 2*time.Second {
		resp.Status = tm.TemplateBuildState_Completed
		resp.Metadata = &tm.TemplateBuildMetadata{RootfsSizeKey: 2048, EnvdVersionKey: "0.9.0", KernelVersion: "vmlinux-6.1.177_5008931", FirecrackerVersion: "v1.14-0.2.0"}
	}

	return resp, nil
}

func (f *fake) TemplateBuildDelete(_ context.Context, r *tm.TemplateBuildDeleteRequest) (*emptypb.Empty, error) {
	log.Printf("delete build %s/%s", r.GetTemplateID(), r.GetBuildID())

	return &emptypb.Empty{}, nil
}

func (f *fake) InitLayerFileUpload(_ context.Context, r *tm.InitLayerFileUploadRequest) (*tm.InitLayerFileUploadResponse, error) {
	u := "http://127.0.0.1:5008/upload?path=files/" + r.GetHash() + "&sig=fake"

	return &tm.InitLayerFileUploadResponse{Present: false, Url: &u}, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:15008", "gRPC listen address")
	httpAddr := flag.String("http", "127.0.0.1:15009", "upload listen address")
	flag.Parse()
	go func() {
		log.Fatal(http.ListenAndServe(*httpAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			log.Printf("upload %s %s (%d bytes, sig=%s)", r.Method, r.URL.Path, n, r.URL.Query().Get("sig"))
		})))
	}()
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	f := &fake{sandboxes: map[string]*orchestrator.SandboxCreateRequest{}, builds: map[string]time.Time{}}
	s := grpc.NewServer()
	orchestrator.RegisterSandboxServiceServer(s, f)
	tm.RegisterTemplateServiceServer(s, f)
	log.Printf("fake orchestrator on %s", *addr)
	log.Fatal(s.Serve(lis))
}
