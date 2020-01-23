package smokescreen

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	proxyproto "github.com/armon/go-proxyproto"
	"github.com/elazarl/goproxy"
	"github.com/sirupsen/logrus"
	"github.com/stripe/go-einhorn/einhorn"
	acl "github.com/stripe/smokescreen/pkg/smokescreen/acl/v1"
	"github.com/stripe/smokescreen/pkg/smokescreen/conntrack"
)

const (
	ipAllowDefault ipType = iota
	ipAllowUserConfigured
	ipDenyNotGlobalUnicast
	ipDenyPrivateRange
	ipDenyUserConfigured

	denyMsgTmpl = "Egress proxying is denied to host '%s': %s."

	httpProxy    = "http"
	connectProxy = "connect"
)

var LOGLINE_CANONICAL_PROXY_DECISION = "CANONICAL-PROXY-DECISION"

type ipType int

type aclDecision struct {
	reason, role, project, outboundHost string
	resolvedAddr                        *net.TCPAddr
	allow                               bool
	enforceWouldDeny                    bool
}

type smokescreenContext struct {
	cfg       *Config
	start     time.Time
	decision  *aclDecision
	traceId   string
	proxyType string
}

type denyError struct {
	error
}

func (t ipType) IsAllowed() bool {
	return t == ipAllowDefault || t == ipAllowUserConfigured
}

func (t ipType) String() string {
	switch t {
	case ipAllowDefault:
		return "Allow: Default"
	case ipAllowUserConfigured:
		return "Allow: User Configured"
	case ipDenyNotGlobalUnicast:
		return "Deny: Not Global Unicast"
	case ipDenyPrivateRange:
		return "Deny: Private Range"
	case ipDenyUserConfigured:
		return "Deny: User Configured"
	default:
		panic(fmt.Errorf("unknown ip type %d", t))
	}
}

func (t ipType) statsdString() string {
	switch t {
	case ipAllowDefault:
		return "resolver.allow.default"
	case ipAllowUserConfigured:
		return "resolver.allow.user_configured"
	case ipDenyNotGlobalUnicast:
		return "resolver.deny.not_global_unicast"
	case ipDenyPrivateRange:
		return "resolver.deny.private_range"
	case ipDenyUserConfigured:
		return "resolver.deny.user_configured"
	default:
		panic(fmt.Errorf("unknown ip type %d", t))
	}
}

const errorHeader = "X-Smokescreen-Error"
const roleHeader = "X-Smokescreen-Role"
const traceHeader = "X-Smokescreen-Trace-ID"

func addrIsInRuleRange(ranges []RuleRange, addr *net.TCPAddr) bool {
	for _, rng := range ranges {
		// If the range specifies a port and the port doesn't match,
		// then this range doesn't match
		if rng.Port != 0 && addr.Port != rng.Port {
			continue
		}

		if rng.Net.Contains(addr.IP) {
			return true
		}
	}
	return false
}

func classifyAddr(config *Config, addr *net.TCPAddr) ipType {
	if !addr.IP.IsGlobalUnicast() || addr.IP.IsLoopback() {
		if addrIsInRuleRange(config.AllowRanges, addr) {
			return ipAllowUserConfigured
		} else {
			return ipDenyNotGlobalUnicast
		}
	}

	if addrIsInRuleRange(config.AllowRanges, addr) {
		return ipAllowUserConfigured
	} else if addrIsInRuleRange(config.DenyRanges, addr) {
		return ipDenyUserConfigured
	} else if addrIsInRuleRange(PrivateRuleRanges, addr) {
		return ipDenyPrivateRange
	} else {
		return ipAllowDefault
	}
}

func resolveTCPAddr(config *Config, network, addr string) (*net.TCPAddr, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("unknown network type %q", network)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	resolvedPort, err := config.Resolver.LookupPort(ctx, network, port)
	if err != nil {
		return nil, err
	}

	ips, err := config.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) < 1 {
		return nil, fmt.Errorf("no IPs resolved")
	}

	return &net.TCPAddr{
		IP:   ips[0].IP,
		Zone: ips[0].Zone,
		Port: resolvedPort,
	}, nil
}

