// open_models_test.go: open models through
// OpenRouter on both harnesses, on the customer's own OpenRouter key or
// KeepState's. The catalog lists each open model under every harness that
// runs it, with provider openrouter, the words "open model via OpenRouter",
// its list price per million tokens and, while a managed book is in force,
// its price on KeepState's key. A model is chosen only where the catalog
// offers it; a withheld one is refused with the catalog's own note, and an
// open model calls an OpenRouter key.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const kimiWithheldNote = "not certified yet: Kimi K3.1 Pro has not passed KeepState's certification run on Claude Code, so it cannot be chosen yet"

// openModelHarnesses is a catalog's harnesses with the open-model fields: a
// native model, an offered open model priced on both keys, and a withheld
// open model, under Claude Code; Codex with its native model and the same
// two open models.
func openModelHarnesses() []any {
	price := func(in, out int64) map[string]any {
		return map[string]any{"standing": "priced", "rate_version": "rates-v2",
			"list_microusd_per_mtok": map[string]any{"input": in, "output": out, "cache_write": in, "cache_read": in / 10},
			"note":                   "OpenRouter's list price per million tokens"}
	}
	native := func(id, name, provider string) map[string]any {
		return map[string]any{"id": id, "name": name, "offer": map[string]any{"standing": "offered"}, "efforts": []any{"low", "high"}, "default_effort": "high",
			"provider": provider, "price": price(3000000, 15000000)}
	}
	glm := func() map[string]any {
		return map[string]any{"id": "z-ai/glm-5.3", "name": "GLM-5.3", "offer": map[string]any{"standing": "offered"}, "efforts": []any{}, "default_effort": "",
			"provider": "openrouter", "route": "open model via OpenRouter", "context_tokens": 202752, "price": price(69900, 1400000),
			"managed_offer": map[string]any{"standing": "offered", "charged_microusd_per_mtok": map[string]any{"input": 72696, "output": 1456000, "cache_write": 72696, "cache_read": 7270}},
			"key_route":     map[string]any{"provider": "openrouter", "standing": "enabled", "key_id": "vlt_or", "managed": "offered"}}
	}
	kimi := func(note string) map[string]any {
		return map[string]any{"id": "moonshotai/kimi-k3.1-pro", "name": "Kimi K3.1 Pro", "offer": map[string]any{"standing": "withheld", "note": note}, "efforts": []any{}, "default_effort": "",
			"provider": "openrouter", "route": "open model via OpenRouter", "context_tokens": 1048576, "price": price(2000000, 10000000),
			"key_route": map[string]any{"provider": "openrouter", "standing": "enabled", "key_id": "vlt_or", "managed": "unavailable"}}
	}
	return []any{
		map[string]any{"id": "claude-code", "name": "Claude Code", "provider": "anthropic", "default_model": "claude-haiku-5-5", "offer": map[string]any{"standing": "offered"},
			"models": []any{native("claude-haiku-5-5", "Claude Haiku 5.5", "anthropic"), glm(), kimi(kimiWithheldNote)}},
		map[string]any{"id": "codex", "name": "Codex CLI", "provider": "openai", "default_model": "gpt-6-luna", "offer": map[string]any{"standing": "offered"},
			"models": []any{native("gpt-6-luna", "GPT-6 Luna", "openai"), glm(), kimi(strings.Replace(kimiWithheldNote, "Claude Code", "Codex CLI", 1))}},
	}
}

func openModelKeys() []any {
	return []any{
		map[string]any{"id": "vlt_a", "provider": "anthropic", "last4": "aaaa", "enabled": true},
		map[string]any{"id": "vlt_o", "provider": "openai", "last4": "oooo", "enabled": true},
		map[string]any{"id": "vlt_or", "provider": "openrouter", "last4": "rrrr", "enabled": true},
	}
}

