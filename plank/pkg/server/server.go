// Copyright 2019-2021 VMware, Inc.
// SPDX-License-Identifier: BSD-2-Clause

package server

import (
    "context"
    "crypto/tls"
    "encoding/json"
    "errors"
    "fmt"
    "github.com/pb33f/ranch/stompserver"
    "github.com/spf13/pflag"
    "io/ioutil"
    "log/slog"
    "net"
    "net/http"
    "os"
    "os/signal"
    "path"
    "reflect"
    "sync"
    "sync/atomic"
    "syscall"
    "time"

    "github.com/gorilla/handlers"
    "github.com/gorilla/mux"
    "github.com/pb33f/ranch/bus"
    "github.com/pb33f/ranch/model"
    "github.com/pb33f/ranch/plank/pkg/middleware"
    "github.com/pb33f/ranch/service"
)

const RANCH_SERVER_ONLINE_CHANNEL = bus.RANCH_INTERNAL_CHANNEL_PREFIX + "ranch-online-notify"
const AllMethodsWildcard = "*" // every method, open the gates!

// NewPlatformServer configures and returns a new platformServer instance
func NewPlatformServer(config *PlatformServerConfig) PlatformServer {

    // configure a default logger if none is provided
    if config.Logger == nil {
        config.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
            Level: slog.LevelInfo,
        }))
    }

    ps := new(platformServer)
    sanitizeConfigRootPath(config)
    ps.serverConfig = config
    ps.ServerAvailability = &ServerAvailability{}
    ps.routerConcurrencyProtection = new(int32)
    ps.messageBridgeMap = make(map[string]*MessageBridge)
    ps.eventbus = bus.GetBus()
    ps.initialize()
    return ps
}

// NewPlatformServerFromConfig returns a new instance of PlatformServer based on the config JSON file provided as configPath
func NewPlatformServerFromConfig(configPath string) (PlatformServer, error) {
    var config PlatformServerConfig

    // no config no server
    configBytes, err := ioutil.ReadFile(configPath)
    if err != nil {
        return nil, err
    }

    // malformed config no server as well
    if err = json.Unmarshal(configBytes, &config); err != nil {
        return nil, err
    }

    ps := new(platformServer)
    ps.eventbus = bus.GetBus()
    sanitizeConfigRootPath(&config)

    // handle invalid duration by setting it to the default value of 5 minutes
    if config.ShutdownTimeout <= 0 {
        config.ShutdownTimeout = 5
    }

    // handle invalid duration by setting it to the default value of 1 minute
    if config.RestBridgeTimeout <= 0 {
        config.RestBridgeTimeout = 1
    }

    // the raw value from the config.json needs to be multiplied by time.Minute otherwise it's interpreted as nanosecond
    config.ShutdownTimeout = config.ShutdownTimeout * time.Minute

    // the raw value from the config.json needs to be multiplied by time.Minute otherwise it's interpreted as nanosecond
    config.RestBridgeTimeout = config.RestBridgeTimeout * time.Minute

    if config.TLSCertConfig != nil {
        if !path.IsAbs(config.TLSCertConfig.CertFile) {
            config.TLSCertConfig.CertFile = path.Clean(path.Join(config.RootDir, config.TLSCertConfig.CertFile))
        }

        if !path.IsAbs(config.TLSCertConfig.KeyFile) {
            config.TLSCertConfig.KeyFile = path.Clean(path.Join(config.RootDir, config.TLSCertConfig.KeyFile))
        }
    }

    ps.serverConfig = &config
    ps.ServerAvailability = &ServerAvailability{}
    ps.routerConcurrencyProtection = new(int32)
    ps.initialize()
    return ps, nil
}

// CreateServerConfig creates a new instance of PlatformServerConfig and returns the pointer to it.
func CreateServerConfig() (*PlatformServerConfig, error) {
    factory := &serverConfigFactory{}
    factory.configureFlags(pflag.CommandLine)
    return generatePlatformServerConfig(factory)
}

// GetConnectionListener
func (ps *platformServer) GetFabricConnectionListener() stompserver.RawConnectionListener {
    return ps.fabricConn
}

