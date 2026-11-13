package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func resetCatalogForTest(t *testing.T) {
	t.Helper()
	catalogMu.Lock()
	old := catalogSaved
	catalogSaved = nil
	catalogMu.Unlock()
	raw := kvGet(modelCatalogKey)
	rtMu.Lock()
	oldRT := rtVal
	rtVal.DefaultModel = "gemini-3.6-flash"
	rtMu.Unlock()
	t.Cleanup(func() {
		catalogMu.Lock()
		catalogSaved = old
		catalogMu.Unlock()
		_ = kvSet(modelCatalogKey, raw)
		rtMu.Lock()
		rtVal = oldRT
		rtMu.Unlock()
	})
}

func TestCatalogHotReloadPersistenceAndConflict(t *testing.T) {
	resetCatalogForTest(t)
	withPoolCookie(t)
	rows := catalogSnapshot()
	revision := catalogRevision(rows)
	rows = append(rows, catalogModel{ID: "gemini-custom-hydrogen", HexID: hexHydrogen4, Mode: 1, Upstream: "4 Hydrogen", Enabled: true, RequiresLogin: true})
	if status, err := saveModelCatalog(rows, revision); err != nil {
		t.Fatalf("save: %d %v", status, err)
	}
	_, mc, err := resolveModel("gemini-custom-hydrogen")
	if err != nil || mc.HexID != hexHydrogen4 || modelDowngraded(mc, "4 Hydrogen") {
		t.Fatalf("routing not hot-reloaded: %+v %v", mc, err)
	}
	if !modelDowngraded(mc, "3.5 Flash-Lite") {
		t.Fatal("strict validation was bypassed")
	}
	if status, _ := saveModelCatalog(rows, revision); status != 409 {
		t.Fatal("stale update must conflict")
	}
	catalogMu.Lock()
	catalogSaved = nil
	catalogMu.Unlock()
	initModelCatalog()
	if _, _, err := resolveModel("gemini-custom-hydrogen"); err != nil {
		t.Fatalf("restart lost model: %v", err)
	}
	rows = catalogSnapshot()
	revision = catalogRevision(rows)
	for i := range rows {
		if rows[i].ID == "gemini-custom-hydrogen" {
			rows[i].Enabled = false
		}
	}
	if _, err := saveModelCatalog(rows, revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveModel("gemini-custom-hydrogen"); err == nil {
		t.Fatal("disabled model still routes")
	}
	w := httptest.NewRecorder()
	handleModels(w, httptest.NewRequest("GET", "/v1/models", nil))
	if strings.Contains(w.Body.String(), "gemini-custom-hydrogen") {
		t.Fatal("disabled model exposed to OpenAI clients")
	}
	w = httptest.NewRecorder()
	handleGeminiModels(w, httptest.NewRequest("GET", "/v1beta/models", nil))
	if strings.Contains(w.Body.String(), "gemini-custom-hydrogen") {
		t.Fatal("disabled model exposed to Gemini clients")
	}
}

func TestCatalogValidationPreservesLastGoodConfiguration(t *testing.T) {
	resetCatalogForTest(t)
	for _, kind := range []string{"duplicate", "default", "hex", "upstream", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			rows := catalogSnapshot()
			revision := catalogRevision(rows)
			switch kind {
			case "duplicate":
				rows = append(rows, rows[0])
			case "default":
				for i := range rows {
					if rows[i].ID == "gemini-3.6-flash" {
						rows[i].Enabled = false
					}
				}
			case "hex":
				rows[0].HexID = "bad"
			case "upstream":
				rows[0].Upstream = ""
				rows[0].Tool = 0
			case "thinking":
				rows[0].Thinking = true
				rows[0].ThinkWire = 4
			}
			if status, err := saveModelCatalog(rows, revision); status != 400 || err == nil {
				t.Fatalf("accepted invalid %s", kind)
			}
			if catalogRevision(catalogSnapshot()) != revision {
				t.Fatal("failed save changed runtime")
			}
		})
	}
}