// The catalog print: an open model reads beside "open model via OpenRouter"
// with its list price and its price on KeepState's key; a withheld one with
// the catalog's own note. The native models' line reads as before.
func TestOpenModelsCatalogPrintsOpenModelsWithTheirRouteAndPrice(t *testing.T) {
	c := &runCtl{workspace: "available", harnesses: openModelHarnesses()}
	syncFixture(t, &c.mu)
	bin, cfg := runAgentEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "models")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		// the native models, exactly as before
		"calls anthropic; models claude-haiku-5-5\n",
		"calls openai; models gpt-6-luna\n",
		// the offered open model: route, price on your key and on KeepState's
		"open model via OpenRouter: z-ai/glm-5.3 (GLM-5.3), offered; $0.0699 in / $1.40 out per million tokens on your OpenRouter key; on KeepState's key: $0.072696 in / $1.456 out per million tokens",
		// the withheld one, with the catalog's note
		"open model via OpenRouter: moonshotai/kimi-k3.1-pro (Kimi K3.1 Pro), withheld; $2.00 in / $10.00 out per million tokens on your OpenRouter key",
		"withheld: " + kimiWithheldNote,
		"withheld: not certified yet: Kimi K3.1 Pro has not passed KeepState's certification run on Codex CLI",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the catalog print lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "claude-haiku-5-5, z-ai") || strings.Contains(out, "open model via OpenRouter: claude") {
		t.Errorf("a native model reads as an open model, or open models joined the native list:\n%s", out)
	}
}

// A run on an offered open model calls an OpenRouter key: the preflight
// checks OpenRouter, the session is bound to the account's OpenRouter key,
// and the creation names the model.
func TestOpenModelsRunAgentOnAnOpenModelCallsAnOpenRouterKey(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer(), keys: openModelKeys(), harnesses: openModelHarnesses()}
	syncFixture(t, &c.mu)
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "z-ai/glm-5.3")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	c.mu.Lock()
	created, pre, bound := append([]map[string]any(nil), c.creations...), append([]map[string]any(nil), c.preflight...), append([]string(nil), c.bindings...)
	c.mu.Unlock()
	if len(created) != 1 || created[0]["runner_model"] != "z-ai/glm-5.3" {
		t.Fatalf("the creation does not carry the open model: %v", created)
	}
	if _, sent := created[0]["harness"]; sent {
		t.Fatalf("a Claude Code run sent a harness: %v", created[0])
	}
	if len(pre) != 1 || pre[0]["provider"] != "openrouter" {
		t.Fatalf("the preflight did not check the OpenRouter key: %v", pre)
	}
	if len(bound) != 1 || !strings.Contains(bound[0], `"openrouter":"vlt_or"`) {
		t.Fatalf("the session was not bound to the OpenRouter key: %v", bound)
	}
	if !strings.Contains(errOut, "open model via OpenRouter") {
		t.Errorf("the run does not say the model is an open model via OpenRouter:\n%s", errOut)
	}
	// on Codex too
	out, errOut, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--harness", "codex", "--model", "z-ai/glm-5.3")
	if code != 0 {
		t.Fatalf("codex: exit %d\n%s\n%s", code, out, errOut)
	}
	c.mu.Lock()
	created, pre, bound = append([]map[string]any(nil), c.creations...), append([]map[string]any(nil), c.preflight...), append([]string(nil), c.bindings...)
	c.mu.Unlock()
	if created[1]["harness"] != "codex" || pre[1]["provider"] != "openrouter" || !strings.Contains(bound[1], `"openrouter":"vlt_or"`) {
		t.Fatalf("codex on an open model: %v %v %v", created[1], pre[1], bound[1])
	}
	// a key of another provider is refused, naming OpenRouter
	_, errOut, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "z-ai/glm-5.3", "--key", "vlt_a")
	if code != exitUsage || !strings.Contains(errOut, "OpenRouter") || !strings.Contains(errOut, "Nothing was created") {
		t.Fatalf("an Anthropic key for an open model: exit %d\n%s", code, errOut)
	}
	if n := len(c.creations); n != 2 {
		t.Fatalf("a refused run created a session: %d", n)
	}
	// no OpenRouter key: refused naming OpenRouter and how to add one
	c2 := &runCtl{workspace: "available", prov: readyAnswer(), keys: openModelKeys()[:2], harnesses: openModelHarnesses()}
	syncFixture(t, &c2.mu)
	bin2, cfg2 := runAgentEnv(t, c2)
	_, errOut, code = auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "run", "--agent", "--model", "z-ai/glm-5.3")
	if code == 0 || !strings.Contains(errOut, "OpenRouter") || !strings.Contains(errOut, "ks key add --provider openrouter") || c2.asked("POST /api/v2/sessions") {
		t.Fatalf("no OpenRouter key: exit %d\n%s", code, errOut)
	}
}

