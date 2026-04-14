package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/diffservice"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	erofsdiff "github.com/containerd/containerd/v2/plugins/diff/erofs"
	snapshot "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"

	erofsgrpc "github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc"
	"github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc/credentials"
	"github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc/daemon"
)

var (
	rootDir        = flag.String("root", "/var/lib/containerd-erofs/snapshotter", "EROFS snapshotter root directory")
	sockAddr       = flag.String("addr", "/run/containerd-erofs-grpc/containerd-erofs-grpc.sock", "Socket path to listen on")
	containerdAddr = flag.String("containerd-addr", "/run/containerd/containerd.sock", "Address for containerd's GRPC server")
	hostsDir       = flag.String("hosts-dir", "", "Optional registry hosts.toml root directory")
	dockerConfig   = flag.String("docker-config", "", "Optional Docker config directory or config.json path used for registry credentials")
	daemonMode     = flag.String("daemon-mode", "eager", "Daemon implementation to use: eager or dummy")
)

func main() {
	flag.Parse()

	if err := log.SetLevel("debug"); err != nil {
		fmt.Printf("error: set log level: %v\n", err)
		os.Exit(1)
	}
	log.L.WithFields(log.Fields{
		"root":            *rootDir,
		"addr":            *sockAddr,
		"containerd_addr": *containerdAddr,
		"hosts_dir":       *hostsDir,
		"docker_config":   *dockerConfig,
		"daemon_mode":     *daemonMode,
	}).Info("Starting containerd-erofs-grpc")

	if err := serve(*containerdAddr, *sockAddr, *rootDir); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func serve(containerdAddress, address, root string) error {
	// Prepare the address directory
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		return err
	}
	// Remove the socket if exist to avoid EADDRINUSE
	if err := os.RemoveAll(address); err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(streamServerInterceptor),
		grpc.UnaryInterceptor(unaryServerInterceptor),
	}

	rpc := grpc.NewServer(serverOpts...)

	client, err := containerd.New(containerdAddress)
	if err != nil {
		return err
	}
	defer client.Close()

	// Instantiate the EROFS differ
	d := &diffService{contentStore: client.ContentStore()}
	service := diffservice.FromApplierAndComparer(d, d)
	diffapi.RegisterDiffServer(rpc, service)

	var opts []snapshot.Opt
	baseSnapshotter, err := snapshot.NewSnapshotter(root, opts...)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := baseSnapshotter.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	creds := credentials.NewDockerConfigBackend(*dockerConfig)
	daemonClient, err := newDaemonClient(*daemonMode)
	if err != nil {
		return err
	}
	erofsGRPCSnapshotter, err := erofsgrpc.New(erofsgrpc.Config{
		Root: root,
		Base: baseSnapshotter,
		ManifestProvider: erofsgrpc.NewManifestProvider(erofsgrpc.ManifestProviderConfig{
			Credentials: creds,
			HostsDir:    *hostsDir,
		}),
		BlobProvider: erofsgrpc.NewBlobProvider(erofsgrpc.BlobProviderConfig{
			Credentials: creds,
			HostsDir:    *hostsDir,
		}),
		Daemon:       daemonClient,
		DaemonConfig: erofsgrpc.DaemonConfig{Root: root},
	})
	if err != nil {
		return err
	}
	defer erofsGRPCSnapshotter.Close()

	// Convert the snapshotter to a gRPC service,
	// example in github.com/containerd/containerd/contrib/snapshotservice
	ss := snapshotservice.FromSnapshotter(erofsGRPCSnapshotter)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, ss)

	// Listen and serve
	l, err := net.Listen("unix", address)
	if err != nil {
		return err
	}
	log.L.WithFields(log.Fields{
		"listen_addr":     address,
		"root":            root,
		"containerd_addr": containerdAddress,
	}).Info("Listening")
	return rpc.Serve(l)
}

func newDaemonClient(mode string) (daemon.DaemonClient, error) {
	switch mode {
	case "eager":
		return daemon.NewEagerDaemon(), nil
	case "dummy":
		return daemon.NewDummyDaemonClient(), nil
	default:
		return nil, fmt.Errorf("unsupported daemon mode %q", mode)
	}
}

func unaryServerInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
	}
	return handler(ctx, req)
}

func streamServerInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
		ss = &wrappedSSWithContext{ctx: ctx, ServerStream: ss}
	}
	return handler(srv, ss)
}

type wrappedSSWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedSSWithContext) Context() context.Context {
	return w.ctx
}

type differ interface {
	diff.Applier
	diff.Comparer
}

type diffService struct {
	contentStore content.Store
	differ       differ
	loaded       uint32
	loadM        sync.Mutex

	diffapi.UnimplementedDiffServer
}

func (a *diffService) getDiffer() (differ, error) {
	if atomic.LoadUint32(&a.loaded) == 1 {
		return a.differ, nil
	}

	a.loadM.Lock()
	defer a.loadM.Unlock()

	if a.loaded == 1 {
		return a.differ, nil
	}

	if a.contentStore == nil {
		return nil, errors.New("content store is not configured")
	}

	a.differ = erofsdiff.NewErofsDiffer(a.contentStore, []string{})
	atomic.StoreUint32(&a.loaded, 1)
	return a.differ, nil
}

func (s *diffService) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Apply(ctx, desc, mounts, opts...)
}

func (s *diffService) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Compare(ctx, lower, upper, opts...)
}
