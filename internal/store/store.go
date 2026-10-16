// Package store keeps the little state the API needs in one JSON file:
// templates with their builds, and sandboxes (running or paused). It replaces
// the Postgres/Redis/ClickHouse trio of the full E2B API.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	BuildWaiting  = "waiting"
	BuildBuilding = "building"
	BuildReady    = "ready"
	BuildError    = "error"

	SandboxRunning = "running"
	SandboxPaused  = "paused"
)

type Build struct {
	ID                 string    `json:"id"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	ReasonStep         string    `json:"reasonStep,omitempty"`
	CPUCount           int64     `json:"cpuCount"`
	MemoryMB           int64     `json:"memoryMB"`
	MinFreeDiskMB      int64     `json:"minFreeDiskMB"`
	DiskSizeMB         int64     `json:"diskSizeMB"`
	EnvdVersion        string    `json:"envdVersion,omitempty"`
	KernelVersion      string    `json:"kernelVersion,omitempty"`
	FirecrackerVersion string    `json:"firecrackerVersion,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	FinishedAt         time.Time `json:"finishedAt,omitzero"`
}

type Template struct {
	ID        string    `json:"id"`
	Names     []string  `json:"names"`
	Tags      []string  `json:"tags,omitempty"`
	Builds    []*Build  `json:"builds"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// SpawnCount and LastSpawnedAt only feed GET /templates.
	SpawnCount    int64     `json:"spawnCount"`
	LastSpawnedAt time.Time `json:"lastSpawnedAt,omitzero"`
}

// ReadyBuild is the newest build that finished successfully.
func (t *Template) ReadyBuild() *Build {
	for i := len(t.Builds) - 1; i >= 0; i-- {
		if t.Builds[i].Status == BuildReady {
			return t.Builds[i]
		}
	}

	return nil
}

func (t *Template) Build(id string) *Build {
	for _, b := range t.Builds {
		if b.ID == id {
			return b
		}
	}

	return nil
}

// LatestBuild is the newest build in any state.
func (t *Template) LatestBuild() *Build {
	if len(t.Builds) == 0 {
		return nil
	}

	return t.Builds[len(t.Builds)-1]
}

type Sandbox struct {
	ID         string `json:"id"`
	TemplateID string `json:"templateID"`
	Alias      string `json:"alias,omitempty"`
	// RunTemplateID/RunBuildID is what the orchestrator boots from: the
	// template build on first start, the snapshot after a pause.
	RunTemplateID string `json:"runTemplateID"`
	RunBuildID    string `json:"runBuildID"`
	// SnapshotTemplateID is the per-sandbox template id under which pauses are
	// stored; created on the first pause.
	SnapshotTemplateID string `json:"snapshotTemplateID,omitempty"`
	SnapshotFSOnly     bool   `json:"snapshotFsOnly,omitempty"`
	// SnapshotBuilds are all pause layers, oldest first. Each resumes on top
	// of the one before, so they are only removed together on kill.
	SnapshotBuilds []string `json:"snapshotBuilds,omitempty"`

	ClientID            string            `json:"clientID"`
	ExecutionID         string            `json:"executionID"`
	EnvdVersion         string            `json:"envdVersion"`
	KernelVersion       string            `json:"kernelVersion"`
	FirecrackerVersion  string            `json:"firecrackerVersion"`
	CPUCount            int64             `json:"cpuCount"`
	MemoryMB            int64             `json:"memoryMB"`
	DiskSizeMB          int64             `json:"diskSizeMB"`
	EnvdAccessToken     string            `json:"envdAccessToken"`
	TrafficAccessToken  string            `json:"trafficAccessToken,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	EnvVars             map[string]string `json:"envVars,omitempty"`
	AllowInternetAccess *bool             `json:"allowInternetAccess,omitempty"`
	AllowOut            []string          `json:"allowOut,omitempty"`
	DenyOut             []string          `json:"denyOut,omitempty"`
	AutoPause           bool              `json:"autoPause"`
	AutoPauseMemory     bool              `json:"autoPauseMemory"`
	AutoResume          bool              `json:"autoResume"`
	State               string            `json:"state"`
	StartedAt           time.Time         `json:"startedAt"`
	EndAt               time.Time         `json:"endAt"`
	// Busy marks a pause, resume or kill in flight, so the reaper and a second
	// request do not act on the same sandbox at once.
	Busy bool `json:"-"`
}

type data struct {
	Templates map[string]*Template `json:"templates"`
	Sandboxes map[string]*Sandbox  `json:"sandboxes"`
}

// Store guards all state with one mutex. Callers mutate inside Update and the
// file is rewritten atomically afterwards; the data set is tiny.
type Store struct {
	mu   sync.Mutex
	path string
	d    data
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	s := &Store{
		path: filepath.Join(dir, "state.json"),
		d:    data{Templates: map[string]*Template{}, Sandboxes: map[string]*Sandbox{}},
	}

	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	}

	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if s.d.Templates == nil {
		s.d.Templates = map[string]*Template{}
	}
	if s.d.Sandboxes == nil {
		s.d.Sandboxes = map[string]*Sandbox{}
	}

	return s, nil
}

// Tx is the view a caller gets inside View/Update.
type Tx struct{ d *data }

func (tx Tx) Template(id string) *Template { return tx.d.Templates[id] }

// ResolveTemplate finds a template by id or by name. A ":tag" suffix is
// accepted and ignored: this API keeps no tags, the newest ready build wins.
func (tx Tx) ResolveTemplate(ref string) *Template {
	for i := 0; i < len(ref); i++ {
		if ref[i] == ':' {
			ref = ref[:i]

			break
		}
	}
	if t, ok := tx.d.Templates[ref]; ok {
		return t
	}
	for _, t := range tx.d.Templates {
		for _, n := range t.Names {
			if n == ref {
				return t
			}
		}
	}

	return nil
}

func (tx Tx) PutTemplate(t *Template)  { tx.d.Templates[t.ID] = t }
func (tx Tx) DeleteTemplate(id string) { delete(tx.d.Templates, id) }

func (tx Tx) Templates() []*Template {
	out := make([]*Template, 0, len(tx.d.Templates))
	for _, t := range tx.d.Templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })

	return out
}

func (tx Tx) Sandbox(id string) *Sandbox { return tx.d.Sandboxes[id] }
func (tx Tx) PutSandbox(s *Sandbox)      { tx.d.Sandboxes[s.ID] = s }
func (tx Tx) DeleteSandbox(id string)    { delete(tx.d.Sandboxes, id) }

func (tx Tx) Sandboxes() []*Sandbox {
	out := make([]*Sandbox, 0, len(tx.d.Sandboxes))
	for _, s := range tx.d.Sandboxes {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })

	return out
}

// View runs fn under the lock without persisting.
func (s *Store) View(fn func(tx Tx)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(Tx{&s.d})
}

// Update runs fn under the lock and persists when fn returns nil.
func (s *Store) Update(fn func(tx Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := fn(Tx{&s.d}); err != nil {
		return err
	}

	return s.persist()
}

func (s *Store) persist() error {
	raw, err := json.MarshalIndent(&s.d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}

	return nil
}

// Clone copies a sandbox so the stored record and a caller's working copy
// never share memory. Maps are copied too.
func (s *Sandbox) Clone() *Sandbox {
	c := *s
	c.Metadata = maps.Clone(s.Metadata)
	c.EnvVars = maps.Clone(s.EnvVars)
	c.SnapshotBuilds = slices.Clone(s.SnapshotBuilds)
	c.AllowOut = slices.Clone(s.AllowOut)
	c.DenyOut = slices.Clone(s.DenyOut)

	return &c
}
