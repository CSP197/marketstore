package start

import (
	"context"
	"fmt"
	"github.com/alpacahq/marketstore/v4/replication"
	"github.com/pkg/errors"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alpacahq/marketstore/v4/executor"
	"github.com/alpacahq/marketstore/v4/frontend"
	"github.com/alpacahq/marketstore/v4/frontend/stream"
	"github.com/alpacahq/marketstore/v4/proto"
	"github.com/alpacahq/marketstore/v4/utils"
	"github.com/alpacahq/marketstore/v4/utils/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

const (
	usage                 = "start"
	short                 = "Start a marketstore database server"
	long                  = "This command starts a marketstore database server"
	example               = "marketstore start --config <path>"
	defaultConfigFilePath = "./mkts.yml"
	configDesc            = "set the path for the marketstore YAML configuration file"
)

var (
	// Cmd is the start command.
	Cmd = &cobra.Command{
		Use:        usage,
		Short:      short,
		Long:       long,
		Aliases:    []string{"s"},
		SuggestFor: []string{"boot", "up"},
		Example:    example,
		RunE:       executeStart,
	}
	// configFilePath set flag for a path to the config file.
	configFilePath string
)

func init() {
	utils.InstanceConfig.StartTime = time.Now()
	Cmd.Flags().StringVarP(&configFilePath, "config", "c", defaultConfigFilePath, configDesc)
}

// executeStart implements the start command.
func executeStart(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	globalCtx, globalCancel := context.WithCancel(ctx)

	// Attempt to read config file.
	data, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		return fmt.Errorf("failed to read configuration file error: %s", err.Error())
	}

	// Log config location.
	log.Info("using %v for configuration", configFilePath)

	// Attempt to set configuration.
	err = utils.InstanceConfig.Parse(data)
	if err != nil {
		return fmt.Errorf("failed to parse configuration file error: %v", err.Error())
	}

	// New grpc server for marketstore API.
	grpcServer := grpc.NewServer(
		grpc.MaxSendMsgSize(utils.InstanceConfig.GRPCMaxSendMsgSize),
		grpc.MaxRecvMsgSize(utils.InstanceConfig.GRPCMaxRecvMsgSize),
	)
	proto.RegisterMarketstoreServer(grpcServer, frontend.GRPCService{})

	// New gRPC stream server for replication.
	grpcReplicationServer := grpc.NewServer(
		grpc.MaxSendMsgSize(utils.InstanceConfig.GRPCMaxSendMsgSize),
		grpc.MaxRecvMsgSize(utils.InstanceConfig.GRPCMaxRecvMsgSize),
	)

	// Spawn a goroutine and listen for a signal.
	signalChan := make(chan os.Signal)
	go func() {
		for s := range signalChan {
			switch s {
			case syscall.SIGUSR1:
				log.Info("dumping stack traces due to SIGUSR1 request")
				pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			case syscall.SIGINT:
				fallthrough
			case syscall.SIGTERM:
				log.Info("initiating graceful shutdown due to '%v' request", s)
				grpcServer.GracefulStop()
				log.Info("shutdown grpc API server...")
				globalCancel()
				grpcReplicationServer.Stop() // gRPC stream connection cannot be closed by GracefulStop()
				log.Info("shutdown grpc Replication server...")

				atomic.StoreUint32(&frontend.Queryable, uint32(0))
				log.Info("waiting a grace period of %v to shutdown...", utils.InstanceConfig.StopGracePeriod)
				time.Sleep(utils.InstanceConfig.StopGracePeriod)
				shutdown()
			}
		}
	}()
	signal.Notify(signalChan, syscall.SIGUSR1, syscall.SIGINT, syscall.SIGTERM)

	// Initialize marketstore services.
	// --------------------------------
	log.Info("initializing marketstore...")

	// initialize replication master or client
	var rs executor.ReplicationSender
	if utils.InstanceConfig.Replication.Enabled {
		rs = initReplicationMaster(globalCtx, grpcReplicationServer)
		log.Info("initialized replication master")
	} else if utils.InstanceConfig.Replication.MasterHost != "" {
		err = initReplicationClient(globalCtx)
		if err != nil {
			log.Fatal("Unable to startup Replication", err)
		}
		log.Info("initialized replication client")
	}

	executor.NewInstanceSetup(
		utils.InstanceConfig.RootDirectory,
		rs,
		utils.InstanceConfig.InitCatalog,
		utils.InstanceConfig.InitWALCache,
		utils.InstanceConfig.BackgroundSync,
		utils.InstanceConfig.WALBypass,
	)

	// New server.
	server, _ := frontend.NewServer()

	// Set rpc handler.
	log.Info("launching rpc data server...")
	http.Handle("/rpc", server)

	// Set websocket handler.
	log.Info("initializing websocket...")
	stream.Initialize()
	http.HandleFunc("/ws", stream.Handler)

	// Set monitoring handler.
	log.Info("launching prometheus metrics server...")
	http.Handle("/metrics", promhttp.Handler())

	// Initialize any provided plugins.
	InitializeTriggers()
	RunBgWorkers()

	if utils.InstanceConfig.UtilitiesURL != "" {
		// Start utility endpoints.
		log.Info("launching utility service...")
		go frontend.Utilities(utils.InstanceConfig.UtilitiesURL)
	}

	log.Info("enabling query access...")
	atomic.StoreUint32(&frontend.Queryable, 1)

	// Serve.
	log.Info("launching tcp listener for all services...")
	if utils.InstanceConfig.GRPCListenURL != "" {
		grpcLn, err := net.Listen("tcp", utils.InstanceConfig.GRPCListenURL)
		if err != nil {
			return fmt.Errorf("failed to start GRPC server - error: %s", err.Error())
		}
		go func() {
			err := grpcServer.Serve(grpcLn)
			if err != nil {
				grpcServer.GracefulStop()
			}
		}()
	}

	if err := http.ListenAndServe(utils.InstanceConfig.ListenURL, nil); err != nil {
		return fmt.Errorf("failed to start server - error: %s", err.Error())
	}

	return nil
}

func shutdown() {
	executor.ThisInstance.ShutdownPending = true
	executor.ThisInstance.WALWg.Wait()
	log.Info("exiting...")
	os.Exit(0)
}

func initReplicationMaster(ctx context.Context, grpcServer *grpc.Server) *replication.Sender {
	grpcReplicationServer, err := replication.NewGRPCReplicationService(
		grpcServer,
		utils.InstanceConfig.Replication.ListenPort,
	)
	if err != nil {
		log.Fatal("Unable to startup gRPC server for replication")
	}
	replicationSender := replication.NewSender(grpcReplicationServer)
	replicationSender.Run(ctx)

	return replicationSender
}

func initReplicationClient(ctx context.Context) error {
	c, err := replication.NewGRPCReplicationClient(utils.InstanceConfig.Replication.MasterHost, false)
	if err != nil {
		return errors.Wrap(err, "failed to initialize gRPC client for replication")
	}

	// TODO: implement TLS between master and replica
	replicationReceiver := replication.NewReceiver(c)
	err = replicationReceiver.Run(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to connect Master instance from Replica")
	}

	return nil
}
