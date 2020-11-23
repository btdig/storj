// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information

package testplanet

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"storj.io/common/errs2"
	"storj.io/common/identity"
	"storj.io/common/memory"
	"storj.io/common/peertls/extensions"
	"storj.io/common/peertls/tlsopts"
	"storj.io/common/rpc"
	"storj.io/common/storj"
	"storj.io/common/uuid"
	"storj.io/private/debug"
	"storj.io/private/version"
	"storj.io/storj/pkg/revocation"
	"storj.io/storj/pkg/server"
	versionchecker "storj.io/storj/private/version/checker"
	"storj.io/storj/private/web"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/accounting"
	"storj.io/storj/satellite/accounting/live"
	"storj.io/storj/satellite/accounting/projectbwcleanup"
	"storj.io/storj/satellite/accounting/reportedrollup"
	"storj.io/storj/satellite/accounting/rollup"
	"storj.io/storj/satellite/accounting/tally"
	"storj.io/storj/satellite/admin"
	"storj.io/storj/satellite/audit"
	"storj.io/storj/satellite/console"
	"storj.io/storj/satellite/console/consoleauth"
	"storj.io/storj/satellite/console/consoleweb"
	"storj.io/storj/satellite/contact"
	"storj.io/storj/satellite/dbcleanup"
	"storj.io/storj/satellite/downtime"
	"storj.io/storj/satellite/gc"
	"storj.io/storj/satellite/gracefulexit"
	"storj.io/storj/satellite/inspector"
	"storj.io/storj/satellite/mailservice"
	"storj.io/storj/satellite/marketingweb"
	"storj.io/storj/satellite/metainfo"
	"storj.io/storj/satellite/metainfo/expireddeletion"
	"storj.io/storj/satellite/metainfo/objectdeletion"
	"storj.io/storj/satellite/metainfo/piecedeletion"
	"storj.io/storj/satellite/metrics"
	"storj.io/storj/satellite/nodestats"
	"storj.io/storj/satellite/orders"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/payments/paymentsconfig"
	"storj.io/storj/satellite/payments/stripecoinpayments"
	"storj.io/storj/satellite/repair/checker"
	"storj.io/storj/satellite/repair/irreparable"
	"storj.io/storj/satellite/repair/repairer"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
	"storj.io/storj/storage/redis/redisserver"
)

// Satellite contains all the processes needed to run a full Satellite setup.
type Satellite struct {
	Name   string
	Config satellite.Config

	Core     *satellite.Core
	API      *satellite.API
	Repairer *satellite.Repairer
	Admin    *satellite.Admin
	GC       *satellite.GarbageCollection

	Log      *zap.Logger
	Identity *identity.FullIdentity
	DB       satellite.DB

	Dialer rpc.Dialer

	Server *server.Server

	Version *versionchecker.Service

	Contact struct {
		Service  *contact.Service
		Endpoint *contact.Endpoint
	}

	Overlay struct {
		DB        overlay.DB
		Service   *overlay.Service
		Inspector *overlay.Inspector
	}

	Metainfo struct {
		Database  metainfo.PointerDB
		Service   *metainfo.Service
		Endpoint2 *metainfo.Endpoint
		Loop      *metainfo.Loop
	}

	Inspector struct {
		Endpoint *inspector.Endpoint
	}

	Orders struct {
		DB       orders.DB
		Endpoint *orders.Endpoint
		Service  *orders.Service
		Chore    *orders.Chore
	}

	Repair struct {
		Checker   *checker.Checker
		Repairer  *repairer.Service
		Inspector *irreparable.Inspector
	}
	Audit struct {
		Queues   *audit.Queues
		Worker   *audit.Worker
		Chore    *audit.Chore
		Verifier *audit.Verifier
		Reporter *audit.Reporter
	}

	GarbageCollection struct {
		Service *gc.Service
	}

	ExpiredDeletion struct {
		Chore *expireddeletion.Chore
	}

	DBCleanup struct {
		Chore *dbcleanup.Chore
	}

	Accounting struct {
		Tally            *tally.Service
		Rollup           *rollup.Service
		ProjectUsage     *accounting.Service
		ReportedRollup   *reportedrollup.Chore
		ProjectBWCleanup *projectbwcleanup.Chore
	}

	LiveAccounting struct {
		Cache accounting.Cache
	}

	ProjectLimits struct {
		Cache *accounting.ProjectLimitCache
	}

	Mail struct {
		Service *mailservice.Service
	}

	Console struct {
		Listener net.Listener
		Service  *console.Service
		Endpoint *consoleweb.Server
	}

	Marketing struct {
		Listener net.Listener
		Endpoint *marketingweb.Server
	}

	NodeStats struct {
		Endpoint *nodestats.Endpoint
	}

	GracefulExit struct {
		Chore    *gracefulexit.Chore
		Endpoint *gracefulexit.Endpoint
	}

	Metrics struct {
		Chore *metrics.Chore
	}

	DowntimeTracking struct {
		DetectionChore  *downtime.DetectionChore
		EstimationChore *downtime.EstimationChore
		Service         *downtime.Service
	}
}