// StartServer starts listening on the host and port as specified by ServerConfig
func (ps *platformServer) StartServer(syschan chan os.Signal) {
    connClosed := make(chan struct{})

    ps.SyscallChan = syschan
    signal.Notify(ps.SyscallChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

    // ensure port is available
    ps.checkPortAvailability()

    // finalize handler by setting out writer
    ps.loadGlobalHttpHandler(ps.router)

    // configure SPA
    // NOTE: the reason SPA app route is configured during server startup is that if the base uri is `/` for SPA
    // then all other routes registered after SPA route will be masked away.
    ps.configureSPA()

    // if Fabric broker configuration is found, start the broker
    if ps.serverConfig.FabricConfig != nil {
        go func() {
            fabricPort := ps.serverConfig.Port
            fabricEndpoint := ps.serverConfig.FabricConfig.FabricEndpoint
            if ps.serverConfig.FabricConfig.UseTCP {
                // if using TCP adjust port accordingly and drop endpoint
                fabricPort = ps.serverConfig.FabricConfig.TCPPort
                fabricEndpoint = ""
            }
            brokerLocation := fmt.Sprintf("%s:%d%s", ps.serverConfig.Host, fabricPort, fabricEndpoint)
            ps.serverConfig.Logger.Info("[ranch] hot-dang! starting up the ranch's STOMP message broker", "location", brokerLocation)
            ps.ServerAvailability.Fabric = true

            if err := ps.eventbus.StartFabricEndpoint(ps.fabricConn, *ps.serverConfig.FabricConfig.EndpointConfig); err != nil {
                ps.serverConfig.Logger.Error(wrapError(errServerInit, err).Error())
            }
        }()
    }

    go func() {
        ps.ServerAvailability.Http = true
        if ps.serverConfig.TLSCertConfig != nil {
            ps.serverConfig.Logger.Info("[ranch] yee-haw! starting up the ranch's HTTPS server at %s:%d with TLS", "host", ps.serverConfig.Host, "port", ps.serverConfig.Port)
            if err := ps.HttpServer.ListenAndServeTLS(ps.serverConfig.TLSCertConfig.CertFile, ps.serverConfig.TLSCertConfig.KeyFile); err != nil {
                if !errors.Is(err, http.ErrServerClosed) {
                    ps.serverConfig.Logger.Error(wrapError(errServerInit, err).Error())
                }
            }
        } else {
            ps.serverConfig.Logger.Info("[ranch] yee-haw! starting up the ranch's HTTP server", "host", ps.serverConfig.Host, "port", ps.serverConfig.Port)
            if err := ps.HttpServer.ListenAndServe(); err != nil {
                if !errors.Is(err, http.ErrServerClosed) {
                    ps.serverConfig.Logger.Error(wrapError(errServerInit, err).Error())
                }
            }
        }
    }()

    // spawn another goroutine to respond to syscall to shut down servers and terminate the main thread
    go func() {
        <-ps.SyscallChan
        // notify subscribers that the server is shutting down
        _ = ps.eventbus.SendResponseMessage(RANCH_SERVER_ONLINE_CHANNEL, false, nil)
        ps.StopServer()
        close(connClosed)
    }()

    // notify subscribers that the server is ready to interact with
    httpReady := false
    for {
        _, err := net.Dial("tcp", fmt.Sprintf(":%d", ps.serverConfig.Port))
        httpReady = err == nil
        if !httpReady {
            time.Sleep(1 * time.Millisecond)
            continue
        }
        _ = ps.eventbus.SendResponseMessage(RANCH_SERVER_ONLINE_CHANNEL, true, nil)
        break
    }

    <-connClosed
}

// StopServer attempts to gracefully stop the HTTP and STOMP server if running
func (ps *platformServer) StopServer() {
    ps.serverConfig.Logger.Info("[ranch] server shutting down... see you around soon, partner!")
    ps.ServerAvailability.Http = false

    baseCtx := context.Background()
    shutdownCtx, cancel := context.WithTimeout(baseCtx, ps.serverConfig.ShutdownTimeout)

    go func() {
        select {
        case <-shutdownCtx.Done():
            if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
                ps.serverConfig.Logger.Error(
                    "server failed to gracefully shut down after timeout", "timeout",
                    ps.serverConfig.ShutdownTimeout.String())
            }
        }
    }()
    defer cancel()

    // call all registered services' OnServerShutdown() hook
    svcRegistry := service.GetServiceRegistry()
    lcm := service.GetServiceLifecycleManager()
    wg := sync.WaitGroup{}
    for _, svcChannel := range svcRegistry.GetAllServiceChannels() {
        hooks := lcm.GetOnServerShutdownService(svcChannel)
        if hooks != nil {
            ps.serverConfig.Logger.Info("teardown in progress for service", "channel", svcChannel)
            wg.Add(1)
            go func(cName string, h service.OnServerShutdownEnabled) {
                h.OnServerShutdown()
                ps.serverConfig.Logger.Info("teardown completed for service", "channel", cName)
                wg.Done()

            }(svcChannel, hooks)
        }
    }

    // start graceful shutdown
    err := ps.HttpServer.Shutdown(shutdownCtx)
    if err != nil {
        ps.serverConfig.Logger.Error(err.Error())
    }

    if ps.fabricConn != nil {
        err = ps.eventbus.StopFabricEndpoint()
        if err != nil {
            ps.serverConfig.Logger.Error(err.Error())
        }
        ps.ServerAvailability.Fabric = false
    }

    // wait for all teardown jobs to be done. if shutdown deadline arrives earlier
    // the main thread will be terminated forcefully
    wg.Wait()
}

