package agent

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	_ "github.com/yetone/magpie/internal/sqlitedb"
)

// uosaiSetup makes a UOS AI "app" database under home with the four tables,
// ready for magpie to read and write. It returns the database path.
func uosaiSetup(t *testing.T, home string) string {
	t.Helper()
	dbPath := uosaiDB(home)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE config (name TEXT PRIMARY KEY, value TEXT, additional TEXT)`,
		`CREATE TABLE usedModel (assistant TEXT PRIMARY KEY, model TEXT NOT NULL, additional TEXT)`,
		`CREATE TABLE provider (id TEXT PRIMARY KEY, name TEXT NOT NULL, auth TEXT, provider TEXT NOT NULL, additional TEXT)`,
		`CREATE TABLE models (id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL, params TEXT, additional TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

// TestUOSAIDB locates the database under each platform's AppDataLocation.
func TestUOSAIDB(t *testing.T) {
	home := t.TempDir()
	got := uosaiDB(home)
	if got == "" {
		t.Fatal("no path")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("not absolute: %s", got)
	}
	if filepath.Base(got) != "app" {
		t.Fatalf("base = %s", got)
	}
}

// TestUOSAINotDetected when the database is absent: the agent is not here.
func TestUOSAINotDetected(t *testing.T) {
	home := t.TempDir()
	a := uosai(home)
	if a.Detected() {
		t.Fatal("detected without a database")
	}
}

// TestUOSAIDetected when the database exists.
func TestUOSAIDetected(t *testing.T) {
	home := t.TempDir()
	uosaiSetup(t, home)
	a := uosai(home)
	if !a.Detected() {
		t.Fatal("not detected with a database")
	}
}

// TestUOSAIIsInBuiltins: every agent is listed by All, so the picker can
// pick it by name and the Agents page shows it.
func TestUOSAIIsInBuiltins(t *testing.T) {
	home := t.TempDir()
	uosaiSetup(t, home)
	for _, a := range builtins(home, filepath.Join(home, ".config")) {
		if a.ID == uosaiID {
			if !a.Detected() {
				t.Fatal("UOS AI isn't detected by its listed agent")
			}
			return
		}
	}
	t.Fatal("UOS AI isn't listed by All")
}

// TestUOSAINotOnMagpie when the database has no magpie provider row.
func TestUOSAINotOnMagpie(t *testing.T) {
	home := t.TempDir()
	db := uosaiSetup(t, home)
	if uosaiOnMagpie(db, gateway.URL()) {
		t.Fatal("on magpie without a provider")
	}
}

// TestUOSAIPick writes the provider and one model row through magpie's
// field Set, and reads the pick back through Get. A second provider the
// user added is left alone.
func TestUOSAIPick(t *testing.T) {
	home := t.TempDir()
	db := uosaiSetup(t, home)
	// a provider the user added: not magpie's, kept by disconnect
	d, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO provider (id, name, auth, provider, additional) VALUES ('prov-deepseek', 'DeepSeek', '{"api_key":"sk-x"}', 'deepseek', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, provider, model, params, additional) VALUES ('mod-ds', 'prov-deepseek', '{"id":"9fad318a"}', '{}', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO usedModel (assistant, model, additional) VALUES ('uos-ai-generic', 'mod-ds', '')`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// save a deepseek provider magpie can serve
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}

	a := uosai(home)
	field := a.Fields[0]
	if err := field.Set("magpie/deepseek/deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	// the provider row points at the gateway with magpie's key
	r, ok := uosaiProviderRow(db, uosaiProvider)
	if !ok {
		t.Fatal("no magpie provider row")
	}
	if host := uosaiJSONString(r.Additional, "provider_host"); host != gatewayV1() {
		t.Fatalf("provider_host = %s, want %s", host, gatewayV1())
	}
	key := uosaiJSONString(r.Auth, "api_key")
	if !gatewayKey(key) {
		t.Fatalf("api_key = %s, not magpie's", key)
	}
	// a model row was written with the catalog id as model_id
	ms := uosaiModelRows(db, uosaiProvider)
	if len(ms) == 0 {
		t.Fatal("no model rows")
	}
	found := false
	for _, m := range ms {
		if uosaiModelRef(m.Model) == "deepseek/deepseek-chat" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no model row for deepseek/deepseek-chat: %+v", ms)
	}
	// usedModel points at a magpie model row
	used := uosaiUsedModels(db)
	if used["uos-ai-generic"] == "" || used["uos-ai-generic"] == "mod-ds" {
		t.Fatalf("usedModel[uos-ai-generic] = %s", used["uos-ai-generic"])
	}
	if !strings.HasPrefix(used["uos-ai-generic"], uosaiModelPfx) {
		t.Fatalf("usedModel not a magpie row: %s", used["uos-ai-generic"])
	}
	// Get reads the pick back
	if got := field.Get(); got != "magpie/deepseek/deepseek-chat" {
		t.Fatalf("Get = %s", got)
	}
	// on magpie
	if !uosaiOnMagpie(db, gateway.URL()) {
		t.Fatal("not on magpie after Set")
	}
	// the user's own provider is still there
	if r, ok := uosaiProviderRow(db, "prov-deepseek"); !ok || r.Name != "DeepSeek" {
		t.Fatal("user's provider lost")
	}
}

// TestUOSAIDisconnect restores the original provider row and usedModel pick
// after magpie's Set, leaving nothing of magpie's behind.
func TestUOSAIDisconnect(t *testing.T) {
	home := t.TempDir()
	db := uosaiSetup(t, home)
	// user's own provider and model
	d, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO provider (id, name, auth, provider, additional) VALUES ('prov-magpie', 'Mine', '{"api_key":"sk-pre"}', 'openai_compatible', '{"provider_host":"https://pre/v1"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, provider, model, params, additional) VALUES ('mod-pre', 'prov-magpie', '{"arch":1,"ability":1,"model_id":"pre","name":"Pre"}', '{}', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO usedModel (assistant, model, additional) VALUES ('uos-ai-generic', 'mod-pre', '')`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}

	a := uosai(home)
	field := a.Fields[0]
	if err := field.Set("magpie/deepseek/deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	// disconnect: Set with empty restores
	if err := field.Set(""); err != nil {
		t.Fatal(err)
	}
	// magpie's provider row is gone (or restored to the user's original)
	r, ok := uosaiProviderRow(db, uosaiProvider)
	if ok {
		// restored: the user's original values
		if r.Name != "Mine" || uosaiJSONString(r.Auth, "api_key") != "sk-pre" {
			t.Fatalf("provider not restored: %+v", r)
		}
	}
	// no magpie model rows
	if ms := uosaiModelRows(db, uosaiProvider); len(ms) != 0 {
		// restored rows are fine; magpie-written rows (mod-magpie-) are not
		for _, m := range ms {
			if m.ID == "mod-pre" {
				continue
			}
			t.Fatalf("magpie model row left: %+v", m)
		}
	}
	// usedModel restored
	used := uosaiUsedModels(db)
	if used["uos-ai-generic"] != "mod-pre" {
		t.Fatalf("usedModel not restored: %s", used["uos-ai-generic"])
	}
	// not on magpie
	if uosaiOnMagpie(db, gateway.URL()) {
		t.Fatal("still on magpie after disconnect")
	}
}

// TestUOSAISync rewrites the model rows when the catalog changes. A row
// the catalog dropped goes; one the user kept beside magpie's fields
// stays.
func TestUOSAISync(t *testing.T) {
	home := t.TempDir()
	db := uosaiSetup(t, home)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}
	a := uosai(home)
	field := a.Fields[0]
	if err := field.Set("magpie/deepseek/deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	// Sync rewrites: the model is still there
	if a.Sync != nil {
		if err := a.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	ms := uosaiModelRows(db, uosaiProvider)
	if len(ms) == 0 {
		t.Fatal("Sync dropped all models")
	}
	if n := a.Notice(); !strings.Contains(n, "restart UOS AI") {
		t.Fatalf("Notice = %q, want a restart", n)
	}
}

// uosaiRepoint changes UOS AI's Magpie provider's provider_host in its
// SQLite database, as TestDriftUnwiredEveryAgent does to every agent's
// config file: how many rows it changed. Agents other than UOS AI return 0.
func uosaiRepoint(a *Agent, home, from, to string) int {
	if a.ID != uosaiID {
		return 0
	}
	db := uosaiDB(home)
	d, err := sql.Open("sqlite", db)
	if err != nil {
		return 0
	}
	defer d.Close()
	var cnt int
	if err := d.QueryRow(`SELECT count(*) FROM provider WHERE id = ?`, uosaiProvider).Scan(&cnt); err != nil {
		return 0
	}
	var add string
	if d.QueryRow(`SELECT COALESCE(additional,'') FROM provider WHERE id = ?`, uosaiProvider).Scan(&add) != nil {
		return 0
	}
	if !strings.Contains(add, from) {
		return 0
	}
	if _, err := d.Exec(`UPDATE provider SET additional = ? WHERE id = ?`, strings.ReplaceAll(add, from, to), uosaiProvider); err != nil {
		return 0
	}
	return 1
}
