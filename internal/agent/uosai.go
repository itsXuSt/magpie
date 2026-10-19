package agent

// UOS AI (统信 UOS AI Assistant, the uos-ai-assistant desktop app) keeps
// every account and model a user picked in its own SQLite database, under
// QStandardPaths::AppDataLocation (~/.local/share/deepin/uos-ai-assistant
// on Linux, ~/Library/Application Support/deepin/uos-ai-assistant on a
// Mac, %LOCALAPPDATA%/deepin/uos-ai-assistant on Windows). Four tables
// live in one "app" file:
//
//	provider (id, name, auth, provider, additional)
//	models   (id, provider, model, params, additional)
//	usedModel(assistant, model, additional)
//	config   (name, value, additional)
//
// A provider row's `provider` names its kind; "openai_compatible" reads
// `additional.provider_host` as its base URL (the OaiChatModel appends
// "/chat/completions"), `auth.api_key` as the bearer key, and the model
// row's `model.model_id` (or a referenced built-in) as the request's
// `model`. So magpie points UOS AI at the gateway the same way it points
// Cline: by owning one "openai_compatible" provider row and the model rows
// under it. UOS AI's requests carry no User-Agent of their own (its Qt
// HTTP client sets none), so the gateway key magpie gives it names it
// (magpie-uosai), as Prime Agent's does.
//
// UOS AI keeps one usedModel row per assistant feature (uos-ai-generic,
// uos-ai-writing, uos-ai-knowledge-base, uos-ai-data-analysis,
// uos-ai-translation, uos-ai-claw); picking a model sets the ones the user
// has, so every feature follows magpie. The app reads the database when a
// session starts, so a running session needs a restart to use the pick.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/gateway"
	_ "github.com/yetone/magpie/internal/sqlitedb"
)

// uosaiProvider is the id magpie owns in UOS AI's provider table, and
// uosaiModelPfx is the prefix of its model rows' ids, so they are told
// apart from a user's own at a glance.
const (
	uosaiID            = "uosai"
	uosaiProvider      = "prov-magpie"
	uosaiModelPfx      = "mod-magpie-"
	uosaiAssistantsKey = "uosai.assistants"
)

// uosaiAssistants are the UOS AI features that keep a usedModel row. The
// generic one is always set when none is there; the rest are set when they
// appear in the table, so a pick follows every feature the user uses
// without forcing the others into being.
var uosaiAssistants = []string{
	"uos-ai-generic", "uos-ai-writing", "uos-ai-knowledge-base",
	"uos-ai-data-analysis", "uos-ai-translation", "uos-ai-claw",
}

// uosaiDB returns the path of UOS AI's "app" database under home, as
// QStandardPaths::AppDataLocation resolves it on each platform. "" when
// home can't be read.
func uosaiDB(home string) string {
	if home == "" {
		return ""
	}
	var dir string
	switch runtime.GOOS {
	case "darwin":
		dir = filepath.Join(home, "Library", "Application Support", "deepin", "uos-ai-assistant")
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		dir = filepath.Join(local, "deepin", "uos-ai-assistant")
	default:
		dir = filepath.Join(home, ".local", "share", "deepin", "uos-ai-assistant")
	}
	return filepath.Join(dir, "db", "app")
}

// uosaiOpen opens UOS AI's database with a busy timeout, so a running UOS
// AI holding it doesn't fail a pick at once. The url is built like cindy's
// and kiro's, with the path absolute and slash-spelt.
func uosaiOpen(db string) (*sql.DB, error) {
	p := filepath.ToSlash(db)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "_pragma=busy_timeout(3000)"}
	return sql.Open("sqlite", u.String())
}

