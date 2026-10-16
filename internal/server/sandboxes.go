package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/traumventures/e2b-minimal/internal/pb/orchestrator"
	"github.com/traumventures/e2b-minimal/internal/store"
)

const (
	defaultTimeout = 300 * time.Second
	// syncInterval is how often sandboxes the orchestrator lost (a crash, an
	// OOM kill at SANDBOX_ALL_MEMORY_MAX) are dropped from the store.
	syncInterval = 5 * time.Second
)

type networkBody struct {
	AllowPublicTraffic *bool    `json:"allowPublicTraffic"`
	AllowOut           []string `json:"allowOut"`
	DenyOut            []string `json:"denyOut"`
}

type newSandboxBody struct {
	TemplateID          string            `json:"templateID"`
	Timeout             *int64            `json:"timeout"`
	AutoPause           *bool             `json:"autoPause"`
	AutoPauseMemory     *bool             `json:"autoPauseMemory"`
	AllowInternetAccess *bool             `json:"allow_internet_access"`
	Network             *networkBody      `json:"network"`
	Metadata            map[string]string `json:"metadata"`
	EnvVars             map[string]string `json:"envVars"`
	// Secure comes from the v1 route. Every sandbox here gets an envd
	// access token, so the flag is accepted and ignored.
	Secure *bool `json:"secure"`
}

type sandboxResponse struct {
	TemplateID         string  `json:"templateID"`
	SandboxID          string  `json:"sandboxID"`
	Alias              string  `json:"alias,omitempty"`
	ClientID           string  `json:"clientID"`
	EnvdVersion        string  `json:"envdVersion"`
	EnvdAccessToken    string  `json:"envdAccessToken,omitempty"`
	TrafficAccessToken *string `json:"trafficAccessToken"`
	Domain             *string `json:"domain"`
}

type lifecycle struct {
	AutoResume bool   `json:"autoResume"`
	OnTimeout  string `json:"onTimeout"`
}

type sandboxDetail struct {
	TemplateID          string            `json:"templateID"`
	Alias               string            `json:"alias,omitempty"`
	SandboxID           string            `json:"sandboxID"`
	ClientID            string            `json:"clientID"`
	StartedAt           time.Time         `json:"startedAt"`
	EndAt               time.Time         `json:"endAt"`
	EnvdVersion         string            `json:"envdVersion"`
	EnvdAccessToken     string            `json:"envdAccessToken,omitempty"`
	AllowInternetAccess *bool             `json:"allowInternetAccess,omitempty"`
	Domain              *string           `json:"domain"`
	CPUCount            int64             `json:"cpuCount"`
	MemoryMB            int64             `json:"memoryMB"`
	DiskSizeMB          int64             `json:"diskSizeMB"`
	Metadata            map[string]string `json:"metadata"`
	State               string            `json:"state"`
	Lifecycle           lifecycle         `json:"lifecycle"`
}

func (s *Server) domain() *string {
	if s.cfg.Domain == "" {
		return nil
	}
	d := s.cfg.Domain

	return &d
}

func (s *Server) toResponse(sb *store.Sandbox) sandboxResponse {
	var traffic *string
	if sb.TrafficAccessToken != "" {
		t := sb.TrafficAccessToken
		traffic = &t
	}

	return sandboxResponse{
		TemplateID:         sb.TemplateID,
		SandboxID:          sb.ID,
		Alias:              sb.Alias,
		ClientID:           sb.ClientID,
		EnvdVersion:        sb.EnvdVersion,
		EnvdAccessToken:    sb.EnvdAccessToken,
		TrafficAccessToken: traffic,
		Domain:             s.domain(),
	}
}

func (s *Server) toDetail(sb *store.Sandbox) sandboxDetail {
	onTimeout := "kill"
	if sb.AutoPause {
		onTimeout = "pause"
	}
	md := sb.Metadata
	if md == nil {
		md = map[string]string{}
	}

	return sandboxDetail{
		TemplateID:          sb.TemplateID,
		Alias:               sb.Alias,
		SandboxID:           sb.ID,
		ClientID:            sb.ClientID,
		StartedAt:           sb.StartedAt,
		EndAt:               sb.EndAt,
		EnvdVersion:         sb.EnvdVersion,
		EnvdAccessToken:     sb.EnvdAccessToken,
		AllowInternetAccess: sb.AllowInternetAccess,
		Domain:              s.domain(),
		CPUCount:            sb.CPUCount,
		MemoryMB:            sb.MemoryMB,
		DiskSizeMB:          sb.DiskSizeMB,
		Metadata:            md,
		State:               sb.State,
		Lifecycle:           lifecycle{AutoResume: false, OnTimeout: onTimeout},
	}
}