// SetStaticRoute adds a route where static resources will be served
func (ps *platformServer) SetStaticRoute(prefix, fullpath string, middlewareFn ...mux.MiddlewareFunc) {
    //ps.router.Handle(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    //	http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
    //}))

    ndir := NoDirFileSystem{http.Dir(fullpath)}
    endpointHandlerMapKey := prefix + "*"
    compositeHandler := http.StripPrefix(prefix, middleware.BasicSecurityHeaderMiddleware()(http.FileServer(ndir)))

    for _, mw := range middlewareFn {
        compositeHandler = mw(compositeHandler)
    }

    ps.endpointHandlerMap[endpointHandlerMapKey] = compositeHandler.(http.HandlerFunc)
    ps.router.PathPrefix(prefix + "/").Name(endpointHandlerMapKey).Handler(ps.endpointHandlerMap[endpointHandlerMapKey])
}

// RegisterService registers a Fabric service with Bifrost
func (ps *platformServer) RegisterService(svc service.FabricService, svcChannel string) error {
    sr := service.GetServiceRegistry()
    err := sr.RegisterService(svc, svcChannel)
    svcType := reflect.TypeOf(svc)

    if err == nil {
        ps.serverConfig.Logger.Info("[ranch] Service registered", "name", svcType.String(), "channel", svcChannel)
        svcLifecycleManager := service.GetServiceLifecycleManager()
        var hooks service.OnServiceReadyEnabled
        if hooks = svcLifecycleManager.GetOnReadyCapableService(svcChannel); hooks == nil {
            // if service has no lifecycle hooks mark the channel as ready straight up
            storeManager := ps.eventbus.GetStoreManager()
            store := storeManager.GetStore(service.ServiceReadyStore)
            store.Put(svcChannel, true, service.ServiceInitStateChange)
            ps.serverConfig.Logger.Info("[ranch] service initialized successfully", "name", svcType.String())
        }
    }
    return err
}

// SetHttpChannelBridge establishes a conduit between the transport service channel and an HTTP endpoint
// that allows a client to invoke the service via REST.
func (ps *platformServer) SetHttpChannelBridge(bridgeConfig *service.RESTBridgeConfig) {
    ps.lock.Lock()
    defer ps.lock.Unlock()

    endpointHandlerKey := bridgeConfig.Uri + "-" + bridgeConfig.Method

    if _, ok := ps.endpointHandlerMap[endpointHandlerKey]; ok {
        ps.serverConfig.Logger.Warn("[ranch] endpoint is already associated with a handler, "+
            "Try another endpoint or remove it before assigning a new handler", "uri", bridgeConfig.Uri, "method", bridgeConfig.Method)
        return
    }

    // create a map for service channel - bridges mapping if it does not exist
    if ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] == nil {
        ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] = make([]string, 0)
    }

    if _, exists := ps.messageBridgeMap[bridgeConfig.ServiceChannel]; !exists {
        handler, _ := ps.eventbus.ListenStream(bridgeConfig.ServiceChannel)
        handler.Handle(func(message *model.Message) {
            ps.messageBridgeMap[bridgeConfig.ServiceChannel].payloadChannel <- message
        }, func(err error) {})

        ps.messageBridgeMap[bridgeConfig.ServiceChannel] = &MessageBridge{
            ServiceListenStream: handler,
            payloadChannel:      make(chan *model.Message, 100),
        }
    }

    // NOTE: mux.Router does not have mutex or any locking mechanism so it could sometimes lead to concurrency write
    // panics. the following is to ensure the modification to ps.router can happen only once per thread, this atomic
    // counter also protects against concurrent writing to ps.endpointHandlerMap
    //for !atomic.CompareAndSwapInt32(ps.routerConcurrencyProtection, 0, 1) {
    //	time.Sleep(1 * time.Nanosecond)
    //}

    // build endpoint handler
    ps.endpointHandlerMap[endpointHandlerKey] = ps.buildEndpointHandler(
        bridgeConfig.ServiceChannel,
        bridgeConfig.FabricRequestBuilder,
        ps.serverConfig.RestBridgeTimeout,
        ps.messageBridgeMap[bridgeConfig.ServiceChannel].payloadChannel)

    ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] = append(
        ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel], endpointHandlerKey)

    permittedMethods := []string{bridgeConfig.Method}
    if bridgeConfig.AllowHead {
        permittedMethods = append(permittedMethods, http.MethodHead)
    }
    if bridgeConfig.AllowOptions {
        permittedMethods = append(permittedMethods, http.MethodOptions)
    }

    ps.router.
        Path(bridgeConfig.Uri).
        Methods(permittedMethods...).
        Name(fmt.Sprintf("%s-%s", bridgeConfig.Uri, bridgeConfig.Method)).
        Handler(ps.endpointHandlerMap[endpointHandlerKey])
    //if !atomic.CompareAndSwapInt32(ps.routerConcurrencyProtection, 1, 0) {
    //	panic("Concurrency write on router detected when running ")
    //}

    ps.serverConfig.Logger.Info(
        "[ranch] service channel is bridged to a REST endpoint",
        "channel", bridgeConfig.ServiceChannel, "url", bridgeConfig.Uri, "method", bridgeConfig.Method)
}

