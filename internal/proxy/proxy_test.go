// SPDX-License-Identifier: AGPL-3.0-or-later

package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestBuildParseStartupRoundtrip(t *testing.T) {
	in := map[string]string{"user": "vectoradb", "database": "qa", "application_name": "psql"}
	msg := buildStartup(in)

	if int(binary.BigEndian.Uint32(msg[:4])) != len(msg) {
		t.Fatalf("length prefix %d != actual %d", binary.BigEndian.Uint32(msg[:4]), len(msg))
	}
	if binary.BigEndian.Uint32(msg[4:8]) != codeStartup30 {
		t.Fatalf("wrong protocol version code")
	}
	got := parseParams(msg[8:])
	for k, v := range in {
		if got[k] != v {
			t.Errorf("param %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestReadStartupSkipsSSL(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	errc := make(chan error, 1)
	go func() {
		ssl := make([]byte, 8)
		binary.BigEndian.PutUint32(ssl[0:], 8)
		binary.BigEndian.PutUint32(ssl[4:], codeSSL)
		if _, err := client.Write(ssl); err != nil {
			errc <- err
			return
		}
		buf := make([]byte, 1)
		if _, err := client.Read(buf); err != nil {
			errc <- err
			return
		}
		if buf[0] != 'N' {
			errc <- fmt.Errorf("expected 'N' after SSLRequest, got %q", buf[0])
			return
		}
		_, err := client.Write(buildStartup(map[string]string{"user": "u", "database": "main"}))
		errc <- err
	}()

	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	params, err := readStartup(server)
	if err != nil {
		t.Fatalf("readStartup: %v", err)
	}
	if params["database"] != "main" {
		t.Fatalf("database = %q, want main", params["database"])
	}
	if err := <-errc; err != nil {
		t.Fatalf("client goroutine: %v", err)
	}
}

func TestSendError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		sendError(server, "3D000", "boom")
		server.Close()
	}()
	got, _ := io.ReadAll(client)
	if len(got) == 0 || got[0] != 'E' {
		t.Fatalf("expected an 'E' ErrorResponse, got % x", got)
	}
	if !bytes.Contains(got, []byte("boom")) {
		t.Errorf("ErrorResponse missing message text: %q", got)
	}
	if !bytes.Contains(got, []byte("3D000")) {
		t.Errorf("ErrorResponse missing SQLSTATE code: %q", got)
	}
}
