// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package rpc

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/netutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

func newTestServer(t *testing.T, ctx *Context, compression bool) (*grpc.Server, net.Listener) {
	tlsConfig, err := ctx.GetServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	opts := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.RPCDecompressor(snappyDecompressor{}),
	}
	if compression {
		opts = append(opts, grpc.RPCCompressor(snappyCompressor{}))
	}
	s := grpc.NewServer(opts...)

	ln, err := netutil.ListenAndServeGRPC(ctx.Stopper, s, util.TestAddr)
	if err != nil {
		t.Fatal(err)
	}

	return s, ln
}

func TestHeartbeatCB(t *testing.T) {
	defer leaktest.AfterTest(t)()

	for _, compression := range []bool{false, true} {
		t.Run("", func(t *testing.T) {
			stopper := stop.NewStopper()
			defer stopper.Stop()

			clock := hlc.NewClock(time.Unix(0, 20).UnixNano, time.Nanosecond)
			serverCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
			serverCtx.rpcCompression = compression
			s, ln := newTestServer(t, serverCtx, true)
			remoteAddr := ln.Addr().String()

			RegisterHeartbeatServer(s, &HeartbeatService{
				clock:              clock,
				remoteClockMonitor: serverCtx.RemoteClocks,
			})

			// Clocks don't matter in this test.
			clientCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
			clientCtx.rpcCompression = compression

			var once sync.Once
			ch := make(chan struct{})

			clientCtx.HeartbeatCB = func() {
				once.Do(func() {
					close(ch)
				})
			}

			_, err := clientCtx.GRPCDial(remoteAddr)
			if err != nil {
				t.Fatal(err)
			}

			<-ch
		})
	}
}

// TestHeartbeatHealth verifies that the health status changes after
// heartbeats succeed or fail.
func TestHeartbeatHealth(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()

	// Can't be zero because that'd be an empty offset.
	clock := hlc.NewClock(time.Unix(0, 1).UnixNano, time.Nanosecond)

	serverCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	s, ln := newTestServer(t, serverCtx, true)
	remoteAddr := ln.Addr().String()

	heartbeat := &ManualHeartbeatService{
		ready:              make(chan error),
		stopper:            stopper,
		clock:              clock,
		remoteClockMonitor: serverCtx.RemoteClocks,
	}
	RegisterHeartbeatServer(s, heartbeat)

	clientCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	// Make the interval shorter to speed up the test.
	clientCtx.heartbeatInterval = 1 * time.Millisecond
	if _, err := clientCtx.GRPCDial(remoteAddr); err != nil {
		t.Fatal(err)
	}

	errFailedHeartbeat := errors.New("failed heartbeat")

	var hbSuccess atomic.Value
	hbSuccess.Store(true)

	go func() {
		for {
			var err error
			if !hbSuccess.Load().(bool) {
				err = errFailedHeartbeat
			}

			select {
			case <-stopper.ShouldStop():
				return
			case heartbeat.ready <- err:
			}
		}
	}()

	// Wait for the connection.
	testutils.SucceedsSoon(t, func() error {
		err := clientCtx.ConnHealth(remoteAddr)
		if err != nil && err != errNotHeartbeated {
			t.Fatal(err)
		}
		return err
	})

	// Should be unhealthy in the presence of failing heartbeats.
	hbSuccess.Store(false)
	testutils.SucceedsSoon(t, func() error {
		if err := clientCtx.ConnHealth(remoteAddr); !testutils.IsError(err, errFailedHeartbeat.Error()) {
			return errors.Errorf("unexpected error: %v", err)
		}
		return nil
	})

	// Should become healthy in the presence of successful heartbeats.
	hbSuccess.Store(true)
	testutils.SucceedsSoon(t, func() error {
		return clientCtx.ConnHealth(remoteAddr)
	})

	// Should become unhealthy again in the presence of failing heartbeats.
	hbSuccess.Store(false)
	testutils.SucceedsSoon(t, func() error {
		if err := clientCtx.ConnHealth(remoteAddr); !testutils.IsError(err, errFailedHeartbeat.Error()) {
			return errors.Errorf("unexpected error: %v", err)
		}
		return nil
	})

	// Should become healthy in the presence of successful heartbeats.
	hbSuccess.Store(true)
	testutils.SucceedsSoon(t, func() error {
		return clientCtx.ConnHealth(remoteAddr)
	})

	if err := clientCtx.ConnHealth("non-existent connection"); err != errNotConnected {
		t.Errorf("unexpected error: %v", err)
	}
}

