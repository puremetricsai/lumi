package llamacpp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAnswer(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "local context") {
			t.Fatalf("request omitted context: %s", body)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Local answer"}}]}`)
	}))
	defer srv.Close()

	answer, err := (Client{BaseURL: srv.URL}).Answer(context.Background(), "What?", "local context")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Local answer" {
		t.Fatalf("unexpected answer %q", answer)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotAuth != "" {
		t.Fatalf("local backend must not send Authorization, got %q", gotAuth)
	}
}