// parseTimeout validates a timeout in seconds against the configured cap.
func (s *Server) parseTimeout(sec *int64) (time.Duration, error) {
	if sec == nil {
		return defaultTimeout, nil
	}
	if *sec < 0 {
		return 0, errors.New("timeout must not be negative")
	}
	d := time.Duration(*sec) * time.Second
	if max := time.Duration(s.cfg.MaxSandboxHours) * time.Hour; d > max {
		return 0, fmt.Errorf("timeout cannot exceed %d hours", s.cfg.MaxSandboxHours)
	}

	return d, nil
}

// Mirrors E2B's API (packages/api/internal/orchestrator/create_instance.go):
// addresses become CIDRs, allowed domains also allow the default nameserver
// so the sandbox can resolve them, and allowInternetAccess=false blocks all
// traffic, overriding denyOut. The orchestrator itself does not read
// allow_internet_access.
const (
	allTraffic        = "0.0.0.0/0"
	defaultNameserver = "8.8.8.8/32"
)

func isIPOrCIDR(v string) bool {
	if _, _, err := net.ParseCIDR(v); err == nil {
		return true
	}

	return net.ParseIP(v) != nil
}

func toCIDR(v string) string {
	if strings.Contains(v, "/") {
		return v
	}

	return v + "/32"
}

func egressConfig(n *networkBody, allowInternet *bool) *orchestrator.SandboxNetworkEgressConfig {
	e := &orchestrator.SandboxNetworkEgressConfig{}
	if n != nil {
		for _, v := range n.AllowOut {
			if isIPOrCIDR(v) {
				e.AllowedCidrs = append(e.AllowedCidrs, toCIDR(v))
			} else {
				e.AllowedDomains = append(e.AllowedDomains, v)
			}
		}
		if len(e.AllowedDomains) > 0 {
			e.AllowedCidrs = append(e.AllowedCidrs, defaultNameserver)
		}
		for _, v := range n.DenyOut {
			e.DeniedCidrs = append(e.DeniedCidrs, toCIDR(v))
		}
	}
	if allowInternet != nil && !*allowInternet {
		e.DeniedCidrs = []string{allTraffic}
	}
	if len(e.AllowedCidrs) == 0 && len(e.AllowedDomains) == 0 && len(e.DeniedCidrs) == 0 {
		return nil
	}

	return e
}

// validateNetwork applies E2B's rules: denyOut takes only addresses, and
// domains in allowOut need denyOut 0.0.0.0/0 to mean anything.
func validateNetwork(n *networkBody) error {
	if n == nil {
		return nil
	}
	for _, v := range n.DenyOut {
		if !isIPOrCIDR(v) {
			return fmt.Errorf("denyOut only accepts IP addresses and CIDR ranges: %s", v)
		}
	}
	hasDomain := slices.ContainsFunc(n.AllowOut, func(v string) bool { return !isIPOrCIDR(v) })
	if hasDomain && !slices.Contains(n.DenyOut, allTraffic) {
		return errors.New("allow out contains domains but deny out is missing 0.0.0.0/0 (ALL_TRAFFIC)")
	}

	return nil
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body newSandboxBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())

		return
	}
	timeout, err := s.parseTimeout(body.Timeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	if body.TemplateID == "" {
		body.TemplateID = "base"
	}
	if err := validateNetwork(body.Network); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	var (
		tpl   store.Template
		build store.Build
		found bool
	)
	s.store.View(func(tx store.Tx) {
		t := tx.ResolveTemplate(body.TemplateID)
		if t == nil {
			return
		}
		b := t.ReadyBuild()
		if b == nil {
			return
		}
		tpl, build, found = *t, *b, true
	})
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found or has no ready build", body.TemplateID))

		return
	}

	now := time.Now().UTC()
	sb := &store.Sandbox{
		ID:                  newSandboxID(),
		TemplateID:          tpl.ID,
		RunTemplateID:       tpl.ID,
		RunBuildID:          build.ID,
		EnvdVersion:         build.EnvdVersion,
		KernelVersion:       build.KernelVersion,
		FirecrackerVersion:  build.FirecrackerVersion,
		CPUCount:            build.CPUCount,
		MemoryMB:            build.MemoryMB,
		DiskSizeMB:          build.DiskSizeMB,
		RegularPages:        build.RegularPages,
		EnvdAccessToken:     newToken(),
		Metadata:            body.Metadata,
		EnvVars:             body.EnvVars,
		AllowInternetAccess: body.AllowInternetAccess,
		AutoPause:           body.AutoPause != nil && *body.AutoPause,
		AutoPauseMemory:     body.AutoPauseMemory == nil || *body.AutoPauseMemory,
		State:               store.SandboxRunning,
		StartedAt:           now,
		EndAt:               now.Add(timeout),
		Busy:                true,
	}
	if slices.Contains(tpl.Names, body.TemplateID) || strings.HasPrefix(body.TemplateID, firstName(tpl.Names)+":") {
		sb.Alias = firstName(tpl.Names)
	}
	if body.Network != nil && body.Network.AllowPublicTraffic != nil && !*body.Network.AllowPublicTraffic {
		sb.TrafficAccessToken = newToken()
	}
	if body.Network != nil {
		sb.AllowOut, sb.DenyOut = body.Network.AllowOut, body.Network.DenyOut
	}

	// Reserve the id before booting so the reaper and list see it.
	if err := s.store.Update(func(tx store.Tx) error { tx.PutSandbox(sb.Clone()); return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	if err := s.boot(r.Context(), sb, false); err != nil {
		_ = s.store.Update(func(tx store.Tx) error { tx.DeleteSandbox(sb.ID); return nil })
		writeGRPCError(w, "create sandbox", err)

		return
	}

	_ = s.store.Update(func(tx store.Tx) error {
		if t := tx.Template(tpl.ID); t != nil {
			t.SpawnCount++
			t.LastSpawnedAt = now
		}

		return nil
	})

	writeJSON(w, http.StatusCreated, s.toResponse(sb))
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}

	return names[0]
}

