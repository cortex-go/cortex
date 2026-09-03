package app

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Workspace availability categories returned to the client for each stored
// conversation. They describe only the workspace's current state; the strict
// resolve check remains mandatory immediately before browsing or executing.
const (
	wsAvailable    = "available"
	wsMissing      = "missing"
	wsInaccessible = "inaccessible"
	wsOutsideRoot  = "outside-root"
	wsSymlinkEsc   = "symlink-escape"
)

type conversationEvent struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Name      string `json:"name,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
	// RunID is a private server-only field carrying the owning run for
	// server-owned events. It is excluded from JSON and used internally for
	// run-scoped supersession so a replacement never removes assistant content
	// from an unrelated run.
	RunID string `json:"-"`
}

type conversation struct {
	ID              string              `json:"id"`
	Title           string              `json:"title"`
	Workspace       string              `json:"workspace"`
	WorkspaceStatus string              `json:"workspaceStatus,omitempty"`
	Provider        string              `json:"provider,omitempty"`
	Model           string              `json:"model,omitempty"`
	OpenCodeSession string              `json:"openCodeSession,omitempty"`
	State           string              `json:"state,omitempty"`
	CurrentRunID    string              `json:"currentRunId,omitempty"`
	CreatedAt       int64               `json:"createdAt"`
	UpdatedAt       int64               `json:"updatedAt"`
	ArchivedAt      int64               `json:"archivedAt,omitempty"`
	Events          []conversationEvent `json:"events"`
}

// workspaceStatus reports the structured availability of a stored workspace
// without rewriting it. It mirrors the lexical and symlink checks of resolve
// but reports a category instead of failing, so conversation metadata and
// transcripts remain persistable when the historical workspace is missing,
// renamed, inaccessible or outside the current root. Execution itself still
// goes through the strict resolve boundary.
func (a *App) workspaceStatus(workspace string) string {
	w := strings.TrimSpace(workspace)
	if w == "" || w == "/" {
		if _, err := os.Stat(a.root); err == nil {
			return wsAvailable
		}
		return wsMissing
	}
	var p string
	if filepath.IsAbs(w) {
		p = filepath.Clean(w)
	} else {
		p = filepath.Join(a.root, w)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return wsInaccessible
	}
	rel, err := filepath.Rel(a.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return wsOutsideRoot
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wsMissing
		}
		return wsInaccessible
	}
	rel, err = filepath.Rel(a.root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return wsSymlinkEsc
	}
	return wsAvailable
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

// hasControlChars reports whether s contains CR, LF, NUL or other control
// characters that must not reach stored conversation metadata.
func hasControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
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
	if hasControlChars(c.Workspace) || hasControlChars(c.Title) {
		return errors.New("conversation contains control characters")
	}
	// The workspace is stored verbatim: a historical workspace may be missing,
	// renamed, inaccessible or outside the current root, and the transcript
	// must remain persistable in every one of those cases. Strict resolution
	// is enforced immediately before browsing or executing, never here, and an
	// unavailable workspace is never replaced with the current root.
	now := time.Now().UnixMilli()
	if c.CreatedAt <= 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	// The client never controls conversation state. The server owns the
	// running and terminal transitions, so a stale or manipulated browser
	// save can never rewrite `running`, `completed`, `failed`, `cancelled`,
	// `truncated` or `interrupted` back to `idle` or `running`. State is only
	// written for brand-new records (idle).
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
		state=conversations.state, updated_at=excluded.updated_at, archived_at=excluded.archived_at`,
		c.ID, strings.TrimSpace(c.Title), c.Workspace, c.Provider, c.Model, c.OpenCodeSession, "idle", c.CreatedAt, c.UpdatedAt, archived)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM conversation_events WHERE conversation_id = ?", c.ID); err != nil {
		return err
	}
	for i, event := range c.Events {
		if len(event.Text) > 1<<20 || len(event.Name) > 500 || event.Kind == "" {
			return errors.New("invalid conversation event")
		}
		created := event.CreatedAt
		if created <= 0 {
			created = c.CreatedAt + int64(i)
		}
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,sequence,kind,text,name,created_at) VALUES(?,?,?,?,?,?)", c.ID, i, event.Kind, event.Text, event.Name, created); err != nil {
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
	rows, err := a.db.Query(`SELECT id,title,workspace,provider,model,opencode_session_id,state,current_run_id,created_at,updated_at,archived_at
		FROM conversations`+where+` ORDER BY updated_at DESC LIMIT 251`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []conversation{}
	for rows.Next() {
		var c conversation
		var archived sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Title, &c.Workspace, &c.Provider, &c.Model, &c.OpenCodeSession, &c.State, &c.CurrentRunID, &c.CreatedAt, &c.UpdatedAt, &archived); err != nil {
			return nil, err
		}
		if archived.Valid {
			c.ArchivedAt = archived.Int64
		}
		c.WorkspaceStatus = a.workspaceStatus(c.Workspace)
		events, err := a.loadConversationMerged(c.ID)
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
	rows, err := a.db.Query("SELECT kind,text,name,created_at FROM conversation_events WHERE conversation_id = ? ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []conversationEvent{}
	for rows.Next() {
		var event conversationEvent
		if err := rows.Scan(&event.Kind, &event.Text, &event.Name, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// loadConversationMerged returns the conversation transcript as the union of
// server-owned run events (authoritative) and client-authored events,
// deduplicated by signature. Server events win: a client save that mirrored
// the streamed run cannot duplicate them, and a stale client PUT can never
// erase them because they live in a separate table.
func (a *App) loadConversationMerged(id string) ([]conversationEvent, error) {
	server, err := a.loadAgentRunEvents(id)
	if err != nil {
		return nil, err
	}
	client, err := a.loadConversationEvents(id)
	if err != nil {
		return nil, err
	}
	return mergeConversationEvents(server, client), nil
}

func eventSignature(e conversationEvent) string {
	return e.Kind + "\x00" + e.Text + "\x00" + e.Name
}

// mergeConversationEvents interleaves server-owned and client-authored events
// by creation time. Occurrence-aware deduplication removes client copies of
// server events while preserving legitimately repeated identical messages:
// a client event is dropped only when an equal server event is still unmatched.
func mergeConversationEvents(server, client []conversationEvent) []conversationEvent {
	available := map[string]int{}
	serverSig := map[string]bool{}
	for _, e := range server {
		available[eventSignature(e)]++
		serverSig[eventSignature(e)] = true
	}
	out := append([]conversationEvent{}, server...)
	for _, e := range client {
		sig := eventSignature(e)
		if available[sig] > 0 {
			available[sig]--
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		// Server events precede client events on timestamp ties so the
		// authoritative copy is not shadowed by its duplicate.
		return serverSig[eventSignature(out[i])] && !serverSig[eventSignature(out[j])]
	})
	// Terminal-event supersession: a run may have multiple durable terminal
	// markers (e.g. an ordinary "completed" marker superseded by a storage
	// failure). The latest marker per run is authoritative; earlier markers are
	// retained only as delivery history, not as a competing final outcome.
	return supersedeTerminalMarkers(supersedeAssistantPrefixes(out))
}

// replRunID extracts the run ID from a "repl:<id>" recovery-replacement marker
// name, or "" if the event is not a recovery replacement.
func replRunID(name string) string {
	if strings.HasPrefix(name, "repl:") && len(name) > len("repl:") {
		return name[len("repl:"):]
	}
	return ""
}

// supersedeAssistantPrefixes removes earlier streamed assistant events that
// are fragments of a recovery-replacement event *from the same run*. A
// replacement (e.g. "hello world") supersedes its own run's streamed fragment
// ("world" was a suffix of the recovered response, so appending would be
// misordered), leaving the full recovered response as the only assistant
// content for that answer. Text containment is never applied across runs:
// ordinary assistant events from other runs, and client-authored events, are
// preserved verbatim.
func supersedeAssistantPrefixes(events []conversationEvent) []conversationEvent {
	type replacement struct {
		text string
		run  string
	}
	replacements := []replacement{}
	for _, e := range events {
		if e.Kind == "assistant" {
			if id := replRunID(e.Name); id != "" {
				replacements = append(replacements, replacement{text: strings.TrimSpace(e.Text), run: id})
			}
		}
	}
	if len(replacements) == 0 {
		return events
	}
	drop := map[int]bool{}
	for i, e := range events {
		if e.Kind != "assistant" || replRunID(e.Name) != "" || e.RunID == "" {
			// Skip replacements and client-authored events (no run identity).
			continue
		}
		et := strings.TrimSpace(e.Text)
		if et == "" {
			continue
		}
		for _, r := range replacements {
			if r.run == e.RunID && r.text != et && strings.Contains(r.text, et) {
				drop[i] = true
				break
			}
		}
	}
	if len(drop) == 0 {
		return events
	}
	out := make([]conversationEvent, 0, len(events))
	for i, e := range events {
		if !drop[i] {
			out = append(out, e)
		}
	}
	return out
}

// runMarkerRunID extracts the run ID from a "run:<id>:<outcome>" terminal
// marker name, or "" if the event is not a run terminal marker.
func runMarkerRunID(name string) string {
	if m := strings.SplitN(name, ":", 3); len(m) == 3 && m[0] == "run" && m[1] != "" {
		return m[1]
	}
	return ""
}

// supersedeTerminalMarkers keeps only the latest durable terminal marker per
// run, so the reloaded transcript never shows both "Done" and a later
// storage-failure outcome as independent final outcomes.
func supersedeTerminalMarkers(events []conversationEvent) []conversationEvent {
	last := map[string]int{}
	for i, e := range events {
		if id := runMarkerRunID(e.Name); id != "" {
			last[id] = i
		}
	}
	if len(last) == 0 {
		return events
	}
	drop := map[int]bool{}
	for i, e := range events {
		if id := runMarkerRunID(e.Name); id != "" && i != last[id] {
			drop[i] = true
		}
	}
	out := make([]conversationEvent, 0, len(events))
	for i, e := range events {
		if !drop[i] {
			out = append(out, e)
		}
	}
	return out
}

// startAgentRun creates the durable running record and marks the conversation
// as running under this run's identity. Only the runner may write the running
// and terminal states; client PUTs never transition conversation state.
func (a *App) startAgentRun(id, conversationID, prompt, workspace, provider, model string) error {
	now := time.Now().UnixMilli()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// At most one active run per conversation: a follow-up prompt is rejected
	// while the conversation is still running.
	var running int
	if err := tx.QueryRow("SELECT COUNT(*) FROM agent_runs WHERE conversation_id=? AND state='running'", conversationID).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return errors.New("agent is already running for this conversation")
	}
	if _, err = tx.Exec(`INSERT INTO conversations(id,title,workspace,provider,model,state,created_at,updated_at,current_run_id)
		VALUES(?,?,?,?,?,'running',?,?,?)
		ON CONFLICT(id) DO UPDATE SET workspace=excluded.workspace, provider=excluded.provider,
		model=excluded.model, state='running', current_run_id=excluded.current_run_id, updated_at=excluded.updated_at`, conversationID, "", workspace, provider, model, now, now, id); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO agent_runs(id,conversation_id,state,prompt,started_at) VALUES(?,?,'running',?,?)", id, conversationID, prompt, now); err != nil {
		return err
	}
	return tx.Commit()
}

// finishAgentRun finalizes a run and its conversation. The conversation
// terminal state is only written when this run is still the conversation's
// current run, so a stale old-run finalization can never overwrite the state
// of a newer run. The execution outcome and delivery outcome are recorded
// separately; diagnostics holds the bounded structured detail.
//
// Any error is returned so the runner can record that the terminal run-state
// update did not persist, rather than silently claiming completion.
func (a *App) finishAgentRun(id, conversationID, state, sessionID, message string, input, output uint64, cost float64, diag string) error {
	if a.failFinishAgentRun != nil {
		if err := a.failFinishAgentRun(id); err != nil {
			return err
		}
	}
	now := time.Now().UnixMilli()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE agent_runs SET state=?,finished_at=?,error=?,input_tokens=?,output_tokens=?,estimated_cost_usd=?,diagnostics=? WHERE id=?`, state, now, message, input, output, cost, diag, id); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE conversations SET state=?,opencode_session_id=CASE WHEN ?='' THEN opencode_session_id ELSE ? END,updated_at=? WHERE id=? AND current_run_id=?`, state, sessionID, sessionID, now, conversationID, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// persistAgentRunEvent records one server-owned run event before it is
// attempted for delivery to the browser. Events are keyed by run ID and a
// monotonically increasing sequence so reloads can merge without duplication.
func (a *App) persistAgentRunEvent(runID, kind, text, name string, sequence, createdAt int64) error {
	if a.failAgentRunEvent != nil {
		if err := a.failAgentRunEvent(runID, kind); err != nil {
			return err
		}
	}
	_, err := a.db.Exec(`INSERT INTO agent_run_events(run_id,sequence,kind,text,name,created_at) VALUES(?,?,?,?,?,?)`, runID, sequence, kind, text, name, createdAt)
	return err
}

// loadAgentRunEvents returns the server-owned events for a conversation,
// ordered by creation time then sequence. These are authoritative: a client
// conversation PUT never touches this table. Each event carries its owning run
// ID in a private field for run-scoped supersession.
func (a *App) loadAgentRunEvents(conversationID string) ([]conversationEvent, error) {
	rows, err := a.db.Query(`SELECT e.kind,e.text,e.name,e.created_at,e.run_id
		FROM agent_run_events e JOIN agent_runs r ON r.id = e.run_id
		WHERE r.conversation_id = ? ORDER BY e.created_at, e.sequence`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []conversationEvent{}
	for rows.Next() {
		var event conversationEvent
		if err := rows.Scan(&event.Kind, &event.Text, &event.Name, &event.CreatedAt, &event.RunID); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
