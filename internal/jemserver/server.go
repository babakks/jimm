// Copyright 2015 Canonical Ltd.

package jemserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"time"

	"github.com/juju/aclstore"
	"github.com/juju/juju/cloud"
	"github.com/juju/simplekv/mgosimplekv"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	candidclient "gopkg.in/CanonicalLtd/candidclient.v1"
	"gopkg.in/errgo.v1"
	"gopkg.in/httprequest.v1"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	"gopkg.in/macaroon-bakery.v2/bakery/mgorootkeystore"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
	"gopkg.in/mgo.v2"

	"github.com/CanonicalLtd/jimm/internal/auth"
	"github.com/CanonicalLtd/jimm/internal/jem"
	"github.com/CanonicalLtd/jimm/internal/jemerror"
	"github.com/CanonicalLtd/jimm/internal/mgosession"
	"github.com/CanonicalLtd/jimm/internal/monitor"
	"github.com/CanonicalLtd/jimm/internal/usagesender"
	"github.com/CanonicalLtd/jimm/internal/zapctx"
	"github.com/CanonicalLtd/jimm/internal/zaputil"
	"github.com/CanonicalLtd/jimm/params"
)

var (
	usageSenderPeriod = 5 * time.Minute
)

// NewAPIHandlerFunc is a function that returns set of httprequest
// handlers that uses the given JEM pool and server params.
type NewAPIHandlerFunc func(context.Context, HandlerParams) ([]httprequest.Handler, error)

// Params holds configuration for a new API server.
// It must be kept in sync with identical definition in the
// top level jem package.
type Params struct {
	// DB holds the mongo database that will be used to
	// store the JEM information.
	DB *mgo.Database

	// MaxMgoSessions holds the maximum number of sessions
	// that will be held in the pool. The actual number of sessions
	// may temporarily go above this.
	MaxMgoSessions int

	// ControllerAdmin holds the identity of the user
	// or group that is allowed to create controllers.
	ControllerAdmin params.User

	// IdentityLocation holds the location of the third party identity service.
	IdentityLocation string

	// CharmstoreLocation holds the location of the charmstore
	// associated with the controller.
	CharmstoreLocation string

	// MeteringLocation holds the location of the metering service
	// associated with the controller.
	MeteringLocation string

	// ThirdPartyLocator holds a third-party info store. It may be
	// nil.
	ThirdPartyLocator bakery.ThirdPartyLocator

	// AgentUsername and AgentKey hold the credentials used for agent
	// authentication.
	AgentUsername string
	AgentKey      *bakery.KeyPair

	// RunMonitor specifies that the monitor worker should be run.
	// This should always be set when running the server in production.
	RunMonitor bool

	// ControllerUUID holds the UUID the JIMM controller uses to
	// identify itself.
	ControllerUUID string

	// WebsocketRequestTimeout is the time to wait before failing a
	// connection because the server has not received a request.
	WebsocketRequestTimeout time.Duration

	// GUILocation holds the address that serves the GUI that will be
	// used with this controller.
	GUILocation string

	// UsageSenderURL holds the URL where we obtain authorization
	// to collect and report usage metrics.
	UsageSenderURL string

	// UsageSenderSpoolPath holds the path to a directory where the usage
	// send worker will store metrics.
	UsageSenderSpoolPath string

	// Domain holds the domain to which users must belong, not
	// including the leading "@". If this is empty, users may be in
	// any domain.
	Domain string

	// PublicCloudMetadata contains the path of the file containing
	// the public cloud metadata. If this is empty or the file
	// doesn't exist the default public cloud information is used.
	PublicCloudMetadata string
}

// HandlerParams are the parameters used to initialize a handler.
type HandlerParams struct {
	Params

	// SessionPool contains the pool of mgo sessions.
	SessionPool *mgosession.Pool

	// JEMPool contains the pool of JEM instances.
	JEMPool *jem.Pool

	// Authenticator contains the authenticator to use to
	// authenticate requests.
	Authenticator *auth.Authenticator

	// ACLManager contains the manager for the ACLs.
	ACLManager *aclstore.Manager
}

// Server represents a JEM HTTP server.
type Server struct {
	router          *httprouter.Router
	context         context.Context
	pool            *jem.Pool
	auth            *auth.Authenticator
	sessionPool     *mgosession.Pool
	monitor         *monitor.Monitor
	usageSender     worker.Worker
	jemModelStats   *jem.ModelStats
	jemMachineStats *jem.MachineStats
}

