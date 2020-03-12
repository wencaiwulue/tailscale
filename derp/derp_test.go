// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package derp

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"tailscale.com/net/nettest"
	"tailscale.com/types/key"
)

func newPrivateKey(t *testing.T) (k key.Private) {
	t.Helper()
	if _, err := crand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSendRecv(t *testing.T) {
	serverPrivateKey := newPrivateKey(t)
	s := NewServer(serverPrivateKey, t.Logf)
	defer s.Close()

	const numClients = 3
	var clientPrivateKeys []key.Private
	var clientKeys []key.Public
	for i := 0; i < numClients; i++ {
		priv := newPrivateKey(t)
		clientPrivateKeys = append(clientPrivateKeys, priv)
		clientKeys = append(clientKeys, priv.Public())
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var clients []*Client
	var connsOut []Conn
	var recvChs []chan []byte
	errCh := make(chan error, 3)

	for i := 0; i < numClients; i++ {
		t.Logf("Connecting client %d ...", i)
		cout, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cout.Close()
		connsOut = append(connsOut, cout)

		cin, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer cin.Close()
		brwServer := bufio.NewReadWriter(bufio.NewReader(cin), bufio.NewWriter(cin))
		go s.Accept(cin, brwServer, fmt.Sprintf("test-client-%d", i))

		key := clientPrivateKeys[i]
		brw := bufio.NewReadWriter(bufio.NewReader(cout), bufio.NewWriter(cout))
		c, err := NewClient(key, cout, brw, t.Logf)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		clients = append(clients, c)
		recvChs = append(recvChs, make(chan []byte))
		t.Logf("Connected client %d.", i)
	}

	t.Logf("Starting read loops")
	for i := 0; i < numClients; i++ {
		go func(i int) {
			for {
				b := make([]byte, 1<<16)
				m, err := clients[i].Recv(b)
				if err != nil {
					errCh <- err
					return
				}
				switch m := m.(type) {
				default:
					t.Errorf("unexpected message type %T", m)
					continue
				case ReceivedPacket:
					if m.Source.IsZero() {
						t.Errorf("zero Source address in ReceivedPacket")
					}
					recvChs[i] <- m.Data
				}
			}
		}(i)
	}

	recv := func(i int, want string) {
		t.Helper()
		select {
		case b := <-recvChs[i]:
			if got := string(b); got != want {
				t.Errorf("client1.Recv=%q, want %q", got, want)
			}
		case <-time.After(1 * time.Second):
			t.Errorf("client%d.Recv, got nothing, want %q", i, want)
		}
	}
	recvNothing := func(i int) {
		t.Helper()
		select {
		case b := <-recvChs[0]:
			t.Errorf("client%d.Recv=%q, want nothing", i, string(b))
		default:
		}
	}

	wantActive := func(total, home int64) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		var gotTotal, gotHome int64
		for time.Now().Before(dl) {
			gotTotal, gotHome = s.curClients.Value(), s.curHomeClients.Value()
			if gotTotal == total && gotHome == home {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("total/home=%v/%v; want %v/%v", gotTotal, gotHome, total, home)
	}

	msg1 := []byte("hello 0->1\n")
	if err := clients[0].Send(clientKeys[1], msg1); err != nil {
		t.Fatal(err)
	}
	recv(1, string(msg1))
	recvNothing(0)
	recvNothing(2)

	msg2 := []byte("hello 1->2\n")
	if err := clients[1].Send(clientKeys[2], msg2); err != nil {
		t.Fatal(err)
	}
	recv(2, string(msg2))
	recvNothing(0)
	recvNothing(1)

	wantActive(3, 0)
	clients[0].NotePreferred(true)
	wantActive(3, 1)
	clients[0].NotePreferred(true)
	wantActive(3, 1)
	clients[0].NotePreferred(false)
	wantActive(3, 0)
	clients[0].NotePreferred(false)
	wantActive(3, 0)
	clients[1].NotePreferred(true)
	wantActive(3, 1)
	connsOut[1].Close()
	wantActive(2, 0)
	clients[2].NotePreferred(true)
	wantActive(2, 1)
	clients[2].NotePreferred(false)
	wantActive(2, 0)
	connsOut[2].Close()
	wantActive(1, 0)

	t.Logf("passed")
	s.Close()
}

func TestSendFreeze(t *testing.T) {
	serverPrivateKey := newPrivateKey(t)
	s := NewServer(serverPrivateKey, t.Logf)
	defer s.Close()
	s.WriteTimeout = 100 * time.Millisecond

	// We send two streams of messages:
	//
	//	alice --> bob
	//	alice --> cathy
	//
	// Then cathy stops processing messsages.
	// That should not interfere with alice talking to bob.

	newClient := func(name string, k key.Private) (c *Client, clientConn nettest.Conn) {
		t.Helper()
		c1, c2 := nettest.NewConn(name, 1024)
		go s.Accept(c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), name)

		brw := bufio.NewReadWriter(bufio.NewReader(c2), bufio.NewWriter(c2))
		c, err := NewClient(k, c2, brw, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		return c, c2
	}

	aliceKey := newPrivateKey(t)
	aliceClient, aliceConn := newClient("alice", aliceKey)

	bobKey := newPrivateKey(t)
	bobClient, bobConn := newClient("bob", bobKey)

	cathyKey := newPrivateKey(t)
	cathyClient, cathyConn := newClient("cathy", cathyKey)

	var aliceCount, bobCount, cathyCount expvar.Int

	errCh := make(chan error, 4)
	recvAndCount := func(count *expvar.Int, name string, client *Client) {
		for {
			b := make([]byte, 1<<9)
			m, err := client.Recv(b)
			if err != nil {
				errCh <- fmt.Errorf("%s: %w", name, err)
				return
			}
			switch m := m.(type) {
			default:
				errCh <- fmt.Errorf("%s: unexpected message type %T", name, m)
				return
			case ReceivedPacket:
				if m.Source.IsZero() {
					errCh <- fmt.Errorf("%s: zero Source address in ReceivedPacket", name)
					return
				}
				count.Add(1)
			}
		}
	}
	go recvAndCount(&aliceCount, "alice", aliceClient)
	go recvAndCount(&bobCount, "bob", bobClient)
	go recvAndCount(&cathyCount, "cathy", cathyClient)

	var cancel func()
	go func() {
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		for {
			select {
			case <-t.C:
			case <-ctx.Done():
				errCh <- nil
				return
			}

			msg1 := []byte("hello alice->bob\n")
			if err := aliceClient.Send(bobKey.Public(), msg1); err != nil {
				errCh <- fmt.Errorf("alice send to bob: %w", err)
				return
			}
			msg2 := []byte("hello alice->cathy\n")

			// TODO: an error is expected here.
			// We ignore it, maybe we should log it somehow?
			aliceClient.Send(cathyKey.Public(), msg2)
		}
	}()

	var countSnapshot [3]int64
	loadCounts := func() (adiff, bdiff, cdiff int64) {
		t.Helper()

		atotal := aliceCount.Value()
		btotal := bobCount.Value()
		ctotal := cathyCount.Value()

		adiff = atotal - countSnapshot[0]
		bdiff = btotal - countSnapshot[1]
		cdiff = ctotal - countSnapshot[2]

		countSnapshot[0] = atotal
		countSnapshot[1] = btotal
		countSnapshot[2] = ctotal

		t.Logf("count diffs: alice=%d, bob=%d, cathy=%d", adiff, bdiff, cdiff)
		return adiff, bdiff, cdiff
	}

	t.Run("initial send", func(t *testing.T) {
		time.Sleep(10 * time.Millisecond)
		a, b, c := loadCounts()
		if a != 0 {
			t.Errorf("alice diff=%d, want 0", a)
		}
		if b == 0 {
			t.Errorf("no bob diff, want positive value")
		}
		if c == 0 {
			t.Errorf("no cathy diff, want positive value")
		}
	})

	t.Run("block cathy", func(t *testing.T) {
		// Block cathy. Now the cathyConn buffer will fill up quickly,
		// and the derp server will back up.
		cathyConn.SetReadBlock(true)
		time.Sleep(2 * s.WriteTimeout)

		a, b, _ := loadCounts()
		if a != 0 {
			t.Errorf("alice diff=%d, want 0", a)
		}
		if b == 0 {
			t.Errorf("no bob diff, want positive value")
		}

		// Now wait a little longer, and ensure packets still flow to bob
		time.Sleep(10 * time.Millisecond)
		if _, b, _ := loadCounts(); b == 0 {
			t.Errorf("connection alice->bob frozen by alice->cathy")
		}
	})

	// Cleanup, make sure we process all errors.
	t.Logf("TEST COMPLETE, cancelling sender")
	cancel()
	t.Logf("closing connections")
	aliceConn.Close()
	bobConn.Close()
	cathyConn.Close()

	for i := 0; i < cap(errCh); i++ {
		err := <-errCh
		if err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}
			t.Error(err)
		}
	}
}
