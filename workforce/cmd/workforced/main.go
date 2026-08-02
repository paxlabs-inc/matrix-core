package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	neoprovider "matrix/neo/provider"
	"matrix/vault"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/audit"
	"matrix/workforce/internal/businessoutcome"
	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/commercialcapability"
	"matrix/workforce/internal/commercialexecution"
	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/companyrecovery"
	"matrix/workforce/internal/companyruntime"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/controlapi"
	"matrix/workforce/internal/departmentadapter"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/developer"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/execution"
	"matrix/workforce/internal/founderprojection"
	"matrix/workforce/internal/initiative"
	"matrix/workforce/internal/learning"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/lineage"
	"matrix/workforce/internal/mail"
	"matrix/workforce/internal/mission"
	"matrix/workforce/internal/modelclient"
	"matrix/workforce/internal/organization"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/productcapability"
	"matrix/workforce/internal/productexecution"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/provider/customer"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/provider/financial"
	"matrix/workforce/internal/securityqualification"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/squad"
	"matrix/workforce/internal/wakeruntime"
	"matrix/workforce/internal/workcompile"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer cancel()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("workforced", flag.ContinueOnError)
	flags.SetOutput(stderr)
	versionOnly := flags.Bool("version", false, "print the workforced build version")
	serve := flags.Bool("serve", false, "serve the authenticated Workforce control plane")
	listen := flags.String("listen", envOr("WORKFORCE_LISTEN", "127.0.0.1:8091"), "HTTP listen address")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		printUsage(stderr)
		return 2
	}
	if *versionOnly {
		if *serve {
			printUsage(stderr)
			return 2
		}
		fmt.Fprintln(stdout, version)
		return 0
	}
	if !*serve {
		printUsage(stderr)
		return 2
	}
	config, err := loadConfig(*listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	pool, err := pgxpool.New(ctx, config.postgresURI)
	if err != nil {
		fmt.Fprintln(stderr, "workforced: connect:", err)
		return 1
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintln(stderr, "workforced: ping:", err)
		return 1
	}
	now := time.Now().UTC()
	if err := ledger.ApplyMigrations(ctx, pool, now); err != nil {
		fmt.Fprintln(stderr, "workforced: migrate:", err)
		return 1
	}
	vaultSession, err := vault.Boot(
		ctx, vault.ConfigFromEnv(config.dataDir, config.tenantID),
	)
	if err != nil || vaultSession.UserVault() == nil {
		fmt.Fprintln(stderr, "workforced: encrypted Vault is required")
		return 1
	}
	principal := controlapi.Principal{
		TenantID: config.tenantID, OrganizationID: config.organizationID,
		OwnerID: config.ownerID,
	}
	auth, err := controlapi.NewStaticAuthenticator(
		map[string]controlapi.Principal{config.ownerToken: principal},
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	service, err := controlapi.New(
		pool, auth,
		map[string]controlapi.OwnerKey{
			config.tenantID: {
				KeyID: config.ownerKeyID, PublicKey: config.ownerPublicKey,
			},
		},
		func() time.Time { return time.Now().UTC() }, 256,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachVault(vaultSession.UserVault()); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachRuntimeAuthority(
		config.runtimeKeyID, config.runtimePublicKey,
	); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachCompanyIssuerAuthority(
		config.companyIssuerKeyID, config.companyIssuerPublicKey,
	); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachRuntimeModel(
		config.model.Provider, config.model.ModelID,
	); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	schedulerStore, err := scheduler.New(
		pool, vaultSession.UserVault(), config.tenantID,
		config.scheduler, func() time.Time { return time.Now().UTC() },
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachScheduler(schedulerStore); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	nowFunc := func() time.Time { return time.Now().UTC() }
	companyRecoveryStore, err := companyrecovery.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID,
		companyrecovery.Authority{
			FounderKeyID:      config.ownerKeyID,
			FounderPublicKey:  config.ownerPublicKey,
			RuntimeKeyID:      config.runtimeKeyID,
			RuntimePrivateKey: config.runtimeKey,
		},
		nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if config.recoveryBackend != "" {
		recoveryBackend, backendErr := companyrecovery.NewCommandBackend(
			config.recoveryBackend, config.recoveryBackendArgs, config.recoveryBackendTimeout,
		)
		if backendErr != nil {
			fmt.Fprintln(stderr, backendErr)
			return 2
		}
		if backendErr = companyRecoveryStore.AttachPITRBackend(recoveryBackend); backendErr != nil {
			fmt.Fprintln(stderr, backendErr)
			return 2
		}
		if backendErr = companyRecoveryStore.AttachErasureBackend(recoveryBackend); backendErr != nil {
			fmt.Fprintln(stderr, backendErr)
			return 2
		}
	}
	if err := service.AttachCompanyRecovery(companyRecoveryStore); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	founderProjectionStore, err := founderprojection.NewStore(
		pool, config.tenantID, config.organizationID, config.ownerID,
		config.runtimeKeyID, config.runtimeKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachFounderProjection(founderProjectionStore); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	securityStore, err := securityqualification.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID,
		config.runtimeKeyID, config.runtimePublicKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachSecurityQualification(
		securityStore, config.runtimeKeyID, config.runtimeKey,
	); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	learningStore, err := learning.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID,
		config.runtimeKeyID, config.runtimePublicKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	learningEngine, err := learning.NewEngine(config.runtimeKeyID, config.runtimeKey)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachLearning(learningStore, learningEngine); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	missionStore, err := mission.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID,
		config.ownerID, config.ownerKeyID, config.ownerPublicKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	portfolioStore, err := portfolio.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID,
		config.companyIssuerKeyID, config.companyIssuerKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	companyStore, err := companyruntime.NewStore(
		pool, vaultSession.UserVault(), missionStore, portfolioStore,
		config.tenantID, config.organizationID, config.ownerKeyID,
		config.ownerPublicKey, config.companyIssuerKeyID, config.companyIssuerKey, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	lifecycleStore, err := companylifecycle.New(
		pool, vaultSession.UserVault(), config.tenantID,
		config.companyIssuerKeyID, config.companyIssuerKey, companyStore, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	initiativeCompiler, err := initiative.NewCompiler(
		config.ownerKeyID, config.ownerPublicKey, config.companyIssuerKey,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	initiativeStore, err := initiative.NewStore(
		pool, vaultSession.UserVault(), config.tenantID, config.organizationID, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	initiativeDispatcher, err := initiative.NewDispatcher(
		pool, schedulerStore, config.tenantID, config.organizationID, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	companyCoordinator, err := companyruntime.NewCoordinatorWithDispatcher(
		pool, companyStore, missionStore, portfolioStore, lifecycleStore,
		initiativeCompiler, initiativeStore, initiativeDispatcher, nowFunc,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cyclePack, err := skills.WorkforcePack()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cycleCatalog, err := skills.NewCatalog(cyclePack)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cycleDispatcher, err := companyruntime.NewCycleDispatcher(
		companyStore, schedulerStore, companyruntime.CycleRuntimeBinding{
			ModelProvider: config.model.Provider, ModelID: config.model.ModelID,
			MGSReference: "skill-catalog:workforce",
			MGSDigest:    cycleCatalog.Digest().Digest,
		},
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := companyCoordinator.AttachCycleDispatcher(cycleDispatcher); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := service.AttachCompanyRuntime(companyCoordinator); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	runtimeReload := make(chan struct{}, 1)
	if err := service.AttachRuntimeReload(func() {
		select {
		case runtimeReload <- struct{}{}:
		default:
		}
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	runtimeStopped := make(chan struct{})
	go func() {
		defer close(runtimeStopped)
		ticker := time.NewTicker(config.pollInterval)
		defer ticker.Stop()
		var wakeRunner *wakeruntime.Runner
		for {
			if err := companyRecoveryStore.AssertOperational(ctx); err != nil {
				if ctx.Err() == nil {
					fmt.Fprintln(stderr, "workforced: company runtime paused:", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-runtimeReload:
					wakeRunner = nil
				case <-ticker.C:
				}
				continue
			}
			if wakeRunner == nil {
				ownerRoot, rootErr := service.RuntimeOwnerRoot(ctx, principal)
				if rootErr == nil {
					wakeRunner, rootErr = buildWakeRunner(
						ctx, pool, vaultSession.UserVault(), service,
						principal, schedulerStore, config, ownerRoot, missionStore,
						companyStore, lifecycleStore, initiativeStore, cycleDispatcher,
					)
					if rootErr == nil {
						rootErr = service.AttachProductExecution(wakeRunner.ProductExecution)
					}
					if rootErr == nil {
						rootErr = service.AttachFinancial(wakeRunner.Financial)
					}
					if rootErr == nil {
						rootErr = service.AttachBusinessOutcomes(wakeRunner.BusinessOutcomes)
					}
					if rootErr == nil {
						rootErr = service.AttachCommercialExecution(
							wakeRunner.CommercialExecution,
							wakeRunner.CommercialCoordinator,
						)
					}
				}
				if rootErr != nil &&
					!errors.Is(rootErr, controlapi.ErrNotActivated) &&
					ctx.Err() == nil {
					fmt.Fprintln(stderr, "workforced: initialize wake runtime:", rootErr)
				}
			}
			dispatches, dispatchErr := companyCoordinator.DispatchReady(
				ctx, uint16(config.claimBatch),
			)
			if dispatchErr != nil && !errors.Is(dispatchErr, companyruntime.ErrNotStarted) && ctx.Err() == nil {
				fmt.Fprintln(stderr, "workforced: company dispatch:", dispatchErr)
			}
			for _, dispatch := range dispatches {
				_, publishErr := service.Publish(ctx, principal, controlapi.LifecycleEvent{
					ID:             "event:company-work-order-dispatch:" + dispatch.WorkOrderID,
					OrganizationID: principal.OrganizationID,
					Type:           "company.work_order.queued", ResourceKind: "company-work-order",
					ResourceID: dispatch.WorkOrderID, ResourceVersion: dispatch.PlanVersion,
					VerifiedCompletion: false,
					Fields: map[string]any{
						"state": "queued", "initiative_id": dispatch.InitiativeID,
						"plan_node_id": dispatch.PlanNodeID, "goal_id": dispatch.GoalID,
						"wake_id": dispatch.WakeID, "intent_ids": dispatch.IntentIDs,
					},
				})
				if publishErr != nil && ctx.Err() == nil {
					fmt.Fprintln(stderr, "workforced: publish company dispatch:", publishErr)
				}
			}
			cycles, cycleErr := companyCoordinator.RunDue(ctx, uint16(config.claimBatch))
			if cycleErr != nil && !errors.Is(cycleErr, companyruntime.ErrNotStarted) && ctx.Err() == nil {
				fmt.Fprintln(stderr, "workforced: company controller:", cycleErr)
			}
			for _, cycle := range cycles {
				cycleDispatch, cycleDispatchErr := companyCoordinator.DispatchCycle(ctx, cycle)
				if cycleDispatchErr != nil {
					if ctx.Err() == nil {
						fmt.Fprintln(stderr, "workforced: dispatch company cycle:", cycleDispatchErr)
					}
					continue
				}
				_, publishErr := service.Publish(ctx, principal, controlapi.LifecycleEvent{
					ID:             "event:company-cycle:" + cycle.ID,
					OrganizationID: principal.OrganizationID, Type: "company.cycle.queued",
					ResourceKind: "company-cycle", ResourceID: cycle.ID, ResourceVersion: 1,
					VerifiedCompletion: false,
					Fields: map[string]any{
						"kind": cycle.Kind, "due_at": cycle.DueAt, "next_at": cycle.NextAt,
						"departments":           cycle.Departments,
						"required_capabilities": cycle.RequiredCapabilities,
						"independent_audit":     cycle.IndependentAudit,
						"state":                 "queued",
						"work_order_id":         cycleDispatch.WorkOrderID,
						"goal_id":               cycleDispatch.GoalID,
						"intent_ids":            cycleDispatch.IntentIDs,
						"wake_id":               cycleDispatch.WakeID,
					},
				})
				if publishErr != nil && ctx.Err() == nil {
					fmt.Fprintln(stderr, "workforced: publish company cycle:", publishErr)
				}
			}
			if wakeRunner != nil {
				if err := wakeRunner.RunDue(
					ctx, config.organizationID, config.claimBatch,
				); err != nil && ctx.Err() == nil {
					fmt.Fprintln(stderr, "workforced: wake runtime:", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-runtimeReload:
				wakeRunner = nil
			case <-ticker.C:
			}
		}
	}()
	rootHandler := http.NewServeMux()
	rootHandler.Handle(
		"/internal/workforce/wake",
		scheduler.Handler(schedulerStore, config.wakeToken),
	)
	rootHandler.Handle("/", service.Handler())
	server := &http.Server{
		Addr: config.listen, Handler: rootHandler,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdownID := companyrecovery.ShutdownID(
			"shutdown-" + strconv.FormatInt(nowFunc().UnixNano(), 10),
		)
		if _, shutdownErr := companyRecoveryStore.BeginShutdown(
			shutdownCtx, shutdownID, "process_signal",
		); shutdownErr != nil {
			fmt.Fprintln(stderr, "workforced: begin graceful shutdown:", shutdownErr)
		}
		_ = server.Shutdown(shutdownCtx)
		select {
		case <-runtimeStopped:
		case <-shutdownCtx.Done():
		}
		if _, shutdownErr := companyRecoveryStore.CompleteShutdown(
			shutdownCtx, shutdownID,
		); shutdownErr != nil {
			fmt.Fprintln(stderr, "workforced: complete graceful shutdown:", shutdownErr)
		}
	}()
	fmt.Fprintf(stdout, "workforced %s listening on %s\n", version, config.listen)
	err = server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, "workforced: serve:", err)
		return 1
	}
	if ctx.Err() != nil {
		<-stopped
		<-runtimeStopped
	}
	return 0
}

type runtimeConfig struct {
	listen                 string
	postgresURI            string
	tenantID               string
	organizationID         contracts.OrganizationID
	ownerID                contracts.OwnerID
	ownerToken             string
	wakeToken              string
	ownerKeyID             string
	ownerPublicKey         ed25519.PublicKey
	runtimeKeyID           string
	runtimeKey             ed25519.PrivateKey
	runtimePublicKey       ed25519.PublicKey
	companyIssuerKeyID     string
	companyIssuerKey       ed25519.PrivateKey
	companyIssuerPublicKey ed25519.PublicKey
	scheduler              scheduler.Config
	model                  modelclient.Config
	bubblewrap             string
	seatBinary             string
	auditorBinary          string
	developerRepo          string
	codegraph              string
	auditorSeatID          contracts.SeatID
	pollInterval           time.Duration
	leaseDuration          time.Duration
	claimBatch             uint32
	dataDir                string
	recoveryBackend        string
	recoveryBackendArgs    []string
	recoveryBackendTimeout time.Duration
}

func loadConfig(listen string) (runtimeConfig, error) {
	gatewayURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MATRIX_GATEWAY_URL")), "/")
	gatewayToken := strings.TrimSpace(os.Getenv("MATRIX_GATEWAY_TOKEN"))
	modelEndpoint := ""
	modelCredential := ""
	if gatewayURL != "" || gatewayToken != "" {
		if gatewayURL == "" || gatewayToken == "" {
			return runtimeConfig{}, fmt.Errorf("workforced: MATRIX_GATEWAY_URL and MATRIX_GATEWAY_TOKEN must be configured together")
		}
		modelEndpoint = gatewayURL + "/v1/chat/completions"
		modelCredential = gatewayToken
	} else {
		modelCredential = strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
		if modelCredential == "" {
			modelCredential = strings.TrimSpace(os.Getenv("XIAOMI_API_KEY"))
		}
		modelEndpoint = neoprovider.MiMoChatEndpoint
	}
	config := runtimeConfig{
		listen: listen, postgresURI: os.Getenv("WORKFORCE_POSTGRES_URI"),
		tenantID:           os.Getenv("WORKFORCE_TENANT_ID"),
		organizationID:     contracts.OrganizationID(os.Getenv("WORKFORCE_ORGANIZATION_ID")),
		ownerID:            contracts.OwnerID(os.Getenv("WORKFORCE_OWNER_ID")),
		ownerToken:         os.Getenv("WORKFORCE_OWNER_TOKEN"),
		wakeToken:          os.Getenv("WORKFORCE_WAKE_TOKEN"),
		ownerKeyID:         os.Getenv("WORKFORCE_OWNER_KEY_ID"),
		runtimeKeyID:       os.Getenv("WORKFORCE_RUNTIME_KEY_ID"),
		companyIssuerKeyID: os.Getenv("WORKFORCE_COMPANY_ISSUER_KEY_ID"),
		bubblewrap:         os.Getenv("WORKFORCE_BUBBLEWRAP"),
		seatBinary:         os.Getenv("WORKFORCE_SEAT_BINARY"),
		auditorBinary:      os.Getenv("WORKFORCE_AUDITOR_BINARY"),
		developerRepo:      os.Getenv("WORKFORCE_DEVELOPER_REPOSITORY"),
		codegraph:          os.Getenv("WORKFORCE_CODEGRAPH_EXECUTABLE"),
		auditorSeatID:      contracts.SeatID(os.Getenv("WORKFORCE_AUDITOR_SEAT_ID")),
		dataDir:            os.Getenv("WORKFORCE_DATA_DIR"),
		recoveryBackend:    strings.TrimSpace(os.Getenv("WORKFORCE_RECOVERY_BACKEND_EXECUTABLE")),
	}
	for name, value := range map[string]string{
		"WORKFORCE_POSTGRES_URI":                              config.postgresURI,
		"WORKFORCE_TENANT_ID":                                 config.tenantID,
		"WORKFORCE_ORGANIZATION_ID":                           string(config.organizationID),
		"WORKFORCE_OWNER_ID":                                  string(config.ownerID),
		"WORKFORCE_OWNER_TOKEN":                               config.ownerToken,
		"WORKFORCE_WAKE_TOKEN":                                config.wakeToken,
		"WORKFORCE_OWNER_KEY_ID":                              config.ownerKeyID,
		"WORKFORCE_OWNER_PUBLIC_KEY":                          os.Getenv("WORKFORCE_OWNER_PUBLIC_KEY"),
		"WORKFORCE_RUNTIME_KEY_ID":                            config.runtimeKeyID,
		"WORKFORCE_RUNTIME_PRIVATE_KEY":                       os.Getenv("WORKFORCE_RUNTIME_PRIVATE_KEY"),
		"WORKFORCE_COMPANY_ISSUER_KEY_ID":                     config.companyIssuerKeyID,
		"WORKFORCE_COMPANY_ISSUER_PRIVATE_KEY":                os.Getenv("WORKFORCE_COMPANY_ISSUER_PRIVATE_KEY"),
		"MATRIX_GATEWAY_TOKEN or MIMO_API_KEY/XIAOMI_API_KEY": modelCredential,
		"WORKFORCE_BUBBLEWRAP":                                config.bubblewrap,
		"WORKFORCE_SEAT_BINARY":                               config.seatBinary,
		"WORKFORCE_AUDITOR_BINARY":                            config.auditorBinary,
		"WORKFORCE_DEVELOPER_REPOSITORY":                      config.developerRepo,
		"WORKFORCE_CODEGRAPH_EXECUTABLE":                      config.codegraph,
		"WORKFORCE_AUDITOR_SEAT_ID":                           string(config.auditorSeatID),
		"WORKFORCE_DATA_DIR":                                  config.dataDir,
	} {
		if strings.TrimSpace(value) == "" {
			return runtimeConfig{}, fmt.Errorf("workforced: %s is required", name)
		}
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(os.Getenv("WORKFORCE_OWNER_PUBLIC_KEY"))
	if err != nil {
		publicKey, err = base64.StdEncoding.DecodeString(os.Getenv("WORKFORCE_OWNER_PUBLIC_KEY"))
	}
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return runtimeConfig{}, fmt.Errorf("workforced: WORKFORCE_OWNER_PUBLIC_KEY must be an Ed25519 public key")
	}
	config.ownerPublicKey = ed25519.PublicKey(publicKey)
	runtimeKey, err := decodePrivateKey(
		os.Getenv("WORKFORCE_RUNTIME_PRIVATE_KEY"),
	)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_RUNTIME_PRIVATE_KEY must be an Ed25519 private key",
		)
	}
	config.runtimeKey = runtimeKey
	config.runtimePublicKey = append(
		ed25519.PublicKey(nil), runtimeKey.Public().(ed25519.PublicKey)...,
	)
	companyIssuerKey, err := decodePrivateKey(
		os.Getenv("WORKFORCE_COMPANY_ISSUER_PRIVATE_KEY"),
	)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_COMPANY_ISSUER_PRIVATE_KEY must be an Ed25519 private key",
		)
	}
	if config.companyIssuerKeyID == config.runtimeKeyID ||
		companyIssuerKey.Equal(runtimeKey) {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: company issuer and runtime authority must be distinct",
		)
	}
	config.companyIssuerKey = companyIssuerKey
	config.companyIssuerPublicKey = append(
		ed25519.PublicKey(nil), companyIssuerKey.Public().(ed25519.PublicKey)...,
	)
	schedulerConfig, err := loadSchedulerConfig()
	if err != nil {
		return runtimeConfig{}, err
	}
	config.scheduler = schedulerConfig
	modelMaxTokens, err := envInt("WORKFORCE_MODEL_MAX_TOKENS", 4096)
	if err != nil {
		return runtimeConfig{}, err
	}
	modelTimeout, err := time.ParseDuration(
		envOr("WORKFORCE_MODEL_TIMEOUT", "2m"),
	)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_MODEL_TIMEOUT is invalid",
		)
	}
	config.model = modelclient.Config{
		Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		ModelVersion: neoprovider.MiMoV25ProModel,
		Endpoint:     modelEndpoint,
		APIKey:       modelCredential,
		ActorDID:     "did:matrix:" + config.tenantID + ":workforce",
		Temperature:  neoprovider.MiMoTemperature,
		MaxTokens:    modelMaxTokens, Timeout: modelTimeout,
	}
	config.pollInterval, err = time.ParseDuration(
		envOr("WORKFORCE_POLL_INTERVAL", "1s"),
	)
	if err != nil || config.pollInterval <= 0 {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_POLL_INTERVAL is invalid",
		)
	}
	config.leaseDuration, err = time.ParseDuration(
		envOr("WORKFORCE_WAKE_LEASE_DURATION", "30m"),
	)
	if err != nil || config.leaseDuration <= 0 ||
		config.leaseDuration > 2*time.Hour {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_WAKE_LEASE_DURATION is invalid",
		)
	}
	config.claimBatch, err = envUint32("WORKFORCE_CLAIM_BATCH", 8)
	if err != nil || config.claimBatch == 0 || config.claimBatch > 1000 {
		return runtimeConfig{}, fmt.Errorf(
			"workforced: WORKFORCE_CLAIM_BATCH is invalid",
		)
	}
	config.recoveryBackendArgs = strings.Fields(os.Getenv("WORKFORCE_RECOVERY_BACKEND_ARGUMENTS"))
	config.recoveryBackendTimeout, err = time.ParseDuration(
		envOr("WORKFORCE_RECOVERY_BACKEND_TIMEOUT", "30m"),
	)
	if err != nil || config.recoveryBackendTimeout <= 0 || config.recoveryBackendTimeout > 24*time.Hour {
		return runtimeConfig{}, fmt.Errorf("workforced: WORKFORCE_RECOVERY_BACKEND_TIMEOUT is invalid")
	}
	return config, nil
}

func loadSchedulerConfig() (scheduler.Config, error) {
	organizationConcurrency, err := envUint32(
		"WORKFORCE_MAX_ORGANIZATION_CONCURRENCY", 8,
	)
	if err != nil {
		return scheduler.Config{}, err
	}
	seatConcurrency, err := envUint32("WORKFORCE_MAX_SEAT_CONCURRENCY", 1)
	if err != nil {
		return scheduler.Config{}, err
	}
	dailyTasks, err := envUint32("WORKFORCE_DAILY_TASK_LIMIT", 1000)
	if err != nil {
		return scheduler.Config{}, err
	}
	dailySpend, err := envUint64(
		"WORKFORCE_DAILY_SPEND_MICROUNITS", 1_000_000_000,
	)
	if err != nil {
		return scheduler.Config{}, err
	}
	quietStart, err := envInt("WORKFORCE_QUIET_HOURS_START_UTC", 0)
	if err != nil {
		return scheduler.Config{}, err
	}
	quietEnd, err := envInt("WORKFORCE_QUIET_HOURS_END_UTC", 0)
	if err != nil {
		return scheduler.Config{}, err
	}
	claimLease, err := time.ParseDuration(
		envOr("WORKFORCE_CLAIM_LEASE", "2m"),
	)
	if err != nil {
		return scheduler.Config{}, fmt.Errorf(
			"workforced: WORKFORCE_CLAIM_LEASE is invalid",
		)
	}
	config := scheduler.Config{
		MaxOrganizationConcurrency: organizationConcurrency,
		MaxSeatConcurrency:         seatConcurrency,
		DailyTaskLimit:             dailyTasks,
		DailySpendMicrounits:       dailySpend,
		QuietHoursStartUTC:         quietStart,
		QuietHoursEndUTC:           quietEnd,
		ClaimLease:                 claimLease,
	}
	return config, nil
}

func buildWakeRunner(
	ctx context.Context,
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	service *controlapi.Service,
	principal controlapi.Principal,
	schedulerStore *scheduler.Store,
	config runtimeConfig,
	ownerRoot policy.OwnerRoot,
	missionStore *mission.Store,
	companyStore *companyruntime.Store,
	lifecycleStore *companylifecycle.Store,
	initiativeStore *initiative.Store,
	cycleDispatcher *companyruntime.CycleDispatcher,
) (*wakeruntime.Runner, error) {
	now := func() time.Time { return time.Now().UTC() }
	authority, err := policy.New(
		pool, userVault, ownerRoot, now,
	)
	if err != nil {
		return nil, err
	}
	skillStore, err := skills.NewStore(
		pool, userVault, config.tenantID, config.organizationID,
		ownerRoot.KeyID, ownerRoot.PublicKey, now,
	)
	if err != nil {
		return nil, err
	}
	pack, err := skills.WorkforcePack()
	if err != nil {
		return nil, err
	}
	catalog, err := skills.NewCatalog(pack)
	if err != nil {
		return nil, err
	}
	graph, err := dependency.New(pool, config.tenantID, now)
	if err != nil {
		return nil, err
	}
	leaseStore, err := lease.New(pool, config.tenantID, now)
	if err != nil {
		return nil, err
	}
	ledgerStore, err := ledger.New(
		pool, userVault, config.tenantID, now,
	)
	if err != nil {
		return nil, err
	}
	mailStore, err := mail.New(
		pool, userVault, graph, config.tenantID, mail.Config{
			MaxMailboxMessages: 10000, MaxThreadMessages: 1000,
			MaxThreadDepth: 64, MaxRecipients: 64, MaxAutoReplies: 100,
			MaxAttachmentBytes: 64 << 20, MaxMessageLifetime: 90 * 24 * time.Hour,
		}, now,
	)
	if err != nil {
		return nil, err
	}
	assembler, err := actorstate.NewAssembler(
		graph, ledgerStore, mailStore, authority, leaseStore, catalog, nil, now,
	)
	if err != nil {
		return nil, err
	}
	orderStore, err := workorder.NewStore(
		pool, userVault, config.tenantID, ownerRoot.KeyID,
		ownerRoot.PublicKey,
	)
	if err != nil {
		return nil, err
	}
	if err := orderStore.AttachCompanyAuthority(func(ctx context.Context) (workorder.CompanyAuthority, error) {
		current, err := missionStore.LoadCurrent(ctx)
		at := now()
		if err != nil || !current.Executable(at) {
			return workorder.CompanyAuthority{}, fmt.Errorf("workforced: company issuer authority is not executable")
		}
		return workorder.CompanyAuthority{
			Policy:       current.Authority.IssuerPolicy,
			FounderKeyID: ownerRoot.KeyID, FounderPublicKey: ownerRoot.PublicKey,
			CurrentMissionVersion:         current.Authority.Mission.Version,
			CurrentConstitutionVersion:    current.Authority.Constitution.Version,
			CurrentCapitalEnvelopeVersion: current.Authority.Capital.Version,
			At:                            at,
		}, nil
	}); err != nil {
		return nil, err
	}
	if err := orderStore.AttachCompanyCycleAuthority(cycleDispatcher.Authority); err != nil {
		return nil, err
	}
	compiler, err := workcompile.New(
		pool, userVault, config.tenantID, skillStore, authority,
		leaseStore, now,
	)
	if err != nil {
		return nil, err
	}
	executionStore, err := execution.New(
		pool, userVault, config.tenantID, now,
	)
	if err != nil {
		return nil, err
	}
	lineageStore, err := lineage.New(
		pool, userVault, config.tenantID, config.runtimeKeyID,
		config.runtimeKey, now,
	)
	if err != nil {
		return nil, err
	}
	auditStore, err := audit.NewStore(
		pool, userVault, config.tenantID, config.runtimeKeyID,
		config.runtimeKey, authority, now,
	)
	if err != nil {
		return nil, err
	}
	breakers, err := circuit.New(
		pool, config.tenantID, circuit.Config{
			FailureThreshold: 3, SuccessThreshold: 2,
			Window: 5 * time.Minute, OpenDuration: time.Minute,
			HalfOpenLimit: 1, TrialDuration: 30 * time.Second,
		}, now,
	)
	if err != nil {
		return nil, err
	}
	codeGraph, err := projectbrain.NewCodeGraph(config.codegraph, now)
	if err != nil {
		return nil, err
	}
	runtimePublic := config.runtimeKey.Public().(ed25519.PublicKey)
	brain, err := projectbrain.New(
		pool, userVault, config.tenantID, config.runtimeKeyID,
		runtimePublic, authority, codeGraph, now,
	)
	if err != nil {
		return nil, err
	}
	developerAuthority, err := developer.NewAuthority(
		pool, leaseStore, codeGraph, brain, config.tenantID, now,
	)
	if err != nil {
		return nil, err
	}
	developerAdapter, err := developer.NewAdapter(
		developerAuthority, brain,
		[]developer.VerificationCommand{{
			ID: "codegraph-stats", Bubblewrap: config.bubblewrap,
			Executable: config.codegraph,
			Arguments: []string{
				"stats", "--repo", filepath.Base(config.developerRepo),
			},
			Timeout: 30 * time.Second,
		}},
		now,
	)
	if err != nil {
		return nil, err
	}
	approvalStore, err := approval.New(
		pool, userVault, config.tenantID, config.organizationID,
		ownerRoot.OwnerID, ownerRoot.KeyID, ownerRoot.PublicKey, now,
	)
	if err != nil {
		return nil, err
	}
	departmentAdapters, err := departmentadapter.New(now, approvalStore)
	if err != nil {
		return nil, err
	}
	productCapabilityAdapter, err := productcapability.NewAdapter(now)
	if err != nil {
		return nil, err
	}
	commercialCapabilityAdapter, err := commercialcapability.NewAdapter(now)
	if err != nil {
		return nil, err
	}
	externalStore, err := external.NewStore(
		pool, userVault, config.tenantID, ownerRoot.KeyID,
		ownerRoot.PublicKey, now,
	)
	if err != nil {
		return nil, err
	}
	externalAdapters, err := external.LoadAdapters(ctx, externalStore, config.organizationID)
	if err != nil {
		return nil, err
	}
	currentCompany, err := missionStore.LoadCurrent(ctx)
	if err != nil || !currentCompany.Executable(now()) {
		return nil, fmt.Errorf("workforced: company authority is not executable")
	}
	controllerPublicKey, err := base64.RawURLEncoding.DecodeString(
		currentCompany.Authority.IssuerPolicy.IssuerPublicKey,
	)
	if err != nil || len(controllerPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("workforced: company issuer key is invalid")
	}
	customerStore, err := customer.NewStore(
		pool, userVault, config.tenantID, ownerRoot.KeyID, ownerRoot.PublicKey,
		map[string]ed25519.PublicKey{
			currentCompany.Authority.IssuerPolicy.IssuerKeyID: ed25519.PublicKey(controllerPublicKey),
		},
		now,
	)
	if err != nil {
		return nil, err
	}
	externalRegistry := make(map[string]effect.Adapter, len(externalAdapters))
	for _, adapter := range externalAdapters {
		externalRegistry[adapter.Name()] = adapter
	}
	var customerAdapters []effect.Adapter
	if len(externalRegistry) != 0 {
		customerAdapters, err = customer.LoadAdapters(
			ctx, customerStore, config.organizationID, externalRegistry,
		)
		if err != nil {
			return nil, err
		}
	}
	financialStore, err := financial.NewStore(
		pool, userVault, config.tenantID, ownerRoot.KeyID, ownerRoot.PublicKey,
		map[string]ed25519.PublicKey{
			currentCompany.Authority.IssuerPolicy.IssuerKeyID: ed25519.PublicKey(controllerPublicKey),
		},
		now,
	)
	if err != nil {
		return nil, err
	}
	financialConnections, err := financialStore.ListActiveConnections(ctx, config.organizationID)
	if err != nil {
		return nil, err
	}
	var financialAdapters []effect.Adapter
	if len(financialConnections) != 0 {
		financialAdapters, err = financial.LoadAdapters(
			ctx, financialStore, config.organizationID, externalRegistry,
		)
		if err != nil {
			return nil, err
		}
	}
	businessOutcomeStore, err := businessoutcome.NewStore(
		pool, userVault, config.tenantID, config.organizationID, now,
	)
	if err != nil {
		return nil, err
	}
	financialSourceVerifier, err := businessoutcome.NewPostgreSQLFinancialSourceVerifier(
		pool, config.tenantID,
	)
	if err != nil {
		return nil, err
	}
	if err := businessOutcomeStore.AttachFinancialSourceVerifier(financialSourceVerifier); err != nil {
		return nil, err
	}
	commercialExecutionStore, err := commercialexecution.NewStore(
		pool, userVault, leaseStore, config.tenantID, config.organizationID,
		currentCompany.Authority.IssuerPolicy.IssuerKeyID,
		ed25519.PublicKey(controllerPublicKey),
		[]commercialexecution.IssuerPolicy{{
			KeyID:     config.runtimeKeyID,
			PublicKey: runtimePublic,
			Phases: []commercialexecution.Phase{
				commercialexecution.PhaseAcquisition,
				commercialexecution.PhaseCustomerQualification,
				commercialexecution.PhaseSale,
				commercialexecution.PhaseFinancialIntent,
				commercialexecution.PhaseFinancialReconciliation,
				commercialexecution.PhaseSupport,
				commercialexecution.PhaseMeasurement,
			},
		}},
		now,
	)
	if err != nil {
		return nil, err
	}
	commercialCoordinator, err := commercialexecution.NewCoordinator(
		commercialExecutionStore, businessOutcomeStore,
	)
	if err != nil {
		return nil, err
	}
	adapters := append(
		[]effect.Adapter{
			developerAdapter,
			productCapabilityAdapter,
			commercialCapabilityAdapter,
		},
		departmentAdapters...,
	)
	adapters = append(adapters, externalAdapters...)
	adapters = append(adapters, customerAdapters...)
	adapters = append(adapters, financialAdapters...)
	effectGateway, err := effect.New(
		pool, userVault, leaseStore, authority, breakers,
		config.tenantID, approval.Authority{
			OwnerID: ownerRoot.OwnerID, KeyID: ownerRoot.KeyID,
			PublicKey: ownerRoot.PublicKey,
		}, now, adapters...,
	)
	if err != nil {
		return nil, err
	}
	organizationStore, err := organization.NewStore(
		pool, userVault, organization.OwnerAuthority{
			TenantID: config.tenantID, OrganizationID: config.organizationID,
			OwnerID: ownerRoot.OwnerID, KeyID: ownerRoot.KeyID,
			PublicKey: ownerRoot.PublicKey,
		}, now,
	)
	if err != nil {
		return nil, err
	}
	squadStore, err := squad.NewStore(
		pool, userVault, organizationStore, squad.ControllerAuthority{
			TenantID: config.tenantID, OrganizationID: config.organizationID,
			KeyID: config.companyIssuerKeyID, PrivateKey: config.companyIssuerKey,
			EffectiveAt: currentCompany.Authority.IssuerPolicy.EffectiveAt,
			ExpiresAt:   currentCompany.Authority.IssuerPolicy.ExpiresAt,
		}, now,
	)
	if err != nil {
		return nil, err
	}
	productStore, err := productcapability.NewStore(
		pool, userVault, config.tenantID, config.organizationID, now,
	)
	if err != nil {
		return nil, err
	}
	productExecutionStore, err := productexecution.NewStore(
		pool, userVault, missionStore, squadStore, productStore,
		initiativeStore, lifecycleStore, companyStore, schedulerStore,
		lineageStore, effectGateway, config.tenantID, config.organizationID, now,
	)
	if err != nil {
		return nil, err
	}
	model, err := modelclient.New(config.model)
	if err != nil {
		return nil, err
	}
	seatDigest, err := executableDigest(config.seatBinary)
	if err != nil {
		return nil, fmt.Errorf("workforced: seat binary: %w", err)
	}
	auditorDigest, err := executableDigest(config.auditorBinary)
	if err != nil {
		return nil, fmt.Errorf("workforced: auditor binary: %w", err)
	}
	runtime := contracts.RuntimeBinding{
		BuildDigest: seatDigest, AuditorBuildDigest: auditorDigest,
		OperationRegistryDigest: catalog.Digest(),
	}
	if err := runtime.Validate(); err != nil {
		return nil, err
	}
	return &wakeruntime.Runner{
		Scheduler: schedulerStore, Graph: graph, Orders: orderStore,
		Authority: authority, Leases: leaseStore, Assembler: assembler,
		Seat: actorstate.Runner{
			Bubblewrap: config.bubblewrap, Binary: config.seatBinary,
		},
		Model: model, Compiler: compiler, Effects: effectGateway,
		Execution: executionStore, Lineage: lineageStore,
		Auditor: audit.Runner{
			Bubblewrap: config.bubblewrap, Binary: config.auditorBinary,
			DeveloperAuthorityKeyID: config.runtimeKeyID,
			DeveloperAuthorityKey:   runtimePublic,
		},
		Audits: auditStore, Catalog: catalog, SkillStore: skillStore,
		Developer:             developerAuthority,
		ProductExecution:      productExecutionStore,
		Financial:             financialStore,
		BusinessOutcomes:      businessOutcomeStore,
		CommercialExecution:   commercialExecutionStore,
		CommercialCoordinator: commercialCoordinator,
		RuntimeKeyID:          config.runtimeKeyID, RuntimeKey: config.runtimeKey,
		Runtime: runtime, TenantID: config.tenantID,
		AuditorSeatID: config.auditorSeatID,
		LeaseDuration: config.leaseDuration,
		WakeBudget: contracts.WakeBudget{
			MaxDurationMillis: uint64(config.leaseDuration / time.Millisecond),
			MaxSteps:          25, MaxModelCalls: 25, MaxToolCalls: 25,
			MaxCostMinor: 1_000_000_000, Currency: "USD",
			MaxOutputBytes: 1 << 20,
		},
		ReplayEvidence: true, Now: now,
		PublishLifecycle: func(
			ctx context.Context,
			resourceID, kind string,
			verified bool,
			receiptID contracts.ReceiptID,
			fields map[string]any,
		) error {
			_, err := service.Publish(ctx, principal, controlapi.LifecycleEvent{
				ID:             "event:" + kind + ":" + resourceID,
				OrganizationID: principal.OrganizationID,
				Type:           kind, ResourceKind: "wake", ResourceID: resourceID,
				ResourceVersion: 1, VerifiedCompletion: verified,
				ReceiptID: receiptID, Fields: fields,
			})
			return err
		},
	}, nil
}

func executableDigest(path string) (contracts.ContentHash, error) {
	file, err := os.Open(path)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return contracts.ContentHash{}, err
	}
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func envUint32(name string, fallback uint32) (uint32, error) {
	value, err := envUint64(name, uint64(fallback))
	if err != nil || value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("workforced: %s is invalid", name)
	}
	return uint32(value), nil
}

func envUint64(name string, fallback uint64) (uint64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("workforced: %s is invalid", name)
	}
	return value, nil
}

func envInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("workforced: %s is invalid", name)
	}
	return value, nil
}

func decodePrivateKey(value string) (ed25519.PrivateKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key")
	}
	return ed25519.PrivateKey(decoded), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: workforced -version | workforced -serve [-listen address]")
}