// boot starts sb on the orchestrator, fresh or from its last snapshot, and
// persists the result. sb must be claimed (Busy) by the caller; boot releases
// it on success.
func (s *Server) boot(ctx context.Context, sb *store.Sandbox, resume bool) error {
	if err := s.admit(sb.ID); err != nil {
		return err
	}
	sb.ExecutionID = newUUID()
	allowInternet := sb.AllowInternetAccess

	cfg := &orchestrator.SandboxConfig{
		TemplateId:          sb.RunTemplateID,
		BuildId:             sb.RunBuildID,
		BaseTemplateId:      sb.TemplateID,
		KernelVersion:       sb.KernelVersion,
		FirecrackerVersion:  sb.FirecrackerVersion,
		HugePages:           !sb.RegularPages,
		SandboxId:           sb.ID,
		ExecutionId:         sb.ExecutionID,
		EnvVars:             sb.EnvVars,
		Metadata:            sb.Metadata,
		EnvdVersion:         sb.EnvdVersion,
		Vcpu:                sb.CPUCount,
		RamMb:               sb.MemoryMB,
		TeamId:              teamID,
		MaxSandboxLength:    s.cfg.MaxSandboxHours,
		TotalDiskSizeMb:     sb.DiskSizeMB,
		Snapshot:            resume,
		AutoPause:           sb.AutoPause,
		EnvdAccessToken:     &sb.EnvdAccessToken,
		AllowInternetAccess: allowInternet,
	}
	if sb.Alias != "" {
		alias := sb.Alias
		cfg.Alias = &alias
	}
	egress := egressConfig(&networkBody{AllowOut: sb.AllowOut, DenyOut: sb.DenyOut}, sb.AllowInternetAccess)
	if traffic := sb.TrafficAccessToken; egress != nil || traffic != "" {
		cfg.Network = &orchestrator.SandboxNetworkConfig{Egress: egress}
		if traffic != "" {
			cfg.Network.Ingress = &orchestrator.SandboxNetworkIngressConfig{TrafficAccessToken: &traffic}
		}
	}

	req := &orchestrator.SandboxCreateRequest{
		Sandbox:   cfg,
		StartTime: timestamppb.New(sb.StartedAt),
		EndTime:   timestamppb.New(sb.EndAt),
	}
	if s.cfg.ColdBoot && !resume {
		cold := true
		req.FilesystemBoot = &cold
	}
	resp, err := s.sbx.Create(ctx, req)
	if err != nil {
		return err
	}

	sb.ClientID = resp.GetClientId()
	sb.State = store.SandboxRunning
	sb.Busy = false

	err = s.store.Update(func(tx store.Tx) error { tx.PutSandbox(sb.Clone()); return nil })
	s.applyMemoryPromises()

	return err
}