func safeResolve(config *Config, network, addr string) (*net.TCPAddr, string, error) {
	config.StatsdClient.Incr("resolver.attempts_total", []string{}, 1)
	resolved, err := resolveTCPAddr(config, network, addr)
	if err != nil {
		config.StatsdClient.Incr("resolver.errors_total", []string{}, 1)
		return nil, "", err
	}

	classification := classifyAddr(config, resolved)
	config.StatsdClient.Incr(classification.statsdString(), []string{}, 1)

	if classification.IsAllowed() {
		return resolved, classification.String(), nil
	}
	return nil, "destination address was denied by rule, see error", denyError{fmt.Errorf("The destination address (%s) was denied by rule '%s'", resolved.IP, classification)}
}

func proxyContext(ctx context.Context) (*goproxy.ProxyCtx, bool) {
	pctx, ok := ctx.Value(goproxy.ProxyContextKey).(*goproxy.ProxyCtx)
	return pctx, ok
}

func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	pctx, ok := proxyContext(ctx)
	if !ok {
		return nil, fmt.Errorf("dialContext missing required *goproxy.ProxyCtx")
	}

	sctx, ok := pctx.UserData.(*smokescreenContext)
	if !ok {
		return nil, fmt.Errorf("dialContext missing required *smokescreenContext")
	}
	d := sctx.decision

	// If an address hasn't been resolved, does not match the original outboundHost,
	// or is not tcp we must re-resolve it before establishing the connection.
	if d.resolvedAddr == nil || d.outboundHost != addr || network != "tcp" {
		var err error
		d.resolvedAddr, d.reason, err = safeResolve(sctx.cfg, network, addr)
		if err != nil {
			if _, ok := err.(denyError); ok {
				sctx.cfg.Log.WithFields(
					logrus.Fields{
						"address": addr,
						"error":   err,
					}).Error("unexpected illegal address in dialer")
			}
			return nil, err
		}
	}

	sctx.cfg.StatsdClient.Incr("cn.atpt.total", []string{}, 1)
	conn, err := net.DialTimeout(network, d.resolvedAddr.String(), sctx.cfg.ConnectTimeout)
	if err != nil {
		sctx.cfg.StatsdClient.Incr("cn.atpt.fail.total", []string{}, 1)
		return nil, err
	}
	sctx.cfg.StatsdClient.Incr("cn.atpt.success.total", []string{}, 1)

	// Only wrap CONNECT conns with an InstrumentedConn. Connections used for traditional HTTP proxy
	// requests are pooled and reused by net.Transport.
	if sctx.proxyType == connectProxy {
		conn = sctx.cfg.ConnTracker.NewInstrumentedConnWithTimeout(conn, sctx.cfg.IdleTimeout, sctx.traceId, d.role, d.outboundHost, sctx.proxyType)
	} else {
		conn = NewTimeoutConn(conn, sctx.cfg.IdleTimeout)
	}
	return conn, nil
}

func rejectResponse(req *http.Request, config *Config, err error) *http.Response {
	var msg string
	switch err.(type) {
	case denyError:
		msg = fmt.Sprintf(denyMsgTmpl, req.Host, err.Error())
	default:
		config.Log.WithFields(logrus.Fields{
			"error": err,
		}).Warn("rejectResponse called with unexpected error")
		msg = "An unexpected error occurred."
	}

	if config.AdditionalErrorMessageOnDeny != "" {
		msg = fmt.Sprintf("%s\n\n%s\n", msg, config.AdditionalErrorMessageOnDeny)
	}

	resp := goproxy.NewResponse(req,
		goproxy.ContentTypeText,
		http.StatusProxyAuthRequired,
		msg+"\n")
	resp.Status = "Request Rejected by Proxy" // change the default status message
	resp.ProtoMajor = req.ProtoMajor
	resp.ProtoMinor = req.ProtoMinor
	resp.Header.Set(errorHeader, msg)
	return resp
}

