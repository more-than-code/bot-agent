// Package run is the phase-1 bot-agent daemon loop.
package run

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/collect"
	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/conn"
	"github.com/more-than-code/bot-agent/internal/agent/discover"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/spool"
)

// Pace overrides clamped intervals when a stored duration is positive.
// Tests may change the values while the agent is running. Production leaves
// the pointer nil. The values are nanoseconds.
type Pace struct {
	Metrics atomic.Int64
	Process atomic.Int64
	Health  atomic.Int64
}

// Options configures a daemon. Zero values use the production defaults.
// DiscoverEvery, ServicesRefreshEvery, Pace, OnTaskStart, Sleep, EUID, Now,
// After, OnSend, and the roots exist so tests can run without root, without
// /proc, and without waiting out the contract clamps. They are not
// environment variables. After, when set, replaces the session loop's wall
// timer; nil uses time.NewTimer. OnSend, when set, runs at the start of each
// client Send; nil is production.
type Options struct {
	EnvFile              string
	ProcRoot             string
	SysRoot              string
	Passwd               string
	StatFS               collect.StatFS
	EUID                 func() int
	Now                  func() time.Time
	After                func(time.Duration) <-chan time.Time
	OnSend               func(any) error
	Sleep                func(context.Context, time.Duration) error
	DiscoverEvery        time.Duration
	ServicesRefreshEvery time.Duration
	Pace                 *Pace
	OnTaskStart          func()
	Log                  *log.Logger
	// PolicyPath is the runner policy. Empty means /etc/bot-agent/runner.toml.
	// PolicyUID overrides the owner check in tests; production leaves it nil.
	// Helper, when set, replaces the socket dial. HelperPath is the socket
	// path. Empty means /run/bot-agent-runner.sock.
	PolicyPath string
	GroupPath  string
	HelperPath string
	PolicyUID  func(os.FileInfo) (uint32, bool)
	// PolicyExec is the filesystem the v1.18 executable check reads. Nil
	// is the host's; only tests set it.
	PolicyExec policy.ExecFS
	Helper     Helper
	// MachineID is the file whose contents feed the container host label.
	// Empty means /etc/machine-id. Tests set a fixture path.
	MachineID string
	// SuperviseEvery is the health supervisor poll. Zero means 15s.
	SuperviseEvery time.Duration
	// HostFilesEvery is the host_files_status period. Zero means 10 minutes.
	HostFilesEvery time.Duration
	// UpgradePollEvery and UpgradePollFor are the v1.20 result polling
	// period and window. Zero means 10 s and 15 minutes.
	UpgradePollEvery time.Duration
	UpgradePollFor   time.Duration
}

