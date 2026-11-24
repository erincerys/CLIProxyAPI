package helps

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
)

func TestClaudeThreadBetaConflictErrorIsReplayableThreadNotFound(t *testing.T) {
	err := ClaudeThreadBetaConflictError{}
	if got := err.StatusCode(); got != http.StatusNotFound {
		t.Fatalf("StatusCode() = %d, want 404", got)
	}
	if !err.IsRequestScoped() {
		t.Fatal("IsRequestScoped() = false, want true")
	}
	if !clienterror.IsClaudeThreadNotFound(err.StatusCode(), err) {
		t.Fatalf("IsClaudeThreadNotFound = false for %q", err.Error())
	}
}