func configureTransport(tr *http.Transport, cfg *Config) {
	if cfg.TransportMaxIdleConns != 0 {
		tr.MaxIdleConns = cfg.TransportMaxIdleConns
	}

	if cfg.TransportMaxIdleConnsPerHost != 0 {
		tr.MaxIdleConnsPerHost = cfg.TransportMaxIdleConns
	}

	if cfg.IdleTimeout != 0 {
		tr.IdleConnTimeout = cfg.IdleTimeout
	}
}

func newContext(cfg *Config, proxyType string) *smokescreenContext {
	return &smokescreenContext{
		cfg:       cfg,
		start:     time.Now(),
		proxyType: proxyType,
	}
}

func BuildProxy(config *Config) *goproxy.ProxyHttpServer {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false
	configureTransport(proxy.Tr, config)

	// dialContext will be invoked for both CONNECT and traditional proxy requests
	proxy.Tr.DialContext = dialContext

	// Use a custom goproxy.RoundTripperFunc to ensure that the correct context is attached to the request.
	// This is only used for non-CONNECT HTTP proxy requests. For connect requests, goproxy automatically
	// attaches goproxy.ProxyCtx prior to calling dialContext.
	rtFn := goproxy.RoundTripperFunc(func(req *http.Request, pctx *goproxy.ProxyCtx) (*http.Response, error) {
		ctx := context.WithValue(req.Context(), goproxy.ProxyContextKey, pctx)
		return proxy.Tr.RoundTrip(req.WithContext(ctx))
	})

	// Associate a timeout with the CONNECT proxy client connection
	if config.IdleTimeout != 0 {
		proxy.ConnectClientConnHandler = func(conn net.Conn) net.Conn {
			return NewTimeoutConn(conn, config.IdleTimeout)
		}
	}

	// Handle traditional HTTP proxy
	proxy.OnRequest().DoFunc(func(req *http.Request, pctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		// Attach smokescreenContext to goproxy.ProxyCtx
		sctx := newContext(config, httpProxy)
		pctx.UserData = sctx

		// Set this on every request as every request mints a new goproxy.ProxyCtx
		pctx.RoundTripper = rtFn

		// Build an address parsable by net.ResolveTCPAddr
		remoteHost := req.Host
		if strings.LastIndex(remoteHost, ":") <= strings.LastIndex(remoteHost, "]") {
			switch req.URL.Scheme {
			case "http":
				remoteHost = net.JoinHostPort(remoteHost, "80")
			case "https":
				remoteHost = net.JoinHostPort(remoteHost, "443")
			default:
				remoteHost = net.JoinHostPort(remoteHost, "0")
			}
		}

		config.Log.WithFields(
			logrus.Fields{
				"source_ip":      req.RemoteAddr,
				"requested_host": req.Host,
				"url":            req.RequestURI,
				"trace_id":       req.Header.Get(traceHeader),
			}).Debug("received HTTP proxy request")

		decision, err := checkIfRequestShouldBeProxied(config, req, remoteHost)
		sctx.decision = decision
		sctx.traceId = req.Header.Get(traceHeader)

		req.Header.Del(roleHeader)
		req.Header.Del(traceHeader)

		if err != nil {
			pctx.Error = err
			return req, rejectResponse(req, config, err)
		}
		if !decision.allow {
			return req, rejectResponse(req, config, denyError{errors.New(decision.reason)})
		}

		// Proceed with proxying the request
		return req, nil
	})

	// Handle CONNECT proxy to TLS & other TCP protocols destination
	proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		defer ctx.Req.Header.Del(traceHeader)
		ctx.UserData = newContext(config, connectProxy)

		err := handleConnect(config, ctx)
		if err != nil {
			ctx.Resp = rejectResponse(ctx.Req, config, err)
			return goproxy.RejectConnect, ""
		}
		return goproxy.OkConnect, host
	})

	proxy.OnResponse().DoFunc(func(resp *http.Response, pctx *goproxy.ProxyCtx) *http.Response {
		sctx := pctx.UserData.(*smokescreenContext)
		if resp != nil && sctx.decision.allow {
			resp.Header.Del(errorHeader)
		}

		if resp == nil && pctx.Error != nil {
			logrus.Warnf("rejecting with %#v", pctx.Error)
			return rejectResponse(pctx.Req, config, pctx.Error)
		}

		// In case of an error, this function is called a second time to filter the
		// response we generate so this logger will be called once.
		logHTTP(config, pctx)
		return resp
	})
	return proxy
}