// claim marks a sandbox busy and returns a copy to work on. It fails with
// 404 when the sandbox is unknown and 409 when another operation holds it.
func (s *Server) claim(id string) (*store.Sandbox, int, error) {
	var (
		out  *store.Sandbox
		code int
	)
	s.store.View(func(tx store.Tx) {
		sb := tx.Sandbox(id)
		switch {
		case sb == nil:
			code = http.StatusNotFound
		case sb.Busy:
			code = http.StatusConflict
		default:
			sb.Busy = true
			out = sb.Clone()
		}
	})
	switch code {
	case http.StatusNotFound:
		return nil, code, fmt.Errorf("sandbox '%s' not found", id)
	case http.StatusConflict:
		return nil, code, fmt.Errorf("sandbox '%s' is busy with another operation, retry", id)
	}

	return out, 0, nil
}

func (s *Server) release(id string) {
	s.store.View(func(tx store.Tx) {
		if sb := tx.Sandbox(id); sb != nil {
			sb.Busy = false
		}
	})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	var (
		d     sandboxDetail
		found bool
	)
	s.store.View(func(tx store.Tx) {
		if sb := tx.Sandbox(r.PathValue("sandboxID")); sb != nil {
			d, found = s.toDetail(sb), true
		}
	})
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("sandbox '%s' not found", r.PathValue("sandboxID")))

		return
	}
	writeJSON(w, http.StatusOK, d)
}

// listSandboxes serves GET /v2/sandboxes. All results fit one page, so no
// x-next-token header is ever set.
func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	states := map[string]bool{}
	for _, v := range q["state"] {
		for _, st := range strings.Split(v, ",") {
			if st = strings.TrimSpace(st); st != "" {
				states[st] = true
			}
		}
	}
	if len(states) == 0 {
		states[store.SandboxRunning], states[store.SandboxPaused] = true, true
	}
	md, err := parseMetadataFilter(q.Get("metadata"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid metadata filter: "+err.Error())

		return
	}
	writeJSON(w, http.StatusOK, s.filterSandboxes(states, md))
}