// SetHttpPathPrefixChannelBridge establishes a conduit between the transport service channel and a path prefix
// every request on this prefix will be sent through to the target service, all methods, all sub paths, lock, stock and barrel.
func (ps *platformServer) SetHttpPathPrefixChannelBridge(bridgeConfig *service.RESTBridgeConfig) {
    ps.lock.Lock()
    defer ps.lock.Unlock()

    endpointHandlerKey := bridgeConfig.Uri + "-" + AllMethodsWildcard

    if _, ok := ps.endpointHandlerMap[endpointHandlerKey]; ok {
        ps.serverConfig.Logger.Warn("[ranch] path prefix is already being handled. "+
            "Try another prefix or remove it before assigning a new handler", "uri", bridgeConfig.Uri, "method", bridgeConfig.Method)
        return
    }

    // create a map for service channel - bridges mapping if it does not exist
    if ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] == nil {
        ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] = make([]string, 0)
    }

    if _, exists := ps.messageBridgeMap[bridgeConfig.ServiceChannel]; !exists {
        handler, _ := ps.eventbus.ListenStream(bridgeConfig.ServiceChannel)
        handler.Handle(func(message *model.Message) {
            ps.messageBridgeMap[bridgeConfig.ServiceChannel].payloadChannel <- message
        }, func(err error) {})

        ps.messageBridgeMap[bridgeConfig.ServiceChannel] = &MessageBridge{
            ServiceListenStream: handler,
            payloadChannel:      make(chan *model.Message, 100),
        }
    }

    // build endpoint handler
    ps.endpointHandlerMap[endpointHandlerKey] = ps.buildEndpointHandler(
        bridgeConfig.ServiceChannel,
        bridgeConfig.FabricRequestBuilder,
        ps.serverConfig.RestBridgeTimeout,
        ps.messageBridgeMap[bridgeConfig.ServiceChannel].payloadChannel)

    ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel] = append(
        ps.serviceChanToBridgeEndpoints[bridgeConfig.ServiceChannel], endpointHandlerKey)

    // NOTE: mux.Router does not have mutex or any locking mechanism so it could sometimes lead to concurrency write
    // panics. the following is to ensure the modification to ps.router can happen only once per thread
    for !atomic.CompareAndSwapInt32(ps.routerConcurrencyProtection, 0, 1) {
        time.Sleep(1 * time.Nanosecond)
    }
    ps.router.
        PathPrefix(bridgeConfig.Uri).
        Name(endpointHandlerKey).
        Handler(ps.endpointHandlerMap[endpointHandlerKey])
    if !atomic.CompareAndSwapInt32(ps.routerConcurrencyProtection, 1, 0) {
        panic("Concurrency write on router detected when running SetHttpPathPrefixChannelBridge()")
    }

    ps.serverConfig.Logger.Info(
        "[ranch] Service channel is now bridged to a REST path prefix",
        "channel", bridgeConfig.ServiceChannel, "url", bridgeConfig.Uri)

}