// uosaiOpenRW opens UOS AI's database for writing, making its folder and
// the file if UOS AI is not here yet (magpie's pick is what creates them,
// as any agent's config file magpie writes).
func uosaiOpenRW(db string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		return nil, err
	}
	p := filepath.ToSlash(db)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "_pragma=busy_timeout(3000)&mode=rwc"}
	d, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// a database magpie makes carries UOS AI's schema, so a later write
	// or the app itself finds the tables it reads
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS config (name TEXT PRIMARY KEY, value TEXT, additional TEXT)`,
		`CREATE TABLE IF NOT EXISTS usedModel (assistant TEXT PRIMARY KEY, model TEXT NOT NULL, additional TEXT)`,
		`CREATE TABLE IF NOT EXISTS provider (id TEXT PRIMARY KEY, name TEXT NOT NULL, auth TEXT, provider TEXT NOT NULL, additional TEXT)`,
		`CREATE TABLE IF NOT EXISTS models (id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL, params TEXT, additional TEXT)`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}

// uosaiRow is one row of UOS AI's provider or models table, as the JSON
// magpie reads and writes: the columns are TEXT, and the JSON columns are
// parsed back into maps when read.
type uosaiRow struct {
	ID, Name, Provider string
	Auth, Additional   string
	Model, Params      string
}

// uosaiProviderRow reads the row with id from UOS AI's provider table. No
// row, or a database that can't be read, is false; the app may not be
// installed.
func uosaiProviderRow(db, id string) (uosaiRow, bool) {
	d, err := uosaiOpen(db)
	if err != nil {
		return uosaiRow{}, false
	}
	defer d.Close()
	var r uosaiRow
	err = d.QueryRow(`SELECT id, name, COALESCE(auth,''), provider, COALESCE(additional,'') FROM provider WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Auth, &r.Provider, &r.Additional)
	if err != nil {
		return uosaiRow{}, false
	}
	return r, true
}

