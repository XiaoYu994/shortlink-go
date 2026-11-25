package mailer

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSMTPServer struct {
	ln          net.Listener
	rcptCode    string
	tlsCfg      *tls.Config
	stall       <-chan struct{}
	closeAfter2 bool

	conns   atomic.Int32
	gotFrom string
	gotTo   string
	gotData string
}

func startFakeSMTPServer(t *testing.T, rcptCode string, startTLS bool, stall <-chan struct{}) (*fakeSMTPServer, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake smtp: %v", err)
	}
	srv := &fakeSMTPServer{
		ln:       ln,
		rcptCode: rcptCode,
		stall:    stall,
	}
	if startTLS {
		// 使用 httptest TLS 证书配置快速获得合法本地证书。
		ts := httptest.NewTLSServer(nil)
		srv.tlsCfg = ts.TLS.Clone()
		ts.Close()
	}

	go srv.serve()
	t.Cleanup(func() { _ = ln.Close() })

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return srv, host, port
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)
		go s.handleConn(conn)
	}
}

func (s *fakeSMTPServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	writeLine := func(line string) {
		_, _ = conn.Write([]byte(line + "\r\n"))
	}
	writeLine("220 localhost ESMTP test")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO "):
			if s.tlsCfg != nil {
				writeLine("250-localhost")
				writeLine("250 STARTTLS")
			} else {
				writeLine("250 localhost")
			}
		case upper == "STARTTLS" && s.tlsCfg != nil:
			writeLine("220 Ready to start TLS")
			tlsConn := tls.Server(conn, s.tlsCfg)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(conn)
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.gotFrom = strings.TrimPrefix(cmd, "MAIL FROM:")
			writeLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.gotTo = strings.TrimPrefix(cmd, "RCPT TO:")
			if s.rcptCode != "" && s.rcptCode != "250" {
				writeLine(s.rcptCode + " simulated error")
			} else {
				writeLine("250 OK")
			}
		case upper == "DATA":
			if s.stall != nil {
				<-s.stall
				return
			}
			writeLine("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body.WriteString(l)
			}
			s.gotData = body.String()
			writeLine("250 Queued")
			if s.closeAfter2 {
				return
			}
		case upper == "QUIT":
			writeLine("221 Bye")
			return
		default:
			writeLine("250 OK")
		}
	}
}

