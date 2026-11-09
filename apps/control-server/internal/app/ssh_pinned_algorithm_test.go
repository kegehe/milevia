package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHPinnedKeyNegotiatesSavedTypeAndStillRejectsReplacement(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	rsaPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaPrivate)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ed25519", "rsa-sha2", "replaced"} {
		t.Run(kind, func(t *testing.T) {
			server := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
			server.AddHostKey(rsaSigner)
			server.AddHostKey(ed)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				incoming, channels, requests, err := ssh.NewServerConn(conn, server)
				if err != nil {
					done <- err
					return
				}
				defer incoming.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					channel.Reject(ssh.Prohibited, "test")
				}
				done <- nil
			}()
			pin := ed.PublicKey()
			if kind == "rsa-sha2" {
				pin = rsaSigner.PublicKey()
			}
			if kind == "replaced" {
				_, replacement, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.NewSignerFromKey(replacement)
				if err != nil {
					t.Fatal(err)
				}
				pin = signer.PublicKey()
			}
			host, portRaw, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, _ := strconv.Atoi(portRaw)
			client, err := newSSHClient(SSHConnection{Host: host, Port: port, User: "test", Password: "test-only", KnownHosts: string(ssh.MarshalAuthorizedKey(pin))}, false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = client.connect(ctx)
			client.close()
			if kind == "replaced" && err == nil {
				t.Fatal("replaced host key accepted")
			}
			if kind != "replaced" && err != nil {
				t.Fatal("valid saved host key could not negotiate", err)
			}
			select {
			case serverErr := <-done:
				if kind != "replaced" && serverErr != nil {
					t.Fatal(serverErr)
				}
			case <-ctx.Done():
				t.Fatal("SSH server did not settle")
			}
		})
	}
}
