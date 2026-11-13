package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const modelCatalogKey = "model_catalog_v1"

// Only routing fields are persisted; request-specific attachments and conversation
// state must never enter the shared model registry.
type catalogModel struct {
	ID            string `json:"id"`
	HexID         string `json:"hex_id"`
	Mode          int    `json:"mode"`
	Upstream      string `json:"upstream"`
	Description   string `json:"description"`
	Thinking      bool   `json:"thinking"`
	ThinkWire     int    `json:"think_wire"`
	Tool          int    `json:"tool"`
	RequiresLogin bool   `json:"requires_login"`
	Enabled       bool   `json:"enabled"`
}

var (
	catalogMu    sync.RWMutex
	catalogSaved []catalogModel
	// Serialize default-model changes with registry changes.
	modelSettingsMu sync.Mutex
	modelIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)
	modelHexPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)
)

func (m catalogModel) config() ModelConfig {
	return ModelConfig{HexID: m.HexID, Mode: m.Mode, Upstream: m.Upstream, Desc: m.Description,
		Thinking: m.Thinking, ThinkWire: m.ThinkWire, Tool: m.Tool, RequiresLogin: m.RequiresLogin, loginPolicySet: true}
}

func catalogSnapshot() []catalogModel {
	catalogMu.RLock()
	rows := append([]catalogModel(nil), catalogSaved...)
	catalogMu.RUnlock()
	if rows == nil {
		for id, m := range Models {
			rows = append(rows, catalogModel{ID: id, HexID: m.HexID, Mode: m.Mode, Upstream: m.Upstream,
				Description: m.Desc, Thinking: m.Thinking, ThinkWire: m.ThinkWire, Tool: m.Tool,
				RequiresLogin: modelNeedsLogin(m), Enabled: true})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func configuredModels() map[string]ModelConfig {
	out := make(map[string]ModelConfig)
	for _, m := range catalogSnapshot() {
		if m.Enabled {
			out[m.ID] = m.config()
		}
	}
	return out
}

func catalogRevision(rows []catalogModel) string {
	b, _ := json.Marshal(rows)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func validateCatalog(rows []catalogModel) error {
	if len(rows) == 0 || len(rows) > 256 {
		return fmt.Errorf("模型数量必须为 1–256")
	}
	seen := map[string]bool{}
	enabled := 0
	for _, m := range rows {
		if !modelIDPattern.MatchString(m.ID) || seen[m.ID] {
			return fmt.Errorf("模型 ID 无效或重复：%s", m.ID)
		}
		seen[m.ID] = true
		if !modelHexPattern.MatchString(m.HexID) {
			return fmt.Errorf("%s：路由 ID 必须是 16 位小写十六进制", m.ID)
		}
		if m.Mode < 1 || m.Mode > 32 {
			return fmt.Errorf("%s：Mode 必须为 1–32", m.ID)
		}
		if m.ThinkWire != 0 && m.ThinkWire != 1 && m.ThinkWire != 2 && m.ThinkWire != 4 {
			return fmt.Errorf("%s：思考档位只能为 0、1、2、4", m.ID)
		}
		if m.Thinking && m.ThinkWire != 0 && m.ThinkWire != 2 {
			return fmt.Errorf("%s：输出思考链需使用扩展档（2）", m.ID)
		}
		switch m.Tool {
		case 0, toolImage, toolMusic, toolVideo, toolCanvas:
		default:
			return fmt.Errorf("%s：不支持的工具类型", m.ID)
		}
		if (m.Tool == 0 && strings.TrimSpace(m.Upstream) == "") || len(m.Upstream) > 120 || strings.ContainsAny(m.Upstream, "\r\n\x00") {
			return fmt.Errorf("%s：请填写有效的上游模型名称", m.ID)
		}
		if len(m.Description) > 2000 {
			return fmt.Errorf("%s：描述过长", m.ID)
		}
		if m.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return fmt.Errorf("至少保留一个启用的模型")
	}
	return nil
}

func initModelCatalog() {
	var rows []catalogModel
	if raw := kvGet(modelCatalogKey); raw != "" {
		if json.Unmarshal([]byte(raw), &rows) != nil || validateCatalog(rows) != nil {
			logf("[models] 已保存的模型配置无效，使用内置清单")
			rows = nil
		}
	}
	catalogMu.Lock()
	catalogSaved = rows
	catalogMu.Unlock()
}

func saveModelCatalog(rows []catalogModel, revision string) (int, error) {
	modelSettingsMu.Lock()
	defer modelSettingsMu.Unlock()
	if revision == "" || revision != catalogRevision(catalogSnapshot()) {
		return 409, fmt.Errorf("模型配置已变化，请刷新后重新编辑")
	}
	if err := validateCatalog(rows); err != nil {
		return 400, err
	}
	def := rtCfg().DefaultModel
	if def != "" {
		found := false
		for _, m := range rows {
			if m.ID == def && m.Enabled && m.Tool == 0 {
				found = true
			}
		}
		if !found {
			return 400, fmt.Errorf("请先在设置中更换默认模型，再停用、删除或改名 %s", def)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	b, _ := json.Marshal(rows)
	if err := kvSet(modelCatalogKey, string(b)); err != nil {
		return 500, fmt.Errorf("模型配置保存失败")
	}
	catalogMu.Lock()
	catalogSaved = append([]catalogModel(nil), rows...)
	catalogMu.Unlock()
	return 200, nil
}

func writeCatalog(w http.ResponseWriter) {
	rows := catalogSnapshot()
	accounts := []map[string]any{}
	for _, a := range accountList() {
		if a.Status == "enabled" {
			accounts = append(accounts, map[string]any{"id": a.ID, "label": accountDisplayName(&a)})
		}
	}
	writeJSON(w, 200, map[string]any{"models": rows, "revision": catalogRevision(rows), "accounts": accounts, "default_model": rtCfg().DefaultModel})
}

func decodeCatalogBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("请求格式无效或内容过大")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("请求只能包含一个 JSON 对象")
	}
	return nil
}

func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeCatalog(w)
	case http.MethodPut:
		var req struct {
			Models   []catalogModel `json:"models"`
			Revision string         `json:"revision"`
		}
		if err := decodeCatalogBody(w, r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if status, err := saveModelCatalog(req.Models, req.Revision); err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeCatalog(w)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func handleAdminDiscoverModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		AccountID int64 `json:"account_id"`
	}
	if err := decodeCatalogBody(w, r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var selected *CookieAccount
	for _, a := range accountList() {
		if a.Status == "enabled" && (req.AccountID == 0 || a.ID == req.AccountID) {
			selected = &a
			break
		}
	}
	if selected == nil {
		writeJSON(w, 400, map[string]string{"error": "请选择一个启用的 Cookie 账号"})
		return
	}
	var rows []catalogModel
	err := viaSlot(*selected, func(proxyURL string) error {
		xsrf, err := getXSRF(selected.Cookie, selected.AuthUser, proxyURL)
		if err != nil {
			return fmt.Errorf("获取账号登录状态失败，请检查该账号和代理")
		}
		freq, _ := json.Marshal([]any{[]any{[]any{"otAQ7b", "[1]", nil, "generic"}}})
		form := url.Values{"f.req": {string(freq)}, "at": {xsrf}}
		ep := fmt.Sprintf("%s/_/BardChatUi/data/batchexecute?rpcids=otAQ7b&bl=%s&hl=en&_reqid=%d&rt=c", geminiBase(selected.AuthUser), currentBL(proxyURL), time.Now().UnixNano()%1000000)
		h := buildGeminiHeaders(selected.Cookie, extractSAPISID(selected.Cookie), "")
		setAuthUserHeaders(h, selected.AuthUser)
		delete(h, "x-goog-ext-525001261-jspb")
		status, _, body, err := uploadPost(ep, h, []byte(form.Encode()), proxyURL)
		if err != nil {
			return fmt.Errorf("获取上游模型失败，请检查网络和代理")
		}
		if status != 200 {
			return fmt.Errorf("上游模型接口返回 HTTP %d", status)
		}
		rows, err = parseDiscoveredModels(string(body))
		return err
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"models": rows, "account_id": selected.ID, "fetched_at": time.Now().Unix(), "note": "此账号当前可见的模型。添加或更新后才生效；不会自动删除已有配置。"})
}

var modelSlugPattern = regexp.MustCompile(`[^a-z0-9.]+`)
var versionedModelNamePattern = regexp.MustCompile(`(?i)^(?:gemini\s+)?[0-9]+(?:\.[0-9]+)*\s+[a-z]`)

func parseDiscoveredModels(body string) ([]catalogModel, error) {
	rows := []catalogModel{}
	seen := map[string]bool{}
	// Decode only this RPC's primary model records, never compatibility hex lists.
	for _, line := range strings.Split(body, "\n") {
		var frames [][]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &frames) != nil {
			continue
		}
		for _, f := range frames {
			if len(f) < 3 || f[0] != "wrb.fr" || f[1] != "otAQ7b" {
				continue
			}
			raw, ok := f[2].(string)
			if !ok {
				continue
			}
			var p []any
			if json.Unmarshal([]byte(raw), &p) != nil || len(p) <= 15 {
				continue
			}
			list, ok := p[15].([]any)
			if !ok {
				continue
			}
			for _, v := range list {
				m, ok := v.([]any)
				if !ok || len(m) <= 17 {
					return nil, fmt.Errorf("上游模型记录格式变化，未导入任何模型")
				}
				hex, hOK := m[0].(string)
				label, nOK := m[1].(string)
				mode, mOK := m[17].(float64)
				if !hOK || !nOK || !mOK || mode != float64(int(mode)) {
					return nil, fmt.Errorf("上游缺少模型路由字段，未导入任何模型")
				}
				// Older records use [11] for the versioned name; current records
				// put it in [2] and use [11] for a subtitle such as "Everyday help".
				name := ""
				for _, pos := range []int{11, 2, 12, 1, 19} {
					if pos < len(m) {
						candidate, _ := m[pos].(string)
						if versionedModelNamePattern.MatchString(candidate) {
							name = candidate
							break
						}
					}
				}
				slugName := name
				if slugName == "" {
					slugName = label
				}
				id := "gemini-" + strings.Trim(modelSlugPattern.ReplaceAllString(strings.ToLower(slugName), "-"), "-")
				desc, _ := m[2].(string)
				if desc == name {
					desc, _ = m[11].(string)
				}
				if name == "" {
					desc = label + "：上游未提供固定模型名称，添加前请手动填写实际返回的校验名称。"
				}
				row := catalogModel{ID: id, HexID: hex, Mode: int(mode), Upstream: name, Description: desc, RequiresLogin: true, Enabled: true}
				checked := row
				if checked.Upstream == "" {
					checked.Upstream = label
				}
				if err := validateCatalog([]catalogModel{checked}); err != nil {
					return nil, fmt.Errorf("上游模型字段不合法，未导入任何模型")
				}
				if seen[id] {
					return nil, fmt.Errorf("上游返回重复模型名称，请手动核对")
				}
				seen[id] = true
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("上游未返回可识别的模型清单，请检查账号权限或稍后重试")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}