// uosaiModelRows reads every row of UOS AI's models table under provider
// id. Missing columns default to "".
func uosaiModelRows(db, providerID string) []uosaiRow {
	d, err := uosaiOpen(db)
	if err != nil {
		return nil
	}
	defer d.Close()
	rows, err := d.Query(`SELECT id, provider, COALESCE(model,''), COALESCE(params,''), COALESCE(additional,'') FROM models WHERE provider = ?`, providerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []uosaiRow
	for rows.Next() {
		var r uosaiRow
		if rows.Scan(&r.ID, &r.Provider, &r.Model, &r.Params, &r.Additional) != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// uosaiUsedModels reads every (assistant, model) pair in usedModel.
func uosaiUsedModels(db string) map[string]string {
	d, err := uosaiOpen(db)
	if err != nil {
		return nil
	}
	defer d.Close()
	rows, err := d.Query(`SELECT assistant, model FROM usedModel`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, m string
		if rows.Scan(&a, &m) == nil {
			out[a] = m
		}
	}
	return out
}

// uosaiExec runs one statement with args. Errors are returned, not logged,
// so a pick can report them.
func uosaiExec(db, stmt string, args ...any) error {
	d, err := uosaiOpenRW(db)
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = d.Exec(stmt, args...)
	return err
}

// uosaiOnMagpie reports whether magpie's provider row points at the gateway
// at gw and still carries a key magpie gives UOS AI. That is how the
// picker tells a connected UOS AI from one a user set up by hand.
func uosaiOnMagpie(db, gw string) bool {
	r, ok := uosaiProviderRow(db, uosaiProvider)
	if !ok {
		return false
	}
	host := uosaiJSONString(r.Additional, "provider_host")
	if !strings.HasPrefix(host, gw) {
		return false
	}
	// gatewayKey, not ourKey: UOS AI's key names it (magpie-uosai), and
	// ourKey would miss a TokenFor key, since the gateway on loopback
	// doesn't Authenticate it (it lets it through without a check).
	return gatewayKey(uosaiJSONString(r.Auth, "api_key"))
}

// uosaiJSONString reads one string field out of a JSON column. "" for
// missing, non-string or unparseable.
func uosaiJSONString(raw, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// uosaiStash records what UOS AI's tables had before magpie's first write,
// so Disconnect puts it back. The stash keeps the provider row, its model
// rows (as one JSON array) and the usedModel pairs (as one JSON object),
// keyed by "uosai.provider", "uosai.models" and "uosai.usedModel"; the
// assistants the user had are kept too, so a pick only follows features
// that were there. Each value is "" when nothing was there, so a restore
// removes magpie's rows outright.
func uosaiStash(db string) map[string]string {
	r, ok := uosaiProviderRow(db, uosaiProvider)
	var prov, models, used string
	if ok {
		prov = uosaiRowJSON(r)
	}
	rows := uosaiModelRows(db, uosaiProvider)
	if len(rows) > 0 {
		b, _ := json.Marshal(rows)
		models = string(b)
	}
	usedMap := uosaiUsedModels(db)
	if len(usedMap) > 0 {
		b, _ := json.Marshal(usedMap)
		used = string(b)
	}
	assistants := strings.Join(uosaiAssistantsIn(usedMap), ",")
	return map[string]string{
		"uosai.provider":   prov,
		"uosai.models":     models,
		"uosai.usedModel":  used,
		uosaiAssistantsKey: assistants,
	}
}

// uosaiAssistantsIn returns the assistants the user actually has rows for,
// in the canonical order, so a pick writes the same set back.
func uosaiAssistantsIn(used map[string]string) []string {
	var out []string
	for _, a := range uosaiAssistants {
		if _, ok := used[a]; ok {
			out = append(out, a)
		}
	}
	return out
}

// uosaiRowJSON is one provider row as JSON, for the stash.
func uosaiRowJSON(r uosaiRow) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// uosaiWriteProvider upserts magpie's provider row pointing at v1 with key.
// name is "Magpie" so the row reads as magpie in UOS AI's own UI.
func uosaiWriteProvider(db, key, v1 string) error {
	auth, _ := json.Marshal(map[string]string{"api_key": key})
	add, _ := json.Marshal(map[string]string{"provider_host": v1})
	r, _ := uosaiProviderRow(db, uosaiProvider)
	if r.ID == "" {
		return uosaiExec(db, `INSERT INTO provider (id, name, auth, provider, additional) VALUES (?, ?, ?, ?, ?)`,
			uosaiProvider, "Magpie", string(auth), "openai_compatible", string(add))
	}
	return uosaiExec(db, `UPDATE provider SET name=?, auth=?, provider=?, additional=? WHERE id=?`,
		"Magpie", string(auth), "openai_compatible", string(add), uosaiProvider)
}

// uosaiWriteModels rewrites magpie's model rows under uosaiProvider to the
// catalog now: old magpie rows that the catalog no longer has go, new ones
// come, and a row a user changed beside magpie's model id is left alone. A
// row's `model` is the JSON UOS AI reads: a reference by id
// (`{"id": "<ref>"}`), which UOS AI resolves through its own list, and
// which the gateway answers either way.
func uosaiWriteModels(db string) error {
	// One connection, one transaction: many opens of the same database
	// write in modernc SQLite can leave it malformed, and a whole-catalog
	// rewrite should be atomic anyway — the agent never sees half a list.
	d, err := uosaiOpenRW(db)
	if err != nil {
		return err
	}
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// only rows magpie wrote (mod-magpie- prefix) are removed when the
	// catalog drops them; a row the user added under magpie's provider by
	// hand stays, whatever its model_id.
	if _, err := tx.Exec(`DELETE FROM models WHERE provider = ? AND id LIKE ?`, uosaiProvider, uosaiModelPfx+"%"); err != nil {
		return err
	}
	for _, m := range magpieModels(uosaiID) {
		id := uosaiModelPfx + m.ID
		model := uosaiModelJSON(m)
		if _, err := tx.Exec(`INSERT INTO models (id, provider, model, params, additional) VALUES (?, ?, ?, '{}', '{}')`,
			id, uosaiProvider, model); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// uosaiModelRef reads the model_id of a model row JSON, the value
// magpie wrote there and UOS AI sends upstream as its model field.
func uosaiModelRef(raw string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	s, _ := m["model_id"].(string)
	return s
}

// uosaiModelJSON is the `model` JSON for one catalog model: a reference by
// id, which UOS AI resolves through its own list if it has the id, and
// which magpie's gateway answers either way, so the reference form is
// preferred.
func uosaiModelJSON(m catalog.Model) string {
	// UOS AI resolves a built-in id by name, and magpie's models aren't
	// among them, so the row carries a full description: `model_id` is the
	// catalog id magpie's gateway answers (provider/model, or a routing
	// group's), and is what UOS AI sends upstream as its `model` field.
	// `arch` is MaLanguage (1); `ability` carries text, and reasoning when
	// the model has effort levels, as UOS AI's own models do.
	ability := 1 // MaText
	if len(m.Efforts) > 0 {
		ability |= 8 // MaReasoning
	}
	b, _ := json.Marshal(map[string]any{
		"arch":     1, // ModelArch::MaLanguage
		"ability":  ability,
		"model_id": m.ID,
		"name":     m.Name,
	})
	return string(b)
}

// uosaiPick sets the usedModel rows for the assistants the user has (or at
// least the generic one) to the model row magpie wrote for ref. ref is a
// catalog id; its row is mod-magpie-<ref>.
func uosaiPick(db, ref string) error {
	used := uosaiUsedModels(db)
	assistants := uosaiAssistantsIn(used)
	if len(assistants) == 0 {
		assistants = []string{uosaiAssistants[0]}
	}
	modelID := uosaiModelPfx + ref
	for _, a := range assistants {
		if err := uosaiUpsertUsed(db, a, modelID); err != nil {
			return err
		}
	}
	return nil
}

// uosaiUpsertUsed sets one usedModel row, inserting or updating.
func uosaiUpsertUsed(db, assistant, model string) error {
	d, err := uosaiOpenRW(db)
	if err != nil {
		return err
	}
	defer d.Close()
	var existing string
	err = d.QueryRow(`SELECT assistant FROM usedModel WHERE assistant = ?`, assistant).Scan(&existing)
	if err == sql.ErrNoRows {
		_, err = d.Exec(`INSERT INTO usedModel (assistant, model, additional) VALUES (?, ?, '')`, assistant, model)
		return err
	}
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE usedModel SET model=? WHERE assistant=?`, model, assistant)
	return err
}

// uosaiRestore puts back what uosaiStash saved: magpie's provider row and
// model rows go, the originals come back, and usedModel rows are restored
// (or removed, if the user had none for an assistant). A stash entry that
// is empty means magpie made the row, so it is deleted.
func uosaiRestore(db string) error {
	providerRaw := unstash("uosai.provider")
	modelsRaw := unstash("uosai.models")
	usedRaw := unstash("uosai.usedModel")
	// nothing was stashed: remove magpie's rows outright
	if providerRaw == "" && modelsRaw == "" && usedRaw == "" {
		_ = uosaiExec(db, `DELETE FROM models WHERE provider = ?`, uosaiProvider)
		_ = uosaiExec(db, `DELETE FROM provider WHERE id = ?`, uosaiProvider)
		return nil
	}
	// provider row
	if providerRaw == "" {
		_ = uosaiExec(db, `DELETE FROM provider WHERE id = ?`, uosaiProvider)
	} else {
		var r uosaiRow
		if json.Unmarshal([]byte(providerRaw), &r) == nil {
			if err := uosaiExec(db, `UPDATE provider SET name=?, auth=?, provider=?, additional=? WHERE id=?`,
				r.Name, r.Auth, r.Provider, r.Additional, r.ID); err != nil {
				return err
			}
		}
	}
	// model rows: remove magpie's, put back the stashed ones
	if err := uosaiExec(db, `DELETE FROM models WHERE provider = ?`, uosaiProvider); err != nil {
		return err
	}
	if modelsRaw != "" {
		var rows []uosaiRow
		if json.Unmarshal([]byte(modelsRaw), &rows) == nil {
			for _, r := range rows {
				if err := uosaiExec(db, `INSERT INTO models (id, provider, model, params, additional) VALUES (?, ?, ?, ?, ?)`,
					r.ID, r.Provider, r.Model, r.Params, r.Additional); err != nil {
					return err
				}
			}
		}
	}
	// usedModel: restore stashed pairs, remove magpie's picks it added
	if usedRaw != "" {
		var used map[string]string
		if json.Unmarshal([]byte(usedRaw), &used) == nil {
			// remove rows for assistants magpie may have added, then put back
			for _, a := range uosaiAssistants {
				if _, ok := used[a]; !ok {
					_ = uosaiExec(db, `DELETE FROM usedModel WHERE assistant = ?`, a)
				}
			}
			for a, m := range used {
				if err := uosaiUpsertUsed(db, a, m); err != nil {
					return err
				}
			}
		}
	} else {
		// magpie added usedModel rows for assistants the user had none for;
		// remove only the generic one if it had nothing before
		for _, a := range uosaiAssistants {
			_ = uosaiExec(db, `DELETE FROM usedModel WHERE assistant = ? AND model LIKE ?`, a, uosaiModelPfx+"%")
		}
	}
	return nil
}

// uosai is the UOS AI agent at home, as magpie opens it. It owns one
// openai_compatible provider row and the model rows under it in UOS AI's
// database, and the model field picks among magpie's catalog by writing the
// usedModel rows. A running UOS AI reads the database when a session
// starts, so its Notice asks for a restart after a pick.
func uosai(home string) *Agent {
	db := uosaiDB(home)
	on := func() bool { return uosaiOnMagpie(db, gateway.URL()) }
	return &Agent{
		ID: uosaiID, Name: "UOS AI", Icon: "uosai-color",
		Aliases: []string{"uos-ai", "uos-ai-assistant"},
		Dir:     filepath.Dir(filepath.Dir(db)),
		Path:    db,
		detect:  func() bool { return isFile(db) },
		Spelled: prefixed,
		Sync: func() error {
			// a provider row magpie wrote means UOS AI is connected, even
			// when the gateway's own address has changed; the row is
			// pointed at it again below.
			if _, ok := uosaiProviderRow(db, uosaiProvider); !ok {
				return nil
			}
			// the gateway's port or address changed: point the provider
			// row at it again, as every agent's Sync does
			if err := uosaiWriteProvider(db, agentKeyAt(uosaiID, gateway.URL()), gatewayV1()); err != nil {
				return err
			}
			return uosaiWriteModels(db)
		},
		Unwire: func() error { return uosaiRestore(db) },
		move: func(from, to string) error {
			r, ok := uosaiProviderRow(db, uosaiProvider)
			if !ok {
				return nil
			}
			host := uosaiJSONString(r.Additional, "provider_host")
			if host == "" {
				return nil
			}
			add := strings.ReplaceAll(r.Additional, from, to)
			return uosaiExec(db, `UPDATE provider SET additional=? WHERE id=?`, add, uosaiProvider)
		},
		Check: func() string {
			// a row magpie wrote can be broken while UOS AI's pick is no
			// longer on one of its models (the user picked another in the
			// app); the row being there is what magpie checks.
			if r, ok := uosaiProviderRow(db, uosaiProvider); ok {
				key, host := uosaiJSONString(r.Auth, "api_key"), uosaiJSONString(r.Additional, "provider_host")
				if !keyHolds(key, agentKeyAt(uosaiID, gateway.URL())) {
					return "UOS AI's Magpie provider no longer carries magpie's key — another tool rewrote it"
				}
				if host == "" || host != gatewayV1() && !reachesMagpie(host, gatewayV1()) {
					return "UOS AI's Magpie provider no longer points at the gateway — another tool rewrote it"
				}
			}
			return ""
		},
		Notice: func() string { return noticeUOSAI.String() },
		Fields: []Field{{
			Key: "model", Label: "model",
			Get: func() string {
				used := uosaiUsedModels(db)
				m := used[uosaiAssistants[0]]
				if m == "" {
					return ""
				}
				if ref, ok := uosaiModelRefByID(db, m); ok {
					return magpieID + "/" + ref
				}
				return ""
			},
			Set: func(v string) error {
				if v == "" {
					if on() {
						return uosaiRestore(db)
					}
					return nil
				}
				ref, ok := cutMagpie(v)
				if !ok {
					return fmt.Errorf("expected a magpie/<ref> model, got %q", v)
				}
				if !on() {
					stash(uosaiStash(db))
				}
				if err := uosaiWriteProvider(db, agentKeyAt(uosaiID, gateway.URL()), gatewayV1()); err != nil {
					return err
				}
				if err := uosaiWriteModels(db); err != nil {
					return err
				}
				return uosaiPick(db, ref)
			},
			Options: func(cur map[string]string) []Option {
				return append(ownOptions("", cur["model"]), viaMagpie(uosaiID, magpieID+"/")...)
			},
		}},
	}
}

// uosaiModelRefByID reads the catalog ref magpie wrote for a model row id,
// when the row is magpie's (its id starts with mod-magpie- and its model
// JSON carries the model_id magpie wrote). ok is false for a row that
// isn't magpie's.
func uosaiModelRefByID(db, id string) (string, bool) {
	// a row magpie wrote carries the mod-magpie- prefix; another tool's
	// does not, so a usedModel pointing elsewhere is not magpie's pick.
	if !strings.HasPrefix(id, uosaiModelPfx) {
		return "", false
	}
	d, err := uosaiOpen(db)
	if err != nil {
		return "", false
	}
	defer d.Close()
	var model string
	if d.QueryRow(`SELECT COALESCE(model,'') FROM models WHERE id = ?`, id).Scan(&model) != nil {
		return "", false
	}
	ref := uosaiModelRef(model)
	return ref, ref != ""
}

// what UOS AI says after a change (notice.go): it reads its database on
// each turn, so a pick needs no restart, but a feature that cached its
// model in a running session picks one of magpie's the next time it asks.
var noticeUOSAI = newNotice("UOS AI reads its model at start-up — restart UOS AI to use magpie's model.")
