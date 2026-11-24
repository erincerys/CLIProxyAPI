package claude

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

func TestClaudeErrorMessageCarriesThreadNotFoundMarker(t *testing.T) {
	const upstream = "No thread state was found for the requested previous_message_id. Replay the full conversation with thread create to start a new Thread."
	for name, err := range map[string]error{
		"json":     errors.New(`{"type":"error","error":{"type":"not_found_error","message":"` + upstream + `"}}`),
		"plain":    errors.New(upstream),
		"wrapped":  responseBodyOnlyClaudeError{body: []byte(`{"type":"error","error":{"type":"not_found_error","message":"` + upstream + `"}}`)},
		"executor": mockClaudeThreadNotFoundError{msg: upstream, body: []byte(`{"type":"error","error":{"type":"not_found_error","message":"` + upstream + `"}}`)},
		"marked":   errors.New("thread_not_found: " + upstream),
	} {
		t.Run(name, func(t *testing.T) {
			body, errMarshal := json.Marshal((&ClaudeCodeAPIHandler{}).toClaudeError(&interfaces.ErrorMessage{StatusCode: http.StatusNotFound, Error: err}))
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			message := ojson.GetBytes(body, "error.message").String()
			if !strings.HasPrefix(message, "thread_not_found: ") || strings.Count(message, "thread_not_found") != 1 {
				t.Fatalf("error.message = %q, want one thread_not_found: prefix", message)
			}
			if got := ojson.GetBytes(body, "error.details.error_code").String(); got != "thread_not_found" {
				t.Fatalf("error.details.error_code = %q", got)
			}
		})
	}
}

func TestClaudeErrorMessageLeavesOtherNotFoundErrorsAlone(t *testing.T) {
	body, errMarshal := json.Marshal((&ClaudeCodeAPIHandler{}).toClaudeError(&interfaces.ErrorMessage{
		StatusCode: http.StatusNotFound,
		Error:      errors.New(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-x"}}`),
	}))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if message := ojson.GetBytes(body, "error.message").String(); strings.Contains(message, "thread_not_found") {
		t.Fatalf("error.message = %q, want no marker", message)
	}
}

func TestWriteClaudeDirectErrorMessageCarriesThreadNotFoundMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&ClaudeCodeAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode:     http.StatusNotFound,
		DirectResponse: true,
		Body:           []byte(`{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id. Replay the full conversation with thread create to start a new Thread."}}`),
	})
	if message := ojson.GetBytes(recorder.Body.Bytes(), "error.message").String(); !strings.HasPrefix(message, "thread_not_found: ") {
		t.Fatalf("error.message = %q, want thread_not_found: prefix", message)
	}
}
