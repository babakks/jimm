package monitor

import (
	"sync"
	"time"

	jujuparams "github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/state/multiwatcher"
	jujujujutesting "github.com/juju/juju/testing"
	jujutesting "github.com/juju/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	gc "gopkg.in/check.v1"
	"gopkg.in/errgo.v1"

	"github.com/CanonicalLtd/jem/internal/jem"
	"github.com/CanonicalLtd/jem/internal/mongodoc"
	"github.com/CanonicalLtd/jem/params"
)

func newJEMShimWithUpdateNotify(j jemInterface) jemShimWithUpdateNotify {
	return jemShimWithUpdateNotify{
		controllerStatsSet:        make(chan struct{}, 10),
		modelLifeSet:              make(chan struct{}, 10),
		modelUnitCountSet:         make(chan struct{}, 10),
		leaseAcquired:             make(chan struct{}, 10),
		controllerAvailabilitySet: make(chan struct{}, 10),
		jemInterface:              j,
	}
}

type jemShimWithUpdateNotify struct {
	controllerStatsSet        chan struct{}
	modelLifeSet              chan struct{}
	modelUnitCountSet         chan struct{}
	leaseAcquired             chan struct{}
	controllerAvailabilitySet chan struct{}
	jemInterface
}

// await waits for the given function to return the expected value,
// retrying after any of the shim notification channels have
// received a value.
func (s jemShimWithUpdateNotify) await(c *gc.C, f func() interface{}, want interface{}) {
	timeout := time.After(jujujujutesting.LongWait)
	for {
		got := f()
		ok, _ := jc.DeepEqual(got, want)
		if ok {
			break
		}
		select {
		case <-s.controllerStatsSet:
		case <-s.modelLifeSet:
		case <-s.modelUnitCountSet:
		case <-s.leaseAcquired:
		case <-timeout:
			c.Assert(got, jc.DeepEquals, want, gc.Commentf("timed out waiting for value"))
		}
	}
	// We've got the expected value; now check that it remains stable
	// as long as events continue to arrive.
	for {
		event := s.waitAny(jujujujutesting.ShortWait)
		got := f()
		c.Assert(got, jc.DeepEquals, want, gc.Commentf("value changed after waiting (last event %q)", event))
		if event == "" {
			return
		}
	}
}

func (s jemShimWithUpdateNotify) Clone() jemInterface {
	s.jemInterface = s.jemInterface.Clone()
	return s
}

func (s jemShimWithUpdateNotify) assertNoEvent(c *gc.C) {
	if event := s.waitAny(jujutesting.ShortWait); event != "" {
		c.Fatalf("unexpected event received: %v", event)
	}
}

// waitAny waits for any update to happen, waiting for a maximum
// of the given time. If nothing happens, it returns the empty string,
// otherwise it returns the name of the event that happened.
func (s jemShimWithUpdateNotify) waitAny(maxWait time.Duration) string {
	select {
	case <-s.controllerStatsSet:
		return "controller stats"
	case <-s.modelLifeSet:
		return "model life"
	case <-s.modelUnitCountSet:
		return "model unit count"
	case <-s.leaseAcquired:
		return "lease acquired"
	case <-time.After(jujutesting.ShortWait):
		return ""
	}
}

func (s jemShimWithUpdateNotify) SetControllerStats(ctlPath params.EntityPath, stats *mongodoc.ControllerStats) error {
	if err := s.jemInterface.SetControllerStats(ctlPath, stats); err != nil {
		return err
	}
	s.controllerStatsSet <- struct{}{}
	return nil
}

func (s jemShimWithUpdateNotify) SetControllerUnavailableAt(ctlPath params.EntityPath, t time.Time) error {
	if err := s.jemInterface.SetControllerUnavailableAt(ctlPath, t); err != nil {
		return err
	}
	s.controllerAvailabilitySet <- struct{}{}
	return nil
}

func (s jemShimWithUpdateNotify) SetControllerAvailable(ctlPath params.EntityPath) error {
	if err := s.jemInterface.SetControllerAvailable(ctlPath); err != nil {
		return err
	}
	s.controllerAvailabilitySet <- struct{}{}
	return nil
}

func (s jemShimWithUpdateNotify) SetModelLife(ctlPath params.EntityPath, uuid string, life string) error {
	if err := s.jemInterface.SetModelLife(ctlPath, uuid, life); err != nil {
		return err
	}
	s.modelLifeSet <- struct{}{}
	return nil
}