// New returns a new handler that handles model manager
// requests and stores its data in the given database.
// The returned handler should be closed when finished
// with.
func New(ctx context.Context, config Params, versions map[string]NewAPIHandlerFunc) (*Server, error) {
	if len(versions) == 0 {
		return nil, errgo.Newf("JEM server must serve at least one version of the API")
	}
	if config.MaxMgoSessions <= 0 {
		config.MaxMgoSessions = 1
	}
	identityClient, bclient, err := newIdentityClient(config)
	if err != nil {
		return nil, errgo.Mask(err)
	}

	key, err := bakery.GenerateKey()
	if err != nil {
		return nil, errgo.Mask(err)
	}

	sessionPool := mgosession.NewPool(ctx, config.DB.Session, config.MaxMgoSessions)
	var publicCloudMetadataPath []string
	if config.PublicCloudMetadata != "" {
		publicCloudMetadataPath = append(publicCloudMetadataPath, config.PublicCloudMetadata)
	}
	publicCloudMetadata, _, err := cloud.PublicCloudMetadata(publicCloudMetadataPath...)
	if err != nil {
		return nil, errgo.Notef(err, "cannot load public cloud metadata")
	}
	jconfig := jem.Params{
		DB:                  config.DB,
		SessionPool:         sessionPool,
		ControllerAdmin:     config.ControllerAdmin,
		UsageSenderURL:      config.UsageSenderURL,
		Client:              bclient,
		PublicCloudMetadata: publicCloudMetadata,
	}
	p, err := jem.NewPool(ctx, jconfig)
	if err != nil {
		return nil, errgo.Notef(err, "cannot make store")
	}
	jem := p.JEM(ctx)
	defer jem.Close()

	bakery := identchecker.NewBakery(identchecker.BakeryParams{
		RootKeyStore: auth.NewRootKeyStore(auth.RootKeyStoreParams{
			Pool:     sessionPool,
			RootKeys: mgorootkeystore.NewRootKeys(100),
			Policy: mgorootkeystore.Policy{
				ExpiryDuration: 24 * time.Hour,
			},
			Collection: jem.DB.Macaroons(),
		}),

		Locator:        config.ThirdPartyLocator,
		Key:            key,
		IdentityClient: identityClient,
		Authorizer: identchecker.ACLAuthorizer{
			GetACL: func(ctx context.Context, op bakery.Op) (acl []string, allowPublic bool, err error) {
				if op == identchecker.LoginOp {
					return []string{identchecker.Everyone}, false, nil
				}
				return nil, false, nil
			},
		},

		// TODO The location is attached to any macaroons that we
		// mint. Currently we don't know the location of the current
		// service. We potentially provide a way to configure this,
		// but it probably doesn't matter, as nothing currently uses
		// the macaroon location for anything.
		Location: "jimm",

		// TODO(mhilton): work out how to make the logger better.
		Logger: nil,
	})

	kvstore, err := mgosimplekv.NewStore(config.DB.C("acls"))
	if err != nil {
		return nil, errgo.Notef(err, "cannot create ACL store")
	}
	aclStore := aclstore.NewACLStore(kvstore)

	authenticator := auth.NewAuthenticator(bakery)

	aclManager, err := aclstore.NewManager(ctx, aclstore.Params{
		Store:    aclStore,
		RootPath: "/admin/acls",
		Authenticate: func(ctx context.Context, w http.ResponseWriter, req *http.Request) (aclstore.Identity, error) {
			ctx, err := authenticator.AuthenticateRequest(ctx, req)
			if err != nil {
				status, body := jemerror.Mapper(ctx, err)
				httprequest.WriteJSON(w, status, body)
				return nil, errgo.Mask(err, errgo.Any)
			}
			return identity{ctx}, nil
		},
		InitialAdminUsers: []string{string(config.ControllerAdmin)},
	})
	if err != nil {
		return nil, errgo.Mask(err)
	}
	srv := &Server{
		router:      httprouter.New(),
		auth:        authenticator,
		pool:        p,
		sessionPool: sessionPool,
		context:     ctx,
	}
	if config.RunMonitor {
		owner, err := monitorLeaseOwner(config.AgentUsername)
		if err != nil {
			return nil, errgo.Mask(err)
		}
		srv.monitor = monitor.New(ctx, p, owner)
	}
	if config.UsageSenderURL != "" {
		worker, err := usagesender.NewSendModelUsageWorker(usagesender.SendModelUsageWorkerConfig{
			OmnibusURL:     config.UsageSenderURL,
			Pool:           p,
			Period:         usageSenderPeriod,
			Context:        ctx,
			SpoolDirectory: config.UsageSenderSpoolPath,
		})
		if err != nil {
			return nil, errgo.Mask(err)
		}
		srv.usageSender = worker
	}
	srv.router.Handler("GET", "/admin/acls/*path", aclManager)
	srv.router.Handler("POST", "/admin/acls/*path", aclManager)
	srv.router.Handler("PUT", "/admin/acls/*path", aclManager)
	srv.router.Handler("GET", "/metrics", promhttp.Handler())
	for name, newAPI := range versions {
		handlers, err := newAPI(ctx, HandlerParams{
			Params:        config,
			SessionPool:   sessionPool,
			JEMPool:       p,
			Authenticator: authenticator,
			ACLManager:    aclManager,
		})
		if err != nil {
			return nil, errgo.Notef(err, "cannot create API %s", name)
		}
		for _, h := range handlers {
			srv.router.Handle(h.Method, h.Path, h.Handle)
			l, _, _ := srv.router.Lookup("OPTIONS", h.Path)
			if l == nil {
				srv.router.OPTIONS(h.Path, srv.options)
			}
		}
	}

	srv.jemModelStats = p.ModelStats(ctx)
	if err := prometheus.Register(srv.jemModelStats); err != nil {
		// This happens when the stats have already been registered. In
		// this case, we don't care much - just let the first one work.
		// This is useful to enable tests that use more than one Server
		// at the same time.
		zapctx.Error(ctx, "cannot register JEM model prometheus stats", zaputil.Error(err))
		srv.jemModelStats = nil
	}

	srv.jemMachineStats = p.MachineStats(ctx)
	if err := prometheus.Register(srv.jemMachineStats); err != nil {
		// This happens when the stats have already been registered. In
		// this case, we don't care much - just let the first one work.
		// This is useful to enable tests that use more than one Server
		// at the same time.
		zapctx.Error(ctx, "cannot register JEM machine prometheus stats", zaputil.Error(err))
		srv.jemMachineStats = nil
	}

	return srv, nil
}