func selfSignedPoolForFakeTLS(t *testing.T, cfg *tls.Config) *tls.Config {
	t.Helper()
	// 为 fake STARTTLS 使用证书指纹免除系统 CA；生产默认走严格校验。
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	return &tls.Config{ServerName: "127.0.0.1", RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func TestSMTPSenderSendModesAndErrors(t *testing.T) {
	t.Run("plain none mode sends utf8 invitation message", func(t *testing.T) {
		srv, host, port := startFakeSMTPServer(t, "250", false, nil)
		sender, err := NewSMTPSender(SMTPConfig{
			Host:    host,
			Port:    port,
			From:    "Shortlink <noreply@example.com>",
			TLSMode: TLSModeNone,
			Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		msg := BuildInvitationMessage(InvitationEmailTarget{
			InvitationID: 12,
			TenantID:     7,
			TenantName:   "Acme",
			Email:        "alice@example.com",
			Role:         "admin",
			InviterName:  "Bob",
		}, "https://console.example.com")
		if err := sender.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if srv.gotTo != "<alice@example.com>" {
			t.Errorf("RCPT TO = %q, want <alice@example.com>", srv.gotTo)
		}
		if !strings.Contains(srv.gotData, "https://console.example.com/login") || !strings.Contains(srv.gotData, "Acme") {
			t.Errorf("unexpected DATA payload:\n%s", srv.gotData)
		}
	})

	t.Run("starttls upgrades connection cleanly", func(t *testing.T) {
		srv, host, port := startFakeSMTPServer(t, "250", true, nil)
		clientTLS := selfSignedPoolForFakeTLS(t, srv.tlsCfg)
		sender, err := NewSMTPSender(SMTPConfig{
			Host:    host,
			Port:    port,
			From:    "noreply@example.com",
			TLSMode: TLSModeStartTLS,
			Timeout: 2 * time.Second,
		}, clientTLS)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		if err := sender.Send(context.Background(), Message{
			To:      "bob@example.com",
			Subject: "Hello",
			Body:    "Welcome",
		}); err != nil {
			t.Fatalf("Send STARTTLS: %v", err)
		}
	})

	t.Run("5xx rejected is permanent while 4xx is retryable", func(t *testing.T) {
		_, host5, port5 := startFakeSMTPServer(t, "550", false, nil)
		s5, _ := NewSMTPSender(SMTPConfig{Host: host5, Port: port5, From: "a@example.com", TLSMode: TLSModeNone})
		err5 := s5.Send(context.Background(), Message{To: "b@example.com", Subject: "Hi", Body: "Body"})
		if err5 == nil || !IsPermanent(err5) {
			t.Fatalf("5xx error = %v, want IsPermanent=true", err5)
		}

		_, host4, port4 := startFakeSMTPServer(t, "451", false, nil)
		s4, _ := NewSMTPSender(SMTPConfig{Host: host4, Port: port4, From: "a@example.com", TLSMode: TLSModeNone})
		err4 := s4.Send(context.Background(), Message{To: "b@example.com", Subject: "Hi", Body: "Body"})
		if err4 == nil || IsPermanent(err4) {
			t.Fatalf("4xx error = %v, want retryable (IsPermanent=false)", err4)
		}
	})

	t.Run("rejects crlf header injection without connecting", func(t *testing.T) {
		srv, host, port := startFakeSMTPServer(t, "250", false, nil)
		s, _ := NewSMTPSender(SMTPConfig{Host: host, Port: port, From: "a@example.com", TLSMode: TLSModeNone})
		err := s.Send(context.Background(), Message{
			To:      "b@example.com",
			Subject: "Ok\r\nBcc: evil@example.com",
			Body:    "Body",
		})
		if !errors.Is(err, ErrHeaderInjection) || !IsPermanent(err) {
			t.Fatalf("header injection err = %v, want ErrHeaderInjection + IsPermanent", err)
		}
		if srv.conns.Load() != 0 {
			t.Fatalf("conns = %d, want 0 when header validation fails", srv.conns.Load())
		}
	})

	t.Run("pre-canceled ctx returns early without connecting", func(t *testing.T) {
		srv, host, port := startFakeSMTPServer(t, "250", false, nil)
		s, _ := NewSMTPSender(SMTPConfig{Host: host, Port: port, From: "a@example.com", TLSMode: TLSModeNone})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.Send(ctx, Message{To: "b@example.com", Subject: "Hi", Body: "Body"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if srv.conns.Load() != 0 {
			t.Fatalf("conns = %d, want 0 when ctx is already canceled", srv.conns.Load())
		}
	})

	t.Run("ignores QUIT failure after MTA accepts DATA", func(t *testing.T) {
		srv, host, port := startFakeSMTPServer(t, "250", false, nil)
		srv.closeAfter2 = true
		s, _ := NewSMTPSender(SMTPConfig{Host: host, Port: port, From: "a@example.com", TLSMode: TLSModeNone})
		if err := s.Send(context.Background(), Message{To: "b@example.com", Subject: "Hi", Body: "Body"}); err != nil {
			t.Fatalf("Send failed when server closed before QUIT: %v", err)
		}
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		stall := make(chan struct{})
		defer close(stall)
		_, host, port := startFakeSMTPServer(t, "250", false, stall)
		s, _ := NewSMTPSender(SMTPConfig{Host: host, Port: port, From: "a@example.com", TLSMode: TLSModeNone, Timeout: 5 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		err := s.Send(ctx, Message{To: "b@example.com", Subject: "Wait", Body: "Body"})
		if err == nil {
			t.Fatal("expected deadline error on stalled SMTP server")
		}
	})

	t.Run("log value masks password", func(t *testing.T) {
		cfg := SMTPConfig{Host: "smtp.example.com", Username: "u", Password: "super-secret-password"}
		if strings.Contains(cfg.String(), "super-secret-password") {
			t.Fatalf("SMTPConfig.String() leaked password: %s", cfg.String())
		}
	})
}

func TestInvitationEmailTargetPending(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)
	base := InvitationEmailTarget{InvitationID: 1, Email: "a@example.com", ExpiresAt: &future}
	if !base.Pending(now) {
		t.Fatal("active invitation should be pending")
	}
	exp := base
	exp.ExpiresAt = &past
	if exp.Pending(now) {
		t.Fatal("expired invitation should not be pending")
	}
	rev := base
	rev.RevokedAt = &now
	if rev.Pending(now) {
		t.Fatal("revoked invitation should not be pending")
	}
}