func (s jemShimWithUpdateNotify) SetModelUnitCount(ctlPath params.EntityPath, uuid string, n int) error {
	if err := s.jemInterface.SetModelUnitCount(ctlPath, uuid, n); err != nil {
		return err
	}
	s.modelUnitCountSet <- struct{}{}
	return nil
}

func (s jemShimWithUpdateNotify) AcquireMonitorLease(ctlPath params.EntityPath, oldExpiry time.Time, oldOwner string, newExpiry time.Time, newOwner string) (time.Time, error) {
	t, err := s.jemInterface.AcquireMonitorLease(ctlPath, oldExpiry, oldOwner, newExpiry, newOwner)
	if err != nil {
		return time.Time{}, err
	}
	s.leaseAcquired <- struct{}{}
	return t, err
}

type jemShimWithAPIOpener struct {
	// openAPI is called when the OpenAPI method is called.
	openAPI func(path params.EntityPath) (jujuAPI, error)
	jemInterface
}

func (s jemShimWithAPIOpener) OpenAPI(path params.EntityPath) (jujuAPI, error) {
	return s.openAPI(path)
}

func (s jemShimWithAPIOpener) Clone() jemInterface {
	s.jemInterface = s.jemInterface.Clone()
	return s
}

type jemShimWithMonitorLeaseAcquirer struct {
	// acquireMonitorLease is called when the AcquireMonitorLease
	// method is called.
	acquireMonitorLease func(ctlPath params.EntityPath, oldExpiry time.Time, oldOwner string, newExpiry time.Time, newOwner string) (time.Time, error)
	jemInterface
}

func (s jemShimWithMonitorLeaseAcquirer) AcquireMonitorLease(ctlPath params.EntityPath, oldExpiry time.Time, oldOwner string, newExpiry time.Time, newOwner string) (time.Time, error) {
	return s.acquireMonitorLease(ctlPath, oldExpiry, oldOwner, newExpiry, newOwner)
}

func (s jemShimWithMonitorLeaseAcquirer) Clone() jemInterface {
	s.jemInterface = s.jemInterface.Clone()
	return s
}

type jemShimInMemory struct {
	mu                          sync.Mutex
	refCount                    int
	controllers                 map[params.EntityPath]*mongodoc.Controller
	models                      map[params.EntityPath]*mongodoc.Model
	controllerUpdateCredentials map[params.EntityPath]bool
}

var _ jemInterface = (*jemShimInMemory)(nil)

func newJEMShimInMemory() *jemShimInMemory {
	return &jemShimInMemory{
		controllers: make(map[params.EntityPath]*mongodoc.Controller),
		models:      make(map[params.EntityPath]*mongodoc.Model),
		controllerUpdateCredentials: make(map[params.EntityPath]bool),
	}
}

func (s *jemShimInMemory) AddController(ctl *mongodoc.Controller) {
	if ctl.Path == (params.EntityPath{}) {
		panic("no path in controller")
	}
	ctl.Id = ctl.Path.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl1 := *ctl
	s.controllers[ctl.Path] = &ctl1
}

func (s *jemShimInMemory) AddModel(m *mongodoc.Model) {
	if m.Path.IsZero() {
		panic("no path in model")
	}
	if m.Controller.IsZero() {
		panic("no controller in model")
	}
	m.Id = m.Path.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	m1 := *m
	s.models[m.Path] = &m1
}

func (s *jemShimInMemory) Clone() jemInterface {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refCount++
	return s
}

func (s *jemShimInMemory) SetControllerStats(ctlPath params.EntityPath, stats *mongodoc.ControllerStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controllers[ctlPath]
	if !ok {
		return errgo.WithCausef(nil, params.ErrNotFound, "")
	}
	ctl.Stats = *stats
	return nil
}

func (s *jemShimInMemory) SetControllerAvailable(ctlPath params.EntityPath) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controllers[ctlPath]
	if ok {
		ctl.UnavailableSince = time.Time{}
	}
	return nil
}

func (s *jemShimInMemory) SetControllerUnavailableAt(ctlPath params.EntityPath, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controllers[ctlPath]
	if ok && ctl.UnavailableSince.IsZero() {
		ctl.UnavailableSince = mongodoc.Time(t)
	}
	return nil
}

func (s *jemShimInMemory) SetModelLife(ctlPath params.EntityPath, uuid string, life string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.models {
		if m.Controller == ctlPath && m.UUID == uuid {
			m.Life = life
		}
	}
	return nil
}

func (s jemShimInMemory) SetModelUnitCount(ctlPath params.EntityPath, uuid string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.models {
		if m.Controller == ctlPath && m.UUID == uuid {
			m.UnitCount = n
		}
	}
	return nil
}