// GetMiddlewareManager returns the MiddleManager instance
func (ps *platformServer) GetMiddlewareManager() middleware.MiddlewareManager {
    return ps.middlewareManager
}

func (ps *platformServer) GetRestBridgeSubRoute(uri, method string) (*mux.Route, error) {
    route, err := ps.getSubRoute(fmt.Sprintf("%s-%s", uri, method))
    if route == nil {
        return nil, fmt.Errorf("no route exists at %s (%s) exists", uri, method)
    }
    return route, err
}

// CustomizeTLSConfig is used to create a customized TLS configuration for use with http.Server.
// this function needs to be called before the server starts, otherwise it will error out.
func (c *platformServer) CustomizeTLSConfig(tls *tls.Config) error {
    if c.ServerAvailability.Http || c.ServerAvailability.Fabric {
        return fmt.Errorf("TLS configuration can be provided only if the server is not running")
    }
    c.HttpServer.TLSConfig = tls
    return nil
}

// clearHttpChannelBridgesForService takes serviceChannel, gets all mux.Route instances associated with
// the service and removes them while keeping the rest of the routes intact. returns the pointer
// of a new instance of mux.Router.
func (ps *platformServer) clearHttpChannelBridgesForService(serviceChannel string) *mux.Router {
    ps.lock.Lock()
    defer ps.lock.Unlock()

    // NOTE: gorilla mux doesn't allow us to mutate routes field of the Router struct which is critical in rerouting incoming
    // requests to the new route. there is not a public API that allows us to do it so we're instead creating a new instance of
    // Router and assigning the existing config and route. this means `ps.route` is treated as immutable and will be
    // replaced with a new instance of mux.Router by the operation performed in this function

    // walk over existing routes and store them temporarily EXCEPT the ones that are being overwritten which can
    // be tracked by the service channel
    newRouter := mux.NewRouter().Schemes("http", "https").Subrouter()
    lookupMap := make(map[string]bool)
    for _, key := range ps.serviceChanToBridgeEndpoints[serviceChannel] {
        lookupMap[key] = true
    }

    ps.router.Walk(func(r *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
        name := r.GetName()
        path, _ := r.GetPathTemplate()
        handler := r.GetHandler()
        methods, _ := r.GetMethods()
        // do not want to copy over the routes that will be overridden
        if lookupMap[name] {
            ps.serverConfig.Logger.Debug("[ranch] route will be overridden so not copying over to the new router instance", "route", name)
        } else {
            newRouter.Name(name).Path(path).Methods(methods...).Handler(handler)
        }
        return nil
    })

    // if in override mode delete existing mappings associated with the service
    existingMappings := ps.serviceChanToBridgeEndpoints[serviceChannel]
    ps.serviceChanToBridgeEndpoints[serviceChannel] = make([]string, 0)
    for _, handlerKey := range existingMappings {
        ps.serverConfig.Logger.Info("[ranch] Removing existing service - REST mapping", "key", handlerKey, "channel", serviceChannel)
        delete(ps.endpointHandlerMap, handlerKey)
    }
    return newRouter
}

func (ps *platformServer) getSubRoute(name string) (*mux.Route, error) {
    route := ps.router.Get(name)
    if route == nil {
        return nil, fmt.Errorf("no route exists under name %s", name)
    }
    return route, nil
}

func (ps *platformServer) loadGlobalHttpHandler(h *mux.Router) {
    ps.lock.Lock()
    defer ps.lock.Unlock()
    ps.router = h
    ps.HttpServer.Handler = handlers.RecoveryHandler()(
        handlers.CompressHandler(
            handlers.ProxyHeaders(ps.router)))
    //handlers.CombinedLoggingHandler(
    //	ps.serverConfig.LogConfig.GetAccessLogFilePointer(), ps.router)))
}

func (ps *platformServer) checkPortAvailability() {
    // is the port free?
    _, err := net.Dial("tcp", fmt.Sprintf(":%d", ps.serverConfig.Port))

    // connection should fail otherwise it means there's already a listener on the host+port combination, in which case we stop here
    if err == nil {
        ps.serverConfig.Logger.Error("Server could not start because another process is using the port - try another",
            "host", ps.serverConfig.Host, "port", ps.serverConfig.Port)
    }
}

func (ps *platformServer) setEventBusRef(evtBus bus.EventBus) {
    ps.lock.Lock()
    ps.eventbus = evtBus
    ps.lock.Unlock()
}