func logProxy(config *Config, pctx *goproxy.ProxyCtx, proxyType string) {
	sctx := pctx.UserData.(*smokescreenContext)

	var contentLength int64
	if pctx.Resp != nil {
		contentLength = pctx.Resp.ContentLength
	}

	fromHost, fromPort, _ := net.SplitHostPort(pctx.Req.RemoteAddr)

	fields := logrus.Fields{
		"proxy_type":     proxyType,
		"src_host":       fromHost,
		"src_port":       fromPort,
		"requested_host": pctx.Req.Host,
		"start_time":     sctx.start.Unix(),
		"content_length": contentLength,
		"trace_id":       sctx.traceId,
	}

	if sctx.decision.resolvedAddr != nil {
		fields["dest_ip"] = sctx.decision.resolvedAddr.IP.String()
		fields["dest_port"] = sctx.decision.resolvedAddr.Port
	}

	// attempt to retrieve information about the host originating the proxy request
	fields["src_host_common_name"] = "unknown"
	fields["src_host_organization_unit"] = "unknown"
	if pctx.Req.TLS != nil && len(pctx.Req.TLS.PeerCertificates) > 0 {
		fields["src_host_common_name"] = pctx.Req.TLS.PeerCertificates[0].Subject.CommonName
		var ouEntries = pctx.Req.TLS.PeerCertificates[0].Subject.OrganizationalUnit
		if ouEntries != nil && len(ouEntries) > 0 {
			fields["src_host_organization_unit"] = ouEntries[0]
		}
	}

	decision := sctx.decision
	if sctx.decision != nil {
		fields["role"] = decision.role
		fields["project"] = decision.project
		fields["decision_reason"] = decision.reason
		fields["enforce_would_deny"] = decision.enforceWouldDeny
		fields["allow"] = decision.allow
	}

	err := pctx.Error
	if err != nil {
		fields["error"] = err.Error()
	}

	entry := config.Log.WithFields(fields)
	var logMethod func(...interface{})
	if _, ok := err.(denyError); !ok && err != nil {
		logMethod = entry.Error
	} else if decision != nil && decision.allow {
		logMethod = entry.Info
	} else {
		logMethod = entry.Warn
	}
	logMethod(LOGLINE_CANONICAL_PROXY_DECISION)
}

func logHTTP(config *Config, pctx *goproxy.ProxyCtx) {
	logProxy(config, pctx, "http")
}

func handleConnect(config *Config, pctx *goproxy.ProxyCtx) error {
	config.Log.WithFields(
		logrus.Fields{
			"remote":         pctx.Req.RemoteAddr,
			"requested_host": pctx.Req.Host,
			"trace_id":       pctx.Req.Header.Get(traceHeader),
		}).Debug("received CONNECT proxy request")
	sctx := pctx.UserData.(*smokescreenContext)

	// Check if requesting role is allowed to talk to remote
	sctx.decision, pctx.Error = checkIfRequestShouldBeProxied(config, pctx.Req, pctx.Req.Host)
	sctx.traceId = pctx.Req.Header.Get(traceHeader)

	logProxy(config, pctx, "connect")
	if pctx.Error != nil {
		return pctx.Error
	}
	if !sctx.decision.allow {
		return denyError{errors.New(sctx.decision.reason)}
	}

	return nil
}

func findListener(ip string, defaultPort uint16) (net.Listener, error) {
	if einhorn.IsWorker() {
		listener, err := einhorn.GetListener(0)
		if err != nil {
			return nil, err
		}

		return &einhornListener{Listener: listener}, err
	} else {
		return net.Listen("tcp", fmt.Sprintf("%s:%d", ip, defaultPort))
	}
}

