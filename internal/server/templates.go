package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	tm "github.com/traumventures/e2b-minimal/internal/pb/templatemanager"
	"github.com/traumventures/e2b-minimal/internal/store"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type buildRequestBody struct {
	Name          string   `json:"name"`
	Alias         string   `json:"alias"`
	Tags          []string `json:"tags"`
	CPUCount      *int64   `json:"cpuCount"`
	MemoryMB      *int64   `json:"memoryMB"`
	MinFreeDiskMB *int64   `json:"minFreeDiskMb"`
}

// requestBuild serves POST /v3/templates: find or create the template by
// name and add a waiting build.
func (s *Server) requestBuild(w http.ResponseWriter, r *http.Request) {
	var body buildRequestBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())

		return
	}
	name := body.Name
	if name == "" {
		name = body.Alias
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		// name:tag. Tags are not kept; every build of a name replaces the last.
		body.Tags = append(body.Tags, name[i+1:])
		name = name[:i]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !nameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "template name must be lowercase letters, digits, '.', '_' or '-'")

		return
	}

	b, err := s.newBuild(name, body.CPUCount, body.MemoryMB, body.MinFreeDiskMB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}
	tags := body.Tags
	if len(tags) == 0 {
		tags = []string{"default"}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"templateID": b.templateID,
		"buildID":    b.buildID,
		"public":     false,
		"names":      []string{name},
		"aliases":    []string{name},
		"tags":       tags,
	})
}

type buildRef struct{ templateID, buildID string }

func (s *Server) newBuild(name string, cpu, mem, minFree *int64) (buildRef, error) {
	or := func(v *int64, def int64) int64 {
		if v == nil || *v <= 0 {
			return def
		}

		return *v
	}
	now := time.Now().UTC()
	var ref buildRef
	err := s.store.Update(func(tx store.Tx) error {
		t := tx.ResolveTemplate(name)
		if t == nil {
			t = &store.Template{ID: newTemplateID(), Names: []string{name}, CreatedAt: now}
			tx.PutTemplate(t)
		}
		t.UpdatedAt = now
		b := &store.Build{
			ID:            newUUID(),
			Status:        store.BuildWaiting,
			CPUCount:      or(cpu, s.cfg.DefaultCPUCount),
			MemoryMB:      or(mem, s.cfg.DefaultMemoryMB),
			MinFreeDiskMB: or(minFree, s.cfg.DefaultFreeDiskMB),
			CreatedAt:     now,
		}
		t.Builds = append(t.Builds, b)
		ref = buildRef{t.ID, b.ID}

		return nil
	})

	return ref, err
}

func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	var exists bool
	s.store.View(func(tx store.Tx) { exists = tx.Template(templateID) != nil })
	if !exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found", templateID))

		return
	}
	scope := teamID
	resp, err := s.tpl.InitLayerFileUpload(r.Context(), &tm.InitLayerFileUploadRequest{
		TemplateID: templateID,
		Hash:       r.PathValue("hash"),
		CacheScope: &scope,
	})
	if err != nil {
		writeGRPCError(w, "init file upload", err)

		return
	}
	out := map[string]any{"present": resp.GetPresent()}
	if resp.Url != nil {
		out["url"] = s.rewriteUploadURL(r, resp.GetUrl())
	}
	if h := resp.GetHeaders(); len(h) > 0 {
		out["headers"] = h
	}
	writeJSON(w, http.StatusCreated, out)
}

type templateStep struct {
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	FilesHash *string  `json:"filesHash"`
	Force     *bool    `json:"force"`
}

type buildStartBody struct {
	FromImage         string          `json:"fromImage"`
	FromTemplate      string          `json:"fromTemplate"`
	FromImageRegistry json.RawMessage `json:"fromImageRegistry"`
	Force             *bool           `json:"force"`
	Steps             []templateStep  `json:"steps"`
	StartCmd          string          `json:"startCmd"`
	ReadyCmd          string          `json:"readyCmd"`
}

