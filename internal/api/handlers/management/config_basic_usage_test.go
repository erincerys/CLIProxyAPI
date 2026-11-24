package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRoutingStrategyFillMostUsed(t *testing.T) {
	for _, input := range []string{"fill-most-used", "fmu", "FillMostUsed", " FMU "} {
		t.Run(input, func(t *testing.T) {
			if normalized, ok := normalizeRoutingStrategy(input); !ok || normalized != "fill-most-used" {
				t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", input, normalized, ok)
			}
			cfg := &config.Config{}
			path := writeTestConfigFile(t)
			handler := &Handler{cfg: cfg, configFilePath: path}
			router := gin.New()
			router.PUT("/routing-strategy", handler.PutRoutingStrategy)
			router.GET("/routing-strategy", handler.GetRoutingStrategy)
			body, errMarshal := json.Marshal(map[string]string{"value": input})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/routing-strategy", strings.NewReader(string(body))))
			if recorder.Code != http.StatusOK || cfg.Routing.Strategy != "fill-most-used" {
				t.Fatalf("PUT status=%d body=%s strategy=%s", recorder.Code, recorder.Body.String(), cfg.Routing.Strategy)
			}
			saved, errLoad := config.LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if saved.Routing.Strategy != "fill-most-used" {
				t.Fatalf("persisted strategy = %s", saved.Routing.Strategy)
			}
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/routing-strategy", nil))
			if recorder.Code != http.StatusOK || recorder.Body.String() != `{"strategy":"fill-most-used"}` {
				t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