// Label returns name for debugger.
func (system *Satellite) Label() string { return system.Name }

// ID returns the ID of the Satellite system.
func (system *Satellite) ID() storj.NodeID { return system.API.Identity.ID }

// Addr returns the public address from the Satellite system API.
func (system *Satellite) Addr() string { return system.API.Server.Addr().String() }

// URL returns the node url from the Satellite system API.
func (system *Satellite) URL() string { return system.NodeURL().String() }

// NodeURL returns the storj.NodeURL from the Satellite system API.
func (system *Satellite) NodeURL() storj.NodeURL {
	return storj.NodeURL{ID: system.API.ID(), Address: system.API.Addr()}
}

// AddUser adds user to a satellite. Password from newUser will be always overridden by FullName to have
// known password which can be used automatically.
func (system *Satellite) AddUser(ctx context.Context, newUser console.CreateUser, maxNumberOfProjects int) (*console.User, error) {
	regToken, err := system.API.Console.Service.CreateRegToken(ctx, maxNumberOfProjects)
	if err != nil {
		return nil, err
	}

	newUser.Password = newUser.FullName
	user, err := system.API.Console.Service.CreateUser(ctx, newUser, regToken.Secret, "")
	if err != nil {
		return nil, err
	}

	activationToken, err := system.API.Console.Service.GenerateActivationToken(ctx, user.ID, user.Email)
	if err != nil {
		return nil, err
	}

	err = system.API.Console.Service.ActivateAccount(ctx, activationToken)
	if err != nil {
		return nil, err
	}

	authCtx, err := system.AuthenticatedContext(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	err = system.API.Console.Service.Payments().SetupAccount(authCtx)
	if err != nil {
		return nil, err
	}

	return user, nil
}

// AddProject adds project to a satellite and makes specified user an owner.
func (system *Satellite) AddProject(ctx context.Context, ownerID uuid.UUID, name string) (*console.Project, error) {
	authCtx, err := system.AuthenticatedContext(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	project, err := system.API.Console.Service.CreateProject(authCtx, console.ProjectInfo{
		Name: name,
	})
	if err != nil {
		return nil, err
	}
	return project, nil
}

// AuthenticatedContext creates context with authentication date for given user.
func (system *Satellite) AuthenticatedContext(ctx context.Context, userID uuid.UUID) (context.Context, error) {
	user, err := system.API.Console.Service.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	// we are using full name as a password
	token, err := system.API.Console.Service.Token(ctx, user.Email, user.FullName)
	if err != nil {
		return nil, err
	}

	auth, err := system.API.Console.Service.Authorize(consoleauth.WithAPIKey(ctx, []byte(token)))
	if err != nil {
		return nil, err
	}

	return console.WithAuth(ctx, auth), nil
}

// Close closes all the subsystems in the Satellite system.
func (system *Satellite) Close() error {
	return errs.Combine(
		system.API.Close(),
		system.Core.Close(),
		system.Repairer.Close(),
		system.Admin.Close(),
		system.GC.Close(),
	)
}

// Run runs all the subsystems in the Satellite system.
func (system *Satellite) Run(ctx context.Context) (err error) {
	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		return errs2.IgnoreCanceled(system.Core.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(system.API.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(system.Repairer.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(system.Admin.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(system.GC.Run(ctx))
	})
	return group.Wait()
}

// PrivateAddr returns the private address from the Satellite system API.
func (system *Satellite) PrivateAddr() string { return system.API.Server.PrivateAddr().String() }

// newSatellites initializes satellites.
func (planet *Planet) newSatellites(ctx context.Context, count int, databases satellitedbtest.SatelliteDatabases) ([]*Satellite, error) {
	var satellites []*Satellite

	for i := 0; i < count; i++ {
		index := i
		prefix := "satellite" + strconv.Itoa(index)
		log := planet.log.Named(prefix)

		var system *Satellite
		var err error

		pprof.Do(ctx, pprof.Labels("peer", prefix), func(ctx context.Context) {
			system, err = planet.newSatellite(ctx, prefix, index, log, databases)
		})
		if err != nil {
			return nil, err
		}

		log.Debug("id=" + system.ID().String() + " addr=" + system.Addr())
		satellites = append(satellites, system)
		planet.peers = append(planet.peers, newClosablePeer(system))
	}

	return satellites, nil
}

func (planet *Planet) newSatellite(ctx context.Context, prefix string, index int, log *zap.Logger, databases satellitedbtest.SatelliteDatabases) (*Satellite, error) {
	storageDir := filepath.Join(planet.directory, prefix)
	if err := os.MkdirAll(storageDir, 0700); err != nil {
		return nil, err
	}

	identity, err := planet.NewIdentity()
	if err != nil {
		return nil, err
	}

	db, err := satellitedbtest.CreateMasterDB(ctx, log.Named("db"), planet.config.Name, "S", index, databases.MasterDB)
	if err != nil {
		return nil, err
	}

	if planet.config.Reconfigure.SatelliteDB != nil {
		var newdb satellite.DB
		newdb, err = planet.config.Reconfigure.SatelliteDB(log.Named("db"), index, db)
		if err != nil {
			return nil, errs.Combine(err, db.Close())
		}
		db = newdb
	}
	planet.databases = append(planet.databases, db)

	pointerDB, err := satellitedbtest.CreatePointerDB(ctx, log.Named("pointerdb"), planet.config.Name, "P", index, databases.PointerDB)
	if err != nil {
		return nil, err
	}

	if planet.config.Reconfigure.SatellitePointerDB != nil {
		var newPointerDB metainfo.PointerDB
		newPointerDB, err = planet.config.Reconfigure.SatellitePointerDB(log.Named("pointerdb"), index, pointerDB)
		if err != nil {
			return nil, errs.Combine(err, pointerDB.Close())
		}
		pointerDB = newPointerDB
	}
	planet.databases = append(planet.databases, pointerDB)

	redis, err := redisserver.Mini(ctx)
	if err != nil {
		return nil, err
	}
	planet.databases = append(planet.databases, redis)

	config := satellite.Config{
		Server: server.Config{
			Address:        "127.0.0.1:0",
			PrivateAddress: "127.0.0.1:0",

			Config: tlsopts.Config{
				RevocationDBURL:     "bolt://" + filepath.Join(storageDir, "revocation.db"),
				UsePeerCAWhitelist:  true,
				PeerCAWhitelistPath: planet.whitelistPath,
				PeerIDVersions:      "latest",
				Extensions: extensions.Config{
					Revocation:          false,
					WhitelistSignedLeaf: false,
				},
			},
		},
		Debug: debug.Config{
			Address: "",
		},
		Admin: admin.Config{
			Address: "127.0.0.1:0",
		},
		Contact: contact.Config{
			Timeout: 1 * time.Minute,
		},
		Overlay: overlay.Config{
			Node: overlay.NodeSelectionConfig{
				UptimeCount:      0,
				AuditCount:       0,
				NewNodeFraction:  1,
				OnlineWindow:     time.Minute,
				DistinctIP:       false,
				MinimumDiskSpace: 100 * memory.MB,

				AuditReputationRepairWeight: 1,
				AuditReputationUplinkWeight: 1,
				AuditReputationLambda:       0.95,
				AuditReputationWeight:       1,
				AuditReputationDQ:           0.6,
				SuspensionGracePeriod:       time.Hour,
				SuspensionDQEnabled:         true,
			},
			NodeSelectionCache: overlay.CacheConfig{
				Staleness: 3 * time.Minute,
			},
			UpdateStatsBatchSize: 100,
			AuditHistory: overlay.AuditHistoryConfig{
				WindowSize:       10 * time.Minute,
				TrackingPeriod:   time.Hour,
				GracePeriod:      time.Hour,
				OfflineThreshold: 0.6,
			},
		},
		Metainfo: metainfo.Config{
			DatabaseURL:          "", // not used
			MinRemoteSegmentSize: 0,  // TODO: fix tests to work with 1024
			MaxInlineSegmentSize: 4 * memory.KiB,
			MaxSegmentSize:       64 * memory.MiB,
			MaxMetadataSize:      2 * memory.KiB,
			MaxCommitInterval:    1 * time.Hour,
			Overlay:              true,
			RS: metainfo.RSConfig{
				ErasureShareSize: memory.Size(256),
				Min:              atLeastOne(planet.config.StorageNodeCount * 1 / 5),
				Repair:           atLeastOne(planet.config.StorageNodeCount * 2 / 5),
				Success:          atLeastOne(planet.config.StorageNodeCount * 3 / 5),
				Total:            atLeastOne(planet.config.StorageNodeCount * 4 / 5),
			},
			Loop: metainfo.LoopConfig{
				CoalesceDuration: 1 * time.Second,
				ListLimit:        10000,
			},
			RateLimiter: metainfo.RateLimiterConfig{
				Enabled:         true,
				Rate:            1000,
				CacheCapacity:   100,
				CacheExpiration: 10 * time.Second,
			},
			ProjectLimits: metainfo.ProjectLimitConfig{
				MaxBuckets:          10,
				DefaultMaxUsage:     25 * memory.GB,
				DefaultMaxBandwidth: 25 * memory.GB,
			},
			PieceDeletion: piecedeletion.Config{
				MaxConcurrency:      100,
				MaxConcurrentPieces: 1000,

				MaxPiecesPerBatch:   4000,
				MaxPiecesPerRequest: 2000,

				DialTimeout:    2 * time.Second,
				RequestTimeout: 2 * time.Second,
				FailThreshold:  2 * time.Second,
			},
			ObjectDeletion: objectdeletion.Config{
				MaxObjectsPerRequest:     100,
				ZombieSegmentsPerRequest: 3,
				MaxConcurrentRequests:    100,
			},
		},
		Orders: orders.Config{
			Expiration:                 7 * 24 * time.Hour,
			SettlementBatchSize:        10,
			FlushBatchSize:             10,
			FlushInterval:              defaultInterval,
			NodeStatusLogging:          true,
			WindowEndpointRolloutPhase: orders.WindowEndpointRolloutPhase3,
		},
		Checker: checker.Config{
			Interval:                  defaultInterval,
			IrreparableInterval:       defaultInterval,
			ReliabilityCacheStaleness: 1 * time.Minute,
		},
		Payments: paymentsconfig.Config{
			StorageTBPrice: "10",
			EgressTBPrice:  "45",
			ObjectPrice:    "0.0000022",
			StripeCoinPayments: stripecoinpayments.Config{
				TransactionUpdateInterval:    defaultInterval,
				AccountBalanceUpdateInterval: defaultInterval,
				ConversionRatesCycleInterval: defaultInterval,
				ListingLimit:                 100,
			},
			CouponDuration:    2,
			CouponValue:       275,
			PaywallProportion: 1,
		},
		Repairer: repairer.Config{
			MaxRepair:                     10,
			Interval:                      defaultInterval,
			Timeout:                       1 * time.Minute, // Repairs can take up to 10 seconds. Leaving room for outliers
			DownloadTimeout:               1 * time.Minute,
			TotalTimeout:                  10 * time.Minute,
			MaxBufferMem:                  4 * memory.MiB,
			MaxExcessRateOptimalThreshold: 0.05,
			InMemoryRepair:                false,
		},
		Audit: audit.Config{
			MaxRetriesStatDB:   0,
			MinBytesPerSecond:  1 * memory.KB,
			MinDownloadTimeout: 5 * time.Second,
			MaxReverifyCount:   3,
			ChoreInterval:      defaultInterval,
			QueueInterval:      defaultInterval,
			Slots:              3,
			WorkerConcurrency:  2,
		},
		GarbageCollection: gc.Config{
			Interval:          defaultInterval,
			Enabled:           true,
			InitialPieces:     10,
			FalsePositiveRate: 0.1,
			ConcurrentSends:   1,
			RunInCore:         false,
		},
		ExpiredDeletion: expireddeletion.Config{
			Interval: defaultInterval,
			Enabled:  true,
		},
		DBCleanup: dbcleanup.Config{
			SerialsInterval: defaultInterval,
			BatchSize:       1000,
		},
		Tally: tally.Config{
			Interval: defaultInterval,
		},
		Rollup: rollup.Config{
			Interval:      defaultInterval,
			DeleteTallies: false,
		},
		ReportedRollup: reportedrollup.Config{
			Interval: defaultInterval,
		},
		ProjectBWCleanup: projectbwcleanup.Config{
			Interval:     defaultInterval,
			RetainMonths: 1,
		},
		LiveAccounting: live.Config{
			StorageBackend:    "redis://" + redis.Addr() + "?db=0",
			BandwidthCacheTTL: 5 * time.Minute,
		},
		Mail: mailservice.Config{
			SMTPServerAddress: "smtp.mail.test:587",
			From:              "Labs <storj@mail.test>",
			AuthType:          "simulate",
			TemplatePath:      filepath.Join(developmentRoot, "web/satellite/static/emails"),
		},
		Console: consoleweb.Config{
			Address:         "127.0.0.1:0",
			StaticDir:       filepath.Join(developmentRoot, "web/satellite"),
			AuthToken:       "very-secret-token",
			AuthTokenSecret: "my-suppa-secret-key",
			Config: console.Config{
				PasswordCost:        console.TestPasswordCost,
				DefaultProjectLimit: 5,
			},
			RateLimit: web.IPRateLimiterConfig{
				Duration:  5 * time.Minute,
				Burst:     3,
				NumLimits: 10,
			},
		},
		Marketing: marketingweb.Config{
			Address:   "127.0.0.1:0",
			StaticDir: filepath.Join(developmentRoot, "web/marketing"),
		},
		Version: planet.NewVersionConfig(),
		GracefulExit: gracefulexit.Config{
			Enabled: true,

			ChoreBatchSize: 10,
			ChoreInterval:  defaultInterval,

			EndpointBatchSize:            100,
			MaxFailuresPerPiece:          5,
			MaxInactiveTimeFrame:         time.Second * 10,
			OverallMaxFailuresPercentage: 10,
			RecvTimeout:                  time.Minute * 1,
			MaxOrderLimitSendCount:       3,
			NodeMinAgeInMonths:           0,
		},
		Metrics: metrics.Config{
			ChoreInterval: defaultInterval,
		},
		Downtime: downtime.Config{
			DetectionInterval:          defaultInterval,
			EstimationInterval:         defaultInterval,
			EstimationBatchSize:        5,
			EstimationConcurrencyLimit: 5,
		},
	}

	if planet.ReferralManager != nil {
		config.Referrals.ReferralManagerURL = storj.NodeURL{
			ID:      planet.ReferralManager.Identity().ID,
			Address: planet.ReferralManager.Addr().String(),
		}
	}
	if planet.config.Reconfigure.Satellite != nil {
		planet.config.Reconfigure.Satellite(log, index, &config)
	}

	versionInfo := planet.NewVersionInfo()

	revocationDB, err := revocation.OpenDBFromCfg(ctx, config.Server.Config)
	if err != nil {
		return nil, errs.Wrap(err)
	}

	planet.databases = append(planet.databases, revocationDB)

	liveAccounting, err := live.NewCache(log.Named("live-accounting"), config.LiveAccounting)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	planet.databases = append(planet.databases, liveAccounting)

	rollupsWriteCache := orders.NewRollupsWriteCache(log.Named("orders-write-cache"), db.Orders(), config.Orders.FlushBatchSize)
	planet.databases = append(planet.databases, rollupsWriteCacheCloser{rollupsWriteCache})

	peer, err := satellite.New(log, identity, db, pointerDB, revocationDB, liveAccounting, rollupsWriteCache, versionInfo, &config, nil)
	if err != nil {
		return nil, err
	}

	err = db.TestingMigrateToLatest(ctx)
	if err != nil {
		return nil, err
	}

	api, err := planet.newAPI(ctx, index, identity, db, pointerDB, config, versionInfo)
	if err != nil {
		return nil, err
	}

	adminPeer, err := planet.newAdmin(ctx, index, identity, db, config, versionInfo)
	if err != nil {
		return nil, err
	}

	repairerPeer, err := planet.newRepairer(ctx, index, identity, db, pointerDB, config, versionInfo)
	if err != nil {
		return nil, err
	}

	gcPeer, err := planet.newGarbageCollection(ctx, index, identity, db, pointerDB, config, versionInfo)
	if err != nil {
		return nil, err
	}

	return createNewSystem(prefix, log, config, peer, api, repairerPeer, adminPeer, gcPeer), nil
}

// createNewSystem makes a new Satellite System and exposes the same interface from
// before we split out the API. In the short term this will help keep all the tests passing
// without much modification needed. However long term, we probably want to rework this
// so it represents how the satellite will run when it is made up of many prrocesses.
func createNewSystem(name string, log *zap.Logger, config satellite.Config, peer *satellite.Core, api *satellite.API, repairerPeer *satellite.Repairer, adminPeer *satellite.Admin, gcPeer *satellite.GarbageCollection) *Satellite {
	system := &Satellite{
		Name:     name,
		Config:   config,
		Core:     peer,
		API:      api,
		Repairer: repairerPeer,
		Admin:    adminPeer,
		GC:       gcPeer,
	}
	system.Log = log
	system.Identity = peer.Identity
	system.DB = api.DB

	system.Dialer = api.Dialer

	system.Contact.Service = api.Contact.Service
	system.Contact.Endpoint = api.Contact.Endpoint

	system.Overlay.DB = api.Overlay.DB
	system.Overlay.Service = api.Overlay.Service
	system.Overlay.Inspector = api.Overlay.Inspector

	system.Metainfo.Database = api.Metainfo.Database
	system.Metainfo.Service = peer.Metainfo.Service
	system.Metainfo.Endpoint2 = api.Metainfo.Endpoint2
	system.Metainfo.Loop = peer.Metainfo.Loop

	system.Inspector.Endpoint = api.Inspector.Endpoint

	system.Orders.DB = api.Orders.DB
	system.Orders.Endpoint = api.Orders.Endpoint
	system.Orders.Service = peer.Orders.Service
	system.Orders.Chore = api.Orders.Chore

	system.Repair.Checker = peer.Repair.Checker
	system.Repair.Repairer = repairerPeer.Repairer
	system.Repair.Inspector = api.Repair.Inspector

	system.Audit.Queues = peer.Audit.Queues
	system.Audit.Worker = peer.Audit.Worker
	system.Audit.Chore = peer.Audit.Chore
	system.Audit.Verifier = peer.Audit.Verifier
	system.Audit.Reporter = peer.Audit.Reporter

	system.GarbageCollection.Service = gcPeer.GarbageCollection.Service

	system.ExpiredDeletion.Chore = peer.ExpiredDeletion.Chore

	system.DBCleanup.Chore = peer.DBCleanup.Chore

	system.Accounting.Tally = peer.Accounting.Tally
	system.Accounting.Rollup = peer.Accounting.Rollup
	system.Accounting.ProjectUsage = api.Accounting.ProjectUsage
	system.Accounting.ReportedRollup = peer.Accounting.ReportedRollupChore
	system.Accounting.ProjectBWCleanup = peer.Accounting.ProjectBWCleanupChore

	system.LiveAccounting = peer.LiveAccounting

	system.ProjectLimits.Cache = api.ProjectLimits.Cache

	system.Marketing.Listener = api.Marketing.Listener
	system.Marketing.Endpoint = api.Marketing.Endpoint

	system.GracefulExit.Chore = peer.GracefulExit.Chore
	system.GracefulExit.Endpoint = api.GracefulExit.Endpoint

	system.Metrics.Chore = peer.Metrics.Chore

	system.DowntimeTracking.DetectionChore = peer.DowntimeTracking.DetectionChore
	system.DowntimeTracking.EstimationChore = peer.DowntimeTracking.EstimationChore
	system.DowntimeTracking.Service = peer.DowntimeTracking.Service

	return system
}

func (planet *Planet) newAPI(ctx context.Context, index int, identity *identity.FullIdentity, db satellite.DB, pointerDB metainfo.PointerDB, config satellite.Config, versionInfo version.Info) (*satellite.API, error) {
	prefix := "satellite-api" + strconv.Itoa(index)
	log := planet.log.Named(prefix)
	var err error

	revocationDB, err := revocation.OpenDBFromCfg(ctx, config.Server.Config)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	planet.databases = append(planet.databases, revocationDB)

	liveAccounting, err := live.NewCache(log.Named("live-accounting"), config.LiveAccounting)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	planet.databases = append(planet.databases, liveAccounting)

	rollupsWriteCache := orders.NewRollupsWriteCache(log.Named("orders-write-cache"), db.Orders(), config.Orders.FlushBatchSize)
	planet.databases = append(planet.databases, rollupsWriteCacheCloser{rollupsWriteCache})

	return satellite.NewAPI(log, identity, db, pointerDB, revocationDB, liveAccounting, rollupsWriteCache, &config, versionInfo, nil)
}

func (planet *Planet) newAdmin(ctx context.Context, index int, identity *identity.FullIdentity, db satellite.DB, config satellite.Config, versionInfo version.Info) (*satellite.Admin, error) {
	prefix := "satellite-admin" + strconv.Itoa(index)
	log := planet.log.Named(prefix)

	return satellite.NewAdmin(log, identity, db, versionInfo, &config, nil)
}

func (planet *Planet) newRepairer(ctx context.Context, index int, identity *identity.FullIdentity, db satellite.DB, pointerDB metainfo.PointerDB, config satellite.Config, versionInfo version.Info) (*satellite.Repairer, error) {
	prefix := "satellite-repairer" + strconv.Itoa(index)
	log := planet.log.Named(prefix)

	revocationDB, err := revocation.OpenDBFromCfg(ctx, config.Server.Config)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	planet.databases = append(planet.databases, revocationDB)

	rollupsWriteCache := orders.NewRollupsWriteCache(log.Named("orders-write-cache"), db.Orders(), config.Orders.FlushBatchSize)
	planet.databases = append(planet.databases, rollupsWriteCacheCloser{rollupsWriteCache})

	return satellite.NewRepairer(log, identity, pointerDB, revocationDB, db.RepairQueue(), db.Buckets(), db.OverlayCache(), rollupsWriteCache, db.Irreparable(), versionInfo, &config, nil)
}

type rollupsWriteCacheCloser struct {
	*orders.RollupsWriteCache
}

func (cache rollupsWriteCacheCloser) Close() error {
	return cache.RollupsWriteCache.CloseAndFlush(context.TODO())
}

func (planet *Planet) newGarbageCollection(ctx context.Context, index int, identity *identity.FullIdentity, db satellite.DB, pointerDB metainfo.PointerDB, config satellite.Config, versionInfo version.Info) (*satellite.GarbageCollection, error) {
	prefix := "satellite-gc" + strconv.Itoa(index)
	log := planet.log.Named(prefix)

	revocationDB, err := revocation.OpenDBFromCfg(ctx, config.Server.Config)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	planet.databases = append(planet.databases, revocationDB)
	return satellite.NewGarbageCollection(log, identity, db, pointerDB, revocationDB, versionInfo, &config, nil)
}

// atLeastOne returns 1 if value < 1, or value otherwise.
func atLeastOne(value int) int {
	if value < 1 {
		return 1
	}
	return value
}