func (o Options) withDefaults() Options {
	if o.EUID == nil {
		o.EUID = os.Geteuid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Log == nil {
		o.Log = log.Default()
	}
	if o.ProcRoot == "" {
		o.ProcRoot = "/proc"
	}
	if o.SysRoot == "" {
		o.SysRoot = "/sys"
	}
	if o.Passwd == "" {
		o.Passwd = "/etc/passwd"
	}
	if o.DiscoverEvery <= 0 {
		o.DiscoverEvery = 60 * time.Second
	}
	if o.SuperviseEvery <= 0 {
		o.SuperviseEvery = superviseEvery
	}
	if o.ServicesRefreshEvery <= 0 {
		o.ServicesRefreshEvery = 300 * time.Second
	}
	if o.GroupPath == "" {
		o.GroupPath = policy.DefaultGroup
	}
	if o.HelperPath == "" {
		o.HelperPath = defaultSocketPath
	}
	if o.Helper == nil {
		o.Helper = socketHelper{path: o.HelperPath}
	}
	return o
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run dials the server until ctx is cancelled. A forbidden credential does
// not reconnect; the process waits for shutdown so a supervisor does not
// restart it in a loop.
func Run(ctx context.Context, opt Options) error {
	opt = opt.withDefaults()
	if opt.EUID() == 0 {
		return errors.New("refusing to run as root")
	}
	pol := policy.Load(policy.Files{
		Policy: opt.PolicyPath, Passwd: opt.Passwd, Group: opt.GroupPath, UID: opt.PolicyUID,
		Exec: opt.PolicyExec,
	})
	if pol.Exists && !pol.Valid {
		opt.Log.Printf("bot-agent: runner policy invalid: %s", quoteLog(pol.Reason))
	}
	for _, d := range pol.Dropped {
		opt.Log.Printf("bot-agent: runner policy dropped run_as %s", quoteLog(d.Name))
	}
	path := opt.EnvFile
	if path == "" {
		if v := os.Getenv("BOT_AGENT_ENV_FILE"); v != "" {
			path = v
		} else {
			path = config.DefaultEnvFile
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	host, err := hostname(cfg)
	if err != nil {
		return err
	}
	if cfg.HasCredential() && cfg.DiskHasJoin() {
		if err := cfg.PersistEnrolled(cfg.HostID, cfg.CredentialID, cfg.Credential); err != nil {
			opt.Log.Printf("bot-agent: could not clear join token")
		}
	}
	sp, err := spool.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	a := &agent{
		opt:    opt,
		cfg:    cfg,
		host:   host,
		spool:  sp,
		policy: pol,
		col: collect.New(collect.Roots{
			Proc: opt.ProcRoot, Sys: opt.SysRoot, Passwd: opt.Passwd,
		}, opt.StatFS),
		bo:        conn.NewBackoff(),
		healthCh:  make(chan protocol.Sample, 128),
		taskSlots: make(chan struct{}, maxRunningTasks),
	}
	a.life = ctx
	// v1.20 item 2: the helper's last upgrade result, read once at start.
	a.noteUpgradeResult(a.readUpgradeResult(ctx))
	if pol.AgentUpgradeEnabled() {
		a.startUpgradePoll()
	}
	a.noteUnlistedUnits(ctx)
	if pol.ImagesEnabled() {
		go a.supervise(ctx)
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, conn.ErrReconnect) {
			a.bo.Reset()
			continue
		}
		if errors.Is(err, conn.ErrForbidden) {
			opt.Log.Printf("bot-agent: credential rejected; not retrying")
			<-ctx.Done()
			return nil
		}
		opt.Log.Printf("bot-agent: connection ended (%s)", conn.Safe(err, a.cfg.Credential, a.cfg.JoinToken))
		if errors.Is(err, conn.ErrUnauthenticated) {
			a.bo.Set(30 * time.Second)
		}
		if err := opt.Sleep(ctx, a.bo.Take()); err != nil {
			return nil
		}
	}
}

func hostname(cfg config.Config) (string, error) {
	h := cfg.Hostname
	if h == "" {
		var err error
		h, err = os.Hostname()
		if err != nil || h == "" {
			return "", errors.New("hostname is empty")
		}
	}
	return config.SanitizeHostname(h)
}

type agent struct {
	opt    Options
	cfg    config.Config
	host   string
	spool  *spool.Spool
	col    *collect.Collector
	policy policy.Policy
	bo     conn.Backoff

	mu       sync.Mutex
	disc     discover.Result
	services []protocol.Discovered
	offset   time.Duration
	checks   []protocol.HealthCheck

	metricsEvery     time.Duration
	procEvery        time.Duration
	healthEvery      time.Duration
	pingEvery        time.Duration
	configured       []protocol.HealthCheck
	lastCanon        string
	lastServicesSent time.Time

	unmappedOnce  atomic.Bool
	ownedOnce     atomic.Bool
	containerOnce atomic.Bool
	listCutOnce   atomic.Bool
	// docker is the hello.programs.docker state (agent-protocol v1.17
	// item 3). Only discovery and the session loop touch it, under mu.
	docker dockerFlag
	// helperKind is the task kind while a task is talking to the helper.
	// Empty during discovery, so those calls are not logged as task failures.
	helperKind   string
	idOnce       sync.Once
	idLabel      string
	idMiss       bool
	idLogOnce    atomic.Bool
	healthCh     chan protocol.Sample
	healthCancel context.CancelFunc
	// dockerHealth is config.docker_health. Docker health samples are queued
	// only while it is true. dockerPending is drained into the next metrics batch.
	dockerHealth  bool
	dockerPending []protocol.Sample
	pending       pendingTasks
	inflight      inflight
	taskSlots     chan struct{}
	taskMu        sync.Mutex
	taskRunning   int
	taskWaiting   int
	// entries is the per-entry image lock; super and autoq are the v1.9
	// health supervisor state and its unacked auto_action frames.
	entries entryLocks
	super   supervisor
	autoq   autoQueue
	// autoSent is the auto_action ids sent on the current connection.
	// Only the session goroutine touches it.
	autoSent map[string]bool
	// hostFilesWake is signalled after each host_file_apply so the session
	// sends host_files_status (agent-protocol v1.18 item 5).
	hostFilesWake wakeSignal
	// life is Run's context; the v1.20 result poller outlives a session.
	life context.Context
	// lastUpgrade is the helper's last upgrade result (v1.20), under mu.
	// upgradeSeq counts new results; upgradeSent is the last one the
	// server got, in hello or the agent_upgrade_result frame.
	lastUpgrade *agentupgrade.Result
	upgradeSeq  uint64
	upgradeSent uint64
	upgradeWake wakeSignal
	markerOnce  atomic.Bool
	// polling, pollUntil and pollSawNew are the result poller's state,
	// under mu.
	polling    bool
	pollUntil  time.Time
	pollSawNew bool
}

type writer struct {
	c      *websocket.Conn
	mu     sync.Mutex
	onSend func(any) error
}

func (w *writer) Send(ctx context.Context, v any) error {
	if w.onSend != nil {
		if err := w.onSend(v); err != nil {
			return err
		}
	}
	b, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	return w.SendRaw(ctx, b)
}

func (w *writer) SendRaw(ctx context.Context, b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return conn.WriteRaw(ctx, w.c, b)
}