func (s *Server) startBuild(w http.ResponseWriter, r *http.Request) {
	var body buildStartBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())

		return
	}
	code, err := s.start(r.Context(), r.PathValue("templateID"), r.PathValue("buildID"), body, templateVersion(r.UserAgent()))
	if err != nil {
		writeError(w, code, err.Error())

		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) start(ctx context.Context, templateID, buildID string, body buildStartBody, version string) (int, error) {
	if (body.FromImage == "") == (body.FromTemplate == "") {
		return http.StatusBadRequest, errors.New("specify exactly one of fromImage and fromTemplate")
	}
	registry, err := parseRegistry(body.FromImageRegistry)
	if err != nil {
		return http.StatusBadRequest, err
	}

	var (
		build    store.Build
		found    bool
		fromTpl  *tm.FromTemplateConfig
		srcError string
	)
	s.store.View(func(tx store.Tx) {
		t := tx.Template(templateID)
		if t == nil {
			return
		}
		b := t.Build(buildID)
		if b == nil {
			return
		}
		build, found = *b, true
		if body.FromTemplate != "" {
			src := tx.ResolveTemplate(body.FromTemplate)
			if src == nil || src.ReadyBuild() == nil {
				srcError = fmt.Sprintf("template '%s' not found or has no ready build", body.FromTemplate)

				return
			}
			fromTpl = &tm.FromTemplateConfig{Alias: body.FromTemplate, BuildID: src.ReadyBuild().ID}
		}
	})
	switch {
	case !found:
		return http.StatusNotFound, fmt.Errorf("build '%s' of template '%s' not found", buildID, templateID)
	case build.Status != store.BuildWaiting:
		return http.StatusBadRequest, errors.New("build is not in waiting state")
	case srcError != "":
		return http.StatusBadRequest, errors.New(srcError)
	}

	steps := make([]*tm.TemplateStep, 0, len(body.Steps))
	for _, st := range body.Steps {
		steps = append(steps, &tm.TemplateStep{Type: st.Type, Args: st.Args, FilesHash: st.FilesHash, Force: st.Force})
	}
	freeDisk := int32(build.MinFreeDiskMB)
	cfg := &tm.TemplateConfig{
		TemplateID:        templateID,
		BuildID:           buildID,
		TeamID:            teamID,
		MemoryMB:          int32(build.MemoryMB),
		VCpuCount:         int32(build.CPUCount),
		DiskSizeMB:        int32(s.cfg.BuildDiskMB),
		FreeDiskSizeMB:    &freeDisk,
		StartCommand:      body.StartCmd,
		ReadyCommand:      body.ReadyCmd,
		Force:             body.Force,
		Steps:             steps,
		FromImageRegistry: registry,
	}
	if fromTpl != nil {
		cfg.Source = &tm.TemplateConfig_FromTemplate{FromTemplate: fromTpl}
	} else {
		cfg.Source = &tm.TemplateConfig_FromImage{FromImage: body.FromImage}
	}

	scope := teamID
	if _, err := s.tpl.TemplateCreate(ctx, &tm.TemplateCreateRequest{Template: cfg, CacheScope: &scope, Version: &version}); err != nil {
		_ = s.setBuild(templateID, buildID, func(b *store.Build) {
			b.Status, b.Reason, b.FinishedAt = store.BuildError, err.Error(), time.Now().UTC()
		})

		return http.StatusInternalServerError, fmt.Errorf("start build: %w", err)
	}
	if err := s.setBuild(templateID, buildID, func(b *store.Build) { b.Status = store.BuildBuilding }); err != nil {
		return http.StatusInternalServerError, err
	}
	go s.watchBuild(templateID, buildID)

	return 0, nil
}