func TestCatalogLoginPolicy(t *testing.T) {
	m := catalogModel{HexID: hexHydrogen4, Mode: 1}
	if modelNeedsLogin(m.config()) {
		t.Fatal("explicit anonymous policy ignored")
	}
	m.RequiresLogin = true
	if !modelNeedsLogin(m.config()) {
		t.Fatal("explicit login policy ignored")
	}
	m.RequiresLogin = false
	m.Thinking = true
	if !modelNeedsLogin(m.config()) {
		t.Fatal("thinking must still require login")
	}
}

func TestModelCatalogAdminAuthenticationAndBody(t *testing.T) {
	old := cfg.AdminToken
	cfg.AdminToken = "catalog-test-admin"
	t.Cleanup(func() { cfg.AdminToken = old })
	for _, handler := range []http.HandlerFunc{handleAdminModels, handleAdminDiscoverModels} {
		w := httptest.NewRecorder()
		requireAuth(handler)(w, httptest.NewRequest("POST", "/admin/api/models", strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("admin route lacks auth: %d", w.Code)
		}
	}
	for _, body := range []string{`{"models":[]} {}`, `{"unexpected":true}`, strings.Repeat("x", 600000)} {
		w := httptest.NewRecorder()
		handleAdminModels(w, httptest.NewRequest("PUT", "/admin/api/models", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid body accepted: %d", w.Code)
		}
	}
}

func TestDiscoverModelsUsesPrimaryRecords(t *testing.T) {
	body, err := os.ReadFile("testdata/model_catalog_rpc.txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseDiscoveredModels(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, compatibility IDs must not become models", len(rows))
	}
	for _, r := range rows {
		if r.ID == "gemini-3.8-flash" && (r.HexID != "56fdd199312815e2" || r.Mode != 1 || r.Upstream != "3.8 Flash") {
			t.Fatalf("wrong routing: %+v", r)
		}
		if !r.RequiresLogin {
			t.Fatal("discovered account models must require login")
		}
	}
	// Unknown names are derived from the current upstream response, not a fixed list.
	updated := strings.ReplaceAll(string(body), "3.8 Flash", "4 Hydrogen")
	rows, err = parseDiscoveredModels(updated)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == "gemini-4-hydrogen" {
			found = true
		}
	}
	if !found {
		t.Fatal("new upstream name missing")
	}
	for _, bad := range []string{"", `[["wrb.fr","other","[]"]]`, strings.ReplaceAll(string(body), "otAQ7b", "other")} {
		if _, err := parseDiscoveredModels(bad); err == nil {
			t.Fatal("invalid RPC accepted")
		}
	}
	// A partially malformed catalog is rejected rather than silently dropping models.
	badRow := make([]any, 18)
	badRow[0] = "bad"
	badRow[11] = "4 Hydrogen"
	badRow[17] = 1
	p := make([]any, 16)
	p[15] = []any{badRow}
	payload, _ := json.Marshal(p)
	f, _ := json.Marshal([]any{[]any{"wrb.fr", "otAQ7b", string(payload)}})
	if _, err := parseDiscoveredModels(string(f)); err == nil {
		t.Fatal("malformed model accepted")
	}
}

func TestDiscoverCurrentModelSchemaAndAutoNeedsReview(t *testing.T) {
	body, err := os.ReadFile("testdata/model_catalog_16_rpc.txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseDiscoveredModels(string(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gemini-4-hydrogen": "4 Hydrogen", "gemini-4-carbon": "4 Carbon", "gemini-3.5-flash-lite": "3.5 Flash-Lite"}
	if len(rows) != len(want) {
		t.Fatalf("unexpected models: %+v", rows)
	}
	for _, m := range rows {
		if want[m.ID] != m.Upstream {
			t.Fatalf("subtitle mistaken for model identity: %+v", m)
		}
	}
	body, err = os.ReadFile("testdata/model_catalog_8_rpc.txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = parseDiscoveredModels(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "gemini-auto" || rows[0].Upstream != "" {
		t.Fatalf("Auto must require manual identity: %+v", rows)
	}
	if err := validateCatalog(rows); err == nil {
		t.Fatal("Auto imported without strict identity review")
	}
}