type interceptingListener struct {
	net.Listener
	connCB func(net.Conn)
}

func (ln *interceptingListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err == nil {
		ln.connCB(conn)
	}
	return conn, err
}

// TestHeartbeatHealth verifies that the health status changes after
// heartbeats succeed or fail due to transport failures.
func TestHeartbeatHealthTransport(t *testing.T) {
	defer leaktest.AfterTest(t)()

	if runtime.GOOS == "windows" {
		t.Skip("TODO(tamird): https://golang.org/cl/39490")
	}

	stopper := stop.NewStopper()
	defer stopper.Stop()

	// Can't be zero because that'd be an empty offset.
	clock := hlc.NewClock(time.Unix(0, 1).UnixNano, time.Nanosecond)

	serverCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	// newTestServer with a custom listener.
	tlsConfig, err := serverCtx.GetServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	ln, err := net.Listen("tcp", util.TestAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	mu := struct {
		syncutil.Mutex
		conns []net.Conn
	}{}
	ln = &interceptingListener{Listener: ln, connCB: func(conn net.Conn) {
		mu.Lock()
		mu.conns = append(mu.conns, conn)
		mu.Unlock()
	}}
	stopper.RunWorker(func() {
		<-stopper.ShouldQuiesce()
		netutil.FatalIfUnexpected(ln.Close())
		<-stopper.ShouldStop()
		s.Stop()
	})

	stopper.RunWorker(func() {
		netutil.FatalIfUnexpected(s.Serve(ln))
	})

	remoteAddr := ln.Addr().String()

	RegisterHeartbeatServer(s, &HeartbeatService{
		clock:              clock,
		remoteClockMonitor: serverCtx.RemoteClocks,
	})

	clientCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	// Make the interval shorter to speed up the test.
	clientCtx.heartbeatInterval = 1 * time.Millisecond
	if _, err := clientCtx.GRPCDial(remoteAddr); err != nil {
		t.Fatal(err)
	}
	// Everything is normal; should become healthy.
	testutils.SucceedsSoon(t, func() error {
		return clientCtx.ConnHealth(remoteAddr)
	})

	closeConns := func() error {
		mu.Lock()
		defer mu.Unlock()

		for i := len(mu.conns) - 1; i >= 0; i-- {
			if err := mu.conns[i].Close(); err != nil {
				return err
			}
			mu.conns = mu.conns[:i]
		}
		return nil
	}

	testutils.SucceedsSoon(t, func() error {
		// Close all the connections until we see a failure.
		if err := closeConns(); err != nil {
			t.Fatal(err)
		}

		if err := clientCtx.ConnHealth(remoteAddr); grpc.Code(err) != codes.Unavailable {
			return errors.Errorf("unexpected error: %v", err)
		}
		return nil
	})

	// Should become healthy again after GRPC reconnects.
	testutils.SucceedsSoon(t, func() error {
		return clientCtx.ConnHealth(remoteAddr)
	})

	// Close the listener and all the connections.
	//
	// NB: Closing the connections is done in the retry loop below because
	// sometimes the call to `ln.Close` interleaves with a connection attempt in
	// such a way that a connection manages to slip through.
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	// Should become unhealthy again now that the connection was closed.
	testutils.SucceedsSoon(t, func() error {
		if err := closeConns(); err != nil {
			t.Fatal(err)
		}

		if err := clientCtx.ConnHealth(remoteAddr); grpc.Code(err) != codes.Unavailable {
			return errors.Errorf("unexpected error: %v", err)
		}
		return nil
	})

	// Should stay unhealthy despite reconnection attempts.
	errUnhealthy := errors.New("connection is still unhealthy")
	if err := util.RetryForDuration(100*clientCtx.heartbeatInterval, func() error {
		if err := clientCtx.ConnHealth(remoteAddr); grpc.Code(err) != codes.Unavailable {
			return errors.Errorf("unexpected error: %v", err)
		}
		return errUnhealthy
	}); err != errUnhealthy {
		t.Fatal(err)
	}
}

func TestOffsetMeasurement(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()

	serverTime := time.Unix(0, 20)
	serverClock := hlc.NewClock(serverTime.UnixNano, time.Nanosecond)
	serverCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), serverClock, stopper)
	s, ln := newTestServer(t, serverCtx, true)
	remoteAddr := ln.Addr().String()

	RegisterHeartbeatServer(s, &HeartbeatService{
		clock:              serverClock,
		remoteClockMonitor: serverCtx.RemoteClocks,
	})

	// Create a client clock that is behind the server clock.
	clientAdvancing := AdvancingClock{time: time.Unix(0, 10)}
	clientClock := hlc.NewClock(clientAdvancing.UnixNano, time.Nanosecond)
	clientCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clientClock, stopper)
	// Make the interval shorter to speed up the test.
	clientCtx.heartbeatInterval = 1 * time.Millisecond
	clientCtx.RemoteClocks.offsetTTL = 5 * clientAdvancing.getAdvancementInterval()
	if _, err := clientCtx.GRPCDial(remoteAddr); err != nil {
		t.Fatal(err)
	}

	expectedOffset := RemoteOffset{Offset: 10, Uncertainty: 0, MeasuredAt: 10}
	testutils.SucceedsSoon(t, func() error {
		clientCtx.RemoteClocks.mu.Lock()
		defer clientCtx.RemoteClocks.mu.Unlock()

		if o, ok := clientCtx.RemoteClocks.mu.offsets[remoteAddr]; !ok {
			return errors.Errorf("expected offset of %s to be initialized, but it was not", remoteAddr)
		} else if o != expectedOffset {
			return errors.Errorf("expected:\n%v\nactual:\n%v", expectedOffset, o)
		}
		return nil
	})

	// Change the client such that it receives a heartbeat right after the
	// maximum clock reading delay.
	clientAdvancing.setAdvancementInterval(
		maximumPingDurationMult*clientClock.MaxOffset() + 1*time.Nanosecond)

	testutils.SucceedsSoon(t, func() error {
		clientCtx.RemoteClocks.mu.Lock()
		defer clientCtx.RemoteClocks.mu.Unlock()

		if o, ok := clientCtx.RemoteClocks.mu.offsets[remoteAddr]; ok {
			return errors.Errorf("expected offset to have been cleared, but found %s", o)
		}
		return nil
	})
}