type identity struct {
	ctx context.Context
}

func (i identity) Allow(_ context.Context, acl []string) (bool, error) {
	if err := auth.CheckACL(i.ctx, acl); err != nil {
		if errgo.Cause(err) == params.ErrUnauthorized {
			return false, nil
		}
		return false, errgo.Mask(err)
	}
	return true, nil
}

func monitorLeaseOwner(agentName string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errgo.Notef(err, "cannot make random owner")
	}
	return fmt.Sprintf("%s-%x", agentName, buf), nil
}

func newIdentityClient(config Params) (*auth.IdentityClient, *httpbakery.Client, error) {
	// Note: no need for persistent cookies as we'll
	// be able to recreate the macaroons on startup.
	bclient := httpbakery.NewClient()
	bclient.Key = config.AgentKey
	client, err := candidclient.New(candidclient.NewParams{
		BaseURL:       config.IdentityLocation,
		Client:        bclient,
		AgentUsername: config.AgentUsername,
		CacheTime:     10 * time.Minute,
	})
	if err != nil {
		return nil, nil, errgo.Notef(err, "cannot create IDM client")
	}
	return auth.NewIdentityClient(auth.IdentityClientParams{
		CandidClient: client,
		Domain:       config.Domain,
	}), bclient, nil
}

// ServeHTTP implements http.Handler.Handle.
func (srv *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	header := w.Header()
	ao := "*"
	if o := req.Header.Get("Origin"); o != "" {
		ao = o
	}
	header.Set("Access-Control-Allow-Origin", ao)
	header.Set("Access-Control-Allow-Headers", "Bakery-Protocol-Version, Macaroons, X-Requested-With, Content-Type")
	header.Set("Access-Control-Allow-Credentials", "true")
	header.Set("Access-Control-Cache-Max-Age", "600")
	// TODO: in handlers, look up methods for this request path and return only those methods here.
	header.Set("Access-Control-Allow-Methods", "DELETE,GET,HEAD,PUT,POST,OPTIONS")
	header.Set("Access-Control-Expose-Headers", "WWW-Authenticate")
	srv.router.ServeHTTP(w, req)
}

func (srv *Server) options(http.ResponseWriter, *http.Request, httprouter.Params) {
	// We don't need to do anything here because all the headers
	// required by OPTIONS are added for every request anyway.
}

// Close implements io.Closer.Close. It should not be called
// until all requests on the handler have completed.
func (srv *Server) Close() error {
	if srv.jemModelStats != nil {
		prometheus.Unregister(srv.jemModelStats)
	}
	if srv.jemMachineStats != nil {
		prometheus.Unregister(srv.jemMachineStats)
	}
	if srv.monitor != nil {
		srv.monitor.Kill()
		if err := srv.monitor.Wait(); err != nil {
			zapctx.Warn(srv.context, "error shutting down monitor", zaputil.Error(err))
		}
	}
	if srv.usageSender != nil {
		if err := worker.Stop(srv.usageSender); err != nil {
			zapctx.Warn(srv.context, "error shutting down usage sender", zaputil.Error(err))
		}
	}
	srv.pool.Close()
	srv.sessionPool.Close()
	return nil
}

// Pool returns the JEM pool used by the server.
// It is made available for testing purposes.
func (srv *Server) Pool() *jem.Pool {
	return srv.pool
}
