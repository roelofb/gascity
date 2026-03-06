package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// newPostRequest creates a POST httptest request with the X-GC-Request header
// set, satisfying the CSRF protection middleware.
func newPostRequest(url string, body io.Reader) *http.Request {
	req := httptest.NewRequest("POST", url, body)
	req.Header.Set("X-GC-Request", "true")
	return req
}

// fakeState implements State for testing.
type fakeState struct {
	cfg         *config.City
	sp          *session.Fake
	stores      map[string]beads.Store
	mailProvs   map[string]mail.Provider
	eventProv   events.Provider
	cityName    string
	cityPath    string
	startedAt   time.Time
	quarantined map[string]bool
}

func newFakeState(t *testing.T) *fakeState {
	t.Helper()
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	return &fakeState{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{
				{Name: "worker", Dir: "myrig"},
			},
			Rigs: []config.Rig{
				{Name: "myrig", Path: "/tmp/myrig"},
			},
		},
		sp:        session.NewFake(),
		stores:    map[string]beads.Store{"myrig": store},
		mailProvs: map[string]mail.Provider{"myrig": mp},
		eventProv: events.NewFake(),
		cityName:  "test-city",
		cityPath:  t.TempDir(),
		startedAt: time.Now(),
	}
}

func (f *fakeState) Config() *config.City                    { return f.cfg }
func (f *fakeState) SessionProvider() session.Provider       { return f.sp }
func (f *fakeState) BeadStore(rig string) beads.Store        { return f.stores[rig] }
func (f *fakeState) BeadStores() map[string]beads.Store      { return f.stores }
func (f *fakeState) MailProvider(rig string) mail.Provider   { return f.mailProvs[rig] }
func (f *fakeState) MailProviders() map[string]mail.Provider { return f.mailProvs }
func (f *fakeState) EventProvider() events.Provider          { return f.eventProv }
func (f *fakeState) CityName() string                        { return f.cityName }
func (f *fakeState) CityPath() string                        { return f.cityPath }
func (f *fakeState) Version() string                         { return "test" }
func (f *fakeState) StartedAt() time.Time                    { return f.startedAt }
func (f *fakeState) IsQuarantined(sessionName string) bool   { return f.quarantined[sessionName] }

// fakeMutatorState extends fakeState with StateMutator for testing mutations.
type fakeMutatorState struct {
	*fakeState
	suspended map[string]bool
	killed    map[string]bool
	drained   map[string]bool
	nudges    map[string]string
}

func newFakeMutatorState(t *testing.T) *fakeMutatorState {
	t.Helper()
	return &fakeMutatorState{
		fakeState: newFakeState(t),
		suspended: make(map[string]bool),
		killed:    make(map[string]bool),
		drained:   make(map[string]bool),
		nudges:    make(map[string]string),
	}
}

func (f *fakeMutatorState) SuspendAgent(name string) error    { f.suspended[name] = true; return nil }
func (f *fakeMutatorState) ResumeAgent(name string) error     { delete(f.suspended, name); return nil }
func (f *fakeMutatorState) KillAgent(name string) error       { f.killed[name] = true; return nil }
func (f *fakeMutatorState) DrainAgent(name string) error      { f.drained[name] = true; return nil }
func (f *fakeMutatorState) UndrainAgent(name string) error    { delete(f.drained, name); return nil }
func (f *fakeMutatorState) NudgeAgent(name, msg string) error { f.nudges[name] = msg; return nil }
func (f *fakeMutatorState) SuspendRig(name string) error {
	cfg := f.Config()
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("rig %q not found", name)
	}
	tmpl := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if a.Dir != name {
			continue
		}
		expanded := expandAgent(a, f.cityName, tmpl, f.sp)
		for _, ea := range expanded {
			sessionName := agent.SessionNameFor(f.cityName, ea.qualifiedName, tmpl)
			_ = f.sp.SetMeta(sessionName, "suspended", "true")
		}
	}
	return nil
}

func (f *fakeMutatorState) ResumeRig(name string) error {
	cfg := f.Config()
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("rig %q not found", name)
	}
	tmpl := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if a.Dir != name {
			continue
		}
		expanded := expandAgent(a, f.cityName, tmpl, f.sp)
		for _, ea := range expanded {
			sessionName := agent.SessionNameFor(f.cityName, ea.qualifiedName, tmpl)
			_ = f.sp.RemoveMeta(sessionName, "suspended")
		}
	}
	return nil
}

func (f *fakeMutatorState) SuspendCity() error { f.cfg.Workspace.Suspended = true; return nil }
func (f *fakeMutatorState) ResumeCity() error  { f.cfg.Workspace.Suspended = false; return nil }
func (f *fakeMutatorState) CreateAgent(a config.Agent) error {
	f.cfg.Agents = append(f.cfg.Agents, a)
	return nil
}

func (f *fakeMutatorState) UpdateAgent(name string, a config.Agent) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range f.cfg.Agents {
		if f.cfg.Agents[i].Dir == dir && f.cfg.Agents[i].Name == base {
			a.Dir = f.cfg.Agents[i].Dir
			f.cfg.Agents[i] = a
			return nil
		}
	}
	return fmt.Errorf("agent %q not found", name)
}

func (f *fakeMutatorState) DeleteAgent(name string) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range f.cfg.Agents {
		if f.cfg.Agents[i].Dir == dir && f.cfg.Agents[i].Name == base {
			f.cfg.Agents = append(f.cfg.Agents[:i], f.cfg.Agents[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("agent %q not found", name)
}

func (f *fakeMutatorState) CreateRig(r config.Rig) error {
	f.cfg.Rigs = append(f.cfg.Rigs, r)
	return nil
}

func (f *fakeMutatorState) UpdateRig(name string, r config.Rig) error {
	for i := range f.cfg.Rigs {
		if f.cfg.Rigs[i].Name == name {
			if r.Path != "" {
				f.cfg.Rigs[i].Path = r.Path
			}
			f.cfg.Rigs[i].Suspended = r.Suspended
			return nil
		}
	}
	return fmt.Errorf("rig %q not found", name)
}

func (f *fakeMutatorState) DeleteRig(name string) error {
	for i := range f.cfg.Rigs {
		if f.cfg.Rigs[i].Name == name {
			f.cfg.Rigs = append(f.cfg.Rigs[:i], f.cfg.Rigs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("rig %q not found", name)
}