func StartWithConfig(config *Config, quit <-chan interface{}) {
	config.Log.Println("starting")
	proxy := BuildProxy(config)

	listener, err := findListener(config.Ip, config.Port)
	if err != nil {
		config.Log.Fatal("can't find listener", err)
	}

	if config.SupportProxyProtocol {
		listener = &proxyproto.Listener{Listener: listener}
	}

	var handler http.Handler = proxy

	if config.Healthcheck != nil {
		handler = &HealthcheckMiddleware{
			Proxy:       handler,
			Healthcheck: config.Healthcheck,
		}
	}

	// TLS support
	if config.TlsConfig != nil {
		listener = tls.NewListener(listener, config.TlsConfig)
	}

	// Setup connection tracking
	config.ConnTracker = conntrack.NewTracker(config.IdleTimeout, config.StatsdClient, config.Log, config.ShuttingDown)

	server := http.Server{
		Handler: handler,
	}

	config.ShuttingDown.Store(false)
	runServer(config, &server, listener, quit)
	return
}

func runServer(config *Config, server *http.Server, listener net.Listener, quit <-chan interface{}) {
	// Runs the server and shuts it down when it receives a signal.
	//
	// Why aren't we using goji's graceful shutdown library? Great question!
	//
	// There are several things we might want to do when shutting down gracefully:
	// 1. close the listening socket (so that we don't accept *new* connections)
	// 2. close *existing* keepalive connections once they become idle
	//
	// It is impossible to close existing keepalive connections, because goproxy
	// hijacks the socket and doesn't tell us when they become idle. So all we
	// can do is close the listening socket when we receive a signal, not accept
	// new connections, and then exit the program after a timeout.

	if len(config.StatsSocketDir) > 0 {
		config.StatsServer = StartStatsServer(config)
	}

	graceful := true
	kill := make(chan os.Signal, 1)
	signal.Notify(kill, syscall.SIGUSR2, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-kill:
			config.Log.Print("quitting gracefully")

		case <-quit:
			config.Log.Print("quitting now")
			graceful = false
		}
		config.ShuttingDown.Store(true)

		// Shutdown() will block until all connections are closed unless we
		// provide it with a cancellation context.
		timeout := config.ExitTimeout
		if !graceful {
			timeout = 10 * time.Second
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		err := server.Shutdown(ctx)
		if err != nil {
			config.Log.Errorf("error shutting down http server: %v", err)
		}
	}()

	if err := server.Serve(listener); err != http.ErrServerClosed {
		config.Log.Errorf("http serve error: %v", err)
	}

	if graceful {
		// Wait for all connections to close or become idle before
		// continuing in an attempt to shutdown gracefully.
		exit := make(chan bool, 1)

		// This subroutine blocks until all connections close.
		go func() {
			config.Log.Print("Waiting for all connections to close...")
			config.ConnTracker.Wg.Wait()
			config.Log.Print("All connections are closed. Continuing with shutdown...")
			exit <- true
		}()

		// Sometimes, connections don't close and remain in the idle state. This subroutine
		// waits until all open connections are idle before sending the exit signal.
		go func() {
			config.Log.Print("Waiting for all connections to become idle...")
			beginTs := time.Now()
			for {
				checkAgainIn := config.ConnTracker.MaybeIdleIn()
				if checkAgainIn > 0 {
					if time.Now().Sub(beginTs) > config.ExitTimeout {
						config.Log.Print(fmt.Sprintf("Timed out at %v while waiting for all open connections to become idle.", config.ExitTimeout))
						exit <- true
						break
					} else {
						config.Log.Print(fmt.Sprintf("There are still active connections. Waiting %v before checking again.", checkAgainIn))
						time.Sleep(checkAgainIn)
					}
				} else {
					config.Log.Print("All connections are idle. Continuing with shutdown...")
					exit <- true
					break
				}
			}
		}()

		// Wait for the exit signal.
		<-exit
	}

	// Close all open (and idle) connections to send their metrics to log.
	config.ConnTracker.Range(func(k, v interface{}) bool {
		k.(*conntrack.InstrumentedConn).Close()
		return true
	})

	if config.StatsServer != nil {
		config.StatsServer.Shutdown()
	}
}

