package vhost

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestManagerRoutesSharedPortByHostname(t *testing.T) {
	port := freePort(t)
	m := NewManager(t.Context())
	t.Cleanup(m.Stop)

	m.SetBindings("registry", []Binding{
		{ID: "docker", Host: "127.0.0.1", Port: port, Hostname: "docker.test", Handler: textHandler("docker")},
	})
	m.SetBindings("serve", []Binding{
		{ID: "s3", Host: "127.0.0.1", Port: port, Hostname: "s3.test", Handler: textHandler("s3")},
	})

	want := map[string]string{"docker.test": "docker", "s3.test": "s3"}
	for host, body := range want {
		resp := request(t, port, host)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status: got %d, want 200", host, resp.StatusCode)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s body: %v", host, err)
		}
		if string(got) != body {
			t.Fatalf("%s body: got %q, want %q", host, got, body)
		}
	}

	resp := request(t, port, "unknown.test")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host status: got %d, want %d", resp.StatusCode, http.StatusMisdirectedRequest)
	}

	registryStatus := m.Status("registry")
	serveStatus := m.Status("serve")
	if len(registryStatus) != 1 || len(serveStatus) != 1 || !registryStatus[0].Running || !serveStatus[0].Running {
		t.Fatalf("statuses = registry %#v, serve %#v; want both running", registryStatus, serveStatus)
	}
	if !registryStatus[0].Shared || !serveStatus[0].Shared {
		t.Fatalf("statuses = registry %#v, serve %#v; want shared=true", registryStatus, serveStatus)
	}
}

func TestManagerCatchAllAndHotRouteSwap(t *testing.T) {
	port := freePort(t)
	m := NewManager(t.Context())
	t.Cleanup(m.Stop)

	m.SetBindings("test", []Binding{
		{ID: "default", Host: "127.0.0.1", Port: port, Handler: textHandler("first")},
	})
	resp := request(t, port, "anything.test")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "first" {
		t.Fatalf("first route body = %q", got)
	}

	// Same address and TLS mode: routing table should swap without a
	// listener rebind.
	m.SetBindings("test", []Binding{
		{ID: "default", Host: "127.0.0.1", Port: port, Handler: textHandler("second")},
	})
	resp = request(t, port, "anything.test")
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "second" {
		t.Fatalf("hot-swapped route body = %q", got)
	}
}

func TestManagerRejectsDuplicateHostname(t *testing.T) {
	port := freePort(t)
	m := NewManager(t.Context())
	t.Cleanup(m.Stop)

	m.SetBindings("test", []Binding{
		{ID: "one", Host: "127.0.0.1", Port: port, Hostname: "same.test", Handler: textHandler("one")},
		{ID: "two", Host: "127.0.0.1", Port: port, Hostname: "same.test", Handler: textHandler("two")},
	})

	status := m.Status("test")
	if len(status) != 2 {
		t.Fatalf("status len = %d, want 2", len(status))
	}
	if !status[0].Running || status[0].Error != "" {
		t.Fatalf("first status = %#v, want running", status[0])
	}
	if status[1].Running || status[1].Error == "" {
		t.Fatalf("second status = %#v, want duplicate-host error", status[1])
	}
}

func textHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func request(t *testing.T, port int, host string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	var lastErr error
	for range 20 {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := client.Do(req)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("request %s: %v", url, lastErr)
	return nil
}
