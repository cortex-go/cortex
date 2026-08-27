package app

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

type conversationEvent struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

type conversation struct {
	ID              string              `json:"id"`
	Title           string              `json:"title"`
	Workspace       string              `json:"workspace"`
	Provider        string              `json:"provider,omitempty"`
	Model           string              `json:"model,omitempty"`
	OpenCodeSession string              `json:"openCodeSession,omitempty"`
	State           string              `json:"state,omitempty"`
	CreatedAt       int64               `json:"createdAt"`
	UpdatedAt       int64               `json:"updatedAt"`
	ArchivedAt      int64               `json:"archivedAt,omitempty"`
	Events          []conversationEvent `json:"events"`
}

func validRecordID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func (a *App) conversationsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := a.loadConversations(r.URL.Query().Get("q"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOut(w, items)
	case http.MethodPost:
		var q struct {
			Conversations []conversation `json:"conversations"`
		}
		if !decodeSized(w, r, &q, 16<<20) {
			return
		}
		if len(q.Conversations) > 250 {
			http.Error(w, "too many conversations", 400)
			return
		}
		tx, err := a.db.Begin()
		if err != nil {
			http.Error(w, "begin import", 500)
			return
		}
		defer tx.Rollback()
		for i := range q.Conversations {
			if err := a.saveConversationTx(tx, &q.Conversations[i]); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit import", 500)
			return
		}
		jsonOut(w, map[string]any{"ok": true, "imported": len(q.Conversations)})
	default:
		http.Error(w, "method", 405)
	}
}

func (a *App) conversationAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		var q conversation
		if !decodeSized(w, r, &q, 16<<20) {
			return
		}
		if err := a.saveConversation(&q); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jsonOut(w, map[string]bool{"ok": true})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if !validRecordID(id) {
			http.Error(w, "invalid conversation id", 400)
			return
		}
		if _, err := a.db.Exec("DELETE FROM conversations WHERE id = ?", id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOut(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "method", 405)
	}
}

func decodeSized(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	return decodeJSON(w, r, v, limit)
}

func (a *App) saveConversation(c *conversation) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := a.saveConversationTx(tx, c); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) saveConversationTx(tx *sql.Tx, c *conversation) error {
	if !validRecordID(c.ID) {
		return errors.New("invalid conversation id")
	}
	if len(c.Title) > 500 || len(c.Workspace) > 4096 || len(c.OpenCodeSession) > 256 || len(c.Events) > 2000 {
		return errors.New("conversation exceeds storage limits")
	}
	if c.Workspace != "" {
		resolved, err := a.resolve(c.Workspace)
		if err != nil {
			return errors.New("invalid conversation workspace")
		}
		c.Workspace = resolved
	}
	now := time.Now().UnixMilli()
	if c.CreatedAt <= 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.State == "" {
		c.State = "idle"
	}
	var archived any
	if c.ArchivedAt > 0 {
		archived = c.ArchivedAt
	}
	_, err := tx.Exec(`INSERT INTO conversations(id,title,workspace,provider,model,opencode_session_id,state,created_at,updated_at,archived_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, workspace=excluded.workspace,
		provider=excluded.provider, model=excluded.model, opencode_session_id=excluded.opencode_session_id,
		state=excluded.state, updated_at=excluded.updated_at, archived_at=excluded.archived_at`,
		c.ID, strings.TrimSpace(c.Title), c.Workspace, c.Provider, c.Model, c.OpenCodeSession, c.State, c.CreatedAt, c.UpdatedAt, archived)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM conversation_events WHERE conversation_id = ?", c.ID); err != nil {
		return err
	}
	for i, event := range c.Events {
		if len(event.Text) > 1<<20 || event.Kind == "" {
			return errors.New("invalid conversation event")
		}
		created := event.CreatedAt
		if created <= 0 {
			created = c.CreatedAt + int64(i)
		}
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,sequence,kind,text,created_at) VALUES(?,?,?,?,?)", c.ID, i, event.Kind, event.Text, created); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) loadConversations(query string) ([]conversation, error) {
	args := []any{}
	where := ""
	if query = strings.TrimSpace(query); query != "" {
		where = " WHERE title LIKE ? ESCAPE '\\' OR workspace LIKE ? ESCAPE '\\'"
		like := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(query) + "%"
		args = append(args, like, like)
	}
	rows, err := a.db.Query(`SELECT id,title,workspace,provider,model,opencode_session_id,state,created_at,updated_at,archived_at
		FROM conversations`+where+` ORDER BY updated_at DESC LIMIT 251`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []conversation{}
	for rows.Next() {
		var c conversation
		var archived sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Title, &c.Workspace, &c.Provider, &c.Model, &c.OpenCodeSession, &c.State, &c.CreatedAt, &c.UpdatedAt, &archived); err != nil {
			return nil, err
		}
		if archived.Valid {
			c.ArchivedAt = archived.Int64
		}
		events, err := a.loadConversationEvents(c.ID)
		if err != nil {
			return nil, err
		}
		c.Events = events
		items = append(items, c)
		if len(items) > 250 {
			return nil, errors.New("conversation result exceeds 250 items; narrow the search")
		}
	}
	return items, rows.Err()
}

func (a *App) loadConversationEvents(id string) ([]conversationEvent, error) {
	rows, err := a.db.Query("SELECT kind,text,created_at FROM conversation_events WHERE conversation_id = ? ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []conversationEvent{}
	for rows.Next() {
		var event conversationEvent
		if err := rows.Scan(&event.Kind, &event.Text, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (a *App) startAgentRun(id, conversationID, prompt, workspace, provider, model string) error {
	now := time.Now().UnixMilli()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO conversations(id,title,workspace,provider,model,state,created_at,updated_at)
		VALUES(?,?,?,?,?,'running',?,?)
		ON CONFLICT(id) DO UPDATE SET workspace=excluded.workspace, provider=excluded.provider,
		model=excluded.model, state='running', updated_at=excluded.updated_at`, conversationID, "", workspace, provider, model, now, now); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO agent_runs(id,conversation_id,state,prompt,started_at) VALUES(?,?,'running',?,?)", id, conversationID, prompt, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) finishAgentRun(id, conversationID, state, sessionID, message string, input, output uint64, cost float64) {
	now := time.Now().UnixMilli()
	tx, err := a.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE agent_runs SET state=?,finished_at=?,error=?,input_tokens=?,output_tokens=?,estimated_cost_usd=? WHERE id=?`, state, now, message, input, output, cost, id); err != nil {
		return
	}
	_, err = tx.Exec(`UPDATE conversations SET state=?,opencode_session_id=CASE WHEN ?='' THEN opencode_session_id ELSE ? END,updated_at=? WHERE id=?`, state, sessionID, sessionID, now, conversationID)
	if err == nil {
		_ = tx.Commit()
	}
}