func TestFailedOffsetMeasurement(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()

	// Can't be zero because that'd be an empty offset.
	clock := hlc.NewClock(time.Unix(0, 1).UnixNano, time.Nanosecond)

	serverCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	s, ln := newTestServer(t, serverCtx, true)
	remoteAddr := ln.Addr().String()

	heartbeat := &ManualHeartbeatService{
		clock:              clock,
		remoteClockMonitor: serverCtx.RemoteClocks,
		ready:              make(chan error),
		stopper:            stopper,
	}
	RegisterHeartbeatServer(s, heartbeat)

	// Create a client that never receives a heartbeat after the first.
	clientCtx := NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
	// Remove the timeout so that failure arises from exceeding the maximum
	// clock reading delay, not the timeout.
	clientCtx.heartbeatTimeout = 0
	if _, err := clientCtx.GRPCDial(remoteAddr); err != nil {
		t.Fatal(err)
	}
	heartbeat.ready <- nil // Allow one heartbeat for initialization.

	testutils.SucceedsSoon(t, func() error {
		clientCtx.RemoteClocks.mu.Lock()
		defer clientCtx.RemoteClocks.mu.Unlock()

		if _, ok := clientCtx.RemoteClocks.mu.offsets[remoteAddr]; !ok {
			return errors.Errorf("expected offset of %s to be initialized, but it was not", remoteAddr)
		}
		return nil
	})

	testutils.SucceedsSoon(t, func() error {
		serverCtx.RemoteClocks.mu.Lock()
		defer serverCtx.RemoteClocks.mu.Unlock()

		if o, ok := serverCtx.RemoteClocks.mu.offsets[remoteAddr]; ok {
			return errors.Errorf("expected offset of %s to not be initialized, but it was: %v", remoteAddr, o)
		}
		return nil
	})
}

