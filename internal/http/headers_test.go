package httpserver

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requireHeaderValue(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	values := header.Values(name)
	if len(values) != 1 {
		t.Fatalf("%s: want exactly one value, got %d", name, len(values))
	}
	got := values[0]
	if strings.Contains(got, `\"`) {
		t.Fatalf("%s contains a literal backslash-quote: %q", name, got)
	}
	if got != want {
		t.Fatalf("%s: got %q, want %q", name, got, want)
	}
}

func TestBasicAuthChallengeHeader(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct{ name, authorization string }{
		{"missing credentials", ""},
		{"wrong password", "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:wrong"))},
		{"malformed credentials", "Basic invalid"},
		{"device token", "Bearer " + f.token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/sources", nil)
			if tc.authorization != "" {
				r.Header.Set("Authorization", tc.authorization)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			requireStatus(t, w, http.StatusUnauthorized)
			requireHeaderValue(t, w.Header(), "WWW-Authenticate", `Basic realm="OpenDTU-HEMS telemetry", charset="UTF-8"`)
		})
	}
	w := f.request(http.MethodGet, "/sources", "", "admin", "")
	requireStatus(t, w, http.StatusOK)
	if len(w.Header().Values("WWW-Authenticate")) != 0 {
		t.Fatal("authenticated response unexpectedly contains an authentication challenge")
	}
}