func parseRegistry(raw json.RawMessage) (*tm.FromImageRegistry, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var reg struct {
		Type               string `json:"type"`
		Username           string `json:"username"`
		Password           string `json:"password"`
		AWSAccessKeyID     string `json:"awsAccessKeyId"`
		AWSSecretAccessKey string `json:"awsSecretAccessKey"`
		AWSRegion          string `json:"awsRegion"`
		ServiceAccountJSON string `json:"serviceAccountJson"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("invalid fromImageRegistry: %w", err)
	}
	switch reg.Type {
	case "registry":
		return &tm.FromImageRegistry{Type: &tm.FromImageRegistry_General{General: &tm.GeneralRegistry{Username: reg.Username, Password: reg.Password}}}, nil
	case "aws":
		return &tm.FromImageRegistry{Type: &tm.FromImageRegistry_Aws{Aws: &tm.AWSRegistry{AwsAccessKeyId: reg.AWSAccessKeyID, AwsSecretAccessKey: reg.AWSSecretAccessKey, AwsRegion: reg.AWSRegion}}}, nil
	case "gcp":
		return &tm.FromImageRegistry{Type: &tm.FromImageRegistry_Gcp{Gcp: &tm.GCPRegistry{ServiceAccountJson: reg.ServiceAccountJSON}}}, nil
	default:
		return nil, fmt.Errorf("unknown fromImageRegistry type %q", reg.Type)
	}
}

// templateVersion mirrors the E2B API: SDKs before 2.3.0 get the beta build
// format, everything else the current one.
func templateVersion(userAgent string) string {
	for _, agent := range strings.Fields(userAgent) {
		for _, prefix := range []string{"e2b-js-sdk/", "e2b-python-sdk/"} {
			if v, ok := strings.CutPrefix(agent, prefix); ok {
				if versionLess(v, "2.3.0") {
					return "v2.0.0"
				}

				return "v2.1.0"
			}
		}
	}

	return "v2.1.0"
}

func versionLess(a, b string) bool {
	num := func(s string) int {
		end := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
		if end >= 0 {
			s = s[:end]
		}
		n, _ := strconv.Atoi(s)

		return n
	}
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(b, ".")
	for i := range 3 {
		var x, y int
		if i < len(pa) {
			x = num(pa[i])
		}
		if i < len(pb) {
			y = num(pb[i])
		}
		if x != y {
			return x < y
		}
	}

	return false
}

func (s *Server) setBuild(templateID, buildID string, fn func(b *store.Build)) error {
	return s.store.Update(func(tx store.Tx) error {
		t := tx.Template(templateID)
		if t == nil {
			return fmt.Errorf("template '%s' not found", templateID)
		}
		b := t.Build(buildID)
		if b == nil {
			return fmt.Errorf("build '%s' not found", buildID)
		}
		fn(b)
		t.UpdatedAt = time.Now().UTC()

		return nil
	})
}

// watchBuild polls the template manager until the build ends and records
// the result, so it survives the template manager forgetting the build. A
// build the template manager no longer knows (it restarted) is failed after
// a minute of errors.
func (s *Server) watchBuild(templateID, buildID string) {
	ctx := context.Background()
	zero := uint32(0)
	failures := 0
	for {
		time.Sleep(2 * time.Second)
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := s.tpl.TemplateBuildStatus(cctx, &tm.TemplateStatusRequest{TemplateID: templateID, BuildID: buildID, Limit: &zero})
		cancel()
		if err != nil {
			failures++
			slog.Warn("build status", "template", templateID, "build", buildID, "err", err)
			if failures >= 30 {
				_ = s.setBuild(templateID, buildID, func(b *store.Build) {
					b.Status, b.FinishedAt = store.BuildError, time.Now().UTC()
					b.Reason = "the template manager lost this build: " + err.Error()
				})

				return
			}

			continue
		}
		failures = 0
		if s.recordResult(templateID, buildID, resp) {
			return
		}
	}
}

// recordResult stores a finished build's outcome and reports whether the
// build has finished.
func (s *Server) recordResult(templateID, buildID string, resp *tm.TemplateBuildStatusResponse) bool {
	switch resp.GetStatus() {
	case tm.TemplateBuildState_Completed:
		meta := resp.GetMetadata()
		err := s.setBuild(templateID, buildID, func(b *store.Build) {
			if b.Status == store.BuildReady {
				return
			}
			b.Status, b.FinishedAt = store.BuildReady, time.Now().UTC()
			b.EnvdVersion = meta.GetEnvdVersionKey()
			b.KernelVersion = orDefault(meta.GetKernelVersion(), s.cfg.KernelVersion)
			b.FirecrackerVersion = orDefault(meta.GetFirecrackerVersion(), s.cfg.FirecrackerVersion)
			b.DiskSizeMB = int64(meta.GetRootfsSizeKey())
			slog.Info("template build ready", "template", templateID, "build", buildID, "envd", b.EnvdVersion)
		})
		if err != nil {
			slog.Error("record build result", "err", err)
		}

		return true
	case tm.TemplateBuildState_Failed:
		_ = s.setBuild(templateID, buildID, func(b *store.Build) {
			if b.Status == store.BuildError {
				return
			}
			b.Status, b.FinishedAt = store.BuildError, time.Now().UTC()
			b.Reason = resp.GetReason().GetMessage()
			b.ReasonStep = resp.GetReason().GetStep()
			slog.Warn("template build failed", "template", templateID, "build", buildID, "reason", b.Reason)
		})

		return true
	default:
		return false
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}

	return v
}

var levelNames = map[tm.LogLevel]string{tm.LogLevel_Debug: "debug", tm.LogLevel_Info: "info", tm.LogLevel_Warn: "warn", tm.LogLevel_Error: "error"}

type logEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Level     string    `json:"level"`
	Step      string    `json:"step,omitempty"`
}

func (s *Server) buildStatus(w http.ResponseWriter, r *http.Request) {
	templateID, buildID := r.PathValue("templateID"), r.PathValue("buildID")
	var (
		build store.Build
		found bool
	)
	s.store.View(func(tx store.Tx) {
		if t := tx.Template(templateID); t != nil {
			if b := t.Build(buildID); b != nil {
				build, found = *b, true
			}
		}
	})
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("build '%s' of template '%s' not found", buildID, templateID))

		return
	}

	q := r.URL.Query()
	req := &tm.TemplateStatusRequest{TemplateID: templateID, BuildID: buildID}
	if v, err := strconv.Atoi(q.Get("logsOffset")); err == nil && v > 0 {
		o := int32(v)
		req.Offset = &o
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v >= 0 {
		l := uint32(v)
		req.Limit = &l
	}
	for lvl, name := range levelNames {
		if q.Get("level") == name {
			l := lvl
			req.Level = &l
		}
	}

	entries := []logEntry{}
	if build.Status != store.BuildWaiting {
		resp, err := s.tpl.TemplateBuildStatus(r.Context(), req)
		if err == nil {
			for _, e := range resp.GetLogEntries() {
				entries = append(entries, logEntry{
					Timestamp: e.GetTimestamp().AsTime(),
					Message:   e.GetMessage(),
					Level:     levelNames[e.GetLevel()],
					Step:      e.GetFields()["step"],
				})
			}
			// Record before answering: an SDK that sees "ready" creates a
			// sandbox right away and needs the build metadata stored.
			if s.recordResult(templateID, buildID, resp) {
				s.store.View(func(tx store.Tx) { build = *tx.Template(templateID).Build(buildID) })
			}
		} else if build.Status == store.BuildBuilding {
			slog.Warn("build status", "template", templateID, "build", buildID, "err", err)
		}
	}

	logs := make([]string, 0, len(entries))
	for _, e := range entries {
		logs = append(logs, fmt.Sprintf("[%s] %s", e.Timestamp.Format(time.RFC3339), e.Message))
	}
	out := map[string]any{
		"templateID": templateID,
		"buildID":    buildID,
		"status":     build.Status,
		"logs":       logs,
		"logEntries": entries,
	}
	if build.Status == store.BuildError {
		reason := map[string]any{"message": orDefault(build.Reason, "build failed"), "logEntries": []logEntry{}}
		if build.ReasonStep != "" {
			reason["step"] = build.ReasonStep
		}
		out["reason"] = reason
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listTemplates(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]any{}
	s.store.View(func(tx store.Tx) {
		for _, t := range tx.Templates() {
			b := t.ReadyBuild()
			if b == nil {
				b = t.LatestBuild()
			}
			if b == nil {
				continue
			}
			var lastSpawned any
			if !t.LastSpawnedAt.IsZero() {
				lastSpawned = t.LastSpawnedAt
			}
			out = append(out, map[string]any{
				"templateID":    t.ID,
				"buildID":       b.ID,
				"cpuCount":      b.CPUCount,
				"memoryMB":      b.MemoryMB,
				"diskSizeMB":    b.DiskSizeMB,
				"public":        false,
				"aliases":       t.Names,
				"names":         t.Names,
				"createdAt":     t.CreatedAt,
				"updatedAt":     t.UpdatedAt,
				"createdBy":     nil,
				"lastSpawnedAt": lastSpawned,
				"spawnCount":    t.SpawnCount,
				"buildCount":    len(t.Builds),
				"envdVersion":   b.EnvdVersion,
				"buildStatus":   b.Status,
			})
		}
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAlias(w http.ResponseWriter, r *http.Request) {
	var id string
	s.store.View(func(tx store.Tx) {
		if t := tx.ResolveTemplate(r.PathValue("alias")); t != nil {
			id = t.ID
		}
	})
	if id == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found", r.PathValue("alias")))

		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templateID": id, "public": false})
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	var (
		t     *store.Template
		inUse bool
	)
	s.store.View(func(tx store.Tx) {
		t = tx.ResolveTemplate(r.PathValue("templateID"))
		if t == nil {
			return
		}
		for _, sb := range tx.Sandboxes() {
			if sb.TemplateID == t.ID {
				inUse = true
			}
		}
	})
	switch {
	case t == nil:
		writeError(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found", r.PathValue("templateID")))

		return
	case inUse:
		writeError(w, http.StatusConflict, "template is used by running or paused sandboxes; kill them first")

		return
	}
	for _, b := range t.Builds {
		if b.Status == store.BuildWaiting {
			continue
		}
		if _, err := s.tpl.TemplateBuildDelete(r.Context(), buildDeleteReq(t.ID, b.ID)); err != nil {
			slog.Warn("delete template build", "template", t.ID, "build", b.ID, "err", err)
		}
	}
	if err := s.store.Update(func(tx store.Tx) error { tx.DeleteTemplate(t.ID); return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func buildDeleteReq(templateID, buildID string) *tm.TemplateBuildDeleteRequest {
	return &tm.TemplateBuildDeleteRequest{TemplateID: templateID, BuildID: buildID}
}

// BootstrapBase builds the "base" template from the configured image when it
// has no ready build yet, so Sandbox.create() works on a fresh install.
func (s *Server) BootstrapBase(ctx context.Context) error {
	var ready, building bool
	s.store.View(func(tx store.Tx) {
		if t := tx.ResolveTemplate("base"); t != nil {
			ready = t.ReadyBuild() != nil
			if b := t.LatestBuild(); b != nil && b.Status == store.BuildBuilding {
				building = true
			}
		}
	})
	if ready || building {
		return nil
	}
	slog.Info("building the base template", "image", s.cfg.BaseImage)
	ref, err := s.newBuild("base", nil, nil, nil)
	if err != nil {
		return err
	}
	if _, err := s.start(ctx, ref.templateID, ref.buildID, buildStartBody{FromImage: s.cfg.BaseImage}, "v2.1.0"); err != nil {
		return err
	}

	return nil
}

// ResumeWatches restarts the result watchers of builds that were running
// when the server stopped.
func (s *Server) ResumeWatches() {
	s.store.View(func(tx store.Tx) {
		for _, t := range tx.Templates() {
			for _, b := range t.Builds {
				if b.Status == store.BuildBuilding {
					go s.watchBuild(t.ID, b.ID)
				}
			}
		}
	})
}