// Extract the client's ACL role from the HTTP request, using the configured
// RoleFromRequest function.  Returns the role, or an error if the role cannot
// be determined (including no RoleFromRequest configured), unless
// AllowMissingRole is configured, in which case an empty role and no error is
// returned.
func getRole(config *Config, req *http.Request) (string, error) {
	var role string
	var err error

	if config.RoleFromRequest != nil {
		role, err = config.RoleFromRequest(req)
	} else {
		err = MissingRoleError("RoleFromRequest is not configured")
	}

	switch {
	case err == nil:
		return role, nil
	case IsMissingRoleError(err) && config.AllowMissingRole:
		return "", nil
	default:
		config.Log.WithFields(logrus.Fields{
			"error":              err,
			"is_missing_role":    IsMissingRoleError(err),
			"allow_missing_role": config.AllowMissingRole,
		}).Error("Unable to get role for request")
		return "", err
	}
}

func checkIfRequestShouldBeProxied(config *Config, req *http.Request, outboundHost string) (*aclDecision, error) {
	decision := checkACLsForRequest(config, req, outboundHost)

	if decision.allow {
		resolved, reason, err := safeResolve(config, "tcp", outboundHost)
		if err != nil {
			if _, ok := err.(denyError); !ok {
				return decision, err
			}
			decision.reason = fmt.Sprintf("%s. %s", err.Error(), reason)
			decision.allow = false
			decision.enforceWouldDeny = true
		} else {
			decision.resolvedAddr = resolved
		}
	}

	return decision, nil
}

func checkACLsForRequest(config *Config, req *http.Request, outboundHost string) *aclDecision {
	decision := &aclDecision{
		outboundHost: outboundHost,
	}

	if config.EgressACL == nil {
		decision.allow = true
		decision.reason = "Egress ACL is not configured"
		return decision
	}

	role, roleErr := getRole(config, req)
	if roleErr != nil {
		config.StatsdClient.Incr("acl.role_not_determined", []string{}, 1)
		decision.reason = "Client role cannot be determined"
		return decision
	}

	decision.role = role

	submatch := hostExtractRE.FindStringSubmatch(outboundHost)
	destination := submatch[1]

	aclDecision, err := config.EgressACL.Decide(role, destination)
	if err != nil {
		config.Log.WithFields(logrus.Fields{
			"error": err,
			"role":  role,
		}).Warn("EgressAcl.Decide returned an error.")

		config.StatsdClient.Incr("acl.decide_error", []string{}, 1)
		decision.reason = aclDecision.Reason
		return decision
	}

	tags := []string{
		fmt.Sprintf("role:%s", decision.role),
		fmt.Sprintf("def_rule:%t", aclDecision.Default),
		fmt.Sprintf("project:%s", aclDecision.Project),
	}

	decision.reason = aclDecision.Reason
	switch aclDecision.Result {
	case acl.Deny:
		decision.enforceWouldDeny = true
		config.StatsdClient.Incr("acl.deny", tags, 1)

	case acl.AllowAndReport:
		decision.enforceWouldDeny = true
		config.StatsdClient.Incr("acl.report", tags, 1)
		decision.allow = true

	case acl.Allow:
		// Well, everything is going as expected.
		decision.allow = true
		decision.enforceWouldDeny = false
		config.StatsdClient.Incr("acl.allow", tags, 1)
	default:
		config.Log.WithFields(logrus.Fields{
			"role":        role,
			"destination": destination,
			"action":      aclDecision.Result.String(),
		}).Warn("Unknown ACL action")
		decision.reason = "Internal error"
		config.StatsdClient.Incr("acl.unknown_error", tags, 1)
	}

	return decision
}