// A withheld open model is refused with the catalog's own note, before
// anything is created; no hard-coded list is consulted.
func TestOpenModelsRunAgentRefusesAWithheldOpenModelWithTheCatalogsNote(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer(), keys: openModelKeys(), harnesses: openModelHarnesses()}
	syncFixture(t, &c.mu)
	bin, cfg := runAgentEnv(t, c)
	_, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "moonshotai/kimi-k3.1-pro")
	if code != exitConflict || !strings.Contains(errOut, kimiWithheldNote) || !strings.Contains(errOut, "Nothing was created") {
		t.Fatalf("a withheld open model: exit %d\n%s", code, errOut)
	}
	if c.asked("POST /api/v2/sessions") || c.asked("POST /api/v2/preflight") {
		t.Fatalf("a withheld model reached the service: %v", c.requests)
	}
	// a model the catalog does not list for the harness goes to the service,
	// which decides (an older catalog, or a model it alone knows)
	if _, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "claude-sonnet-5-5", "--key", "vlt_a"); code != 0 {
		t.Fatalf("an unlisted model was refused by the client: exit %d", code)
	}
}

// An open model on KeepState's key: managed ["openrouter"], no own key bound.
func TestOpenModelsRunAgentOnAnOpenModelOnKeepStatesKey(t *testing.T) {
	c := newMKCtl()
	c.harnesses = openModelHarnesses()
	c.doc["managed"].(map[string]any)["providers"].(map[string]any)["openrouter"] = map[string]any{"standing": "offered", "note": "", "spread_pct": 4,
		"models": []any{map[string]any{"id": "z-ai/glm-5.3", "standing": "offered", "note": ""}}}
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "z-ai/glm-5.3", "--key", "keepstate")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	b := c.body("POST /api/v2/sessions")
	if jsonOf(b["managed"]) != `["openrouter"]` || b["runner_model"] != "z-ai/glm-5.3" {
		t.Fatalf("the creation did not put the open model's OpenRouter calls on KeepState's key: %v", b)
	}
	if p := c.body("POST /api/v2/preflight"); p["provider"] != "openrouter" || jsonOf(p["managed"]) != `["openrouter"]` {
		t.Fatalf("the preflight did not judge KeepState's OpenRouter key: %v", p)
	}
	if c.asked("POST /api/v2/sessions/session_abc123/bindings") {
		t.Fatalf("a run on KeepState's key bound an own key: %v", c.requests)
	}
	if !strings.Contains(errs, "OpenRouter") {
		t.Errorf("the run does not name OpenRouter:\n%s", errs)
	}
}

// ks key default and ks session key set take openrouter where the service
// now does.
func TestOpenModelsOpenRouterIsAProviderForKeyDefaultsAndSessionKeys(t *testing.T) {
	c := newMKCtl()
	c.doc["defaults"].(map[string]any)["openrouter"] = "own"
	c.doc["managed"].(map[string]any)["providers"].(map[string]any)["openrouter"] = map[string]any{"standing": "offered", "note": "", "spread_pct": 4, "models": []any{}}
	bin, cfg := mkEnv(t, c)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default")
	if code != 0 || !strings.Contains(out, "openrouter") {
		t.Fatalf("the defaults do not list openrouter: %d\n%s%s", code, out, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default", "openrouter", "keepstate")
	if code != 0 {
		t.Fatalf("set openrouter: %d\n%s%s", code, out, errs)
	}
	if raw := c.rawBody("PUT /api/v2/account/model-keys"); raw != `{"defaults":{"openrouter":"managed"},"expected_revision":2}` {
		t.Fatalf("PUT body: %s", raw)
	}
	if !strings.Contains(out, "OpenRouter calls on KeepState's key") {
		t.Errorf("the answer does not name OpenRouter:\n%s", out)
	}
	// a session on an open model: --key keepstate defaults to the provider
	// its model calls, which the record's runner key names
	c.record = map[string]any{"id": "session_rec1", "revision": 3, "harness": "claude-code", "runner_model": "z-ai/glm-5.3",
		"runner_key": map[string]any{"provider": "openrouter", "route": "own"}, "managed": []any{}}
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate")
	if code != 0 {
		t.Fatalf("session key set: %d\n%s%s", code, out, errs)
	}
	if b := c.body("POST /api/v2/sessions/session_rec1/bindings"); jsonOf(b["managed"]) != `["openrouter"]` {
		t.Fatalf("the open model's OpenRouter calls were not moved to KeepState's key: %v", b)
	}
	if _, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate", "--provider", "openrouter"); code != 0 {
		t.Fatalf("--provider openrouter: %d\n%s", code, errs)
	}
	if _, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate", "--provider", "mistral"); code != exitUsage || !strings.Contains(errs, "anthropic, openai or openrouter") {
		t.Fatalf("--provider mistral: %d\n%s", code, errs)
	}
}