type AdvancingClock struct {
	syncutil.Mutex
	time                time.Time
	advancementInterval atomic.Value // time.Duration
}

func (ac *AdvancingClock) setAdvancementInterval(d time.Duration) {
	ac.advancementInterval.Store(d)
}

func (ac *AdvancingClock) getAdvancementInterval() time.Duration {
	v := ac.advancementInterval.Load()
	if v == nil {
		return 0
	}
	return v.(time.Duration)
}

func (ac *AdvancingClock) UnixNano() int64 {
	ac.Lock()
	time := ac.time
	ac.time = time.Add(ac.getAdvancementInterval())
	ac.Unlock()
	return time.UnixNano()
}

func TestRemoteOffsetUnhealthy(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()

	const maxOffset = 100 * time.Millisecond

	type nodeContext struct {
		offset  time.Duration
		ctx     *Context
		errChan chan error
	}

	start := time.Date(2012, 12, 07, 0, 0, 0, 0, time.UTC)

	nodeCtxs := []nodeContext{
		{offset: 0},
		{offset: 0},
		{offset: 0},
		// The minimum offset that actually triggers node death.
		{offset: maxOffset + 1},
	}

	for i := range nodeCtxs {
		clock := hlc.NewClock(start.Add(nodeCtxs[i].offset).UnixNano, maxOffset)
		nodeCtxs[i].errChan = make(chan error, 1)
		nodeCtxs[i].ctx = NewContext(log.AmbientContext{}, testutils.NewNodeTestBaseContext(), clock, stopper)
		nodeCtxs[i].ctx.heartbeatInterval = maxOffset

		s, ln := newTestServer(t, nodeCtxs[i].ctx, true)
		RegisterHeartbeatServer(s, &HeartbeatService{
			clock:              clock,
			remoteClockMonitor: nodeCtxs[i].ctx.RemoteClocks,
		})
		nodeCtxs[i].ctx.Addr = ln.Addr().String()
	}

	// Fully connect the nodes.
	for i, clientNodeContext := range nodeCtxs {
		for j, serverNodeContext := range nodeCtxs {
			if i == j {
				continue
			}
			if _, err := clientNodeContext.ctx.GRPCDial(serverNodeContext.ctx.Addr); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Wait until all nodes are connected to all other nodes.
	for _, nodeCtx := range nodeCtxs {
		testutils.SucceedsSoon(t, func() error {
			nodeCtx.ctx.RemoteClocks.mu.Lock()
			defer nodeCtx.ctx.RemoteClocks.mu.Unlock()

			if a, e := len(nodeCtx.ctx.RemoteClocks.mu.offsets), len(nodeCtxs)-1; a != e {
				return errors.Errorf("not yet fully connected: have %d of %d connections: %v", a, e, nodeCtx.ctx.RemoteClocks.mu.offsets)
			}
			return nil
		})
	}

	for i, nodeCtx := range nodeCtxs {
		if nodeOffset := nodeCtx.offset; nodeOffset > maxOffset {
			if err := nodeCtx.ctx.RemoteClocks.VerifyClockOffset(nodeCtx.ctx.masterCtx); testutils.IsError(err, errOffsetGreaterThanMaxOffset) {
				t.Logf("max offset: %s - node %d with excessive clock offset of %s returned expected error: %s", maxOffset, i, nodeOffset, err)
			} else {
				t.Errorf("max offset: %s - node %d with excessive clock offset of %s returned unexpected error: %v", maxOffset, i, nodeOffset, err)
			}
		} else {
			if err := nodeCtx.ctx.RemoteClocks.VerifyClockOffset(nodeCtx.ctx.masterCtx); err != nil {
				t.Errorf("max offset: %s - node %d with acceptable clock offset of %s returned unexpected error: %s", maxOffset, i, nodeOffset, err)
			} else {
				t.Logf("max offset: %s - node %d with acceptable clock offset of %s did not return an error, as expected", maxOffset, i, nodeOffset)
			}
		}
	}
}
