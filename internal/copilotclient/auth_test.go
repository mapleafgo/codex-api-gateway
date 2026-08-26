package copilotclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newAuthTestServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthClientStartDeviceFlow(t *testing.T) {
	srv := newAuthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/login/device/code" {
			t.Errorf("path = %s, want /login/device/code", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept = %q", accept)
		}
		body, _ := io.ReadAll(r.Body)
		for _, want := range []string{"client_id=6e3a0413e62d19d75ff1", "scope=read%3Auser"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("body missing %q: %s", want, body)
			}
		}
		_, _ = w.Write([]byte(`{
			"device_code": "dc-1",
			"user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"interval": 7
		}`))
	})

	client := NewAuthClient(srv.Client(), srv.URL+"/login/device/code", srv.URL+"/login/oauth/access_token")
	flow, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if flow.UserCode != "ABCD-1234" {
		t.Errorf("UserCode = %q", flow.UserCode)
	}
	if flow.VerificationURI != "https://github.com/login/device" {
		t.Errorf("VerificationURI = %q", flow.VerificationURI)
	}
	if flow.Interval != 7*time.Second {
		t.Errorf("Interval = %v, want 7s", flow.Interval)
	}
}

func TestAuthClientStartDeviceFlowNon200(t *testing.T) {
	srv := newAuthTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	client := NewAuthClient(srv.Client(), srv.URL, srv.URL)
	if _, err := client.StartDeviceFlow(context.Background()); err == nil {
		t.Fatal("expected error for non-200 device-code response")
	}
}

func TestPollDeviceFlow(t *testing.T) {
	interval := 2 * time.Second
	tests := []struct {
		name       string
		response   string
		wantToken  string
		wantNext   time.Duration
		wantErrSub string
	}{
		{
			name:     "authorization pending keeps interval",
			response: `{"error":"authorization_pending"}`,
			wantNext: interval,
		},
		{
			name:     "slow down increases interval",
			response: `{"error":"slow_down"}`,
			wantNext: interval + slowDownStep,
		},
		{
			name:      "success returns token",
			response:  `{"access_token":"ghu_test-token"}`,
			wantToken: "ghu_test-token",
		},
		{
			name:       "expired token terminates",
			response:   `{"error":"expired_token","error_description":"code expired"}`,
			wantErrSub: "expired",
		},
		{
			name:       "access denied terminates",
			response:   `{"error":"access_denied","error_description":"denied"}`,
			wantErrSub: "cancelled",
		},
		{
			name:       "unknown error terminates",
			response:   `{"error":"invalid_request"}`,
			wantErrSub: "invalid_request",
		},
		{
			name:       "missing error and token is unexpected",
			response:   `{}`,
			wantErrSub: "unexpected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newAuthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/login/oauth/access_token" {
					t.Errorf("path = %s, want /login/oauth/access_token", r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				for _, want := range []string{"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code", "device_code=dc-1"} {
					if !strings.Contains(string(body), want) {
						t.Errorf("body missing %q: %s", want, body)
					}
				}
				_, _ = w.Write([]byte(tt.response))
			})
			client := NewAuthClient(srv.Client(), srv.URL, srv.URL+"/login/oauth/access_token")
			flow := &DeviceFlow{deviceCode: "dc-1", Interval: interval}
			token, next, err := client.PollDeviceFlow(context.Background(), flow)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("PollDeviceFlow: %v", err)
			}
			if token != tt.wantToken {
				t.Errorf("token = %q, want %q", token, tt.wantToken)
			}
			if next != tt.wantNext {
				t.Errorf("next interval = %v, want %v", next, tt.wantNext)
			}
		})
	}
}

func TestPollDeviceFlowNon200(t *testing.T) {
	srv := newAuthTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := NewAuthClient(srv.Client(), srv.URL, srv.URL+"/login/oauth/access_token")
	flow := &DeviceFlow{deviceCode: "dc-1", Interval: time.Second}
	if _, _, err := client.PollDeviceFlow(context.Background(), flow); err == nil {
		t.Fatal("expected error for non-200 token response")
	}
}