// listSandboxesV1 serves GET /sandboxes: running sandboxes only.
func (s *Server) listSandboxesV1(w http.ResponseWriter, r *http.Request) {
	md, err := parseMetadataFilter(r.URL.Query().Get("metadata"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid metadata filter: "+err.Error())

		return
	}
	writeJSON(w, http.StatusOK, s.filterSandboxes(map[string]bool{store.SandboxRunning: true}, md))
}

func (s *Server) filterSandboxes(states map[string]bool, md map[string]string) []sandboxDetail {
	out := []sandboxDetail{}
	s.store.View(func(tx store.Tx) {
	next:
		for _, sb := range tx.Sandboxes() {
			if !states[sb.State] {
				continue
			}
			for k, v := range md {
				if sb.Metadata[k] != v {
					continue next
				}
			}
			d := s.toDetail(sb)
			d.EnvdAccessToken = ""
			out = append(out, d)
		}
	})

	return out
}

// parseMetadataFilter reads the SDK's metadata query: an url-encoded
// "k1=v1&k2=v2" string.
func parseMetadataFilter(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range vals {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}

	return out, nil
}

func (s *Server) killSandbox(w http.ResponseWriter, r *http.Request) {
	sb, code, err := s.claim(r.PathValue("sandboxID"))
	if err != nil {
		writeError(w, code, err.Error())

		return
	}
	if err := s.kill(r.Context(), sb); err != nil {
		s.release(sb.ID)
		writeGRPCError(w, "kill sandbox", err)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// kill stops a running sandbox, removes the snapshots of a paused one and
// forgets it.
func (s *Server) kill(ctx context.Context, sb *store.Sandbox) error {
	if sb.State == store.SandboxRunning {
		_, err := s.sbx.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: sb.ID})
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
	}
	s.deleteSnapshots(ctx, sb)

	return s.store.Update(func(tx store.Tx) error { tx.DeleteSandbox(sb.ID); return nil })
}

func (s *Server) deleteSnapshots(ctx context.Context, sb *store.Sandbox) {
	for _, b := range sb.SnapshotBuilds {
		if _, err := s.tpl.TemplateBuildDelete(ctx, buildDeleteReq(sb.SnapshotTemplateID, b)); err != nil {
			slog.Warn("delete snapshot build", "sandbox", sb.ID, "build", b, "err", err)
		}
	}
}

type timeoutBody struct {
	Timeout *int64 `json:"timeout"`
}

func (s *Server) setTimeout(w http.ResponseWriter, r *http.Request) {
	var body timeoutBody
	if err := readJSON(r, &body); err != nil || body.Timeout == nil {
		writeError(w, http.StatusBadRequest, "body must be {\"timeout\": seconds}")

		return
	}
	d, err := s.parseTimeout(body.Timeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	if code, err := s.extend(r.Context(), r.PathValue("sandboxID"), time.Now().UTC().Add(d), true); err != nil {
		writeError(w, code, err.Error())

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Duration *int64 `json:"duration"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	d, err := s.parseTimeout(body.Duration)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	if code, err := s.extend(r.Context(), r.PathValue("sandboxID"), time.Now().UTC().Add(d), false); err != nil {
		writeError(w, code, err.Error())

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// extend moves the end of a running sandbox. With exact it sets the end,
// otherwise it only ever moves it later.
func (s *Server) extend(ctx context.Context, id string, end time.Time, exact bool) (int, error) {
	sb, code, err := s.claim(id)
	if err != nil {
		return code, err
	}
	defer s.release(id)
	if sb.State != store.SandboxRunning {
		return http.StatusNotFound, fmt.Errorf("sandbox '%s' is not running", id)
	}
	if !exact && !end.After(sb.EndAt) {
		return 0, nil
	}
	if _, err := s.sbx.Update(ctx, &orchestrator.SandboxUpdateRequest{SandboxId: id, EndTime: timestamppb.New(end)}); err != nil {
		return http.StatusInternalServerError, err
	}

	return http.StatusInternalServerError, s.store.Update(func(tx store.Tx) error {
		if cur := tx.Sandbox(id); cur != nil {
			cur.EndAt = end
		}

		return nil
	})
}

func (s *Server) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Memory *bool `json:"memory"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	sb, code, err := s.claim(r.PathValue("sandboxID"))
	if err != nil {
		writeError(w, code, err.Error())

		return
	}
	if sb.State == store.SandboxPaused {
		s.release(sb.ID)
		writeError(w, http.StatusConflict, fmt.Sprintf("sandbox '%s' is already paused", sb.ID))

		return
	}
	fsOnly := body.Memory != nil && !*body.Memory
	if err := s.pause(r.Context(), sb, fsOnly); err != nil {
		s.release(sb.ID)
		writeGRPCError(w, "pause sandbox", err)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pause snapshots a running, claimed sandbox and releases it as paused.
func (s *Server) pause(ctx context.Context, sb *store.Sandbox, fsOnly bool) error {
	if sb.SnapshotTemplateID == "" {
		sb.SnapshotTemplateID = newTemplateID()
	}
	buildID := newUUID()
	_, err := s.sbx.Pause(ctx, &orchestrator.SandboxPauseRequest{
		SandboxId:      sb.ID,
		TemplateId:     sb.SnapshotTemplateID,
		BuildId:        buildID,
		FilesystemOnly: fsOnly,
	})
	if err != nil {
		return err
	}
	sb.RunTemplateID = sb.SnapshotTemplateID
	sb.RunBuildID = buildID
	sb.SnapshotBuilds = append(sb.SnapshotBuilds, buildID)
	sb.SnapshotFSOnly = fsOnly
	sb.State = store.SandboxPaused
	sb.Busy = false

	return s.store.Update(func(tx store.Tx) error { tx.PutSandbox(sb.Clone()); return nil })
}

// resumeSandbox serves the v1 POST /sandboxes/{id}/resume.
func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Timeout   *int64 `json:"timeout"`
		AutoPause *bool  `json:"autoPause"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	s.connect(w, r, body.Timeout, body.AutoPause, true)
}

// connectSandbox serves POST /v2/sandboxes/{id}/connect: resume when paused,
// otherwise extend the timeout and return the running sandbox.
func (s *Server) connectSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Timeout *int64 `json:"timeout"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	s.connect(w, r, body.Timeout, nil, false)
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request, timeoutSec *int64, autoPause *bool, onlyPaused bool) {
	timeout, err := s.parseTimeout(timeoutSec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	sb, code, err := s.claim(r.PathValue("sandboxID"))
	if err != nil {
		writeError(w, code, err.Error())

		return
	}
	end := time.Now().UTC().Add(timeout)

	if sb.State == store.SandboxRunning {
		s.release(sb.ID)
		if onlyPaused {
			writeError(w, http.StatusConflict, fmt.Sprintf("sandbox '%s' is already running", sb.ID))

			return
		}
		if code, err := s.extend(r.Context(), sb.ID, end, false); err != nil {
			writeError(w, code, err.Error())

			return
		}
		writeJSON(w, http.StatusOK, s.toResponse(sb))

		return
	}

	if autoPause != nil {
		sb.AutoPause = *autoPause
	}
	sb.StartedAt = time.Now().UTC()
	sb.EndAt = end
	if err := s.boot(r.Context(), sb, true); err != nil {
		s.release(sb.ID)
		writeGRPCError(w, "resume sandbox", err)

		return
	}
	writeJSON(w, http.StatusCreated, s.toResponse(sb))
}

// RunReaper ends sandboxes whose timeout passed: pause when they were created
// with autoPause, kill otherwise.
func (s *Server) RunReaper(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastSync time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if now.Sub(lastSync) >= syncInterval {
			lastSync = now
			if err := s.Reconcile(ctx); err != nil {
				slog.Warn("sync with orchestrator", "err", err)
			}
			s.applyMemoryPromises()
		}
		var due []string
		s.store.View(func(tx store.Tx) {
			for _, sb := range tx.Sandboxes() {
				if sb.State == store.SandboxRunning && !sb.Busy && now.After(sb.EndAt) {
					due = append(due, sb.ID)
				}
			}
		})
		for _, id := range due {
			sb, _, err := s.claim(id)
			if err != nil {
				continue
			}
			go s.expire(ctx, sb)
		}
	}
}

func (s *Server) expire(ctx context.Context, sb *store.Sandbox) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if sb.AutoPause {
		err := s.pause(ctx, sb, !sb.AutoPauseMemory)
		if err == nil {
			slog.Info("sandbox timed out, paused", "sandbox", sb.ID)

			return
		}
		slog.Error("auto-pause failed, killing instead", "sandbox", sb.ID, "err", err)
	}
	if err := s.kill(ctx, sb); err != nil {
		slog.Error("kill timed out sandbox", "sandbox", sb.ID, "err", err)
		s.release(sb.ID)

		return
	}
	slog.Info("sandbox timed out, killed", "sandbox", sb.ID)
}

// Reconcile aligns the store with the orchestrator after a restart: sandboxes
// the orchestrator lost are forgotten, sandboxes the store does not know are
// killed.
func (s *Server) Reconcile(ctx context.Context) error {
	resp, err := s.sbx.List(ctx, &emptypb.Empty{})
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, rs := range resp.GetSandboxes() {
		live[rs.GetSandboxId()] = true
	}
	var lost []*store.Sandbox
	known := map[string]bool{}
	s.store.View(func(tx store.Tx) {
		for _, sb := range tx.Sandboxes() {
			known[sb.ID] = true
			// A sandbox that booted after List was taken is not in it yet;
			// only ones running for a while can have been lost.
			if sb.State == store.SandboxRunning && !sb.Busy && !live[sb.ID] && time.Since(sb.StartedAt) > 30*time.Second {
				lost = append(lost, sb.Clone())
			}
		}
	})
	for _, sb := range lost {
		slog.Warn("sandbox no longer runs on the orchestrator (crashed or OOM-killed), forgetting it", "sandbox", sb.ID)
		s.deleteSnapshots(ctx, sb)
		_ = s.store.Update(func(tx store.Tx) error { tx.DeleteSandbox(sb.ID); return nil })
	}
	for id := range live {
		if !known[id] {
			slog.Warn("killing sandbox unknown to the store", "sandbox", id)
			_, _ = s.sbx.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: id})
		}
	}

	return nil
}

func writeGRPCError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, errNoMemory) {
		slog.Warn(op, "err", err)
		writeError(w, http.StatusServiceUnavailable, err.Error())

		return
	}
	st, _ := status.FromError(err)
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.ResourceExhausted:
		code = http.StatusTooManyRequests
	case codes.InvalidArgument, codes.FailedPrecondition:
		code = http.StatusBadRequest
	case codes.AlreadyExists:
		code = http.StatusConflict
	case codes.Unavailable:
		code = http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		code = http.StatusGatewayTimeout
	}
	slog.Error(op, "err", err)
	writeError(w, code, fmt.Sprintf("%s: %s", op, st.Message()))
}
