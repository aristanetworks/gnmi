/*
Copyright 2018 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The gnmi_collector program implements a caching gNMI collector.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/client"
	gnmiclient "github.com/openconfig/gnmi/client/gnmi"
	coll "github.com/openconfig/gnmi/collector"
	cpb "github.com/openconfig/gnmi/proto/collector"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	tpb "github.com/openconfig/gnmi/proto/target"
	"github.com/openconfig/gnmi/subscribe"
	"github.com/openconfig/gnmi/target"
	tunnelpb "github.com/openconfig/grpctunnel/proto/tunnel"
	"github.com/openconfig/grpctunnel/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	tunnelCert = flag.String("tunnel_cert", "", "Tunnel certificate file.")
	tunnelKey  = flag.String("tunnel_key", "", "Tunnel private key file.")
	tunnelCA   = flag.String("tunnel_ca", "", "Tunnel CA certificate file.")

	targetName   = flag.String("target", "", "Name of the gNMI target.")
	gnmiCert     = flag.String("gnmi_cert", "", "gNMI certificate file.")
	gnmiKey      = flag.String("gnmi_key", "", "gNMI private key file.")
	gnmiCA       = flag.String("gnmi_ca", "", "gNMI CA certificate file.")
	gnmiInsecure = flag.Bool("gnmi_insecure", false, "Skip TLS validation for gNMI connection.")

	username = flag.String("username", "", "Username to authenticate with.")
	password = flag.String("password", "", "Password to authenticate with.")

	configFile  = flag.String("config_file", "", "File path for collector configuration.")
	port        = flag.Int("port", 0, "server port")
	dialTimeout = flag.Duration("dial_timeout", time.Minute,
		"Timeout for dialing a connection to a target.")
	metadataUpdatePeriod = flag.Duration("metadata_update_period", 0,
		"Period for target metadata update. 0 disables updates.")
	sizeUpdatePeriod = flag.Duration("size_update_period", 0,
		"Period for updating the target size in metadata. 0 disables updates.")
	// non-tunnel request will be contained in the config file.
	tunnelRequest = flag.String("tunnel_request", "", "request to be performed via tunnel")
)

func loadCertificates(cert, key, ca string) ([]tls.Certificate, *x509.CertPool) {
	var certificates []tls.Certificate
	var certPool *x509.CertPool
	certificate, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		log.Fatal(err)
	}
	certificates = []tls.Certificate{certificate}
	if ca != "" {
		b, err := ioutil.ReadFile(ca)
		if err != nil {
			log.Fatal(err)
		}
		certPool = x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(b) {
			log.Fatal(err)
		}
	}
	return certificates, certPool
}

func getServerCredentials() grpc.ServerOption {
	certificates, certPool := loadCertificates(*tunnelCert, *tunnelKey, *tunnelCA)
	clientAuth := tls.RequireAndVerifyClientCert
	if certPool == nil {
		clientAuth = tls.NoClientCert
	}
	return grpc.Creds(credentials.NewTLS(&tls.Config{
		ClientAuth:   clientAuth,
		Certificates: certificates,
		ClientCAs:    certPool,
	}))
}

func getClientTLS() *tls.Config {
	tlsConfig := &tls.Config{}
	if *gnmiInsecure {
		tlsConfig.InsecureSkipVerify = true
	} else {
		certificates, certPool := loadCertificates(*gnmiCert, *gnmiKey, *gnmiCA)
		tlsConfig.ServerName = *targetName
		tlsConfig.Certificates = certificates
		tlsConfig.RootCAs = certPool
	}
	return tlsConfig
}

func periodic(period time.Duration, fn func()) {
	if period == 0 {
		return
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for range t.C {
		fn()
	}
}

func (c *collector) isTargetActive(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.tActive[id]
	return ok
}

func (c *collector) addTargetHandler(t tunnel.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.tActive[t.ID]; ok {
		return fmt.Errorf("trying to add target %s, but already in config", t.ID)
	}
	c.chAddTarget <- t
	return nil
}

func (c *collector) deleteTargetHandler(t tunnel.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.tActive[t.ID]; !ok {
		return fmt.Errorf("trying to delete target %s, but not found in config", t.ID)
	}
	c.chDeleteTarget <- t
	return nil
}

// Under normal conditions, this function will not terminate.  Cancelling
// the context will stop the collector.
func runCollector(ctx context.Context) error {
	if *configFile == "" {
		return errors.New("config_file must be specified")
	}
	if (*tunnelCert != "" && *tunnelKey == "") || (*tunnelCert == "" && *tunnelKey != "") {
		log.Fatal("tunnel_cert and tunnel_key must be specified")
	}
	if (*gnmiCert != "" && *gnmiKey == "") || (*gnmiCert == "" && *gnmiKey != "") {
		log.Fatal("gnmi_cert and gnmi_key must be specified")
	}
	if *tunnelCert == "" && *tunnelCA != "" {
		log.Fatal("tunnel_ca must be specified with tunnel_cert and tunnel_key")
	}
	if *gnmiCert == "" && *gnmiCA != "" {
		log.Fatal("gnmi_ca must be specified with gnmi_cert and gnmi_key")
	}
	if *gnmiCert != "" && *targetName == "" {
		log.Fatal("target must be specified with gnmi_cert and gnmi_key")
	}

	c := collector{config: &tpb.Configuration{},
		cancelFuncs:    map[string]func(){},
		tActive:        map[string]*tunnTarget{},
		tRequest:       *tunnelRequest,
		chDeleteTarget: make(chan tunnel.Target, 1),
		chAddTarget:    make(chan tunnel.Target, 1),
		addr:           fmt.Sprintf("localhost:%d", *port)}

	// Initialize configuration.
	buf, err := ioutil.ReadFile(*configFile)
	if err != nil {
		return fmt.Errorf("Could not read configuration from %q: %v", *configFile, err)
	}
	if err := proto.UnmarshalText(string(buf), c.config); err != nil {
		return fmt.Errorf("Could not parse configuration from %q: %v", *configFile, err)
	}
	if err := target.Validate(c.config); err != nil {
		return fmt.Errorf("Configuration in %q is invalid: %v", *configFile, err)
	}

	// Initialize TLS credentials.
	var serverOptions []grpc.ServerOption
	if *tunnelCert != "" {
		serverOptions = append(serverOptions, getServerCredentials())
	}

	// Create a grpc Server.
	srv := grpc.NewServer(serverOptions...)

	// Initialize tunnel server.
	c.tServer, err = tunnel.NewServer(tunnel.ServerConfig{
		AddTargetHandler: c.addTargetHandler, DeleteTargetHandler: c.deleteTargetHandler})
	if err != nil {
		log.Fatalf("failed to setup tunnel server: %v", err)
	}
	tunnelpb.RegisterTunnelServer(srv, c.tServer)

	// Initialize cache.
	c.cache = cache.New(nil)

	// Start functions to periodically update metadata stored in the cache for each target.
	go periodic(*metadataUpdatePeriod, c.cache.UpdateMetadata)
	go periodic(*sizeUpdatePeriod, c.cache.UpdateSize)

	// Initialize collectors.
	c.start(context.Background())

	// Initialize the Collector server.
	cpb.RegisterCollectorServer(srv, coll.New(c.reconnect))
	// Initialize gNMI Proxy Subscribe server.
	subscribeSrv, err := subscribe.NewServer(c.cache)
	if err != nil {
		return fmt.Errorf("Could not instantiate gNMI server: %v", err)
	}
	gnmipb.RegisterGNMIServer(srv, subscribeSrv)
	// Forward streaming updates to clients.
	c.cache.SetClient(subscribeSrv.Update)
	// Register listening port and start serving.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("gNMI server error: %v", err)
		}
	}()
	defer srv.Stop()
	<-ctx.Done()
	return ctx.Err()
}

// Container for some of the target state data. It is created once
// for every device and used as a closure parameter by ProtoHandler.
type state struct {
	name   string
	target *cache.Target
	// connected status is set to true when the first gnmi notification is received.
	// it gets reset to false when disconnect call back of ReconnectClient is called.
	connected bool
}

func (s *state) disconnect() {
	s.connected = false
	s.target.Reset()
}

// handleUpdate parses a protobuf message received from the target. This implementation handles only
// gNMI SubscribeResponse messages. When the message is an Update, the GnmiUpdate method of the
// cache.Target is called to generate an update. If the message is a sync_response, then target is
// marked as synchronised.
func (s *state) handleUpdate(msg proto.Message) error {
	if !s.connected {
		s.target.Connect()
		s.connected = true
	}
	resp, ok := msg.(*gnmipb.SubscribeResponse)
	if !ok {
		return fmt.Errorf("failed to type assert message %#v", msg)
	}
	switch v := resp.Response.(type) {
	case *gnmipb.SubscribeResponse_Update:
		// Gracefully handle gNMI implementations that do not set Prefix.Target in their
		// SubscribeResponse Updates.
		if v.Update.GetPrefix() == nil {
			v.Update.Prefix = &gnmipb.Path{}
		}
		if v.Update.Prefix.Target == "" {
			v.Update.Prefix.Target = s.name
		}
		_ = s.target.GnmiUpdate(v.Update)
		log.Print(v)
	case *gnmipb.SubscribeResponse_SyncResponse:
		s.target.Sync()
	case *gnmipb.SubscribeResponse_Error:
		return fmt.Errorf("error in response: %s", v)
	default:
		return fmt.Errorf("unknown response %T: %s", v, v)
	}
	return nil
}

// Struct for managing the dynamic creation of tunnel targets.
type tunnTarget struct {
	conf *tpb.Target
	conn *tunnel.Conn
}

type collector struct {
	cache          *cache.Cache
	config         *tpb.Configuration
	mu             sync.Mutex
	cancelFuncs    map[string]func()
	addr           string
	tActive        map[string]*tunnTarget
	tServer        *tunnel.Server
	tRequest       string
	chAddTarget    chan tunnel.Target
	chDeleteTarget chan tunnel.Target
}

func (c *collector) addCancel(target string, cancel func()) {
	c.mu.Lock()
	c.cancelFuncs[target] = cancel
	c.mu.Unlock()
}

func (c *collector) reconnect(target string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cancel, ok := c.cancelFuncs[target]
	if !ok {
		return fmt.Errorf("no such target: %q", target)
	}
	cancel()
	delete(c.cancelFuncs, target)
	return nil
}

func (c *collector) runSingleTarget(ctx context.Context, targetID string, tc *tunnel.Conn) {
	c.mu.Lock()
	target, ok := c.tActive[targetID]
	c.mu.Unlock()
	if !ok {
		log.Printf("Unknown tunnel target %q", targetID)
		return
	}

	go func(name string, target *tunnTarget) {
		s := &state{name: name, target: c.cache.Add(name)}
		qr := c.config.Request[target.conf.GetRequest()]
		q, err := client.NewQuery(qr)
		if err != nil {
			log.Printf("NewQuery(%s): %v", qr.String(), err)
			return
		}
		q.Addrs = target.conf.GetAddresses()

		user := *username
		pass := *password
		if target.conf.Credentials != nil {
			user = target.conf.Credentials.Username
			pass = target.conf.Credentials.Password
		}
		if user != "" {
			q.Credentials = &client.Credentials{
				Username: user,
				Password: pass,
			}
		}

		if *gnmiInsecure || *gnmiCert != "" {
			q.TLS = getClientTLS()
		}

		q.Target = name
		q.Timeout = *dialTimeout
		q.ProtoHandler = s.handleUpdate
		q.TunnelConn = target.conn
		if err := q.Validate(); err != nil {
			log.Printf("query.Validate(): %v", err)
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
		cl := client.BaseClient{}
		if err := cl.Subscribe(ctx, q, gnmiclient.Type); err != nil {
			log.Printf("Subscribe failed for target %q: %v", name, err)
			// remove target once it becomes unavailable
			if err := c.removeTarget(name); err != nil {
				log.Printf("removeTarget %q error : %v", name, err)
			}
		}
	}(targetID, target)
}

func (c *collector) start(ctx context.Context) {
	// First, run for non-tunnel targets.
	for id := range c.config.Target {
		c.runSingleTarget(ctx, id, nil)
	}

	if c.tRequest == "" {
		log.Printf("tunnel request is not specified")
	}
	// Monitor targets from the tunnel.
	go func() {
		log.Printf("watching for tunnel connections")
		for {
			select {
			case target := <-c.chAddTarget:
				log.Printf("adding target: %+v", target)
				if target.Type != tunnelpb.TargetType_GNMI_GNOI.String() {
					log.Printf("received unsupported type type: %s from target: %s, skipping",
						target.Type, target.ID)
					continue
				}
				if c.isTargetActive(target.ID) {
					log.Printf("received target %s, which is already registered. skipping",
						target.ID)
					continue
				}

				ctx, cancel := context.WithCancel(ctx)
				c.addCancel(target.ID, cancel)

				tc, err := tunnel.ServerConn(ctx, c.tServer, c.addr, &target)
				if err != nil {
					log.Printf("failed to get new tunnel session for target %v:%v", target.ID, err)
					continue
				}
				cfg := c.config.Target[target.ID]
				if cfg == nil {
					cfg = &tpb.Target{
						Request: c.tRequest,
					}
				}
				t := &tunnTarget{
					conn: tc,
					conf: cfg,
				}
				if err := c.addTarget(ctx, target.ID, t); err != nil {
					log.Printf("failed to add target %s: %s", target.ID, err)
				}
				c.runSingleTarget(ctx, target.ID, tc)

			case target := <-c.chDeleteTarget:
				log.Printf("deleting target: %+v", target)
				if target.Type != tunnelpb.TargetType_GNMI_GNOI.String() {
					log.Printf("received unsupported type type: %s from target: %s, skipping",
						target.Type, target.ID)
					continue
				}
				if !c.isTargetActive(target.ID) {
					log.Printf("received target %s, which is not registered. skipping", target.ID)
					continue
				}
				if err := c.removeTarget(target.ID); err != nil {
					log.Printf("failed to remove target %s: %s", target.ID, err)
				}
			}
		}
	}()
}

func (c *collector) removeTarget(target string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tActive[target]
	if !ok {
		log.Printf("trying to remove target %s, but not found in config. do nothing", target)
		return nil
	}
	delete(c.tActive, target)
	cancel, ok := c.cancelFuncs[target]
	if !ok {
		return fmt.Errorf("cannot find cancel for target %q", target)
	}
	cancel()
	delete(c.cancelFuncs, target)
	t.conn.Close()
	log.Printf("target %s removed", target)
	return nil
}

func (c *collector) addTarget(ctx context.Context, name string, target *tunnTarget) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.tActive[name]; ok {
		log.Printf("trying to add target %q:%+v, but already in exists. do nothing", name, target)
		return nil
	}

	// For non-tunnel target, don't modify the addresses.
	if target.conf.GetDialer() == "tunnel" {
		target.conf.Addresses = []string{c.addr}
	}

	c.tActive[name] = target
	log.Printf("Added target: %q:%+v", name, target)
	return nil
}

func main() {
	// Flag initialization.
	flag.Parse()
	log.SetOutput(os.Stdout)
	if err := runCollector(context.Background()); err != nil {
		log.Print(err)
	}
}
