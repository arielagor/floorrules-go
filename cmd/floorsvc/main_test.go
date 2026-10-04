package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/auth"
	"github.com/arielagor/floorrules-go/internal/config"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/store/memstore"
)

var testKey = []byte("test-only-hmac-key-0123456789abcdef")

// slowSSP takes `delay` to write each floor and signals when the first write
// has started, so a test can begin shutdown in the middle of an apply.
type slowSSP struct {
	*adapter.Mock
	delay   time.Duration
	entered chan struct{}
	once    sync.Once
}

func (s *slowSSP) SetFloor(ctx context.Context, pub string, f domain.PlatformFloor) error {
	s.once.Do(func() { close(s.entered) })
	time.Sleep(s.delay)
	return s.Mock.SetFloor(ctx, pub, f)
}

type client struct {
	t    *testing.T
	base string
	tok  string
}

func (c client) post(path, body string, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Error(err)
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Error(err)
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// H2 (day-2 review): a rolling deploy during an apply used to exit the
// process with the apply mid-flight once srv.Shutdown's wait ran out,
// leaving a partial, unaudited platform change. Shutdown must wait for
// in-flight applies to record their result, whatever SHUTDOWN_WAIT says.
func TestShutdownWaitsForInFlightApply(t *testing.T) {
	st := memstore.New()
	ssp := &slowSSP{Mock: adapter.NewMock(), delay: 500 * time.Millisecond, entered: make(chan struct{})}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		StoreBackend: "memory", HMACSecret: testKey, Issuer: "https://issuer.test", Audience: "floorrules",
		ShutdownDrain: 0, ShutdownWait: 10 * time.Millisecond, OutboxInterval: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.DiscardHandler), runOpts{store: st, ssp: ssp, listener: ln})
	}()

	tok, err := auth.SignHS256(testKey, auth.Claims{
		Subject: "user:ops", Issuer: cfg.Issuer, Audience: auth.Audience{cfg.Audience},
		ExpiresAt: time.Now().Add(time.Hour).Unix(), Scope: "rules:write plans:write plans:apply", Publishers: []string{"acme-tv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := client{t: t, base: "http://" + ln.Addr().String(), tok: tok}
	if code, body := c.post("/v1/publishers/acme-tv/rules", `{"segment":{"device":"ctv","geo":"US"},"floor_micros":2500000}`, nil); code != http.StatusCreated {
		t.Fatalf("create rule = %d %v", code, body)
	}
	code, body := c.post("/v1/publishers/acme-tv/plans", `{}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("create plan = %d %v", code, body)
	}
	planID, _ := body["id"].(string)

	const key = "shutdown-key-0001"
	applied := make(chan int, 1)
	go func() {
		code, _ := c.post("/v1/publishers/acme-tv/plans/"+planID+"/apply", `{}`, map[string]string{"Idempotency-Key": key})
		applied <- code
	}()
	select {
	case <-ssp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("apply never reached the platform")
	}

	cancel() // SIGTERM mid-apply
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}

	// The moment run returns, the process would exit. The apply must
	// already be recorded.
	a, err := st.GetAttempt(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AttemptSucceeded {
		t.Fatalf("attempt status when run returned = %s, want succeeded (apply was cut off by shutdown)", a.Status)
	}
	<-applied
}