func (s *jemShimInMemory) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refCount--
	if s.refCount < 0 {
		panic("close too many times")
	}
}

func (s *jemShimInMemory) AllControllers() ([]*mongodoc.Controller, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r []*mongodoc.Controller
	for _, c := range s.controllers {
		c1 := *c
		r = append(r, &c1)
	}
	return r, nil
}

func (s *jemShimInMemory) ControllerUpdateCredentials(ctlPath params.EntityPath) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controllerUpdateCredentials[ctlPath] = true
	return nil
}

func (s *jemShimInMemory) OpenAPI(params.EntityPath) (jujuAPI, error) {
	return nil, errgo.New("jemShimInMemory doesn't implement OpenAPI")
}

func (s *jemShimInMemory) AcquireMonitorLease(ctlPath params.EntityPath, oldExpiry time.Time, oldOwner string, newExpiry time.Time, newOwner string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controllers[ctlPath]
	if !ok {
		return time.Time{}, errgo.WithCausef(nil, params.ErrNotFound, "")
	}
	if ctl.MonitorLeaseOwner != oldOwner || !ctl.MonitorLeaseExpiry.UTC().Equal(oldExpiry.UTC()) {
		return time.Time{}, errgo.WithCausef(nil, jem.ErrLeaseUnavailable, "")
	}
	ctl.MonitorLeaseOwner = newOwner
	if newOwner == "" {
		ctl.MonitorLeaseExpiry = time.Time{}
	} else {
		ctl.MonitorLeaseExpiry = mongodoc.Time(newExpiry)
	}
	return ctl.MonitorLeaseExpiry, nil
}

type jujuAPIShims struct {
	mu           sync.Mutex
	openCount    int
	watcherCount int
}

func newJujuAPIShims() *jujuAPIShims {
	return &jujuAPIShims{}
}

// newJujuAPIShim returns an implementation of the jujuAPI interface
// that, when WatchAllModels is called, returns the given initial
// deltas and then nothing.
// The
func (s *jujuAPIShims) newJujuAPIShim(initial []multiwatcher.Delta) *jujuAPIShim {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openCount++
	return &jujuAPIShim{
		initial: initial,
		shims:   s,
	}
}

var closedAttempt = &utils.AttemptStrategy{
	Total: time.Second,
	Delay: time.Millisecond,
}

// CheckAllClosed checks that all API connections and watchers
// have been closed.
func (s *jujuAPIShims) CheckAllClosed(c *gc.C) {
	// The API connections can be closed asynchronously after
	// the worker is closed down, so wait for a while to make sure
	// they are actually closed.
	for a := closedAttempt.Start(); a.Next(); {
		s.mu.Lock()
		n := s.openCount
		s.mu.Unlock()
		if n == 0 {
			break
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c.Check(s.openCount, gc.Equals, 0)
	c.Check(s.watcherCount, gc.Equals, 0)
}

var _ jujuAPI = (*jujuAPIShim)(nil)

// jujuAPIShim implements the jujuAPI interface.
type jujuAPIShim struct {
	shims   *jujuAPIShims
	closed  bool
	initial []multiwatcher.Delta
	stack   string
}

func (s *jujuAPIShim) Evict() {
	s.Close()
}

func (s *jujuAPIShim) Close() error {
	s.shims.mu.Lock()
	defer s.shims.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.shims.openCount--
	return nil
}

func (s *jujuAPIShim) WatchAllModels() (allWatcher, error) {
	s.shims.mu.Lock()
	defer s.shims.mu.Unlock()
	s.shims.watcherCount++
	return &watcherShim{
		jujuAPIShim: s,
		stopped:     make(chan struct{}),
		initial:     s.initial,
	}, nil
}

type watcherShim struct {
	jujuAPIShim *jujuAPIShim
	mu          sync.Mutex
	stopped     chan struct{}
	initial     []multiwatcher.Delta
}

func (s *watcherShim) Next() ([]multiwatcher.Delta, error) {
	s.jujuAPIShim.shims.mu.Lock()
	d := s.initial
	s.initial = nil
	s.jujuAPIShim.shims.mu.Unlock()
	if len(d) > 0 {
		return d, nil
	}
	<-s.stopped
	return nil, &jujuparams.Error{
		Message: "fake watcher was stopped",
		Code:    jujuparams.CodeStopped,
	}
}

func (s *watcherShim) Stop() error {
	close(s.stopped)
	s.jujuAPIShim.shims.mu.Lock()
	s.jujuAPIShim.shims.watcherCount--
	s.jujuAPIShim.shims.mu.Unlock()
	return nil
}
