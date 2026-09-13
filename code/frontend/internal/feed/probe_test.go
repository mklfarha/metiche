package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeTeam(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/teams/public-team":
			_, _ = w.Write([]byte(`{"sequence":1}`))
		case "/v1/teams/broken":
			http.Error(w, "database password is hunter2", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	if res, err := ProbeTeam(ctx, srv.Client(), srv.URL, "", "public-team"); err != nil || res != ProbeFound {
		t.Fatalf("public: %v %v", res, err)
	}
	if res, err := ProbeTeam(ctx, srv.Client(), srv.URL+"/v1/", "", "private-team"); err != nil || res != ProbeNotFound {
		t.Fatalf("private: %v %v", res, err)
	}
	res, err := ProbeTeam(ctx, srv.Client(), srv.URL, "", "broken")
	if err == nil || res != 0 {
		t.Fatalf("500 must be an error, got %v %v", res, err)
	}
	if contains(err.Error(), "hunter2") {
		t.Fatalf("probe error echoes the response body: %v", err)
	}
	if _, err := ProbeTeam(ctx, srv.Client(), srv.URL, "board-secret", "public-team"); err != nil {
		t.Fatal(err)
	}
	if auth[0] != "" || auth[len(auth)-1] != "Bearer board-secret" {
		t.Fatalf("authorization headers = %q", auth)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