// ks session model and ks session runner: an open model the catalog offers
// for the session's harness is sent (with --key, an OpenRouter key id or
// keepstate); a withheld one is refused with the catalog's note, nothing sent.
func TestOpenModelsSessionModelAndRunnerTakeAnOfferedOpenModel(t *testing.T) {
	c := &modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}
	syncFixture(t, &c.mu)
	harnesses := openModelHarnesses()
	srv := httptest.NewServer(catalogBeside(harnesses, c))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	_, errs, code := auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark", "moonshotai/kimi-k3.1-pro")
	if code != exitConflict || !strings.Contains(errs, kimiWithheldNote) || !strings.Contains(errs, "Nothing changed") {
		t.Fatalf("a withheld open model: %d\n%s", code, errs)
	}
	if len(c.patched()) != 0 {
		t.Fatalf("a withheld model was sent: %v", c.patched())
	}
	// offered: sent with the OpenRouter key chosen (modelCtl refuses any
	// model but its two natives, so its refusal is the service's word)
	_, errs, _ = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark", "z-ai/glm-5.3", "--key", "vlt_or")
	p := c.patched()
	if len(p) != 1 || p[0]["runner_model"] != "z-ai/glm-5.3" || p[0]["key"] != "vlt_or" {
		t.Fatalf("the change did not carry the open model and its key: %v\n%s", p, errs)
	}
}

// catalogBeside serves the catalog's harnesses at GET /api/v2/models and
// everything else from next.
func catalogBeside(harnesses []any, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v2/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": map[string]any{"catalog_version": "t", "harnesses": harnesses}})
			return
		}
		next.ServeHTTP(w, r)
	}
}

func TestOpenModelsSessionRunnerChecksTheModelAgainstTheTargetHarness(t *testing.T) {
	c := &runnerCtl{modelCtl: modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}, ops: map[string]map[string]any{}, reads: map[string]int{},
		harness: "claude-code", key: map[string]any{"provider": "anthropic", "route": "own", "key": map[string]any{"id": "vlt_own", "label": "Your Anthropic key", "last4": "7Q2a"}}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(catalogBeside(openModelHarnesses(), c))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	_, errs, code := auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--harness", "codex", "--model", "moonshotai/kimi-k3.1-pro")
	if code != exitConflict || !strings.Contains(errs, "on Codex CLI, so it cannot be chosen yet") || !strings.Contains(errs, "Nothing changed") {
		t.Fatalf("a withheld open model on codex: %d\n%s", code, errs)
	}
	c.rmu.Lock()
	n := len(c.sent)
	c.rmu.Unlock()
	if n != 0 {
		t.Fatal("a withheld model was sent")
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--model", "z-ai/glm-5.3", "--key", "vlt_or")
	if code != 0 {
		t.Fatalf("an offered open model: %d\n%s%s", code, out, errs)
	}
	c.rmu.Lock()
	sent := append([]map[string]any(nil), c.sent...)
	c.rmu.Unlock()
	if len(sent) != 1 || sent[0]["runner_model"] != "z-ai/glm-5.3" || sent[0]["key"] != "vlt_or" {
		t.Fatalf("the change did not carry the open model and its key: %v", sent)
	}
	if !strings.Contains(errs, "open model via OpenRouter") {
		t.Errorf("the change does not say the model is an open model via OpenRouter:\n%s", errs)
	}
}
